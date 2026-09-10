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

// missingQuery is sql/missing.sql (see its own doc comment for what it
// reads and why) - embedded here, rather than in embed.go, for the same
// reason query.go and top.go embed their own sql/*.sql locally: it is
// this file's own concern alone.
//
//go:embed sql/missing.sql
var missingQuery string

// SuggestionsTable is the one table "idx missing" emits (design spec's
// declared table order: "idx missing: suggestions"). impact_score is
// named for exactly what it is - a ranking score derived from optimizer
// estimates, never a predicted execution-time saving (design spec:
// "label it a ranking score, not predicted elapsed-time savings") - and
// this table never carries a CREATE INDEX script (design spec: "idx
// missing": "no CREATE script"), even though equality_columns,
// inequality_columns and included_columns compose naturally into one.
//
// schema_name/object_name are nullable for a reason distinct from every
// other nullable column in this project's tables: not a masked-versus-
// absent ambiguity (see IndexesTable's own doc comment for that one),
// but the deliberate LEFT JOIN design spec names ("Use left joins for
// optional local names so metadata visibility cannot silently remove
// DMV evidence") - a principal who can see this row's instance-level
// evidence but not the referenced object's own catalog row still gets
// the row, with these two columns NULL, rather than the row disappearing.
// Rows are ordered by impact_score descending with index_handle as a
// stable tie-break (design spec: "Rows use IDs/ordinals ascending
// unless ranking is specified" - suggestions is the one table of the
// three this task adds that IS a ranking), via sql/missing.sql's own
// ORDER BY.
var SuggestionsTable = model.TableSpec{
	Name: "suggestions",
	Columns: []model.Column{
		{Name: "index_handle", SQLType: "INT"},
		{Name: "object_id", SQLType: "INT"},
		{Name: "schema_name", SQLType: "NVARCHAR"},
		{Name: "object_name", SQLType: "NVARCHAR"},
		{Name: "equality_columns", SQLType: "NVARCHAR"},
		{Name: "inequality_columns", SQLType: "NVARCHAR"},
		{Name: "included_columns", SQLType: "NVARCHAR"},
		{Name: "user_seeks", SQLType: "BIGINT"},
		{Name: "user_scans", SQLType: "BIGINT"},
		{Name: "avg_total_user_cost", SQLType: "FLOAT"},
		{Name: "avg_user_impact", SQLType: "FLOAT"},
		{Name: "impact_score", SQLType: "FLOAT"},
	},
}

// suggestionsLimitationsNotice is the advisory-limitations Notice "idx
// missing" always emits (design spec: "idx missing": "... transparent
// impact score, and advisory limitations; no CREATE script"):
// impact_score is a ranking score, not a time estimate, it does not
// account for indexes that already exist or for write cost, and this
// command never fabricates a percentage of existing coverage (design
// spec: "Do not fabricate percentage coverage by existing indexes").
func suggestionsLimitationsNotice() model.Notice {
	return model.Notice{
		Kind: "ranking_score",
		Message: "impact_score is a ranking score derived from optimizer estimates, not a predicted execution-time saving; " +
			"it ignores write cost and indexes that already exist, and this command never emits a CREATE INDEX script",
		Table: SuggestionsTable.Name,
	}
}

// hiddenObjectNotice is fix 1's A3: sql/missing.sql's own LEFT JOINs to
// sys.objects/sys.schemas preserve a suggestion row whose referenced
// object's catalog metadata this principal cannot see (design spec:
// "Use left joins for optional local names so metadata visibility
// cannot silently remove DMV evidence") - but preserving the row is not
// the same thing as declaring it complete. Measured on a real engine
// under principal S with a DENY on one fixture object: the row
// survives with schema_name/object_name both NULL, and the command
// used to report properties_complete=true anyway, with no warning at
// all. Same treatment as readIndexes's/Stats's own masked-property
// notices: name the cause, mark the flag, never claim nothing is
// missing.
func hiddenObjectNotice() model.Notice {
	return model.Notice{
		Kind:    "definition_properties_unavailable",
		Message: "one or more suggestions reference an object whose schema_name/object_name this principal cannot see; the DMV evidence is preserved, but its local identification is incomplete",
		Table:   SuggestionsTable.Name,
	}
}

// MissingOptions carries "idx missing"'s own flags: Table is the
// optional --table <schema.name> filter (empty means every object),
// Top is --top, already validated and defaulted by the registry
// (range [1, 100], default 10 - design spec: "Both ranking commands
// accept --top from 1 to 100").
type MissingOptions struct {
	Table string
	Top   int
}

// Missing runs "idx missing [--table <schema.name>] [--top N]": the
// top N missing-index suggestions by impact_score, across the whole
// database or filtered to one resolved object (design spec: "idx
// missing": "Default top 10 suggestions, raw DMV evidence, transparent
// impact score, and advisory limitations; no CREATE script").
//
// --table, when given, resolves through the exact same object-visibility
// contract every other object-name flag in this program does (design
// spec: "An optional table filter must first resolve through the
// object-visibility contract") - an unresolved --table name fails at
// code 8 here, before sql/missing.sql ever runs, which is also before
// that query could otherwise fail at code 4 for the instance-level
// permission sys.dm_db_missing_index_details/groups/group_stats all
// require (measured against Microsoft's own documentation) - design
// spec: "Permission checks follow target resolution for commands
// taking object names." @object_id is always this resolved obj.ID,
// never opts.Table's own raw text.
//
// properties_complete reflects the LEFT JOIN's own local-name coverage
// (fix 1's A3), not merely that the query ran: any row whose
// schema_name or object_name came back NULL - the referenced object is
// invisible to this principal, even though its DMV evidence was kept -
// turns the whole table incomplete, with hiddenObjectNotice naming why,
// rather than a flag claiming nothing is missing.
func Missing(ctx context.Context, s *sqlserver.Session, opts MissingOptions, dst model.Sink) error {
	var objectID sql.NullInt64
	if opts.Table != "" {
		obj, err := sqlserver.Resolve(ctx, s.Conn, opts.Table)
		if err != nil {
			return err
		}
		if !tableAllowedTypes[obj.Type] {
			return wrongObjectTypeError(obj, "idx missing", "tables")
		}
		objectID = sql.NullInt64{Int64: obj.ID, Valid: true}
	}

	rows, err := s.Conn.QueryContext(ctx, missingQuery, sql.Named("top", opts.Top), sql.Named("object_id", objectID))
	if err != nil {
		return classifyQueryError(err, "missing-index query failed")
	}
	defer rows.Close()

	if err := dst.Begin(SuggestionsTable); err != nil {
		return err
	}

	hiddenObject := false
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
			return &model.PublicError{Code: 5, Kind: "execution", Message: fmt.Sprintf("scanning suggestion row: %s", err.Error())}
		}
		// cells, in sql/missing.sql's own column order: index_handle,
		// object_id, schema_name, object_name, equality_columns,
		// inequality_columns, included_columns, user_seeks, user_scans,
		// avg_total_user_cost, avg_user_impact, impact_score.
		if cells[2] == nil || cells[3] == nil {
			hiddenObject = true
		}
		if err := dst.Row(cells); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return classifyQueryError(err, "reading suggestion rows")
	}

	dst.Notice(suggestionsLimitationsNotice())
	if hiddenObject {
		dst.Notice(hiddenObjectNotice())
	}
	return dst.End(true, !hiddenObject)
}
