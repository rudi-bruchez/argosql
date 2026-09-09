//go:build integration

package integration

import (
	"bytes"
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rudi-bruchez/argosql/internal/config"
	"github.com/rudi-bruchez/argosql/internal/model"
)

// bootstrapScript is sql/bootstrap.sql, applied once per container by
// NewLab before a Lab is handed to any test.
//
//go:embed sql/bootstrap.sql
var bootstrapScript string

// workloadScript is sql/workload.sql: the load Lab.QueryID (below) runs,
// with its {{MARKER}} placeholder substituted, to generate one Query
// Store entry a test can discover by query_id rather than guessing or
// hardcoding one.
//
//go:embed sql/workload.sql
var workloadScript string

// splitBatches cuts a sqlcmd-style script into batches on lines that are,
// once trimmed, exactly "GO" (case-insensitively, as sqlcmd itself
// accepts). CREATE DATABASE and a QUERY_STORE state change each need their
// own batch (see sql/bootstrap.sql's header comment), and database/sql has
// no notion of a multi-statement batch separator of its own: each batch
// becomes one ExecContext call.
func splitBatches(script string) []string {
	var batches []string
	var cur strings.Builder
	flush := func() {
		if b := strings.TrimSpace(cur.String()); b != "" {
			batches = append(batches, b)
		}
		cur.Reset()
	}
	for _, line := range strings.Split(script, "\n") {
		if strings.EqualFold(strings.TrimSpace(line), "GO") {
			flush()
			continue
		}
		cur.WriteString(line)
		cur.WriteByte('\n')
	}
	flush()
	return batches
}

// applyBootstrap runs every batch of sql/bootstrap.sql, in order, against
// db. Every object the script creates is named with an explicit AppDB.
// prefix so the whole script can run from db's own current database
// (master): database/sql may hand successive ExecContext calls to
// different pooled physical connections, and a USE issued by one batch
// would not be guaranteed to still be in effect for the next.
func applyBootstrap(ctx context.Context, db *sql.DB) error {
	for i, batch := range splitBatches(bootstrapScript) {
		if _, err := db.ExecContext(ctx, batch); err != nil {
			return fmt.Errorf("bootstrap batch %d: %w", i+1, err)
		}
	}
	return nil
}

// queryStoreMarker names a result column alias in the load
// TestFixtureQueryStoreFlush generates, so the poll loop below can find
// that exact query's text in Query Store rather than matching anything
// else AppDB's schema or another run might have left behind.
//
// It is a column alias, not a SQL comment: measured against a real
// container, SQL Server's own simple parameterization rewrites this
// query's text before Query Store ever sees it - the literal 0 in
// "Quantity >= 0" becomes "@1", brackets get added around identifiers,
// and any comment is discarded outright. An alias survives that rewrite;
// a comment does not.
const queryStoreMarker = "AsqFixtureQueryStoreMarker"

// reissueWorkload re-runs a fixture's own workload and forces another
// Query Store flush, and the poll loops below call it every
// flushReissueEvery attempts rather than only sleeping.
//
// Why re-running beats waiting longer, measured on this project: the
// three tests that failed a full 2022 suite (two runs out of three, a
// different test each time, every one of them green in isolation) all
// failed the same way, a marker never appearing within the poll bound.
// Raising that bound from 30 to 120 seconds reduced the frequency and
// did not remove it, which is the signature of something that is not
// merely slow. The corroborating measurement is in this file's own
// history: a SINGLE execution of a batch was not reliably visible to
// sp_query_store_flush_db, and five executions of the same batch were.
// So the failure is a capture that did not happen, not a write that had
// not landed yet, and no amount of additional waiting produces a row
// the engine never captured. Re-issuing the work does.
// queryIDForMarkerBatch is the ONE poll every fixture in this package
// uses to turn a marked batch into its Query Store query_id. Three
// copies of this loop existed before, and the copy a first version of
// this fix did not touch is the one that failed the very run meant to
// prove the fix: patching call sites instead of the shared path proved
// nothing and cost a full suite run.
//
// It runs batch five times, not once, then flushes, then polls -
// re-issuing the whole thing every flushReissueEvery attempts. The five
// is not superstition: a single execution of a batch was measured not
// reliably visible to sp_query_store_flush_db on 2019, and five
// executions of the same batch were.
func queryIDForMarkerBatch(ctx context.Context, t *testing.T, lab *Lab, marker, batch string) int64 {
	t.Helper()

	for i := 0; i < 5; i++ {
		if _, err := lab.Admin.ExecContext(ctx, batch); err != nil {
			t.Fatalf("running the workload for marker %q: %v", marker, err)
		}
	}
	if _, err := lab.Admin.ExecContext(ctx, "EXEC sys.sp_query_store_flush_db"); err != nil {
		t.Fatalf("sp_query_store_flush_db: %v", err)
	}

	const findID = "SELECT q.query_id FROM sys.query_store_query_text AS qt " +
		"JOIN sys.query_store_query AS q ON q.query_text_id = qt.query_text_id " +
		"WHERE qt.query_sql_text LIKE '%' + @p1 + '%'"

	deadline := time.Now().Add(flushPollDeadline)
	for attempt := 0; ; attempt++ {
		var id int64
		err := lab.Admin.QueryRowContext(ctx, findID, marker).Scan(&id)
		if err == nil {
			return id
		}
		if !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("polling for query_id of marker %q: %v", marker, err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("query store marker %q did not appear within %s, across %d polls and %d workload re-issues",
				marker, flushPollDeadline, attempt, attempt/flushReissueEvery)
		}
		if attempt > 0 && attempt%flushReissueEvery == 0 {
			reissueWorkload(ctx, lab.Admin, batch)
		}
		time.Sleep(flushPollDelay)
	}
}

func reissueWorkload(ctx context.Context, db *sql.DB, batch string) {
	for i := 0; i < 5; i++ {
		row := db.QueryRowContext(ctx, batch)
		var discard any
		_ = row.Scan(&discard)
	}
	_, _ = db.ExecContext(ctx, "EXEC sys.sp_query_store_flush_db")
}

// flushPollDeadline bounds the wait for the flushed marker to become
// visible in Query Store; flushPollDelay is the short interval between
// polls, never one fixed sleep for the whole wait. flushPollBudget is
// the context budget of the whole round trip, workload and flush
// included, and is the context of the two functions that poll.
//
// Why these numbers, and why the ordering between them matters (fix 1's
// B9, raised here rather than in the pass that deferred it): nothing
// guarantees that sp_query_store_flush_db makes a query visible through
// the catalog views synchronously, so this wait is a poll and its only
// correct bound is "longer than the engine has ever taken". 30 seconds
// was not: it failed twice in six runs for a reviewer under CPU load,
// and once more here during the verification of that pass, with two
// containers competing for the machine.
//
// flushPollDeadline must stay strictly BELOW flushPollBudget. Raising
// the deadline past the context budget would not extend the wait at
// all: the polling query itself dies of its context first, and the
// failure then arrives as an opaque driver error from the poll instead
// of the message below, which names the marker and the bound it waited.
const (
	flushPollBudget   = 150 * time.Second
	flushPollDeadline = 120 * time.Second
	flushPollDelay    = 300 * time.Millisecond

	// Every this many polls (about ten seconds at flushPollDelay), the
	// loop re-issues its workload instead of only sleeping. See
	// reissueWorkload for the measurement that made this necessary.
	flushReissueEvery = 33
)

// TestFixtureQueryStoreFlush proves the fixture capability every later
// diagnostics task relies on: a query run against AppDB is captured by
// Query Store, sys.sp_query_store_flush_db forces it to disk, and the
// query's text becomes visible through the Query Store DMVs. The twelve
// tasks after this one build Query Store diagnostics on top of this
// database; if this round trip is not provably reliable now, every test
// built on it later would be guessing.
func TestFixtureQueryStoreFlush(t *testing.T) {
	lab := NewLab(t, os.Getenv("ASQ_TEST_IMAGE"))
	ctx, cancel := context.WithTimeout(context.Background(), flushPollBudget)
	defer cancel()

	var major int
	if err := lab.Admin.QueryRowContext(ctx, "SELECT CAST(SERVERPROPERTY('ProductMajorVersion') AS int)").Scan(&major); err != nil {
		t.Fatalf("reading engine major version: %v", err)
	}
	logEngineIdentity(t, lab, major)

	loadQuery := fmt.Sprintf("SELECT COUNT(*) AS %s FROM dbo.Widgets WHERE Quantity >= 0", queryStoreMarker)
	for i := 0; i < 5; i++ {
		var count int
		if err := lab.Admin.QueryRowContext(ctx, loadQuery).Scan(&count); err != nil {
			t.Fatalf("generating query store load: %v", err)
		}
	}

	if _, err := lab.Admin.ExecContext(ctx, "EXEC sys.sp_query_store_flush_db"); err != nil {
		t.Fatalf("sp_query_store_flush_db: %v", err)
	}

	findMarker := "SELECT COUNT(*) FROM sys.query_store_query_text WHERE query_sql_text LIKE '%' + @p1 + '%'"
	deadline := time.Now().Add(flushPollDeadline)
	for attempt := 0; ; attempt++ {
		var n int
		if err := lab.Admin.QueryRowContext(ctx, findMarker, queryStoreMarker).Scan(&n); err != nil {
			t.Fatalf("polling sys.query_store_query_text: %v", err)
		}
		if n > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("query store marker did not appear within %s, across %d polls and %d workload re-issues",
				flushPollDeadline, attempt, attempt/flushReissueEvery)
		}
		if attempt > 0 && attempt%flushReissueEvery == 0 {
			reissueWorkload(ctx, lab.Admin, loadQuery)
		}
		time.Sleep(flushPollDelay)
	}
}

// QueryID runs sql/workload.sql's load against lab.Admin (AppDB), with
// {{MARKER}} replaced by marker, flushes Query Store, and polls -
// bounded by flushPollDeadline, the same 30s budget
// TestFixtureQueryStoreFlush proves reliable above - until a query
// carrying marker in its text is visible in Query Store, returning its
// query_id.
//
// This is task 10's own fixture-discovery mechanism, exposed as a Lab
// method so every later diagnostics task that needs a real, uniquely
// identifiable Query Store entry reaches for this helper instead of
// discovering (or worse, hardcoding) a query_id of its own - task 11
// reuses it verbatim rather than redefining it.
func (lab *Lab) QueryID(t *testing.T, marker string) int64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), flushPollBudget)
	defer cancel()

	load := strings.ReplaceAll(workloadScript, "{{MARKER}}", marker)
	return queryIDForMarkerBatch(ctx, t, lab, marker, load)
}

// testBinaryOnce/testBinaryPath/testBinaryErr build this module's asq
// binary exactly once for the whole test binary run, the first time
// any Lab.Run call actually needs it.
//
// The brief attributes this construction to "a binary built once in
// TestMain (task 4)"; podman_test.go's TestMain has never done that -
// it installs only the SIGINT/SIGTERM container-cleanup handler, a
// narrow, single responsibility its own doc comment states explicitly
// ("it does not decide how the process should terminate"). Building
// the binary there would mean every test in this package pays a ~60s
// go-build cost before main() even runs, including
// TestContainerCarriesRunLabel, TestStatus, TestPermissions and every
// other test that never execs this binary at all - and a build
// failure inside TestMain has no *testing.T to report through at all
// (TestMain runs before any test's T exists), only os.Exit with a bare
// stderr message. A package-level sync.Once, triggered lazily from the
// one method that actually needs the binary (Lab.Run), keeps the cost
// and the failure mode scoped to exactly the tests that pay it, each
// reporting a build failure through its own t.Fatalf.
var (
	testBinaryOnce sync.Once
	testBinaryPath string
	testBinaryErr  error
)

// buildTestBinary returns the path to this module's asq binary,
// building it into a temporary directory on the first call. The
// directory is intentionally never removed: it must outlive every
// individual *testing.T in this run (t.TempDir() would not, since
// different tests have different, independently-cleaned TempDirs),
// and a handful of megabytes in the OS temp directory is the same
// cost `go test -c` itself already leaves behind for the unrelated
// signal drill in podman_test.go.
func buildTestBinary(t *testing.T) string {
	t.Helper()
	testBinaryOnce.Do(func() {
		moduleRoot, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}").Output()
		if err != nil {
			testBinaryErr = fmt.Errorf("locating module root: %s", describeExecError(err))
			return
		}
		dir, err := os.MkdirTemp("", "asq-test-bin-")
		if err != nil {
			testBinaryErr = fmt.Errorf("creating temp dir for the asq test binary: %w", err)
			return
		}
		path := filepath.Join(dir, "asq")
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "go", "build", "-o", path, "./cmd/asq")
		cmd.Dir = strings.TrimSpace(string(moduleRoot))
		if out, buildErr := cmd.CombinedOutput(); buildErr != nil {
			testBinaryErr = fmt.Errorf("building asq test binary: %w\n%s", buildErr, out)
			return
		}
		testBinaryPath = path
	})
	if testBinaryErr != nil {
		t.Fatalf("buildTestBinary: %v", testBinaryErr)
	}
	return testBinaryPath
}

// principalUsername maps a Lab.Run principal letter ("Q", "I" or "S")
// to the SQL login principals.sql creates for it.
func principalUsername(principal string) string {
	switch principal {
	case "Q":
		return "asq_test_q"
	case "I":
		return "asq_test_i"
	case "S":
		return "asq_test_s"
	case "metadata_only":
		return "asq_test_metadata_only"
	default:
		return ""
	}
}

// secretFor returns pw's own password for principal ("Q", "I" or "S"),
// or ok=false for anything else - the one place that decides which of
// the three stored secrets a given letter means, so Lab.Run cannot
// reach for the wrong field by a copy/paste mistake.
func (pw principalPasswords) secretFor(principal string) (string, bool) {
	switch principal {
	case "Q":
		return pw.Q, true
	case "I":
		return pw.I, true
	case "S":
		return pw.S, true
	case "metadata_only":
		return pw.MetadataOnly, true
	default:
		return "", false
	}
}

// ensurePrincipals creates this Lab's own Q/I/S SQL logins, once, the
// first time it is called (by Lab.Run, or directly by a test that
// wants a principal's credentials without going through the
// subprocess binary) - reusing permissions_test.go's own
// applyObjects/applyPrincipals/openMasterDB, package-level functions
// TestPermissions already calls with its own, separately-generated
// principalPasswords against its own, separate Lab/container. There
// is no collision between the two: every test's NewLab starts its own
// disposable container, so this Lab's principals.sql application
// (and its own random passwords) never interacts with any other
// test's.
//
// objects.sql must run first: principals.sql's AppDB half DENYs SELECT
// on the Restricted schema objects.sql creates, exactly like
// TestPermissions's own setup.
func (lab *Lab) ensurePrincipals(t *testing.T) principalPasswords {
	t.Helper()
	lab.principalsOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		if err := applyObjects(ctx, lab.Admin); err != nil {
			lab.principalsErr = fmt.Errorf("applying objects.sql: %w", err)
			return
		}
		pw := principalPasswords{Q: randomPassword(), I: randomPassword(), S: randomPassword(), MetadataOnly: randomPassword()}
		masterDB := openMasterDB(t, lab)
		if err := applyPrincipals(ctx, t, masterDB, lab.Admin, pw); err != nil {
			lab.principalsErr = fmt.Errorf("applying principals.sql: %w", err)
			return
		}
		lab.secrets = pw
	})
	if lab.principalsErr != nil {
		t.Fatalf("ensurePrincipals: %v", lab.principalsErr)
	}
	return lab.secrets
}

// labRunContextName is the one profile name every temporary YAML
// config.yaml buildChildConfigAndEnv writes declares.
const labRunContextName = "lab"

// labRunSecretEnv is the one environment variable name Lab.Run's
// temporary profile names via password_env, and the one name the
// child's environment ever carries a secret under.
const labRunSecretEnv = "ASQ_LAB_SECRET"

// labRunEnv builds the environment slice Lab.Run's child process
// receives: EXACTLY one KEY=VALUE entry, secretEnv=secret, and nothing
// else - fix 1's A5. It used to be the parent's own os.Environ() plus
// that one entry; measured with three distinct fake secrets and two of
// them placed in two otherwise-ordinary inherited variables, the child
// launched for a THIRD principal received all three, because
// inheriting the parent's environment is exactly as safe as trusting
// every variable already sitting in it, which this harness cannot
// promise for code the tâche 15 matrices will run under. The asq
// binary itself needs nothing else from the environment to run: every
// path this test package ever gives it (--config, --out-dir) is
// already absolute and explicit, so there is no PATH lookup, no
// $HOME-relative default, and no other inherited variable for it to
// depend on.
func labRunEnv(secretEnv, secret string) []string {
	return []string{secretEnv + "=" + secret}
}

// buildChildConfigAndEnv is Lab.Run's own profile/environment
// construction, factored out so it can be exercised without a real
// container or the real asq binary: it takes pw directly rather than
// calling ensurePrincipals itself, which is what lets
// TestLabRunOnlyInjectsRequestedSecret observe the REAL child process
// and its REAL YAML file (fix 1's A5: "le test doit observer l'ENFANT,
// pas la fonction qui prépare son environnement") with a synthetic,
// container-free principalPasswords.
func buildChildConfigAndEnv(t *testing.T, profile config.Profile, pw principalPasswords, principal string) (configPath string, env []string) {
	t.Helper()
	secret, ok := pw.secretFor(principal)
	if !ok {
		t.Fatalf("buildChildConfigAndEnv: unknown principal %q (want Q, I or S)", principal)
	}
	username := principalUsername(principal)

	configPath = filepath.Join(t.TempDir(), "config.yaml")
	configContent := fmt.Sprintf(
		"profiles:\n  %s:\n    host: %q\n    port: %d\n    username: %q\n    password_env: %s\n    database: %q\n    trust_server_certificate: true\n",
		labRunContextName, profile.Host, profile.Port, username, labRunSecretEnv, profile.Database,
	)
	if err := os.WriteFile(configPath, []byte(configContent), 0o600); err != nil {
		t.Fatalf("buildChildConfigAndEnv: writing temporary profile: %v", err)
	}
	return configPath, labRunEnv(labRunSecretEnv, secret)
}

// Run writes a temporary profile naming principal's own context,
// injects ONLY that principal's own secret into the child process's
// environment - never the other two ensurePrincipals holds, nor
// anything inherited from this process's own environment (fix 1's A5) -
// and runs this module's own asq binary (buildTestBinary, built once
// for the whole run) as a real subprocess against it. No test in this
// package calls internal/cli.Run directly: every one of them either
// calls internal/diagnostics functions directly against a real
// *sqlserver.Session (the existing convention throughout this
// package: status_test.go, top_test.go and permissions_test.go all do
// this), or, for the one thing only a real subprocess can prove - that
// the compiled binary's own argv parsing, exit code and stdout JSON
// envelope actually agree end to end - goes through this method.
//
// args is the command and its own flags/positional arguments (for
// example []string{"qs", "query", "4821"}); Run always appends
// --format json, --ctx, --config and --out-dir itself - --out-dir a
// fresh temporary directory per call, so every model.Artifact path in
// the decoded Result is directly readable by the caller (both this
// process and the child are on the same machine).
//
// The temporary profile names password_env, never a literal password:
// internal/config.Load resolves the secret from the child's own
// environment at load time, precisely the project's normal contract
// (internal/config/load.go). If internal/config could not do this,
// that would be a defect in the project, not something to work around
// here.
func (lab *Lab) Run(t *testing.T, principal string, args []string) (model.Result, int) {
	t.Helper()
	bin := buildTestBinary(t)

	pw := lab.ensurePrincipals(t)
	configPath, env := buildChildConfigAndEnv(t, lab.Profile, pw, principal)
	outDir := filepath.Join(t.TempDir(), "out")

	fullArgs := append([]string{"--format", "json", "--ctx", labRunContextName, "--config", configPath, "--out-dir", outDir}, args...)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, fullArgs...)
	cmd.Env = env

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()

	code := 0
	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			code = exitErr.ExitCode()
		} else {
			t.Fatalf("Lab.Run: running %s: %v\nstderr:\n%s", bin, runErr, stderr.String())
		}
	}

	var result model.Result
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("Lab.Run: decoding stdout as model.Result: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
	}
	return result, code
}

// envProbeSource is a standalone Go program, built on demand by
// TestLabRunOnlyInjectsRequestedSecret, whose only job is to print its
// own os.Environ() - one line per entry - so that test can inspect
// what a REAL child process, launched exactly the way Lab.Run launches
// the real asq binary, actually received. Asserting on labRunEnv's
// return value alone (the previous form of this test) only proves what
// the function that BUILDS the environment intends; fix 1's reviewers
// measured that this left a green test when Run itself was mutated to
// inject all three secrets, or when the YAML carried one in a comment.
const envProbeSource = `package main

import (
	"bufio"
	"os"
)

func main() {
	w := bufio.NewWriter(os.Stdout)
	defer w.Flush()
	for _, kv := range os.Environ() {
		w.WriteString(kv)
		w.WriteByte('\n')
	}
}
`

// buildEnvProbe compiles envProbeSource into a temporary binary and
// returns its path. Built fresh (not memoized like buildTestBinary):
// this probe is only ever used by one test, and compiling a
// ten-line program costs a fraction of a second.
func buildEnvProbe(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "envprobe.go")
	if err := os.WriteFile(srcPath, []byte(envProbeSource), 0o600); err != nil {
		t.Fatalf("buildEnvProbe: writing probe source: %v", err)
	}
	binPath := filepath.Join(dir, "envprobe")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-o", binPath, srcPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("buildEnvProbe: building probe: %v\n%s", err, out)
	}
	return binPath
}

// envKey returns the variable NAME of a KEY=VALUE entry, and envKeys
// maps that over a whole environment. Every failure message in
// TestLabRunOnlyInjectsRequestedSecret goes through these, because the
// obvious form of those messages - printing the offending entry, or the
// whole environment the probe reported - prints secrets. Measured while
// verifying the A5 break by hand: the dump carried this machine's real
// CLAUDE_CODE_MESSAGING_TOKEN and SSH_AUTH_SOCK, and a test failure is
// exactly where that text gets pasted into a log or a bug report. A
// variable name localises the leak just as well as its value does.
func envKey(kv string) string {
	name, _, _ := strings.Cut(kv, "=")
	return name
}

func envKeys(env []string) []string {
	names := make([]string, len(env))
	for i, kv := range env {
		names[i] = envKey(kv)
	}
	return names
}

// TestLabRunOnlyInjectsRequestedSecret proves the one property
// Lab.Run exists to guarantee, on the REAL child process and its REAL
// YAML file, not on labRunEnv's return value alone (fix 1's A5): only
// the requested principal's own secret ever reaches the child's
// environment, and the YAML config.yaml carries never carries a secret
// literal at all, under any of the three principals. It needs no
// container, no podman, and no ASQ_TEST_IMAGE: buildChildConfigAndEnv
// takes a synthetic principalPasswords directly, and envprobe - a real
// compiled binary, launched exactly the way Lab.Run launches the real
// asq binary - reports what it actually received.
func TestLabRunOnlyInjectsRequestedSecret(t *testing.T) {
	probe := buildEnvProbe(t)
	pw := principalPasswords{Q: "qSecretValueXYZ", I: "iSecretValueXYZ", S: "sSecretValueXYZ"}
	all := []string{pw.Q, pw.I, pw.S}
	profile := config.Profile{Host: "127.0.0.1", Port: 14330, Database: "AppDB"}

	// Reproduces the reviewers' own repro for A5, inside THIS process's
	// environment: two ordinary, otherwise-unrelated variables happen to
	// carry the OTHER two principals' secrets, exactly as an operator's
	// shell or a CI job might leave lying around. A labRunEnv that still
	// inherited os.Environ() would leak both into the child launched for
	// a third principal; building the child's environment explicitly
	// must not, regardless of what this process's own environment holds.
	t.Setenv("ASQ_UNRELATED_I", pw.I)
	t.Setenv("ASQ_UNRELATED_S", pw.S)

	for _, principal := range []string{"Q", "I", "S"} {
		t.Run(principal, func(t *testing.T) {
			secret, ok := pw.secretFor(principal)
			if !ok {
				t.Fatalf("secretFor(%q): not ok", principal)
			}

			configPath, env := buildChildConfigAndEnv(t, profile, pw, principal)

			configBytes, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatalf("reading the written YAML profile: %v", err)
			}
			for _, s := range all {
				if strings.Contains(string(configBytes), s) {
					t.Fatalf("%s: YAML profile %s carries a secret literal: %q", principal, configPath, configBytes)
				}
			}
			if !strings.Contains(string(configBytes), "password_env: "+labRunSecretEnv) {
				t.Fatalf("%s: YAML profile does not name password_env: %s", principal, configBytes)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, probe)
			cmd.Env = env
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("running envprobe: %v", describeExecError(err))
			}
			childEnv := strings.Split(strings.TrimRight(string(out), "\n"), "\n")

			matches := 0
			for _, kv := range childEnv {
				if strings.Contains(kv, secret) {
					matches++
				}
			}
			if matches != 1 {
				t.Fatalf("%s: the real child's environment carries the requested secret in %d entries, want exactly 1; child variable names were %v", principal, matches, envKeys(childEnv))
			}

			for _, other := range all {
				if other == secret {
					continue
				}
				for _, kv := range childEnv {
					if strings.Contains(kv, other) {
						t.Fatalf("%s: the real child's environment leaks another principal's secret via variable %q", principal, envKey(kv))
					}
				}
			}
		})
	}
}
