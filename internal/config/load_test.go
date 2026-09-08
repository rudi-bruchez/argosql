package config

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rudi-bruchez/argosql/internal/model"
)

// writeConfig writes text to a fresh config.yaml under t.TempDir and
// returns its path.
func writeConfig(t *testing.T, text string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

// publicErrorCode fails the test unless err wraps a *model.PublicError with
// the given code, and returns that error's Message.
func publicErrorCode(t *testing.T, err error, want int) string {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error")
	}
	pe, ok := err.(*model.PublicError)
	if !ok {
		t.Fatalf("expected *model.PublicError, got %T: %v", err, err)
	}
	if pe.Code != want {
		t.Fatalf("got code %d want %d (%v)", pe.Code, want, err)
	}
	return pe.Message
}

func TestDefaultTrust(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	text := "profiles:\n  lab:\n    host: localhost\n    database: AppDB\n    username: reader\n    password_env: DB_PASSWORD\n"
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	p, err := Load(path, "lab", "", func(string) string { return "secret" })
	if err != nil {
		t.Fatal(err)
	}
	if !p.TrustServerCertificate || p.Port != 1433 {
		t.Fatalf("bad defaults")
	}
	dsn, err := DSN(p)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if u.Query().Get("encrypt") != "true" || u.Query().Get("TrustServerCertificate") != "true" {
		t.Fatal("TLS defaults")
	}
}

// TestTrustExplicitFalse checks that "trust_server_certificate: false" is
// honored, not silently overridden back to the default.
func TestTrustExplicitFalse(t *testing.T) {
	path := writeConfig(t, "profiles:\n  lab:\n    host: localhost\n    database: AppDB\n"+
		"    username: reader\n    password_env: DB_PASSWORD\n    trust_server_certificate: false\n")
	p, err := Load(path, "lab", "", func(string) string { return "secret" })
	if err != nil {
		t.Fatal(err)
	}
	if p.TrustServerCertificate {
		t.Fatal("expected trust_server_certificate: false to be honored")
	}
	dsn, err := DSN(p)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if u.Query().Get("TrustServerCertificate") != "false" {
		t.Fatal("expected TrustServerCertificate=false in the DSN")
	}
}

// TestTrustExplicitTrue checks that an explicit "true" behaves like the
// absent-field default, not like a distinct third state.
//
// Honest limitation: this test cannot, by itself, prove that
// trust_server_certificate: true was actually decoded rather than simply
// defaulted. Profile.TrustServerCertificate is true in both cases by
// construction (the default is true), so no value-based assertion here can
// tell "explicit true" from "absent" apart - reducing this test's own
// assertion to a decorative one that cannot fail under the bug it claims
// to guard against. What does distinguish the two cases is not the
// resulting value but whether the field was decoded at all:
// TestTrustNonBooleanValueRejected below supplies a value that cannot be a
// bool ("maybe") and requires it be rejected. That only holds if
// trust_server_certificate really is decoded into *bool; if the pointer
// were never populated (the field silently ignored), "maybe" would cause
// no decode error and Load would return the same default true this test
// checks for, undetected. That test is the real coverage; this one only
// documents the shape of the default and catches an accidental hard-coded
// false.
func TestTrustExplicitTrue(t *testing.T) {
	path := writeConfig(t, "profiles:\n  lab:\n    host: localhost\n    database: AppDB\n"+
		"    username: reader\n    password_env: DB_PASSWORD\n    trust_server_certificate: true\n")
	p, err := Load(path, "lab", "", func(string) string { return "secret" })
	if err != nil {
		t.Fatal(err)
	}
	if !p.TrustServerCertificate {
		t.Fatal("expected trust_server_certificate: true to be honored")
	}
}

// TestTrustNonBooleanValueRejected supplies a value that cannot be a bool
// at all. Measured: go.yaml.in/yaml/v4 rejects "maybe" for a *bool field
// with "cannot construct !!str `maybe` into bool" (unlike "yes"/"no",
// which it accepts as legacy YAML 1.1 booleans). This only fails if
// trust_server_certificate is genuinely decoded into *bool; see the
// comment on TestTrustExplicitTrue above for why that test alone cannot
// establish this.
func TestTrustNonBooleanValueRejected(t *testing.T) {
	path := writeConfig(t, "profiles:\n  lab:\n    host: localhost\n    database: AppDB\n"+
		"    username: reader\n    password_env: DB_PASSWORD\n    trust_server_certificate: maybe\n")
	_, err := Load(path, "lab", "", func(string) string { return "secret" })
	publicErrorCode(t, err, 2)
}

// TestDatabaseOverride checks that a non-empty databaseOverride replaces
// the profile's own database field.
func TestDatabaseOverride(t *testing.T) {
	path := writeConfig(t, "profiles:\n  lab:\n    host: localhost\n    database: AppDB\n"+
		"    username: reader\n    password_env: DB_PASSWORD\n")
	p, err := Load(path, "lab", "OtherDB", func(string) string { return "secret" })
	if err != nil {
		t.Fatal(err)
	}
	if p.Database != "OtherDB" {
		t.Fatalf("got database %q want OtherDB", p.Database)
	}
}

// TestEmptyDatabaseRejected checks that a profile with no database and no
// override is rejected rather than producing a DSN with an empty database.
func TestEmptyDatabaseRejected(t *testing.T) {
	path := writeConfig(t, "profiles:\n  lab:\n    host: localhost\n"+
		"    username: reader\n    password_env: DB_PASSWORD\n")
	_, err := Load(path, "lab", "", func(string) string { return "secret" })
	publicErrorCode(t, err, 2)
}

// TestMissingUsernameRejected and TestMissingPasswordEnvKeyRejected check
// the two mandatory identity fields the brief calls out by name.
func TestMissingUsernameRejected(t *testing.T) {
	path := writeConfig(t, "profiles:\n  lab:\n    host: localhost\n    database: AppDB\n"+
		"    password_env: DB_PASSWORD\n")
	_, err := Load(path, "lab", "", func(string) string { return "secret" })
	publicErrorCode(t, err, 2)
}

// TestMissingPasswordEnvKeyRejected covers the password_env *key* being
// absent from the YAML profile. Distinct from
// TestPasswordEnvVariableUndefinedRejected below, where the key is present
// but the environment variable it names is not set.
func TestMissingPasswordEnvKeyRejected(t *testing.T) {
	path := writeConfig(t, "profiles:\n  lab:\n    host: localhost\n    database: AppDB\n"+
		"    username: reader\n")
	_, err := Load(path, "lab", "", func(string) string { return "secret" })
	publicErrorCode(t, err, 2)
}

// TestPasswordEnvVariableUndefinedRejected covers the password_env key
// being present and naming a variable, but getenv returning an empty
// string for that name - the getenv signature cannot distinguish an unset
// variable from one explicitly set to empty, and an empty password is not
// usable for SQL Server authentication either way. The spec
// (docs/superpowers/specs/2026-09-08-argosql-mvp-design.md, "Reject ...
// missing password environment variables ... with code 2") requires this
// to fail before any connection is attempted, at code 2, not at connection
// time at code 3.
func TestPasswordEnvVariableUndefinedRejected(t *testing.T) {
	path := writeConfig(t, "profiles:\n  lab:\n    host: localhost\n    database: AppDB\n"+
		"    username: reader\n    password_env: DB_PASSWORD\n")
	_, err := Load(path, "lab", "", func(string) string { return "" })
	msg := publicErrorCode(t, err, 2)
	if !strings.Contains(msg, "DB_PASSWORD") {
		t.Fatalf("expected message to name the expected variable, got %q", msg)
	}
}

// TestEmptyHostRejected, TestEmptyUsernameRejected, and
// TestEmptyPasswordEnvKeyRejected check that an explicit empty string -
// "host: \"\"" etc. - is rejected exactly like the key being absent
// (covered above by TestMissingUsernameRejected and
// TestMissingPasswordEnvKeyRejected). The two cases decode to a different
// pointer state (non-nil pointer to "" vs. a nil pointer): Load's guard is
// "== nil || == \"\"", but nothing previously exercised the second half of
// that guard on its own. Measured: reducing the guard to "== nil" alone -
// accepting an explicit empty value - left the full suite green.
func TestEmptyHostRejected(t *testing.T) {
	path := writeConfig(t, "profiles:\n  lab:\n    host: \"\"\n    database: AppDB\n"+
		"    username: reader\n    password_env: DB_PASSWORD\n")
	_, err := Load(path, "lab", "", func(string) string { return "secret" })
	publicErrorCode(t, err, 2)
}

func TestEmptyUsernameRejected(t *testing.T) {
	path := writeConfig(t, "profiles:\n  lab:\n    host: localhost\n    database: AppDB\n"+
		"    username: \"\"\n    password_env: DB_PASSWORD\n")
	_, err := Load(path, "lab", "", func(string) string { return "secret" })
	publicErrorCode(t, err, 2)
}

func TestEmptyPasswordEnvKeyRejected(t *testing.T) {
	path := writeConfig(t, "profiles:\n  lab:\n    host: localhost\n    database: AppDB\n"+
		"    username: reader\n    password_env: \"\"\n")
	_, err := Load(path, "lab", "", func(string) string { return "secret" })
	publicErrorCode(t, err, 2)
}

// TestEmptyCAFileRejected covers the explicit ca_file: "" that the other
// empty-field tests do not: it is not the same as the key being absent,
// because an operator who wrote it believes the line does something.
//
// The assertion is on the message and not only on code 2, deliberately. An
// empty ca_file reaches validateCAFile as the config directory once joined,
// which is rejected for a wrong extension, also with code 2. A test that
// asserted the code alone would pass with the empty-key check deleted.
// Measured: it did, before this comment was written.
func TestEmptyCAFileRejected(t *testing.T) {
	path := writeConfig(t, "profiles:\n  lab:\n    host: localhost\n    database: AppDB\n"+
		"    username: reader\n    password_env: DB_PASSWORD\n"+
		"    trust_server_certificate: false\n    ca_file: \"\"\n")
	_, err := Load(path, "lab", "", func(string) string { return "secret" })
	publicErrorCode(t, err, 2)
	if !strings.Contains(err.Error(), "remove the key entirely") {
		t.Fatalf("the error must name the empty key as the problem, got %q", err.Error())
	}
}

// TestPortOutOfRange checks both ends of the accepted 1-65535 range.
func TestPortOutOfRange(t *testing.T) {
	for _, port := range []string{"0", "65536", "-1"} {
		path := writeConfig(t, "profiles:\n  lab:\n    host: localhost\n    database: AppDB\n"+
			"    username: reader\n    password_env: DB_PASSWORD\n    port: "+port+"\n")
		_, err := Load(path, "lab", "", func(string) string { return "secret" })
		publicErrorCode(t, err, 2)
	}
}

// TestSecretSpecialChars checks that a password containing '@', ';', and
// '?' - all meaningful in a URL - survives DSN encoding and decodes back to
// the original value rather than corrupting the connection string.
func TestSecretSpecialChars(t *testing.T) {
	path := writeConfig(t, "profiles:\n  lab:\n    host: localhost\n    database: AppDB\n"+
		"    username: reader\n    password_env: DB_PASSWORD\n")
	const secret = `p@ss;w?rd`
	p, err := Load(path, "lab", "", func(string) string { return secret })
	if err != nil {
		t.Fatal(err)
	}
	dsn, err := DSN(p)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := u.User.Password()
	if !ok || got != secret {
		t.Fatalf("password round-trip failed: got %q ok=%v", got, ok)
	}
}

// TestLiteralPasswordFieldRejected checks that a profile carrying a
// literal "password" field - as opposed to "password_env" - is rejected as
// an unknown field rather than silently accepted as a secret in the file.
//
// The YAML below is otherwise valid on every other point (it also sets
// password_env) so that the literal password field is the only thing that
// can cause the rejection. Measured: the previous version of this test
// omitted password_env, so it was rejected by the missing-password_env
// check regardless of whether the plaintext-password rejection worked at
// all - confirmed by temporarily making "password" a recognized field: the
// old test still passed, green for the wrong reason, because the
// co-occurring missing password_env fired instead. That left the spec's
// "plaintext password fields" clause (line 115) with no test of its own.
func TestLiteralPasswordFieldRejected(t *testing.T) {
	path := writeConfig(t, "profiles:\n  lab:\n    host: localhost\n    database: AppDB\n"+
		"    username: reader\n    password_env: DB_PASSWORD\n    password: hunter2\n")
	_, err := Load(path, "lab", "", func(string) string { return "secret" })
	publicErrorCode(t, err, 2)
}

// TestUnknownFieldRejected checks that any other unrecognized key is
// rejected too, not just "password".
func TestUnknownFieldRejected(t *testing.T) {
	path := writeConfig(t, "profiles:\n  lab:\n    host: localhost\n    database: AppDB\n"+
		"    username: reader\n    password_env: DB_PASSWORD\n    nickname: lab1\n")
	_, err := Load(path, "lab", "", func(string) string { return "secret" })
	publicErrorCode(t, err, 2)
}

// TestDuplicateKeyRejected checks that a profile with the same key twice
// is rejected rather than silently keeping the last value.
func TestDuplicateKeyRejected(t *testing.T) {
	path := writeConfig(t, "profiles:\n  lab:\n    host: localhost\n    host: otherhost\n"+
		"    database: AppDB\n    username: reader\n    password_env: DB_PASSWORD\n")
	_, err := Load(path, "lab", "", func(string) string { return "secret" })
	publicErrorCode(t, err, 2)
}

// TestExtraYAMLDocumentRejected checks that a second YAML document after
// the profiles document is rejected, not silently ignored.
func TestExtraYAMLDocumentRejected(t *testing.T) {
	path := writeConfig(t, "profiles:\n  lab:\n    host: localhost\n    database: AppDB\n"+
		"    username: reader\n    password_env: DB_PASSWORD\n---\nnotes: something else\n")
	_, err := Load(path, "lab", "", func(string) string { return "secret" })
	publicErrorCode(t, err, 2)
}

// TestYAMLErrorReformulated checks that a YAML decoding error is
// reformulated rather than forwarded verbatim: the underlying yaml library
// error can quote raw document content (here, the mistyped port value),
// and that content must not leak into the public error message.
//
// Measured: go.yaml.in/yaml/v4 truncates a scalar quoted inside a type
// mismatch message to at most 10 characters, appending "..." beyond that -
// an 11-character marker already comes back as its first 7 characters plus
// "...". A 24-character marker is therefore masked by the library's own
// truncation regardless of whether Load reformulates anything, which made
// the previous version of this test pass vacuously (confirmed: forwarding
// the native error verbatim still left it green). With the 10-character
// marker below, the native yaml/v4 error for this exact YAML is:
//
//	yaml: construct errors: line 3: cannot construct !!str `ABCDEFGHIJ` into int
//
// - the marker appears whole, which is what makes this test meaningful.
func TestYAMLErrorReformulated(t *testing.T) {
	const marker = "ABCDEFGHIJ" // 10 characters: verified above to survive yaml/v4's truncation whole
	path := writeConfig(t, "profiles:\n  lab:\n    host: localhost\n    database: AppDB\n"+
		"    username: reader\n    password_env: DB_PASSWORD\n    port: \""+marker+"\"\n")
	_, err := Load(path, "lab", "", func(string) string { return "secret" })
	msg := publicErrorCode(t, err, 2)
	if strings.Contains(msg, marker) {
		t.Fatalf("public error message leaked source content: %q", msg)
	}
}

// TestUnknownProfileRejected checks that requesting a profile name absent
// from the file fails cleanly.
func TestUnknownProfileRejected(t *testing.T) {
	path := writeConfig(t, "profiles:\n  lab:\n    host: localhost\n    database: AppDB\n"+
		"    username: reader\n    password_env: DB_PASSWORD\n")
	_, err := Load(path, "missing", "", func(string) string { return "secret" })
	publicErrorCode(t, err, 2)
}

// TestProfileNeverPrintsPassword checks that Profile - the only structure
// in this codebase carrying a secret - does not leak it through the
// generic paths that a struct crossing package boundaries eventually
// meets: a bare %v or %+v, %#v, and json.Marshal. Both the value form
// (Profile) and the pointer form (*Profile) are checked for each, since a
// method with the wrong receiver can protect one and not the other -
// measured beforehand: a pointer-receiver String() left fmt.Sprintf("%v",
// p) (value form) printing the raw struct, secret included, while
// fmt.Sprintf("%v", &p) (pointer form) was redacted; a value receiver, as
// implemented here, covers both. The secret is searched for as a
// substring rather than compared for exact output, so this test does not
// depend on the exact redacted rendering.
func TestProfileNeverPrintsPassword(t *testing.T) {
	const secret = "UNLIKELY-DSN-SECRET-9f3ac21b"
	p := Profile{
		Host:                   "localhost",
		Database:               "AppDB",
		Username:               "reader",
		Password:               secret,
		CAFile:                 "/etc/argosql/ca.pem",
		Port:                   1433,
		TrustServerCertificate: false,
	}

	forms := map[string]string{
		"%v value":    fmt.Sprintf("%v", p),
		"%v pointer":  fmt.Sprintf("%v", &p),
		"%+v value":   fmt.Sprintf("%+v", p),
		"%+v pointer": fmt.Sprintf("%+v", &p),
		"%#v value":   fmt.Sprintf("%#v", p),
		"%#v pointer": fmt.Sprintf("%#v", &p),
	}
	for label, got := range forms {
		if strings.Contains(got, secret) {
			t.Fatalf("%s leaked the secret: %s", label, got)
		}
	}

	jsonValue, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(jsonValue), secret) {
		t.Fatalf("json.Marshal(value) leaked the secret: %s", jsonValue)
	}
	jsonPointer, err := json.Marshal(&p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(jsonPointer), secret) {
		t.Fatalf("json.Marshal(pointer) leaked the secret: %s", jsonPointer)
	}
}
