-- health.sql reads sys.database_query_store_options' one row for the
-- current database - it always returns exactly one row per database,
-- even with Query Store OFF, per Microsoft's own documentation of the
-- view - plus a database-wide has_history fact: whether
-- sys.query_store_runtime_stats holds any row at all, independent of
-- any requested time window. This is the shared source query behind
-- both ReadHealth (every Query Store command's first read) and Status
-- (health.go): ReadHealth keeps only desired_state/actual_state/
-- readonly_reason/has_history, Status also keeps the display-only
-- fields (capture_mode, storage, retention, interval).
--
-- readonly_reason's bit values are documented at
-- https://learn.microsoft.com/en-us/sql/relational-databases/system-catalog-views/sys-database-query-store-options-transact-sql#columns
-- and decoded in Go by DecodeReadOnly (health.go) from that
-- documentation, never guessed from a SELECT * sample against one
-- observed server.
--
-- Every numeric/bit column is explicitly CAST to the type this query
-- declares, rather than left as whatever sys.database_query_store_options
-- itself happens to type them (bigint/smallint mixed with the
-- project's own BIGINT/INT/BIT/DECIMAL(10,2) column contracts) so
-- internal/output.ScanRow converts each one exactly once, unambiguously.
SELECT
    o.desired_state_desc                             AS desired_state,
    o.actual_state_desc                              AS actual_state,
    CAST(o.readonly_reason AS bigint)                AS readonly_reason,
    o.query_capture_mode_desc                        AS capture_mode,
    CAST(o.current_storage_size_mb AS decimal(10,2)) AS current_storage_mb,
    CAST(o.max_storage_size_mb AS decimal(10,2))     AS max_storage_mb,
    CAST(o.stale_query_threshold_days AS int)        AS retention_days,
    CAST(o.interval_length_minutes AS int)           AS interval_minutes,
    CAST(
        CASE WHEN EXISTS (SELECT 1 FROM sys.query_store_runtime_stats) THEN 1 ELSE 0 END
        AS bit
    ) AS has_history
FROM sys.database_query_store_options AS o;
