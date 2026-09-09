//go:build integration

package integration

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rudi-bruchez/argosql/internal/config"
	"github.com/rudi-bruchez/argosql/internal/model"
	"github.com/rudi-bruchez/argosql/internal/sqlserver"
)

//go:embed sql/principals.sql
var principalsScript string

//go:embed sql/objects.sql
var objectsScript string

// appDBMarker is the line principals.sql's header comment documents as
// the split point between its master-context half (server logins and the
// one server-scoped GRANT) and its AppDB-context half (database users
// and every database-scoped GRANT/DENY).
const appDBMarker = "-- == APPDB =="

// splitPrincipalsScript splits script at the line that, once trimmed,
// exactly equals appDBMarker - a line match, not a raw substring search:
// principals.sql's own header comment quotes the marker text in prose
// ("Split at the line ..."), and a substring search would wrongly cut the
// script there instead of at the real marker several lines below. It is
// a hard failure (not a silent no-op) if no line matches: that would mean
// this test and principals.sql's own header comment have drifted apart.
func splitPrincipalsScript(t *testing.T, script string) (masterHalf, appDBHalf string) {
	t.Helper()
	lines := strings.Split(script, "\n")
	for i, line := range lines {
		if strings.TrimSpace(line) == appDBMarker {
			return strings.Join(lines[:i], "\n"), strings.Join(lines[i:], "\n")
		}
	}
	t.Fatalf("principals.sql: no line exactly matches marker %q", appDBMarker)
	return "", ""
}

// applyScriptOnConn runs every GO-separated batch of script, in order, on
// a single connection acquired from db and held for the whole script -
// not on db itself. principals.sql's master half needs this: its GRANT
// statement depends on the CREATE LOGIN batch just above it having
// already taken effect on the same session, an ordering database/sql's
// pool does not promise across separate ExecContext calls on db. This is
// the same concern bootstrap.sql's header comment raises about USE;
// applyBootstrap sidesteps it by never using USE, but principals.sql's
// dependency here is on login/grant ordering, not on a USE, so holding
// one connection for the script is the more direct fix.
func applyScriptOnConn(ctx context.Context, db *sql.DB, script string) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquiring connection: %w", err)
	}
	defer conn.Close()
	for i, batch := range splitBatches(script) {
		if _, err := conn.ExecContext(ctx, batch); err != nil {
			return fmt.Errorf("batch %d: %w", i+1, err)
		}
	}
	return nil
}

// applyObjects runs objects.sql against appDB. Unlike principals.sql, its
// batches carry no same-connection dependency (every statement names
// AppDB. explicitly or relies only on the pool's own default database),
// so it is applied exactly like bootstrap.sql: one ExecContext per batch,
// any pooled connection.
func applyObjects(ctx context.Context, appDB *sql.DB) error {
	for i, batch := range splitBatches(objectsScript) {
		if _, err := appDB.ExecContext(ctx, batch); err != nil {
			return fmt.Errorf("objects.sql batch %d: %w", i+1, err)
		}
	}
	return nil
}

// principalPasswords holds the harness-generated random passwords for
// the Q/I/S logins principals.sql creates. principals.sql never carries
// a literal password: these are substituted for its
// __Q_PASSWORD__/__I_PASSWORD__/__S_PASSWORD__ tokens right before the
// script is executed, the same convention NewLab already follows for the
// container's own sa account (see randomPassword in podman_test.go).
type principalPasswords struct {
	Q, I, S string
}

// applyPrincipals substitutes pw into principals.sql and applies its two
// halves against the connections they each need: masterDB for the server
// logins and the server-scoped GRANT, appDB for the database users and
// every database-scoped GRANT/DENY. objects.sql must already have been
// applied to appDB before this runs: the DENY in principals.sql's AppDB
// half targets the Restricted schema objects.sql creates.
func applyPrincipals(ctx context.Context, t *testing.T, masterDB, appDB *sql.DB, pw principalPasswords) error {
	script := principalsScript
	script = strings.ReplaceAll(script, "__Q_PASSWORD__", pw.Q)
	script = strings.ReplaceAll(script, "__I_PASSWORD__", pw.I)
	script = strings.ReplaceAll(script, "__S_PASSWORD__", pw.S)

	masterHalf, appDBHalf := splitPrincipalsScript(t, script)
	if err := applyScriptOnConn(ctx, masterDB, masterHalf); err != nil {
		return fmt.Errorf("principals.sql (master): %w", err)
	}
	if err := applyScriptOnConn(ctx, appDB, appDBHalf); err != nil {
		return fmt.Errorf("principals.sql (AppDB): %w", err)
	}
	return nil
}

// openMasterDB opens a fresh pool against the same server as lab, but
// against master rather than AppDB: principals.sql's server-level half
// (CREATE LOGIN, the server-scoped GRANT) must run in that context. It
// reuses lab.Profile's host, port and sa credentials; only Database
// changes.
func openMasterDB(t *testing.T, lab *Lab) *sql.DB {
	t.Helper()
	p := lab.Profile
	p.Database = "master"
	dsn, err := config.DSN(p)
	if err != nil {
		t.Fatalf("building master connection string: %v", err)
	}
	db, err := sql.Open("sqlserver", dsn)
	if err != nil {
		t.Fatalf("opening master pool: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// principalProfile builds the config.Profile a test principal connects
// with: same host, port and AppDB database as lab.Profile, but the named
// principal's own credentials rather than sa's.
func principalProfile(lab *Lab, username, password string) config.Profile {
	p := lab.Profile
	p.Username = username
	p.Password = password
	return p
}

// instancePermissionFor mirrors the version split sqlserver.AllProbes
// applies: VIEW SERVER STATE on major 15, VIEW SERVER PERFORMANCE STATE
// on major 16 and later.
func instancePermissionFor(major int) string {
	if major >= 16 {
		return "VIEW SERVER PERFORMANCE STATE"
	}
	return "VIEW SERVER STATE"
}

// assertPublicErrorCode fails the test unless err wraps a
// *model.PublicError with exactly code.
func assertPublicErrorCode(t *testing.T, err error, code int) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error, got none")
	}
	var public *model.PublicError
	if !errors.As(err, &public) {
		t.Fatalf("error is not a *model.PublicError: %v", err)
	}
	if public.Code != code {
		t.Fatalf("got code %d, want %d (%v)", public.Code, code, public)
	}
}

// TestPermissions exercises sqlserver.Resolve, sqlserver.Probe and
// sqlserver.ServerProbe against the Q/I/S bundles from the design spec,
// applied by principals.sql, and the fixture objects from objects.sql:
// a plain two-part name, one whose identifier contains a literal dot, an
// encrypted module, an object under an explicit schema-level DENY, and an
// object that does not exist at all.
func TestPermissions(t *testing.T) {
	lab := NewLab(t, os.Getenv("ASQ_TEST_IMAGE"))
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	var major int
	if err := lab.Admin.QueryRowContext(ctx, "SELECT CAST(SERVERPROPERTY('ProductMajorVersion') AS int)").Scan(&major); err != nil {
		t.Fatalf("reading engine major version: %v", err)
	}
	logEngineIdentity(t, lab, major)

	if err := applyObjects(ctx, lab.Admin); err != nil {
		t.Fatalf("applying objects.sql: %v", err)
	}

	pw := principalPasswords{Q: randomPassword(), I: randomPassword(), S: randomPassword()}
	masterDB := openMasterDB(t, lab)
	if err := applyPrincipals(ctx, t, masterDB, lab.Admin, pw); err != nil {
		t.Fatalf("applying principals.sql: %v", err)
	}

	qProfile := principalProfile(lab, "asq_test_q", pw.Q)
	iProfile := principalProfile(lab, "asq_test_i", pw.I)
	sProfile := principalProfile(lab, "asq_test_s", pw.S)
	instancePermission := instancePermissionFor(major)

	t.Run("Q cannot resolve dbo.Orders", func(t *testing.T) {
		sess, err := sqlserver.Open(ctx, qProfile)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer sess.Close()
		_, err = sqlserver.Resolve(ctx, sess.Conn, "dbo.Orders")
		assertPublicErrorCode(t, err, 8)
	})

	t.Run("I resolves dbo.Orders", func(t *testing.T) {
		sess, err := sqlserver.Open(ctx, iProfile)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer sess.Close()
		obj, err := sqlserver.Resolve(ctx, sess.Conn, "dbo.Orders")
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if obj.Schema != "dbo" || obj.Name != "Orders" {
			t.Fatalf("got %+v", obj)
		}
	})

	t.Run("I instance probe is denied", func(t *testing.T) {
		sess, err := sqlserver.Open(ctx, iProfile)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer sess.Close()
		perm, err := sqlserver.ServerProbe(ctx, sess.Conn, instancePermission)
		if err != nil {
			t.Fatalf("server probe: %v", err)
		}
		if perm != sqlserver.Denied {
			t.Fatalf("got %v, want Denied", perm)
		}
	})

	t.Run("I holds the 2022-only granular database permissions", func(t *testing.T) {
		if major < 16 {
			t.Skip("VIEW DATABASE PERFORMANCE STATE and VIEW SECURITY DEFINITION do not exist as grantable permissions before major 16")
		}
		sess, err := sqlserver.Open(ctx, iProfile)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer sess.Close()
		for _, permission := range []string{"VIEW DATABASE PERFORMANCE STATE", "VIEW SECURITY DEFINITION"} {
			got, err := sqlserver.Probe(ctx, sess.Conn, "", "DATABASE", permission)
			if err != nil {
				t.Fatalf("%s: %v", permission, err)
			}
			if got != sqlserver.Allowed {
				t.Fatalf("%s: got %v, want Allowed", permission, got)
			}
		}
	})

	// Q was only ever granted the older, broader VIEW DATABASE STATE (and
	// CONNECT), never either 2022-only granular permission explicitly.
	// Measured on this same server: VIEW DATABASE STATE implies VIEW
	// DATABASE PERFORMANCE STATE (it is the backward-compatible successor
	// of exactly the performance-related visibility VIEW DATABASE STATE
	// always granted - the 2022 split lets a future principal be granted
	// only the narrow one, but the broad one still covers it), while it
	// does NOT imply VIEW SECURITY DEFINITION, a capability VIEW DATABASE
	// STATE never covered. So Q is Allowed on the first and Denied on
	// the second - not Denied on both, which is what an untested
	// assumption would have guessed and what this test originally
	// asserted before being corrected against the real engine.
	t.Run("Q's VIEW DATABASE STATE implies VIEW DATABASE PERFORMANCE STATE but not VIEW SECURITY DEFINITION", func(t *testing.T) {
		if major < 16 {
			t.Skip("VIEW DATABASE PERFORMANCE STATE and VIEW SECURITY DEFINITION do not exist as grantable permissions before major 16")
		}
		sess, err := sqlserver.Open(ctx, qProfile)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer sess.Close()

		got, err := sqlserver.Probe(ctx, sess.Conn, "", "DATABASE", "VIEW DATABASE PERFORMANCE STATE")
		if err != nil {
			t.Fatalf("VIEW DATABASE PERFORMANCE STATE: %v", err)
		}
		if got != sqlserver.Allowed {
			t.Fatalf("VIEW DATABASE PERFORMANCE STATE: got %v, want Allowed (implied by VIEW DATABASE STATE)", got)
		}

		got, err = sqlserver.Probe(ctx, sess.Conn, "", "DATABASE", "VIEW SECURITY DEFINITION")
		if err != nil {
			t.Fatalf("VIEW SECURITY DEFINITION: %v", err)
		}
		if got != sqlserver.Denied {
			t.Fatalf("VIEW SECURITY DEFINITION: got %v, want Denied (not implied by VIEW DATABASE STATE, never granted explicitly)", got)
		}
	})

	t.Run("S instance probe is allowed", func(t *testing.T) {
		sess, err := sqlserver.Open(ctx, sProfile)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer sess.Close()
		perm, err := sqlserver.ServerProbe(ctx, sess.Conn, instancePermission)
		if err != nil {
			t.Fatalf("server probe: %v", err)
		}
		if perm != sqlserver.Allowed {
			t.Fatalf("got %v, want Allowed", perm)
		}
	})

	t.Run("encrypted module still resolves", func(t *testing.T) {
		sess, err := sqlserver.Open(ctx, iProfile)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer sess.Close()
		obj, err := sqlserver.Resolve(ctx, sess.Conn, "dbo.EncryptedProc")
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if obj.Name != "EncryptedProc" {
			t.Fatalf("got %+v", obj)
		}
	})

	t.Run("name with brackets resolves", func(t *testing.T) {
		sess, err := sqlserver.Open(ctx, iProfile)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer sess.Close()
		obj, err := sqlserver.Resolve(ctx, sess.Conn, "dbo.[Order.Detail]")
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if obj.Name != "Order.Detail" {
			t.Fatalf("got %+v", obj)
		}
	})

	t.Run("object under a schema-level DENY probes denied", func(t *testing.T) {
		sess, err := sqlserver.Open(ctx, iProfile)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer sess.Close()
		// I holds VIEW DEFINITION database-wide, so it can still resolve
		// Restricted.Secret: VIEW DEFINITION governs catalog visibility,
		// a different permission from SELECT.
		obj, err := sqlserver.Resolve(ctx, sess.Conn, "Restricted.Secret")
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		perm, err := sqlserver.Probe(ctx, sess.Conn, obj.Schema+"."+obj.Name, "OBJECT", "SELECT")
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if perm != sqlserver.Denied {
			t.Fatalf("got %v, want Denied", perm)
		}
	})

	t.Run("absent object resolves to not_found_or_not_visible", func(t *testing.T) {
		sess, err := sqlserver.Open(ctx, sProfile)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer sess.Close()
		_, err = sqlserver.Resolve(ctx, sess.Conn, "dbo.NoSuchObject")
		assertPublicErrorCode(t, err, 8)
	})

	// This is the measured fact that makes the "resolve before probe"
	// ordering mandatory rather than a presentation preference: probing a
	// name directly, without resolving it first, cannot tell "this does
	// not exist" apart from "this exists and you may not see it" - both
	// come back 0 from HAS_PERMS_BY_NAME, which this package converts to
	// Denied. If a caller skipped Resolve and probed dbo.NoSuchObject
	// directly, it would see exactly this same Denied and could wrongly
	// report "permission denied" about an object that was never there.
	t.Run("probing an absent object directly is denied, not unknown", func(t *testing.T) {
		sess, err := sqlserver.Open(ctx, sProfile)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer sess.Close()
		perm, err := sqlserver.Probe(ctx, sess.Conn, "dbo.NoSuchObject", "OBJECT", "SELECT")
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if perm != sqlserver.Denied {
			t.Fatalf("got %v, want Denied: an absent object must probe exactly like a denied one", perm)
		}
	})
}

// TestEveryProbeIsWellFormed is the test that actually carries this
// package's guarantee, per the task 8 brief: it walks sqlserver.AllProbes
// - the exact registry the diagnostic commands built on this package will
// issue - and runs every one of them against a real server. A result of
// sqlserver.Unknown here is never a fact about the sa principal's
// permissions (sa can see and do everything); it is proof that one of
// this package's own probes is malformed. The deliberate negative case
// (securable_class 'SERVER', which does not exist for HAS_PERMS_BY_NAME)
// exists so this test cannot pass by some future change quietly erasing
// the Unknown/malformed distinction altogether.
func TestEveryProbeIsWellFormed(t *testing.T) {
	lab := NewLab(t, os.Getenv("ASQ_TEST_IMAGE"))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := applyObjects(ctx, lab.Admin); err != nil {
		t.Fatalf("applying objects.sql: %v", err)
	}

	sess, err := sqlserver.Open(ctx, lab.Profile)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	logEngineIdentity(t, lab, sess.Major)

	probes := sqlserver.AllProbes(sess.Major)
	// A registry that silently lost an entry - for example the two
	// 2022-only granular permissions (VIEW DATABASE PERFORMANCE STATE,
	// VIEW SECURITY DEFINITION) quietly not being emitted on major 16 -
	// would otherwise pass this test by ranging over a shorter list and
	// finding nothing wrong in it. Pinning the count on both images
	// forces that omission to surface here rather than disappearing.
	wantCount := 5
	if sess.Major >= 16 {
		wantCount = 7
	}
	if len(probes) != wantCount {
		t.Fatalf("AllProbes(%d) returned %d entries, want %d: %v", sess.Major, len(probes), wantCount, probes)
	}

	for _, pr := range probes {
		got, err := pr.Run(ctx, sess.Conn, "dbo.Orders")
		if err != nil {
			t.Fatalf("%s: %v", pr.Label, err)
		}
		if got == sqlserver.Unknown {
			t.Fatalf("%s rend NULL sur un serveur réel : sonde malformée", pr.Label)
		}
	}

	// Deliberate negative case: measured, HAS_PERMS_BY_NAME always
	// returns NULL for securable_class 'SERVER' (it is not a valid class
	// for this function, unlike for fn_my_permissions). If this assertion
	// ever saw anything but Unknown, the distinction this whole package
	// is built on would have silently stopped holding. Checked against
	// both the Permission value and the accompanying error's Code/Kind:
	// a reviewer proved that checking only perm != Unknown and err == nil
	// (without looking at what Code or Kind the error actually carried)
	// let Probe's malformed-error mapping drift to a wrong code and kind
	// without this test - or any test - noticing.
	perm, err := sqlserver.Probe(ctx, sess.Conn, "", "SERVER", "VIEW SERVER STATE")
	if perm != sqlserver.Unknown {
		t.Fatalf("class SERVER: got %v, want Unknown (err=%v)", perm, err)
	}
	assertPublicErrorCode(t, err, 5)
	var public *model.PublicError
	if errors.As(err, &public) && public.Kind != "probe_malformed" {
		t.Fatalf("class SERVER: got kind %q, want probe_malformed", public.Kind)
	}
}
