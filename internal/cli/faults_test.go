package cli

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rudi-bruchez/argosql/internal/config"
	"github.com/rudi-bruchez/argosql/internal/sqlserver"
)

// Task 15's own deterministic fault injections for internal/cli: a
// closed stdout pipe, and an argument error that must never reach a
// connection attempt. TestRunRejectsMalformedPositionalObjectBeforeConnecting
// (run_test.go) already proves one instance of the second kind for a
// malformed positional object; TestRunRejectsOutOfRangeTimeoutBeforeConnecting
// below applies the same never-invoked-opener technique to a different
// argument error (an out-of-range --timeout) rather than redefining it.

// failWriter is the brief's own verbatim fault: a writer that refuses
// every write it is given. Test-local, no production flag - the same
// construct internal/artifacts/faults_test.go uses, kept as its own
// copy here rather than exported across packages for a nine-line type.
type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("fixture disk failure") }

// TestRunStdoutWriteFailureIsObservedNotHidden is "un pipe stdout fermé
// ... constater l'échec d'écriture sans prétendre le contraire" (the
// brief's own wording for design spec line 105/111): when stdout itself
// refuses every write - a closed pipe, a reader that has already gone
// away - run must not panic and must still return promptly with a
// deterministic exit code, never hang while trying to deliver a result
// it cannot actually write.
//
// This test deliberately does NOT assert that the returned exit code
// itself changes to reflect the write failure: run's observed behavior
// today is that stdout.Write's own error return is discarded (see
// run.go's bare "stdout.Write(out)"), so a command that otherwise
// succeeded still reports that command's own exit code even though
// nothing was actually delivered on stdout. Asserting success here
// would be exactly the "prétendre le contraire" the brief warns
// against; this test instead pins the narrower, true property - no
// panic, no hang - and the wider one is recorded as an open question in
// the task report, not quietly papered over.
func TestRunStdoutWriteFailureIsObservedNotHidden(t *testing.T) {
	profile := config.Profile{Host: "fake", Database: "db", Username: "user", Password: "secret", Port: 1433, TrustServerCertificate: true}
	args := []string{"--ctx", "x", "--format", "json", "--out-dir", t.TempDir(), "info"}

	done := make(chan int, 1)
	go func() {
		var errout bytes.Buffer
		done <- run(context.Background(), args, failWriter{}, &errout, fakeLoadConfig(profile), fakeOpenSession(t, 16))
	}()

	select {
	case <-done:
		// No panic (a panic in this goroutine would fail the test binary
		// outright, not just this test), no hang: the property this test
		// actually pins.
	case <-time.After(10 * time.Second):
		t.Fatal("run did not return within 10s: a failing stdout writer must never hang the whole invocation")
	}
}

// TestRunRejectsOutOfRangeTimeoutBeforeConnecting proves --timeout 0
// (out of its documented 1-300s range) is refused at code 2 without
// ever reaching openSession - the same ordering CLAUDE.md records as a
// corollary worth protecting: an argument error must surface before
// any connection attempt, never as a connection error for an
// unreachable host. The host below (198.51.100.1, TEST-NET-2) is
// deliberately non-routable so that, had the validation been removed,
// this test would fail loudly on a hang or a wrong exit code rather
// than accidentally succeeding against a real server.
func TestRunRejectsOutOfRangeTimeoutBeforeConnecting(t *testing.T) {
	profile := config.Profile{Host: "198.51.100.1", Database: "db", Username: "user", Password: "secret", Port: 1433, TrustServerCertificate: true}
	var out, errout bytes.Buffer
	args := []string{"--ctx", "x", "--format", "json", "--out-dir", t.TempDir(), "--timeout", "0", "info"}

	neverOpen := func(ctx context.Context, p config.Profile) (*sqlserver.Session, error) {
		t.Fatal("openSession must never be called for --timeout 0, which Parse should reject before any connection is attempted")
		return nil, nil
	}
	code := run(context.Background(), args, &out, &errout, fakeLoadConfig(profile), neverOpen)
	if code != 2 {
		t.Fatalf("exit code: got %d, want 2 (argument error)", code)
	}
}
