package artifacts

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/rudi-bruchez/argosql/internal/model"
)

// TestManifestNeverCarriesPreviewState is the brief's own mandated
// assertion, verbatim: it reads the manifest this package actually
// produced and fails if it contains any of PreviewState's JSON keys.
// At the point the collector writes, no preview has been computed yet;
// serializing PreviewState's zero value would falsely assert that no
// row was ever shown.
func TestManifestNeverCarriesPreviewState(t *testing.T) {
	c, err := New(t.TempDir(), "json", Limits{Rows: 1000, Bytes: 1 << 20})
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
	result, err := c.Finish(model.ContextInfo{}, nil)
	if err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(result.ManifestPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"rows_shown"`, `"preview_complete"`, `"omitted_reasons"`} {
		if strings.Contains(string(data), key) {
			t.Fatalf("manifest must never carry PreviewState, found key %s in %s", key, data)
		}
	}
}

// TestManifestExcludesSQLValues proves the manifest never embeds a
// collected SQL value: only TableSpec (names and SQL type labels) and
// Completeness (counts and booleans) ever describe a table, never a
// row.
func TestManifestExcludesSQLValues(t *testing.T) {
	c, err := New(t.TempDir(), "json", Limits{Rows: 1000, Bytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	spec := model.TableSpec{Name: "x", Columns: []model.Column{{Name: "s", SQLType: "varchar"}}}
	if err := c.Begin(spec); err != nil {
		t.Fatal(err)
	}
	secret := "super-secret-production-value-12345"
	if err := c.Row([]model.Cell{secret}); err != nil {
		t.Fatal(err)
	}
	if err := c.End(true, true); err != nil {
		t.Fatal(err)
	}
	result, err := c.Finish(model.ContextInfo{}, nil)
	if err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(result.ManifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), secret) {
		t.Fatalf("manifest must never embed a collected SQL value, found it in %s", data)
	}
	if strings.Contains(string(data), `"rows"`) {
		t.Fatalf("manifest must never carry a rows field at all, found it in %s", data)
	}
}

// TestManifestUTF8NoBOM proves manifest.json (and, incidentally, the
// table artifact it describes) starts with no UTF-8 byte-order mark:
// this project's exports are UTF-8 without a BOM throughout.
func TestManifestUTF8NoBOM(t *testing.T) {
	c, err := New(t.TempDir(), "json", Limits{Rows: 1000, Bytes: 1 << 20})
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
	result, err := c.Finish(model.ContextInfo{}, nil)
	if err != nil {
		t.Fatal(err)
	}

	bom := []byte{0xEF, 0xBB, 0xBF}
	manifestData, err := os.ReadFile(result.ManifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.HasPrefix(manifestData, bom) {
		t.Fatal("manifest.json must not start with a UTF-8 BOM")
	}
	tableData, err := os.ReadFile(result.Artifacts[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.HasPrefix(tableData, bom) {
		t.Fatal("table artifact must not start with a UTF-8 BOM")
	}
}

// TestManifestReservationReclaimsIncompleteKeepsComplete is the brief's
// required proof for the manifest reservation: when the manifest
// itself needs more than the 64 KiB reserved for it, the collector
// reclaims space only from incomplete artifacts this run created - a
// table an earlier diagnostic explicitly marked incomplete, here - and
// a complete artifact survives untouched, on disk and in the manifest,
// throughout.
func TestManifestReservationReclaimsIncompleteKeepsComplete(t *testing.T) {
	dir := t.TempDir()
	// Collection budget generous enough that nothing is limited while
	// writing; only the manifest's own size, forced huge below, will
	// ever be tight.
	c, err := New(dir, "json", Limits{Rows: 1000, Bytes: 100 * 1024})
	if err != nil {
		t.Fatal(err)
	}

	// Table "complete": finished in full, must survive no matter what.
	if err := c.Begin(intSpec("complete")); err != nil {
		t.Fatal(err)
	}
	if err := c.Row([]model.Cell{int64(1)}); err != nil {
		t.Fatal(err)
	}
	if err := c.End(true, true); err != nil {
		t.Fatal(err)
	}
	completePath := c.artifacts[0].Path

	// Table "partial": a diagnostic reporting it stopped early (a
	// timeout elsewhere, not a collection-limit breach) - incomplete,
	// and large, so reclaiming it actually frees meaningful space.
	if err := c.Begin(intSpec("partial")); err != nil {
		t.Fatal(err)
	}
	largeValue := strings.Repeat("x", 35*1024)
	if err := c.Row([]model.Cell{largeValue}); err != nil {
		t.Fatal(err)
	}
	if err := c.End(false, true); err != nil {
		t.Fatal(err)
	}
	partialPath := c.artifacts[1].Path
	if c.artifacts[1].Complete {
		t.Fatal("fixture error: the partial table must be incomplete")
	}

	// Force the manifest itself to exceed the 64 KiB reservation, via a
	// context field with no cap of its own (unlike the notices list).
	hugeServer := strings.Repeat("s", 80*1024)

	result, err := c.Finish(model.ContextInfo{Server: hugeServer}, nil)
	if err != nil {
		t.Fatalf("Finish must still succeed once reclaiming the partial table frees enough room: %v", err)
	}
	if result.ManifestPath == "" {
		t.Fatal("expected a manifest path")
	}

	if _, err := os.Stat(completePath); err != nil {
		t.Fatalf("the complete artifact must survive reservation reclaim untouched: %v", err)
	}
	if _, err := os.Stat(partialPath); !os.IsNotExist(err) {
		t.Fatalf("the incomplete artifact must have been removed to free space, stat err: %v", err)
	}

	foundComplete, foundPartial := false, false
	for _, a := range result.Artifacts {
		if a.Path == completePath {
			foundComplete = true
		}
		if a.Path == partialPath {
			foundPartial = true
		}
	}
	if !foundComplete {
		t.Fatal("the complete artifact must still be listed in the result")
	}
	if foundPartial {
		t.Fatal("the removed, incomplete artifact must no longer be listed in the result")
	}
}

// TestManifestReservationFailsCode6WithoutClaimingComplete is the
// converse: when the manifest still does not fit even after reclaiming
// every incomplete artifact, Finish must fail with the file/
// serialization exit code (6) rather than silently succeeding with a
// manifest it could not actually write - and a complete artifact must
// still never be touched.
func TestManifestReservationFailsCode6WithoutClaimingComplete(t *testing.T) {
	dir := t.TempDir()
	c, err := New(dir, "json", Limits{Rows: 1000, Bytes: 100 * 1024})
	if err != nil {
		t.Fatal(err)
	}

	if err := c.Begin(intSpec("complete")); err != nil {
		t.Fatal(err)
	}
	if err := c.Row([]model.Cell{int64(1)}); err != nil {
		t.Fatal(err)
	}
	if err := c.End(true, true); err != nil {
		t.Fatal(err)
	}
	completePath := c.artifacts[0].Path

	// A manifest this large cannot fit within the 100 KiB budget no
	// matter what is reclaimed: there is nothing incomplete to reclaim
	// at all here, and the context field alone already dwarfs the
	// budget.
	hugeServer := strings.Repeat("s", 400*1024)

	result, err := c.Finish(model.ContextInfo{Server: hugeServer}, nil)
	if code := model.ExitCode(err); code != 6 {
		t.Fatalf("an unfittable manifest must fail as exit code 6, got %d (%v)", code, err)
	}
	if result.ManifestPath != "" {
		t.Fatal("Finish must not claim a manifest path when it could not write one")
	}
	if _, statErr := os.Stat(completePath); statErr != nil {
		t.Fatalf("the complete artifact must still be untouched: %v", statErr)
	}
	if _, statErr := os.Stat(dir); statErr != nil {
		t.Fatal(statErr)
	}
	entries, err := os.ReadDir(c.Dir())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() == "manifest.json" {
			t.Fatal("no manifest.json must exist when Finish reports it could not write one")
		}
	}
}
