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
	seq                   int // document (traversal) order, assigned once per RelOp encountered
	nodeID                int64
	physicalOp, logicalOp string
	cost                  float64
}

// operatorWorse reports whether a is a worse operator to keep than b:
// lower estimated subtree cost is worse; cost tied, the one seen LATER
// in the document (higher seq) is worse (design spec, line 89: "Order
// operator ties by statement traversal order then NodeId" - an earlier
// operator survives a later one at the same cost); cost and seq both
// tied (never possible in practice - seq is unique per RelOp) falls
// back to NodeId, purely for a fully deterministic order.
func operatorWorse(a, b operatorCandidate) bool {
	if a.cost != b.cost {
		return a.cost < b.cost
	}
	if a.seq != b.seq {
		return a.seq > b.seq
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
// rows: highest estimated subtree cost first, ties broken by earlier
// traversal order then by NodeId - the display-order mirror of
// operatorWorse above (design spec, line 89's ordering rule stated
// once, in the direction it is actually consumed twice: to decide
// what to evict, and to decide what to print).
func sortOperatorsForDisplay(ops []operatorCandidate) {
	sort.Slice(ops, func(i, j int) bool {
		a, b := ops[i], ops[j]
		if a.cost != b.cost {
			return a.cost > b.cost
		}
		if a.seq != b.seq {
			return a.seq < b.seq
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
func trimBrackets(s string) string {
	if len(s) >= 2 && strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]") {
		return s[1 : len(s)-1]
	}
	return s
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

// malformedXMLError builds the code-5 error Summarize returns when
// dec.Token() itself fails - a syntax error, or any other failure
// reading src (design spec, line 89: "reject malformed XML with code
// 5"; the brief's own shorthand: "parse failure=5 garde son chemin").
func malformedXMLError(err error) error {
	return &model.PublicError{Code: 5, Kind: "malformed_plan_xml", Message: fmt.Sprintf("parsing plan XML: %s", err.Error())}
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
// length-capped slices - see summary_test.go's own retention test for
// the load-bearing proof against a 10,000-operator, 80 KiB+ document.
//
// A statement-level cost is read from the FIRST RelOp encountered in
// document order: xml.Decoder visits elements in that order, so the
// first RelOp inside a plan's <QueryPlan> is always its outermost,
// root operator, whose own EstimatedTotalSubtreeCost already IS the
// statement's total estimated cost - Summarize never sums subtree
// costs itself (design spec, line 87: "do not sum overlapping subtree
// costs"), and never counts statements at all (design spec, line 89:
// "No statement count is required in the summary" - a Query Store
// plan always carries exactly one).
//
// A capped references or warnings list is reported as an incomplete
// table (End(false, true)) plus a Notice pointing at the raw artifact,
// using the project's own closed vocabulary for this exact fact
// (model.ReasonCollectionLimit, surfaced automatically by
// internal/output.Render from that same completeness flag - see
// internal/output/preview.go's tableReasons/buildPreviewState). The
// operators table is never marked incomplete this way: retaining only
// the top five is this summary's own declared shape (design spec: "up
// to five operators"), not a limit unexpectedly reached.
func Summarize(src io.Reader, dst model.Sink) error {
	dec := xml.NewDecoder(src)

	var (
		seq               int
		haveStatement     bool
		statementCost     model.Cell
		ops               operatorHeap
		refs              []referenceRow
		refSeen           = map[string]bool{}
		refsTruncated     bool
		warnings          []warningRow
		warningsTruncated bool
		inWarnings        bool
		warningsDepth     int
		depth             int
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
			switch t.Name.Local {
			case "RelOp":
				nodeID, cost, ok := relOpValues(t)
				if ok {
					if !haveStatement {
						statementCost = cost
						haveStatement = true
					}
					physicalOp, _ := attrValue(t, "PhysicalOp")
					logicalOp, _ := attrValue(t, "LogicalOp")
					considerOperator(&ops, operatorCandidate{seq: seq, nodeID: nodeID, physicalOp: physicalOp, logicalOp: logicalOp, cost: cost})
					seq++
				}
			case "Object":
				if r, ok := objectValues(t); ok {
					key := r.database + "|" + r.schema + "|" + r.table + "|" + r.index
					if !refSeen[key] {
						if len(refs) < referenceCap {
							refSeen[key] = true
							refs = append(refs, r)
						} else {
							refsTruncated = true
						}
					}
				}
			case "Warnings":
				inWarnings = true
				warningsDepth = depth
			default:
				if inWarnings && depth == warningsDepth+1 {
					if len(warnings) < warningCap {
						warnings = append(warnings, warningRow{warningType: t.Name.Local, detail: warningDetail(t)})
					} else {
						warningsTruncated = true
					}
				}
			}
		case xml.EndElement:
			if inWarnings && t.Name.Local == "Warnings" && depth == warningsDepth {
				inWarnings = false
			}
			depth--
		}
	}

	if err := writeStatementTable(dst, statementCost); err != nil {
		return err
	}
	if err := writeOperatorsTable(dst, ops); err != nil {
		return err
	}
	if err := writeReferencesTable(dst, refs, refsTruncated); err != nil {
		return err
	}
	return writeWarningsTable(dst, warnings, warningsTruncated)
}

func writeStatementTable(dst model.Sink, cost model.Cell) error {
	if err := dst.Begin(StatementTable); err != nil {
		return err
	}
	if err := dst.Row([]model.Cell{summarySource, cost}); err != nil {
		return err
	}
	return dst.End(true, true)
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
