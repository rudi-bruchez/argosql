package artifacts

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/rudi-bruchez/argosql/internal/model"
	"github.com/rudi-bruchez/argosql/internal/output"
)

// Task 15's own deterministic fault injections for internal/artifacts,
// filling the gaps collector_test.go/manifest_test.go/store_test.go left
// open rather than duplicating what they already prove:
//
//   - TestDiskFailureDuringFlush/FileClose (collector_test.go) already
//     cover a write failing mid-flush at exit code 6 - this file adds no
//     second copy of that fault.
//   - TestManifestReservationReclaimsIncompleteKeepsComplete/
//     TestManifestReservationFailsCode6WithoutClaimingComplete
//     (manifest_test.go) already cover "manifeste trop long" (design
//     spec line 105/111) for a table-row manifest overflow.
//   - TestFileCollisionAvoidsSymlink (collector_test.go) proves store.create
//     routes around a planted symlink; it never proves the same for an
//     ordinary pre-existing regular file, which TestBeginNeverOverwrites
//     ExistingRegularFile below does.
//   - TestFileLimitSingleSource (collector_test.go) proves one oversized
//     File() call is refused; it never proves a SECOND File() call is
//     refused while the FIRST, already-complete one survives, which is
//     the actual "dépassement pendant le deuxième fichier" the brief
//     names. TestFileSecondSourceOverflowRetainsFirstCompleted below is
//     that case.
//   - sanitizeName (store.go) had no test at all before this task.

// TestNewReturnsCode6WhenStoreCreationFails is fix 1's A2: New must
// wrap a store-creation failure as the project's own code-6 artifact
// error, not let the underlying os.MkdirAll/os.Mkdir error reach the
// CLI unwrapped - which publicErrorOf falls back to classifying as a
// generic code-5 execution error. Design spec lines 105 and 210 both
// name a local storage/output failure code 6; measured before this
// fix, --out-dir pointed at an ordinary file made "info" return 5 on
// both 2019 and 2022.
func TestNewReturnsCode6WhenStoreCreationFails(t *testing.T) {
	// An ordinary file, not a directory, at the path New will try to
	// MkdirAll: os.MkdirAll refuses to create a directory where a file
	// already exists.
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := New(blocker, "json", Limits{Rows: 1000, Bytes: 1 << 20})
	if err == nil {
		t.Fatal("New must fail when its base directory is an ordinary file")
	}
	if code := model.ExitCode(err); code != 6 {
		t.Fatalf("exit code: got %d, want 6, err=%v", code, err)
	}
}

// failWriter is the brief's own verbatim fault: a writer that refuses
// every write it is ever given. Test-local, no production flag.
type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("fixture disk failure") }

// TestEncoderFailsAtNthWriteDoesNotPanic is "échec à la Nième écriture"
// at its narrowest: output.NewTableEncoder over a writer that refuses
// every underlying Write must surface a non-nil error from WriteRow or
// Close, never panic, and never report success. bufio buffers a single
// small row internally (see collector.go's own measureTotal doc
// comment on why a shadow encoder cannot see a failure early); Close's
// forced flush is what guarantees the failure actually reaches the
// caller.
func TestEncoderFailsAtNthWriteDoesNotPanic(t *testing.T) {
	spec := intSpec("x")
	enc, err := output.NewTableEncoder(failWriter{}, "tsv", spec)
	if err != nil {
		// An error straight out of the constructor is an acceptable way
		// to surface the fault too; either way nothing must panic.
		return
	}
	writeErr := enc.WriteRow([]model.Cell{int64(1)})
	closeErr := enc.Close()
	if writeErr == nil && closeErr == nil {
		t.Fatal("a writer that refuses every Write must surface a non-nil error from WriteRow or Close, got neither")
	}
}

// TestBeginNeverOverwritesExistingRegularFile proves store.create's
// O_EXCL guarantee against a PLAIN pre-existing file, not only a
// symlink: design spec line 109, "Never overwrite existing files".
// Break it by dropping os.O_EXCL from store.create's OpenFile call and
// this test must fail, because Begin would then truncate the planted
// file in place.
func TestBeginNeverOverwritesExistingRegularFile(t *testing.T) {
	c, err := New(t.TempDir(), "json", Limits{Rows: 1000, Bytes: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })

	preexisting := filepath.Join(c.Dir(), "x.json")
	const sentinel = "do not touch this pre-existing artifact"
	if err := os.WriteFile(preexisting, []byte(sentinel), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := c.Begin(intSpec("x")); err != nil {
		t.Fatalf("Begin must route around the collision, not fail: %v", err)
	}
	if err := c.Row([]model.Cell{int64(7)}); err != nil {
		t.Fatal(err)
	}
	if err := c.End(true, true); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(preexisting)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != sentinel {
		t.Fatalf("pre-existing artifact was overwritten: got %q, want %q", data, sentinel)
	}
	if c.artifacts[0].Path == preexisting {
		t.Fatalf("the new artifact must not have been written through the pre-existing path")
	}
}

// TestSanitizedFilenameNeverEscapesRunDir proves sanitizeName actually
// neutralizes a hostile table/artifact name - design spec line 109,
// "sanitized generated filenames" - which had no test at all before
// this task. A name built to look like a path-traversal attempt must
// still land as a single file directly under the run directory.
func TestSanitizedFilenameNeverEscapesRunDir(t *testing.T) {
	c, err := New(t.TempDir(), "json", Limits{Rows: 1000, Bytes: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	hostile := "../../etc/passwd"
	if err := c.Begin(intSpec(hostile)); err != nil {
		t.Fatal(err)
	}
	if err := c.Row([]model.Cell{int64(1)}); err != nil {
		t.Fatal(err)
	}
	if err := c.End(true, true); err != nil {
		t.Fatal(err)
	}

	path := c.artifacts[0].Path
	if filepath.Dir(path) != c.Dir() {
		t.Fatalf("artifact for a hostile name escaped the run directory: %q (run dir %q)", path, c.Dir())
	}
	if _, err := os.Stat(filepath.Join(c.Dir(), "..", "..", "etc", "passwd")); !os.IsNotExist(err) {
		t.Fatalf("a file was created outside the run directory: stat err=%v", err)
	}
}

// limitedErrReader returns n bytes of 'a', then a permanent read error -
// the "invalid XML après export" fault as a source that goes bad mid-
// stream (a dropped connection while copying a plan's XML, say), not a
// well-formedness question File() never checks.
type limitedErrReader struct {
	remaining int
}

func (r *limitedErrReader) Read(p []byte) (int, error) {
	if r.remaining <= 0 {
		return 0, errors.New("fixture: source went bad mid-export")
	}
	n := len(p)
	if n > r.remaining {
		n = r.remaining
	}
	for i := 0; i < n; i++ {
		p[i] = 'a'
	}
	r.remaining -= n
	return n, nil
}

// TestFileSourceReadFailureLeavesNoPartialFile proves File() never
// publishes a truncated export when its own source reader fails
// partway: the partial file is removed, never recorded as an artifact,
// and the caller gets a file/serialization error (code 6) - not the
// collection-limit code (7), which is reserved for a source that is
// simply too large, not one that is broken.
func TestFileSourceReadFailureLeavesNoPartialFile(t *testing.T) {
	c, err := New(t.TempDir(), "json", Limits{Rows: 1000, Bytes: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	src := &limitedErrReader{remaining: 64}
	_, fileErr := c.File("plan_xml", ".sqlplan", src)
	if fileErr == nil {
		t.Fatal("a source that fails mid-read must return a non-nil error")
	}
	if code := model.ExitCode(fileErr); code != 6 {
		t.Fatalf("a broken source must fail as exit code 6 (file/serialization), got %d: %v", code, fileErr)
	}
	if len(c.artifacts) != 0 {
		t.Fatalf("a read failure must leave no artifact record at all, got %+v", c.artifacts)
	}
	entries, err := os.ReadDir(c.Dir())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		t.Fatalf("a broken-source export must leave no file on disk, found %q", e.Name())
	}
}

// TestFileSecondSourceOverflowRetainsFirstCompleted is "dépassement
// pendant le deuxième fichier" exactly as the brief names it: a FIRST
// File() call fits and completes; a SECOND, on the same run, exceeds
// the remaining byte budget. Design spec line 111: "Retain previously
// completed artifacts as partial invocation results" - the first
// file's own entry must survive Finish, on disk and in the result,
// untouched by the second call's refusal.
func TestFileSecondSourceOverflowRetainsFirstCompleted(t *testing.T) {
	const budget = 100
	c, err := New(t.TempDir(), "json", Limits{Rows: 1000, Bytes: manifestReserveBytes + budget})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })

	first, err := c.File("module_definition", ".sql", bytes.NewReader(bytes.Repeat([]byte("a"), budget/2)))
	if err != nil {
		t.Fatalf("the first, well-within-budget file must succeed: %v", err)
	}
	if !first.Complete {
		t.Fatalf("first artifact must be complete: %+v", first)
	}

	_, secondErr := c.File("plan_xml", ".sqlplan", bytes.NewReader(bytes.Repeat([]byte("b"), budget)))
	if code := model.ExitCode(secondErr); code != 7 {
		t.Fatalf("the second, overflowing file must breach at exit code 7, got %d: %v", code, secondErr)
	}

	if _, statErr := os.Stat(first.Path); statErr != nil {
		t.Fatalf("the first, already-complete artifact must survive the second call's overflow: %v", statErr)
	}

	result, finishErr := c.Finish(model.ContextInfo{}, secondErr)
	if model.ExitCode(finishErr) != 7 {
		t.Fatalf("Finish must preserve the second call's collection-limit error, got %v", finishErr)
	}
	foundFirst, foundOmittedSecond := false, false
	for _, a := range result.Artifacts {
		if a.Path == first.Path && a.Complete {
			foundFirst = true
		}
		if a.Kind == "plan_xml" && !a.Complete && a.Reason == model.ReasonCollectionLimit {
			foundOmittedSecond = true
		}
	}
	if !foundFirst {
		t.Fatalf("result.Artifacts must still list the first, completed file: %+v", result.Artifacts)
	}
	if !foundOmittedSecond {
		t.Fatalf("result.Artifacts must carry the second file's omitted record: %+v", result.Artifacts)
	}

	// The manifest itself (written by Finish, a separate on-disk fact
	// from the in-memory result above) must tell the same story.
	data, readErr := os.ReadFile(result.ManifestPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Contains(data, []byte(filepath.Base(first.Path))) {
		t.Fatalf("manifest must still reference the first, completed artifact: %s", data)
	}
}

// TestFileOverflowComposesWithRenderJSON is fix 1's A6, point 1: the
// task-15 version of the second-file-overflow fault stopped at
// Collector.Finish - it proved the in-memory model.Result and the
// manifest on disk agree, but nothing ever fed that Result through
// output.Render, the other real production piece that actually builds
// the JSON this program's own stdout carries. No command in this
// codebase calls Sink.File twice in one run (grep confirms exactly
// one File call each in obj code, qs query and plan) - composing this
// proof through cli.run's own Parse/registry dispatch is not possible
// without a command shaped that way, so this composes the two REAL
// pieces that matter, Collector and Render, directly: the exact
// result TestFileSecondSourceOverflowRetainsFirstCompleted already
// built, now rendered for real and decoded back from actual JSON
// bytes, not inspected as a Go struct.
func TestFileOverflowComposesWithRenderJSON(t *testing.T) {
	const budget = 100
	c, err := New(t.TempDir(), "json", Limits{Rows: 1000, Bytes: manifestReserveBytes + budget})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	first, err := c.File("module_definition", ".sql", bytes.NewReader(bytes.Repeat([]byte("a"), budget/2)))
	if err != nil {
		t.Fatalf("the first, well-within-budget file must succeed: %v", err)
	}
	_, secondErr := c.File("plan_xml", ".sqlplan", bytes.NewReader(bytes.Repeat([]byte("b"), budget)))
	if model.ExitCode(secondErr) != 7 {
		t.Fatalf("the second file must breach at exit code 7, got %v", secondErr)
	}
	result, _ := c.Finish(model.ContextInfo{}, secondErr)

	out, renderErr := output.Render(result, output.PreviewOptions{Rows: 10, ByteLimit: 32768}, "json")
	if renderErr != nil {
		t.Fatalf("Render must still produce a response for a partial run, got: %v", renderErr)
	}
	var decoded struct {
		OK    bool `json:"ok"`
		Error struct {
			Code int `json:"code"`
		} `json:"error"`
		Artifacts []struct {
			Kind     string `json:"kind"`
			Path     string `json:"path"`
			Complete bool   `json:"complete"`
			Reason   string `json:"reason"`
		} `json:"artifacts"`
	}
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("Render's own output must be valid, complete JSON: %v\noutput: %s", err, out)
	}
	if decoded.OK {
		t.Fatal("decoded JSON: ok=true, want false (the run did not complete cleanly)")
	}
	if decoded.Error.Code != 7 {
		t.Fatalf("decoded JSON: error.code=%d, want 7", decoded.Error.Code)
	}
	var renderedFirst, renderedOmittedSecond bool
	for _, a := range decoded.Artifacts {
		if a.Complete && a.Path == first.Path {
			renderedFirst = true
		}
		if a.Kind == "plan_xml" && !a.Complete && a.Reason == model.ReasonCollectionLimit {
			renderedOmittedSecond = true
		}
	}
	if !renderedFirst {
		t.Fatalf("the rendered JSON must still list the first, completed artifact: %s", out)
	}
	if !renderedOmittedSecond {
		t.Fatalf("the rendered JSON must carry the second file's omitted record: %s", out)
	}
}

// Compile-time check that limitedErrReader really implements io.Reader,
// so a refactor of its signature fails to build rather than silently
// changing what this fault actually exercises.
var _ io.Reader = &limitedErrReader{}

// swapReader changes the path after File has opened its destination.
type swapReader struct {
	swap func()
	err  error
}

func (r *swapReader) Read(p []byte) (int, error) {
	if r.swap != nil {
		r.swap()
		r.swap = nil
	}
	p[0] = 'x'
	return 1, r.err
}

func TestFileCleanupAfterDirectorySwap(t *testing.T) {
	// Unix only, and the reason is the point: this test renames the
	// invocation directory out from under an in-flight artifact write, to
	// prove the store still operates inside the directory it opened rather
	// than inside whatever the old path now resolves to. Windows refuses
	// that rename outright while any handle on the directory is open, which
	// is a STRONGER guarantee than the one measured here and leaves nothing
	// to assert - measured on the Windows CI job, where the rename failed
	// with "the process cannot access the file because it is being used by
	// another process".
	if runtime.GOOS == "windows" {
		t.Skip("renaming a directory with an open handle is impossible on Windows, which protects this case by construction")
	}
	for _, tc := range []struct {
		name    string
		readErr error
		code    int
	}{
		{"read_error", errors.New("fixture read failure"), 6},
		{"quota", nil, 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := New(t.TempDir(), "json", Limits{Rows: 10, Bytes: manifestReserveBytes + 2})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { c.Close() })
			victim := t.TempDir()
			target := filepath.Join(victim, "plan_xml.sqlplan")
			if err := os.WriteFile(target, []byte("keep"), 0600); err != nil {
				t.Fatal(err)
			}
			moved := c.Dir() + "-moved"
			reader := &swapReader{err: tc.readErr, swap: func() {
				if err := os.Rename(c.Dir(), moved); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(victim, c.Dir()); err != nil {
					t.Skipf("directory symlink unavailable: %v", err)
				}
			}}
			_, err = c.File("plan_xml", ".sqlplan", reader)
			if model.ExitCode(err) != tc.code {
				t.Fatalf("code = %d, want %d", model.ExitCode(err), tc.code)
			}
			if b, err := os.ReadFile(target); err != nil || string(b) != "keep" {
				t.Errorf("third-party file changed: %v", err)
			}
			if _, err := os.Stat(filepath.Join(moved, "plan_xml.sqlplan")); !os.IsNotExist(err) {
				t.Errorf("partial file remains: %v", err)
			}
			// Finish releases the opened directory even when the manifest exceeds quota.
			_, _ = c.Finish(model.ContextInfo{}, nil)
		})
	}
}
