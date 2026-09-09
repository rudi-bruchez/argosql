package diagnostics

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"

	"github.com/rudi-bruchez/argosql/internal/model"
	"github.com/rudi-bruchez/argosql/internal/sqlserver"
)

// indexesQuery is sql/indexes.sql (see its own doc comment for what it
// reads and why) - embedded here, rather than in embed.go, for the same
// reason query.go and top.go embed their own sql/*.sql locally: it is
// this file's own concern alone.
//
//go:embed sql/indexes.sql
var indexesQuery string

// IndexesTable is the "indexes" table both "obj table" and "idx list"
// emit (design spec's declared table order: "obj table: table,
// columns, indexes"; "idx list: indexes") - the same TableSpec value in
// both registry entries, so help's advertised schema and what
// readIndexes actually writes can never drift between the two
// commands that share it.
//
// keys and includes are pre-aggregated, escaped text (see
// sql/indexes.sql), never a second nested table: a schema this project
// already keeps flat everywhere else (TableResult.Rows is [][]Cell, no
// row ever nests another row) is not reopened here for one command.
// name is nullable in principle (a heap would have NULL here), but
// sql/indexes.sql already excludes index_id = 0 (a heap is not an
// index), so every row this table ever carries names a real index.
//
// filter is nullable for two different reasons this table cannot tell
// apart per cell: an index genuinely has no filter, or this
// principal's VIEW DEFINITION is denied on the whole object and
// filter_definition comes back NULL regardless (task 13 fix-1,
// measured: SELECT alone gives full key/include/unique/disabled
// visibility but hides filter_definition uniformly - the same
// masking ColumnsTable's default_definition/computed_definition
// suffer, see table.go's own doc comment). readIndexes probes VIEW
// DEFINITION once, for the whole object, and reports
// properties_complete=false with a notice when denied, rather than
// ever calling this table complete when it cannot tell the two apart:
// the reviewer's own example is exactly the risk of not doing this -
// an agent could mistake a unique FILTERED index for a table-wide
// uniqueness constraint with no signal telling it that conclusion is
// impossible.
var IndexesTable = model.TableSpec{
	Name: "indexes",
	Columns: []model.Column{
		{Name: "index_id", SQLType: "INT"},
		{Name: "name", SQLType: "NVARCHAR"},
		{Name: "type", SQLType: "NVARCHAR"},
		{Name: "keys", SQLType: "NVARCHAR"},
		{Name: "includes", SQLType: "NVARCHAR"},
		{Name: "filter", SQLType: "NVARCHAR"},
		{Name: "unique", SQLType: "BIT"},
		{Name: "disabled", SQLType: "BIT"},
	},
}

// readIndexes streams obj's indexes straight into dst: the "same
// private index reader" this task's brief requires Table (obj table's
// own implementation) to call rather than invoking Indexes (this
// file's own registered command) recursively through the CLI. Both
// callers must already have resolved obj (and, for Indexes, checked
// its type) before calling this; readIndexes never resolves anything
// itself.
//
// definitionComplete is the caller's own columnsPropertiesComplete
// result - see IndexesTable's own doc comment on why filter_definition
// needs it and keys/includes/unique/disabled do not - passed in rather
// than probed again here: Table (table.go) already probes VIEW
// DEFINITION once for its own ColumnsTable and reuses that same
// answer for the "indexes" table it opens right after, instead of
// this function issuing a second, redundant probe round trip for the
// exact same object and permission.
func readIndexes(ctx context.Context, s *sqlserver.Session, obj sqlserver.Object, definitionComplete bool, dst model.Sink) error {
	if err := dst.Begin(IndexesTable); err != nil {
		return err
	}
	if err := queryRows(ctx, s.Conn, indexesQuery, dst, sql.Named("id", obj.ID)); err != nil {
		return err
	}
	if !definitionComplete {
		dst.Notice(model.Notice{
			Kind:    "definition_properties_unavailable",
			Message: fmt.Sprintf("%s.%s: VIEW DEFINITION denied; filter may be masked, not genuinely absent", obj.Schema, obj.Name),
			Table:   IndexesTable.Name,
		})
	}
	return dst.End(true, definitionComplete)
}

// Indexes runs "idx list <schema.name>": obj's ordered indexes, their
// keys (with direction), included columns, filter, uniqueness and
// disabled state (design spec: "idx list": "Name/type, ordered keys
// with direction, included columns, filter, uniqueness, disabled
// state; shares implementation with table inspection"). Resolution
// precedes everything else, per the design spec's stated ordering
// (line 172: "Permission checks follow target resolution for commands
// taking object names") - an unresolved name fails at code 8 before
// readIndexes ever runs a query that could otherwise fail at code 4
// for an unrelated permission reason. A resolved object of the wrong
// type is rejected at code 2 immediately after (design spec line
// 202) - task 13 fix-1 measured that a procedure used to resolve and
// then succeed here with an empty result declared complete, instead
// of naming the type mismatch.
func Indexes(ctx context.Context, s *sqlserver.Session, name string, dst model.Sink) error {
	obj, err := sqlserver.Resolve(ctx, s.Conn, name)
	if err != nil {
		return err
	}
	if !tableAllowedTypes[obj.Type] {
		return wrongObjectTypeError(obj, "idx list", "tables")
	}
	complete, err := columnsPropertiesComplete(ctx, s, obj)
	if err != nil {
		return err
	}
	return readIndexes(ctx, s, obj, complete, dst)
}
