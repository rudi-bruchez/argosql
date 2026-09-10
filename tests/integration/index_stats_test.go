//go:build integration

package integration

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rudi-bruchez/argosql/internal/diagnostics"
	"github.com/rudi-bruchez/argosql/internal/model"
	"github.com/rudi-bruchez/argosql/internal/sqlserver"
)

// TestPartialStatistics is task 14's own brief, verbatim: "stats list"
// as metadata_only (object metadata access without SELECT, design spec
// line 176's first additional principal) on dbo.Orders must collect at
// least one statistics row - the OUTER APPLY in sql/stats.sql must not
// have lost it - while properties_complete is false, because none of
// that row's properties are readable without SELECT.
func TestPartialStatistics(t *testing.T) {
	lab := NewLab(t, os.Getenv("ASQ_TEST_IMAGE"))
	r, code := lab.Run(t, "metadata_only", []string{"stats", "list", "dbo.Orders"})
	if code != 0 || len(r.Tables) != 1 || r.Tables[0].State.PropertiesComplete {
		t.Fatalf("%d %#v", code, r.Tables)
	}
	if r.Tables[0].State.RowsCollected == 0 {
		t.Fatal("OUTER APPLY lost statistics")
	}
}

// usageIndexRow finds tbl's row whose index_id cell equals indexID, or
// fails the test: every assertion below is about one specific named
// index, never "some row, whichever it is".
func usageIndexRow(t *testing.T, tbl *capturedTable, indexID int64) []model.Cell {
	t.Helper()
	for _, row := range tbl.rows {
		if row[0] == indexID {
			return row
		}
	}
	t.Fatalf("usage: no row for index_id %d, got %#v", indexID, tbl.rows)
	return nil
}

// indexIDOf resolves name's own index_id on obj - a small, direct
// catalog read via lab.Admin, so this test does not have to hardcode
// index_id values that CREATE TABLE/CREATE INDEX's own ordering could
// silently change.
func indexIDOf(t *testing.T, lab *Lab, ctx context.Context, table, index string) int64 {
	t.Helper()
	var id int64
	err := lab.Admin.QueryRowContext(ctx,
		"SELECT index_id FROM sys.indexes WHERE object_id = OBJECT_ID(@p1) AND name = @p2",
		table, index,
	).Scan(&id)
	if err != nil {
		t.Fatalf("resolving index_id for %s.%s: %v", table, index, err)
	}
	return id
}

// TestIndexDMV is dispatch's own named red/green target. It exercises,
// against a real engine, the design spec's Q/I/S permission matrix for
// "idx usage" and "idx missing" (both require the instance-level
// permission sys.dm_db_index_usage_stats/sys.dm_db_missing_index_details
// actually need - measured against Microsoft's own documentation, not
// merely the database-level bundle "obj table"/"idx list" rely on);
// observation_status's never_observed/observed distinction on a real,
// genuinely untouched index; the LEFT JOIN that keeps a missing-index
// suggestion for an object S cannot see in sys.objects (dispatch
// cassure 5's real-engine counterpart to the fake-driver proof in
// missing_test.go); and the two-database isolation design spec itself
// requires database_id = DB_ID() to provide (dispatch: "la fixture à
// deuxième base ... n'est pas décorative").
func TestIndexDMV(t *testing.T) {
	lab := NewLab(t, os.Getenv("ASQ_TEST_IMAGE"))
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	pw := lab.ensurePrincipals(t)
	qSess, err := sqlserver.Open(ctx, principalProfile(lab, "asq_test_q", pw.Q))
	if err != nil {
		t.Fatalf("open as Q: %v", err)
	}
	defer qSess.Close()
	iSess, err := sqlserver.Open(ctx, principalProfile(lab, "asq_test_i", pw.I))
	if err != nil {
		t.Fatalf("open as I: %v", err)
	}
	defer iSess.Close()
	sSess, err := sqlserver.Open(ctx, principalProfile(lab, "asq_test_s", pw.S))
	if err != nil {
		t.Fatalf("open as S: %v", err)
	}
	defer sSess.Close()

	t.Run("idx usage Q unresolved", func(t *testing.T) {
		err := diagnostics.Usage(ctx, qSess, "dbo.UsageFixture", &captureSink{})
		assertPublicErrorCode(t, err, 8)
	})
	t.Run("idx usage I lacks the instance permission", func(t *testing.T) {
		err := diagnostics.Usage(ctx, iSess, "dbo.UsageFixture", &captureSink{})
		assertPublicErrorCode(t, err, 4)
	})
	t.Run("idx usage S succeeds with real evidence", func(t *testing.T) {
		sink := &captureSink{}
		if err := diagnostics.Usage(ctx, sSess, "dbo.UsageFixture", sink); err != nil {
			t.Fatalf("idx usage as S: %v", err)
		}
		tbl := sink.table("usage")
		if tbl == nil {
			t.Fatalf("idx usage as S wrote no usage table")
		}

		queriedID := indexIDOf(t, lab, ctx, "dbo.UsageFixture", "IX_UsageFixture_Category")
		queried := usageIndexRow(t, tbl, queriedID)
		seeks, ok := queried[2].(int64)
		if !ok || seeks < 1 {
			t.Fatalf("IX_UsageFixture_Category: want seeks >= 1 (observed by objects.sql's own query), got %#v", queried[2])
		}
		if queried[10] != "observed" {
			t.Fatalf("IX_UsageFixture_Category observation_status: got %#v, want observed", queried[10])
		}

		neverID := indexIDOf(t, lab, ctx, "dbo.UsageFixture", "IX_UsageFixture_NeverQueried")
		never := usageIndexRow(t, tbl, neverID)
		if never[2] != nil || never[6] != nil {
			t.Fatalf("IX_UsageFixture_NeverQueried: want NULL seeks/last_seek, got %#v/%#v", never[2], never[6])
		}
		if never[10] != "never_observed" {
			t.Fatalf("IX_UsageFixture_NeverQueried observation_status: got %#v, want never_observed", never[10])
		}

		// Design spec's declared row order ("Rows use IDs/ordinals
		// ascending unless ranking is specified") for "usage" - a fake
		// driver's own unit test cannot catch a reversed ORDER BY in
		// usage.sql itself, since it just echoes back whatever rows a
		// test hands it; only a real engine, asked to actually sort,
		// can (dispatch cassure 7's own target for this half).
		for i := 1; i < len(tbl.rows); i++ {
			prev, _ := tbl.rows[i-1][0].(int64)
			cur, _ := tbl.rows[i][0].(int64)
			if prev > cur {
				t.Fatalf("usage rows are not index_id ascending: row %d has index_id %d after %d", i, cur, prev)
			}
		}
	})

	t.Run("idx missing Q lacks the instance permission", func(t *testing.T) {
		err := diagnostics.Missing(ctx, qSess, diagnostics.MissingOptions{Top: 100}, &captureSink{})
		assertPublicErrorCode(t, err, 4)
	})
	t.Run("idx missing I lacks the instance permission", func(t *testing.T) {
		err := diagnostics.Missing(ctx, iSess, diagnostics.MissingOptions{Top: 100}, &captureSink{})
		assertPublicErrorCode(t, err, 4)
	})

	// adminMissingCount mirrors sql/missing.sql's own join shape (minus
	// the LEFT JOINs to sys.objects/sys.schemas, irrelevant to row
	// count), scoped to one database by name - lab.Admin sees every
	// database on the instance, so this is the ground truth "idx
	// missing" as S must match, neither more (a leaked row) nor fewer
	// (a silently dropped one).
	adminMissingCount := func(t *testing.T, database string) int {
		t.Helper()
		var n int
		err := lab.Admin.QueryRowContext(ctx, `
SELECT COUNT(*)
FROM sys.dm_db_missing_index_group_stats AS g
JOIN sys.dm_db_missing_index_groups AS ig ON ig.index_group_handle = g.group_handle
JOIN sys.dm_db_missing_index_details AS d ON d.index_handle = ig.index_handle
WHERE d.database_id = DB_ID(@p1)`, database).Scan(&n)
		if err != nil {
			t.Fatalf("counting missing-index rows for %s: %v", database, err)
		}
		return n
	}

	var suggestions *capturedTable
	t.Run("idx missing S succeeds with the LEFT JOIN's own evidence", func(t *testing.T) {
		sink := &captureSink{}
		if err := diagnostics.Missing(ctx, sSess, diagnostics.MissingOptions{Top: 100}, sink); err != nil {
			t.Fatalf("idx missing as S: %v", err)
		}
		suggestions = sink.table("suggestions")
		if suggestions == nil {
			t.Fatalf("idx missing as S wrote no suggestions table")
		}

		wantCount := adminMissingCount(t, "AppDB")
		if len(suggestions.rows) != wantCount {
			t.Fatalf("idx missing as S: got %d rows, want exactly %d (AppDB's own raw DMV count)", len(suggestions.rows), wantCount)
		}

		var visible, hidden bool
		for _, row := range suggestions.rows {
			objectName, _ := row[3].(string)
			if objectName == "MissingIndexFixture" {
				visible = true
				if row[2] != "dbo" {
					t.Fatalf("MissingIndexFixture: want schema_name dbo, got %#v", row[2])
				}
			}
			if row[2] == nil && row[3] == nil {
				hidden = true
			}
		}
		if !visible {
			t.Fatalf("idx missing as S: dbo.MissingIndexFixture's own suggestion is missing entirely, got %#v", suggestions.rows)
		}
		if !hidden {
			t.Fatalf("idx missing as S: no row with NULL schema_name/object_name - "+
				"dbo.MissingIndexHiddenFromS's own evidence must survive the LEFT JOIN even though "+
				"its catalog row is denied to S, got %#v", suggestions.rows)
		}

		// Dispatch B2: impact_score descending is the core of this
		// command's own ranking (design spec's declared row order for
		// "suggestions"), never checked against a real engine before -
		// a fake-driver unit test cannot catch a reversed ORDER BY,
		// since it only ever echoes back rows already sorted by the
		// test itself (same limitation this project's own report
		// already named for usage.sql's ORDER BY, not reproduced here
		// for missing.sql despite ranking being the entire point).
		for i := 1; i < len(suggestions.rows); i++ {
			prevScore, _ := suggestions.rows[i-1][11].(float64)
			curScore, _ := suggestions.rows[i][11].(float64)
			if prevScore < curScore {
				t.Fatalf("suggestions rows are not impact_score descending: row %d has impact_score %v after %v", i, curScore, prevScore)
			}
		}
	})

	// Task 14's own second-database fixture: sys.dm_db_missing_index_details
	// is instance-wide, so a second database's own missing-index
	// evidence must never leak into "idx missing" run from AppDB
	// (design spec: "filter on details.database_id = DB_ID()").
	t.Run("idx missing does not leak a second database's evidence", func(t *testing.T) {
		if _, err := lab.Admin.ExecContext(ctx, "IF DB_ID(N'AppDB2') IS NULL CREATE DATABASE AppDB2;"); err != nil {
			t.Fatalf("creating AppDB2: %v", err)
		}
		t.Cleanup(func() {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			lab.Admin.ExecContext(cleanupCtx, "ALTER DATABASE AppDB2 SET SINGLE_USER WITH ROLLBACK IMMEDIATE; DROP DATABASE AppDB2;")
		})
		if _, err := lab.Admin.ExecContext(ctx, `
IF OBJECT_ID(N'AppDB2.dbo.MissingIndexFixture2') IS NULL
BEGIN
    CREATE TABLE AppDB2.dbo.MissingIndexFixture2 (
        Id INT IDENTITY(1,1) PRIMARY KEY,
        Status NVARCHAR(20) NOT NULL,
        Payload NVARCHAR(400) NOT NULL
    );
    INSERT INTO AppDB2.dbo.MissingIndexFixture2 (Status, Payload)
    SELECT CASE WHEN rn = 1 THEN N'rare' ELSE N'common' END, REPLICATE(N'x', 400)
    FROM (
        SELECT TOP (200000) ROW_NUMBER() OVER (ORDER BY (SELECT NULL)) AS rn
        FROM sys.all_objects AS a CROSS JOIN sys.all_objects AS b
    ) AS n;
END`); err != nil {
			t.Fatalf("building AppDB2's own fixture: %v", err)
		}
		var dummy int
		if err := lab.Admin.QueryRowContext(ctx,
			"SELECT Id FROM AppDB2.dbo.MissingIndexFixture2 WHERE Status = N'rare'").Scan(&dummy); err != nil {
			t.Fatalf("triggering AppDB2's own missing-index suggestion: %v", err)
		}
		if adminMissingCount(t, "AppDB2") == 0 {
			t.Fatalf("AppDB2's own fixture query never registered a missing-index suggestion - fixture did not trigger, test proves nothing")
		}

		sink := &captureSink{}
		if err := diagnostics.Missing(ctx, sSess, diagnostics.MissingOptions{Top: 100}, sink); err != nil {
			t.Fatalf("idx missing as S, after AppDB2 exists: %v", err)
		}
		wantCount := adminMissingCount(t, "AppDB")
		if len(sink.table("suggestions").rows) != wantCount {
			t.Fatalf("idx missing as S leaked AppDB2's own evidence: got %d rows, want %d (AppDB's own count, unchanged)",
				len(sink.table("suggestions").rows), wantCount)
		}
	})

	// Fix 1's B4: missing.sql had this fixture; usage.sql carries the
	// exact same database_id = DB_ID() filter (sys.dm_db_index_usage_stats
	// is instance-wide too) and had none at all - a structural absence,
	// not merely an untested assertion. sys.indexes itself is already
	// database-scoped (a connection to UsageIsoA can never see AppDB's
	// own sys.indexes rows), so the ONLY way this filter's absence can
	// ever surface is a genuine (object_id, index_id) collision between
	// two databases' own DMV rows - the same collision technique the
	// conformity reviewer measured reproducibly on both engines for
	// sys.dm_db_missing_index_details, by creating an identical table
	// FIRST in each of two freshly created databases.
	t.Run("idx usage does not leak a second database's evidence", func(t *testing.T) {
		const collisionDDL = `
IF OBJECT_ID(N'%[1]s.dbo.Collision') IS NULL
BEGIN
    CREATE TABLE %[1]s.dbo.Collision (
        Id INT IDENTITY(1,1) PRIMARY KEY,
        Category NVARCHAR(50) NOT NULL
    );
    CREATE NONCLUSTERED INDEX IX_Collision_Category ON %[1]s.dbo.Collision (Category);
    INSERT INTO %[1]s.dbo.Collision (Category) VALUES (N'a');
END`
		for _, db := range []string{"UsageIsoA", "UsageIsoB"} {
			db := db
			if _, err := lab.Admin.ExecContext(ctx, fmt.Sprintf("IF DB_ID(N'%s') IS NULL CREATE DATABASE %s;", db, db)); err != nil {
				t.Fatalf("creating %s: %v", db, err)
			}
			t.Cleanup(func() {
				cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				lab.Admin.ExecContext(cleanupCtx, fmt.Sprintf("ALTER DATABASE %s SET SINGLE_USER WITH ROLLBACK IMMEDIATE; DROP DATABASE %s;", db, db))
			})
			if _, err := lab.Admin.ExecContext(ctx, fmt.Sprintf(collisionDDL, db)); err != nil {
				t.Fatalf("building %s.dbo.Collision: %v", db, err)
			}
		}

		var idA, idB int64
		if err := lab.Admin.QueryRowContext(ctx, "SELECT OBJECT_ID(N'UsageIsoA.dbo.Collision')").Scan(&idA); err != nil {
			t.Fatalf("reading UsageIsoA.dbo.Collision's own object_id: %v", err)
		}
		if err := lab.Admin.QueryRowContext(ctx, "SELECT OBJECT_ID(N'UsageIsoB.dbo.Collision')").Scan(&idB); err != nil {
			t.Fatalf("reading UsageIsoB.dbo.Collision's own object_id: %v", err)
		}
		if idA != idB {
			t.Fatalf("UsageIsoA/UsageIsoB's own dbo.Collision did not collide on object_id (got %d vs %d) - "+
				"this fixture cannot demonstrate the leak it was built to catch on this engine/run; "+
				"the database_id filter itself is unverified by this subtest", idA, idB)
		}

		// Touch A's index once, B's index seven times - two distinct,
		// attributable seek counts, so a leaked row is distinguishable
		// from A's own by value, not merely by its existence.
		var dummy int
		if err := lab.Admin.QueryRowContext(ctx, "SELECT Id FROM UsageIsoA.dbo.Collision WHERE Category = N'a'").Scan(&dummy); err != nil {
			t.Fatalf("touching UsageIsoA's own index: %v", err)
		}
		for i := 0; i < 7; i++ {
			if err := lab.Admin.QueryRowContext(ctx, "SELECT Id FROM UsageIsoB.dbo.Collision WHERE Category = N'a'").Scan(&dummy); err != nil {
				t.Fatalf("touching UsageIsoB's own index (iteration %d): %v", i, err)
			}
		}

		// sysadmin, not S: S has no user mapped in either fresh
		// database, and the instance-level permission this command
		// needs is orthogonal to what this subtest is actually
		// checking (the database_id filter, not the permission
		// matrix, which the earlier subtests above already cover).
		profileA := lab.Profile
		profileA.Database = "UsageIsoA"
		sessA, err := sqlserver.Open(ctx, profileA)
		if err != nil {
			t.Fatalf("open UsageIsoA: %v", err)
		}
		defer sessA.Close()

		sink := &captureSink{}
		if err := diagnostics.Usage(ctx, sessA, "dbo.Collision", sink); err != nil {
			t.Fatalf("idx usage dbo.Collision on UsageIsoA: %v", err)
		}
		tbl := sink.table("usage")
		// Two rows, not one: dbo.Collision's own clustered primary key
		// gets its own usage row too (a lookup, from this query's own
		// SELECT Id), alongside IX_Collision_Category. A THIRD row
		// here - or a duplicate of either - would be the fan-out a
		// missing database_id filter produces when two databases'
		// DMV rows both match the same (object_id, index_id) pair.
		if tbl == nil || len(tbl.rows) != 2 {
			t.Fatalf("idx usage on UsageIsoA's own dbo.Collision: want exactly two index rows (no fan-out from a colliding database), got %#v", tbl)
		}
		// Not indexIDOf: sys.indexes is itself database-scoped, and
		// lab.Admin's own connection context is AppDB - a three-part
		// OBJECT_ID('UsageIsoA...') joined against AppDB's own
		// sys.indexes would resolve nothing. sessA is already
		// connected to UsageIsoA, so a plain, unqualified lookup on
		// it reads the right catalog.
		var categoryID int64
		if err := sessA.Conn.QueryRowContext(ctx,
			"SELECT index_id FROM sys.indexes WHERE object_id = OBJECT_ID(N'dbo.Collision') AND name = N'IX_Collision_Category'",
		).Scan(&categoryID); err != nil {
			t.Fatalf("resolving IX_Collision_Category's own index_id on UsageIsoA: %v", err)
		}
		row := usageIndexRow(t, tbl, categoryID)
		seeks, _ := row[2].(int64)
		if seeks != 1 {
			t.Fatalf("idx usage on UsageIsoA's own IX_Collision_Category: want seeks=1 (this database's own activity only), got %d - "+
				"UsageIsoB's own 7 seeks leaked in if this is 7 or 8", seeks)
		}
	})
}

// adminStatsColumns re-reads a statistic's own ordered key columns
// directly from sys.stats_columns/sys.columns, via lab.Admin (always
// visible, regardless of the principal "stats list" itself ran as) -
// fix 2's A2, a genuine second reading of the engine rather than a
// literal copied from the fixture's own CREATE STATISTICS text: this
// aggregates the names in Go, not by reusing sql/stats.sql's own
// STRING_AGG, so a defect in that shared query (wrong join, wrong
// order column) cannot also corrupt this comparison's own expectation.
func adminStatsColumns(t *testing.T, lab *Lab, ctx context.Context, objectID, statsID int64) string {
	t.Helper()
	rows, err := lab.Admin.QueryContext(ctx,
		"SELECT c.name FROM sys.stats_columns AS sc JOIN sys.columns AS c ON c.object_id = sc.object_id AND c.column_id = sc.column_id "+
			"WHERE sc.object_id = @p1 AND sc.stats_id = @p2 ORDER BY sc.stats_column_id",
		objectID, statsID)
	if err != nil {
		t.Fatalf("adminStatsColumns: %v", err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("adminStatsColumns: scanning: %v", err)
		}
		names = append(names, "["+name+"]")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("adminStatsColumns: %v", err)
	}
	return strings.Join(names, ", ")
}

// adminStatsProperties is a second, independent reading of
// sys.dm_db_stats_properties, via lab.Admin rather than the principal
// "stats list" itself ran as (fix 2's A2). lab.Admin is sysadmin, so
// this always sees real values regardless of any other principal's own
// SELECT grants - exactly the asymmetry TestStatsMixedAvailabilityPrincipal
// needs to prove a denial is real and per-row, not a coincidental NULL.
type adminStatsProperties struct {
	rows, rowsSampled, modCounter sql.NullInt64
	lastUpdated                   sql.NullTime
}

func readAdminStatsProperties(t *testing.T, lab *Lab, ctx context.Context, objectID, statsID int64) adminStatsProperties {
	t.Helper()
	var p adminStatsProperties
	err := lab.Admin.QueryRowContext(ctx,
		"SELECT rows, rows_sampled, last_updated, modification_counter FROM sys.dm_db_stats_properties(@p1, @p2)",
		objectID, statsID,
	).Scan(&p.rows, &p.rowsSampled, &p.lastUpdated, &p.modCounter)
	if err != nil {
		t.Fatalf("readAdminStatsProperties: %v", err)
	}
	return p
}

// TestStatsMixedAvailabilityPrincipal is design spec line 176's second
// additional principal (dispatch's own reproduction, verbatim): "SELECT
// on only one statistic's columns (mixed availability)". A principal
// with column-level SELECT on dbo.StatsFixture.Granted, and nothing
// else, must see St_Granted's own properties (available) while
// St_Withheld's come back unavailable or permission_denied - and the
// table's own properties_complete is false, even though the granted
// row still carries real values (dispatch: "c'est le cas qui distingue
// un code qui décide la disponibilité une fois pour la commande d'un
// code qui la décide PAR LIGNE").
func TestStatsMixedAvailabilityPrincipal(t *testing.T) {
	lab := NewLab(t, os.Getenv("ASQ_TEST_IMAGE"))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// lab.ensurePrincipals applies objects.sql first, which is what
	// creates dbo.StatsFixture this login's own GRANT below targets.
	lab.ensurePrincipals(t)

	const username = "asq_test_stats_subset"
	password := randomPassword()
	masterDB := openMasterDB(t, lab)
	if _, err := masterDB.ExecContext(ctx,
		"IF SUSER_ID(N'"+username+"') IS NOT NULL DROP LOGIN "+username+"; "+
			"CREATE LOGIN "+username+" WITH PASSWORD = N'"+password+"', CHECK_POLICY = OFF;"); err != nil {
		t.Fatalf("creating %s login: %v", username, err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		masterDB.ExecContext(cleanupCtx, "IF SUSER_ID(N'"+username+"') IS NOT NULL DROP LOGIN "+username+";")
	})
	if _, err := lab.Admin.ExecContext(ctx,
		"IF USER_ID(N'"+username+"') IS NOT NULL DROP USER "+username+"; "+
			"CREATE USER "+username+" FOR LOGIN "+username+"; "+
			"GRANT CONNECT TO "+username+"; "+
			"GRANT VIEW DEFINITION TO "+username+"; "+
			"GRANT SELECT ON dbo.StatsFixture (Granted) TO "+username+";"); err != nil {
		t.Fatalf("granting column-level SELECT to %s: %v", username, err)
	}

	sess, err := sqlserver.Open(ctx, principalProfile(lab, username, password))
	if err != nil {
		t.Fatalf("open as %s: %v", username, err)
	}
	defer sess.Close()

	sink := &captureSink{}
	if err := diagnostics.Stats(ctx, sess, "dbo.StatsFixture", sink); err != nil {
		t.Fatalf("stats list dbo.StatsFixture as %s: %v", username, err)
	}
	// Four rows, not three: dbo.StatsFixture's own clustered primary
	// key auto-creates its own statistic on Id, alongside the three
	// explicit CREATE STATISTICS this test cares about - measured, not
	// assumed. St_Ordered (fix 2's A1) is the two-column statistic that
	// makes sql/stats.sql's own column ORDER BY observable at all - a
	// single-column statistic cannot distinguish a correct order from a
	// reversed or absent one.
	tbl := sink.table("statistics")
	if tbl == nil || len(tbl.rows) != 4 {
		t.Fatalf("dbo.StatsFixture: want exactly four statistics rows (the PK's own plus St_Granted/St_Withheld/St_Ordered), got %#v", tbl)
	}
	if tbl.propertiesComplete {
		t.Fatalf("mixed availability: want properties_complete=false on the table, got true")
	}

	type capturedStat struct {
		statsID                        int64
		status, columns                string
		rows, rowsSampled, lastUpdated model.Cell
		modCounter                     model.Cell
	}
	captured := map[string]capturedStat{}
	for _, row := range tbl.rows {
		name, _ := row[1].(string)
		if name != "St_Granted" && name != "St_Withheld" && name != "St_Ordered" {
			continue
		}
		statsID, _ := row[0].(int64)
		columns, _ := row[2].(string)
		status, _ := row[11].(string)
		captured[name] = capturedStat{
			statsID: statsID, status: status, columns: columns,
			rows: row[3], rowsSampled: row[4], lastUpdated: row[6], modCounter: row[7],
		}
	}
	granted, withheld, ordered := captured["St_Granted"], captured["St_Withheld"], captured["St_Ordered"]

	if granted.status != "available" {
		t.Fatalf("St_Granted (column-level SELECT granted): want properties_status=available, got %q", granted.status)
	}
	if granted.rows == nil {
		t.Fatalf("St_Granted: properties_status=available but rows cell is NULL - the granted row must carry its own real values")
	}
	if withheld.status != "permission_denied" && withheld.status != "unavailable" {
		t.Fatalf("St_Withheld (no SELECT granted): want permission_denied or unavailable, got %q", withheld.status)
	}
	if withheld.status == granted.status {
		t.Fatalf("St_Granted and St_Withheld must not share the same properties_status - that is exactly the "+
			"decide-once-for-the-command defect this fixture exists to catch, got %q for both", granted.status)
	}
	// St_Ordered needs SELECT on BOTH Withheld and Granted to be
	// readable; this principal has only Granted, so it must be denied
	// exactly like St_Withheld, never "available".
	if ordered.status == "available" {
		t.Fatalf("St_Ordered (SELECT missing on one of its two columns): want permission_denied or unavailable, got available")
	}

	// Dispatch A2 (fix 2): every cell compared below is a second,
	// independent reading of the engine via lab.Admin (sysadmin, always
	// visible) - never a literal copied from the fixture's own DDL, so
	// this tells apart "the command read the right row" from "the
	// command produced a plausible-looking value". No data in
	// dbo.StatsFixture changes between the two reads (no INSERT/UPDATE
	// runs anywhere in this test after objects.sql's own setup), so
	// rows/rows_sampled/modification_counter/last_updated are stable
	// and compared for exact equality - no invented tolerance.
	var objectID int64
	if err := lab.Admin.QueryRowContext(ctx, "SELECT OBJECT_ID(N'dbo.StatsFixture')").Scan(&objectID); err != nil {
		t.Fatalf("resolving dbo.StatsFixture's own object_id: %v", err)
	}

	for name, stat := range map[string]capturedStat{"St_Granted": granted, "St_Withheld": withheld, "St_Ordered": ordered} {
		wantColumns := adminStatsColumns(t, lab, ctx, objectID, stat.statsID)
		if stat.columns != wantColumns {
			t.Fatalf("%s columns cell: got %q, want %q (re-read from sys.stats_columns/sys.columns)", name, stat.columns, wantColumns)
		}
	}

	grantedAdmin := readAdminStatsProperties(t, lab, ctx, objectID, granted.statsID)
	if !grantedAdmin.rows.Valid || !grantedAdmin.rowsSampled.Valid || !grantedAdmin.modCounter.Valid || !grantedAdmin.lastUpdated.Valid {
		t.Fatalf("St_Granted: admin's own independent read found an unreadable property, fixture assumption broken: %#v", grantedAdmin)
	}
	if granted.rows != grantedAdmin.rows.Int64 {
		t.Fatalf("St_Granted rows cell: got %v, admin's independent read says %d", granted.rows, grantedAdmin.rows.Int64)
	}
	if granted.rowsSampled != grantedAdmin.rowsSampled.Int64 {
		t.Fatalf("St_Granted rows_sampled cell: got %v, admin's independent read says %d", granted.rowsSampled, grantedAdmin.rowsSampled.Int64)
	}
	if granted.modCounter != grantedAdmin.modCounter.Int64 {
		t.Fatalf("St_Granted modification_counter cell: got %v, admin's independent read says %d", granted.modCounter, grantedAdmin.modCounter.Int64)
	}
	wantLastUpdated := grantedAdmin.lastUpdated.Time.Format("2006-01-02T15:04:05.9999999")
	if granted.lastUpdated != wantLastUpdated {
		t.Fatalf("St_Granted last_updated cell: got %v, admin's independent read says %q", granted.lastUpdated, wantLastUpdated)
	}

	// The asymmetry that proves the denial is real and per-row: admin's
	// own independent read of St_Withheld sees real values (the blob
	// exists, this principal simply cannot read it), while the command
	// itself rendered every one of those same cells NULL.
	withheldAdmin := readAdminStatsProperties(t, lab, ctx, objectID, withheld.statsID)
	if !withheldAdmin.rows.Valid || !withheldAdmin.modCounter.Valid {
		t.Fatalf("St_Withheld: admin's own independent read found it unreadable too, fixture assumption broken: %#v", withheldAdmin)
	}
	if withheld.rows != nil || withheld.rowsSampled != nil || withheld.modCounter != nil || withheld.lastUpdated != nil {
		t.Fatalf("St_Withheld: command must render every properties cell NULL when denied, got %#v", withheld)
	}

	// Dispatch B3: statistics' own stats_id ascending order (the same
	// design spec declaration B2 covers for suggestions) was never
	// checked against a real engine either - this table's own four
	// rows are enough to prove it.
	for i := 1; i < len(tbl.rows); i++ {
		prevID, _ := tbl.rows[i-1][0].(int64)
		curID, _ := tbl.rows[i][0].(int64)
		if prevID > curID {
			t.Fatalf("statistics rows are not stats_id ascending: row %d has stats_id %d after %d", i, curID, prevID)
		}
	}
}
