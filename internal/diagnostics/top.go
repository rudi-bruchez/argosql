package diagnostics

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"strings"
	"time"

	"github.com/rudi-bruchez/argosql/internal/model"
	"github.com/rudi-bruchez/argosql/internal/output"
	"github.com/rudi-bruchez/argosql/internal/sqlserver"
)

// topQuery2019 and topQuery2022 are sql/top_2019.sql and sql/top_2022.sql
// (see each file's own doc comment for what it reads and why the two
// differ) - embedded here, rather than in embed.go, because they are
// this file's own concern alone.
var (
	//go:embed sql/top_2019.sql
	topQuery2019 string

	//go:embed sql/top_2022.sql
	topQuery2022 string
)

// OrderPlaceholder is the literal token both embedded queries carry in
// their ORDER BY clause; orderColumnFor below resolves it to one of a
// fixed set of literal column names before the query text is ever sent
// to the server. Exported so tests/integration's synthetic aggregation
// fixture (which runs the real query text, via TopQuerySQL, against
// temp tables standing in for the catalog views) can resolve it the
// same way Top itself does, rather than duplicating the literal.
const OrderPlaceholder = "@@ORDER_COLUMN@@"

// TopQueriesTable is the one table "qs top" emits: one ranked row per
// (query_id[, replica_group_id]) (design spec: declared table order
// "qs top: queries"). internal/cli's registry uses this exact TableSpec
// as "qs top"'s declared Command.Tables, so help's advertised schema and
// what Top actually writes can never drift apart.
var TopQueriesTable = model.TableSpec{
	Name: "queries",
	Columns: []model.Column{
		{Name: "query_id", SQLType: "BIGINT"},
		{Name: "replica_group_id", SQLType: "BIGINT"},
		{Name: "executions", SQLType: "BIGINT"},
		{Name: "cpu_total_ms", SQLType: "FLOAT"},
		{Name: "cpu_avg_ms", SQLType: "FLOAT"},
		{Name: "duration_total_ms", SQLType: "FLOAT"},
		{Name: "duration_avg_ms", SQLType: "FLOAT"},
		{Name: "reads_total", SQLType: "FLOAT"},
		{Name: "reads_avg", SQLType: "FLOAT"},
	},
}

// TopRankingTable is "qs top"'s header table, emitted before
// TopQueriesTable (design spec line 103's declared table order predates
// this table; it still must precede the rows it describes). It exists
// because the design spec requires two facts the ranking rows
// themselves never carry: the requested window and the database's
// actual stored interval coverage (design spec line 73: "Disclose the
// requested window and actual interval coverage"), and the filter this
// particular ranking was run under (design spec line 63: "The ranking
// header records its filter") - two unrelated omissions the project
// fixes as one table, not three scattered additions, matching the
// existing idiom ("help" renders commands+flags, "qs status" renders
// status+coverage).
//
// coverage_oldest/coverage_newest are nullable: a database with no
// stored interval at all reports both NULL, never an invented window.
// parent_module is nullable too: NULL when --object was not given, the
// resolved schema.name (never the raw --object argument) when it was.
var TopRankingTable = model.TableSpec{
	Name: "ranking",
	Columns: []model.Column{
		{Name: "requested_since", SQLType: "DATETIMEOFFSET"},
		{Name: "requested_until", SQLType: "DATETIMEOFFSET"},
		{Name: "coverage_oldest", SQLType: "DATETIMEOFFSET"},
		{Name: "coverage_newest", SQLType: "DATETIMEOFFSET"},
		{Name: "by", SQLType: "NVARCHAR"},
		{Name: "aggregate", SQLType: "NVARCHAR"},
		{Name: "top", SQLType: "INT"},
		{Name: "min_executions", SQLType: "BIGINT"},
		{Name: "include_internal", SQLType: "BIT"},
		{Name: "parent_module", SQLType: "NVARCHAR"},
	},
}

// datetimeOffsetFormat mirrors internal/output/cell.go's own
// DATETIMEOFFSET rendering exactly, so ranking's hand-built cells look
// like any other project-rendered timestamp. coverage.sql's
// oldest/newest columns are genuinely DATETIMEOFFSET (see
// CoverageTable's own doc comment: they were mislabeled DATETIME2 since
// task 9b, fixed alongside this), so ScanRow always renders them in
// exactly this one shape - there is no second format to fall back to.
const datetimeOffsetFormat = "2006-01-02T15:04:05.9999999Z07:00"

// formatDateTimeOffset renders t (already UTC; Window's own contract)
// the same way internal/output.ScanRow would render a real
// DATETIMEOFFSET column.
func formatDateTimeOffset(t time.Time) string {
	return t.UTC().Format(datetimeOffsetFormat)
}

// reformatCoverageCell turns one of coverage.sql's own Cell values (a
// DATETIMEOFFSET-shaped string, per cell.go's convention, or nil for
// "no interval at all") into ranking's own DATETIMEOFFSET-shaped form -
// the identity transform today (CoverageTable now declares the correct
// type), kept as its own function because Top also needs the parsed
// time.Time for the coverage_window comparison, not just the string.
//
// A non-nil, non-string cell is this package's own defect:
// coverage.sql's columns are genuinely datetimeoffset (no CAST needed),
// so ScanRow can only ever hand back a string or nil for these two.
func reformatCoverageCell(c model.Cell) (model.Cell, *time.Time, error) {
	if c == nil {
		return nil, nil, nil
	}
	s, ok := c.(string)
	if !ok {
		return nil, nil, &model.PublicError{Code: 5, Kind: "execution", Message: fmt.Sprintf("coverage timestamp: unexpected value %#v", c)}
	}
	t, err := time.Parse(datetimeOffsetFormat, s)
	if err != nil {
		return nil, nil, &model.PublicError{Code: 5, Kind: "execution", Message: fmt.Sprintf("parsing coverage timestamp %q: %s", s, err.Error())}
	}
	return formatDateTimeOffset(t), &t, nil
}

// readCoverage runs the package's own coverage.sql - already embedded
// for "qs status" (see health.go/embed.go's coverageQuery) - and
// returns the oldest/newest stored interval bounds both as ranking's
// DATETIMEOFFSET-shaped Cells and as parsed time.Time (nil/nil when
// Query Store holds no interval at all), reused rather than duplicated
// per this fix's own instruction.
func readCoverage(ctx context.Context, s *sqlserver.Session) (oldestCell, newestCell model.Cell, oldest, newest *time.Time, err error) {
	cells, err := queryOneRow(ctx, s.Conn, coverageQuery)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	oldestCell, oldest, err = reformatCoverageCell(cells[0])
	if err != nil {
		return nil, nil, nil, nil, err
	}
	newestCell, newest, err = reformatCoverageCell(cells[1])
	if err != nil {
		return nil, nil, nil, nil, err
	}
	return oldestCell, newestCell, oldest, newest, nil
}

// TopOptions is one resolved "qs top" request: the already-parsed
// Window (window.go's ParseWindow), the ranking metric/aggregate, an
// optional parent-module filter, the row cap, and the minimum-execution
// and internal-query filters.
type TopOptions struct {
	Window          Window
	By, Aggregate   string
	Object          string
	Top             int
	MinExecutions   int64
	IncludeInternal bool
}

// nonCollectingStates are the sys.database_query_store_options
// actual_state_desc values the design spec names (line 83) as needing a
// readable runtime history for "qs top"/"qs query" to permit analysis at
// all: OFF, READ_ONLY and ERROR. READ_WRITE (the ordinary collecting
// state) and READ_CAPTURE_SECONDARY (a readable secondary's own
// collecting state) are deliberately not in this set.
var nonCollectingStates = map[string]bool{"OFF": true, "READ_ONLY": true, "ERROR": true}

// orderColumns maps every valid (--by, --aggregate) combination to the
// one literal output column name that combination ranks by. Both inputs
// are already restricted to a fixed Enum by internal/cli's registry
// (never free text) by the time Top runs; --by=executions has no "avg"
// entry because --aggregate avg is rejected for it before connection
// (internal/cli's Parse), so that combination never reaches here.
var orderColumns = map[string]map[string]string{
	"cpu":        {"total": "cpu_total_ms", "avg": "cpu_avg_ms"},
	"duration":   {"total": "duration_total_ms", "avg": "duration_avg_ms"},
	"reads":      {"total": "reads_total", "avg": "reads_avg"},
	"executions": {"total": "executions"},
}

// orderColumnFor resolves --by/--aggregate to the literal SQL column
// name orderPlaceholder is substituted with. An unrecognized combination
// here is this package's own defect (every real caller's by/aggregate
// values are already Enum-validated by internal/cli before Top ever
// runs), not a user input to report as a flag error.
func orderColumnFor(by, aggregate string) (string, error) {
	byCols, ok := orderColumns[by]
	if !ok {
		return "", fmt.Errorf("diagnostics: unknown --by %q", by)
	}
	col, ok := byCols[aggregate]
	if !ok {
		return "", fmt.Errorf("diagnostics: --by %q does not support --aggregate %q", by, aggregate)
	}
	return col, nil
}

// topAllowedObjectTypes are the sys.objects.type codes "qs top"'s
// --object filter accepts: SQL stored procedures (P), scalar/inline-
// table/multi-statement-table functions (FN/IF/TF), and triggers (TR).
// A resolved table or any other object type is rejected with code 2
// (design spec: "Accept procedures, functions and triggers; reject a
// resolved table or other unsupported object type with code 2").
var topAllowedObjectTypes = map[string]bool{"P": true, "FN": true, "IF": true, "TF": true, "TR": true}

// captureModeMessage names mode (query_capture_mode_desc: ALL, NONE,
// AUTO or CUSTOM) in the "capture_mode" notice's text, distinguishing
// NONE (nothing new is being captured at all) from a merely selective
// mode (AUTO or CUSTOM, which may still miss some queries, but is not
// the same failure) - design spec: warn, never quantify what is
// missing. Worded generically ("this result"), not "this ranking":
// query.go's Query reuses this same message through
// emitQueryStoreNotices below, and a query detail is not a ranking.
func captureModeMessage(mode string) string {
	if mode == "NONE" {
		return "Query Store capture mode is NONE: no new queries are being captured"
	}
	return fmt.Sprintf("Query Store capture mode is %s: capture is selective, so this result may not include every query", mode)
}

// emitQueryStoreNotices writes the four independent health/coverage
// facts every Query Store command discloses (design spec: "Every Query
// Store command reads health first... Emit structured warnings for
// non-READ_WRITE state, capture restrictions, and requested history
// outside available coverage"), against table (the command's own
// primary table name, used only to label each Notice). Shared by Top
// and query.go's Query, rather than duplicated, because both commands
// read the exact same Health and coverage facts before running their
// own, different queries.
//
// The four facts are independent, never substituted for one another -
// see each one's own comment below for why: "capture" is about the
// engine's own collecting state (checked against READ_WRITE directly,
// not against nonCollectingStates, which deliberately excludes
// READ_CAPTURE_SECONDARY - a state this notice must still cover, since
// it is not READ_WRITE either even though it is not a code-4 state);
// "capture_mode" is the capture restriction the design spec names
// separately, independent of the collecting state itself (a healthy
// READ_WRITE database can still have capture mode NONE); "coverage" is
// about this database having no runtime history at all, ever;
// "coverage_window" is about a database that does have history, but
// not covering the requested window.
func emitQueryStoreNotices(dst model.Sink, table string, health Health, win Window, oldest, newest *time.Time) {
	if health.Actual != "READ_WRITE" {
		dst.Notice(model.Notice{
			Kind:    "capture",
			Message: fmt.Sprintf("Query Store is %s; this result may not reflect every execution in the requested window", health.Actual),
			Table:   table,
		})
	}
	if health.CaptureMode != "ALL" {
		dst.Notice(model.Notice{
			Kind:    "capture_mode",
			Message: captureModeMessage(health.CaptureMode),
			Table:   table,
		})
	}
	if !health.HasHistory {
		dst.Notice(model.Notice{
			Kind:    "coverage",
			Message: "this database has no Query Store runtime history yet",
			Table:   table,
		})
	}
	if oldest != nil && newest != nil && (win.Since.Before(*oldest) || win.Until.After(*newest)) {
		dst.Notice(model.Notice{
			Kind: "coverage_window",
			Message: fmt.Sprintf(
				"requested window [%s, %s) extends outside available coverage [%s, %s]",
				formatDateTimeOffset(win.Since), formatDateTimeOffset(win.Until),
				formatDateTimeOffset(*oldest), formatDateTimeOffset(*newest),
			),
			Table: table,
		})
	}
}

// Top runs "qs top": the exact Query Store ranking of queries by total
// or average CPU, duration, logical reads, or executions over
// opts.Window (design spec: "qs top": "Rank queries by total CPU,
// duration, logical reads, or executions").
//
// Every Query Store command reads health first (design spec). A state
// that cannot be collecting at all (OFF, READ_ONLY, ERROR) with no
// readable runtime history anywhere in the database fails at code 4 -
// never an empty ranking pretending the window was merely unmatched. A
// window that genuinely matches no rows, by contrast, is an empty
// ranking at code 0 (design spec: "no matching intervals within
// retained history returns an empty ranking, not unavailable").
func Top(ctx context.Context, s *sqlserver.Session, opts TopOptions, dst model.Sink) error {
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

	orderColumn, err := orderColumnFor(opts.By, opts.Aggregate)
	if err != nil {
		return &model.PublicError{Code: 5, Kind: "execution", Message: err.Error()}
	}

	var objectID sql.NullInt64
	var parentModule model.Cell // NULL unless --object resolved; never the raw --object argument.
	if opts.Object != "" {
		obj, err := sqlserver.Resolve(ctx, s.Conn, opts.Object)
		if err != nil {
			return err
		}
		if !topAllowedObjectTypes[obj.Type] {
			return &model.PublicError{
				Code:    2,
				Kind:    "invalid_argument",
				Message: fmt.Sprintf("--object %q resolves to object type %q: qs top accepts procedures, functions and triggers only", opts.Object, obj.Type),
			}
		}
		objectID = sql.NullInt64{Int64: obj.ID, Valid: true}
		parentModule = obj.Schema + "." + obj.Name
	}

	oldestCell, newestCell, oldest, newest, err := readCoverage(ctx, s)
	if err != nil {
		return err
	}

	// ReplaceAll, not Replace(...,1): both embedded files also mention
	// OrderPlaceholder once in their own doc comment, ahead of the real
	// ORDER BY occurrence - a count-limited replace would silently
	// consume that comment match instead and leave the ORDER BY clause's
	// own placeholder unresolved, exactly as measured against a real
	// server (SQL Server reports it as an undeclared scalar variable).
	query := strings.ReplaceAll(topQueryFor(s.Major), OrderPlaceholder, orderColumn)

	// See emitQueryStoreNotices' own doc comment for why these four
	// facts are independent and never substitute for one another.
	emitQueryStoreNotices(dst, TopQueriesTable.Name, health, opts.Window, oldest, newest)

	if err := dst.Begin(TopRankingTable); err != nil {
		return err
	}
	rankingRow := []model.Cell{
		formatDateTimeOffset(opts.Window.Since),
		formatDateTimeOffset(opts.Window.Until),
		oldestCell,
		newestCell,
		opts.By,
		opts.Aggregate,
		int64(opts.Top),
		opts.MinExecutions,
		opts.IncludeInternal,
		parentModule,
	}
	if err := dst.Row(rankingRow); err != nil {
		return err
	}
	if err := dst.End(true, true); err != nil {
		return err
	}

	if err := dst.Begin(TopQueriesTable); err != nil {
		return err
	}
	// queryRows streams straight into dst; the ranking table above must
	// still be written and closed first, since a Sink only ever holds
	// one table open at a time.
	if err := queryRows(ctx, s.Conn, query, dst,
		sql.Named("since", opts.Window.Since),
		sql.Named("until", opts.Window.Until),
		sql.Named("include_internal", opts.IncludeInternal),
		sql.Named("object_id", objectID),
		sql.Named("min_executions", opts.MinExecutions),
		sql.Named("top", opts.Top),
	); err != nil {
		return err
	}
	return dst.End(true, true)
}

// topQueryFor picks top_2019.sql or top_2022.sql by major version: 16
// (2022) and 17 (2025, smoke-tested only per the design spec) both carry
// rs.replica_group_id, so both use the 2022 form; only 15 (2019) ever
// reaches the 2019 branch - sqlserver.Open already rejects every other
// major version outright.
func topQueryFor(major int) string {
	if major >= 16 {
		return topQuery2022
	}
	return topQuery2019
}

// TopQuerySQL exposes the exact embedded query text Top runs for major -
// OrderPlaceholder still unresolved. tests/integration's synthetic
// aggregation fixture runs this real text (with only its FROM/JOIN
// table names substituted) against temp tables, rather than a
// hand-duplicated copy of the formula, so a defect introduced into this
// actual query is what that fixture is built to catch.
func TopQuerySQL(major int) string { return topQueryFor(major) }

// queryRows runs query (optionally parameterized, e.g. with sql.Named
// values) and streams every returned row straight to dst.Row as it is
// scanned through output.ScanRow - the same sanctioned
// SQL-value-to-model.Cell conversion queryOneRow (see info.go) uses for
// its single row. It never accumulates the result set into a slice
// first (design spec: "Stream table exports; never accumulate the full
// result set solely to produce a preview") - fix 1 measured that the
// previous accumulating form let a Sink that refused row 1 still pull
// every one of 20,000 rows off the wire first, a cost qs top's own
// bounded TOP(@top) always hid. The caller must already have called
// dst.Begin for the table these rows belong to; zero rows is not an
// error.
func queryRows(ctx context.Context, conn *sql.Conn, query string, dst model.Sink, args ...any) error {
	rows, err := conn.QueryContext(ctx, query, args...)
	if err != nil {
		return classifyQueryError(err, "ranking query failed")
	}
	defer rows.Close()

	var types []*sql.ColumnType
	for rows.Next() {
		if types == nil {
			types, err = rows.ColumnTypes()
			if err != nil {
				return &model.PublicError{Code: 5, Kind: "execution", Message: fmt.Sprintf("reading column types: %s", err.Error())}
			}
		}
		cells, err := output.ScanRow(rows, types)
		if err != nil {
			return &model.PublicError{Code: 5, Kind: "execution", Message: fmt.Sprintf("scanning ranking row: %s", err.Error())}
		}
		if err := dst.Row(cells); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return classifyQueryError(err, "reading ranking rows")
	}
	return nil
}
