//go:build integration

package integration

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rudi-bruchez/argosql/internal/diagnostics"
	"github.com/rudi-bruchez/argosql/internal/model"
	"github.com/rudi-bruchez/argosql/internal/sqlserver"
)

// runExactAggregationFixture proves top_2019.sql/top_2022.sql's real,
// embedded aggregation formula against synthetic data in temp tables
// standing in for the catalog views Query Store itself only fills
// through real workload execution (design spec's runtime-statistics
// semantics cannot be seeded directly: sys.query_store_runtime_stats is
// a read-only system view).
//
// It substitutes ONLY the FROM/JOIN table names in the real, embedded
// query text (diagnostics.TopQuerySQL) - never the aggregation
// expressions themselves - so a defect introduced into that real SQL
// (for example, dropping the "* rs.count_executions" weighting, or
// recopying the 2019 form's GROUP BY/ORDER BY into the 2022 file) is
// exactly what this fixture exists to catch: it runs the actual
// production query text, unmodified beyond table names, against
// exactly the fixture the brief specifies.
//
// The fixture: two runtime rows for the SAME plan (10) and the SAME
// interval (1) - one as if flushed to disk, one as if still
// in-memory, per Microsoft's own documented reason
// sys.query_store_runtime_stats can hold more than one row per
// plan/interval on the active interval - with (count=1,
// avg_cpu_us=1000) and (count=9, avg_cpu_us=3000), which must sum to
// executions=10, cpu_total_ms=28, cpu_avg_ms=2.8; a second plan (11) on
// the SAME query and the SAME replica_group_id (1), which must
// combine with plan 10's own total on 2022; and a THIRD plan, which is
// plan 10 again but on a DIFFERENT replica_group_id (2).
//
// That third row is the case a second reviewer measured as missing: a
// fixture that only ever varies plan_id and replica_group_id together
// (as an earlier version of this fixture did) cannot tell a correct
// "GROUP BY query_id, replica_group_id" apart from an incorrect
// "GROUP BY query_id, plan_id" - both partition the same two rows into
// the same two groups. Plan 10 appearing on two different replica
// groups, and plan 11 sharing plan 10's OWN replica group, is what
// forces the two discriminants apart: grouping by plan_id would now
// produce three output rows (one per distinct plan_id) instead of two
// (one per distinct replica_group_id), and a column rendered via
// MAX(replica_group_id) instead of the real GROUP BY key would expose
// that divergence as a wrong row count or a wrong replica_group_id on
// this exact fixture. Two rows with execution_type 3 and 4 are also
// seeded, which must be excluded from every total.
func runExactAggregationFixture(ctx context.Context, t *testing.T, lab *Lab, major int) {
	t.Helper()

	// #temp tables are connection-scoped: every statement below must run
	// on this one *sql.Conn, never through lab.Admin's pool directly,
	// which could hand different statements to different pooled
	// connections.
	conn, err := lab.Admin.Conn(ctx)
	if err != nil {
		t.Fatalf("acquiring a dedicated connection for temp tables: %v", err)
	}
	defer conn.Close()

	ddl := []string{
		`CREATE TABLE #rs (
			plan_id bigint, runtime_stats_interval_id bigint, replica_group_id bigint NULL,
			execution_type int, count_executions bigint, avg_cpu_time bigint, avg_duration bigint, avg_logical_io_reads bigint)`,
		`CREATE TABLE #interval (runtime_stats_interval_id bigint, start_time datetime2, end_time datetime2)`,
		`CREATE TABLE #plan (plan_id bigint, query_id bigint)`,
		`CREATE TABLE #query (query_id bigint, is_internal_query bit, object_id int NULL)`,
	}
	for _, stmt := range ddl {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("creating temp table (%s): %v", stmt, err)
		}
	}

	since := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	until := since.Add(time.Hour)
	intervalStart := since.Add(time.Minute)
	intervalEnd := intervalStart.Add(time.Minute)

	if _, err := conn.ExecContext(ctx, "INSERT INTO #interval VALUES (@p1, @p2, @p3)", int64(1), intervalStart, intervalEnd); err != nil {
		t.Fatalf("seeding #interval: %v", err)
	}
	if _, err := conn.ExecContext(ctx, "INSERT INTO #plan (plan_id, query_id) VALUES (@p1,@p2), (@p3,@p4)", int64(10), int64(100), int64(11), int64(100)); err != nil {
		t.Fatalf("seeding #plan: %v", err)
	}
	if _, err := conn.ExecContext(ctx, "INSERT INTO #query (query_id, is_internal_query, object_id) VALUES (@p1, 0, NULL)", int64(100)); err != nil {
		t.Fatalf("seeding #query: %v", err)
	}

	insertRS := "INSERT INTO #rs (plan_id, runtime_stats_interval_id, replica_group_id, execution_type, count_executions, avg_cpu_time, avg_duration, avg_logical_io_reads) VALUES (@p1,@p2,@p3,@p4,@p5,@p6,@p7,@p8)"
	rsRows := [][8]any{
		{int64(10), int64(1), int64(1), 0, int64(1), int64(1000), int64(2000), int64(30)},
		{int64(10), int64(1), int64(1), 0, int64(9), int64(3000), int64(1000), int64(10)},
		// plan 11, SAME replica group (1) as plan 10 above: on 2022 this
		// combines with plan 10's total; on 2019 it is one of the three
		// that all combine together regardless.
		{int64(11), int64(1), int64(1), 0, int64(4), int64(500), int64(500), int64(5)},
		// plan 10 AGAIN, but a DIFFERENT replica group (2): the row that
		// separates grouping by replica_group_id from grouping by
		// plan_id - see this function's own doc comment above.
		{int64(10), int64(1), int64(2), 0, int64(2), int64(5000), int64(3000), int64(40)},
		// execution_type 3/4: aborted, must never contribute to a total -
		// grossly large values so an accidental inclusion cannot hide.
		{int64(10), int64(1), int64(1), 3, int64(1000), int64(999999), int64(999999), int64(999999)},
		{int64(10), int64(1), int64(1), 4, int64(1000), int64(999999), int64(999999), int64(999999)},
	}
	for _, r := range rsRows {
		if _, err := conn.ExecContext(ctx, insertRS, r[0], r[1], r[2], r[3], r[4], r[5], r[6], r[7]); err != nil {
			t.Fatalf("seeding #rs: %v", err)
		}
	}

	query := diagnostics.TopQuerySQL(major)
	// ReplaceAll: the embedded file also mentions OrderPlaceholder once
	// in its own doc comment, ahead of the real ORDER BY occurrence - see
	// top.go's matching comment on why a count-limited replace is wrong
	// here.
	query = strings.ReplaceAll(query, diagnostics.OrderPlaceholder, "cpu_total_ms")
	query = strings.NewReplacer(
		// Longer, more specific names first: NewReplacer tries patterns
		// in argument order at each position, so the "_interval" suffix
		// form must be offered before its own prefix.
		"sys.query_store_runtime_stats_interval", "#interval",
		"sys.query_store_runtime_stats", "#rs",
		"sys.query_store_plan", "#plan",
		"sys.query_store_query", "#query",
	).Replace(query)

	dbRows, err := conn.QueryContext(ctx, query,
		sql.Named("since", since),
		sql.Named("until", until),
		sql.Named("include_internal", true),
		sql.Named("object_id", sql.NullInt64{}),
		sql.Named("min_executions", int64(1)),
		sql.Named("top", int64(10)),
	)
	if err != nil {
		t.Fatalf("running the substituted aggregation query: %v", err)
	}
	defer dbRows.Close()

	type aggRow struct {
		queryID      int64
		replicaGroup sql.NullInt64
		executions   int64
		cpuTotalMs   float64
		cpuAvgMs     float64
	}
	var got []aggRow
	for dbRows.Next() {
		var r aggRow
		var durTotal, durAvg, readsTotal, readsAvg float64
		if err := dbRows.Scan(&r.queryID, &r.replicaGroup, &r.executions, &r.cpuTotalMs, &r.cpuAvgMs, &durTotal, &durAvg, &readsTotal, &readsAvg); err != nil {
			t.Fatalf("scanning aggregation row: %v", err)
		}
		got = append(got, r)
	}
	if err := dbRows.Err(); err != nil {
		t.Fatalf("reading aggregation rows: %v", err)
	}

	const tol = 1e-9
	if major >= 16 {
		// Group 1 (replica_group_id=1): plan 10's own two rows
		// (executions=10, cpu_us=28000) plus plan 11 (executions=4,
		// cpu_us=2000) = executions=14, cpu_total_ms=30. Group 2
		// (replica_group_id=2): plan 10's other row alone,
		// executions=2, cpu_total_ms=10. Ordered by cpu_total_ms DESC,
		// group 1 (30ms) ranks before group 2 (10ms). These numbers are
		// measured, not deduced from the formula in the abstract: see
		// this function's own doc comment for why this exact fixture
		// shape (plan 10 on two replica groups, plan 11 sharing plan
		// 10's own group) is what actually separates a correct
		// GROUP BY replica_group_id from an incorrect GROUP BY plan_id.
		if len(got) != 2 {
			t.Fatalf("2022: got %d rows, want 2 (one per replica group): %+v", len(got), got)
		}
		first, second := got[0], got[1]
		if first.executions != 14 {
			t.Fatalf("row 0 executions: got %d, want 14", first.executions)
		}
		if math.Abs(first.cpuTotalMs-30) > tol {
			t.Fatalf("row 0 cpu_total_ms: got %v, want 30 (tolerance %v)", first.cpuTotalMs, tol)
		}
		wantAvg1 := 30000.0 / 14 / 1000.0
		if math.Abs(first.cpuAvgMs-wantAvg1) > tol {
			t.Fatalf("row 0 cpu_avg_ms: got %v, want %v (tolerance %v)", first.cpuAvgMs, wantAvg1, tol)
		}
		if !first.replicaGroup.Valid || first.replicaGroup.Int64 != 1 {
			t.Fatalf("row 0 replica_group_id: got %+v, want 1", first.replicaGroup)
		}
		if !second.replicaGroup.Valid || second.replicaGroup.Int64 != 2 {
			t.Fatalf("row 1 replica_group_id: got %+v, want 2 (kept distinct from row 0)", second.replicaGroup)
		}
		if second.executions != 2 {
			t.Fatalf("row 1 executions: got %d, want 2", second.executions)
		}
		if math.Abs(second.cpuTotalMs-10) > tol {
			t.Fatalf("row 1 cpu_total_ms: got %v, want 10 (tolerance %v)", second.cpuTotalMs, tol)
		}
	} else {
		// 2019 has no replica_group_id column at all: every one of the
		// three (plan 10/group 1's two rows, plan 11, and plan 10's
		// second row under what would be group 2 on 2022) combines into
		// ONE row for query 100: executions=10+4+2=16,
		// cpu_total_ms=(28000+2000+10000)/1000=40.
		if len(got) != 1 {
			t.Fatalf("2019: got %d rows, want 1 (no replica grouping): %+v", len(got), got)
		}
		row := got[0]
		if row.executions != 16 {
			t.Fatalf("executions: got %d, want 16 (10 + 4 + 2, plans combined)", row.executions)
		}
		wantTotal := 40.0
		wantAvg := 40000.0 / 16 / 1000.0
		if math.Abs(row.cpuTotalMs-wantTotal) > tol {
			t.Fatalf("cpu_total_ms: got %v, want %v (tolerance %v)", row.cpuTotalMs, wantTotal, tol)
		}
		if math.Abs(row.cpuAvgMs-wantAvg) > tol {
			t.Fatalf("cpu_avg_ms: got %v, want %v (tolerance %v)", row.cpuAvgMs, wantAvg, tol)
		}
		if row.replicaGroup.Valid {
			t.Fatalf("replica_group_id: got %+v, want a typed NULL", row.replicaGroup)
		}
	}
}

// TestTop exercises diagnostics.Top against a real engine, in five
// subtests. The first proves the real, embedded aggregation formula and
// its 2019/2022 replica_group_id divergence against synthetic
// temp-table data (sys.query_store_runtime_stats cannot be written to
// directly). The second proves the full pipeline - health, window, SQL
// dispatch, row scanning, and the new "ranking" header table - against a
// real Query Store entry discovered by Lab.QueryID's marker mechanism,
// the same helper task 11 reuses rather than redefining. The third
// proves the "coverage_window" notice fires when the requested window
// reaches outside what is actually covered. The fourth and fifth prove
// the "capture" notice and the code-4 failure this fix's point 6
// discovered neither test in this repository could previously assert,
// because captureSink.Notice used to discard every notice it received.
func TestTop(t *testing.T) {
	lab := NewLab(t, os.Getenv("ASQ_TEST_IMAGE"))
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	sess, err := sqlserver.Open(ctx, lab.Profile)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer sess.Close()
	logEngineIdentity(t, lab, sess.Major)

	t.Run("exact aggregation via temp tables, separating plan_id from replica_group_id", func(t *testing.T) {
		runExactAggregationFixture(ctx, t, lab, sess.Major)
	})

	t.Run("real captured load is ranked, and the ranking header names the real window and coverage", func(t *testing.T) {
		marker := "AsqTopMarker" + randomHex(6)
		queryID := lab.QueryID(t, marker)

		sink := &captureSink{}
		opts := diagnostics.TopOptions{
			Window:        diagnostics.Window{Since: time.Now().Add(-30 * time.Minute), Until: time.Now().Add(2 * time.Minute)},
			By:            "cpu",
			Aggregate:     "total",
			Top:           50,
			MinExecutions: 1,
		}
		if err := diagnostics.Top(ctx, sess, opts, sink); err != nil {
			t.Fatalf("Top: %v", err)
		}

		ranking := sink.table(diagnostics.TopRankingTable.Name)
		if ranking == nil || len(ranking.rows) != 1 {
			t.Fatalf("%q table missing or wrong row count: %+v", diagnostics.TopRankingTable.Name, sink.tables)
		}
		requireComplete(t, ranking, "ranking")
		row := ranking.rows[0]
		wantSince := opts.Window.Since.UTC().Format("2006-01-02T15:04:05.9999999Z07:00")
		wantUntil := opts.Window.Until.UTC().Format("2006-01-02T15:04:05.9999999Z07:00")
		if got, ok := row[0].(string); !ok || got != wantSince {
			t.Fatalf("requested_since: got %#v, want %q", row[0], wantSince)
		}
		if got, ok := row[1].(string); !ok || got != wantUntil {
			t.Fatalf("requested_until: got %#v, want %q", row[1], wantUntil)
		}
		// AppDB already has real Query Store history by this point in
		// the suite (TestFixtureQueryStoreFlush and this very call's own
		// marker load, at minimum): a coverage bound hardcoded to NULL
		// would pass only by never reading coverage.sql's real result at
		// all - exactly cassure 4 below.
		if row[2] == nil {
			t.Fatal("coverage_oldest: got nil, want a real coverage bound (AppDB has history)")
		}
		if row[3] == nil {
			t.Fatal("coverage_newest: got nil, want a real coverage bound (AppDB has history)")
		}
		if got, ok := row[4].(string); !ok || got != "cpu" {
			t.Fatalf("by: got %#v, want \"cpu\"", row[4])
		}
		if got, ok := row[5].(string); !ok || got != "total" {
			t.Fatalf("aggregate: got %#v, want \"total\"", row[5])
		}
		if got, ok := row[6].(int64); !ok || got != 50 {
			t.Fatalf("top: got %#v, want int64(50)", row[6])
		}
		if got, ok := row[7].(int64); !ok || got != 1 {
			t.Fatalf("min_executions: got %#v, want int64(1)", row[7])
		}
		if got, ok := row[8].(bool); !ok || got != false {
			t.Fatalf("include_internal: got %#v, want false", row[8])
		}
		if row[9] != nil {
			t.Fatalf("parent_module: got %#v, want nil (no --object given)", row[9])
		}

		queries := sink.table(diagnostics.TopQueriesTable.Name)
		if queries == nil {
			t.Fatalf("no %q table in Top's output: %+v", diagnostics.TopQueriesTable.Name, sink.tables)
		}
		requireComplete(t, queries, "queries")

		found := false
		for _, row := range queries.rows {
			if id, ok := row[0].(int64); ok && id == queryID {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("query_id %d (marker %q) not found in qs top output: %+v", queryID, marker, queries.rows)
		}
	})

	t.Run("a window reaching before available coverage emits a coverage_window notice", func(t *testing.T) {
		sink := &captureSink{}
		opts := diagnostics.TopOptions{
			Window:        diagnostics.Window{Since: time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), Until: time.Now().Add(time.Minute)},
			By:            "cpu",
			Aggregate:     "total",
			Top:           10,
			MinExecutions: 1,
		}
		if err := diagnostics.Top(ctx, sess, opts, sink); err != nil {
			t.Fatalf("Top: %v", err)
		}
		notice := sink.noticeWithKind("coverage_window")
		if notice == nil {
			t.Fatalf("no coverage_window notice emitted for a window starting in year 2000: %+v", sink.notices)
		}
		if !strings.Contains(notice.Message, "2000-01-01") {
			t.Fatalf("coverage_window notice does not name the requested bound: %q", notice.Message)
		}
	})

	t.Run("READ_ONLY with history succeeds and emits a capture notice", func(t *testing.T) {
		runCaptureNoticeFixture(ctx, t, lab)
	})

	t.Run("OFF with no history after QUERY_STORE CLEAR ALL fails at code 4", func(t *testing.T) {
		runUnavailableFixture(ctx, t, lab)
	})
}

// generateLoadAndWaitForHistory opens a session against dbName (Query
// Store must already be ON there), runs a few harmless SELECTs against
// sys.objects to generate runtime rows, flushes them to disk, and polls
// (bounded to 30s) until diagnostics.ReadHealth reports has_history -
// the same poll budget TestFixtureQueryStoreFlush already measured
// reliable. The session is closed before returning: both fixtures below
// need the database free of any other connection for their own next
// ALTER DATABASE statement (READ_ONLY, or a state change right after).
func generateLoadAndWaitForHistory(ctx context.Context, t *testing.T, lab *Lab, dbName string) {
	t.Helper()
	profile := lab.Profile
	profile.Database = dbName
	sess, err := sqlserver.Open(ctx, profile)
	if err != nil {
		t.Fatalf("open for load: %v", err)
	}
	defer sess.Close()

	for i := 0; i < 5; i++ {
		var n int
		if err := sess.Conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM sys.objects").Scan(&n); err != nil {
			t.Fatalf("generating Query Store load: %v", err)
		}
	}
	if _, err := sess.Conn.ExecContext(ctx, "EXEC sys.sp_query_store_flush_db"); err != nil {
		t.Fatalf("sp_query_store_flush_db: %v", err)
	}

	deadline := time.Now().Add(flushPollDeadline)
	for {
		h, err := diagnostics.ReadHealth(ctx, sess)
		if err != nil {
			t.Fatalf("ReadHealth while polling for history: %v", err)
		}
		if h.HasHistory {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("Query Store history never appeared within %s", flushPollDeadline)
		}
		time.Sleep(flushPollDelay)
	}
}

// runCaptureNoticeFixture is the brief's point 6 measured scenario: a
// throwaway database with real Query Store history, then set READ_ONLY.
// diagnostics.Top must still succeed (design spec: "READ_ONLY history
// remains inspectable with a warning") and must emit a "capture" notice
// naming READ_ONLY.
func runCaptureNoticeFixture(ctx context.Context, t *testing.T, lab *Lab) {
	t.Helper()
	dbName := createThrowawayDatabase(ctx, t, lab, "asq_top_ro_")
	if _, err := lab.Admin.ExecContext(ctx, "ALTER DATABASE ["+dbName+"] SET QUERY_STORE = ON (OPERATION_MODE = READ_WRITE, INTERVAL_LENGTH_MINUTES = 1)"); err != nil {
		t.Fatalf("enabling Query Store: %v", err)
	}
	generateLoadAndWaitForHistory(ctx, t, lab, dbName)

	if _, err := lab.Admin.ExecContext(ctx, "ALTER DATABASE ["+dbName+"] SET READ_ONLY WITH ROLLBACK IMMEDIATE"); err != nil {
		t.Fatalf("setting READ_ONLY: %v", err)
	}
	// Registered after createThrowawayDatabase's own t.Cleanup, so
	// t.Cleanup's LIFO order runs this first: a READ_ONLY database
	// cannot be DROPped, so this restores READ_WRITE before that
	// cleanup's own DROP DATABASE runs (same pattern as
	// status_test.go's own READ_ONLY subtest).
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), podmanCmdTimeout)
		defer cleanupCancel()
		if _, err := lab.Admin.ExecContext(cleanupCtx, "ALTER DATABASE ["+dbName+"] SET READ_WRITE WITH ROLLBACK IMMEDIATE"); err != nil {
			t.Logf("cleanup: restoring %s to READ_WRITE: %v", dbName, err)
		}
	})

	roProfile := lab.Profile
	roProfile.Database = dbName
	roSess, err := sqlserver.Open(ctx, roProfile)
	if err != nil {
		t.Fatalf("open on a READ_ONLY database: %v", err)
	}
	defer roSess.Close()

	sink := &captureSink{}
	opts := diagnostics.TopOptions{
		Window:        diagnostics.Window{Since: time.Now().Add(-time.Hour), Until: time.Now().Add(time.Minute)},
		By:            "cpu",
		Aggregate:     "total",
		Top:           10,
		MinExecutions: 1,
	}
	if err := diagnostics.Top(ctx, roSess, opts, sink); err != nil {
		t.Fatalf("Top on a READ_ONLY database with history should succeed, got: %v", err)
	}
	notice := sink.noticeWithKind("capture")
	if notice == nil {
		t.Fatalf("no capture notice emitted: %+v", sink.notices)
	}
	if !strings.Contains(notice.Message, "READ_ONLY") {
		t.Fatalf("capture notice does not name READ_ONLY: %q", notice.Message)
	}
}

// runUnavailableFixture is the brief's other point 6 measured scenario:
// a throwaway database that genuinely had Query Store history, then
// ALTER DATABASE ... SET QUERY_STORE CLEAR ALL purges it, then QUERY_
// STORE is turned OFF. diagnostics.Top must fail at code 4,
// query_store_unavailable - the state is non-collecting and, after the
// clear, has_history is false again, exactly as if it had never been
// collected at all.
func runUnavailableFixture(ctx context.Context, t *testing.T, lab *Lab) {
	t.Helper()
	dbName := createThrowawayDatabase(ctx, t, lab, "asq_top_off_")
	if _, err := lab.Admin.ExecContext(ctx, "ALTER DATABASE ["+dbName+"] SET QUERY_STORE = ON (OPERATION_MODE = READ_WRITE, INTERVAL_LENGTH_MINUTES = 1)"); err != nil {
		t.Fatalf("enabling Query Store: %v", err)
	}
	generateLoadAndWaitForHistory(ctx, t, lab, dbName)

	if _, err := lab.Admin.ExecContext(ctx, "ALTER DATABASE ["+dbName+"] SET QUERY_STORE CLEAR ALL"); err != nil {
		t.Fatalf("clearing Query Store: %v", err)
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
	opts := diagnostics.TopOptions{
		Window:        diagnostics.Window{Since: time.Now().Add(-time.Hour), Until: time.Now().Add(time.Minute)},
		By:            "cpu",
		Aggregate:     "total",
		Top:           10,
		MinExecutions: 1,
	}
	topErr := diagnostics.Top(ctx, offSess, opts, sink)
	var pub *model.PublicError
	if !errors.As(topErr, &pub) {
		t.Fatalf("Top on an OFF database with cleared history should fail with a *model.PublicError, got: %v", topErr)
	}
	if pub.Code != 4 {
		t.Fatalf("code: got %d, want 4", pub.Code)
	}
	if pub.Kind != "query_store_unavailable" {
		t.Fatalf("kind: got %q, want %q", pub.Kind, "query_store_unavailable")
	}
}
