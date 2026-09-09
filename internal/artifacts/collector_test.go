package artifacts

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rudi-bruchez/argosql/internal/model"
	"github.com/rudi-bruchez/argosql/internal/output"
)

// failingWriter always refuses a write with a fresh error. It never
// closes anything and never touches a real file - unlike closing an
// *os.File out from under an encoder (which, on the very next Close,
// fails to write its own flush AND fails a second time closing the
// file itself, so a missing check on either return value goes
// unnoticed - see the two TestDiskFailureDuring* tests below), this
// isolates a pure write failure on the artifact's own writer.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) {
	return 0, errors.New("artifacts test: injected write failure")
}

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

// TestBeginRefusesSecondTableWithoutHeaderRoom proves Begin's own guard
// (measuring even an empty table's size against the remaining budget
// before ever creating the real file) is not dead code: once a first
// table has consumed the entire collection budget, a second table -
// with no room even for its own empty header - must be refused, not
// silently opened. Without this guard, accounted usage can exceed
// Limits.Bytes outright: a correctness review measured it reaching
// double the promised budget for two same-shaped tables.
func TestBeginRefusesSecondTableWithoutHeaderRoom(t *testing.T) {
	emptyBytes, err := measureTotal("json", intSpec("a"), nil)
	if err != nil {
		t.Fatal(err)
	}

	c, err := New(t.TempDir(), "json", Limits{Rows: 10, Bytes: manifestReserveBytes + emptyBytes})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Begin(intSpec("a")); err != nil {
		t.Fatal(err)
	}
	if err := c.End(true, true); err != nil {
		t.Fatal(err)
	}

	usedBeforeSecond := c.store.used
	collectionBudget := c.limits.Bytes - manifestReserveBytes

	err = c.Begin(intSpec("b"))
	if model.ExitCode(err) != 7 {
		t.Fatalf("a second table with no room even for its own empty header must be refused with code 7, got %v", err)
	}
	if c.store.used != usedBeforeSecond {
		t.Fatalf("a refused Begin must not change accounted usage: before=%d after=%d", usedBeforeSecond, c.store.used)
	}
	if c.store.used > collectionBudget {
		t.Fatalf("accounted usage %d must never exceed the collection budget %d - a cap that can be doubled is not a cap", c.store.used, collectionBudget)
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

// TestRowByteAccountingMatchesDiskSize proves rowSeparatorBytes is not
// dead weight: the cumulative byte count Row predicts for a table
// (c.artifacts[0].Bytes, and store.used alongside it) must equal the
// artifact's actual size on disk once several rows have been written,
// in both formats. A drift of one byte per row after the first -
// exactly what a rowSeparatorBytes that always returned 0 would cause
// - is invisible on a single row and only shows up once there is more
// than one: this is why 5 rows, not 1, are written here.
func TestRowByteAccountingMatchesDiskSize(t *testing.T) {
	for _, format := range []string{"json", "tsv"} {
		t.Run(format, func(t *testing.T) {
			c, err := New(t.TempDir(), format, Limits{Rows: 1000, Bytes: 1 << 30})
			if err != nil {
				t.Fatal(err)
			}
			if err := c.Begin(intSpec("x")); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 5; i++ {
				if err := c.Row([]model.Cell{int64(i)}); err != nil {
					t.Fatalf("row %d: %v", i, err)
				}
			}
			if err := c.End(true, true); err != nil {
				t.Fatal(err)
			}

			data, err := os.ReadFile(c.artifacts[0].Path)
			if err != nil {
				t.Fatal(err)
			}
			actual := int64(len(data))
			if c.artifacts[0].Bytes != actual {
				t.Fatalf("%s: accounted artifact size %d does not match actual file size %d", format, c.artifacts[0].Bytes, actual)
			}
			if c.store.used != actual {
				t.Fatalf("%s: accounted store usage %d does not match actual file size %d", format, c.store.used, actual)
			}
		})
	}
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

// TestDiskFailureDuringFlush proves End reports a distinct file-error
// exit code (6) - not the collection-limit code (7) - when flushing the
// table's buffered content actually fails to write. The injection
// replaces the open table's encoder with one writing to a writer that
// always refuses, while the table's real file is left open and healthy
// throughout: only the flush fails, so this is the one case that
// exercises finalizeCurrent's closeErr check in isolation. Closing the
// real *os.File early instead (as an earlier, hollow version of this
// test did) makes both Close calls fail for confounded reasons and
// proves nothing about closeErr specifically - a correctness review
// measured that removing the closeErr check entirely left that version
// green.
func TestDiskFailureDuringFlush(t *testing.T) {
	c, err := New(t.TempDir(), "json", Limits{Rows: 1000, Bytes: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	spec := intSpec("x")
	if err := c.Begin(spec); err != nil {
		t.Fatal(err)
	}
	if err := c.Row([]model.Cell{int64(0)}); err != nil {
		t.Fatal(err)
	}

	// Fault injection: swap the table's encoder for one over a writer
	// that always fails, so Close's flush hits a genuine write error.
	// The real file (c.current.file) is untouched and still perfectly
	// closeable.
	faulty, err := output.NewTableEncoder(failingWriter{}, "json", spec)
	if err != nil {
		t.Fatal(err)
	}
	c.current.real = faulty

	err = c.End(true, true)
	if code := model.ExitCode(err); code != 6 {
		t.Fatalf("a flush failure while closing the artifact's encoder must be exit code 6, got %d (%v)", code, err)
	}
}

// TestDiskFailureDuringFileClose proves End reports the same exit code
// (6) when the table's own file fails to close, even though the
// encoder's own flush succeeded cleanly moments before. The injection
// replaces the open table's encoder with one over an in-memory buffer
// (so its Close always succeeds, isolating the failure to the file),
// then closes the real file early so the file.Close inside End is a
// genuine second close on an already-closed descriptor.
func TestDiskFailureDuringFileClose(t *testing.T) {
	c, err := New(t.TempDir(), "json", Limits{Rows: 1000, Bytes: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	spec := intSpec("x")
	if err := c.Begin(spec); err != nil {
		t.Fatal(err)
	}
	if err := c.Row([]model.Cell{int64(0)}); err != nil {
		t.Fatal(err)
	}

	harmless, err := output.NewTableEncoder(&bytes.Buffer{}, "json", spec)
	if err != nil {
		t.Fatal(err)
	}
	c.current.real = harmless
	if err := c.current.file.Close(); err != nil {
		t.Fatal(err)
	}

	err = c.End(true, true)
	if code := model.ExitCode(err); code != 6 {
		t.Fatalf("a file-close failure must be exit code 6, got %d (%v)", code, err)
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
// exactly one model.Notice carrying model.KindEncodingNormalized.
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
		if n.Kind == model.KindEncodingNormalized {
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
		if n.Kind == model.KindEncodingNormalized {
			t.Fatalf("expected zero encoding_normalized notices for clean input, got %+v", n)
		}
	}
}
