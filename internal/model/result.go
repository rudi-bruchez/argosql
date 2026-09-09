package model

import (
	"io"
	"time"
)

// Cell holds one output value: nil, string, bool, int64, float64, or the
// exact SQL text as string when no narrower Go type applies.
//
// It is a bare interface type and not a struct wrapping one field, because a
// struct needs a MarshalJSON method to serialize as a naked value instead of
// {"Value":"abc"}, and a bare type needs nothing: encoding/json renders the
// dynamic value directly, so a row is an array of naked values for free.
// Measured byte-identical to the struct-plus-marshaller form. Consumers
// type-switch on a Cell directly rather than on a field of it.
type Cell any

// Closed vocabulary of omission reasons (design spec, line 101). This is
// the only vocabulary TableResult.Preview.OmittedReasons may ever carry;
// any other value there is a defect. The five are independent facts, not
// mutually exclusive - a table can carry more than one at once.
const (
	ReasonRowLimit            = "row_limit"            // rows beyond --preview N
	ReasonByteLimit           = "byte_limit"           // rows or cells removed to fit the stdout budget
	ReasonCellLimit           = "cell_limit"           // cell truncated by rune count
	ReasonCollectionLimit     = "collection_limit"     // collection itself stopped at a ceiling
	ReasonPropertyUnavailable = "property_unavailable" // optional property unreadable
)

// Kind values that are facts about a run, not reasons a preview omitted
// something - neither ever belongs in OmittedReasons.
//
// KindPreviewOmitted is the boolean field name of Render's tier-A compact
// response (design spec, line 105): the whole-response counterpart of the
// per-table state "zero rows shown while rows were collected" that line
// 101 also calls preview_omitted, as distinct from a table that is
// legitimately empty.
//
// KindEncodingNormalized is the Notice.Kind the collector emits (design
// spec, line 91) when it had to replace an invalid byte sequence while
// encoding a table artifact - a fact recorded for its own sake, not a
// reason any preview left something out.
const (
	KindPreviewOmitted     = "preview_omitted"
	KindEncodingNormalized = "encoding_normalized"
)

// Column describes one collected column. SQLType must be the exact
// string (*sql.ColumnType).DatabaseTypeName() returns for the column -
// "BIGINT", "DECIMAL(38,4)", "UNIQUEIDENTIFIER" - not a hand-written SQL
// type name: internal/output's JSON and TSV encoders and decoders key
// off SQLType, case-insensitively and up to its first "(", to decide
// whether a cell is a bigint (quoted in JSON, parsed back to int64 on
// read) or one of the int/bit/float families reconstructed from artifact
// text; any other spelling of the same type is read back as an opaque
// string, not as the narrower Go type a diagnostic would expect.
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
//
// Reason is set only for an OMITTED artifact - one that was never
// written at all because a single collected value exceeded the
// artifact byte quota (design spec, line 111: "return code 7 with an
// explicit omitted-artifact record" - fix 1's A5). Path stays empty
// (nothing was ever written) and Bytes holds however many bytes were
// actually read before the quota breach was detected - a lower bound
// on the source's true size, never a size that was ever committed to
// disk. Drawn from the same closed vocabulary as every other omission
// reason in this project (ReasonCollectionLimit); empty for every
// ordinary, successfully written artifact.
type Artifact struct {
	Kind     string `json:"kind"`
	Path     string `json:"path"`
	Bytes    int64  `json:"bytes"`
	Complete bool   `json:"complete"`
	Reason   string `json:"reason,omitempty"`
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

// ManifestOnlyResult is Render's tier-A fallback (design spec, line 105):
// when a run's metadata alone - every table rendered with zero rows -
// still does not fit ByteLimit, but the manifest path that can recover
// everything collected does, this is what Render returns instead of the
// usual Result. Full metadata stays on disk in the manifest;
// PreviewOmitted is always true here, the whole-response analogue of a
// single table reporting rows_shown=0 despite rows collected. It is a
// dedicated type, not a degenerate Result, for the same reason
// FallbackError is one: Result's own fields (context, tables, ...) would
// still be present under omitempty, and this response must never carry
// them - only the two facts a caller needs to recover the run's data.
type ManifestOnlyResult struct {
	SchemaVersion  int          `json:"schema_version"`
	OK             bool         `json:"ok"`
	ManifestPath   string       `json:"manifest_path"`
	PreviewOmitted bool         `json:"preview_omitted"`
	Error          *PublicError `json:"error"`
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
