package cli

import (
	"bytes"
	"context"
	"database/sql/driver"
	"errors"
	"testing"

	"github.com/rudi-bruchez/argosql/internal/config"
	"github.com/rudi-bruchez/argosql/internal/model"
	"github.com/rudi-bruchez/argosql/internal/sqlserver"
)

// TestIdxMissingTopDefault is dispatch cassure 4's own target: --top
// defaults to 10 (design spec: "Both ranking commands accept --top
// from 1 to 100" and "idx missing": "Default top 10 suggestions").
func TestIdxMissingTopDefault(t *testing.T) {
	req, cmd, err := Parse([]string{"--ctx", "client", "idx", "missing"})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cmd == nil || cmd.Name != "idx missing" {
		t.Fatalf("Parse: command = %#v, want idx missing", cmd)
	}
	if req.Top != 10 {
		t.Fatalf("idx missing with no --top: got %d, want default 10", req.Top)
	}
}

// TestIdxMissingTopBounds is dispatch cassure 4's other half: --top 0
// and --top 101 are both out of [1, 100] and yield code 2, validated
// before any connection opens.
func TestIdxMissingTopBounds(t *testing.T) {
	cases := []string{"0", "101"}
	for _, v := range cases {
		t.Run("--top "+v, func(t *testing.T) {
			_, _, err := Parse([]string{"--ctx", "client", "idx", "missing", "--top", v})
			var pub *model.PublicError
			if !errors.As(err, &pub) {
				t.Fatalf("idx missing --top %s: want *model.PublicError, got %#v", v, err)
			}
			if pub.Code != 2 {
				t.Fatalf("idx missing --top %s: want code 2, got %d", v, pub.Code)
			}
		})
	}
}

// TestIdxMissingTopWithinBounds proves --top 1 and --top 100 - the
// two boundary values the range [1, 100] itself admits - are accepted,
// not merely that values outside it are rejected: a bound that was
// quietly narrowed to, say, [2, 99] would still pass
// TestIdxMissingTopBounds above.
func TestIdxMissingTopWithinBounds(t *testing.T) {
	cases := map[string]int{"1": 1, "100": 100}
	for v, want := range cases {
		t.Run("--top "+v, func(t *testing.T) {
			req, _, err := Parse([]string{"--ctx", "client", "idx", "missing", "--top", v})
			if err != nil {
				t.Fatalf("idx missing --top %s: %v", v, err)
			}
			if req.Top != want {
				t.Fatalf("idx missing --top %s: got %d, want %d", v, req.Top, want)
			}
		})
	}
}

// TestIdxMissingTableFlagValidatedBeforeConnection is this command's
// own analogue of --object's pre-connection syntax check: a malformed
// --table name (not two parts) fails at code 2 before Parse returns,
// never reaching a connection.
func TestIdxMissingTableFlagValidatedBeforeConnection(t *testing.T) {
	_, _, err := Parse([]string{"--ctx", "client", "idx", "missing", "--table", "dbo"})
	var pub *model.PublicError
	if !errors.As(err, &pub) {
		t.Fatalf("idx missing --table dbo: want *model.PublicError, got %#v", err)
	}
	if pub.Code != 2 {
		t.Fatalf("idx missing --table dbo: want code 2, got %d", pub.Code)
	}
}

// TestRunDispatchesIdxMissingTableAndTop is fix 1's B1: the only test
// in this project that drives "idx missing" through idxMissingCommand's
// own Execute closure, from parsed args all the way to the SQL
// parameters diagnostics.Missing actually sends - every other test
// either stops at Parse (this file's own four tests, above) or calls
// diagnostics.Missing directly with hand-built MissingOptions
// (tests/integration's TestIndexDMV), bypassing the registry
// entirely. Measured directly on the compiled binary before this fix
// (task-14-fix-1.md): with this wiring broken, "idx missing --top 1"
// silently returned two rows at exit code 0 - a user-requested ceiling
// ignored without a word.
func TestRunDispatchesIdxMissingTableAndTop(t *testing.T) {
	profile := config.Profile{Host: "fake", Database: "db", Username: "user", Password: "secret", Port: 1433, TrustServerCertificate: true}

	var captured []driver.NamedValue
	var out, errout bytes.Buffer
	args := []string{"--ctx", "x", "--format", "json", "--out-dir", t.TempDir(), "idx", "missing", "--table", "dbo.Orders", "--top", "1"}
	code := run(context.Background(), args, &out, &errout, fakeLoadConfig(profile), fakeOpenSessionCapturingMissingArgs(t, 16, &captured))
	if code != 0 {
		t.Fatalf("got code %d, want 0 (%s / %s)", code, out.String(), errout.String())
	}

	var topArg, objectIDArg driver.NamedValue
	foundTop, foundObjectID := false, false
	for _, a := range captured {
		switch a.Name {
		case "top":
			topArg, foundTop = a, true
		case "object_id":
			objectIDArg, foundObjectID = a, true
		}
	}
	if !foundTop {
		t.Fatalf("idx missing --top 1: no @top argument reached diagnostics.Missing at all, got args %#v", captured)
	}
	if got, ok := topArg.Value.(int64); !ok || got != 1 {
		t.Fatalf("idx missing --top 1: @top must be 1 (req.Top, via MissingOptions), got %#v - the registry wiring is broken", topArg.Value)
	}
	if !foundObjectID {
		t.Fatalf("idx missing --table dbo.Orders: no @object_id argument reached diagnostics.Missing at all, got args %#v", captured)
	}
	if got, ok := objectIDArg.Value.(int64); !ok || got != 4242 {
		t.Fatalf("idx missing --table dbo.Orders: @object_id must be the resolved object id 4242 (req.Table, via MissingOptions), got %#v", objectIDArg.Value)
	}
}

// fakeOpenSessionCapturingMissingArgs mirrors run_test.go's own
// fakeOpenSession, for a session that also answers "idx missing"'s own
// resolve/query pair and captures the query's args (testdriver_test.go).
func fakeOpenSessionCapturingMissingArgs(t *testing.T, major int, missingArgs *[]driver.NamedValue) sessionOpener {
	return func(ctx context.Context, p config.Profile) (*sqlserver.Session, error) {
		return newFakeSessionCapturingMissingArgs(t, major, missingArgs), nil
	}
}
