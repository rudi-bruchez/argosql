//go:build integration

package integration

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mssql "github.com/microsoft/go-mssqldb"
	"github.com/rudi-bruchez/argosql/internal/config"
)

// TestPrincipalMatrix is task 15's own reproduction of the design
// spec's Q/I/S permission matrix (lines 155-170), all thirteen
// commands times all three principals, plus a direct confirmation that
// Admin DML/DDL/EXEC are refused under every limited principal without
// ever handing asq itself an arbitrary-SQL capability it does not
// have. help's own offline guarantee is TestHelpNeverConnects, its own
// top-level function (fix 1's A9: this function's first statement is
// an unconditional NewLab, which a container-free guarantee must never
// depend on, even when only that one subtest is selected).
//
// Every id this test feeds a command (query_id, plan_id) is discovered
// against the real fixture through lab.QueryID/planIDFor, never
// hardcoded; every object name (dbo.Orders, dbo.UsageFixture,
// dbo.PlainModule) is a stable fixture name objects.sql/bootstrap.sql
// already declare, the same convention every other file in this
// package already uses.
func TestPrincipalMatrix(t *testing.T) {
	lab := NewLab(t, os.Getenv("ASQ_TEST_IMAGE"))
	ctx, cancel := context.WithTimeout(context.Background(), flushPollBudget)
	defer cancel()

	var major int
	if err := lab.Admin.QueryRowContext(ctx, "SELECT CAST(SERVERPROPERTY('ProductMajorVersion') AS int)").Scan(&major); err != nil {
		t.Fatalf("reading engine major version: %v", err)
	}
	logEngineIdentity(t, lab, major)

	queryID := lab.QueryID(t, "AsqMatrixMarker")
	planID := planIDFor(ctx, t, lab, queryID)

	qid := fmt.Sprintf("%d", queryID)
	pid := fmt.Sprintf("%d", planID)

	// The spec's own matrix (design spec lines 155-170), thirteen
	// commands times Q/I/S. The three lines the dispatch calls out by
	// name as "not to be guessed" are marked inline below, at the exact
	// cell they apply to.
	cases := []struct {
		command string
		args    []string
		q, i, s int
	}{
		{"info", []string{"info"}, 0, 0, 0},
		{"qs status", []string{"qs", "status"}, 0, 0, 0},
		{"qs top", []string{"qs", "top"}, 0, 0, 0},
		// qs query: Q renders 0 even though its parent_module name may
		// be unavailable (Q holds no VIEW DEFINITION) - success here is
		// not conditioned on that name ever being populated, and this
		// test must not (and does not) assert anything about it.
		{"qs query", []string{"qs", "query", qid}, 0, 0, 0},
		{"plan", []string{"plan", qid, "--plan-id", pid}, 0, 0, 0},
		{"obj table", []string{"obj", "table", "dbo.Orders"}, 8, 0, 0},
		{"obj code", []string{"obj", "code", "dbo.PlainModule"}, 8, 0, 0},
		{"size table", []string{"size", "table", "dbo.Orders"}, 8, 0, 0},
		{"idx list", []string{"idx", "list", "dbo.Orders"}, 8, 0, 0},
		// idx usage: Q renders 8, NOT 4 - target resolution precedes the
		// permission check ("8 avant 4"), and Q cannot even resolve
		// dbo.UsageFixture (no VIEW DEFINITION). I resolves it fine but
		// lacks the instance-level VIEW SERVER STATE/VIEW SERVER
		// PERFORMANCE STATE permission idx usage's own DMV needs.
		{"idx usage", []string{"idx", "usage", "dbo.UsageFixture"}, 8, 4, 0},
		// idx missing, no --table filter: the one row where Q and I
		// fail IDENTICALLY (both lack the instance-level permission the
		// missing-index DMVs require database-wide), unlike every other
		// row in this matrix.
		{"idx missing", []string{"idx", "missing"}, 4, 4, 0},
		{"stats list", []string{"stats", "list", "dbo.Orders"}, 8, 0, 0},
	}

	for _, tc := range cases {
		t.Run(tc.command, func(t *testing.T) {
			for _, pc := range []struct {
				principal string
				want      int
			}{{"Q", tc.q}, {"I", tc.i}, {"S", tc.s}} {
				t.Run(pc.principal, func(t *testing.T) {
					result, code := lab.Run(t, pc.principal, tc.args)
					if code != pc.want {
						t.Fatalf("%s %s %v: got exit code %d, want %d (ok=%v error=%+v)",
							pc.principal, tc.command, tc.args, code, pc.want, result.OK, result.Error)
					}
				})
			}
		})
	}

	t.Run("admin_refusals", testAdminOperationsRefused(ctx, lab))
}

// TestHelpNeverConnects proves "help --json" is Command.Offline in the
// deepest sense the dispatch asks for: it is launched with NEITHER
// Lab.Run NOR any reachable SQL environment at all - built and run as
// its own subprocess, pointed at a config file naming an unreachable
// host (198.51.100.1, TEST-NET-2, never routable), and still returns
// code 0 with valid JSON well within the time a real connection attempt
// would need (sqlserver.Open's own connectTimeout is 5s). A command
// that actually tried to connect would either hang past this test's
// own deadline or take multiple seconds; returning almost instantly is
// the evidence that no connection was ever attempted, not merely that
// the exit code came back 0.
//
// Fix 1's A9: this used to be a subtest of TestPrincipalMatrix, whose
// own first statement unconditionally calls NewLab - a container, and
// ASQ_TEST_IMAGE, neither of which this specific guarantee should ever
// need. Filtering -run to only this subtest still ran NewLab first
// (Go runs every statement in a parent test function regardless of
// which of its own t.Run subtests a -run pattern selects; only the
// subtest bodies themselves are skipped), so the documented claim -
// "help is launched without Lab.Run and without an SQL environment" -
// was true of the subtest's own body but false of the only way to
// actually exercise it in isolation. A standalone top-level function
// needs neither NewLab nor ASQ_TEST_IMAGE at all.
func TestHelpNeverConnects(t *testing.T) {
	bin := buildTestBinary(t)

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	configContent := "profiles:\n  lab:\n    host: \"198.51.100.1\"\n    port: 14330\n    username: \"nobody\"\n    password_env: ASQ_MATRIX_UNUSED\n    database: \"AppDB\"\n    trust_server_certificate: true\n"
	if err := os.WriteFile(configPath, []byte(configContent), 0o600); err != nil {
		t.Fatalf("writing unreachable-host config: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "--format", "json", "--ctx", "lab", "--config", configPath, "help", "--json")
	cmd.Env = []string{"ASQ_MATRIX_UNUSED=unused"}

	start := time.Now()
	out, err := cmd.Output()
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("help --json must succeed offline even with an unreachable host configured: %v", err)
	}
	if cmd.ProcessState.ExitCode() != 0 {
		t.Fatalf("exit code: got %d, want 0", cmd.ProcessState.ExitCode())
	}
	if !strings.Contains(string(out), `"commands"`) {
		t.Fatalf("expected help's commands table in the JSON output, got: %s", out)
	}
	// connectTimeout (sqlserver/session.go) is 5s; a genuine attempt at
	// 198.51.100.1 could only return this fast by having never been
	// made at all.
	if elapsed > 2*time.Second {
		t.Fatalf("help --json took %s to return against an unreachable host - too slow to be offline, a real connection attempt was likely made", elapsed)
	}
}

// openAsPrincipal opens a direct, non-asq connection as principal
// ("Q", "I" or "S") against lab's own AppDB, using the exact same SQL
// login credentials Lab.Run's subprocess would inject - so a refusal
// measured here is a refusal of that same login, not a different one.
func openAsPrincipal(t *testing.T, lab *Lab, principal string) *sql.DB {
	t.Helper()
	pw := lab.ensurePrincipals(t)
	secret, ok := pw.secretFor(principal)
	if !ok {
		t.Fatalf("openAsPrincipal: unknown principal %q", principal)
	}
	profile := principalProfile(lab, principalUsername(principal), secret)
	dsn, err := config.DSN(profile)
	if err != nil {
		t.Fatalf("building DSN for principal %s: %v", principal, err)
	}
	db, err := sql.Open("sqlserver", dsn)
	if err != nil {
		t.Fatalf("opening pool for principal %s: %v", principal, err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// testAdminOperationsRefused confirms, directly against the engine and
// never through asq itself (asq issues no DML/DDL/EXEC anywhere in
// this program - this test exists to prove the fixture's own grants
// hold, not to give asq a capability it lacks), that DML, DDL and EXEC
// are refused for every one of Q, I and S. None of the three bundles
// (design spec, principals.sql) ever grants INSERT, ALTER or EXECUTE
// on dbo.PlainModule.
func testAdminOperationsRefused(ctx context.Context, lab *Lab) func(t *testing.T) {
	return func(t *testing.T) {
		for _, principal := range []string{"Q", "I", "S"} {
			t.Run(principal, func(t *testing.T) {
				db := openAsPrincipal(t, lab, principal)

				t.Run("DML", func(t *testing.T) {
					// The columns must be real ones. dbo.Orders is
					// created with OrderId, CustomerName and Total
					// (objects.sql): a statement naming columns that do
					// not exist fails at name resolution (SQL error 207)
					// whatever the principal's rights, so the old
					// "CustomerId, Quantity" form proved nothing. With a
					// valid statement the only reason left to fail is
					// permission, and a permission refusal is recognized
					// by its own SQL error number, never by the mere
					// non-nilness of an error.
					_, err := db.ExecContext(ctx, "INSERT INTO dbo.Orders (CustomerName, Total) VALUES (N'asq-matrix-probe', 0)")
					if err == nil {
						t.Fatalf("principal %s must be refused INSERT on dbo.Orders", principal)
					}
					var sqlErr mssql.Error
					if !errors.As(err, &sqlErr) {
						t.Fatalf("principal %s: INSERT error is not a *mssql.Error: %v", principal, err)
					}
					if sqlErr.Number != 229 {
						t.Fatalf("principal %s: INSERT got SQL error %d, want 229 (INSERT permission denied); any other number means the statement failed for a reason other than permission: %v", principal, sqlErr.Number, err)
					}
				})
				t.Run("DDL", func(t *testing.T) {
					_, err := db.ExecContext(ctx, "ALTER TABLE dbo.Orders ADD AsqMatrixProbeCol INT NULL")
					if err == nil {
						t.Fatalf("principal %s must be refused ALTER TABLE on dbo.Orders", principal)
					}
				})
				t.Run("EXEC", func(t *testing.T) {
					_, err := db.ExecContext(ctx, "EXEC dbo.PlainModule")
					if err == nil {
						t.Fatalf("principal %s must be refused EXEC on dbo.PlainModule", principal)
					}
				})
			})
		}
	}
}
