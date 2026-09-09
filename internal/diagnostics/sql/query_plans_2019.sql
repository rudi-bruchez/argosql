-- query_plans_2019.sql lists every recorded plan for one query_id, on
-- SQL Server 2019, with execution counts and totals/averages over a
-- requested window - reusing top_2019.sql's own weighted-average
-- formula exactly (see that file's own doc comment for why
-- execution_type = 0, the plan/interval duplicate-row dedup, the
-- sum(avg_metric * count_executions) weighting, and the float CAST),
-- with one deliberate difference: the join here runs from the plan
-- outward. "runtime" is LEFT JOINed onto every plan sys.query_store_plan
-- records for this query_id, never the other way around, so a plan
-- with zero executions in the requested window is still reported, with
-- executions = 0 and every average NULL, rather than silently dropped
-- by an inner join (design spec: "qs query lists plans with no
-- executions in the window explicitly, with zero executions and null
-- averages"). Plans are never filtered to is_forced_plan = 1: every
-- recorded plan for this query is reported, forced or not.
--
-- 2019's sys.query_store_runtime_stats has no replica_group_id column
-- at all (see top_2019.sql); this file returns a typed NULL in its
-- place, exactly like that one.
WITH runtime AS (
    SELECT
        rs.plan_id,
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
      AND rs.plan_id IN (SELECT plan_id FROM sys.query_store_plan WHERE query_id = @id)
    GROUP BY rs.plan_id
)
SELECT
    p.plan_id                                                        AS plan_id,
    CAST(NULL AS bigint)                                              AS replica_group_id,
    p.is_forced_plan                                                  AS forced,
    ISNULL(r.executions, 0)                                           AS executions,
    CAST(ISNULL(r.cpu_us, 0) AS float) / 1000.0                       AS cpu_total_ms,
    CASE WHEN r.executions IS NULL OR r.executions = 0 THEN NULL
         ELSE CAST(r.cpu_us AS float) / r.executions / 1000.0 END     AS cpu_avg_ms,
    CAST(ISNULL(r.duration_us, 0) AS float) / 1000.0                  AS duration_total_ms,
    CASE WHEN r.executions IS NULL OR r.executions = 0 THEN NULL
         ELSE CAST(r.duration_us AS float) / r.executions / 1000.0 END AS duration_avg_ms,
    CAST(ISNULL(r.reads_pages, 0) AS float)                           AS reads_total,
    CASE WHEN r.executions IS NULL OR r.executions = 0 THEN NULL
         ELSE CAST(r.reads_pages AS float) / r.executions END         AS reads_avg
FROM sys.query_store_plan AS p
LEFT JOIN runtime AS r ON r.plan_id = p.plan_id
WHERE p.query_id = @id
ORDER BY p.plan_id;
