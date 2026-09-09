package output

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/rudi-bruchez/argosql/internal/model"
)

func TestJSONRoundTripAllTypes(t *testing.T) {
	spec := model.TableSpec{
		Name: "t",
		Columns: []model.Column{
			{Name: "id", SQLType: "INT"},
			{Name: "big", SQLType: "BIGINT"},
			{Name: "amount", SQLType: "DECIMAL(38,4)"},
			{Name: "flag", SQLType: "BIT"},
			{Name: "score", SQLType: "FLOAT"},
			{Name: "label", SQLType: "NVARCHAR(50)"},
			{Name: "blob", SQLType: "VARBINARY(8)"},
			{Name: "guid", SQLType: "UNIQUEIDENTIFIER"},
			{Name: "created", SQLType: "DATETIME2"},
			{Name: "maybe", SQLType: "NVARCHAR(10)"},
		},
	}
	rows := [][]model.Cell{
		{
			int64(1), int64(9223372036854775807),
			"12345678901234567890123456789012.3456", true, 3.5,
			"hello", "0x1234", "ff19966f-868b-11d0-b42d-00c04fc964ff",
			"2024-01-15T10:30:00.1234567", nil,
		},
		{
			int64(-5), int64(-9223372036854775808),
			"-0.0001", false, -1.25,
			"unicode: héllo 世界", "0x00ff", "00000000-0000-0000-0000-000000000000",
			"2024-01-15T10:30:00", "present",
		},
	}

	var buf bytes.Buffer
	enc, err := NewTableEncoder(&buf, FormatJSON, spec)
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

	// The artifact must be valid JSON end to end, even though the
	// decoder below never unmarshals it whole.
	var probe map[string]any
	if err := json.Unmarshal(buf.Bytes(), &probe); err != nil {
		t.Fatalf("artifact is not valid JSON: %v\n%s", err, buf.String())
	}

	dec, err := NewTableDecoder(bytes.NewReader(buf.Bytes()), FormatJSON, spec)
	if err != nil {
		t.Fatalf("NewTableDecoder: %v", err)
	}
	for i, want := range rows {
		got, err := dec.Next()
		if err != nil {
			t.Fatalf("Next() row %d: %v", i, err)
		}
		for j := range want {
			if got[j] != want[j] {
				t.Fatalf("row %d cell %d: got %#v (%T), want %#v (%T)", i, j, got[j], got[j], want[j], want[j])
			}
		}
	}
	if _, err := dec.Next(); err != io.EOF {
		t.Fatalf("Next() past last row: got err %v, want io.EOF", err)
	}
}

// TestJSONBigIntIsQuoted pins down the exact artifact bytes for a bigint
// cell: it must be a quoted JSON string, not a bare number, so that a
// JSON parser limited to float64 (JavaScript's Number, in particular)
// never silently loses precision on a value this large.
func TestJSONBigIntIsQuoted(t *testing.T) {
	spec := model.TableSpec{Columns: []model.Column{{Name: "big", SQLType: "BIGINT"}}}
	var buf bytes.Buffer
	enc, err := NewTableEncoder(&buf, FormatJSON, spec)
	if err != nil {
		t.Fatalf("NewTableEncoder: %v", err)
	}
	if err := enc.WriteRow([]model.Cell{int64(9223372036854775807)}); err != nil {
		t.Fatalf("WriteRow: %v", err)
	}
	if err := enc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !strings.Contains(buf.String(), `"9223372036854775807"`) {
		t.Fatalf("expected a quoted bigint in the artifact, got: %s", buf.String())
	}
}

// TestJSONPlainIntIsNotQuoted proves the quoting above is specific to
// bigint columns: an ordinary INT cell is a bare JSON number.
func TestJSONPlainIntIsNotQuoted(t *testing.T) {
	spec := model.TableSpec{Columns: []model.Column{{Name: "n", SQLType: "INT"}}}
	var buf bytes.Buffer
	enc, err := NewTableEncoder(&buf, FormatJSON, spec)
	if err != nil {
		t.Fatalf("NewTableEncoder: %v", err)
	}
	if err := enc.WriteRow([]model.Cell{int64(42)}); err != nil {
		t.Fatalf("WriteRow: %v", err)
	}
	if err := enc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if strings.Contains(buf.String(), `"42"`) || !strings.Contains(buf.String(), `[[42]]`) {
		t.Fatalf("expected a bare JSON number for a plain INT cell, got: %s", buf.String())
	}
}

// TestJSONColumnsWrittenOnce proves the "columns" header is written
// exactly once, before any row, and never repeated.
func TestJSONColumnsWrittenOnce(t *testing.T) {
	spec := model.TableSpec{Columns: []model.Column{{Name: "a", SQLType: "INT"}, {Name: "b", SQLType: "INT"}}}
	var buf bytes.Buffer
	enc, err := NewTableEncoder(&buf, FormatJSON, spec)
	if err != nil {
		t.Fatalf("NewTableEncoder: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := enc.WriteRow([]model.Cell{int64(i), int64(i * 2)}); err != nil {
			t.Fatalf("WriteRow: %v", err)
		}
	}
	if err := enc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if n := strings.Count(buf.String(), `"columns"`); n != 1 {
		t.Fatalf(`expected exactly one "columns" key, found %d in: %s`, n, buf.String())
	}
}

// TestJSONMismatchedColumnCountRejected proves a header whose column
// count disagrees with the caller's spec is rejected explicitly, rather
// than silently truncating or padding rows.
func TestJSONMismatchedColumnCountRejected(t *testing.T) {
	artifact := `{"columns":[{"name":"a","sql_type":"INT"}],"rows":[[1]]}`
	spec := model.TableSpec{Columns: []model.Column{{Name: "a", SQLType: "INT"}, {Name: "b", SQLType: "INT"}}}
	if _, err := NewTableDecoder(strings.NewReader(artifact), FormatJSON, spec); err == nil {
		t.Fatal("expected an error for a column-count mismatch, got nil")
	}
}

// TestJSONMalformedRowRejected proves a row whose JSON array has the
// wrong number of elements is rejected rather than silently accepted.
func TestJSONMalformedRowRejected(t *testing.T) {
	artifact := `{"columns":[{"name":"a","sql_type":"INT"}],"rows":[[1,2]]}`
	spec := model.TableSpec{Columns: []model.Column{{Name: "a", SQLType: "INT"}}}
	dec, err := NewTableDecoder(strings.NewReader(artifact), FormatJSON, spec)
	if err != nil {
		t.Fatalf("NewTableDecoder: %v", err)
	}
	if _, err := dec.Next(); err == nil {
		t.Fatal("expected an error for a row with too many cells, got nil")
	}
}

// TestJSONFailingWriterPropagated proves a write failure reaches the
// caller rather than being swallowed. The encoder buffers internally
// (bufio.Writer), so a small header/row write does not itself reach the
// underlying writer; Close's Flush is what guarantees the failure
// surfaces, and that is what this test exercises.
func TestJSONFailingWriterPropagated(t *testing.T) {
	spec := model.TableSpec{Columns: []model.Column{{Name: "a", SQLType: "INT"}}}
	fw := &failingWriter{failAfter: 0}
	enc, err := NewTableEncoder(fw, FormatJSON, spec)
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

// TestJSONEncodingNormalizedDetection proves the encoder detects, and
// Encoder.EncodingNormalized exposes, the one silent substitution this
// package does not otherwise surface: encoding/json.Marshal replacing an
// invalid UTF-8 byte sequence (something a VARCHAR/CHAR column under a
// non-UTF-8 collation can legitimately carry) with U+FFFD.
func TestJSONEncodingNormalizedDetection(t *testing.T) {
	spec := model.TableSpec{Columns: []model.Column{{Name: "s", SQLType: "VARCHAR(10)"}}}

	t.Run("valid UTF-8 never flags normalization", func(t *testing.T) {
		var buf bytes.Buffer
		enc, err := NewTableEncoder(&buf, FormatJSON, spec)
		if err != nil {
			t.Fatalf("NewTableEncoder: %v", err)
		}
		if err := enc.WriteRow([]model.Cell{"héllo"}); err != nil {
			t.Fatalf("WriteRow: %v", err)
		}
		if enc.EncodingNormalized() {
			t.Fatal("valid UTF-8 text must not be reported as normalized")
		}
	})

	t.Run("invalid UTF-8 is detected and the artifact substitutes U+FFFD", func(t *testing.T) {
		invalid := string([]byte{0x68, 0x69, 0xff, 0xfe}) // "hi" + two bytes that are not valid UTF-8
		var buf bytes.Buffer
		enc, err := NewTableEncoder(&buf, FormatJSON, spec)
		if err != nil {
			t.Fatalf("NewTableEncoder: %v", err)
		}
		if err := enc.WriteRow([]model.Cell{invalid}); err != nil {
			t.Fatalf("WriteRow: %v", err)
		}
		if !enc.EncodingNormalized() {
			t.Fatal("invalid UTF-8 text must be reported as normalized")
		}
		if err := enc.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if !strings.Contains(buf.String(), "�") {
			t.Fatalf("expected the artifact to contain the U+FFFD replacement character encoding/json itself substitutes, got: %s", buf.String())
		}
	})
}

// TestTSVNeverReportsEncodingNormalized proves EncodingNormalized's
// fallback: tsvEncoder writes a Cell's bytes unmodified (no JSON-style
// substitution is possible), so Encoder.EncodingNormalized must stay
// false for it even when a cell carries invalid UTF-8.
func TestTSVNeverReportsEncodingNormalized(t *testing.T) {
	spec := model.TableSpec{Columns: []model.Column{{Name: "s", SQLType: "VARCHAR(10)"}}}
	invalid := string([]byte{0x68, 0x69, 0xff, 0xfe})
	var buf strings.Builder
	enc, err := NewTableEncoder(&buf, FormatTSV, spec)
	if err != nil {
		t.Fatalf("NewTableEncoder: %v", err)
	}
	if err := enc.WriteRow([]model.Cell{invalid}); err != nil {
		t.Fatalf("WriteRow: %v", err)
	}
	if enc.EncodingNormalized() {
		t.Fatal("tsvEncoder never substitutes bytes; EncodingNormalized must stay false")
	}
}

// failingWriter succeeds failAfter writes, then fails every write after
// that, to exercise error propagation out of Encoder without needing a
// real broken pipe.
type failingWriter struct {
	failAfter int
	calls     int
}

func (f *failingWriter) Write(p []byte) (int, error) {
	f.calls++
	if f.calls > f.failAfter {
		return 0, errors.New("failingWriter: simulated write failure")
	}
	return len(p), nil
}
