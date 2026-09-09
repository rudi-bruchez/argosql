// decode.go is the explicit reverse of tsv.go's and json.go's encoders:
// the reader task 7 needs to read an artifact back. It is deliberately
// never a single bulk unmarshal of the whole artifact - JSON is read
// through json.Decoder's token stream, TSV through a buffered line reader
// and the inverse of tsv.go's escape machine - because an artifact this
// program writes can be large enough that holding a decoded copy of all
// of it in memory at once is exactly what this package elsewhere tries to
// avoid.
package output

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/rudi-bruchez/argosql/internal/model"
)

// -- TSV --

// tsvDecoder reads back what tsvEncoder wrote: a header line of escaped
// column names (read and checked against spec.Columns' length up front),
// then one escaped, tab-joined data line per row.
type tsvDecoder struct {
	br    *bufio.Reader
	types []string // baseSQLType(spec.Columns[i].SQLType), positional
}

func newTSVDecoder(r io.Reader, spec model.TableSpec) (*tsvDecoder, error) {
	br := bufio.NewReader(r)
	header, err := readTSVLine(br)
	if err != nil {
		if err == io.EOF {
			return nil, fmt.Errorf("output: malformed tsv artifact: missing header line")
		}
		return nil, fmt.Errorf("output: reading tsv header: %w", err)
	}
	fields := splitTSVLine(header)
	if len(fields) != len(spec.Columns) {
		return nil, fmt.Errorf("output: malformed tsv artifact: header has %d columns, spec has %d", len(fields), len(spec.Columns))
	}
	types := make([]string, len(spec.Columns))
	for i, c := range spec.Columns {
		types[i] = baseSQLType(c.SQLType)
	}
	return &tsvDecoder{br: br, types: types}, nil
}

func (d *tsvDecoder) Next() ([]model.Cell, error) {
	line, err := readTSVLine(d.br)
	if err != nil {
		return nil, err // io.EOF propagates as-is
	}
	fields := splitTSVLine(line)
	if len(fields) != len(d.types) {
		return nil, fmt.Errorf("output: malformed tsv artifact: row has %d fields, want %d", len(fields), len(d.types))
	}

	row := make([]model.Cell, len(fields))
	for i, raw := range fields {
		cell, err := decodeTSVField(raw, d.types[i])
		if err != nil {
			return nil, err
		}
		row[i] = cell
	}
	return row, nil
}

// splitTSVLine splits a line on literal tab bytes. This is safe and
// unambiguous because tsvEscaper (tsv.go) never lets a literal tab byte
// survive into an encoded field: a tab that is part of a value's text is
// always rewritten to the two-character sequence \t before writing, so
// every literal tab byte left in the line is a genuine field separator.
func splitTSVLine(line string) []string {
	return strings.Split(line, "\t")
}

// readTSVLine reads one line, stripping its trailing "\n". It returns
// io.EOF only when the stream ends exactly on a line boundary (no partial
// line pending); a line present but missing its terminator - the
// artifact ended mid-line - is reported as malformed instead, since every
// line tsvEncoder writes, including the last, always ends in "\n".
func readTSVLine(br *bufio.Reader) (string, error) {
	line, err := br.ReadString('\n')
	if err != nil {
		if err == io.EOF {
			if line == "" {
				return "", io.EOF
			}
			return "", fmt.Errorf("output: malformed tsv artifact: truncated line (missing trailing newline)")
		}
		return "", err
	}
	return strings.TrimSuffix(line, "\n"), nil
}

// decodeTSVField turns one still-escaped field into a Cell. The \N null
// check happens on the raw, not-yet-unescaped text deliberately: the null
// marker is written unescaped (see EncodeTSVCell), while a real string
// cell whose value is the two characters \N is written escaped as \\N,
// so checking before unescaping is what keeps the two from colliding.
func decodeTSVField(raw, baseType string) (model.Cell, error) {
	if raw == tsvNullMarker {
		return nil, nil
	}
	text, err := unescapeTSV(raw)
	if err != nil {
		return nil, err
	}
	return cellFromText(text, baseType)
}

// unescapeTSV is the explicit inverse of tsvEscaper: every backslash
// must be followed by one of \, t, r or n, or the artifact is malformed.
// It scans byte by byte rather than rune by rune deliberately: backslash
// is an ASCII byte and can never appear as a continuation byte of a
// multi-byte UTF-8 sequence, so byte-level scanning never misinterprets
// non-ASCII text - this is what makes Unicode round-trip safely.
func unescapeTSV(s string) (string, error) {
	if !strings.ContainsRune(s, '\\') {
		return s, nil
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '\\' {
			b.WriteByte(c)
			continue
		}
		i++
		if i >= len(s) {
			return "", fmt.Errorf("output: malformed tsv artifact: trailing backslash")
		}
		switch s[i] {
		case '\\':
			b.WriteByte('\\')
		case 't':
			b.WriteByte('\t')
		case 'r':
			b.WriteByte('\r')
		case 'n':
			b.WriteByte('\n')
		default:
			return "", fmt.Errorf("output: malformed tsv artifact: invalid escape sequence \\%c", s[i])
		}
	}
	return b.String(), nil
}

// cellFromText reconstructs a Cell from TSV field text (or a decoded JSON
// string token), narrowing it to int64/bool/float64 only for column types
// ScanRow itself would have produced that Go type for; every other type -
// including decimal, money, varbinary, uniqueidentifier and every
// date/time type, all of which ScanRow already turns into an exact string
// - is returned as the text itself. An unrecognized or empty baseType
// also falls through to plain text: unlike cell.go's convertCell (which
// must reject a type it cannot safely disambiguate from raw driver
// bytes), text read back from an artifact is never ambiguous, so there is
// nothing unsafe about preserving it as a string rather than guessing a
// narrower type for it.
func cellFromText(text, baseType string) (model.Cell, error) {
	switch {
	case isIntFamily(baseType):
		v, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("output: malformed artifact: decoding %s value %q: %w", baseType, text, err)
		}
		return v, nil
	case baseType == "BIT":
		v, err := strconv.ParseBool(text)
		if err != nil {
			return nil, fmt.Errorf("output: malformed artifact: decoding BIT value %q: %w", text, err)
		}
		return v, nil
	case baseType == "REAL" || baseType == "FLOAT":
		v, err := strconv.ParseFloat(text, 64)
		if err != nil {
			return nil, fmt.Errorf("output: malformed artifact: decoding %s value %q: %w", baseType, text, err)
		}
		return v, nil
	default:
		return text, nil
	}
}

// -- JSON --

// jsonDecoder reads back what jsonEncoder wrote, through json.Decoder's
// token stream: "columns" is decoded once (it is small - one entry per
// column, never per row) and checked against spec, then "rows" is walked
// one row array at a time.
type jsonDecoder struct {
	dec   *json.Decoder
	types []string // baseSQLType(spec.Columns[i].SQLType), positional
	done  bool
}

func newJSONDecoder(r io.Reader, spec model.TableSpec) (*jsonDecoder, error) {
	dec := json.NewDecoder(r)
	dec.UseNumber()

	if err := expectJSONDelim(dec, '{'); err != nil {
		return nil, err
	}
	if err := expectJSONKey(dec, "columns"); err != nil {
		return nil, err
	}

	var columns []model.Column
	if err := dec.Decode(&columns); err != nil {
		return nil, fmt.Errorf("output: malformed json artifact: decoding columns: %w", err)
	}
	if len(columns) != len(spec.Columns) {
		return nil, fmt.Errorf("output: malformed json artifact: %d columns, spec has %d", len(columns), len(spec.Columns))
	}
	if err := expectJSONKey(dec, "rows"); err != nil {
		return nil, err
	}
	if err := expectJSONDelim(dec, '['); err != nil {
		return nil, err
	}

	types := make([]string, len(spec.Columns))
	for i, c := range spec.Columns {
		types[i] = baseSQLType(c.SQLType)
	}
	return &jsonDecoder{dec: dec, types: types}, nil
}

func (d *jsonDecoder) Next() ([]model.Cell, error) {
	if d.done {
		return nil, io.EOF
	}
	tok, err := d.dec.Token()
	if err != nil {
		return nil, fmt.Errorf("output: malformed json artifact: %w", err)
	}
	if delim, ok := tok.(json.Delim); ok && delim == ']' {
		// The rows array is closed, but io.EOF must mean "the whole
		// artifact was read", not merely "the rows array looked
		// closed". A disk-full write or a cut connection truncates a
		// file exactly at a byte boundary like this one; without this
		// check, a decoder reading that truncated file sees the same
		// "]" a complete file would have had at this position, and
		// returns a clean io.EOF indistinguishable from having read
		// every row. Requiring and consuming the artifact's closing
		// "}" is what turns that silent partial read into an explicit
		// error instead.
		if err := expectJSONDelim(d.dec, '}'); err != nil {
			return nil, fmt.Errorf("output: malformed json artifact: truncated after the rows array (missing closing brace): %w", err)
		}
		d.done = true
		return nil, io.EOF
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '[' {
		return nil, fmt.Errorf("output: malformed json artifact: expected a row array, got %v", tok)
	}

	row := make([]model.Cell, len(d.types))
	for i := range row {
		cell, err := decodeJSONCellToken(d.dec, d.types[i])
		if err != nil {
			return nil, err
		}
		row[i] = cell
	}

	tok, err = d.dec.Token()
	if err != nil {
		return nil, fmt.Errorf("output: malformed json artifact: %w", err)
	}
	if delim, ok := tok.(json.Delim); !ok || delim != ']' {
		return nil, fmt.Errorf("output: malformed json artifact: row has more cells than the %d columns declared", len(d.types))
	}
	return row, nil
}

// decodeJSONCellToken reads exactly one JSON value token and turns it
// into a Cell. A quoted string is narrowed to int64 only for a "bigint"
// column - the one case jsonEncoder deliberately quotes a number as a
// string (see json.go) - and left as a Cell string otherwise, since every
// other SQL type that needs string form (decimal, money, varbinary,
// uniqueidentifier, date/time) already is one.
func decodeJSONCellToken(dec *json.Decoder, baseType string) (model.Cell, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("output: malformed json artifact: %w", err)
	}
	switch v := tok.(type) {
	case nil:
		return nil, nil
	case bool:
		return v, nil
	case json.Number:
		s := string(v)
		if strings.ContainsAny(s, ".eE") {
			f, err := v.Float64()
			if err != nil {
				return nil, fmt.Errorf("output: malformed json artifact: %w", err)
			}
			return f, nil
		}
		i, err := v.Int64()
		if err != nil {
			return nil, fmt.Errorf("output: malformed json artifact: %w", err)
		}
		return i, nil
	case string:
		if isIntFamily(baseType) {
			i, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("output: malformed json artifact: invalid %s value %q: %w", baseType, v, err)
			}
			return i, nil
		}
		return v, nil
	default:
		return nil, fmt.Errorf("output: malformed json artifact: unexpected token %v (%T)", tok, tok)
	}
}

func expectJSONDelim(dec *json.Decoder, want json.Delim) error {
	tok, err := dec.Token()
	if err != nil {
		return fmt.Errorf("output: malformed json artifact: %w", err)
	}
	d, ok := tok.(json.Delim)
	if !ok || d != want {
		return fmt.Errorf("output: malformed json artifact: expected %q, got %v", want, tok)
	}
	return nil
}

func expectJSONKey(dec *json.Decoder, want string) error {
	tok, err := dec.Token()
	if err != nil {
		return fmt.Errorf("output: malformed json artifact: %w", err)
	}
	s, ok := tok.(string)
	if !ok || s != want {
		return fmt.Errorf("output: malformed json artifact: expected key %q, got %v", want, tok)
	}
	return nil
}
