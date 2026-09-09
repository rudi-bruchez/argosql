-- top_2019.sql ranks Query Store queries by total/average CPU,
-- duration, logical reads, or plain execution count, over a requested
-- window, on SQL Server 2019. 2019's sys.query_store_runtime_stats has
-- no replica_group_id column at all - that column arrives in 2022 for
-- secondary-replica Query Store capture - so this file never selects
-- it; it returns a typed NULL in its place instead. See top_2022.sql
-- for that version's own form, which carries the real column through
-- every GROUP BY and the ORDER BY tie-break.
--
-- sys.query_store_runtime_stats can hold more than one row for the
-- SAME (plan_id, runtime_stats_interval_id) on the current, still-
-- accumulating interval - one row already flushed to disk, others
-- still in memory (Microsoft's own documentation of the view). The
-- inner "runtime" CTE sums count_executions and every weighted metric
-- per plan/interval FIRST, before this query ever combines plans
-- together for the same query_id; picking one representative row
-- instead would silently drop real executions.
--
-- execution_type = 0 means "regular"; the view's only other documented
-- values are 3 (aborted by the client) and 4 (aborted by an exception),
-- so filtering to 0 and filtering out {3,4} are the same predicate -
-- 0 is kept because it stays correct if the engine ever adds a new
-- value this file has never seen.
--
-- Totals are sum(avg_metric * count_executions); averages divide those
-- totals by summed executions, NULLIF'd so a zero denominator returns
-- NULL rather than dividing by zero. CPU and duration stay in
-- microseconds through every SUM and are converted to milliseconds
-- only in the final SELECT; logical reads stay in 8-KB pages
-- throughout (design spec). Every computed metric is explicitly CAST
-- to float, so internal/output.ScanRow reports it as a JSON number -
-- these are estimates built from averages, not exact counters, and the
-- design spec says so explicitly; only "executions" itself stays an
-- exact bigint.
--
-- @@ORDER_COLUMN@@ is substituted, in Go (top.go's orderColumnFor),
-- with one literal column name chosen from a fixed map keyed by
-- --by/--aggregate - both already restricted to a static Enum by
-- internal/cli's registry, never free text - before this text is ever
-- sent to the server.
WITH runtime AS (
    SELECT
        rs.plan_id,
        rs.runtime_stats_interval_id,
        SUM(CONVERT(bigint, rs.count_executions))          AS executions,
        SUM(rs.avg_cpu_time * rs.count_executions)          AS cpu_us,
        SUM(rs.avg_duration * rs.count_executions)          AS duration_us,
        SUM(rs.avg_logical_io_reads * rs.count_executions)  AS reads_pages
    FROM sys.query_store_runtime_stats AS rs
    JOIN sys.query_store_runtime_stats_interval AS i
        ON i.runtime_stats_interval_id = rs.runtime_stats_interval_id
    WHERE rs.execution_type = 0
      AND i.start_time < @until
      AND i.end_time > @since
    GROUP BY rs.plan_id, rs.runtime_stats_interval_id
)
SELECT TOP (@top)
    q.query_id                                                                AS query_id,
    CAST(NULL AS bigint)                                                      AS replica_group_id,
    SUM(r.executions)                                                         AS executions,
    CAST(SUM(r.cpu_us) AS float) / 1000.0                                     AS cpu_total_ms,
    CAST(SUM(r.cpu_us) AS float) / NULLIF(SUM(r.executions), 0) / 1000.0      AS cpu_avg_ms,
    CAST(SUM(r.duration_us) AS float) / 1000.0                                AS duration_total_ms,
    CAST(SUM(r.duration_us) AS float) / NULLIF(SUM(r.executions), 0) / 1000.0 AS duration_avg_ms,
    CAST(SUM(r.reads_pages) AS float)                                         AS reads_total,
    CAST(SUM(r.reads_pages) AS float) / NULLIF(SUM(r.executions), 0)          AS reads_avg
FROM runtime AS r
JOIN sys.query_store_plan AS p ON p.plan_id = r.plan_id
JOIN sys.query_store_query AS q ON q.query_id = p.query_id
WHERE (@include_internal = 1 OR q.is_internal_query = 0)
  AND (@object_id IS NULL OR q.object_id = @object_id)
GROUP BY q.query_id
HAVING SUM(r.executions) >= @min_executions
ORDER BY @@ORDER_COLUMN@@ DESC, q.query_id;
