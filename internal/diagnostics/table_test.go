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
		tableRowResponse(int64(42), nil),
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

	indexesTbl := sink.table("indexes")
	if indexesTbl == nil || len(indexesTbl.rows) != 1 {
		t.Fatalf("indexes: want exactly one row, got %#v", indexesTbl)
	}
	if got := indexesTbl.rows[0][3]; got != "[OrderId] ASC" {
		t.Fatalf("indexes[0].keys cell: got %#v, want %q", got, "[OrderId] ASC")
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
	if sink.noticeWithKind("row_count_unavailable") == nil {
		t.Fatal("want a row_count_unavailable notice when size permissions are absent")
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
		tableRowResponse(nil, true),
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
