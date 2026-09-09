-- query_plans_2022.sql is query_plans_2019.sql's sibling for SQL Server
-- 2022, which adds rs.replica_group_id to sys.query_store_runtime_stats
-- for secondary-replica Query Store capture. This file carries that
-- column through the inner "runtime" CTE's own GROUP BY (a row for
-- replica group A and one for replica group B of the same plan must
-- never be summed together) and LEFT JOINs it onto every plan by
-- plan_id alone: a plan with runtime in two different replica groups
-- fans out into two output rows (one per group), and a plan with no
-- matching runtime row at all in this window (in any group) still
-- produces exactly one output row, with replica_group_id NULL,
-- executions = 0 and every average NULL - design spec: "qs query
-- lists plans with no executions in the window explicitly" and "plan
-- rows sort by plan_id then replica_group_id". See query_plans_2019.sql
-- for every other aspect of this query (the LEFT-JOIN-from-the-plan
-- direction, the plan/interval duplicate-row dedup, execution_type = 0,
-- the sum(avg*count) weighting, the float CAST, and why plans are never
-- filtered to is_forced_plan = 1), which this file shares unchanged.
WITH runtime AS (
    SELECT
        rs.plan_id,
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
      AND rs.plan_id IN (SELECT plan_id FROM sys.query_store_plan WHERE query_id = @id)
    GROUP BY rs.plan_id, rs.replica_group_id
)
SELECT
    p.plan_id                                                        AS plan_id,
    r.replica_group_id                                                AS replica_group_id,
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
ORDER BY p.plan_id, r.replica_group_id;
