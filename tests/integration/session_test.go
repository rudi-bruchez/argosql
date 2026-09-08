//go:build integration

// Package integration runs argosql's diagnostics against real, disposable
// SQL Server containers. Every test in this package requires the
// "integration" build tag and, at minimum, ASQ_TEST_IMAGE naming the image
// to run; without either, a test in this package must fail explicitly
// rather than skip silently (see NewLab in podman_test.go).
package integration

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"
	"time"

	mssql "github.com/microsoft/go-mssqldb"
	"github.com/rudi-bruchez/argosql/internal/sqlserver"
)

// TestSessionTLS is verbatim from the task 4 brief. It proves two things at
// once about the session sqlserver.Open holds: the LOCK_TIMEOUT setting from
// setup survives on the held connection (@@LOCK_TIMEOUT reads back 5000),
// and the connection that setting lives on is actually encrypted, as seen
// from the server side (sys.dm_exec_connections.encrypt_option for that
// session's own @@SPID), not merely assumed from the client's connection
// string.
func TestSessionTLS(t *testing.T) {
	lab := NewLab(t, os.Getenv("ASQ_TEST_IMAGE"))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, err := sqlserver.Open(ctx, lab.Profile)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	logEngineIdentity(t, lab, s.Major)
	var lock, spid int
	if err := s.Conn.QueryRowContext(ctx, "SELECT @@LOCK_TIMEOUT, @@SPID").Scan(&lock, &spid); err != nil {
		t.Fatal(err)
	}
	var encrypted string
	if err := lab.Admin.QueryRowContext(ctx, "SELECT encrypt_option FROM sys.dm_exec_connections WHERE session_id=@p1", spid).Scan(&encrypted); err != nil {
		t.Fatal(err)
	}
	if lock != 5000 || encrypted != "TRUE" {
		t.Fatalf("lock=%d encrypted=%s", lock, encrypted)
	}
}

// TestSessionLockConflict proves two distinct effects of the held
// session's SET LOCK_TIMEOUT 5000: a real conflicting lock held by another
// connection makes a statement wait out that setting and fail with SQL
// error 1222 (lock request timeout), and a caller context whose own
// deadline is shorter than 5s interrupts the wait earlier still - the
// global per-call budget bounds the block, not just the session setting.
func TestSessionLockConflict(t *testing.T) {
	lab := NewLab(t, os.Getenv("ASQ_TEST_IMAGE"))

	t.Run("server lock timeout", func(t *testing.T) {
		openCtx, openCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer openCancel()
		s, err := sqlserver.Open(openCtx, lab.Profile)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		logEngineIdentity(t, lab, s.Major)

		_, release := holdRowLock(t, lab)
		defer release()

		queryCtx, queryCancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer queryCancel()
		start := time.Now()
		_, err = s.Conn.ExecContext(queryCtx, "UPDATE dbo.Widgets SET Quantity = Quantity + 1 WHERE Id = 1")
		elapsed := time.Since(start)
		if err == nil {
			t.Fatal("expected the conflicting update to fail with a lock timeout, got no error")
		}
		var sqlErr mssql.Error
		if !errors.As(err, &sqlErr) {
			t.Fatalf("error is not a *mssql.Error: %v", err)
		}
		if sqlErr.Number != 1222 {
			t.Fatalf("got SQL error %d, want 1222 (lock request timeout): %v", sqlErr.Number, err)
		}
		// ~5s from SET LOCK_TIMEOUT 5000, with tolerance for a slow CI
		// runner; far outside this band means something other than the
		// session's own LOCK_TIMEOUT fired.
		if elapsed < 3*time.Second || elapsed > 15*time.Second {
			t.Fatalf("lock wait took %s, want roughly 5s (tolerance 3-15s)", elapsed)
		}
	})

	t.Run("context deadline interrupts the wait", func(t *testing.T) {
		openCtx, openCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer openCancel()
		s, err := sqlserver.Open(openCtx, lab.Profile)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		logEngineIdentity(t, lab, s.Major)

		_, release := holdRowLock(t, lab)
		defer release()

		queryCtx, queryCancel := context.WithTimeout(context.Background(), 1*time.Second)
		defer queryCancel()
		start := time.Now()
		_, err = s.Conn.ExecContext(queryCtx, "UPDATE dbo.Widgets SET Quantity = Quantity + 1 WHERE Id = 1")
		elapsed := time.Since(start)
		if err == nil {
			t.Fatal("expected the conflicting update to fail, got no error")
		}
		// Not just "an error, quickly": a query that fails instantly for
		// any other reason (a dropped connection, a typo'd table name)
		// would also produce a non-nil error well under 4s, and this
		// subtest would then pass while asserting the deadline interrupted
		// a wait it never actually affected. errors.Is pins the failure to
		// context cancellation specifically.
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("error is not context.DeadlineExceeded: %v", err)
		}
		// The server's own LOCK_TIMEOUT is 5s; a 1s caller deadline must
		// win well before that.
		if elapsed > 4*time.Second {
			t.Fatalf("context deadline of 1s did not interrupt the wait: took %s", elapsed)
		}
	})
}

// holdRowLock acquires a dedicated connection out of lab.Admin's pool,
// starts a transaction that updates dbo.Widgets row Id=1 without
// committing, and returns that connection together with a release func
// that rolls back and closes it. The exclusive row lock outlives the call
// that creates it; callers must defer release().
func holdRowLock(t *testing.T, lab *Lab) (*sql.Conn, func()) {
	t.Helper()
	ctx := context.Background()
	conn, err := lab.Admin.Conn(ctx)
	if err != nil {
		t.Fatalf("acquiring admin connection to hold a lock: %v", err)
	}
	if _, err := conn.ExecContext(ctx, "BEGIN TRANSACTION"); err != nil {
		conn.Close()
		t.Fatalf("BEGIN TRANSACTION: %v", err)
	}
	if _, err := conn.ExecContext(ctx, "UPDATE dbo.Widgets SET Quantity = Quantity WHERE Id = 1"); err != nil {
		conn.ExecContext(ctx, "ROLLBACK")
		conn.Close()
		t.Fatalf("acquiring row lock: %v", err)
	}
	return conn, func() {
		conn.ExecContext(context.Background(), "ROLLBACK")
		conn.Close()
	}
}
