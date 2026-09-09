package diagnostics

import (
	"errors"
	"fmt"
	"slices"
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
