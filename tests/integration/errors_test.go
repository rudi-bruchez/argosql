//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rudi-bruchez/argosql/internal/artifacts"
	"github.com/rudi-bruchez/argosql/internal/diagnostics"
	"github.com/rudi-bruchez/argosql/internal/model"
	"github.com/rudi-bruchez/argosql/internal/output"
	"github.com/rudi-bruchez/argosql/internal/sqlserver"
)

// TestErrors covers the two deterministic fault injections this task's
// dispatch flags as needing a REAL container rather than a fake
// driver: a SQL read interrupted partway through, on the single pinned
// connection every asq invocation holds (the exact trap the dispatch
// warns about - an injection that issues anything else on that same
// connection while a row set is still open would measure its own
// harness hanging, not a defect in the program), and a search for
// secrets/DSNs across every byte the real binary emits.
func TestErrors(t *testing.T) {
	lab := NewLab(t, os.Getenv("ASQ_TEST_IMAGE"))
	ctx, cancel := context.WithTimeout(context.Background(), flushPollBudget)
	defer cancel()

	var major int
	if err := lab.Admin.QueryRowContext(ctx, "SELECT CAST(SERVERPROPERTY('ProductMajorVersion') AS int)").Scan(&major); err != nil {
		t.Fatalf("reading engine major version: %v", err)
	}
	logEngineIdentity(t, lab, major)

	t.Run("interrupted_read_does_not_block_pinned_connection", func(t *testing.T) {
		testInterruptedReadClosesRows(ctx, t, lab)
	})
	t.Run("no_secret_leakage_in_diagnostic_output", func(t *testing.T) {
		testNoSecretLeakageInDiagnosticOutput(ctx, t, lab)
	})
	t.Run("output_fallback_tiers", func(t *testing.T) {
		testOutputFallbackTiers(t, lab)
	})
}

// testOutputFallbackTiers is fix 1's A7: the task-15 report claimed no
// real command with this project's own fixtures could ever reach
// design spec line 105's two degraded tiers. A reviewer refuted that
// by measurement on both engines, with two recipes that touch no SQL
// fixture at all - only --out-dir and the positional object name, both
// plain CLI arguments. Both are reproduced here verbatim.
func testOutputFallbackTiers(t *testing.T, lab *Lab) {
	t.Run("tier_a_compact_manifest_response", func(t *testing.T) {
		pw := lab.ensurePrincipals(t)
		configPath, env := buildChildConfigAndEnv(t, lab.Profile, pw, "S")

		// Ten nested components of 200 "<" characters each: valid on a
		// Linux filesystem, and long enough that every model.Artifact
		// path (and the manifest path) this run's metadata carries
		// pushes the zero-row envelope past the 32768-byte stdout cap,
		// even though the real response ends up far smaller once
		// Render falls back.
		deep := t.TempDir()
		for i := 0; i < 10; i++ {
			deep = filepath.Join(deep, strings.Repeat("<", 200))
		}
		if err := os.MkdirAll(deep, 0o700); err != nil {
			t.Fatalf("creating the deep --out-dir: %v", err)
		}

		bin := buildTestBinary(t)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, "--format", "json", "--ctx", labRunContextName, "--config", configPath, "--out-dir", deep, "obj", "table", "dbo.Orders")
		cmd.Env = env
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("obj table dbo.Orders with a deep --out-dir must still succeed (tier A is a valid response, not a failure): %v", err)
		}
		if len(out) > 32768 {
			t.Fatalf("stdout is %d bytes, exceeds the 32768 cap tier A exists to respect", len(out))
		}
		var mr model.ManifestOnlyResult
		if err := json.Unmarshal(bytes.TrimRight(out, "\n"), &mr); err != nil {
			t.Fatalf("decoding tier A's own compact response: %v\noutput: %s", err, out)
		}
		if !mr.PreviewOmitted {
			t.Fatalf("preview_omitted: got false, want true (tier A)")
		}
		if mr.ManifestPath == "" {
			t.Fatal("tier A response carries no manifest_path")
		}
		if _, statErr := os.Stat(mr.ManifestPath); statErr != nil {
			t.Fatalf("tier A's own manifest_path does not exist on disk: %v", statErr)
		}
	})

	t.Run("tier_b_fixed_error_envelope", func(t *testing.T) {
		pw := lab.ensurePrincipals(t)
		configPath, env := buildChildConfigAndEnv(t, lab.Profile, pw, "S")
		outDir := t.TempDir()

		// A syntactically valid two-part name the parser accepts, whose
		// second part is 40,000 "x" characters - resolution fails (no
		// such object), and the resulting "not found" error message
		// embeds the whole name, which is what makes even tier A's own
		// compact response too large this time.
		oversizedName := "dbo." + strings.Repeat("x", 40000)

		bin := buildTestBinary(t)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, "--format", "json", "--ctx", labRunContextName, "--config", configPath, "--out-dir", outDir, "obj", "table", oversizedName)
		cmd.Env = env
		var outBuf, errBuf bytes.Buffer
		cmd.Stdout = &outBuf
		cmd.Stderr = &errBuf
		runErr := cmd.Run()
		code := 0
		if runErr != nil {
			var exitErr *exec.ExitError
			if errors.As(runErr, &exitErr) {
				code = exitErr.ExitCode()
			} else {
				t.Fatalf("running %s: %v\nstderr: %s", bin, runErr, errBuf.String())
			}
		}
		if code != 6 {
			t.Fatalf("exit code: got %d, want 6 (tier B, output_budget)", code)
		}
		out := outBuf.Bytes()
		if len(out) > 32768 {
			t.Fatalf("stdout is %d bytes, exceeds the 32768 cap tier B exists to respect", len(out))
		}
		var fe model.FallbackError
		if err := json.Unmarshal(bytes.TrimRight(out, "\n"), &fe); err != nil {
			t.Fatalf("decoding tier B's own fixed envelope: %v\noutput: %s", err, out)
		}
		if fe.Error == nil || fe.Error.Kind != "output_budget" {
			t.Fatalf("tier B error: got %+v, want kind=output_budget", fe.Error)
		}
		if strings.Contains(string(out), strings.Repeat("x", 1000)) {
			t.Fatal("tier B must never embed the oversized object name")
		}
	})
}

// abortAfterNCollector is fix 1's A6 (points 2 and 3) rewrite of the
// task-15 fixture: a REAL *artifacts.Collector, embedded so Begin/End/
// File/Notice all go through its actual production logic, with only
// Row intercepted to refuse after N rows. The task-15 version used a
// standalone in-memory sink (abortAfterNSink) that the real collector
// and output.Render never touched at all - "sans collecteur ni JSON",
// the reviewer's own wording. This version composes the real
// collector and Render below, so the question this proves is no
// longer only "does the diagnostic close its *sql.Rows on an early
// Row error" (still true, and still checked by the connection-reuse
// probe below) but also "does the resulting model.Result and its
// rendered JSON correctly reflect a run that stopped mid-stream."
//
// This still is not a driver-level read error after N lines (a
// genuine mid-query network/context failure, the form the reviewer
// names as more realistic): no diagnostic in this codebase streams
// enough rows, over enough wall-clock time, for a context deadline to
// land deterministically between two specific rows against a live
// container without flaky timing, and this project's own convention
// (CLAUDE.md, "ERROR et propriétés inconnues via backend de test, pas
// corruption de base") is to reach for a fake driver rather than
// chase that kind of timing against a live engine - which a Sink-level
// refusal approximates here without needing one.
type abortAfterNCollector struct {
	*artifacts.Collector
	n, seen int
}

func (s *abortAfterNCollector) Row(row []model.Cell) error {
	s.seen++
	if s.seen > s.n {
		return fmt.Errorf("errors_test: fixture sink stopped consuming after %d rows", s.n)
	}
	return s.Collector.Row(row)
}

// testInterruptedReadClosesRows opens its OWN session as principal S -
// a TEST principal, never lab.Profile's administrative account (fix
// 1's A6, point 3: "le principal de test doit être un principal de
// test") - runs "idx list" on dbo.UsageFixture through a real
// artifacts.Collector wrapped by abortAfterNCollector, then finishes
// and renders the result for real, checking the composed JSON, before
// reusing the SAME *sql.Conn for a trivial query with a short, bounded
// context. If the diagnostic left its result set open, that trivial
// query hangs until the bounded context expires and this test fails
// with a clear timeout message, never a silent pass.
func testInterruptedReadClosesRows(ctx context.Context, t *testing.T, lab *Lab) {
	pw := lab.ensurePrincipals(t)
	secret, ok := pw.secretFor("S")
	if !ok {
		t.Fatal("no secret on file for principal S")
	}
	profile := principalProfile(lab, principalUsername("S"), secret)
	sess, err := sqlserver.Open(ctx, profile)
	if err != nil {
		t.Fatalf("open as principal S: %v", err)
	}
	defer sess.Close()

	collector, err := artifacts.New(t.TempDir(), "json", artifacts.Limits{Rows: 10000, Bytes: 104857600})
	if err != nil {
		t.Fatalf("artifacts.New: %v", err)
	}
	sink := &abortAfterNCollector{Collector: collector, n: 1}
	indexesErr := diagnostics.Indexes(ctx, sess, "dbo.UsageFixture", sink)
	if indexesErr == nil {
		t.Fatal("Indexes must return the sink's own abort error, got nil")
	}
	if sink.seen < 2 {
		t.Fatalf("fixture produced only %d index row(s) before returning; dbo.UsageFixture's own definition (objects.sql) guarantees at least 3 and this test cannot prove a mid-stream interruption without at least 2", sink.seen)
	}

	info := model.ContextInfo{Server: profile.Host, Database: profile.Database, Principal: profile.Username}
	result, finishErr := collector.Finish(info, indexesErr)
	finalErr := indexesErr
	if finishErr != nil {
		finalErr = finishErr
	}
	out, renderErr := output.Render(result, output.PreviewOptions{Rows: 10, ByteLimit: 32768}, "json")
	if renderErr != nil {
		t.Fatalf("Render must still produce a response for an interrupted run: %v", renderErr)
	}
	var decoded struct {
		OK    bool `json:"ok"`
		Error struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("the composed JSON for an interrupted read must still be valid and complete: %v\noutput: %s", err, out)
	}
	if decoded.OK {
		t.Fatal("decoded JSON: ok=true, want false (the read was interrupted)")
	}
	if model.ExitCode(finalErr) != decoded.Error.Code {
		t.Fatalf("decoded JSON error.code=%d does not match model.ExitCode(finalErr)=%d", decoded.Error.Code, model.ExitCode(finalErr))
	}

	probeCtx, probeCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer probeCancel()
	var probe int
	if err := sess.Conn.QueryRowContext(probeCtx, "SELECT 1").Scan(&probe); err != nil {
		t.Fatalf("a trivial query on the same connection after an aborted read must succeed promptly, not hang or error: %v (if this timed out, the diagnostic left its result set open on the one pinned connection)", err)
	}
	if probe != 1 {
		t.Fatalf("probe query: got %d, want 1", probe)
	}
}

// testNoSecretLeakageInDiagnosticOutput is design spec line 261's own
// last sentence: search every byte the real binary emits - stdout,
// stderr, and the manifest it writes to disk - for a secret, under
// every form it could leak in. It never prints the secret itself in a
// failure message (CLAUDE.md's own recorded defect, paid for once
// already on this exact guarantee): a match is reported by WHERE it
// was found, never by the value found.
//
// Fix 1's A1: the task-15 version of this test only searched for the
// literal password in three invocations that never produce an error
// containing it at all (info, qs status, and a not-found query_id all
// fail, if they fail, for reasons unrelated to the secret). The
// reviewer's real repro needs an error whose MESSAGE embeds the
// secret, and needs the search done on the JSON-DECODED value, not on
// raw bytes: JSON escaping can make a literal byte search answer false
// while the decoded string still carries the secret in the clear.
// benign_invocations below keeps the cheap sanity net the three
// original invocations gave; storage_failure_embeds_secret is the real
// reproduction.
func testNoSecretLeakageInDiagnosticOutput(ctx context.Context, t *testing.T, lab *Lab) {
	pw := lab.ensurePrincipals(t)
	secret := pw.S

	t.Run("benign_invocations", func(t *testing.T) {
		for _, step := range []struct {
			name string
			args []string
		}{
			{"info", []string{"info"}},
			{"qs status", []string{"qs", "status"}},
			{"bad_query_id", []string{"qs", "query", "999999999"}},
		} {
			t.Run(step.name, func(t *testing.T) {
				stdout, stderr, _ := lab.RunRaw(t, "S", step.args)
				if strings.Contains(stdout, secret) {
					t.Fatalf("%v: stdout leaks the connection secret", step.args)
				}
				if strings.Contains(stderr, secret) {
					t.Fatalf("%v: stderr leaks the connection secret", step.args)
				}

				var result model.Result
				if jsonErr := json.Unmarshal([]byte(stdout), &result); jsonErr == nil && result.ManifestPath != "" {
					data, readErr := os.ReadFile(result.ManifestPath)
					if readErr != nil {
						t.Fatalf("%v: reading manifest %q: %v", step.args, result.ManifestPath, readErr)
					}
					if strings.Contains(string(data), secret) {
						t.Fatalf("%v: manifest %q leaks the connection secret", step.args, result.ManifestPath)
					}
				}
			})
		}
	})

	t.Run("storage_failure_embeds_secret", func(t *testing.T) {
		testStorageFailureNeverLeaksSecret(ctx, t, lab, pw)
	})
}

// testStorageFailureNeverLeaksSecret reproduces the reviewer's own
// repro exactly: --out-dir names an ordinary file (never a directory),
// whose NAME embeds a secret, so newStore's own error message
// ("artifacts: creating output directory %q: ...") embeds that secret
// directly - the realistic path the reviewer names, a driver or
// storage error that happens to echo a connection detail, rather than
// an artificial string search.
//
// The secret is synthetic, not asq_test_s's normal fixture password,
// specifically so it carries a JSON-escape-significant character (a
// double quote and a backslash) the way the reviewer's own 2022 probe
// did: a literal byte search on stdout can answer false once json.Marshal
// has escaped that character, while the decoded string still carries it.
// The login's real password is restored once this subtest returns.
func testStorageFailureNeverLeaksSecret(ctx context.Context, t *testing.T, lab *Lab, pw principalPasswords) {
	const synthetic = `Secr3t"Quote\Back`
	originalS, ok := pw.secretFor("S")
	if !ok {
		t.Fatal("no secret on file for principal S")
	}

	quoted := strings.ReplaceAll(synthetic, "'", "''")
	if _, err := lab.Admin.ExecContext(ctx, "ALTER LOGIN asq_test_s WITH PASSWORD = N'"+quoted+"'"); err != nil {
		t.Fatalf("setting synthetic password on asq_test_s: %v", err)
	}
	t.Cleanup(func() {
		restoreCtx, cancel := context.WithTimeout(context.Background(), podmanCmdTimeout)
		defer cancel()
		restoreQuoted := strings.ReplaceAll(originalS, "'", "''")
		if _, err := lab.Admin.ExecContext(restoreCtx, "ALTER LOGIN asq_test_s WITH PASSWORD = N'"+restoreQuoted+"'"); err != nil {
			t.Logf("cleanup: restoring asq_test_s's original password: %v", err)
		}
	})

	syntheticPW := principalPasswords{S: synthetic}
	configPath, env := buildChildConfigAndEnv(t, lab.Profile, syntheticPW, "S")

	blocker := filepath.Join(t.TempDir(), "out-"+synthetic)
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	bin := buildTestBinary(t)
	runCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(runCtx, bin, "--format", "json", "--ctx", labRunContextName, "--config", configPath, "--out-dir", blocker, "info")
	cmd.Env = env
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	runErr := cmd.Run()

	code := 0
	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			code = exitErr.ExitCode()
		} else {
			t.Fatalf("running %s: %v\nstderr: %s", bin, runErr, errBuf.String())
		}
	}
	if code != 6 {
		t.Fatalf("exit code: got %d, want 6 (a regular file as --out-dir is a storage failure, design spec lines 105/210)", code)
	}

	stdout, stderr := outBuf.String(), errBuf.String()
	if strings.Contains(stderr, synthetic) {
		t.Fatal("stderr leaks the secret")
	}
	if strings.Contains(stdout, synthetic) {
		t.Fatal("stdout raw bytes leak the secret (this alone would have missed the JSON-escaped case the reviewer measured - kept as a belt-and-suspenders check, not the decisive one)")
	}

	var envelope model.FallbackError
	if err := json.Unmarshal(outBuf.Bytes(), &envelope); err != nil {
		t.Fatalf("decoding stdout as JSON: %v\nstdout: %s", err, stdout)
	}
	if envelope.Error == nil {
		t.Fatalf("expected a non-nil error in the envelope, got: %s", stdout)
	}
	if strings.Contains(envelope.Error.Message, synthetic) {
		t.Fatalf("error.message, once JSON-DECODED, still carries the secret: %q", envelope.Error.Message)
	}
}
