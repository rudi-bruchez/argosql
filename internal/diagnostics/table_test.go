package diagnostics

import (
	"context"
	"database/sql/driver"
	"errors"
	"testing"

	"github.com/rudi-bruchez/argosql/internal/model"

	mssql "github.com/microsoft/go-mssqldb"
)

// TestTableUnresolvedNameReturnsEight proves design spec line 172's
// stated ordering ("Permission checks follow target resolution for
// commands taking object names") the direction that actually matters:
// an unresolved name must fail at code 8 (not_found_or_not_visible)
// without Table ever reaching a permission-dependent query that could
// otherwise misreport the failure as code 4. This is dispatch cassure
// 1's own target: swapping Resolve and a permission check would turn
// this exact assertion from 8 to 4.
func TestTableUnresolvedNameReturnsEight(t *testing.T) {
	conn := &fakeObjConn{responses: []objQueryResponse{resolveNotFoundResponse()}}
	sess := newFakeObjSession(t, conn)

	err := Table(context.Background(), sess, "dbo.Missing", &objCaptureSink{})
	var pub *model.PublicError
	if !errors.As(err, &pub) {
		t.Fatalf("Table: want *model.PublicError, got %#v", err)
	}
	if pub.Code != 8 || pub.Kind != "not_found_or_not_visible" {
		t.Fatalf("Table on an unresolved name: want code 8/not_found_or_not_visible, got code %d/%s", pub.Code, pub.Kind)
	}
}

// TestTableWritesTableColumnsIndexesInOrderWithCellValues covers three
// things at once, deliberately: the declared table order (design spec
// line 103: "obj table: table, columns, indexes"), and - the recurring
// defect this project has paid for three times on other tasks - real
// CELL VALUES for every table, not merely row counts. A formatting
// function collapsed to the identity, or a column silently gone null,
// would not fail a test that only checked len(rows) or a completeness
// flag; every assertion below reads a specific cell.
func TestTableWritesTableColumnsIndexesInOrderWithCellValues(t *testing.T) {
	conn := &fakeObjConn{responses: []objQueryResponse{
		resolveFoundResponse(501, "dbo", "Orders", "U"),
		tableRowResponse(int64(42), int64(10), int64(12), nil),
		permProbeResponse(int64(1)), // VIEW DEFINITION allowed: columns/indexes properties complete
		columnsRowsResponse([][]driver.Value{
			{int64(1), "OrderId", "int", int64(4), int64(10), int64(0), false, true, false, nil, nil},
			{int64(2), "Total", "decimal", int64(9), int64(10), int64(2), false, false, false, "((0))", nil},
		}),
		indexesRowsResponse([][]driver.Value{
			{int64(1), "PK_Orders", "CLUSTERED", "[OrderId] ASC", nil, nil, true, false},
		}),
	}}
	sess := newFakeObjSession(t, conn)
	sink := &objCaptureSink{}

	if err := Table(context.Background(), sess, "dbo.Orders", sink); err != nil {
		t.Fatalf("Table: %v", err)
	}

	wantOrder := []string{"table", "columns", "indexes"}
	if len(sink.beginOrder) != len(wantOrder) {
		t.Fatalf("Begin call order: got %v, want %v", sink.beginOrder, wantOrder)
	}
	for i, name := range wantOrder {
		if sink.beginOrder[i] != name {
			t.Fatalf("Begin call order: got %v, want %v", sink.beginOrder, wantOrder)
		}
	}

	tableTbl := sink.table("table")
	if tableTbl == nil || len(tableTbl.rows) != 1 {
		t.Fatalf("table: want exactly one row, got %#v", tableTbl)
	}
	if got := tableTbl.rows[0][3]; got != int64(42) {
		t.Fatalf("table row_count cell: got %#v, want int64(42)", got)
	}
	// design spec lines 53/174: size table (and, sharing this same row,
	// obj table) must expose the used/reserved/unused totals, not only
	// the per-row allocation breakdown (task 13 fix-1).
	if got := tableTbl.rows[0][4]; got != int64(10*pageBytes) {
		t.Fatalf("table total_used_bytes cell: got %#v, want %d", got, int64(10*pageBytes))
	}
	if got := tableTbl.rows[0][5]; got != int64(12*pageBytes) {
		t.Fatalf("table total_reserved_bytes cell: got %#v, want %d", got, int64(12*pageBytes))
	}
	if got := tableTbl.rows[0][6]; got != int64(2*pageBytes) {
		t.Fatalf("table total_unused_bytes cell: got %#v, want %d (reserved-used)", got, int64(2*pageBytes))
	}
	if !tableTbl.propertiesComplete {
		t.Fatal("table.propertiesComplete: want true when row count is available")
	}

	columnsTbl := sink.table("columns")
	if columnsTbl == nil || len(columnsTbl.rows) != 2 {
		t.Fatalf("columns: want exactly two rows, got %#v", columnsTbl)
	}
	if got := columnsTbl.rows[1][2]; got != "decimal" {
		t.Fatalf("columns[1].type cell: got %#v, want %q", got, "decimal")
	}
	if got := columnsTbl.rows[1][9]; got != "((0))" {
		t.Fatalf("columns[1].default_definition cell: got %#v, want %q", got, "((0))")
	}
	// fix-2/A9: the POSITIVE completeness case was never asserted - only
	// TestTableColumnsPropertiesIncompleteWhenDefinitionDenied checked
	// false. A cassure fixing dst.End(true, false) here would have
	// passed silently without this.
	if !columnsTbl.propertiesComplete {
		t.Fatal("columns.propertiesComplete: want true when VIEW DEFINITION is allowed")
	}

	indexesTbl := sink.table("indexes")
	if indexesTbl == nil || len(indexesTbl.rows) != 1 {
		t.Fatalf("indexes: want exactly one row, got %#v", indexesTbl)
	}
	if got := indexesTbl.rows[0][3]; got != "[OrderId] ASC" {
		t.Fatalf("indexes[0].keys cell: got %#v, want %q", got, "[OrderId] ASC")
	}
	if !indexesTbl.propertiesComplete {
		t.Fatal("indexes.propertiesComplete: want true when VIEW DEFINITION is allowed")
	}
}

// TestTableDegradesWhenSizePermissionAbsent is dispatch cassure 5's own
// target: design spec line 172's second half ("obj table may return
// columns/indexes with unavailable optional row count if size
// permissions are absent; size table itself requires those
// permissions") requires Table to SUCCEED, with rows=nil and a notice,
// when the underlying sys.dm_db_partition_stats read is denied -
// never to fail the whole command the way size.go's own Size does on
// the identical denial (see TestSizeFailsWhenPermissionAbsent).
func TestTableDegradesWhenSizePermissionAbsent(t *testing.T) {
	conn := &fakeObjConn{responses: []objQueryResponse{
		resolveFoundResponse(501, "dbo", "Orders", "U"),
		tableErrResponse(mssql.Error{Number: 229, Message: "denied"}),
		permProbeResponse(int64(1)), // VIEW DEFINITION allowed: columns/indexes properties complete
		columnsRowsResponse(nil),
		indexesRowsResponse(nil),
	}}
	sess := newFakeObjSession(t, conn)
	sink := &objCaptureSink{}

	if err := Table(context.Background(), sess, "dbo.Orders", sink); err != nil {
		t.Fatalf("Table: want success (degraded row count), got error: %v", err)
	}

	tableTbl := sink.table("table")
	if tableTbl == nil || len(tableTbl.rows) != 1 {
		t.Fatalf("table: want exactly one row, got %#v", tableTbl)
	}
	if got := tableTbl.rows[0][3]; got != nil {
		t.Fatalf("table row_count cell: got %#v, want nil (unavailable)", got)
	}
	if tableTbl.propertiesComplete {
		t.Fatal("table.propertiesComplete: want false when row count is unavailable")
	}
	notice := sink.noticeWithKind("row_count_unavailable")
	if notice == nil {
		t.Fatal("want a row_count_unavailable notice when size permissions are absent")
	}
	// fix-2/A9: the notice's own Table field, which names what it's
	// about, had no assertion - a cassure setting it to an arbitrary
	// string passed silently.
	if notice.Table != TableTable.Name {
		t.Fatalf("row_count_unavailable notice.Table: got %q, want %q", notice.Table, TableTable.Name)
	}
}

// TestTableDegradesOnMemoryOptimized proves the other rowCountUnavailableReason
// Table must degrade on rather than fail: design spec line 174's
// "Memory-optimized table size is unavailable with code 4 in v0.1" -
// stated for size table (see TestSizeMemoryOptimizedReturnsFour) - but
// obj table's own row still succeeds, with rows=nil and a notice.
func TestTableDegradesOnMemoryOptimized(t *testing.T) {
	conn := &fakeObjConn{responses: []objQueryResponse{
		resolveFoundResponse(777, "dbo", "MemTab", "U"),
		tableRowResponse(nil, nil, nil, true),
		permProbeResponse(int64(1)), // VIEW DEFINITION allowed: columns/indexes properties complete
		columnsRowsResponse(nil),
		indexesRowsResponse(nil),
	}}
	sess := newFakeObjSession(t, conn)
	sink := &objCaptureSink{}

	if err := Table(context.Background(), sess, "dbo.MemTab", sink); err != nil {
		t.Fatalf("Table: want success (degraded row count), got error: %v", err)
	}
	tableTbl := sink.table("table")
	if got := tableTbl.rows[0][3]; got != nil {
		t.Fatalf("table row_count cell: got %#v, want nil (memory-optimized)", got)
	}
	if tableTbl.propertiesComplete {
		t.Fatal("table.propertiesComplete: want false for a memory-optimized table")
	}
}

// TestTableRejectsWrongObjectType is task 13 fix-1's own A0 target:
// design spec line 202, "A resolved object of the wrong type gives
// code 2" - measured before this fix: obj table on dbo.PlainModule (a
// procedure) resolved and then succeeded with an empty columns/indexes
// result instead of naming the type mismatch.
func TestTableRejectsWrongObjectType(t *testing.T) {
	conn := &fakeObjConn{responses: []objQueryResponse{
		resolveFoundResponse(999, "dbo", "PlainModule", "P"),
	}}
	sess := newFakeObjSession(t, conn)
	sink := &objCaptureSink{}

	err := Table(context.Background(), sess, "dbo.PlainModule", sink)
	var pub *model.PublicError
	if !errors.As(err, &pub) {
		t.Fatalf("Table: want *model.PublicError, got %#v", err)
	}
	if pub.Code != 2 || pub.Kind != "invalid_argument" {
		t.Fatalf("Table on a procedure: want code 2/invalid_argument, got code %d/%s", pub.Code, pub.Kind)
	}
	if len(sink.tables) != 0 {
		t.Fatalf("wrong object type: want no table written, got %#v", sink.tables)
	}
}

// TestTableColumnsPropertiesIncompleteWhenDefinitionDenied is task 13
// fix-1's own A1 target: a principal with SELECT alone (no VIEW
// DEFINITION) sees full column/type/nullability metadata, but
// default_definition/computed_definition come back NULL exactly like
// a genuinely absent default or computed formula - measured by the
// reviewer against a real engine. columns must report
// properties_complete=false with a notice rather than the fixed
// "true" it used to pass regardless.
func TestTableColumnsPropertiesIncompleteWhenDefinitionDenied(t *testing.T) {
	conn := &fakeObjConn{responses: []objQueryResponse{
		resolveFoundResponse(501, "dbo", "ColumnsFixture", "U"),
		tableRowResponse(int64(2), int64(1), int64(1), nil),
		permProbeResponse(int64(0)), // VIEW DEFINITION denied
		columnsRowsResponse([][]driver.Value{
			{int64(1), "Price", "decimal", int64(9), int64(12), int64(4), false, false, false, nil, nil},
		}),
		indexesRowsResponse(nil),
	}}
	sess := newFakeObjSession(t, conn)
	sink := &objCaptureSink{}

	if err := Table(context.Background(), sess, "dbo.ColumnsFixture", sink); err != nil {
		t.Fatalf("Table: want success (masked properties, not a failure), got error: %v", err)
	}

	columnsTbl := sink.table("columns")
	if columnsTbl == nil {
		t.Fatal("columns table missing")
	}
	if columnsTbl.propertiesComplete {
		t.Fatal("columns.propertiesComplete: want false when VIEW DEFINITION is denied - default_definition may be masked, not genuinely absent")
	}
	if sink.noticeWithKind("definition_properties_unavailable") == nil {
		t.Fatal("want a definition_properties_unavailable notice when VIEW DEFINITION is denied")
	}

	indexesTbl := sink.table("indexes")
	if indexesTbl == nil {
		t.Fatal("indexes table missing")
	}
	if indexesTbl.propertiesComplete {
		t.Fatal("indexes.propertiesComplete: want false too - readIndexes reuses the same VIEW DEFINITION answer Table already probed")
	}
}
