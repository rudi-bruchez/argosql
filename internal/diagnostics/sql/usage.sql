-- usage.sql lists @id's indexes (excluding the heap, index_id = 0,
-- exactly like indexes.sql) alongside whatever
-- sys.dm_db_index_usage_stats row the engine has recorded for each
-- since the instance last started - design spec: "sys.dm_db_index_usage_stats
-- is filtered by database_id = DB_ID() before joining local object
-- metadata." That filter lives inside the subquery below, applied
-- before the LEFT JOIN to sys.indexes, exactly as the spec's own
-- wording names it.
--
-- LEFT JOIN, never an inner join: a never-touched index legitimately
-- has no matching row in the DMV at all, and that absence must not
-- remove the index from this result - it is reported as
-- observation_status = 'never_observed', with every counter and
-- timestamp NULL, rather than a fabricated zero (design spec: "Do not
-- include a stale verdict in v0.1"; this program never turns a NULL
-- counter into an 'unused' conclusion either).
SELECT
    i.index_id,
    i.name,
    u.user_seeks,
    u.user_scans,
    u.user_lookups,
    u.user_updates,
    u.last_user_seek,
    u.last_user_scan,
    u.last_user_lookup,
    u.last_user_update,
    CASE WHEN u.index_id IS NULL THEN 'never_observed' ELSE 'observed' END AS observation_status
FROM sys.indexes AS i
LEFT JOIN (
    SELECT object_id, index_id, user_seeks, user_scans, user_lookups, user_updates,
           last_user_seek, last_user_scan, last_user_lookup, last_user_update
    FROM sys.dm_db_index_usage_stats
    WHERE database_id = DB_ID()
) AS u ON u.object_id = i.object_id AND u.index_id = i.index_id
WHERE i.object_id = @id AND i.index_id > 0
ORDER BY i.index_id;
