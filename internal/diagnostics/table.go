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

// tableAllowedTypes are the sys.objects.type codes "obj table", "idx
// list" and "size table" accept: user tables only (design spec line
// 202, "A resolved object of the wrong type gives code 2" - measured,
// task 13 fix-1: dbo.PlainModule, a procedure, used to resolve and
// then succeed on all three commands with an empty or fabricated
// result instead of naming the type mismatch).
var tableAllowedTypes = map[string]bool{"U": true}

// wrongObjectTypeError builds the code-2 error Table, Indexes and
// Size all return for a resolved object whose type command does not
// accept (design spec line 202). Checked immediately after Resolve,
// before any other query, so a wrong-type object never gets a
// partial, misleading result (an empty column/index list, or a
// memory-optimized-shaped rejection) instead of this argument error.
func wrongObjectTypeError(obj sqlserver.Object, command, accepts string) error {
	return &model.PublicError{
		Code:    2,
		Kind:    "invalid_argument",
		Message: fmt.Sprintf("%s.%s resolves to object type %q: %s accepts %s only", obj.Schema, obj.Name, obj.Type, command, accepts),
	}
}

// TableTable is the "table" header row both "obj table" and "size
// table" open first (design spec's declared table order: "obj table:
// table, columns, indexes"; "size table: table, allocations") - the
// SAME TableSpec value in both registry entries, so help's advertised
// schema and what Table/Size actually write can never drift between
// the two commands that share it. rows/total_used_bytes/
// total_reserved_bytes/total_unused_bytes are nullable together:
// unavailable, with a notice and properties_complete=false, when size
// permissions are absent or the object is memory-optimized (design
// spec line 172: "obj table may return columns/indexes with
// unavailable optional row count if size permissions are absent; size
// table itself requires those permissions") - Table degrades on this;
// Size (size.go) treats the identical unavailability as its own
// failure instead.
//
// The three byte totals were added in task 13 fix-1: "size table"
// used to expose only the per-row allocation breakdown, never the sum
// design spec lines 53/174 both require ("Approximate row count and
// allocated/used/reserved space"; "unused = reserved - used") - an
// agent could not reconstruct that sum from a preview that may be
// truncated. They live on this shared header row, not on a new table,
// because the design spec's declared table order for "size table"
// names only "table, allocations" - no third table for a summary.
var TableTable = model.TableSpec{
	Name: "table",
	Columns: []model.Column{
		{Name: "object_id", SQLType: "INT"},
		{Name: "schema", SQLType: "NVARCHAR"},
		{Name: "name", SQLType: "NVARCHAR"},
		{Name: "rows", SQLType: "BIGINT"},
		{Name: "total_used_bytes", SQLType: "BIGINT"},
		{Name: "total_reserved_bytes", SQLType: "BIGINT"},
		{Name: "total_unused_bytes", SQLType: "BIGINT"},
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
//
// default_definition and computed_definition are nullable for two
// different reasons that this table cannot tell apart per cell: a
// column genuinely has neither, or this principal's VIEW DEFINITION
// is denied on the whole object and every definition-bearing catalog
// column comes back NULL regardless (task 13 fix-1, measured: SELECT
// alone gives full column/type/nullability visibility but hides
// default_definition and computed_definition uniformly). Table probes
// VIEW DEFINITION once, for the whole object, and reports
// properties_complete=false with a notice when denied - see Table's
// own doc comment - rather than ever calling this table complete when
// it cannot tell the two apart.
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
// tableHeaderRowCount could not produce real size facts - never
// surfaced to a consumer directly, only used to pick the right notice
// text and, for Size, the right error kind.
type rowCountUnavailableReason string

const (
	rowCountReasonNone            rowCountUnavailableReason = ""
	rowCountReasonPermission      rowCountUnavailableReason = "permission_unavailable"
	rowCountReasonMemoryOptimized rowCountUnavailableReason = "memory_optimized"
	rowCountReasonNotApplicable   rowCountUnavailableReason = "not_applicable"
)

// tableHeader is table.sql's one row, converted to the four nullable
// facts TableTable's row carries beyond identity: RowCount and the
// three byte totals are either all present together or all absent
// together (design spec line 174: unused is reserved-used, computed
// here, once, rather than trusted to a caller that might forget it).
type tableHeader struct {
	RowCount                                             model.Cell
	TotalUsedBytes, TotalReservedBytes, TotalUnusedBytes model.Cell
}

// tableHeaderRowCount runs table.sql for obj and classifies its
// result into either a real tableHeader or one of
// rowCountUnavailableReason's three reasons - never both. A genuine
// execution failure (any classifyQueryError outcome that is not a
// permission denial) is returned as err and is fatal to both of this
// function's callers; every other outcome comes back err=nil, for
// Table (below) to degrade on and for Size (size.go) to treat as its
// own failure instead - seeing the SAME three reasons, because both
// commands read the exact same row.
func tableHeaderRowCount(ctx context.Context, s *sqlserver.Session, obj sqlserver.Object) (header tableHeader, reason rowCountUnavailableReason, err error) {
	cells, qerr := queryOneRow(ctx, s.Conn, tableQuery, sql.Named("id", obj.ID))
	if qerr != nil {
		var pub *model.PublicError
		if errors.As(qerr, &pub) && pub.Kind == "permission" {
			return tableHeader{}, rowCountReasonPermission, nil
		}
		return tableHeader{}, rowCountReasonNone, qerr
	}
	if memOpt, ok := cells[3].(bool); ok && memOpt {
		return tableHeader{}, rowCountReasonMemoryOptimized, nil
	}
	if cells[0] == nil {
		return tableHeader{}, rowCountReasonNotApplicable, nil
	}
	usedPages, ok := cells[1].(int64)
	if !ok {
		return tableHeader{}, rowCountReasonNone, unexpectedCell("total_used_pages")
	}
	reservedPages, ok := cells[2].(int64)
	if !ok {
		return tableHeader{}, rowCountReasonNone, unexpectedCell("total_reserved_pages")
	}
	usedBytes := usedPages * pageBytes
	reservedBytes := reservedPages * pageBytes
	return tableHeader{
		RowCount:           cells[0],
		TotalUsedBytes:     usedBytes,
		TotalReservedBytes: reservedBytes,
		TotalUnusedBytes:   reservedBytes - usedBytes,
	}, rowCountReasonNone, nil
}

// rowCountUnavailableMessage renders reason as the human text Table's
// own notice carries for obj - never invented at the call site, so
// Table and any later command sharing tableHeaderRowCount say the
// exact same thing for the exact same reason.
func rowCountUnavailableMessage(obj sqlserver.Object, reason rowCountUnavailableReason) string {
	switch reason {
	case rowCountReasonMemoryOptimized:
		return fmt.Sprintf("%s.%s is memory-optimized: row count and size are unavailable in v0.1", obj.Schema, obj.Name)
	case rowCountReasonNotApplicable:
		return fmt.Sprintf("%s.%s has no sys.dm_db_partition_stats row: row count and size do not apply to this object type", obj.Schema, obj.Name)
	default:
		return fmt.Sprintf("%s.%s: row count and size unavailable, size permissions absent (sys.dm_db_partition_stats denied)", obj.Schema, obj.Name)
	}
}

// columnsPropertiesComplete probes VIEW DEFINITION on obj once and
// reports whether ColumnsTable's default_definition/computed_definition
// columns can be trusted as genuinely absent rather than merely
// masked by this principal's own missing permission (task 13 fix-1;
// see ColumnsTable's own doc comment). A malformed-probe error
// (sqlserver.Unknown, always accompanied by a non-nil error) is
// returned rather than silently treated as complete or incomplete.
func columnsPropertiesComplete(ctx context.Context, s *sqlserver.Session, obj sqlserver.Object) (bool, error) {
	perm, err := sqlserver.Probe(ctx, s.Conn, obj.QualifiedName(), "OBJECT", "VIEW DEFINITION")
	if err != nil {
		return false, err
	}
	return perm != sqlserver.Denied, nil
}

// Table runs "obj table <schema.name>": obj's ordered columns and
// index structure, plus its approximate row count and size totals
// when size permissions allow it (design spec: "obj table": "Ordered
// columns, types/length/precision/scale, nullability,
// identity/computed/default metadata; index structure and approximate
// row count").
//
// Resolution precedes everything else (design spec line 172:
// "Permission checks follow target resolution for commands taking
// object names"). A resolved object of the wrong type is rejected at
// code 2 (design spec line 202) immediately after that, before any
// other query runs: task 13 fix-1 measured that dbo.PlainModule, a
// procedure, used to resolve and then succeed here with an empty
// columns/indexes result instead of naming the type mismatch. Once
// obj resolves and its type is accepted, a missing or denied row
// count/size never fails the whole command - only size table itself
// requires it (design spec line 172's second half); obj table
// degrades to rows/totals=nil, a notice, and properties_complete=false
// on the "table" row, then still writes columns and indexes in full.
// Columns' own default/computed definitions get the same treatment,
// independently, when VIEW DEFINITION specifically is denied (see
// columnsPropertiesComplete).
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
	if !tableAllowedTypes[obj.Type] {
		return wrongObjectTypeError(obj, "obj table", "tables")
	}

	header, reason, err := tableHeaderRowCount(ctx, s, obj)
	if err != nil {
		return err
	}

	if err := dst.Begin(TableTable); err != nil {
		return err
	}
	row := []model.Cell{obj.ID, obj.Schema, obj.Name, header.RowCount, header.TotalUsedBytes, header.TotalReservedBytes, header.TotalUnusedBytes}
	if err := dst.Row(row); err != nil {
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

	columnsComplete, err := columnsPropertiesComplete(ctx, s, obj)
	if err != nil {
		return err
	}
	if err := dst.Begin(ColumnsTable); err != nil {
		return err
	}
	if err := queryRows(ctx, s.Conn, columnsQuery, dst, sql.Named("id", obj.ID)); err != nil {
		return err
	}
	if !columnsComplete {
		dst.Notice(model.Notice{
			Kind:    "definition_properties_unavailable",
			Message: fmt.Sprintf("%s.%s: VIEW DEFINITION denied; default_definition and computed_definition may be masked, not genuinely absent", obj.Schema, obj.Name),
			Table:   ColumnsTable.Name,
		})
	}
	if err := dst.End(true, columnsComplete); err != nil {
		return err
	}

	return readIndexes(ctx, s, obj, columnsComplete, dst)
}
