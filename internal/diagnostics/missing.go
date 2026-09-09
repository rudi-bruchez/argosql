package diagnostics

import (
	"context"
	"database/sql"
	_ "embed"

	"github.com/rudi-bruchez/argosql/internal/model"
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

	if err := dst.Begin(SuggestionsTable); err != nil {
		return err
	}
	if err := queryRows(ctx, s.Conn, missingQuery, dst, sql.Named("top", opts.Top), sql.Named("object_id", objectID)); err != nil {
		return err
	}
	dst.Notice(suggestionsLimitationsNotice())
	return dst.End(true, true)
}
