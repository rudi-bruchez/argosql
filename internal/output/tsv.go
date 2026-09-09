package output

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/rudi-bruchez/argosql/internal/model"
)

// tsvEscaper escapes the four bytes that would otherwise be ambiguous in a
// tab-separated line: a literal backslash (which would otherwise collide
// with the escape sequences below, including the \N null marker), a tab
// (the field separator), and CR/LF (the line terminator). strings.Replacer
// performs all four substitutions in a single simultaneous pass over the
// input, never re-scanning text it has already emitted - so the order the
// pairs are listed in does not create a double-escaping bug the way
// applying four sequential strings.Replace calls in the wrong order
// would (escaping tab before backslash, for instance, would then escape
// the backslash that the tab substitution itself just wrote).
var tsvEscaper = strings.NewReplacer("\\", "\\\\", "\t", "\\t", "\r", "\\r", "\n", "\\n")

// tsvNullMarker is the literal, unescaped text that means "this cell is
// SQL NULL", never a value. A real string cell whose value happens to be
// the two characters backslash-N is written as \\N instead (see
// EncodeTSVCell), so the two can never collide on read-back.
const tsvNullMarker = `\N`

// EncodeTSVCell renders one Cell as the text of one TSV field, escaped.
// nil becomes the literal \N marker, unescaped, so it is never confused
// with a string value that happens to equal "\N" (which is escaped to
// \\N instead, see the doc comment on tsvEscaper).
func EncodeTSVCell(c model.Cell) string {
	if c == nil {
		return tsvNullMarker
	}
	return tsvEscaper.Replace(cellDisplayText(c))
}

// cellDisplayText renders a non-nil Cell's value as plain text, before
// TSV escaping. Cell's domain outside nil is string, bool, int64 and
// float64 (see model.Cell's doc comment); a SQL type whose exact value
// needs to be a string (decimal, varbinary, uniqueidentifier, date/time)
// already arrives here as Cell string, produced once by convertCell in
// cell.go, so this never reformats it again.
func cellDisplayText(c model.Cell) string {
	switch v := c.(type) {
	case string:
		return v
	case bool:
		return strconv.FormatBool(v)
	case int64:
		return strconv.FormatInt(v, 10)
	case float64:
		// 'g' with precision -1 renders the shortest decimal text that
		// round-trips back to the exact same float64 (strconv's
		// guarantee): no rounding is introduced here.
		return strconv.FormatFloat(v, 'g', -1, 64)
	default:
		// Cell's own contract forbids any other dynamic type; reaching
		// this means a caller built a Cell outside that contract.
		panic(fmt.Sprintf("output: Cell holds unsupported type %T", c))
	}
}

// tsvEncoder writes one table as TSV: a header line of (escaped) column
// names, then one escaped, tab-joined line per row.
type tsvEncoder struct {
	w     *bufio.Writer
	ncols int
}

func newTSVEncoder(w io.Writer, spec model.TableSpec) (*tsvEncoder, error) {
	bw := bufio.NewWriter(w)
	names := make([]string, len(spec.Columns))
	for i, c := range spec.Columns {
		names[i] = tsvEscaper.Replace(c.Name)
	}
	if err := writeTSVLine(bw, names); err != nil {
		return nil, err
	}
	return &tsvEncoder{w: bw, ncols: len(spec.Columns)}, nil
}

func (e *tsvEncoder) WriteRow(row []model.Cell) error {
	if len(row) != e.ncols {
		return fmt.Errorf("output: tsv row has %d cells, spec has %d columns", len(row), e.ncols)
	}
	fields := make([]string, len(row))
	for i, c := range row {
		fields[i] = EncodeTSVCell(c)
	}
	return writeTSVLine(e.w, fields)
}

func (e *tsvEncoder) Close() error {
	return e.w.Flush()
}

// writeTSVLine writes fields tab-joined, newline-terminated. Every line
// this package writes, header or data, always ends in "\n": decode.go's
// reader relies on that to tell a truncated artifact from one that ends
// cleanly.
func writeTSVLine(w *bufio.Writer, fields []string) error {
	for i, f := range fields {
		if i > 0 {
			if err := w.WriteByte('\t'); err != nil {
				return err
			}
		}
		if _, err := w.WriteString(f); err != nil {
			return err
		}
	}
	return w.WriteByte('\n')
}
