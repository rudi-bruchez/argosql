package diagnostics

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"

	"github.com/rudi-bruchez/argosql/internal/model"
	"github.com/rudi-bruchez/argosql/internal/output"
	"github.com/rudi-bruchez/argosql/internal/sqlserver"
)

// statsQuery is sql/stats.sql (see its own doc comment for what it
// reads and why) - embedded here, rather than in embed.go, for the
// same reason query.go and top.go embed their own sql/*.sql locally:
// it is this file's own concern alone.
//
//go:embed sql/stats.sql
var statsQuery string

// The three properties_status values design spec line 69 names, and
// nothing else: "Return properties_status=available|permission_denied|
// unavailable; only use permission_denied when a permission check
// establishes it. Missing properties alone do not prove the cause."
// This is the whole closed vocabulary, the same discipline obj code's
// definitionState already applies (code.go): this package never
// defaults to propertiesStatusPermissionDenied from an absence alone -
// only a confirmed sqlserver.Denied probe result ever produces it; every
// other cause of a missing properties row is propertiesStatusUnavailable.
const (
	propertiesStatusAvailable        = "available"
	propertiesStatusPermissionDenied = "permission_denied"
	propertiesStatusUnavailable      = "unavailable"
)

// StatisticsTable is the one table "stats list" emits (design spec's
// declared table order: "stats list: statistics"). Its twelve columns,
// in this exact order, are the brief's own named extension of
// sql/stats.sql's six-column skeleton - stats_id, name, columns, rows,
// rows_sampled, sample_pct, last_updated, modification_counter,
// auto_created, user_created, filter, properties_status - and
// TestStatisticsTableHasTwelveColumns exists specifically to fail if
// one of them goes missing.
//
// sample_pct is computed in Go (see Stats, below), not in SQL: it is
// NULL whenever rows itself is NULL or zero (design spec: "NULL
// sample_pct si rows=0"), never a division by zero pushed onto the
// engine. filter can be masked by a denied VIEW DEFINITION exactly like
// IndexesTable's own filter column (see that type's doc comment) - this
// task does not add a second probe for it; "stats list" already
// requires enough metadata visibility to resolve obj and read sys.stats
// at all, and filter's own masking is no worse than every other
// catalog-text column this project already tolerates without a second,
// dedicated completeness flag.
var StatisticsTable = model.TableSpec{
	Name: "statistics",
	Columns: []model.Column{
		{Name: "stats_id", SQLType: "INT"},
		{Name: "name", SQLType: "NVARCHAR"},
		{Name: "columns", SQLType: "NVARCHAR"},
		{Name: "rows", SQLType: "BIGINT"},
		{Name: "rows_sampled", SQLType: "BIGINT"},
		{Name: "sample_pct", SQLType: "FLOAT"},
		{Name: "last_updated", SQLType: "DATETIME2"},
		{Name: "modification_counter", SQLType: "BIGINT"},
		{Name: "auto_created", SQLType: "BIT"},
		{Name: "user_created", SQLType: "BIT"},
		{Name: "filter", SQLType: "NVARCHAR"},
		{Name: "properties_status", SQLType: "NVARCHAR"},
	},
}

// selectPermissionCache lazily probes OBJECT/SELECT on obj exactly
// once, however many rows of sql/stats.sql need an answer for it - the
// same permission, on the same object, does not change row to row, so
// a per-row probe would be a redundant round trip for every statistic
// past the first one that needs it. Probe's own documented limitation
// (AllProbes' object:SELECT entry, permissions.go) - a column-level-only
// grant still probes Denied at the object level - does not cause a
// false permission_denied here: a statistic whose own columns DO carry
// enough SELECT for sys.dm_db_stats_properties to succeed never reaches
// this probe at all, because its modification_counter is already
// non-NULL (see Stats, below).
type selectPermissionCache struct {
	probed bool
	denied bool
}

func (c *selectPermissionCache) deniedFor(ctx context.Context, s *sqlserver.Session, obj sqlserver.Object) (bool, error) {
	if !c.probed {
		perm, err := sqlserver.Probe(ctx, s.Conn, obj.Schema+"."+obj.Name, "OBJECT", "SELECT")
		if err != nil {
			return false, err
		}
		c.denied = perm == sqlserver.Denied
		c.probed = true
	}
	return c.denied, nil
}

// propertiesUnavailableNotice summarizes, once, how many of total rows
// did not reach propertiesStatusAvailable, split by cause (design spec:
// "Report unavailable-property counts separately from preview
// truncation" - this count is its own Notice, on its own Kind, never
// folded into rows_shown/preview_complete, which internal/output alone
// owns).
func propertiesUnavailableNotice(total, deniedCount, unavailableCount int) model.Notice {
	return model.Notice{
		Kind: "properties_unavailable",
		Message: fmt.Sprintf(
			"%d of %d statistics have inaccessible properties (permission_denied=%d, unavailable=%d); this is separate from any preview truncation",
			deniedCount+unavailableCount, total, deniedCount, unavailableCount,
		),
		Table: StatisticsTable.Name,
	}
}

// Stats runs "stats list <schema.name>": obj's statistics, their
// ordered key columns, update/sampling facts when readable, and a
// per-row properties_status that never guesses a cause it cannot
// establish (design spec: "stats list": "Ordered columns, update time,
// row/sample counts, sample percentage, modification counter,
// auto/user-created flags, and filter; per-row completeness; limited
// principals need SELECT on statistics columns for properties").
//
// Resolution precedes everything else (design spec: "Permission checks
// follow target resolution"). A resolved object of the wrong type is
// rejected at code 2 immediately after, exactly like "idx list"/"obj
// table"/"size table". A table with zero sys.stats rows is a
// successful, complete, empty result - design spec: "No readable
// statistics is a successful empty result only after the target object
// was resolved" - which Resolve above already guarantees before this
// function ever opens sql/stats.sql.
func Stats(ctx context.Context, s *sqlserver.Session, name string, dst model.Sink) error {
	obj, err := sqlserver.Resolve(ctx, s.Conn, name)
	if err != nil {
		return err
	}
	if !tableAllowedTypes[obj.Type] {
		return wrongObjectTypeError(obj, "stats list", "tables")
	}

	rows, err := s.Conn.QueryContext(ctx, statsQuery, sql.Named("id", obj.ID))
	if err != nil {
		return classifyQueryError(err, "statistics query failed")
	}
	defer rows.Close()

	if err := dst.Begin(StatisticsTable); err != nil {
		return err
	}

	var perm selectPermissionCache
	total, deniedCount, unavailableCount := 0, 0, 0

	var types []*sql.ColumnType
	for rows.Next() {
		if types == nil {
			types, err = rows.ColumnTypes()
			if err != nil {
				return &model.PublicError{Code: 5, Kind: "execution", Message: fmt.Sprintf("reading column types: %s", err.Error())}
			}
		}
		cells, err := output.ScanRow(rows, types)
		if err != nil {
			return &model.PublicError{Code: 5, Kind: "execution", Message: fmt.Sprintf("scanning statistics row: %s", err.Error())}
		}
		// cells, in sql/stats.sql's own column order: stats_id, name,
		// columns, rows, rows_sampled, last_updated, modification_counter,
		// auto_created, user_created, filter.
		total++
		modificationCounter := cells[6]

		var status string
		var samplePct model.Cell
		if modificationCounter != nil {
			status = propertiesStatusAvailable
			if rowsVal, ok := cells[3].(int64); ok && rowsVal > 0 {
				sampledVal, _ := cells[4].(int64) // rows>0 with properties available always carries a sampled count alongside it
				samplePct = float64(sampledVal) * 100.0 / float64(rowsVal)
			}
		} else {
			denied, perr := perm.deniedFor(ctx, s, obj)
			if perr != nil {
				return perr
			}
			if denied {
				status = propertiesStatusPermissionDenied
				deniedCount++
			} else {
				status = propertiesStatusUnavailable
				unavailableCount++
			}
		}

		row := []model.Cell{
			cells[0], cells[1], cells[2], cells[3], cells[4], samplePct, cells[5], cells[6], cells[7], cells[8], cells[9],
			status,
		}
		if err := dst.Row(row); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return classifyQueryError(err, "reading statistics rows")
	}

	complete := deniedCount == 0 && unavailableCount == 0
	if !complete {
		dst.Notice(propertiesUnavailableNotice(total, deniedCount, unavailableCount))
	}
	return dst.End(true, complete)
}
