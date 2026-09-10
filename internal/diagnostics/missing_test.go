package diagnostics

import (
	"context"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"

	"github.com/rudi-bruchez/argosql/internal/model"
)

// TestMissingUnresolvedTableReturnsEight is dispatch cassure 6's own
// target, the resolution half: --table follows the same
// object-visibility contract as every other object-name flag (design
// spec: "An optional table filter must first resolve through the
// object-visibility contract") - an unresolved --table name fails at
// code 8, never a complete empty result.
func TestMissingUnresolvedTableReturnsEight(t *testing.T) {
	conn := &fakeObjConn{responses: []objQueryResponse{resolveNotFoundResponse()}}
	sess := newFakeObjSession(t, conn)

	err := Missing(context.Background(), sess, MissingOptions{Table: "dbo.Missing", Top: 10}, &objCaptureSink{})
	var pub *model.PublicError
	if !errors.As(err, &pub) {
		t.Fatalf("Missing: want *model.PublicError, got %#v", err)
	}
	if pub.Code != 8 || pub.Kind != "not_found_or_not_visible" {
		t.Fatalf("Missing on an unresolved --table: want code 8/not_found_or_not_visible, got code %d/%s", pub.Code, pub.Kind)
	}
}

// TestMissingRejectsWrongObjectType mirrors every other object-name
// command's own test: design spec line 202, "A resolved object of the
// wrong type gives code 2."
func TestMissingRejectsWrongObjectType(t *testing.T) {
	conn := &fakeObjConn{responses: []objQueryResponse{
		resolveFoundResponse(999, "dbo", "PlainModule", "P"),
	}}
	sess := newFakeObjSession(t, conn)
	sink := &objCaptureSink{}

	err := Missing(context.Background(), sess, MissingOptions{Table: "dbo.PlainModule", Top: 10}, sink)
	var pub *model.PublicError
	if !errors.As(err, &pub) {
		t.Fatalf("Missing: want *model.PublicError, got %#v", err)
	}
	if pub.Code != 2 || pub.Kind != "invalid_argument" {
		t.Fatalf("Missing on a procedure filter: want code 2/invalid_argument, got code %d/%s", pub.Code, pub.Kind)
	}
}

// TestMissingUsesResolvedObjectIDNotRawTableText is dispatch cassure
// 6's other half: @object_id must be the resolved obj.ID, never
// opts.Table's own raw command-line text (design spec: "Resolve
// schema/object names through parameters rather than concatenating
// executable identifiers").
func TestMissingUsesResolvedObjectIDNotRawTableText(t *testing.T) {
	var captured []driver.NamedValue
	conn := &fakeObjConn{responses: []objQueryResponse{
		resolveFoundResponse(4242, "dbo", "Orders", "U"),
		missingRowsResponse(nil, &captured),
	}}
	sess := newFakeObjSession(t, conn)
	sink := &objCaptureSink{}

	if err := Missing(context.Background(), sess, MissingOptions{Table: "dbo.Orders", Top: 10}, sink); err != nil {
		t.Fatalf("Missing: %v", err)
	}

	var objectIDArg driver.NamedValue
	found := false
	for _, a := range captured {
		if a.Name == "object_id" {
			objectIDArg = a
			found = true
		}
	}
	if !found {
		t.Fatalf("Missing: no @object_id argument sent, got args %#v", captured)
	}
	got, ok := objectIDArg.Value.(int64)
	if !ok || got != 4242 {
		t.Fatalf("Missing: @object_id must be the resolved object id 4242, got %#v (raw --table text must never reach this parameter)", objectIDArg.Value)
	}
}

// TestMissingNoTableFilterSendsNullObjectID proves the other half of
// sql/missing.sql's WHERE clause: with no --table, @object_id is SQL
// NULL, never zero or an empty string, matching "(@object_id IS NULL
// OR d.object_id = @object_id)".
func TestMissingNoTableFilterSendsNullObjectID(t *testing.T) {
	var captured []driver.NamedValue
	conn := &fakeObjConn{responses: []objQueryResponse{
		missingRowsResponse(nil, &captured),
	}}
	sess := newFakeObjSession(t, conn)
	sink := &objCaptureSink{}

	if err := Missing(context.Background(), sess, MissingOptions{Top: 10}, sink); err != nil {
		t.Fatalf("Missing: %v", err)
	}
	found := false
	for _, a := range captured {
		if a.Name == "object_id" {
			found = true
			// database/sql converts a driver.Valuer (sql.NullInt64) to
			// its underlying driver.Value before the driver ever sees
			// it, so an absent --table must arrive here as a bare nil,
			// never an int64 (and never the zero-value sentinel 0,
			// which is a real, resolvable object_id on a real server).
			if a.Value != nil {
				t.Fatalf("Missing with no --table: @object_id must be NULL, got %#v", a.Value)
			}
		}
	}
	if !found {
		t.Fatalf("Missing: no @object_id argument sent, got args %#v", captured)
	}
}

// TestMissingCellValuesLeftJoinAndOrder covers real cell values
// (dispatch's own recurring-defect warning: a whole table with no cell
// assertion), the LEFT JOIN's own nullable schema_name/object_name
// (design spec: "Use left joins for optional local names so metadata
// visibility cannot silently remove DMV evidence" - the structural
// half a fake driver can prove; the real regression protection for an
// accidental INNER JOIN is tests/integration's own, against a real
// metadata-visibility difference), and impact_score's exact formula
// and descending order with index_handle as a stable tie-break (design
// spec's declared row order for "suggestions").
func TestMissingCellValuesLeftJoinAndOrder(t *testing.T) {
	conn := &fakeObjConn{responses: []objQueryResponse{
		missingRowsResponse([][]driver.Value{
			{int64(501), int64(701), "dbo", "Orders", "[CustomerId]", nil, "[OrderDate]",
				int64(100), int64(50), float64(20), float64(80), float64(2400)},
			{int64(502), int64(702), nil, nil, "[Email]", nil, nil,
				int64(10), int64(5), float64(5), float64(50), float64(37.5)},
		}, nil),
	}}
	sess := newFakeObjSession(t, conn)
	sink := &objCaptureSink{}

	if err := Missing(context.Background(), sess, MissingOptions{Top: 10}, sink); err != nil {
		t.Fatalf("Missing: %v", err)
	}

	tbl := sink.table("suggestions")
	if tbl == nil || len(tbl.rows) != 2 {
		t.Fatalf("suggestions: want exactly two rows, got %#v", tbl)
	}

	top := tbl.rows[0]
	if top[0] != int64(501) || top[11] != float64(2400) {
		t.Fatalf("top suggestion cells: got index_handle=%#v impact_score=%#v", top[0], top[11])
	}
	if top[2] != "dbo" || top[3] != "Orders" {
		t.Fatalf("visible object cells: got schema=%#v name=%#v", top[2], top[3])
	}

	hidden := tbl.rows[1]
	if hidden[2] != nil || hidden[3] != nil {
		t.Fatalf("LEFT JOIN must keep the row with NULL schema_name/object_name, got %#v %#v", hidden[2], hidden[3])
	}
	if hidden[4] != "[Email]" {
		t.Fatalf("hidden row's own DMV evidence must survive the LEFT JOIN, got equality_columns=%#v", hidden[4])
	}
	// The rest of this row's own DMV evidence, previously unchecked
	// (dispatch B5): inequality_columns is genuinely absent here (nil
	// in the fixture, unlike equality_columns above), included_columns,
	// user_seeks/scans, and the two optimizer-estimate columns the
	// impact_score formula itself consumes.
	if hidden[5] != nil {
		t.Fatalf("hidden row inequality_columns cell: got %#v, want nil", hidden[5])
	}
	if hidden[6] != nil {
		t.Fatalf("hidden row included_columns cell: got %#v, want nil", hidden[6])
	}
	if hidden[1] != int64(702) {
		t.Fatalf("hidden row object_id cell: got %#v, want 702", hidden[1])
	}
	if hidden[7] != int64(10) || hidden[8] != int64(5) {
		t.Fatalf("hidden row user_seeks/user_scans cells: got %#v/%#v, want 10/5", hidden[7], hidden[8])
	}
	if hidden[9] != float64(5) || hidden[10] != float64(50) {
		t.Fatalf("hidden row avg_total_user_cost/avg_user_impact cells: got %#v/%#v, want 5/50", hidden[9], hidden[10])
	}

	// Fix 1's A3: the LEFT JOIN preserves this row's DMV evidence, but
	// its local identification is incomplete - the table's own
	// properties_complete must say so, with a notice naming why,
	// instead of silently declaring everything collected.
	if tbl.propertiesComplete {
		t.Fatalf("a row with NULL schema_name/object_name must make properties_complete=false on the table, got true")
	}
	if n := sink.noticeWithKind("definition_properties_unavailable"); n == nil {
		t.Fatalf("a hidden object's own row must emit a definition_properties_unavailable notice")
	} else if n.Table != SuggestionsTable.Name {
		t.Fatalf("definition_properties_unavailable notice Table: got %q, want %q", n.Table, SuggestionsTable.Name)
	}
}

// TestMissingAllObjectsVisibleIsComplete is
// TestMissingCellValuesLeftJoinAndOrder's other half: when every
// suggestion's referenced object IS visible, properties_complete stays
// true and no hidden-object notice fires - the flag must track the
// real LEFT JOIN outcome in both directions, not default to false.
func TestMissingAllObjectsVisibleIsComplete(t *testing.T) {
	conn := &fakeObjConn{responses: []objQueryResponse{
		missingRowsResponse([][]driver.Value{
			{int64(501), int64(701), "dbo", "Orders", "[CustomerId]", nil, "[OrderDate]",
				int64(100), int64(50), float64(20), float64(80), float64(2400)},
		}, nil),
	}}
	sess := newFakeObjSession(t, conn)
	sink := &objCaptureSink{}

	if err := Missing(context.Background(), sess, MissingOptions{Top: 10}, sink); err != nil {
		t.Fatalf("Missing: %v", err)
	}
	tbl := sink.table("suggestions")
	if !tbl.propertiesComplete {
		t.Fatalf("every object visible: want properties_complete=true, got false")
	}
	if n := sink.noticeWithKind("definition_properties_unavailable"); n != nil {
		t.Fatalf("no hidden object: want no definition_properties_unavailable notice, got %#v", n)
	}
}

// TestMissingNeverEmitsCreateIndexScript is design spec line 56's own
// prohibition ("idx missing": "no CREATE script") - the clause
// dispatch names as the easiest to violate in good faith, because
// equality_columns/inequality_columns/included_columns compose
// naturally into one. No row cell and no Notice this command emits
// ever contains the literal text "CREATE INDEX".
func TestMissingNeverEmitsCreateIndexScript(t *testing.T) {
	conn := &fakeObjConn{responses: []objQueryResponse{
		missingRowsResponse([][]driver.Value{
			{int64(501), int64(701), "dbo", "Orders", "[CustomerId]", "[Status]", "[OrderDate]",
				int64(100), int64(50), float64(20), float64(80), float64(2400)},
		}, nil),
	}}
	sess := newFakeObjSession(t, conn)
	sink := &objCaptureSink{}

	if err := Missing(context.Background(), sess, MissingOptions{Top: 10}, sink); err != nil {
		t.Fatalf("Missing: %v", err)
	}
	tbl := sink.table("suggestions")
	for _, row := range tbl.rows {
		for _, cell := range row {
			if s, ok := cell.(string); ok && strings.Contains(strings.ToUpper(s), "CREATE INDEX") {
				t.Fatalf("Missing must never compose a CREATE INDEX script, got cell %q", s)
			}
		}
	}
	n := sink.noticeWithKind("ranking_score")
	if n == nil || !strings.Contains(n.Message, "CREATE INDEX") {
		t.Fatalf("Missing must disclose, in its own advisory notice, that it never emits a CREATE INDEX script, got %#v", n)
	}
	// Dispatch B6: noticeWithKind only ever filters on Kind; nothing
	// checked Notice.Table before this fix, for any of the three
	// notices this task emits.
	if n.Table != SuggestionsTable.Name {
		t.Fatalf("ranking_score notice Table: got %q, want %q", n.Table, SuggestionsTable.Name)
	}

	// Dispatch B5: equality_columns/inequality_columns/included_columns
	// are exactly the columns "idx missing" is forbidden to compose
	// into a script - their own escaped content, not merely its
	// absence of "CREATE INDEX", deserves an assertion.
	row := tbl.rows[0]
	if row[4] != "[CustomerId]" {
		t.Fatalf("equality_columns cell: got %#v, want %q", row[4], "[CustomerId]")
	}
	if row[5] != "[Status]" {
		t.Fatalf("inequality_columns cell: got %#v, want %q", row[5], "[Status]")
	}
	if row[6] != "[OrderDate]" {
		t.Fatalf("included_columns cell: got %#v, want %q", row[6], "[OrderDate]")
	}
}

// TestSuggestionsTableColumnCount guards against silently dropping one
// of SuggestionsTable's twelve declared columns.
func TestSuggestionsTableColumnCount(t *testing.T) {
	if len(SuggestionsTable.Columns) != 12 {
		t.Fatalf("SuggestionsTable: want 12 columns, got %d: %#v", len(SuggestionsTable.Columns), SuggestionsTable.Columns)
	}
}

// TestDeclaredTableNames is dispatch cassure 7's own target: the
// design spec names these three tables explicitly ("idx usage: usage";
// "idx missing: suggestions"; "stats list: statistics"), and every
// other test in this task's three files only ever finds its own rows
// by calling sink.table with one of these literals - a silent rename
// would make every one of them report "no such table" instead of
// naming the real defect, which is why this test asserts the names
// directly, once, here.
func TestDeclaredTableNames(t *testing.T) {
	if UsageTable.Name != "usage" {
		t.Fatalf("UsageTable.Name: got %q, want %q", UsageTable.Name, "usage")
	}
	if SuggestionsTable.Name != "suggestions" {
		t.Fatalf("SuggestionsTable.Name: got %q, want %q", SuggestionsTable.Name, "suggestions")
	}
	if StatisticsTable.Name != "statistics" {
		t.Fatalf("StatisticsTable.Name: got %q, want %q", StatisticsTable.Name, "statistics")
	}
}
