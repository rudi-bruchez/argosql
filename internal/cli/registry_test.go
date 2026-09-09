package cli

import "testing"

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
