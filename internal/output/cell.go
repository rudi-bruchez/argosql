// Package output is the only place in this program that turns a value
// coming back from the SQL Server driver into a model.Cell, and the two
// formats (TSV and JSON) that Cell values are serialized to and read back
// from. Every one of the project's diagnostics depends on this
// conversion being exact: a wrong decision here is wrong everywhere a
// result is displayed.
package output

import (
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/rudi-bruchez/argosql/internal/model"

	// Used only for its UniqueIdentifier type's byte-order correction
	// (Scan) and canonical string rendering (String): see convertCell's
	// "UNIQUEIDENTIFIER" case. Nothing else in this package reaches for
	// the driver directly.
	mssql "github.com/microsoft/go-mssqldb"
)

// The two artifact formats NewTableEncoder and NewTableDecoder accept,
// matched case-insensitively.
const (
	FormatTSV  = "tsv"
	FormatJSON = "json"
)

// Supported SQL Server column types, keyed by the exact string
// *sql.ColumnType.DatabaseTypeName() returns for them (measured against
// github.com/microsoft/go-mssqldb v1.11.0; see scan_test.go and
// tests/integration for the measurements this table is built from):
//
//	TINYINT, SMALLINT, INT, BIGINT     -> driver value int64       -> Cell int64
//	BIT                                -> driver value bool         -> Cell bool
//	REAL, FLOAT                        -> driver value float64      -> Cell float64
//	VARCHAR, NVARCHAR, CHAR, NCHAR,
//	TEXT, NTEXT, XML                   -> driver value string       -> Cell string
//	DECIMAL, MONEY, SMALLMONEY         -> driver value []byte        -> Cell string (exact digits, see below)
//	VARBINARY, BINARY, IMAGE           -> driver value []byte        -> Cell string ("0x"-prefixed hex)
//	UNIQUEIDENTIFIER                   -> driver value []byte        -> Cell string (canonical dashed hex)
//	DATE                               -> driver value time.Time     -> Cell string (2006-01-02)
//	TIME                               -> driver value time.Time     -> Cell string (15:04:05.9999999)
//	SMALLDATETIME, DATETIME, DATETIME2 -> driver value time.Time     -> Cell string (2006-01-02T15:04:05.9999999)
//	DATETIMEOFFSET                     -> driver value time.Time     -> Cell string (2006-01-02T15:04:05.9999999Z07:00)
//
// A DatabaseTypeName not listed here is an explicit error, never a
// fmt.Sprintf fallback: go-mssqldb v1.11.0 hands DECIMAL, MONEY,
// VARBINARY and UNIQUEIDENTIFIER back as the identical Go type ([]byte),
// so the Go type alone cannot tell them apart - only DatabaseTypeName
// can, and deciding on the Go type instead would silently turn, for
// example, a varbinary value into a number written out in text.
//
// DECIMAL, MONEY and SMALLMONEY deserve a second note: go-mssqldb's own
// decodeDecimal/decodeMoney already render the exact value as ASCII
// decimal digits (via its internal decimal.ScaleBytes) before this
// package ever sees it - there is no float64 anywhere in that path, so
// converting it here is exactly string(buf), not a reformatting of a
// floating-point approximation.
var errUnsupportedType = func(typeName string, columnName string) error {
	return fmt.Errorf("output: unsupported SQL type %q for column %q", typeName, columnName)
}

func errUnexpectedGoType(typeName, columnName string, v any) error {
	return fmt.Errorf("output: column %q: SQL type %s produced unexpected Go type %T", columnName, typeName, v)
}

// convertCell turns one driver-scanned value (the interface{} database/sql
// handed back for one column of one row) into a model.Cell, deciding any
// ambiguous case on ct.DatabaseTypeName() rather than on v's Go type.
func convertCell(v any, ct *sql.ColumnType) (model.Cell, error) {
	if v == nil {
		return nil, nil
	}
	typeName := ct.DatabaseTypeName()
	name := ct.Name()

	switch typeName {
	case "TINYINT", "SMALLINT", "INT", "BIGINT":
		i64, ok := v.(int64)
		if !ok {
			return nil, errUnexpectedGoType(typeName, name, v)
		}
		return i64, nil

	case "BIT":
		b, ok := v.(bool)
		if !ok {
			return nil, errUnexpectedGoType(typeName, name, v)
		}
		return b, nil

	case "REAL", "FLOAT":
		f, ok := v.(float64)
		if !ok {
			return nil, errUnexpectedGoType(typeName, name, v)
		}
		return f, nil

	case "VARCHAR", "NVARCHAR", "CHAR", "NCHAR", "TEXT", "NTEXT", "XML":
		s, ok := v.(string)
		if !ok {
			return nil, errUnexpectedGoType(typeName, name, v)
		}
		return s, nil

	case "DECIMAL", "MONEY", "SMALLMONEY":
		b, ok := v.([]byte)
		if !ok {
			return nil, errUnexpectedGoType(typeName, name, v)
		}
		// b is already the exact decimal digits as ASCII text (see the
		// package doc comment above): no float64 ever involved.
		return string(b), nil

	case "VARBINARY", "BINARY", "IMAGE":
		b, ok := v.([]byte)
		if !ok {
			return nil, errUnexpectedGoType(typeName, name, v)
		}
		return "0x" + hex.EncodeToString(b), nil

	case "UNIQUEIDENTIFIER":
		b, ok := v.([]byte)
		if !ok {
			return nil, errUnexpectedGoType(typeName, name, v)
		}
		var uid mssql.UniqueIdentifier
		if err := uid.Scan(b); err != nil {
			return nil, fmt.Errorf("output: column %q: decoding uniqueidentifier: %w", name, err)
		}
		// uid.String() renders the upper-case canonical 8-4-4-4-12 hex
		// form; lower-cased to match SQL Server's own CONVERT(varchar(36), ...)
		// textual convention.
		return strings.ToLower(uid.String()), nil

	case "DATE":
		t, ok := v.(time.Time)
		if !ok {
			return nil, errUnexpectedGoType(typeName, name, v)
		}
		return t.Format("2006-01-02"), nil

	case "TIME":
		t, ok := v.(time.Time)
		if !ok {
			return nil, errUnexpectedGoType(typeName, name, v)
		}
		return t.Format("15:04:05.9999999"), nil

	case "SMALLDATETIME", "DATETIME", "DATETIME2":
		t, ok := v.(time.Time)
		if !ok {
			return nil, errUnexpectedGoType(typeName, name, v)
		}
		return t.Format("2006-01-02T15:04:05.9999999"), nil

	case "DATETIMEOFFSET":
		t, ok := v.(time.Time)
		if !ok {
			return nil, errUnexpectedGoType(typeName, name, v)
		}
		return t.Format("2006-01-02T15:04:05.9999999Z07:00"), nil

	default:
		return nil, errUnsupportedType(typeName, name)
	}
}

// baseSQLType normalizes a model.Column.SQLType value (e.g. "DECIMAL(38,4)",
// "varchar(100)") to the bare upper-case type keyword used to classify it
// ("DECIMAL", "VARCHAR"), by dropping everything from the first "(" on and
// upper-casing what remains. It is the shared classifier json.go and
// decode.go use to decide, from a TableSpec column's declared SQL type,
// which Cell Go type a serialized value should be reconstructed as.
func baseSQLType(sqlType string) string {
	s := strings.ToUpper(strings.TrimSpace(sqlType))
	if i := strings.IndexByte(s, '('); i >= 0 {
		s = s[:i]
	}
	return s
}

// isIntFamily reports whether base (already normalized by baseSQLType) is
// one of the integer column types ScanRow hands back as Cell int64.
func isIntFamily(base string) bool {
	switch base {
	case "TINYINT", "SMALLINT", "INT", "BIGINT":
		return true
	}
	return false
}

// rowWriter is the per-format half of Encoder: tsv.go's tsvEncoder and
// json.go's jsonEncoder each implement it.
type rowWriter interface {
	WriteRow(row []model.Cell) error
	Close() error
}

// rowReader is the per-format half of Decoder: decode.go's tsvDecoder and
// jsonDecoder each implement it.
type rowReader interface {
	// Next returns one row, or io.EOF once the artifact is exhausted.
	Next() ([]model.Cell, error)
}

// Encoder writes one table to w, one row at a time, in the format Begin
// picked. It is the incremental counterpart of model.Sink.Row: a
// diagnostic's Begin creates one, every Row call becomes one WriteRow,
// and End calls Close.
type Encoder struct {
	inner rowWriter
}

// NewTableEncoder creates an Encoder for spec, writing to w in format
// ("tsv" or "json", case-insensitive). It writes whatever header the
// format requires (TSV's column-name line, JSON's "columns" array)
// immediately, before returning.
func NewTableEncoder(w io.Writer, format string, spec model.TableSpec) (*Encoder, error) {
	inner, err := newRowWriter(w, format, spec)
	if err != nil {
		return nil, err
	}
	return &Encoder{inner: inner}, nil
}

func newRowWriter(w io.Writer, format string, spec model.TableSpec) (rowWriter, error) {
	switch strings.ToLower(format) {
	case FormatTSV:
		return newTSVEncoder(w, spec)
	case FormatJSON:
		return newJSONEncoder(w, spec)
	default:
		return nil, fmt.Errorf("output: unsupported output format %q", format)
	}
}

// WriteRow writes one row. len(row) must equal the column count spec was
// created with.
func (e *Encoder) WriteRow(row []model.Cell) error { return e.inner.WriteRow(row) }

// Close writes whatever trailer the format requires (JSON's closing
// brackets) and flushes any buffered output. It does not close w.
func (e *Encoder) Close() error { return e.inner.Close() }

// Decoder reads one table back from an artifact Encoder wrote, one row at
// a time. It is the explicit reverse of Encoder: decode.go's machinery,
// not json.Unmarshal or a single bulk read, since an artifact can be
// larger than is reasonable to hold in memory at once.
type Decoder struct {
	inner rowReader
}

// NewTableDecoder creates a Decoder reading from r in format ("tsv" or
// "json", case-insensitive), positionally matched against spec's columns.
// It reads and validates whatever header the format carries (TSV's
// column-name line, JSON's "columns" array) immediately, rejecting a
// header that does not match spec's column count.
func NewTableDecoder(r io.Reader, format string, spec model.TableSpec) (*Decoder, error) {
	inner, err := newRowReader(r, format, spec)
	if err != nil {
		return nil, err
	}
	return &Decoder{inner: inner}, nil
}

func newRowReader(r io.Reader, format string, spec model.TableSpec) (rowReader, error) {
	switch strings.ToLower(format) {
	case FormatTSV:
		return newTSVDecoder(r, spec)
	case FormatJSON:
		return newJSONDecoder(r, spec)
	default:
		return nil, fmt.Errorf("output: unsupported output format %q", format)
	}
}

// Next returns the next row, or io.EOF once the artifact is exhausted.
func (d *Decoder) Next() ([]model.Cell, error) { return d.inner.Next() }
