-- query.sql resolves one Query Store query's identity by query_id: its
-- parent module (when the catalog can resolve and show it), whether it
-- is an internal (system-generated) query, its query_hash, and its full
-- SQL text - read once here and reused by Go for both the "query"
-- table's text_preview cell and the complete .sql artifact Query (Go)
-- exports through Sink.File.
--
-- object_id is sys.query_store_query's own column: 0 for an ad-hoc
-- query that matches no module (never NULL - design spec: "Ad-hoc
-- queries with object_id 0 do not match this filter"), a real
-- sys.objects id for a statement compiled inside a module.
-- parent_schema/parent_name LEFT JOIN sys.objects/sys.schemas rather
-- than INNER JOIN: a module id that is no longer visible to the
-- current principal (the same catalog-metadata-visibility ambiguity
-- sqlserver.Resolve documents) or was dropped after its statements
-- were captured must not make the whole row disappear - Go reports
-- that case as parent_module = NULL plus a warning notice, never a
-- not-found error (design spec: "qs query | 0; parent name may be
-- unavailable").
--
-- Returns zero rows when query_id does not exist in this database, or
-- exists but is not visible to the current principal (the catalog
-- views themselves already restrict what is visible) - Go reports
-- that as code 8, not_found_or_not_visible, never an execution error,
-- and is internal queries resolve here exactly like any other query
-- (design spec: "Direct lookup with qs query <id> returns an existing
-- internal query and labels is_internal_query=true").
SELECT
    q.query_id          AS query_id,
    q.object_id         AS object_id,
    s.name              AS parent_schema,
    o.name              AS parent_name,
    q.is_internal_query AS is_internal_query,
    q.query_hash        AS query_hash,
    qt.query_sql_text   AS query_sql_text
FROM sys.query_store_query AS q
JOIN sys.query_store_query_text AS qt ON qt.query_text_id = q.query_text_id
LEFT JOIN sys.objects AS o ON o.object_id = q.object_id
LEFT JOIN sys.schemas AS s ON s.schema_id = o.schema_id
WHERE q.query_id = @id;
