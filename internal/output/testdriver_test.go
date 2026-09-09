package output

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"testing"
)

// fakeColumn describes one column of a fakeRowsSource: its name, the
// DatabaseTypeName a real go-mssqldb column of that SQL type would report,
// and the raw driver.Value generic scanning into interface{} would
// produce for it (nil, string, bool, int64, float64, []byte or
// time.Time - the only types driver.Value allows).
type fakeColumn struct {
	name   string
	dbType string
	value  driver.Value
}

// fakeRowsSource is a driver.Rows over exactly one row, whose columns and
// values are whatever the test configures - it never talks to a real
// server. It implements driver.RowsColumnTypeDatabaseTypeName so that
// (*sql.Rows).ColumnTypes() reports the DatabaseTypeName this package's
// conversion logic must decide on, exactly the way go-mssqldb's real Rows
// does.
type fakeRowsSource struct {
	cols []fakeColumn
	sent bool
}

func (r *fakeRowsSource) Columns() []string {
	names := make([]string, len(r.cols))
	for i, c := range r.cols {
		names[i] = c.name
	}
	return names
}

func (r *fakeRowsSource) Close() error { return nil }

func (r *fakeRowsSource) Next(dest []driver.Value) error {
	if r.sent {
		return io.EOF
	}
	for i, c := range r.cols {
		dest[i] = c.value
	}
	r.sent = true
	return nil
}

func (r *fakeRowsSource) ColumnTypeDatabaseTypeName(index int) string {
	return r.cols[index].dbType
}

type fakeConn struct {
	rows *fakeRowsSource
}

func (c *fakeConn) Prepare(query string) (driver.Stmt, error) {
	return nil, errors.New("fakeConn: Prepare not supported, use QueryContext")
}
func (c *fakeConn) Close() error { return nil }
func (c *fakeConn) Begin() (driver.Tx, error) {
	return nil, errors.New("fakeConn: Begin not supported")
}
func (c *fakeConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	return c.rows, nil
}

type fakeDriver struct{}

func (d *fakeDriver) Open(name string) (driver.Conn, error) {
	return nil, errors.New("fakeDriver: Open not supported, use the connector")
}

type fakeConnector struct {
	conn *fakeConn
}

func (c *fakeConnector) Connect(ctx context.Context) (driver.Conn, error) { return c.conn, nil }
func (c *fakeConnector) Driver() driver.Driver                            { return &fakeDriver{} }

// queryFakeRow opens a *sql.DB backed by a fake driver reporting exactly
// the one row described by cols, runs a trivial query against it, and
// returns the resulting, already-advanced *sql.Rows plus its
// ColumnTypes. Both are registered for cleanup on t.
func queryFakeRow(t *testing.T, cols []fakeColumn) (*sql.Rows, []*sql.ColumnType) {
	t.Helper()
	db := sql.OpenDB(&fakeConnector{conn: &fakeConn{rows: &fakeRowsSource{cols: cols}}})
	t.Cleanup(func() { db.Close() })

	rows, err := db.QueryContext(context.Background(), "SELECT 1")
	if err != nil {
		t.Fatalf("fake query: %v", err)
	}
	t.Cleanup(func() { rows.Close() })

	if !rows.Next() {
		t.Fatalf("fake rows: expected one row, got none (err: %v)", rows.Err())
	}
	types, err := rows.ColumnTypes()
	if err != nil {
		t.Fatalf("column types: %v", err)
	}
	return rows, types
}
