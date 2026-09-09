package output

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/rudi-bruchez/argosql/internal/model"
)

// TestOversizedCellIsNotEmpty is the directing test from the task-7 brief,
// copied verbatim. A row whose only cell overflows the byte budget on its
// own must be kept, truncated, never dropped: dropping it would make
// --no-truncate on one big cell look exactly like an empty table, which is
// the confusion this whole task exists to prevent.
func TestOversizedCellIsNotEmpty(t *testing.T) {
	r := model.Result{SchemaVersion: 1, OK: true, Tables: []model.TableResult{{
		Spec:  model.TableSpec{Name: "x", Columns: []model.Column{{Name: "text", SQLType: "nvarchar(max)"}}},
		Rows:  [][]model.Cell{{strings.Repeat("x", 40000)}},
		State: model.Completeness{RowsCollected: 1, CollectionComplete: true, PropertiesComplete: true},
	}}}
	b, err := Render(r, PreviewOptions{Rows: 10, NoTruncate: true, ByteLimit: 32768}, "json")
	if err != nil || !json.Valid(b) || len(b) > 32768 {
		t.Fatalf("bad preview: %v", err)
	}
	if !bytes.Contains(b, []byte("preview_omitted")) {
		t.Fatal("must distinguish omitted from empty")
	}
}

// TestPreviewBudget is the second directing test the brief's red/green
// filter requires by name. It exercises the equal-share/redistribute
// ordering (steps 3-5) across three tables: one of them ("b") only ever
// has a single candidate row, so under a tight shared byte budget the
// room it leaves unused must flow to "a" and "c" rather than sit idle.
func TestPreviewBudget(t *testing.T) {
	// Each row's cell is deliberately long: the point of this test is the
	// division of whole rows across tables, and a short cell would let
	// the few bytes an omitted_reasons entry costs swamp a row's own
	// size, masking the effect this test exists to measure.
	mkTable := func(name string, n int) model.TableResult {
		rows := make([][]model.Cell, n)
		for i := range rows {
			rows[i] = []model.Cell{fmt.Sprintf("row%03d-%s", i, strings.Repeat("x", 40))}
		}
		return model.TableResult{
			Spec:  model.TableSpec{Name: name, Columns: []model.Column{{Name: "v", SQLType: "NVARCHAR(64)"}}},
			Rows:  rows,
			State: model.Completeness{RowsCollected: int64(n), CollectionComplete: true, PropertiesComplete: true},
		}
	}

	type decoded struct {
		Tables []struct {
			Spec struct {
				Name string `json:"name"`
			} `json:"spec"`
			Preview struct {
				RowsShown      int64    `json:"rows_shown"`
				OmittedReasons []string `json:"omitted_reasons"`
			} `json:"preview"`
		} `json:"tables"`
	}
	decode := func(t *testing.T, b []byte) decoded {
		t.Helper()
		var d decoded
		if err := json.Unmarshal(bytes.TrimRight(b, "\n"), &d); err != nil {
			t.Fatalf("decoding render: %v; body=%s", err, b)
		}
		return d
	}
	rowsShown := func(d decoded, name string) int64 {
		for _, tb := range d.Tables {
			if tb.Spec.Name == name {
				return tb.Preview.RowsShown
			}
		}
		return -1
	}
	reasonsOf := func(d decoded, name string) []string {
		for _, tb := range d.Tables {
			if tb.Spec.Name == name {
				return tb.Preview.OmittedReasons
			}
		}
		return nil
	}

	// "small": b has only one row to show, ever. "busy": same shape, but
	// b is just as demanding as a and c, leaving nothing to redistribute.
	small := model.Result{SchemaVersion: 1, OK: true, Tables: []model.TableResult{
		mkTable("a", 5), mkTable("b", 1), mkTable("c", 5),
	}}
	busy := model.Result{SchemaVersion: 1, OK: true, Tables: []model.TableResult{
		mkTable("a", 5), mkTable("b", 5), mkTable("c", 5),
	}}

	zero, err := Render(small, PreviewOptions{Rows: 0, ByteLimit: 1 << 20}, "json")
	if err != nil {
		t.Fatalf("zero-row render: %v", err)
	}
	full, err := Render(small, PreviewOptions{Rows: 5, NoTruncate: true, ByteLimit: 1 << 20}, "json")
	if err != nil {
		t.Fatalf("full render: %v", err)
	}
	// A budget strictly between "nothing shown" and "everything shown":
	// generous enough for more than one row per table, tight enough that
	// not every candidate row can fit.
	limit := len(zero) + (len(full)-len(zero))/2
	opts := PreviewOptions{Rows: 5, NoTruncate: true, ByteLimit: limit}

	smallOut, err := Render(small, opts, "json")
	if err != nil {
		t.Fatalf("small-b render: %v", err)
	}
	if len(smallOut) > limit || !json.Valid(bytes.TrimRight(smallOut, "\n")) {
		t.Fatalf("small-b render: bad output (len=%d, limit=%d)", len(smallOut), limit)
	}
	busyOut, err := Render(busy, opts, "json")
	if err != nil {
		t.Fatalf("busy render: %v", err)
	}
	if len(busyOut) > limit || !json.Valid(bytes.TrimRight(busyOut, "\n")) {
		t.Fatalf("busy render: bad output (len=%d, limit=%d)", len(busyOut), limit)
	}

	smallDec, busyDec := decode(t, smallOut), decode(t, busyOut)

	for _, name := range []string{"a", "b", "c"} {
		if got := rowsShown(smallDec, name); got < 1 {
			t.Fatalf("table %q: rows_shown=%d, the first candidate row of a non-empty table must always be retained", name, got)
		}
	}
	if got := rowsShown(smallDec, "b"); got != 1 {
		t.Fatalf("table b: rows_shown=%d, want 1 (it only ever had one row)", got)
	}
	for _, reason := range reasonsOf(smallDec, "b") {
		if reason == model.ReasonRowsTruncated {
			t.Fatalf("table b shows every row it collected; rows_truncated must not apply")
		}
	}

	// The point of step 5: with "b" needing only one row, "a" and "c"
	// together must get to show at least as many rows as when every
	// table is equally demanding and nothing is left over to hand out.
	aSmall, aBusy := rowsShown(smallDec, "a"), rowsShown(busyDec, "a")
	cSmall, cBusy := rowsShown(smallDec, "c"), rowsShown(busyDec, "c")
	if aSmall < aBusy {
		t.Fatalf("table a: got fewer rows (%d) when b left budget unused than when b was equally busy (%d); leftover was not redistributed", aSmall, aBusy)
	}
	if cSmall < cBusy {
		t.Fatalf("table c: got fewer rows (%d) when b left budget unused than when b was equally busy (%d); leftover was not redistributed", cSmall, cBusy)
	}
	if aSmall+cSmall <= aBusy+cBusy {
		t.Fatalf("redistribution had no measurable effect: a+c shown %d in both scenarios (small: %d, busy: %d)", aSmall+cSmall, aSmall+cSmall, aBusy+cBusy)
	}
}

// TestPreviewExplicitZeroRows checks --preview 0: a deliberate request to
// show nothing must still say so honestly - rows_truncated, not an empty
// table indistinguishable from one that collected nothing.
func TestPreviewExplicitZeroRows(t *testing.T) {
	r := model.Result{SchemaVersion: 1, OK: true, Tables: []model.TableResult{{
		Spec:  model.TableSpec{Name: "t", Columns: []model.Column{{Name: "v", SQLType: "INT"}}},
		Rows:  [][]model.Cell{{int64(1)}, {int64(2)}, {int64(3)}},
		State: model.Completeness{RowsCollected: 3, CollectionComplete: true, PropertiesComplete: true},
	}}}
	b, err := Render(r, PreviewOptions{Rows: 0, ByteLimit: 32768}, "json")
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	var dec struct {
		Tables []struct {
			Rows    [][]any `json:"rows"`
			Preview struct {
				RowsShown       int64    `json:"rows_shown"`
				PreviewComplete bool     `json:"preview_complete"`
				OmittedReasons  []string `json:"omitted_reasons"`
			} `json:"preview"`
		} `json:"tables"`
	}
	if err := json.Unmarshal(bytes.TrimRight(b, "\n"), &dec); err != nil {
		t.Fatalf("decode: %v", err)
	}
	tb := dec.Tables[0]
	if tb.Preview.RowsShown != 0 {
		t.Fatalf("rows_shown=%d, want 0", tb.Preview.RowsShown)
	}
	if len(tb.Rows) != 0 {
		t.Fatalf("rows has %d entries, want 0", len(tb.Rows))
	}
	if tb.Preview.PreviewComplete {
		t.Fatalf("preview_complete=true, want false (3 rows were collected, 0 shown)")
	}
	found := false
	for _, r := range tb.Preview.OmittedReasons {
		if r == model.ReasonRowsTruncated {
			found = true
		}
	}
	if !found {
		t.Fatalf("omitted_reasons=%v, want rows_truncated present: a --preview 0 table must not look like an empty one", tb.Preview.OmittedReasons)
	}
}

// TestPreviewRuneCountNotByteCount checks that the cell-length limit
// counts Unicode code points, not bytes, and never splits a multi-byte
// rune.
func TestPreviewRuneCountNotByteCount(t *testing.T) {
	cell := "héllo 世界 test" // 13 runes, more bytes than runes
	if utf8.RuneCountInString(cell) != 13 {
		t.Fatalf("fixture has %d runes, want 13", utf8.RuneCountInString(cell))
	}
	r := model.Result{SchemaVersion: 1, OK: true, Tables: []model.TableResult{{
		Spec:  model.TableSpec{Name: "t", Columns: []model.Column{{Name: "v", SQLType: "NVARCHAR(50)"}}},
		Rows:  [][]model.Cell{{cell}},
		State: model.Completeness{RowsCollected: 1, CollectionComplete: true, PropertiesComplete: true},
	}}}
	b, err := Render(r, PreviewOptions{Rows: 10, CellLimit: 5, ByteLimit: 32768}, "json")
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !utf8.Valid(b) {
		t.Fatalf("output is not valid UTF-8: a rune must never be split")
	}
	var dec struct {
		Tables []struct {
			Rows    [][]string `json:"rows"`
			Preview struct {
				OmittedReasons []string `json:"omitted_reasons"`
			} `json:"preview"`
		} `json:"tables"`
	}
	if err := json.Unmarshal(bytes.TrimRight(b, "\n"), &dec); err != nil {
		t.Fatalf("decode: %v", err)
	}
	got := dec.Tables[0].Rows[0][0]
	kept, _ := splitMarker(got)
	if utf8.RuneCountInString(kept) != 5 {
		t.Fatalf("kept %d runes, want 5 (got %q)", utf8.RuneCountInString(kept), got)
	}
	found := false
	for _, r := range dec.Tables[0].Preview.OmittedReasons {
		if r == model.ReasonCellTruncated {
			found = true
		}
	}
	if !found {
		t.Fatalf("omitted_reasons=%v, want cell_truncated", dec.Tables[0].Preview.OmittedReasons)
	}
}

// splitMarker recovers the kept prefix of a cell Render truncated, for
// assertions that only care about the kept content.
func splitMarker(s string) (kept string, hadMarker bool) {
	if i := strings.LastIndex(s, "…[+"); i >= 0 {
		return s[:i], true
	}
	return s, false
}

// TestPreviewSecondReadFromArtifact exercises the case where
// result.Tables[i].Rows is empty and the candidates must come from the
// "table" artifact Finish recorded in result.Artifacts, decoded with the
// same machinery task 5 built for artifacts - never an in-memory shortcut.
func TestPreviewSecondReadFromArtifact(t *testing.T) {
	spec := model.TableSpec{Name: "events", Columns: []model.Column{
		{Name: "id", SQLType: "INT"}, {Name: "label", SQLType: "NVARCHAR(50)"},
	}}
	dir := t.TempDir()
	path := dir + "/events.tsv"
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create artifact: %v", err)
	}
	enc, err := NewTableEncoder(f, FormatTSV, spec)
	if err != nil {
		t.Fatalf("NewTableEncoder: %v", err)
	}
	const total = 7
	for i := 0; i < total; i++ {
		if err := enc.WriteRow([]model.Cell{int64(i), fmt.Sprintf("item-%d", i)}); err != nil {
			t.Fatalf("WriteRow: %v", err)
		}
	}
	if err := enc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close file: %v", err)
	}

	r := model.Result{
		SchemaVersion: 1,
		OK:            true,
		Tables: []model.TableResult{{
			Spec:  spec,
			State: model.Completeness{RowsCollected: total, CollectionComplete: true, PropertiesComplete: true},
		}},
		Artifacts: []model.Artifact{{Kind: "table", Path: path, Bytes: 0, Complete: true}},
	}

	b, err := Render(r, PreviewOptions{Rows: 3, NoTruncate: true, ByteLimit: 32768}, "json")
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	var dec struct {
		Tables []struct {
			Rows    [][]any `json:"rows"`
			Preview struct {
				RowsShown int64 `json:"rows_shown"`
			} `json:"preview"`
		} `json:"tables"`
	}
	if err := json.Unmarshal(bytes.TrimRight(b, "\n"), &dec); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := dec.Tables[0].Preview.RowsShown; got != 3 {
		t.Fatalf("rows_shown=%d, want 3 (Rows option capping a 7-row artifact)", got)
	}
	if len(dec.Tables[0].Rows) != 3 {
		t.Fatalf("rows array has %d entries, want 3", len(dec.Tables[0].Rows))
	}
	if dec.Tables[0].Rows[0][1] != "item-0" {
		t.Fatalf("first row's label = %v, want item-0 (rows must come back in collection order from the artifact)", dec.Tables[0].Rows[0][1])
	}
}

// TestPreviewMetadataOverflowFallback checks the last-resort tier: when
// metadata alone (here, an oversized manifest path) cannot fit ByteLimit,
// Render must fall back to the fixed model.FallbackError literal, never
// to a degenerate model.Result, and never embed the oversized field.
func TestPreviewMetadataOverflowFallback(t *testing.T) {
	r := model.Result{
		SchemaVersion: 1,
		OK:            false,
		ManifestPath:  strings.Repeat("p", 5000),
		Error:         &model.PublicError{Code: 7, Kind: "collection_limit", Message: "row limit exceeded"},
	}
	b, err := Render(r, PreviewOptions{Rows: 10, ByteLimit: 64}, "json")
	if err == nil {
		t.Fatalf("want a non-nil error once even the metadata-only response cannot fit")
	}
	var pub *model.PublicError
	if !errors.As(err, &pub) || pub.Code != 6 {
		t.Fatalf("want a code-6 error, got %v", err)
	}
	var fe model.FallbackError
	if uerr := json.Unmarshal(bytes.TrimRight(b, "\n"), &fe); uerr != nil {
		t.Fatalf("fallback literal is not valid JSON: %v", uerr)
	}
	if fe.OK {
		t.Fatalf("fallback literal must keep ok:false")
	}
	if fe.Error == nil || fe.Error.Code != 6 {
		t.Fatalf("fallback literal error = %+v, want code 6", fe.Error)
	}
	if bytes.Contains(b, []byte(r.ManifestPath)) {
		t.Fatalf("fallback literal must never embed the oversized manifest path")
	}
	if len(b) > 256 {
		t.Fatalf("fallback literal should be tiny; got %d bytes: %s", len(b), b)
	}
}

// TestPreviewKeepsErrorAndPartialArtifacts checks that a failed
// collection run (exit code 7, some artifacts left incomplete) still
// renders a valid, bounded preview of whatever it has - Render's own job
// is display, not deciding whether the run succeeded.
func TestPreviewKeepsErrorAndPartialArtifacts(t *testing.T) {
	r := model.Result{
		SchemaVersion: 1,
		OK:            false,
		Tables: []model.TableResult{{
			Spec:  model.TableSpec{Name: "big", Columns: []model.Column{{Name: "v", SQLType: "INT"}}},
			Rows:  [][]model.Cell{{int64(1)}, {int64(2)}},
			State: model.Completeness{RowsCollected: 2, CollectionComplete: false, PropertiesComplete: true},
		}},
		Artifacts: []model.Artifact{
			{Kind: "table", Path: "/tmp/run/big.json", Bytes: 2048, Complete: false},
		},
		Error: &model.PublicError{Code: 7, Kind: "collection_limit", Message: "row limit of 10000 exceeded"},
	}
	b, err := Render(r, PreviewOptions{Rows: 10, NoTruncate: true, ByteLimit: 32768}, "json")
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	var dec struct {
		OK    bool `json:"ok"`
		Error struct {
			Code int `json:"code"`
		} `json:"error"`
		Artifacts []struct {
			Complete bool `json:"complete"`
		} `json:"artifacts"`
		Tables []struct {
			Preview struct {
				RowsShown int64 `json:"rows_shown"`
			} `json:"preview"`
		} `json:"tables"`
	}
	if err := json.Unmarshal(bytes.TrimRight(b, "\n"), &dec); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if dec.OK {
		t.Fatalf("ok=true, want false")
	}
	if dec.Error.Code != 7 {
		t.Fatalf("error.code=%d, want 7", dec.Error.Code)
	}
	if len(dec.Artifacts) != 1 || dec.Artifacts[0].Complete {
		t.Fatalf("artifacts=%v, want one incomplete entry preserved", dec.Artifacts)
	}
	if dec.Tables[0].Preview.RowsShown != 2 {
		t.Fatalf("rows_shown=%d, want 2: an error elsewhere must not suppress an otherwise-good preview", dec.Tables[0].Preview.RowsShown)
	}
}

// TestPreviewGoldenFixtureBothFormats locks both formats' rendering of the
// identical small response, so a later accidental change to either
// encoding shows up as a diff here rather than silently drifting.
func TestPreviewGoldenFixtureBothFormats(t *testing.T) {
	r := model.Result{
		SchemaVersion: 1,
		OK:            true,
		Context:       model.ContextInfo{Server: "srv", Database: "db"},
		Tables: []model.TableResult{{
			Spec:  model.TableSpec{Name: "t", Columns: []model.Column{{Name: "id", SQLType: "INT"}}},
			Rows:  [][]model.Cell{{int64(1)}, {int64(2)}},
			State: model.Completeness{RowsCollected: 2, CollectionComplete: true, PropertiesComplete: true},
		}},
	}
	opts := PreviewOptions{Rows: 10, NoTruncate: true, ByteLimit: 32768}

	jsonOut, err := Render(r, opts, "json")
	if err != nil {
		t.Fatalf("json render: %v", err)
	}
	const wantJSON = `{"schema_version":1,"ok":true,"context":{"server":"srv","database":"db","principal":"","version":"","tls_encryption":"","certificate_validation":"","collected_at":"0001-01-01T00:00:00Z"},"tables":[{"spec":{"name":"t","columns":[{"name":"id","sql_type":"INT"}]},"rows":[[1],[2]],"state":{"rows_collected":2,"collection_complete":true,"properties_complete":true},"preview":{"rows_shown":2,"preview_complete":true,"omitted_reasons":[]}}],"notices":null,"artifacts":null,"manifest_path":"","error":null}` + "\n"
	if string(jsonOut) != wantJSON {
		t.Fatalf("json render mismatch:\ngot:  %s\nwant: %s", jsonOut, wantJSON)
	}

	tsvOut, err := Render(r, opts, "tsv")
	if err != nil {
		t.Fatalf("tsv render: %v", err)
	}
	const wantTSV = "schema_version\t1\nok\ttrue\nserver\tsrv\ndatabase\tdb\nprincipal\t\nversion\t\ntls_encryption\t\ncertificate_validation\t\nmanifest_path\t\ntable\tt\trows_collected=2\tcollection_complete=true\tproperties_complete=true\trows_shown=2\tpreview_complete=true\tomitted_reasons=\ncolumns\tid\nrow\t1\nrow\t2\n"
	if string(tsvOut) != wantTSV {
		t.Fatalf("tsv render mismatch:\ngot:  %q\nwant: %q", tsvOut, wantTSV)
	}
}

// TestPreviewByteLimitCountsTrailingNewline pins ByteLimit to exactly the
// natural JSON encoding's length, with no byte of slack left for the
// trailing newline Render always appends. If the budget check ever
// stopped counting that newline, this is the exact boundary where the
// returned bytes would exceed ByteLimit by one without the check ever
// noticing: total == ByteLimit would look like a pass, and the one byte
// added afterward would never be re-measured.
func TestPreviewByteLimitCountsTrailingNewline(t *testing.T) {
	// The cell is a string, not a bare int: if the off-by-one forces a
	// shrink, there is real content to shrink, so the correct outcome is
	// a truncated row, not a fallback forced by an unrelated degenerate
	// case (a table whose zero-row state, reason text included, happens
	// to cost more than its one tiny row - a real possibility this test
	// must not trip over while aiming at the newline count specifically).
	r := model.Result{SchemaVersion: 1, OK: true, Tables: []model.TableResult{{
		Spec:  model.TableSpec{Name: "t", Columns: []model.Column{{Name: "v", SQLType: "NVARCHAR(64)"}}},
		Rows:  [][]model.Cell{{strings.Repeat("x", 50)}},
		State: model.Completeness{RowsCollected: 1, CollectionComplete: true, PropertiesComplete: true},
	}}}
	opts := PreviewOptions{Rows: 10, NoTruncate: true, ByteLimit: 1 << 20}
	generous, err := Render(r, opts, "json")
	if err != nil {
		t.Fatalf("generous render: %v", err)
	}
	if generous[len(generous)-1] != '\n' {
		t.Fatalf("generous render does not end in a newline")
	}
	natural := len(generous) - 1 // the encoding's own length, newline excluded

	opts.ByteLimit = natural
	b, err := Render(r, opts, "json")
	if err != nil {
		t.Fatalf("exact-fit render: %v", err)
	}
	if len(b) > opts.ByteLimit {
		t.Fatalf("render is %d bytes, exceeds ByteLimit %d: the trailing newline was not counted against the budget", len(b), opts.ByteLimit)
	}
}
