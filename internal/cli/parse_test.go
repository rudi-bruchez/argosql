package cli

import (
	"strings"
	"testing"

	"github.com/rudi-bruchez/argosql/internal/model"
)

// publicErrorCode fails the test unless err wraps a *model.PublicError
// with the given code, and returns that error's Message.
func publicErrorCode(t *testing.T, err error, want int) string {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error")
	}
	pe, ok := err.(*model.PublicError)
	if !ok {
		t.Fatalf("expected *model.PublicError, got %T: %v", err, err)
	}
	if pe.Code != want {
		t.Fatalf("got code %d want %d (%v)", pe.Code, want, err)
	}
	return pe.Message
}

func TestParseDefaults(t *testing.T) {
	req, cmd, err := Parse([]string{"--ctx", "client", "info"})
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Name != "info" {
		t.Fatalf("got command %q want info", cmd.Name)
	}
	if req.Format != "tsv" {
		t.Fatalf("Format = %q, want tsv", req.Format)
	}
	if req.TimeoutSeconds != 30 {
		t.Fatalf("TimeoutSeconds = %d, want 30", req.TimeoutSeconds)
	}
	if req.PreviewRows != 10 {
		t.Fatalf("PreviewRows = %d, want 10", req.PreviewRows)
	}
	if req.TruncateRunes != 200 {
		t.Fatalf("TruncateRunes = %d, want 200", req.TruncateRunes)
	}
	if req.NoTruncate {
		t.Fatal("NoTruncate defaulted to true")
	}
	// ConfigPath and OutDir must stay empty: resolving their defaults is
	// deferred to Run, not done here (see parse.go's defaultConfigPath
	// and defaultOutDir, and TestHelpOfflineWithoutConfigDir in
	// run_test.go for the reason).
	if req.ConfigPath != "" {
		t.Fatalf("ConfigPath = %q, want empty (deferred)", req.ConfigPath)
	}
	if req.OutDir != "" {
		t.Fatalf("OutDir = %q, want empty (deferred)", req.OutDir)
	}
}

func TestParseFlagsBeforeAndAfterCommandPath(t *testing.T) {
	before, _, err := Parse([]string{"--ctx", "client", "--format", "json", "qs", "status"})
	if err != nil {
		t.Fatal(err)
	}
	after, _, err := Parse([]string{"qs", "status", "--ctx", "client", "--format", "json"})
	if err != nil {
		t.Fatal(err)
	}
	if before.Command != "qs status" || after.Command != "qs status" {
		t.Fatalf("got commands %q / %q, want both qs status", before.Command, after.Command)
	}
	if before.ContextName != "client" || after.ContextName != "client" {
		t.Fatalf("got ContextName %q / %q, want both client", before.ContextName, after.ContextName)
	}
	if before.Format != "json" || after.Format != "json" {
		t.Fatalf("got Format %q / %q, want both json", before.Format, after.Format)
	}
}

// TestParseFlagValueConsumedBeforeSubpath proves that a flag's value is
// consumed before the parser ever treats a bare token as part of a
// command path: --ctx's value here ("qs") must never be mistaken for
// the start of some "qs ..." command.
func TestParseFlagValueConsumedBeforeSubpath(t *testing.T) {
	req, cmd, err := Parse([]string{"--ctx", "qs", "info"})
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Name != "info" {
		t.Fatalf("got command %q, want info", cmd.Name)
	}
	if req.ContextName != "qs" {
		t.Fatalf("ContextName = %q, want %q", req.ContextName, "qs")
	}
}

func TestParseUnknownFlag(t *testing.T) {
	_, _, err := Parse([]string{"--ctx", "client", "--bogus", "x", "info"})
	publicErrorCode(t, err, 2)
}

func TestParseDuplicateFlag(t *testing.T) {
	t.Run("same value is tolerated", func(t *testing.T) {
		_, _, err := Parse([]string{"--ctx", "client", "--format", "tsv", "--format", "tsv", "info"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
	t.Run("conflicting values are rejected", func(t *testing.T) {
		_, _, err := Parse([]string{"--ctx", "client", "--format", "tsv", "--format", "json", "info"})
		publicErrorCode(t, err, 2)
	})
}

// TestParseNoTruncateTruncateConflict is the central trap the brief
// calls out: --no-truncate must be rejected alongside an explicit
// --truncate even when that --truncate names exactly the default value
// (200). A check based on value instead of presence would miss this
// exact case.
func TestParseNoTruncateTruncateConflict(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"no-truncate then truncate at default", []string{"--ctx", "client", "--no-truncate", "--truncate", "200", "info"}},
		{"truncate at default then no-truncate", []string{"--ctx", "client", "--truncate", "200", "--no-truncate", "info"}},
		{"no-truncate then truncate non-default", []string{"--ctx", "client", "--no-truncate", "--truncate", "50", "info"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, err := Parse(c.args)
			publicErrorCode(t, err, 2)
		})
	}
}

func TestParseNoTruncateAlone(t *testing.T) {
	req, _, err := Parse([]string{"--ctx", "client", "--no-truncate", "info"})
	if err != nil {
		t.Fatal(err)
	}
	if !req.NoTruncate {
		t.Fatal("NoTruncate was not set")
	}
}

func TestParseTruncateAlone(t *testing.T) {
	req, _, err := Parse([]string{"--ctx", "client", "--truncate", "50", "info"})
	if err != nil {
		t.Fatal(err)
	}
	if req.TruncateRunes != 50 {
		t.Fatalf("TruncateRunes = %d, want 50", req.TruncateRunes)
	}
}

func TestParseCtxRequiredForConnectingCommand(t *testing.T) {
	_, _, err := Parse([]string{"info"})
	publicErrorCode(t, err, 2)
}

func TestParseHelpNeedsNoCtx(t *testing.T) {
	_, cmd, err := Parse([]string{"help"})
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Name != "help" {
		t.Fatalf("got command %q, want help", cmd.Name)
	}
}

func TestParseUnknownCommand(t *testing.T) {
	// "qs" alone is not registered (only "qs status" is): it must be
	// reported as an unknown command, not matched against "qs status"
	// by a partial/prefix rule.
	_, _, err := Parse([]string{"--ctx", "client", "qs"})
	publicErrorCode(t, err, 2)
}

func TestParseNoCommandAtAll(t *testing.T) {
	_, _, err := Parse([]string{"--ctx", "client"})
	publicErrorCode(t, err, 2)
}

func TestParseExtraPositionalArguments(t *testing.T) {
	_, _, err := Parse([]string{"--ctx", "client", "info", "extra"})
	publicErrorCode(t, err, 2)
}

func TestParseFlagNotValidForOtherCommand(t *testing.T) {
	// --json is help's own flag; info does not declare it and must
	// reject it, even though it is recognized globally enough to
	// tokenize correctly.
	_, _, err := Parse([]string{"--ctx", "client", "--json", "info"})
	publicErrorCode(t, err, 2)
}

func TestParseJSONSugarOverridesFormat(t *testing.T) {
	cases := [][]string{
		{"help", "--json", "--format", "tsv"},
		{"help", "--format", "tsv", "--json"},
	}
	for _, args := range cases {
		req, _, err := Parse(args)
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if req.Format != "json" {
			t.Fatalf("%v: Format = %q, want json", args, req.Format)
		}
	}
}

func TestParseFormatEnum(t *testing.T) {
	_, _, err := Parse([]string{"--ctx", "client", "--format", "xml", "info"})
	publicErrorCode(t, err, 2)
}

// TestParseFlagBounds drives every global numeric flag's documented
// default and both placements (before and after the command path), plus
// one in-range and one out-of-range value on each side of its bound.
func TestParseFlagBounds(t *testing.T) {
	cases := []struct {
		name    string
		flag    string
		value   string
		wantErr bool
	}{
		{"timeout min ok", "timeout", "1", false},
		{"timeout max ok", "timeout", "300", false},
		{"timeout below min", "timeout", "0", true},
		{"timeout above max", "timeout", "301", true},
		{"preview min ok", "preview", "0", false},
		{"preview max ok", "preview", "10000", false},
		{"preview below min", "preview", "-1", true},
		{"preview above max", "preview", "10001", true},
		{"truncate min ok", "truncate", "1", false},
		{"truncate max ok", "truncate", "10000", false},
		{"truncate below min", "truncate", "0", true},
		{"truncate above max", "truncate", "10001", true},
	}
	for _, c := range cases {
		t.Run(c.name+"/before", func(t *testing.T) {
			_, _, err := Parse([]string{"--" + c.flag, c.value, "--ctx", "client", "info"})
			if c.wantErr {
				publicErrorCode(t, err, 2)
			} else if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
		t.Run(c.name+"/after", func(t *testing.T) {
			_, _, err := Parse([]string{"--ctx", "client", "info", "--" + c.flag, c.value})
			if c.wantErr {
				publicErrorCode(t, err, 2)
			} else if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestParseMissingFlagValue(t *testing.T) {
	_, _, err := Parse([]string{"--ctx", "client", "info", "--timeout"})
	publicErrorCode(t, err, 2)
}

// TestParseRejectsExplicitEmptySinceUntil is fix-2's A2: an explicitly
// given --since or --until with an empty value is a malformed
// timestamp, never a silent "not given" that falls back to the
// relative --hours window. Measured before this fix:
// Parse([]string{"--ctx","client","qs","top","--since","","--until",""})
// returned no error at all and a resolved 24-hour relative window - a
// blank environment-variable expansion in an operations script would
// otherwise rank a different window than the one requested, with no
// error to notice.
func TestParseRejectsExplicitEmptySinceUntil(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"both empty", []string{"--ctx", "client", "qs", "top", "--since", "", "--until", ""}},
		{"since empty alone", []string{"--ctx", "client", "qs", "top", "--since", ""}},
		{"until empty alone", []string{"--ctx", "client", "qs", "top", "--until", ""}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, err := Parse(c.args)
			publicErrorCode(t, err, 2)
		})
	}
}

// TestParseAcceptsNeitherSinceNorUntil proves the fix above does not
// break the legitimate case: neither flag given at all still resolves
// the relative --hours window, exactly as before.
func TestParseAcceptsNeitherSinceNorUntil(t *testing.T) {
	req, _, err := Parse([]string{"--ctx", "client", "qs", "top"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.Window.Since.IsZero() || req.Window.Until.IsZero() {
		t.Fatalf("Window not resolved: %+v", req.Window)
	}
}

// TestParseValidatesObjectSyntaxBeforeConnection is A4: --object's
// two-part syntax is checked in Parse, before any connection opens -
// the same ordering already applied to --aggregate/--by and to the
// window above. Measured before this fix: both invocations below only
// ever reached sqlserver.Resolve's own check after a real connection
// attempt (code 3 on an unreachable server), instead of code 2 here.
func TestParseValidatesObjectSyntaxBeforeConnection(t *testing.T) {
	cases := []string{"dbo", "[dbo.x"}
	for _, object := range cases {
		t.Run(object, func(t *testing.T) {
			_, _, err := Parse([]string{"--ctx", "client", "qs", "top", "--object", object})
			publicErrorCode(t, err, 2)
		})
	}
}

// TestParseValidatesPositionalObjectSyntaxBeforeConnection is task 13
// fix-2's own A1 target: a reviewer removed the
// sqlserver.ValidateQualifiedName call in Parse's PositionalIsObject
// branch and the entire suite, unitary and integration, stayed green -
// no test in this package ever exercised "obj table", "obj code", "idx
// list" or "size table" other than by name, in
// TestHelpOfflineListsAllCommands's own list. Unlike
// TestParseValidatesObjectSyntaxBeforeConnection above, which only
// exercises "qs top"'s --object FLAG, this covers the POSITIONAL form
// the four object-taking commands actually use.
func TestParseValidatesPositionalObjectSyntaxBeforeConnection(t *testing.T) {
	for _, args := range [][]string{
		{"--ctx", "client", "obj", "table", "Orders"},
		{"--ctx", "client", "obj", "code", "Orders"},
		{"--ctx", "client", "idx", "list", "Orders"},
		{"--ctx", "client", "size", "table", "Orders"},
	} {
		t.Run(strings.Join(args[2:], " "), func(t *testing.T) {
			_, _, err := Parse(args)
			publicErrorCode(t, err, 2)
		})
	}
}

// TestParseRejectsExecutionsAvg is B8: grep -rn 'aggregate'
// --include=*_test.go found nothing in this repository before this
// test - the guard existed in parse.go but no test exercised it, so
// removing its three lines left the entire suite green. Measured:
// without it, the combination reaches the SQL as code 5 instead of
// failing here at code 2.
func TestParseRejectsExecutionsAvg(t *testing.T) {
	_, _, err := Parse([]string{"--ctx", "client", "qs", "top", "--by", "executions", "--aggregate", "avg"})
	publicErrorCode(t, err, 2)
}

// TestParseRejectsHoursWithValidSinceUntilPair is B11: a reviewer
// bypassed registry.go's --hours/--since/--until ExclusiveWith
// declarations and measured a real regression behind them - without
// that guard, --hours is silently ignored whenever a complete, valid
// --since/--until pair is also given, and no unit test protected it.
func TestParseRejectsHoursWithValidSinceUntilPair(t *testing.T) {
	_, _, err := Parse([]string{"--ctx", "client", "qs", "top", "--hours", "5", "--since", "2026-01-01T00:00:00Z", "--until", "2026-01-02T00:00:00Z"})
	publicErrorCode(t, err, 2)
}

// TestParseEqualsForm is fix 1's A7 / pending-fixes.md P1: a
// pre-existing, registered flag must be recognized in "--flag=value"
// form, not rejected with a message claiming it does not exist.
// Measured before this fix: "--top=5" produced "flag: --top=5: unknown
// flag" - the flag exists, only the message, and the parse itself,
// were wrong.
func TestParseEqualsForm(t *testing.T) {
	t.Run("--top=5 resolves top, not an unknown-flag error", func(t *testing.T) {
		req, _, err := Parse([]string{"--ctx", "client", "qs", "top", "--top=5"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if req.Top != 5 {
			t.Fatalf("Top: got %d, want 5", req.Top)
		}
	})

	// Constraint 1: a FlagBool written with "=" must take the value on
	// the right, not the loop's usual forced "true" for a bare
	// "--no-truncate".
	t.Run("--no-truncate=false values false, not the usual forced true", func(t *testing.T) {
		req, _, err := Parse([]string{"--ctx", "client", "info", "--no-truncate=false"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if req.NoTruncate {
			t.Fatal("NoTruncate: got true, want false")
		}
	})

	// Constraint 2: the duplicate-flag-with-conflicting-values rule
	// must still fire when the separated and "=" forms are mixed.
	t.Run("mixing separated and equals forms still catches a conflicting duplicate", func(t *testing.T) {
		_, _, err := Parse([]string{"--ctx", "client", "qs", "top", "--top", "5", "--top=6"})
		publicErrorCode(t, err, 2)
	})

	// Constraint 3: a value that itself contains "=" must survive
	// intact when it arrives as its own separate token (never cut),
	// and cutting the "=" form on the FIRST sign, not the last, must
	// keep everything after it together when the value embeds one.
	t.Run("a value containing its own equals sign survives both forms", func(t *testing.T) {
		// dbo.a=b is a syntactically valid two-part name by this
		// project's own splitter (splitIdentifierParts only treats "."
		// and brackets specially; "=" is an ordinary character within a
		// part) - sqlserver.Resolve would fail to find it against a
		// real catalog, but Parse itself must not reject or mangle it.
		req, _, err := Parse([]string{"--ctx", "client", "qs", "top", "--object", "dbo.a=b"})
		if err != nil {
			t.Fatalf("unexpected error (separated form): %v", err)
		}
		if req.Object != "dbo.a=b" {
			t.Fatalf("Object: got %q, want %q (separated form, never cut apart)", req.Object, "dbo.a=b")
		}

		req2, _, err2 := Parse([]string{"--ctx", "client", "qs", "top", "--object=dbo.a=b"})
		if err2 != nil {
			t.Fatalf("unexpected error (equals form): %v", err2)
		}
		if req2.Object != "dbo.a=b" {
			t.Fatalf("Object: got %q, want %q (cut on the first \"=\" only)", req2.Object, "dbo.a=b")
		}
	})
}

// TestParseQueryIDPositional is fix 1's B5: nothing protected the
// "qs query" positional argument's own rejection. Measured: a mutant
// that always accepts (the id<=0-or-unparseable guard forced false)
// leaves every test in internal/diagnostics and internal/cli green,
// while the real mutated binary answers "qs query abc" against an
// unreachable server with exit code 3 (connection failure) instead of
// 2 (argument error) - exactly the arguments-before-connection
// regression task 10 already paid a whole pass to fix once. A
// syntactically invalid or non-positive id must never reach a
// connection attempt at all.
func TestParseQueryIDPositional(t *testing.T) {
	t.Run("non-numeric is rejected before any connection", func(t *testing.T) {
		_, _, err := Parse([]string{"--ctx", "client", "qs", "query", "abc"})
		publicErrorCode(t, err, 2)
	})
	t.Run("zero is rejected", func(t *testing.T) {
		_, _, err := Parse([]string{"--ctx", "client", "qs", "query", "0"})
		publicErrorCode(t, err, 2)
	})
	t.Run("negative is rejected", func(t *testing.T) {
		_, _, err := Parse([]string{"--ctx", "client", "qs", "query", "-5"})
		publicErrorCode(t, err, 2)
	})
	t.Run("missing entirely is rejected", func(t *testing.T) {
		_, _, err := Parse([]string{"--ctx", "client", "qs", "query"})
		publicErrorCode(t, err, 2)
	})
	t.Run("a valid positive id is accepted and resolves QueryID", func(t *testing.T) {
		req, _, err := Parse([]string{"--ctx", "client", "qs", "query", "4821"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if req.QueryID != 4821 {
			t.Fatalf("QueryID: got %d, want 4821", req.QueryID)
		}
	})
}

// TestParsePlanIDRequired is fix 2's B2: no test in this project parsed
// "plan" at all, so the "--plan-id is required" check added in this
// project's own dispatch for the plan command had nothing holding it -
// a reviewer measured that removing it left the whole suite green, and
// then measured the REAL consequence against the compiled binary: with
// the check, "asq --ctx client plan 4821" (no --plan-id, no reachable
// server) reports code 2, "flag: --plan-id is required"; without it,
// the same call opens a connection to a dead port and reports code 3,
// "connection: connection failed" - exactly the defect CLAUDE.md
// records as measured and fixed once already (an argument error
// reported after the connection attempt reads as a network problem to
// an agent that retries the network instead of fixing its own
// argument). This is the second time on this project a correct
// argument check shipped with no test - the first was task 11's own
// B5.
func TestParsePlanIDRequired(t *testing.T) {
	t.Run("missing --plan-id is rejected before any connection", func(t *testing.T) {
		_, _, err := Parse([]string{"--ctx", "client", "plan", "4821"})
		publicErrorCode(t, err, 2)
	})
	t.Run("a valid --plan-id is accepted and resolves PlanID", func(t *testing.T) {
		req, cmd, err := Parse([]string{"--ctx", "client", "plan", "4821", "--plan-id", "9033"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cmd.Name != "plan" {
			t.Fatalf("command: got %q, want %q", cmd.Name, "plan")
		}
		if req.QueryID != 4821 {
			t.Fatalf("QueryID: got %d, want 4821", req.QueryID)
		}
		if req.PlanID != 9033 {
			t.Fatalf("PlanID: got %d, want 9033", req.PlanID)
		}
	})
}
