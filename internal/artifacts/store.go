package artifacts

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// manifestReserveBytes is the headroom store keeps free of every table
// and File() write for the whole run: the manifest is the last thing
// written, once the run's shape (how many tables, how many artifacts,
// how many notices) is fully known, so it must never have to contend
// for space with the collection it is about to describe.
const manifestReserveBytes = 64 * 1024

// Limits caps one run's collection. Rows is a single, shared total
// across every table a Collector opens (Begin/Row are not reset per
// table): ten tables at 1,000 rows each exhaust a 10,000-row Limits.Rows
// exactly the way one table of 10,000 rows would. Bytes is the total
// on-disk footprint of every artifact file this run writes, manifest
// included.
type Limits struct {
	Rows, Bytes int64
}

// storedFile tracks one artifact this run created: its final path, the
// exact number of bytes it holds, and whether it is complete. Only an
// incomplete file (a table closed early by a limit breach, or never
// closed at all) is ever eligible for removal to reclaim space for the
// manifest; a complete one is never touched once recorded.
type storedFile struct {
	path     string
	bytes    int64
	complete bool
}

// store manages the on-disk footprint of one run: a freshly created,
// uniquely named invocation directory, and a byte budget shared across
// every file it creates.
type store struct {
	dir    string
	root   *os.Root
	budget int64 // Limits.Bytes minus the manifest reservation
	used   int64
	files  []*storedFile
}

func newStore(base string, limits Limits) (*store, error) {
	if err := os.MkdirAll(base, 0o700); err != nil {
		return nil, fmt.Errorf("artifacts: creating output directory %q: %w", base, err)
	}
	runDir, err := uniqueRunDir(base)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(runDir)
	if err != nil {
		return nil, fmt.Errorf("artifacts: opening run directory: %w", err)
	}
	budget := limits.Bytes - manifestReserveBytes
	if budget < 0 {
		budget = 0
	}
	return &store{dir: runDir, root: root, budget: budget}, nil
}

// uniqueRunDir creates, and returns the path of, a new, never-before-
// used 0700 subdirectory of base - the "sous-répertoire d'invocation
// unique" the brief requires, so two concurrent runs against the same
// base directory never share, collide on, or race over a single run's
// files.
func uniqueRunDir(base string) (string, error) {
	for attempt := 0; attempt < 1000; attempt++ {
		name := fmt.Sprintf("run-%d-%d", time.Now().UnixNano(), attempt)
		path := filepath.Join(base, name)
		if err := os.Mkdir(path, 0o700); err == nil {
			return path, nil
		} else if !os.IsExist(err) {
			return "", fmt.Errorf("artifacts: creating run directory under %q: %w", base, err)
		}
	}
	return "", fmt.Errorf("artifacts: could not create a unique run directory under %q", base)
}

// sanitizeName turns an arbitrary table or artifact kind name into a
// single, safe path component: no directory separators (so a table
// named "../../etc/passwd" cannot escape the run directory), no
// characters outside a conservative allow-list.
func sanitizeName(name string) string {
	name = filepath.Base(name)
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	s := b.String()
	if s == "" || s == "." || s == ".." {
		s = "artifact"
	}
	return s
}

// create opens a new artifact file for writing under the run directory,
// named after name (sanitized). It always uses O_EXCL: if a file (or a
// symlink planted at that path) already exists there, create never
// follows or overwrites it - it instead tries a numbered alternative,
// so a pre-existing or malicious path entry can never redirect a write
// meant for a fresh artifact.
func (s *store) create(name string) (*os.File, string, error) {
	clean := sanitizeName(name)
	ext := filepath.Ext(clean)
	base := strings.TrimSuffix(clean, ext)
	for attempt := 0; attempt < 1000; attempt++ {
		candidate := clean
		if attempt > 0 {
			candidate = fmt.Sprintf("%s-%d%s", base, attempt+1, ext)
		}
		path := filepath.Join(s.dir, candidate)
		f, err := s.root.OpenFile(candidate, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			return f, path, nil
		}
		if !os.IsExist(err) {
			return nil, "", fmt.Errorf("artifacts: creating artifact file %q: %w", path, err)
		}
	}
	return nil, "", fmt.Errorf("artifacts: could not create a unique artifact file for %q", name)
}

// hasRoom reports whether extra more bytes can be written without the
// run's total (excluding the manifest's own reservation) exceeding its
// budget.
func (s *store) hasRoom(extra int64) bool {
	return s.used+extra <= s.budget
}

// record registers one finished artifact file's accounting (its path,
// size and completeness) for later reclaim bookkeeping. It never
// changes s.used: callers add bytes to s.used themselves, incrementally,
// as they are actually written, so s.used and the sum of recorded
// complete-or-not bytes always agree.
func (s *store) record(path string, bytes int64, complete bool) {
	s.files = append(s.files, &storedFile{path: path, bytes: bytes, complete: complete})
}

// reclaimIncomplete deletes every recorded artifact file that is not
// complete, frees its bytes from s.used, and returns the paths removed.
// It never touches a file recorded as complete.
func (s *store) reclaimIncomplete() []string {
	var removed []string
	kept := s.files[:0]
	for _, f := range s.files {
		if !f.complete {
			_ = s.remove(f.path)
			s.used -= f.bytes
			removed = append(removed, f.path)
			continue
		}
		kept = append(kept, f)
	}
	s.files = kept
	return removed
}

// remove uses only store-generated basenames relative to the opened directory.
func (s *store) remove(path string) error {
	return s.root.Remove(filepath.Base(path))
}
