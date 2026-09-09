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

// flushPollDeadline bounds the wait for the flushed marker to become
// visible in Query Store; flushPollDelay is the short interval between
// polls, never one fixed sleep for the whole wait.
const (
	flushPollDeadline = 30 * time.Second
	flushPollDelay    = 300 * time.Millisecond
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
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
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
	for {
		var n int
		if err := lab.Admin.QueryRowContext(ctx, findMarker, queryStoreMarker).Scan(&n); err != nil {
			t.Fatalf("polling sys.query_store_query_text: %v", err)
		}
		if n > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("query store marker did not appear within %s of flush", flushPollDeadline)
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
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	load := strings.ReplaceAll(workloadScript, "{{MARKER}}", marker)
	for i := 0; i < 5; i++ {
		var count int
		if err := lab.Admin.QueryRowContext(ctx, load).Scan(&count); err != nil {
			t.Fatalf("running marked workload %q: %v", marker, err)
		}
	}

	if _, err := lab.Admin.ExecContext(ctx, "EXEC sys.sp_query_store_flush_db"); err != nil {
		t.Fatalf("sp_query_store_flush_db: %v", err)
	}

	findID := "SELECT q.query_id FROM sys.query_store_query_text AS qt " +
		"JOIN sys.query_store_query AS q ON q.query_text_id = qt.query_text_id " +
		"WHERE qt.query_sql_text LIKE '%' + @p1 + '%'"
	deadline := time.Now().Add(flushPollDeadline)
	for {
		var id int64
		err := lab.Admin.QueryRowContext(ctx, findID, marker).Scan(&id)
		if err == nil {
			return id
		}
		if !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("polling for query_id of marker %q: %v", marker, err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("query store marker %q did not appear within %s of flush", marker, flushPollDeadline)
		}
		time.Sleep(flushPollDelay)
	}
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
		pw := principalPasswords{Q: randomPassword(), I: randomPassword(), S: randomPassword()}
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

// labRunEnv builds the environment slice Lab.Run's child process
// receives: the parent's own os.Environ() plus exactly one KEY=VALUE
// entry carrying secret under secretEnv. Factored out of Run itself so
// TestLabRunOnlyInjectsRequestedSecret can assert directly on what Run
// actually constructs, without a container, a subprocess, or the asq
// binary: the other two secrets a caller might hold are never read
// here at all, by construction - not merely by convention - which is
// exactly the property that test proves. Only this principal's own
// secret ever becomes an environment variable in the first place
// (randomPassword generates the other two as plain Go strings, never
// exported anywhere), so inheriting the parent's own os.Environ()
// cannot leak them either.
func labRunEnv(secretEnv, secret string) []string {
	return append(os.Environ(), secretEnv+"="+secret)
}

// Run writes a temporary profile naming principal's own context,
// injects ONLY that principal's own secret into the child process's
// environment - never the other two ensurePrincipals holds, the one
// property this method exists to guarantee, proven by
// TestLabRunOnlyInjectsRequestedSecret below - and runs this module's
// own asq binary (buildTestBinary, built once for the whole run) as a
// real subprocess against it. No test in this package calls
// internal/cli.Run directly: every one of them either calls
// internal/diagnostics functions directly against a real
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
	secret, ok := pw.secretFor(principal)
	if !ok {
		t.Fatalf("Lab.Run: unknown principal %q (want Q, I or S)", principal)
	}
	username := principalUsername(principal)

	const ctxName = "lab"
	const secretEnv = "ASQ_LAB_SECRET"

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	outDir := filepath.Join(t.TempDir(), "out")

	configContent := fmt.Sprintf(
		"profiles:\n  %s:\n    host: %q\n    port: %d\n    username: %q\n    password_env: %s\n    database: %q\n    trust_server_certificate: true\n",
		ctxName, lab.Profile.Host, lab.Profile.Port, username, secretEnv, lab.Profile.Database,
	)
	if err := os.WriteFile(configPath, []byte(configContent), 0o600); err != nil {
		t.Fatalf("Lab.Run: writing temporary profile: %v", err)
	}

	fullArgs := append([]string{"--format", "json", "--ctx", ctxName, "--config", configPath, "--out-dir", outDir}, args...)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, fullArgs...)
	cmd.Env = labRunEnv(secretEnv, secret)

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

// TestLabRunOnlyInjectsRequestedSecret proves the one property
// Lab.Run exists to guarantee: only the requested principal's own
// secret ever reaches labRunEnv's result, never either of the other
// two. It needs no container, no podman, and no ASQ_TEST_IMAGE at
// all - labRunEnv reads only its own two string arguments, so a
// synthetic principalPasswords fabricated right here exercises the
// exact same code Lab.Run's real subprocess path calls, without
// paying for one.
func TestLabRunOnlyInjectsRequestedSecret(t *testing.T) {
	pw := principalPasswords{Q: "qSecretValueXYZ", I: "iSecretValueXYZ", S: "sSecretValueXYZ"}
	all := []string{pw.Q, pw.I, pw.S}

	for _, principal := range []string{"Q", "I", "S"} {
		t.Run(principal, func(t *testing.T) {
			secret, ok := pw.secretFor(principal)
			if !ok {
				t.Fatalf("secretFor(%q): not ok", principal)
			}

			env := labRunEnv("ASQ_LAB_SECRET", secret)

			matches := 0
			for _, kv := range env {
				if strings.Contains(kv, secret) {
					matches++
				}
			}
			if matches != 1 {
				t.Fatalf("%s: requested secret appears in %d env entries, want exactly 1: %v", principal, matches, env)
			}

			for _, other := range all {
				if other == secret {
					continue
				}
				for _, kv := range env {
					if strings.Contains(kv, other) {
						t.Fatalf("%s: env leaks another principal's secret %q via entry %q", principal, other, kv)
					}
				}
			}
		})
	}
}
