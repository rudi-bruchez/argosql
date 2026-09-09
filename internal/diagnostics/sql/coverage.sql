-- coverage.sql reports the oldest and newest stored Query Store
-- interval, restricted to intervals that actually carry at least one
-- sys.query_store_runtime_stats row.
--
-- What that restriction actually guards against is narrower than it
-- looks. Measured on a disposable SQL Server 2022 CU26 container:
--
--   - on a genuinely fresh database with Query Store just turned on,
--     BOTH sys.query_store_runtime_stats_interval and
--     sys.query_store_runtime_stats are empty (intervals = 0,
--     runtime_rows = 0). The correlated form (this query), the
--     uncorrelated form, and an unrestricted MIN/MAX all return exactly
--     the same NULL/NULL here - it is the absence of any interval row
--     at all that produces that NULL, not the correlation to
--     runtime_stats;
--   - ordinary interval rollover does not separate them either: 746
--     samples over 150 seconds with INTERVAL_LENGTH_MINUTES = 1,
--     crossing two interval boundaries, produced zero divergence
--     between the correlated and uncorrelated forms and zero interval
--     row without a matching runtime_stats row. The engine only
--     materializes an interval row once it has statistics to persist
--     into it, never ahead of time for the interval currently
--     accumulating;
--   - the state that actually separates them is real, but transient:
--     running sys.sp_query_store_remove_query against every captured
--     query leaves one interval row (the current one) with no
--     surviving runtime_stats row behind it. That window lasts well
--     under a second.
--
-- So the measured fact is that the unrestricted form would return the
-- same result as this correlated one in ordinary operation, including
-- on a fresh database - it is not the restriction protecting the fresh
-- case, it is the fresh case having no interval row to mismatch in the
-- first place. The correlation stays in this query regardless: it keeps
-- a state a real sp_query_store_remove_query administrator action can
-- reach provably correct, even though that state does not last long
-- enough to be worth a test asserting on it - an assertion that has to
-- race a sub-second window would be worth less than saying so here.
SELECT
    MIN(i.start_time) AS oldest_interval,
    MAX(i.end_time)   AS newest_interval
FROM sys.query_store_runtime_stats_interval AS i
WHERE EXISTS (
    SELECT 1
    FROM sys.query_store_runtime_stats AS rs
    WHERE rs.runtime_stats_interval_id = i.runtime_stats_interval_id
);
