package config

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// selfSignedCert generates a throwaway self-signed certificate for tests
// and returns its PEM and DER encodings. Generated at test time rather
// than checked in: the point of these fixtures is their extension and
// content, not any particular key material.
func selfSignedCert(t *testing.T) (pemBytes, derBytes []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "argosql-test-ca"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IsCA:         true,
		KeyUsage:     x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	pemBlock := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	return pemBlock, der
}

// caProfile writes a profile file whose ca_file points at caPath, with
// trust_server_certificate: false so the CA file is meaningful, and loads
// it.
func caProfile(t *testing.T, caPath string) (Profile, error) {
	t.Helper()
	path := writeConfig(t, "profiles:\n  lab:\n    host: localhost\n    database: AppDB\n"+
		"    username: reader\n    password_env: DB_PASSWORD\n"+
		"    trust_server_certificate: false\n    ca_file: "+caPath+"\n")
	return Load(path, "lab", "", func(string) string { return "secret" })
}

// TestCAFilePEMAccepted checks that a valid PEM file named ca.pem is
// accepted.
func TestCAFilePEMAccepted(t *testing.T) {
	pemBytes, _ := selfSignedCert(t)
	dir := t.TempDir()
	caPath := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(caPath, pemBytes, 0600); err != nil {
		t.Fatal(err)
	}
	p, err := caProfile(t, caPath)
	if err != nil {
		t.Fatal(err)
	}
	if p.CAFile != caPath {
		t.Fatalf("got CAFile %q want %q", p.CAFile, caPath)
	}
}

// TestCAFileWrongExtensionRejected checks that the same valid PEM content,
// named ca.crt, is rejected with code 2 - not accepted, and not left to
// fail at connection time with code 3. Measured against go-mssqldb
// v1.11.0's msdsn.Parse: see the comment on validateCAFile.
func TestCAFileWrongExtensionRejected(t *testing.T) {
	pemBytes, _ := selfSignedCert(t)
	dir := t.TempDir()
	caPath := filepath.Join(dir, "ca.crt")
	if err := os.WriteFile(caPath, pemBytes, 0600); err != nil {
		t.Fatal(err)
	}
	_, err := caProfile(t, caPath)
	msg := publicErrorCode(t, err, 2)
	if !strings.Contains(msg, ".pem") || !strings.Contains(msg, ".der") {
		t.Fatalf("expected message to name both accepted extensions, got %q", msg)
	}
}

// TestCAFileNoExtensionRejected checks the same rejection for a CA file
// with no extension at all.
func TestCAFileNoExtensionRejected(t *testing.T) {
	pemBytes, _ := selfSignedCert(t)
	dir := t.TempDir()
	caPath := filepath.Join(dir, "ca")
	if err := os.WriteFile(caPath, pemBytes, 0600); err != nil {
		t.Fatal(err)
	}
	_, err := caProfile(t, caPath)
	publicErrorCode(t, err, 2)
}

// TestCAFileEmptyPEMRejected checks that a .pem file with no content -
// so AppendCertsFromPEM would silently produce an empty pool - is rejected
// at Load, not left to fail later at connection time.
func TestCAFileEmptyPEMRejected(t *testing.T) {
	dir := t.TempDir()
	caPath := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(caPath, []byte(""), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := caProfile(t, caPath)
	publicErrorCode(t, err, 2)
}

// TestCAFileCorruptPEMRejected checks the same for a .pem file whose
// content is not valid PEM at all.
func TestCAFileCorruptPEMRejected(t *testing.T) {
	dir := t.TempDir()
	caPath := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(caPath, []byte("this is not a certificate"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := caProfile(t, caPath)
	publicErrorCode(t, err, 2)
}

// TestCAFileDERAccepted checks that a valid DER file is accepted, since
// the brief's parsing requirement covers PEM and DER alike.
func TestCAFileDERAccepted(t *testing.T) {
	_, derBytes := selfSignedCert(t)
	dir := t.TempDir()
	caPath := filepath.Join(dir, "ca.der")
	if err := os.WriteFile(caPath, derBytes, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := caProfile(t, caPath); err != nil {
		t.Fatal(err)
	}
}

// TestCAFileCorruptDERRejected checks that a .der file whose content does
// not parse as a certificate is rejected at Load.
func TestCAFileCorruptDERRejected(t *testing.T) {
	dir := t.TempDir()
	caPath := filepath.Join(dir, "ca.der")
	if err := os.WriteFile(caPath, []byte("not a der certificate"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := caProfile(t, caPath)
	publicErrorCode(t, err, 2)
}

// TestCAFileWithTrustTrueRejected checks that ca_file combined with
// trust_server_certificate: true - a contradiction, since the certificate
// would never be validated - is rejected with code 2 rather than silently
// ignoring one of the two settings.
func TestCAFileWithTrustTrueRejected(t *testing.T) {
	pemBytes, _ := selfSignedCert(t)
	dir := t.TempDir()
	caPath := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(caPath, pemBytes, 0600); err != nil {
		t.Fatal(err)
	}
	path := writeConfig(t, "profiles:\n  lab:\n    host: localhost\n    database: AppDB\n"+
		"    username: reader\n    password_env: DB_PASSWORD\n"+
		"    trust_server_certificate: true\n    ca_file: "+caPath+"\n")
	_, err := Load(path, "lab", "", func(string) string { return "secret" })
	publicErrorCode(t, err, 2)
}

// TestCAFileRelativeResolvedAgainstConfigDir checks that a relative
// ca_file is resolved against the directory holding the config file, not
// against the process's working directory.
func TestCAFileRelativeResolvedAgainstConfigDir(t *testing.T) {
	pemBytes, _ := selfSignedCert(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ca.pem"), pemBytes, 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.yaml")
	text := "profiles:\n  lab:\n    host: localhost\n    database: AppDB\n" +
		"    username: reader\n    password_env: DB_PASSWORD\n" +
		"    trust_server_certificate: false\n    ca_file: ca.pem\n"
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	p, err := Load(path, "lab", "", func(string) string { return "secret" })
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, "ca.pem")
	if p.CAFile != want {
		t.Fatalf("got CAFile %q want %q", p.CAFile, want)
	}
}

// TestDSNComponents checks every component DSN is responsible for putting
// in the connection string, not just encrypt and TrustServerCertificate as
// TestDefaultTrust (verbatim from the brief, left untouched) does. Missing
// database silently opens the login's default database - in practice
// master - instead of the one the profile names; a spec violation
// ("Never silently connect to `master`.",
// docs/superpowers/specs/2026-09-08-argosql-mvp-design.md line 41). Missing
// certificate silently falls back to the OS trust store for a profile that
// named a private CA. This test builds a Profile directly (DSN takes a
// Profile, not a loaded file) with a distinctive, distinguishable value in
// every field, so a bug swapping two fields would also be caught. Failure
// messages name only the mismatched component, never the assembled DSN,
// which carries the password.
func TestDSNComponents(t *testing.T) {
	p := Profile{
		Host:                   "dsn-distinctive-host.example.test",
		Port:                   14330,
		Database:               "dsn-distinctive-db",
		Username:               "dsn-distinctive-user",
		Password:               "dsn-distinctive-password",
		CAFile:                 "/etc/argosql/dsn-distinctive-ca.pem",
		TrustServerCertificate: false,
	}
	dsn, err := DSN(p)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}

	if u.Scheme != "sqlserver" {
		t.Fatalf("wrong scheme: got %q want sqlserver", u.Scheme)
	}
	if u.Hostname() != p.Host {
		t.Fatalf("wrong host: got %q want %q", u.Hostname(), p.Host)
	}
	if u.Port() != strconv.Itoa(p.Port) {
		t.Fatalf("wrong port: got %q want %q", u.Port(), strconv.Itoa(p.Port))
	}
	if u.User.Username() != p.Username {
		t.Fatalf("wrong username: got %q want %q", u.User.Username(), p.Username)
	}
	q := u.Query()
	if q.Get("database") != p.Database {
		t.Fatalf("wrong database in DSN query: got %q want %q", q.Get("database"), p.Database)
	}
	if q.Get("certificate") != p.CAFile {
		t.Fatalf("wrong certificate in DSN query: got %q want %q", q.Get("certificate"), p.CAFile)
	}
}
