-- top_2022.sql is top_2019.sql's sibling for SQL Server 2022, which
-- adds rs.replica_group_id to sys.query_store_runtime_stats for
-- secondary-replica Query Store capture. This file carries that column
-- through every stage: grouped in the inner "runtime" CTE (a row for
-- replica group A and one for replica group B of the same plan/interval
-- must never be summed together), grouped again in the outer
-- aggregation (so the same query_id ranked on two replica groups
-- produces two distinct rows, never one merged row), returned as its
-- own output column, and used as the ORDER BY tie-break after
-- query_id (design spec: "qs top ranks (query_id, replica_group_id)
-- ... Ties sort by query_id then replica_group_id"). See top_2019.sql's
-- own doc comment for every other aspect of this query (the
-- plan/interval duplicate-row dedup, execution_type = 0, the
-- sum(avg*count) weighting, the float CAST, and the @@ORDER_COLUMN@@
-- substitution point), which this file shares unchanged.
WITH runtime AS (
    SELECT
        rs.plan_id,
        rs.runtime_stats_interval_id,
        rs.replica_group_id,
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
    GROUP BY rs.plan_id, rs.runtime_stats_interval_id, rs.replica_group_id
)
SELECT TOP (@top)
    q.query_id                                                                AS query_id,
    r.replica_group_id                                                        AS replica_group_id,
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
GROUP BY q.query_id, r.replica_group_id
HAVING SUM(r.executions) >= @min_executions
ORDER BY @@ORDER_COLUMN@@ DESC, q.query_id, r.replica_group_id;
