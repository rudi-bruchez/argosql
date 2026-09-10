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
	t.Cleanup(func() { c.Close() })
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
	t.Cleanup(func() { c1.Close() })
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

func TestStoreOperationsAfterDirectorySwap(t *testing.T) {
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
	c, err := New(t.TempDir(), "json", Limits{Rows: 10, Bytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	f, path, err := c.store.create("partial.sql")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	c.store.record(path, 0, false)
	victim := t.TempDir()
	target := filepath.Join(victim, "partial.sql")
	if err := os.WriteFile(target, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	moved := c.Dir() + "-moved"
	if err := os.Rename(c.Dir(), moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, c.Dir()); err != nil {
		t.Skipf("directory symlink unavailable: %v", err)
	}
	c.store.reclaimIncomplete()
	if b, err := os.ReadFile(target); err != nil || string(b) != "keep" {
		t.Errorf("reclaim changed third-party file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(moved, "partial.sql")); !os.IsNotExist(err) {
		t.Errorf("incomplete artifact remains: %v", err)
	}
	f, _, err = c.store.create("next.sql")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if _, err := c.Finish(model.ContextInfo{}, nil); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"next.sql", "manifest.json"} {
		if _, err := os.Stat(filepath.Join(moved, name)); err != nil {
			t.Errorf("%s not created in opened directory: %v", name, err)
		}
		if _, err := os.Stat(filepath.Join(victim, name)); !os.IsNotExist(err) {
			t.Errorf("%s created in third-party directory: %v", name, err)
		}
	}
}
