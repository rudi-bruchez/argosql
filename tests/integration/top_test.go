//go:build integration

package integration

import (
	"context"
	"database/sql"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rudi-bruchez/argosql/internal/diagnostics"
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
// the SAME query but a DIFFERENT replica_group_id, which must stay a
// distinct ranked row on 2022 (replica_group_id grouped) and combine
// into the same row as plan 10 on 2019 (no replica_group_id column at
// all); and two rows with execution_type 3 and 4, which must be
// excluded from every total.
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
		{int64(11), int64(1), int64(2), 0, int64(4), int64(500), int64(500), int64(5)},
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
		if len(got) != 2 {
			t.Fatalf("2022: got %d rows, want 2 (one per replica group): %+v", len(got), got)
		}
		first, second := got[0], got[1]
		if first.executions != 10 {
			t.Fatalf("row 0 executions: got %d, want 10", first.executions)
		}
		if math.Abs(first.cpuTotalMs-28) > tol {
			t.Fatalf("row 0 cpu_total_ms: got %v, want 28 (tolerance %v)", first.cpuTotalMs, tol)
		}
		if math.Abs(first.cpuAvgMs-2.8) > tol {
			t.Fatalf("row 0 cpu_avg_ms: got %v, want 2.8 (tolerance %v)", first.cpuAvgMs, tol)
		}
		if !first.replicaGroup.Valid || first.replicaGroup.Int64 != 1 {
			t.Fatalf("row 0 replica_group_id: got %+v, want 1", first.replicaGroup)
		}
		if !second.replicaGroup.Valid || second.replicaGroup.Int64 != 2 {
			t.Fatalf("row 1 replica_group_id: got %+v, want 2 (kept distinct from row 0)", second.replicaGroup)
		}
		if second.executions != 4 {
			t.Fatalf("row 1 executions: got %d, want 4", second.executions)
		}
	} else {
		// 2019 has no replica_group_id column at all: plan 10 (replica
		// group 1) and plan 11 (replica group 2, meaningless on 2019)
		// combine into ONE row for query 100.
		if len(got) != 1 {
			t.Fatalf("2019: got %d rows, want 1 (no replica grouping): %+v", len(got), got)
		}
		row := got[0]
		if row.executions != 14 {
			t.Fatalf("executions: got %d, want 14 (10 + 4, plans combined)", row.executions)
		}
		wantTotal := 30.0 // (1*1000 + 9*3000 + 4*500) / 1000
		wantAvg := 30000.0 / 14 / 1000.0
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

// TestTop exercises diagnostics.Top against a real engine, in two
// halves: runExactAggregationFixture above proves the real, embedded
// aggregation formula and its 2019/2022 replica_group_id divergence
// against synthetic temp-table data (sys.query_store_runtime_stats
// cannot be written to directly); the second half proves the full
// pipeline - health, window, SQL dispatch, row scanning - against a
// real Query Store entry discovered by Lab.QueryID's marker mechanism,
// the same helper task 11 reuses rather than redefining.
func TestTop(t *testing.T) {
	lab := NewLab(t, os.Getenv("ASQ_TEST_IMAGE"))
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	sess, err := sqlserver.Open(ctx, lab.Profile)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer sess.Close()
	logEngineIdentity(t, lab, sess.Major)

	runExactAggregationFixture(ctx, t, lab, sess.Major)

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

	queries := sink.table("queries")
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
}
