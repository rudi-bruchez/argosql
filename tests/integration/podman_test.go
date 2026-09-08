//go:build integration

package integration

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rudi-bruchez/argosql/internal/config"
)

// testRunID tags every container this test binary creates, via the
// io.argosql.test label, so a final "podman ps -a --filter
// label=io.argosql.test=<value>" audit can find everything one run left
// behind (it should find nothing once every Lab's t.Cleanup has fired). It
// is computed once per test binary, not once per Lab: several Labs created
// in the same run share it, which is what makes a single audit command
// enough for the whole run.
var testRunID = randomHex(6)

// readyTimeout bounds the whole polling loop that waits for a freshly
// started container's SQL Server to accept connections. bootstrapTimeout
// bounds applying sql/bootstrap.sql once it does.
const (
	readyTimeout     = 90 * time.Second
	readyPollDelay   = 500 * time.Millisecond
	readyPingTimeout = 3 * time.Second
	bootstrapTimeout = 60 * time.Second
	podmanCmdTimeout = 30 * time.Second
)

// Lab is one disposable SQL Server container plus everything a test needs
// to talk to it: Admin is a pool already connected to the AppDB fixture
// database created by sql/bootstrap.sql, Profile is the connection profile
// tests hand to sqlserver.Open, and ContainerID/ImageID identify the
// container and the resolved image it was started from, for reporting.
type Lab struct {
	Admin       *sql.DB
	Profile     config.Profile
	ContainerID string
	ImageID     string
}

// NewLab starts a brand-new, uniquely named SQL Server container from
// image, waits for it to accept connections, applies the fixture
// bootstrap, and registers cleanup that closes Admin and removes the
// container by ID. It never reuses or touches any container that already
// exists on the host: every container it can see belongs to someone else.
//
// image must be non-empty (ASQ_TEST_IMAGE unset is an explicit t.Fatal,
// never a silent t.Skip: a test in this package that runs at all must
// prove the harness actually exercised a real server), and podman must be
// on PATH.
func NewLab(t *testing.T, image string) *Lab {
	t.Helper()

	if image == "" {
		t.Fatal("ASQ_TEST_IMAGE is not set: the integration suite requires an image and never chooses one on its own")
	}
	if _, err := exec.LookPath("podman"); err != nil {
		t.Fatalf("podman not found on PATH: %v", err)
	}

	name := "asq-test-" + randomHex(6)
	password := randomPassword()

	envPath := filepath.Join(t.TempDir(), "env")
	envContent := "ACCEPT_EULA=Y\nMSSQL_PID=Developer\nMSSQL_SA_PASSWORD=" + password + "\n"
	if err := os.WriteFile(envPath, []byte(envContent), 0o600); err != nil {
		t.Fatalf("writing container env file: %v", err)
	}

	containerID := podmanRun(t, name, image, envPath)

	// Registered immediately after the container exists, before anything
	// that can fail below: a readiness timeout or a bad bootstrap must
	// still leave nothing of this Lab's container behind.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), podmanCmdTimeout)
		defer cancel()
		if err := exec.CommandContext(ctx, "podman", "rm", "--force", containerID).Run(); err != nil {
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
	// closes Admin before the container is removed, matching the brief's
	// "closes its clients, then removes the container by ID".
	t.Cleanup(func() { appDB.Close() })

	return &Lab{
		Admin:       appDB,
		Profile:     appProfile,
		ContainerID: containerID,
		ImageID:     imageID,
	}
}

// podmanRun starts the container detached, labeled for this run, and
// publishing SQL Server's port on a loopback address Podman assigns
// itself, then returns its ID. The container name is always random and
// harness-generated, never derived from anything SQL-related.
func podmanRun(t *testing.T, name, image, envPath string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), podmanCmdTimeout)
	defer cancel()
	args := []string{
		"run", "--detach",
		"--name", name,
		"--label", "io.argosql.test=" + testRunID,
		"--publish", "127.0.0.1::1433",
		"--env-file", envPath,
		image,
	}
	out, err := exec.CommandContext(ctx, "podman", args...).Output()
	if err != nil {
		t.Fatalf("podman run: %v", describeExecError(err))
	}
	id := strings.TrimSpace(string(out))
	if id == "" {
		t.Fatal("podman run produced no container ID")
	}
	return id
}

// podmanPublishedAddr resolves the loopback host and port Podman actually
// bound container port 1433/tcp to. --publish 127.0.0.1::1433 leaves the
// host port for Podman to pick, so this is the only way to learn it.
func podmanPublishedAddr(t *testing.T, containerID string) (string, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), podmanCmdTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "podman", "port", containerID, "1433/tcp").Output()
	if err != nil {
		t.Fatalf("podman port: %v", describeExecError(err))
	}
	line := strings.TrimSpace(out2firstLine(string(out)))
	host, portStr, err := net.SplitHostPort(line)
	if err != nil {
		t.Fatalf("parsing %q from podman port: %v", line, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parsing published port %q: %v", portStr, err)
	}
	return host, port
}

func out2firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// podmanInspectImage resolves the container's image ID. It asks for
// exactly the {{.Image}} field: inspect's default output also includes
// the container's Env, which carries MSSQL_SA_PASSWORD, and must never be
// requested or printed.
func podmanInspectImage(t *testing.T, containerID string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), podmanCmdTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "podman", "inspect", "--format", "{{.Image}}", containerID).Output()
	if err != nil {
		t.Fatalf("podman inspect: %v", describeExecError(err))
	}
	return strings.TrimSpace(string(out))
}

// waitReady polls PingContext against dsn until it succeeds or
// readyTimeout elapses. Each attempt gets its own short-lived context
// (readyPingTimeout) and a fresh *sql.DB, so one wedged attempt cannot
// consume the whole budget or poison a later one; a short delay separates
// attempts, never a single fixed sleep for the whole wait.
func waitReady(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	deadline := time.Now().Add(readyTimeout)
	var lastErr error
	for {
		db, err := sql.Open("sqlserver", dsn)
		if err != nil {
			lastErr = err
		} else {
			pingCtx, cancel := context.WithTimeout(context.Background(), readyPingTimeout)
			lastErr = db.PingContext(pingCtx)
			cancel()
			if lastErr == nil {
				return db
			}
			db.Close()
		}
		if time.Now().After(deadline) {
			t.Fatalf("container did not become ready within %s: %v", readyTimeout, lastErr)
		}
		time.Sleep(readyPollDelay)
	}
}

// describeExecError renders err with the failed command's stderr, when
// there is one, so a t.Fatal on a podman failure is actionable. Output()
// records stderr on *exec.ExitError itself; this never reaches for the
// command's or the process's environment.
func describeExecError(err error) string {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return fmt.Sprintf("%v: %s", err, strings.TrimSpace(string(exitErr.Stderr)))
	}
	return err.Error()
}

// randomHex returns n bytes of crypto/rand as a hex string, used for
// container name suffixes and the run label. Never used for the SQL
// password: randomPassword below guarantees the character classes SQL
// Server's password policy checks for, which a plain hex string does not.
func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("crypto/rand: %v", err))
	}
	return hex.EncodeToString(b)
}

const (
	pwUpper   = "ABCDEFGHJKLMNPQRSTUVWXYZ"
	pwLower   = "abcdefghijkmnpqrstuvwxyz"
	pwDigit   = "23456789"
	pwSymbol  = "!@#$%^&*-_="
	pwPerBand = 4
)

// randomPassword generates a crypto/rand password for the container's own
// sa account: at least pwPerBand characters from each of upper, lower,
// digit and symbol (SQL Server's default policy requires 3 of 4 classes;
// this always supplies all four), shuffled, then written once into a 0600
// env file. It never appears on a command line, in a log, or in a test
// failure message.
func randomPassword() string {
	var b []byte
	b = append(b, randomFrom(pwUpper, pwPerBand)...)
	b = append(b, randomFrom(pwLower, pwPerBand)...)
	b = append(b, randomFrom(pwDigit, pwPerBand)...)
	b = append(b, randomFrom(pwSymbol, pwPerBand)...)
	shuffleBytes(b)
	return string(b)
}

func randomFrom(alphabet string, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = alphabet[randIndex(len(alphabet))]
	}
	return out
}

func shuffleBytes(b []byte) {
	for i := len(b) - 1; i > 0; i-- {
		j := randIndex(i + 1)
		b[i], b[j] = b[j], b[i]
	}
}

func randIndex(n int) int {
	v, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		panic(fmt.Sprintf("crypto/rand: %v", err))
	}
	return int(v.Int64())
}
