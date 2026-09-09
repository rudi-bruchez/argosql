//go:build integration

package integration

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/rudi-bruchez/argosql/internal/cli"
	"github.com/rudi-bruchez/argosql/internal/diagnostics"
	"github.com/rudi-bruchez/argosql/internal/model"
	"github.com/rudi-bruchez/argosql/internal/sqlserver"
)

// TestEncryptedModule is task 13's own brief, verbatim: "obj code" on
// a WITH ENCRYPTION module (dbo.EncryptedProc, objects.sql) must fail
// at code 4 with Kind "encrypted".
func TestEncryptedModule(t *testing.T) {
	lab := NewLab(t, os.Getenv("ASQ_TEST_IMAGE"))
	r, code := lab.Run(t, "I", []string{"obj", "code", "dbo.EncryptedProc"})
	if code != 4 || r.Error == nil || r.Error.Kind != "encrypted" {
		t.Fatalf("%d %#v", code, r.Error)
	}
}

// TestObjects exercises the design spec's own Q/I/S permission matrix
// for obj table, obj code, idx list and size table (design spec:
// "obj table | 8, not_found_or_not_visible | 0 | 0", and identically
// for obj code/size table/idx list): Q, which holds no metadata
// visibility on the fixture schema at all, gets code 8 on every one
// of these four commands; I and S, which hold VIEW DEFINITION and
// SELECT on dbo, get code 0 on all four. obj code uses
// dbo.PlainModule rather than dbo.Orders: a table is not a module, and
// this fixture proc is neither encrypted nor definition-denied, so it
// reaches definitionStateAvailable for I/S exactly as the matrix's
// "0" expects.
func TestObjects(t *testing.T) {
	lab := NewLab(t, os.Getenv("ASQ_TEST_IMAGE"))

	type invocation struct {
		name string
		args []string
	}
	invocations := []invocation{
		{"obj table", []string{"obj", "table", "dbo.Orders"}},
		{"obj code", []string{"obj", "code", "dbo.PlainModule"}},
		{"idx list", []string{"idx", "list", "dbo.Orders"}},
		{"size table", []string{"size", "table", "dbo.Orders"}},
	}

	for _, inv := range invocations {
		t.Run(inv.name+"/Q is unresolved", func(t *testing.T) {
			r, code := lab.Run(t, "Q", inv.args)
			if code != 8 || r.Error == nil || r.Error.Kind != "not_found_or_not_visible" {
				t.Fatalf("%s as Q: got code %d, error %#v; want 8/not_found_or_not_visible", inv.name, code, r.Error)
			}
		})
		t.Run(inv.name+"/I succeeds", func(t *testing.T) {
			r, code := lab.Run(t, "I", inv.args)
			if code != 0 {
				t.Fatalf("%s as I: got code %d, error %#v; want 0", inv.name, code, r.Error)
			}
		})
		t.Run(inv.name+"/S succeeds", func(t *testing.T) {
			r, code := lab.Run(t, "S", inv.args)
			if code != 0 {
				t.Fatalf("%s as S: got code %d, error %#v; want 0", inv.name, code, r.Error)
			}
		})
	}

	// Measured (fix-0), and NOT confused with the fixture below:
	// dbo.DeniedDefinitionProc (GRANT EXECUTE, DENY VIEW DEFINITION on
	// the SAME object) is not_found_or_not_visible to I/S, code 8 -
	// DENY VIEW DEFINITION removes the object's sys.objects row itself,
	// not merely its definition text. That is a different question
	// from the one design spec line 204 actually asks - see
	// TestObjCodePermissionDenied, which asks the right one.
}

// TestObjCodePermissionDenied is design spec line 204's own confirmed
// permission_denied fixture, fix-0's first point: "denied" there does
// not require an explicit DENY - the ABSENCE of a VIEW DEFINITION
// grant already makes the permission missing. Q holds no VIEW
// DEFINITION anywhere (schema- or database-wide); principals.sql
// grants it EXECUTE alone, with no DENY at all, on
// dbo.ExecuteOnlyProc. Measured directly here, not merely inferred
// from the exit code: HAS_PERMS_BY_NAME is two-state on this engine
// (0 for an absent permission and for an invisible object alike - see
// CLAUDE.md), so the object's actual presence in sys.objects is
// asserted explicitly, via a successful sqlserver.Resolve, rather than
// trusted to the VIEW DEFINITION probe alone.
func TestObjCodePermissionDenied(t *testing.T) {
	lab := NewLab(t, os.Getenv("ASQ_TEST_IMAGE"))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pw := lab.ensurePrincipals(t)
	qProfile := principalProfile(lab, "asq_test_q", pw.Q)
	sess, err := sqlserver.Open(ctx, qProfile)
	if err != nil {
		t.Fatalf("open as Q: %v", err)
	}
	defer sess.Close()

	obj, err := sqlserver.Resolve(ctx, sess.Conn, "dbo.ExecuteOnlyProc")
	if err != nil {
		t.Fatalf("dbo.ExecuteOnlyProc must be visible to Q (EXECUTE only, no DENY): %v", err)
	}
	if obj.Schema != "dbo" || obj.Name != "ExecuteOnlyProc" {
		t.Fatalf("Resolve: got %+v", obj)
	}

	perm, err := sqlserver.Probe(ctx, sess.Conn, "dbo.ExecuteOnlyProc", "OBJECT", "VIEW DEFINITION")
	if err != nil {
		t.Fatalf("VIEW DEFINITION probe: %v", err)
	}
	if perm != sqlserver.Denied {
		t.Fatalf("VIEW DEFINITION probe for Q on dbo.ExecuteOnlyProc: got %v, want Denied", perm)
	}

	r, code := lab.Run(t, "Q", []string{"obj", "code", "dbo.ExecuteOnlyProc"})
	if code != 4 || r.Error == nil || r.Error.Kind != "permission_denied" {
		t.Fatalf("obj code dbo.ExecuteOnlyProc as Q: got code %d, error %#v; want 4/permission_denied", code, r.Error)
	}
}

// TestObjCodeOnNonModuleObject is task 13 fix-1's own A0 target:
// design spec line 202, "A resolved object of the wrong type gives
// code 2", settled BEFORE moduleQuery ever runs. This supersedes
// fix-0's own decision (code 4, definition_unavailable) - itself a
// correction of an even earlier code 5 (unexpectedCell, because
// OBJECTPROPERTYEX(object_id,'IsEncrypted') reads NULL for a table).
// Both were wrong: this is an argument error, never a
// definition-state question.
func TestObjCodeOnNonModuleObject(t *testing.T) {
	lab := NewLab(t, os.Getenv("ASQ_TEST_IMAGE"))
	r, code := lab.Run(t, "I", []string{"obj", "code", "dbo.Orders"})
	if code != 2 || r.Error == nil || r.Error.Kind != "invalid_argument" {
		t.Fatalf("obj code dbo.Orders (a table) as I: got code %d, error %#v; want 2/invalid_argument", code, r.Error)
	}
}

// TestWrongObjectTypeRejected is task 13 fix-1's own A0 target for the
// other three commands, the SIXTH lost clause the first pre-flight
// missed: design spec line 202, "A resolved object of the wrong type
// gives code 2". Measured by the first reviewer, before this fix, on
// the visible procedure dbo.PlainModule: obj table succeeded claiming
// it was a table with empty columns/indexes, idx list succeeded with
// an empty result declared complete, and size table failed at code 4
// (a reason that did not actually apply). All three must reject at
// code 2 instead.
func TestWrongObjectTypeRejected(t *testing.T) {
	lab := NewLab(t, os.Getenv("ASQ_TEST_IMAGE"))

	for _, args := range [][]string{
		{"obj", "table", "dbo.PlainModule"},
		{"idx", "list", "dbo.PlainModule"},
		{"size", "table", "dbo.PlainModule"},
	} {
		t.Run(args[0]+" "+args[1], func(t *testing.T) {
			r, code := lab.Run(t, "I", args)
			if code != 2 || r.Error == nil || r.Error.Kind != "invalid_argument" {
				t.Fatalf("%v on dbo.PlainModule (a procedure): got code %d, error %#v; want 2/invalid_argument", args, code, r.Error)
			}
		})
	}
}

// TestColumnsAndIndexesPropertiesMaskedBySelectOnly is task 13 fix-1's
// own A1 target: a principal with SELECT alone (no VIEW DEFINITION)
// on dbo.ColumnsFixture sees full column/index structure, but
// default_definition, computed_definition and filter all come back
// NULL - measured by the reviewer to be indistinguishable, before
// this fix, from those properties genuinely not existing, with
// properties_complete=true and no warning either way. Granting Q
// (which the shared fixture never gives object-level grants) SELECT
// on this one table, ad hoc, isolates exactly this scenario without
// touching the I/S bundles every other test in this package depends
// on.
func TestColumnsAndIndexesPropertiesMaskedBySelectOnly(t *testing.T) {
	lab := NewLab(t, os.Getenv("ASQ_TEST_IMAGE"))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	lab.ensurePrincipals(t) // creates asq_test_q and applies objects.sql
	if _, err := lab.Admin.ExecContext(ctx, "GRANT SELECT ON dbo.ColumnsFixture TO asq_test_q;"); err != nil {
		t.Fatalf("granting SELECT to Q: %v", err)
	}

	r, code := lab.Run(t, "Q", []string{"obj", "table", "dbo.ColumnsFixture"})
	if code != 0 {
		t.Fatalf("obj table dbo.ColumnsFixture as Q (SELECT only): got code %d, error %#v; want 0", code, r.Error)
	}

	var columns, indexes *model.TableResult
	for i := range r.Tables {
		switch r.Tables[i].Spec.Name {
		case "columns":
			columns = &r.Tables[i]
		case "indexes":
			indexes = &r.Tables[i]
		}
	}
	if columns == nil || indexes == nil {
		t.Fatalf("expected both columns and indexes tables, got %#v", r.Tables)
	}
	if columns.State.PropertiesComplete {
		t.Fatal("columns.properties_complete: want false when VIEW DEFINITION is denied (SELECT alone) - defaults/computed formulas may be masked")
	}
	if indexes.State.PropertiesComplete {
		t.Fatal("indexes.properties_complete: want false too - filter may be masked, not genuinely absent")
	}
	// The row/column shape must still be fully populated: masking hides
	// definition TEXT only, never structural metadata SELECT already
	// grants visibility into.
	if len(columns.Rows) == 0 || len(indexes.Rows) == 0 {
		t.Fatalf("SELECT alone must still see full column/index structure, got columns=%d indexes=%d rows", len(columns.Rows), len(indexes.Rows))
	}
}

// TestObjTableSizeTableSurviveMissingViewDatabaseState is task 13
// fix-1's own A1 target: the shared classifier used to recognize only
// SQL error numbers 229 and 300 as permission denials, not 297 - the
// number sys.dm_db_partition_stats actually raises. Measured by the
// reviewer, and confirmed again while building this fixture: on 2022,
// VIEW DATABASE STATE alone is NOT what gates this DMV for a
// principal that also already holds VIEW DATABASE PERFORMANCE STATE
// and VIEW SECURITY DEFINITION (I's own 2022 bundle) - revoking only
// the first left the DMV fully readable, silently testing nothing.
// All three of I's state-viewing grants that this major version
// actually has must be revoked together to reproduce the reviewer's
// own "I conservant ses droits sur les objets mais privé des
// permissions d'état." I is used here, with its own state grants
// revoked on top of its object-level ones, rather than a fresh
// principal: the fixture's Q cannot resolve dbo.Orders at all (no
// VIEW DEFINITION), which would test the wrong thing entirely.
func TestObjTableSizeTableSurviveMissingViewDatabaseState(t *testing.T) {
	lab := NewLab(t, os.Getenv("ASQ_TEST_IMAGE"))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	lab.ensurePrincipals(t)
	var major int
	if err := lab.Admin.QueryRowContext(ctx, "SELECT CAST(SERVERPROPERTY('ProductMajorVersion') AS int)").Scan(&major); err != nil {
		t.Fatalf("reading engine major version: %v", err)
	}
	revoke := "REVOKE VIEW DATABASE STATE FROM asq_test_i;"
	grantBack := "GRANT VIEW DATABASE STATE TO asq_test_i;"
	if major >= 16 {
		revoke += " REVOKE VIEW DATABASE PERFORMANCE STATE FROM asq_test_i; REVOKE VIEW SECURITY DEFINITION FROM asq_test_i;"
		grantBack += " GRANT VIEW DATABASE PERFORMANCE STATE TO asq_test_i; GRANT VIEW SECURITY DEFINITION TO asq_test_i;"
	}
	if _, err := lab.Admin.ExecContext(ctx, revoke); err != nil {
		t.Fatalf("revoking state permissions from I: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		lab.Admin.ExecContext(cleanupCtx, grantBack)
	})

	r, code := lab.Run(t, "I", []string{"obj", "table", "dbo.Orders"})
	if code != 0 || r.Error != nil {
		t.Fatalf("obj table dbo.Orders as I (no VIEW DATABASE STATE): got code %d, error %#v; want 0", code, r.Error)
	}
	var tableRow *model.TableResult
	for i := range r.Tables {
		if r.Tables[i].Spec.Name == "table" {
			tableRow = &r.Tables[i]
		}
	}
	if tableRow == nil || len(tableRow.Rows) != 1 {
		t.Fatalf("table row missing: %#v", r.Tables)
	}
	if tableRow.Rows[0][3] != nil {
		t.Fatalf("rows cell: got %#v, want nil (size permissions absent)", tableRow.Rows[0][3])
	}
	if tableRow.State.PropertiesComplete {
		t.Fatal("table.properties_complete: want false when size permissions are absent")
	}

	r2, code2 := lab.Run(t, "I", []string{"size", "table", "dbo.Orders"})
	if code2 != 4 || r2.Error == nil || r2.Error.Kind != "permission" {
		t.Fatalf("size table dbo.Orders as I (no VIEW DATABASE STATE): got code %d, error %#v; want 4/permission", code2, r2.Error)
	}
}

// TestIdxListIgnoresPartitioningColumn is task 13 fix-1's own A1
// target: a nonclustered index created on a partition scheme gets an
// implicitly added partitioning column with key_ordinal=0 and
// is_included_column=0 - neither a declared key nor an included
// column. Measured by the reviewer: before this fix, idx list
// reported it as the index's own FIRST key, ahead of the one column
// actually declared.
func TestIdxListIgnoresPartitioningColumn(t *testing.T) {
	lab := NewLab(t, os.Getenv("ASQ_TEST_IMAGE"))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := applyObjects(ctx, lab.Admin); err != nil {
		t.Fatalf("applying objects.sql: %v", err)
	}
	if _, err := lab.Admin.ExecContext(ctx,
		"CREATE INDEX IX_ReviewPartition ON dbo.SizePartitioned(Value) ON AsqSizePS(Bucket);"); err != nil {
		t.Fatalf("creating partitioned index: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		lab.Admin.ExecContext(cleanupCtx, "DROP INDEX IX_ReviewPartition ON dbo.SizePartitioned;")
	})

	sess, err := sqlserver.Open(ctx, lab.Profile)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer sess.Close()

	sink := &captureSink{}
	if err := diagnostics.Indexes(ctx, sess, "dbo.SizePartitioned", sink); err != nil {
		t.Fatalf("Indexes: %v", err)
	}
	tbl := sink.table("indexes")
	if tbl == nil {
		t.Fatal("indexes table missing")
	}
	found := false
	for _, row := range tbl.rows {
		name, _ := row[1].(string)
		if name != "IX_ReviewPartition" {
			continue
		}
		found = true
		if row[3] != "[Value] ASC" {
			t.Fatalf("IX_ReviewPartition keys cell: got %#v, want %q (the partitioning column Bucket must not appear)", row[3], "[Value] ASC")
		}
	}
	if !found {
		t.Fatal("IX_ReviewPartition not found in idx list output")
	}
}

// TestSizeTableExposesTotals is task 13 fix-1's own A2 target: size
// table used to expose only the per-row allocation breakdown, never
// the used/reserved/unused totals design spec lines 53/174 both
// require. Compared against an independent SUM over the same DMV,
// bypassing production's own unpivot entirely.
func TestSizeTableExposesTotals(t *testing.T) {
	lab := NewLab(t, os.Getenv("ASQ_TEST_IMAGE"))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := applyObjects(ctx, lab.Admin); err != nil {
		t.Fatalf("applying objects.sql: %v", err)
	}

	var objectID, wantUsed, wantReserved int64
	if err := lab.Admin.QueryRowContext(ctx, "SELECT OBJECT_ID(N'AppDB.dbo.SizeFixture')").Scan(&objectID); err != nil {
		t.Fatalf("resolving object_id: %v", err)
	}
	if err := lab.Admin.QueryRowContext(ctx,
		"SELECT SUM(used_page_count), SUM(reserved_page_count) FROM sys.dm_db_partition_stats WHERE object_id=@p1", objectID,
	).Scan(&wantUsed, &wantReserved); err != nil {
		t.Fatalf("independent totals: %v", err)
	}

	sess, err := sqlserver.Open(ctx, lab.Profile)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer sess.Close()

	sink := &captureSink{}
	if err := diagnostics.Size(ctx, sess, "dbo.SizeFixture", sink); err != nil {
		t.Fatalf("Size: %v", err)
	}
	tbl := sink.table("table")
	if tbl == nil || len(tbl.rows) != 1 {
		t.Fatalf("table row missing: %#v", tbl)
	}
	wantUsedBytes := wantUsed * 8192
	wantReservedBytes := wantReserved * 8192
	wantUnusedBytes := wantReservedBytes - wantUsedBytes
	if got := tbl.rows[0][4]; got != wantUsedBytes {
		t.Fatalf("total_used_bytes cell: got %#v, want %d", got, wantUsedBytes)
	}
	if got := tbl.rows[0][5]; got != wantReservedBytes {
		t.Fatalf("total_reserved_bytes cell: got %#v, want %d", got, wantReservedBytes)
	}
	if got := tbl.rows[0][6]; got != wantUnusedBytes {
		t.Fatalf("total_unused_bytes cell: got %#v, want %d", got, wantUnusedBytes)
	}
}

// TestIdxListHandlesWideIndex is task 13 fix-1's own A2 target: each
// STRING_AGG in indexes.sql used to aggregate an NVARCHAR expression
// of bounded length, capping its result at 4,000 characters -
// measured by the reviewer with a 32-column index whose column names
// are 123 characters each. This builds the same shape: enough key
// columns, with long enough names, to exceed 4,000 characters in the
// unfixed query.
func TestIdxListHandlesWideIndex(t *testing.T) {
	lab := NewLab(t, os.Getenv("ASQ_TEST_IMAGE"))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	const columnCount = 32
	const nameWidth = 123

	var createCols, indexCols strings.Builder
	for i := 0; i < columnCount; i++ {
		name := fmt.Sprintf("C%0*d", nameWidth-1, i)
		if i > 0 {
			createCols.WriteString(", ")
			indexCols.WriteString(", ")
		}
		fmt.Fprintf(&createCols, "[%s] INT NOT NULL", name)
		fmt.Fprintf(&indexCols, "[%s]", name)
	}

	if _, err := lab.Admin.ExecContext(ctx, "IF OBJECT_ID(N'AppDB.dbo.WideIndexFixture') IS NOT NULL DROP TABLE dbo.WideIndexFixture;"); err != nil {
		t.Fatalf("dropping WideIndexFixture: %v", err)
	}
	if _, err := lab.Admin.ExecContext(ctx, fmt.Sprintf("CREATE TABLE dbo.WideIndexFixture (%s);", createCols.String())); err != nil {
		t.Fatalf("creating WideIndexFixture: %v", err)
	}
	if _, err := lab.Admin.ExecContext(ctx, fmt.Sprintf("CREATE INDEX IX_WideIndexFixture ON dbo.WideIndexFixture (%s);", indexCols.String())); err != nil {
		t.Fatalf("creating wide index: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		lab.Admin.ExecContext(cleanupCtx, "DROP TABLE dbo.WideIndexFixture;")
	})

	sess, err := sqlserver.Open(ctx, lab.Profile)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer sess.Close()

	sink := &captureSink{}
	if err := diagnostics.Indexes(ctx, sess, "dbo.WideIndexFixture", sink); err != nil {
		t.Fatalf("Indexes: %v", err)
	}
	tbl := sink.table("indexes")
	if tbl == nil || len(tbl.rows) != 1 {
		t.Fatalf("indexes table: want exactly one row, got %#v", tbl)
	}
	keys, _ := tbl.rows[0][3].(string)
	if len(keys) <= 4000 {
		t.Fatalf("this fixture's own keys text (%d chars) does not exceed the old 4000-char STRING_AGG cap - fixture too small to prove the fix", len(keys))
	}
	wantFirst := fmt.Sprintf("[C%0*d]", nameWidth-1, 0)
	if !strings.HasPrefix(keys, wantFirst) {
		t.Fatalf("keys cell does not start with the first declared key column: got prefix %q, want %q", keys[:min(len(keys), len(wantFirst))], wantFirst)
	}
}

// TestSize proves size table's own extension and ordering
// requirements against a real engine, one fixture per subtest, each
// asserting real cell VALUES rather than only a row count - the
// recurring defect this project has paid for on other tasks.
func TestSize(t *testing.T) {
	lab := NewLab(t, os.Getenv("ASQ_TEST_IMAGE"))
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// objects.sql (Size*/ColumnsFixture included) is only ever applied
	// lazily by lab.ensurePrincipals, which lab.Run triggers - this
	// test calls diagnostics functions directly instead (as
	// top_test.go/query_test.go already do for their own admin-level
	// checks), so it must apply the fixture itself. Missing this call
	// once already produced a false "object not found" failure here
	// rather than a real defect - measured, not assumed.
	if err := applyObjects(ctx, lab.Admin); err != nil {
		t.Fatalf("applying objects.sql: %v", err)
	}

	sess, err := sqlserver.Open(ctx, lab.Profile)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer sess.Close()

	// independentPartitionStatsTotals reads sys.dm_db_partition_stats
	// directly, bypassing diagnostics.Size entirely, so the assertions
	// below compare Size's own output against a SECOND, independent
	// read of the same DMV rather than against a hand-computed
	// constant this test's author might get wrong the same way a
	// production defect would.
	independentTotals := func(t *testing.T, qualifiedName string) (rowCount, usedPages int64) {
		t.Helper()
		var objectID int64
		if err := lab.Admin.QueryRowContext(ctx, "SELECT OBJECT_ID(@p1)", qualifiedName).Scan(&objectID); err != nil {
			t.Fatalf("resolving object_id for %s: %v", qualifiedName, err)
		}
		row := lab.Admin.QueryRowContext(ctx,
			"SELECT SUM(CASE WHEN index_id IN (0,1) THEN row_count ELSE 0 END), SUM(used_page_count) "+
				"FROM sys.dm_db_partition_stats WHERE object_id = @p1", objectID)
		if err := row.Scan(&rowCount, &usedPages); err != nil {
			t.Fatalf("independent partition-stats totals for %s: %v", qualifiedName, err)
		}
		return rowCount, usedPages
	}

	sumUsedPages := func(rows [][]model.Cell) int64 {
		var total int64
		for _, r := range rows {
			p, _ := r[3].(int64)
			total += p
		}
		return total
	}
	sumUsedBytes := func(rows [][]model.Cell) int64 {
		var total int64
		for _, r := range rows {
			b, _ := r[5].(int64)
			total += b
		}
		return total
	}
	allocationTypesSeen := func(rows [][]model.Cell) map[string]bool {
		seen := map[string]bool{}
		for _, r := range rows {
			typ, _ := r[2].(string)
			seen[typ] = true
		}
		return seen
	}

	t.Run("heap", func(t *testing.T) {
		wantRows, wantUsed := independentTotals(t, "AppDB.dbo.SizeHeap")
		sink := &captureSink{}
		if err := diagnostics.Size(ctx, sess, "dbo.SizeHeap", sink); err != nil {
			t.Fatalf("Size: %v", err)
		}
		tbl := sink.table("table")
		if tbl == nil || tbl.rows[0][3] != wantRows {
			t.Fatalf("row_count cell: got %#v, want %d", tbl, wantRows)
		}
		alloc := sink.table("allocations")
		if alloc == nil || len(alloc.rows) == 0 {
			t.Fatalf("allocations: want at least one row for a heap, got %#v", alloc)
		}
		if got := sumUsedPages(alloc.rows); got != wantUsed {
			t.Fatalf("sum(used_pages): got %d, want %d", got, wantUsed)
		}
		// fix-2/A4: presence alone ("IN_ROW_DATA is there") does not
		// catch the zero-page categories the three filters in size.sql
		// exist to suppress - a reviewer removed all three and every
		// partition started reporting LOB_DATA/ROW_OVERFLOW_DATA rows
		// at zero pages, which this table's own comment declares must
		// never happen, and this assertion (checked only for presence)
		// stayed green. Named exhaustively: a heap with no LOB/overflow
		// column must report IN_ROW_DATA and NOTHING else.
		if seen := allocationTypesSeen(alloc.rows); len(seen) != 1 || !seen["IN_ROW_DATA"] {
			t.Fatalf("a heap with no LOB/overflow column must report exactly {IN_ROW_DATA}, got %v", seen)
		}
	})

	t.Run("clustered plus two nonclustered with LOB and overflow", func(t *testing.T) {
		wantRows, wantUsed := independentTotals(t, "AppDB.dbo.SizeFixture")
		sink := &captureSink{}
		if err := diagnostics.Size(ctx, sess, "dbo.SizeFixture", sink); err != nil {
			t.Fatalf("Size: %v", err)
		}
		tbl := sink.table("table")
		if tbl == nil || tbl.rows[0][3] != wantRows {
			t.Fatalf("row_count cell: got %#v, want %d", tbl, wantRows)
		}
		alloc := sink.table("allocations")
		if alloc == nil {
			t.Fatal("allocations table missing")
		}
		// Brief's own extension assertion: more than one allocation row,
		// all three allocation_type values encountered, and the sum of
		// used_pages equal to the independent DMV total.
		if len(alloc.rows) <= 1 {
			t.Fatalf("allocations: want more than one row for a clustered+2NC+LOB table, got %d", len(alloc.rows))
		}
		seen := allocationTypesSeen(alloc.rows)
		if !seen["IN_ROW_DATA"] || !seen["LOB_DATA"] || !seen["ROW_OVERFLOW_DATA"] {
			t.Fatalf("allocation_type coverage: got %v, want all three of IN_ROW_DATA/LOB_DATA/ROW_OVERFLOW_DATA", seen)
		}
		if got := sumUsedPages(alloc.rows); got != wantUsed {
			t.Fatalf("sum(used_pages): got %d, want %d", got, wantUsed)
		}
		if got := sumUsedBytes(alloc.rows); got != wantUsed*8192 {
			t.Fatalf("sum(used_bytes): got %d, want %d (used_pages*8192)", got, wantUsed*8192)
		}
		// distinct index_id values: clustered (1) plus the two
		// nonclustered indexes.
		indexIDs := map[int64]bool{}
		for _, r := range alloc.rows {
			id, _ := r[0].(int64)
			indexIDs[id] = true
		}
		if len(indexIDs) < 3 {
			t.Fatalf("index_id coverage: got %v, want at least 3 distinct indexes", indexIDs)
		}
	})

	t.Run("partitioned", func(t *testing.T) {
		sink := &captureSink{}
		if err := diagnostics.Size(ctx, sess, "dbo.SizePartitioned", sink); err != nil {
			t.Fatalf("Size: %v", err)
		}
		alloc := sink.table("allocations")
		if alloc == nil {
			t.Fatal("allocations table missing")
		}
		partitions := map[int64]bool{}
		for _, r := range alloc.rows {
			p, _ := r[1].(int64)
			partitions[p] = true
		}
		if len(partitions) < 2 {
			t.Fatalf("partition_number coverage: got %v, want at least 2 distinct partitions", partitions)
		}
		// design spec line 103: allocation rows ordered by index_id,
		// then partition_number, then allocation_type - checked here
		// against the REAL engine's own row order, never assumed.
		for i := 1; i < len(alloc.rows); i++ {
			prevIdx, _ := alloc.rows[i-1][0].(int64)
			prevPart, _ := alloc.rows[i-1][1].(int64)
			prevType, _ := alloc.rows[i-1][2].(string)
			curIdx, _ := alloc.rows[i][0].(int64)
			curPart, _ := alloc.rows[i][1].(int64)
			curType, _ := alloc.rows[i][2].(string)
			if curIdx < prevIdx ||
				(curIdx == prevIdx && curPart < prevPart) ||
				(curIdx == prevIdx && curPart == prevPart && curType < prevType) {
				t.Fatalf("allocations not ordered: row %d (%d,%d,%s) precedes row %d (%d,%d,%s)",
					i, curIdx, curPart, curType, i-1, prevIdx, prevPart, prevType)
			}
		}
	})

	t.Run("columnstore", func(t *testing.T) {
		sink := &captureSink{}
		if err := diagnostics.Size(ctx, sess, "dbo.SizeColumnstore", sink); err != nil {
			t.Fatalf("Size: %v", err)
		}
		alloc := sink.table("allocations")
		if alloc == nil {
			t.Fatal("allocations table missing")
		}
		// design spec line 174: columnstore segments are stored as
		// LOB_DATA by the engine itself - this table has no user LOB
		// column at all, so any LOB_DATA row here is unambiguously the
		// columnstore index's own compressed segments, never a user
		// column being mislabeled.
		if seen := allocationTypesSeen(alloc.rows); !seen["LOB_DATA"] {
			t.Fatalf("a forcibly compressed columnstore index must report LOB_DATA, got %v", seen)
		}
	})

	t.Run("empty object", func(t *testing.T) {
		sink := &captureSink{}
		if err := diagnostics.Size(ctx, sess, "dbo.SizeEmpty", sink); err != nil {
			t.Fatalf("Size: %v", err)
		}
		tbl := sink.table("table")
		if tbl == nil || tbl.rows[0][3] != int64(0) {
			t.Fatalf("row_count cell for an empty table: got %#v, want int64(0)", tbl)
		}
		// A genuinely empty selection succeeds at code 0 with zero rows
		// - never an error - design spec's own "genuinely empty
		// selection succeeds" principle, applied here to allocations
		// rather than to a Query Store ranking.
		allocTbl := sink.table("allocations")
		if allocTbl == nil || !allocTbl.collectionComplete {
			t.Fatalf("allocations: want a complete, possibly zero-row table, got %#v", allocTbl)
		}
	})
}

// independentAllocationOrder reads sys.dm_db_partition_stats' raw
// pages directly and unpivots them in Go - never reusing size.sql's
// own UNION ALL - into the (index_id, partition_number,
// allocation_type) triplets that actually carry pages, sorted by
// those three keys with Go's own sort.Slice. This is the "seconde
// lecture indépendante" the brief names as the one assertion form
// that survived every cassure a reviewer tried against TestSize.
func independentAllocationOrder(t *testing.T, ctx context.Context, lab *Lab, qualifiedName string) []string {
	t.Helper()
	var objectID int64
	if err := lab.Admin.QueryRowContext(ctx, "SELECT OBJECT_ID(@p1)", qualifiedName).Scan(&objectID); err != nil {
		t.Fatalf("resolving object_id for %s: %v", qualifiedName, err)
	}
	rows, err := lab.Admin.QueryContext(ctx,
		"SELECT index_id, partition_number, in_row_used_page_count, in_row_reserved_page_count, "+
			"lob_used_page_count, lob_reserved_page_count, row_overflow_used_page_count, row_overflow_reserved_page_count "+
			"FROM sys.dm_db_partition_stats WHERE object_id = @p1", objectID)
	if err != nil {
		t.Fatalf("independent partition-stats read for %s: %v", qualifiedName, err)
	}
	defer rows.Close()

	type triplet struct {
		indexID, partitionNumber int64
		allocationType           string
	}
	var triplets []triplet
	for rows.Next() {
		var indexID, partitionNumber, inRowUsed, inRowReserved, lobUsed, lobReserved, rowOverflowUsed, rowOverflowReserved int64
		if err := rows.Scan(&indexID, &partitionNumber, &inRowUsed, &inRowReserved, &lobUsed, &lobReserved, &rowOverflowUsed, &rowOverflowReserved); err != nil {
			t.Fatalf("scanning independent partition-stats row: %v", err)
		}
		if inRowUsed > 0 || inRowReserved > 0 {
			triplets = append(triplets, triplet{indexID, partitionNumber, "IN_ROW_DATA"})
		}
		if lobUsed > 0 || lobReserved > 0 {
			triplets = append(triplets, triplet{indexID, partitionNumber, "LOB_DATA"})
		}
		if rowOverflowUsed > 0 || rowOverflowReserved > 0 {
			triplets = append(triplets, triplet{indexID, partitionNumber, "ROW_OVERFLOW_DATA"})
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading independent partition-stats rows: %v", err)
	}

	sort.Slice(triplets, func(i, j int) bool {
		a, b := triplets[i], triplets[j]
		if a.indexID != b.indexID {
			return a.indexID < b.indexID
		}
		if a.partitionNumber != b.partitionNumber {
			return a.partitionNumber < b.partitionNumber
		}
		return a.allocationType < b.allocationType
	})

	out := make([]string, len(triplets))
	for i, tr := range triplets {
		out[i] = fmt.Sprintf("%d/%d/%s", tr.indexID, tr.partitionNumber, tr.allocationType)
	}
	return out
}

// productionAllocationOrder reads back the (index_id, partition_number,
// allocation_type) triplets diagnostics.Size actually wrote, in the
// order it wrote them - no sorting here, this is what production says.
func productionAllocationOrder(sink *captureSink) []string {
	tbl := sink.table("allocations")
	if tbl == nil {
		return nil
	}
	out := make([]string, len(tbl.rows))
	for i, row := range tbl.rows {
		indexID, _ := row[0].(int64)
		partitionNumber, _ := row[1].(int64)
		allocationType, _ := row[2].(string)
		out[i] = fmt.Sprintf("%d/%d/%s", indexID, partitionNumber, allocationType)
	}
	return out
}

// TestSizeAllocationsOrderMatchesIndependentUnpivot is task 13 fix-2's
// own A0 target: fix-1 moved allocations.sql's row order entirely into
// sql/size.sql's own ORDER BY (index_id, partition_number,
// allocation_type - design spec line 103's own declared key), and its
// own replacement test, TestSizeAllocationsPreservesDriverOrder, can
// only prove "the code does not reorder the driver's rows" - a fake
// driver always hands back whatever order the test wrote, so no unit
// test can ever exercise the real ORDER BY clause. This does, on two
// fixtures that each vary a different subset of the three keys:
// dbo.SizeFixture (multiple index_id, and multiple allocation_type on
// the same index/partition) and dbo.SizePartitioned (multiple
// partition_number on its one index) - between the two, all three
// keys are exercised by a fixture that actually varies them, unlike
// the reviewer's own measurement of the OLD Go-level test, whose three
// input rows all shared index_id=1 and could never have caught a lost
// index_id comparison.
func TestSizeAllocationsOrderMatchesIndependentUnpivot(t *testing.T) {
	lab := NewLab(t, os.Getenv("ASQ_TEST_IMAGE"))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := applyObjects(ctx, lab.Admin); err != nil {
		t.Fatalf("applying objects.sql: %v", err)
	}

	sess, err := sqlserver.Open(ctx, lab.Profile)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer sess.Close()

	for _, name := range []string{"dbo.SizeFixture", "dbo.SizePartitioned"} {
		t.Run(name, func(t *testing.T) {
			want := independentAllocationOrder(t, ctx, lab, "AppDB."+name)
			sink := &captureSink{}
			if err := diagnostics.Size(ctx, sess, name, sink); err != nil {
				t.Fatalf("Size: %v", err)
			}
			got := productionAllocationOrder(sink)
			if len(got) != len(want) {
				t.Fatalf("%s: allocation row count: got %d, want %d (independent unpivot): got=%v want=%v", name, len(got), len(want), got, want)
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("%s: allocations[%d]: got %q, want %q (independent unpivot, full sequence got=%v want=%v)", name, i, got[i], want[i], got, want)
				}
			}
		})
	}
}

// TestRegisteredTableOrderMatchesExecution is task 13 fix-2's own A8
// target: a reviewer inverted "obj table"'s registered Tables list and
// removed AllocationsTable from "size table"'s, and
// TestHelpOfflineListsAllCommands (the only test that ever reads the
// registry's Tables field) checks nothing but command NAMES - it
// never even looks at Tables. The comment on TableTable/ColumnsTable/
// IndexesTable/AllocationsTable itself makes the promise this closes:
// "help's advertised schema and what Table/Size actually write can
// never drift" - a promise kept only by the diagnostics functions
// happening to agree with the registry, never verified. This compares
// cli.NewRegistry()'s own declared order, by name, against the
// sequence of tables diagnostics.Table/Size actually open (captureSink's
// own tables field, appended in Begin/End order - real execution, not
// a second literal). Limited to these two commands, the ones this
// task's own reviewer found live cassures on: a fully generic version
// across all ten commands would need a fixture and a fake session per
// command (several needing real Query Store history), which this task
// does not build.
func TestRegisteredTableOrderMatchesExecution(t *testing.T) {
	lab := NewLab(t, os.Getenv("ASQ_TEST_IMAGE"))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := applyObjects(ctx, lab.Admin); err != nil {
		t.Fatalf("applying objects.sql: %v", err)
	}
	sess, err := sqlserver.Open(ctx, lab.Profile)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer sess.Close()

	reg := cli.NewRegistry()
	findCommand := func(name string) cli.Command {
		for _, c := range reg {
			if c.Name == name {
				return c
			}
		}
		t.Fatalf("registry has no command named %q", name)
		return cli.Command{}
	}

	cases := []struct {
		command string
		run     func(sink *captureSink) error
	}{
		{"obj table", func(sink *captureSink) error { return diagnostics.Table(ctx, sess, "dbo.Orders", sink) }},
		{"size table", func(sink *captureSink) error { return diagnostics.Size(ctx, sess, "dbo.Orders", sink) }},
	}
	for _, c := range cases {
		t.Run(c.command, func(t *testing.T) {
			cmd := findCommand(c.command)
			var wantNames []string
			for _, spec := range cmd.Tables {
				wantNames = append(wantNames, spec.Name)
			}

			sink := &captureSink{}
			if err := c.run(sink); err != nil {
				t.Fatalf("%s: %v", c.command, err)
			}
			var gotNames []string
			for _, tbl := range sink.tables {
				gotNames = append(gotNames, tbl.spec.Name)
			}

			if len(gotNames) != len(wantNames) {
				t.Fatalf("%s: table count: got %v, want %v (registry's own declared Tables)", c.command, gotNames, wantNames)
			}
			for i := range wantNames {
				if gotNames[i] != wantNames[i] {
					t.Fatalf("%s: table order: got %v, want %v (registry's own declared Tables)", c.command, gotNames, wantNames)
				}
			}
		})
	}
}

// TestObjTableColumnsAndIndexes proves obj table's columns/indexes
// content against a real engine on dbo.ColumnsFixture: a DECIMAL
// column, a persisted computed column, a column default, an ordered
// composite key with mixed ASC/DESC direction plus an included
// column, a unique filtered index, and a disabled index - the brief's
// own named test cases for columns.sql/indexes.sql, asserted by real
// cell VALUE, not by row count alone.
func TestObjTableColumnsAndIndexes(t *testing.T) {
	lab := NewLab(t, os.Getenv("ASQ_TEST_IMAGE"))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := applyObjects(ctx, lab.Admin); err != nil {
		t.Fatalf("applying objects.sql: %v", err)
	}

	sess, err := sqlserver.Open(ctx, lab.Profile)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer sess.Close()

	// fix-2/A2: an independent read of sys.objects/sys.schemas, never a
	// literal copied from the fixture - the "table" row's own identity
	// had no assertion at all before this (a reviewer froze
	// object_id/schema/name to -1/"zzzBREAK18"/"zzzBREAK18" and the
	// suite stayed green).
	var wantObjectID int64
	var wantSchema, wantName string
	if err := lab.Admin.QueryRowContext(ctx,
		"SELECT o.object_id, s.name, o.name FROM sys.objects o JOIN sys.schemas s ON s.schema_id=o.schema_id WHERE o.object_id=OBJECT_ID(N'AppDB.dbo.ColumnsFixture')",
	).Scan(&wantObjectID, &wantSchema, &wantName); err != nil {
		t.Fatalf("independent identity read: %v", err)
	}

	// fix-2/A5: an independent read of sys.columns, ordered by
	// column_id - the sequence AND the values a reviewer measured were
	// unguarded (max_length frozen to 0, precision/scale swapped,
	// is_identity frozen to 0, ORDER BY replaced by name DESC, all
	// green).
	type wantColumn struct {
		name                        string
		maxLength, precision, scale int64
		isIdentity                  bool
	}
	var wantColumns []wantColumn
	colRows, err := lab.Admin.QueryContext(ctx,
		"SELECT c.name, c.max_length, c.precision, c.scale, c.is_identity FROM sys.columns c WHERE c.object_id=OBJECT_ID(N'AppDB.dbo.ColumnsFixture') ORDER BY c.column_id")
	if err != nil {
		t.Fatalf("independent columns read: %v", err)
	}
	for colRows.Next() {
		var wc wantColumn
		if err := colRows.Scan(&wc.name, &wc.maxLength, &wc.precision, &wc.scale, &wc.isIdentity); err != nil {
			t.Fatalf("scanning independent column row: %v", err)
		}
		wantColumns = append(wantColumns, wc)
	}
	colRows.Close()

	// fix-2/A6: an independent read of sys.indexes, ordered by
	// index_id - a reviewer replaced i.type_desc with the literal
	// N'MYSTERY' and reversed ORDER BY i.index_id, both green, because
	// no assertion anywhere read the type cell or compared order.
	type wantIndex struct {
		indexID  int64
		name     string
		typeDesc string
	}
	var wantIndexes []wantIndex
	idxRows, err := lab.Admin.QueryContext(ctx,
		"SELECT i.index_id, i.name, i.type_desc FROM sys.indexes i WHERE i.object_id=OBJECT_ID(N'AppDB.dbo.ColumnsFixture') AND i.index_id > 0 ORDER BY i.index_id")
	if err != nil {
		t.Fatalf("independent indexes read: %v", err)
	}
	for idxRows.Next() {
		var wi wantIndex
		if err := idxRows.Scan(&wi.indexID, &wi.name, &wi.typeDesc); err != nil {
			t.Fatalf("scanning independent index row: %v", err)
		}
		wantIndexes = append(wantIndexes, wi)
	}
	idxRows.Close()

	sink := &captureSink{}
	if err := diagnostics.Table(ctx, sess, "dbo.ColumnsFixture", sink); err != nil {
		t.Fatalf("Table: %v", err)
	}

	tableRow := sink.table("table")
	if tableRow == nil || len(tableRow.rows) != 1 {
		t.Fatalf("table row missing: %#v", tableRow)
	}
	if got := tableRow.rows[0][0]; got != wantObjectID {
		t.Fatalf("table object_id cell: got %#v, want %d (independent sys.objects read)", got, wantObjectID)
	}
	if got := tableRow.rows[0][1]; got != wantSchema {
		t.Fatalf("table schema cell: got %#v, want %q (independent sys.schemas read)", got, wantSchema)
	}
	if got := tableRow.rows[0][2]; got != wantName {
		t.Fatalf("table name cell: got %#v, want %q (independent sys.objects read)", got, wantName)
	}

	columns := sink.table("columns")
	if columns == nil {
		t.Fatal("columns table missing")
	}
	if len(columns.rows) != len(wantColumns) {
		t.Fatalf("columns row count: got %d, want %d (independent sys.columns read)", len(columns.rows), len(wantColumns))
	}
	// Sequence checked BEFORE any map is built: the spec says "Ordered
	// columns", and a map thrown together first (as this test used to
	// do) discards that order before anything can look at it.
	for i, wc := range wantColumns {
		row := columns.rows[i]
		gotName, _ := row[1].(string)
		if gotName != wc.name {
			t.Fatalf("columns[%d].name: got %q, want %q (independent column_id order)", i, gotName, wc.name)
		}
		if row[3] != wc.maxLength {
			t.Fatalf("columns[%d] (%s).max_length: got %#v, want %d", i, wc.name, row[3], wc.maxLength)
		}
		if row[4] != wc.precision {
			t.Fatalf("columns[%d] (%s).precision: got %#v, want %d", i, wc.name, row[4], wc.precision)
		}
		if row[5] != wc.scale {
			t.Fatalf("columns[%d] (%s).scale: got %#v, want %d", i, wc.name, row[5], wc.scale)
		}
		if row[7] != wc.isIdentity {
			t.Fatalf("columns[%d] (%s).identity: got %#v, want %v", i, wc.name, row[7], wc.isIdentity)
		}
	}

	// The map now serves what it's actually for: looking up one named
	// column for the assertions the sequence check above does not
	// make (type name, default/computed definitions, nullability).
	byName := map[string][]model.Cell{}
	for _, row := range columns.rows {
		name, _ := row[1].(string)
		byName[name] = row
	}
	price, ok := byName["Price"]
	if !ok {
		t.Fatalf("Price column missing from %v", byName)
	}
	if price[2] != "decimal" {
		t.Fatalf("Price.type cell: got %#v, want %q", price[2], "decimal")
	}
	if price[9] == nil {
		t.Fatal("Price.default_definition cell: want a non-null default constraint definition")
	}
	extended, ok := byName["Extended"]
	if !ok {
		t.Fatalf("Extended column missing from %v", byName)
	}
	if extended[8] != true {
		t.Fatalf("Extended.computed cell: got %#v, want true", extended[8])
	}
	if extended[10] == nil {
		t.Fatal("Extended.computed_definition cell: want a non-null computed-column definition")
	}
	note, ok := byName["Note"]
	if !ok {
		t.Fatalf("Note column missing from %v", byName)
	}
	if note[6] != true {
		t.Fatalf("Note.nullable cell: got %#v, want true", note[6])
	}

	indexes := sink.table("indexes")
	if indexes == nil {
		t.Fatal("indexes table missing")
	}
	if len(indexes.rows) != len(wantIndexes) {
		t.Fatalf("indexes row count: got %d, want %d (independent sys.indexes read)", len(indexes.rows), len(wantIndexes))
	}
	// Sequence and type_desc checked BEFORE any map is built - same
	// reasoning as columns above, and this is index_id's own order,
	// the second half of A6.
	for i, wi := range wantIndexes {
		row := indexes.rows[i]
		gotID, _ := row[0].(int64)
		gotName, _ := row[1].(string)
		gotType, _ := row[2].(string)
		if gotID != wi.indexID || gotName != wi.name {
			t.Fatalf("indexes[%d]: got (index_id=%v, name=%q), want (index_id=%d, name=%q) (independent index_id order)", i, gotID, gotName, wi.indexID, wi.name)
		}
		if gotType != wi.typeDesc {
			t.Fatalf("indexes[%d] (%s).type: got %q, want %q", i, wi.name, gotType, wi.typeDesc)
		}
	}

	byIndexName := map[string][]model.Cell{}
	for _, row := range indexes.rows {
		name, _ := row[1].(string)
		byIndexName[name] = row
	}
	composite, ok := byIndexName["IX_ColumnsFixture_Composite"]
	if !ok {
		t.Fatalf("IX_ColumnsFixture_Composite missing from %v", byIndexName)
	}
	if composite[3] != "[Quantity] DESC, [Price] ASC" {
		t.Fatalf("composite index keys cell: got %#v, want %q", composite[3], "[Quantity] DESC, [Price] ASC")
	}
	if composite[4] != "[Note]" {
		t.Fatalf("composite index includes cell: got %#v, want %q", composite[4], "[Note]")
	}
	unique, ok := byIndexName["UX_ColumnsFixture_Note"]
	if !ok {
		t.Fatalf("UX_ColumnsFixture_Note missing from %v", byIndexName)
	}
	if unique[6] != true {
		t.Fatalf("unique index unique cell: got %#v, want true", unique[6])
	}
	if unique[5] == nil {
		t.Fatal("unique index filter cell: want a non-null filter definition")
	}
	disabled, ok := byIndexName["IX_ColumnsFixture_Disabled"]
	if !ok {
		t.Fatalf("IX_ColumnsFixture_Disabled missing from %v", byIndexName)
	}
	if disabled[7] != true {
		t.Fatalf("disabled index disabled cell: got %#v, want true", disabled[7])
	}
}

// TestIdxListOnHeapExcludesIndexZero is task 13 fix-2's own A3 target:
// sql/indexes.sql's "AND i.index_id > 0" clause is what IndexesTable's
// own doc comment relies on to claim "every row this table ever
// carries names a real index" - a claim no fixture before this test
// ever exercised, since dbo.SizeHeap (the only heap this project's
// fixtures define) was read only by TestSize, which never opens the
// "indexes" table at all, and every other fixture (Orders,
// ColumnsFixture) has a clustered primary key. Measured by a reviewer
// with the clause removed: idx list on a heap then returns TWO rows,
// the first being the heap's own index_id=0 pseudo-row with a NULL
// name - exactly what this test asserts never happens.
func TestIdxListOnHeapExcludesIndexZero(t *testing.T) {
	lab := NewLab(t, os.Getenv("ASQ_TEST_IMAGE"))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := applyObjects(ctx, lab.Admin); err != nil {
		t.Fatalf("applying objects.sql: %v", err)
	}

	sess, err := sqlserver.Open(ctx, lab.Profile)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer sess.Close()

	sink := &captureSink{}
	if err := diagnostics.Indexes(ctx, sess, "dbo.SizeHeap", sink); err != nil {
		t.Fatalf("Indexes: %v", err)
	}
	tbl := sink.table("indexes")
	if tbl == nil {
		t.Fatal("indexes table missing")
	}
	if len(tbl.rows) != 1 {
		t.Fatalf("dbo.SizeHeap (one real nonclustered index, plus its own heap pseudo-row): want exactly one row, got %d: %#v", len(tbl.rows), tbl.rows)
	}
	row := tbl.rows[0]
	indexID, _ := row[0].(int64)
	if indexID == 0 {
		t.Fatal("indexes[0].index_id: got 0 (the heap's own pseudo-row), want a real index")
	}
	if row[1] == nil {
		t.Fatal("indexes[0].name: want a non-null real index name, not the heap's own NULL")
	}
	if row[2] != "NONCLUSTERED" {
		t.Fatalf("indexes[0].type: got %#v, want %q (IX_SizeHeap_Value)", row[2], "NONCLUSTERED")
	}
}
