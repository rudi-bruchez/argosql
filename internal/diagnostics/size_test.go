package diagnostics

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/rudi-bruchez/argosql/internal/model"
	"github.com/rudi-bruchez/argosql/internal/sqlserver"

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

// TestSizeRejectsWrongObjectType is task 13 fix-1's own A0 target for
// "size table": design spec line 202, "A resolved object of the wrong
// type gives code 2" - measured before this fix: size table on a
// procedure resolved and then failed at code 4
// (memory_optimized_unavailable or size_unavailable, neither of which
// actually applied) instead of naming the type mismatch.
func TestSizeRejectsWrongObjectType(t *testing.T) {
	conn := &fakeObjConn{responses: []objQueryResponse{
		resolveFoundResponse(999, "dbo", "PlainModule", "P"),
	}}
	sess := newFakeObjSession(t, conn)
	sink := &objCaptureSink{}

	err := Size(context.Background(), sess, "dbo.PlainModule", sink)
	var pub *model.PublicError
	if !errors.As(err, &pub) {
		t.Fatalf("Size: want *model.PublicError, got %#v", err)
	}
	if pub.Code != 2 || pub.Kind != "invalid_argument" {
		t.Fatalf("Size on a procedure: want code 2/invalid_argument, got code %d/%s", pub.Code, pub.Kind)
	}
	if len(sink.tables) != 0 {
		t.Fatalf("wrong object type: want no table written, got %#v", sink.tables)
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
		tableRowResponse(nil, nil, nil, true),
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

// TestSizeNotApplicableReturnsFour is fix-2/A7's own target: the third
// of rowCountUnavailableReason's four values, rowCountReasonNotApplicable
// (an object with no sys.dm_db_partition_stats row at all, neither
// memory-optimized nor permission-denied), had no test at all before
// this - a reviewer measured that renaming its Kind, or defaulting to
// it from a genuine permission denial, passed silently. Its own Kind
// ("size_unavailable") must be distinct from the other two
// (memory_optimized_unavailable, permission) - design spec: "without
// inventing its cause", the same discipline obj code's definitionState
// vocabulary already gets.
func TestSizeNotApplicableReturnsFour(t *testing.T) {
	conn := &fakeObjConn{responses: []objQueryResponse{
		resolveFoundResponse(906, "dbo", "Orders", "U"),
		tableRowResponse(nil, nil, nil, false),
	}}
	sess := newFakeObjSession(t, conn)
	sink := &objCaptureSink{}

	err := Size(context.Background(), sess, "dbo.Orders", sink)
	var pub *model.PublicError
	if !errors.As(err, &pub) {
		t.Fatalf("Size: want *model.PublicError, got %#v", err)
	}
	if pub.Code != 4 || pub.Kind != "size_unavailable" {
		t.Fatalf("Size with no partition-stats row: want code 4/size_unavailable, got code %d/%s", pub.Code, pub.Kind)
	}
	if pub.Kind == "memory_optimized_unavailable" || pub.Kind == "permission" {
		t.Fatalf("size_unavailable must not collapse into either sibling Kind, got %q", pub.Kind)
	}
}

// TestRowCountUnavailableMessageDistinctPerReason is fix-2/A7's other
// half: a reviewer measured that freezing rowCountUnavailableMessage's
// switch to one constant string passed every existing test, because
// nothing compared the three reasons' messages against EACH OTHER,
// only against a fixed expectation each in isolation (which a shared
// constant would also satisfy if all three tests happened to want the
// same literal - they don't here, but the isolation itself was the
// gap). The three messages must be pairwise distinct.
func TestRowCountUnavailableMessageDistinctPerReason(t *testing.T) {
	obj := sqlserver.Object{Schema: "dbo", Name: "Orders"}
	permission := rowCountUnavailableMessage(obj, rowCountReasonPermission)
	memOpt := rowCountUnavailableMessage(obj, rowCountReasonMemoryOptimized)
	notApplicable := rowCountUnavailableMessage(obj, rowCountReasonNotApplicable)

	if permission == memOpt || permission == notApplicable || memOpt == notApplicable {
		t.Fatalf("rowCountUnavailableMessage must render three distinct texts, got %q / %q / %q", permission, memOpt, notApplicable)
	}
}

// TestTableHeaderRowCountReachesNotApplicable is fix-2/A7's proof that
// rowCountReasonNotApplicable is reachable at all, even though task
// 13 fix-1's own type gate (tableAllowedTypes) means no real 'U' table
// can hit it through Table/Size any more in practice (every disk-based
// table always has at least one sys.dm_db_partition_stats row).
// "Inatteignable par l'interface" is not accepted on this project as a
// reason to leave a vocabulary branch untested - this calls
// tableHeaderRowCount directly, bypassing Table/Size, with a fake row
// that has no row_count and is not memory-optimized: the one shape
// that still reaches this branch.
func TestTableHeaderRowCountReachesNotApplicable(t *testing.T) {
	conn := &fakeObjConn{responses: []objQueryResponse{
		tableRowResponse(nil, nil, nil, false),
	}}
	sess := newFakeObjSession(t, conn)
	obj := sqlserver.Object{ID: 1, Schema: "dbo", Name: "Orders", Type: "U"}

	_, reason, err := tableHeaderRowCount(context.Background(), sess, obj)
	if err != nil {
		t.Fatalf("tableHeaderRowCount: %v", err)
	}
	if reason != rowCountReasonNotApplicable {
		t.Fatalf("reason: got %q, want %q", reason, rowCountReasonNotApplicable)
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
		tableRowResponse(int64(50), int64(52), int64(57), nil),
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

// TestSizeAllocationsPreservesDriverOrder is design spec line 103's
// row-order clause for allocations ("allocation rows use index_id,
// partition_number, allocation type"), re-targeted by task 13 fix-1:
// writeAllocations no longer accumulates and sorts (see
// TestSizeStreamsWithoutAccumulating below for why), so ordering is
// now entirely size.sql's own ORDER BY's responsibility, unreachable
// through this fake driver. What IS still this function's own job,
// and what this test proves, is that it never reorders what the
// driver hands it: fed rows already in the order a real ORDER BY
// would produce, Size must write them in that same order, unchanged.
// tests/integration's own TestSize/partitioned checks the real
// ORDER BY against a genuinely multi-partition table.
func TestSizeAllocationsPreservesDriverOrder(t *testing.T) {
	conn := &fakeObjConn{responses: []objQueryResponse{
		resolveFoundResponse(904, "dbo", "SizePartitioned", "U"),
		tableRowResponse(int64(5), int64(3), int64(3), nil),
		sizeRowsResponse([][]driver.Value{
			{int64(1), int64(1), "IN_ROW_DATA", int64(1), int64(1)},
			{int64(1), int64(2), "IN_ROW_DATA", int64(1), int64(1)},
			{int64(1), int64(3), "IN_ROW_DATA", int64(1), int64(1)},
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

// TestSizeStreamsWithoutAccumulating is task 13 fix-1's own A2 target:
// the reviewer measured that writeAllocations used to read every
// allocation row into a slice and sort it before the first dst.Row
// call, with nothing bounding how many rows that could be - design
// spec line 111 requires streaming, and this is the third time this
// exact pattern has been found on this project (after queryRows, task
// 11). Proof: a Sink that refuses the FIRST allocations row must stop
// the driver's own Rows.Next() at row 1, never advance through all
// three - which only holds if writeAllocations calls dst.Row as it
// scans, rather than buffering first.
func TestSizeStreamsWithoutAccumulating(t *testing.T) {
	rows := &fakeStaticRows{
		cols:  []string{"index_id", "partition_number", "allocation_type", "used_pages", "reserved_pages"},
		types: []string{"INT", "INT", "NVARCHAR", "BIGINT", "BIGINT"},
		data: [][]driver.Value{
			{int64(1), int64(1), "IN_ROW_DATA", int64(1), int64(1)},
			{int64(2), int64(1), "IN_ROW_DATA", int64(1), int64(1)},
			{int64(3), int64(1), "IN_ROW_DATA", int64(1), int64(1)},
		},
	}
	conn := &fakeObjConn{responses: []objQueryResponse{
		resolveFoundResponse(905, "dbo", "SizeFixture", "U"),
		tableRowResponse(int64(5), int64(3), int64(3), nil),
		{
			match:  func(q string) bool { return strings.Contains(q, "in_row_used_page_count") },
			handle: func(args []driver.NamedValue) (driver.Rows, error) { return rows, nil },
		},
	}}
	sess := newFakeObjSession(t, conn)
	sink := &refusingAllocationsSink{}

	err := Size(context.Background(), sess, "dbo.SizeFixture", sink)
	if err == nil {
		t.Fatal("Size: want the sink's refusal to propagate as an error")
	}
	if rows.idx != 1 {
		t.Fatalf("driver rows advanced: got %d, want exactly 1 (streaming, no accumulation before the sink refused row 1)", rows.idx)
	}
}

// refusingAllocationsSink refuses the first "allocations" row -
// TestSizeStreamsWithoutAccumulating's own regression proof.
type refusingAllocationsSink struct {
	objCaptureSink
	refused bool
}

func (s *refusingAllocationsSink) Row(row []model.Cell) error {
	if s.cur != nil && s.cur.spec.Name == AllocationsTable.Name && !s.refused {
		s.refused = true
		return fmt.Errorf("refusingAllocationsSink: refused the first allocations row")
	}
	return s.objCaptureSink.Row(row)
}
