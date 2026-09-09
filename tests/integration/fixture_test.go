//go:build integration

package integration

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// bootstrapScript is sql/bootstrap.sql, applied once per container by
// NewLab before a Lab is handed to any test.
//
//go:embed sql/bootstrap.sql
var bootstrapScript string

// workloadScript is sql/workload.sql: the load Lab.QueryID (below) runs,
// with its {{MARKER}} placeholder substituted, to generate one Query
// Store entry a test can discover by query_id rather than guessing or
// hardcoding one.
//
//go:embed sql/workload.sql
var workloadScript string

// splitBatches cuts a sqlcmd-style script into batches on lines that are,
// once trimmed, exactly "GO" (case-insensitively, as sqlcmd itself
// accepts). CREATE DATABASE and a QUERY_STORE state change each need their
// own batch (see sql/bootstrap.sql's header comment), and database/sql has
// no notion of a multi-statement batch separator of its own: each batch
// becomes one ExecContext call.
func splitBatches(script string) []string {
	var batches []string
	var cur strings.Builder
	flush := func() {
		if b := strings.TrimSpace(cur.String()); b != "" {
			batches = append(batches, b)
		}
		cur.Reset()
	}
	for _, line := range strings.Split(script, "\n") {
		if strings.EqualFold(strings.TrimSpace(line), "GO") {
			flush()
			continue
		}
		cur.WriteString(line)
		cur.WriteByte('\n')
	}
	flush()
	return batches
}

// applyBootstrap runs every batch of sql/bootstrap.sql, in order, against
// db. Every object the script creates is named with an explicit AppDB.
// prefix so the whole script can run from db's own current database
// (master): database/sql may hand successive ExecContext calls to
// different pooled physical connections, and a USE issued by one batch
// would not be guaranteed to still be in effect for the next.
func applyBootstrap(ctx context.Context, db *sql.DB) error {
	for i, batch := range splitBatches(bootstrapScript) {
		if _, err := db.ExecContext(ctx, batch); err != nil {
			return fmt.Errorf("bootstrap batch %d: %w", i+1, err)
		}
	}
	return nil
}

// queryStoreMarker names a result column alias in the load
// TestFixtureQueryStoreFlush generates, so the poll loop below can find
// that exact query's text in Query Store rather than matching anything
// else AppDB's schema or another run might have left behind.
//
// It is a column alias, not a SQL comment: measured against a real
// container, SQL Server's own simple parameterization rewrites this
// query's text before Query Store ever sees it - the literal 0 in
// "Quantity >= 0" becomes "@1", brackets get added around identifiers,
// and any comment is discarded outright. An alias survives that rewrite;
// a comment does not.
const queryStoreMarker = "AsqFixtureQueryStoreMarker"

// flushPollDeadline bounds the wait for the flushed marker to become
// visible in Query Store; flushPollDelay is the short interval between
// polls, never one fixed sleep for the whole wait.
const (
	flushPollDeadline = 30 * time.Second
	flushPollDelay    = 300 * time.Millisecond
)

// TestFixtureQueryStoreFlush proves the fixture capability every later
// diagnostics task relies on: a query run against AppDB is captured by
// Query Store, sys.sp_query_store_flush_db forces it to disk, and the
// query's text becomes visible through the Query Store DMVs. The twelve
// tasks after this one build Query Store diagnostics on top of this
// database; if this round trip is not provably reliable now, every test
// built on it later would be guessing.
func TestFixtureQueryStoreFlush(t *testing.T) {
	lab := NewLab(t, os.Getenv("ASQ_TEST_IMAGE"))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var major int
	if err := lab.Admin.QueryRowContext(ctx, "SELECT CAST(SERVERPROPERTY('ProductMajorVersion') AS int)").Scan(&major); err != nil {
		t.Fatalf("reading engine major version: %v", err)
	}
	logEngineIdentity(t, lab, major)

	loadQuery := fmt.Sprintf("SELECT COUNT(*) AS %s FROM dbo.Widgets WHERE Quantity >= 0", queryStoreMarker)
	for i := 0; i < 5; i++ {
		var count int
		if err := lab.Admin.QueryRowContext(ctx, loadQuery).Scan(&count); err != nil {
			t.Fatalf("generating query store load: %v", err)
		}
	}

	if _, err := lab.Admin.ExecContext(ctx, "EXEC sys.sp_query_store_flush_db"); err != nil {
		t.Fatalf("sp_query_store_flush_db: %v", err)
	}

	findMarker := "SELECT COUNT(*) FROM sys.query_store_query_text WHERE query_sql_text LIKE '%' + @p1 + '%'"
	deadline := time.Now().Add(flushPollDeadline)
	for {
		var n int
		if err := lab.Admin.QueryRowContext(ctx, findMarker, queryStoreMarker).Scan(&n); err != nil {
			t.Fatalf("polling sys.query_store_query_text: %v", err)
		}
		if n > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("query store marker did not appear within %s of flush", flushPollDeadline)
		}
		time.Sleep(flushPollDelay)
	}
}

// QueryID runs sql/workload.sql's load against lab.Admin (AppDB), with
// {{MARKER}} replaced by marker, flushes Query Store, and polls -
// bounded by flushPollDeadline, the same 30s budget
// TestFixtureQueryStoreFlush proves reliable above - until a query
// carrying marker in its text is visible in Query Store, returning its
// query_id.
//
// This is task 10's own fixture-discovery mechanism, exposed as a Lab
// method so every later diagnostics task that needs a real, uniquely
// identifiable Query Store entry reaches for this helper instead of
// discovering (or worse, hardcoding) a query_id of its own - task 11
// reuses it verbatim rather than redefining it.
func (lab *Lab) QueryID(t *testing.T, marker string) int64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	load := strings.ReplaceAll(workloadScript, "{{MARKER}}", marker)
	for i := 0; i < 5; i++ {
		var count int
		if err := lab.Admin.QueryRowContext(ctx, load).Scan(&count); err != nil {
			t.Fatalf("running marked workload %q: %v", marker, err)
		}
	}

	if _, err := lab.Admin.ExecContext(ctx, "EXEC sys.sp_query_store_flush_db"); err != nil {
		t.Fatalf("sp_query_store_flush_db: %v", err)
	}

	findID := "SELECT q.query_id FROM sys.query_store_query_text AS qt " +
		"JOIN sys.query_store_query AS q ON q.query_text_id = qt.query_text_id " +
		"WHERE qt.query_sql_text LIKE '%' + @p1 + '%'"
	deadline := time.Now().Add(flushPollDeadline)
	for {
		var id int64
		err := lab.Admin.QueryRowContext(ctx, findID, marker).Scan(&id)
		if err == nil {
			return id
		}
		if !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("polling for query_id of marker %q: %v", marker, err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("query store marker %q did not appear within %s of flush", marker, flushPollDeadline)
		}
		time.Sleep(flushPollDelay)
	}
}
