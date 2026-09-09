//go:build integration

package integration

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
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
// A third subtest covers READ_ONLY with no history, fix 1's B6: of
// design spec line 83's four non-collecting states, only OFF had ever
// been exercised at code 4 - removing "READ_ONLY" from
// nonCollectingStates left the entire 2022 suite green. READ_ONLY here
// is reached without ever generating any load at all (a throwaway
// database set READ_ONLY the moment Query Store turns on), unlike
// top_test.go's own READ_ONLY-with-history fixture, which is the state
// this one is paired against. ERROR, the fourth state, has no known
// real-engine repro (neither reviewer found one that does not corrupt
// Query Store's own structures) and is covered only at the unit level:
// see TestQueryErrorStateNoHistoryFailsAtCode4 in this package's own
// internal/diagnostics tests.
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

	t.Run("READ_ONLY with no history fails at code 4", func(t *testing.T) {
		dbName := createThrowawayDatabase(ctx, t, lab, "asq_query_ro_nohist_")
		if _, err := lab.Admin.ExecContext(ctx, "ALTER DATABASE ["+dbName+"] SET QUERY_STORE = ON (OPERATION_MODE = READ_WRITE, INTERVAL_LENGTH_MINUTES = 1)"); err != nil {
			t.Fatalf("enabling Query Store: %v", err)
		}
		// No load generated at all - this database never has any Query
		// Store history to begin with, unlike the OFF/READ_ONLY fixtures
		// with history elsewhere in this file and in top_test.go.
		if _, err := lab.Admin.ExecContext(ctx, "ALTER DATABASE ["+dbName+"] SET READ_ONLY WITH ROLLBACK IMMEDIATE"); err != nil {
			t.Fatalf("setting READ_ONLY: %v", err)
		}
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

		opts := diagnostics.QueryOptions{
			ID:     1,
			Window: diagnostics.Window{Since: time.Now().Add(-time.Hour), Until: time.Now()},
		}
		sink := &captureSink{}
		queryErr := diagnostics.Query(ctx, roSess, opts, sink)
		var pub *model.PublicError
		if !errors.As(queryErr, &pub) {
			t.Fatalf("Query on a READ_ONLY database with no history should fail with a *model.PublicError, got: %v", queryErr)
		}
		if pub.Code != 4 {
			t.Fatalf("code: got %d, want 4", pub.Code)
		}
		if pub.Kind != "query_store_unavailable" {
			t.Fatalf("kind: got %q, want %q", pub.Kind, "query_store_unavailable")
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

// b4LineWithCRLF is one repeated line of TestQueryExportLongUnicodeTextByteIdentity's
// own payload: real CR-LF, not just LF (fix 1's B4 - the prior probe's
// 158-byte, 120-character marker carried zero CRLF at all, so
// replacing CRLF with LF anywhere in Collector.File's write path
// stayed green), and several genuinely multi-byte UTF-8 scripts, not
// just ASCII.
const b4LineWithCRLF = "Ligne de test avec accents éàüñ et 漢字ひらがなカタカナ 한글 texte multi-octets\r\n"

// b4BigCRLFText builds marker's own probe text: the marker itself
// (for the polling LIKE match below), a real CRLF, then b4LineWithCRLF
// repeated until the payload comfortably exceeds 32 KiB - io.Copy's
// own default internal buffer size (see internal/artifacts/collector.go's
// Collector.File, which copies through io.Copy with no buffer of its
// own) - so a chunk-boundary-unaware mutation (a BOM prepended once, a
// truncation off by a few bytes, a CRLF/LF substitution applied only
// to the first buffer's worth) has more than one chunk to corrupt
// across. The prior probe's 158 bytes could never exercise more than a
// single read.
func b4BigCRLFText(marker string) string {
	var b strings.Builder
	b.WriteString(marker)
	b.WriteString("\r\n")
	for b.Len() < 48*1024 {
		b.WriteString(b4LineWithCRLF)
	}
	return b.String()
}

// queryIDForLiteralText runs a single ad-hoc SELECT whose projection
// embeds text verbatim as an N'...' string literal, flushes, and polls
// (the same bounded loop Lab.QueryID uses) until a query whose
// captured text contains marker is visible, returning its query_id.
//
// A literal, never a bound parameter: database/sql would send text as
// an RPC call through sp_executesql, and Query Store's own
// query_sql_text for that form captures the parameterized placeholder
// text, not the substituted value - proven already by this package's
// existing marker technique (fixture_test.go's own QueryID), which
// this reuses the same reasoning for, just with a literal too large to
// fit as a bare column alias (sysname's 128-character limit). text
// must contain no literal "'" - not escaped here, and every caller in
// this file avoids it by construction (accented Latin, CJK, Hangul,
// digits and punctuation other than the apostrophe).
func queryIDForLiteralText(t *testing.T, lab *Lab, marker, text string) int64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), flushPollBudget)
	defer cancel()

	batch := fmt.Sprintf("SELECT N'%s' AS Marker, COUNT(*) AS Cnt FROM dbo.Widgets WHERE Quantity >= 0;", text)
	return queryIDForMarkerBatch(ctx, t, lab, marker, batch)
}

// TestQueryExportLongUnicodeTextByteIdentity is the brief's "long texte
// Unicode export identique" case, fix 1's B4 target once strengthened:
// the previous probe's payload (158 bytes, 120 characters, zero CRLF)
// left both a CRLF-to-LF mutation and a BOM added only inside
// Collector.File green, because neither mutation had a real CRLF or
// more than one io.Copy buffer's worth of bytes to corrupt, and the
// comparison read only what the in-memory captureSink received, never
// the file Collector.File actually wrote.
//
// This version runs the real subprocess (Lab.Run, not captureSink) so
// a real file lands on disk through the real internal/artifacts
// pipeline, embeds a payload with real CRLF sequences and comfortably
// over one copy buffer's worth of multi-byte text (b4BigCRLFText), and
// compares that file's own bytes (os.ReadFile) against query_sql_text
// read back independently through lab.Admin - never Query's own
// in-memory copy of the same string - so a corruption introduced
// anywhere between the scan and the file Collector.File actually wrote
// is exactly what this comparison is built to catch.
func TestQueryExportLongUnicodeTextByteIdentity(t *testing.T) {
	lab := NewLab(t, os.Getenv("ASQ_TEST_IMAGE"))
	ctx, cancel := context.WithTimeout(context.Background(), flushPollBudget)
	defer cancel()

	sess, err := sqlserver.Open(ctx, lab.Profile)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer sess.Close()
	logEngineIdentity(t, lab, sess.Major)

	marker := "AsqB4Probe" + randomHex(6)
	text := b4BigCRLFText(marker)
	id := queryIDForLiteralText(t, lab, marker, text)

	var wantText string
	findText := "SELECT qt.query_sql_text FROM sys.query_store_query AS q " +
		"JOIN sys.query_store_query_text AS qt ON qt.query_text_id = q.query_text_id " +
		"WHERE q.query_id = @p1"
	if err := lab.Admin.QueryRowContext(ctx, findText, id).Scan(&wantText); err != nil {
		t.Fatalf("reading query_sql_text independently for query_id %d: %v", id, err)
	}
	if !strings.Contains(wantText, "\r\n") {
		t.Fatalf("independently-read query_sql_text lost its CRLF: this fixture is not testing what it claims to (%d bytes)", len(wantText))
	}

	r, code := lab.Run(t, "Q", []string{"qs", "query", strconv.FormatInt(id, 10)})
	if code != 0 {
		t.Fatalf("qs query: code=%d", code)
	}
	var artifactPath string
	for _, a := range r.Artifacts {
		if a.Kind == "query_sql_text" {
			artifactPath = a.Path
		}
	}
	if artifactPath == "" {
		t.Fatalf("no query_sql_text artifact in the result: %+v", r.Artifacts)
	}

	diskBytes, err := os.ReadFile(artifactPath)
	if err != nil {
		t.Fatalf("reading the exported artifact from disk (%s): %v", artifactPath, err)
	}
	if string(diskBytes) != wantText {
		t.Fatalf("artifact on disk does not match query_sql_text byte for byte:\n got  (%d bytes)\n want (%d bytes)",
			len(diskBytes), len(wantText))
	}
}

// planAggRow is one row of query_plans_*.sql's own ten columns, scanned
// exactly as the real, embedded query returns them - see
// runSubstitutedPlansQuery.
type planAggRow struct {
	planID          int64
	replicaGroup    sql.NullInt64
	forced          bool
	executions      int64
	cpuTotalMs      float64
	cpuAvgMs        sql.NullFloat64
	durationTotalMs float64
	durationAvgMs   sql.NullFloat64
	readsTotal      float64
	readsAvg        sql.NullFloat64
}

// runSubstitutedPlansQuery runs query (already table-substituted, per
// runQueryPlansAggregationFixture's own doc comment) for queryID over
// [since, until), scanning every row into a planAggRow.
func runSubstitutedPlansQuery(ctx context.Context, t *testing.T, conn *sql.Conn, query string, queryID int64, since, until time.Time) []planAggRow {
	t.Helper()
	dbRows, err := conn.QueryContext(ctx, query, sql.Named("id", queryID), sql.Named("since", since), sql.Named("until", until))
	if err != nil {
		t.Fatalf("running the substituted plans aggregation query: %v", err)
	}
	defer dbRows.Close()

	var got []planAggRow
	for dbRows.Next() {
		var r planAggRow
		if err := dbRows.Scan(&r.planID, &r.replicaGroup, &r.forced, &r.executions,
			&r.cpuTotalMs, &r.cpuAvgMs, &r.durationTotalMs, &r.durationAvgMs, &r.readsTotal, &r.readsAvg); err != nil {
			t.Fatalf("scanning plans aggregation row: %v", err)
		}
		got = append(got, r)
	}
	if err := dbRows.Err(); err != nil {
		t.Fatalf("reading plans aggregation rows: %v", err)
	}
	return got
}

// findPlanAggRow returns the one row of got matching planID and a
// non-NULL replicaGroup equal to replicaGroup, failing the test if
// there is none.
func findPlanAggRow(t *testing.T, got []planAggRow, planID, replicaGroup int64) planAggRow {
	t.Helper()
	for _, r := range got {
		if r.planID == planID && r.replicaGroup.Valid && r.replicaGroup.Int64 == replicaGroup {
			return r
		}
	}
	t.Fatalf("no row for plan_id=%d replica_group=%d in %+v", planID, replicaGroup, got)
	return planAggRow{}
}

// findPlanAggRowNoGroup returns the one row of got matching planID
// with a NULL replicaGroup - either 2019's own permanent shape, or a
// plan with no matching runtime row at all in the requested window on
// 2022 - failing the test if there is none.
func findPlanAggRowNoGroup(t *testing.T, got []planAggRow, planID int64) planAggRow {
	t.Helper()
	for _, r := range got {
		if r.planID == planID && !r.replicaGroup.Valid {
			return r
		}
	}
	t.Fatalf("no row for plan_id=%d with a NULL replica_group in %+v", planID, got)
	return planAggRow{}
}

// runQueryPlansAggregationFixture proves query_plans_2019.sql/query_plans_2022.sql's
// real, embedded aggregation formula against synthetic data in temp
// tables standing in for the catalog views (fix 1's B2, B3 and B7 -
// design spec line 79 names this synthetic proof explicitly, and the
// fake driver this package's unit tests use cannot carry it: it
// returns results already computed by the test itself, never by
// running the real SQL). It substitutes ONLY the FROM/JOIN table names
// in the real, embedded query text (diagnostics.QueryPlansSQL), never
// the aggregation expressions themselves, mirroring top_test.go's own
// runExactAggregationFixture exactly.
//
// The fixture is one query_id (555) with three plans: plan 10 appears
// on TWO replica groups (group 1: two runtime rows on the same
// plan/interval, one as if flushed and one still in memory, per
// Microsoft's own documented reason sys.query_store_runtime_stats can
// hold more than one row there - summing to executions=10,
// cpu_total_ms=28; group 2: a single row, executions=2,
// cpu_total_ms=10); plan 11 (forced) on group 1 alone, independent of
// plan 10; plan 12 with no runtime row at all, the zero-execution case
// query_plans_*.sql's LEFT JOIN must still report as a row. Two rows
// with execution_type 3 and 4 are also seeded on plan 10/group 1, with
// grossly inflated values, and must never contribute to any total
// (B7's first half).
//
// This shape is what actually separates a correct "GROUP BY plan_id,
// replica_group_id" from a broken "GROUP BY plan_id" with
// MIN(replica_group_id): measured by a reviewer, that exact mutation
// merges plan 10's two groups into one row (executions=12,
// cpu_total_ms=38 - 10+2 and 28+10) and drops the row count from four
// to three, while leaving every existing TestQuery* test green.
func runQueryPlansAggregationFixture(ctx context.Context, t *testing.T, lab *Lab, major int) {
	t.Helper()

	// #temp tables are connection-scoped: every statement below must run
	// on this one *sql.Conn, never through lab.Admin's pool directly.
	conn, err := lab.Admin.Conn(ctx)
	if err != nil {
		t.Fatalf("acquiring a dedicated connection for temp tables: %v", err)
	}
	defer conn.Close()

	ddl := []string{
		`CREATE TABLE #plan (plan_id bigint, query_id bigint, is_forced_plan bit)`,
		`CREATE TABLE #interval (runtime_stats_interval_id bigint, start_time datetime2, end_time datetime2)`,
		`CREATE TABLE #rs (
			plan_id bigint, runtime_stats_interval_id bigint, replica_group_id bigint NULL,
			execution_type int, count_executions bigint, avg_cpu_time bigint, avg_duration bigint, avg_logical_io_reads bigint)`,
	}
	for _, stmt := range ddl {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("creating temp table (%s): %v", stmt, err)
		}
	}

	const queryID = int64(555)
	windowSince := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	windowUntil := windowSince.Add(time.Hour)
	intervalStart := windowSince.Add(time.Minute)
	intervalEnd := intervalStart.Add(time.Minute)

	if _, err := conn.ExecContext(ctx, "INSERT INTO #interval (runtime_stats_interval_id, start_time, end_time) VALUES (@p1,@p2,@p3)",
		int64(1), intervalStart, intervalEnd); err != nil {
		t.Fatalf("seeding #interval: %v", err)
	}

	if _, err := conn.ExecContext(ctx, "INSERT INTO #plan (plan_id, query_id, is_forced_plan) VALUES (@p1,@p2,@p3), (@p4,@p5,@p6), (@p7,@p8,@p9)",
		int64(10), queryID, false,
		int64(11), queryID, true,
		int64(12), queryID, false,
	); err != nil {
		t.Fatalf("seeding #plan: %v", err)
	}

	insertRS := "INSERT INTO #rs (plan_id, runtime_stats_interval_id, replica_group_id, execution_type, count_executions, avg_cpu_time, avg_duration, avg_logical_io_reads) VALUES (@p1,@p2,@p3,@p4,@p5,@p6,@p7,@p8)"
	rsRows := [][8]any{
		{int64(10), int64(1), int64(1), 0, int64(1), int64(1000), int64(2000), int64(30)},
		{int64(10), int64(1), int64(1), 0, int64(9), int64(3000), int64(1000), int64(10)},
		// plan 10, DIFFERENT replica group (2): the row that separates
		// grouping by replica_group_id from grouping by plan_id alone.
		{int64(10), int64(1), int64(2), 0, int64(2), int64(5000), int64(3000), int64(40)},
		// execution_type 3/4: aborted, must never contribute to any
		// total - grossly large values so an accidental inclusion
		// cannot hide (B7's first half).
		{int64(10), int64(1), int64(1), 3, int64(1000), int64(999999), int64(999999), int64(999999)},
		{int64(10), int64(1), int64(1), 4, int64(1000), int64(999999), int64(999999), int64(999999)},
		// plan 11, forced, its own independent group.
		{int64(11), int64(1), int64(1), 0, int64(10), int64(1000), int64(1000), int64(5)},
	}
	for _, r := range rsRows {
		if _, err := conn.ExecContext(ctx, insertRS, r[0], r[1], r[2], r[3], r[4], r[5], r[6], r[7]); err != nil {
			t.Fatalf("seeding #rs: %v", err)
		}
	}

	baseQuery := diagnostics.QueryPlansSQL(major)
	substituteTables := strings.NewReplacer(
		"sys.query_store_runtime_stats_interval", "#interval",
		"sys.query_store_runtime_stats", "#rs",
		"sys.query_store_plan", "#plan",
	).Replace
	query := substituteTables(baseQuery)

	got := runSubstitutedPlansQuery(ctx, t, conn, query, queryID, windowSince, windowUntil)
	const tol = 1e-9

	if major >= 16 {
		if len(got) != 4 {
			t.Fatalf("got %d rows, want 4 (plan 10/group 1, plan 10/group 2, plan 11, plan 12): %+v", len(got), got)
		}
		g1 := findPlanAggRow(t, got, 10, 1)
		if g1.executions != 10 {
			t.Fatalf("plan 10/group 1 executions: got %d, want 10", g1.executions)
		}
		if math.Abs(g1.cpuTotalMs-28) > tol {
			t.Fatalf("plan 10/group 1 cpu_total_ms: got %v, want 28 (tolerance %v)", g1.cpuTotalMs, tol)
		}
		wantAvg1 := 28000.0 / 10 / 1000.0
		if !g1.cpuAvgMs.Valid || math.Abs(g1.cpuAvgMs.Float64-wantAvg1) > tol {
			t.Fatalf("plan 10/group 1 cpu_avg_ms: got %+v, want %v (tolerance %v)", g1.cpuAvgMs, wantAvg1, tol)
		}

		g2 := findPlanAggRow(t, got, 10, 2)
		if g2.executions != 2 {
			t.Fatalf("plan 10/group 2 executions: got %d, want 2", g2.executions)
		}
		if math.Abs(g2.cpuTotalMs-10) > tol {
			t.Fatalf("plan 10/group 2 cpu_total_ms: got %v, want 10 (tolerance %v)", g2.cpuTotalMs, tol)
		}

		p11 := findPlanAggRow(t, got, 11, 1)
		if p11.executions != 10 {
			t.Fatalf("plan 11 executions: got %d, want 10", p11.executions)
		}
		if !p11.forced {
			t.Fatal("plan 11 forced: got false, want true")
		}
		if math.Abs(p11.cpuTotalMs-10) > tol {
			t.Fatalf("plan 11 cpu_total_ms: got %v, want 10 (tolerance %v)", p11.cpuTotalMs, tol)
		}

		p12 := findPlanAggRowNoGroup(t, got, 12)
		if p12.executions != 0 {
			t.Fatalf("plan 12 executions: got %d, want 0 (no matching runtime row)", p12.executions)
		}
		if p12.cpuAvgMs.Valid {
			t.Fatalf("plan 12 cpu_avg_ms: got %+v, want NULL", p12.cpuAvgMs)
		}
		if p12.forced {
			t.Fatal("plan 12 forced: got true, want false")
		}
	} else {
		// 2019 has no replica_group_id column at all: plan 10's group 1
		// (two rows) and group 2 (one row) all combine into ONE row -
		// executions=10+2=12, cpu_total_ms=28+10=38, the same numbers
		// the 2022 mutation this fixture guards against would wrongly
		// produce - which is expected: 2019 genuinely has no way to
		// separate them.
		if len(got) != 3 {
			t.Fatalf("got %d rows, want 3 on 2019 (plan 10 merged, plan 11, plan 12): %+v", len(got), got)
		}
		p10 := findPlanAggRowNoGroup(t, got, 10)
		if p10.executions != 12 {
			t.Fatalf("plan 10 executions: got %d, want 12 (10 + 2, no replica grouping on 2019)", p10.executions)
		}
		if math.Abs(p10.cpuTotalMs-38) > tol {
			t.Fatalf("plan 10 cpu_total_ms: got %v, want 38 (tolerance %v)", p10.cpuTotalMs, tol)
		}
	}

	// B7, second half: a boundary window whose "until" lands exactly on
	// interval 1's own start_time. The half-open predicate
	// (i.start_time < @until) must exclude that interval entirely -
	// plan 11 (single group, no other interval) must report zero
	// executions. A "closed bounds" mutation (i.start_time <= @until)
	// would include it instead and report 10.
	boundary := runSubstitutedPlansQuery(ctx, t, conn, query, queryID, windowSince, intervalStart)
	b11 := findPlanAggRowNoGroup(t, boundary, 11)
	if b11.executions != 0 {
		t.Fatalf("plan 11 at the exact interval boundary: got %d executions, want 0 (half-open predicate)", b11.executions)
	}
}

// TestQueryPlansSyntheticAggregation runs
// runQueryPlansAggregationFixture against a real engine.
func TestQueryPlansSyntheticAggregation(t *testing.T) {
	lab := NewLab(t, os.Getenv("ASQ_TEST_IMAGE"))
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	sess, err := sqlserver.Open(ctx, lab.Profile)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer sess.Close()
	logEngineIdentity(t, lab, sess.Major)

	runQueryPlansAggregationFixture(ctx, t, lab, sess.Major)
}
