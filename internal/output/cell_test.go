package output

import (
	"database/sql/driver"
	"testing"
	"time"

	"github.com/rudi-bruchez/argosql/internal/model"
)

// uniqueidentifierRawBytes is the raw driver.Value a generic scan into
// interface{} produces for a uniqueidentifier column, under this
// project's actual DSN (which never sets "guid conversion", so
// go-mssqldb's default - false - applies): the 16 bytes exactly as SQL
// Server stores them internally, unmodified by the driver. It is the
// cast(0x6F9619FF8B86D011B42D00C04FC964FF as uniqueidentifier) case
// go-mssqldb's own test suite measures (queries_test.go's
// "scan into interface{}" subtest), reused here rather than invented.
var uniqueidentifierRawBytes = []byte{
	0x6F, 0x96, 0x19, 0xFF, 0x8B, 0x86, 0xD0, 0x11,
	0xB4, 0x2D, 0x00, 0xC0, 0x4F, 0xC9, 0x64, 0xFF,
}

func TestConvertCellNull(t *testing.T) {
	rows, types := queryFakeRow(t, []fakeColumn{{name: "c", dbType: "INT", value: nil}})
	cells, err := ScanRow(rows, types)
	if err != nil {
		t.Fatalf("ScanRow: %v", err)
	}
	if cells[0] != nil {
		t.Fatalf("expected nil Cell, got %#v", cells[0])
	}
}

func TestConvertCellTypes(t *testing.T) {
	cases := []struct {
		name   string
		dbType string
		value  driver.Value
		want   model.Cell
	}{
		{"tinyint passthrough", "TINYINT", int64(200), int64(200)},
		{"smallint passthrough", "SMALLINT", int64(-32000), int64(-32000)},
		{"int passthrough", "INT", int64(42), int64(42)},
		{"bigint passthrough", "BIGINT", int64(9223372036854775807), int64(9223372036854775807)},
		{"bit true", "BIT", true, true},
		{"bit false", "BIT", false, false},
		{"real", "REAL", float64(0.5), float64(0.5)},
		{"float", "FLOAT", float64(3.25), float64(3.25)},
		{"varchar", "VARCHAR", "hello", "hello"},
		{"nvarchar", "NVARCHAR", "héllo", "héllo"},
		{"char", "CHAR", "x", "x"},
		{"nchar", "NCHAR", "y", "y"},
		{"text", "TEXT", "long text", "long text"},
		{"ntext", "NTEXT", "long ntext", "long ntext"},
		{"xml", "XML", "<a/>", "<a/>"},
		{"decimal exact", "DECIMAL", []byte("1234567890123456789012345678901234.5678"), "1234567890123456789012345678901234.5678"},
		{"money", "MONEY", []byte("1.2345"), "1.2345"},
		{"money negative", "MONEY", []byte("-1.2345"), "-1.2345"},
		{"smallmoney", "SMALLMONEY", []byte("99.9900"), "99.9900"},
		{"varbinary", "VARBINARY", []byte{0x12, 0x34}, "0x1234"},
		{"binary", "BINARY", []byte{0xAB, 0xCD, 0xEF}, "0xabcdef"},
		{"image", "IMAGE", []byte{0x00, 0xFF}, "0x00ff"},
		{"uniqueidentifier", "UNIQUEIDENTIFIER", uniqueidentifierRawBytes, "ff19966f-868b-11d0-b42d-00c04fc964ff"},
		{"date", "DATE", time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC), "2024-01-15"},
		{"time", "TIME", time.Date(1, 1, 1, 13, 45, 30, 123456700, time.UTC), "13:45:30.1234567"},
		{"smalldatetime", "SMALLDATETIME", time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC), "2024-01-15T10:30:00"},
		{"datetime", "DATETIME", time.Date(2024, 1, 15, 10, 30, 0, 123000000, time.UTC), "2024-01-15T10:30:00.123"},
		{"datetime2", "DATETIME2", time.Date(2024, 1, 15, 10, 30, 0, 123456700, time.UTC), "2024-01-15T10:30:00.1234567"},
		{"datetimeoffset utc", "DATETIMEOFFSET", time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC), "2024-01-15T10:30:00Z"},
		{"datetimeoffset +02:00", "DATETIMEOFFSET", time.Date(2024, 1, 15, 10, 30, 0, 0, time.FixedZone("", 2*3600)), "2024-01-15T10:30:00+02:00"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rows, types := queryFakeRow(t, []fakeColumn{{name: "c", dbType: c.dbType, value: c.value}})
			cells, err := ScanRow(rows, types)
			if err != nil {
				t.Fatalf("ScanRow: %v", err)
			}
			if cells[0] != c.want {
				t.Fatalf("got %#v (%T), want %#v (%T)", cells[0], cells[0], c.want, c.want)
			}
		})
	}
}

func TestConvertCellUnsupportedType(t *testing.T) {
	rows, types := queryFakeRow(t, []fakeColumn{{name: "c", dbType: "SQL_VARIANT", value: int64(1)}})
	if _, err := ScanRow(rows, types); err == nil {
		t.Fatal("expected an explicit error for an unsupported SQL type, got nil")
	}
}

func TestConvertCellWrongGoType(t *testing.T) {
	// A DECIMAL column's value must arrive as []byte; if it somehow
	// arrived as an int64 instead, this must be an explicit error, never
	// a silent reformat.
	rows, types := queryFakeRow(t, []fakeColumn{{name: "c", dbType: "DECIMAL", value: int64(5)}})
	if _, err := ScanRow(rows, types); err == nil {
		t.Fatal("expected an explicit error for a DECIMAL column carrying a non-[]byte value, got nil")
	}
}

func TestBaseSQLType(t *testing.T) {
	cases := []struct{ in, want string }{
		{"DECIMAL(38,4)", "DECIMAL"},
		{"varchar(100)", "VARCHAR"},
		{"BIGINT", "BIGINT"},
		{"  int  ", "INT"},
		{"", ""},
	}
	for _, c := range cases {
		if got := baseSQLType(c.in); got != c.want {
			t.Fatalf("baseSQLType(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
