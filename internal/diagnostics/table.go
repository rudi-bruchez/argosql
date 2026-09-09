package diagnostics

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"

	"github.com/rudi-bruchez/argosql/internal/model"
	"github.com/rudi-bruchez/argosql/internal/sqlserver"
)

// tableQuery and columnsQuery are sql/table.sql and sql/columns.sql
// (see each one's own doc comment for what it reads and why) -
// embedded here, rather than in embed.go, for the same reason
// query.go and top.go embed their own sql/*.sql locally: they are this
// file's own concern alone.
var (
	//go:embed sql/table.sql
	tableQuery string

	//go:embed sql/columns.sql
	columnsQuery string
)

// TableTable is the "table" header row both "obj table" and "size
// table" open first (design spec's declared table order: "obj table:
// table, columns, indexes"; "size table: table, allocations") - the
// SAME TableSpec value in both registry entries, so help's advertised
// schema and what Table/Size actually write can never drift between
// the two commands that share it. rows is nullable: unavailable, with
// a notice and properties_complete=false, when size permissions are
// absent or the object is memory-optimized (design spec line 172:
// "obj table may return columns/indexes with unavailable optional row
// count if size permissions are absent; size table itself requires
// those permissions") - Table degrades on this; Size (size.go) treats
// the identical unavailability as its own failure instead.
var TableTable = model.TableSpec{
	Name: "table",
	Columns: []model.Column{
		{Name: "object_id", SQLType: "INT"},
		{Name: "schema", SQLType: "NVARCHAR"},
		{Name: "name", SQLType: "NVARCHAR"},
		{Name: "rows", SQLType: "BIGINT"},
	},
}

// ColumnsTable is "obj table"'s second table (design spec: "obj
// table": "Ordered columns, types/length/precision/scale, nullability,
// identity/computed/default metadata"). max_length is SQL Server's own
// byte length, never divided by two for a Unicode type - see
// sql/columns.sql's own doc comment and this command's registered
// Units, which document that conversion explicitly rather than adding
// a column this declared schema does not carry (design spec line 24's
// two options: signal the byte length explicitly, or add a separate
// character-length field; this project signals it, in help, rather
// than growing the schema).
var ColumnsTable = model.TableSpec{
	Name: "columns",
	Columns: []model.Column{
		{Name: "ordinal", SQLType: "INT"},
		{Name: "name", SQLType: "NVARCHAR"},
		{Name: "type", SQLType: "NVARCHAR"},
		{Name: "max_length", SQLType: "SMALLINT"},
		{Name: "precision", SQLType: "TINYINT"},
		{Name: "scale", SQLType: "TINYINT"},
		{Name: "nullable", SQLType: "BIT"},
		{Name: "identity", SQLType: "BIT"},
		{Name: "computed", SQLType: "BIT"},
		{Name: "default_definition", SQLType: "NVARCHAR"},
		{Name: "computed_definition", SQLType: "NVARCHAR"},
	},
}

// rowCountUnavailableReason is Table/Size's shared vocabulary for why
// tableHeaderRowCount could not produce a real row count - never
// surfaced to a consumer directly, only used to pick the right notice
// text and, for Size, the right error kind.
type rowCountUnavailableReason string

const (
	rowCountReasonNone            rowCountUnavailableReason = ""
	rowCountReasonPermission      rowCountUnavailableReason = "permission_unavailable"
	rowCountReasonMemoryOptimized rowCountUnavailableReason = "memory_optimized"
	rowCountReasonNotApplicable   rowCountUnavailableReason = "not_applicable"
)

// tableHeaderRowCount runs table.sql for obj and classifies its result
// into either a real row count or one of rowCountUnavailableReason's
// three reasons - never both a row count and a reason. A genuine
// execution failure (any classifyQueryError outcome that is not a
// permission denial) is returned as err and is fatal to both of this
// function's callers; every other outcome comes back err=nil, for
// Table (below) to degrade on and for Size (size.go) to treat as its
// own failure instead - seeing the SAME three reasons, because both
// commands read the exact same row.
func tableHeaderRowCount(ctx context.Context, s *sqlserver.Session, obj sqlserver.Object) (rows model.Cell, reason rowCountUnavailableReason, err error) {
	cells, qerr := queryOneRow(ctx, s.Conn, tableQuery, sql.Named("id", obj.ID))
	if qerr != nil {
		var pub *model.PublicError
		if errors.As(qerr, &pub) && pub.Kind == "permission" {
			return nil, rowCountReasonPermission, nil
		}
		return nil, rowCountReasonNone, qerr
	}
	if memOpt, ok := cells[1].(bool); ok && memOpt {
		return nil, rowCountReasonMemoryOptimized, nil
	}
	if cells[0] == nil {
		return nil, rowCountReasonNotApplicable, nil
	}
	return cells[0], rowCountReasonNone, nil
}

// rowCountUnavailableMessage renders reason as the human text Table's
// own notice carries for obj - never invented at the call site, so
// Table and any later command sharing tableHeaderRowCount say the
// exact same thing for the exact same reason.
func rowCountUnavailableMessage(obj sqlserver.Object, reason rowCountUnavailableReason) string {
	switch reason {
	case rowCountReasonMemoryOptimized:
		return fmt.Sprintf("%s.%s is memory-optimized: row count is unavailable in v0.1", obj.Schema, obj.Name)
	case rowCountReasonNotApplicable:
		return fmt.Sprintf("%s.%s has no sys.dm_db_partition_stats row: row count does not apply to this object type", obj.Schema, obj.Name)
	default:
		return fmt.Sprintf("%s.%s: row count unavailable, size permissions absent (sys.dm_db_partition_stats denied)", obj.Schema, obj.Name)
	}
}

// Table runs "obj table <schema.name>": obj's ordered columns and
// index structure, plus its approximate row count when size
// permissions allow it (design spec: "obj table": "Ordered columns,
// types/length/precision/scale, nullability, identity/computed/default
// metadata; index structure and approximate row count").
//
// Resolution precedes everything else (design spec line 172:
// "Permission checks follow target resolution for commands taking
// object names"): an unresolved name fails at code 8 before this
// function ever runs a query that could otherwise fail at code 4 for
// an unrelated permission reason. Once obj resolves, a missing or
// denied row count never fails the whole command - only size table
// itself requires it (design spec line 172's second half); obj table
// degrades to rows=nil, a notice, and properties_complete=false on the
// "table" row, then still writes columns and indexes in full.
//
// Table calls readIndexes, the exact same private reader "idx list"
// (Indexes, in indexes.go) calls, rather than invoking the CLI's own
// "idx list" command recursively - this task's brief requires exactly
// this: "Table appelle le même lecteur privé d'index que Indexes, sans
// invoquer le CLI récursivement."
func Table(ctx context.Context, s *sqlserver.Session, name string, dst model.Sink) error {
	obj, err := sqlserver.Resolve(ctx, s.Conn, name)
	if err != nil {
		return err
	}

	rows, reason, err := tableHeaderRowCount(ctx, s, obj)
	if err != nil {
		return err
	}

	if err := dst.Begin(TableTable); err != nil {
		return err
	}
	if err := dst.Row([]model.Cell{obj.ID, obj.Schema, obj.Name, rows}); err != nil {
		return err
	}
	complete := reason == rowCountReasonNone
	if !complete {
		dst.Notice(model.Notice{
			Kind:    "row_count_unavailable",
			Message: rowCountUnavailableMessage(obj, reason),
			Table:   TableTable.Name,
		})
	}
	if err := dst.End(true, complete); err != nil {
		return err
	}

	if err := dst.Begin(ColumnsTable); err != nil {
		return err
	}
	if err := queryRows(ctx, s.Conn, columnsQuery, dst, sql.Named("id", obj.ID)); err != nil {
		return err
	}
	if err := dst.End(true, true); err != nil {
		return err
	}

	return readIndexes(ctx, s, obj, dst)
}
