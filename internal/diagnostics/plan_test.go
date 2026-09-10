package diagnostics

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/rudi-bruchez/argosql/internal/artifacts"
	"github.com/rudi-bruchez/argosql/internal/model"
	"github.com/rudi-bruchez/argosql/internal/output"
	"github.com/rudi-bruchez/argosql/internal/plan"
	"github.com/rudi-bruchez/argosql/internal/sqlserver"
)

// fakePlanRows is a driver.Rows over sql/plan.sql's own single
// "query_plan" column (XML), zero or one row.
type fakePlanRows struct {
	data [][]driver.Value
	idx  int
}

func (r *fakePlanRows) Columns() []string                     { return []string{"query_plan"} }
func (r *fakePlanRows) Close() error                          { return nil }
func (r *fakePlanRows) ColumnTypeDatabaseTypeName(int) string { return "XML" }
func (r *fakePlanRows) Next(dest []driver.Value) error {
	if r.idx >= len(r.data) {
		return io.EOF
	}
	copy(dest, r.data[r.idx])
	r.idx++
	return nil
}

// fakePlanConn is a minimal database/sql/driver.Conn answering both of
// the two queries Plan issues: the shared health.sql read (fix 1's A4
// - Plan reads health first, like every other Query Store command,
// even though its own exit code is never gated on it) and sql/plan.sql's
// own lookup. actual/captureMode/noHistory configure fakeHealthRows
// (top_test.go, this same package) exactly like fakeTopConn does;
// left at their zero value, ReadHealth sees a plain, fully-collecting
// READ_WRITE/ALL database with history, so a test that does not care
// about health gets no notice at all from it.
type fakePlanConn struct {
	row  []driver.Value // nil means zero rows (not found/mismatch)
	args []driver.NamedValue
	err  error

	actual, captureMode string
	noHistory           bool
}

func (c *fakePlanConn) Prepare(query string) (driver.Stmt, error) {
	return nil, errors.New("fakePlanConn: Prepare not supported, use QueryContext")
}
func (c *fakePlanConn) Close() error { return nil }
func (c *fakePlanConn) Begin() (driver.Tx, error) {
	return nil, errors.New("fakePlanConn: Begin not supported")
}
func (c *fakePlanConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	switch {
	case strings.Contains(query, "database_query_store_options"):
		return &fakeHealthRows{actual: c.actual, captureMode: c.captureMode, noHistory: c.noHistory}, nil
	case strings.Contains(query, "query_store_plan"):
		c.args = args
		if c.err != nil {
			return nil, c.err
		}
		var data [][]driver.Value
		if c.row != nil {
			data = [][]driver.Value{c.row}
		}
		return &fakePlanRows{data: data}, nil
	default:
		return nil, errors.New("fakePlanConn: unexpected query: " + query)
	}
}

type fakePlanDriver struct{}

func (d *fakePlanDriver) Open(name string) (driver.Conn, error) {
	return nil, errors.New("fakePlanDriver: Open not supported, use the connector")
}

type fakePlanConnector struct{ conn *fakePlanConn }

func (c *fakePlanConnector) Connect(ctx context.Context) (driver.Conn, error) { return c.conn, nil }
func (c *fakePlanConnector) Driver() driver.Driver                            { return &fakePlanDriver{} }

func newFakePlanSession(t *testing.T, conn *fakePlanConn) *sqlserver.Session {
	t.Helper()
	db := sql.OpenDB(&fakePlanConnector{conn: conn})
	t.Cleanup(func() { db.Close() })
	sqlConn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatalf("db.Conn: %v", err)
	}
	t.Cleanup(func() { sqlConn.Close() })
	return &sqlserver.Session{Conn: sqlConn, Major: 16}
}

// TestPlanNotFoundOrMismatch is design spec lines 223-224's own two
// cases, both reported identically by sql/plan.sql's single WHERE
// clause (query_id AND plan_id together): zero rows means either "the
// pair does not exist" or "--plan-id does not belong to the given
// query_id" - Go cannot and does not try to tell them apart (see
// plan.go's own planNotFound), so one fake-conn shape (row: nil)
// exercises both design-spec table rows at once.
func TestPlanNotFoundOrMismatch(t *testing.T) {
	sess := newFakePlanSession(t, &fakePlanConn{row: nil})
	sink := &queryCaptureSink{}
	err := Plan(context.Background(), sess, 4821, 9033, false, sink)
	var pub *model.PublicError
	if !errors.As(err, &pub) {
		t.Fatalf("Plan: error is not a *model.PublicError: %v", err)
	}
	if pub.Code != 8 {
		t.Fatalf("code: got %d, want 8", pub.Code)
	}
	if pub.Kind != "not_found_or_not_visible" {
		t.Fatalf("kind: got %q, want %q", pub.Kind, "not_found_or_not_visible")
	}
}

// TestPlanUnavailableWhenQueryPlanIsNull is this task's own resolution
// of the brief's "plan existant sans runtime exportable" case: the
// (query_id, plan_id) pair resolves, but sys.query_store_plan.query_plan
// itself is NULL - reported as code 4, plan_unavailable, never a
// silent empty export.
func TestPlanUnavailableWhenQueryPlanIsNull(t *testing.T) {
	sess := newFakePlanSession(t, &fakePlanConn{row: []driver.Value{nil}})
	sink := &queryCaptureSink{}
	err := Plan(context.Background(), sess, 4821, 9033, false, sink)
	var pub *model.PublicError
	if !errors.As(err, &pub) {
		t.Fatalf("Plan: error is not a *model.PublicError: %v", err)
	}
	if pub.Code != 4 {
		t.Fatalf("code: got %d, want 4", pub.Code)
	}
	if pub.Kind != "plan_unavailable" {
		t.Fatalf("kind: got %q, want %q", pub.Kind, "plan_unavailable")
	}
}

// TestPlanEmitsCaptureNoticeWhenNotReadWrite is fix 1's A4, the
// reviewer's own repro reproduced at the unit level: after Query Store
// goes OFF (or otherwise leaves READ_WRITE), Plan must still export at
// code 0, but it must also emit the same "capture" notice
// emitQueryStoreNotices already gives Top and Query for the identical
// fact - never silently exporting as if the collection were live.
func TestPlanEmitsCaptureNoticeWhenNotReadWrite(t *testing.T) {
	sess := newFakePlanSession(t, &fakePlanConn{row: []driver.Value{planXMLFixture}, actual: "OFF"})
	sink := &queryCaptureSink{}
	if err := Plan(context.Background(), sess, 4821, 9033, false, sink); err != nil {
		t.Fatalf("Plan on an OFF database should still succeed, got: %v", err)
	}
	if n := sink.noticeWithKind("capture"); n == nil {
		t.Fatal("no capture notice emitted for a non-READ_WRITE Query Store state")
	}
	if len(sink.files) != 1 {
		t.Fatalf("files: got %d, want 1 (export must still happen)", len(sink.files))
	}
}

// TestPlanEmitsNoNoticeWhenReadWrite is the negative control: a plain,
// fully-collecting database emits neither a "capture" nor a
// "capture_mode" notice - the fake driver's own zero-value defaults
// (READ_WRITE, ALL, history present).
func TestPlanEmitsNoNoticeWhenReadWrite(t *testing.T) {
	sess := newFakePlanSession(t, &fakePlanConn{row: []driver.Value{planXMLFixture}})
	sink := &queryCaptureSink{}
	if err := Plan(context.Background(), sess, 4821, 9033, false, sink); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if n := sink.noticeWithKind("capture"); n != nil {
		t.Fatalf("unexpected capture notice for a READ_WRITE database: %+v", n)
	}
	if n := sink.noticeWithKind("capture_mode"); n != nil {
		t.Fatalf("unexpected capture_mode notice for capture mode ALL: %+v", n)
	}
}

// planXMLFixture is a small, well-formed plan XML value, exactly the
// shape queryOneOptionalRow would hand back for a real query_plan
// column: no XML declaration (the ordinary Query Store shape - design
// spec line 91), one RelOp with a real cost, one Object reference, one
// Warnings child.
const planXMLFixture = `<ShowPlanXML><QueryPlan><RelOp NodeId="0" PhysicalOp="Clustered Index Scan" LogicalOp="Clustered Index Scan" EstimatedTotalSubtreeCost="1.23"><Object Database="[AppDB]" Schema="[dbo]" Table="[Widgets]" Index="[PK_Widgets]"/></RelOp><Warnings><ColumnsWithNoStatistics/></Warnings></QueryPlan></ShowPlanXML>`

// TestPlanExportsRawArtifactWithoutSummary proves a plain "plan
// <query_id> --plan-id <id>" (summary=false) exports exactly one
// ".sqlplan" artifact, byte-identical to the source value (no
// declaration to normalize here), and writes no table at all.
func TestPlanExportsRawArtifactWithoutSummary(t *testing.T) {
	sess := newFakePlanSession(t, &fakePlanConn{row: []driver.Value{planXMLFixture}})
	sink := &queryCaptureSink{}
	if err := Plan(context.Background(), sess, 4821, 9033, false, sink); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(sink.files) != 1 {
		t.Fatalf("files: got %d, want exactly 1", len(sink.files))
	}
	f := sink.files[0]
	if f.kind != "plan_xml" || f.suffix != ".sqlplan" {
		t.Fatalf("artifact kind/suffix: got %q/%q, want \"plan_xml\"/\".sqlplan\"", f.kind, f.suffix)
	}
	if string(f.content) != planXMLFixture {
		t.Fatalf("artifact content: got %q, want %q (byte-identical, no declaration to normalize)", f.content, planXMLFixture)
	}
	if len(sink.tables) != 0 {
		t.Fatalf("tables: got %d, want 0 (no --summary)", len(sink.tables))
	}
	// Fix 2's B8: forcing this notice to fire unconditionally (instead
	// of only when NormalizeXML actually rewrote a declaration) left
	// every existing test green, because none of them checked its
	// ABSENCE. planXMLFixture carries no declaration at all - the
	// ordinary shape of every real plan measured in this project - so
	// this export must never claim a rewrite that never happened.
	if n := sink.noticeWithKind(model.KindEncodingNormalized); n != nil {
		t.Fatalf("unexpected encoding_normalized notice for a declaration-free source: %+v", n)
	}
}

// TestPlanQuotaSaturationYieldsCode7NoArtifactNoTables is fix 2's B1:
// internal/artifacts.Collector.File's own guard against an oversized
// single value is proven by its own test, but nothing before this
// exercised that path all the way through diagnostics.Plan - a
// reviewer measured that neutralizing exportAndSummarize's own
// propagation of that error (`if fileErr != nil { return fileErr }`
// turned into a no-op) left the whole suite green. Without this test,
// a plan whose normalized XML exceeds the run's artifact quota could
// exit at code 0, with no plan_xml artifact recorded, and, under
// --summary, a fully populated four-table summary describing a plan
// that was never actually exported - a more misleading failure than a
// truncated file, since a truncated file is visible on disk and a
// clean exit code is not.
func TestPlanQuotaSaturationYieldsCode7NoArtifactNoTables(t *testing.T) {
	// The quota must breach on the raw ".sqlplan" export specifically,
	// and ONLY on that export - not on the table writes Summarize would
	// make afterwards, or this test cannot tell "the guard fired" apart
	// from "some other quota fired first", which would pass even with
	// exportAndSummarize's own error-propagation removed. bigXML pads
	// itself past 6 KiB with an XML comment (invisible to both
	// NormalizeXML's declaration scan and Summarize's own token loop),
	// and the budget below (500 bytes, well under manifestReserveBytes)
	// is comfortably large enough for the handful of small JSON table
	// writes Summarize would make if it incorrectly got to run, and
	// comfortably smaller than bigXML itself.
	bigXML := `<ShowPlanXML><!--` + strings.Repeat("x", 6000) + `--><QueryPlan>` +
		`<RelOp NodeId="0" PhysicalOp="Root" LogicalOp="Root" EstimatedTotalSubtreeCost="1.0"/>` +
		`</QueryPlan></ShowPlanXML>`
	// manifestReserveBytes (internal/artifacts/store.go, unexported) is
	// 64 KiB; duplicated here as a literal since this test lives outside
	// that package.
	const manifestReserveBytes = 64 * 1024
	const budget = 500
	collector, err := artifacts.New(t.TempDir(), "json", artifacts.Limits{Rows: 1000, Bytes: manifestReserveBytes + budget})
	if err != nil {
		t.Fatalf("artifacts.New: %v", err)
	}

	sess := newFakePlanSession(t, &fakePlanConn{row: []driver.Value{bigXML}})
	planErr := Plan(context.Background(), sess, 4821, 9033, true, collector)
	var pub *model.PublicError
	if !errors.As(planErr, &pub) {
		t.Fatalf("Plan: error is not a *model.PublicError: %v", planErr)
	}
	if pub.Code != 7 {
		t.Fatalf("code: got %d, want 7", pub.Code)
	}

	result, finishErr := collector.Finish(model.ContextInfo{}, planErr)
	if finishErr != nil {
		planErr = finishErr
	}
	if len(result.Tables) != 0 {
		t.Fatalf("tables: got %d, want 0 - a quota breach on the raw export must never reach Summarize", len(result.Tables))
	}
	for _, a := range result.Artifacts {
		if a.Kind == "plan_xml" && a.Complete {
			t.Fatalf("a complete plan_xml artifact must not exist when the quota was exceeded: %+v", a)
		}
	}
}

// TestPlanSummaryWritesFourTablesInDeclaredOrder is this task's own
// cassure-3 target: the design spec's declared table order for "plan
// --summary" (line 103) is statement, operators, references, warnings
// - exactly, in that order, and no other table.
func TestPlanSummaryWritesFourTablesInDeclaredOrder(t *testing.T) {
	sess := newFakePlanSession(t, &fakePlanConn{row: []driver.Value{planXMLFixture}})
	sink := &queryCaptureSink{}
	if err := Plan(context.Background(), sess, 4821, 9033, true, sink); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	want := []string{plan.StatementTable.Name, plan.OperatorsTable.Name, plan.ReferencesTable.Name, plan.WarningsTable.Name}
	if len(sink.tables) != len(want) {
		t.Fatalf("tables: got %d, want exactly %d: %+v", len(sink.tables), len(want), sink.tables)
	}
	for i, name := range want {
		if got := sink.tables[i].spec.Name; got != name {
			t.Fatalf("table %d: got %q, want %q (declared order: statement, operators, references, warnings)", i, got, name)
		}
	}
}

// TestPlanSummaryFailurePreservesRawArtifact is the brief's own
// "malformed XML" case for the full Plan pipeline, and this task's own
// cassure-4 target: a plan whose XML cannot be parsed must still leave
// the raw .sqlplan artifact recorded, and must fail with code 5 - the
// raw export is finalized (dst.File already succeeded) before
// Summarize ever runs (design spec, line 87: "malformed XML yields a
// summary error while retaining a successfully exported artifact").
func TestPlanSummaryFailurePreservesRawArtifact(t *testing.T) {
	malformed := `<ShowPlanXML><QueryPlan><RelOp NodeId="0" EstimatedTotalSubtreeCost="1"></NotTheSameTag></ShowPlanXML>`
	sess := newFakePlanSession(t, &fakePlanConn{row: []driver.Value{malformed}})
	sink := &queryCaptureSink{}
	err := Plan(context.Background(), sess, 4821, 9033, true, sink)
	var pub *model.PublicError
	if !errors.As(err, &pub) {
		t.Fatalf("Plan: error is not a *model.PublicError: %v", err)
	}
	if pub.Code != 5 {
		t.Fatalf("code: got %d, want 5", pub.Code)
	}
	if len(sink.files) != 1 {
		t.Fatalf("raw artifact must still be recorded after a summary failure: got %d files", len(sink.files))
	}
	if string(sink.files[0].content) != malformed {
		t.Fatalf("raw artifact content changed: got %q, want %q", sink.files[0].content, malformed)
	}
}

// TestPlanSummaryFailurePreservesRawArtifactOnDisk is fix 1's A6,
// point 4: the test above proves the same contract through
// queryCaptureSink, an in-memory model.Sink with no real file and no
// rendered envelope. This version composes the two real production
// pieces a live run actually uses - a real *artifacts.Collector
// (internal/artifacts) writing to a real temp directory, and
// output.Render producing the real JSON a caller would decode - around
// the exact same fake driver and malformed XML. It never corrupts a
// real database to produce this: CLAUDE.md's own recorded rule ("ERROR
// et propriétés inconnues via backend de test, pas corruption de
// base") applies identically here, and SQL Server's own Query Store
// does not let a caller write malformed XML into sys.query_store_plan
// in the first place - a fake driver is the only way to construct this
// case at all, on a real file or otherwise.
func TestPlanSummaryFailurePreservesRawArtifactOnDisk(t *testing.T) {
	malformed := `<ShowPlanXML><QueryPlan><RelOp NodeId="0" EstimatedTotalSubtreeCost="1"></NotTheSameTag></ShowPlanXML>`
	sess := newFakePlanSession(t, &fakePlanConn{row: []driver.Value{malformed}})

	collector, err := artifacts.New(t.TempDir(), "json", artifacts.Limits{Rows: 10000, Bytes: 104857600})
	if err != nil {
		t.Fatalf("artifacts.New: %v", err)
	}
	planErr := Plan(context.Background(), sess, 4821, 9033, true, collector)
	var pub *model.PublicError
	if !errors.As(planErr, &pub) || pub.Code != 5 {
		t.Fatalf("Plan: got %v, want a code-5 *model.PublicError", planErr)
	}

	result, finishErr := collector.Finish(model.ContextInfo{}, planErr)
	finalErr := planErr
	if finishErr != nil {
		finalErr = finishErr
	}
	if len(result.Artifacts) != 1 || !result.Artifacts[0].Complete {
		t.Fatalf("expected exactly one complete artifact, got %+v", result.Artifacts)
	}
	onDisk, readErr := os.ReadFile(result.Artifacts[0].Path)
	if readErr != nil {
		t.Fatalf("reading the real .sqlplan artifact: %v", readErr)
	}
	if string(onDisk) != malformed {
		t.Fatalf("on-disk artifact content changed: got %q, want %q", onDisk, malformed)
	}

	out, renderErr := output.Render(result, output.PreviewOptions{Rows: 10, ByteLimit: 32768}, "json")
	if renderErr != nil {
		t.Fatalf("Render must still produce a response: %v", renderErr)
	}
	var decoded struct {
		OK    bool `json:"ok"`
		Error struct {
			Code int `json:"code"`
		} `json:"error"`
		Artifacts []struct {
			Complete bool `json:"complete"`
		} `json:"artifacts"`
	}
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("decoding Render's own output: %v\noutput: %s", err, out)
	}
	if decoded.OK {
		t.Fatal("decoded JSON: ok=true, want false")
	}
	if decoded.Error.Code != 5 {
		t.Fatalf("decoded JSON error.code: got %d, want 5", decoded.Error.Code)
	}
	if len(decoded.Artifacts) != 1 || !decoded.Artifacts[0].Complete {
		t.Fatalf("decoded JSON must still list the one complete artifact: %s", out)
	}
	if model.ExitCode(finalErr) != 5 {
		t.Fatalf("model.ExitCode(finalErr): got %d, want 5", model.ExitCode(finalErr))
	}
}

// TestPlanNormalizesDeclaredEncodingAndNotices proves the encoding
// declaration path end to end: a plan XML declaring "utf-16" is
// exported with that one declaration rewritten to utf-8 (byte for byte
// identical everywhere else), and a KindEncodingNormalized notice is
// emitted through the project's own existing vocabulary - never a
// second, bespoke Kind string for the same fact.
func TestPlanNormalizesDeclaredEncodingAndNotices(t *testing.T) {
	src := `<?xml version="1.0" encoding="utf-16"?><ShowPlanXML><QueryPlan/></ShowPlanXML>`
	sess := newFakePlanSession(t, &fakePlanConn{row: []driver.Value{src}})
	sink := &queryCaptureSink{}
	if err := Plan(context.Background(), sess, 4821, 9033, false, sink); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(sink.files) != 1 {
		t.Fatalf("files: got %d, want 1", len(sink.files))
	}
	got := string(sink.files[0].content)
	if strings.Contains(got, "utf-16") || !strings.Contains(got, "utf-8") {
		t.Fatalf("artifact still declares utf-16 or does not declare utf-8: %q", got)
	}
	wantRest := strings.Replace(src, "utf-16", "utf-8", 1)
	if got != wantRest {
		t.Fatalf("artifact: got %q, want %q (only the encoding value should differ from the source)", got, wantRest)
	}
	if n := sink.noticeWithKind(model.KindEncodingNormalized); n == nil {
		t.Fatal("no encoding_normalized notice emitted")
	}
}
