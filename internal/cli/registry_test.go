package cli

import (
	"fmt"
	"strings"
	"testing"
)

// errorfHelper is the minimal subset of *testing.T the checks below need:
// just enough to report a failure and mark the caller's own stack frame as
// uninteresting in a trace. *testing.T satisfies it for every ordinary
// test below; recordingT (further down) satisfies it for the two tests
// that must observe a check FAIL without failing the outer test itself -
// the brief's own required proof that a dropped handler, or an added
// deferred command, is actually caught, without ever mutating the real
// registry NewRegistry() returns.
type errorfHelper interface {
	Helper()
	Errorf(format string, args ...any)
}

// recordingT is a throwaway errorfHelper that only remembers whether
// Errorf was called. It exists so TestRegistryCoherenceCheckCatchesAMissingHandler
// and TestDeferredCapabilitiesCheckCatchesAnAddedCommand can feed a
// deliberately broken FAKE registry to the real check functions and
// observe the failure, rather than asserting by inspection that the
// check "should" fail - the dispatch's own warning about a cassure that
// does not actually mordre.
type recordingT struct {
	failed bool
	// msgs keeps each reported message, not only the fact that one was
	// reported: TestExactCommandSetCheckCatchesAnUnpredictedCommand has
	// to prove the check named the command it was supposed to catch, and
	// "something failed" is exactly the weak signal this project's method
	// rejects everywhere else.
	msgs []string
}

func (r *recordingT) Helper() {}
func (r *recordingT) Errorf(format string, args ...any) {
	r.failed = true
	r.msgs = append(r.msgs, fmt.Sprintf(format, args...))
}

// specCommandNames is the design spec's own command table (spec lines
// 46-53), verbatim in the document's order: the thirteen commands this
// build of asq claims to implement, help included. TestRegistryCommandsHaveHandlers
// uses it to prove NewRegistry() actually registers each one with a
// handler, rather than merely checking that no Command in the real
// registry happens to have a nil Execute - a registry that silently
// dropped "qs top" entirely would still pass that weaker check.
var specCommandNames = []string{
	"help", "info", "qs status", "qs top", "qs query", "plan",
	"obj table", "obj code", "size table", "idx list", "idx usage",
	"idx missing", "stats list",
}

// assertEveryCommandHasAHandler is the actual coherence check, factored
// out of its test so TestRegistryCoherenceCheckCatchesAMissingHandler can
// run it a second time against a fake registry through a recordingT,
// without ever touching the production NewRegistry().
func assertEveryCommandHasAHandler(t errorfHelper, reg Registry, want []string) {
	t.Helper()
	byName := map[string]*Command{}
	for i := range reg {
		byName[reg[i].Name] = &reg[i]
	}
	for _, name := range want {
		cmd, ok := byName[name]
		if !ok {
			t.Errorf("registry is missing command %q", name)
			continue
		}
		if cmd.Execute == nil {
			t.Errorf("command %q is registered with no handler", name)
		}
	}
}

func TestRegistryCommandsHaveHandlers(t *testing.T) {
	assertEveryCommandHasAHandler(t, NewRegistry(), specCommandNames)
}

// TestRegistryCoherenceCheckCatchesAMissingHandler is the brief's own
// required proof, in its own required form: drop one entry ("qs top")
// from a copy of the real registry - never from NewRegistry() itself,
// which this test never mutates - and confirm assertEveryCommandHasAHandler
// actually reports it missing, rather than trusting that it would.
func TestRegistryCoherenceCheckCatchesAMissingHandler(t *testing.T) {
	real := NewRegistry()
	var fake Registry
	for _, c := range real {
		if c.Name == "qs top" {
			continue
		}
		fake = append(fake, c)
	}

	rec := &recordingT{}
	assertEveryCommandHasAHandler(rec, fake, specCommandNames)
	if !rec.failed {
		t.Fatal("fake registry missing \"qs top\" did not fail the coherence check")
	}

	// The real registry, untouched, must still pass: this is the
	// "restore and get green" half the brief asks for, proven by simply
	// never having broken it in the first place.
	rec = &recordingT{}
	assertEveryCommandHasAHandler(rec, real, specCommandNames)
	if rec.failed {
		t.Fatal("the real, untouched registry failed the coherence check")
	}
}

// specFlagsFor names, per command, the non-global flags the design spec
// declares for it (spec's command table and the prose beneath it) - not
// their bounds or defaults, which parse_test.go and the command's own
// doc comments already exercise against real values, but their bare
// presence in this build's registry: a Flags entry dropped by accident
// would otherwise only be caught by help silently advertising less, with
// no test ever failing.
var specFlagsFor = map[string][]string{
	"help":        {"json"},
	"qs top":      {"object", "min-executions", "by", "aggregate", "hours", "since", "until", "top", "include-internal"},
	"qs query":    {"hours", "since", "until"},
	"plan":        {"plan-id", "summary"},
	"idx missing": {"table", "top"},
}

// globalFlagNames are the nine flags the design spec says "every
// connecting command" accepts (spec: "Connection and execution"),
// reproduced here by name only, for TestRegistryFlagsMatchSpec's second
// half.
var globalFlagNames = []string{
	"ctx", "db", "config", "format", "timeout", "preview", "truncate", "no-truncate", "out-dir",
}

func TestRegistryFlagsMatchSpec(t *testing.T) {
	reg := NewRegistry()
	byName := map[string]Command{}
	for _, c := range reg {
		byName[c.Name] = c
	}

	for cmdName, want := range specFlagsFor {
		cmd, ok := byName[cmdName]
		if !ok {
			t.Fatalf("command %q not registered", cmdName)
		}
		have := map[string]bool{}
		for _, f := range cmd.Flags {
			have[f.Name] = true
		}
		for _, name := range want {
			if !have[name] {
				t.Errorf("%s: spec-required flag --%s is not registered", cmdName, name)
			}
		}
	}

	for _, c := range reg {
		if c.Offline {
			continue
		}
		have := map[string]bool{}
		for _, f := range flagsFor(c) {
			have[f.Name] = true
		}
		for _, name := range globalFlagNames {
			if !have[name] {
				t.Errorf("%s: global flag --%s is not accepted", c.Name, name)
			}
		}
	}
}

// TestOfflineCommandAnnouncesGlobalFlags pins the announced surface to the
// accepted one. Parse accepts every global flag for every command, help
// included; before this, help's own flags table omitted them, so
// `asq help --db foo` was validated against a flag `help --json` listed
// nowhere. An offline command now advertises the globals it accepts, so a
// misspelled global is visibly not a help flag instead of being parsed
// while the announcement says nothing about it.
func TestOfflineCommandAnnouncesGlobalFlags(t *testing.T) {
	reg := NewRegistry()
	found := false
	for _, c := range reg {
		if !c.Offline {
			continue
		}
		found = true
		have := map[string]bool{}
		for _, f := range flagsFor(c) {
			have[f.Name] = true
		}
		for _, name := range globalFlagNames {
			if !have[name] {
				t.Errorf("offline command %q accepts but does not announce global flag --%s", c.Name, name)
			}
		}
	}
	if !found {
		t.Fatal("registry has no offline command to check")
	}
}

// deferredCommandNames names the command-shaped capabilities spec line
// 275 places out of scope for v0.1, each reduced to the registry.Name
// string it would take if someone implemented it anyway: "arbitrary q"
// becomes "q", "regression/temporal comparison" becomes "qs compare",
// and so on. This list, not the registry, is what TestDeferredCapabilitiesAbsent
// actually verifies: a deferred command added to NewRegistry() without
// also being added here would pass silently, which is exactly the
// failure mode the dispatch warns an invented or emptied list produces.
// See TestDeferredCapabilitiesCheckCatchesAnAddedCommand for the
// required proof that this list, fed to the real check, actually bites.
var deferredCommandNames = []string{
	"q",               // arbitrary q
	"mcp",             // MCP
	"broker",          // broker service
	"qs compare",      // regression/temporal comparison
	"qs waits",        // waits
	"plan force",      // forced-plan reports
	"plan diff",       // plan diff
	"obj script",      // script generation
	"idx scan",        // physical index scans
	"idx consolidate", // index consolidation verdicts
	"idx unused",      // unused-index verdicts
	"stats histogram", // statistics histograms
	"stats stale",     // statistics staleness verdicts
	"server compare",  // cross-server comparisons
	"snapshot",        // snapshot history
	"maintenance",     // automatic maintenance
}

// deferredFlagNames are the spec-line-275 items that would surface as a
// flag rather than a command: integrated/Entra/Kerberos authentication
// has no command of its own to add, but would need a flag naming the
// auth mode on every connecting command, or on the profile it reads -
// the same globalFlags()/command-Flags surface deferredCommandNames
// checks, just a different kind of entry in it.
var deferredFlagNames = []string{"integrated", "entra", "kerberos", "auth-mode"}

// deferredFormatValues is JSONL (spec line 275): a third value the
// global --format enum must never advertise alongside tsv and json.
var deferredFormatValues = []string{"jsonl"}

// assertDeferredCapabilitiesAbsent is the actual absence check, factored
// out exactly like assertEveryCommandHasAHandler above, for the same
// reason: TestDeferredCapabilitiesCheckCatchesAnAddedCommand runs it
// against a fake registry through a recordingT.
func assertDeferredCapabilitiesAbsent(t errorfHelper, reg Registry) {
	t.Helper()

	for _, name := range deferredCommandNames {
		for _, c := range reg {
			if c.Name == name {
				t.Errorf("deferred command %q (spec line 275) is registered", name)
			}
		}
	}

	checkFlag := func(where string, f Flag) {
		for _, name := range deferredFlagNames {
			if f.Name == name {
				t.Errorf("%s declares deferred flag --%s (spec line 275)", where, name)
			}
		}
		for _, v := range f.Enum {
			for _, deferred := range deferredFormatValues {
				if v == deferred {
					t.Errorf("%s --%s enum advertises deferred value %q (spec line 275)", where, f.Name, v)
				}
			}
		}
	}

	for _, f := range globalFlags() {
		checkFlag("global flags", f)
	}
	for _, c := range reg {
		for _, f := range c.Flags {
			checkFlag(c.Name, f)
		}
	}
}

func TestDeferredCapabilitiesAbsent(t *testing.T) {
	assertDeferredCapabilitiesAbsent(t, NewRegistry())
}

// TestDeferredCapabilitiesCheckCatchesAnAddedCommand is
// TestRegistryCoherenceCheckCatchesAMissingHandler's counterpart for the
// absence check: append a deferred command ("qs compare") to a copy of
// the real registry and confirm assertDeferredCapabilitiesAbsent actually
// reports it, rather than trusting that an absence check of this shape
// would.
func TestDeferredCapabilitiesCheckCatchesAnAddedCommand(t *testing.T) {
	fake := append(Registry{}, NewRegistry()...)
	fake = append(fake, Command{Name: "qs compare"})

	rec := &recordingT{}
	assertDeferredCapabilitiesAbsent(rec, fake)
	if !rec.failed {
		t.Fatal("adding a deferred command to the registry did not fail the absence check")
	}
}

// TestDeferredCommandNamesListIsNotEmpty guards the other direction the
// dispatch names explicitly: a name quietly dropped from
// deferredCommandNames must narrow what TestDeferredCapabilitiesAbsent
// can catch, never make it pass by testing nothing at all.
func TestDeferredCommandNamesListIsNotEmpty(t *testing.T) {
	if len(deferredCommandNames) == 0 {
		t.Fatal("deferredCommandNames is empty: TestDeferredCapabilitiesAbsent would pass vacuously")
	}
}

// TestMatchCommandPrefersLongestExactMatch makes the longest-match
// preference observable: the real registry (help, info, qs status) has
// no two commands sharing a leading word, so reversing matchCommand's
// search order from longest-first to shortest-first leaves every test
// against the real registry green. A registry carrying both "qs" and
// "qs status" is what actually exercises the choice between them.
func TestMatchCommandPrefersLongestExactMatch(t *testing.T) {
	reg := Registry{
		{Name: "qs"},
		{Name: "qs status"},
	}

	cmd, consumed, err := reg.matchCommand([]string{"qs", "status"})
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Name != "qs status" || consumed != 2 {
		t.Fatalf("got %q consumed=%d, want %q consumed=2", cmd.Name, consumed, "qs status")
	}

	// The one-word command must still resolve on its own, with nothing
	// left over, when "status" is not also present.
	cmd, consumed, err = reg.matchCommand([]string{"qs"})
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Name != "qs" || consumed != 1 {
		t.Fatalf("got %q consumed=%d, want %q consumed=1", cmd.Name, consumed, "qs")
	}

	// A bare token that is an exact prefix of a registered word, but not
	// itself a registered command or a registered command's first word,
	// must never match by abbreviation.
	if _, _, err := reg.matchCommand([]string{"q"}); err == nil {
		t.Fatal("\"q\" matched a command by prefix, want an unknown-command error")
	}
}

// assertRegistryIsExactlyTheSpecCommands pins the registry's command set
// to specCommandNames in BOTH directions: every spec command present,
// and nothing else present at all. It closes the structural gap
// deferredCommandNames' own doc comment declares - that list can only
// catch a deferred command someone remembered to add to it, so a
// capability registered under a name nobody predicted passes the
// absence check silently. Set equality needs no prediction: any
// fourteenth command fails this, whatever it is called.
//
// Factored out of its test for the same reason the two checks above
// are, so the proof below can run it against a fake registry through a
// recordingT without ever touching NewRegistry().
func assertRegistryIsExactlyTheSpecCommands(t errorfHelper, reg Registry, want []string) {
	t.Helper()
	wanted := map[string]bool{}
	for _, n := range want {
		wanted[n] = true
	}
	got := map[string]bool{}
	for i := range reg {
		got[reg[i].Name] = true
		if !wanted[reg[i].Name] {
			t.Errorf("registry registers %q, which the design spec's command table does not list: v0.1 ships exactly %d commands, and spec line 275 defers everything else", reg[i].Name, len(want))
		}
	}
	for _, n := range want {
		if !got[n] {
			t.Errorf("registry does not register %q, which the design spec's command table lists", n)
		}
	}
	if len(reg) != len(want) {
		t.Errorf("registry holds %d commands, want exactly %d (duplicate names would show up here and nowhere else)", len(reg), len(want))
	}
}

// TestRegistryRegistersExactlyTheSpecCommands is the scope gate for this
// build: it fails both when a spec command disappears and when anything
// at all is added, which is what TestDeferredCapabilitiesAbsent cannot
// do on its own.
func TestRegistryRegistersExactlyTheSpecCommands(t *testing.T) {
	assertRegistryIsExactlyTheSpecCommands(t, NewRegistry(), specCommandNames)
}

// TestExactCommandSetCheckCatchesAnUnpredictedCommand is this check's
// own required proof, in the same form as its two siblings: append a
// command whose name appears in NO list in this file - not in
// specCommandNames, not in deferredCommandNames - to a COPY of the real
// registry, and confirm the check reports it. That is precisely the case
// the absence list misses.
func TestExactCommandSetCheckCatchesAnUnpredictedCommand(t *testing.T) {
	real := NewRegistry()
	fake := make(Registry, len(real), len(real)+1)
	copy(fake, real)
	fake = append(fake, Command{Name: "waits top", Execute: real[0].Execute})

	var rec recordingT
	assertRegistryIsExactlyTheSpecCommands(&rec, fake, specCommandNames)
	if !rec.failed {
		t.Fatal("the exact-set check passed a registry carrying an unlisted command: it cannot serve as a scope gate")
	}
	for _, e := range rec.msgs {
		if strings.Contains(e, "waits top") {
			return
		}
	}
	t.Fatalf("the exact-set check reported something, but never named the added command: %v", rec.msgs)
}
