//go:build integration

package integration

import (
	"context"
	"database/sql"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/rudi-bruchez/argosql/internal/output"
	"github.com/rudi-bruchez/argosql/internal/sqlserver"
)

// TestScanRowMeasuredGoTypes is the direct verification of the task 5
// brief's central measurement: against a real SQL Server (not a value
// invented for a unit test), decimal, money, varbinary and
// uniqueidentifier all arrive from a generic scan into interface{} as the
// identical Go type, []byte - so the Go type alone cannot distinguish a
// decimal from a varbinary - while datetime2 and datetimeoffset arrive as
// time.Time. If this measurement ever drifts from what a future
// go-mssqldb version does, this test is where that drift shows up.
func TestScanRowMeasuredGoTypes(t *testing.T) {
	lab := NewLab(t, os.Getenv("ASQ_TEST_IMAGE"))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, err := sqlserver.Open(ctx, lab.Profile)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	logEngineIdentity(t, lab, s.Major)

	cases := []struct {
		name     string
		query    string
		wantType reflect.Type
	}{
		{"decimal", "SELECT CAST(1.5 AS DECIMAL(10,2))", reflect.TypeOf([]byte{})},
		{"money", "SELECT CAST(1.5 AS MONEY)", reflect.TypeOf([]byte{})},
		{"varbinary", "SELECT CAST(0x1234 AS VARBINARY(10))", reflect.TypeOf([]byte{})},
		{"uniqueidentifier", "SELECT NEWID()", reflect.TypeOf([]byte{})},
		{"datetime2", "SELECT CAST(SYSDATETIME() AS DATETIME2(7))", reflect.TypeOf(time.Time{})},
		{"datetimeoffset", "SELECT CAST(SYSDATETIMEOFFSET() AS DATETIMEOFFSET(7))", reflect.TypeOf(time.Time{})},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var raw any
			row := s.Conn.QueryRowContext(ctx, c.query)
			if err := row.Scan(&raw); err != nil {
				t.Fatalf("scanning %s: %v", c.query, err)
			}
			got := reflect.TypeOf(raw)
			t.Logf("measured: %s -> Go type %v", c.name, got)
			if got != c.wantType {
				t.Fatalf("measured Go type for %s is %v, brief claims %v: the brief's measurement does not hold against this server/driver combination", c.name, got, c.wantType)
			}
		})
	}
}

// scanTwoColumns runs query (which must select exactly two columns: the
// value under test, then a VARCHAR rendering of something to compare it
// against) through output.ScanRow itself - the same function every
// diagnostic in this program uses - and returns both converted Cells.
func scanTwoColumns(t *testing.T, ctx context.Context, conn *sql.Conn, query string) (value, text any) {
	t.Helper()
	rows, err := conn.QueryContext(ctx, query)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	types, err := rows.ColumnTypes()
	if err != nil {
		t.Fatalf("column types: %v", err)
	}
	if !rows.Next() {
		t.Fatalf("no row: %v", rows.Err())
	}
	cells, err := output.ScanRow(rows, types)
	if err != nil {
		t.Fatalf("ScanRow: %v", err)
	}
	if len(cells) != 2 {
		t.Fatalf("expected 2 columns, got %d", len(cells))
	}
	return cells[0], cells[1]
}

// TestConvertCellAgainstRealServer proves output.ScanRow's conversion for
// each of the six types the brief singles out, not against a fabricated
// value, but against the identical value rendered to text by SQL Server
// itself in the same statement - two independent derivations of the same
// value that must agree exactly.
func TestConvertCellAgainstRealServer(t *testing.T) {
	lab := NewLab(t, os.Getenv("ASQ_TEST_IMAGE"))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, err := sqlserver.Open(ctx, lab.Profile)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	logEngineIdentity(t, lab, s.Major)

	t.Run("decimal(38,4) 38 digits exact, no float64 rounding", func(t *testing.T) {
		const want = "1234567890123456789012345678901234.5678"
		value, text := scanTwoColumns(t, ctx, s.Conn,
			"SELECT CAST('"+want+"' AS DECIMAL(38,4)) AS v, CAST(CAST('"+want+"' AS DECIMAL(38,4)) AS VARCHAR(50)) AS v_text")
		if value != want {
			t.Fatalf("got %#v, want the exact 38-digit value %q", value, want)
		}
		if value != text {
			t.Fatalf("conversion (%#v) disagrees with SQL Server's own CAST-to-text (%#v)", value, text)
		}
	})

	t.Run("money exact, near the MONEY range boundary", func(t *testing.T) {
		// CAST(money AS VARCHAR) with the default style (0) truncates to
		// two decimal digits - a documented SQL Server quirk, not a
		// rounding bug in this project's conversion (see "money and
		// smallmoney styles" in the CAST and CONVERT docs). Style 2 is
		// the one that keeps money's full four decimal digits, so that
		// is the oracle this test must compare against.
		value, text := scanTwoColumns(t, ctx, s.Conn,
			"SELECT CAST(922337203685477.5807 AS MONEY) AS v, CONVERT(VARCHAR(50), CAST(922337203685477.5807 AS MONEY), 2) AS v_text")
		if value != text {
			t.Fatalf("conversion (%#v) disagrees with SQL Server's own CONVERT(..., 2) (%#v)", value, text)
		}
		if value != "922337203685477.5807" {
			t.Fatalf("got %#v, want the exact 4-decimal boundary value", value)
		}
	})

	t.Run("varbinary hex-prefixed", func(t *testing.T) {
		value, _ := scanTwoColumns(t, ctx, s.Conn,
			"SELECT CAST(0x0123456789ABCDEF AS VARBINARY(8)) AS v, '' AS unused")
		want := "0x0123456789abcdef"
		if value != want {
			t.Fatalf("got %#v, want %q", value, want)
		}
	})

	t.Run("uniqueidentifier canonical form matches server's own CONVERT", func(t *testing.T) {
		value, text := scanTwoColumns(t, ctx, s.Conn,
			"DECLARE @g UNIQUEIDENTIFIER = NEWID(); SELECT @g AS v, CAST(@g AS VARCHAR(36)) AS v_text")
		got, ok := value.(string)
		if !ok {
			t.Fatalf("expected a string Cell, got %#v", value)
		}
		serverText, ok := text.(string)
		if !ok {
			t.Fatalf("expected the server-text column to be a string, got %#v", text)
		}
		if got != strings.ToLower(serverText) {
			t.Fatalf("got %q, server's own CAST(... AS VARCHAR(36)) rendered %q", got, serverText)
		}
		// Canonical shape: 8-4-4-4-12 lower-case hex.
		if len(got) != 36 || got[8] != '-' || got[13] != '-' || got[18] != '-' || got[23] != '-' {
			t.Fatalf("unexpected uniqueidentifier shape: %q", got)
		}
	})

	t.Run("datetime2 ISO 8601 text", func(t *testing.T) {
		value, _ := scanTwoColumns(t, ctx, s.Conn,
			"SELECT CAST('2024-06-15 13:45:30.1234567' AS DATETIME2(7)) AS v, '' AS unused")
		want := "2024-06-15T13:45:30.1234567"
		if value != want {
			t.Fatalf("got %#v, want %q", value, want)
		}
	})

	t.Run("datetimeoffset ISO 8601 text with offset", func(t *testing.T) {
		value, _ := scanTwoColumns(t, ctx, s.Conn,
			"SELECT CAST('2024-06-15 13:45:30.1234567 +02:30' AS DATETIMEOFFSET(7)) AS v, '' AS unused")
		want := "2024-06-15T13:45:30.1234567+02:30"
		if value != want {
			t.Fatalf("got %#v, want %q", value, want)
		}
	})
}
