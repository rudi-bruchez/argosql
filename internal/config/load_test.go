package config

import (
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

// TestMissingUsernameRejected and TestMissingPasswordEnvRejected check the
// two mandatory identity fields the brief calls out by name.
func TestMissingUsernameRejected(t *testing.T) {
	path := writeConfig(t, "profiles:\n  lab:\n    host: localhost\n    database: AppDB\n"+
		"    password_env: DB_PASSWORD\n")
	_, err := Load(path, "lab", "", func(string) string { return "secret" })
	publicErrorCode(t, err, 2)
}

func TestMissingPasswordEnvRejected(t *testing.T) {
	path := writeConfig(t, "profiles:\n  lab:\n    host: localhost\n    database: AppDB\n"+
		"    username: reader\n")
	_, err := Load(path, "lab", "", func(string) string { return "secret" })
	publicErrorCode(t, err, 2)
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
func TestLiteralPasswordFieldRejected(t *testing.T) {
	path := writeConfig(t, "profiles:\n  lab:\n    host: localhost\n    database: AppDB\n"+
		"    username: reader\n    password: hunter2\n")
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
func TestYAMLErrorReformulated(t *testing.T) {
	const marker = "LEAKED-SOURCE-MARKER-XYZ"
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
