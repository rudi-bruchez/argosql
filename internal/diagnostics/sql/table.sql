-- table.sql reads the facts "obj table" and "size table" share on
-- their common "table" header row (design spec's declared table
-- order: "obj table: table, columns, indexes"; "size table: table,
-- allocations" - the SAME "table" name in both, deliberately the same
-- TableSpec in Go): the object's approximate row count and its total
-- used/reserved pages from sys.dm_db_partition_stats, and whether it
-- is memory-optimized (whose size this program does not attempt to
-- read in v0.1 - design spec: "Memory-optimized table size is
-- unavailable with code 4 in v0.1").
--
-- Row count sums row_count only for index_id IN (0, 1) - the heap row
-- (0) or the clustered index row (1), never every index row, which
-- would multiply the true row count by however many nonclustered
-- indexes the table has (design spec line 174: "Table row count sums
-- row_count only for index_id IN (0,1)"). used_page_count and
-- reserved_page_count, by contrast, are summed across every index_id
-- and partition - the "sum used/reserved pages once across relevant
-- partitions and indexes" design spec line 174 also names - because
-- they are page counts, not row counts: an index's own pages are real
-- space the table occupies, not a duplicate of another index's rows.
-- Go computes bytes (*8192) and unused (reserved-used) from these two
-- sums; this query hands back raw page counts only (task 13 fix-1:
-- "size table" used to expose no total at all, only the per-row
-- allocation breakdown - design spec lines 53/174 require both).
--
-- The three correlated subqueries are the ONLY things besides
-- is_memory_optimized this SELECT projects, so a permission error
-- against sys.dm_db_partition_stats fails this whole one-row query
-- outright, rather than surfacing as a fabricated NULL: Go's caller
-- tells "these are legitimately NULL" (a memory-optimized table, or
-- an object sys.dm_db_partition_stats has no row for at all) apart
-- from "the query itself failed" by whether this query returns a row
-- or an error, never by inspecting the NULLs themselves.
--
-- LEFT JOIN sys.tables: only object type 'U' ever has a matching row
-- there, so is_memory_optimized comes back NULL - never an error - for
-- a view or a module, the other object types "obj table"/"size table"
-- might still be asked to read metadata for.
SELECT
    (SELECT SUM(CASE WHEN ps.index_id IN (0, 1) THEN ps.row_count ELSE 0 END)
     FROM sys.dm_db_partition_stats AS ps
     WHERE ps.object_id = @id) AS row_count,
    (SELECT SUM(ps.used_page_count)
     FROM sys.dm_db_partition_stats AS ps
     WHERE ps.object_id = @id) AS total_used_pages,
    (SELECT SUM(ps.reserved_page_count)
     FROM sys.dm_db_partition_stats AS ps
     WHERE ps.object_id = @id) AS total_reserved_pages,
    t.is_memory_optimized
FROM sys.objects AS o
LEFT JOIN sys.tables AS t ON t.object_id = o.object_id
WHERE o.object_id = @id;
