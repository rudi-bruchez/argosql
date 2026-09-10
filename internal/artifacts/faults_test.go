package artifacts

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
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

// Compile-time check that limitedErrReader really implements io.Reader,
// so a refactor of its signature fails to build rather than silently
// changing what this fault actually exercises.
var _ io.Reader = &limitedErrReader{}
