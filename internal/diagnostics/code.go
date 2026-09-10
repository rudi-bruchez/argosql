package diagnostics

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"strings"

	"github.com/rudi-bruchez/argosql/internal/model"
	"github.com/rudi-bruchez/argosql/internal/sqlserver"
)

// moduleQuery is sql/module.sql (see its own doc comment for what it
// reads and why) - embedded here, rather than in embed.go, for the
// same reason query.go and top.go embed their own sql/*.sql locally:
// it is this file's own concern alone.
//
//go:embed sql/module.sql
var moduleQuery string

// The four definition_state values design spec line 204 names, and
// nothing else: "For obj code, a visible supported module with a
// non-null definition yields available and an artifact. A confirmed
// encrypted module yields encrypted, code 4, no definition artifact. A
// visible module with a confirmed denied definition permission yields
// permission_denied, code 4. Otherwise a null definition yields
// definition_unavailable, code 4, without inventing its cause." This
// is the whole closed vocabulary: Code never returns a fifth state,
// and it never defaults to permission_denied - a state this function
// cannot actually confirm is always definitionStateDefinitionUnavailable,
// never a guess dressed up as a confirmed denial.
const (
	definitionStateAvailable             = "available"
	definitionStateEncrypted             = "encrypted"
	definitionStatePermissionDenied      = "permission_denied"
	definitionStateDefinitionUnavailable = "definition_unavailable"
)

// ModuleTable is the "module" table "obj code" emits, only ever for
// the definitionStateAvailable outcome (design spec's declared table
// order: "obj code: module"). Every other outcome - encrypted,
// permission_denied, definition_unavailable, or an unresolved name's
// own code 8 - is reported purely through the returned error's Kind,
// with no table row at all, the same precedent Plan (plan.go)
// established for planUnavailable: "the row exists, but the thing it
// should carry does not" is a failure, not a row that describes its
// own absence.
var ModuleTable = model.TableSpec{
	Name: "module",
	Columns: []model.Column{
		{Name: "object_id", SQLType: "INT"},
		{Name: "name", SQLType: "NVARCHAR"},
		{Name: "type", SQLType: "NVARCHAR"},
		{Name: "definition_state", SQLType: "NVARCHAR"},
		{Name: "line_count", SQLType: "INT"},
		{Name: "artifact", SQLType: "NVARCHAR"},
	},
}

// moduleDisappeared, moduleEncrypted, modulePermissionDenied and
// moduleDefinitionUnavailable each build the code-4 error Code returns
// for one of the three non-available definitionState outcomes, plus
// the disappearance case module.sql's own recheck exists to catch -
// design spec: "Recheck disappearance during collection and report
// unavailable rather than treating it as an empty definition." A
// module that resolved successfully (Resolve, above) but is gone by
// the time moduleQuery re-joins sys.objects on the same object_id is
// reported exactly like a null, uncaused definition - definitionState
// "unavailable" is the one word this project's own vocabulary has for
// "the thing that should be here is not, and this function will not
// guess why."
func moduleDisappeared(obj sqlserver.Object) error {
	return &model.PublicError{
		Code:    4,
		Kind:    definitionStateDefinitionUnavailable,
		Message: fmt.Sprintf("%s.%s: resolved, but no longer visible when its definition was read (recheck failed)", obj.Schema, obj.Name),
	}
}

func moduleEncrypted(obj sqlserver.Object) error {
	return &model.PublicError{
		Code:    4,
		Kind:    definitionStateEncrypted,
		Message: fmt.Sprintf("%s.%s is encrypted (WITH ENCRYPTION): its definition is never exportable, to any principal", obj.Schema, obj.Name),
	}
}

func modulePermissionDenied(obj sqlserver.Object) error {
	return &model.PublicError{
		Code:    4,
		Kind:    definitionStatePermissionDenied,
		Message: fmt.Sprintf("%s.%s: VIEW DEFINITION is denied for the current principal on this object", obj.Schema, obj.Name),
	}
}

func moduleDefinitionUnavailable(obj sqlserver.Object) error {
	return &model.PublicError{
		Code:    4,
		Kind:    definitionStateDefinitionUnavailable,
		Message: fmt.Sprintf("%s.%s: definition is unavailable; its cause could not be established", obj.Schema, obj.Name),
	}
}

// moduleAllowedTypes are the sys.objects.type codes "obj code"
// accepts: procedures, scalar/inline-table/multi-statement-table
// functions, triggers and views - every type sys.sql_modules can hold
// a definition for. A resolved object of any other type (a table,
// most commonly) is rejected at code 2 (design spec line 202, "A
// resolved object of the wrong type gives code 2") rather than routed
// through the definitionState vocabulary at all.
//
// Task 13 fix-1 replaces an earlier, WRONG decision here: obj code on
// a table used to reach moduleQuery, read OBJECTPROPERTYEX's NULL
// (the property does not apply to a table) and report
// definitionStateDefinitionUnavailable at code 4 - itself a fix-0
// correction of an even earlier code 5. Both were wrong: design spec
// line 202 settles the question before any module-specific query
// ever runs, exactly like it now does for obj table/idx list/size
// table (see tableAllowedTypes in table.go) - a resolved object of
// the wrong type is an argument error, never a definition-state
// question. The message naming the real type, from the earlier fix,
// was already correct and is kept.
var moduleAllowedTypes = map[string]bool{"P": true, "FN": true, "IF": true, "TF": true, "TR": true, "V": true}

// Code runs "obj code <schema.name>": exports obj's visible module
// definition to a .sql artifact, or reports one of the three failure
// states the design spec's definition-state contract (line 204)
// defines, without ever inventing which one applies when the engine
// itself cannot say (design spec: "obj code": "Export visible module
// definition to .sql; return identity and line count; use the
// definition-state contract below; preserve an explicit unresolved
// state when absence and invisibility cannot be separated").
//
// Resolution precedes everything else (design spec line 172): an
// unresolved name fails at code 8, via sqlserver.Resolve, before this
// function ever probes a permission that could otherwise be
// misreported as the reason - Resolve's own not_found_or_not_visible
// stays the one answer for "this name did not resolve", never
// upgraded (or downgraded) to a permission-shaped error. Right after,
// a resolved object of the wrong type is rejected at code 2 (design
// spec line 202) - see moduleAllowedTypes' own doc comment for why
// this replaces an earlier, wrong decision to route this case through
// the definitionState vocabulary instead.
//
// Once obj resolves and its type is accepted, moduleQuery reads
// sys.sql_modules.definition and
// OBJECTPROPERTYEX's own encryption flag in the same round trip,
// rejoining sys.objects on obj.ID rather than trusting Resolve's
// earlier read: zero rows here means obj disappeared in between
// (moduleDisappeared). A confirmed encrypted module (moduleEncrypted)
// is checked before ever looking at whether the definition itself is
// null, because OBJECTPROPERTYEX's flag is the one fact visible
// regardless of VIEW DEFINITION - see sql/module.sql's own doc
// comment. Only when the definition is null AND the module is not
// encrypted does Code probe VIEW DEFINITION on obj's own resolved
// name to tell a confirmed denial (modulePermissionDenied) apart from
// every other null-definition cause, which it reports as
// moduleDefinitionUnavailable without ever inventing which one it
// was.
func Code(ctx context.Context, s *sqlserver.Session, name string, dst model.Sink) error {
	obj, err := sqlserver.Resolve(ctx, s.Conn, name)
	if err != nil {
		return err
	}
	if !moduleAllowedTypes[obj.Type] {
		return wrongObjectTypeError(obj, "obj code", "procedures, functions, triggers and views")
	}

	cells, found, err := queryOneOptionalRow(ctx, s.Conn, moduleQuery, sql.Named("id", obj.ID))
	if err != nil {
		return err
	}
	if !found {
		return moduleDisappeared(obj)
	}

	encrypted, ok := cells[1].(bool)
	if !ok {
		return unexpectedCell("is_encrypted")
	}
	if encrypted {
		return moduleEncrypted(obj)
	}

	if cells[0] == nil {
		qualified := obj.QualifiedName()
		perm, perr := sqlserver.Probe(ctx, s.Conn, qualified, "OBJECT", "VIEW DEFINITION")
		if perr != nil {
			return perr
		}
		if perm == sqlserver.Denied {
			return modulePermissionDenied(obj)
		}
		return moduleDefinitionUnavailable(obj)
	}
	definition, ok := cells[0].(string)
	if !ok {
		return unexpectedCell("definition")
	}

	lineCount := int64(strings.Count(definition, "\n") + 1)
	artifact, err := dst.File("module_definition", ".sql", strings.NewReader(definition))
	if err != nil {
		return err
	}

	if err := dst.Begin(ModuleTable); err != nil {
		return err
	}
	row := []model.Cell{
		obj.ID,
		obj.Schema + "." + obj.Name,
		obj.Type,
		definitionStateAvailable,
		lineCount,
		artifact.Path,
	}
	if err := dst.Row(row); err != nil {
		return err
	}
	return dst.End(true, true)
}
