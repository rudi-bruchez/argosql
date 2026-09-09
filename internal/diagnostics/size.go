package diagnostics

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"

	"github.com/rudi-bruchez/argosql/internal/model"
	"github.com/rudi-bruchez/argosql/internal/output"
	"github.com/rudi-bruchez/argosql/internal/sqlserver"
)

// sizeQuery is sql/size.sql (see its own doc comment for what it reads
// and why) - embedded here, rather than in embed.go, for the same
// reason query.go and top.go embed their own sql/*.sql locally: it is
// this file's own concern alone.
//
//go:embed sql/size.sql
var sizeQuery string

// pageBytes is SQL Server's fixed page size, 8 KB, never configurable:
// design spec line 174, "Label bytes as pages * 8192".
const pageBytes = 8192

// mibBytes is the divisor design spec line 174 names for MiB
// ("MiB as bytes / 1048576"). It is documented here, and in this
// command's registered Units, rather than materialized as a column
// this task's declared allocations schema does not carry - an agent
// consuming used_bytes/reserved_bytes divides by this itself.
const mibBytes = 1048576

// AllocationsTable is "size table"'s second table (design spec's
// declared table order: "size table: table, allocations"). One row
// per (index_id, partition_number, allocation_type) actually present
// - see sql/size.sql's own doc comment for why a category with no
// pages at all is never given a row. used_bytes/reserved_bytes are
// used_pages/reserved_pages * pageBytes, computed here in Go rather
// than in SQL, so the one multiplication this task's spec names
// (line 174) has exactly one place it can be wrong.
var AllocationsTable = model.TableSpec{
	Name: "allocations",
	Columns: []model.Column{
		{Name: "index_id", SQLType: "INT"},
		{Name: "partition_number", SQLType: "INT"},
		{Name: "allocation_type", SQLType: "NVARCHAR"},
		{Name: "used_pages", SQLType: "BIGINT"},
		{Name: "reserved_pages", SQLType: "BIGINT"},
		{Name: "used_bytes", SQLType: "BIGINT"},
		{Name: "reserved_bytes", SQLType: "BIGINT"},
	},
}

// sizeUnavailableError builds the code-4 error Size returns when
// tableHeaderRowCount could not produce real size facts for obj -
// unlike Table (table.go), which degrades on the exact same three
// reasons, Size itself requires them (design spec line 172: "size
// table itself requires those permissions"), so every one of
// tableHeaderRowCount's non-nil reasons is this command's own
// failure, never a warning on an otherwise-successful result.
func sizeUnavailableError(obj sqlserver.Object, reason rowCountUnavailableReason) error {
	switch reason {
	case rowCountReasonMemoryOptimized:
		return &model.PublicError{
			Code:    4,
			Kind:    "memory_optimized_unavailable",
			Message: fmt.Sprintf("%s.%s is memory-optimized: size collection is unavailable in v0.1", obj.Schema, obj.Name),
		}
	case rowCountReasonNotApplicable:
		return &model.PublicError{
			Code:    4,
			Kind:    "size_unavailable",
			Message: fmt.Sprintf("%s.%s has no sys.dm_db_partition_stats row: size does not apply to this object type", obj.Schema, obj.Name),
		}
	default:
		return &model.PublicError{
			Code:    4,
			Kind:    "permission",
			Message: fmt.Sprintf("%s.%s: size permissions absent (sys.dm_db_partition_stats denied)", obj.Schema, obj.Name),
		}
	}
}

// Size runs "size table <schema.name>": obj's approximate row count,
// its total used/reserved/unused space, and its allocated space
// broken down by index and allocation type (design spec: "size
// table": "Approximate row count and allocated/used/reserved space,
// with index and allocation-type breakdowns that avoid double
// counting").
//
// Resolution precedes everything else (design spec line 172), exactly
// like Table and Indexes. A resolved object of the wrong type is
// rejected at code 2 immediately after (design spec line 202) - task
// 13 fix-1 measured that a procedure used to resolve and then fail at
// code 4 (memory_optimized/size_unavailable) here instead of naming
// the type mismatch, since neither reason actually applied. Once obj
// resolves and its type is accepted, this command itself REQUIRES a
// real row count/size - unlike Table, which degrades on the identical
// unavailability - so any of tableHeaderRowCount's three reasons ends
// Size outright via sizeUnavailableError.
func Size(ctx context.Context, s *sqlserver.Session, name string, dst model.Sink) error {
	obj, err := sqlserver.Resolve(ctx, s.Conn, name)
	if err != nil {
		return err
	}
	if !tableAllowedTypes[obj.Type] {
		return wrongObjectTypeError(obj, "size table", "tables")
	}

	header, reason, err := tableHeaderRowCount(ctx, s, obj)
	if err != nil {
		return err
	}
	if reason != rowCountReasonNone {
		return sizeUnavailableError(obj, reason)
	}

	if err := dst.Begin(TableTable); err != nil {
		return err
	}
	row := []model.Cell{obj.ID, obj.Schema, obj.Name, header.RowCount, header.TotalUsedBytes, header.TotalReservedBytes, header.TotalUnusedBytes}
	if err := dst.Row(row); err != nil {
		return err
	}
	if err := dst.End(true, true); err != nil {
		return err
	}

	return writeAllocations(ctx, s, obj, dst)
}

// writeAllocations streams sql/size.sql's rows straight into
// AllocationsTable, computing used_bytes/reserved_bytes from the raw
// page counts (see pageBytes above) as it goes - never accumulating
// the result set first (design spec line 111: "Stream table exports;
// never accumulate the full result set"). This is task 13 fix-1's own
// correction of a defect measured to repeat a pattern already fixed
// once on this project (queryRows, task 11): the earlier version of
// this function read every row into a slice and sorted it before the
// first dst.Row call, with nothing bounding how many rows that could
// be. Row order now comes entirely from sql/size.sql's own ORDER BY
// (index_id, partition_number, allocation_type - the design spec's
// declared row order, line 103), the same trust queryRows (top.go)
// already places in its own callers' ORDER BY clauses.
func writeAllocations(ctx context.Context, s *sqlserver.Session, obj sqlserver.Object, dst model.Sink) error {
	rows, err := s.Conn.QueryContext(ctx, sizeQuery, sql.Named("id", obj.ID))
	if err != nil {
		return classifyQueryError(err, "allocation query failed")
	}
	defer rows.Close()

	if err := dst.Begin(AllocationsTable); err != nil {
		return err
	}

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
			return &model.PublicError{Code: 5, Kind: "execution", Message: fmt.Sprintf("scanning allocation row: %s", err.Error())}
		}
		usedPages, ok := cells[3].(int64)
		if !ok {
			return unexpectedCell("used_pages")
		}
		reservedPages, ok := cells[4].(int64)
		if !ok {
			return unexpectedCell("reserved_pages")
		}
		row := []model.Cell{
			cells[0], cells[1], cells[2],
			usedPages, reservedPages,
			usedPages * pageBytes, reservedPages * pageBytes,
		}
		if err := dst.Row(row); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return classifyQueryError(err, "reading allocation rows")
	}
	return dst.End(true, true)
}
