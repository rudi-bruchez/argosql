package plan

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"

	"github.com/rudi-bruchez/argosql/internal/model"
)

// buildPlanXML is a synthetic, ShowPlan-shaped fixture: numOperators
// self-closing RelOp elements (plus one root, NodeId 0, whose own
// EstimatedTotalSubtreeCost is deliberately the highest of all so it
// always survives the top-operatorCap cut), numRefs distinct Object
// references (plus one deliberate duplicate of the first, to prove
// de-duplication), and one Warnings block carrying warningTypes as its
// direct children.
//
// It is not a byte-real showplan document (no real nesting, no
// namespace) - Summarize's own token-by-token algorithm does not
// depend on nesting depth or namespace to find RelOp/Object/Warnings,
// only on element names and, for Warnings' own children, direct
// parent/child depth, both of which this flat shape reproduces
// faithfully. Real-shaped documents from an actual engine are covered
// separately, in tests/integration/plan_test.go.
//
// Operator costs run downward from numOperators: RelOp i (i>=1) costs
// numOperators-i, so the numOperatorCap highest-cost children are
// always exactly NodeId 1..operatorCap-1 plus the root - deterministic,
// assertable top-operatorCap membership for any numOperators.
//
// buildPlanStmtCost is StmtSimple's own StatementSubTreeCost attribute
// in every fixture this function builds: fix 1's A2 requires
// Summarize to read this value, never any RelOp's own cost, so it is
// deliberately distinct from every RelOp cost buildPlanXML ever
// generates - a test asserting on estimated_cost proves the fix is
// reading the right attribute, not one that happens to coincide.
const buildPlanStmtCost = 424242.0

func buildPlanXML(numOperators, numRefs int, warningTypes []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, `<ShowPlanXML><BatchSequence><Batch><Statements><StmtSimple StatementSubTreeCost="%v"><QueryPlan>`, buildPlanStmtCost)
	fmt.Fprintf(&b, `<RelOp NodeId="0" PhysicalOp="Root" LogicalOp="Root" EstimatedTotalSubtreeCost="%d"/>`, numOperators+1000)
	for i := 1; i <= numOperators; i++ {
		fmt.Fprintf(&b, `<RelOp NodeId="%d" PhysicalOp="Op%d" LogicalOp="L%d" EstimatedTotalSubtreeCost="%d"/>`, i, i, i, numOperators-i)
	}
	for i := 0; i < numRefs; i++ {
		fmt.Fprintf(&b, `<Object Database="[AppDB]" Schema="[dbo]" Table="[T%d]" Index="[IX_T%d]"/>`, i, i)
	}
	if numRefs > 0 {
		// A repeat of the very first reference: proves distinct-reference
		// de-duplication does not count a repeat toward referenceCap.
		b.WriteString(`<Object Database="[AppDB]" Schema="[dbo]" Table="[T0]" Index="[IX_T0]"/>`)
	}
	b.WriteString(`<Warnings>`)
	for _, wt := range warningTypes {
		fmt.Fprintf(&b, `<%s/>`, wt)
	}
	b.WriteString(`</Warnings>`)
	b.WriteString(`</QueryPlan></StmtSimple></Statements></Batch></BatchSequence></ShowPlanXML>`)
	return b.String()
}

// TestSummarizeStatementIdentifiesSourceAndCost proves the statement
// table's own two facts (design spec, line 87: "identify the source
// as Query Store compiled plan XML... the statement's estimated
// cost"): the source is the fixed literal, and estimated_cost equals
// StmtSimple's own StatementSubTreeCost attribute - never a RelOp's
// own cost (fix 1's A2), and never a sum this function computes itself
// (design spec: "do not sum overlapping subtree costs").
func TestSummarizeStatementIdentifiesSourceAndCost(t *testing.T) {
	doc := buildPlanXML(5, 0, nil)
	sink := &fakeSink{}
	if err := Summarize(strings.NewReader(doc), sink); err != nil {
		t.Fatal(err)
	}
	stmt := sink.table(StatementTable.Name)
	if stmt == nil || len(stmt.rows) != 1 {
		t.Fatalf("statement table: got %+v", stmt)
	}
	if !stmt.propertiesComplete {
		t.Fatal("properties_complete should be true: StatementSubTreeCost is present")
	}
	row := stmt.rows[0]
	if row[0] != summarySource {
		t.Fatalf("source: got %#v, want %q", row[0], summarySource)
	}
	cost, ok := row[1].(float64)
	if !ok || cost != buildPlanStmtCost {
		t.Fatalf("estimated_cost: got %#v, want %v (StmtSimple's own StatementSubTreeCost, not a RelOp's)", row[1], buildPlanStmtCost)
	}
}

// TestSummarizeStatementCostUnavailableWhenAttributeMissing is fix 1's
// A2 own negative control, built directly from the reviewer's
// synthetic repro: a StmtSimple with NO StatementSubTreeCost attribute
// must report estimated_cost unavailable (nil, properties_complete
// false), never silently borrow a descendant RelOp's own cost -
// measured against the previous implementation with
// "<StmtSimple StatementSubTreeCost=\"12\">...<RelOp NodeId=\"0\">
// <RelOp NodeId=\"1\" EstimatedTotalSubtreeCost=\"3\"/></RelOp>...":
// the previous code wrote 3 (a descendant's cost) instead of 12.
func TestSummarizeStatementCostUnavailableWhenAttributeMissing(t *testing.T) {
	doc := `<ShowPlanXML><QueryPlan><StmtSimple>` +
		`<RelOp NodeId="0" EstimatedTotalSubtreeCost="99">` +
		`<RelOp NodeId="1" EstimatedTotalSubtreeCost="3"/></RelOp></StmtSimple></QueryPlan></ShowPlanXML>`
	sink := &fakeSink{}
	if err := Summarize(strings.NewReader(doc), sink); err != nil {
		t.Fatal(err)
	}
	stmt := sink.table(StatementTable.Name)
	if stmt == nil || len(stmt.rows) != 1 {
		t.Fatalf("statement table: got %+v", stmt)
	}
	if stmt.rows[0][1] != nil {
		t.Fatalf("estimated_cost: got %#v, want nil (no StatementSubTreeCost attribute anywhere)", stmt.rows[0][1])
	}
	if stmt.propertiesComplete {
		t.Fatal("properties_complete should be false: no StatementSubTreeCost attribute was found")
	}
	if n := sink.noticeWithKind("statement_cost_unavailable"); n == nil {
		t.Fatal("no statement_cost_unavailable notice emitted")
	}
}

// TestSummarizeReadsRealStatementCostFromStmtSimple is the reviewer's
// own repro, byte for byte: a StmtSimple whose StatementSubTreeCost
// (12) differs from a descendant RelOp's own EstimatedTotalSubtreeCost
// (3) - the summary must write 12, never 3.
func TestSummarizeReadsRealStatementCostFromStmtSimple(t *testing.T) {
	doc := `<ShowPlanXML><StmtSimple StatementSubTreeCost="12"><QueryPlan>` +
		`<RelOp NodeId="0"><RelOp NodeId="1" EstimatedTotalSubtreeCost="3"/></RelOp>` +
		`</QueryPlan></StmtSimple></ShowPlanXML>`
	sink := &fakeSink{}
	if err := Summarize(strings.NewReader(doc), sink); err != nil {
		t.Fatal(err)
	}
	stmt := sink.table(StatementTable.Name)
	cost, ok := stmt.rows[0][1].(float64)
	if !ok || cost != 12 {
		t.Fatalf("estimated_cost: got %#v, want 12 (StatementSubTreeCost, not the descendant RelOp's 3)", stmt.rows[0][1])
	}
}

// TestSummarizeOperatorsCappedAndOrdered is this task's own "10,000
// opérateurs, ordre stable coût/NodeId" case (brief's Vert checklist):
// a document with 10,000 operators, well over 80 KiB, must retain at
// most operatorCap rows, and those rows must be exactly the
// operatorCap highest-cost ones (root plus NodeId 1..operatorCap-1 by
// construction), in descending cost order.
func TestSummarizeOperatorsCappedAndOrdered(t *testing.T) {
	const n = 10000
	doc := buildPlanXML(n, 0, nil)
	if len(doc) < 80*1024 {
		t.Fatalf("fixture too small: %d bytes, want >= 80KiB", len(doc))
	}

	sink := &fakeSink{}
	if err := Summarize(strings.NewReader(doc), sink); err != nil {
		t.Fatal(err)
	}
	ops := sink.table(OperatorsTable.Name)
	if ops == nil || len(ops.rows) != operatorCap {
		t.Fatalf("operators retained: got %d rows, want exactly %d", len(ops.rows), operatorCap)
	}
	if !ops.collectionComplete {
		t.Fatal("operators table: the operatorCap cap is this summary's own declared shape, never a reported collection limit")
	}
	wantNodeIDs := []int64{0, 1, 2, 3, 4}
	for i, row := range ops.rows {
		nodeID, ok := row[0].(int64)
		if !ok || nodeID != wantNodeIDs[i] {
			t.Fatalf("row %d node_id: got %#v, want %d (descending cost order)", i, row[0], wantNodeIDs[i])
		}
	}
}

// TestSummarizeOperatorTieBreak is design spec line 89's own ordering
// rule, exercised directly: two RelOps sharing the exact same cost,
// with the heap already full of five strictly better candidates plus
// the tied pair contending for the last slot - the one seen EARLIER in
// document order (lower NodeId here, since this fixture assigns NodeId
// in traversal order) must be the one retained.
func TestSummarizeOperatorTieBreak(t *testing.T) {
	var b strings.Builder
	b.WriteString(`<ShowPlanXML><QueryPlan>`)
	// Four operators strictly better than the tied pair, filling four of
	// the five slots.
	for i := 1; i <= 4; i++ {
		fmt.Fprintf(&b, `<RelOp NodeId="%d" EstimatedTotalSubtreeCost="%d"/>`, i, 100+i)
	}
	// The tied pair: NodeId 5 (encountered first) and NodeId 6 (encountered
	// second), identical cost. Only one fifth slot remains.
	b.WriteString(`<RelOp NodeId="5" EstimatedTotalSubtreeCost="50"/>`)
	b.WriteString(`<RelOp NodeId="6" EstimatedTotalSubtreeCost="50"/>`)
	b.WriteString(`</QueryPlan></ShowPlanXML>`)

	sink := &fakeSink{}
	if err := Summarize(strings.NewReader(b.String()), sink); err != nil {
		t.Fatal(err)
	}
	ops := sink.table(OperatorsTable.Name)
	if ops == nil || len(ops.rows) != operatorCap {
		t.Fatalf("operators retained: got %d rows, want %d", len(ops.rows), operatorCap)
	}
	var sawNode5, sawNode6 bool
	for _, row := range ops.rows {
		switch row[0].(int64) {
		case 5:
			sawNode5 = true
		case 6:
			sawNode6 = true
		}
	}
	if !sawNode5 || sawNode6 {
		t.Fatalf("tie-break: want NodeId 5 (earlier traversal order) kept over NodeId 6, got rows %+v", ops.rows)
	}
}

// TestSummarizeReferencesCappedAndTruncated is the brief's own "plus
// de 100 références" case, and fix 1's A3: 150 distinct Object
// references (plus one deliberate duplicate) must retain exactly
// referenceCap distinct rows, mark the references table incomplete
// (which internal/output.Render turns into
// omitted_reasons=[collection_limit] automatically - this project's
// own closed vocabulary), emit a notice pointing at the raw artifact,
// AND return a code-7 error: design spec line 101 states "incomplete
// collection yields code 7" without exception, and a truncated summary
// table must not be indistinguishable, at the exit code, from a
// complete one.
func TestSummarizeReferencesCappedAndTruncated(t *testing.T) {
	doc := buildPlanXML(1, 150, nil)
	sink := &fakeSink{}
	err := Summarize(strings.NewReader(doc), sink)
	var pub *model.PublicError
	if !errors.As(err, &pub) {
		t.Fatalf("Summarize: error is not a *model.PublicError: %v", err)
	}
	if pub.Code != 7 {
		t.Fatalf("code: got %d, want 7", pub.Code)
	}
	refs := sink.table(ReferencesTable.Name)
	if refs == nil || len(refs.rows) != referenceCap {
		t.Fatalf("references retained: got %d rows, want exactly %d", len(refs.rows), referenceCap)
	}
	if refs.collectionComplete {
		t.Fatal("references table: capped list must be reported incomplete (collection_limit)")
	}
	if !refs.propertiesComplete {
		t.Fatal("references table: properties_complete should stay true - truncation is a collection fact, not a property one")
	}
	if n := sink.noticeWithKind("plan_summary_truncated"); n == nil {
		t.Fatal("no truncation notice emitted for references")
	}
}

// TestSummarizeReferencesUnderCapStaysComplete is
// TestSummarizeReferencesCappedAndTruncated's negative control: well
// under referenceCap distinct references must report the table
// complete, with no truncation notice.
func TestSummarizeReferencesUnderCapStaysComplete(t *testing.T) {
	doc := buildPlanXML(1, 3, nil)
	sink := &fakeSink{}
	if err := Summarize(strings.NewReader(doc), sink); err != nil {
		t.Fatal(err)
	}
	refs := sink.table(ReferencesTable.Name)
	// buildPlanXML always adds one duplicate of the first reference on
	// top of numRefs distinct ones - de-duplication must not count it.
	if refs == nil || len(refs.rows) != 3 {
		t.Fatalf("references retained: got %d rows, want exactly 3 (the duplicate must not be counted twice)", len(refs.rows))
	}
	if !refs.collectionComplete {
		t.Fatal("references table under the cap should be reported complete")
	}
	if n := sink.noticeWithKind("plan_summary_truncated"); n != nil {
		t.Fatalf("unexpected truncation notice under the cap: %+v", n)
	}
}

// TestSummarizeWarningsUnknownTypeAndCap covers the brief's own
// "warning inconnu" case (an unrecognized Warnings child is still
// captured, never dropped as "unsupported" - design spec: "Accept
// unknown elements") together with the warningCap truncation path, the
// same shape as the references cap test above.
func TestSummarizeWarningsUnknownTypeAndCap(t *testing.T) {
	types := []string{"ColumnsWithNoStatistics", "SomeCompletelyUnknownWarningType"}
	for i := 0; i < warningCap+10; i++ {
		types = append(types, fmt.Sprintf("Warn%d", i))
	}
	doc := buildPlanXML(1, 0, types)
	sink := &fakeSink{}
	err := Summarize(strings.NewReader(doc), sink)
	var pub *model.PublicError
	if !errors.As(err, &pub) {
		t.Fatalf("Summarize: error is not a *model.PublicError: %v", err)
	}
	if pub.Code != 7 {
		t.Fatalf("code: got %d, want 7 (fix 1's A3: incomplete collection yields code 7)", pub.Code)
	}
	warn := sink.table(WarningsTable.Name)
	if warn == nil || len(warn.rows) != warningCap {
		t.Fatalf("warnings retained: got %d rows, want exactly %d", len(warn.rows), warningCap)
	}
	if warn.collectionComplete {
		t.Fatal("warnings table: capped list must be reported incomplete (collection_limit)")
	}
	foundKnown, foundUnknown := false, false
	for _, row := range warn.rows {
		switch row[0].(string) {
		case "ColumnsWithNoStatistics":
			foundKnown = true
		case "SomeCompletelyUnknownWarningType":
			foundUnknown = true
		}
	}
	if !foundKnown || !foundUnknown {
		t.Fatalf("expected both a known and an unrecognized warning type retained (order-of-arrival, both before the cap): known=%v unknown=%v", foundKnown, foundUnknown)
	}
}

// TestSummarizeWarningsAttributeFormAndNestedChildrenInSameDocument
// is fix 1's A1, exercised precisely against both real-engine shapes
// the reviewer measured: <Warnings NoJoinPredicate="1"/> (an
// attribute-form warning, dropped entirely by the previous
// implementation, which read only Warnings' own children) together,
// in the SAME document, with a warning that carries a real nested
// descendant (<ColumnsWithNoStatistics><ColumnReference/></...>) -
// the depth guard (inWarnings && depth == warningsDepth+1) must keep
// treating that descendant as part of its own parent warning, never
// as a second, separate warning row, exactly as the brief warns: this
// project's own fixtures before this fix only ever built
// self-closing, childless warnings, which is why nothing caught a
// regression here.
func TestSummarizeWarningsAttributeFormAndNestedChildrenInSameDocument(t *testing.T) {
	doc := `<ShowPlanXML><QueryPlan><RelOp NodeId="0" EstimatedTotalSubtreeCost="1"/>` +
		`<Warnings NoJoinPredicate="1"><ColumnsWithNoStatistics><ColumnReference Column="[x]"/></ColumnsWithNoStatistics></Warnings>` +
		`</QueryPlan></ShowPlanXML>`
	sink := &fakeSink{}
	if err := Summarize(strings.NewReader(doc), sink); err != nil {
		t.Fatal(err)
	}
	warn := sink.table(WarningsTable.Name)
	if warn == nil {
		t.Fatal("no warnings table written")
	}
	// Exactly two rows: the Warnings element's own attribute, and its
	// one direct child - never a third row for ColumnReference, the
	// grandchild the depth guard must keep excluded.
	if len(warn.rows) != 2 {
		t.Fatalf("warnings retained: got %d rows, want exactly 2: %+v", len(warn.rows), warn.rows)
	}
	var sawAttr, sawChild bool
	for _, row := range warn.rows {
		switch row[0].(string) {
		case "NoJoinPredicate":
			sawAttr = true
			if row[1] != "1" {
				t.Fatalf("NoJoinPredicate detail: got %#v, want %q", row[1], "1")
			}
		case "ColumnsWithNoStatistics":
			sawChild = true
		case "ColumnReference":
			t.Fatalf("ColumnReference (a grandchild of Warnings) must not become its own warning row: %+v", warn.rows)
		}
	}
	if !sawAttr {
		t.Fatalf("Warnings' own NoJoinPredicate attribute was dropped: %+v", warn.rows)
	}
	if !sawChild {
		t.Fatalf("Warnings' own ColumnsWithNoStatistics child was dropped: %+v", warn.rows)
	}
}

// TestReferenceKeyDoesNotCollideOnDelimitedCharacters is fix 1's A6:
// two distinct references whose Schema/Table split differently across
// a "|" character must never share a dedup key, and a doubled "]]"
// inside a delimited identifier must decode to a single "]", never
// stay doubled.
func TestReferenceKeyDoesNotCollideOnDelimitedCharacters(t *testing.T) {
	a := referenceRow{database: "D", schema: "a|b", table: "c", index: ""}
	b := referenceRow{database: "D", schema: "a", table: "b|c", index: ""}
	if referenceKey(a) == referenceKey(b) {
		t.Fatalf("referenceKey collides on a delimiter character: %q", referenceKey(a))
	}

	if got := trimBrackets("[a]]b]"); got != "a]b" {
		t.Fatalf("trimBrackets: got %q, want %q", got, "a]b")
	}
}

// TestSummarizeReferencesDistinctAcrossDelimiters is
// TestReferenceKeyDoesNotCollideOnDelimitedCharacters proven through
// the real parsing path: two Object elements whose Schema/Table split
// differently across a "|" must both survive as distinct rows.
func TestSummarizeReferencesDistinctAcrossDelimiters(t *testing.T) {
	doc := `<ShowPlanXML><QueryPlan>` +
		`<Object Database="[D]" Schema="[a|b]" Table="[c]"/>` +
		`<Object Database="[D]" Schema="[a]" Table="[b|c]"/>` +
		`</QueryPlan></ShowPlanXML>`
	sink := &fakeSink{}
	if err := Summarize(strings.NewReader(doc), sink); err != nil {
		t.Fatal(err)
	}
	refs := sink.table(ReferencesTable.Name)
	if refs == nil || len(refs.rows) != 2 {
		t.Fatalf("references retained: got %+v, want exactly 2 distinct rows", refs)
	}
}

// TestSummarizeRejectsMultipleRootElements is fix 1's A9, the
// reviewer's own repro: two sibling root elements are not a
// well-formed XML document, even though encoding/xml.Decoder.Token()
// reaches io.EOF cleanly over them - Summarize must reject this with
// code 5, the same as any other malformed document.
func TestSummarizeRejectsMultipleRootElements(t *testing.T) {
	doc := `<ShowPlanXML/><ShowPlanXML/>`
	sink := &fakeSink{}
	err := Summarize(strings.NewReader(doc), sink)
	var pub *model.PublicError
	if !errors.As(err, &pub) {
		t.Fatalf("multiple root elements: want a *model.PublicError, got %v", err)
	}
	if pub.Code != 5 {
		t.Fatalf("code: got %d, want 5", pub.Code)
	}
}

// TestSummarizeUnknownTopLevelElementIgnored proves an element this
// package does not recognize, sitting outside any Warnings block, is
// silently ignored rather than causing an error or polluting any table
// (design spec, line 89: "Accept unknown elements").
func TestSummarizeUnknownTopLevelElementIgnored(t *testing.T) {
	doc := `<ShowPlanXML><QueryPlan><SomeFutureShowplanFeature Foo="1"/>` +
		`<RelOp NodeId="0" EstimatedTotalSubtreeCost="1"/></QueryPlan></ShowPlanXML>`
	sink := &fakeSink{}
	if err := Summarize(strings.NewReader(doc), sink); err != nil {
		t.Fatalf("unknown element should not fail the summary: %v", err)
	}
	ops := sink.table(OperatorsTable.Name)
	if ops == nil || len(ops.rows) != 1 {
		t.Fatalf("operators: got %+v, want exactly the one real RelOp", ops)
	}
}

// TestSummarizeMalformedXMLReturnsCode5 is the brief's own "élément
// malformed" case: an unbalanced document must fail with code 5
// (design spec, line 89: "reject malformed XML with code 5").
func TestSummarizeMalformedXMLReturnsCode5(t *testing.T) {
	doc := `<ShowPlanXML><QueryPlan><RelOp NodeId="0" EstimatedTotalSubtreeCost="1"></NotTheSameTag></ShowPlanXML>`
	sink := &fakeSink{}
	err := Summarize(strings.NewReader(doc), sink)
	if err == nil {
		t.Fatal("malformed XML: want an error, got nil")
	}
	var pub *model.PublicError
	if !errors.As(err, &pub) {
		t.Fatalf("malformed XML error is not a *model.PublicError: %v", err)
	}
	if pub.Code != 5 {
		t.Fatalf("code: got %d, want 5", pub.Code)
	}
}

// TestSummarizeBoundedRetentionAcrossGrowingDocumentSize is this
// task's own load-bearing proof against "un résumé à cinq lignes
// produit par un parseur qui a chargé tout le document satisfait
// l'assertion et viole la clause" (this task's dispatch, quoting
// design spec line 89): growing the document 200x (100 operators/refs/
// warnings to 20,000) must produce ZERO growth in what any of the four
// tables retains - operators, references and warnings each stay at or
// under their fixed cap regardless of N.
func TestSummarizeBoundedRetentionAcrossGrowingDocumentSize(t *testing.T) {
	for _, n := range []int{100, 20000} {
		types := []string{"ColumnsWithNoStatistics"}
		for i := 0; i < 150; i++ {
			types = append(types, fmt.Sprintf("Warn%d", i))
		}
		doc := buildPlanXML(n, 150, types)
		sink := &fakeSink{}
		// 150 references and 151 warnings always exceed their caps
		// here, so Summarize always returns fix 1's A3 code-7 error;
		// this test is about retention bounds, not the exit code, so
		// it only requires that error to be exactly the truncation one.
		err := Summarize(strings.NewReader(doc), sink)
		var pub *model.PublicError
		if !errors.As(err, &pub) || pub.Code != 7 {
			t.Fatalf("n=%d: want a code-7 truncation error, got %v", n, err)
		}
		if ops := sink.table(OperatorsTable.Name); ops == nil || len(ops.rows) > operatorCap {
			t.Fatalf("n=%d: operators retained %d rows, want <= %d", n, len(ops.rows), operatorCap)
		}
		if refs := sink.table(ReferencesTable.Name); refs == nil || len(refs.rows) > referenceCap {
			t.Fatalf("n=%d: references retained %d rows, want <= %d", n, len(refs.rows), referenceCap)
		}
		if warn := sink.table(WarningsTable.Name); warn == nil || len(warn.rows) > warningCap {
			t.Fatalf("n=%d: warnings retained %d rows, want <= %d", n, len(warn.rows), warningCap)
		}
	}
}

// TestSummarizeDoesNotHoldWholeDocumentInMemory is this task's own
// instrumentation of the parser's own memory footprint, as the brief
// requires explicitly ("instrumenter... les allocations pour prouver
// qu'aucune représentation complète du document n'est construite" -
// not decorative, since the output-shape tests above would stay green
// even against a full xml.Unmarshal-into-a-tree implementation that
// only truncates its RESULT to five/100/100 rows afterwards).
//
// It samples runtime.MemStats.HeapAlloc from a second goroutine WHILE
// Summarize is running against a 20,000-operator, 1MB+ fixture, and
// asserts the observed peak growth stays within a small, generous
// multiple of the document's own byte size. A genuine whole-document
// DOM (one Go value - struct, slice, or attribute map - per element,
// held simultaneously for the call's whole duration) costs several
// times the raw byte size in Go's own struct/pointer/interface
// overhead; this bounded, token-by-token parser's own working set is a
// handful of fixed-size slices plus whatever short-lived garbage a
// single xml.Decoder.Token() call produces, which GC can reclaim
// between samples.
//
// This measurement is necessarily coarse - GC timing and allocator
// behavior are not fully deterministic - which is why the bound is
// generous and why TestSummarizeBoundedRetentionAcrossGrowingDocumentSize
// above, not this test, is the primary, exact evidence; this one is a
// second, independent signal specifically against a DOM-shaped
// regression.
func TestSummarizeDoesNotHoldWholeDocumentInMemory(t *testing.T) {
	doc := buildPlanXML(20000, 0, nil)
	if len(doc) < 1024*1024 {
		t.Fatalf("fixture too small to make a DOM's overhead clearly visible: %d bytes", len(doc))
	}

	runtime.GC()
	var base runtime.MemStats
	runtime.ReadMemStats(&base)

	var peak uint64 = base.HeapAlloc
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		var m runtime.MemStats
		for {
			select {
			case <-stop:
				return
			default:
				runtime.ReadMemStats(&m)
				if m.HeapAlloc > peak {
					peak = m.HeapAlloc
				}
			}
		}
	}()

	sink := &fakeSink{}
	err := Summarize(strings.NewReader(doc), sink)
	close(stop)
	<-done
	if err != nil {
		t.Fatal(err)
	}

	growth := int64(peak) - int64(base.HeapAlloc)
	limit := int64(6 * len(doc))
	if growth > limit {
		t.Fatalf("peak heap growth while parsing: %d bytes, for a %d-byte document (limit %d) - looks like a retained whole-document structure", growth, len(doc), limit)
	}
}
