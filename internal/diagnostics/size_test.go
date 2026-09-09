package diagnostics

import (
	"context"
	"database/sql/driver"
	"errors"
	"testing"

	"github.com/rudi-bruchez/argosql/internal/model"

	mssql "github.com/microsoft/go-mssqldb"
)

// TestSizeUnresolvedNameReturnsEight is dispatch cassure 1's own
// target for "size table": resolution precedes everything else
// (design spec line 172).
func TestSizeUnresolvedNameReturnsEight(t *testing.T) {
	conn := &fakeObjConn{responses: []objQueryResponse{resolveNotFoundResponse()}}
	sess := newFakeObjSession(t, conn)

	err := Size(context.Background(), sess, "dbo.Missing", &objCaptureSink{})
	var pub *model.PublicError
	if !errors.As(err, &pub) {
		t.Fatalf("Size: want *model.PublicError, got %#v", err)
	}
	if pub.Code != 8 || pub.Kind != "not_found_or_not_visible" {
		t.Fatalf("Size on an unresolved name: want code 8/not_found_or_not_visible, got code %d/%s", pub.Code, pub.Kind)
	}
}

// TestSizeFailsWhenPermissionAbsent is dispatch cassure 5's own
// target, the OTHER half: design spec line 172's second half ("size
// table itself requires those permissions") - the exact same
// tableHeaderRowCount denial Table (table.go) degrades on
// (TestTableDegradesWhenSizePermissionAbsent) must instead FAIL Size
// outright, at code 4.
func TestSizeFailsWhenPermissionAbsent(t *testing.T) {
	conn := &fakeObjConn{responses: []objQueryResponse{
		resolveFoundResponse(901, "dbo", "Orders", "U"),
		tableErrResponse(mssql.Error{Number: 300, Message: "denied"}),
	}}
	sess := newFakeObjSession(t, conn)
	sink := &objCaptureSink{}

	err := Size(context.Background(), sess, "dbo.Orders", sink)
	var pub *model.PublicError
	if !errors.As(err, &pub) {
		t.Fatalf("Size: want *model.PublicError, got %#v", err)
	}
	if pub.Code != 4 || pub.Kind != "permission" {
		t.Fatalf("Size on a denied size permission: want code 4/permission, got code %d/%s", pub.Code, pub.Kind)
	}
	if len(sink.tables) != 0 {
		t.Fatalf("Size on a denial must write no table at all, got %#v", sink.tables)
	}
}

// TestSizeMemoryOptimizedReturnsFour is design spec line 174's own
// clause: "Memory-optimized table size is unavailable with code 4 in
// v0.1."
func TestSizeMemoryOptimizedReturnsFour(t *testing.T) {
	conn := &fakeObjConn{responses: []objQueryResponse{
		resolveFoundResponse(902, "dbo", "MemTab", "U"),
		tableRowResponse(nil, true),
	}}
	sess := newFakeObjSession(t, conn)
	sink := &objCaptureSink{}

	err := Size(context.Background(), sess, "dbo.MemTab", sink)
	var pub *model.PublicError
	if !errors.As(err, &pub) {
		t.Fatalf("Size: want *model.PublicError, got %#v", err)
	}
	if pub.Code != 4 || pub.Kind != "memory_optimized_unavailable" {
		t.Fatalf("Size on a memory-optimized table: want code 4/memory_optimized_unavailable, got code %d/%s", pub.Code, pub.Kind)
	}
}

// TestSizeAllocationsCellValuesAndByteMath is the brief's own named
// extension assertion: on a table with a clustered index, two
// nonclustered indexes and a filled LOB column, the allocations table
// carries more than one row, its categories cover all three
// allocation_type values encountered, and the sum of its used_pages
// equals the row-count query's own total - proving used_bytes is
// really used_pages*8192, not a copy of the page count (the recurring
// defect this project has paid for three times: a table with no cell
// VALUE assertion, only a row count).
func TestSizeAllocationsCellValuesAndByteMath(t *testing.T) {
	conn := &fakeObjConn{responses: []objQueryResponse{
		resolveFoundResponse(903, "dbo", "SizeFixture", "U"),
		tableRowResponse(int64(50), nil),
		sizeRowsResponse([][]driver.Value{
			{int64(1), int64(1), "IN_ROW_DATA", int64(10), int64(12)},
			{int64(1), int64(1), "LOB_DATA", int64(30), int64(35)},
			{int64(2), int64(1), "IN_ROW_DATA", int64(5), int64(6)},
			{int64(3), int64(1), "ROW_OVERFLOW_DATA", int64(7), int64(8)},
		}),
	}}
	sess := newFakeObjSession(t, conn)
	sink := &objCaptureSink{}

	if err := Size(context.Background(), sess, "dbo.SizeFixture", sink); err != nil {
		t.Fatalf("Size: %v", err)
	}

	tableTbl := sink.table("table")
	if tableTbl == nil || tableTbl.rows[0][3] != int64(50) {
		t.Fatalf("table row_count cell: got %#v, want int64(50)", tableTbl)
	}

	allocTbl := sink.table("allocations")
	if allocTbl == nil || len(allocTbl.rows) != 4 {
		t.Fatalf("allocations: want exactly four rows, got %#v", allocTbl)
	}

	seen := map[string]bool{}
	var usedPagesTotal int64
	for _, row := range allocTbl.rows {
		typ, _ := row[2].(string)
		seen[typ] = true
		usedPages, _ := row[3].(int64)
		usedPagesTotal += usedPages
		usedBytes, ok := row[5].(int64)
		if !ok || usedBytes != usedPages*8192 {
			t.Fatalf("used_bytes cell for %s: got %#v, want %d (usedPages*8192)", typ, row[5], usedPages*8192)
		}
		reservedPages, _ := row[4].(int64)
		reservedBytes, ok := row[6].(int64)
		if !ok || reservedBytes != reservedPages*8192 {
			t.Fatalf("reserved_bytes cell for %s: got %#v, want %d (reservedPages*8192)", typ, row[6], reservedPages*8192)
		}
	}
	if !seen["IN_ROW_DATA"] || !seen["LOB_DATA"] || !seen["ROW_OVERFLOW_DATA"] {
		t.Fatalf("allocations must cover all three allocation types encountered, got %v", seen)
	}
	if usedPagesTotal != 10+30+5+7 {
		t.Fatalf("sum of used_pages: got %d, want %d", usedPagesTotal, 10+30+5+7)
	}
}

// TestSizeAllocationsOrderedAcrossIndexes is design spec line 103's
// row-order clause for allocations ("allocation rows use index_id,
// partition_number, allocation type") and dispatch cassure 4's second
// half. The fake driver hands rows back deliberately scrambled -
// size.sql's own real ORDER BY cannot be exercised through a fake
// driver at all, which is exactly why writeAllocations (size.go) sorts
// this bounded result itself rather than trusting the driver's order:
// removing that sort would make this test fail on these exact,
// deliberately out-of-order input rows.
func TestSizeAllocationsOrderedAcrossIndexes(t *testing.T) {
	conn := &fakeObjConn{responses: []objQueryResponse{
		resolveFoundResponse(904, "dbo", "SizePartitioned", "U"),
		tableRowResponse(int64(5), nil),
		sizeRowsResponse([][]driver.Value{
			{int64(1), int64(3), "IN_ROW_DATA", int64(1), int64(1)},
			{int64(1), int64(1), "IN_ROW_DATA", int64(1), int64(1)},
			{int64(1), int64(2), "IN_ROW_DATA", int64(1), int64(1)},
		}),
	}}
	sess := newFakeObjSession(t, conn)
	sink := &objCaptureSink{}

	if err := Size(context.Background(), sess, "dbo.SizePartitioned", sink); err != nil {
		t.Fatalf("Size: %v", err)
	}

	allocTbl := sink.table("allocations")
	if allocTbl == nil || len(allocTbl.rows) != 3 {
		t.Fatalf("allocations: want exactly three rows, got %#v", allocTbl)
	}
	var partitions []int64
	for _, row := range allocTbl.rows {
		p, _ := row[1].(int64)
		partitions = append(partitions, p)
	}
	want := []int64{1, 2, 3}
	for i, p := range partitions {
		if p != want[i] {
			t.Fatalf("allocations partition_number order: got %v, want %v", partitions, want)
		}
	}
}
