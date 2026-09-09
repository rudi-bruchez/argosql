package sqlserver

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/rudi-bruchez/argosql/internal/model"
)

// Permission is the result of probing one capability: whether the current
// principal can do something, or whether the probe itself could not be
// answered. Measured against SQL Server 2022 RTM-CU26 (16.0.4265.3):
// HAS_PERMS_BY_NAME only ever returns two useful values, 0 and 1. It
// returns NULL exclusively when the probe itself is malformed (an
// unrecognized permission name, or the nonexistent 'SERVER' securable
// class) - never as a legitimate "I don't know" about an existing
// securable the principal cannot see. Unknown therefore never describes a
// fact about anyone's permissions; it describes a defect in this
// package's own code, and callers must present it that way (code 5,
// kind probe_malformed), never as permission information.
type Permission int

const (
	Unknown Permission = iota
	Allowed
	Denied
)

// permissionFromSQL converts HAS_PERMS_BY_NAME's single nullable integer
// result into a Permission. It is a pure conversion exercised directly by
// TestUnknownIsNotDenied on fabricated values; that test proves nothing
// about the engine by itself - see TestEveryProbeIsWellFormed in
// tests/integration/permissions_test.go for the test that does, by
// running every probe this package's registry actually emits against a
// real server and failing if any of them lands here on the NULL branch.
func permissionFromSQL(v sql.NullInt64) Permission {
	if !v.Valid {
		return Unknown
	}
	if v.Int64 == 0 {
		return Denied
	}
	return Allowed
}

const hasPermsByNameQuery = "SELECT HAS_PERMS_BY_NAME(@securable, @class, @permission)"

// Probe asks HAS_PERMS_BY_NAME whether the current principal holds
// permission on a securable identified by name and class. name is
// expected to already be resolved (see Resolve): probing an unresolved
// name measures nothing about existence, only about permission, and an
// absent object probes exactly like a denied one (see the package-level
// note on ordering in objects.go). An empty name is sent as SQL NULL
// rather than an empty string literal, which is how HAS_PERMS_BY_NAME
// itself spells "the current database" for a DATABASE-class securable -
// see AllProbes, whose database-level entries rely on exactly this.
//
// A NULL result is never surfaced as Unknown alone: it always comes back
// with a non-nil *model.PublicError (code 5, kind probe_malformed),
// because a NULL here is a defect in how this package built the probe,
// not a fact this program may report to a user as "permission denied" or
// "permission unknown".
func Probe(ctx context.Context, conn *sql.Conn, name, class, permission string) (Permission, error) {
	securable := sql.NullString{String: name, Valid: name != ""}
	var result sql.NullInt64
	err := conn.QueryRowContext(ctx, hasPermsByNameQuery,
		sql.Named("securable", securable),
		sql.Named("class", class),
		sql.Named("permission", permission),
	).Scan(&result)
	if err != nil {
		return Unknown, classifySQLError(err, "probe permission")
	}
	perm := permissionFromSQL(result)
	if perm == Unknown {
		return Unknown, &model.PublicError{
			Code:    5,
			Kind:    "probe_malformed",
			Message: fmt.Sprintf("HAS_PERMS_BY_NAME(%q, %q, %q) returned NULL: malformed probe, not a permission fact", name, class, permission),
		}
	}
	return perm, nil
}

// serverProbeQuery is deliberately this exact shape and nothing else.
// Measured: HAS_PERMS_BY_NAME has no securable at the instance level, and
// the only form that answers rather than returning NULL is
// HAS_PERMS_BY_NAME(NULL, NULL, @permission) - not a securable_class of
// 'SERVER' (which does not exist for this function and always comes back
// NULL; see TestEveryProbeIsWellFormed's deliberate negative case) and
// not an empty-string securable (which is not the same thing as SQL NULL).
const serverProbeQuery = "SELECT HAS_PERMS_BY_NAME(NULL, NULL, @permission)"

// ServerProbe asks HAS_PERMS_BY_NAME about an instance-level permission.
// There is no securable to name at this level, so unlike Probe there is
// no name parameter: see serverProbeQuery's doc comment for why the
// (NULL, NULL, @permission) form is mandatory rather than a stylistic
// choice.
func ServerProbe(ctx context.Context, conn *sql.Conn, permission string) (Permission, error) {
	var result sql.NullInt64
	err := conn.QueryRowContext(ctx, serverProbeQuery, sql.Named("permission", permission)).Scan(&result)
	if err != nil {
		return Unknown, classifySQLError(err, "probe instance permission")
	}
	perm := permissionFromSQL(result)
	if perm == Unknown {
		return Unknown, &model.PublicError{
			Code:    5,
			Kind:    "probe_malformed",
			Message: fmt.Sprintf("HAS_PERMS_BY_NAME(NULL, NULL, %q) returned NULL: malformed probe, not a permission fact", permission),
		}
	}
	return perm, nil
}

// instanceStatePermission names the single instance-level permission this
// program's "S" (instance diagnostics) bundle relies on. It depends on
// major: SQL Server 2022 (major 16) introduced VIEW SERVER PERFORMANCE
// STATE as the narrower permission the spec prefers over the older VIEW
// SERVER STATE, which remains what 2019 (major 15) has.
func instanceStatePermission(major int) string {
	if major <= 15 {
		return "VIEW SERVER STATE"
	}
	return "VIEW SERVER PERFORMANCE STATE"
}

// ProbeSpec names one permission probe this program's commands issue, so
// a test can walk the exact set the registry actually emits rather than a
// hand-maintained parallel list that can drift from it. Run's name
// argument only matters to an object-scoped probe (today, object:SELECT);
// the database- and instance-level entries ignore it and always probe the
// current database or the current server, per HAS_PERMS_BY_NAME's own
// NULL-securable contract (see Probe and ServerProbe above).
type ProbeSpec struct {
	Label string
	Run   func(ctx context.Context, conn *sql.Conn, name string) (Permission, error)
}

// AllProbes returns the exact (securable_class, permission) pairs this
// program's diagnostic commands issue against HAS_PERMS_BY_NAME, plus the
// instance-level probe ServerProbe uses, derived from the three
// permission bundles the design spec defines:
//
//   - Q (Query Store): CONNECT and VIEW DATABASE STATE, both DATABASE-class.
//   - I (inspection): Q plus VIEW DEFINITION (DATABASE-class, so catalog
//     metadata across the whole fixture database is visible) and SELECT
//     on fixture tables (OBJECT-class, resolved-name-scoped).
//   - S (instance diagnostics): I plus the version-dependent instance-level
//     state permission.
//
// It depends on major only through that last entry: the instance
// permission differs between Major 15 (VIEW SERVER STATE) and Major 16
// or later (VIEW SERVER PERFORMANCE STATE).
func AllProbes(major int) []ProbeSpec {
	instancePermission := instanceStatePermission(major)
	return []ProbeSpec{
		{
			Label: "database:CONNECT",
			Run: func(ctx context.Context, conn *sql.Conn, _ string) (Permission, error) {
				return Probe(ctx, conn, "", "DATABASE", "CONNECT")
			},
		},
		{
			Label: "database:VIEW DATABASE STATE",
			Run: func(ctx context.Context, conn *sql.Conn, _ string) (Permission, error) {
				return Probe(ctx, conn, "", "DATABASE", "VIEW DATABASE STATE")
			},
		},
		{
			Label: "database:VIEW DEFINITION",
			Run: func(ctx context.Context, conn *sql.Conn, _ string) (Permission, error) {
				return Probe(ctx, conn, "", "DATABASE", "VIEW DEFINITION")
			},
		},
		{
			Label: "object:SELECT",
			Run: func(ctx context.Context, conn *sql.Conn, name string) (Permission, error) {
				return Probe(ctx, conn, name, "OBJECT", "SELECT")
			},
		},
		{
			Label: "instance:" + instancePermission,
			Run: func(ctx context.Context, conn *sql.Conn, _ string) (Permission, error) {
				return ServerProbe(ctx, conn, instancePermission)
			},
		},
	}
}
