package diagnostics

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"strings"

	"github.com/rudi-bruchez/argosql/internal/model"
	"github.com/rudi-bruchez/argosql/internal/sqlserver"
)

// queryQuery, queryPlans2019 and queryPlans2022 are sql/query.sql and
// sql/query_plans_{2019,2022}.sql (see each file's own doc comment for
// what it reads and why the plan queries differ by version) - embedded
// here, rather than in embed.go, for the same reason top.go embeds its
// own sql/top_*.sql locally: they are this file's own concern alone.
var (
	//go:embed sql/query.sql
	queryQuery string

	//go:embed sql/query_plans_2019.sql
	queryPlans2019 string

	//go:embed sql/query_plans_2022.sql
	queryPlans2022 string
)

// QueryTable is "qs query"'s first table: one query's identity (design
// spec: "qs query <query_id>": "Identity, parent object when visible,
// SQL preview and full-text artifact..."). internal/cli's registry
// uses this exact TableSpec as "qs query"'s declared Command.Tables,
// so help's advertised schema and what Query actually writes can never
// drift apart.
//
// parent_module is nullable: NULL for an ad-hoc query (object_id = 0)
// and also NULL for a query compiled inside a module the current
// principal cannot see in the catalog right now (a warning notice
// covers that second case - see Query's own doc comment). query_hash
// is query.sql's own binary(8) column, rendered by
// internal/output.ScanRow as a "0x"-prefixed hex string, like every
// other VARBINARY/BINARY cell in this project. text_preview carries
// the query's full SQL text as an ordinary string cell, subject to the
// same project-wide preview/truncation rules as any other text cell;
// text_artifact is the path of the complete .sql file Query exports
// through Sink.File, never truncated.
var QueryTable = model.TableSpec{
	Name: "query",
	Columns: []model.Column{
		{Name: "query_id", SQLType: "BIGINT"},
		{Name: "object_id", SQLType: "INT"},
		{Name: "parent_module", SQLType: "NVARCHAR"},
		{Name: "is_internal_query", SQLType: "BIT"},
		{Name: "query_hash", SQLType: "BINARY(8)"},
		{Name: "text_preview", SQLType: "NVARCHAR(MAX)"},
		{Name: "text_artifact", SQLType: "NVARCHAR"},
	},
}

// PlansTable is "qs query"'s second table: one row per recorded plan
// for that query, execution count and the same total/average metrics
// TopQueriesTable carries (design spec: "qs query <query_id>": "...
// per-plan execution counts and averages for the selected window").
// replica_group_id is nullable exactly like TopQueriesTable's own
// column (NULL on 2019, where sys.query_store_runtime_stats carries no
// such column at all); forced is sys.query_store_plan.is_forced_plan.
//
// A plan with zero executions in the requested window is still a row
// here - executions = 0, every average NULL, never a row silently
// dropped (design spec: "qs query lists plans with no executions in
// the window explicitly, with zero executions and null averages") -
// see query_plans_2019.sql/query_plans_2022.sql's own doc comments for
// the LEFT JOIN that guarantees it.
var PlansTable = model.TableSpec{
	Name: "plans",
	Columns: []model.Column{
		{Name: "plan_id", SQLType: "BIGINT"},
		{Name: "replica_group_id", SQLType: "BIGINT"},
		{Name: "forced", SQLType: "BIT"},
		{Name: "executions", SQLType: "BIGINT"},
		{Name: "cpu_total_ms", SQLType: "FLOAT"},
		{Name: "cpu_avg_ms", SQLType: "FLOAT"},
		{Name: "duration_total_ms", SQLType: "FLOAT"},
		{Name: "duration_avg_ms", SQLType: "FLOAT"},
		{Name: "reads_total", SQLType: "FLOAT"},
		{Name: "reads_avg", SQLType: "FLOAT"},
	},
}

// QueryOptions is one resolved "qs query" request: the query_id
// (internal/cli's Parse already validated it as a syntactically
// positive integer before any connection opened) and the
// already-resolved Window (window.go's ParseWindow) bounding which
// plan executions count toward each plan's totals/averages.
type QueryOptions struct {
	ID     int64
	Window Window
}

// Positional indexes into query.sql's seven returned cells, named so
// Query never indexes that slice with a bare literal.
const (
	colQQueryID = iota
	colQObjectID
	colQParentSchema
	colQParentName
	colQIsInternalQuery
	colQQueryHash
	colQQuerySQLText
)

// queryStoreNotFound builds the code-8 error Query returns when
// opts.ID does not exist in this database, or exists but is not
// visible to the current principal - the catalog views query.sql
// reads already restrict what is visible, so Query cannot (and does
// not try to) tell the two cases apart, exactly like
// sqlserver.Resolve's own not_found_or_not_visible.
func queryStoreNotFound(id int64) error {
	return &model.PublicError{
		Code:    8,
		Kind:    "not_found_or_not_visible",
		Message: fmt.Sprintf("query_id %d not found or not visible to the current principal", id),
	}
}

// Query runs "qs query <query_id>": one Query Store query's identity
// plus every recorded plan's execution counts and averages over
// opts.Window (design spec).
//
// Every Query Store command reads health first (design spec), exactly
// like Top; a state that cannot be collecting at all (OFF, READ_ONLY,
// ERROR) with no readable runtime history anywhere in the database
// fails at code 4, before Query ever looks up opts.ID.
//
// Once health clears, Query looks up the query's identity before ever
// running the plans query: a query_id that does not exist, or is not
// visible, fails at code 8 (queryStoreNotFound) - this ordering is
// deliberate and load-bearing, not incidental. The plans query below
// reads sys.query_store_runtime_stats, a view some principal's
// permission bundle could deny outright (a permission error there maps
// to code 4, via classifyQueryError, exactly like Top); running the
// identity lookup first guarantees a not-found ID always reports 8,
// never 4, even when that same principal also lacks a permission the
// plans query would have needed - the spec's own stated precedence
// ("With permissions established, a missing query/plan is code 8").
func Query(ctx context.Context, s *sqlserver.Session, opts QueryOptions, dst model.Sink) error {
	health, err := ReadHealth(ctx, s)
	if err != nil {
		return err
	}
	if nonCollectingStates[health.Actual] && !health.HasHistory {
		return &model.PublicError{
			Code:    4,
			Kind:    "query_store_unavailable",
			Message: fmt.Sprintf("Query Store is %s with no readable runtime history", health.Actual),
		}
	}

	cells, found, err := queryOneOptionalRow(ctx, s.Conn, queryQuery, sql.Named("id", opts.ID))
	if err != nil {
		return err
	}
	if !found {
		return queryStoreNotFound(opts.ID)
	}

	queryID, ok := cells[colQQueryID].(int64)
	if !ok {
		return unexpectedCell("query_id")
	}
	objectID, ok := cells[colQObjectID].(int64)
	if !ok {
		return unexpectedCell("object_id")
	}
	isInternal, ok := cells[colQIsInternalQuery].(bool)
	if !ok {
		return unexpectedCell("is_internal_query")
	}
	querySQLText, ok := cells[colQQuerySQLText].(string)
	if !ok {
		return unexpectedCell("query_sql_text")
	}

	// parent_module: NULL for an ad-hoc query (object_id = 0, design
	// spec's own stated default - never attempted to resolve). For a
	// query that does belong to a module, the LEFT JOIN in query.sql
	// may still have found nothing - the module is no longer visible to
	// the current principal, or was dropped - in which case Go reports
	// parent_module = NULL plus a warning notice rather than failing
	// the whole command (design spec: "qs query | 0; parent name may be
	// unavailable").
	var parentModule model.Cell
	if objectID != 0 {
		schema, schemaOK := cells[colQParentSchema].(string)
		name, nameOK := cells[colQParentName].(string)
		if schemaOK && nameOK {
			parentModule = schema + "." + name
		} else {
			dst.Notice(model.Notice{
				Kind:    "parent_module_unavailable",
				Message: fmt.Sprintf("query %d: parent module (object_id %d) is not visible to the current principal", queryID, objectID),
				Table:   QueryTable.Name,
			})
		}
	}

	textArtifact, err := dst.File("query_sql_text", ".sql", strings.NewReader(querySQLText))
	if err != nil {
		return err
	}

	oldestCell, newestCell, oldest, newest, err := readCoverage(ctx, s)
	if err != nil {
		return err
	}
	_ = oldestCell
	_ = newestCell
	emitQueryStoreNotices(dst, QueryTable.Name, health, opts.Window, oldest, newest)

	if err := dst.Begin(QueryTable); err != nil {
		return err
	}
	queryRow := []model.Cell{
		queryID,
		objectID,
		parentModule,
		isInternal,
		cells[colQQueryHash],
		querySQLText,
		textArtifact.Path,
	}
	if err := dst.Row(queryRow); err != nil {
		return err
	}
	if err := dst.End(true, true); err != nil {
		return err
	}

	plans, err := queryRows(ctx, s.Conn, plansQueryFor(s.Major),
		sql.Named("id", opts.ID),
		sql.Named("since", opts.Window.Since),
		sql.Named("until", opts.Window.Until),
	)
	if err != nil {
		return err
	}

	if err := dst.Begin(PlansTable); err != nil {
		return err
	}
	for _, row := range plans {
		if err := dst.Row(row); err != nil {
			return err
		}
	}
	return dst.End(true, true)
}

// plansQueryFor picks query_plans_2019.sql or query_plans_2022.sql by
// major version - the exact same split and the exact same reasoning as
// top.go's topQueryFor, which its own doc comment explains in full.
func plansQueryFor(major int) string {
	if major >= 16 {
		return queryPlans2022
	}
	return queryPlans2019
}
