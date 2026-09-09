package sqlserver

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/rudi-bruchez/argosql/internal/model"
)

// TestUnknownIsNotDenied is verbatim from the task 8 brief. It proves
// nothing about the engine by itself - it is a pure conversion test on
// fabricated values - but it pins down the one conversion every other
// guarantee in this package depends on: a NULL from HAS_PERMS_BY_NAME
// must never collapse into Denied. The test that actually proves the
// engine behaves this way is TestEveryProbeIsWellFormed, in
// tests/integration/permissions_test.go, which runs every probe this
// package's registry emits against a real server.
func TestUnknownIsNotDenied(t *testing.T) {
	if permissionFromSQL(sql.NullInt64{}) != Unknown {
		t.Fatal("NULL lost")
	}
	if permissionFromSQL(sql.NullInt64{Int64: 0, Valid: true}) != Denied {
		t.Fatal("zero")
	}
}

// TestPermissionFromSQLAllowed rounds out permissionFromSQL's three
// branches: TestUnknownIsNotDenied above only pins NULL and zero: this
// covers the nonzero branch those two leave unchecked.
func TestPermissionFromSQLAllowed(t *testing.T) {
	if got := permissionFromSQL(sql.NullInt64{Int64: 1, Valid: true}); got != Allowed {
		t.Fatalf("got %v, want Allowed", got)
	}
}

// TestInstanceStatePermission pins the version split AllProbes relies on:
// major 15 gets the older VIEW SERVER STATE, everything from 16 up gets
// the narrower VIEW SERVER PERFORMANCE STATE the design spec prefers.
func TestInstanceStatePermission(t *testing.T) {
	cases := []struct {
		major int
		want  string
	}{
		{major: 15, want: "VIEW SERVER STATE"},
		{major: 16, want: "VIEW SERVER PERFORMANCE STATE"},
		{major: 17, want: "VIEW SERVER PERFORMANCE STATE"},
	}
	for _, tc := range cases {
		if got := instanceStatePermission(tc.major); got != tc.want {
			t.Fatalf("major %d: got %q, want %q", tc.major, got, tc.want)
		}
	}
}

// TestAllProbesLabelsByMajor proves AllProbes has two different shapes,
// not one: five entries on Major 15 (three database-level, one
// object-level, one instance-level), and seven on Major 16 and later -
// the same five plus the two 2022-only granular permissions (VIEW
// DATABASE PERFORMANCE STATE, VIEW SECURITY DEFINITION) the design spec
// requires on that version (line 149). It also pins the one entry that
// changes permission rather than merely appearing or not: the instance
// probe's label.
func TestAllProbesLabelsByMajor(t *testing.T) {
	probes15 := AllProbes(15)
	probes16 := AllProbes(16)
	if len(probes15) != 5 {
		t.Fatalf("major 15: got %d probes, want 5", len(probes15))
	}
	if len(probes16) != 7 {
		t.Fatalf("major 16: got %d probes, want 7", len(probes16))
	}

	wantLabels16 := map[string]bool{
		"database:VIEW DATABASE PERFORMANCE STATE": false,
		"database:VIEW SECURITY DEFINITION":        false,
	}
	for _, pr := range probes16 {
		if _, ok := wantLabels16[pr.Label]; ok {
			wantLabels16[pr.Label] = true
		}
	}
	for label, found := range wantLabels16 {
		if !found {
			t.Fatalf("major 16: missing probe %q", label)
		}
	}
	for _, pr := range probes15 {
		if pr.Label == "database:VIEW DATABASE PERFORMANCE STATE" || pr.Label == "database:VIEW SECURITY DEFINITION" {
			t.Fatalf("major 15: got %q, which does not exist as a grantable permission before major 16", pr.Label)
		}
	}

	last15, last16 := probes15[len(probes15)-1], probes16[len(probes16)-1]
	if last15.Label != "instance:VIEW SERVER STATE" {
		t.Fatalf("major 15 instance label = %q", last15.Label)
	}
	if last16.Label != "instance:VIEW SERVER PERFORMANCE STATE" {
		t.Fatalf("major 16 instance label = %q", last16.Label)
	}
}

// TestProbeMalformedIsCode5AndKind is the test that carries this
// package's central guarantee, at the unit level: Probe must never
// surface Unknown as a bare result. A reviewer proved this guarantee was
// previously unverified by changing Probe's malformed-probe error from
// Code 5/Kind "probe_malformed" to Code 7/Kind "collection_limit_reached"
// and finding every test, unit and integration, still green - because
// the only negative case that existed (TestEveryProbeIsWellFormed's
// deliberate SERVER-class probe) checked err != nil and nothing more.
// This test checks both model.ExitCode(err) and the PublicError's Kind
// explicitly, using the fake driver (see openFakeConn in
// testdriver_test.go) rather than a real server, so it runs in
// milliseconds and needs no Docker.
func TestProbeMalformedIsCode5AndKind(t *testing.T) {
	null := sql.NullInt64{}
	conn := openFakeConn(t, fakeOptions{permsResult: &null})

	perm, err := Probe(context.Background(), conn, "dbo.Orders", "OBJECT", "NOT A REAL PERMISSION")
	if perm != Unknown {
		t.Fatalf("got %v, want Unknown", perm)
	}
	if err == nil {
		t.Fatal("expected a non-nil error alongside Unknown")
	}
	if got := model.ExitCode(err); got != 5 {
		t.Fatalf("model.ExitCode(err) = %d, want 5", got)
	}
	var public *model.PublicError
	if !errors.As(err, &public) {
		t.Fatalf("error is not a *model.PublicError: %v", err)
	}
	if public.Kind != "probe_malformed" {
		t.Fatalf("Kind = %q, want probe_malformed", public.Kind)
	}
}

// TestServerProbeMalformedIsCode5AndKind is TestProbeMalformedIsCode5AndKind's
// counterpart for ServerProbe, which builds its own *model.PublicError
// independently of Probe's (see serverProbeQuery's call site) and so
// needs its own proof that the same guarantee holds there too.
func TestServerProbeMalformedIsCode5AndKind(t *testing.T) {
	null := sql.NullInt64{}
	conn := openFakeConn(t, fakeOptions{permsResult: &null})

	perm, err := ServerProbe(context.Background(), conn, "NOT A REAL PERMISSION")
	if perm != Unknown {
		t.Fatalf("got %v, want Unknown", perm)
	}
	if err == nil {
		t.Fatal("expected a non-nil error alongside Unknown")
	}
	if got := model.ExitCode(err); got != 5 {
		t.Fatalf("model.ExitCode(err) = %d, want 5", got)
	}
	var public *model.PublicError
	if !errors.As(err, &public) {
		t.Fatalf("error is not a *model.PublicError: %v", err)
	}
	if public.Kind != "probe_malformed" {
		t.Fatalf("Kind = %q, want probe_malformed", public.Kind)
	}
}

// TestProbeAllowedAndDenied exercises Probe's SQL path end to end
// against the fake driver - parameter binding and row scanning included -
// for both non-malformed outcomes, a gap a reviewer found: before this
// test existed, only Resolve's and Probe's pure helper functions
// (permissionFromSQL, splitTwoPart) and the Docker-backed integration
// suite ever ran this code at all.
func TestProbeAllowedAndDenied(t *testing.T) {
	allowed := sql.NullInt64{Int64: 1, Valid: true}
	conn := openFakeConn(t, fakeOptions{permsResult: &allowed})
	perm, err := Probe(context.Background(), conn, "dbo.Orders", "OBJECT", "SELECT")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if perm != Allowed {
		t.Fatalf("got %v, want Allowed", perm)
	}

	denied := sql.NullInt64{Int64: 0, Valid: true}
	conn = openFakeConn(t, fakeOptions{permsResult: &denied})
	perm, err = Probe(context.Background(), conn, "dbo.Orders", "OBJECT", "SELECT")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if perm != Denied {
		t.Fatalf("got %v, want Denied", perm)
	}
}

// TestServerProbeAllowedAndDenied is TestProbeAllowedAndDenied's
// counterpart for ServerProbe's own, separate SQL path.
func TestServerProbeAllowedAndDenied(t *testing.T) {
	allowed := sql.NullInt64{Int64: 1, Valid: true}
	conn := openFakeConn(t, fakeOptions{permsResult: &allowed})
	perm, err := ServerProbe(context.Background(), conn, "VIEW SERVER STATE")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if perm != Allowed {
		t.Fatalf("got %v, want Allowed", perm)
	}

	denied := sql.NullInt64{Int64: 0, Valid: true}
	conn = openFakeConn(t, fakeOptions{permsResult: &denied})
	perm, err = ServerProbe(context.Background(), conn, "VIEW SERVER STATE")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if perm != Denied {
		t.Fatalf("got %v, want Denied", perm)
	}
}
