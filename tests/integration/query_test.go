//go:build integration

package integration

import (
	"context"
	"errors"
	"math"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/rudi-bruchez/argosql/internal/diagnostics"
	"github.com/rudi-bruchez/argosql/internal/model"
	"github.com/rudi-bruchez/argosql/internal/sqlserver"
)

// TestQueryExport is the task 11 brief's own directing test. The
// brief's own literal marker, "asq-fixture-query", does not survive
// Lab.QueryID/workload.sql's "AS {{MARKER}}" substitution: a hyphen is
// not valid in an unquoted SQL identifier, and measured against a real
// server this produced "Incorrect syntax near '-'" before the workload
// ever ran. Every existing caller of Lab.QueryID (top_test.go's own
// "AsqTopMarker"+hex) avoids hyphens for exactly this reason; this one
// does too - a hyphen-free marker, not the brief's literal string, is
// the one syntactic deviation this test takes from the brief (see this
// task's report for the measurement). Looked up through "qs query" run
// as a real subprocess (Lab.Run) under principal Q, it must succeed
// and export at least one artifact (the full SQL text).
func TestQueryExport(t *testing.T) {
	lab := NewLab(t, os.Getenv("ASQ_TEST_IMAGE"))
	id := lab.QueryID(t, "asqFixtureQuery")
	r, code := lab.Run(t, "Q", []string{"qs", "query", strconv.FormatInt(id, 10)})
	if code != 0 || len(r.Artifacts) == 0 {
		t.Fatalf("code=%d", code)
	}
}

// TestQueryNotFound is the brief's "ID manquant=8" case against a real
// engine: a query_id that certainly does not exist in AppDB must fail
// at code 8, not_found_or_not_visible.
func TestQueryNotFound(t *testing.T) {
	lab := NewLab(t, os.Getenv("ASQ_TEST_IMAGE"))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	sess, err := sqlserver.Open(ctx, lab.Profile)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer sess.Close()
	logEngineIdentity(t, lab, sess.Major)

	opts := diagnostics.QueryOptions{
		ID:     math.MaxInt64 - 7,
		Window: diagnostics.Window{Since: time.Now().Add(-time.Hour), Until: time.Now()},
	}
	sink := &captureSink{}
	err = diagnostics.Query(ctx, sess, opts, sink)
	var pub *model.PublicError
	if !errors.As(err, &pub) {
		t.Fatalf("Query: error is not a *model.PublicError: %v", err)
	}
	if pub.Code != 8 {
		t.Fatalf("code: got %d, want 8: %v", pub.Code, pub)
	}
}

// TestQueryStoreUnavailableAndOffWithHistory is the brief's "OFF avec
// histoire lisible" case, paired with its own negative control: OFF
// with history cleared must fail at code 4 (mirroring
// tests/integration/top_test.go's own runUnavailableFixture exactly,
// since Query and Top share the exact same health-gate code); OFF with
// history retained must succeed with a "capture" warning naming OFF.
func TestQueryStoreUnavailableAndOffWithHistory(t *testing.T) {
	lab := NewLab(t, os.Getenv("ASQ_TEST_IMAGE"))
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	sess, err := sqlserver.Open(ctx, lab.Profile)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer sess.Close()
	logEngineIdentity(t, lab, sess.Major)

	t.Run("OFF with history cleared fails at code 4", func(t *testing.T) {
		dbName := createThrowawayDatabase(ctx, t, lab, "asq_query_off_")
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

		opts := diagnostics.QueryOptions{
			ID:     1,
			Window: diagnostics.Window{Since: time.Now().Add(-time.Hour), Until: time.Now()},
		}
		sink := &captureSink{}
		queryErr := diagnostics.Query(ctx, offSess, opts, sink)
		var pub *model.PublicError
		if !errors.As(queryErr, &pub) {
			t.Fatalf("Query on an OFF database with cleared history should fail with a *model.PublicError, got: %v", queryErr)
		}
		if pub.Code != 4 {
			t.Fatalf("code: got %d, want 4", pub.Code)
		}
		if pub.Kind != "query_store_unavailable" {
			t.Fatalf("kind: got %q, want %q", pub.Kind, "query_store_unavailable")
		}
	})

	t.Run("OFF with history retained succeeds with a capture warning", func(t *testing.T) {
		dbName := createThrowawayDatabase(ctx, t, lab, "asq_query_off_history_")
		if _, err := lab.Admin.ExecContext(ctx, "ALTER DATABASE ["+dbName+"] SET QUERY_STORE = ON (OPERATION_MODE = READ_WRITE, INTERVAL_LENGTH_MINUTES = 1)"); err != nil {
			t.Fatalf("enabling Query Store: %v", err)
		}
		generateLoadAndWaitForHistory(ctx, t, lab, dbName)

		// sys.query_store_query is per-database: lab.Admin is a pool
		// bound to AppDB, never dbName, so the id must be read through a
		// session actually connected to the throwaway database itself.
		dbProfile := lab.Profile
		dbProfile.Database = dbName
		dbSess, err := sqlserver.Open(ctx, dbProfile)
		if err != nil {
			t.Fatalf("open to read a real query_id: %v", err)
		}
		var id int64
		scanErr := dbSess.Conn.QueryRowContext(ctx, "SELECT TOP (1) query_id FROM sys.query_store_query ORDER BY query_id DESC").Scan(&id)
		dbSess.Close()
		if scanErr != nil {
			t.Fatalf("reading a real query_id from the throwaway database: %v", scanErr)
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

		opts := diagnostics.QueryOptions{
			ID:     id,
			Window: diagnostics.Window{Since: time.Now().Add(-time.Hour), Until: time.Now().Add(time.Minute)},
		}
		sink := &captureSink{}
		if err := diagnostics.Query(ctx, offSess, opts, sink); err != nil {
			t.Fatalf("Query on an OFF database with retained history should succeed, got: %v", err)
		}
		notice := sink.noticeWithKind("capture")
		if notice == nil {
			t.Fatalf("no capture notice emitted: %+v", sink.notices)
		}
	})
}

// TestQueryNoExecutionsInWindowKeepsPlanRow is the brief's "pas
// d'exécutions dans fenêtre" case: the same real plan must still
// appear in the plans table, with executions = 0 and every average
// NULL, when queried with a window that predates every execution this
// query has ever had - never silently dropped by query_plans_*.sql's
// own LEFT JOIN (design spec: "qs query lists plans with no executions
// in the window explicitly").
func TestQueryNoExecutionsInWindowKeepsPlanRow(t *testing.T) {
	lab := NewLab(t, os.Getenv("ASQ_TEST_IMAGE"))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	sess, err := sqlserver.Open(ctx, lab.Profile)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer sess.Close()
	logEngineIdentity(t, lab, sess.Major)

	id := lab.QueryID(t, "asqQuery11Window")

	recentSink := &captureSink{}
	recentOpts := diagnostics.QueryOptions{
		ID:     id,
		Window: diagnostics.Window{Since: time.Now().Add(-time.Hour), Until: time.Now().Add(time.Minute)},
	}
	if err := diagnostics.Query(ctx, sess, recentOpts, recentSink); err != nil {
		t.Fatalf("Query with a recent window: %v", err)
	}
	recentPlans := recentSink.table(diagnostics.PlansTable.Name)
	if recentPlans == nil || len(recentPlans.rows) == 0 {
		t.Fatalf("no plan rows in the recent window: %+v", recentSink.tables)
	}
	var planID int64
	var sawExecutions bool
	for _, row := range recentPlans.rows {
		pid, ok := row[0].(int64)
		if !ok {
			t.Fatalf("plan_id: got %#v, want int64", row[0])
		}
		planID = pid
		executions, ok := row[3].(int64)
		if !ok {
			t.Fatalf("executions: got %#v, want int64", row[3])
		}
		if executions > 0 {
			sawExecutions = true
		}
	}
	if !sawExecutions {
		t.Fatalf("no plan row reported any execution in the recent window: %+v", recentPlans.rows)
	}

	oldSink := &captureSink{}
	oldOpts := diagnostics.QueryOptions{
		ID:     id,
		Window: diagnostics.Window{Since: time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), Until: time.Date(2000, 1, 2, 0, 0, 0, 0, time.UTC)},
	}
	if err := diagnostics.Query(ctx, sess, oldOpts, oldSink); err != nil {
		t.Fatalf("Query with a window predating every execution: %v", err)
	}
	oldPlans := oldSink.table(diagnostics.PlansTable.Name)
	if oldPlans == nil || len(oldPlans.rows) == 0 {
		t.Fatalf("plan row disappeared entirely for a window with no matching executions: %+v", oldSink.tables)
	}
	found := false
	for _, row := range oldPlans.rows {
		pid, ok := row[0].(int64)
		if !ok || pid != planID {
			continue
		}
		found = true
		executions, ok := row[3].(int64)
		if !ok || executions != 0 {
			t.Fatalf("plan %d executions: got %#v, want int64(0)", planID, row[3])
		}
		if row[5] != nil || row[7] != nil || row[9] != nil {
			t.Fatalf("plan %d averages: got cpu_avg=%#v duration_avg=%#v reads_avg=%#v, want all nil", planID, row[5], row[7], row[9])
		}
	}
	if !found {
		t.Fatalf("plan %d (present in the recent window) is missing from the old window's plans table entirely: %+v", planID, oldPlans.rows)
	}
}

// asq11ParentProbeProc is a plain, unencrypted procedure created
// directly by TestQueryParentModuleVisibility (never added to
// sql/objects.sql - this need is this test's own, not a fixture other
// tests share). objects.sql's dbo.EncryptedProc was tried first and
// measured NOT to work for this, for a reason that turned out to have
// nothing to do with encryption: its one statement, "SELECT 1 AS
// Placeholder", has no FROM clause at all, and measured against a real
// server, a pure constant SELECT with no table reference never gets a
// compiled plan and never gets a sys.query_store_query row either -
// SQL Server's own debug probe (this task's report) showed the exact
// same "SELECT 1" body producing zero Query Store rows after 10
// one-second polls, while swapping it for "SELECT COUNT(*) FROM ..."
// (a real table reference, actually worth optimizing) was captured on
// the very next flush. This procedure's body queries dbo.Widgets for
// exactly that reason.
const asq11ParentProbeProc = "Asq11ParentProbe"

// TestQueryParentModuleVisibility is the design spec's own permission
// matrix entry for "qs query": "0; parent name may be unavailable" for
// Q, plain "0" for I. asq11ParentProbeProc is EXECed to generate a real
// Query Store entry whose object_id resolves: Q (no VIEW DEFINITION,
// no SELECT on dbo) cannot see that module's name in sys.objects, so
// Query must still succeed at code 0 with parent_module = NULL plus a
// warning notice; I (VIEW DEFINITION database-wide) sees it and gets
// "dbo.Asq11ParentProbe" with no warning.
func TestQueryParentModuleVisibility(t *testing.T) {
	lab := NewLab(t, os.Getenv("ASQ_TEST_IMAGE"))
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	pw := lab.ensurePrincipals(t)

	if _, err := lab.Admin.ExecContext(ctx, "CREATE OR ALTER PROCEDURE dbo."+asq11ParentProbeProc+" AS BEGIN SELECT COUNT(*) AS Cnt FROM dbo.Widgets; END"); err != nil {
		t.Fatalf("creating %s: %v", asq11ParentProbeProc, err)
	}
	for i := 0; i < 5; i++ {
		if _, err := lab.Admin.ExecContext(ctx, "EXEC dbo."+asq11ParentProbeProc); err != nil {
			t.Fatalf("executing dbo.%s to generate Query Store load: %v", asq11ParentProbeProc, err)
		}
	}
	if _, err := lab.Admin.ExecContext(ctx, "EXEC sys.sp_query_store_flush_db"); err != nil {
		t.Fatalf("sp_query_store_flush_db: %v", err)
	}

	findID := "SELECT q.query_id FROM sys.query_store_query AS q " +
		"JOIN sys.objects AS o ON o.object_id = q.object_id " +
		"WHERE o.name = N'" + asq11ParentProbeProc + "'"
	deadline := time.Now().Add(flushPollDeadline)
	var id int64
	for {
		err := lab.Admin.QueryRowContext(ctx, findID).Scan(&id)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s's query_id never appeared within %s: %v", asq11ParentProbeProc, flushPollDeadline, err)
		}
		time.Sleep(flushPollDelay)
	}

	qProfile := principalProfile(lab, "asq_test_q", pw.Q)
	iProfile := principalProfile(lab, "asq_test_i", pw.I)
	opts := diagnostics.QueryOptions{
		ID:     id,
		Window: diagnostics.Window{Since: time.Now().Add(-time.Hour), Until: time.Now().Add(time.Minute)},
	}

	t.Run("Q cannot see the procedure's name", func(t *testing.T) {
		sess, err := sqlserver.Open(ctx, qProfile)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer sess.Close()
		sink := &captureSink{}
		if err := diagnostics.Query(ctx, sess, opts, sink); err != nil {
			t.Fatalf("Query as Q should succeed at code 0, got: %v", err)
		}
		row := sink.table(diagnostics.QueryTable.Name).rows[0]
		if row[2] != nil {
			t.Fatalf("parent_module: got %#v, want nil (Q cannot see sys.objects' row for the procedure)", row[2])
		}
		if notice := sink.noticeWithKind("parent_module_unavailable"); notice == nil {
			t.Fatalf("expected a parent_module_unavailable notice for Q, got none: %+v", sink.notices)
		}
	})

	t.Run("I sees the procedure's name", func(t *testing.T) {
		sess, err := sqlserver.Open(ctx, iProfile)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer sess.Close()
		sink := &captureSink{}
		if err := diagnostics.Query(ctx, sess, opts, sink); err != nil {
			t.Fatalf("Query as I: %v", err)
		}
		row := sink.table(diagnostics.QueryTable.Name).rows[0]
		want := "dbo." + asq11ParentProbeProc
		if got, ok := row[2].(string); !ok || got != want {
			t.Fatalf("parent_module: got %#v, want %q", row[2], want)
		}
		if notice := sink.noticeWithKind("parent_module_unavailable"); notice != nil {
			t.Fatalf("unexpected parent_module_unavailable notice for I: %+v", notice)
		}
	})
}

// longUnicodeMarker is TestQueryExportLongUnicodeTextByteIdentity's own
// marker: only valid unqualified-identifier characters (Unicode
// letters and digits, no spaces or symbols - SQL Server's own
// identifier rules, measured elsewhere in this codebase, e.g. "SELECT
// 1 AS 日本語"), so workload.sql's "AS {{MARKER}}" substitution stays
// syntactically valid while still exercising several genuinely
// multi-byte UTF-8 scripts (CJK, Hangul, Latin with diacritics) - not
// just ASCII, which an octet-identity bug involving multi-byte
// boundaries could otherwise pass trivially.
const longUnicodeMarker = "asqUnicodeProbeVérification日本語テスト中文文本한국어테스트Ωμέγα"

// TestQueryExportLongUnicodeTextByteIdentity is the brief's "long texte
// Unicode export identique" case, and the target of this task's third
// mandatory breakage (dispatch: truncate the exported text by one
// character and confirm this exact test, not an approximate length
// check, is what falls). The artifact Query exports through Sink.File
// is compared octet for octet against query_sql_text read back
// independently through lab.Admin - a second, separate query, never
// Query's own in-memory copy of the same string - so a corruption
// introduced between the scan and the File call is exactly what this
// comparison is built to catch.
func TestQueryExportLongUnicodeTextByteIdentity(t *testing.T) {
	lab := NewLab(t, os.Getenv("ASQ_TEST_IMAGE"))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	sess, err := sqlserver.Open(ctx, lab.Profile)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer sess.Close()
	logEngineIdentity(t, lab, sess.Major)

	id := lab.QueryID(t, longUnicodeMarker)

	var wantText string
	findText := "SELECT qt.query_sql_text FROM sys.query_store_query AS q " +
		"JOIN sys.query_store_query_text AS qt ON qt.query_text_id = q.query_text_id " +
		"WHERE q.query_id = @p1"
	if err := lab.Admin.QueryRowContext(ctx, findText, id).Scan(&wantText); err != nil {
		t.Fatalf("reading query_sql_text independently for query_id %d: %v", id, err)
	}
	if len(wantText) == 0 {
		t.Fatal("independently-read query_sql_text is empty")
	}

	sink := &captureSink{}
	opts := diagnostics.QueryOptions{
		ID:     id,
		Window: diagnostics.Window{Since: time.Now().Add(-time.Hour), Until: time.Now().Add(time.Minute)},
	}
	if err := diagnostics.Query(ctx, sess, opts, sink); err != nil {
		t.Fatalf("Query: %v", err)
	}

	exported := sink.file("query_sql_text")
	if exported == nil {
		t.Fatal("no query_sql_text artifact was exported")
	}
	if string(exported) != wantText {
		t.Fatalf("exported artifact does not match query_sql_text byte for byte:\n got  (%d bytes): %q\n want (%d bytes): %q",
			len(exported), exported, len(wantText), wantText)
	}

	row := sink.table(diagnostics.QueryTable.Name).rows[0]
	preview, ok := row[5].(string)
	if !ok || preview != wantText {
		t.Fatalf("text_preview does not match query_sql_text: got %q", preview)
	}
}
