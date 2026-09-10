//go:build integration

package integration

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"

	"github.com/rudi-bruchez/argosql/internal/config"
	"github.com/rudi-bruchez/argosql/internal/model"
)

// TestWorkflow replays the diagnostic workflow end to end, through the
// real compiled binary (Lab.Run), across the three Query Store states
// the design spec distinguishes: READ_WRITE with history, OFF, and
// READ_ONLY with no history. It also runs the full command sequence
// (info, qs status, qs top, qs query, plan, obj table, idx usage, idx
// missing, stats list) against AppDB and confirms the artifacts and
// JSON Lab.Run hands back are the real files on disk, not just a
// plausible-looking envelope.
//
// Fixtures that need an ERROR actual_state or an unreadable/unknown
// property are deliberately NOT reproduced here by corrupting a real
// database (design spec line 252's own instruction, echoed by the
// brief: "ERROR et propriétés inconnues via backend de test, pas
// corruption de base") - internal/diagnostics/health_test.go already
// covers both through a fake driver
// (TestDecodeReadOnlyKnownAndUnknownMixed and the ERROR-state case
// nearby), and this suite does not re-derive them against a real
// engine.
func TestWorkflow(t *testing.T) {
	lab := NewLab(t, os.Getenv("ASQ_TEST_IMAGE"))
	ctx, cancel := context.WithTimeout(context.Background(), flushPollBudget)
	defer cancel()

	var major int
	if err := lab.Admin.QueryRowContext(ctx, "SELECT CAST(SERVERPROPERTY('ProductMajorVersion') AS int)").Scan(&major); err != nil {
		t.Fatalf("reading engine major version: %v", err)
	}
	logEngineIdentity(t, lab, major)

	t.Run("query_store_states", func(t *testing.T) { testWorkflowQueryStoreStates(ctx, t, lab) })
	t.Run("full_replay", func(t *testing.T) { testWorkflowFullReplay(ctx, t, lab) })
}

// cellOf returns the value of column col in table's first row, failing
// the test if either is missing - the per-table content assertion this
// task's own review grid requires: a row count or a completeness flag
// alone never proves a column survived intact.
func cellOf(t *testing.T, result model.Result, table, col string) model.Cell {
	t.Helper()
	for _, tbl := range result.Tables {
		if tbl.Spec.Name != table {
			continue
		}
		for i, c := range tbl.Spec.Columns {
			if c.Name == col {
				if len(tbl.Rows) == 0 {
					t.Fatalf("table %q has no rows to read column %q from", table, col)
				}
				return tbl.Rows[0][i]
			}
		}
		t.Fatalf("table %q has no column %q (columns: %+v)", table, col, tbl.Spec.Columns)
	}
	t.Fatalf("result has no table %q (tables: %+v)", table, result.Tables)
	return nil
}

// grantPrincipalInDatabase creates a USER for principal's own login in
// dbName and grants it VIEW DATABASE STATE there: principals.sql only
// ever maps Q/I/S into AppDB (the "== APPDB ==" half of that script),
// so a throwaway database this file creates has no user for any of
// them until this runs. Measured the hard way on this task's own first
// pass: Lab.Run against a throwaway database with no matching user
// fails at code 3 (connection failed), read the wrong way as a TLS or
// network problem rather than the real cause - a login with no user in
// the target database.
func grantPrincipalInDatabase(ctx context.Context, t *testing.T, lab *Lab, dbName, principal string) {
	t.Helper()
	// Ensures the server-level LOGIN actually exists before CREATE USER
	// ... FOR LOGIN references it: ensurePrincipals is sync.Once-guarded
	// per Lab and otherwise only triggered lazily by the first Lab.Run
	// call, which has not necessarily happened yet at this point.
	lab.ensurePrincipals(t)
	profile := lab.Profile
	profile.Database = dbName
	dsn, err := config.DSN(profile)
	if err != nil {
		t.Fatalf("building DSN for %s: %v", dbName, err)
	}
	db, err := sql.Open("sqlserver", dsn)
	if err != nil {
		t.Fatalf("opening pool for %s: %v", dbName, err)
	}
	defer db.Close()

	username := principalUsername(principal)
	if _, err := db.ExecContext(ctx, "CREATE USER ["+username+"] FOR LOGIN ["+username+"]"); err != nil {
		t.Fatalf("creating user %s in %s: %v", username, dbName, err)
	}
	if _, err := db.ExecContext(ctx, "GRANT VIEW DATABASE STATE TO ["+username+"]"); err != nil {
		t.Fatalf("granting VIEW DATABASE STATE to %s in %s: %v", username, dbName, err)
	}
}

// testWorkflowQueryStoreStates builds three throwaway databases, one
// per state, and runs "qs status" against each through the real
// binary as principal S.
//
// Measured fact this task's brief names explicitly: on 2022, CREATE
// DATABASE alone already leaves actual_state_desc=READ_WRITE (Query
// Store is on by default for a new user database there), unlike 2019.
// The OFF fixture therefore disables Query Store EXPLICITLY and
// unconditionally - never gated on major, since the explicit ALTER is
// harmless on 2019 too - so it tests a genuine OFF state on both
// versions rather than accidentally testing READ_WRITE on 2022.
func testWorkflowQueryStoreStates(ctx context.Context, t *testing.T, lab *Lab) {
	t.Run("read_write_with_history", func(t *testing.T) {
		dbName := createThrowawayDatabase(ctx, t, lab, "asq_wf_rw_")
		if _, err := lab.Admin.ExecContext(ctx, "ALTER DATABASE ["+dbName+"] SET QUERY_STORE = ON (OPERATION_MODE = READ_WRITE, INTERVAL_LENGTH_MINUTES = 1)"); err != nil {
			t.Fatalf("enabling Query Store: %v", err)
		}
		generateLoadAndWaitForHistory(ctx, t, lab, dbName)
		grantPrincipalInDatabase(ctx, t, lab, dbName, "S")

		result, code := lab.Run(t, "S", []string{"--db", dbName, "qs", "status"})
		if code != 0 {
			t.Fatalf("qs status on READ_WRITE with history: got exit code %d, want 0 (error=%+v)", code, result.Error)
		}
		if got := cellOf(t, result, "status", "actual_state"); got != "READ_WRITE" {
			t.Fatalf("status.actual_state: got %#v, want %q", got, "READ_WRITE")
		}
		if got := cellOf(t, result, "coverage", "has_history"); got != true {
			t.Fatalf("coverage.has_history: got %#v, want true", got)
		}
	})

	t.Run("off_explicit", func(t *testing.T) {
		dbName := createThrowawayDatabase(ctx, t, lab, "asq_wf_off_")
		// Explicit and unconditional: on 2022 this is required to reach
		// a genuine OFF state at all (see this function's own doc
		// comment); on 2019 it is a harmless no-op on top of the
		// engine's own default.
		if _, err := lab.Admin.ExecContext(ctx, "ALTER DATABASE ["+dbName+"] SET QUERY_STORE = OFF"); err != nil {
			t.Fatalf("disabling Query Store: %v", err)
		}
		grantPrincipalInDatabase(ctx, t, lab, dbName, "S")

		result, code := lab.Run(t, "S", []string{"--db", dbName, "qs", "status"})
		if code != 0 {
			t.Fatalf("qs status on an explicitly OFF database: got exit code %d, want 0 (error=%+v)", code, result.Error)
		}
		if got := cellOf(t, result, "status", "actual_state"); got != "OFF" {
			t.Fatalf("status.actual_state: got %#v, want %q", got, "OFF")
		}
	})

	t.Run("read_only_no_history", func(t *testing.T) {
		dbName := createThrowawayDatabase(ctx, t, lab, "asq_wf_ro_")
		if _, err := lab.Admin.ExecContext(ctx, "ALTER DATABASE ["+dbName+"] SET QUERY_STORE = ON (OPERATION_MODE = READ_WRITE, INTERVAL_LENGTH_MINUTES = 1)"); err != nil {
			t.Fatalf("enabling Query Store: %v", err)
		}
		// Before READ_ONLY: CREATE USER itself needs a writable database.
		grantPrincipalInDatabase(ctx, t, lab, dbName, "S")
		// No load generated at all: this database never accumulates any
		// Query Store runtime history before going READ_ONLY.
		if _, err := lab.Admin.ExecContext(ctx, "ALTER DATABASE ["+dbName+"] SET READ_ONLY WITH ROLLBACK IMMEDIATE"); err != nil {
			t.Fatalf("setting READ_ONLY: %v", err)
		}
		t.Cleanup(func() {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), podmanCmdTimeout)
			defer cleanupCancel()
			lab.Admin.ExecContext(cleanupCtx, "ALTER DATABASE ["+dbName+"] SET READ_WRITE WITH ROLLBACK IMMEDIATE")
		})

		result, code := lab.Run(t, "S", []string{"--db", dbName, "qs", "status"})
		if code != 0 {
			t.Fatalf("qs status on READ_ONLY with no history: got exit code %d, want 0 (error=%+v) - qs status reads configuration metadata alone and design spec's own OFF-case wording ('toutes les vues de catalogue restent lisibles') applies here too", code, result.Error)
		}
		if got := cellOf(t, result, "status", "actual_state"); got != "READ_ONLY" {
			t.Fatalf("status.actual_state: got %#v, want %q", got, "READ_ONLY")
		}
		if got := cellOf(t, result, "coverage", "has_history"); got != false {
			t.Fatalf("coverage.has_history: got %#v, want false (no load was ever generated)", got)
		}
	})
}

// testWorkflowFullReplay runs the declared workflow sequence against
// AppDB, through the real binary, as principal S, and confirms the
// artifacts Lab.Run reports are real, readable files - not merely a
// plausible JSON envelope.
func testWorkflowFullReplay(ctx context.Context, t *testing.T, lab *Lab) {
	queryID := lab.QueryID(t, "AsqWorkflowReplayMarker")
	planID := planIDFor(ctx, t, lab, queryID)
	qid := fmt.Sprintf("%d", queryID)
	pid := fmt.Sprintf("%d", planID)

	steps := []struct {
		name string
		args []string
	}{
		{"info", []string{"info"}},
		{"qs status", []string{"qs", "status"}},
		{"qs top", []string{"qs", "top"}},
		{"qs query", []string{"qs", "query", qid}},
		{"plan", []string{"plan", qid, "--plan-id", pid}},
		{"obj table", []string{"obj", "table", "dbo.Orders"}},
		{"idx usage", []string{"idx", "usage", "dbo.UsageFixture"}},
		{"idx missing", []string{"idx", "missing"}},
		{"stats list", []string{"stats", "list", "dbo.Orders"}},
	}

	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			result, code := lab.Run(t, "S", step.args)
			if code != 0 {
				t.Fatalf("%v: got exit code %d, want 0 (error=%+v)", step.args, code, result.Error)
			}
			if !result.OK {
				t.Fatalf("%v: result.OK is false despite exit code 0", step.args)
			}
			for _, a := range result.Artifacts {
				if !a.Complete {
					continue
				}
				data, err := os.ReadFile(a.Path)
				if err != nil {
					t.Fatalf("%v: reading real artifact %q: %v", step.args, a.Path, err)
				}
				if int64(len(data)) != a.Bytes {
					t.Fatalf("%v: artifact %q on-disk size %d does not match reported Bytes %d", step.args, a.Path, len(data), a.Bytes)
				}
			}
			if result.ManifestPath != "" {
				if _, err := os.Stat(result.ManifestPath); err != nil {
					t.Fatalf("%v: manifest path %q does not exist: %v", step.args, result.ManifestPath, err)
				}
			}
		})
	}
}
