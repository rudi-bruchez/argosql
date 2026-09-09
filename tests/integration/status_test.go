//go:build integration

package integration

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rudi-bruchez/argosql/internal/diagnostics"
	"github.com/rudi-bruchez/argosql/internal/model"
	"github.com/rudi-bruchez/argosql/internal/sqlserver"
)

// captureSink is a minimal model.Sink: every table's rows stay in
// memory, nothing ever touches a file. It lets this file's tests
// assert directly on what diagnostics.Info/diagnostics.Status produced,
// without going through internal/artifacts or internal/cli.
type captureSink struct {
	tables []capturedTable
	cur    *capturedTable
}

type capturedTable struct {
	spec               model.TableSpec
	rows               [][]model.Cell
	collectionComplete bool
	propertiesComplete bool
}

func (s *captureSink) Begin(spec model.TableSpec) error {
	s.cur = &capturedTable{spec: spec}
	return nil
}

func (s *captureSink) Row(row []model.Cell) error {
	if s.cur == nil {
		return fmt.Errorf("captureSink: Row called with no open table")
	}
	s.cur.rows = append(s.cur.rows, row)
	return nil
}

// End records both completeness flags diagnostics.Info/diagnostics.Status
// pass it, rather than discarding them: TestInfo and TestStatus assert on
// collectionComplete/propertiesComplete below, so a command that reports
// incomplete data by mistake (or a refactor that starts passing false)
// shows up here instead of only downstream in internal/model.Completeness.
func (s *captureSink) End(collectionComplete, propertiesComplete bool) error {
	if s.cur == nil {
		return fmt.Errorf("captureSink: End called with no open table")
	}
	s.cur.collectionComplete = collectionComplete
	s.cur.propertiesComplete = propertiesComplete
	s.tables = append(s.tables, *s.cur)
	s.cur = nil
	return nil
}

func (s *captureSink) File(kind, suffix string, src io.Reader) (model.Artifact, error) {
	return model.Artifact{}, fmt.Errorf("captureSink: File not supported")
}

func (s *captureSink) Notice(model.Notice) {}

func (s *captureSink) table(name string) *capturedTable {
	for i := range s.tables {
		if s.tables[i].spec.Name == name {
			return &s.tables[i]
		}
	}
	return nil
}

// requireComplete asserts that tbl's table was Begin/Row/End'd with both
// completeness flags true. Every diagnostics.Info/diagnostics.Status table
// is complete by construction at this stage (no truncation, no partial
// read), so true is the value every caller of this helper expects.
func requireComplete(t *testing.T, tbl *capturedTable, name string) {
	t.Helper()
	if !tbl.collectionComplete {
		t.Fatalf("%s: collectionComplete got false, want true", name)
	}
	if !tbl.propertiesComplete {
		t.Fatalf("%s: propertiesComplete got false, want true", name)
	}
}

// TestInfo proves diagnostics.Info actually reads the engine it is
// connected to, rather than anything carried over from config.Profile
// or a hardcoded literal: the server cell is checked against
// SERVERPROPERTY('ServerName') queried independently through lab.Admin
// (a separate connection, never through diagnostics.Info itself), and
// the database cell and compatibility level are checked against the
// known fixture values. Checking the server cell this way, rather than
// only forbidding known secret values, is what actually catches a
// hardcoded literal standing in for SERVERPROPERTY: measured, swapping
// SERVERPROPERTY('ServerName') for the fixed literal 'localhost:1433'
// in info.sql does not contain lab.Profile's real host or port (podman
// publishes SQL Server on a random ephemeral port, never 1433, and
// lab.Profile.Host measures "127.0.0.1", not "localhost"), so a loop
// that only forbids those two real values lets that substitution pass;
// comparing the cell against the independently queried real server name
// does not. The design spec also requires info to carry "no connection
// string or secrets", so this additionally checks that neither the
// admin password nor lab's own host:port (combined, and the port alone)
// ever appears in any cell, and that Info reports both completeness
// flags true.
func TestInfo(t *testing.T) {
	lab := NewLab(t, os.Getenv("ASQ_TEST_IMAGE"))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	sess, err := sqlserver.Open(ctx, lab.Profile)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer sess.Close()
	logEngineIdentity(t, lab, sess.Major)

	var wantServer string
	if err := lab.Admin.QueryRowContext(ctx, "SELECT CONVERT(nvarchar(128), SERVERPROPERTY('ServerName'))").Scan(&wantServer); err != nil {
		t.Fatalf("querying SERVERPROPERTY('ServerName') independently: %v", err)
	}

	sink := &captureSink{}
	if err := diagnostics.Info(ctx, sess, sink); err != nil {
		t.Fatalf("info: %v", err)
	}

	identity := sink.table("identity")
	if identity == nil || len(identity.rows) != 1 {
		t.Fatalf("identity table missing or wrong row count: %+v", sink.tables)
	}
	requireComplete(t, identity, "identity")
	row := identity.rows[0]
	if got, ok := row[0].(string); !ok || got != wantServer {
		t.Fatalf("server: got %#v, want %q (independently queried SERVERPROPERTY('ServerName'))", row[0], wantServer)
	}
	if got, ok := row[1].(string); !ok || got != "AppDB" {
		t.Fatalf("database: got %#v, want AppDB", row[1])
	}
	if got, ok := row[5].(int64); !ok || got == 0 {
		t.Fatalf("compatibility_level: got %#v, want a nonzero integer", row[5])
	}
	hostPort := fmt.Sprintf("%s:%d", lab.Profile.Host, lab.Profile.Port)
	port := strconv.Itoa(lab.Profile.Port)
	for i, cell := range row {
		s, ok := cell.(string)
		if !ok {
			continue
		}
		if strings.Contains(s, lab.Profile.Password) {
			t.Fatalf("column %d leaks the connection password: %#v", i, cell)
		}
		if strings.Contains(s, hostPort) {
			t.Fatalf("column %d leaks the connection host:port %q: %#v", i, hostPort, cell)
		}
		if strings.Contains(s, port) {
			t.Fatalf("column %d leaks the connection port %q: %#v", i, port, cell)
		}
	}
}

// createThrowawayDatabase creates a uniquely named database on lab's
// own dedicated container (never AppDB, never a database any other
// test or fixture touches) and registers its cleanup. It is a real
// CREATE DATABASE on this test's own disposable server, not a fixture
// shared with anything else: every throwaway database this file opens
// starts completely empty, no table, no row, ever created in it.
func createThrowawayDatabase(ctx context.Context, t *testing.T, lab *Lab, prefix string) string {
	t.Helper()
	dbName := prefix + randomHex(4)
	if _, err := lab.Admin.ExecContext(ctx, "CREATE DATABASE ["+dbName+"]"); err != nil {
		t.Fatalf("creating throwaway database %q: %v", dbName, err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), podmanCmdTimeout)
		defer cleanupCancel()
		lab.Admin.ExecContext(cleanupCtx, "ALTER DATABASE ["+dbName+"] SET SINGLE_USER WITH ROLLBACK IMMEDIATE")
		lab.Admin.ExecContext(cleanupCtx, "DROP DATABASE ["+dbName+"]")
	})
	return dbName
}

// TestStatus exercises diagnostics.Status against a real engine.
//
// Its first subtest is the task 9 brief's most important case, but not
// quite the way the brief first describes it: AppDB (the shared
// fixture NewLab's sql/bootstrap.sql seeds with a CREATE TABLE and
// three INSERT statements, Query Store already ON by then) is not
// "fresh" by the time any test reaches it - those INSERT statements
// are real DML, genuinely captured by Query Store. "No runtime history
// yet" instead needs a database that has had Query Store turned on and
// has had literally no query executed against it beyond the
// diagnostic's own reads - this subtest builds exactly that, and
// measured, it proves sqlserver.Open's own two session-setup SELECT
// statements and diagnostics.Status's own two embedded queries do not
// themselves get captured by Query Store (they read sys.*
// catalog/DMV state, not user data), so the real code path - Open then
// Status, with no shortcut - genuinely observes has_history = false
// and a NULL oldest/newest coverage window, never an invented one-hour
// window from the interval that already exists with zero runtime
// rows.
//
// Its second subtest proves the opposite, and measured, not on the
// first try: AppDB's own bootstrap INSERTs do eventually show up as
// real interval coverage, but not synchronously. Query Store's
// sys.query_store_runtime_stats DMV does not reflect a just-executed
// statement the instant it commits; a first attempt taken right after
// NewLab returns sometimes saw has_history still false from
// health.sql's own EXISTS check, while coverage.sql's separately
// issued MIN/MAX, a moment later in the very same Status() call, had
// already started seeing the same INSERTs' rows. Two queries issued
// back to back are not a single consistent snapshot of a continuously
// catching-up DMV, so this subtest polls until both agree rather than
// asserting on that race.
//
// Its third subtest proves Query Store OFF does not stop qs status
// from succeeding at code 0 - the whole reason this command exists is
// to report exactly that state. Measured: SQL Server 2022 (unlike 2019)
// enables Query Store by default on every newly created database, so
// this subtest turns it OFF explicitly rather than relying on a
// version-dependent default.
func TestStatus(t *testing.T) {
	lab := NewLab(t, os.Getenv("ASQ_TEST_IMAGE"))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	sess, err := sqlserver.Open(ctx, lab.Profile)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer sess.Close()
	logEngineIdentity(t, lab, sess.Major)

	t.Run("database with Query Store on and no query ever run has null coverage", func(t *testing.T) {
		dbName := createThrowawayDatabase(ctx, t, lab, "asq_fresh_")
		if _, err := lab.Admin.ExecContext(ctx, "ALTER DATABASE ["+dbName+"] SET QUERY_STORE = ON"); err != nil {
			t.Fatalf("enabling Query Store: %v", err)
		}
		if _, err := lab.Admin.ExecContext(ctx, "ALTER DATABASE ["+dbName+"] SET QUERY_STORE (OPERATION_MODE = READ_WRITE, INTERVAL_LENGTH_MINUTES = 1)"); err != nil {
			t.Fatalf("configuring Query Store: %v", err)
		}

		freshProfile := lab.Profile
		freshProfile.Database = dbName
		freshSess, err := sqlserver.Open(ctx, freshProfile)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer freshSess.Close()

		sink := &captureSink{}
		if err := diagnostics.Status(ctx, freshSess, sink); err != nil {
			t.Fatalf("status: %v", err)
		}

		statusTable := sink.table("status")
		if statusTable == nil || len(statusTable.rows) != 1 {
			t.Fatalf("status table missing or wrong row count: %+v", sink.tables)
		}
		requireComplete(t, statusTable, "status")
		statusRow := statusTable.rows[0]
		if got, ok := statusRow[0].(string); !ok || got != "READ_WRITE" {
			t.Fatalf("desired_state: got %#v, want READ_WRITE", statusRow[0])
		}
		if got, ok := statusRow[1].(string); !ok || got != "READ_WRITE" {
			t.Fatalf("actual_state: got %#v, want READ_WRITE", statusRow[1])
		}
		if got, ok := statusRow[2].(int64); !ok || got != 0 {
			t.Fatalf("readonly_reason: got %#v, want 0", statusRow[2])
		}
		if got, ok := statusRow[3].(string); !ok || got != "none" {
			t.Fatalf("readonly_reason_decoded: got %#v, want \"none\"", statusRow[3])
		}

		coverage := sink.table("coverage")
		if coverage == nil || len(coverage.rows) != 1 {
			t.Fatalf("coverage table missing or wrong row count: %+v", sink.tables)
		}
		requireComplete(t, coverage, "coverage")
		coverageRow := coverage.rows[0]
		if coverageRow[0] != nil {
			t.Fatalf("oldest_interval: got %#v, want nil (no invented coverage window)", coverageRow[0])
		}
		if coverageRow[1] != nil {
			t.Fatalf("newest_interval: got %#v, want nil (no invented coverage window)", coverageRow[1])
		}
		if got, ok := coverageRow[2].(bool); !ok || got != false {
			t.Fatalf("has_history: got %#v, want false", coverageRow[2])
		}
	})

	t.Run("AppDB eventually carries real interval coverage from its own fixture data", func(t *testing.T) {
		deadline := time.Now().Add(20 * time.Second)
		var row []model.Cell
		for {
			sink := &captureSink{}
			if err := diagnostics.Status(ctx, sess, sink); err != nil {
				t.Fatalf("status: %v", err)
			}
			coverage := sink.table("coverage")
			if coverage == nil || len(coverage.rows) != 1 {
				t.Fatalf("coverage table missing or wrong row count: %+v", sink.tables)
			}
			requireComplete(t, coverage, "coverage")
			row = coverage.rows[0]
			hasHistory, ok := row[2].(bool)
			if !ok {
				t.Fatalf("has_history: got %#v, want a bool", row[2])
			}
			if hasHistory && row[0] != nil && row[1] != nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("AppDB's bootstrap INSERTs never became visible in Query Store's runtime stats within 20s: last coverage row = %#v", row)
			}
			time.Sleep(300 * time.Millisecond)
		}
		if row[0] == nil || row[1] == nil {
			t.Fatalf("oldest/newest_interval: got %#v, %#v, want non-nil", row[0], row[1])
		}
	})

	t.Run("Query Store OFF still succeeds at code 0", func(t *testing.T) {
		dbName := createThrowawayDatabase(ctx, t, lab, "asq_qs_off_")
		if _, err := lab.Admin.ExecContext(ctx, "ALTER DATABASE ["+dbName+"] SET QUERY_STORE = OFF"); err != nil {
			t.Fatalf("disabling Query Store: %v", err)
		}

		offProfile := lab.Profile
		offProfile.Database = dbName
		offSess, err := sqlserver.Open(ctx, offProfile)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer offSess.Close()

		sink := &captureSink{}
		if err := diagnostics.Status(ctx, offSess, sink); err != nil {
			t.Fatalf("status on a Query-Store-OFF database should succeed at code 0, got: %v", err)
		}
		statusTable := sink.table("status")
		if statusTable == nil || len(statusTable.rows) != 1 {
			t.Fatalf("status table missing or wrong row count: %+v", sink.tables)
		}
		requireComplete(t, statusTable, "status")
		if got, ok := statusTable.rows[0][1].(string); !ok || got != "OFF" {
			t.Fatalf("actual_state: got %#v, want OFF", statusTable.rows[0][1])
		}
	})

	// t.Run("a READ_ONLY database separates desired from actual state and
	// still succeeds at code 0") is built on the exact sequence measured
	// against a disposable SQL Server 2022 CU26 container:
	//
	//	CREATE DATABASE RO;
	//	ALTER DATABASE RO SET QUERY_STORE = ON (OPERATION_MODE = READ_WRITE, INTERVAL_LENGTH_MINUTES = 1);
	//	ALTER DATABASE RO SET READ_ONLY WITH ROLLBACK IMMEDIATE;
	//
	// which measured as desired_state_desc=READ_WRITE,
	// actual_state_desc=READ_ONLY, readonly_reason=1,
	// query_capture_mode_desc=AUTO. This one scenario is the spec's other
	// named code-0 state besides OFF (no test covered READ_ONLY before
	// this), it is the first state where desired_state and actual_state
	// genuinely differ (a prior bug swapping statusRow[0] and
	// statusRow[1] in health.go would pass every other subtest in this
	// file but fail here), and readonly_reason=1 is bit 0, so
	// readonly_reason_decoded must come back exactly
	// "database_read_only" - the first time DecodeReadOnly's bit table is
	// exercised against a real engine value rather than a value this
	// file fabricates.
	t.Run("a READ_ONLY database separates desired from actual state and still succeeds at code 0", func(t *testing.T) {
		dbName := createThrowawayDatabase(ctx, t, lab, "asq_ro_")
		if _, err := lab.Admin.ExecContext(ctx, "ALTER DATABASE ["+dbName+"] SET QUERY_STORE = ON (OPERATION_MODE = READ_WRITE, INTERVAL_LENGTH_MINUTES = 1)"); err != nil {
			t.Fatalf("enabling Query Store: %v", err)
		}
		if _, err := lab.Admin.ExecContext(ctx, "ALTER DATABASE ["+dbName+"] SET READ_ONLY WITH ROLLBACK IMMEDIATE"); err != nil {
			t.Fatalf("setting database READ_ONLY: %v", err)
		}
		// Registered after createThrowawayDatabase's own t.Cleanup above,
		// so t.Cleanup's LIFO order runs this one first: a READ_ONLY
		// database cannot be DROPped, so this restores READ_WRITE before
		// that cleanup's own DROP DATABASE runs.
		t.Cleanup(func() {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), podmanCmdTimeout)
			defer cleanupCancel()
			if _, err := lab.Admin.ExecContext(cleanupCtx, "ALTER DATABASE ["+dbName+"] SET READ_WRITE WITH ROLLBACK IMMEDIATE"); err != nil {
				t.Logf("cleanup: restoring %s to READ_WRITE: %v", dbName, err)
			}
		})

		roProfile := lab.Profile
		roProfile.Database = dbName
		roSess, err := sqlserver.Open(ctx, roProfile)
		if err != nil {
			t.Fatalf("open on a READ_ONLY database: %v", err)
		}
		defer roSess.Close()

		sink := &captureSink{}
		if err := diagnostics.Status(ctx, roSess, sink); err != nil {
			t.Fatalf("status on a READ_ONLY database should succeed at code 0, got: %v", err)
		}
		statusTable := sink.table("status")
		if statusTable == nil || len(statusTable.rows) != 1 {
			t.Fatalf("status table missing or wrong row count: %+v", sink.tables)
		}
		requireComplete(t, statusTable, "status")
		statusRow := statusTable.rows[0]
		if got, ok := statusRow[0].(string); !ok || got != "READ_WRITE" {
			t.Fatalf("desired_state: got %#v, want READ_WRITE", statusRow[0])
		}
		if got, ok := statusRow[1].(string); !ok || got != "READ_ONLY" {
			t.Fatalf("actual_state: got %#v, want READ_ONLY", statusRow[1])
		}
		if got, ok := statusRow[2].(int64); !ok || got != 1 {
			t.Fatalf("readonly_reason: got %#v, want 1", statusRow[2])
		}
		if got, ok := statusRow[3].(string); !ok || got != "database_read_only" {
			t.Fatalf("readonly_reason_decoded: got %#v, want \"database_read_only\"", statusRow[3])
		}
	})
}
