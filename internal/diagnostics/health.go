package diagnostics

import (
	"context"
	"fmt"
	"strings"

	"github.com/rudi-bruchez/argosql/internal/model"
	"github.com/rudi-bruchez/argosql/internal/sqlserver"
)

// Health is every Query Store command's first read (design spec:
// "Every Query Store command reads health first"): just enough to
// decide whether Query Store and its runtime history are usable at
// all, before spending a second, command-specific query. Desired and
// Actual are sys.database_query_store_options' own
// desired_state_desc/actual_state_desc text (OFF, READ_ONLY,
// READ_WRITE, ERROR, READ_CAPTURE_SECONDARY); ReadOnlyReason is the
// raw bitmask DecodeReadOnly below interprets; HasHistory is a global
// fact - "does this database have any Query Store runtime row at
// all" - independent of any requested time window; a later command
// narrows by window on its own.
type Health struct {
	Desired, Actual string
	ReadOnlyReason  int64
	HasHistory      bool
}

// StatusTable and CoverageTable are the two tables "qs status" emits,
// in the design spec's declared order (status, coverage).
// internal/cli's registry uses these exact TableSpecs as "qs status"'s
// Command.Tables, so help's advertised schema and what Status actually
// writes can never drift apart.
var StatusTable = model.TableSpec{
	Name: "status",
	Columns: []model.Column{
		{Name: "desired_state", SQLType: "NVARCHAR"},
		{Name: "actual_state", SQLType: "NVARCHAR"},
		{Name: "readonly_reason", SQLType: "BIGINT"},
		{Name: "readonly_reason_decoded", SQLType: "NVARCHAR"},
		{Name: "capture_mode", SQLType: "NVARCHAR"},
		{Name: "current_storage_mb", SQLType: "DECIMAL(10,2)"},
		{Name: "max_storage_mb", SQLType: "DECIMAL(10,2)"},
		{Name: "retention_days", SQLType: "INT"},
		{Name: "interval_minutes", SQLType: "INT"},
	},
}

var CoverageTable = model.TableSpec{
	Name: "coverage",
	Columns: []model.Column{
		{Name: "oldest_interval", SQLType: "DATETIME2"},
		{Name: "newest_interval", SQLType: "DATETIME2"},
		{Name: "has_history", SQLType: "BIT"},
	},
}

// Positional indexes into the nine model.Cell values healthQuery
// (sql/health.sql) returns for its one row, named so healthFromCells
// and Status below never index that slice with a bare literal.
const (
	colDesired = iota
	colActual
	colReadOnlyReason
	colCaptureMode
	colCurrentStorageMB
	colMaxStorageMB
	colRetentionDays
	colIntervalMinutes
	colHasHistory
)

// queryOptionsRow runs sql/health.sql: sys.database_query_store_options'
// one row for the current database, plus the has_history fact, as the
// nine model.Cell values colDesired..colHasHistory above index into.
// Both ReadHealth and Status read through this one query - see
// sql/health.sql's own doc comment for why a single query serves both.
func queryOptionsRow(ctx context.Context, s *sqlserver.Session) ([]model.Cell, error) {
	return queryOneRow(ctx, s.Conn, healthQuery)
}

// healthFromCells narrows a queryOptionsRow result to the four fields
// Health carries. Every Query Store command needs only these to decide
// whether it can proceed; the display-only fields (capture mode,
// storage, retention, interval) are Status's alone to report.
func healthFromCells(cells []model.Cell) (Health, error) {
	desired, ok := cells[colDesired].(string)
	if !ok {
		return Health{}, unexpectedCell("desired_state")
	}
	actual, ok := cells[colActual].(string)
	if !ok {
		return Health{}, unexpectedCell("actual_state")
	}
	reason, ok := cells[colReadOnlyReason].(int64)
	if !ok {
		return Health{}, unexpectedCell("readonly_reason")
	}
	hasHistory, ok := cells[colHasHistory].(bool)
	if !ok {
		return Health{}, unexpectedCell("has_history")
	}
	return Health{Desired: desired, Actual: actual, ReadOnlyReason: reason, HasHistory: hasHistory}, nil
}

// unexpectedCell builds the code-5 error healthFromCells returns when
// a cell that sql/health.sql explicitly CASTs to a fixed SQL type
// still comes back as a Go type this package did not ask for - a
// defect in this package's own query or in internal/output's
// conversion, never a fact about the server worth reporting as
// permission or state information.
func unexpectedCell(column string) error {
	return &model.PublicError{Code: 5, Kind: "execution", Message: fmt.Sprintf("%s: unexpected value from sys.database_query_store_options", column)}
}

// ReadHealth is the guard every Query Store command runs before
// anything else (design spec: "Every Query Store command reads health
// first"). It never fails merely because Query Store is OFF, READ_ONLY
// or ERROR - those are facts this reports, not errors (design spec:
// "qs status succeeds ... including OFF, READ_ONLY or ERROR and no
// history"; the same health read backs every later Query Store
// command too). It only fails if the row itself could not be read at
// all: a permission, connection, or execution problem.
func ReadHealth(ctx context.Context, s *sqlserver.Session) (Health, error) {
	cells, err := queryOptionsRow(ctx, s)
	if err != nil {
		return Health{}, err
	}
	return healthFromCells(cells)
}

// knownReadOnlyBits maps each bit sys.database_query_store_options'
// readonly_reason column can set to a short snake_case token, taken
// verbatim from Microsoft's own documentation of that column:
// https://learn.microsoft.com/en-us/sql/relational-databases/system-catalog-views/sys-database-query-store-options-transact-sql#columns
// - never from a SELECT * sample against one observed server. A bit
// this program has never seen on a live server is exactly the case
// this table exists to still name correctly, via the
// "unknown_reason_bit_N" fallback in DecodeReadOnly below, rather than
// silently dropping it.
var knownReadOnlyBits = map[int64]string{
	1:      "database_read_only",            // the database itself is in read-only mode
	2:      "database_single_user",          // the database is in single-user mode
	4:      "database_emergency_mode",       // the database is in emergency mode
	8:      "secondary_replica",             // AG secondary replica, or geo-replication secondary
	65536:  "storage_limit_reached",         // Query Store reached max_storage_size_mb
	131072: "statement_count_limit_reached", // distinct statements reached the internal memory limit
	262144: "memory_limit_reached",          // in-memory items awaiting disk persistence reached the internal memory limit
	524288: "disk_size_limit_reached",       // the database itself reached its disk size limit
}

// DecodeReadOnly turns h's raw ReadOnlyReason into a nonempty list of
// tokens.
//
// Zero is not "no reason" by itself (design spec: "Never infer
// deliberate read-only configuration from zero alone"): it means
// configured_read_only only when both desired and actual state are
// READ_ONLY, none only when actual is READ_WRITE, and
// no_reason_reported for every other state (OFF, ERROR,
// READ_CAPTURE_SECONDARY, or a READ_ONLY/READ_WRITE combination the
// first two cases do not cover) - exactly the brief's TestReasonZero
// case and its siblings below.
//
// Nonzero is decoded bit by bit, low bit first, every set bit reported
// independently (design spec: "Decode nonzero values as a bitmask,
// retain the raw integer, and expose unknown bits numerically") - a
// known bit by its documented token, an unknown one as
// unknown_reason_bit_<N>, never silently dropped.
func DecodeReadOnly(h Health) []string {
	if h.ReadOnlyReason == 0 {
		if h.Actual == "READ_ONLY" && h.Desired == "READ_ONLY" {
			return []string{"configured_read_only"}
		}
		if h.Actual == "READ_WRITE" {
			return []string{"none"}
		}
		return []string{"no_reason_reported"}
	}
	var reasons []string
	for i := 0; i < 64; i++ {
		bit := int64(1) << i
		if h.ReadOnlyReason&bit == 0 {
			continue
		}
		if token, known := knownReadOnlyBits[bit]; known {
			reasons = append(reasons, token)
		} else {
			reasons = append(reasons, fmt.Sprintf("unknown_reason_bit_%d", bit))
		}
	}
	return reasons
}

// Status reports "qs status"'s two tables, in the design spec's
// declared order: status (desired/actual state, raw and decoded
// read-only reason, capture mode, storage usage/limit,
// retention/interval settings) then coverage (oldest/newest stored
// interval, has_history). It succeeds at code 0 for every Query Store
// state the engine can report - OFF, READ_ONLY and ERROR included
// (design spec: "qs status succeeds with code 0 whenever it can read
// its required status fields, including OFF, READ_ONLY or ERROR and
// no history") - because qs status exists specifically to report those
// states; failing on one of them would defeat its own purpose.
func Status(ctx context.Context, s *sqlserver.Session, dst model.Sink) error {
	optionCells, err := queryOptionsRow(ctx, s)
	if err != nil {
		return err
	}
	health, err := healthFromCells(optionCells)
	if err != nil {
		return err
	}
	decoded := strings.Join(DecodeReadOnly(health), "; ")

	statusRow := []model.Cell{
		optionCells[colDesired],
		optionCells[colActual],
		optionCells[colReadOnlyReason],
		decoded,
		optionCells[colCaptureMode],
		optionCells[colCurrentStorageMB],
		optionCells[colMaxStorageMB],
		optionCells[colRetentionDays],
		optionCells[colIntervalMinutes],
	}
	if err := dst.Begin(StatusTable); err != nil {
		return err
	}
	if err := dst.Row(statusRow); err != nil {
		return err
	}
	if err := dst.End(true, true); err != nil {
		return err
	}

	coverageCells, err := queryOneRow(ctx, s.Conn, coverageQuery)
	if err != nil {
		return err
	}
	coverageRow := []model.Cell{coverageCells[0], coverageCells[1], health.HasHistory}
	if err := dst.Begin(CoverageTable); err != nil {
		return err
	}
	if err := dst.Row(coverageRow); err != nil {
		return err
	}
	return dst.End(true, true)
}
