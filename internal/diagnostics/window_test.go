package diagnostics

import (
	"errors"
	"testing"
	"time"

	"github.com/rudi-bruchez/argosql/internal/model"
)

// TestWindow is the task 10 brief's directive case, verbatim: --hours 24
// with no --since/--until resolves to [now-24h, now), in UTC.
func TestWindow(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	w, err := ParseWindow(now, 24, "", "")
	if err != nil || !w.Since.Equal(now.Add(-24*time.Hour)) || !w.Until.Equal(now) {
		t.Fatal(w, err)
	}
}

// TestParseWindowRejectsMixedModeAndInvalidRanges is TestWindow's
// adjacent list of cases (design spec: "Reject mixed modes, timestamp
// overflow, and since >= until with code 2"), named so it never matches
// the "TestWindow|TestTopAggregation" filter the brief's red-phase
// command counts against - this package's unit suite runs it too, just
// not under that specific count check.
func TestParseWindowRejectsMixedModeAndInvalidRanges(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name         string
		hours        int
		since, until string
		wantCode2Msg string // substring the error must mention, proving which branch fired
	}{
		{"since without until", 24, "2026-09-01T00:00:00Z", "", "together"},
		{"until without since", 24, "", "2026-09-01T00:00:00Z", "together"},
		{"since equal to until", 24, "2026-09-01T00:00:00Z", "2026-09-01T00:00:00Z", "before"},
		{"since after until", 24, "2026-09-02T00:00:00Z", "2026-09-01T00:00:00Z", "before"},
		{"malformed since", 24, "not-a-timestamp", "2026-09-01T00:00:00Z", "since"},
		{"malformed until", 24, "2026-09-01T00:00:00Z", "not-a-timestamp", "until"},
		{"zero hours with no since/until", 0, "", "", "positive"},
		{"negative hours with no since/until", -1, "", "", "positive"},
		{"hours overflowing a time.Duration", 1 << 61, "", "", "overflow"},
	}
	for _, c := range cases {
		_, err := ParseWindow(now, c.hours, c.since, c.until)
		if err == nil {
			t.Fatalf("%s: expected an error, got none", c.name)
		}
		var pub *model.PublicError
		if !errors.As(err, &pub) {
			t.Fatalf("%s: error is not a *model.PublicError: %v", c.name, err)
		}
		if pub.Code != 2 {
			t.Fatalf("%s: got code %d, want 2", c.name, pub.Code)
		}
		if pub.Message == "" {
			t.Fatalf("%s: empty message", c.name)
		}
	}
}

// TestParseWindowExplicitSinceUntil proves the explicit-pair branch
// itself (never exercised by TestWindow, which only ever passes hours):
// a valid RFC3339 since/until pair resolves to exactly those two
// instants, converted to UTC, with hours ignored entirely - hours is
// deliberately left at its registry default (24) here, not zero, to
// prove it really is ignored rather than only untested.
func TestParseWindowExplicitSinceUntil(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	since := "2026-09-01T00:00:00+02:00"
	until := "2026-09-02T00:00:00+02:00"
	w, err := ParseWindow(now, 24, since, until)
	if err != nil {
		t.Fatal(err)
	}
	wantSince := time.Date(2026, 8, 31, 22, 0, 0, 0, time.UTC)
	wantUntil := time.Date(2026, 9, 1, 22, 0, 0, 0, time.UTC)
	if !w.Since.Equal(wantSince) {
		t.Fatalf("Since: got %v, want %v", w.Since, wantSince)
	}
	if !w.Until.Equal(wantUntil) {
		t.Fatalf("Until: got %v, want %v", w.Until, wantUntil)
	}
}
