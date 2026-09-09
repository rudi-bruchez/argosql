package output

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strconv"

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
		enc, err := encodeJSONCell(c, e.columns[i].SQLType)
		if err != nil {
			return err
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

// encodeJSONCell renders one Cell as JSON. Every Cell type outside nil
// marshals as the JSON kind it naturally is (string, bool, number) with
// one exception: an int64 Cell for a "bigint" column is written as a
// quoted JSON string instead of a bare number, because a JSON number that
// size silently loses precision in widely used JSON parsers (JavaScript's
// in particular, limited to 2^53). A decimal/money Cell needs no such
// exception: it already arrived here as a Cell string (see cell.go), so
// it is already quoted by virtue of being a string.
func encodeJSONCell(c model.Cell, sqlType string) ([]byte, error) {
	if c == nil {
		return []byte("null"), nil
	}
	switch v := c.(type) {
	case string:
		return json.Marshal(v)
	case bool:
		return json.Marshal(v)
	case float64:
		return json.Marshal(v)
	case int64:
		if baseSQLType(sqlType) == "BIGINT" {
			return json.Marshal(strconv.FormatInt(v, 10))
		}
		return json.Marshal(v)
	default:
		return nil, fmt.Errorf("output: Cell holds unsupported type %T for JSON encoding", c)
	}
}
