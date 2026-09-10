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
// engine. filter CAN be masked by a denied VIEW DEFINITION, exactly
// like IndexesTable's own filter column (see that type's doc comment
// and readIndexes's own notice) - fix 1's A2, measured: a principal
// with SELECT on a statistic's columns but no VIEW DEFINITION on the
// object reads filter=NULL for a statistic that genuinely has one.
// Stats (below) probes columnsPropertiesComplete once, the same shared
// helper table.go/indexes.go already use for their own definition-text
// columns, and folds its result into properties_complete/the table's
// own notice - the earlier version of this comment claimed resolving
// obj was enough to guarantee filter is readable, which is false.
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

// objectSelectDenied probes OBJECT/SELECT on obj exactly once, BEFORE
// sql/stats.sql's own rows are ever opened (fix 2, measured on a real
// engine): a probe issued while those rows are still open, mid-stream,
// nests a second request onto the SAME held connection
// (internal/sqlserver.Session.Conn is one dedicated *sql.Conn, not a
// pool) - fine by coincidence when the row needing it happens to be
// the LAST one sql/stats.sql returns (the main result set is already
// fully received off the wire by then), but a genuine TDS-level
// deadlock when it is not: dbo.StatsFixture's fourth statistic,
// St_Ordered, added after St_Withheld, turned exactly this coincidence
// into a hang this project's own tests caught. Probing once, eagerly,
// for the whole command - the same shape columnsPropertiesComplete
// already uses for VIEW DEFINITION, right above - costs one round trip
// even when every statistic turns out to be available, and removes the
// hazard entirely rather than only making it rarer.
//
// Probe's own documented limitation (AllProbes' object:SELECT entry,
// permissions.go) - a column-level-only grant still probes Denied at
// the object level - does not cause a false permission_denied here: a
// statistic whose own columns DO carry enough SELECT for
// sys.dm_db_stats_properties to succeed never consults this value at
// all, because sql/stats.sql's own properties_stats_id column is
// non-NULL whenever the function returned a row (see Stats, below) -
// regardless of whether modification_counter/last_updated happen to be
// NULL too (a statistic on an empty table, or one whose blob was never
// built, legitimately has both NULL on an otherwise perfectly readable
// row; fix 1's A1, measured on a real engine).
func objectSelectDenied(ctx context.Context, s *sqlserver.Session, obj sqlserver.Object) (bool, error) {
	perm, err := sqlserver.Probe(ctx, s.Conn, obj.Schema+"."+obj.Name, "OBJECT", "SELECT")
	if err != nil {
		return false, err
	}
	return perm == sqlserver.Denied, nil
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

// filterMaskedNotice is Stats's own analogue of readIndexes's notice
// for the exact same masking (fix 1's A2): sys.stats.filter_definition
// reads NULL for a principal with SELECT but no VIEW DEFINITION on
// obj, indistinguishable at the cell level from a statistic that
// genuinely carries no filter. Same Kind, same wording as
// IndexesTable's own filter column (indexes.go) - reused rather than
// invented a second time, per dispatch.
func filterMaskedNotice(obj sqlserver.Object) model.Notice {
	return model.Notice{
		Kind:    "definition_properties_unavailable",
		Message: fmt.Sprintf("%s.%s: VIEW DEFINITION denied; filter may be masked, not genuinely absent", obj.Schema, obj.Name),
		Table:   StatisticsTable.Name,
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
//
// filter's own masking (fix 1's A2) is probed once, via the same
// columnsPropertiesComplete table.go/indexes.go already share, and
// folded into properties_complete alongside the per-row decision below
// - a narrower SELECT grant never has to widen into VIEW DEFINITION to
// succeed (design spec: partial success stays the contract, exit code
// stays 0); it only has to be declared, not hidden behind a flag
// claiming nothing is missing.
func Stats(ctx context.Context, s *sqlserver.Session, name string, dst model.Sink) error {
	obj, err := sqlserver.Resolve(ctx, s.Conn, name)
	if err != nil {
		return err
	}
	if !tableAllowedTypes[obj.Type] {
		return wrongObjectTypeError(obj, "stats list", "tables")
	}

	filterComplete, err := columnsPropertiesComplete(ctx, s, obj)
	if err != nil {
		return err
	}
	selectDenied, err := objectSelectDenied(ctx, s, obj)
	if err != nil {
		return err
	}

	rows, err := s.Conn.QueryContext(ctx, statsQuery, sql.Named("id", obj.ID))
	if err != nil {
		return classifyQueryError(err, "statistics query failed")
	}
	defer rows.Close()

	if err := dst.Begin(StatisticsTable); err != nil {
		return err
	}

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
		// auto_created, user_created, filter, properties_stats_id.
		total++
		// The signal is properties_stats_id (cells[10]), never
		// modification_counter (cells[6]): fix 1's A1. A properties row
		// that genuinely exists can carry a NULL modification_counter
		// and a NULL last_updated together (an empty table, or a blob
		// never built) without that being a permission question at
		// all - see sql/stats.sql's own doc comment and
		// selectPermissionCache's.
		propertiesRowExists := cells[10] != nil

		var status string
		var samplePct model.Cell
		if propertiesRowExists {
			status = propertiesStatusAvailable
			if rowsVal, ok := cells[3].(int64); ok && rowsVal > 0 {
				if sampledVal, ok2 := cells[4].(int64); ok2 {
					samplePct = float64(sampledVal) * 100.0 / float64(rowsVal)
				}
			}
		} else if selectDenied {
			status = propertiesStatusPermissionDenied
			deniedCount++
		} else {
			status = propertiesStatusUnavailable
			unavailableCount++
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

	if !filterComplete {
		dst.Notice(filterMaskedNotice(obj))
	}
	if deniedCount > 0 || unavailableCount > 0 {
		dst.Notice(propertiesUnavailableNotice(total, deniedCount, unavailableCount))
	}
	complete := filterComplete && deniedCount == 0 && unavailableCount == 0
	return dst.End(true, complete)
}
