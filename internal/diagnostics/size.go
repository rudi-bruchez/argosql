package diagnostics

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"sort"

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
// tableHeaderRowCount could not produce a real row count for obj -
// unlike Table (table.go), which degrades on the exact same three
// reasons, Size itself requires a real row count (design spec line
// 172: "size table itself requires those permissions"), so every one
// of tableHeaderRowCount's non-nil reasons is this command's own
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

// Size runs "size table <schema.name>": obj's approximate row count
// and its allocated/used/reserved space, broken down by index and
// allocation type (design spec: "size table": "Approximate row count
// and allocated/used/reserved space, with index and allocation-type
// breakdowns that avoid double counting").
//
// Resolution precedes everything else (design spec line 172), exactly
// like Table and Indexes: an unresolved name fails at code 8 before
// this function ever runs a query that could otherwise fail at code 4
// for an unrelated permission reason. Once obj resolves, this command
// itself REQUIRES a real row count - unlike Table, which degrades on
// the identical unavailability - so any of tableHeaderRowCount's three
// reasons ends Size outright via sizeUnavailableError.
func Size(ctx context.Context, s *sqlserver.Session, name string, dst model.Sink) error {
	obj, err := sqlserver.Resolve(ctx, s.Conn, name)
	if err != nil {
		return err
	}

	rows, reason, err := tableHeaderRowCount(ctx, s, obj)
	if err != nil {
		return err
	}
	if reason != rowCountReasonNone {
		return sizeUnavailableError(obj, reason)
	}

	if err := dst.Begin(TableTable); err != nil {
		return err
	}
	if err := dst.Row([]model.Cell{obj.ID, obj.Schema, obj.Name, rows}); err != nil {
		return err
	}
	if err := dst.End(true, true); err != nil {
		return err
	}

	return writeAllocations(ctx, s, obj, dst)
}

// allocationRow is one row this program will hand to AllocationsTable,
// held in memory just long enough for writeAllocations to sort the
// whole (small, bounded) set before writing any of it out.
type allocationRow struct {
	indexID, partitionNumber int64
	allocationType           string
	usedPages, reservedPages int64
}

// writeAllocations reads every row sql/size.sql returns for obj,
// computes used_bytes/reserved_bytes from the raw page counts (see
// pageBytes above), and writes them to AllocationsTable ordered by
// (index_id, partition_number, allocation_type) - the design spec's
// own declared row order (line 103: "allocation rows use index_id,
// partition_number, allocation type"). The order is enforced here in
// Go, with sort.SliceStable, rather than trusted to size.sql's own
// ORDER BY alone: this table is small and bounded by construction (at
// most three allocation types per index/partition, and this
// program's diagnostics never inspect a table with an unbounded
// number of indexes or partitions), so accumulating it in memory to
// guarantee the order costs nothing worth avoiding, and it is what
// makes the order independently verifiable and independently
// breakable - see size_test.go's TestSizeAllocationsOrderedAcrossIndexes,
// which feeds this function driver rows in scrambled order and would
// not catch a lost ORDER BY in size.sql alone, only a lost sort here.
func writeAllocations(ctx context.Context, s *sqlserver.Session, obj sqlserver.Object, dst model.Sink) error {
	rows, err := s.Conn.QueryContext(ctx, sizeQuery, sql.Named("id", obj.ID))
	if err != nil {
		return classifyQueryError(err, "allocation query failed")
	}
	defer rows.Close()

	var collected []allocationRow
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
		indexID, ok := cells[0].(int64)
		if !ok {
			return unexpectedCell("index_id")
		}
		partitionNumber, ok := cells[1].(int64)
		if !ok {
			return unexpectedCell("partition_number")
		}
		allocationType, ok := cells[2].(string)
		if !ok {
			return unexpectedCell("allocation_type")
		}
		usedPages, ok := cells[3].(int64)
		if !ok {
			return unexpectedCell("used_pages")
		}
		reservedPages, ok := cells[4].(int64)
		if !ok {
			return unexpectedCell("reserved_pages")
		}
		collected = append(collected, allocationRow{
			indexID: indexID, partitionNumber: partitionNumber, allocationType: allocationType,
			usedPages: usedPages, reservedPages: reservedPages,
		})
	}
	if err := rows.Err(); err != nil {
		return classifyQueryError(err, "reading allocation rows")
	}

	sort.SliceStable(collected, func(i, j int) bool {
		a, b := collected[i], collected[j]
		if a.indexID != b.indexID {
			return a.indexID < b.indexID
		}
		if a.partitionNumber != b.partitionNumber {
			return a.partitionNumber < b.partitionNumber
		}
		return a.allocationType < b.allocationType
	})

	if err := dst.Begin(AllocationsTable); err != nil {
		return err
	}
	for _, a := range collected {
		row := []model.Cell{
			a.indexID, a.partitionNumber, a.allocationType,
			a.usedPages, a.reservedPages,
			a.usedPages * pageBytes, a.reservedPages * pageBytes,
		}
		if err := dst.Row(row); err != nil {
			return err
		}
	}
	return dst.End(true, true)
}
