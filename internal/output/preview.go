// preview.go bounds what this program ever writes to stdout. The rest of
// this package and internal/artifacts can let a diagnostic's full result
// grow as large as the collection limits allow; this file is the one place
// that takes that result and a caller's preview options and produces a
// byte string that is never larger than options.ByteLimit, newline
// included, while never letting a row that was merely too big to show in
// full collapse into a table that looks like it collected nothing.
//
// Render does not decide how many rows a diagnostic collected, and it does
// not touch internal/artifacts or internal/model.Completeness - the
// collector already settled those facts before Render ever runs. What
// Render decides is purely a display question: of the rows a caller
// already has in memory, or that are sitting in a table artifact Finish
// recorded in model.Result.Artifacts, how many can be shown, and in what
// truncated form, without the serialized response exceeding its budget.
package output

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/rudi-bruchez/argosql/internal/model"
)

// unboundedBytes is the sentinel "no byte ceiling yet" value passed to
// buildCandidateRow while gathering candidates: at that stage only the
// rune-count cell limit applies (step 1 of the ordering below), never a
// forced byte truncation, so no real row ever reaches this value.
const unboundedBytes = int64(1) << 62

// PreviewOptions bounds one call to Render: at most Rows rows per table,
// each text cell truncated to CellLimit Unicode code points unless
// NoTruncate is set, and the whole serialized response capped at
// ByteLimit bytes including its trailing newline. NoTruncate disables only
// the rune-count cell limit; it never disables ByteLimit.
type PreviewOptions struct {
	Rows       int
	CellLimit  int
	NoTruncate bool
	ByteLimit  int
}

// tableWork is Render's working state for one table while it decides what
// to show: the candidate rows actually available (original, exactly as
// read, and the version currently selected for display), and how many of
// them, as a prefix, are selected for the final output.
type tableWork struct {
	spec      model.TableSpec
	collected int64

	// original holds each gathered row exactly as read from its source
	// (after no truncation of any kind); candidates holds the version
	// currently offered for display, which starts as original's
	// rune-limited form and may be shrunk further, in place, by the
	// final budget pass. cellTrunc and byteTrunc track, per row, which
	// kind of truncation (if any) candidates[i] currently carries.
	original   [][]model.Cell
	candidates [][]model.Cell
	cellTrunc  []bool
	byteTrunc  []bool

	// byteCappedAtGather is true when gatherTable stopped reading
	// candidate rows early because the loose per-table byte reservation
	// (cumulative+sz>ByteLimit) tripped, rather than because --preview N
	// (options.Rows) was reached or the source was exhausted. It
	// disambiguates why len(candidates) can be less than collected: that
	// gap is row_limit when this is false (the row cap is what stopped
	// gathering) and byte_limit when this is true (the byte budget is
	// what stopped it), never both for the same table.
	byteCappedAtGather bool

	// selected is how many of candidates (always a prefix, in
	// collection order) are currently part of the output.
	selected int
}

// Render turns result into a bounded stdout response in format ("json" or
// "tsv"), honoring options. It is the only place this project decides what
// a diagnostic's stdout actually looks like.
//
// Render works on a copy of result: it never mutates result.Tables, and it
// fills model.PreviewState itself - the collector's manifest never carries
// PreviewState (see model.PreviewState's doc comment), so there is nothing
// upstream for Render to contradict.
//
// Candidates come from one of two places per table, chosen unambiguously:
// if result.Tables[i].Rows is non-empty, those rows are the candidates and
// Render never touches disk for that table; otherwise, if a "table"
// artifact for that table's name is recorded in result.Artifacts, Render
// rereads it with NewTableDecoder. A caller that already holds its rows in
// memory has no reason to pay for a second read.
func Render(result model.Result, options PreviewOptions, format string) ([]byte, error) {
	if !strings.EqualFold(format, FormatJSON) && !strings.EqualFold(format, FormatTSV) {
		return nil, fmt.Errorf("output: unsupported output format %q", format)
	}

	works := make([]tableWork, len(result.Tables))
	for i, t := range result.Tables {
		w, err := gatherTable(t, result.Artifacts, options, format)
		if err != nil {
			return nil, err
		}
		works[i] = w
	}

	// Step 2: reserve the envelope and every table's metadata by
	// measuring the response exactly as it would be with every table
	// showing zero rows. This is usually, but not provably always, a
	// floor the trimming loop below can fall back to: the
	// omitted_reasons text in each omitted table's preview has its own
	// cost, which can exceed what a single small retained row would have
	// cost (a one-digit int cell is a real example), and a row that
	// needs its own cell_limit or byte_limit truncation once shown costs
	// more than the zero-row envelope assumed, since the envelope never
	// shows any row and so never pays for those two reasons. When either
	// happens, the loop below still terminates correctly - it just
	// reaches its own fallback call rather than this early one.
	envelope, err := encodeResult(format, buildZeroResult(result, works))
	if err != nil {
		return nil, err
	}
	effectiveLimit := int64(options.ByteLimit) - finalNewlineBytes(envelope)
	if int64(len(envelope)) > effectiveLimit {
		return fallback(result, options)
	}
	remaining := effectiveLimit - int64(len(envelope))

	// Step 3 + 4: equal shares among non-empty tables, whole rows within
	// each table's share, the table's first candidate row always
	// included regardless of its size.
	var nonEmpty []int
	for i := range works {
		if len(works[i].candidates) > 0 {
			nonEmpty = append(nonEmpty, i)
		}
	}
	if len(nonEmpty) > 0 {
		share := remaining / int64(len(nonEmpty))
		for _, i := range nonEmpty {
			if err := fillTableShare(&works[i], format, share); err != nil {
				return nil, err
			}
		}
		// Step 5: redistribute what non-empty tables left unused, in
		// spec order, to tables that still have more candidates
		// waiting.
		if err := redistribute(works, nonEmpty, format, share); err != nil {
			return nil, err
		}
	}

	// Step 6: re-encode for real and verify the exact size, including
	// the trailing newline. Adding rows can grow preview metadata
	// (rows_shown's digits, omitted_reasons' entries) in ways the share
	// accounting above does not model exactly, so this is not a
	// redundant check - it is the actual guarantee. When it fails,
	// shrink: first drop rows beyond each table's mandatory first row,
	// then shrink a mandatory row's own content, and only as a last
	// resort drop a mandatory row entirely (marking that table omitted,
	// never silently empty).
	for iterations := 0; ; iterations++ {
		if iterations > 100000 {
			return fallback(result, options)
		}
		enc, err := encodeResult(format, buildResult(result, works))
		if err != nil {
			return nil, err
		}
		total := int64(len(enc)) + finalNewlineBytes(enc)
		if total <= int64(options.ByteLimit) {
			return appendFinalNewline(enc), nil
		}
		overage := total - int64(options.ByteLimit)

		if dropOneSurplusRow(works) {
			continue
		}
		shrunk, err := shrinkOneMandatoryRow(works, format, options, overage)
		if err != nil {
			return nil, err
		}
		if shrunk {
			continue
		}
		if dropOneMandatoryRow(works) {
			continue
		}
		// Nothing left to shrink or drop: every table is down to zero
		// rows, and that still does not fit (the early check above
		// does not rule this out - see its comment on rows that cost
		// less than their own omission reason). Fall back rather than
		// emit something over budget.
		return fallback(result, options)
	}
}

// gatherTable reads up to options.Rows candidate rows for one table, from
// whichever source applies (see Render's doc comment), applying the
// rune-count cell limit (step 1) as it goes. It also enforces a loose,
// ByteLimit-wide memory reservation across rows after the first: a row
// that collectively would push the table's gathered bytes past ByteLimit
// stops further reading for that table, but the first candidate row of a
// non-empty table is always gathered regardless of its own size - dropping
// it here would be exactly the defect this task exists to prevent, before
// Render even gets to decide how to display it.
func gatherTable(t model.TableResult, artifacts []model.Artifact, options PreviewOptions, format string) (tableWork, error) {
	w := tableWork{spec: t.Spec, collected: t.State.RowsCollected}

	src, err := openSource(t, artifacts, format)
	if err != nil {
		return w, err
	}
	if src == nil {
		return w, nil
	}
	defer src.Close()

	var cumulative int64
	for i := 0; i < options.Rows; i++ {
		row, ok, err := src.Next()
		if err != nil {
			return w, wrapPreviewReadErr(err)
		}
		if !ok {
			break
		}
		display, cellTrunc, _, err := buildCandidateRow(row, t.Spec.Columns, format, i, options.CellLimit, options.NoTruncate, unboundedBytes)
		if err != nil {
			return w, err
		}
		sz, err := rowBytes(format, t.Spec.Columns, display, i)
		if err != nil {
			return w, err
		}
		if i > 0 && cumulative+sz > int64(options.ByteLimit) {
			w.byteCappedAtGather = true
			break
		}
		cumulative += sz
		w.original = append(w.original, row)
		w.candidates = append(w.candidates, display)
		w.cellTrunc = append(w.cellTrunc, cellTrunc)
		w.byteTrunc = append(w.byteTrunc, false)
	}
	return w, nil
}

// fillTableShare selects w's first candidate row unconditionally, then
// greedily adds further whole candidate rows, in order, while they still
// fit within share.
func fillTableShare(w *tableWork, format string, share int64) error {
	if len(w.candidates) == 0 {
		return nil
	}
	w.selected = 1
	used, err := rowBytes(format, w.spec.Columns, w.candidates[0], 0)
	if err != nil {
		return err
	}
	for j := 1; j < len(w.candidates); j++ {
		sz, err := rowBytes(format, w.spec.Columns, w.candidates[j], j)
		if err != nil {
			return err
		}
		if used+sz > share {
			break
		}
		used += sz
		w.selected++
	}
	return nil
}

// redistribute hands out whatever share non-empty tables left unused to
// tables that still have more candidates waiting, processing the table
// list in spec order, repeating rounds until no table can use any more of
// the pool.
func redistribute(works []tableWork, nonEmpty []int, format string, share int64) error {
	var pool int64
	for _, i := range nonEmpty {
		w := &works[i]
		var used int64
		for j := 0; j < w.selected; j++ {
			sz, err := rowBytes(format, w.spec.Columns, w.candidates[j], j)
			if err != nil {
				return err
			}
			used += sz
		}
		if leftover := share - used; leftover > 0 {
			pool += leftover
		}
	}
	changed := true
	for changed && pool > 0 {
		changed = false
		for _, i := range nonEmpty {
			w := &works[i]
			if w.selected >= len(w.candidates) {
				continue
			}
			sz, err := rowBytes(format, w.spec.Columns, w.candidates[w.selected], w.selected)
			if err != nil {
				return err
			}
			if sz <= pool {
				w.selected++
				pool -= sz
				changed = true
			}
		}
	}
	return nil
}

// dropOneSurplusRow removes one row beyond some table's mandatory first
// row, scanning tables in reverse spec order so that an earlier table's
// extra rows survive longer than a later table's. It reports whether it
// found one to drop.
func dropOneSurplusRow(works []tableWork) bool {
	for i := len(works) - 1; i >= 0; i-- {
		if works[i].selected > 1 {
			works[i].selected--
			return true
		}
	}
	return false
}

// shrinkOneMandatoryRow, once every surplus row is already gone, tries to
// shrink a table's sole remaining (mandatory) row's own content further,
// in reverse spec order, by roughly overage bytes plus a safety margin. It
// reports whether any table's row actually got smaller.
func shrinkOneMandatoryRow(works []tableWork, format string, options PreviewOptions, overage int64) (bool, error) {
	for i := len(works) - 1; i >= 0; i-- {
		w := &works[i]
		if w.selected != 1 {
			continue
		}
		cur, err := rowBytes(format, w.spec.Columns, w.candidates[0], 0)
		if err != nil {
			return false, err
		}
		target := cur - overage - 16
		if target < 0 {
			target = 0
		}
		display, cellTrunc, byteTrunc, err := buildCandidateRow(w.original[0], w.spec.Columns, format, 0, options.CellLimit, options.NoTruncate, target)
		if err != nil {
			return false, err
		}
		newSz, err := rowBytes(format, w.spec.Columns, display, 0)
		if err != nil {
			return false, err
		}
		if newSz >= cur {
			continue // nothing left to shrink on this row
		}
		w.candidates[0] = display
		w.cellTrunc[0] = cellTrunc
		w.byteTrunc[0] = byteTrunc
		return true, nil
	}
	return false, nil
}

// dropOneMandatoryRow, only once nothing else can be shrunk, removes a
// table's last remaining row entirely - that table will show rows_shown=0
// with omitted_reasons set, never a silently empty preview, because
// buildPreviewState always distinguishes the two. Scans in reverse spec
// order, same priority as the other two drop/shrink helpers.
func dropOneMandatoryRow(works []tableWork) bool {
	for i := len(works) - 1; i >= 0; i-- {
		if works[i].selected == 1 {
			works[i].selected = 0
			return true
		}
	}
	return false
}

// buildCandidateRow derives the cell content one row should display,
// always starting from original (never from a previously truncated
// value, so repeated calls with a shrinking maxBytes never compound
// truncation markers). It applies the rune-count cell limit first (unless
// noTruncate or cellLimit <= 0), then, only if the row's encoded form
// still exceeds maxBytes, shrinks string cells further - largest first -
// until it fits or nothing more can be cut. Any truncation appends a
// "…[+N]" marker, N counting the omitted Unicode code points, so truncated
// content never reads as complete.
func buildCandidateRow(original []model.Cell, cols []model.Column, format string, idx int, cellLimit int, noTruncate bool, maxBytes int64) (row []model.Cell, cellTrunc, byteTrunc bool, err error) {
	row = make([]model.Cell, len(original))
	copy(row, original)

	if !noTruncate && cellLimit > 0 {
		for i, c := range row {
			s, ok := c.(string)
			if !ok {
				continue
			}
			if rc := utf8.RuneCountInString(s); rc > cellLimit {
				row[i] = truncateRunes(s, cellLimit)
				cellTrunc = true
			}
		}
	}

	sz, err := rowBytes(format, cols, row, idx)
	if err != nil {
		return nil, false, false, err
	}
	for sz > maxBytes {
		ci := largestStringCell(row)
		if ci < 0 {
			break
		}
		base, _ := stripMarker(row[ci].(string))
		lo, hi, best := 0, utf8.RuneCountInString(base), -1
		for lo <= hi {
			mid := (lo + hi) / 2
			trial := make([]model.Cell, len(row))
			copy(trial, row)
			trial[ci] = truncateRunes(base, mid)
			tsz, err := rowBytes(format, cols, trial, idx)
			if err != nil {
				return nil, false, false, err
			}
			if tsz <= maxBytes {
				best = mid
				lo = mid + 1
			} else {
				hi = mid - 1
			}
		}
		if best < 0 {
			best = 0
		}
		newText := truncateRunes(base, best)
		if newText == row[ci] {
			break // this cell cannot shrink any further
		}
		row[ci] = newText
		byteTrunc = true
		sz, err = rowBytes(format, cols, row, idx)
		if err != nil {
			return nil, false, false, err
		}
	}
	return row, cellTrunc, byteTrunc, nil
}

// truncateRunes keeps the first n runes of s, appending "…[+K]" (K the
// number of omitted Unicode code points) when that is fewer than s holds;
// it returns s unchanged when n covers all of it.
func truncateRunes(s string, n int) string {
	if n < 0 {
		n = 0
	}
	runes := []rune(s)
	if n >= len(runes) {
		return s
	}
	omitted := len(runes) - n
	return string(runes[:n]) + fmt.Sprintf("…[+%d]", omitted)
}

// stripMarker removes a trailing "…[+K]" marker truncateRunes may have
// appended, so a second, tighter truncation pass always searches from the
// real content rather than compounding markers.
func stripMarker(s string) (string, bool) {
	const sep = "…[+"
	i := strings.LastIndex(s, sep)
	if i < 0 {
		return s, false
	}
	rest := s[i+len(sep):]
	if !strings.HasSuffix(rest, "]") {
		return s, false
	}
	digits := rest[:len(rest)-1]
	if digits == "" {
		return s, false
	}
	for _, c := range digits {
		if c < '0' || c > '9' {
			return s, false
		}
	}
	return s[:i], true
}

// largestStringCell returns the index of row's string cell with the most
// runes, or -1 if row holds no string cell with any content left to cut.
func largestStringCell(row []model.Cell) int {
	best, bestLen := -1, 0
	for i, c := range row {
		s, ok := c.(string)
		if !ok {
			continue
		}
		if l := utf8.RuneCountInString(s); l > bestLen {
			bestLen = l
			best = i
		}
	}
	return best
}

// rowBytes measures exactly how many bytes row contributes to the
// response in format, at position idx within its table's row list
// (needed only for JSON's leading comma before every row but the first).
func rowBytes(format string, cols []model.Column, row []model.Cell, idx int) (int64, error) {
	if strings.EqualFold(format, FormatJSON) {
		b, err := encodeJSONRow(cols, row)
		if err != nil {
			return 0, err
		}
		n := int64(len(b))
		if idx > 0 {
			n++ // the leading comma jsonEncoder.WriteRow would add
		}
		return n, nil
	}
	fields := make([]string, len(row))
	for i, c := range row {
		fields[i] = EncodeTSVCell(c)
	}
	line := "row\t" + strings.Join(fields, "\t") + "\n"
	return int64(len(line)), nil
}

// tableReasons derives, for one table, which of the closed vocabulary's
// five conditions apply when exactly selected of w.candidates are shown:
//
//   - rowLimited: gathering itself never saw every collected row, and the
//     byte budget was not why (w.byteCappedAtGather is false) - --preview
//     N is what capped it.
//   - byteLimited: either gathering stopped early to stay within the
//     byte budget (w.byteCappedAtGather), or the final selection shows
//     fewer rows than were actually gathered (selected < len(candidates),
//     room for them existed but the output budget did not), or one of
//     the selected rows itself needed a byte-level shrink
//     (w.byteTrunc[j]) to fit.
//   - cellLimited: one of the selected rows needed rune-count truncation
//     (w.cellTrunc[j]).
//
// These two sources of "rows left out" are deliberately kept apart: a row
// beyond --preview N and a row dropped to stay under the stdout byte cap
// are different facts a caller needs to tell apart, and the code that
// used to conflate them into one rows_truncated reason is exactly the
// defect this function exists to correct. rowLimited and byteLimited can
// both be true for the same table at once (more rows exist beyond what
// --preview N let through, and what did get gathered still didn't all
// fit the byte budget) - they are independent facts, not alternatives.
func tableReasons(w tableWork, selected int) (rowLimited, byteLimited, cellLimited bool) {
	rowLimited = !w.byteCappedAtGather && w.collected > int64(len(w.candidates))
	byteLimited = w.byteCappedAtGather || selected < len(w.candidates)
	for j := 0; j < selected; j++ {
		if w.cellTrunc[j] {
			cellLimited = true
		}
		if w.byteTrunc[j] {
			byteLimited = true
		}
	}
	return rowLimited, byteLimited, cellLimited
}

// buildPreviewState is the single place that turns a table's collected
// count, how many rows ended up shown, and the specific limiting causes
// that applied, into model.PreviewState - including the reasons
// vocabulary, which is the closed set from internal/model.ReasonRowLimit
// etc. and nothing else. The five conditions are independent facts: a
// table can carry more than one omitted_reasons entry at once.
func buildPreviewState(collected, shown int64, rowLimited, byteLimited, cellLimited, collectionLimited, propertyUnavailable bool) model.PreviewState {
	reasons := []string{}
	if rowLimited {
		reasons = append(reasons, model.ReasonRowLimit)
	}
	if byteLimited {
		reasons = append(reasons, model.ReasonByteLimit)
	}
	if cellLimited {
		reasons = append(reasons, model.ReasonCellLimit)
	}
	if collectionLimited {
		reasons = append(reasons, model.ReasonCollectionLimit)
	}
	if propertyUnavailable {
		reasons = append(reasons, model.ReasonPropertyUnavailable)
	}
	return model.PreviewState{
		RowsShown:       shown,
		PreviewComplete: shown == collected,
		OmittedReasons:  reasons,
	}
}

// buildResult assembles the model.Result Render will encode, given the
// current selection in works: each table shows candidates[:selected], and
// its PreviewState reflects exactly that selection. It never mutates
// result or its Tables.
func buildResult(result model.Result, works []tableWork) model.Result {
	out := result
	out.Tables = make([]model.TableResult, len(result.Tables))
	for i, t := range result.Tables {
		w := works[i]
		rows := make([][]model.Cell, w.selected)
		copy(rows, w.candidates[:w.selected])
		rowLimited, byteLimited, cellLimited := tableReasons(w, w.selected)
		out.Tables[i] = model.TableResult{
			Spec:  t.Spec,
			Rows:  rows,
			State: t.State,
			Preview: buildPreviewState(w.collected, int64(w.selected), rowLimited, byteLimited, cellLimited,
				!t.State.CollectionComplete, !t.State.PropertiesComplete),
		}
	}
	return out
}

// buildZeroResult is buildResult with every table forced to show zero
// rows, regardless of what works currently has selected. It is the
// envelope-and-metadata reservation (step 2), and the state the trimming
// loop in Render converges toward in the worst case - though not a floor
// it is provably always under ByteLimit itself: see Render's comment on
// step 2 for the case where a row costs less than its own omission
// reason.
func buildZeroResult(result model.Result, works []tableWork) model.Result {
	out := result
	out.Tables = make([]model.TableResult, len(result.Tables))
	for i, t := range result.Tables {
		w := works[i]
		rowLimited, byteLimited, cellLimited := tableReasons(w, 0)
		out.Tables[i] = model.TableResult{
			Spec:  t.Spec,
			Rows:  nil,
			State: t.State,
			Preview: buildPreviewState(w.collected, 0, rowLimited, byteLimited, cellLimited,
				!t.State.CollectionComplete, !t.State.PropertiesComplete),
		}
	}
	return out
}

// fallback picks between Render's two degraded tiers (design spec, line
// 105) once neither the full response nor any amount of trimming fits
// ByteLimit: tier A (the manifest path plus preview_omitted=true) when it
// itself fits, tier B (the fixed, bounded error envelope, no embedded
// path) otherwise.
func fallback(result model.Result, options PreviewOptions) ([]byte, error) {
	if b, ok, err := fallbackTierA(result, options.ByteLimit); err != nil {
		return nil, err
	} else if ok {
		return b, nil
	}
	return fallbackTierB(result.SchemaVersion)
}

// fallbackTierA is Render's first fallback tier: when the zero-row
// envelope itself does not fit ByteLimit, a compact response carrying
// only result.ManifestPath and preview_omitted=true may still fit, and is
// far more useful to a caller than tier B's fixed error envelope - it
// names exactly where to find everything that was actually collected,
// rather than nothing at all. It reports ok=false (never an error) when
// no such response fits, or when result.ManifestPath is empty to begin
// with (nothing to point a caller at); a response that does fit is
// returned with a nil error, exactly like Render's normal path, because
// tier A is a valid, bounded, parseable answer, not a failure of this
// package. Like tier B, it is always JSON, regardless of the format the
// caller asked for, for the same reason: it is meant to be the one
// response that reliably fits and parses once the normal TSV/JSON choice
// has already failed.
func fallbackTierA(result model.Result, byteLimit int) (b []byte, ok bool, err error) {
	if result.ManifestPath == "" {
		return nil, false, nil
	}
	mr := model.ManifestOnlyResult{
		SchemaVersion:  result.SchemaVersion,
		OK:             result.OK,
		ManifestPath:   result.ManifestPath,
		PreviewOmitted: true,
		Error:          result.Error,
	}
	enc, err := json.Marshal(mr)
	if err != nil {
		return nil, false, err
	}
	enc = appendFinalNewline(enc)
	if int64(len(enc)) > int64(byteLimit) {
		return nil, false, nil
	}
	return enc, true, nil
}

// fallbackTierB is the last resort: response metadata alone (or Render's
// own attempt to shrink toward it) does not fit ByteLimit. It serializes
// from model.FallbackError, never from model.Result - Result would also
// carry context, tables and sql_number, and omitempty on PublicError's own
// fields would not remove any of those, only SQLNumber; FallbackError is
// the dedicated type with no such fields to begin with, and with OK
// required present (no omitempty) so "ok":false is never dropped. This
// literal is always JSON, regardless of the format the caller asked for:
// it is the one response this package guarantees fits and parses no
// matter what, so it does not participate in the normal TSV/JSON choice
// that has already failed by the time this runs.
func fallbackTierB(schemaVersion int) ([]byte, error) {
	fe := model.FallbackError{
		SchemaVersion: schemaVersion,
		OK:            false,
		Error: &model.PublicError{
			Code:    6,
			Kind:    "output_budget",
			Message: "response metadata exceeds output budget",
		},
	}
	b, err := json.Marshal(fe)
	if err != nil {
		return nil, err
	}
	return appendFinalNewline(b), fe.Error
}

// finalNewlineBytes reports how many bytes Render's trailing newline
// still costs on top of enc: 0 if enc (TSV's own line-oriented format
// always does) already ends in "\n", 1 otherwise (JSON never does). The
// byte cap always counts exactly one trailing newline, never two.
func finalNewlineBytes(enc []byte) int64 {
	if bytes.HasSuffix(enc, []byte("\n")) {
		return 0
	}
	return 1
}

// appendFinalNewline appends Render's trailing newline, unless enc
// already ends in one.
func appendFinalNewline(enc []byte) []byte {
	if bytes.HasSuffix(enc, []byte("\n")) {
		return enc
	}
	return append(enc, '\n')
}

// -- second-read sources --

// rowSource is the uniform view Render takes of a table's candidate rows,
// whether they are already in memory (memorySource) or have to be read
// back from a table artifact on disk (artifactSource). Next reports ok =
// false, err = nil once the source is exhausted.
type rowSource interface {
	Next() (row []model.Cell, ok bool, err error)
	Close() error
}

type memorySource struct {
	rows [][]model.Cell
	i    int
}

func (s *memorySource) Next() ([]model.Cell, bool, error) {
	if s.i >= len(s.rows) {
		return nil, false, nil
	}
	row := s.rows[s.i]
	s.i++
	return row, true, nil
}

func (s *memorySource) Close() error { return nil }

type artifactSource struct {
	dec *Decoder
	f   *os.File
}

func (s *artifactSource) Next() ([]model.Cell, bool, error) {
	row, err := s.dec.Next()
	if err == io.EOF {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return row, true, nil
}

func (s *artifactSource) Close() error { return s.f.Close() }

// openSource picks t's candidate source per Render's doc comment: t.Rows
// when non-empty, otherwise the "table" artifact recorded for t.Spec.Name
// in artifacts, if any. It returns a nil source (not an error) when
// neither is available - that table simply has nothing to preview.
func openSource(t model.TableResult, artifacts []model.Artifact, format string) (rowSource, error) {
	if len(t.Rows) > 0 {
		return &memorySource{rows: t.Rows}, nil
	}
	path, artifactFormat, ok := findTableArtifact(artifacts, t.Spec.Name)
	if !ok {
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, wrapPreviewReadErr(fmt.Errorf("opening preview artifact %q: %w", path, err))
	}
	dec, err := NewTableDecoder(f, artifactFormat, t.Spec)
	if err != nil {
		f.Close()
		return nil, wrapPreviewReadErr(fmt.Errorf("decoding preview artifact %q: %w", path, err))
	}
	return &artifactSource{dec: dec, f: f}, nil
}

// findTableArtifact looks for the "table" artifact Finish recorded for
// tableName, matching on the same sanitization internal/artifacts/store.go
// applies to a table name before using it as a file name. Render cannot
// import internal/artifacts to reuse that function directly - that
// package already imports this one, and internal/output must stay below
// it - so sanitizeTableName below is a deliberate, small, pure copy of the
// identical algorithm.
func findTableArtifact(artifacts []model.Artifact, tableName string) (path, format string, ok bool) {
	base := sanitizeTableName(tableName)
	for _, a := range artifacts {
		if a.Kind != "table" {
			continue
		}
		name := filepath.Base(a.Path)
		ext := strings.ToLower(filepath.Ext(name))
		stem := strings.TrimSuffix(name, filepath.Ext(name))
		if stem != base {
			continue
		}
		switch ext {
		case ".tsv":
			return a.Path, FormatTSV, true
		case ".json":
			return a.Path, FormatJSON, true
		}
	}
	return "", "", false
}

// sanitizeTableName mirrors internal/artifacts/store.go's sanitizeName
// exactly: a table artifact's file name is always this function's result
// plus "." plus its format extension, so matching an artifact back to the
// table it belongs to requires the identical transformation.
func sanitizeTableName(name string) string {
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

// wrapPreviewReadErr turns a failure reading back a table artifact into
// the project's code-6 file/serialization error.
func wrapPreviewReadErr(err error) error {
	return &model.PublicError{Code: 6, Kind: "artifact", Message: fmt.Sprintf("reading preview artifact: %s", err.Error())}
}

// -- full-response encoding --

// encodeResult renders the whole result (metadata plus, for each table,
// whatever rows its Rows field currently holds) in format. It is used both
// for the real final output and for every trial size measurement along
// the way (the zero-row envelope, and the repeated re-encodes while
// Render's trimming loop converges).
func encodeResult(format string, r model.Result) ([]byte, error) {
	if strings.EqualFold(format, FormatJSON) {
		return encodeResultJSON(r)
	}
	return encodeResultTSV(r)
}

// encodeResultJSON renders r as one JSON object, matching model.Result's
// own json tags for every field with no model.Cell inside it
// (schema_version, ok, context, notices, artifacts, manifest_path, error -
// all safe to hand to encoding/json.Marshal directly) and, for each
// table's rows, encoding cell by cell through encodeJSONCell so that a
// bigint cell is quoted exactly the way a table artifact quotes it - a
// plain json.Marshal of a []model.Cell holding an int64 would not know
// that distinction and would silently lose precision in the normal case.
func encodeResultJSON(r model.Result) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString(`{"schema_version":`)
	if err := writeJSON(&buf, r.SchemaVersion); err != nil {
		return nil, err
	}
	buf.WriteString(`,"ok":`)
	if err := writeJSON(&buf, r.OK); err != nil {
		return nil, err
	}
	buf.WriteString(`,"context":`)
	if err := writeJSON(&buf, r.Context); err != nil {
		return nil, err
	}
	buf.WriteString(`,"tables":[`)
	for i, t := range r.Tables {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.WriteString(`{"spec":`)
		if err := writeJSON(&buf, t.Spec); err != nil {
			return nil, err
		}
		buf.WriteString(`,"rows":[`)
		for j, row := range t.Rows {
			if j > 0 {
				buf.WriteByte(',')
			}
			rb, err := encodeJSONRow(t.Spec.Columns, row)
			if err != nil {
				return nil, err
			}
			buf.Write(rb)
		}
		buf.WriteString(`],"state":`)
		if err := writeJSON(&buf, t.State); err != nil {
			return nil, err
		}
		buf.WriteString(`,"preview":`)
		if err := writeJSON(&buf, t.Preview); err != nil {
			return nil, err
		}
		buf.WriteByte('}')
	}
	buf.WriteString(`],"notices":`)
	if err := writeJSON(&buf, r.Notices); err != nil {
		return nil, err
	}
	buf.WriteString(`,"artifacts":`)
	if err := writeJSON(&buf, r.Artifacts); err != nil {
		return nil, err
	}
	buf.WriteString(`,"manifest_path":`)
	if err := writeJSON(&buf, r.ManifestPath); err != nil {
		return nil, err
	}
	buf.WriteString(`,"error":`)
	if err := writeJSON(&buf, r.Error); err != nil {
		return nil, err
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

func writeJSON(buf *bytes.Buffer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	buf.Write(b)
	return nil
}

// encodeJSONRow renders one row as a JSON array of naked cell values,
// cell by cell through encodeJSONCell (json.go) so bigint quoting and
// UTF-8 handling match table artifacts exactly.
func encodeJSONRow(cols []model.Column, row []model.Cell) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('[')
	for i, c := range row {
		if i > 0 {
			buf.WriteByte(',')
		}
		sqlType := ""
		if i < len(cols) {
			sqlType = cols[i].SQLType
		}
		enc, _, err := encodeJSONCell(c, sqlType)
		if err != nil {
			return nil, err
		}
		buf.Write(enc)
	}
	buf.WriteByte(']')
	return buf.Bytes(), nil
}

// encodeResultTSV renders r as a flat, tab-separated, line-oriented
// document: one key/value line per scalar metadata field, one line per
// notice and per artifact, then per table a "table" summary line (name
// plus its collection and preview state), a "columns" line, and one "row"
// line per shown row. Every free-text field is escaped exactly the way
// tsv.go escapes a table artifact's column names; row cells already arrive
// escaped from EncodeTSVCell. This layout is internal/output's own - task
// 7 defines the multi-table stdout TSV shape, there being no table
// artifact precedent for more than one table at a time - but it reuses
// tsv.go's cell-level rules rather than inventing a second escaping
// scheme.
func encodeResultTSV(r model.Result) ([]byte, error) {
	var buf bytes.Buffer
	kv := func(key, value string) { appendTSVLine(&buf, key, tsvEscaper.Replace(value)) }

	kv("schema_version", strconv.Itoa(r.SchemaVersion))
	kv("ok", strconv.FormatBool(r.OK))
	kv("server", r.Context.Server)
	kv("database", r.Context.Database)
	kv("principal", r.Context.Principal)
	kv("version", r.Context.Version)
	kv("tls_encryption", r.Context.TLSEncryption)
	kv("certificate_validation", r.Context.CertificateValidation)
	if !r.Context.CollectedAt.IsZero() {
		kv("collected_at", r.Context.CollectedAt.Format(time.RFC3339Nano))
	}
	kv("manifest_path", r.ManifestPath)
	if r.Error != nil {
		kv("error_code", strconv.Itoa(r.Error.Code))
		kv("error_kind", r.Error.Kind)
		kv("error_message", r.Error.Message)
		kv("error_sql_number", strconv.Itoa(int(r.Error.SQLNumber)))
	}
	for _, n := range r.Notices {
		appendTSVLine(&buf, "notice", tsvEscaper.Replace(n.Kind), tsvEscaper.Replace(n.Message), tsvEscaper.Replace(n.Table))
	}
	for _, a := range r.Artifacts {
		appendTSVLine(&buf, "artifact", tsvEscaper.Replace(a.Kind), tsvEscaper.Replace(a.Path), strconv.FormatInt(a.Bytes, 10), strconv.FormatBool(a.Complete))
	}
	for _, t := range r.Tables {
		appendTSVLine(&buf, "table", tsvEscaper.Replace(t.Spec.Name),
			fmt.Sprintf("rows_collected=%d", t.State.RowsCollected),
			fmt.Sprintf("collection_complete=%t", t.State.CollectionComplete),
			fmt.Sprintf("properties_complete=%t", t.State.PropertiesComplete),
			fmt.Sprintf("rows_shown=%d", t.Preview.RowsShown),
			fmt.Sprintf("preview_complete=%t", t.Preview.PreviewComplete),
			fmt.Sprintf("omitted_reasons=%s", strings.Join(t.Preview.OmittedReasons, ",")),
		)
		names := make([]string, len(t.Spec.Columns)+1)
		names[0] = "columns"
		for i, c := range t.Spec.Columns {
			names[i+1] = tsvEscaper.Replace(c.Name)
		}
		appendTSVLine(&buf, names...)
		for _, row := range t.Rows {
			fields := make([]string, len(row)+1)
			fields[0] = "row"
			for i, c := range row {
				fields[i+1] = EncodeTSVCell(c)
			}
			appendTSVLine(&buf, fields...)
		}
	}
	return buf.Bytes(), nil
}

// appendTSVLine writes fields tab-joined and newline-terminated. Callers
// are responsible for escaping any field that needs it (see
// encodeResultTSV): this function does not escape, so it never
// double-escapes a row cell that EncodeTSVCell already escaped.
func appendTSVLine(buf *bytes.Buffer, fields ...string) {
	buf.WriteString(strings.Join(fields, "\t"))
	buf.WriteByte('\n')
}
