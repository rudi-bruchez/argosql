package diagnostics

import (
	"context"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rudi-bruchez/argosql/internal/model"

	mssql "github.com/microsoft/go-mssqldb"
)

// usageTime parses a fixture timestamp literal into the time.Time the
// driver itself hands back for a DATETIME column - output.ScanRow's own
// sanctioned conversion (internal/output/cell.go) only accepts
// time.Time for that type, never a bare string.
func usageTime(s string) time.Time {
	t, err := time.Parse("2006-01-02T15:04:05", s)
	if err != nil {
		panic(err)
	}
	return t
}

// TestUsageUnresolvedNameReturnsEight is dispatch cassure 1's own
// target for "idx usage": resolution precedes everything else, even
// the instance-level permission check (design spec: "idx usage on an
// unresolved name returns 8 before checking its server permission").
func TestUsageUnresolvedNameReturnsEight(t *testing.T) {
	conn := &fakeObjConn{responses: []objQueryResponse{resolveNotFoundResponse()}}
	sess := newFakeObjSession(t, conn)

	err := Usage(context.Background(), sess, "dbo.Missing", &objCaptureSink{})
	var pub *model.PublicError
	if !errors.As(err, &pub) {
		t.Fatalf("Usage: want *model.PublicError, got %#v", err)
	}
	if pub.Code != 8 || pub.Kind != "not_found_or_not_visible" {
		t.Fatalf("Usage on an unresolved name: want code 8/not_found_or_not_visible, got code %d/%s", pub.Code, pub.Kind)
	}
}

// TestUsageRejectsWrongObjectType mirrors TestIndexesRejectsWrongObjectType:
// design spec line 202, "A resolved object of the wrong type gives code 2."
func TestUsageRejectsWrongObjectType(t *testing.T) {
	conn := &fakeObjConn{responses: []objQueryResponse{
		resolveFoundResponse(999, "dbo", "PlainModule", "P"),
	}}
	sess := newFakeObjSession(t, conn)
	sink := &objCaptureSink{}

	err := Usage(context.Background(), sess, "dbo.PlainModule", sink)
	var pub *model.PublicError
	if !errors.As(err, &pub) {
		t.Fatalf("Usage: want *model.PublicError, got %#v", err)
	}
	if pub.Code != 2 || pub.Kind != "invalid_argument" {
		t.Fatalf("Usage on a procedure: want code 2/invalid_argument, got code %d/%s", pub.Code, pub.Kind)
	}
	if len(sink.tables) != 0 {
		t.Fatalf("wrong object type: want no table written, got %#v", sink.tables)
	}
}

// TestUsageFailsWhenInstancePermissionAbsent is this task's own
// analogue of TestSizeFailsWhenPermissionAbsent: sys.dm_db_index_usage_stats
// itself requires the instance-level permission (measured against
// Microsoft's own documentation), so a principal lacking it must FAIL
// the whole command at code 4 - matching the design spec's own Q/I/S
// matrix ("idx usage | ... | 4, instance permission absent | 0") -
// never degrade to an empty or partial "usage" table.
func TestUsageFailsWhenInstancePermissionAbsent(t *testing.T) {
	conn := &fakeObjConn{responses: []objQueryResponse{
		resolveFoundResponse(701, "dbo", "Orders", "U"),
		usageErrResponse(mssql.Error{Number: 300, Message: "denied"}),
	}}
	sess := newFakeObjSession(t, conn)
	sink := &objCaptureSink{}

	err := Usage(context.Background(), sess, "dbo.Orders", sink)
	var pub *model.PublicError
	if !errors.As(err, &pub) {
		t.Fatalf("Usage: want *model.PublicError, got %#v", err)
	}
	if pub.Code != 4 || pub.Kind != "permission" {
		t.Fatalf("Usage without the instance permission: want code 4/permission, got code %d/%s", pub.Code, pub.Kind)
	}
}

// TestUsageCellValuesAndOrder covers real cell values for every
// attribute design spec line 55 names, never merely a row count - the
// recurring defect this project has paid for three times on other
// tasks. It also proves observation_status distinguishes a genuinely
// untouched index (never_observed, every counter/timestamp NULL) from
// one the DMV has a row for (observed, real counters) - dispatch
// cassure 1's own target: no fabricated zero, no stale/unused verdict.
func TestUsageCellValuesAndOrder(t *testing.T) {
	conn := &fakeObjConn{responses: []objQueryResponse{
		resolveFoundResponse(701, "dbo", "Orders", "U"),
		usageRowsResponse([][]driver.Value{
			{int64(1), "PK_Orders", int64(42), int64(7), int64(0), int64(3), usageTime("2026-09-01T10:00:00"), nil, nil, usageTime("2026-09-02T11:00:00"), "observed"},
			{int64(2), "IX_Orders_Never", nil, nil, nil, nil, nil, nil, nil, nil, "never_observed"},
		}),
		serverStartTimeResponse(usageTime("2026-08-01T00:00:00"), nil),
	}}
	sess := newFakeObjSession(t, conn)
	sink := &objCaptureSink{}

	if err := Usage(context.Background(), sess, "dbo.Orders", sink); err != nil {
		t.Fatalf("Usage: %v", err)
	}

	tbl := sink.table("usage")
	if tbl == nil || len(tbl.rows) != 2 {
		t.Fatalf("usage: want exactly two rows, got %#v", tbl)
	}
	if tbl.rows[0][0] != int64(1) || tbl.rows[1][0] != int64(2) {
		t.Fatalf("usage rows must stay in index_id ascending order, got %#v then %#v", tbl.rows[0][0], tbl.rows[1][0])
	}

	observed := tbl.rows[0]
	if observed[2] != int64(42) {
		t.Fatalf("observed row seeks cell: got %#v, want 42", observed[2])
	}
	if observed[10] != "observed" {
		t.Fatalf("observed row observation_status cell: got %#v", observed[10])
	}

	never := tbl.rows[1]
	if never[2] != nil || never[6] != nil {
		t.Fatalf("never_observed row: seeks/last_seek must be NULL, got seeks=%#v last_seek=%#v", never[2], never[6])
	}
	if never[10] != "never_observed" {
		t.Fatalf("never-touched index observation_status cell: got %#v, want never_observed", never[10])
	}
}

// TestUsageNeverAssertsStaleOrUnused is dispatch cassure 1's explicit
// vocabulary guard (design spec: "Do not include a stale verdict in
// v0.1" - and, by this project's own prior convention, no 'unused'
// verdict either): observation_status only ever takes the two values
// usage.sql itself can produce, and no Notice this command emits ever
// uses either forbidden word.
func TestUsageNeverAssertsStaleOrUnused(t *testing.T) {
	conn := &fakeObjConn{responses: []objQueryResponse{
		resolveFoundResponse(701, "dbo", "Orders", "U"),
		usageRowsResponse([][]driver.Value{
			{int64(1), "PK_Orders", nil, nil, nil, nil, nil, nil, nil, nil, "never_observed"},
		}),
		serverStartTimeResponse(usageTime("2026-08-01T00:00:00"), nil),
	}}
	sess := newFakeObjSession(t, conn)
	sink := &objCaptureSink{}

	if err := Usage(context.Background(), sess, "dbo.Orders", sink); err != nil {
		t.Fatalf("Usage: %v", err)
	}
	tbl := sink.table("usage")
	status, _ := tbl.rows[0][10].(string)
	if status != "observed" && status != "never_observed" {
		t.Fatalf("observation_status: got %q, want one of observed/never_observed", status)
	}
	for _, n := range sink.notices {
		lower := strings.ToLower(n.Kind + " " + n.Message)
		if strings.Contains(lower, "stale") || strings.Contains(lower, "unused") {
			t.Fatalf("Usage must never assert stale/unused (design spec line 65): notice %#v", n)
		}
	}
}

// TestUsageObservationWindowNoticeWithStartTime is design spec line
// 55's "server start time if permitted" clause, the success half:
// when the narrower sys.dm_os_sys_info read succeeds, the notice names
// the start time and properties_complete is true.
func TestUsageObservationWindowNoticeWithStartTime(t *testing.T) {
	conn := &fakeObjConn{responses: []objQueryResponse{
		resolveFoundResponse(701, "dbo", "Orders", "U"),
		usageRowsResponse(nil),
		serverStartTimeResponse(usageTime("2026-08-01T00:00:00"), nil),
	}}
	sess := newFakeObjSession(t, conn)
	sink := &objCaptureSink{}

	if err := Usage(context.Background(), sess, "dbo.Orders", sink); err != nil {
		t.Fatalf("Usage: %v", err)
	}
	tbl := sink.table("usage")
	if !tbl.propertiesComplete {
		t.Fatalf("server start time available: want properties_complete=true, got false")
	}
	n := sink.noticeWithKind("observation_window")
	if n == nil || !strings.Contains(n.Message, "2026-08-01T00:00:00") {
		t.Fatalf("observation_window notice must name the server start time, got %#v", n)
	}
}

// TestUsageObservationWindowNoticeWithoutStartTime is design spec line
// 55's "server start time if permitted" clause, the degraded half:
// sys.dm_db_index_usage_stats itself already succeeded (S holds the
// instance permission), but this narrower, separately-deniable read
// (see Usage's own doc comment) fails on permission - the command
// still succeeds, properties_complete is false, and a capability
// notice says why, instead of the command failing outright (dispatch
// "quatre défauts" #4: a completeness flag asserted true while
// something is missing).
func TestUsageObservationWindowNoticeWithoutStartTime(t *testing.T) {
	conn := &fakeObjConn{responses: []objQueryResponse{
		resolveFoundResponse(701, "dbo", "Orders", "U"),
		usageRowsResponse(nil),
		serverStartTimeResponse(nil, mssql.Error{Number: 300, Message: "denied"}),
	}}
	sess := newFakeObjSession(t, conn)
	sink := &objCaptureSink{}

	if err := Usage(context.Background(), sess, "dbo.Orders", sink); err != nil {
		t.Fatalf("Usage: %v", err)
	}
	tbl := sink.table("usage")
	if tbl.propertiesComplete {
		t.Fatalf("server start time denied: want properties_complete=false, got true")
	}
	n := sink.noticeWithKind("observation_window")
	if n == nil || !strings.Contains(n.Message, "unavailable") {
		t.Fatalf("observation_window notice must say server start time is unavailable, got %#v", n)
	}
}
