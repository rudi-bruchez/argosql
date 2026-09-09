package artifacts

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/rudi-bruchez/argosql/internal/model"
)

// TestPermissionsUnix proves the run directory and every artifact file
// this package creates are private to the invoking user: 0700
// directories, 0600 files. Windows has no equivalent POSIX permission
// model; that is covered separately (task 16).
func TestPermissionsUnix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits only; Windows is covered at task 16")
	}

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
	if err := c.End(true, true); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Finish(model.ContextInfo{}, nil); err != nil {
		t.Fatal(err)
	}

	dirInfo, err := os.Stat(c.Dir())
	if err != nil {
		t.Fatal(err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Fatalf("run directory perm = %o, want 0700", perm)
	}

	entries, err := os.ReadDir(c.Dir())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("expected at least one artifact file")
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("file %q perm = %o, want 0600", e.Name(), perm)
		}
	}
}

// TestStoreCreateUniqueRunDir proves two Collectors opened against the
// same base directory never share a run directory.
func TestStoreCreateUniqueRunDir(t *testing.T) {
	base := t.TempDir()
	c1, err := New(base, "json", Limits{Rows: 10, Bytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	c2, err := New(base, "json", Limits{Rows: 10, Bytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if c1.Dir() == c2.Dir() {
		t.Fatalf("two Collectors against the same base directory must not share a run directory: %q", c1.Dir())
	}
	if filepath.Dir(c1.Dir()) != base || filepath.Dir(c2.Dir()) != base {
		t.Fatalf("run directories must live directly under base %q, got %q and %q", base, c1.Dir(), c2.Dir())
	}
}
