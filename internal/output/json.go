package output

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"unicode/utf8"

	"github.com/rudi-bruchez/argosql/internal/model"
)

// jsonEncoder writes one table as a single JSON object:
//
//	{"columns":[{"name":"...","sql_type":"..."},...],"rows":[[...],[...]]}
//
// "columns" is written once, from spec, before the first row; "rows" is a
// positional array of arrays, one row at a time, never buffered whole -
// matching the collector's push model (Begin/Row/End) one for one.
type jsonEncoder struct {
	w        *bufio.Writer
	columns  []model.Column
	wroteAny bool

	// encodingNormalized is set once a string Cell this encoder has
	// written was not valid UTF-8 - a VARCHAR/CHAR column under a
	// non-UTF-8 collation can carry exactly this, and encoding/json's
	// Marshal silently substitutes the invalid bytes with U+FFFD to keep
	// its output valid JSON (see encodeJSONCell). This package only
	// detects and exposes that substitution; see EncodingNormalized.
	encodingNormalized bool
}

func newJSONEncoder(w io.Writer, spec model.TableSpec) (*jsonEncoder, error) {
	bw := bufio.NewWriter(w)
	if _, err := bw.WriteString(`{"columns":`); err != nil {
		return nil, err
	}
	colsJSON, err := json.Marshal(spec.Columns)
	if err != nil {
		return nil, fmt.Errorf("output: marshaling columns: %w", err)
	}
	if _, err := bw.Write(colsJSON); err != nil {
		return nil, err
	}
	if _, err := bw.WriteString(`,"rows":[`); err != nil {
		return nil, err
	}
	return &jsonEncoder{w: bw, columns: spec.Columns}, nil
}

func (e *jsonEncoder) WriteRow(row []model.Cell) error {
	if len(row) != len(e.columns) {
		return fmt.Errorf("output: json row has %d cells, spec has %d columns", len(row), len(e.columns))
	}
	if e.wroteAny {
		if err := e.w.WriteByte(','); err != nil {
			return err
		}
	}
	e.wroteAny = true
	if err := e.w.WriteByte('['); err != nil {
		return err
	}
	for i, c := range row {
		if i > 0 {
			if err := e.w.WriteByte(','); err != nil {
				return err
			}
		}
		enc, normalized, err := encodeJSONCell(c, e.columns[i].SQLType)
		if err != nil {
			return err
		}
		if normalized {
			e.encodingNormalized = true
		}
		if _, err := e.w.Write(enc); err != nil {
			return err
		}
	}
	return e.w.WriteByte(']')
}

func (e *jsonEncoder) Close() error {
	if _, err := e.w.WriteString("]}"); err != nil {
		return err
	}
	return e.w.Flush()
}

// EncodingNormalized reports whether any string Cell written so far was
// not valid UTF-8. This encoder only detects and exposes that fact; it
// never refuses to write the value and never decides what to do about
// it - deciding belongs to whichever later stage holds a model.Sink (and
// can therefore emit a model.Notice carrying model.KindEncodingNormalized),
// not to this encoder. See jsonEncoder.encodingNormalized's doc comment.
func (e *jsonEncoder) EncodingNormalized() bool {
	return e.encodingNormalized
}

// encodeJSONCell renders one Cell as JSON, and reports whether doing so
// silently substituted invalid UTF-8 bytes. Every Cell type outside nil
// marshals as the JSON kind it naturally is (string, bool, number) with
// one exception: an int64 Cell for a "bigint" column is written as a
// quoted JSON string instead of a bare number, because a JSON number that
// size silently loses precision in widely used JSON parsers (JavaScript's
// in particular, limited to 2^53). A decimal/money Cell needs no such
// exception: it already arrived here as a Cell string (see cell.go), so
// it is already quoted by virtue of being a string.
//
// A string Cell is checked against utf8.ValidString before marshaling:
// encoding/json's own Marshal silently replaces any invalid byte with the
// Unicode replacement rune (U+FFFD) to guarantee its output is valid
// JSON, with no signal that it did so. That silent substitution is
// exactly what this check surfaces.
func encodeJSONCell(c model.Cell, sqlType string) (encoded []byte, normalized bool, err error) {
	if c == nil {
		return []byte("null"), false, nil
	}
	switch v := c.(type) {
	case string:
		enc, err := json.Marshal(v)
		if err != nil {
			return nil, false, err
		}
		return enc, !utf8.ValidString(v), nil
	case bool:
		enc, err := json.Marshal(v)
		return enc, false, err
	case float64:
		enc, err := json.Marshal(v)
		return enc, false, err
	case int64:
		if baseSQLType(sqlType) == "BIGINT" {
			enc, err := json.Marshal(strconv.FormatInt(v, 10))
			return enc, false, err
		}
		enc, err := json.Marshal(v)
		return enc, false, err
	default:
		return nil, false, fmt.Errorf("output: Cell holds unsupported type %T for JSON encoding", c)
	}
}
