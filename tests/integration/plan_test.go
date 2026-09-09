//go:build integration

package integration

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/rudi-bruchez/argosql/internal/diagnostics"
	"github.com/rudi-bruchez/argosql/internal/model"
	"github.com/rudi-bruchez/argosql/internal/plan"
	"github.com/rudi-bruchez/argosql/internal/sqlserver"
)

// TestPlanSummaryWarningAttributeForm is fix 1's A1, the reviewer's
// own exact repro run against a real engine (both 2019 and 2022): a
// cross join with no join predicate, forced serial with
// OPTION(MAXDOP 1), produces a real <Warnings NoJoinPredicate="1"/> -
// an attribute of the Warnings element itself, not a child. The
// previous implementation read only Warnings' children and reported
// this table empty, collection_complete=true, on exactly this
// document.
func TestPlanSummaryWarningAttributeForm(t *testing.T) {
	lab := NewLab(t, os.Getenv("ASQ_TEST_IMAGE"))
	ctx, cancel := context.WithTimeout(context.Background(), flushPollBudget)
	defer cancel()

	const marker = "AsqFix1A1Crossjoin"
	batch := "SELECT SUM(a.Quantity+b.Quantity) AS " + marker + " FROM dbo.Widgets a CROSS JOIN dbo.Widgets b OPTION(MAXDOP 1)"
	// Five executions before the flush, matching Lab.QueryID's own
	// resilience pattern (fixture_test.go): measured on 2019, a single
	// execution of this batch was not reliably visible to
	// sp_query_store_flush_db within the poll budget, unlike 2022.
	for i := 0; i < 5; i++ {
		if _, err := lab.Admin.ExecContext(ctx, batch); err != nil {
			t.Fatalf("running the cross join workload: %v", err)
		}
	}
	if _, err := lab.Admin.ExecContext(ctx, "EXEC sys.sp_query_store_flush_db"); err != nil {
		t.Fatalf("sp_query_store_flush_db: %v", err)
	}

	findID := "SELECT q.query_id FROM sys.query_store_query_text AS qt " +
		"JOIN sys.query_store_query AS q ON q.query_text_id = qt.query_text_id " +
		"WHERE qt.query_sql_text LIKE '%' + @p1 + '%'"
	deadline := time.Now().Add(flushPollDeadline)
	var queryID int64
	for {
		err := lab.Admin.QueryRowContext(ctx, findID, marker).Scan(&queryID)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("marker %q did not appear within %s of flush: %v", marker, flushPollDeadline, err)
		}
		time.Sleep(flushPollDelay)
	}
	planID := planIDFor(ctx, t, lab, queryID)

	r, code := lab.Run(t, "Q", []string{"plan", strconv.FormatInt(queryID, 10), "--plan-id", strconv.FormatInt(planID, 10), "--summary"})
	if code != 0 {
		t.Fatalf("plan --summary: code=%d, error=%+v", code, r.Error)
	}
	var warnRows [][]model.Cell
	for _, tbl := range r.Tables {
		if tbl.Spec.Name == plan.WarningsTable.Name {
			warnRows = append(warnRows, tbl.Rows...)
		}
	}
	if len(warnRows) == 0 {
		t.Fatal("warnings table: got 0 rows, want at least the NoJoinPredicate warning (fix 1's A1)")
	}
	found := false
	for _, row := range warnRows {
		if row[0] == "NoJoinPredicate" {
			found = true
		}
	}
	if !found {
		t.Fatalf("NoJoinPredicate warning not found: %+v", warnRows)
	}
}

// planIDFor reads sys.query_store_plan's own plan_id for queryID
// directly through lab.Admin - workload.sql's own load (run through
// Lab.QueryID) is a single, deterministic statement, so it compiles
// exactly one plan.
func planIDFor(ctx context.Context, t *testing.T, lab *Lab, queryID int64) int64 {
	t.Helper()
	var planID int64
	err := lab.Admin.QueryRowContext(ctx,
		"SELECT TOP (1) plan_id FROM sys.query_store_plan WHERE query_id = @p1 ORDER BY plan_id", queryID,
	).Scan(&planID)
	if err != nil {
		t.Fatalf("reading plan_id for query_id %d: %v", queryID, err)
	}
	return planID
}

// TestPlanExport is task 12's own directing test against a real
// engine: a real (query_id, plan_id) pair, looked up through "plan"
// run as a real subprocess (Lab.Run), must succeed and export exactly
// one ".sqlplan" artifact, byte-identical to query_plan read
// independently through lab.Admin - never Plan's own in-memory copy of
// the same value (the same discipline
// tests/integration/query_test.go's own
// TestQueryExportLongUnicodeTextByteIdentity applies to query_sql_text).
func TestPlanExport(t *testing.T) {
	lab := NewLab(t, os.Getenv("ASQ_TEST_IMAGE"))
	ctx, cancel := context.WithTimeout(context.Background(), flushPollBudget)
	defer cancel()

	queryID := lab.QueryID(t, "asqFixturePlanExport")
	planID := planIDFor(ctx, t, lab, queryID)

	var wantXML string
	err := lab.Admin.QueryRowContext(ctx,
		"SELECT query_plan FROM sys.query_store_plan WHERE query_id = @p1 AND plan_id = @p2", queryID, planID,
	).Scan(&wantXML)
	if err != nil {
		t.Fatalf("reading query_plan independently: %v", err)
	}

	r, code := lab.Run(t, "Q", []string{"plan", strconv.FormatInt(queryID, 10), "--plan-id", strconv.FormatInt(planID, 10)})
	if code != 0 {
		t.Fatalf("plan: code=%d, error=%+v", code, r.Error)
	}
	var artifactPath string
	for _, a := range r.Artifacts {
		if a.Kind == "plan_xml" {
			artifactPath = a.Path
		}
	}
	if artifactPath == "" {
		t.Fatalf("no plan_xml artifact in the result: %+v", r.Artifacts)
	}
	diskBytes, err := os.ReadFile(artifactPath)
	if err != nil {
		t.Fatalf("reading the exported artifact from disk (%s): %v", artifactPath, err)
	}
	if string(diskBytes) != wantXML {
		t.Fatalf("artifact on disk does not match query_plan byte for byte:\n got  (%d bytes)\n want (%d bytes)", len(diskBytes), len(wantXML))
	}
}

// TestPlanNotFound is design spec line 223's own case against a real
// engine: a query_id that certainly does not exist in AppDB must fail
// at code 8, not_found_or_not_visible.
func TestPlanNotFound(t *testing.T) {
	lab := NewLab(t, os.Getenv("ASQ_TEST_IMAGE"))
	r, code := lab.Run(t, "Q", []string{"plan", "9223372036854775807", "--plan-id", "1"})
	if code != 8 {
		t.Fatalf("code: got %d, want 8: %+v", code, r.Error)
	}
}

// TestPlanMismatch is design spec line 224's own case against a real
// engine: a real, existing plan_id, but paired with a DIFFERENT real
// query_id it does not belong to, must fail at code 8 exactly like a
// plainly missing id - sql/plan.sql's own WHERE clause cannot, and
// does not try to, tell the two apart.
func TestPlanMismatch(t *testing.T) {
	lab := NewLab(t, os.Getenv("ASQ_TEST_IMAGE"))
	ctx, cancel := context.WithTimeout(context.Background(), flushPollBudget)
	defer cancel()

	queryA := lab.QueryID(t, "asqFixturePlanMismatchA")
	queryB := lab.QueryID(t, "asqFixturePlanMismatchB")
	planOfB := planIDFor(ctx, t, lab, queryB)

	r, code := lab.Run(t, "Q", []string{"plan", strconv.FormatInt(queryA, 10), "--plan-id", strconv.FormatInt(planOfB, 10)})
	if code != 8 {
		t.Fatalf("code: got %d, want 8: %+v", code, r.Error)
	}
}

// TestPlanSummary is the brief's own summary case against a real
// engine: --summary must produce the four design-spec tables, in
// their declared order, with the statement table's own two facts
// (source, estimated_cost) populated and at least one operator row -
// a real engine's compiled plan for workload.sql's own single COUNT(*)
// query always has at least one RelOp.
func TestPlanSummary(t *testing.T) {
	lab := NewLab(t, os.Getenv("ASQ_TEST_IMAGE"))
	ctx, cancel := context.WithTimeout(context.Background(), flushPollBudget)
	defer cancel()

	queryID := lab.QueryID(t, "asqFixturePlanSummary")
	planID := planIDFor(ctx, t, lab, queryID)

	r, code := lab.Run(t, "Q", []string{"plan", strconv.FormatInt(queryID, 10), "--plan-id", strconv.FormatInt(planID, 10), "--summary"})
	if code != 0 {
		t.Fatalf("plan --summary: code=%d, error=%+v", code, r.Error)
	}

	wantOrder := []string{plan.StatementTable.Name, plan.OperatorsTable.Name, plan.ReferencesTable.Name, plan.WarningsTable.Name}
	if len(r.Tables) != len(wantOrder) {
		t.Fatalf("tables: got %d, want exactly %d: %+v", len(r.Tables), len(wantOrder), r.Tables)
	}
	for i, name := range wantOrder {
		if got := r.Tables[i].Spec.Name; got != name {
			t.Fatalf("table %d: got %q, want %q (declared order: statement, operators, references, warnings)", i, got, name)
		}
	}

	stmt := r.Tables[0]
	if len(stmt.Rows) != 1 {
		t.Fatalf("statement table: got %d rows, want 1", len(stmt.Rows))
	}
	if got, ok := stmt.Rows[0][0].(string); !ok || got != "query_store_compiled_plan_xml" {
		t.Fatalf("statement source: got %#v, want %q", stmt.Rows[0][0], "query_store_compiled_plan_xml")
	}
	if stmt.Rows[0][1] == nil {
		t.Fatal("estimated_cost: got nil, want a real StatementSubTreeCost value (fix 1's A2)")
	}

	ops := r.Tables[1]
	if len(ops.Rows) == 0 {
		t.Fatal("operators table: got 0 rows, want at least one real operator")
	}
}

// TestPlanSucceedsWhileQueryStoreOff is design spec line 83's own
// exception, exercised against a real engine: "plan can export a
// retained plan even if no runtime history remains" - a plan/query
// pair captured while Query Store was READ_WRITE must still export
// successfully at code 0 after Query Store is turned OFF, unlike
// "qs top"/"qs query", which gate the EXIT CODE on runtime history for
// a non-collecting state (see top_test.go's/query_test.go's own OFF
// fixtures). Fix 1's A4: unlike this task's own original claim, Plan
// DOES read health first, exactly like every other Query Store
// command (design spec line 81) - measured against a real engine
// before this fix, "plan" under Q exported at code 0 with NO notice
// at all after the same ALTER DATABASE ... SET QUERY_STORE = OFF, so
// this test now asserts the "capture" notice is present, not merely
// that the export still succeeds.
func TestPlanSucceedsWhileQueryStoreOff(t *testing.T) {
	lab := NewLab(t, os.Getenv("ASQ_TEST_IMAGE"))
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	dbName := createThrowawayDatabase(ctx, t, lab, "asq_plan_off_")
	if _, err := lab.Admin.ExecContext(ctx, "ALTER DATABASE ["+dbName+"] SET QUERY_STORE = ON (OPERATION_MODE = READ_WRITE, INTERVAL_LENGTH_MINUTES = 1)"); err != nil {
		t.Fatalf("enabling Query Store: %v", err)
	}
	generateLoadAndWaitForHistory(ctx, t, lab, dbName)

	dbProfile := lab.Profile
	dbProfile.Database = dbName
	dbSess, err := sqlserver.Open(ctx, dbProfile)
	if err != nil {
		t.Fatalf("open to read a real query_id/plan_id: %v", err)
	}
	var queryID, planID int64
	scanErr := dbSess.Conn.QueryRowContext(ctx,
		"SELECT TOP (1) p.query_id, p.plan_id FROM sys.query_store_plan AS p ORDER BY p.plan_id DESC",
	).Scan(&queryID, &planID)
	dbSess.Close()
	if scanErr != nil {
		t.Fatalf("reading a real query_id/plan_id from the throwaway database: %v", scanErr)
	}

	if _, err := lab.Admin.ExecContext(ctx, "ALTER DATABASE ["+dbName+"] SET QUERY_STORE = OFF"); err != nil {
		t.Fatalf("disabling Query Store: %v", err)
	}

	offProfile := lab.Profile
	offProfile.Database = dbName
	offSess, err := sqlserver.Open(ctx, offProfile)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer offSess.Close()

	sink := &captureSink{}
	if err := diagnostics.Plan(ctx, offSess, queryID, planID, false, sink); err != nil {
		t.Fatalf("Plan on an OFF database should still succeed, got: %v", err)
	}
	if len(sink.files) != 1 {
		t.Fatalf("files: got %d, want 1", len(sink.files))
	}
	if n := sink.noticeWithKind("capture"); n == nil {
		t.Fatalf("no capture notice emitted for a non-READ_WRITE Query Store state: notices=%+v", sink.notices)
	}
}

// TestPlanUnavailableForNullQueryPlan proves, against a real engine,
// that a NULL query_plan (see planXMLFixture's own unit-level
// coverage in internal/diagnostics/plan_test.go) is unreachable through
// ordinary Query Store activity on 2019/2022 - sys.query_store_plan
// never leaves query_plan NULL for a plan this project's own workload
// captures. This is documented here, rather than left unstated,
// because the brief's own checklist names "absence de runtime" among
// its required Vert cases and this is where that expectation is
// checked against the real engine and found not reproducible without
// engine-internal corruption - the unit-level fake-driver test is this
// case's real coverage; see this task's report.
func TestPlanUnavailableForNullQueryPlan(t *testing.T) {
	lab := NewLab(t, os.Getenv("ASQ_TEST_IMAGE"))
	ctx, cancel := context.WithTimeout(context.Background(), flushPollBudget)
	defer cancel()

	queryID := lab.QueryID(t, "asqFixturePlanNullCheck")
	planID := planIDFor(ctx, t, lab, queryID)

	var isNull bool
	err := lab.Admin.QueryRowContext(ctx,
		"SELECT CAST(CASE WHEN query_plan IS NULL THEN 1 ELSE 0 END AS BIT) FROM sys.query_store_plan WHERE query_id = @p1 AND plan_id = @p2",
		queryID, planID,
	).Scan(&isNull)
	if err != nil {
		t.Fatalf("checking query_plan nullability: %v", err)
	}
	if isNull {
		t.Fatal("measured: query_plan is unexpectedly NULL for an ordinary captured plan - the plan_unavailable path is reachable here after all; update accordingly")
	}
	t.Log("measured: query_plan is never NULL for a plan this project's own workload captures - plan_unavailable is covered at the unit level only (internal/diagnostics/plan_test.go)")
}
