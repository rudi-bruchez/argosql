package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
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

// helpRow is one row of help --json's "commands" table, by column
// position: name, summary, flags, examples, units, permissions,
// versions (see helpTableSpec in registry.go).
type helpRow struct {
	name, summary, flags, examples, units, permissions, versions string
}

// helpRows runs "help --json" and decodes its one table into helpRow
// values, keyed by command name, failing the test on any shape problem
// along the way rather than letting a caller misread a missing field as
// an empty one.
func helpRows(t *testing.T) map[string]helpRow {
	t.Helper()
	var out, errout bytes.Buffer
	code := Run(context.Background(), []string{"help", "--json"}, &out, &errout)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errout.String())
	}
	var env struct {
		Tables []struct {
			Rows [][]any `json:"rows"`
		} `json:"tables"`
	}
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("invalid JSON: %v (%s)", err, out.String())
	}
	if len(env.Tables) != 1 {
		t.Fatalf("got %d tables, want exactly 1 (%s)", len(env.Tables), out.String())
	}
	rows := map[string]helpRow{}
	for _, r := range env.Tables[0].Rows {
		if len(r) != 7 {
			t.Fatalf("help row has %d columns, want 7: %v", len(r), r)
		}
		field := func(i int) string {
			s, _ := r[i].(string)
			return s
		}
		name := field(0)
		rows[name] = helpRow{
			name: name, summary: field(1), flags: field(2), examples: field(3),
			units: field(4), permissions: field(5), versions: field(6),
		}
	}
	return rows
}

// TestHelpOfflineListsAllCommands proves runHelp actually iterates the
// registry: TestHelpOffline alone only checks the exit code and that
// stdout parses as JSON, which stays green even if help's row loop is
// deleted entirely (a reviewer demonstrated exactly that by removing
// it). This test fails the moment any of the three registered commands
// is missing from the rendered inventory, by name.
func TestHelpOfflineListsAllCommands(t *testing.T) {
	rows := helpRows(t)
	want := []string{"help", "info", "qs status"}
	for _, name := range want {
		if _, ok := rows[name]; !ok {
			t.Fatalf("help output is missing command %q: got %v", name, rows)
		}
	}
	if len(rows) != len(want) {
		t.Fatalf("got %d command rows, want exactly %d: %v", len(rows), len(want), rows)
	}
}

// TestHelpOfflineRendersDeclaredMetadata proves Command's help-only
// fields (Units, Permissions, Versions) actually reach the rendered
// output, not just exist on the struct: dropping any one of them from
// runHelp's row would leave every other cli test green.
func TestHelpOfflineRendersDeclaredMetadata(t *testing.T) {
	rows := helpRows(t)
	info, ok := rows["info"]
	if !ok {
		t.Fatal("help output is missing command \"info\"")
	}
	if !strings.Contains(info.units, "compatibility_level") {
		t.Fatalf("info's units do not mention compatibility_level: %q", info.units)
	}
	if !strings.Contains(info.permissions, "CONNECT") {
		t.Fatalf("info's permissions do not mention CONNECT: %q", info.permissions)
	}
	if !strings.Contains(info.versions, "2019") || !strings.Contains(info.versions, "2022") {
		t.Fatalf("info's versions do not mention 2019/2022: %q", info.versions)
	}
	status, ok := rows["qs status"]
	if !ok {
		t.Fatal("help output is missing command \"qs status\"")
	}
	if !strings.Contains(status.permissions, "VIEW DATABASE STATE") {
		t.Fatalf("qs status's permissions do not mention VIEW DATABASE STATE: %q", status.permissions)
	}
}

// TestHelpOfflineRendersFlagDefaultsBoundsAndEnum proves help's
// inventory carries a flag's default, bound and enum - design spec
// line 45 ("parameters, defaults, and examples"): an agent reading help
// must be able to tell which values are valid without trying one.
// Flag.Validate already enforces these; this test is about whether
// describeFlags actually renders them.
func TestHelpOfflineRendersFlagDefaultsBoundsAndEnum(t *testing.T) {
	rows := helpRows(t)
	info, ok := rows["info"]
	if !ok {
		t.Fatal("help output is missing command \"info\"")
	}
	for _, want := range []string{
		"timeout:int64=30 range=1..300",
		"preview:int64=10 range=0..10000",
		"truncate:int64=200 range=1..10000",
		"format:string=tsv enum=tsv|json",
		"no-truncate:bool=false",
	} {
		if !strings.Contains(info.flags, want) {
			t.Fatalf("info's flags do not contain %q: %q", want, info.flags)
		}
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

// TestRunEmitsUnvalidatedVersionNotice is the brief's directive case,
// extended with Major 18: Run must test Major for strict equality to
// 17, not "17 or above". sqlserver.Open today only ever hands back 15,
// 16 or 17, so 18 cannot happen through the real path yet - but the
// fake sessionOpener bypasses Open entirely, which is exactly why this
// case can be written now, before any future major version exists for
// real: the day Open's own allow-list grows to admit 18, a ">=17"
// condition here would start emitting an "unvalidated" notice for a
// version that may by then be validated, with nothing to catch it.
func TestRunEmitsUnvalidatedVersionNotice(t *testing.T) {
	profile := config.Profile{Host: "fake", Database: "db", Username: "user", Password: "secret", Port: 1433, TrustServerCertificate: true}

	cases := []struct {
		major int
		want  int
	}{
		{17, 1},
		{16, 0},
		{18, 0},
	}
	for _, c := range cases {
		t.Run(fmt.Sprintf("major %d", c.major), func(t *testing.T) {
			var out, errout bytes.Buffer
			args := []string{"--ctx", "x", "--format", "json", "--out-dir", t.TempDir(), "info"}
			run(context.Background(), args, &out, &errout, fakeLoadConfig(profile), fakeOpenSession(c.major))
			n, err := countNoticeKind(out.Bytes(), "unvalidated_version")
			if err != nil {
				t.Fatalf("invalid JSON output: %v (%s)", err, out.String())
			}
			if n != c.want {
				t.Fatalf("got %d unvalidated_version notices, want exactly %d (%s)", n, c.want, out.String())
			}
		})
	}
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
			var env struct {
				Error struct {
					Code    int    `json:"code"`
					Kind    string `json:"kind"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(out.Bytes(), &env); err != nil {
				t.Fatalf("invalid JSON: %v (%s)", err, out.String())
			}
			if env.Error.Code != 5 {
				t.Fatalf("error.code = %d, want 5", env.Error.Code)
			}
			if env.Error.Kind != "not_implemented" {
				t.Fatalf("error.kind = %q, want not_implemented", env.Error.Kind)
			}
			if !strings.Contains(env.Error.Message, name) {
				t.Fatalf("error.message = %q, does not name the command %q", env.Error.Message, name)
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
