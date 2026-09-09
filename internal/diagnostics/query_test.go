package diagnostics

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/rudi-bruchez/argosql/internal/model"
	"github.com/rudi-bruchez/argosql/internal/sqlserver"

	mssql "github.com/microsoft/go-mssqldb"
)

// fakeStaticRows is a driver.Rows over a fixed, in-memory table: a
// column-name/DatabaseTypeName list and a slice of already-typed
// driver.Value rows. Used below for both query.sql's identity row and
// query_plans_*.sql's plan rows - two fixed-shape result sets that do
// not need two near-identical bespoke row types, unlike top_test.go's
// fakeTopRow (whose shape top.go's own tests reuse across many cases).
type fakeStaticRows struct {
	cols  []string
	types []string
	data  [][]driver.Value
	idx   int
}

func (r *fakeStaticRows) Columns() []string { return r.cols }
func (r *fakeStaticRows) Close() error      { return nil }
func (r *fakeStaticRows) ColumnTypeDatabaseTypeName(i int) string {
	return r.types[i]
}
func (r *fakeStaticRows) Next(dest []driver.Value) error {
	if r.idx >= len(r.data) {
		return io.EOF
	}
	copy(dest, r.data[r.idx])
	r.idx++
	return nil
}

// identityColumns/identityTypes and plansColumns/plansTypes are
// query.sql's and query_plans_*.sql's own column shapes, named once so
// every fakeQueryConn response below builds its fakeStaticRows from the
// same declared shape.
var (
	identityColumns = []string{"query_id", "object_id", "parent_schema", "parent_name", "is_internal_query", "query_hash", "query_sql_text"}
	identityTypes   = []string{"BIGINT", "INT", "NVARCHAR", "NVARCHAR", "BIT", "BINARY", "NVARCHAR"}
	plansColumns    = []string{"plan_id", "replica_group_id", "forced", "executions", "cpu_total_ms", "cpu_avg_ms", "duration_total_ms", "duration_avg_ms", "reads_total", "reads_avg"}
	plansTypes      = []string{"BIGINT", "BIGINT", "BIT", "BIGINT", "FLOAT", "FLOAT", "FLOAT", "FLOAT", "FLOAT", "FLOAT"}
)

// fakeQueryConn is a minimal database/sql/driver.Conn answering exactly
// the three queries Query ever issues, matched by a distinctive
// substring exactly like top_test.go's own fakeTopConn: the shared
// health.sql read, the shared coverage.sql read, query.sql's identity
// lookup (identityRow == nil means zero rows: query_id not found), and
// query_plans_*.sql's plan rows.
type fakeQueryConn struct {
	actual, captureMode string
	noHistory           bool

	identityRow  []driver.Value
	identityErr  error
	identityArgs []driver.NamedValue

	plans     [][]driver.Value
	plansErr  error
	plansArgs []driver.NamedValue
	plansRows *fakeStaticRows // set by QueryContext, for TestQueryRowsStreamsWithoutAccumulating to inspect how far it advanced
}

func (c *fakeQueryConn) Prepare(query string) (driver.Stmt, error) {
	return nil, errors.New("fakeQueryConn: Prepare not supported, use QueryContext")
}
func (c *fakeQueryConn) Close() error { return nil }
func (c *fakeQueryConn) Begin() (driver.Tx, error) {
	return nil, errors.New("fakeQueryConn: Begin not supported")
}
func (c *fakeQueryConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	switch {
	case strings.Contains(query, "database_query_store_options"):
		return &fakeHealthRows{actual: c.actual, captureMode: c.captureMode, noHistory: c.noHistory}, nil
	case strings.Contains(query, "oldest_interval"):
		return &fakeCoverageRows{}, nil
	case strings.Contains(query, "query_store_query_text"):
		c.identityArgs = args
		if c.identityErr != nil {
			return nil, c.identityErr
		}
		var data [][]driver.Value
		if c.identityRow != nil {
			data = [][]driver.Value{c.identityRow}
		}
		return &fakeStaticRows{cols: identityColumns, types: identityTypes, data: data}, nil
	default:
		c.plansArgs = args
		if c.plansErr != nil {
			return nil, c.plansErr
		}
		c.plansRows = &fakeStaticRows{cols: plansColumns, types: plansTypes, data: c.plans}
		return c.plansRows, nil
	}
}

type fakeQueryDriver struct{}

func (d *fakeQueryDriver) Open(name string) (driver.Conn, error) {
	return nil, errors.New("fakeQueryDriver: Open not supported, use the connector")
}

type fakeQueryConnector struct{ conn *fakeQueryConn }

func (c *fakeQueryConnector) Connect(ctx context.Context) (driver.Conn, error) { return c.conn, nil }
func (c *fakeQueryConnector) Driver() driver.Driver                            { return &fakeQueryDriver{} }

// newFakeQuerySession builds a *sqlserver.Session backed by conn, the
// same fake-driver plumbing top_test.go's newFakeTopSession builds,
// factored out here since every test below needs it and none needs
// Top's own fixture rows.
func newFakeQuerySession(t *testing.T, conn *fakeQueryConn) *sqlserver.Session {
	t.Helper()
	db := sql.OpenDB(&fakeQueryConnector{conn: conn})
	t.Cleanup(func() { db.Close() })
	sqlConn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatalf("db.Conn: %v", err)
	}
	t.Cleanup(func() { sqlConn.Close() })
	return &sqlserver.Session{Conn: sqlConn, Major: 16}
}

// queryCaptureSink is a minimal model.Sink keeping every table's rows,
// every notice, and every File call's content in memory - unlike
// top_test.go's own captureSink (same package), whose File always
// errors: that sink predates any diagnostic in this package ever
// calling File, and Query always does (its one .sql artifact).
// Renamed to avoid colliding with that existing type.
type queryCaptureSink struct {
	tables  []queryCapturedTable
	cur     *queryCapturedTable
	notices []model.Notice
	files   []queryCapturedFile
}

type queryCapturedTable struct {
	spec                                   model.TableSpec
	rows                                   [][]model.Cell
	collectionComplete, propertiesComplete bool
}

type queryCapturedFile struct {
	kind, suffix string
	content      []byte
}

func (s *queryCaptureSink) Begin(spec model.TableSpec) error {
	s.cur = &queryCapturedTable{spec: spec}
	return nil
}
func (s *queryCaptureSink) Row(row []model.Cell) error {
	if s.cur == nil {
		return fmt.Errorf("queryCaptureSink: Row called with no open table")
	}
	s.cur.rows = append(s.cur.rows, row)
	return nil
}
func (s *queryCaptureSink) End(collectionComplete, propertiesComplete bool) error {
	if s.cur == nil {
		return fmt.Errorf("queryCaptureSink: End called with no open table")
	}
	s.cur.collectionComplete = collectionComplete
	s.cur.propertiesComplete = propertiesComplete
	s.tables = append(s.tables, *s.cur)
	s.cur = nil
	return nil
}
func (s *queryCaptureSink) File(kind, suffix string, src io.Reader) (model.Artifact, error) {
	b, err := io.ReadAll(src)
	if err != nil {
		return model.Artifact{}, err
	}
	s.files = append(s.files, queryCapturedFile{kind: kind, suffix: suffix, content: b})
	return model.Artifact{Kind: kind, Path: "(captured in memory)", Bytes: int64(len(b)), Complete: true}, nil
}
func (s *queryCaptureSink) Notice(n model.Notice) { s.notices = append(s.notices, n) }

func (s *queryCaptureSink) table(name string) *queryCapturedTable {
	for i := range s.tables {
		if s.tables[i].spec.Name == name {
			return &s.tables[i]
		}
	}
	return nil
}

func (s *queryCaptureSink) noticeWithKind(kind string) *model.Notice {
	for i := range s.notices {
		if s.notices[i].Kind == kind {
			return &s.notices[i]
		}
	}
	return nil
}

func (s *queryCaptureSink) fileContent(kind string) []byte {
	for i := range s.files {
		if s.files[i].kind == kind {
			return s.files[i].content
		}
	}
	return nil
}

func defaultWindow() Window {
	now := time.Now().UTC()
	return Window{Since: now.Add(-time.Hour), Until: now}
}

// TestPlansQueryForVersionRouting is fix 1's B1: nothing guarded the
// 2019/2022 SQL routing at all - forcing the 2019 form onto a 2022
// engine left both the unit and integration suites green, because the
// real engine's own replica_group_id column (present on 2022, absent
// on 2019) was never compared against which embedded text actually
// ran. This checks the routing directly, by identity against the two
// embedded constants: major 15 must select query_plans_2019.sql, and
// every major this package accepts as 2022-shaped (16, and 17 for the
// smoke-tested 2025 release, which shares 2022's replica_group_id
// column) must select query_plans_2022.sql - never the reverse.
func TestPlansQueryForVersionRouting(t *testing.T) {
	if got := plansQueryFor(15); got != queryPlans2019 {
		t.Fatalf("plansQueryFor(15) did not select query_plans_2019.sql")
	}
	for _, major := range []int{16, 17} {
		if got := plansQueryFor(major); got != queryPlans2022 {
			t.Fatalf("plansQueryFor(%d) did not select query_plans_2022.sql", major)
		}
	}
	// The two embedded files must actually differ, or the comparisons
	// above would trivially pass regardless of which branch ran.
	if queryPlans2019 == queryPlans2022 {
		t.Fatal("query_plans_2019.sql and query_plans_2022.sql are byte-identical: this test cannot distinguish them")
	}
}

// TestQueryTableColumns is this task's own B6-equivalent: the sketch in
// the brief's SQL block shows four columns for "query" alone; the prose
// extends that to seven, plus an entire ten-column "plans" table beside
// it. Both TableSpecs must declare exactly what the brief's prose, not
// its SQL sketch, requires.
func TestQueryTableColumns(t *testing.T) {
	wantWindow := []model.Column{
		{Name: "requested_since", SQLType: "DATETIMEOFFSET"},
		{Name: "requested_until", SQLType: "DATETIMEOFFSET"},
		{Name: "coverage_oldest", SQLType: "DATETIMEOFFSET"},
		{Name: "coverage_newest", SQLType: "DATETIMEOFFSET"},
	}
	if len(QueryWindowTable.Columns) != len(wantWindow) {
		t.Fatalf("QueryWindowTable: got %d columns, want %d: %+v", len(QueryWindowTable.Columns), len(wantWindow), QueryWindowTable.Columns)
	}
	for i, want := range wantWindow {
		if QueryWindowTable.Columns[i] != want {
			t.Fatalf("QueryWindowTable.Columns[%d]: got %+v, want %+v", i, QueryWindowTable.Columns[i], want)
		}
	}

	wantQuery := []model.Column{
		{Name: "query_id", SQLType: "BIGINT"},
		{Name: "object_id", SQLType: "INT"},
		{Name: "parent_module", SQLType: "NVARCHAR"},
		{Name: "is_internal_query", SQLType: "BIT"},
		{Name: "query_hash", SQLType: "BINARY(8)"},
		{Name: "text_preview", SQLType: "NVARCHAR(MAX)"},
		{Name: "text_artifact", SQLType: "NVARCHAR"},
	}
	if len(QueryTable.Columns) != len(wantQuery) {
		t.Fatalf("QueryTable: got %d columns, want %d: %+v", len(QueryTable.Columns), len(wantQuery), QueryTable.Columns)
	}
	for i, want := range wantQuery {
		if QueryTable.Columns[i] != want {
			t.Fatalf("QueryTable.Columns[%d]: got %+v, want %+v", i, QueryTable.Columns[i], want)
		}
	}

	wantPlans := []model.Column{
		{Name: "plan_id", SQLType: "BIGINT"},
		{Name: "replica_group_id", SQLType: "BIGINT"},
		{Name: "forced", SQLType: "BIT"},
		{Name: "executions", SQLType: "BIGINT"},
		{Name: "cpu_total_ms", SQLType: "FLOAT"},
		{Name: "cpu_avg_ms", SQLType: "FLOAT"},
		{Name: "duration_total_ms", SQLType: "FLOAT"},
		{Name: "duration_avg_ms", SQLType: "FLOAT"},
		{Name: "reads_total", SQLType: "FLOAT"},
		{Name: "reads_avg", SQLType: "FLOAT"},
	}
	if len(PlansTable.Columns) != len(wantPlans) {
		t.Fatalf("PlansTable: got %d columns, want %d: %+v", len(PlansTable.Columns), len(wantPlans), PlansTable.Columns)
	}
	for i, want := range wantPlans {
		if PlansTable.Columns[i] != want {
			t.Fatalf("PlansTable.Columns[%d]: got %+v, want %+v", i, PlansTable.Columns[i], want)
		}
	}
}

// TestQueryNotFound is the brief's "ID manquant=8" case: zero rows from
// query.sql's own identity lookup must report code 8,
// not_found_or_not_visible, and must never reach the plans query at
// all (conn.plansArgs stays nil) - the ordering Query's own doc comment
// documents as load-bearing, not incidental.
func TestQueryNotFound(t *testing.T) {
	conn := &fakeQueryConn{identityRow: nil}
	sess := newFakeQuerySession(t, conn)
	sink := &queryCaptureSink{}

	err := Query(context.Background(), sess, QueryOptions{ID: 999, Window: defaultWindow()}, sink)
	if err == nil {
		t.Fatal("Query: got nil error, want code 8")
	}
	var pub *model.PublicError
	if !errors.As(err, &pub) {
		t.Fatalf("Query: error is not a *model.PublicError: %v", err)
	}
	if pub.Code != 8 {
		t.Fatalf("code: got %d, want 8: %v", pub.Code, pub)
	}
	if pub.Kind != "not_found_or_not_visible" {
		t.Fatalf("kind: got %q, want %q", pub.Kind, "not_found_or_not_visible")
	}
	if conn.plansArgs != nil {
		t.Fatalf("plans query was issued after a not-found identity lookup: args=%v", conn.plansArgs)
	}
}

// TestQueryInternalQuerySucceeds is the brief's "ID interne réussi"
// case: an internal query (is_internal_query = 1) must resolve exactly
// like any other, never treated as not-found (design spec: "Direct
// lookup with qs query <id> returns an existing internal query and
// labels is_internal_query=true, avoiding a misleading not-found
// result"). object_id = 0 here (ad hoc), so parent_module must stay nil
// with no warning notice about it.
func TestQueryInternalQuerySucceeds(t *testing.T) {
	win := defaultWindow()
	conn := &fakeQueryConn{
		identityRow: []driver.Value{int64(42), int64(0), nil, nil, true, []byte{1, 2, 3, 4, 5, 6, 7, 8}, "SELECT 1"},
	}
	sess := newFakeQuerySession(t, conn)
	sink := &queryCaptureSink{}

	if err := Query(context.Background(), sess, QueryOptions{ID: 42, Window: win}, sink); err != nil {
		t.Fatalf("Query: %v", err)
	}

	// Fix 1's A1: the window table is published (requested_since/until,
	// plus coverage_oldest/newest - nil here, fakeCoverageRows' own "no
	// stored interval at all" default), never read and discarded.
	winTbl := sink.table(QueryWindowTable.Name)
	if winTbl == nil || len(winTbl.rows) != 1 {
		t.Fatalf("window table missing or wrong row count: %+v", sink.tables)
	}
	winRow := winTbl.rows[0]
	if got, ok := winRow[0].(string); !ok || got != formatDateTimeOffset(win.Since) {
		t.Fatalf("requested_since: got %#v, want %q", winRow[0], formatDateTimeOffset(win.Since))
	}
	if got, ok := winRow[1].(string); !ok || got != formatDateTimeOffset(win.Until) {
		t.Fatalf("requested_until: got %#v, want %q", winRow[1], formatDateTimeOffset(win.Until))
	}
	if winRow[2] != nil || winRow[3] != nil {
		t.Fatalf("coverage_oldest/newest: got %#v/%#v, want nil/nil (no stored interval)", winRow[2], winRow[3])
	}

	tbl := sink.table(QueryTable.Name)
	if tbl == nil || len(tbl.rows) != 1 {
		t.Fatalf("query table missing or wrong row count: %+v", sink.tables)
	}
	if !tbl.propertiesComplete {
		t.Fatalf("propertiesComplete: got false, want true (ad-hoc query, no parent module to fail to resolve)")
	}
	row := tbl.rows[0]
	if got, ok := row[0].(int64); !ok || got != 42 {
		t.Fatalf("query_id: got %#v, want int64(42)", row[0])
	}
	if row[2] != nil {
		t.Fatalf("parent_module: got %#v, want nil (object_id 0, ad hoc)", row[2])
	}
	if got, ok := row[3].(bool); !ok || !got {
		t.Fatalf("is_internal_query: got %#v, want true", row[3])
	}
	if notice := sink.noticeWithKind("parent_module_unavailable"); notice != nil {
		t.Fatalf("unexpected parent_module_unavailable notice for an ad-hoc query: %+v", notice)
	}
}

// TestQueryParentModuleUnavailable proves query.sql's LEFT JOIN finding
// nothing (a module id that exists but whose name the current
// principal cannot see in the catalog, or that has since been dropped)
// reports parent_module = NULL plus a warning notice, never a failure
// - design spec: "qs query | 0; parent name may be unavailable".
func TestQueryParentModuleUnavailable(t *testing.T) {
	conn := &fakeQueryConn{
		identityRow: []driver.Value{int64(7), int64(55), nil, nil, false, []byte{0, 0, 0, 0, 0, 0, 0, 1}, "SELECT 2"},
	}
	sess := newFakeQuerySession(t, conn)
	sink := &queryCaptureSink{}

	if err := Query(context.Background(), sess, QueryOptions{ID: 7, Window: defaultWindow()}, sink); err != nil {
		t.Fatalf("Query: %v", err)
	}

	tbl := sink.table(QueryTable.Name)
	row := tbl.rows[0]
	if row[1] != int64(55) {
		t.Fatalf("object_id: got %#v, want int64(55)", row[1])
	}
	if row[2] != nil {
		t.Fatalf("parent_module: got %#v, want nil (unresolvable)", row[2])
	}
	// Fix 1's A2: propertiesComplete must go false here, or Render's
	// model.ReasonPropertyUnavailable never reaches omitted_reasons and
	// a consumer reading properties_complete=true is told the identity
	// is fully known when it is not.
	if tbl.propertiesComplete {
		t.Fatalf("propertiesComplete: got true, want false (parent module unresolvable)")
	}
	notice := sink.noticeWithKind("parent_module_unavailable")
	if notice == nil {
		t.Fatalf("expected a parent_module_unavailable notice, got none: %+v", sink.notices)
	}
	if !strings.Contains(notice.Message, "55") {
		t.Fatalf("notice does not name the object_id: %q", notice.Message)
	}
	// Fix 1's A4: the message must not assert the narrower "not
	// visible" when the catalog cannot distinguish that from "no
	// longer exists" - the same not_found_or_not_visible discipline
	// sqlserver.Resolve already follows.
	if !strings.Contains(notice.Message, "not found or not visible") {
		t.Fatalf("notice asserts a narrower cause than the catalog can prove: %q", notice.Message)
	}
}

// TestQueryParentModuleResolved is TestQueryParentModuleUnavailable's
// positive counterpart: a resolvable module reports parent_module as
// "schema.name" and emits no warning notice about it.
func TestQueryParentModuleResolved(t *testing.T) {
	conn := &fakeQueryConn{
		identityRow: []driver.Value{int64(8), int64(66), "dbo", "SomeProc", false, []byte{0, 0, 0, 0, 0, 0, 0, 2}, "SELECT 3"},
	}
	sess := newFakeQuerySession(t, conn)
	sink := &queryCaptureSink{}

	if err := Query(context.Background(), sess, QueryOptions{ID: 8, Window: defaultWindow()}, sink); err != nil {
		t.Fatalf("Query: %v", err)
	}

	tbl := sink.table(QueryTable.Name)
	row := tbl.rows[0]
	if got, ok := row[2].(string); !ok || got != "dbo.SomeProc" {
		t.Fatalf("parent_module: got %#v, want %q", row[2], "dbo.SomeProc")
	}
	if !tbl.propertiesComplete {
		t.Fatalf("propertiesComplete: got false, want true (parent module resolved)")
	}
	if notice := sink.noticeWithKind("parent_module_unavailable"); notice != nil {
		t.Fatalf("unexpected parent_module_unavailable notice for a resolved module: %+v", notice)
	}
}

// TestQueryTextArtifactExported proves Query exports the query's full
// SQL text through Sink.File, and that the exported bytes and the
// text_preview cell agree exactly - the same property the integration
// suite's long-Unicode test (tests/integration/query_test.go) proves
// against a real engine value; this is the fast, server-free version
// of the same assertion.
func TestQueryTextArtifactExported(t *testing.T) {
	const text = "SELECT N'héllo wörld 日本語' AS Marker, COUNT(*) FROM dbo.Widgets"
	conn := &fakeQueryConn{
		identityRow: []driver.Value{int64(9), int64(0), nil, nil, false, []byte{0, 0, 0, 0, 0, 0, 0, 3}, text},
	}
	sess := newFakeQuerySession(t, conn)
	sink := &queryCaptureSink{}

	if err := Query(context.Background(), sess, QueryOptions{ID: 9, Window: defaultWindow()}, sink); err != nil {
		t.Fatalf("Query: %v", err)
	}

	exported := sink.fileContent("query_sql_text")
	if string(exported) != text {
		t.Fatalf("exported artifact: got %q, want %q", exported, text)
	}
	row := sink.table(QueryTable.Name).rows[0]
	preview, ok := row[5].(string)
	if !ok || preview != text {
		t.Fatalf("text_preview: got %#v, want %q", row[5], text)
	}
	artifactPath, ok := row[6].(string)
	if !ok || artifactPath == "" {
		t.Fatalf("text_artifact: got %#v, want a non-empty path", row[6])
	}
}

// TestQueryPlansForwardsNullAveragesAndReplicaGroup proves Query
// forwards query_plans_*.sql's own rows to the Sink with exact
// fidelity, NULL cells included: a plan with zero executions in the
// window (query_plans_*.sql's own LEFT JOIN, NULLIF'd averages) must
// reach the Sink as executions = int64(0) and every average cell nil,
// never dropped and never coerced to 0.0. The arithmetic itself (the
// LEFT JOIN that produces this row instead of dropping it) can only be
// proven against a real engine - see tests/integration/query_test.go's
// TestQueryPlansKeepZeroExecutionRow - this test is the Go-side
// forwarding half alone.
func TestQueryPlansForwardsNullAveragesAndReplicaGroup(t *testing.T) {
	conn := &fakeQueryConn{
		identityRow: []driver.Value{int64(10), int64(0), nil, nil, false, []byte{0, 0, 0, 0, 0, 0, 0, 4}, "SELECT 4"},
		plans: [][]driver.Value{
			{int64(100), nil, false, int64(0), float64(0), nil, float64(0), nil, float64(0), nil},
			{int64(101), int64(2), true, int64(5), float64(50), float64(10), float64(25), float64(5), float64(40), float64(8)},
		},
	}
	sess := newFakeQuerySession(t, conn)
	sink := &queryCaptureSink{}

	if err := Query(context.Background(), sess, QueryOptions{ID: 10, Window: defaultWindow()}, sink); err != nil {
		t.Fatalf("Query: %v", err)
	}

	plans := sink.table(PlansTable.Name)
	if plans == nil || len(plans.rows) != 2 {
		t.Fatalf("plans table missing or wrong row count: %+v", sink.tables)
	}

	zero := plans.rows[0]
	if zero[1] != nil {
		t.Fatalf("row 0 replica_group_id: got %#v, want nil", zero[1])
	}
	if got, ok := zero[3].(int64); !ok || got != 0 {
		t.Fatalf("row 0 executions: got %#v, want int64(0)", zero[3])
	}
	if zero[5] != nil || zero[7] != nil || zero[9] != nil {
		t.Fatalf("row 0 averages: got cpu_avg=%#v duration_avg=%#v reads_avg=%#v, want all nil", zero[5], zero[7], zero[9])
	}

	forced := plans.rows[1]
	if got, ok := forced[1].(int64); !ok || got != 2 {
		t.Fatalf("row 1 replica_group_id: got %#v, want int64(2)", forced[1])
	}
	if got, ok := forced[2].(bool); !ok || !got {
		t.Fatalf("row 1 forced: got %#v, want true", forced[2])
	}
}

// TestQueryPlansPermissionErrorNotSwallowed is the brief's "permission
// refusée distincte de zéro ligne" case: a permission failure on the
// plans query (sys.query_store_runtime_stats denied, SQL error 229)
// must surface as a code-4 error, never as an empty, successful plans
// table - the distinction Top's own classifyQueryError already
// guarantees; this proves Query does not catch or discard that error
// on its own path.
func TestQueryPlansPermissionErrorNotSwallowed(t *testing.T) {
	conn := &fakeQueryConn{
		identityRow: []driver.Value{int64(11), int64(0), nil, nil, false, []byte{0, 0, 0, 0, 0, 0, 0, 5}, "SELECT 5"},
		plansErr:    mssql.Error{Number: 229, Message: "denied"},
	}
	sess := newFakeQuerySession(t, conn)
	sink := &queryCaptureSink{}

	err := Query(context.Background(), sess, QueryOptions{ID: 11, Window: defaultWindow()}, sink)
	if err == nil {
		t.Fatal("Query: got nil error, want a permission error")
	}
	var pub *model.PublicError
	if !errors.As(err, &pub) {
		t.Fatalf("Query: error is not a *model.PublicError: %v", err)
	}
	if pub.Code != 4 {
		t.Fatalf("code: got %d, want 4 (permission denied, not an empty result): %v", pub.Code, pub)
	}
	if plans := sink.table(PlansTable.Name); plans != nil {
		t.Fatalf("plans table was written despite the permission error: %+v", plans)
	}
}

// refusingPlansSink wraps queryCaptureSink but refuses the very first
// Row call made once the "plans" table is open - used only by
// TestQueryRowsStreamsWithoutAccumulating to prove queryRows (top.go)
// never reads a second row off the wire once the Sink has already
// refused the first (fix 1's A3: it used to accumulate every row into
// a slice before the Sink ever saw one).
type refusingPlansSink struct {
	queryCaptureSink
	refused bool
}

func (s *refusingPlansSink) Row(row []model.Cell) error {
	if s.cur != nil && s.cur.spec.Name == PlansTable.Name && !s.refused {
		s.refused = true
		return fmt.Errorf("refusingPlansSink: refused the first plans row")
	}
	return s.queryCaptureSink.Row(row)
}

// TestQueryRowsStreamsWithoutAccumulating is fix 1's A3 regression
// check: with five fake plan rows queued and a Sink that refuses the
// very first one, queryRows must stop at that first row - the
// underlying driver.Rows must never have been asked for a second one.
// Before the fix, queryRows read every row into a slice before handing
// any of them to the Sink, so this fake driver's Next would have been
// called five times regardless of what the Sink did with the first.
func TestQueryRowsStreamsWithoutAccumulating(t *testing.T) {
	conn := &fakeQueryConn{
		identityRow: []driver.Value{int64(13), int64(0), nil, nil, false, []byte{0, 0, 0, 0, 0, 0, 0, 7}, "SELECT 7"},
		plans: [][]driver.Value{
			{int64(1), nil, false, int64(1), float64(1), float64(1), float64(1), float64(1), float64(1), float64(1)},
			{int64(2), nil, false, int64(1), float64(1), float64(1), float64(1), float64(1), float64(1), float64(1)},
			{int64(3), nil, false, int64(1), float64(1), float64(1), float64(1), float64(1), float64(1), float64(1)},
			{int64(4), nil, false, int64(1), float64(1), float64(1), float64(1), float64(1), float64(1), float64(1)},
			{int64(5), nil, false, int64(1), float64(1), float64(1), float64(1), float64(1), float64(1), float64(1)},
		},
	}
	sess := newFakeQuerySession(t, conn)
	sink := &refusingPlansSink{}

	err := Query(context.Background(), sess, QueryOptions{ID: 13, Window: defaultWindow()}, sink)
	if err == nil {
		t.Fatal("Query: got nil error, want the Sink's own refusal to propagate")
	}
	if !sink.refused {
		t.Fatal("the plans Sink was never even asked to accept a row")
	}
	if conn.plansRows == nil {
		t.Fatal("the plans query was never issued")
	}
	if conn.plansRows.idx != 1 {
		t.Fatalf("driver.Rows advanced %d rows before the Sink's refusal stopped it, want exactly 1 (no accumulation)", conn.plansRows.idx)
	}
}

// TestQueryOffWithHistorySucceeds is the brief's "OFF avec histoire
// lisible" case: Query Store OFF, with readable runtime history
// (fakeHealthRows' own has_history default, true), must still succeed
// with a "capture" warning, never fail at code 4 - the code-4 guard
// only fires when history is also unreadable (proven against a real
// engine by tests/integration/query_test.go's
// TestQueryStoreUnavailableNoHistory, the negative case this positive
// one is paired with).
func TestQueryOffWithHistorySucceeds(t *testing.T) {
	conn := &fakeQueryConn{
		actual:      "OFF",
		identityRow: []driver.Value{int64(12), int64(0), nil, nil, false, []byte{0, 0, 0, 0, 0, 0, 0, 6}, "SELECT 6"},
	}
	sess := newFakeQuerySession(t, conn)
	sink := &queryCaptureSink{}

	if err := Query(context.Background(), sess, QueryOptions{ID: 12, Window: defaultWindow()}, sink); err != nil {
		t.Fatalf("Query on an OFF database with readable history should succeed, got: %v", err)
	}
	notice := sink.noticeWithKind("capture")
	if notice == nil {
		t.Fatalf("expected a capture notice naming OFF, got none: %+v", sink.notices)
	}
	if !strings.Contains(notice.Message, "OFF") {
		t.Fatalf("capture notice does not name OFF: %q", notice.Message)
	}
}

// TestQueryErrorStateNoHistoryFailsAtCode4 is fix 1's B6, the ERROR
// third of design spec line 83's four non-collecting states: OFF and
// READ_ONLY are proven against a real engine (tests/integration's
// TestQueryStoreUnavailableAndOffWithHistory), but neither reviewer
// found a way to provoke Query Store's ERROR state on a real container
// without corrupting its own internal structures - unit coverage only,
// deliberately, rather than inventing an engine recipe neither of them
// could reproduce. Measured before this test existed: removing "ERROR"
// from nonCollectingStates left the entire 2022 integration suite
// green (73 === RUN), because nothing exercised this state at any
// level.
func TestQueryErrorStateNoHistoryFailsAtCode4(t *testing.T) {
	conn := &fakeQueryConn{actual: "ERROR", noHistory: true}
	sess := newFakeQuerySession(t, conn)
	sink := &queryCaptureSink{}

	err := Query(context.Background(), sess, QueryOptions{ID: 1, Window: defaultWindow()}, sink)
	var pub *model.PublicError
	if !errors.As(err, &pub) {
		t.Fatalf("Query: error is not a *model.PublicError: %v", err)
	}
	if pub.Code != 4 {
		t.Fatalf("code: got %d, want 4", pub.Code)
	}
	if pub.Kind != "query_store_unavailable" {
		t.Fatalf("kind: got %q, want %q", pub.Kind, "query_store_unavailable")
	}
	if conn.identityArgs != nil {
		t.Fatalf("identity query was issued despite the health gate, args=%v", conn.identityArgs)
	}
}
