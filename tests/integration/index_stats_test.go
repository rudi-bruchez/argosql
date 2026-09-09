//go:build integration

package integration

import (
	"context"
	"os"
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
	// Three rows, not two: dbo.StatsFixture's own clustered primary key
	// auto-creates a third statistic on Id, alongside the two explicit
	// CREATE STATISTICS this test cares about - measured, not assumed.
	tbl := sink.table("statistics")
	if tbl == nil || len(tbl.rows) != 3 {
		t.Fatalf("dbo.StatsFixture: want exactly three statistics rows (the PK's own plus St_Granted/St_Withheld), got %#v", tbl)
	}
	if tbl.propertiesComplete {
		t.Fatalf("mixed availability: want properties_complete=false on the table, got true")
	}

	var grantedStatus, withheldStatus string
	var grantedRowsCell model.Cell
	for _, row := range tbl.rows {
		name, _ := row[1].(string)
		status, _ := row[11].(string)
		switch name {
		case "St_Granted":
			grantedStatus = status
			grantedRowsCell = row[3]
		case "St_Withheld":
			withheldStatus = status
		}
	}
	if grantedStatus != "available" {
		t.Fatalf("St_Granted (column-level SELECT granted): want properties_status=available, got %q", grantedStatus)
	}
	if grantedRowsCell == nil {
		t.Fatalf("St_Granted: properties_status=available but rows cell is NULL - the granted row must carry its own real values")
	}
	if withheldStatus != "permission_denied" && withheldStatus != "unavailable" {
		t.Fatalf("St_Withheld (no SELECT granted): want permission_denied or unavailable, got %q", withheldStatus)
	}
	if withheldStatus == grantedStatus {
		t.Fatalf("St_Granted and St_Withheld must not share the same properties_status - that is exactly the "+
			"decide-once-for-the-command defect this fixture exists to catch, got %q for both", grantedStatus)
	}
}
