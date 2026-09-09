-- coverage.sql reports the oldest and newest stored Query Store
-- interval, restricted to intervals that actually carry at least one
-- sys.query_store_runtime_stats row.
--
-- That restriction is not cosmetic. Measured on a freshly created SQL
-- Server 2022 database, right after Query Store is turned on:
-- sys.query_store_runtime_stats_interval already holds a row - the
-- interval currently accumulating - before a single query has ever
-- executed, while sys.query_store_runtime_stats itself is still empty.
-- An unrestricted MIN(start_time)/MAX(end_time) over
-- query_store_runtime_stats_interval alone would report a plausible
-- one-hour-wide coverage window on that database, at the exact moment
-- has_history (health.sql) is reporting false - a diagnostic tool
-- telling its caller "data available since an hour ago" about a
-- database that has captured nothing sends that caller to go analyze
-- nothing.
--
-- Restricted to intervals that EXISTS in sys.query_store_runtime_stats,
-- MIN/MAX over zero matching rows is SQL NULL, which is exactly the
-- fact an empty history must report - never an invented window. See
-- internal/diagnostics/tests/integration's TestStatus subtest against a
-- freshly created database for the measurement this query is built to
-- satisfy.
SELECT
    MIN(i.start_time) AS oldest_interval,
    MAX(i.end_time)   AS newest_interval
FROM sys.query_store_runtime_stats_interval AS i
WHERE EXISTS (
    SELECT 1
    FROM sys.query_store_runtime_stats AS rs
    WHERE rs.runtime_stats_interval_id = i.runtime_stats_interval_id
);
