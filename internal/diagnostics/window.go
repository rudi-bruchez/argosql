package diagnostics

import (
	"fmt"
	"time"

	"github.com/rudi-bruchez/argosql/internal/model"
)

// Window is a resolved, UTC, half-open time range [Since, Until) used to
// select Query Store intervals: start_time < Until AND end_time > Since
// (design spec: "select intervals with start_time < until AND end_time >
// since"). Every "qs top"/"qs query" invocation resolves exactly one
// Window, once, before running any query.
type Window struct {
	Since, Until time.Time
}

// windowError builds the code-2 error ParseWindow returns for a
// malformed or contradictory window request.
func windowError(message string) error {
	return &model.PublicError{Code: 2, Kind: "invalid_window", Message: message}
}

// ParseWindow resolves a "qs top"/"qs query" time window from either
// hours (relative to now) or an explicit since/until RFC3339 pair -
// never both, never only one of the pair (design spec: "accept either
// --hours N ... or both --since RFC3339 --until RFC3339 ... Reject
// mixed modes, timestamp overflow, and since >= until with code 2").
// now is resolved once, in UTC, for the whole command (design spec:
// "Resolve relative time once in UTC for the entire command").
//
// internal/cli's registry already makes --hours mutually exclusive with
// --since and --until (Flag.ExclusiveWith), so by the time a real
// invocation reaches here, hours is always the registry's resolved
// value (explicit or its default) and since/until are only ever both
// empty or both explicitly given; since and until empty together means
// "use hours". This function still rejects a since/until pair with
// only one side filled, because it is called directly (bypassing that
// flag-level guard) by this package's own tests.
func ParseWindow(now time.Time, hours int, since, until string) (Window, error) {
	now = now.UTC()

	if since == "" && until == "" {
		if hours < 1 {
			return Window{}, windowError("--hours must be a positive integer")
		}
		duration := time.Duration(hours) * time.Hour
		if duration/time.Hour != time.Duration(hours) {
			return Window{}, windowError("--hours is too large: the resulting duration overflows")
		}
		return Window{Since: now.Add(-duration), Until: now}, nil
	}

	if since == "" || until == "" {
		return Window{}, windowError("--since and --until must both be given together")
	}

	sinceT, err := time.Parse(time.RFC3339, since)
	if err != nil {
		return Window{}, windowError(fmt.Sprintf("--since: %s", err.Error()))
	}
	untilT, err := time.Parse(time.RFC3339, until)
	if err != nil {
		return Window{}, windowError(fmt.Sprintf("--until: %s", err.Error()))
	}
	sinceT = sinceT.UTC()
	untilT = untilT.UTC()
	if err := validateSQLDateRange("since", sinceT); err != nil {
		return Window{}, err
	}
	if err := validateSQLDateRange("until", untilT); err != nil {
		return Window{}, err
	}
	if !sinceT.Before(untilT) {
		return Window{}, windowError("--since must be before --until")
	}
	return Window{Since: sinceT, Until: untilT}, nil
}

// validateSQLDateRange rejects a UTC-converted --since/--until that
// falls outside SQL Server's representable datetimeoffset domain:
// January 1, year 1 through December 31, year 9999. Go's time.Parse
// happily accepts an RFC3339 timestamp with any offset, and converting
// to UTC can push a value that looked fine in its own offset outside
// this range in either direction (measured: "0001-01-01T00:00:00+01:00"
// converts to UTC year 0; "9999-12-31T23:59:59-01:00" converts to UTC
// year 10000) - a bound the --hours overflow guard does not cover,
// since that guard only prevents the hours*time.Hour multiplication
// itself from overflowing a time.Duration, never checked against this
// SQL-specific range.
func validateSQLDateRange(flag string, t time.Time) error {
	if t.Year() < 1 || t.Year() > 9999 {
		return windowError(fmt.Sprintf("--%s: %s is outside SQL Server's representable date range (years 1-9999 UTC)", flag, t.Format(time.RFC3339)))
	}
	return nil
}
