package cli

import (
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
