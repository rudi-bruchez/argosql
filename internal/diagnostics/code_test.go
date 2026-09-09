package diagnostics

import (
	"context"
	"errors"
	"testing"

	"github.com/rudi-bruchez/argosql/internal/model"
)

// TestCodeUnresolvedNameReturnsEight is dispatch cassure 1's own
// target for "obj code": an unresolved name must fail at code 8
// before Code ever probes VIEW DEFINITION - a permission-dependent
// query that, on an absent name, HAS_PERMS_BY_NAME answers Denied
// exactly like a real denial (see sqlserver/objects.go's own doc
// comment on Probe). If Code probed before resolving, this exact
// assertion would observe code 4/permission_denied instead of 8.
func TestCodeUnresolvedNameReturnsEight(t *testing.T) {
	// permProbeResponse(0) matches what a REAL unresolved name's own
	// HAS_PERMS_BY_NAME probe returns (Denied, not Unknown - see
	// sqlserver/objects.go's own doc comment on Probe): present here
	// so that a reordering bug (probing before resolving) is actually
	// exercised by this fake driver, rather than accidentally masked
	// by an absent responder that correct code never reaches anyway.
	conn := &fakeObjConn{responses: []objQueryResponse{resolveNotFoundResponse(), permProbeResponse(int64(0))}}
	sess := newFakeObjSession(t, conn)

	err := Code(context.Background(), sess, "dbo.Missing", &objCaptureSink{})
	var pub *model.PublicError
	if !errors.As(err, &pub) {
		t.Fatalf("Code: want *model.PublicError, got %#v", err)
	}
	if pub.Code != 8 || pub.Kind != "not_found_or_not_visible" {
		t.Fatalf("Code on an unresolved name: want code 8/not_found_or_not_visible, got code %d/%s", pub.Code, pub.Kind)
	}
}

// TestCodeAvailable is the definitionStateAvailable path: a visible
// module with a non-null definition writes ModuleTable with real cell
// values (not merely a row count - the recurring defect this project
// has paid for three times) and exports the definition as a .sql
// artifact.
func TestCodeAvailable(t *testing.T) {
	definition := "CREATE PROCEDURE dbo.GetOrderTotal\nAS\nBEGIN\n\tSELECT 1;\nEND"
	conn := &fakeObjConn{responses: []objQueryResponse{
		resolveFoundResponse(601, "dbo", "GetOrderTotal", "P"),
		moduleRowResponse(true, definition, false),
	}}
	sess := newFakeObjSession(t, conn)
	sink := &objCaptureSink{}

	if err := Code(context.Background(), sess, "dbo.GetOrderTotal", sink); err != nil {
		t.Fatalf("Code: %v", err)
	}

	tbl := sink.table("module")
	if tbl == nil || len(tbl.rows) != 1 {
		t.Fatalf("module: want exactly one row, got %#v", tbl)
	}
	row := tbl.rows[0]
	if row[3] != definitionStateAvailable {
		t.Fatalf("definition_state cell: got %#v, want %q", row[3], definitionStateAvailable)
	}
	if row[1] != "dbo.GetOrderTotal" {
		t.Fatalf("name cell: got %#v, want %q", row[1], "dbo.GetOrderTotal")
	}
	if row[4] != int64(5) {
		t.Fatalf("line_count cell: got %#v, want int64(5) (5 lines)", row[4])
	}
	if len(sink.files) != 1 || string(sink.files[0].content) != definition {
		t.Fatalf("exported artifact content: got %#v, want %q", sink.files, definition)
	}
	if sink.files[0].suffix != ".sql" {
		t.Fatalf("exported artifact suffix: got %q, want %q", sink.files[0].suffix, ".sql")
	}
}

// TestCodeEncrypted is the definitionStateEncrypted path (design spec
// line 204: "A confirmed encrypted module yields encrypted, code 4,
// no definition artifact"): no table row, no artifact, error Kind is
// exactly "encrypted".
func TestCodeEncrypted(t *testing.T) {
	conn := &fakeObjConn{responses: []objQueryResponse{
		resolveFoundResponse(602, "dbo", "EncryptedProc", "P"),
		moduleRowResponse(true, nil, true),
	}}
	sess := newFakeObjSession(t, conn)
	sink := &objCaptureSink{}

	err := Code(context.Background(), sess, "dbo.EncryptedProc", sink)
	var pub *model.PublicError
	if !errors.As(err, &pub) {
		t.Fatalf("Code: want *model.PublicError, got %#v", err)
	}
	if pub.Code != 4 || pub.Kind != definitionStateEncrypted {
		t.Fatalf("Code on an encrypted module: want code 4/%s, got code %d/%s", definitionStateEncrypted, pub.Code, pub.Kind)
	}
	if len(sink.tables) != 0 {
		t.Fatalf("encrypted module: want no table row, got %#v", sink.tables)
	}
	if len(sink.files) != 0 {
		t.Fatalf("encrypted module: want no artifact, got %#v", sink.files)
	}
}

// TestCodeOnNonModuleObject is fix-0's own point 2: obj code called
// on a non-module object (a table, most commonly) must report
// definitionStateDefinitionUnavailable at code 4, never code 5.
// Measured directly against a real engine (tests/integration) before
// this fix: OBJECTPROPERTYEX(object_id(dbo.Orders),'IsEncrypted')
// returns NULL, and Code used to fall into unexpectedCell's generic
// execution failure (code 5, "unexpected value from
// sys.database_query_store_options" - a message copied from a
// different query entirely) instead of the closed vocabulary's own
// default state.
func TestCodeOnNonModuleObject(t *testing.T) {
	conn := &fakeObjConn{responses: []objQueryResponse{
		resolveFoundResponse(606, "dbo", "Orders", "U"),
		moduleRowResponse(true, nil, nil),
	}}
	sess := newFakeObjSession(t, conn)
	sink := &objCaptureSink{}

	err := Code(context.Background(), sess, "dbo.Orders", sink)
	var pub *model.PublicError
	if !errors.As(err, &pub) {
		t.Fatalf("Code: want *model.PublicError, got %#v", err)
	}
	if pub.Code != 4 || pub.Kind != definitionStateDefinitionUnavailable {
		t.Fatalf("Code on a non-module object: want code 4/%s, got code %d/%s", definitionStateDefinitionUnavailable, pub.Code, pub.Kind)
	}
	if len(sink.tables) != 0 {
		t.Fatalf("non-module object: want no table row, got %#v", sink.tables)
	}
}

// TestCodePermissionDenied is the definitionStatePermissionDenied
// path: a null definition on a visible, unencrypted module, where a
// direct probe of VIEW DEFINITION on the object confirms a denial.
// Design spec line 204 requires this state be reported ONLY when the
// denial is confirmed, never as a default - see
// TestCodeDefinitionUnavailableDoesNotInventPermissionDenied for the
// case this test's own probe result must NOT produce.
func TestCodePermissionDenied(t *testing.T) {
	conn := &fakeObjConn{responses: []objQueryResponse{
		resolveFoundResponse(603, "dbo", "DeniedDefinitionProc", "P"),
		moduleRowResponse(true, nil, false),
		permProbeResponse(int64(0)), // HAS_PERMS_BY_NAME -> 0 -> Denied
	}}
	sess := newFakeObjSession(t, conn)
	sink := &objCaptureSink{}

	err := Code(context.Background(), sess, "dbo.DeniedDefinitionProc", sink)
	var pub *model.PublicError
	if !errors.As(err, &pub) {
		t.Fatalf("Code: want *model.PublicError, got %#v", err)
	}
	if pub.Code != 4 || pub.Kind != definitionStatePermissionDenied {
		t.Fatalf("Code on a confirmed denial: want code 4/%s, got code %d/%s", definitionStatePermissionDenied, pub.Code, pub.Kind)
	}
	if len(sink.tables) != 0 {
		t.Fatalf("permission_denied: want no table row, got %#v", sink.tables)
	}
}

// TestCodeDefinitionUnavailableDoesNotInventPermissionDenied is
// design spec line 204's other half: "Otherwise a null definition
// yields definition_unavailable, code 4, without inventing its
// cause." A null definition whose VIEW DEFINITION probe comes back
// ALLOWED (not denied) must never be reported as permission_denied -
// that would invent a cause this function cannot confirm. Half of
// this task's brief for definitionState is exactly this branch; the
// brief names only "encrypted" and defers the rest to the spec, which
// is why this assertion exists.
func TestCodeDefinitionUnavailableDoesNotInventPermissionDenied(t *testing.T) {
	conn := &fakeObjConn{responses: []objQueryResponse{
		resolveFoundResponse(604, "dbo", "SomeView", "V"),
		moduleRowResponse(true, nil, false),
		permProbeResponse(int64(1)), // HAS_PERMS_BY_NAME -> 1 -> Allowed
	}}
	sess := newFakeObjSession(t, conn)
	sink := &objCaptureSink{}

	err := Code(context.Background(), sess, "dbo.SomeView", sink)
	var pub *model.PublicError
	if !errors.As(err, &pub) {
		t.Fatalf("Code: want *model.PublicError, got %#v", err)
	}
	if pub.Kind == definitionStatePermissionDenied {
		t.Fatalf("Code must not report permission_denied when the probe found it Allowed: got Kind %q", pub.Kind)
	}
	if pub.Code != 4 || pub.Kind != definitionStateDefinitionUnavailable {
		t.Fatalf("Code on an uncaused null definition: want code 4/%s, got code %d/%s", definitionStateDefinitionUnavailable, pub.Code, pub.Kind)
	}
}

// TestCodeDisappearedDuringCollection is design spec line 204's own
// disappearance clause: "Recheck disappearance during collection and
// report unavailable rather than treating it as an empty definition."
// module.sql rejoins sys.objects on the resolved object_id; a module
// gone by the time that second query runs comes back with zero rows,
// which Code must report as definition_unavailable - never write an
// empty artifact and call it a successful export. This is dispatch
// cassure 3's own target.
func TestCodeDisappearedDuringCollection(t *testing.T) {
	conn := &fakeObjConn{responses: []objQueryResponse{
		resolveFoundResponse(605, "dbo", "GoneProc", "P"),
		moduleRowResponse(false, nil, nil),
	}}
	sess := newFakeObjSession(t, conn)
	sink := &objCaptureSink{}

	err := Code(context.Background(), sess, "dbo.GoneProc", sink)
	var pub *model.PublicError
	if !errors.As(err, &pub) {
		t.Fatalf("Code: want *model.PublicError, got %#v", err)
	}
	if pub.Code != 4 || pub.Kind != definitionStateDefinitionUnavailable {
		t.Fatalf("Code on disappearance: want code 4/%s, got code %d/%s", definitionStateDefinitionUnavailable, pub.Code, pub.Kind)
	}
	if len(sink.tables) != 0 {
		t.Fatalf("disappearance: want no table row, got %#v", sink.tables)
	}
	if len(sink.files) != 0 {
		t.Fatalf("disappearance: want no artifact (never an empty one), got %#v", sink.files)
	}
}
