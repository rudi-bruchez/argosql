package diagnostics

import (
	"context"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rudi-bruchez/argosql/internal/model"
)

// statsTime mirrors usage_test.go's own usageTime: output.ScanRow only
// accepts time.Time for a DATETIME2 column, never a bare string.
func statsTime(s string) time.Time {
	tm, err := time.Parse("2006-01-02T15:04:05", s)
	if err != nil {
		panic(err)
	}
	return tm
}

// TestStatisticsTableHasTwelveColumnsInOrder is the brief's own named
// assertion that the extension past sql/stats.sql's six-column
// skeleton actually happened: stats_id, name, columns, rows,
// rows_sampled, sample_pct, last_updated, modification_counter,
// auto_created, user_created, filter, properties_status, in this exact
// order. A panel of reviewers on this project measured three false
// positives from exactly this shortcut - accepting the skeleton block
// verbatim instead of checking the extension landed.
func TestStatisticsTableHasTwelveColumnsInOrder(t *testing.T) {
	want := []string{
		"stats_id", "name", "columns", "rows", "rows_sampled", "sample_pct",
		"last_updated", "modification_counter", "auto_created", "user_created", "filter", "properties_status",
	}
	if len(StatisticsTable.Columns) != len(want) {
		t.Fatalf("StatisticsTable: want %d columns, got %d: %#v", len(want), len(StatisticsTable.Columns), StatisticsTable.Columns)
	}
	for i, name := range want {
		if StatisticsTable.Columns[i].Name != name {
			t.Fatalf("StatisticsTable column %d: want %q, got %q", i, name, StatisticsTable.Columns[i].Name)
		}
	}
}

// TestStatsUnresolvedNameReturnsEight is dispatch cassure 1's own
// target for "stats list": resolution precedes everything else.
func TestStatsUnresolvedNameReturnsEight(t *testing.T) {
	conn := &fakeObjConn{responses: []objQueryResponse{resolveNotFoundResponse()}}
	sess := newFakeObjSession(t, conn)

	err := Stats(context.Background(), sess, "dbo.Missing", &objCaptureSink{})
	var pub *model.PublicError
	if !errors.As(err, &pub) {
		t.Fatalf("Stats: want *model.PublicError, got %#v", err)
	}
	if pub.Code != 8 || pub.Kind != "not_found_or_not_visible" {
		t.Fatalf("Stats on an unresolved name: want code 8/not_found_or_not_visible, got code %d/%s", pub.Code, pub.Kind)
	}
}

// TestStatsRejectsWrongObjectType mirrors every other object-name
// command's own test.
func TestStatsRejectsWrongObjectType(t *testing.T) {
	conn := &fakeObjConn{responses: []objQueryResponse{
		resolveFoundResponse(999, "dbo", "PlainModule", "P"),
	}}
	sess := newFakeObjSession(t, conn)
	sink := &objCaptureSink{}

	err := Stats(context.Background(), sess, "dbo.PlainModule", sink)
	var pub *model.PublicError
	if !errors.As(err, &pub) {
		t.Fatalf("Stats: want *model.PublicError, got %#v", err)
	}
	if pub.Code != 2 || pub.Kind != "invalid_argument" {
		t.Fatalf("Stats on a procedure: want code 2/invalid_argument, got code %d/%s", pub.Code, pub.Kind)
	}
}

// TestStatsAvailableRowComputesSamplePct covers the straightforward
// cell values design spec line 69 names: a statistic with real
// properties gets sample_pct = rows_sampled*100/rows, and its own key
// columns, auto/user-created flags and filter survive untouched.
func TestStatsAvailableRowComputesSamplePct(t *testing.T) {
	conn := &fakeObjConn{responses: []objQueryResponse{
		resolveFoundResponse(701, "dbo", "Orders", "U"),
		statsRowsResponse([][]driver.Value{
			{int64(1), "PK_Orders", "[Id]", int64(1000), int64(250), statsTime("2026-09-01T00:00:00"), int64(12), false, true, nil, int64(1)},
		}),
		permProbeResponse(int64(1)), // VIEW DEFINITION allowed: filter not masked
	}}
	sess := newFakeObjSession(t, conn)
	sink := &objCaptureSink{}

	if err := Stats(context.Background(), sess, "dbo.Orders", sink); err != nil {
		t.Fatalf("Stats: %v", err)
	}
	tbl := sink.table("statistics")
	if tbl == nil || len(tbl.rows) != 1 {
		t.Fatalf("statistics: want exactly one row, got %#v", tbl)
	}
	row := tbl.rows[0]
	if row[5] != float64(25) {
		t.Fatalf("sample_pct cell: got %#v, want 25 (250*100/1000)", row[5])
	}
	// Dispatch B5: columns (the STRING_AGG extension sql/stats.sql's
	// own skeleton had to add) was never compared to a real value by
	// any test in this task - only proven present by name, via
	// TestStatisticsTableHasTwelveColumnsInOrder.
	if row[2] != "[Id]" {
		t.Fatalf("columns cell: got %#v, want %q", row[2], "[Id]")
	}
	if row[8] != false || row[9] != true {
		t.Fatalf("auto_created/user_created cells: got %#v/%#v", row[8], row[9])
	}
	if row[11] != propertiesStatusAvailable {
		t.Fatalf("properties_status cell: got %#v, want available", row[11])
	}
	if !tbl.propertiesComplete {
		t.Fatalf("all rows available: want properties_complete=true, got false")
	}
}

// TestStatsSamplePctNullWhenRowsZero is the brief's own explicit rule:
// "NULL sample_pct si rows=0" - never a division by zero.
func TestStatsSamplePctNullWhenRowsZero(t *testing.T) {
	conn := &fakeObjConn{responses: []objQueryResponse{
		resolveFoundResponse(701, "dbo", "Empty", "U"),
		statsRowsResponse([][]driver.Value{
			{int64(1), "PK_Empty", "[Id]", int64(0), int64(0), nil, int64(0), true, false, nil, int64(1)},
		}),
		permProbeResponse(int64(1)), // VIEW DEFINITION allowed: filter not masked
	}}
	sess := newFakeObjSession(t, conn)
	sink := &objCaptureSink{}

	if err := Stats(context.Background(), sess, "dbo.Empty", sink); err != nil {
		t.Fatalf("Stats: %v", err)
	}
	row := sink.table("statistics").rows[0]
	if row[5] != nil {
		t.Fatalf("rows=0: sample_pct must be NULL, got %#v", row[5])
	}
	if row[11] != propertiesStatusAvailable {
		t.Fatalf("rows=0 with a real properties row: properties_status must still be available, got %#v", row[11])
	}
}

// TestStatsLastUpdatedNullButAvailable is the brief's own explicit
// rule, the one most likely to collide with the wrong completeness
// signal: "last_updated=NULL avec ligne de propriétés existe reste
// available" - a NULL last_updated (no statistics blob yet) is
// legitimate data, not a missing-properties signal. This is exactly
// why Stats keys its available/unavailable decision on
// properties_stats_id (cells[10]) rather than on last_updated or
// modification_counter - see TestStatsNullCounterAndLastUpdatedIsAvailable
// for the sharper case where modification_counter is ALSO NULL on an
// otherwise available row (fix 1's A1).
func TestStatsLastUpdatedNullButAvailable(t *testing.T) {
	conn := &fakeObjConn{responses: []objQueryResponse{
		resolveFoundResponse(701, "dbo", "Filtered", "U"),
		statsRowsResponse([][]driver.Value{
			{int64(1), "FilteredStat", "[Status]", int64(0), int64(0), nil, int64(0), false, true, "([Status]='active')", int64(1)},
		}),
		permProbeResponse(int64(1)), // VIEW DEFINITION allowed: filter not masked
	}}
	sess := newFakeObjSession(t, conn)
	sink := &objCaptureSink{}

	if err := Stats(context.Background(), sess, "dbo.Filtered", sink); err != nil {
		t.Fatalf("Stats: %v", err)
	}
	row := sink.table("statistics").rows[0]
	if row[6] != nil {
		t.Fatalf("fixture setup error: last_updated must be NULL in this case, got %#v", row[6])
	}
	if row[11] != propertiesStatusAvailable {
		t.Fatalf("NULL last_updated alone must not make properties_status anything but available, got %#v", row[11])
	}
	if row[10] != "([Status]='active')" {
		t.Fatalf("filter cell: got %#v", row[10])
	}
}

// TestStatsNullCounterAndLastUpdatedIsAvailable is fix 1's A1, the
// exact scenario measured on a real engine (task-14-fix-1.md): a
// statistic on an empty table, or one whose blob was never built, has
// a properties row that genuinely EXISTS (properties_stats_id
// non-NULL) with BOTH modification_counter and last_updated NULL. The
// superseded code kept modification_counter IS NULL as its own signal
// for "no properties row at all", which reached the SELECT probe here
// and reported a confirmed permission_denied for a principal who
// could read this statistic's properties just fine - a permission
// state that changed when an administrator inserted a row and ran
// UPDATE STATISTICS without touching any grant, which is not a
// permission state at all. No SELECT probe is registered on this
// fake conn at all: reaching it at all, for any reason, fails this
// test outright ("no responder matched"), which is a stronger proof
// than counting calls after the fact.
func TestStatsNullCounterAndLastUpdatedIsAvailable(t *testing.T) {
	conn := &fakeObjConn{responses: []objQueryResponse{
		resolveFoundResponse(701, "dbo", "EmptyEver", "U"),
		statsRowsResponse([][]driver.Value{
			{int64(2), "St_OnEmptyTable", "[A]", nil, nil, nil, nil, false, true, nil, int64(2)},
		}),
		permProbeResponse(int64(1)), // VIEW DEFINITION allowed: answers columnsPropertiesComplete only - the row must never reach the SELECT probe at all
	}}
	sess := newFakeObjSession(t, conn)
	sink := &objCaptureSink{}

	if err := Stats(context.Background(), sess, "dbo.EmptyEver", sink); err != nil {
		t.Fatalf("Stats: %v", err)
	}
	row := sink.table("statistics").rows[0]
	if row[11] != propertiesStatusAvailable {
		t.Fatalf("a properties row that exists with NULL modification_counter AND NULL last_updated must be available, got %#v", row[11])
	}
	if row[5] != nil || row[6] != nil {
		t.Fatalf("fixture setup error: sample_pct/last_updated must be NULL here, got %#v/%#v", row[5], row[6])
	}
}

// TestStatsFilterMaskedWithoutViewDefinition is fix 1's A2: a
// principal with SELECT on a statistic's columns but no VIEW
// DEFINITION on the object reads filter=NULL for a statistic that
// genuinely has one - measured on a real engine, the administrator
// reads the real filter text while this principal reads NULL, with
// every other property (rows, sample_pct, properties_status) fully
// available. properties_complete must go false and name the cause,
// never stay true because the per-row properties_status alone was
// satisfied (design spec lines 57/101/228).
func TestStatsFilterMaskedWithoutViewDefinition(t *testing.T) {
	conn := &fakeObjConn{responses: []objQueryResponse{
		resolveFoundResponse(701, "dbo", "Orders", "U"),
		statsRowsResponse([][]driver.Value{
			{int64(1), "FilteredStat", "[Status]", int64(1), int64(1), statsTime("2026-09-01T00:00:00"), int64(0), false, true, nil, int64(1)},
		}),
		permProbeResponse(int64(0)), // VIEW DEFINITION denied: the filter is masked, not absent
	}}
	sess := newFakeObjSession(t, conn)
	sink := &objCaptureSink{}

	if err := Stats(context.Background(), sess, "dbo.Orders", sink); err != nil {
		t.Fatalf("Stats: %v", err)
	}
	tbl := sink.table("statistics")
	row := tbl.rows[0]
	if row[11] != propertiesStatusAvailable {
		t.Fatalf("a masked filter must not downgrade properties_status, got %#v", row[11])
	}
	if row[10] != nil {
		t.Fatalf("fixture setup error: filter cell must read NULL (masked), got %#v", row[10])
	}
	if tbl.propertiesComplete {
		t.Fatalf("a masked filter must make properties_complete=false on the table - a narrower SELECT must never have to widen into VIEW DEFINITION to look complete, got true")
	}
	n := sink.noticeWithKind("definition_properties_unavailable")
	if n == nil {
		t.Fatalf("a masked filter must emit a definition_properties_unavailable notice, the same Kind readIndexes already uses for the analogous case")
	}
	if n.Table != StatisticsTable.Name {
		t.Fatalf("definition_properties_unavailable notice Table: got %q, want %q", n.Table, StatisticsTable.Name)
	}
}

// TestStatsNeverDefaultsToPermissionDeniedWithoutProbe is dispatch
// cassure 2's own target, and design spec line 69's own condition:
// "only use permission_denied when a permission check establishes it.
// Missing properties alone do not prove the cause." Here the
// permission probe itself comes back Allowed, so an unavailable
// properties row must be reported unavailable, never permission_denied
// invented from the NULL alone.
func TestStatsNeverDefaultsToPermissionDeniedWithoutProbe(t *testing.T) {
	conn := &fakeObjConn{responses: []objQueryResponse{
		resolveFoundResponse(701, "dbo", "Orders", "U"),
		statsRowsResponse([][]driver.Value{
			{int64(1), "PK_Orders", "[Id]", nil, nil, nil, nil, true, false, nil, nil},
		}),
		permProbeResponse(int64(1)), // SELECT allowed: the NULL is not explained by a denial
	}}
	sess := newFakeObjSession(t, conn)
	sink := &objCaptureSink{}

	if err := Stats(context.Background(), sess, "dbo.Orders", sink); err != nil {
		t.Fatalf("Stats: %v", err)
	}
	row := sink.table("statistics").rows[0]
	if row[11] != propertiesStatusUnavailable {
		t.Fatalf("unavailable properties with SELECT allowed: want properties_status=unavailable, got %#v", row[11])
	}
}

// TestStatsPermissionDeniedEstablishedByProbe is
// TestStatsNeverDefaultsToPermissionDeniedWithoutProbe's other half:
// when the SAME probe comes back Denied, permission_denied is now
// established, not guessed.
func TestStatsPermissionDeniedEstablishedByProbe(t *testing.T) {
	conn := &fakeObjConn{responses: []objQueryResponse{
		resolveFoundResponse(701, "dbo", "Orders", "U"),
		statsRowsResponse([][]driver.Value{
			{int64(1), "PK_Orders", "[Id]", nil, nil, nil, nil, true, false, nil, nil},
		}),
		permProbeResponse(int64(0)), // SELECT denied: now confirmed
	}}
	sess := newFakeObjSession(t, conn)
	sink := &objCaptureSink{}

	if err := Stats(context.Background(), sess, "dbo.Orders", sink); err != nil {
		t.Fatalf("Stats: %v", err)
	}
	row := sink.table("statistics").rows[0]
	if row[11] != propertiesStatusPermissionDenied {
		t.Fatalf("unavailable properties with SELECT denied: want properties_status=permission_denied, got %#v", row[11])
	}
}

// TestStatsProbesSelectPermissionAtMostOnce proves
// selectPermissionCache actually caches: two rows that both need an
// answer must only cost one SELECT probe, not one per row - and that
// columnsPropertiesComplete's own VIEW DEFINITION probe (fix 1's A2,
// now issued unconditionally once per command) is itself a SEPARATE,
// single call, never conflated with the per-row SELECT probe. The two
// probes share the exact same query text (hasPermsByNameQuery), so
// this test tells them apart by their own @permission argument rather
// than by a shared counter, the same way the real driver would.
func TestStatsProbesSelectPermissionAtMostOnce(t *testing.T) {
	calls := map[string]int{}
	conn := &fakeObjConn{responses: []objQueryResponse{
		resolveFoundResponse(701, "dbo", "Orders", "U"),
		statsRowsResponse([][]driver.Value{
			{int64(1), "StatA", "[A]", nil, nil, nil, nil, true, false, nil, nil},
			{int64(2), "StatB", "[B]", nil, nil, nil, nil, true, false, nil, nil},
		}),
		permProbeByPermissionResponse(int64(0), calls),
	}}
	sess := newFakeObjSession(t, conn)
	sink := &objCaptureSink{}

	if err := Stats(context.Background(), sess, "dbo.Orders", sink); err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if calls["SELECT"] != 1 {
		t.Fatalf("selectPermissionCache: want exactly one SELECT probe for two unavailable rows, got %d", calls["SELECT"])
	}
	if calls["VIEW DEFINITION"] != 1 {
		t.Fatalf("columnsPropertiesComplete: want exactly one VIEW DEFINITION probe per command, got %d", calls["VIEW DEFINITION"])
	}
}

// TestStatsMixedAvailabilityLeavesTableIncomplete is dispatch's own
// named fixture for "per-row completeness" (design spec: "stats list":
// "... per-row completeness"): one statistic's properties are
// available, a second's are not - properties_complete on the table is
// false, while the authorized row still carries its real values, and
// the unavailable-properties count is reported on its own Notice, kept
// apart from anything preview-shaped (design spec: "Report
// unavailable-property counts separately from preview truncation").
func TestStatsMixedAvailabilityLeavesTableIncomplete(t *testing.T) {
	conn := &fakeObjConn{responses: []objQueryResponse{
		resolveFoundResponse(701, "dbo", "Orders", "U"),
		statsRowsResponse([][]driver.Value{
			{int64(1), "Granted", "[Id]", int64(1000), int64(1000), statsTime("2026-09-01T00:00:00"), int64(3), false, true, nil, int64(1)},
			{int64(2), "NotGranted", "[Email]", nil, nil, nil, nil, false, true, nil, nil},
		}),
		permProbeResponse(int64(0)),
	}}
	sess := newFakeObjSession(t, conn)
	sink := &objCaptureSink{}

	if err := Stats(context.Background(), sess, "dbo.Orders", sink); err != nil {
		t.Fatalf("Stats: %v", err)
	}
	tbl := sink.table("statistics")
	if tbl.propertiesComplete {
		t.Fatalf("mixed availability: want properties_complete=false on the table, got true")
	}
	if tbl.rows[0][11] != propertiesStatusAvailable || tbl.rows[0][3] != int64(1000) {
		t.Fatalf("the granted row must still carry its own real values, got %#v", tbl.rows[0])
	}
	if tbl.rows[1][11] != propertiesStatusPermissionDenied {
		t.Fatalf("the denied row's own status, got %#v", tbl.rows[1][11])
	}

	n := sink.noticeWithKind("properties_unavailable")
	if n == nil {
		t.Fatalf("mixed availability must emit a properties_unavailable notice")
	}
	if strings.Contains(strings.ToLower(n.Kind), "preview") || strings.Contains(strings.ToLower(n.Message), "preview_omitted") {
		t.Fatalf("properties_unavailable notice must not be confused with preview truncation, got %#v", n)
	}
	if !strings.Contains(n.Message, "1 of 2") {
		t.Fatalf("properties_unavailable notice must name the count separately (1 of 2 statistics), got %q", n.Message)
	}
	// Dispatch B6: noticeWithKind only ever filters on Kind.
	if n.Table != StatisticsTable.Name {
		t.Fatalf("properties_unavailable notice Table: got %q, want %q", n.Table, StatisticsTable.Name)
	}
}

// TestStatsPropertiesStatusNeverStale is dispatch cassure 1's own
// target for "stats list": design spec line 65's "Do not include a
// stale verdict in v0.1" applies here exactly as it does to "idx
// usage" - no verdict derived from modification_counter, however
// large, is ever anything but available/permission_denied/unavailable.
// A high modification_counter is evidence an agent can read for
// itself; this command never turns it into a conclusion.
func TestStatsPropertiesStatusNeverStale(t *testing.T) {
	conn := &fakeObjConn{responses: []objQueryResponse{
		resolveFoundResponse(701, "dbo", "Orders", "U"),
		statsRowsResponse([][]driver.Value{
			{int64(1), "PK_Orders", "[Id]", int64(1000), int64(10), statsTime("2020-01-01T00:00:00"), int64(999999999), false, true, nil, int64(1)},
		}),
		permProbeResponse(int64(1)), // VIEW DEFINITION allowed: filter not masked
	}}
	sess := newFakeObjSession(t, conn)
	sink := &objCaptureSink{}

	if err := Stats(context.Background(), sess, "dbo.Orders", sink); err != nil {
		t.Fatalf("Stats: %v", err)
	}
	row := sink.table("statistics").rows[0]
	status, _ := row[11].(string)
	switch status {
	case propertiesStatusAvailable, propertiesStatusPermissionDenied, propertiesStatusUnavailable:
	default:
		t.Fatalf("properties_status with a huge modification_counter: got %q, want one of the three closed values, never a stale verdict", status)
	}
	for _, n := range sink.notices {
		lower := strings.ToLower(n.Kind + " " + n.Message)
		if strings.Contains(lower, "stale") {
			t.Fatalf("Stats must never assert staleness (design spec line 65): notice %#v", n)
		}
	}
}

// TestStatsEmptyResultIsCompleteSuccess is design spec line 69's own
// rule: "No readable statistics is a successful empty result only
// after the target object was resolved" - zero sys.stats rows, after a
// successful Resolve, is complete and ok, never an error.
func TestStatsEmptyResultIsCompleteSuccess(t *testing.T) {
	conn := &fakeObjConn{responses: []objQueryResponse{
		resolveFoundResponse(701, "dbo", "NoStats", "U"),
		permProbeResponse(int64(1)), // VIEW DEFINITION allowed: the probe runs even on zero rows
		statsRowsResponse(nil),
	}}
	sess := newFakeObjSession(t, conn)
	sink := &objCaptureSink{}

	if err := Stats(context.Background(), sess, "dbo.NoStats", sink); err != nil {
		t.Fatalf("Stats: %v", err)
	}
	tbl := sink.table("statistics")
	if tbl == nil || len(tbl.rows) != 0 || !tbl.propertiesComplete || !tbl.collectionComplete {
		t.Fatalf("zero statistics after a resolved object: want a complete, empty table, got %#v", tbl)
	}
}
