package artifacts

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rudi-bruchez/argosql/internal/model"
)

func intSpec(name string) model.TableSpec {
	return model.TableSpec{Name: name, Columns: []model.Column{{Name: "n", SQLType: "int"}}}
}

// TestRowLimit is verbatim from the task-6 brief: at exactly the row
// limit, the accepted rows succeed; the row that would cross it is
// refused with a PublicError whose Code is the collection-limit exit
// code (7), not merely a non-nil error (a plain EOF is non-nil too).
func TestRowLimit(t *testing.T) {
	c, err := New(t.TempDir(), "json", Limits{Rows: 2, Bytes: 1048576})
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Begin(model.TableSpec{Name: "x", Columns: []model.Column{{Name: "n", SQLType: "int"}}}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err = c.Row([]model.Cell{int64(i)}); err != nil {
			t.Fatal(err)
		}
	}
	err = c.Row([]model.Cell{int64(2)})
	if model.ExitCode(err) != 7 {
		t.Fatalf("limit error: %v", err)
	}
}

// TestExactBoundaryComplete proves the frontier the brief calls out by
// name: writing exactly Limits.Rows rows and then calling
// End(true, true) - exactly what a diagnostic does when a TOP N query
// legitimately returns N rows and no more - must leave the table
// CollectionComplete, never incomplete. Only a row that Row itself
// refuses (the N+1th) may ever mark a table incomplete on row-count
// grounds.
func TestExactBoundaryComplete(t *testing.T) {
	c, err := New(t.TempDir(), "json", Limits{Rows: 3, Bytes: 1048576})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Begin(intSpec("x")); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := c.Row([]model.Cell{int64(i)}); err != nil {
			t.Fatalf("row %d: %v", i, err)
		}
	}
	if err := c.End(true, true); err != nil {
		t.Fatalf("End: %v", err)
	}

	result, err := c.Finish(model.ContextInfo{CollectedAt: time.Now()}, nil)
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if len(result.Tables) != 1 {
		t.Fatalf("expected 1 table, got %d", len(result.Tables))
	}
	state := result.Tables[0].State
	if !state.CollectionComplete {
		t.Fatalf("exactly Limits.Rows rows plus End(true,...) must stay complete, got %+v", state)
	}
	if state.RowsCollected != 3 {
		t.Fatalf("expected 3 rows collected, got %d", state.RowsCollected)
	}
	if !result.Artifacts[0].Complete {
		t.Fatalf("artifact must be marked complete, got %+v", result.Artifacts[0])
	}
}

// TestMultiTableRowLimit proves Limits.Rows is one shared total across
// every table a Collector opens, not reset per table: a second table
// can be refused a row even though it itself has written far fewer rows
// than the limit, because an earlier table already spent the budget.
func TestMultiTableRowLimit(t *testing.T) {
	c, err := New(t.TempDir(), "json", Limits{Rows: 3, Bytes: 1048576})
	if err != nil {
		t.Fatal(err)
	}

	if err := c.Begin(intSpec("a")); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := c.Row([]model.Cell{int64(i)}); err != nil {
			t.Fatalf("table a row %d: %v", i, err)
		}
	}
	if err := c.End(true, true); err != nil {
		t.Fatalf("End a: %v", err)
	}

	if err := c.Begin(intSpec("b")); err != nil {
		t.Fatal(err)
	}
	if err := c.Row([]model.Cell{int64(0)}); err != nil {
		t.Fatalf("table b row 0 (3rd row overall, still within the limit of 3): %v", err)
	}
	err = c.Row([]model.Cell{int64(1)})
	if model.ExitCode(err) != 7 {
		t.Fatalf("table b row 1 (4th row overall) must breach the shared limit: %v", err)
	}
}

// TestBytesLimitBeforeAtAfter proves the byte cap's own boundary: a row
// that fits comfortably under the remaining budget succeeds, a row that
// lands exactly at the budget succeeds, and a row that would need one
// byte more than the budget is refused with the collection-limit error
// - without ever leaving a partial row in the file (Close still
// produces valid JSON of whatever was accepted).
func TestBytesLimitBeforeAtAfter(t *testing.T) {
	// Measure the exact on-disk size of one table holding one row,
	// under a budget generous enough that nothing here is limited.
	measure, err := New(t.TempDir(), "json", Limits{Rows: 1000, Bytes: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	if err := measure.Begin(intSpec("x")); err != nil {
		t.Fatal(err)
	}
	if err := measure.Row([]model.Cell{int64(0)}); err != nil {
		t.Fatal(err)
	}
	if err := measure.End(true, true); err != nil {
		t.Fatal(err)
	}
	oneRowBytes := measure.artifacts[0].Bytes

	t.Run("before cap", func(t *testing.T) {
		c, err := New(t.TempDir(), "json", Limits{Rows: 1000, Bytes: manifestReserveBytes + oneRowBytes + 100})
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Begin(intSpec("x")); err != nil {
			t.Fatal(err)
		}
		if err := c.Row([]model.Cell{int64(0)}); err != nil {
			t.Fatalf("a row comfortably under budget must be accepted: %v", err)
		}
	})

	t.Run("exactly at cap", func(t *testing.T) {
		c, err := New(t.TempDir(), "json", Limits{Rows: 1000, Bytes: manifestReserveBytes + oneRowBytes})
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Begin(intSpec("x")); err != nil {
			t.Fatal(err)
		}
		if err := c.Row([]model.Cell{int64(0)}); err != nil {
			t.Fatalf("a row landing exactly on the budget must be accepted: %v", err)
		}
	})

	t.Run("one byte after cap", func(t *testing.T) {
		c, err := New(t.TempDir(), "json", Limits{Rows: 1000, Bytes: manifestReserveBytes + oneRowBytes - 1})
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Begin(intSpec("x")); err != nil {
			t.Fatal(err)
		}
		err = c.Row([]model.Cell{int64(0)})
		if model.ExitCode(err) != 7 {
			t.Fatalf("a row needing one more byte than the budget must breach: %v", err)
		}
		// The table must still close cleanly with valid, empty-of-rows
		// JSON - not a truncated file holding a partial row.
		data, readErr := os.ReadFile(c.artifacts[0].Path)
		if readErr != nil {
			t.Fatalf("reading partial artifact: %v", readErr)
		}
		if !bytes.Equal(data, []byte(`{"columns":[{"name":"n","sql_type":"int"}],"rows":[]}`)) {
			t.Fatalf("expected a cleanly closed, rowless JSON object, got %q", data)
		}
		if c.artifacts[0].Complete {
			t.Fatalf("a table closed by a byte-budget breach must be marked incomplete")
		}
	})
}

// TestFileLimitSingleSource proves File()'s overflow sentinel on an
// indivisible source object: a source exactly as large as the
// remaining budget is kept in full, and a source one byte larger is
// refused with the collection-limit error and leaves no truncated file
// behind at all - unlike a table, there is no "accepted so far" to keep.
func TestFileLimitSingleSource(t *testing.T) {
	const budget = 100

	t.Run("exactly at cap", func(t *testing.T) {
		c, err := New(t.TempDir(), "json", Limits{Rows: 1000, Bytes: manifestReserveBytes + budget})
		if err != nil {
			t.Fatal(err)
		}
		src := bytes.NewReader(bytes.Repeat([]byte("a"), budget))
		artifact, err := c.File("plan", ".xml", src)
		if err != nil {
			t.Fatalf("a source exactly at the remaining budget must be accepted: %v", err)
		}
		if artifact.Bytes != budget || !artifact.Complete {
			t.Fatalf("unexpected artifact: %+v", artifact)
		}
		data, readErr := os.ReadFile(artifact.Path)
		if readErr != nil || len(data) != budget {
			t.Fatalf("artifact file: data=%q err=%v", data, readErr)
		}
	})

	t.Run("one byte after cap", func(t *testing.T) {
		dir := t.TempDir()
		c, err := New(dir, "json", Limits{Rows: 1000, Bytes: manifestReserveBytes + budget})
		if err != nil {
			t.Fatal(err)
		}
		src := bytes.NewReader(bytes.Repeat([]byte("a"), budget+1))
		_, err = c.File("plan", ".xml", src)
		if model.ExitCode(err) != 7 {
			t.Fatalf("a source one byte over the remaining budget must breach: %v", err)
		}
		// No truncated file left behind anywhere under the run directory.
		entries, _ := os.ReadDir(c.Dir())
		for _, e := range entries {
			if strings.Contains(e.Name(), "plan") {
				t.Fatalf("overflowing File() must leave no artifact file, found %q", e.Name())
			}
		}
	})
}

// TestFileCollisionAvoidsSymlink proves store.create never follows or
// overwrites a pre-existing path entry - including a symlink planted
// there by something else - it instead picks a numbered alternative, so
// a collision or a malicious symlink can never redirect an artifact
// write.
func TestFileCollisionAvoidsSymlink(t *testing.T) {
	c, err := New(t.TempDir(), "json", Limits{Rows: 1000, Bytes: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}

	// Plant a symlink at the exact path Begin would otherwise use for
	// table "x", pointing at a file outside the run directory entirely.
	outsideDir := t.TempDir()
	outsideTarget := filepath.Join(outsideDir, "victim")
	if err := os.WriteFile(outsideTarget, []byte("do not touch"), 0o600); err != nil {
		t.Fatal(err)
	}
	collidingPath := filepath.Join(c.Dir(), "x.json")
	if err := os.Symlink(outsideTarget, collidingPath); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if err := c.Begin(intSpec("x")); err != nil {
		t.Fatalf("Begin must route around the collision, not fail: %v", err)
	}
	if err := c.Row([]model.Cell{int64(0)}); err != nil {
		t.Fatal(err)
	}
	if err := c.End(true, true); err != nil {
		t.Fatal(err)
	}

	if c.artifacts[0].Path == collidingPath {
		t.Fatalf("artifact must not have been written through the symlink's path")
	}
	victim, err := os.ReadFile(outsideTarget)
	if err != nil || string(victim) != "do not touch" {
		t.Fatalf("symlink target must be untouched, got %q err=%v", victim, err)
	}
	link, err := os.Readlink(collidingPath)
	if err != nil || link != outsideTarget {
		t.Fatalf("the planted symlink itself must be untouched, got %q err=%v", link, err)
	}
}

// TestDiskFailureDuringClose injects a real write failure - not a
// collection-limit breach - by closing the table's underlying file out
// from under the still-open encoder before End flushes it. End must
// report a distinct, file-error exit code (6), not the collection-limit
// code (7): the two causes must never be confused.
func TestDiskFailureDuringClose(t *testing.T) {
	c, err := New(t.TempDir(), "json", Limits{Rows: 1000, Bytes: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Begin(intSpec("x")); err != nil {
		t.Fatal(err)
	}
	if err := c.Row([]model.Cell{int64(0)}); err != nil {
		t.Fatal(err)
	}
	// Fault injection: close the real file's descriptor directly,
	// out-of-band from the Collector's own lifecycle, so the buffered
	// JSON encoder's flush-on-Close fails with a genuine write error.
	if err := c.current.file.Close(); err != nil {
		t.Fatal(err)
	}

	err = c.End(true, true)
	if err == nil {
		t.Fatal("expected a write failure, got nil")
	}
	if code := model.ExitCode(err); code != 6 {
		t.Fatalf("a file error closing the artifact must be exit code 6, got %d (%v)", code, err)
	}
}

// TestFinishClosesAbandonedTable proves Finish does not lose a table a
// diagnostic began but never called End on (the process hit an error
// elsewhere mid-table): Finish closes it out itself, as incomplete, so
// it is still accounted for rather than silently dropped.
func TestFinishClosesAbandonedTable(t *testing.T) {
	c, err := New(t.TempDir(), "json", Limits{Rows: 1000, Bytes: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Begin(intSpec("x")); err != nil {
		t.Fatal(err)
	}
	if err := c.Row([]model.Cell{int64(0)}); err != nil {
		t.Fatal(err)
	}
	// No End call: simulate the diagnostic dying mid-table.

	runErr := &model.PublicError{Code: 5, Kind: "execution", Message: "diagnostic failed"}
	result, err := c.Finish(model.ContextInfo{}, runErr)
	if model.ExitCode(err) != 5 {
		t.Fatalf("Finish must preserve the original collection error, got %v", err)
	}
	if len(result.Tables) != 1 {
		t.Fatalf("the abandoned table must still be accounted for, got %d tables", len(result.Tables))
	}
	if result.Tables[0].State.CollectionComplete {
		t.Fatalf("an abandoned table must be marked incomplete")
	}
	if len(result.Artifacts) != 1 || result.Artifacts[0].Complete {
		t.Fatalf("the abandoned table's artifact must be recorded, incomplete: %+v", result.Artifacts)
	}
	data, readErr := os.ReadFile(result.Artifacts[0].Path)
	if readErr != nil {
		t.Fatalf("abandoned table's file must still be a valid, closed artifact: %v", readErr)
	}
	if !bytes.HasPrefix(data, []byte("{")) || !bytes.HasSuffix(data, []byte("}")) {
		t.Fatalf("expected cleanly closed JSON, got %q", data)
	}
}

// TestEncodingNormalizedNoticeEmitted proves the collector, not the
// encoder, is the one that speaks: when the encoder detects it silently
// substituted invalid UTF-8 bytes with U+FFFD, the collector emits
// exactly one model.Notice carrying model.ReasonEncodingNormalized.
func TestEncodingNormalizedNoticeEmitted(t *testing.T) {
	c, err := New(t.TempDir(), "json", Limits{Rows: 1000, Bytes: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	spec := model.TableSpec{Name: "x", Columns: []model.Column{{Name: "s", SQLType: "varchar"}}}
	if err := c.Begin(spec); err != nil {
		t.Fatal(err)
	}
	invalid := string([]byte{'a', 0xff, 'b'}) // not valid UTF-8
	if err := c.Row([]model.Cell{invalid}); err != nil {
		t.Fatal(err)
	}
	if err := c.End(true, true); err != nil {
		t.Fatal(err)
	}

	var matches []model.Notice
	for _, n := range c.notices {
		if n.Kind == model.ReasonEncodingNormalized {
			matches = append(matches, n)
		}
	}
	if len(matches) != 1 {
		t.Fatalf("expected exactly 1 encoding_normalized notice, got %d: %+v", len(matches), c.notices)
	}
	if matches[0].Table != "x" {
		t.Fatalf("expected the notice to name table x, got %+v", matches[0])
	}
}

// TestEncodingNormalizedNoticeAbsentForCleanInput proves the converse:
// a value with no invalid byte sequence produces zero
// encoding_normalized notices.
func TestEncodingNormalizedNoticeAbsentForCleanInput(t *testing.T) {
	c, err := New(t.TempDir(), "json", Limits{Rows: 1000, Bytes: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	spec := model.TableSpec{Name: "x", Columns: []model.Column{{Name: "s", SQLType: "varchar"}}}
	if err := c.Begin(spec); err != nil {
		t.Fatal(err)
	}
	if err := c.Row([]model.Cell{"clean value"}); err != nil {
		t.Fatal(err)
	}
	if err := c.End(true, true); err != nil {
		t.Fatal(err)
	}

	for _, n := range c.notices {
		if n.Kind == model.ReasonEncodingNormalized {
			t.Fatalf("expected zero encoding_normalized notices for clean input, got %+v", n)
		}
	}
}
