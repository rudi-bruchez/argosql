package diagnostics

import (
	"context"
	"database/sql"
	_ "embed"

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
// callers must already have resolved obj and be ready to open
// IndexesTable next; readIndexes never resolves anything itself and
// never calls dst.Begin/End for any table but this one.
func readIndexes(ctx context.Context, s *sqlserver.Session, obj sqlserver.Object, dst model.Sink) error {
	if err := dst.Begin(IndexesTable); err != nil {
		return err
	}
	if err := queryRows(ctx, s.Conn, indexesQuery, dst, sql.Named("id", obj.ID)); err != nil {
		return err
	}
	return dst.End(true, true)
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
// for an unrelated permission reason.
func Indexes(ctx context.Context, s *sqlserver.Session, name string, dst model.Sink) error {
	obj, err := sqlserver.Resolve(ctx, s.Conn, name)
	if err != nil {
		return err
	}
	return readIndexes(ctx, s, obj, dst)
}
