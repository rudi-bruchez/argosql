package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/rudi-bruchez/argosql/internal/config"
	"github.com/rudi-bruchez/argosql/internal/model"
	"github.com/rudi-bruchez/argosql/internal/sqlserver"
)

// TestHelpOffline is the brief's directive case, verbatim.
func TestHelpOffline(t *testing.T) {
	var out, errout bytes.Buffer
	code := Run(context.Background(), []string{"help", "--json"}, &out, &errout)
	if code != 0 || !json.Valid(out.Bytes()) {
		t.Fatalf("%d %s", code, errout.String())
	}
}

// TestHelpOfflineWithoutConfigDir proves help keeps working on an
// environment that has neither a configuration directory nor a cache
// directory - exactly the case os.UserConfigDir/os.UserCacheDir fail on,
// which is why Run must never call them for an offline command. Without
// the deferral, this test falls over with the same code-2 config_path
// error a connecting command would get - see the task report for the
// breakage that proved it.
func TestHelpOfflineWithoutConfigDir(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_CACHE_HOME", "")
	t.Setenv("APPDATA", "")
	t.Setenv("LocalAppData", "")

	var out, errout bytes.Buffer
	code := Run(context.Background(), []string{"help", "--json"}, &out, &errout)
	if code != 0 || !json.Valid(out.Bytes()) {
		t.Fatalf("%d %s", code, errout.String())
	}
}

func TestHelpOfflineDefaultIsTSV(t *testing.T) {
	var out, errout bytes.Buffer
	code := Run(context.Background(), []string{"help"}, &out, &errout)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errout.String())
	}
	if json.Valid(out.Bytes()) {
		t.Fatalf("default help output looks like JSON, want TSV: %s", out.String())
	}
	if !bytes.Contains(out.Bytes(), []byte("info")) {
		t.Fatalf("help output does not mention the info command: %s", out.String())
	}
}

// fakeLoadConfig and fakeOpenSession let run_test.go drive run (the
// unexported body behind Run) against a fabricated profile and a
// fabricated *sqlserver.Session - built directly from its exported
// fields, Conn and Major - without a config file or a network
// connection. This is what makes the Major==17/16 notice test below
// possible without a live SQL Server: sqlserver.Session's own package
// already exercises Probe/Resolve/etc. against a fake database/sql/driver
// (see its testdriver_test.go); this package only ever needs a Session
// value, never a live Conn, because the stub info/qs status Execute
// below never touches Conn at all.
func fakeLoadConfig(profile config.Profile) configLoader {
	return func(path, name, databaseOverride string, getenv func(string) string) (config.Profile, error) {
		return profile, nil
	}
}

func fakeOpenSession(major int) sessionOpener {
	return func(ctx context.Context, p config.Profile) (*sqlserver.Session, error) {
		return &sqlserver.Session{Major: major}, nil
	}
}

// resultEnvelope reads back just the field this test needs from
// Render's JSON output.
type resultEnvelope struct {
	Notices []model.Notice `json:"notices"`
}

func countNoticeKind(b []byte, kind string) (int, error) {
	var env resultEnvelope
	if err := json.Unmarshal(b, &env); err != nil {
		return 0, err
	}
	n := 0
	for _, notice := range env.Notices {
		if notice.Kind == kind {
			n++
		}
	}
	return n, nil
}

// TestRunEmitsUnvalidatedVersionNotice is the brief's directive case:
// Run, against a fake server reporting Major 17, emits exactly one
// unvalidated_version notice; against Major 16, zero.
func TestRunEmitsUnvalidatedVersionNotice(t *testing.T) {
	profile := config.Profile{Host: "fake", Database: "db", Username: "user", Password: "secret", Port: 1433, TrustServerCertificate: true}

	t.Run("major 17", func(t *testing.T) {
		var out, errout bytes.Buffer
		args := []string{"--ctx", "x", "--format", "json", "--out-dir", t.TempDir(), "info"}
		run(context.Background(), args, &out, &errout, fakeLoadConfig(profile), fakeOpenSession(17))
		n, err := countNoticeKind(out.Bytes(), "unvalidated_version")
		if err != nil {
			t.Fatalf("invalid JSON output: %v (%s)", err, out.String())
		}
		if n != 1 {
			t.Fatalf("got %d unvalidated_version notices, want exactly 1 (%s)", n, out.String())
		}
	})

	t.Run("major 16", func(t *testing.T) {
		var out, errout bytes.Buffer
		args := []string{"--ctx", "x", "--format", "json", "--out-dir", t.TempDir(), "info"}
		run(context.Background(), args, &out, &errout, fakeLoadConfig(profile), fakeOpenSession(16))
		n, err := countNoticeKind(out.Bytes(), "unvalidated_version")
		if err != nil {
			t.Fatalf("invalid JSON output: %v (%s)", err, out.String())
		}
		if n != 0 {
			t.Fatalf("got %d unvalidated_version notices, want exactly 0 (%s)", n, out.String())
		}
	})
}

// TestRunStubCommandsReportNotImplemented proves 9a's placeholder form
// for info and qs status: a stable *model.PublicError (code 5, kind
// not_implemented), not a panic or a silent success, until 9b replaces
// their Execute.
func TestRunStubCommandsReportNotImplemented(t *testing.T) {
	profile := config.Profile{Host: "fake", Database: "db", Username: "user", Password: "secret", Port: 1433, TrustServerCertificate: true}
	for _, name := range []string{"info", "qs status"} {
		t.Run(name, func(t *testing.T) {
			var out, errout bytes.Buffer
			args := append([]string{"--ctx", "x", "--format", "json", "--out-dir", t.TempDir()}, splitName(name)...)
			code := run(context.Background(), args, &out, &errout, fakeLoadConfig(profile), fakeOpenSession(16))
			if code != 5 {
				t.Fatalf("got code %d, want 5 (%s / %s)", code, out.String(), errout.String())
			}
			if !json.Valid(out.Bytes()) {
				t.Fatalf("stdout is not valid JSON: %s", out.String())
			}
		})
	}
}

func splitName(name string) []string {
	var parts []string
	start := 0
	for i := 0; i <= len(name); i++ {
		if i == len(name) || name[i] == ' ' {
			if i > start {
				parts = append(parts, name[start:i])
			}
			start = i + 1
		}
	}
	return parts
}

// TestErrorBeforeConnectionIsJSON exercises the design spec's "JSON
// errors use the same envelope" for an argument error raised before any
// connection is attempted: --timeout 0 is out of range, --format json
// was explicitly requested, and no config file or network access
// happens at all before Parse rejects it.
func TestErrorBeforeConnectionIsJSON(t *testing.T) {
	var out, errout bytes.Buffer
	code := Run(context.Background(), []string{"--ctx", "x", "--format", "json", "--timeout", "0", "info"}, &out, &errout)
	if code != 2 {
		t.Fatalf("code = %d, want 2 (%s)", code, errout.String())
	}
	if !json.Valid(out.Bytes()) {
		t.Fatalf("stdout is not valid JSON: %s", out.String())
	}
	var env struct {
		OK    bool `json:"ok"`
		Error struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.OK {
		t.Fatal("ok=true on an error response")
	}
	if env.Error.Code != 2 {
		t.Fatalf("error.code = %d, want 2", env.Error.Code)
	}
	if errout.Len() == 0 {
		t.Fatal("nothing written to stderr")
	}
}

func TestRedact(t *testing.T) {
	p := config.Profile{Password: "s3cr3t"}
	got := redact("login failed for user with password s3cr3t", p)
	if got != "login failed for user with password REDACTED" {
		t.Fatalf("redact did not scrub the secret: %q", got)
	}
	// No profile (no secret known yet, e.g. an argument error): a
	// no-op, not a crash on an empty needle.
	if got2 := redact("unchanged", config.Profile{}); got2 != "unchanged" {
		t.Fatalf("redact with no password changed the message: %q", got2)
	}
}

// TestExitCodeForSeparatesSignalFromTimeout is the pure-function version
// of "séparer expiration interne et signal utilisateur": a
// DeadlineExceeded runCtx keeps the error's own mapped code, a Canceled
// one always reports 130.
func TestExitCodeForSeparatesSignalFromTimeout(t *testing.T) {
	deadlineCtx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-deadlineCtx.Done()
	execErr := &model.PublicError{Code: 5, Kind: "execution", Message: "timed out"}
	if code := exitCodeFor(deadlineCtx, execErr); code != 5 {
		t.Fatalf("deadline-exceeded context: got %d, want 5", code)
	}

	signalCtx, stop := context.WithCancel(context.Background())
	stop()
	if code := exitCodeFor(signalCtx, execErr); code != 130 {
		t.Fatalf("canceled context: got %d, want 130", code)
	}

	if code := exitCodeFor(context.Background(), nil); code != 0 {
		t.Fatalf("nil error, live context: got %d, want 0", code)
	}
}
