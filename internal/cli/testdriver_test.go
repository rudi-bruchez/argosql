package cli

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rudi-bruchez/argosql/internal/sqlserver"
)

// This package's own Execute closures (infoCommand, qsStatusCommand in
// registry.go) now call straight into internal/diagnostics, which
// queries *sqlserver.Session.Conn for real. A test that drives Run or
// run through one of those commands therefore needs a *sql.Conn that
// actually answers - unlike task 9a's placeholder Execute, which never
// touched Conn at all and let every test here get away with
// &sqlserver.Session{Major: n} and a nil Conn. cliFakeDriver is the
// smallest database/sql/driver that makes that true again: it
// recognizes internal/diagnostics' three embedded queries (sql/info.sql,
// sql/health.sql, sql/coverage.sql) by one distinctive substring each
// carries, and answers each with one canned row of exactly the shape
// internal/output.ScanRow expects - including each column's
// DatabaseTypeName, via driver.RowsColumnTypeDatabaseTypeName, since
// ScanRow decides every ambiguous conversion from that, never from the
// driver value's bare Go type. Any other query is a test bug, not a
// case to guess at, and errors instead of guessing.

// cliFakeDriverSeq gives every call to newFakeSession its own driver
// name: database/sql.Register panics if the same name is registered
// twice, and this package's test binary calls newFakeSession more than
// once.
var cliFakeDriverSeq atomic.Int64

// cliFakeDriver's planXML, when non-empty, is what its own connections
// answer sql/plan.sql's query with - fix 2's B10 own addition, needed
// to drive the "plan" command through Run/run for the first time in
// this package. Every existing caller of newFakeSession leaves this at
// its zero value ("") and never reaches the plan-query case at all.
//
// missingArgs, when non-nil, is fix 1's B1 own addition: the args
// sql/missing.sql's own query was actually called with, captured here
// so a test can prove idxMissingCommand's Execute closure really built
// MissingOptions from req.Table/req.Top and sent them all the way to
// the query - the registry-to-diagnostics wiring that, before this
// fix, no test in this package exercised at all (idx_missing_test.go
// only drove Parse; TestIndexDMV, the one test against a real engine,
// calls diagnostics.Missing directly with hand-built MissingOptions,
// bypassing the registry entirely).
type cliFakeDriver struct {
	planXML     string
	missingArgs *[]driver.NamedValue
	// queryErr, when set, is returned by every QueryContext after
	// session setup has already succeeded. It exists so a test can fail
	// a command AFTER the Collector was created and therefore after a
	// manifest will be written - the only shape that exercises the
	// manifest's own redaction. Failing the connection instead (see
	// fakeOpenSession variants) never reaches a manifest at all.
	queryErr error
}

func (d cliFakeDriver) Open(name string) (driver.Conn, error) {
	return &cliFakeConn{planXML: d.planXML, missingArgs: d.missingArgs, queryErr: d.queryErr}, nil
}

type cliFakeConn struct {
	planXML     string
	missingArgs *[]driver.NamedValue
	queryErr    error
}

func (c *cliFakeConn) Prepare(query string) (driver.Stmt, error) {
	return nil, fmt.Errorf("cliFakeConn: Prepare not supported, use *Context")
}
func (c *cliFakeConn) Close() error { return nil }
func (c *cliFakeConn) Begin() (driver.Tx, error) {
	return nil, fmt.Errorf("cliFakeConn: Begin not supported")
}

// QueryContext implements driver.QueryerContext. It matches each of
// internal/diagnostics' three embedded queries by a substring unique to
// that query's own SQL text (the catalog object each one reads from),
// so this fake keeps working across cosmetic rewording of the queries
// themselves.
func (c *cliFakeConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	// Session setup (LOCK_TIMEOUT, @@LOCK_TIMEOUT, ProductMajorVersion)
	// must still succeed, or no Collector and no manifest ever exist;
	// queryErr only fails the diagnostic queries that come after.
	// "ProductMajorVersion", not "SERVERPROPERTY": sql/info.sql reads
	// edition and product version through SERVERPROPERTY too, so the
	// broader exemption silently spared the very query this hook exists
	// to fail - measured, the command returned 0 and wrote no error.
	if c.queryErr != nil && !strings.Contains(query, "LOCK_TIMEOUT") && !strings.Contains(query, "ProductMajorVersion") {
		return nil, c.queryErr
	}
	switch {
	case strings.Contains(query, "DATABASEPROPERTYEX"):
		// sql/info.sql
		return &cliFakeRows{
			cols:  []string{"server", "database", "principal", "product_version", "edition", "compatibility_level"},
			types: []string{"NVARCHAR", "NVARCHAR", "NVARCHAR", "NVARCHAR", "NVARCHAR", "INT"},
			row:   []driver.Value{"FAKESRV", "db", "user", "16.0.1000.6", "Developer Edition", int64(160)},
		}, nil
	case strings.Contains(query, "sys.database_query_store_options"):
		// sql/health.sql
		return &cliFakeRows{
			cols: []string{
				"desired_state", "actual_state", "readonly_reason", "capture_mode",
				"current_storage_mb", "max_storage_mb", "retention_days", "interval_minutes", "has_history",
			},
			types: []string{"NVARCHAR", "NVARCHAR", "BIGINT", "NVARCHAR", "DECIMAL", "DECIMAL", "INT", "INT", "BIT"},
			row: []driver.Value{
				"READ_WRITE", "READ_WRITE", int64(0), "ALL",
				[]byte("0.50"), []byte("100.00"), int64(30), int64(60), false,
			},
		}, nil
	case strings.Contains(query, "query_store_runtime_stats_interval"):
		// sql/coverage.sql
		return &cliFakeRows{
			cols:  []string{"oldest_interval", "newest_interval"},
			types: []string{"DATETIME2", "DATETIME2"},
			row:   []driver.Value{nil, nil},
		}, nil
	case strings.Contains(query, "query_store_plan") && c.planXML != "":
		// sql/plan.sql (internal/diagnostics) - only answered when a
		// test actually configured a plan XML via
		// newFakeSessionWithPlanXML; every other caller of
		// newFakeSession never sends this query at all.
		return &cliFakeRows{
			cols:  []string{"query_plan"},
			types: []string{"XML"},
			row:   []driver.Value{c.planXML},
		}, nil
	case strings.Contains(query, "sys.schemas AS s ON s.schema_id") && c.missingArgs != nil:
		// sqlserver.Resolve's own resolveObjectQuery - only reached by
		// "idx missing --table ..."; scanned directly with Row.Scan,
		// never through output.ScanRow, so DatabaseTypeName is not
		// consulted here.
		return &cliFakeRows{
			cols:  []string{"object_id", "schema_name", "object_name", "type"},
			types: []string{"INT", "NVARCHAR", "NVARCHAR", "CHAR"},
			row:   []driver.Value{int64(4242), "dbo", "Orders", "U "},
		}, nil
	case strings.Contains(query, "impact_score") && c.missingArgs != nil:
		// sql/missing.sql: captures its own args (fix 1's B1) rather
		// than merely answering the query, so a test can assert on the
		// exact @top/@object_id values idxMissingCommand's Execute
		// closure actually sent, all the way from the parsed Request.
		*c.missingArgs = args
		return &cliFakeRows{
			cols: []string{"index_handle", "object_id", "schema_name", "object_name",
				"equality_columns", "inequality_columns", "included_columns",
				"user_seeks", "user_scans", "avg_total_user_cost", "avg_user_impact", "impact_score"},
			types: []string{"INT", "INT", "NVARCHAR", "NVARCHAR", "NVARCHAR", "NVARCHAR", "NVARCHAR",
				"BIGINT", "BIGINT", "FLOAT", "FLOAT", "FLOAT"},
			row: []driver.Value{int64(501), int64(4242), "dbo", "Orders", "[CustomerId]", nil, nil,
				int64(100), int64(50), float64(20), float64(80), float64(2400)},
		}, nil
	}
	return nil, fmt.Errorf("cliFakeConn: unexpected query %q", query)
}

// cliFakeRows is a driver.Rows over exactly one row, answering
// ColumnTypeDatabaseTypeName from types - the optional interface
// output.ScanRow's convertCell relies on to decide each column's
// conversion.
type cliFakeRows struct {
	cols  []string
	types []string
	row   []driver.Value
	done  bool
}

func (r *cliFakeRows) Columns() []string                           { return r.cols }
func (r *cliFakeRows) Close() error                                { return nil }
func (r *cliFakeRows) ColumnTypeDatabaseTypeName(index int) string { return r.types[index] }

func (r *cliFakeRows) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	copy(dest, r.row)
	r.done = true
	return nil
}

// newFakeSession opens a *sqlserver.Session backed by cliFakeDriver: a
// real *sql.Conn, so diagnostics.Info and diagnostics.Status run
// exactly as they would against a live server, answering from the
// canned rows above instead of a network connection.
func newFakeSession(t *testing.T, major int) *sqlserver.Session {
	return newFakeSessionWithPlanXML(t, major, "")
}

// newFakeSessionWithPlanXML is newFakeSession, plus a query_plan value
// for sql/plan.sql's own lookup - fix 2's B10, the first test in this
// package to drive the "plan" command through Run/run rather than
// info/qs status.
func newFakeSessionWithPlanXML(t *testing.T, major int, planXML string) *sqlserver.Session {
	t.Helper()
	return newFakeSessionRaw(t, cliFakeDriver{planXML: planXML}, major)
}

// newFakeSessionCapturingMissingArgs is newFakeSession, plus a
// destination for the args "idx missing"'s own query was actually
// called with (fix 1's B1) - see cliFakeDriver's own doc comment.
func newFakeSessionCapturingMissingArgs(t *testing.T, major int, missingArgs *[]driver.NamedValue) *sqlserver.Session {
	t.Helper()
	return newFakeSessionRaw(t, cliFakeDriver{missingArgs: missingArgs}, major)
}

func newFakeSessionRaw(t *testing.T, d cliFakeDriver, major int) *sqlserver.Session {
	t.Helper()
	name := fmt.Sprintf("clifake-%d", cliFakeDriverSeq.Add(1))
	sql.Register(name, d)
	db, err := sql.Open(name, "")
	if err != nil {
		t.Fatalf("opening fake driver: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatalf("acquiring fake conn: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return &sqlserver.Session{Conn: conn, Major: major}
}
