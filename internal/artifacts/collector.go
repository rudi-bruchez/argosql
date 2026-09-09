// Package artifacts streams diagnostic output to disk through
// model.Sink, enforcing the project's two collection limits (a total
// row count and a total byte footprint, manifest included) and writing
// the manifest that documents what landed in the user's own files.
package artifacts

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/rudi-bruchez/argosql/internal/model"
	"github.com/rudi-bruchez/argosql/internal/output"
)

// countingWriter discards every byte written to it but remembers how
// many were requested.
type countingWriter struct{ n int64 }

func (w *countingWriter) Write(p []byte) (int, error) {
	w.n += int64(len(p))
	return len(p), nil
}

// measureTotal returns the exact final size of a table artifact holding
// row (or zero rows, if row is nil), by actually running it - header,
// the row if any, and the format's trailer - through a disposable,
// fully closed Encoder that discards its bytes instead of writing them
// to disk.
//
// A persistent "shadow" encoder, measured incrementally as WriteRow is
// called, cannot give this answer: Encoder's JSON and TSV writers both
// buffer internally (bufio.Writer's default size) and only flush to the
// underlying io.Writer on Close or when that buffer fills, so a
// counting writer behind one sees zero bytes for every row small
// enough to still be sitting in the buffer - exactly every row this
// project's limits care about. Running Close here, every time, is what
// forces that flush and makes the count exact.
func measureTotal(format string, spec model.TableSpec, row []model.Cell) (int64, error) {
	cw := &countingWriter{}
	enc, err := output.NewTableEncoder(cw, format, spec)
	if err != nil {
		return 0, err
	}
	if row != nil {
		if err := enc.WriteRow(row); err != nil {
			return 0, err
		}
	}
	if err := enc.Close(); err != nil {
		return 0, err
	}
	return cw.n, nil
}

// rowSeparatorBytes is the number of extra bytes the real encoder adds
// to every row after the first, purely from row-separator punctuation:
// jsonEncoder.WriteRow (json.go) writes one comma byte before every row
// but the first; tsvEncoder (tsv.go) has no such separator, each row is
// simply its own newline-terminated line. measureTotal above always
// measures a row as if it were the table's only row (so, for JSON, with
// no leading comma); this constant corrects that measurement for every
// row after the first.
func rowSeparatorBytes(format string) int64 {
	if strings.EqualFold(format, output.FormatJSON) {
		return 1
	}
	return 0
}

// tableState is the bookkeeping Collector keeps for the one table
// currently open between Begin and End (or a limit breach).
type tableState struct {
	spec       model.TableSpec
	file       *os.File
	path       string
	real       *output.Encoder
	rows       int64
	bytes      int64 // predicted final size of the artifact if closed right now
	emptyBytes int64 // measureTotal's answer for this spec with zero rows
}

// tableManifestEntry is what Collector keeps per finished table for
// Finish to assemble into model.Result.Tables: the spec and collection
// completeness only - never rows, never a PreviewState, because at the
// point the collector writes, no preview has been computed yet.
type tableManifestEntry struct {
	spec  model.TableSpec
	state model.Completeness
}

// Collector implements model.Sink: it turns the Begin/Row/End/File/
// Notice calls a diagnostic makes into files under one run directory,
// refusing to collect past either configured Limits, and accumulates
// what Finish needs to produce a model.Result and a manifest.
type Collector struct {
	store  *store
	format string
	limits Limits

	totalRows int64
	current   *tableState
	tables    []tableManifestEntry
	notices   []model.Notice
	artifacts []model.Artifact
}

// New creates a Collector writing under a freshly created, unique
// subdirectory of dir, encoding tables in format ("tsv" or "json"), and
// refusing to collect past limits.
func New(dir, format string, limits Limits) (*Collector, error) {
	st, err := newStore(dir, limits)
	if err != nil {
		return nil, err
	}
	return &Collector{store: st, format: format, limits: limits}, nil
}

// Dir returns the run directory this Collector writes under.
func (c *Collector) Dir() string { return c.store.dir }

// collectionLimitError builds the PublicError a breach of either
// collection limit returns: exit code 7, the closed vocabulary's
// "collection_limit" kind.
func collectionLimitError(message string) error {
	return &model.PublicError{Code: 7, Kind: "collection_limit", Message: message}
}

// artifactError builds the PublicError a file or serialization failure
// returns: exit code 6.
func artifactError(message string, cause error) error {
	if cause != nil {
		message = fmt.Sprintf("%s: %s", message, cause.Error())
	}
	return &model.PublicError{Code: 6, Kind: "artifact", Message: message}
}

// Begin opens a new table artifact. It measures the empty artifact's
// size (header plus trailer, zero rows) against the remaining byte
// budget before ever creating the real file, so a run that is already
// out of room fails with the collection-limit error instead of leaving
// an empty or truncated file behind.
func (c *Collector) Begin(spec model.TableSpec) error {
	if c.current != nil {
		return fmt.Errorf("artifacts: Begin called for table %q while table %q is still open", spec.Name, c.current.spec.Name)
	}

	emptyBytes, err := measureTotal(c.format, spec, nil)
	if err != nil {
		return artifactError("measuring empty table artifact", err)
	}
	if !c.store.hasRoom(emptyBytes) {
		return collectionLimitError(fmt.Sprintf("artifact byte budget exhausted before table %q could open", spec.Name))
	}

	ext := strings.ToLower(c.format)
	f, path, err := c.store.create(sanitizeName(spec.Name) + "." + ext)
	if err != nil {
		return artifactError("creating table artifact file", err)
	}
	real, err := output.NewTableEncoder(f, c.format, spec)
	if err != nil {
		f.Close()
		os.Remove(path)
		return artifactError("creating table encoder", err)
	}

	c.store.used += emptyBytes
	c.current = &tableState{
		spec:       spec,
		file:       f,
		path:       path,
		real:       real,
		bytes:      emptyBytes,
		emptyBytes: emptyBytes,
	}
	return nil
}

// Row measures row, in isolation, against the open table's empty-
// artifact baseline before committing it to the real file (see
// measureTotal and rowSeparatorBytes for why this, and not an
// incrementally measured shadow encoder, is what gives an exact byte
// count per row). Exactly Limits.Rows rows, cumulative across every
// table this Collector has opened, are ever accepted: the
// (Limits.Rows+1)-th Row call, on any table, is refused without being
// written, and likewise for a row that would push the run's total
// bytes past Limits.Bytes. Either breach finalizes the open table as
// incomplete (valid, closed JSON/TSV of whatever rows were already
// accepted) and returns a collection-limit error; the caller is not
// expected to call End afterwards.
func (c *Collector) Row(row []model.Cell) error {
	if c.current == nil {
		return fmt.Errorf("artifacts: Row called with no open table")
	}
	if c.totalRows >= c.limits.Rows {
		return c.breach(fmt.Sprintf("row limit of %d exceeded", c.limits.Rows))
	}

	soloTotal, err := measureTotal(c.format, c.current.spec, row)
	if err != nil {
		return fmt.Errorf("artifacts: measuring row: %w", err)
	}
	delta := soloTotal - c.current.emptyBytes
	if c.current.rows > 0 {
		delta += rowSeparatorBytes(c.format)
	}
	if !c.store.hasRoom(delta) {
		return c.breach(fmt.Sprintf("artifact byte budget of %d bytes exceeded", c.limits.Bytes))
	}

	if err := c.current.real.WriteRow(row); err != nil {
		return artifactError("writing table row", err)
	}
	c.store.used += delta
	c.current.bytes += delta
	c.current.rows++
	c.totalRows++
	return nil
}

// breach finalizes the currently open table as incomplete and returns
// the collection-limit error for message, unless finalizing itself
// fails - a file error discovered while closing out the table takes
// precedence over the limit that triggered the close, per the project
// rule that a later file error always overrides an earlier collection
// error.
func (c *Collector) breach(message string) error {
	if err := c.finalizeCurrent(false, false); err != nil {
		return err
	}
	return collectionLimitError(message)
}

// End closes the currently open table, recording collectionComplete and
// propertiesComplete exactly as the caller reports them: End never
// infers completeness from row counts itself (a TOP N query that
// legitimately returns exactly Limits.Rows rows and then calls
// End(true, ...) is complete, not a limit breach - only Row's own
// refusal of the row that would exceed a limit ever marks a table
// incomplete on these grounds).
func (c *Collector) End(collectionComplete, propertiesComplete bool) error {
	if c.current == nil {
		return fmt.Errorf("artifacts: End called with no open table")
	}
	return c.finalizeCurrent(collectionComplete, propertiesComplete)
}

// finalizeCurrent closes the open table's encoder and file, emits the
// encoding-normalization notice if the encoder ever substituted invalid
// UTF-8, and records the resulting artifact and table state. It is a
// no-op if no table is open (Finish calls it unconditionally to clean
// up a table a diagnostic began but never closed).
func (c *Collector) finalizeCurrent(collectionComplete, propertiesComplete bool) error {
	t := c.current
	if t == nil {
		return nil
	}
	c.current = nil

	closeErr := t.real.Close()
	fileErr := t.file.Close()
	if closeErr != nil {
		return artifactError(fmt.Sprintf("closing table %q artifact", t.spec.Name), closeErr)
	}
	if fileErr != nil {
		return artifactError(fmt.Sprintf("closing table %q artifact file", t.spec.Name), fileErr)
	}

	if t.real.EncodingNormalized() {
		c.Notice(model.Notice{
			Kind:    model.KindEncodingNormalized,
			Message: fmt.Sprintf("table %q: invalid UTF-8 byte sequences were replaced with U+FFFD while encoding", t.spec.Name),
			Table:   t.spec.Name,
		})
	}

	c.store.record(t.path, t.bytes, collectionComplete)
	c.artifacts = append(c.artifacts, model.Artifact{
		Kind:     "table",
		Path:     t.path,
		Bytes:    t.bytes,
		Complete: collectionComplete,
	})
	c.tables = append(c.tables, tableManifestEntry{
		spec: t.spec,
		state: model.Completeness{
			RowsCollected:      t.rows,
			CollectionComplete: collectionComplete,
			PropertiesComplete: propertiesComplete,
		},
	})
	return nil
}

// File copies one indivisible source object (an XML execution plan, a
// module's definition text) into a new artifact file, refusing to leave
// a truncated final file behind: it reads at most one byte more than
// the remaining byte budget allows (the brief's overflow sentinel), and
// if that extra byte was actually present, the partial file is deleted
// entirely rather than kept - unlike a table, there is no "rows
// accepted so far" to close cleanly around a single opaque object.
//
// A refusal also records an OMITTED artifact entry in c.artifacts - a
// model.Artifact with no Path (nothing was ever written), Bytes set to
// however many bytes were actually read before the breach was
// detected, and Reason set to the closed vocabulary's own
// "collection_limit" (design spec, line 111: "return code 7 with an
// explicit omitted-artifact record" - fix 1's A5). Before this fix,
// only the returned error named kind, in a sentence; a consumer of the
// artifact registry (the manifest, or the live result - both share
// this same slice, see Finish) found no structured trace that an
// artifact had ever been attempted at all. This entry is recorded
// regardless of what the caller does with the returned error - the
// same "a Sink already accepted what it accepted" rule Finish itself
// documents.
func (c *Collector) File(kind, suffix string, src io.Reader) (model.Artifact, error) {
	remaining := c.store.budget - c.store.used
	if remaining < 0 {
		remaining = 0
	}

	f, path, err := c.store.create(sanitizeName(kind) + suffix)
	if err != nil {
		return model.Artifact{}, artifactError("creating artifact file", err)
	}

	n, copyErr := io.Copy(f, io.LimitReader(src, remaining+1))
	if copyErr != nil {
		f.Close()
		os.Remove(path)
		return model.Artifact{}, artifactError(fmt.Sprintf("writing %q artifact", kind), copyErr)
	}
	if n > remaining {
		f.Close()
		os.Remove(path)
		c.artifacts = append(c.artifacts, model.Artifact{Kind: kind, Bytes: n, Complete: false, Reason: model.ReasonCollectionLimit})
		return model.Artifact{}, collectionLimitError(fmt.Sprintf("artifact %q exceeds the remaining byte budget", kind))
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return model.Artifact{}, artifactError(fmt.Sprintf("closing %q artifact", kind), err)
	}

	c.store.used += n
	c.store.record(path, n, true)
	artifact := model.Artifact{Kind: kind, Path: path, Bytes: n, Complete: true}
	c.artifacts = append(c.artifacts, artifact)
	return artifact, nil
}

// Notice appends n to the run's notices. Collector never caps or filters
// this list; the manifest (which must not carry an unbounded warning
// list) caps its own, separate copy when it is built - see manifest.go.
func (c *Collector) Notice(n model.Notice) {
	c.notices = append(c.notices, n)
}

// toPublicError unwraps err (at any %w depth) to a *model.PublicError,
// falling back to a generic execution error (code 5, matching
// model.ExitCode's own fallback) for anything else.
func toPublicError(err error) *model.PublicError {
	if err == nil {
		return nil
	}
	var pub *model.PublicError
	if errors.As(err, &pub) {
		return pub
	}
	return &model.PublicError{Code: 5, Kind: "execution", Message: err.Error()}
}

// Finish assembles the run's model.Result and writes its manifest.
//
// runErr is the collection error the caller already observed (nil on a
// clean run); Finish preserves it in the returned error and in
// Result.Error, unless finishing itself hits a file or serialization
// problem - that new error's code 6 always takes precedence, per the
// project rule that a later file error overrides an earlier collection
// error.
//
// If a table was left open (a diagnostic returned without calling End),
// Finish closes it out as incomplete before doing anything else, so it
// is never silently lost from the run's accounting.
func (c *Collector) Finish(info model.ContextInfo, runErr error) (model.Result, error) {
	if c.current != nil {
		if err := c.finalizeCurrent(false, false); err != nil {
			return model.Result{}, err
		}
	}

	tables := make([]model.TableResult, len(c.tables))
	for i, t := range c.tables {
		// Rows and Preview are intentionally left zero: the collector
		// never captures preview rows and never computes a preview -
		// see manifest.go's doc comment on why the manifest must never
		// carry PreviewState.
		tables[i] = model.TableResult{Spec: t.spec, State: t.state}
	}

	result := model.Result{
		SchemaVersion: 1,
		OK:            runErr == nil,
		Context:       info,
		Tables:        tables,
		Notices:       c.notices,
		Artifacts:     c.artifacts,
		Error:         toPublicError(runErr),
	}

	manifestPath, err := c.writeManifest(&result)
	if err != nil {
		return result, err
	}
	result.ManifestPath = manifestPath
	return result, runErr
}
