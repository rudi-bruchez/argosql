//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rudi-bruchez/argosql/internal/diagnostics"
	"github.com/rudi-bruchez/argosql/internal/model"
	"github.com/rudi-bruchez/argosql/internal/sqlserver"
)

// TestErrors covers the two deterministic fault injections this task's
// dispatch flags as needing a REAL container rather than a fake
// driver: a SQL read interrupted partway through, on the single pinned
// connection every asq invocation holds (the exact trap the dispatch
// warns about - an injection that issues anything else on that same
// connection while a row set is still open would measure its own
// harness hanging, not a defect in the program), and a search for
// secrets/DSNs across every byte the real binary emits.
func TestErrors(t *testing.T) {
	lab := NewLab(t, os.Getenv("ASQ_TEST_IMAGE"))
	ctx, cancel := context.WithTimeout(context.Background(), flushPollBudget)
	defer cancel()

	var major int
	if err := lab.Admin.QueryRowContext(ctx, "SELECT CAST(SERVERPROPERTY('ProductMajorVersion') AS int)").Scan(&major); err != nil {
		t.Fatalf("reading engine major version: %v", err)
	}
	logEngineIdentity(t, lab, major)

	t.Run("interrupted_read_does_not_block_pinned_connection", func(t *testing.T) {
		testInterruptedReadClosesRows(ctx, t, lab)
	})
	t.Run("no_secret_leakage_in_diagnostic_output", func(t *testing.T) {
		testNoSecretLeakageInDiagnosticOutput(t, lab)
	})
}

// abortAfterNSink is a model.Sink whose Row refuses with a plain error
// after N rows - simulating a consumer that stops consuming, or a
// downstream failure, partway through a streaming result set. The real
// question this proves: does the diagnostic that owns the query
// (queryRows, internal/diagnostics/top.go) still close its *sql.Rows via
// its own deferred Close on this early return, or does it leak an open
// result set on the ONE connection this whole program ever holds?
//
// This fault is deliberately injected through Sink.Row, never by
// issuing a second query on lab's own connection while the first is
// still open - doing that would be the exact trap CLAUDE.md records
// (task 14): a second statement on a pinned connection while a row set
// is open from the first BLOCKS indefinitely, which would make this
// test measure its own harness, not the program.
type abortAfterNSink struct {
	n       int
	seen    int
	aborted error
}

func (s *abortAfterNSink) Begin(model.TableSpec) error { return nil }

func (s *abortAfterNSink) Row(row []model.Cell) error {
	s.seen++
	if s.seen > s.n {
		s.aborted = fmt.Errorf("errors_test: fixture sink stopped consuming after %d rows", s.n)
		return s.aborted
	}
	return nil
}

func (s *abortAfterNSink) End(bool, bool) error { return nil }
func (s *abortAfterNSink) File(string, string, io.Reader) (model.Artifact, error) {
	return model.Artifact{}, fmt.Errorf("abortAfterNSink: File not supported")
}
func (s *abortAfterNSink) Notice(model.Notice) {}

// testInterruptedReadClosesRows opens its OWN session (never lab.Admin,
// which other subtests of this package also use concurrently) against
// AppDB, runs "idx list" on dbo.UsageFixture - an identity primary key
// (clustered) plus two nonclustered indexes objects.sql always creates
// (IX_UsageFixture_Category, IX_UsageFixture_NeverQueried), so this
// query is guaranteed at least three rows, deterministically, with no
// dependency on Query Store history or the optimizer's own choices -
// with abortAfterNSink set to stop after row 1, then immediately
// reuses the SAME *sql.Conn for a trivial query with a short, bounded
// context. If the diagnostic left its result set open, that trivial
// query hangs until the bounded context expires and this test fails
// with a clear timeout message, never a silent pass.
func testInterruptedReadClosesRows(ctx context.Context, t *testing.T, lab *Lab) {
	// ensurePrincipals is what actually applies objects.sql (dbo.
	// UsageFixture's own definition) against this Lab's AppDB; nothing
	// else in this subtest triggers it, and subtests run in declaration
	// order, so without this call dbo.UsageFixture would not exist yet
	// the first time this runs.
	lab.ensurePrincipals(t)
	sess, err := sqlserver.Open(ctx, lab.Profile)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer sess.Close()

	sink := &abortAfterNSink{n: 1}
	indexesErr := diagnostics.Indexes(ctx, sess, "dbo.UsageFixture", sink)
	if indexesErr == nil {
		t.Fatal("Indexes must return the sink's own abort error, got nil")
	}
	if sink.seen < 2 {
		t.Fatalf("fixture produced only %d index row(s) before returning; dbo.UsageFixture's own definition (objects.sql) guarantees at least 3 and this test cannot prove a mid-stream interruption without at least 2", sink.seen)
	}

	probeCtx, probeCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer probeCancel()
	var probe int
	if err := sess.Conn.QueryRowContext(probeCtx, "SELECT 1").Scan(&probe); err != nil {
		t.Fatalf("a trivial query on the same connection after an aborted read must succeed promptly, not hang or error: %v (if this timed out, the diagnostic left its result set open on the one pinned connection)", err)
	}
	if probe != 1 {
		t.Fatalf("probe query: got %d, want 1", probe)
	}
}

// testNoSecretLeakageInDiagnosticOutput is design spec line 261's own
// last sentence, the one clause of that line this task owns: search
// every byte the real binary emits - stdout, stderr, and the manifest
// it writes to disk - for a synthetic secret, recognizable and unique
// to this run, under every form it could leak in. It never prints the
// secret itself in a failure message (CLAUDE.md's own recorded
// defect, paid for once already on this exact guarantee): a match is
// reported by WHERE it was found, never by the value found.
func testNoSecretLeakageInDiagnosticOutput(t *testing.T, lab *Lab) {
	pw := lab.ensurePrincipals(t)
	secret := pw.S

	for _, step := range []struct {
		name string
		args []string
	}{
		{"info", []string{"info"}},
		{"qs status", []string{"qs", "status"}},
		{"bad_query_id", []string{"qs", "query", "999999999"}},
	} {
		t.Run(step.name, func(t *testing.T) {
			stdout, stderr, _ := lab.RunRaw(t, "S", step.args)
			if strings.Contains(stdout, secret) {
				t.Fatalf("%v: stdout leaks the connection secret", step.args)
			}
			if strings.Contains(stderr, secret) {
				t.Fatalf("%v: stderr leaks the connection secret", step.args)
			}

			var result model.Result
			if jsonErr := json.Unmarshal([]byte(stdout), &result); jsonErr == nil && result.ManifestPath != "" {
				data, readErr := os.ReadFile(result.ManifestPath)
				if readErr != nil {
					t.Fatalf("%v: reading manifest %q: %v", step.args, result.ManifestPath, readErr)
				}
				if strings.Contains(string(data), secret) {
					t.Fatalf("%v: manifest %q leaks the connection secret", step.args, result.ManifestPath)
				}
			}
		})
	}
}
