package artifacts

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/rudi-bruchez/argosql/internal/model"
)

// maxManifestNotices bounds how many notices the manifest ever embeds.
// A manifest is a document the user can hand to someone else; an
// unbounded warning list (one per row that needed it, say) would make
// that handoff unbounded too. noticeCount always records the true
// total, so truncation is visible rather than silent.
const maxManifestNotices = 50

// manifestTable is a table's manifest entry: its spec and its
// collection completeness only. It deliberately has no field for rows
// or for model.PreviewState: the collector writes the manifest before
// any preview has been computed, and a manifest carrying
// PreviewState's zero value (rows_shown: 0, preview_complete: false)
// would falsely assert that no row was ever shown.
type manifestTable struct {
	Spec  model.TableSpec    `json:"spec"`
	State model.Completeness `json:"state"`
}

// manifestNotice is a notice as the manifest carries it: the same three
// fields model.Notice has. It is its own type, not model.Notice reused
// directly, so that capping the manifest's own list (see
// maxManifestNotices) is visibly a manifest concern, not a change to
// what model.Notice means elsewhere.
type manifestNotice struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
	Table   string `json:"table"`
}

// manifestDoc is the document Collector writes to manifest.json. It
// never carries a collected SQL value (no Cell ever appears here - only
// Column names and SQLType labels, never row data) and never carries an
// unbounded list (Notices is capped; NoticeCount records the true
// total) and never carries model.PreviewState.
type manifestDoc struct {
	SchemaVersion int                `json:"schema_version"`
	OK            bool               `json:"ok"`
	Context       model.ContextInfo  `json:"context"`
	Tables        []manifestTable    `json:"tables"`
	Artifacts     []model.Artifact   `json:"artifacts"`
	Notices       []manifestNotice   `json:"notices"`
	NoticeCount   int                `json:"notice_count"`
	Error         *model.PublicError `json:"error"`
}

func buildManifestDoc(result model.Result) manifestDoc {
	tables := make([]manifestTable, len(result.Tables))
	for i, t := range result.Tables {
		tables[i] = manifestTable{Spec: t.Spec, State: t.State}
	}

	capped := result.Notices
	if len(capped) > maxManifestNotices {
		capped = capped[:maxManifestNotices]
	}
	notices := make([]manifestNotice, len(capped))
	for i, n := range capped {
		notices[i] = manifestNotice{Kind: n.Kind, Message: n.Message, Table: n.Table}
	}

	return manifestDoc{
		SchemaVersion: result.SchemaVersion,
		OK:            result.OK,
		Context:       result.Context,
		Tables:        tables,
		Artifacts:     result.Artifacts,
		Notices:       notices,
		NoticeCount:   len(result.Notices),
		Error:         result.Error,
	}
}

// filterArtifacts returns artifacts with every entry whose Path is in
// removed dropped - used after reclaimIncomplete has actually deleted
// those files from disk, so the manifest never points at a path that no
// longer exists.
func filterArtifacts(artifacts []model.Artifact, removed []string) []model.Artifact {
	if len(removed) == 0 {
		return artifacts
	}
	drop := make(map[string]bool, len(removed))
	for _, p := range removed {
		drop[p] = true
	}
	kept := make([]model.Artifact, 0, len(artifacts))
	for _, a := range artifacts {
		if !drop[a.Path] {
			kept = append(kept, a)
		}
	}
	return kept
}

// writeManifest marshals result into the manifest document and writes
// it as manifest.json under the run directory, 0600, via O_EXCL.
//
// manifestReserveBytes of headroom was kept free of every table and
// File() write for exactly this moment, but that reservation is a
// working assumption, not a guarantee: if the manifest itself turns out
// to need more than Limits.Bytes leaves free, writeManifest first
// reclaims space by deleting only the incomplete artifacts this run
// created (never a complete one - see store.reclaimIncomplete) and
// removes them from result.Artifacts so the manifest does not reference
// a file that no longer exists. If the manifest still does not fit
// after that, writeManifest fails with a code-6 PublicError rather than
// writing a manifest and calling the run complete.
func (c *Collector) writeManifest(result *model.Result) (string, error) {
	marshal := func() ([]byte, error) {
		return json.Marshal(buildManifestDoc(*result))
	}

	body, err := marshal()
	if err != nil {
		return "", artifactError("marshaling manifest", err)
	}

	if c.store.used+int64(len(body)) > c.limits.Bytes {
		removed := c.store.reclaimIncomplete()
		if len(removed) > 0 {
			result.Artifacts = filterArtifacts(result.Artifacts, removed)
			body, err = marshal()
			if err != nil {
				return "", artifactError("marshaling manifest", err)
			}
		}
		if c.store.used+int64(len(body)) > c.limits.Bytes {
			return "", artifactError(fmt.Sprintf(
				"manifest needs %d bytes but only %d remain of the %d-byte collection budget",
				len(body), c.limits.Bytes-c.store.used, c.limits.Bytes), nil)
		}
	}

	path := filepath.Join(c.store.dir, "manifest.json")
	f, err := c.store.root.OpenFile("manifest.json", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", artifactError("creating manifest file", err)
	}
	defer f.Close()
	if _, err := f.Write(body); err != nil {
		return "", artifactError("writing manifest file", err)
	}

	c.store.used += int64(len(body))
	c.store.record(path, int64(len(body)), true)
	return path, nil
}
