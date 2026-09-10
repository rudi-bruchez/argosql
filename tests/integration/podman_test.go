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
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"syscall"
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
//
// ASQ_TEST_RUN_ID overrides the random default - fix 1's A6: a parent
// process that builds and runs this package's own test binary as a
// child subprocess (TestSignalInterruptRemovesContainer) needs to know
// the child's run ID in advance to filter on it, which a purely random
// value computed inside the child would never let it do.
var testRunID = func() string {
	if v := os.Getenv("ASQ_TEST_RUN_ID"); v != "" {
		return v
	}
	return randomHex(6)
}()

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

// TestMain installs the safety net t.Cleanup cannot provide: Go's testing
// framework only runs a test's registered Cleanup funcs when that test
// returns normally, never when the process dies from a signal. Measured:
// a build of this package from before this handler existed, run under
// `timeout --signal=INT <n> <binary>`, left its container behind in
// "Stopping" - never removed, recoverable only by hand with a
// SIGTERM-then-SIGKILL `podman rm`. Every day this suite is interrupted at
// the keyboard (Ctrl-C is not a rare event) is a day that leaves a ~1.5GB
// SQL Server container running until someone notices.
//
// On the first SIGINT or SIGTERM, this removes every container labeled
// io.argosql.test for this process's own run ID, then restores that
// signal's default disposition and re-delivers it to this process. It
// does not decide how the process should terminate and does not swallow
// the signal: it only buys itself enough time, once, to clean up before
// stepping out of the way and letting the normal default behavior (the
// process dying) happen as it would have without this handler.
func TestMain(m *testing.M) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		fmt.Fprintf(os.Stderr, "\nintegration tests: received %s, removing containers labeled io.argosql.test=%s before exiting\n", sig, testRunID)

		ctx, cancel := context.WithTimeout(context.Background(), podmanCmdTimeout)
		removeContainersByLabel(ctx, "label=io.argosql.test="+testRunID)
		cancel()

		signal.Stop(sigCh)
		signal.Reset(sig)
		if p, err := os.FindProcess(os.Getpid()); err == nil {
			if err := p.Signal(sig); err != nil {
				fmt.Fprintf(os.Stderr, "integration tests: re-delivering %s to self: %v\n", sig, err)
			}
		}
	}()
	os.Exit(m.Run())
}

// podmanPS lists container IDs matching a single --filter expression
// (e.g. "label=io.argosql.test=<value>" or "id=<id>"), including stopped
// containers.
func podmanPS(ctx context.Context, filter string) ([]string, error) {
	out, err := exec.CommandContext(ctx, "podman", "ps", "-aq", "--filter", filter).Output()
	if err != nil {
		return nil, err
	}
	return strings.Fields(string(out)), nil
}

// podmanContainerIDs is podmanPS for use from a test: it fails the test,
// rather than returning an error, on a podman failure.
func podmanContainerIDs(t *testing.T, filter string) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), podmanCmdTimeout)
	defer cancel()
	ids, err := podmanPS(ctx, filter)
	if err != nil {
		t.Fatalf("podman ps --filter %q: %v", filter, describeExecError(err))
	}
	return ids
}

// removeContainersByLabel is TestMain's signal handler's only job: find
// every container matching labelFilter and force-remove it by ID. It has
// no *testing.T to report through (it runs outside any test), so a
// failure here is logged to stderr, best-effort, rather than failing
// anything.
//
// --time 0 matters here, measured: "podman rm --force" on its own still
// waits up to its default 10s stop grace period before killing a running
// container, which on this harness is long enough for the interrupted
// test's own goroutine to race ahead and finish normally before this
// removal (and the signal re-delivery that follows it) ever completes -
// the exact failure mode this handler exists to prevent. --time 0 skips
// the grace period and kills outright.
func removeContainersByLabel(ctx context.Context, labelFilter string) {
	ids, err := podmanPS(ctx, labelFilter)
	if err != nil {
		fmt.Fprintf(os.Stderr, "signal cleanup: listing containers (%s): %v\n", labelFilter, describeExecError(err))
		return
	}
	if len(ids) == 0 {
		return
	}
	args := append([]string{"rm", "--force", "--time", "0"}, ids...)
	if err := exec.CommandContext(ctx, "podman", args...).Run(); err != nil {
		fmt.Fprintf(os.Stderr, "signal cleanup: removing containers %v: %v\n", ids, describeExecError(err))
	}
}

// driverModuleVersion reports the exact github.com/microsoft/go-mssqldb
// version this test binary was built against, from the binary's own build
// info rather than a hand-maintained constant, so it cannot drift from
// go.mod. Used only for t.Logf identity lines (see logEngineIdentity);
// never for anything that affects behavior.
func driverModuleVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	for _, dep := range info.Deps {
		if dep.Path == "github.com/microsoft/go-mssqldb" {
			return dep.Version
		}
	}
	return "unknown"
}

// engineProductVersion reads SERVERPROPERTY('ProductVersion') directly
// (the full dotted version - "15.0.4480.2", "16.0.4265.3" - not the
// major number alone), via lab's own Admin pool. Fix 1's A8: the
// resolved image ID identifies an IMAGE, which is not the same
// observation as the engine version a release record actually needs
// (design spec lines 248/252/265); "unknown" on any read failure,
// never fatal - logEngineIdentity's whole point is to record what was
// actually run against, and a failed version probe is itself a fact
// worth logging rather than aborting the test over.
func engineProductVersion(lab *Lab) string {
	ctx, cancel := context.WithTimeout(context.Background(), podmanCmdTimeout)
	defer cancel()
	var v string
	if err := lab.Admin.QueryRowContext(ctx, "SELECT CAST(SERVERPROPERTY('ProductVersion') AS nvarchar(128))").Scan(&v); err != nil {
		return "unknown"
	}
	return v
}

// logEngineIdentity records what a diagnostics test actually ran against:
// the resolved image ID (never the mutable "latest" tag), the server's
// major version and its full ProductVersion, the Go toolchain this test
// binary was built with, and the driver version. A result from a month
// ago is otherwise unable to say what it was measured against - fix 1's
// A8 names the image digest alone as insufficient for exactly this
// reason: it identifies an image, not the engine version actually
// observed, nor the Go version a release record also requires.
// Deliberately logs nothing from Profile: no host, no port, no
// credentials.
func logEngineIdentity(t *testing.T, lab *Lab, majorVersion int) {
	t.Helper()
	t.Logf("engine identity: image=%s major_version=%d product_version=%s go=%s driver=go-mssqldb@%s",
		lab.ImageID, majorVersion, engineProductVersion(lab), runtime.Version(), driverModuleVersion())
}

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

	// principalsOnce/principalsErr/secrets back Lab.Run (fixture_test.go):
	// the Q/I/S SQL logins Lab.Run's subprocess needs are created lazily,
	// once per Lab, the first time any test on this Lab actually needs
	// one - not by NewLab itself, which every test in this package calls
	// whether or not it ever runs a command as one of these principals.
	// See ensurePrincipals' own doc comment for why this reuses
	// permissions_test.go's existing applyObjects/applyPrincipals rather
	// than a second, divergent definition of the same three principals.
	principalsOnce sync.Once
	principalsErr  error
	secrets        principalPasswords
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

// sameContainer compares two container ID strings that may be truncated
// to different lengths by whichever podman subcommand produced them, by
// comparing their shared prefix.
func sameContainer(a, b string) bool {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	if n == 0 {
		return false
	}
	return a[:n] == b[:n]
}

// TestContainerCarriesRunLabel proves the one property the rest of this
// harness's safety net depends on: a container NewLab starts carries
// io.argosql.test set to this process's own run ID, and a podman filter
// query on that exact label value finds it. removeContainersByLabel
// (TestMain's SIGINT/SIGTERM handler, above) uses precisely this filter;
// if the label were ever dropped, renamed, or misspelled on the podman run
// call, every other test in this package would still pass - only this
// test, or a real signal, would notice.
//
// The handler's own code path - receiving an actual SIGINT or SIGTERM -
// is covered separately, by TestSignalInterruptRemovesContainer below,
// which runs this package's own compiled test binary as a subprocess and
// signals it for real. This test only needs the label mechanism the
// handler is built on; it does not attempt to simulate an interrupt.
func TestContainerCarriesRunLabel(t *testing.T) {
	lab := NewLab(t, os.Getenv("ASQ_TEST_IMAGE"))

	ids := podmanContainerIDs(t, "label=io.argosql.test="+testRunID)
	found := false
	for _, id := range ids {
		if sameContainer(id, lab.ContainerID) {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("container %s not found via --filter label=io.argosql.test=%s (filter returned: %v)", lab.ContainerID, testRunID, ids)
	}
}

// TestCleanupRemovesByID proves NewLab's t.Cleanup removes a container by
// its ID, not by the name NewLab happened to give it - the distinction
// matters because a name can collide with a container that belongs to the
// user (see this package's doc comments on never touching one), while an
// ID cannot.
//
// It renames the container immediately after NewLab creates it, inside a
// subtest, so that the subtest's own t.Run returns only once NewLab's
// t.Cleanup (registered on the subtest's *testing.T) has already run.
// NewLab's cleanup logs and continues on a podman rm failure rather than
// failing the test (deliberately: one bad cleanup must not fail an
// otherwise-passing test), so a cleanup that still tried to remove the
// renamed container by its old name would fail silently from NewLab's
// point of view and leave the container running under its new name - this
// is checked from outside, by querying podman for the ID after the
// subtest has fully unwound, exactly like the SIGINT drill in this
// package's task report is checked by podman ps -a rather than by an
// assertion inside the interrupted test.
func TestCleanupRemovesByID(t *testing.T) {
	image := os.Getenv("ASQ_TEST_IMAGE")
	var containerID string
	t.Run("create and rename", func(t *testing.T) {
		lab := NewLab(t, image)
		containerID = lab.ContainerID
		newName := "asq-test-renamed-" + randomHex(4)
		ctx, cancel := context.WithTimeout(context.Background(), podmanCmdTimeout)
		defer cancel()
		if err := exec.CommandContext(ctx, "podman", "rename", lab.ContainerID, newName).Run(); err != nil {
			t.Fatalf("renaming container for the by-ID cleanup check: %v", describeExecError(err))
		}
	})
	// By the time t.Run above returns, every Cleanup registered on its
	// child *testing.T - including NewLab's podman rm - has already run.
	if ids := podmanContainerIDs(t, "id="+containerID); len(ids) != 0 {
		t.Fatalf("container %s is still present after its subtest returned: cleanup did not remove it by ID (it was renamed before cleanup ran, so removal by name would have missed it)", containerID)
	}
}

// TestSignalInterruptRemovesContainer reproduces, in-process, the defect
// TestMain's signal handler exists to fix: it builds this package's own
// test binary, runs it as a subprocess with ASQ_TEST_IMAGE and a known,
// unique ASQ_TEST_RUN_ID set, waits (bounded, polling, never a fixed
// sleep) for that child to have actually created a container carrying
// that exact run ID, sends it SIGINT, and asserts that no container
// carrying it survives once the child has exited.
//
// Fix 1's A6: this used to filter on the bare "io.argosql.test" label,
// with no run ID - which made it fail whenever ANY other container
// anywhere carried that label, including another concurrent run of this
// same suite's own containers. Measured by two independent reviewers at
// once, each breaking the other's run without realizing it. Filtering
// on childRunID, generated here and handed to the child, means this
// test's own correctness no longer depends on being the only thing
// using Podman on the host.
func TestSignalInterruptRemovesContainer(t *testing.T) {
	image := os.Getenv("ASQ_TEST_IMAGE")
	if image == "" {
		t.Fatal("ASQ_TEST_IMAGE is not set: the integration suite requires an image and never chooses one on its own")
	}
	if _, err := exec.LookPath("podman"); err != nil {
		t.Fatalf("podman not found on PATH: %v", err)
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Fatalf("go not found on PATH: %v", err)
	}

	childRunID := "sigdrill-" + randomHex(6)
	childLabelFilter := "label=io.argosql.test=" + childRunID

	moduleRoot, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}").Output()
	if err != nil {
		t.Fatalf("locating module root: %v", describeExecError(err))
	}

	binPath := filepath.Join(t.TempDir(), "integration.test")
	buildCtx, buildCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer buildCancel()
	buildCmd := exec.CommandContext(buildCtx, "go", "test", "-tags=integration", "-c", "-o", binPath, "./tests/integration")
	buildCmd.Dir = strings.TrimSpace(string(moduleRoot))
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("building test binary for the signal drill: %v\n%s", err, out)
	}

	cmd := exec.Command(binPath, "-test.run=TestSessionTLS", "-test.count=1", "-test.v")
	cmd.Env = append(os.Environ(), "ASQ_TEST_IMAGE="+image, "ASQ_TEST_RUN_ID="+childRunID)
	var childOut strings.Builder
	cmd.Stdout = &childOut
	cmd.Stderr = &childOut
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting child test binary: %v", err)
	}

	// Wait for the child to have created a container - not a fixed sleep:
	// poll for the label's appearance, bounded, short delay per attempt.
	var childContainers []string
	deadline := time.Now().Add(60 * time.Second)
	for {
		childContainers = podmanContainerIDs(t, childLabelFilter)
		if len(childContainers) > 0 {
			break
		}
		if time.Now().After(deadline) {
			cmd.Process.Kill()
			cmd.Wait()
			t.Fatalf("child process never created a container labeled %q within 60s; child output:\n%s", childLabelFilter, childOut.String())
		}
		time.Sleep(300 * time.Millisecond)
	}

	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("sending SIGINT to child process: %v", err)
	}

	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()
	select {
	case <-waitDone:
		// The child is expected to exit non-zero (interrupted); that is
		// not itself a failure of this test.
	case <-time.After(30 * time.Second):
		cmd.Process.Kill()
		t.Fatalf("child process did not exit within 30s of SIGINT; child output:\n%s", childOut.String())
	}

	after := podmanContainerIDs(t, childLabelFilter)
	if len(after) != 0 {
		// This test just proved the orphan the handler is supposed to
		// prevent; do not also leave it behind.
		for _, id := range after {
			rmCtx, rmCancel := context.WithTimeout(context.Background(), podmanCmdTimeout)
			exec.CommandContext(rmCtx, "podman", "rm", "--force", "--time", "0", id).Run()
			rmCancel()
		}
		t.Fatalf("containers survived SIGINT to the child process: %v\nchild output:\n%s", after, childOut.String())
	}
}
