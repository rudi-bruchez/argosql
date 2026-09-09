package sqlserver

import (
	"context"
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

// TestResolveNotFoundIsCode8 exercises Resolve's own SQL path - not just
// splitTwoPart upstream of it - against the fake driver's zero-row
// answer, catching exactly the defect a reviewer demonstrated was
// otherwise invisible without Docker: changing Resolve's Code: 8 to 9
// left every unit test green and only turned the integration suite red.
func TestResolveNotFoundIsCode8(t *testing.T) {
	conn := openFakeConn(t, fakeOptions{resolveNoRows: true})
	_, err := Resolve(context.Background(), conn, "dbo.NoSuchObject")
	if err == nil {
		t.Fatal("expected an error, got none")
	}
	var public *model.PublicError
	if !errors.As(err, &public) {
		t.Fatalf("error is not a *model.PublicError: %v", err)
	}
	if public.Code != 8 {
		t.Fatalf("got code %d, want 8", public.Code)
	}
	if public.Kind != "not_found_or_not_visible" {
		t.Fatalf("got kind %q, want not_found_or_not_visible", public.Kind)
	}
}

// TestResolveFound exercises Resolve's happy path against the fake
// driver: parameter binding (@schema/@name, checked by the fake itself -
// see namedArg in testdriver_test.go), row scanning into Object, and the
// TrimSpace on sys.objects.type's fixed-width char(2) padding.
func TestResolveFound(t *testing.T) {
	conn := openFakeConn(t, fakeOptions{resolveRow: &fakeResolvedRow{
		objectID: 42,
		schema:   "dbo",
		name:     "Orders",
		typ:      "U ", // sys.objects.type is char(2); Resolve must trim the padding
	}})
	obj, err := Resolve(context.Background(), conn, "dbo.Orders")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if obj.ID != 42 || obj.Schema != "dbo" || obj.Name != "Orders" || obj.Type != "U" {
		t.Fatalf("got %+v", obj)
	}
}
