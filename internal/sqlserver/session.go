// Package sqlserver opens and holds the single SQL Server session every
// diagnostic in this program runs against. Session settings applied at
// setup time (LOCK_TIMEOUT) must survive for the life of the run, which
// means the same *sql.Conn has to stay checked out of the pool from Open
// to Close: returning it to the pool between diagnostics would let
// database/sql reset it on the next acquisition (see ResetSession in
// testdriver_test.go), silently losing the setting.
package sqlserver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/rudi-bruchez/argosql/internal/config"
	"github.com/rudi-bruchez/argosql/internal/model"

	// Registers the "sqlserver" database/sql driver used by Open, and
	// gives this package the mssql.Error type used to classify SQL
	// Server errors by number below.
	mssql "github.com/microsoft/go-mssqldb"
)

// connectTimeout bounds acquiring the pool's single connection. It is part
// of the caller's global budget, not on top of it: connectCtx below is
// derived from the caller's ctx, so a global deadline that is already
// closer than 5s wins.
const connectTimeout = 5 * time.Second

const (
	setLockTimeoutStatement = "SET LOCK_TIMEOUT 5000"
	selectLockTimeoutQuery  = "SELECT @@LOCK_TIMEOUT"
	selectMajorVersionQuery = "SELECT CAST(SERVERPROPERTY('ProductMajorVersion') AS int)"
)

// sqlErrorPermission lists the SQL Server error numbers that mean
// "connected fine, but not allowed to do this" rather than an execution
// failure: they map to exit code 4 instead of 5.
var sqlErrorPermission = map[int32]bool{229: true, 300: true}

// Session is the single held connection every diagnostic in a run queries
// through. Major is the server's major version, read once during setup;
// it is what later tasks use to pick a SQL variant. The pool behind Conn
// is private: nothing outside this package may reach for a second
// connection out of it.
type Session struct {
	Conn  *sql.Conn
	Major int

	db *sql.DB // pool privé : une seule connexion en est jamais tirée
}

// Open builds p's connection string, opens a pool against it, acquires and
// holds its one connection, and runs session setup (LOCK_TIMEOUT) and
// version detection on it. ctx is the caller's global diagnostic context:
// Open keeps it for setup and version detection, and only ever derives a
// bounded child of it for the connection acquisition itself.
func Open(ctx context.Context, p config.Profile) (*Session, error) {
	dsn, err := config.DSN(p)
	if err != nil {
		// DSN's own contract forbids ever printing its result; err here
		// must not carry it either, so this is not wrapped verbatim.
		return nil, fmt.Errorf("build connection string: %w", err)
	}
	db, err := sql.Open("sqlserver", dsn)
	if err != nil {
		return nil, &model.PublicError{Code: 3, Kind: "connection", Message: "connection failed"}
	}
	return open(ctx, db)
}

// open is the constructor shared by Open and the tests in this package:
// it does all the work once a *sql.DB pool exists, real or fake. Tests
// reach it directly (see openRecordedSession in testdriver_test.go) to
// exercise setup and version detection against a fake driver without
// going through config.Profile or a real network connection.
func open(ctx context.Context, db *sql.DB) (*Session, error) {
	connectCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	conn, err := db.Conn(connectCtx)
	cancel()
	if err != nil {
		db.Close()
		return nil, &model.PublicError{Code: 3, Kind: "connection", Message: "connection failed"}
	}
	// From here on, setup and version detection use ctx (the caller's
	// global budget), never connectCtx: connectCtx is already canceled
	// above, and reusing it would fail every subsequent call outright.

	if err := setupSession(ctx, conn); err != nil {
		conn.Close()
		db.Close()
		return nil, err
	}

	major, err := fetchMajorVersion(ctx, conn)
	if err != nil {
		conn.Close()
		db.Close()
		return nil, err
	}

	// Deliberately an explicit allow-list, not "major >= 15": a future
	// major version must fail closed at code 4 until this package
	// declares it supported, not be silently treated as one of today's
	// variants.
	switch major {
	case 15, 16, 17:
	default:
		conn.Close()
		db.Close()
		return nil, &model.PublicError{
			Code:    4,
			Kind:    "unsupported_version",
			Message: fmt.Sprintf("unsupported SQL Server major version %d", major),
		}
	}

	return &Session{Conn: conn, Major: major, db: db}, nil
}

// setupSession applies and verifies the session-wide settings every
// diagnostic on this connection relies on.
func setupSession(ctx context.Context, conn *sql.Conn) error {
	if _, err := conn.ExecContext(ctx, setLockTimeoutStatement); err != nil {
		return classifySQLError(err, "failed to set session lock timeout")
	}

	var value int
	if err := conn.QueryRowContext(ctx, selectLockTimeoutQuery).Scan(&value); err != nil {
		return classifySQLError(err, "failed to verify session lock timeout")
	}
	if value != 5000 {
		return &model.PublicError{Code: 5, Kind: "execution", Message: "session lock timeout did not take effect"}
	}
	return nil
}

// fetchMajorVersion reads the server's major version via SERVERPROPERTY.
func fetchMajorVersion(ctx context.Context, conn *sql.Conn) (int, error) {
	var major int
	if err := conn.QueryRowContext(ctx, selectMajorVersionQuery).Scan(&major); err != nil {
		return 0, classifySQLError(err, "failed to read server version")
	}
	return major, nil
}

// classifySQLError turns a driver error into a *model.PublicError. It
// never wraps the driver's error verbatim: that error's text can echo
// back query context this package must not surface, so it is reformulated
// from message, keeping only the SQL Server error number when there is
// one. Errors 229 and 300 mean "connected, but not permitted" and map to
// exit code 4 only here, after a connection already exists; a connection
// failure before this point is always exit code 3, never this path.
func classifySQLError(err error, message string) error {
	var sqlErr mssql.Error
	if errors.As(err, &sqlErr) {
		if sqlErrorPermission[sqlErr.Number] {
			return &model.PublicError{Code: 4, Kind: "permission", Message: "insufficient permission", SQLNumber: sqlErr.Number}
		}
		return &model.PublicError{Code: 5, Kind: "execution", Message: message, SQLNumber: sqlErr.Number}
	}
	return &model.PublicError{Code: 5, Kind: "execution", Message: message}
}

// Close releases the held connection and its pool. It is idempotent:
// calling it more than once, or on a Session left partially closed by a
// failed Open, never errors or panics.
func (s *Session) Close() error {
	// errors.Join drops nil arguments, so the two closes need no slice and
	// no append: a successful close contributes nothing to the result, and
	// both failing yields both errors.
	var connErr, dbErr error
	if s.Conn != nil {
		connErr = s.Conn.Close()
		s.Conn = nil
	}
	if s.db != nil {
		dbErr = s.db.Close()
		s.db = nil
	}
	return errors.Join(connErr, dbErr)
}
