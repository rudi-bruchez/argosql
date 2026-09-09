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

type cliFakeDriver struct{}

func (cliFakeDriver) Open(name string) (driver.Conn, error) { return &cliFakeConn{}, nil }

type cliFakeConn struct{}

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
	t.Helper()
	name := fmt.Sprintf("clifake-%d", cliFakeDriverSeq.Add(1))
	sql.Register(name, cliFakeDriver{})
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
