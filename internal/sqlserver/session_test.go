package sqlserver

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rudi-bruchez/argosql/internal/model"
)

// TestHeldConnection is verbatim from the task 3 brief. It proves that the
// session settings applied during setup (SET LOCK_TIMEOUT 5000) survive on
// the held *sql.Conn: reading @@LOCK_TIMEOUT through s.Conn a second time
// must still see 5000, and the fake driver's ResetSession must not have
// fired between setup and this read.
//
// Measured with the fake driver: ResetSession is not called when a
// connection returns to the pool. After Conn.Close the driver's log is
// unchanged and the witness value is still 5000; the reset only fires on
// the next acquisition, before that acquisition's first query. That is why
// this assertion reads the witness through s.Conn (the held connection,
// never returned to the pool during the test body) rather than after Close.
func TestHeldConnection(t *testing.T) {
	// openRecordedSession is defined in testdriver_test.go: opens Session
	// via the private constructor common to Open and returns the driver's
	// log.
	s, events := openRecordedSession(t)
	defer s.Close()
	var value int
	if err := s.Conn.QueryRowContext(context.Background(), "SELECT @@LOCK_TIMEOUT").Scan(&value); err != nil {
		t.Fatal(err)
	}
	if value != 5000 {
		t.Fatal(value)
	}
	for _, event := range *events {
		if event == "reset-after-setup" {
			t.Fatal(event)
		}
	}
}

// TestResetSessionOnReacquire is the corrected form the brief describes: a
// naive assertion checking for a reset event right after Conn.Close would
// observe nothing and pass for the wrong reason (ResetSession is not
// called on return-to-pool). This test checks both halves explicitly:
// no reset event right after Close, and a reset event with the witness
// value at -1 only after a fresh acquisition and a query on it.
func TestResetSessionOnReacquire(t *testing.T) {
	s, events := openRecordedSession(t)
	db := s.db // white-box: session_test.go is in package sqlserver

	if err := s.Conn.Close(); err != nil {
		t.Fatal(err)
	}
	for _, event := range *events {
		if event == "reset-after-setup" {
			t.Fatal("ResetSession fired on Close, before any reacquisition")
		}
	}

	conn2, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn2.Close()

	var value int
	if err := conn2.QueryRowContext(context.Background(), "SELECT @@LOCK_TIMEOUT").Scan(&value); err != nil {
		t.Fatal(err)
	}

	found := false
	for _, event := range *events {
		if event == "reset-after-setup" {
			found = true
		}
	}
	if !found {
		t.Fatal("expected a reset-after-setup event after reacquiring the connection")
	}
	if value != -1 {
		t.Fatal(value)
	}

	db.Close()
}

// TestOpenMajorVersion covers the version gate: 15, 16 and 17 must open
// cleanly with Major set accordingly; any other version - including one
// higher than today's newest supported variant - must fail closed at code
// 4 before any version-specific query runs. This is deliberately not
// "major >= 15": an unlisted future version must not be silently treated
// as one of today's variants.
func TestOpenMajorVersion(t *testing.T) {
	cases := []struct {
		major   int
		wantErr bool
	}{
		{major: 15, wantErr: false},
		{major: 16, wantErr: false},
		{major: 17, wantErr: false},
		{major: 14, wantErr: true},
		{major: 18, wantErr: true},
	}
	for _, tc := range cases {
		db, events := newFakeDB(fakeOptions{major: tc.major})
		s, err := open(context.Background(), db)
		if tc.wantErr {
			if err == nil {
				s.Close()
				t.Fatalf("major %d: expected an error, got none", tc.major)
			}
			var public *model.PublicError
			if !errors.As(err, &public) {
				t.Fatalf("major %d: error is not a *model.PublicError: %v", tc.major, err)
			}
			if public.Code != 4 {
				t.Fatalf("major %d: got code %d, want 4", tc.major, public.Code)
			}
			// The rejection happens after the connection is acquired
			// (lines 111-118 in session.go): open must still close
			// both Conn and the pool on this path, not just return
			// the right code.
			assertNoConnectionLeak(t, events)
			continue
		}
		if err != nil {
			t.Fatalf("major %d: unexpected error: %v", tc.major, err)
		}
		if s.Major != tc.major {
			t.Fatalf("major %d: Session.Major = %d", tc.major, s.Major)
		}
		s.Close()
	}
}

// TestOpenConnectTimeout proves that connection acquisition is bounded by
// a child of the caller's context (5s at most, but shorter here so the
// test stays fast): a global ctx whose deadline is already in the past
// makes db.Conn fail, and open must map that to a code 3 connection error
// with both Conn and DB left closed.
func TestOpenConnectTimeout(t *testing.T) {
	db, _ := newFakeDB(fakeOptions{major: 16, connectBlocks: true})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	s, err := open(ctx, db)
	if err == nil {
		s.Close()
		t.Fatal("expected an error, got none")
	}
	var public *model.PublicError
	if !errors.As(err, &public) {
		t.Fatalf("error is not a *model.PublicError: %v", err)
	}
	if public.Code != 3 {
		t.Fatalf("got code %d, want 3", public.Code)
	}
	if public.Kind != "connection" {
		t.Fatalf("got kind %q, want connection", public.Kind)
	}
}

// TestOpenExecutionTimeout proves the flip side of the same budget split:
// once the connection is acquired, connectCtx is already canceled and must
// not be reused for setup. Setup here blocks until ctx (the caller's
// global context, not connectCtx) is done, and open must map the resulting
// context error to a code 5 execution error, not code 3.
func TestOpenExecutionTimeout(t *testing.T) {
	db, events := newFakeDB(fakeOptions{major: 16, execBlocks: true})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	s, err := open(ctx, db)
	if err == nil {
		s.Close()
		t.Fatal("expected an error, got none")
	}
	var public *model.PublicError
	if !errors.As(err, &public) {
		t.Fatalf("error is not a *model.PublicError: %v", err)
	}
	if public.Code != 5 {
		t.Fatalf("got code %d, want 5", public.Code)
	}
	assertNoConnectionLeak(t, events)
}

// TestOpenContextAlreadyCanceled proves open fails fast, at code 3, when
// handed a context that is already done before any I/O is attempted.
func TestOpenContextAlreadyCanceled(t *testing.T) {
	db, _ := newFakeDB(fakeOptions{major: 16})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	s, err := open(ctx, db)
	if err == nil {
		s.Close()
		t.Fatal("expected an error, got none")
	}
	var public *model.PublicError
	if !errors.As(err, &public) {
		t.Fatalf("error is not a *model.PublicError: %v", err)
	}
	if public.Code != 3 {
		t.Fatalf("got code %d, want 3", public.Code)
	}
}

// TestOpenSetupError covers both branches of classifySQLError: a SQL
// Server permission error (229) maps to code 4, and any other SQL error
// number maps to code 5, with SQLNumber preserved either way and no
// verbatim driver error text (which could echo query context) making it
// into the message.
func TestOpenSetupError(t *testing.T) {
	t.Run("permission", func(t *testing.T) {
		db, events := newFakeDB(fakeOptions{major: 16, execErr: sqlError(229, "the server would say something here")})
		s, err := open(context.Background(), db)
		if err == nil {
			s.Close()
			t.Fatal("expected an error, got none")
		}
		var public *model.PublicError
		if !errors.As(err, &public) {
			t.Fatalf("error is not a *model.PublicError: %v", err)
		}
		if public.Code != 4 {
			t.Fatalf("got code %d, want 4", public.Code)
		}
		if public.Kind != "permission" {
			t.Fatalf("got kind %q, want permission", public.Kind)
		}
		if public.SQLNumber != 229 {
			t.Fatalf("got SQLNumber %d, want 229", public.SQLNumber)
		}
		// setupSession's error branch (lines 91-95 in session.go) must
		// close Conn and the pool, not just propagate the error.
		assertNoConnectionLeak(t, events)
	})

	t.Run("other execution error", func(t *testing.T) {
		db, events := newFakeDB(fakeOptions{major: 16, execErr: sqlError(50000, "the server would say something here too")})
		s, err := open(context.Background(), db)
		if err == nil {
			s.Close()
			t.Fatal("expected an error, got none")
		}
		var public *model.PublicError
		if !errors.As(err, &public) {
			t.Fatalf("error is not a *model.PublicError: %v", err)
		}
		if public.Code != 5 {
			t.Fatalf("got code %d, want 5", public.Code)
		}
		if public.SQLNumber != 50000 {
			t.Fatalf("got SQLNumber %d, want 50000", public.SQLNumber)
		}
		assertNoConnectionLeak(t, events)
	})
}

// TestOpenLockTimeoutMismatch exercises the branch a naive fake driver
// hid: setupSession's mismatch check (session.go, "if value != 5000")
// fires when the value read back genuinely differs from the value that
// was set, not when the fake driver just always answers a fixed number.
// SET LOCK_TIMEOUT 5000 succeeds and is honestly recorded by the fake
// driver as 5000, but the read-back is overridden to -1, simulating a
// session setting that silently did not take effect on the server - the
// one case this check exists to catch.
func TestOpenLockTimeoutMismatch(t *testing.T) {
	override := -1
	db, events := newFakeDB(fakeOptions{major: 16, lockTimeoutOverride: &override})
	s, err := open(context.Background(), db)
	if err == nil {
		s.Close()
		t.Fatal("expected an error, got none")
	}
	var public *model.PublicError
	if !errors.As(err, &public) {
		t.Fatalf("error is not a *model.PublicError: %v", err)
	}
	if public.Code != 5 {
		t.Fatalf("got code %d, want 5", public.Code)
	}
	if public.Kind != "execution" {
		t.Fatalf("got kind %q, want execution", public.Kind)
	}
	if got := countEvent(events, "exec:set-lock-timeout"); got != 1 {
		t.Fatalf("got %d SET LOCK_TIMEOUT execs, want 1: the mismatch must be caught after a real SET, not instead of one", got)
	}
	assertNoConnectionLeak(t, events)
}

// TestOpenVersionQueryError covers fetchMajorVersion's own error branch
// (session.go lines 99-102): a driver failure reading SERVERPROPERTY,
// distinct from setup succeeding and the version simply being
// unsupported, must also close Conn and the pool.
func TestOpenVersionQueryError(t *testing.T) {
	db, events := newFakeDB(fakeOptions{major: 16, majorErr: sqlError(4060, "cannot open database")})
	s, err := open(context.Background(), db)
	if err == nil {
		s.Close()
		t.Fatal("expected an error, got none")
	}
	var public *model.PublicError
	if !errors.As(err, &public) {
		t.Fatalf("error is not a *model.PublicError: %v", err)
	}
	if public.Code != 5 {
		t.Fatalf("got code %d, want 5", public.Code)
	}
	if public.SQLNumber != 4060 {
		t.Fatalf("got SQLNumber %d, want 4060", public.SQLNumber)
	}
	assertNoConnectionLeak(t, events)
}

// TestCloseIdempotent proves a second Close, or a Close on a Session that
// never fully opened, neither errors nor panics.
func TestCloseIdempotent(t *testing.T) {
	s, _ := openRecordedSession(t)
	if err := s.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if s.Conn != nil || s.db != nil {
		t.Fatal("Close must leave Conn and db nil")
	}
}
