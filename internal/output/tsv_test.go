package output

import (
	"testing"

	"github.com/rudi-bruchez/argosql/internal/model"
)

// TestTSVFailingWriterPropagated is the TSV counterpart of
// TestJSONFailingWriterPropagated (json_test.go), which shares its
// failingWriter helper: a TSV encoder buffers internally too
// (bufio.Writer), so the failure only reliably surfaces once Close's
// Flush reaches the underlying writer.
func TestTSVFailingWriterPropagated(t *testing.T) {
	spec := model.TableSpec{Columns: []model.Column{{Name: "a", SQLType: "INT"}}}
	fw := &failingWriter{failAfter: 0}
	enc, err := NewTableEncoder(fw, FormatTSV, spec)
	if err != nil {
		t.Fatalf("NewTableEncoder: %v", err)
	}
	if err := enc.WriteRow([]model.Cell{int64(1)}); err != nil {
		t.Fatalf("WriteRow: %v", err)
	}
	if err := enc.Close(); err == nil {
		t.Fatal("expected the failing writer's error to propagate from Close, got nil")
	}
}

func TestTSVEscapes(t *testing.T) {
	cases := []struct {
		c    model.Cell
		want string
	}{
		{nil, `\N`}, {"", ""},
		{`\N`, `\\N`}, {"a\tb\nc", `a\tb\nc`},
	}
	for _, c := range cases {
		if got := EncodeTSVCell(c.c); got != c.want {
			t.Fatalf("%q != %q", got, c.want)
		}
	}
}

// TestTSVEscapeBackslashAndTabCombined covers a case TestTSVEscapes does
// not: a value carrying both a literal backslash and a tab together. A
// rule about the *order* two separate escaping passes run in would pass
// against each of TestTSVEscapes' cases individually without ever
// proving the two escapes combine correctly in one value; fixing the
// exact expected output here, byte for byte, instead of stating a rule,
// is what actually pins the combined behavior down - it stays right
// under either of the two orders a single-pass strings.Replacer could
// apply its replacements in (backslash before tab or the reverse:
// strings.NewReplacer has no "order" at all, since it matches all of its
// patterns simultaneously in one scan - see tsvEscaper's doc comment in
// tsv.go), and would instead catch the real defect: a tab-then-backslash
// implementation built from two sequential strings.Replace calls, which
// re-escapes the backslash a prior tab substitution just wrote.
func TestTSVEscapeBackslashAndTabCombined(t *testing.T) {
	// Input (interpreted string, not raw): x, backslash, y, TAB, z.
	in := "x\\y\tz"
	// Expected output (raw string): x, \, \, y, \, t, z - the backslash
	// doubled to \\, the tab turned into \t, nothing else touched.
	want := `x\\y\tz`
	if got := EncodeTSVCell(in); got != want {
		t.Fatalf("%q != %q", got, want)
	}
}
