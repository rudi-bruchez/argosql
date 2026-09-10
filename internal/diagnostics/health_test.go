package diagnostics

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/rudi-bruchez/argosql/internal/model"
)

// TestReasonZero is the task 9 brief's directive case, verbatim.
func TestReasonZero(t *testing.T) {
	got := DecodeReadOnly(Health{Desired: "READ_WRITE", Actual: "READ_WRITE"})
	if len(got) != 1 || got[0] != "none" {
		t.Fatal(got)
	}
}

// TestReasonZeroConfiguredReadOnly is TestReasonZero's sibling case
// from the same design spec sentence: zero plus desired=READ_ONLY and
// actual=READ_ONLY means configured_read_only, not none - a test that
// only ever exercised the READ_WRITE branch could pass with
// configured_read_only silently broken (for example, swapped for
// "none") and never notice.
func TestReasonZeroConfiguredReadOnly(t *testing.T) {
	got := DecodeReadOnly(Health{Desired: "READ_ONLY", Actual: "READ_ONLY"})
	if len(got) != 1 || got[0] != "configured_read_only" {
		t.Fatalf("got %v, want [configured_read_only]", got)
	}
}

// TestReasonZeroOtherState is the design spec's third zero-reason
// case: any state neither of the other two branches recognizes (OFF,
// ERROR, READ_CAPTURE_SECONDARY, or a desired/actual combination
// neither covers) reports no_reason_reported, never an invented
// "none" or "configured_read_only".
func TestReasonZeroOtherState(t *testing.T) {
	cases := []struct {
		name            string
		desired, actual string
	}{
		{"off", "READ_WRITE", "OFF"},
		{"error", "READ_WRITE", "ERROR"},
		{"read_only_actual_but_desired_read_write", "READ_WRITE", "READ_ONLY"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := DecodeReadOnly(Health{Desired: c.desired, Actual: c.actual})
			if len(got) != 1 || got[0] != "no_reason_reported" {
				t.Fatalf("got %v, want [no_reason_reported]", got)
			}
		})
	}
}

// TestDecodeReadOnlyKnownBits exercises every bit value documented for
// sys.database_query_store_options.readonly_reason (see health.go's
// knownReadOnlyBits for the source), one at a time, plus a combination
// of two - proving DecodeReadOnly reports every set bit, not just the
// lowest one.
func TestDecodeReadOnlyKnownBits(t *testing.T) {
	cases := []struct {
		reason int64
		want   []string
	}{
		{1, []string{"database_read_only"}},
		{2, []string{"database_single_user"}},
		{4, []string{"database_emergency_mode"}},
		{8, []string{"secondary_replica"}},
		{65536, []string{"storage_limit_reached"}},
		{131072, []string{"statement_count_limit_reached"}},
		{262144, []string{"memory_limit_reached"}},
		{524288, []string{"disk_size_limit_reached"}},
		{1 | 65536, []string{"database_read_only", "storage_limit_reached"}},
	}
	for _, c := range cases {
		t.Run(fmt.Sprintf("reason=%d", c.reason), func(t *testing.T) {
			got := DecodeReadOnly(Health{ReadOnlyReason: c.reason})
			if !slices.Equal(got, c.want) {
				t.Fatalf("got %v, want %v", got, c.want)
			}
		})
	}
}

// TestDecodeReadOnlyUnknownBit is the brief's other mandatory case: a
// bit this program's knownReadOnlyBits table does not document must
// still be reported, numerically, never silently dropped - the design
// spec's "expose unknown bits numerically" applied to a bit that will
// arrive one day from a server build newer than this table.
func TestDecodeReadOnlyUnknownBit(t *testing.T) {
	got := DecodeReadOnly(Health{ReadOnlyReason: 16})
	want := []string{"unknown_reason_bit_16"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// TestDecodeReadOnlyKnownAndUnknownMixed proves a known and an unknown
// bit set together are both reported, each by its own correct form -
// the case a decoder that stops at its first unrecognized bit (or that
// only ever reports one reason) would get wrong.
func TestDecodeReadOnlyKnownAndUnknownMixed(t *testing.T) {
	got := DecodeReadOnly(Health{ReadOnlyReason: 2 | 32})
	want := []string{"database_single_user", "unknown_reason_bit_32"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// TestHealthFromCellsRejectsWrongShape proves healthFromCells refuses
// a malformed cells slice (the wrong Go type for one of the four
// fields it reads) rather than panicking or silently zero-valuing the
// field: a defect in this package's own query or in ScanRow's
// conversion must surface as a code-5 *model.PublicError, not a wrong
// Health value a caller cannot tell from a real zero.
func TestHealthFromCellsRejectsWrongShape(t *testing.T) {
	cells := make([]model.Cell, colHasHistory+1)
	cells[colDesired] = "READ_WRITE"
	cells[colActual] = "READ_WRITE"
	cells[colReadOnlyReason] = "0" // wrong type: string instead of int64
	cells[colHasHistory] = false

	_, err := healthFromCells(cells)
	if err == nil {
		t.Fatal("expected an error for a malformed readonly_reason cell, got none")
	}
	var pub *model.PublicError
	if !errors.As(err, &pub) {
		t.Fatalf("error is not a *model.PublicError: %v", err)
	}
	if pub.Code != 5 {
		t.Fatalf("got code %d, want 5", pub.Code)
	}
}

// TestHealthFromCellsAcceptsErrorState covers the one state the design
// spec names (line 83) as a required code-0 success alongside OFF and
// READ_ONLY that no test before this one ever fed healthFromCells: no
// disposable container reaches sys.database_query_store_options'
// actual_state_desc = ERROR reliably (it marks internal corruption of
// Query Store's own data), so this fixtures the state at the
// healthFromCells boundary instead, per the design spec's own
// instruction to "fixture the otherwise nondeterministic ERROR state at
// the backend boundary". healthFromCells does not special-case
// actual_state's text at all - it only type-asserts each cell - so
// there is no reason to expect it to reject this row, and this proves
// it does not; DecodeReadOnly on the resulting Health falls through to
// its existing no_reason_reported branch (ERROR matches neither the
// READ_ONLY/READ_ONLY nor the READ_WRITE case), already covered for
// readonly_reason=0 by TestReasonZeroOtherState's "error" case above -
// this test instead reaches that same outcome through healthFromCells
// itself, not through a Health literal this file constructs by hand.
func TestHealthFromCellsAcceptsErrorState(t *testing.T) {
	cells := make([]model.Cell, colHasHistory+1)
	cells[colDesired] = "READ_WRITE"
	cells[colActual] = "ERROR"
	cells[colReadOnlyReason] = int64(0)
	cells[colCaptureMode] = "ALL"
	cells[colHasHistory] = false

	h, err := healthFromCells(cells)
	if err != nil {
		t.Fatalf("healthFromCells on an ERROR row: %v", err)
	}
	if h.Actual != "ERROR" {
		t.Fatalf("Actual: got %q, want ERROR", h.Actual)
	}
	got := DecodeReadOnly(h)
	want := []string{"no_reason_reported"}
	if !slices.Equal(got, want) {
		t.Fatalf("DecodeReadOnly on ERROR with readonly_reason=0: got %v, want %v", got, want)
	}
}

// TestStatusAcceptsErrorStateWithHistory is fix 1's A5: every other
// ERROR-state test in this file (TestHealthFromCellsAcceptsErrorState
// above) calls healthFromCells directly, a pure cell-shape converter -
// never Status itself through a real model.Sink/backend boundary, the
// gap the reviewer named explicitly. "ERROR avec historique" (as
// opposed to the already-covered no-history case) is exercised here
// through the SAME fake-driver discipline this package's own
// CLAUDE.md-recorded rule requires for an engine state that cannot be
// forced by real DDL ("ERROR et propriétés inconnues via backend de
// test, pas corruption de base"): a fake backend answers health.sql's
// one row with actual_state=ERROR and has_history=true, and Status
// must still produce a complete "status"/"coverage" pair rather than
// erroring out or panicking on a state it cannot itself provoke.
func TestStatusAcceptsErrorStateWithHistory(t *testing.T) {
	// fakeHealthRows (top_test.go, same package) already builds a
	// correctly-typed health.sql row (decimal cells as []byte digit
	// strings, the shape output.ScanRow actually requires) - reused
	// here rather than a second, independently-typed literal.
	conn := &fakeObjConn{responses: []objQueryResponse{
		{
			match:  func(q string) bool { return strings.Contains(q, "database_query_store_options") },
			handle: func(args []driver.NamedValue) (driver.Rows, error) { return &fakeHealthRows{actual: "ERROR"}, nil },
		},
		{
			match:  func(q string) bool { return strings.Contains(q, "query_store_runtime_stats_interval") },
			handle: func(args []driver.NamedValue) (driver.Rows, error) { return &fakeCoverageRows{}, nil },
		},
	}}
	sess := newFakeObjSession(t, conn)
	sink := &objCaptureSink{}

	if err := Status(context.Background(), sess, sink); err != nil {
		t.Fatalf("Status on an ERROR/has_history row: %v", err)
	}
	status := sink.table("status")
	if status == nil || len(status.rows) != 1 {
		t.Fatalf("expected one status row, got %+v", status)
	}
	// Index 1: StatusTable's own column order (desired_state,
	// actual_state, ...) - distinct from colActual, which indexes
	// health.sql's QUERY result, a differently-shaped row (no
	// readonly_reason_decoded column there; Status computes that one
	// itself).
	if got := status.rows[0][1]; got != "ERROR" {
		t.Fatalf("status.actual_state: got %#v, want ERROR", got)
	}
	coverage := sink.table("coverage")
	if coverage == nil || len(coverage.rows) != 1 {
		t.Fatalf("expected one coverage row, got %+v", coverage)
	}
}
