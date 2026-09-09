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
// (see health.go's colDesired..colHasHistory), reporting desired/actual
// state and capture mode as configured (both default to a plain,
// fully-collecting READ_WRITE/ALL when left empty - the baseline every
// fakeTopConn below answers ReadHealth with, so Top proceeds straight
// to its own ranking query without a code-4 short-circuit or a
// capture/capture_mode notice), with history always true.
type fakeHealthRows struct {
	actual, captureMode string
	done                bool
}

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
	actual := r.actual
	if actual == "" {
		actual = "READ_WRITE"
	}
	captureMode := r.captureMode
	if captureMode == "" {
		captureMode = "ALL"
	}
	dest[0] = "READ_WRITE"
	dest[1] = actual
	dest[2] = int64(0)
	dest[3] = captureMode
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
	data                []fakeTopRow
	args                []driver.NamedValue
	actual, captureMode string // forwarded to fakeHealthRows; both default when empty
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
		return &fakeHealthRows{actual: c.actual, captureMode: c.captureMode}, nil
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

// noticeWithKind returns the first recorded notice of the given Kind,
// or nil if none was emitted.
func (s *captureSink) noticeWithKind(kind string) *model.Notice {
	for i := range s.notices {
		if s.notices[i].Kind == kind {
			return &s.notices[i]
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
	// B6: SQLType, not just Name, is checked here too - it decides
	// internal/output/json.go's rendering (bigint-as-string vs a bare
	// JSON number). Measured: changing "executions" from BIGINT to
	// FLOAT left this check green before SQLType was compared, since
	// only the column names were verified.
	wantColumns := []model.Column{
		{Name: "query_id", SQLType: "BIGINT"},
		{Name: "replica_group_id", SQLType: "BIGINT"},
		{Name: "executions", SQLType: "BIGINT"},
		{Name: "cpu_total_ms", SQLType: "FLOAT"},
		{Name: "cpu_avg_ms", SQLType: "FLOAT"},
		{Name: "duration_total_ms", SQLType: "FLOAT"},
		{Name: "duration_avg_ms", SQLType: "FLOAT"},
		{Name: "reads_total", SQLType: "FLOAT"},
		{Name: "reads_avg", SQLType: "FLOAT"},
	}
	if len(TopQueriesTable.Columns) != len(wantColumns) {
		t.Fatalf("TopQueriesTable: got %d columns, want %d: %+v", len(TopQueriesTable.Columns), len(wantColumns), TopQueriesTable.Columns)
	}
	for i, want := range wantColumns {
		if TopQueriesTable.Columns[i] != want {
			t.Fatalf("TopQueriesTable.Columns[%d]: got %+v, want %+v", i, TopQueriesTable.Columns[i], want)
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

// newFakeTopSession builds a *sqlserver.Session backed by conn,
// registering db's Close on t - the same fake-driver plumbing
// TestTopAggregation builds inline, factored out for the tests below,
// which only need Top's health/notice behavior, not its ranking rows.
func newFakeTopSession(t *testing.T, conn *fakeTopConn) *sqlserver.Session {
	t.Helper()
	db := sql.OpenDB(&fakeTopConnector{conn: conn})
	t.Cleanup(func() { db.Close() })
	sqlConn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatalf("db.Conn: %v", err)
	}
	t.Cleanup(func() { sqlConn.Close() })
	return &sqlserver.Session{Conn: sqlConn, Major: 16}
}

// TestTopCaptureModeNotice is fix-2's A1: Health carries capture_mode,
// and Top emits a "capture_mode" notice whenever it is not ALL -
// independent of the collecting state itself (a READ_WRITE database
// can still have capture mode NONE, AUTO or CUSTOM). Measured by a
// reviewer before this fix, with a fake driver answering health.sql's
// capture_mode cell alone: all four modes produced err=nil,
// notices=[] from a real Top call - nothing could tell NONE from ALL.
func TestTopCaptureModeNotice(t *testing.T) {
	cases := []struct {
		mode       string
		wantNotice bool
		wantSubstr string
	}{
		{"ALL", false, ""},
		{"NONE", true, "NONE"},
		{"AUTO", true, "AUTO"},
		{"CUSTOM", true, "CUSTOM"},
	}
	for _, c := range cases {
		t.Run(c.mode, func(t *testing.T) {
			sess := newFakeTopSession(t, &fakeTopConn{captureMode: c.mode})
			now := time.Now().UTC()
			opts := TopOptions{Window: Window{Since: now.Add(-time.Hour), Until: now}, By: "cpu", Aggregate: "total", Top: 10, MinExecutions: 1}
			sink := &captureSink{}
			if err := Top(context.Background(), sess, opts, sink); err != nil {
				t.Fatalf("Top: %v", err)
			}
			notice := sink.noticeWithKind("capture_mode")
			if c.wantNotice && notice == nil {
				t.Fatalf("mode %s: expected a capture_mode notice, got none: %+v", c.mode, sink.notices)
			}
			if !c.wantNotice && notice != nil {
				t.Fatalf("mode %s: expected no capture_mode notice, got %+v", c.mode, notice)
			}
			if notice != nil && !strings.Contains(notice.Message, c.wantSubstr) {
				t.Fatalf("mode %s: notice message %q does not mention %q", c.mode, notice.Message, c.wantSubstr)
			}
		})
	}
}

// TestTopCaptureNoticeCoversReadCaptureSecondary is the other half of
// A1: the "capture" notice (non-READ_WRITE state) must also fire for
// READ_CAPTURE_SECONDARY, which nonCollectingStates deliberately
// excludes (it is not a code-4 state) but which the design spec's
// "non-READ_WRITE state" warning does not exempt.
func TestTopCaptureNoticeCoversReadCaptureSecondary(t *testing.T) {
	sess := newFakeTopSession(t, &fakeTopConn{actual: "READ_CAPTURE_SECONDARY"})
	now := time.Now().UTC()
	opts := TopOptions{Window: Window{Since: now.Add(-time.Hour), Until: now}, By: "cpu", Aggregate: "total", Top: 10, MinExecutions: 1}
	sink := &captureSink{}
	if err := Top(context.Background(), sess, opts, sink); err != nil {
		t.Fatalf("Top: %v", err)
	}
	notice := sink.noticeWithKind("capture")
	if notice == nil {
		t.Fatalf("expected a capture notice for READ_CAPTURE_SECONDARY, got none: %+v", sink.notices)
	}
	if !strings.Contains(notice.Message, "READ_CAPTURE_SECONDARY") {
		t.Fatalf("capture notice does not name READ_CAPTURE_SECONDARY: %q", notice.Message)
	}
}

// TestOrderColumnForAllValidPairs is B4: every one of the seven valid
// (--by, --aggregate) combinations must resolve to its own, distinct
// literal column name - not just the one pair ("cpu","total") every
// other test in this package happens to exercise. Measured: pointing
// ("cpu","total") at "duration_total_ms" in the orderColumns map left
// every existing test in this repository green.
func TestOrderColumnForAllValidPairs(t *testing.T) {
	cases := []struct{ by, aggregate, want string }{
		{"cpu", "total", "cpu_total_ms"},
		{"cpu", "avg", "cpu_avg_ms"},
		{"duration", "total", "duration_total_ms"},
		{"duration", "avg", "duration_avg_ms"},
		{"reads", "total", "reads_total"},
		{"reads", "avg", "reads_avg"},
		{"executions", "total", "executions"},
	}
	for _, c := range cases {
		t.Run(c.by+"/"+c.aggregate, func(t *testing.T) {
			got, err := orderColumnFor(c.by, c.aggregate)
			if err != nil {
				t.Fatalf("orderColumnFor(%q, %q): %v", c.by, c.aggregate, err)
			}
			if got != c.want {
				t.Fatalf("orderColumnFor(%q, %q) = %q, want %q", c.by, c.aggregate, got, c.want)
			}
		})
	}
}

// TestTopRankingAndCoverageTableTypes is the rest of B6: TopRankingTable
// (ten columns) and CoverageTable (three columns, its two timestamps
// fixed to DATETIMEOFFSET by A5) declare exactly what they claim.
func TestTopRankingAndCoverageTableTypes(t *testing.T) {
	wantRanking := []model.Column{
		{Name: "requested_since", SQLType: "DATETIMEOFFSET"},
		{Name: "requested_until", SQLType: "DATETIMEOFFSET"},
		{Name: "coverage_oldest", SQLType: "DATETIMEOFFSET"},
		{Name: "coverage_newest", SQLType: "DATETIMEOFFSET"},
		{Name: "by", SQLType: "NVARCHAR"},
		{Name: "aggregate", SQLType: "NVARCHAR"},
		{Name: "top", SQLType: "INT"},
		{Name: "min_executions", SQLType: "BIGINT"},
		{Name: "include_internal", SQLType: "BIT"},
		{Name: "parent_module", SQLType: "NVARCHAR"},
	}
	if len(TopRankingTable.Columns) != len(wantRanking) {
		t.Fatalf("TopRankingTable: got %d columns, want %d: %+v", len(TopRankingTable.Columns), len(wantRanking), TopRankingTable.Columns)
	}
	for i, want := range wantRanking {
		if TopRankingTable.Columns[i] != want {
			t.Fatalf("TopRankingTable.Columns[%d]: got %+v, want %+v", i, TopRankingTable.Columns[i], want)
		}
	}

	wantCoverage := []model.Column{
		{Name: "oldest_interval", SQLType: "DATETIMEOFFSET"},
		{Name: "newest_interval", SQLType: "DATETIMEOFFSET"},
		{Name: "has_history", SQLType: "BIT"},
	}
	if len(CoverageTable.Columns) != len(wantCoverage) {
		t.Fatalf("CoverageTable: got %d columns, want %d: %+v", len(CoverageTable.Columns), len(wantCoverage), CoverageTable.Columns)
	}
	for i, want := range wantCoverage {
		if CoverageTable.Columns[i] != want {
			t.Fatalf("CoverageTable.Columns[%d]: got %+v, want %+v", i, CoverageTable.Columns[i], want)
		}
	}
}
