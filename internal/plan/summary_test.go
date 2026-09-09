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
//
// This case is constructed, not observed: every real plan measured
// against a live engine for this task (both 2019 and 2022, via
// tests/integration/plan_test.go's own TestPlanSummary) had its root
// RelOp's cost equal its statement's cost, so a real repro of the bug
// this test guards was never found in the time available. The case
// stays plausible - nothing in the engine's own documentation
// guarantees the two always coincide, and the previous implementation
// silently assumed they did - which is why this synthetic fixture is
// kept rather than dropped, not because it was ever seen on a real
// engine (fix 2, resolving fix 1's own open concern on this point).
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
	// Fix 2's B3: physical_op, logical_op and estimated_subtree_cost were
	// unchecked entirely - a reviewer measured that setting the first two
	// to nil and the third to a fixed 0 for every row left this test
	// green, because sorting operates on the candidates, never on the
	// emitted cells. buildPlanXML names them "Root"/"Root" for NodeId 0
	// and "Op<i>"/"L<i>" for NodeId i>=1, cost n-i.
	wantNodeIDs := []int64{0, 1, 2, 3, 4}
	wantPhysicalOps := []string{"Root", "Op1", "Op2", "Op3", "Op4"}
	wantLogicalOps := []string{"Root", "L1", "L2", "L3", "L4"}
	wantCosts := []float64{float64(n + 1000), float64(n - 1), float64(n - 2), float64(n - 3), float64(n - 4)}
	for i, row := range ops.rows {
		nodeID, ok := row[0].(int64)
		if !ok || nodeID != wantNodeIDs[i] {
			t.Fatalf("row %d node_id: got %#v, want %d (descending cost order)", i, row[0], wantNodeIDs[i])
		}
		if got, ok := row[1].(string); !ok || got != wantPhysicalOps[i] {
			t.Fatalf("row %d physical_op: got %#v, want %q", i, row[1], wantPhysicalOps[i])
		}
		if got, ok := row[2].(string); !ok || got != wantLogicalOps[i] {
			t.Fatalf("row %d logical_op: got %#v, want %q", i, row[2], wantLogicalOps[i])
		}
		if got, ok := row[3].(float64); !ok || got != wantCosts[i] {
			t.Fatalf("row %d estimated_subtree_cost: got %#v, want %v", i, row[3], wantCosts[i])
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

	// Fix 2's B4: no test checked a single reference cell's actual
	// value - a reviewer measured that making trimBrackets the identity
	// function (so every cell keeps its literal "[...]" brackets) left
	// every existing test green. buildPlanXML's Object elements always
	// carry Database="[AppDB]" Schema="[dbo]" Table="[T<i>]"
	// Index="[IX_T<i>]"; the unbracketed forms must be exactly what
	// each cell holds.
	found := false
	for _, row := range refs.rows {
		db, _ := row[0].(string)
		schema, _ := row[1].(string)
		table, _ := row[2].(string)
		index, _ := row[3].(string)
		if db == "AppDB" && schema == "dbo" && table == "T0" && index == "IX_T0" {
			found = true
		}
		if strings.ContainsAny(db+schema+table+index, "[]") {
			t.Fatalf("reference cell still carries a bracket: %+v", row)
		}
	}
	if !found {
		t.Fatalf("no reference row matches the unbracketed AppDB/dbo/T0/IX_T0 tuple: %+v", refs.rows)
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
	// Fix 2's B7: the references test already checks its own truncation
	// notice, but nothing checked the warnings one - a reviewer measured
	// that removing dst.Notice from writeWarningsTable left this test
	// green, because it only ever checked warn.collectionComplete.
	if n := sink.noticeWithKind("plan_summary_truncated"); n == nil {
		t.Fatal("no truncation notice emitted for warnings")
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

// TestSummarizeWarningChildDetailCarriesAttributes is fix 2's B5: no
// existing test read a warning row's own "detail" column, only its
// "warning_type" - a reviewer measured that making warningDetail
// return an unconditional empty string left every test green. A
// direct child of Warnings with two attributes must render both, in
// declaration order, joined by "; ".
func TestSummarizeWarningChildDetailCarriesAttributes(t *testing.T) {
	doc := `<ShowPlanXML><QueryPlan>` +
		`<Warnings><SpillToTempDb SpillLevel="1" SpilledThreadCount="4"/></Warnings>` +
		`</QueryPlan></ShowPlanXML>`
	sink := &fakeSink{}
	if err := Summarize(strings.NewReader(doc), sink); err != nil {
		t.Fatal(err)
	}
	warn := sink.table(WarningsTable.Name)
	if warn == nil || len(warn.rows) != 1 {
		t.Fatalf("warnings retained: got %+v, want exactly 1 row", warn)
	}
	row := warn.rows[0]
	if row[0] != "SpillToTempDb" {
		t.Fatalf("warning_type: got %#v, want %q", row[0], "SpillToTempDb")
	}
	want := "SpillLevel=1; SpilledThreadCount=4"
	if row[1] != want {
		t.Fatalf("detail: got %#v, want %q", row[1], want)
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

// TestSummarizeBoundedRetentionAcrossGrowingDocumentSize checks that
// growing the document 200x (100 operators/refs/warnings to 20,000)
// produces ZERO growth in what any of the four tables retains -
// operators, references and warnings each stay at or under their
// fixed cap regardless of N.
//
// Fix 2's B9 corrects what fix 1's own report claimed for this test:
// it is NOT the primary proof of bounded parsing, and it does not
// distinguish a token-by-token reader from a whole-document DOM. A
// reviewer built four different DOM-shaped implementations of
// Summarize (a pointer tree copying attributes, a tree of pre-parsed
// fields, a plain io.ReadAll + xml.Unmarshal, and a flat-array tree) -
// every one of them PASSES this test, because every one of them still
// truncates its own final result to the same five/100/100 shape
// before handing rows to dst. This test only proves the OUTPUT is
// capped; TestSummarizeDoesNotHoldWholeDocumentInMemory, below, is the
// one test that actually caught all four DOM-shaped implementations -
// see its own doc comment for why, and do not remove it on the belief
// that this test already covers the same ground.
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
// qu'aucune représentation complète du document n'est construite").
//
// Fix 2's B9: this is the PRIMARY, DISCRIMINATING evidence against a
// whole-document DOM, not a secondary or "necessarily coarse" signal -
// fix 1's own report had these two roles backwards, and a reviewer
// measured exactly why that was dangerous. Four separately-built
// DOM-shaped implementations of Summarize (a pointer tree copying
// attributes, a tree of pre-parsed fields, plain io.ReadAll +
// xml.Unmarshal, and a flat-array tree) all PASS
// TestSummarizeBoundedRetentionAcrossGrowingDocumentSize, because a
// DOM can truncate its own final result to the same shape a token
// reader produces - that test only ever looks at the OUTPUT. All four
// FAIL this one. This is the only test in this file that actually
// tells a token-by-token reader apart from a DOM; do not read the
// retention test above as "the real proof" and treat this one as
// removable padding.
//
// It samples runtime.MemStats.HeapAlloc from a second goroutine WHILE
// Summarize is running against a 20,000-operator, ~1.9 MB fixture, and
// asserts the observed peak growth stays within 3x the document's own
// byte size. A genuine whole-document DOM (one Go value - struct,
// slice, or attribute map - per element, held simultaneously for the
// call's whole duration) costs several times the raw byte size in
// Go's own struct/pointer/interface overhead; this bounded,
// token-by-token parser's own working set is a handful of fixed-size
// slices plus whatever short-lived garbage a single
// xml.Decoder.Token() call produces, which GC can reclaim between
// samples.
//
// The factor was measured, not guessed. On a real 1,895,827-byte
// fixture, a reviewer's four DOM-shaped implementations grew the heap
// by 12,238,856 to 15,587,480 bytes (6.5x to 8.2x); this project's own
// honest, token-by-token Summarize grew it by 2,673,072 to 2,815,392
// bytes (1.4x to 1.5x) on the reviewer's machine, and 2,558,832 to
// 2,889,856 bytes (1.35x to 1.52x) across five runs measured on the
// machine this fix was written on. A limit of 6x, this test's own
// previous value, left only 7.6% of headroom below the cheapest DOM
// measured - close enough that a leaner DOM variant, or one lucky GC
// pass, could have passed it. 3x keeps roughly double the honest
// implementation's own worst measured growth as margin while sitting
// comfortably below every DOM variant measured; five consecutive runs
// on this machine stayed under it (worst-case growth 2,889,856 bytes
// against a 5,687,571-byte limit for this fixture's size). This
// measurement still carries ordinary GC/allocator noise and is not
// exact to the byte, which is why the margin exists at all - but it is
// the discriminating measurement in this file, not a decorative one.
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
	limit := int64(3 * len(doc))
	if growth > limit {
		t.Fatalf("peak heap growth while parsing: %d bytes, for a %d-byte document (limit %d) - looks like a retained whole-document structure", growth, len(doc), limit)
	}
}
