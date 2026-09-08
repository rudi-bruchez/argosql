package model

import (
	"encoding/json"
	"io"
	"time"
)

// Cell holds one output value: nil, string, bool, int64, float64, or the
// exact SQL text as string when no narrower Go type applies.
type Cell struct{ Value any }

// MarshalJSON encodes the naked value, never an object. Without this method
// Cell{Value:"abc"} serializes as {"Value":"abc"}, which is neither
// positional nor snake_case: a row must be an array of naked values.
// Measured; this belongs to task 1, not task 5.
func (c Cell) MarshalJSON() ([]byte, error) {
	return json.Marshal(c.Value)
}

// Closed vocabulary of reduction reasons. Any other value is a defect.
const (
	ReasonPreviewOmitted     = "preview_omitted"     // cell too large for the budget
	ReasonRowsTruncated      = "rows_truncated"      // rows dropped to fit
	ReasonCellTruncated      = "cell_truncated"      // truncation by rune count
	ReasonEncodingNormalized = "encoding_normalized" // invalid sequence replaced
)

// Column describes one collected column.
type Column struct {
	Name    string `json:"name"`
	SQLType string `json:"sql_type"`
}

// TableSpec names a table and its columns.
type TableSpec struct {
	Name    string   `json:"name"`
	Columns []Column `json:"columns"`
}

// Notice is a diagnostic note attached to a table or to the run.
type Notice struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
	Table   string `json:"table"`
}

// Completeness describes collection only. The collector fills it, and it,
// and only it, is what the manifest serializes.
type Completeness struct {
	RowsCollected      int64 `json:"rows_collected"`
	CollectionComplete bool  `json:"collection_complete"`
	PropertiesComplete bool  `json:"properties_complete"`
}

// PreviewState describes the preview only. Render fills it on its own copy
// of the result. The collector never writes it and the manifest never
// carries it: a manifest that carried rows_shown=0 would falsely promise
// that no row was ever displayed.
type PreviewState struct {
	RowsShown       int64    `json:"rows_shown"`
	PreviewComplete bool     `json:"preview_complete"`
	OmittedReasons  []string `json:"omitted_reasons"`
}

// TableResult carries one table's spec, its preview rows (never the full
// collection), its collection completeness, and its preview state.
type TableResult struct {
	Spec    TableSpec    `json:"spec"`
	Rows    [][]Cell     `json:"rows"`
	State   Completeness `json:"state"`
	Preview PreviewState `json:"preview"`
}

// Artifact records one exported file: its kind, path, size, and whether it
// was written in full.
type Artifact struct {
	Kind     string `json:"kind"`
	Path     string `json:"path"`
	Bytes    int64  `json:"bytes"`
	Complete bool   `json:"complete"`
}

// ContextInfo describes the connection the diagnostics ran against.
type ContextInfo struct {
	Server                string    `json:"server"`
	Database              string    `json:"database"`
	Principal             string    `json:"principal"`
	Version               string    `json:"version"`
	TLSEncryption         string    `json:"tls_encryption"`
	CertificateValidation string    `json:"certificate_validation"`
	CollectedAt           time.Time `json:"collected_at"`
}

// Result is the top-level diagnostic result serialized to stdout.
type Result struct {
	SchemaVersion int           `json:"schema_version"`
	OK            bool          `json:"ok"`
	Context       ContextInfo   `json:"context"`
	Tables        []TableResult `json:"tables"`
	Notices       []Notice      `json:"notices"`
	Artifacts     []Artifact    `json:"artifacts"`
	ManifestPath  string        `json:"manifest_path"`
	Error         *PublicError  `json:"error"`
}

// Sink receives one table's rows and, optionally, files and notices, as a
// diagnostic runs. Each diagnostic closes its own SQL rows; Sink never
// executes SQL itself.
type Sink interface {
	Begin(TableSpec) error
	Row([]Cell) error
	End(collectionComplete, propertiesComplete bool) error
	File(kind, suffix string, src io.Reader) (Artifact, error)
	Notice(Notice)
}
