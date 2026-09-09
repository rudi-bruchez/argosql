package diagnostics

import (
	"context"
	"database/sql/driver"
	"errors"
	"testing"

	"github.com/rudi-bruchez/argosql/internal/model"
)

// TestIndexesUnresolvedNameReturnsEight is dispatch cassure 1's own
// target for "idx list": resolution precedes everything else (design
// spec line 172).
func TestIndexesUnresolvedNameReturnsEight(t *testing.T) {
	conn := &fakeObjConn{responses: []objQueryResponse{resolveNotFoundResponse()}}
	sess := newFakeObjSession(t, conn)

	err := Indexes(context.Background(), sess, "dbo.Missing", &objCaptureSink{})
	var pub *model.PublicError
	if !errors.As(err, &pub) {
		t.Fatalf("Indexes: want *model.PublicError, got %#v", err)
	}
	if pub.Code != 8 || pub.Kind != "not_found_or_not_visible" {
		t.Fatalf("Indexes on an unresolved name: want code 8/not_found_or_not_visible, got code %d/%s", pub.Code, pub.Kind)
	}
}

// TestIndexesCellValues covers real cell values across every
// attribute the design spec names for "idx list" (name/type, ordered
// keys with direction, included columns, filter, uniqueness, disabled
// state) - not merely a row count, the recurring defect this project
// has paid for three times on other tasks.
func TestIndexesCellValues(t *testing.T) {
	conn := &fakeObjConn{responses: []objQueryResponse{
		resolveFoundResponse(701, "dbo", "ColumnsFixture", "U"),
		indexesRowsResponse([][]driver.Value{
			{int64(1), "PK_ColumnsFixture", "CLUSTERED", "[Id] ASC", nil, nil, true, false},
			{int64(2), "IX_ColumnsFixture_Composite", "NONCLUSTERED", "[Quantity] DESC, [Price] ASC", "[Note]", nil, false, false},
			{int64(3), "UX_ColumnsFixture_Note", "NONCLUSTERED", "[Note] ASC", nil, "([Note] IS NOT NULL)", true, false},
			{int64(4), "IX_ColumnsFixture_Disabled", "NONCLUSTERED", "[Quantity] ASC", nil, nil, false, true},
		}),
	}}
	sess := newFakeObjSession(t, conn)
	sink := &objCaptureSink{}

	if err := Indexes(context.Background(), sess, "dbo.ColumnsFixture", sink); err != nil {
		t.Fatalf("Indexes: %v", err)
	}

	tbl := sink.table("indexes")
	if tbl == nil || len(tbl.rows) != 4 {
		t.Fatalf("indexes: want exactly four rows, got %#v", tbl)
	}

	composite := tbl.rows[1]
	if composite[3] != "[Quantity] DESC, [Price] ASC" {
		t.Fatalf("composite index keys cell: got %#v", composite[3])
	}
	if composite[4] != "[Note]" {
		t.Fatalf("composite index includes cell: got %#v", composite[4])
	}

	unique := tbl.rows[2]
	if unique[5] != "([Note] IS NOT NULL)" {
		t.Fatalf("filtered index filter cell: got %#v", unique[5])
	}
	if unique[6] != true {
		t.Fatalf("unique index unique cell: got %#v, want true", unique[6])
	}

	disabled := tbl.rows[3]
	if disabled[7] != true {
		t.Fatalf("disabled index disabled cell: got %#v, want true", disabled[7])
	}
}

// TestTableAndIndexesShareTheSameReader proves this task's own
// requirement (brief: "Table appelle le même lecteur privé d'index
// que Indexes, sans invoquer le CLI récursivement"): given the exact
// same fixture, Table's own "indexes" table and Indexes' "indexes"
// table hold identical rows - both go through readIndexes, never
// through two independently written queries that could silently
// drift apart.
func TestTableAndIndexesShareTheSameReader(t *testing.T) {
	indexRows := [][]driver.Value{
		{int64(1), "PK_Orders", "CLUSTERED", "[OrderId] ASC", nil, nil, true, false},
	}

	tableConn := &fakeObjConn{responses: []objQueryResponse{
		resolveFoundResponse(801, "dbo", "Orders", "U"),
		tableRowResponse(int64(2), nil),
		columnsRowsResponse(nil),
		indexesRowsResponse(indexRows),
	}}
	tableSess := newFakeObjSession(t, tableConn)
	tableSink := &objCaptureSink{}
	if err := Table(context.Background(), tableSess, "dbo.Orders", tableSink); err != nil {
		t.Fatalf("Table: %v", err)
	}

	indexesConn := &fakeObjConn{responses: []objQueryResponse{
		resolveFoundResponse(801, "dbo", "Orders", "U"),
		indexesRowsResponse(indexRows),
	}}
	indexesSess := newFakeObjSession(t, indexesConn)
	indexesSink := &objCaptureSink{}
	if err := Indexes(context.Background(), indexesSess, "dbo.Orders", indexesSink); err != nil {
		t.Fatalf("Indexes: %v", err)
	}

	fromTable := tableSink.table("indexes")
	fromIndexes := indexesSink.table("indexes")
	if fromTable == nil || fromIndexes == nil {
		t.Fatalf("both commands must write an indexes table: table=%v indexes=%v", fromTable, fromIndexes)
	}
	if len(fromTable.rows) != len(fromIndexes.rows) {
		t.Fatalf("row count differs: Table wrote %d, Indexes wrote %d", len(fromTable.rows), len(fromIndexes.rows))
	}
	for i := range fromTable.rows {
		for j := range fromTable.rows[i] {
			if fromTable.rows[i][j] != fromIndexes.rows[i][j] {
				t.Fatalf("row %d cell %d differs: Table=%#v Indexes=%#v", i, j, fromTable.rows[i][j], fromIndexes.rows[i][j])
			}
		}
	}
}
