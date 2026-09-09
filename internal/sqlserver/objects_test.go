package sqlserver

import (
	"errors"
	"testing"

	"github.com/rudi-bruchez/argosql/internal/model"
)

// TestResolveTwoPart pins splitTwoPart's decomposition rules: plain
// dotted names, bracket-quoted identifiers that themselves contain a dot
// or a space, the "]]" escape for a literal "]" inside a bracket, and the
// rejections - three-part names and malformed brackets - that must come
// back as a *model.PublicError with Code 2, never a panic or a silently
// wrong split. This is a pure parser test: it proves the decomposition
// logic is right, not that a real server agrees an object by that name
// exists - that is TestPermissions' job, in
// tests/integration/permissions_test.go.
func TestResolveTwoPart(t *testing.T) {
	t.Run("plain two-part name", func(t *testing.T) {
		schema, name, err := splitTwoPart("dbo.Orders")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if schema != "dbo" || name != "Orders" {
			t.Fatalf("got (%q, %q)", schema, name)
		}
	})

	t.Run("bracketed identifier with an embedded dot", func(t *testing.T) {
		schema, name, err := splitTwoPart("dbo.[Order.Detail]")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if schema != "dbo" || name != "Order.Detail" {
			t.Fatalf("got (%q, %q)", schema, name)
		}
	})

	t.Run("bracketed identifier with an embedded space", func(t *testing.T) {
		schema, name, err := splitTwoPart("[My Schema].[My Table]")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if schema != "My Schema" || name != "My Table" {
			t.Fatalf("got (%q, %q)", schema, name)
		}
	})

	t.Run("escaped closing bracket inside an identifier", func(t *testing.T) {
		schema, name, err := splitTwoPart("dbo.[Weird]]Name]")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if schema != "dbo" || name != "Weird]Name" {
			t.Fatalf("got (%q, %q)", schema, name)
		}
	})

	t.Run("one bracketed, one plain part", func(t *testing.T) {
		schema, name, err := splitTwoPart("[dbo].Orders")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if schema != "dbo" || name != "Orders" {
			t.Fatalf("got (%q, %q)", schema, name)
		}
	})

	t.Run("three-part name is rejected", func(t *testing.T) {
		_, _, err := splitTwoPart("AppDB.dbo.Orders")
		assertCode2(t, err)
	})

	t.Run("one-part name is rejected", func(t *testing.T) {
		_, _, err := splitTwoPart("Orders")
		assertCode2(t, err)
	})

	t.Run("unterminated bracket is rejected", func(t *testing.T) {
		_, _, err := splitTwoPart("dbo.[Orders")
		assertCode2(t, err)
	})

	t.Run("empty part is rejected", func(t *testing.T) {
		_, _, err := splitTwoPart("dbo.")
		assertCode2(t, err)
	})

	t.Run("empty input is rejected", func(t *testing.T) {
		_, _, err := splitTwoPart("")
		assertCode2(t, err)
	})
}

func assertCode2(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error, got none")
	}
	var public *model.PublicError
	if !errors.As(err, &public) {
		t.Fatalf("error is not a *model.PublicError: %v", err)
	}
	if public.Code != 2 {
		t.Fatalf("got code %d, want 2", public.Code)
	}
}
