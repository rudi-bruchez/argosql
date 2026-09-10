package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/rudi-bruchez/argosql/internal/config"
	"github.com/rudi-bruchez/argosql/internal/model"
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

// TestRunStdoutWriteFailureReturnsCode6 is fix 1's A3, rewritten rather
// than left as the task-15 version that only documented the defect: a
// test that consecrates a bug is worse than no test at all. Design
// spec line 210, verbatim: "A later output failure uses code 6 even if
// the collected data was already partial." A failing stdout writer -
// closed pipe, reader gone away - must make run report code 6, not the
// command's own (here successful, code 0) result, because nothing was
// actually delivered to the caller.
func TestRunStdoutWriteFailureReturnsCode6(t *testing.T) {
	profile := config.Profile{Host: "fake", Database: "db", Username: "user", Password: "secret", Port: 1433, TrustServerCertificate: true}
	args := []string{"--ctx", "x", "--format", "json", "--out-dir", t.TempDir(), "info"}

	done := make(chan int, 1)
	go func() {
		var errout bytes.Buffer
		done <- run(context.Background(), args, failWriter{}, &errout, fakeLoadConfig(profile), fakeOpenSession(t, 16))
	}()

	select {
	case code := <-done:
		if code != 6 {
			t.Fatalf("exit code: got %d, want 6 (design spec line 210: a later output failure uses code 6 even if the collected data was already partial)", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not return within 10s: a failing stdout writer must never hang the whole invocation")
	}
}

// TestRunStdoutWriteFailureReturnsCode6BeforeConnection is the same
// contract at emitError's own site (writeError, run.go): an argument
// error raised before any connection, whose JSON fallback envelope
// itself then fails to write, must still report code 6 - not the
// argument error's own code 2 - because the envelope never reached the
// caller either.
func TestRunStdoutWriteFailureReturnsCode6BeforeConnection(t *testing.T) {
	profile := config.Profile{Host: "fake", Database: "db", Username: "user", Password: "secret", Port: 1433, TrustServerCertificate: true}
	// --timeout 0 is rejected by Parse itself, before loadConfig or
	// openSession are ever reached - an error this test can force
	// deterministically without any real connection.
	args := []string{"--ctx", "x", "--format", "json", "--out-dir", t.TempDir(), "--timeout", "0", "info"}
	var errout bytes.Buffer
	code := run(context.Background(), args, failWriter{}, &errout, fakeLoadConfig(profile), fakeOpenSession(t, 16))
	if code != 6 {
		t.Fatalf("exit code: got %d, want 6 (design spec line 210 applies to writeError's own JSON envelope too)", code)
	}
}

// TestRunRedactsSecretFromJSONErrorEnvelope is fix 1's A1, the highest-
// severity finding of this pass: stderr already passes every error
// through redact before writing it; the JSON envelope on stdout did
// not, for either the pre-connection fallback (writeError) or the
// normal completed-run path (result.Error, rendered by output.Render).
// A driver or collection error whose message happens to contain the
// connection secret - a storage path built from it here, a DSN
// returned by a real driver in production - must never let that
// secret reach stdout while stderr masks the exact same text.
//
// The session opener below returns an error whose message embeds
// profile.Password directly, synthesizing the asymmetry the reviewer
// measured against a real driver without needing one here.
func TestRunRedactsSecretFromJSONErrorEnvelope(t *testing.T) {
	const secret = `p@ss"word\with/escapes`
	profile := config.Profile{Host: "fake", Database: "db", Username: "user", Password: secret, Port: 1433, TrustServerCertificate: true}
	args := []string{"--ctx", "x", "--format", "json", "--out-dir", t.TempDir(), "info"}

	failOpen := func(ctx context.Context, p config.Profile) (*sqlserver.Session, error) {
		return nil, &model.PublicError{Code: 3, Kind: "connection", Message: fmt.Sprintf("dial tcp failed for dsn sqlserver://user:%s@fake:1433", p.Password)}
	}

	var out, errout bytes.Buffer
	code := run(context.Background(), args, &out, &errout, fakeLoadConfig(profile), failOpen)
	if code != 3 {
		t.Fatalf("exit code: got %d, want 3 (connection)", code)
	}
	if strings.Contains(errout.String(), secret) {
		t.Fatalf("stderr leaks the secret, that is the OLD behavior this test must not regress past: %s", errout.String())
	}
	if strings.Contains(out.String(), secret) {
		t.Fatalf("stdout JSON envelope leaks the secret in the raw bytes: %s", out.String())
	}
	var envelope struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(out.Bytes(), &envelope); err != nil {
		t.Fatalf("decoding stdout as JSON: %v\nstdout: %s", err, out.String())
	}
	if strings.Contains(envelope.Error.Message, secret) {
		t.Fatalf("error.message, once JSON-DECODED, still carries the secret: %q", envelope.Error.Message)
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

// TestRedactCoversTheURLEncodedPassword is the harm review's second
// credential finding: redact did a literal ReplaceAll of the password,
// but config.DSN builds its connection string with url.UserPassword,
// which percent-escapes the userinfo. A driver error echoing that DSN
// therefore carried a form the literal replacement walked straight past.
// The two assertions are deliberately separate: the first would pass even
// with the old code, and only the second fails on it.
func TestRedactCoversTheURLEncodedPassword(t *testing.T) {
	const secret = `p@ss"word\with/escapes`
	p := config.Profile{Password: secret}

	encoded := encodedPassword(secret)
	if encoded == secret {
		t.Fatalf("this test is vacuous unless net/url actually escapes the password: got %q", encoded)
	}
	if got := redact("error mentioning "+secret, p); strings.Contains(got, secret) {
		t.Fatalf("the literal form survived redaction: %q", got)
	}
	if got := redact("dial failed for sqlserver://user:"+encoded+"@host:1433", p); strings.Contains(got, encoded) {
		t.Fatalf("the percent-encoded form survived redaction: %q", got)
	}
}
