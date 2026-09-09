-- table.sql reads the one fact "obj table" and "size table" share on
-- their common "table" header row (design spec's declared table
-- order: "obj table: table, columns, indexes"; "size table: table,
-- allocations" - the SAME "table" name in both, deliberately the same
-- TableSpec in Go): the object's approximate row count from
-- sys.dm_db_partition_stats, and whether it is memory-optimized (whose
-- size this program does not attempt to read in v0.1 - design spec:
-- "Memory-optimized table size is unavailable with code 4 in v0.1").
--
-- Row count sums row_count only for index_id IN (0, 1) - the heap row
-- (0) or the clustered index row (1), never every index row, which
-- would multiply the true row count by however many nonclustered
-- indexes the table has (design spec line 174: "Table row count sums
-- row_count only for index_id IN (0,1)").
--
-- The correlated subquery is the ONLY thing besides is_memory_optimized
-- this SELECT projects, so its own failure (a permission error against
-- sys.dm_db_partition_stats) fails this whole one-row query outright,
-- rather than surfacing as a fabricated NULL: Go's caller tells
-- "row_count is legitimately NULL" (a memory-optimized table, or an
-- object sys.dm_db_partition_stats has no row for at all) apart from
-- "the query itself failed" by whether this query returns a row or an
-- error, never by inspecting the NULL itself.
--
-- LEFT JOIN sys.tables: only object type 'U' ever has a matching row
-- there, so is_memory_optimized comes back NULL - never an error - for
-- a view or a module, the other object types "obj table"/"size table"
-- might still be asked to read metadata for.
SELECT
    (SELECT SUM(CASE WHEN ps.index_id IN (0, 1) THEN ps.row_count ELSE 0 END)
     FROM sys.dm_db_partition_stats AS ps
     WHERE ps.object_id = @id) AS row_count,
    t.is_memory_optimized
FROM sys.objects AS o
LEFT JOIN sys.tables AS t ON t.object_id = o.object_id
WHERE o.object_id = @id;
