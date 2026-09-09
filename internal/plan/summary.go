package plan

import (
	"container/heap"
	"encoding/xml"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/rudi-bruchez/argosql/internal/model"
)

// StatementTable, OperatorsTable, ReferencesTable and WarningsTable are
// "plan --summary"'s four tables, in the design spec's own declared
// order (line 103: "plan --summary: statement, operators, references,
// warnings"). internal/cli's registry uses these exact TableSpecs as
// the command's declared Command.Tables, so help's advertised schema
// and what Summarize actually writes can never drift apart - the same
// convention every other command in this project already follows for
// its own tables.
var StatementTable = model.TableSpec{
	Name: "statement",
	Columns: []model.Column{
		{Name: "source", SQLType: "NVARCHAR"},
		{Name: "estimated_cost", SQLType: "FLOAT"},
	},
}

var OperatorsTable = model.TableSpec{
	Name: "operators",
	Columns: []model.Column{
		{Name: "node_id", SQLType: "BIGINT"},
		{Name: "physical_op", SQLType: "NVARCHAR"},
		{Name: "logical_op", SQLType: "NVARCHAR"},
		{Name: "estimated_subtree_cost", SQLType: "FLOAT"},
	},
}

var ReferencesTable = model.TableSpec{
	Name: "references",
	Columns: []model.Column{
		{Name: "database_name", SQLType: "NVARCHAR"},
		{Name: "schema_name", SQLType: "NVARCHAR"},
		{Name: "table_name", SQLType: "NVARCHAR"},
		{Name: "index_name", SQLType: "NVARCHAR"},
	},
}

var WarningsTable = model.TableSpec{
	Name: "warnings",
	Columns: []model.Column{
		{Name: "warning_type", SQLType: "NVARCHAR"},
		{Name: "detail", SQLType: "NVARCHAR"},
	},
}

// summarySource is the statement table's own "source" cell (design
// spec, line 87: "Plan summaries identify the source as Query Store
// compiled plan XML") - identified in the DATA itself, not only in
// this package's prose, so a consumer reading the row directly knows
// it is looking at optimizer estimates from a compiled plan, never an
// observed execution.
const summarySource = "query_store_compiled_plan_xml"

// operatorCap, referenceCap and warningCap are the design spec's own
// bounds (line 89: "retain at most five ranked operators, 100 distinct
// object/index references, and 100 warning summaries") on what
// Summarize ever retains in memory at once, regardless of how many the
// document actually contains: a plan with 10,000 RelOp elements costs
// this function exactly a 5-element heap, never a slice sized to the
// document.
const (
	operatorCap  = 5
	referenceCap = 100
	warningCap   = 100
)

// operatorCandidate is one RelOp Summarize has seen, whether or not it
// survives in the final top-operatorCap cut.
type operatorCandidate struct {
	nodeID                int64
	physicalOp, logicalOp string
	cost                  float64
}

// operatorWorse reports whether a is a worse operator to keep than b:
// lower estimated subtree cost is worse; cost tied, the design spec's
// own tie-break applies (line 89: "Order operator ties by statement
// traversal order then NodeId") - fix 1's A7: a Query Store plan always
// carries exactly one statement (measured fact, CLAUDE.md), so
// "statement traversal order" is the SAME for every operator this
// function ever sees and never distinguishes two tied candidates; the
// tie-break that actually applies is NodeId, directly, ascending among
// the kept set. The previous form used a monotonic seq counter instead
// - unique per RelOp by construction, so the "then NodeId" half was
// dead code, unreachable because no two seq values were ever equal -
// and produced document-encounter order on a tie instead of NodeId
// order, exactly the defect fix 1 measured (NodeId 9, 2, 1 kept in
// that order instead of 1, 2, 9).
func operatorWorse(a, b operatorCandidate) bool {
	if a.cost != b.cost {
		return a.cost < b.cost
	}
	return a.nodeID > b.nodeID
}

// operatorHeap is a container/heap min-heap, ordered by operatorWorse,
// over the at-most-operatorCap candidates Summarize is currently
// keeping: heap[0] is always the single worst-kept operator, the one
// considerOperator evicts first when a better candidate arrives.
type operatorHeap []operatorCandidate

func (h operatorHeap) Len() int           { return len(h) }
func (h operatorHeap) Less(i, j int) bool { return operatorWorse(h[i], h[j]) }
func (h operatorHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *operatorHeap) Push(x any)        { *h = append(*h, x.(operatorCandidate)) }
func (h *operatorHeap) Pop() any {
	old := *h
	n := len(old)
	item := old[n-1]
	*h = old[:n-1]
	return item
}

// considerOperator keeps c among h's operatorCap best candidates: push
// when there is still room, otherwise replace the current worst-kept
// operator only if c beats it. h never grows past operatorCap
// elements - this is the whole of Summarize's bounded top-K retention
// for operators, the load-bearing proof that a 10,000-operator plan
// costs this function a fixed, five-element structure, never one sized
// to the document (design spec, line 89: "do not mistake a five-row
// summary for bounded parsing" - see summary_test.go's own retention
// and allocation instrumentation).
func considerOperator(h *operatorHeap, c operatorCandidate) {
	if h.Len() < operatorCap {
		heap.Push(h, c)
		return
	}
	if operatorWorse((*h)[0], c) {
		(*h)[0] = c
		heap.Fix(h, 0)
	}
}

// sortOperatorsForDisplay orders ops for the operators table's own
// rows: highest estimated subtree cost first, ties broken by NodeId
// ascending - the display-order mirror of operatorWorse above (design
// spec, line 89's ordering rule stated once, in the direction it is
// actually consumed twice: to decide what to evict, and to decide
// what to print).
func sortOperatorsForDisplay(ops []operatorCandidate) {
	sort.Slice(ops, func(i, j int) bool {
		a, b := ops[i], ops[j]
		if a.cost != b.cost {
			return a.cost > b.cost
		}
		return a.nodeID < b.nodeID
	})
}

// referenceRow is one distinct object/index reference Summarize has
// captured from an <Object .../> element.
type referenceRow struct {
	database, schema, table, index string
}

// warningRow is one direct child element of a <Warnings> block.
type warningRow struct {
	warningType, detail string
}

// attrValue returns the value of t's attribute named name, ignoring
// namespace: showplan XML never qualifies its own attributes, so
// matching on the local name alone is exact.
func attrValue(t xml.StartElement, name string) (string, bool) {
	for _, a := range t.Attr {
		if a.Name.Local == name {
			return a.Value, true
		}
	}
	return "", false
}

// relOpValues reads NodeId and EstimatedTotalSubtreeCost off a RelOp
// start element (design spec: "Lire RelOp/NodeId/EstimatedTotalSubtreeCost").
// ok is false when either attribute is missing or unparseable: a
// malformed or unexpectedly shaped RelOp is simply skipped, never a
// reason to fail the whole summary (design spec, line 89: "Accept
// unknown elements").
func relOpValues(t xml.StartElement) (nodeID int64, cost float64, ok bool) {
	nodeStr, hasNode := attrValue(t, "NodeId")
	costStr, hasCost := attrValue(t, "EstimatedTotalSubtreeCost")
	if !hasNode || !hasCost {
		return 0, 0, false
	}
	n, err := strconv.ParseInt(nodeStr, 10, 64)
	if err != nil {
		return 0, 0, false
	}
	c, err := strconv.ParseFloat(costStr, 64)
	if err != nil {
		return 0, 0, false
	}
	return n, c, true
}

// trimBrackets strips one layer of SQL Server's own "[...]" delimited-
// identifier quoting from a showplan Object attribute value
// ("[dbo]" -> "dbo"), leaving anything not shaped that way untouched.
// Fix 1's A6: a delimited identifier escapes a literal "]" inside it
// as "]]" (the same doubling rule QUOTENAME/bracket-quoted names use
// everywhere in SQL Server), so "[a]]b]" names the single identifier
// "a]b", not the four-character string "a]]b" the previous form
// returned by only ever removing the outer pair.
func trimBrackets(s string) string {
	if len(s) >= 2 && strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]") {
		return strings.ReplaceAll(s[1:len(s)-1], "]]", "]")
	}
	return s
}

// referenceKey builds a dedup key for r that cannot collide between
// two genuinely different references. Fix 1's A6: the previous key,
// r.database+"|"+r.schema+"|"+r.table+"|"+r.index, used "|" as a field
// separator even though "|" is a legal character inside a delimited
// identifier - (Database=[D], Schema=[a|b], Table=[c]) and
// (Database=[D], Schema=[a], Table=[b|c]) produced the identical
// string "D|a|b|c|" and were merged into one reference. Prefixing each
// field with its own byte length (a standard length-prefixed, or
// netstring-style, encoding) makes the key injective: reconstructing
// the original four fields from the key is unambiguous, so two
// different tuples can never produce the same string, regardless of
// what characters any field contains.
func referenceKey(r referenceRow) string {
	field := func(s string) string { return strconv.Itoa(len(s)) + ":" + s }
	return field(r.database) + field(r.schema) + field(r.table) + field(r.index)
}

// objectValues reads Database/Schema/Table/Index off an Object start
// element. ok is false only when none of the four are present at all -
// an Object element carrying at least one of them is still a real
// reference worth keeping, even if some fields are absent (a
// heap/table-less index, or a table referenced with no specific index).
func objectValues(t xml.StartElement) (referenceRow, bool) {
	db, _ := attrValue(t, "Database")
	schema, _ := attrValue(t, "Schema")
	table, _ := attrValue(t, "Table")
	index, _ := attrValue(t, "Index")
	if db == "" && schema == "" && table == "" && index == "" {
		return referenceRow{}, false
	}
	return referenceRow{
		database: trimBrackets(db),
		schema:   trimBrackets(schema),
		table:    trimBrackets(table),
		index:    trimBrackets(index),
	}, true
}

// warningDetail renders t's own attributes, if any, as a compact
// "key=value; key2=value2" string - the extent of a Warnings child
// element's own content this bounded parser captures, without
// descending into whatever further nested elements it may carry (a
// deeper scan would grow this function's own memory bound with
// document shape, exactly what design spec line 89 rules out).
func warningDetail(t xml.StartElement) string {
	if len(t.Attr) == 0 {
		return ""
	}
	parts := make([]string, len(t.Attr))
	for i, a := range t.Attr {
		parts[i] = a.Name.Local + "=" + a.Value
	}
	return strings.Join(parts, "; ")
}

// appendWarning records one warning row (warnType, detail) into
// warnings, or marks *truncated once warningCap is reached - shared by
// both of Summarize's two sources of a warning (fix 1's A1): a
// <Warnings ...attr="value".../> element's own attributes, and each of
// its child elements (already handled before A1; see Summarize's own
// doc comment on why both exist and neither substitutes for the
// other).
func appendWarning(warnings *[]warningRow, truncated *bool, warnType, detail string) {
	if len(*warnings) < warningCap {
		*warnings = append(*warnings, warningRow{warningType: warnType, detail: detail})
		return
	}
	*truncated = true
}

// malformedXMLError builds the code-5 error Summarize returns when
// dec.Token() itself fails - a syntax error, or any other failure
// reading src (design spec, line 89: "reject malformed XML with code
// 5"; the brief's own shorthand: "parse failure=5 garde son chemin").
func malformedXMLError(err error) error {
	return &model.PublicError{Code: 5, Kind: "malformed_plan_xml", Message: fmt.Sprintf("parsing plan XML: %s", err.Error())}
}

// summaryTruncatedError builds the code-7 error Summarize returns when
// the references or the warnings list was capped (fix 1's A3): the
// design spec, line 101, states "incomplete collection yields code 7"
// without a carve-out for plan --summary. There is no other End(false,
// ...) caller in this project to follow: the twelve others all pass
// true, and the only other code-7 path is the Collector's own row and
// byte breach, which raises the error itself rather than through a
// completeness flag. This caller is the first, so the spec is the whole
// argument and no in-repo precedent supports it.
// The raw plan_xml artifact and every table's own rows/notices are
// already written and recorded by the time this returns - a later
// error never erases what a Sink already accepted (see
// internal/artifacts/collector.go's Finish, and diagnostics/plan.go's
// exportAndSummarize, which exports the raw artifact before Summarize
// ever runs).
func summaryTruncatedError() error {
	return &model.PublicError{
		Code:    7,
		Kind:    "collection_limit",
		Message: "plan summary: the references or warnings list was truncated at its cap; see the raw plan_xml artifact for the complete plan",
	}
}

// Summarize reads src (a complete, already-normalized plan XML
// document - see plan.go's own exportAndSummarize for why it must be
// the normalized file, never the raw driver string) with a
// token-by-token xml.Decoder, never a whole-document DOM, and writes
// the four design-spec tables to dst in their declared order:
// statement, operators, references, warnings (design spec, line 103).
//
// Memory is bounded independent of document size: at most operatorCap
// operators, referenceCap distinct references and warningCap warnings
// are ever held at once, via considerOperator's heap and two
// length-capped slices. summary_test.go's own
// TestSummarizeBoundedRetentionAcrossGrowingDocumentSize proves this
// output shape holds against a 10,000-operator, 80 KiB+ document, but
// that test alone cannot tell this token-by-token reader apart from a
// whole-document DOM that truncates its own result the same way (see
// TestSummarizeDoesNotHoldWholeDocumentInMemory, fix 2's B9, which is
// the one that actually can, and does).
//
// The statement's own estimated cost (design spec, line 87: "the
// statement's estimated cost") is read from the StatementSubTreeCost
// attribute of the first "Stmt*" element encountered (StmtSimple,
// StmtCursor, ... - a Query Store plan always carries exactly one, so
// only the first is ever consulted). Fix 1's A2: this used to read the
// first RelOp's own EstimatedTotalSubtreeCost instead, which is the
// same value only because the root RelOp's subtree cost happens to
// equal the statement's cost when the root itself carries both
// attributes - a descendant RelOp silently became "the statement's
// cost" whenever the root was missing either one. If StatementSubTreeCost
// is absent or unparseable, estimated_cost is reported unavailable
// (model.ReasonPropertyUnavailable, via propertiesComplete=false) -
// never guessed from an operator of a different scope.
//
// A capped references or warnings list is reported as an incomplete
// table (End(false, true)) plus a Notice pointing at the raw artifact,
// using the project's own closed vocabulary for this exact fact
// (model.ReasonCollectionLimit, surfaced automatically by
// internal/output.Render from that same completeness flag - see
// internal/output/preview.go's tableReasons/buildPreviewState), AND
// (fix 1's A3) a code-7 summaryTruncatedError once every table has
// been written. The operators table is never marked incomplete this
// way: retaining only the top five is this summary's own declared
// shape (design spec: "up to five operators"), not a limit
// unexpectedly reached.
//
// Fix 1's A9: the end of the token stream is not, by itself, proof the
// document was well formed - encoding/xml.Decoder.Token() tokenizes
// without validating that exactly one root element closes the
// document, so "<ShowPlanXML/><ShowPlanXML/>" reached io.EOF cleanly
// under the previous form. A second element at depth 1 is now rejected
// as malformed (code 5), the raw export already made by the time
// Summarize runs is untouched either way.
func Summarize(src io.Reader, dst model.Sink) error {
	dec := xml.NewDecoder(src)

	var (
		statementSeen               bool
		statementCost               model.Cell
		statementPropertiesComplete bool
		ops                         operatorHeap
		refs                        []referenceRow
		refSeen                     = map[string]bool{}
		refsTruncated               bool
		warnings                    []warningRow
		warningsTruncated           bool
		inWarnings                  bool
		warningsDepth               int
		depth                       int
		rootElements                int
	)

	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return malformedXMLError(err)
		}

		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			if depth == 1 {
				rootElements++
				if rootElements > 1 {
					return malformedXMLError(fmt.Errorf("more than one root element (%q at document level)", t.Name.Local))
				}
			}
			switch {
			case strings.HasPrefix(t.Name.Local, "Stmt") && !statementSeen:
				statementSeen = true
				if costStr, ok := attrValue(t, "StatementSubTreeCost"); ok {
					if c, err := strconv.ParseFloat(costStr, 64); err == nil {
						statementCost = c
						statementPropertiesComplete = true
					}
				}
			case t.Name.Local == "RelOp":
				nodeID, cost, ok := relOpValues(t)
				if ok {
					physicalOp, _ := attrValue(t, "PhysicalOp")
					logicalOp, _ := attrValue(t, "LogicalOp")
					considerOperator(&ops, operatorCandidate{nodeID: nodeID, physicalOp: physicalOp, logicalOp: logicalOp, cost: cost})
				}
			case t.Name.Local == "Object":
				if r, ok := objectValues(t); ok {
					key := referenceKey(r)
					if !refSeen[key] {
						if len(refs) < referenceCap {
							refSeen[key] = true
							refs = append(refs, r)
						} else {
							refsTruncated = true
						}
					}
				}
			case t.Name.Local == "Warnings":
				inWarnings = true
				warningsDepth = depth
				// Fix 1's A1: the engine expresses some warnings as
				// attributes of Warnings itself (measured on both
				// engines: <Warnings NoJoinPredicate="1"/>), not only as
				// child elements - the only source this function read
				// before this fix, which silently dropped exactly this
				// shape while still reporting the table complete.
				for _, a := range t.Attr {
					appendWarning(&warnings, &warningsTruncated, a.Name.Local, a.Value)
				}
			default:
				if inWarnings && depth == warningsDepth+1 {
					appendWarning(&warnings, &warningsTruncated, t.Name.Local, warningDetail(t))
				}
			}
		case xml.EndElement:
			if inWarnings && t.Name.Local == "Warnings" && depth == warningsDepth {
				inWarnings = false
			}
			depth--
		}
	}

	if err := writeStatementTable(dst, statementCost, statementPropertiesComplete); err != nil {
		return err
	}
	if err := writeOperatorsTable(dst, ops); err != nil {
		return err
	}
	if err := writeReferencesTable(dst, refs, refsTruncated); err != nil {
		return err
	}
	if err := writeWarningsTable(dst, warnings, warningsTruncated); err != nil {
		return err
	}
	if refsTruncated || warningsTruncated {
		return summaryTruncatedError()
	}
	return nil
}

func writeStatementTable(dst model.Sink, cost model.Cell, propertiesComplete bool) error {
	if err := dst.Begin(StatementTable); err != nil {
		return err
	}
	if err := dst.Row([]model.Cell{summarySource, cost}); err != nil {
		return err
	}
	if !propertiesComplete {
		dst.Notice(model.Notice{
			Kind:    "statement_cost_unavailable",
			Message: "no Stmt* element carries a StatementSubTreeCost attribute; estimated_cost is unavailable",
			Table:   StatementTable.Name,
		})
	}
	return dst.End(true, propertiesComplete)
}

func writeOperatorsTable(dst model.Sink, h operatorHeap) error {
	ops := append(operatorHeap(nil), h...)
	sortOperatorsForDisplay(ops)
	if err := dst.Begin(OperatorsTable); err != nil {
		return err
	}
	for _, op := range ops {
		row := []model.Cell{op.nodeID, cellOrNil(op.physicalOp), cellOrNil(op.logicalOp), op.cost}
		if err := dst.Row(row); err != nil {
			return err
		}
	}
	return dst.End(true, true)
}

func writeReferencesTable(dst model.Sink, refs []referenceRow, truncated bool) error {
	if err := dst.Begin(ReferencesTable); err != nil {
		return err
	}
	for _, r := range refs {
		row := []model.Cell{cellOrNil(r.database), cellOrNil(r.schema), cellOrNil(r.table), cellOrNil(r.index)}
		if err := dst.Row(row); err != nil {
			return err
		}
	}
	if truncated {
		dst.Notice(model.Notice{
			Kind:    "plan_summary_truncated",
			Message: fmt.Sprintf("more than %d distinct referenced objects/indexes; list truncated - see the raw plan_xml artifact for the complete plan", referenceCap),
			Table:   ReferencesTable.Name,
		})
	}
	return dst.End(!truncated, true)
}

func writeWarningsTable(dst model.Sink, warnings []warningRow, truncated bool) error {
	if err := dst.Begin(WarningsTable); err != nil {
		return err
	}
	for _, w := range warnings {
		row := []model.Cell{w.warningType, cellOrNil(w.detail)}
		if err := dst.Row(row); err != nil {
			return err
		}
	}
	if truncated {
		dst.Notice(model.Notice{
			Kind:    "plan_summary_truncated",
			Message: fmt.Sprintf("more than %d warning elements; list truncated - see the raw plan_xml artifact for the complete plan", warningCap),
			Table:   WarningsTable.Name,
		})
	}
	return dst.End(!truncated, true)
}

func cellOrNil(s string) model.Cell {
	if s == "" {
		return nil
	}
	return s
}
