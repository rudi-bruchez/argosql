//go:build integration

package integration

// This file provisions disposable SQL Server containers with deliberately
// crafted TLS certificates and proves that trust_server_certificate and
// ca_file actually drive certificate validation end to end, rather than
// merely being accepted as configuration. Every assertion here traces a
// connection failure to its specific cause (see assertTLSValidationFailure
// below) rather than stopping at exit code 3: a failed connection returns
// code 3 for many unrelated reasons - the server not ready yet, the wrong
// port, a wrong password - and a test that only checked the code would
// still pass if certificate validation silently did nothing at all.
//
// Certificate generation note: crypto/x509 is used directly, never openssl
// as a subprocess, so the expired-certificate variant can set NotBefore and
// NotAfter to arbitrary past timestamps without any dependency on a host
// tool.
//
// Container count note: three TLS-dedicated containers are started by this
// file - one per distinct certificate (valid, wrong-host, expired) - never
// one per test case. A fresh SQL Server container costs tens of seconds to
// become ready; the client-side variations in this matrix (which
// trust_server_certificate/ca_file combination a given connection attempt
// uses) cost nothing by comparison, so every variation that can share a
// container's certificate does.
//
// Rootless permissions note: see NewTLSLab's doc comment for what was
// measured before choosing how these certificate files are made readable
// inside the container.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rudi-bruchez/argosql/internal/config"
	"github.com/rudi-bruchez/argosql/internal/model"
	"github.com/rudi-bruchez/argosql/internal/sqlserver"
)

// tlsMode names the three certificate postures NewTLSLab can provision, and
// its three values are exactly the brief's own spelling (valid, wrong-host,
// expired): there is no fourth.
const (
	tlsModeValid     = "valid"
	tlsModeWrongHost = "wrong-host"
	tlsModeExpired   = "expired"
)

// tlsProbeTimeout bounds rawConnectError's own PingContext: generous enough
// that it is never itself the reason a TLS handshake probe fails, since a
// timeout there would masquerade as a validation failure.
const tlsProbeTimeout = 15 * time.Second

// NewTLSLab starts a brand-new, uniquely named SQL Server container whose
// TLS certificate is deliberately built for mode, and returns a *Lab with
// the same properties NewLab guarantees: it never reuses or touches a
// container that already exists on the host, it waits for readiness in a
// bounded polling loop (never a fixed sleep), it applies the same fixture
// bootstrap (so a TLS Lab is as usable for any test as a plain one), and it
// registers t.Cleanup that closes Admin and removes the container by ID.
// ASQ_TEST_IMAGE unset is an explicit t.Fatal, never a silent t.Skip, same
// as NewLab.
//
// The returned Lab's Profile always carries TrustServerCertificate: true
// (so Admin, and any caller that does not override it, connects regardless
// of this mode's certificate defect) and CAFile pointing at the CA that
// legitimately signed this container's own certificate - a test exercising
// trust_server_certificate: false copies Profile and flips that field
// itself, exactly as session_test.go's tests already copy NewLab's Profile
// to vary Database.
//
// Rootless permissions, measured rather than assumed: this host's Podman is
// rootless, and SQL Server inside the mssql/server image runs as container
// UID 10001 ("mssql"), never root. Measured directly against this host: a
// plain file created by this process (owned by the invoking UID, inside a
// directory at t.TempDir()'s default mode 0700) is unreadable from inside
// the container under either UID - `ls` on its containing directory itself
// fails with "Permission denied" - because rootless Podman's user-namespace
// UID mapping does not map this process's own UID to anything recognizable
// inside the container's namespace; it lands on an unmapped UID that
// matches neither the file's owner nor its group. The fix measured to work:
// a directory mode 0755 and files mode 0644 put the container's mssql UID
// in the "other" permission class, which is readable. NewTLSLab therefore
// widens permissions only on a directory and files it creates itself,
// fresh, solely to hold one run's own certificate and config - never on a
// path or file that belongs to the invoking user, which is what the
// project's "never chmod a user's file" rule actually forbids.
func NewTLSLab(t *testing.T, mode string) *Lab {
	t.Helper()

	switch mode {
	case tlsModeValid, tlsModeWrongHost, tlsModeExpired:
	default:
		t.Fatalf("NewTLSLab: unknown mode %q, want %q, %q, or %q", mode, tlsModeValid, tlsModeWrongHost, tlsModeExpired)
	}

	image := os.Getenv("ASQ_TEST_IMAGE")
	if image == "" {
		t.Fatal("ASQ_TEST_IMAGE is not set: the integration suite requires an image and never chooses one on its own")
	}
	if _, err := exec.LookPath("podman"); err != nil {
		t.Fatalf("podman not found on PATH: %v", err)
	}

	runDir := filepath.Join(t.TempDir(), "tls-run")
	certsDir := filepath.Join(runDir, "certs")
	if err := os.MkdirAll(certsDir, 0o755); err != nil {
		t.Fatalf("creating dedicated TLS run directory: %v", err)
	}
	// Explicit, not trusted to MkdirAll's mode argument surviving the
	// process umask untouched: see the measurement in this function's
	// doc comment for why both of these must end up world-readable.
	if err := os.Chmod(runDir, 0o755); err != nil {
		t.Fatalf("widening TLS run directory permissions: %v", err)
	}
	if err := os.Chmod(certsDir, 0o755); err != nil {
		t.Fatalf("widening TLS certs directory permissions: %v", err)
	}

	caPEMPath := generateTLSCertSet(t, certsDir, mode)

	mssqlConfPath := filepath.Join(runDir, "mssql.conf")
	mssqlConf := "[network]\n" +
		"tlscert = /run/secrets/tls/server.pem\n" +
		"tlskey = /run/secrets/tls/server.key\n" +
		"tlsprotocols = 1.2\n" +
		"forceencryption = 1\n"
	if err := os.WriteFile(mssqlConfPath, []byte(mssqlConf), 0o644); err != nil {
		t.Fatalf("writing mssql.conf: %v", err)
	}

	name := "asq-test-tls-" + randomHex(6)
	password := randomPassword()
	envPath := filepath.Join(runDir, "env")
	envContent := "ACCEPT_EULA=Y\nMSSQL_PID=Developer\nMSSQL_SA_PASSWORD=" + password + "\n"
	if err := os.WriteFile(envPath, []byte(envContent), 0o600); err != nil {
		t.Fatalf("writing container env file: %v", err)
	}

	containerID := podmanRunTLS(t, name, image, envPath, certsDir, mssqlConfPath)

	// Registered immediately after the container exists, before anything
	// below can fail, exactly like NewLab: a readiness timeout or a bad
	// bootstrap must still leave nothing of this Lab's container behind.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), podmanCmdTimeout)
		defer cancel()
		if err := exec.CommandContext(ctx, "podman", "rm", "--force", "--time", "0", containerID).Run(); err != nil {
			t.Logf("cleanup: podman rm %s: %v", containerID, describeExecError(err))
		}
	})

	imageID := podmanInspectImage(t, containerID)
	host, port := podmanPublishedAddr(t, containerID)

	masterProfile := config.Profile{
		Host:                   host,
		Port:                   port,
		Username:               "sa",
		Password:               password,
		Database:               "master",
		TrustServerCertificate: true,
	}
	masterDSN, err := config.DSN(masterProfile)
	if err != nil {
		t.Fatalf("building admin connection string: %v", err)
	}

	masterDB := waitReady(t, masterDSN)
	defer masterDB.Close()

	bootstrapCtx, bootstrapCancel := context.WithTimeout(context.Background(), bootstrapTimeout)
	defer bootstrapCancel()
	if err := applyBootstrap(bootstrapCtx, masterDB); err != nil {
		t.Fatalf("applying fixture bootstrap: %v", err)
	}

	appProfile := masterProfile
	appProfile.Database = "AppDB"
	appProfile.CAFile = caPEMPath
	appDSN, err := config.DSN(appProfile)
	if err != nil {
		t.Fatalf("building AppDB connection string: %v", err)
	}
	appDB, err := sql.Open("sqlserver", appDSN)
	if err != nil {
		t.Fatalf("opening AppDB pool: %v", err)
	}
	pingCtx, pingCancel := context.WithTimeout(context.Background(), readyPingTimeout)
	defer pingCancel()
	if err := appDB.PingContext(pingCtx); err != nil {
		appDB.Close()
		t.Fatalf("pinging AppDB right after bootstrap: %v", err)
	}
	// Registered after the rm cleanup above: t.Cleanup runs LIFO, so this
	// closes Admin before the container is removed, matching NewLab.
	t.Cleanup(func() { appDB.Close() })

	return &Lab{
		Admin:       appDB,
		Profile:     appProfile,
		ContainerID: containerID,
		ImageID:     imageID,
	}
}

// podmanRunTLS is podmanRun's TLS sibling: the same detached, run-labeled,
// randomly-named container on a Podman-assigned loopback port, but with two
// additional bind mounts - a dedicated certificate directory at
// /run/secrets/tls and a pre-written mssql.conf at
// /var/opt/mssql/mssql.conf that points SQL Server's own TLS settings at
// the files in it. It exists only because podmanRun's argv has no room for
// extra --volume flags; podmanRun itself is untouched.
func podmanRunTLS(t *testing.T, name, image, envPath, certsDir, mssqlConfPath string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), podmanCmdTimeout)
	defer cancel()
	args := []string{
		"run", "--detach",
		"--name", name,
		"--label", "io.argosql.test=" + testRunID,
		"--publish", "127.0.0.1::1433",
		"--env-file", envPath,
		"--volume", certsDir + ":/run/secrets/tls:ro,Z",
		"--volume", mssqlConfPath + ":/var/opt/mssql/mssql.conf:ro,Z",
		image,
	}
	out, err := exec.CommandContext(ctx, "podman", args...).Output()
	if err != nil {
		t.Fatalf("podman run (tls): %v", describeExecError(err))
	}
	id := strings.TrimSpace(string(out))
	if id == "" {
		t.Fatal("podman run (tls) produced no container ID")
	}
	return id
}

// generateTLSCertSet mints a fresh self-signed CA and one leaf certificate
// signed by it under certsDir (as server.pem/server.key, the paths
// mssql.conf above points SQL Server at), shaped by mode:
//
//   - valid: SAN covers both "localhost" and 127.0.0.1 - the loopback
//     address this harness always actually connects through - and an
//     ordinary one-year validity window.
//   - wrong-host: SAN deliberately excludes "localhost" and 127.0.0.1
//     (it covers only "wrong-host.invalid"), so connecting to the real
//     published address reproduces a genuine hostname mismatch.
//   - expired: the same SAN as valid, but NotBefore/NotAfter both already
//     in the past, so the only thing wrong with it is the validity
//     window.
//
// It returns the path to the CA certificate (never the key - the CA key
// never leaves this function), which every mode's leaf was signed by and
// which a caller testing trust_server_certificate: false needs as ca_file.
func generateTLSCertSet(t *testing.T, certsDir, mode string) (caPEMPath string) {
	t.Helper()

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating CA key: %v", err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber:          randomSerial(t),
		Subject:               pkix.Name{CommonName: "argosql integration test CA"},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("creating CA certificate: %v", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("parsing freshly minted CA certificate: %v", err)
	}

	caPEMPath = filepath.Join(certsDir, "ca.pem")
	writeCertPEM(t, caPEMPath, caDER)

	var sanDNS []string
	var sanIPs []net.IP
	notBefore := time.Now().Add(-1 * time.Hour)
	notAfter := time.Now().Add(365 * 24 * time.Hour)
	switch mode {
	case tlsModeValid:
		sanDNS = []string{"localhost"}
		sanIPs = []net.IP{net.ParseIP("127.0.0.1")}
	case tlsModeWrongHost:
		sanDNS = []string{"wrong-host.invalid"}
	case tlsModeExpired:
		sanDNS = []string{"localhost"}
		sanIPs = []net.IP{net.ParseIP("127.0.0.1")}
		notBefore = time.Now().Add(-48 * time.Hour)
		notAfter = time.Now().Add(-24 * time.Hour)
	}

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating server key: %v", err)
	}
	leafTemplate := &x509.Certificate{
		SerialNumber: randomSerial(t),
		Subject:      pkix.Name{CommonName: "argosql integration test server"},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     sanDNS,
		IPAddresses:  sanIPs,
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("creating server certificate: %v", err)
	}

	writeCertPEM(t, filepath.Join(certsDir, "server.pem"), leafDER)
	writeKeyPEM(t, filepath.Join(certsDir, "server.key"), leafKey)

	return caPEMPath
}

// writeUnrelatedCAFile mints a second, entirely separate self-signed CA
// that never signs any container's certificate, for the one test case that
// needs a ca_file which cannot possibly validate anything it is pointed
// at: proving that an unknown CA is rejected, specifically, rather than
// whatever a same-CA-as-the-server test would prove.
func writeUnrelatedCAFile(t *testing.T, dir string) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating unrelated CA key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          randomSerial(t),
		Subject:               pkix.Name{CommonName: "argosql integration test unrelated CA"},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("creating unrelated CA certificate: %v", err)
	}
	path := filepath.Join(dir, "unrelated-ca.pem")
	writeCertPEM(t, path, der)
	return path
}

// randomSerial returns a random 128-bit positive serial number, using
// crypto/rand like every other identifier this package generates (see
// randomHex in podman_test.go): never time-based, which could collide
// across the certificates a single test mints back to back.
func randomSerial(t *testing.T) *big.Int {
	t.Helper()
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	n, err := rand.Int(rand.Reader, limit)
	if err != nil {
		t.Fatalf("crypto/rand: %v", err)
	}
	return n
}

func writeCertPEM(t *testing.T, path string, der []byte) {
	t.Helper()
	block := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if block == nil {
		t.Fatalf("encoding PEM for %s", path)
	}
	// 0644: see NewTLSLab's doc comment on why these files must be
	// world-readable to be usable at all from inside a rootless
	// container. Never anywhere outside certsDir, which lives inside
	// t.TempDir() and is removed with it.
	if err := os.WriteFile(path, block, 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

func writeKeyPEM(t *testing.T, path string, key *ecdsa.PrivateKey) {
	t.Helper()
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshaling private key for %s: %v", path, err)
	}
	block := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
	if block == nil {
		t.Fatalf("encoding PEM for %s", path)
	}
	// 0644, not a tighter mode: same rootless-UID reasoning as
	// writeCertPEM. This key is generated fresh per test run, lives only
	// inside t.TempDir(), and is never written anywhere else.
	if err := os.WriteFile(path, block, 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

// logTLSIdentity is logEngineIdentity's (podman_test.go) sibling for a TLS
// Lab: the same image ID, major version, and driver version, plus the
// certificate mode this particular container was started with, so a
// result from this file can say which TLS posture it ran against. It reads
// the major version through lab.Admin, whose own TrustServerCertificate is
// always true (see NewTLSLab), so this always succeeds regardless of which
// mode's certificate defect this Lab carries. Deliberately logs nothing
// from Profile: no host, no port, no credentials.
func logTLSIdentity(t *testing.T, lab *Lab, mode string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), readyPingTimeout)
	defer cancel()
	var major int
	if err := lab.Admin.QueryRowContext(ctx, "SELECT CAST(SERVERPROPERTY('ProductMajorVersion') AS int)").Scan(&major); err != nil {
		t.Fatalf("reading engine major version for identity log: %v", err)
	}
	t.Logf("engine identity: image=%s major_version=%d driver=go-mssqldb@%s tls_cert_mode=%s", lab.ImageID, major, driverModuleVersion(), mode)
}

// rawConnectError opens a throwaway connection pool directly against p and
// returns whatever its own PingContext reports - never through
// sqlserver.Open, whose Open and open deliberately discard every
// connection failure down to one fixed *model.PublicError{Code: 3, Kind:
// "connection", Message: "connection failed"} string (internal/sqlserver/
// session.go), on purpose, so the CLI boundary never leaks connection
// detail. Measured directly against a real container for this task: that
// collapse means sqlserver.Open's own error can never be used to tell a
// TLS validation failure apart from a wrong password or a server that is
// not ready yet - both also produce exit code 3 through the production
// path. rawConnectError is how every negative case in this file tells them
// apart instead: go-mssqldb's own error, read directly, says "TLS
// Handshake failed: <specific x509 reason>" for a certificate problem and
// something entirely different (e.g. "mssql: Login failed for user 'sa'.
// (18456)") for anything else - see assertTLSValidationFailure.
func rawConnectError(t *testing.T, p config.Profile) error {
	t.Helper()
	dsn, err := config.DSN(p)
	if err != nil {
		t.Fatalf("building probe connection string: %v", err)
	}
	db, err := sql.Open("sqlserver", dsn)
	if err != nil {
		t.Fatalf("opening probe pool: %v", err)
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), tlsProbeTimeout)
	defer cancel()
	return db.PingContext(ctx)
}

// assertTLSValidationFailure fails t unless err is specifically a TLS
// handshake failure mentioning wantSubstring. This is the check that
// distinguishes "certificate validation actually ran and rejected this
// certificate for the reason this subtest is about" from a superficially
// identical exit-code-3 failure caused by something else entirely (the
// server not ready, the wrong port, a wrong password): any of those would
// also make rawConnectError return a non-nil error, but none of them would
// ever mention "TLS Handshake failed".
func assertTLSValidationFailure(t *testing.T, err error, wantSubstring string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected the raw connection probe to fail, got a successful connection")
	}
	msg := err.Error()
	if !strings.Contains(msg, "TLS Handshake failed") {
		t.Fatalf("expected a TLS handshake failure, got a different kind of error entirely (so this would not actually prove certificate validation did anything): %v", err)
	}
	if !strings.Contains(msg, wantSubstring) {
		t.Fatalf("expected the TLS failure to mention %q, got: %v", wantSubstring, err)
	}
}

// assertConnectionExitCode3 fails t unless err is non-nil and
// model.ExitCode(err) is 3 (connection/auth/TLS). Used only alongside
// assertTLSValidationFailure above, never alone, on every negative case in
// this file: this alone would not distinguish a TLS cause from an
// unrelated one, which is exactly the hollow assertion this task's brief
// warns against.
func assertConnectionExitCode3(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected sqlserver.Open to fail, got a successful session")
	}
	if code := model.ExitCode(err); code != 3 {
		t.Fatalf("got exit code %d, want 3 (connection/auth/TLS): %v", code, err)
	}
}

// assertSessionEncrypted fails t unless the session behind s is, as seen
// from the server side via sys.dm_exec_connections for that session's own
// @@SPID, actually encrypted - not merely assumed from the client-side
// connection string. Identical in spirit to TestSessionTLS's own check
// (session_test.go), reused here on every positive case in this matrix.
func assertSessionEncrypted(t *testing.T, ctx context.Context, lab *Lab, s *sqlserver.Session) {
	t.Helper()
	var spid int
	if err := s.Conn.QueryRowContext(ctx, "SELECT @@SPID").Scan(&spid); err != nil {
		t.Fatalf("reading @@SPID: %v", err)
	}
	var encrypted string
	if err := lab.Admin.QueryRowContext(ctx, "SELECT encrypt_option FROM sys.dm_exec_connections WHERE session_id=@p1", spid).Scan(&encrypted); err != nil {
		t.Fatalf("reading encrypt_option: %v", err)
	}
	if encrypted != "TRUE" {
		t.Fatalf("encrypt_option=%s, want TRUE", encrypted)
	}
}

// TestTLSValidCertificate covers two of this task's matrix cases against
// one container whose certificate has nothing wrong with it: connecting
// with trust_server_certificate: false and the CA that actually signed
// this certificate succeeds, and connecting with trust_server_certificate:
// false and a ca_file naming a completely unrelated CA fails specifically
// on trust (never on hostname or timing, which this container's
// certificate has no problem with).
func TestTLSValidCertificate(t *testing.T) {
	lab := NewTLSLab(t, tlsModeValid)
	logTLSIdentity(t, lab, tlsModeValid)

	t.Run("trust_server_certificate false with the matching CA succeeds", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		p := lab.Profile
		p.TrustServerCertificate = false
		// lab.Profile.CAFile already names the CA that signed this
		// container's own certificate; see NewTLSLab.
		s, err := sqlserver.Open(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		assertSessionEncrypted(t, ctx, lab, s)
	})

	t.Run("trust_server_certificate false with an unrelated CA fails on trust, not on anything else", func(t *testing.T) {
		p := lab.Profile
		p.TrustServerCertificate = false
		p.CAFile = writeUnrelatedCAFile(t, t.TempDir())

		assertTLSValidationFailure(t, rawConnectError(t, p), "certificate signed by unknown authority")

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, err := sqlserver.Open(ctx, p)
		assertConnectionExitCode3(t, err)
	})
}

// TestTLSWrongHostCertificate covers the matrix case for a certificate
// whose CA is trusted but whose SAN never covers the loopback address this
// harness actually connects through: the failure must be pinned to the
// hostname mismatch specifically, never to trust (the CA is the real
// signer) or to timing (the certificate's validity window is ordinary).
func TestTLSWrongHostCertificate(t *testing.T) {
	lab := NewTLSLab(t, tlsModeWrongHost)
	logTLSIdentity(t, lab, tlsModeWrongHost)

	t.Run("trust_server_certificate false fails on hostname, not on trust or timing", func(t *testing.T) {
		p := lab.Profile
		p.TrustServerCertificate = false
		// lab.Profile.CAFile names the CA that legitimately signed
		// this container's certificate: the only defect is the SAN.

		assertTLSValidationFailure(t, rawConnectError(t, p), fmt.Sprintf("cannot validate certificate for %s", p.Host))

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, err := sqlserver.Open(ctx, p)
		assertConnectionExitCode3(t, err)
	})
}

// TestTLSExpiredCertificate covers the remaining two matrix cases against
// one container whose certificate's validity window has already closed:
// trust_server_certificate: true still connects and encrypts despite that
// defect (proving the default genuinely skips validation, not just that it
// happens not to be tested against a broken certificate), and
// trust_server_certificate: false fails specifically on the validity
// window, never on trust (the CA is the real signer) or on hostname (the
// SAN is otherwise correct).
func TestTLSExpiredCertificate(t *testing.T) {
	lab := NewTLSLab(t, tlsModeExpired)
	logTLSIdentity(t, lab, tlsModeExpired)

	t.Run("trust_server_certificate true encrypts despite the expired certificate", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		p := lab.Profile
		p.TrustServerCertificate = true
		s, err := sqlserver.Open(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		assertSessionEncrypted(t, ctx, lab, s)
	})

	t.Run("trust_server_certificate false fails on the validity window, not on trust or hostname", func(t *testing.T) {
		p := lab.Profile
		p.TrustServerCertificate = false

		assertTLSValidationFailure(t, rawConnectError(t, p), "certificate has expired or is not yet valid")

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, err := sqlserver.Open(ctx, p)
		assertConnectionExitCode3(t, err)
	})
}
