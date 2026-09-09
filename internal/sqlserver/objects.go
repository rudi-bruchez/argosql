package sqlserver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/rudi-bruchez/argosql/internal/model"
)

// Object is a resolved database object: its catalog object_id, its exact
// schema and object names as the catalog holds them, and its one or
// two-character sys.objects type code (for example "U" for a user table),
// trimmed of the padding sys.objects.type's fixed-width char(2) carries.
type Object struct {
	ID     int64
	Schema string
	Name   string
	Type   string
}

// resolveObjectQuery is the core of Resolve's catalog lookup. Both
// parameters are passed as distinct bind parameters, never interpolated
// into the statement text, so neither schema nor object name is ever
// built into SQL from user input.
const resolveObjectQuery = `SELECT o.object_id, s.name, o.name, o.type
FROM sys.objects AS o
JOIN sys.schemas AS s ON s.schema_id = o.schema_id
WHERE s.name = @schema AND o.name = @name`

// Resolve looks up qualified (a two-part schema.name reference, honoring
// bracket-quoted identifiers - see splitTwoPart) in sys.objects joined to
// sys.schemas.
//
// Zero rows means "not found or not visible" (code 8,
// not_found_or_not_visible) and is the default outcome for both an object
// that genuinely does not exist and one that exists but is invisible to
// the current principal: sys.objects and sys.schemas themselves only ever
// list what the current principal has metadata visibility into (via
// ownership, VIEW DEFINITION, CONTROL, or another permission on the
// object), so the catalog cannot distinguish the two cases, and Resolve
// does not invent a distinction the catalog does not expose. Resolve
// deliberately does not require VIEW DEFINITION up front: an object
// already visible to the principal through some other effective
// permission (ownership, a role membership, a GRANT on the object itself)
// still resolves, because sys.objects already reflects that visibility
// without Resolve asking for anything more specific.
//
// This is also why a caller must always call Resolve before probing a
// permission with Probe: an object that does not exist, probed directly
// with HAS_PERMS_BY_NAME, comes back Denied exactly like an object that
// exists but the principal cannot use - see the package doc on Probe and
// tests/integration/permissions_test.go's TestPermissions subtest
// "probing an absent object directly is denied, not unknown" for the
// measured demonstration. Resolving first, and only probing a name
// Resolve already found, is the only thing that keeps this program from
// answering "permission denied" about an object that was never there.
func Resolve(ctx context.Context, conn *sql.Conn, qualified string) (Object, error) {
	schema, name, err := splitTwoPart(qualified)
	if err != nil {
		return Object{}, err
	}

	var obj Object
	var rawType string
	err = conn.QueryRowContext(ctx, resolveObjectQuery,
		sql.Named("schema", schema),
		sql.Named("name", name),
	).Scan(&obj.ID, &obj.Schema, &obj.Name, &rawType)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Object{}, &model.PublicError{
				Code:    8,
				Kind:    "not_found_or_not_visible",
				Message: fmt.Sprintf("object %q not found or not visible to the current principal", qualified),
			}
		}
		return Object{}, classifySQLError(err, "resolve object")
	}
	obj.Type = strings.TrimSpace(rawType)
	return obj, nil
}

// splitTwoPart decomposes a schema.object reference into exactly two
// identifier parts, honoring SQL Server's bracket-quoting rules: inside
// a [bracketed] identifier, "]]" is a literal "]", and any "." or space
// is a literal character rather than a part separator. A name with any
// other part count - one part, three or more, or a malformed bracket -
// is rejected with a *model.PublicError{Code: 2}: this package never
// guesses at a name it cannot parse unambiguously, and it never builds a
// SQL statement out of pieces it has not validated this way first.
func splitTwoPart(qualified string) (schema, name string, err error) {
	parts, err := splitIdentifierParts(qualified)
	if err != nil {
		return "", "", err
	}
	if len(parts) != 2 {
		return "", "", &model.PublicError{
			Code:    2,
			Kind:    "invalid_argument",
			Message: fmt.Sprintf("expected a two-part name (schema.object), got %q", qualified),
		}
	}
	return parts[0], parts[1], nil
}

// splitIdentifierParts splits qualified on "."s that are not inside a
// bracket-quoted identifier, unescaping "]]" to "]" within brackets.
// Every returned part must be non-empty; an unterminated bracket or an
// empty part is reported as a malformed name, never silently dropped or
// merged into its neighbor.
func splitIdentifierParts(qualified string) ([]string, error) {
	var parts []string
	var cur strings.Builder
	runes := []rune(qualified)
	n := len(runes)
	i := 0
	for i < n {
		switch {
		case runes[i] == '[':
			i++
			closed := false
			for i < n {
				if runes[i] == ']' {
					if i+1 < n && runes[i+1] == ']' {
						cur.WriteRune(']')
						i += 2
						continue
					}
					i++
					closed = true
					break
				}
				cur.WriteRune(runes[i])
				i++
			}
			if !closed {
				return nil, &model.PublicError{
					Code:    2,
					Kind:    "invalid_argument",
					Message: fmt.Sprintf("unterminated bracketed identifier in %q", qualified),
				}
			}
		case runes[i] == '.':
			parts = append(parts, cur.String())
			cur.Reset()
			i++
		default:
			cur.WriteRune(runes[i])
			i++
		}
	}
	parts = append(parts, cur.String())

	for _, p := range parts {
		if p == "" {
			return nil, &model.PublicError{
				Code:    2,
				Kind:    "invalid_argument",
				Message: fmt.Sprintf("empty identifier part in %q", qualified),
			}
		}
	}
	return parts, nil
}
