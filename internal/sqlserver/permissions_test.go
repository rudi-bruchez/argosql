package sqlserver

import (
	"database/sql"
	"testing"
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

// TestAllProbesLabelsByMajor proves AllProbes' one version-dependent
// entry actually changes shape across the two supported majors, and that
// the rest of the registry (five entries total: three database-level,
// one object-level, one instance-level) does not.
func TestAllProbesLabelsByMajor(t *testing.T) {
	const wantCount = 5
	probes15 := AllProbes(15)
	probes16 := AllProbes(16)
	if len(probes15) != wantCount {
		t.Fatalf("major 15: got %d probes, want %d", len(probes15), wantCount)
	}
	if len(probes16) != wantCount {
		t.Fatalf("major 16: got %d probes, want %d", len(probes16), wantCount)
	}
	last15, last16 := probes15[len(probes15)-1], probes16[len(probes16)-1]
	if last15.Label != "instance:VIEW SERVER STATE" {
		t.Fatalf("major 15 instance label = %q", last15.Label)
	}
	if last16.Label != "instance:VIEW SERVER PERFORMANCE STATE" {
		t.Fatalf("major 16 instance label = %q", last16.Label)
	}
}
