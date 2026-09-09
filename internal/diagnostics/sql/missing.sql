-- missing.sql ranks this database's unapplied missing-index suggestions
-- by a transparent impact score (design spec: "Missing-index impact is
-- (user_seeks + user_scans) * avg_total_user_cost * avg_user_impact / 100;
-- label it a ranking score, not predicted elapsed-time savings" - the
-- column is named impact_score, in the query and in the table, for
-- exactly that reason).
--
-- LEFT JOINs to sys.objects/sys.schemas, never inner joins (design
-- spec: "Use left joins for optional local names so metadata visibility
-- cannot silently remove DMV evidence"): a principal who can see the
-- instance-level DMV evidence but not a given object's own catalog row
-- still gets the row, with schema_name/object_name NULL, rather than
-- no row at all.
--
-- d.database_id = DB_ID() is applied before the optional object filter:
-- sys.dm_db_missing_index_details is an instance-wide view, and two
-- databases can carry the exact same object_id - without this filter a
-- row from a different database could present itself as missing-index
-- evidence for a table in this one (design spec: "filter on
-- details.database_id = DB_ID()").
--
-- @object_id is NULL unless --table was given, and is always this
-- program's own resolved object id (sqlserver.Resolve), never the raw
-- --table text: resolution happens first, so an unresolved --table name
-- fails at code 8 before this query ever runs (design spec: "An
-- optional table filter must first resolve through the object-visibility
-- contract").
SELECT TOP (@top)
    d.index_handle, d.object_id, s.name AS schema_name, o.name AS object_name,
    d.equality_columns, d.inequality_columns, d.included_columns,
    g.user_seeks, g.user_scans, g.avg_total_user_cost, g.avg_user_impact,
    (CONVERT(float, g.user_seeks) + g.user_scans) * g.avg_total_user_cost * g.avg_user_impact / 100.0 AS impact_score
FROM sys.dm_db_missing_index_group_stats AS g
JOIN sys.dm_db_missing_index_groups AS ig ON ig.index_group_handle = g.group_handle
JOIN sys.dm_db_missing_index_details AS d ON d.index_handle = ig.index_handle
LEFT JOIN sys.objects AS o ON o.object_id = d.object_id
LEFT JOIN sys.schemas AS s ON o.schema_id = s.schema_id
WHERE d.database_id = DB_ID() AND (@object_id IS NULL OR d.object_id = @object_id)
ORDER BY impact_score DESC, d.index_handle;
