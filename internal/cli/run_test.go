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

// helpTable is one decoded table from help --json's response, by name.
type helpTable struct {
	name string
	rows [][]any
}

// helpTables runs "help --json" and decodes its tables, keyed by
// table name, failing the test on any shape problem along the way
// rather than letting a caller misread a missing table as an empty
// one.
func helpTables(t *testing.T) map[string]helpTable {
	t.Helper()
	var out, errout bytes.Buffer
	code := Run(context.Background(), []string{"help", "--json"}, &out, &errout)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errout.String())
	}
	var env struct {
		Tables []struct {
			Spec struct {
				Name string `json:"name"`
			} `json:"spec"`
			Rows [][]any `json:"rows"`
		} `json:"tables"`
	}
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("invalid JSON: %v (%s)", err, out.String())
	}
	tables := map[string]helpTable{}
	for _, tbl := range env.Tables {
		tables[tbl.Spec.Name] = helpTable{name: tbl.Spec.Name, rows: tbl.Rows}
	}
	return tables
}

// helpCommandRow is one row of help --json's "commands" table, by
// column position: name, summary, examples, units, permissions,
// versions (see helpCommandsTableSpec in registry.go).
type helpCommandRow struct {
	name, summary, examples, units, permissions, versions string
}

func helpCommandRows(t *testing.T) map[string]helpCommandRow {
	t.Helper()
	tbl, ok := helpTables(t)["commands"]
	if !ok {
		t.Fatal("help output is missing the \"commands\" table")
	}
	rows := map[string]helpCommandRow{}
	for _, r := range tbl.rows {
		if len(r) != 6 {
			t.Fatalf("commands row has %d columns, want 6: %v", len(r), r)
		}
		field := func(i int) string {
			s, _ := r[i].(string)
			return s
		}
		name := field(0)
		rows[name] = helpCommandRow{
			name: name, summary: field(1), examples: field(2),
			units: field(3), permissions: field(4), versions: field(5),
		}
	}
	return rows
}

// helpFlagRow is one row of help --json's "flags" table, by column
// position: command, flag, kind, default, min, max, enum (see
// helpFlagsTableSpec in registry.go). min/max decode through
// encoding/json into float64 when present, or remain nil when the flag
// declares no bound.
type helpFlagRow struct {
	command, flag, kind string
	def                 any
	min, max            any
	enum                string
}

// helpFlagRows returns help --json's "flags" table rows, keyed by
// "command/flag".
func helpFlagRows(t *testing.T) map[string]helpFlagRow {
	t.Helper()
	tbl, ok := helpTables(t)["flags"]
	if !ok {
		t.Fatal("help output is missing the \"flags\" table")
	}
	rows := map[string]helpFlagRow{}
	for _, r := range tbl.rows {
		if len(r) != 7 {
			t.Fatalf("flags row has %d columns, want 7: %v", len(r), r)
		}
		str := func(i int) string {
			s, _ := r[i].(string)
			return s
		}
		command, flag := str(0), str(1)
		rows[command+"/"+flag] = helpFlagRow{
			command: command, flag: flag, kind: str(2),
			def: r[3], min: r[4], max: r[5], enum: str(6),
		}
	}
	return rows
}

// TestHelpOfflineListsAllCommands proves runHelp actually iterates the
// registry: TestHelpOffline alone only checks the exit code and that
// stdout parses as JSON, which stays green even if help's row loop is
// deleted entirely (a reviewer demonstrated exactly that by removing
// it). This test fails the moment any of the ten registered commands
// is missing from the rendered inventory, by name. Task 13 grew the
// registry from six to ten (obj table, obj code, idx list, size
// table), which is why "want" grew alongside it rather than this
// test's own count going stale.
func TestHelpOfflineListsAllCommands(t *testing.T) {
	rows := helpCommandRows(t)
	want := []string{"help", "info", "qs status", "qs top", "qs query", "plan", "obj table", "obj code", "idx list", "size table"}
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
	rows := helpCommandRows(t)
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
	// design spec line 61: "Help and output label it parent_module,
	// never 'queries touching this table'" - named in "qs top"'s Summary
	// rather than as a new Flag.Description field (see qsTopCommand's own
	// comment on why), so this is the one place that label can be
	// checked at all.
	top, ok := rows["qs top"]
	if !ok {
		t.Fatal("help output is missing command \"qs top\"")
	}
	if !strings.Contains(top.summary, "parent_module") {
		t.Fatalf("qs top's summary does not name parent_module: %q", top.summary)
	}
	if !strings.Contains(top.summary, "--object") {
		t.Fatalf("qs top's summary does not name --object: %q", top.summary)
	}
}

// TestHelpOfflineRendersFlagDefaultsBoundsAndEnum proves help's flags
// table carries each flag's default, bound and enum in its own column
// - design spec line 45 ("parameters, defaults, and examples"): an
// agent reading help must be able to tell which values are valid
// without trying one. Flag.Validate already enforces these; this test
// is about whether runHelp actually renders them.
func TestHelpOfflineRendersFlagDefaultsBoundsAndEnum(t *testing.T) {
	rows := helpFlagRows(t)

	timeout, ok := rows["info/timeout"]
	if !ok {
		t.Fatal("flags table is missing info/timeout")
	}
	if timeout.kind != "int64" || timeout.def != float64(30) || timeout.min != float64(1) || timeout.max != float64(300) {
		t.Fatalf("info/timeout = %+v, want kind=int64 default=30 min=1 max=300", timeout)
	}

	format, ok := rows["info/format"]
	if !ok {
		t.Fatal("flags table is missing info/format")
	}
	if format.def != "tsv" || format.enum != "tsv|json" {
		t.Fatalf("info/format = %+v, want default=tsv enum=tsv|json", format)
	}

	noTruncate, ok := rows["info/no-truncate"]
	if !ok {
		t.Fatal("flags table is missing info/no-truncate")
	}
	if noTruncate.def != false {
		t.Fatalf("info/no-truncate = %+v, want default=false", noTruncate)
	}

	ctx, ok := rows["info/ctx"]
	if !ok {
		t.Fatal("flags table is missing info/ctx")
	}
	if ctx.def != nil || ctx.min != nil || ctx.max != nil {
		t.Fatalf("info/ctx = %+v, want default/min/max all nil (no bound declared)", ctx)
	}
}

// TestHelpOfflineNoCellTruncated is the test the coordinator asked for
// after measuring the single-cell form's actual output: with help's
// flags flattened into one "; "-joined cell per command, info's and
// "qs status"'s flags cell both measured 205 Unicode code points,
// already past the project's 200-code-point default preview cell
// limit, and came back truncated with a "…[+8]" marker under the
// default options help --json normally runs with. A help that
// truncates its own inventory by default defeats the one thing it
// exists for: an agent reading it to learn which values are valid
// without trying one. This test reads help --json under its default
// options - no --no-truncate - and fails if any cell, in any table,
// carries output's truncation marker. It is written to fail again the
// day a future command (qs top, at task 10, alone adds six more flags
// on top of the nine global ones) pushes some cell back over the
// limit, whatever shape future help rendering takes.
func TestHelpOfflineNoCellTruncated(t *testing.T) {
	var out, errout bytes.Buffer
	code := Run(context.Background(), []string{"help", "--json"}, &out, &errout)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errout.String())
	}
	var env struct {
		Tables []struct {
			Spec struct {
				Name string `json:"name"`
			} `json:"spec"`
			Rows [][]any `json:"rows"`
		} `json:"tables"`
	}
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("invalid JSON: %v (%s)", err, out.String())
	}
	for _, tbl := range env.Tables {
		for _, row := range tbl.Rows {
			for i, cell := range row {
				s, ok := cell.(string)
				if !ok {
					continue
				}
				if strings.Contains(s, "…[+") {
					t.Fatalf("table %q row %v column %d is truncated: %q", tbl.Spec.Name, row, i, s)
				}
			}
		}
	}
}

// TestHelpOfflineNoRowOmitted is TestHelpOfflineNoCellTruncated's
// counterpart for rows instead of cells: fixing the cell-truncation
// defect by splitting help's flags into their own table, one row per
// flag, created a second way for the same underlying cause (help's
// inventory growing past a default bound) to cut it again - this time
// the general --preview default of 10 rows, which silently dropped "qs
// status"'s flags from the "flags" table (19 rows collected, only 10
// shown, omitted_reasons row_limit) before runOffline was taught that
// an offline command's own registry metadata is not the kind of result
// --preview exists to cap. This test fails if any table in help's
// default output shows fewer rows than it collected.
func TestHelpOfflineNoRowOmitted(t *testing.T) {
	var out, errout bytes.Buffer
	code := Run(context.Background(), []string{"help", "--json"}, &out, &errout)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errout.String())
	}
	var env struct {
		Tables []struct {
			Spec struct {
				Name string `json:"name"`
			} `json:"spec"`
			State struct {
				RowsCollected int64 `json:"rows_collected"`
			} `json:"state"`
			Preview struct {
				RowsShown      int64    `json:"rows_shown"`
				OmittedReasons []string `json:"omitted_reasons"`
			} `json:"preview"`
		} `json:"tables"`
	}
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("invalid JSON: %v (%s)", err, out.String())
	}
	if len(env.Tables) == 0 {
		t.Fatal("help output has no tables")
	}
	for _, tbl := range env.Tables {
		if tbl.Preview.RowsShown != tbl.State.RowsCollected {
			t.Fatalf("table %q shows %d of %d collected rows, omitted_reasons=%v",
				tbl.Spec.Name, tbl.Preview.RowsShown, tbl.State.RowsCollected, tbl.Preview.OmittedReasons)
		}
	}
}

// TestHelpOfflineExplicitPreviewStillApplies proves the fix above does
// not make --preview inert for help: an explicit --preview still caps
// rows shown, only the silent default changes.
func TestHelpOfflineExplicitPreviewStillApplies(t *testing.T) {
	var out, errout bytes.Buffer
	code := Run(context.Background(), []string{"help", "--json", "--preview", "2"}, &out, &errout)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errout.String())
	}
	var env struct {
		Tables []struct {
			Preview struct {
				RowsShown int64 `json:"rows_shown"`
			} `json:"preview"`
		} `json:"tables"`
	}
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("invalid JSON: %v (%s)", err, out.String())
	}
	for _, tbl := range env.Tables {
		if tbl.Preview.RowsShown > 2 {
			t.Fatalf("table shows %d rows, want at most 2 with --preview 2", tbl.Preview.RowsShown)
		}
	}
}

// fakeLoadConfig and fakeOpenSession let run_test.go drive run (the
// unexported body behind Run) against a fabricated profile and a
// fabricated *sqlserver.Session, without a config file or a network
// connection. info and qs status now dispatch straight into
// internal/diagnostics, which queries Session.Conn for real (task 9a's
// placeholder Execute never did), so fakeOpenSession hands back a
// Session built on newFakeSession (testdriver_test.go): a real *sql.Conn
// over a fake database/sql/driver that answers internal/diagnostics'
// three embedded queries from canned rows, the same technique
// internal/sqlserver's own package uses for Probe/Resolve (see its
// testdriver_test.go) - never a live server.
func fakeLoadConfig(profile config.Profile) configLoader {
	return func(path, name, databaseOverride string, getenv func(string) string) (config.Profile, error) {
		return profile, nil
	}
}

func fakeOpenSession(t *testing.T, major int) sessionOpener {
	return func(ctx context.Context, p config.Profile) (*sqlserver.Session, error) {
		return newFakeSession(t, major), nil
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
			run(context.Background(), args, &out, &errout, fakeLoadConfig(profile), fakeOpenSession(t, c.major))
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

// TestRunDispatchesInfoAndStatus proves 9b's replacement for 9a's
// placeholder Execute: info and qs status now reach
// internal/diagnostics for real and their result flows all the way
// through Run to stdout - code 0, ok:true, and the tables each command
// declares in the registry actually present with a row - rather than
// the stable not_implemented error task 9a's scaffolding returned.
func TestRunDispatchesInfoAndStatus(t *testing.T) {
	profile := config.Profile{Host: "fake", Database: "db", Username: "user", Password: "secret", Port: 1433, TrustServerCertificate: true}

	cases := []struct {
		command string
		tables  []string
	}{
		{"info", []string{"identity"}},
		{"qs status", []string{"status", "coverage"}},
	}
	for _, c := range cases {
		t.Run(c.command, func(t *testing.T) {
			var out, errout bytes.Buffer
			args := append([]string{"--ctx", "x", "--format", "json", "--out-dir", t.TempDir()}, splitName(c.command)...)
			code := run(context.Background(), args, &out, &errout, fakeLoadConfig(profile), fakeOpenSession(t, 16))
			if code != 0 {
				t.Fatalf("got code %d, want 0 (%s / %s)", code, out.String(), errout.String())
			}
			if !json.Valid(out.Bytes()) {
				t.Fatalf("stdout is not valid JSON: %s", out.String())
			}
			var env struct {
				OK     bool `json:"ok"`
				Tables []struct {
					Spec struct {
						Name string `json:"name"`
					} `json:"spec"`
					Rows [][]any `json:"rows"`
				} `json:"tables"`
			}
			if err := json.Unmarshal(out.Bytes(), &env); err != nil {
				t.Fatalf("invalid JSON: %v (%s)", err, out.String())
			}
			if !env.OK {
				t.Fatalf("ok=false: %s", out.String())
			}
			if len(env.Tables) != len(c.tables) {
				t.Fatalf("got %d tables, want %d (%s)", len(env.Tables), len(c.tables), out.String())
			}
			for i, wantName := range c.tables {
				if env.Tables[i].Spec.Name != wantName {
					t.Fatalf("table %d: got name %q, want %q", i, env.Tables[i].Spec.Name, wantName)
				}
				if len(env.Tables[i].Rows) != 1 {
					t.Fatalf("table %q: got %d rows, want 1", wantName, len(env.Tables[i].Rows))
				}
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

// TestRunPlanSummaryTruncationReachesExitCode is fix 2's B10: the
// coordinator verified, by breaking it directly, that
// internal/plan.Summarize RETURNS a code-7 error when its references
// or warnings list is truncated (fix 1's A3) - but never that this
// error actually reaches the compiled binary's own exit code, rather
// than being absorbed or reclassified somewhere between
// diagnostics.Plan and Run. This is the same "guard tested, propagation
// not tested" gap the byte-quota guard had one layer down (fix 2's B1):
// it is not enough for a function to return the right error if nothing
// checks that the error survives to the process's own exit status.
//
// The fixture below carries 101 distinct Object references - one more
// than referenceCap - inside a single RelOp, so Summarize's own
// truncation path fires deterministically without needing the byte
// quota at all.
func TestRunPlanSummaryTruncationReachesExitCode(t *testing.T) {
	var refs strings.Builder
	for i := 0; i < 101; i++ {
		fmt.Fprintf(&refs, `<Object Database="[D]" Schema="[s]" Table="[T%d]"/>`, i)
	}
	planXML := `<ShowPlanXML><StmtSimple StatementSubTreeCost="1"><QueryPlan>` +
		`<RelOp NodeId="0" EstimatedTotalSubtreeCost="1">` + refs.String() + `</RelOp>` +
		`</QueryPlan></StmtSimple></ShowPlanXML>`

	profile := config.Profile{Host: "fake", Database: "db", Username: "user", Password: "secret", Port: 1433, TrustServerCertificate: true}
	sess := newFakeSessionWithPlanXML(t, 16, planXML)

	var out, errout bytes.Buffer
	args := []string{"--ctx", "x", "--format", "json", "--out-dir", t.TempDir(), "plan", "4821", "--plan-id", "9033", "--summary"}
	code := run(context.Background(), args, &out, &errout,
		fakeLoadConfig(profile),
		func(ctx context.Context, p config.Profile) (*sqlserver.Session, error) { return sess, nil },
	)
	if code != 7 {
		t.Fatalf("got code %d, want 7 (truncated references must reach the process exit code): stdout=%s stderr=%s", code, out.String(), errout.String())
	}
}
