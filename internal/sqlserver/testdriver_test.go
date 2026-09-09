package sqlserver

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	mssql "github.com/microsoft/go-mssqldb"
)

// fakeConn is a minimal database/sql/driver.Conn implementing exactly the
// interfaces Session needs: Conn, QueryerContext, ExecerContext and
// SessionResetter. It never talks to a real SQL Server: it recognizes the
// exact statements session.go issues and answers them from in-memory
// state, and it logs every call it receives so tests can assert on the
// order and presence of events without a real server.
type fakeConn struct {
	mu     sync.Mutex
	events *[]string

	lockTimeout int // the value session.go's SET LOCK_TIMEOUT last set, tracked from the statement it actually sent, not assumed
	major       int
	majorErr    error // returned by QueryContext for the SERVERPROPERTY statement instead of major

	// lockTimeoutOverride, when non-nil, makes the @@LOCK_TIMEOUT read-back
	// report this value instead of the one SET LOCK_TIMEOUT actually
	// stored: a session setting that silently failed to take effect on
	// the server, the one scenario setupSession's mismatch check exists
	// to catch.
	lockTimeoutOverride *int

	execErr    error // returned by ExecContext for the SET LOCK_TIMEOUT statement
	execBlocks bool  // ExecContext blocks on ctx.Done() instead of answering
	closeErr   error // returned by Close, to prove Session.Close surfaces it

	// permsResult, when non-nil, is what a HAS_PERMS_BY_NAME query
	// (issued by either Probe or ServerProbe) reports back: Valid=false
	// simulates the malformed-probe NULL, Int64=0/Valid=true simulates
	// Denied, Int64!=0/Valid=true simulates Allowed. permsQueryErr, when
	// set, makes the query fail outright instead. Exactly one of the two
	// must be set for a test that issues such a query, or the fake
	// reports an error rather than guessing.
	permsResult   *sql.NullInt64
	permsQueryErr error

	// resolveRow and resolveNoRows configure what Resolve's
	// sys.objects/sys.schemas join query reports back: resolveRow answers
	// with that one row, resolveNoRows simulates zero rows (io.EOF on the
	// first Next, same as sql.ErrNoRows through QueryRowContext), and
	// resolveQueryErr makes the query fail outright. Exactly one of the
	// three must be set for a test that issues this query.
	resolveRow      *fakeResolvedRow
	resolveNoRows   bool
	resolveQueryErr error
}

// fakeResolvedRow is the one catalog row fakeConn answers Resolve's query
// with when resolveRow is set: object_id, schema name, object name, and
// the raw (unpadded or padded - either works, since Resolve trims it)
// sys.objects.type value.
type fakeResolvedRow struct {
	objectID int64
	schema   string
	name     string
	typ      string
}

func (c *fakeConn) log(event string) {
	c.mu.Lock()
	*c.events = append(*c.events, event)
	c.mu.Unlock()
}

func (c *fakeConn) Prepare(query string) (driver.Stmt, error) {
	return nil, errors.New("fakeConn: Prepare not supported, use *Context")
}

func (c *fakeConn) Close() error {
	c.log("close")
	return c.closeErr
}

func (c *fakeConn) Begin() (driver.Tx, error) {
	return nil, errors.New("fakeConn: Begin not supported")
}

func (c *fakeConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if c.execBlocks {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	if strings.HasPrefix(query, "SET LOCK_TIMEOUT ") {
		c.log("exec:set-lock-timeout")
		if c.execErr != nil {
			return nil, c.execErr
		}
		// Parsed from the statement actually received, not assumed: a
		// production change to the literal this package sends must be
		// visible here, or this fake proves nothing about it.
		var n int
		if _, err := fmt.Sscanf(query, "SET LOCK_TIMEOUT %d", &n); err != nil {
			return nil, fmt.Errorf("fakeConn: cannot parse %q: %w", query, err)
		}
		c.mu.Lock()
		c.lockTimeout = n
		c.mu.Unlock()
		return driver.ResultNoRows, nil
	}
	return nil, fmt.Errorf("fakeConn: unexpected exec %q", query)
}

func (c *fakeConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	switch {
	case strings.Contains(query, "@@LOCK_TIMEOUT"):
		c.mu.Lock()
		v := c.lockTimeout
		if c.lockTimeoutOverride != nil {
			v = *c.lockTimeoutOverride
		}
		c.mu.Unlock()
		c.log("query:lock-timeout")
		return &fakeScalarRows{col: "lock_timeout", value: int64(v)}, nil
	case strings.Contains(query, "ProductMajorVersion"):
		c.log("query:major-version")
		if c.majorErr != nil {
			return nil, c.majorErr
		}
		return &fakeScalarRows{col: "major", value: int64(c.major)}, nil
	case strings.Contains(query, "HAS_PERMS_BY_NAME"):
		c.log("query:has-perms-by-name")
		if _, ok := namedArg(args, "permission"); !ok {
			return nil, fmt.Errorf("fakeConn: HAS_PERMS_BY_NAME query %q is missing its @permission parameter", query)
		}
		if c.permsQueryErr != nil {
			return nil, c.permsQueryErr
		}
		if c.permsResult == nil {
			return nil, fmt.Errorf("fakeConn: HAS_PERMS_BY_NAME probed but the test configured neither permsResult nor permsQueryErr")
		}
		return &fakeNullIntRows{value: *c.permsResult}, nil
	case strings.Contains(query, "sys.objects"):
		c.log("query:resolve-object")
		if _, ok := namedArg(args, "schema"); !ok {
			return nil, fmt.Errorf("fakeConn: resolve query %q is missing its @schema parameter", query)
		}
		if _, ok := namedArg(args, "name"); !ok {
			return nil, fmt.Errorf("fakeConn: resolve query %q is missing its @name parameter", query)
		}
		if c.resolveQueryErr != nil {
			return nil, c.resolveQueryErr
		}
		if c.resolveRow != nil {
			return &fakeResolveRows{row: c.resolveRow}, nil
		}
		if c.resolveNoRows {
			return &fakeResolveRows{}, nil
		}
		return nil, fmt.Errorf("fakeConn: resolve query issued but the test configured neither resolveRow nor resolveNoRows nor resolveQueryErr")
	}
	return nil, fmt.Errorf("fakeConn: unexpected query %q", query)
}

// namedArg looks up a driver.NamedValue by Name (case-insensitively, as
// go-mssqldb itself treats @-prefixed parameter names), returning its
// Value and whether it was found at all. Used to prove a query this
// package builds actually carries the parameter it is supposed to,
// rather than trusting the query text's own @placeholder alone.
func namedArg(args []driver.NamedValue, name string) (driver.Value, bool) {
	for _, a := range args {
		if strings.EqualFold(a.Name, name) {
			return a.Value, true
		}
	}
	return nil, false
}

// ResetSession implements driver.SessionResetter. database/sql calls it on
// a pooled driver.Conn just before handing it out again, never on Close:
// this is the fake driver's witness for that timing. It also resets the
// simulated LOCK_TIMEOUT to -1 (SQL Server's "no limit" default), which is
// what a later read on a freshly reacquired connection must observe.
func (c *fakeConn) ResetSession(ctx context.Context) error {
	c.mu.Lock()
	c.lockTimeout = -1
	c.mu.Unlock()
	c.log("reset-after-setup")
	return nil
}

// fakeScalarRows is a driver.Rows over exactly one integer column and one
// row, enough for the scalar SELECTs session.go issues.
type fakeScalarRows struct {
	col   string
	value int64
	done  bool
}

func (r *fakeScalarRows) Columns() []string { return []string{r.col} }
func (r *fakeScalarRows) Close() error      { return nil }
func (r *fakeScalarRows) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	dest[0] = r.value
	r.done = true
	return nil
}

// fakeNullIntRows is a driver.Rows over exactly one nullable integer
// column and one row, enough to simulate HAS_PERMS_BY_NAME's single
// result - including the NULL case (driver.Value nil), which
// fakeScalarRows above has no way to produce.
type fakeNullIntRows struct {
	value sql.NullInt64
	done  bool
}

func (r *fakeNullIntRows) Columns() []string { return []string{"result"} }
func (r *fakeNullIntRows) Close() error      { return nil }
func (r *fakeNullIntRows) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	if r.value.Valid {
		dest[0] = r.value.Int64
	} else {
		dest[0] = nil
	}
	r.done = true
	return nil
}

// fakeResolveRows is a driver.Rows over Resolve's four-column
// sys.objects/sys.schemas join: object_id, schema name, object name,
// type. A nil row answers with zero rows (immediate io.EOF), simulating
// "not found or not visible" exactly as a real zero-row result set does.
type fakeResolveRows struct {
	row  *fakeResolvedRow
	done bool
}

func (r *fakeResolveRows) Columns() []string {
	return []string{"object_id", "schema_name", "object_name", "type"}
}
func (r *fakeResolveRows) Close() error { return nil }
func (r *fakeResolveRows) Next(dest []driver.Value) error {
	if r.row == nil || r.done {
		return io.EOF
	}
	dest[0] = r.row.objectID
	dest[1] = r.row.schema
	dest[2] = r.row.name
	dest[3] = r.row.typ
	r.done = true
	return nil
}

// fakeDriver only exists to satisfy driver.Connector.Driver(); Connect
// below is always reached through the connector, never through
// driver.Driver.Open.
type fakeDriver struct{}

func (d *fakeDriver) Open(name string) (driver.Conn, error) {
	return nil, errors.New("fakeDriver: Open not supported, use the connector")
}

// fakeConnector hands out a single fakeConn, simulating the one-connection
// pool the session holds for the life of a diagnostic run.
type fakeConnector struct {
	conn          *fakeConn
	connectErr    error
	connectBlocks bool // Connect blocks on ctx.Done() instead of answering
}

func (c *fakeConnector) Connect(ctx context.Context) (driver.Conn, error) {
	if c.connectBlocks {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	if c.connectErr != nil {
		return nil, c.connectErr
	}
	// Logged through the conn's own shared journal, alongside "close":
	// countEvent lets a test prove every successful connect this
	// connector vended was later matched by a driver Close, on any exit
	// path, real database/sql pooling behavior included.
	c.conn.log("connect")
	return c.conn, nil
}

func (c *fakeConnector) Driver() driver.Driver { return &fakeDriver{} }

// fakeOptions configures the fake driver stack built by newFakeDB.
type fakeOptions struct {
	major               int
	majorErr            error
	lockTimeoutOverride *int
	execErr             error
	execBlocks          bool
	closeErr            error
	connectErr          error
	connectBlocks       bool

	permsResult     *sql.NullInt64
	permsQueryErr   error
	resolveRow      *fakeResolvedRow
	resolveNoRows   bool
	resolveQueryErr error
}

// newFakeDB builds a *sql.DB backed by a fakeConn/fakeConnector pair
// configured by opts, and returns the shared event log alongside it.
func newFakeDB(opts fakeOptions) (*sql.DB, *[]string) {
	events := &[]string{}
	conn := &fakeConn{
		events:              events,
		major:               opts.major,
		majorErr:            opts.majorErr,
		lockTimeoutOverride: opts.lockTimeoutOverride,
		execErr:             opts.execErr,
		execBlocks:          opts.execBlocks,
		closeErr:            opts.closeErr,
		permsResult:         opts.permsResult,
		permsQueryErr:       opts.permsQueryErr,
		resolveRow:          opts.resolveRow,
		resolveNoRows:       opts.resolveNoRows,
		resolveQueryErr:     opts.resolveQueryErr,
	}
	connector := &fakeConnector{
		conn:          conn,
		connectErr:    opts.connectErr,
		connectBlocks: opts.connectBlocks,
	}
	return sql.OpenDB(connector), events
}

// countEvent counts how many times name appears in the driver's journal.
// Used to check that every "connect" the fake driver logged was later
// matched by a "close": a leaked pooled connection on an Open error path
// would show up as connect count exceeding close count.
func countEvent(events *[]string, name string) int {
	n := 0
	for _, e := range *events {
		if e == name {
			n++
		}
	}
	return n
}

// assertNoConnectionLeak fails the test unless every successful connect
// logged in events was matched by a driver Close, and at least one
// connect actually happened (a leak check that never saw a connect would
// pass vacuously and prove nothing).
func assertNoConnectionLeak(t *testing.T, events *[]string) {
	t.Helper()
	opens, closes := countEvent(events, "connect"), countEvent(events, "close")
	if opens == 0 {
		t.Fatal("no connect was logged: this leak check ran against a path that never opened a connection")
	}
	if opens != closes {
		t.Fatalf("connection leak: %d connect(s), %d close(s)", opens, closes)
	}
}

// openRecordedSession opens a Session via the private constructor shared
// with Open, backed by a fake driver reporting major version 16 (a plain
// success case), and returns the driver's event log.
func openRecordedSession(t *testing.T) (*Session, *[]string) {
	t.Helper()
	db, events := newFakeDB(fakeOptions{major: 16})
	s, err := open(context.Background(), db)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return s, events
}

// openFakeConn opens a Session through the fake driver configured by
// opts and returns its held *sql.Conn, registering the Session's Close
// as test cleanup. It exists for permissions_test.go and objects_test.go
// to exercise Probe, ServerProbe and Resolve's actual SQL paths -
// parameter binding, row scanning, the NULL and zero-row branches -
// without a real server: before this helper existed, those paths had no
// unit coverage at all, and a change to, say, Resolve's not-found code
// could only be caught by the integration suite under Docker. If
// opts.major is left at zero, it defaults to 16 (any supported major
// works equally for these tests; 16 needs no extra justification).
func openFakeConn(t *testing.T, opts fakeOptions) *sql.Conn {
	t.Helper()
	if opts.major == 0 {
		opts.major = 16
	}
	db, _ := newFakeDB(opts)
	s, err := open(context.Background(), db)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s.Conn
}

// sqlError is a small helper so tests can build a mssql.Error carrying a
// specific SQL Server error number without depending on more of that
// package's shape than SQLNumber classification needs.
func sqlError(number int32, message string) error {
	return mssql.Error{Number: number, Message: message}
}
