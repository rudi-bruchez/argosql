package output

import (
	"io"
	"strings"
	"testing"

	"github.com/rudi-bruchez/argosql/internal/model"
)

// TestTSVDecodeNullVsLiteralBackslashN is the two-way counterpart of
// TestTSVEscapes: it proves \N-as-null and \N-as-a-real-string-value stay
// distinguishable after a full encode -> decode round trip, not just that
// EncodeTSVCell produces different text for the two (which TestTSVEscapes
// already covers on the encode side alone).
func TestTSVDecodeNullVsLiteralBackslashN(t *testing.T) {
	spec := model.TableSpec{Columns: []model.Column{{Name: "c", SQLType: "NVARCHAR(10)"}}}
	var buf strings.Builder
	enc, err := NewTableEncoder(&buf, FormatTSV, spec)
	if err != nil {
		t.Fatalf("NewTableEncoder: %v", err)
	}
	if err := enc.WriteRow([]model.Cell{nil}); err != nil {
		t.Fatalf("WriteRow(nil): %v", err)
	}
	if err := enc.WriteRow([]model.Cell{`\N`}); err != nil {
		t.Fatalf("WriteRow(literal): %v", err)
	}
	if err := enc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	dec, err := NewTableDecoder(strings.NewReader(buf.String()), FormatTSV, spec)
	if err != nil {
		t.Fatalf("NewTableDecoder: %v", err)
	}
	nullRow, err := dec.Next()
	if err != nil {
		t.Fatalf("Next() (null row): %v", err)
	}
	if nullRow[0] != nil {
		t.Fatalf("expected nil for the null row, got %#v", nullRow[0])
	}
	literalRow, err := dec.Next()
	if err != nil {
		t.Fatalf("Next() (literal row): %v", err)
	}
	if literalRow[0] != `\N` {
		t.Fatalf(`expected literal "\N", got %#v`, literalRow[0])
	}
	if _, err := dec.Next(); err != io.EOF {
		t.Fatalf("Next() past last row: got %v, want io.EOF", err)
	}
}

// TestTSVRoundTripAllTypesAndEscapes exercises every escape case plus a
// representative of each reconstructed Cell Go type, through a real
// encode -> decode round trip (not just EncodeTSVCell in isolation).
func TestTSVRoundTripAllTypesAndEscapes(t *testing.T) {
	spec := model.TableSpec{
		Columns: []model.Column{
			{Name: "n", SQLType: "BIGINT"},
			{Name: "b", SQLType: "BIT"},
			{Name: "f", SQLType: "FLOAT"},
			{Name: "s", SQLType: "NVARCHAR(100)"},
			{Name: "amt", SQLType: "DECIMAL(38,4)"},
		},
	}
	rows := [][]model.Cell{
		{int64(9223372036854775807), true, 0.5, "tab\tcr\rlf\nbackslash\\end", "1234567890123456789012345678901234.5678"},
		{int64(-1), false, -2.25, "unicode: héllo 世界 \U0001F600", "-0.0001"},
		{int64(0), true, 0.0, "", nil},
	}

	var buf strings.Builder
	enc, err := NewTableEncoder(&buf, FormatTSV, spec)
	if err != nil {
		t.Fatalf("NewTableEncoder: %v", err)
	}
	for _, row := range rows {
		if err := enc.WriteRow(row); err != nil {
			t.Fatalf("WriteRow: %v", err)
		}
	}
	if err := enc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	dec, err := NewTableDecoder(strings.NewReader(buf.String()), FormatTSV, spec)
	if err != nil {
		t.Fatalf("NewTableDecoder: %v", err)
	}
	for i, want := range rows {
		got, err := dec.Next()
		if err != nil {
			t.Fatalf("Next() row %d: %v", i, err)
		}
		for j := range want {
			// The "amt" column of the third row is nil (SQL NULL), not
			// the string "nil" - the only cell in this table's column 4
			// that is untyped by the NVARCHAR/DECIMAL split above.
			if got[j] != want[j] {
				t.Fatalf("row %d cell %d: got %#v, want %#v", i, j, got[j], want[j])
			}
		}
	}
	if _, err := dec.Next(); err != io.EOF {
		t.Fatalf("Next() past last row: got %v, want io.EOF", err)
	}
}

// TestTSVSpecialColumnNames proves column names themselves can carry the
// same special characters a cell can, and survive the header round trip.
func TestTSVSpecialColumnNames(t *testing.T) {
	spec := model.TableSpec{
		Columns: []model.Column{
			{Name: "col\twith\ttabs", SQLType: "NVARCHAR(10)"},
			{Name: "col\nwith\nnewlines", SQLType: "NVARCHAR(10)"},
			{Name: `col\with\backslash`, SQLType: "NVARCHAR(10)"},
		},
	}
	var buf strings.Builder
	enc, err := NewTableEncoder(&buf, FormatTSV, spec)
	if err != nil {
		t.Fatalf("NewTableEncoder: %v", err)
	}
	if err := enc.WriteRow([]model.Cell{"a", "b", "c"}); err != nil {
		t.Fatalf("WriteRow: %v", err)
	}
	if err := enc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// The header line must still be exactly one line: every special
	// character in a name must have been escaped away.
	header := strings.SplitN(buf.String(), "\n", 2)[0]
	if strings.Count(header, "\t") != 2 {
		t.Fatalf("expected exactly 2 literal separator tabs in the header, got: %q", header)
	}

	dec, err := NewTableDecoder(strings.NewReader(buf.String()), FormatTSV, spec)
	if err != nil {
		t.Fatalf("NewTableDecoder with special column names: %v", err)
	}
	row, err := dec.Next()
	if err != nil {
		t.Fatalf("Next(): %v", err)
	}
	if row[0] != "a" || row[1] != "b" || row[2] != "c" {
		t.Fatalf("unexpected row: %#v", row)
	}
}

// TestTSVMalformedEscapeRejected proves a dangling or invalid escape
// sequence in an artifact is rejected rather than silently accepted.
func TestTSVMalformedEscapeRejected(t *testing.T) {
	spec := model.TableSpec{Columns: []model.Column{{Name: "c", SQLType: "NVARCHAR(10)"}}}

	cases := []string{
		"c\n" + `a\` + "\n",  // trailing backslash with nothing after it
		"c\n" + `a\x` + "\n", // invalid escape letter
	}
	for _, artifact := range cases {
		dec, err := NewTableDecoder(strings.NewReader(artifact), FormatTSV, spec)
		if err != nil {
			// Rejected at header time is also an acceptable way to fail.
			continue
		}
		if _, err := dec.Next(); err == nil {
			t.Fatalf("expected an error decoding malformed artifact %q, got nil", artifact)
		}
	}
}

// TestTSVRowFieldCountMismatchRejected proves a data line whose field
// count disagrees with the header is rejected.
func TestTSVRowFieldCountMismatchRejected(t *testing.T) {
	artifact := "a\tb\n1\n" // header declares 2 columns, row has 1 field
	spec := model.TableSpec{Columns: []model.Column{{Name: "a", SQLType: "INT"}, {Name: "b", SQLType: "INT"}}}
	dec, err := NewTableDecoder(strings.NewReader(artifact), FormatTSV, spec)
	if err != nil {
		t.Fatalf("NewTableDecoder: %v", err)
	}
	if _, err := dec.Next(); err == nil {
		t.Fatal("expected an error for a row with too few fields, got nil")
	}
}

// TestTSVHeaderColumnCountMismatchRejected proves a header whose column
// count disagrees with the caller's spec is rejected up front.
func TestTSVHeaderColumnCountMismatchRejected(t *testing.T) {
	artifact := "a\n1\n"
	spec := model.TableSpec{Columns: []model.Column{{Name: "a", SQLType: "INT"}, {Name: "b", SQLType: "INT"}}}
	if _, err := NewTableDecoder(strings.NewReader(artifact), FormatTSV, spec); err == nil {
		t.Fatal("expected an error for a header/spec column-count mismatch, got nil")
	}
}

// TestTSVTruncatedArtifactRejected proves an artifact that ends mid-line
// (missing the trailing newline every line this package writes always
// has) is reported as malformed rather than silently accepted as a short
// final row.
func TestTSVTruncatedArtifactRejected(t *testing.T) {
	artifact := "a\n1" // no trailing newline after the data line
	spec := model.TableSpec{Columns: []model.Column{{Name: "a", SQLType: "INT"}}}
	dec, err := NewTableDecoder(strings.NewReader(artifact), FormatTSV, spec)
	if err != nil {
		t.Fatalf("NewTableDecoder: %v", err)
	}
	if _, err := dec.Next(); err == nil {
		t.Fatal("expected an error for a truncated artifact, got nil")
	}
}

func TestUnsupportedFormatRejected(t *testing.T) {
	spec := model.TableSpec{Columns: []model.Column{{Name: "a", SQLType: "INT"}}}
	if _, err := NewTableEncoder(&strings.Builder{}, "csv", spec); err == nil {
		t.Fatal("expected an error for an unsupported encoder format, got nil")
	}
	if _, err := NewTableDecoder(strings.NewReader(""), "csv", spec); err == nil {
		t.Fatal("expected an error for an unsupported decoder format, got nil")
	}
}
