package diagnostics

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"os"
	"strings"

	"github.com/rudi-bruchez/argosql/internal/model"
	"github.com/rudi-bruchez/argosql/internal/plan"
	"github.com/rudi-bruchez/argosql/internal/sqlserver"
)

// planQuery is sql/plan.sql (see its own doc comment for what it reads
// and why) - embedded here, rather than in embed.go, for the same
// reason query.go and top.go embed their own sql/*.sql locally: it is
// this file's own concern alone.
//
//go:embed sql/plan.sql
var planQuery string

// planNotFound builds the code-8 error Plan returns when the
// (query_id, plan_id) pair does not exist together in this database -
// either id is simply absent, or plan_id belongs to a different
// query_id (design spec: "query_id or plan_id not present... code 8"
// and "--plan-id does not belong to the given query_id... code 8",
// lines 223-224). sql/plan.sql's own WHERE clause already filters on
// both ids together, so a single zero-row result covers both cases
// identically - the same reasoning query.go's own queryStoreNotFound
// documents for its single not-found case.
func planNotFound(queryID, planID int64) error {
	return &model.PublicError{
		Code:    8,
		Kind:    "not_found_or_not_visible",
		Message: fmt.Sprintf("plan_id %d for query_id %d not found, not visible, or does not belong to that query", planID, queryID),
	}
}

// planUnavailable builds the code-4 error Plan returns when the
// (query_id, plan_id) pair itself resolves, but sys.query_store_plan's
// own query_plan column is NULL for it - a plan whose XML the engine
// never captured, or has since evicted (Microsoft documents this exact
// shape for the closely related sys.dm_exec_query_plan: "If the query
// plan... has been evicted... the query_plan column... is null").
// Mirrors "obj code"'s own null-definition case (design spec: "a null
// definition yields definition_unavailable, code 4, without inventing
// its cause") - the closest existing precedent in this project for "the
// row exists, but the thing it should carry does not."
func planUnavailable(queryID, planID int64) error {
	return &model.PublicError{
		Code:    4,
		Kind:    "plan_unavailable",
		Message: fmt.Sprintf("plan_id %d for query_id %d has no exportable plan XML", planID, queryID),
	}
}

// Plan runs "plan <query_id> --plan-id <id> [--summary]": verifies the
// pair belongs together, exports the complete plan XML to a
// ".sqlplan" artifact, and, when summary is true, a bounded summary
// (design spec: "Verify plan belongs to query; export complete XML to
// .sqlplan; optional bounded summary").
//
// Unlike Top and Query, Plan never reads Health first and never fails
// on a non-collecting Query Store state: design spec line 83's own
// exception ("plan can export a retained plan even if no runtime
// history remains") and its exit-code table's own row ("Retained plan
// export without runtime history | 0") both say a plan export is not
// gated by runtime history the way ranking/detail commands are - it
// reads sys.query_store_plan directly, a catalog view that outlives
// sys.query_store_runtime_stats' own retention window.
func Plan(ctx context.Context, s *sqlserver.Session, queryID, planID int64, summary bool, dst model.Sink) error {
	cells, found, err := queryOneOptionalRow(ctx, s.Conn, planQuery, sql.Named("query_id", queryID), sql.Named("plan_id", planID))
	if err != nil {
		return err
	}
	if !found {
		return planNotFound(queryID, planID)
	}

	if cells[0] == nil {
		return planUnavailable(queryID, planID)
	}
	xmlText, ok := cells[0].(string)
	if !ok {
		return unexpectedCell("query_plan")
	}

	return exportAndSummarize(xmlText, summary, dst)
}

// exportAndSummarize normalizes xmlText into a real temporary file
// (never a pipe), exports that file as this run's raw ".sqlplan"
// artifact, and, if summary is true, re-reads that same finalized file
// a second time to produce the bounded summary - the raw export always
// happens first and is retained even when the summary step later fails
// (design spec, line 87: "malformed XML yields a summary error while
// retaining a successfully exported artifact"): dst.File's own
// bookkeeping (internal/artifacts.Collector) already records the
// artifact as soon as it succeeds, independent of any error this
// function returns afterwards.
//
// This task's dispatch settled the mechanism deliberately. model.Sink's
// own File(kind, suffix string, src io.Reader) takes a reader, and
// plan.NormalizeXML writes to a writer: wiring them directly would need
// a goroutine and a pipe, exactly the push/pull mismatch a prior
// review already fixed elsewhere in this project. internal/artifacts'
// own store is entirely unexported (newStore, create, hasRoom, record,
// reclaimIncomplete), so there is no lower-level "store temp file" this
// package can reach for either. Normalizing into an os.CreateTemp file,
// reopening it for every read, and removing it on return (defer) is
// the one mechanism actually reachable from here with no goroutine at
// all.
//
// That temp file is written in full, off any quota, before dst.File
// ever gets to check the artifact byte budget: a plan whose normalized
// XML exceeds the run's remaining quota is still written to local disk
// once, in full, uncontrolled, and then a second time - bounded,
// truncation-checked - by dst.File's own quota enforcement, which
// refuses and deletes a partial write rather than ever leaving a
// truncated .sqlplan artifact on disk (design spec, line 111: "do not
// publish truncated source files; return code 7"). This double write,
// and the temp file's own lack of a quota, is the cost this mechanism
// accepts; internal/artifacts' store would have to export part of its
// private API to avoid it, which this task does not do.
//
// Summarize always reads the NORMALIZED file, never xmlText directly:
// besides matching "the raw XML is finalized before Summarize" (this
// task's brief), it is also a correctness requirement - encoding/xml's
// own Decoder refuses to parse a document whose declared encoding it
// does not recognize without a registered CharsetReader, and
// NormalizeXML is what already rewrote any such declaration to name
// UTF-8 before this function ever gets here.
func exportAndSummarize(xmlText string, summary bool, dst model.Sink) error {
	tmp, err := os.CreateTemp("", "asq-plan-*.xml")
	if err != nil {
		return &model.PublicError{Code: 6, Kind: "artifact", Message: fmt.Sprintf("creating temporary plan file: %s", err.Error())}
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	normalized, normErr := plan.NormalizeXML(strings.NewReader(xmlText), tmp)
	closeErr := tmp.Close()
	if normErr != nil {
		return &model.PublicError{Code: 6, Kind: "artifact", Message: fmt.Sprintf("normalizing plan XML: %s", normErr.Error())}
	}
	if closeErr != nil {
		return &model.PublicError{Code: 6, Kind: "artifact", Message: fmt.Sprintf("writing temporary plan file: %s", closeErr.Error())}
	}

	exportFile, err := os.Open(tmpPath)
	if err != nil {
		return &model.PublicError{Code: 6, Kind: "artifact", Message: fmt.Sprintf("reopening temporary plan file: %s", err.Error())}
	}
	artifact, fileErr := dst.File("plan_xml", ".sqlplan", exportFile)
	exportFile.Close()
	if fileErr != nil {
		return fileErr
	}

	// Second integration the brief itself did not name: NormalizeXML's
	// own normalized boolean is published through the exact same
	// vocabulary internal/artifacts/collector.go already uses for the
	// identical fact about a table artifact - model.KindEncodingNormalized
	// (design spec, line 91: "record encoding_normalized=true"). Two
	// identical facts under two different names is precisely the defect
	// this project has already paid to fix once (see
	// internal/model/result.go's own doc comment on the closed reasons
	// vocabulary).
	if normalized {
		dst.Notice(model.Notice{
			Kind:    model.KindEncodingNormalized,
			Message: fmt.Sprintf("plan XML declared a non-UTF-8 encoding; only its declaration was rewritten to utf-8, the artifact's bytes are otherwise unmodified (%s)", artifact.Path),
		})
	}

	if !summary {
		return nil
	}

	summaryFile, err := os.Open(tmpPath)
	if err != nil {
		return &model.PublicError{Code: 6, Kind: "artifact", Message: fmt.Sprintf("reopening temporary plan file for summary: %s", err.Error())}
	}
	defer summaryFile.Close()
	return plan.Summarize(summaryFile, dst)
}
