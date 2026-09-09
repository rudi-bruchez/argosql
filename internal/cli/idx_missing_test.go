package cli

import (
	"errors"
	"testing"

	"github.com/rudi-bruchez/argosql/internal/model"
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
