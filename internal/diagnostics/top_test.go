package diagnostics

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/rudi-bruchez/argosql/internal/model"
	"github.com/rudi-bruchez/argosql/internal/sqlserver"
)

// fakeTopRow is one already-ranked row the fake driver below hands back
// for Top's own query, as if a real engine had already run
// top_2019.sql/top_2022.sql's aggregation against it - see
// TestTopAggregation's own doc comment for why this test precomputes
// these numbers by hand rather than executing the real SQL text (which
// only a live SQL Server, exercised by tests/integration/top_test.go,
// can do).
type fakeTopRow struct {
	queryID, replicaGroupID                              int64
	replicaGroupNull                                     bool
	executions                                           int64
	cpuTotalMs, cpuAvgMs, durationTotalMs, durationAvgMs float64
	readsTotal, readsAvg                                 float64
}

// fakeTopRows is a driver.Rows over fakeTopRow values, in the exact
// column order TopQueriesTable declares.
type fakeTopRows struct {
	data []fakeTopRow
	idx  int
}

func (r *fakeTopRows) Columns() []string {
	names := make([]string, len(TopQueriesTable.Columns))
	for i, c := range TopQueriesTable.Columns {
		names[i] = c.Name
	}
	return names
}

func (r *fakeTopRows) Close() error { return nil }

func (r *fakeTopRows) ColumnTypeDatabaseTypeName(i int) string {
	switch TopQueriesTable.Columns[i].Name {
	case "query_id", "replica_group_id", "executions":
		return "BIGINT"
	default:
		return "FLOAT"
	}
}

func (r *fakeTopRows) Next(dest []driver.Value) error {
	if r.idx >= len(r.data) {
		return io.EOF
	}
	row := r.data[r.idx]
	r.idx++
	dest[0] = row.queryID
	if row.replicaGroupNull {
		dest[1] = nil
	} else {
		dest[1] = row.replicaGroupID
	}
	dest[2] = row.executions
	dest[3] = row.cpuTotalMs
	dest[4] = row.cpuAvgMs
	dest[5] = row.durationTotalMs
	dest[6] = row.durationAvgMs
	dest[7] = row.readsTotal
	dest[8] = row.readsAvg
	return nil
}

// fakeHealthRows is a driver.Rows over health.sql's own nine columns
// (see health.go's colDesired..colHasHistory), reporting a plain
// READ_WRITE state with history - the baseline every fakeTopConn below
// answers ReadHealth with, so Top proceeds straight to its own ranking
// query without a code-4 short-circuit or a capture/coverage notice.
type fakeHealthRows struct{ done bool }

func (r *fakeHealthRows) Columns() []string {
	return []string{"desired_state", "actual_state", "readonly_reason", "capture_mode", "current_storage_mb", "max_storage_mb", "retention_days", "interval_minutes", "has_history"}
}
func (r *fakeHealthRows) Close() error { return nil }
func (r *fakeHealthRows) ColumnTypeDatabaseTypeName(i int) string {
	switch i {
	case 0, 1, 3:
		return "NVARCHAR"
	case 2, 6, 7:
		return "BIGINT"
	case 4, 5:
		return "DECIMAL"
	default:
		return "BIT"
	}
}
func (r *fakeHealthRows) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	dest[0] = "READ_WRITE"
	dest[1] = "READ_WRITE"
	dest[2] = int64(0)
	dest[3] = "ALL"
	dest[4] = []byte("0.00")
	dest[5] = []byte("10.00")
	dest[6] = int64(30)
	dest[7] = int64(1)
	dest[8] = true
	r.done = true
	return nil
}

// fakeCoverageRows is a driver.Rows over coverage.sql's own two
// columns (oldest_interval, newest_interval), reporting no stored
// interval at all (both NULL) - the simplest fixture that still lets
// Top's "ranking" row carry a typed NULL for coverage_oldest/newest,
// and deliberately avoids triggering the "coverage_window" notice
// (proven against a real engine instead, by tests/integration).
type fakeCoverageRows struct{ done bool }

func (r *fakeCoverageRows) Columns() []string                     { return []string{"oldest_interval", "newest_interval"} }
func (r *fakeCoverageRows) Close() error                          { return nil }
func (r *fakeCoverageRows) ColumnTypeDatabaseTypeName(int) string { return "DATETIME2" }
func (r *fakeCoverageRows) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	dest[0] = nil
	dest[1] = nil
	r.done = true
	return nil
}

// fakeTopConn is a minimal database/sql/driver.Conn answering exactly
// the three queries Top ever issues: the shared health.sql read, the
// shared coverage.sql read (both matched by a distinctive substring),
// and Top's own ranking query (everything else) - recording the named
// parameters the ranking query was bound with, so TestTopAggregation
// can assert on both without a real server, the same technique
// internal/sqlserver/testdriver_test.go already established for that
// package's own unit tests.
type fakeTopConn struct {
	data []fakeTopRow
	args []driver.NamedValue
}

func (c *fakeTopConn) Prepare(query string) (driver.Stmt, error) {
	return nil, errors.New("fakeTopConn: Prepare not supported, use QueryContext")
}
func (c *fakeTopConn) Close() error { return nil }
func (c *fakeTopConn) Begin() (driver.Tx, error) {
	return nil, errors.New("fakeTopConn: Begin not supported")
}
func (c *fakeTopConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if strings.Contains(query, "database_query_store_options") {
		return &fakeHealthRows{}, nil
	}
	if strings.Contains(query, "oldest_interval") {
		return &fakeCoverageRows{}, nil
	}
	c.args = args
	return &fakeTopRows{data: c.data}, nil
}

type fakeTopDriver struct{}

func (d *fakeTopDriver) Open(name string) (driver.Conn, error) {
	return nil, errors.New("fakeTopDriver: Open not supported, use the connector")
}

type fakeTopConnector struct{ conn *fakeTopConn }

func (c *fakeTopConnector) Connect(ctx context.Context) (driver.Conn, error) { return c.conn, nil }
func (c *fakeTopConnector) Driver() driver.Driver                            { return &fakeTopDriver{} }

// namedArg looks up a driver.NamedValue by name, case-insensitively.
func namedArg(args []driver.NamedValue, name string) (driver.Value, bool) {
	for _, a := range args {
		if strings.EqualFold(a.Name, name) {
			return a.Value, true
		}
	}
	return nil, false
}

// captureSink is a minimal model.Sink keeping every table's rows (and
// every notice) in memory - enough for this test to inspect what Top
// wrote without going through internal/artifacts or internal/cli. Top
// now writes two tables ("ranking" then "queries"): a single-table sink
// that only ever remembered the most recent Begin would silently
// concatenate both tables' rows together under "queries"'s name, so
// this mirrors tests/integration/status_test.go's own multi-table
// captureSink shape.
type captureSink struct {
	tables  []capturedTable
	cur     *capturedTable
	notices []model.Notice
}

type capturedTable struct {
	spec model.TableSpec
	rows [][]model.Cell
}

func (s *captureSink) Begin(spec model.TableSpec) error {
	s.cur = &capturedTable{spec: spec}
	return nil
}
func (s *captureSink) Row(row []model.Cell) error {
	s.cur.rows = append(s.cur.rows, row)
	return nil
}
func (s *captureSink) End(bool, bool) error {
	s.tables = append(s.tables, *s.cur)
	s.cur = nil
	return nil
}
func (s *captureSink) File(kind, suffix string, src io.Reader) (model.Artifact, error) {
	return model.Artifact{}, fmt.Errorf("captureSink: File not supported")
}
func (s *captureSink) Notice(n model.Notice) { s.notices = append(s.notices, n) }

func (s *captureSink) table(name string) *capturedTable {
	for i := range s.tables {
		if s.tables[i].spec.Name == name {
			return &s.tables[i]
		}
	}
	return nil
}

// TestTopAggregation is the task 10 brief's other red-phase test. It
// cannot execute the real embedded SQL - this package's own unit tests
// never touch a live server (see health_test.go: ReadHealth/Status/Info
// are integration-tested only, their own SQL-querying paths carry zero
// unit coverage) - so it proves two separate things pure Go can prove
// without one:
//
//  1. TopQueriesTable declares exactly the nine named columns the
//     design spec requires, in order - the assertion the brief names
//     for the SQL sketch's extension (a missing column fails here).
//  2. Top() forwards a driver's rows through to the Sink with exact
//     numeric fidelity (int64 executions, float64 metrics compared with
//     an absolute 1e-9 tolerance, never a textual comparison) and binds
//     every one of its own SQL parameters correctly - including two
//     rows sharing the same query_id but a different replica_group_id
//     staying two distinct output rows, never silently merged by Go.
//
// The actual aggregation arithmetic this test's fixture numbers came
// from by hand - (count=1, avg_cpu_us=1000) and (count=9,
// avg_cpu_us=3000) on the same plan/interval summing to executions=10,
// cpu_total_ms=28, cpu_avg_ms=2.8, per the design spec's sum(avg*count)
// formula - is proven against a real engine only by
// tests/integration/top_test.go's synthetic temp-table fixture, which
// this test's canned rows are computed to agree with.
func TestTopAggregation(t *testing.T) {
	wantColumns := []string{
		"query_id", "replica_group_id", "executions",
		"cpu_total_ms", "cpu_avg_ms",
		"duration_total_ms", "duration_avg_ms",
		"reads_total", "reads_avg",
	}
	if len(TopQueriesTable.Columns) != len(wantColumns) {
		t.Fatalf("TopQueriesTable: got %d columns, want %d: %+v", len(TopQueriesTable.Columns), len(wantColumns), TopQueriesTable.Columns)
	}
	for i, name := range wantColumns {
		if TopQueriesTable.Columns[i].Name != name {
			t.Fatalf("TopQueriesTable.Columns[%d]: got %q, want %q", i, TopQueriesTable.Columns[i].Name, name)
		}
	}

	conn := &fakeTopConn{data: []fakeTopRow{
		// query 100, replica group 1: the brief's exact fixture -
		// (count=1, avg_cpu_us=1000) + (count=9, avg_cpu_us=3000) on the
		// same plan/interval, already summed by the real CTE's
		// GROUP BY plan_id/runtime_stats_interval_id.
		{queryID: 100, replicaGroupID: 1, executions: 10,
			cpuTotalMs: 28, cpuAvgMs: 2.8,
			durationTotalMs: 50, durationAvgMs: 5,
			readsTotal: 100, readsAvg: 10},
		// query 100, replica group 2: "another plan, same query, another
		// replica_group, kept as a distinct row" - proves Top does not
		// collapse two rows sharing a query_id when their
		// replica_group_id differs.
		{queryID: 100, replicaGroupID: 2, executions: 5,
			cpuTotalMs: 15, cpuAvgMs: 3,
			durationTotalMs: 20, durationAvgMs: 4,
			readsTotal: 40, readsAvg: 8},
	}}
	db := sql.OpenDB(&fakeTopConnector{conn: conn})
	defer db.Close()
	sqlConn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatalf("db.Conn: %v", err)
	}
	defer sqlConn.Close()

	sess := &sqlserver.Session{Conn: sqlConn, Major: 16}
	now := time.Now().UTC()
	win := Window{Since: now.Add(-24 * time.Hour), Until: now}
	opts := TopOptions{Window: win, By: "cpu", Aggregate: "total", Top: 10, MinExecutions: 1}

	sink := &captureSink{}
	if err := Top(context.Background(), sess, opts, sink); err != nil {
		t.Fatalf("Top: %v", err)
	}

	queries := sink.table(TopQueriesTable.Name)
	if queries == nil {
		t.Fatalf("no %q table in Top's output: %+v", TopQueriesTable.Name, sink.tables)
	}
	if len(queries.rows) != 2 {
		t.Fatalf("got %d rows, want 2: %+v", len(queries.rows), queries.rows)
	}

	row := queries.rows[0]
	if got, ok := row[0].(int64); !ok || got != 100 {
		t.Fatalf("row 0 query_id: got %#v, want int64(100)", row[0])
	}
	if got, ok := row[1].(int64); !ok || got != 1 {
		t.Fatalf("row 0 replica_group_id: got %#v, want int64(1)", row[1])
	}
	if got, ok := row[2].(int64); !ok || got != 10 {
		t.Fatalf("row 0 executions: got %#v, want int64(10)", row[2])
	}
	cpuTotal, ok := row[3].(float64)
	if !ok || math.Abs(cpuTotal-28) > 1e-9 {
		t.Fatalf("row 0 cpu_total_ms: got %#v, want 28 (tolerance 1e-9)", row[3])
	}
	cpuAvg, ok := row[4].(float64)
	if !ok || math.Abs(cpuAvg-2.8) > 1e-9 {
		t.Fatalf("row 0 cpu_avg_ms: got %#v, want 2.8 (tolerance 1e-9)", row[4])
	}

	row1 := queries.rows[1]
	if got, ok := row1[1].(int64); !ok || got != 2 {
		t.Fatalf("row 1 replica_group_id: got %#v, want int64(2) (distinct from row 0's replica group)", row1[1])
	}
	if got, ok := row1[0].(int64); !ok || got != 100 {
		t.Fatalf("row 1 query_id: got %#v, want int64(100) (same query as row 0)", row1[0])
	}

	sinceArg, ok := namedArg(conn.args, "since")
	if !ok {
		t.Fatal("ranking query missing its @since parameter")
	}
	if got, ok := sinceArg.(time.Time); !ok || !got.Equal(win.Since) {
		t.Fatalf("@since: got %#v, want %v", sinceArg, win.Since)
	}
	untilArg, ok := namedArg(conn.args, "until")
	if !ok {
		t.Fatal("ranking query missing its @until parameter")
	}
	if got, ok := untilArg.(time.Time); !ok || !got.Equal(win.Until) {
		t.Fatalf("@until: got %#v, want %v", untilArg, win.Until)
	}
	if topArg, ok := namedArg(conn.args, "top"); !ok || topArg != int64(10) {
		t.Fatalf("@top: got %#v, want int64(10)", topArg)
	}
	if minExecArg, ok := namedArg(conn.args, "min_executions"); !ok || minExecArg != int64(1) {
		t.Fatalf("@min_executions: got %#v, want int64(1)", minExecArg)
	}
	if objArg, ok := namedArg(conn.args, "object_id"); !ok || objArg != nil {
		t.Fatalf("@object_id: got %#v, want nil (no --object given)", objArg)
	}
}
