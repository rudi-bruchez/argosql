-- indexes.sql lists @id's indexes, one row per index, with its ordered
-- key columns (direction included) and included columns each
-- pre-aggregated with STRING_AGG BEFORE joining sys.indexes - never
-- joined directly to sys.index_columns, which would multiply this
-- query's own one-row-per-index result by however many key/included
-- columns each index carries (design spec: "préagréger colonnes de
-- clés/inclusions avant jointure aux index").
--
-- Every column name is wrapped in QUOTENAME before aggregation: keys
-- and includes are display labels this program renders as text, never
-- concatenated into anything it executes as SQL (design spec:
-- "échapper leurs libellés dans la sortie, jamais concaténer comme SQL
-- exécutable") - QUOTENAME also protects a name that itself contains a
-- comma or a space from being misread as two column names once joined
-- by ", ".
--
-- Ordering inside each aggregate matches the design spec's own stated
-- key: key_ordinal for the key list, index_column_id for the include
-- list - not the same column, since an included column's position
-- among other included columns is not itself ordered by key_ordinal
-- (key_ordinal is 0 for every included column).
--
-- The key aggregate also filters ic.key_ordinal > 0, not merely
-- is_included_column = 0 (task 13 fix-1, measured): an implicitly
-- added partitioning column (CREATE INDEX ... ON a partition scheme)
-- carries is_included_column = 0 AND key_ordinal = 0, exactly like a
-- column that genuinely is not part of this index at all - it is
-- neither a declared key (real keys start at key_ordinal 1) nor an
-- included column, and sys.index_columns' own documentation names
-- key_ordinal = 0 as precisely this "not a key" case (also covering
-- XML and spatial indexes). Before this filter, such a column was
-- rendered as the index's own FIRST key, in key_ordinal order,
-- silently changing what searches/sorts the index actually supports.
--
-- A heap (index_id = 0) has no matching sys.indexes row at all - it is
-- deliberately absent from this result, exactly like every other
-- index this object does not have; obj table's own row-count query
-- (table.sql) is what still reports a heap's approximate row count.
--
-- Each STRING_AGG's own input expression is CAST to NVARCHAR(MAX)
-- explicitly (task 13 fix-1, measured): STRING_AGG's result type
-- matches its input expression's own declared length, and
-- QUOTENAME(sysname) is bounded well under 4,000 characters, so an
-- index with enough key/included columns silently truncated past
-- that bound and, worse, could fail outright once the truncated
-- aggregate collided with this query's own encoding - neither this
-- program's preview/artifact limits, which are meant to be the one
-- place a long result is bounded, ever got a chance to apply.
SELECT
    i.index_id,
    i.name,
    i.type_desc,
    keys.key_list AS keys,
    incl.include_list AS includes,
    i.filter_definition,
    i.is_unique,
    i.is_disabled
FROM sys.indexes AS i
LEFT JOIN (
    SELECT ic.object_id, ic.index_id,
           STRING_AGG(CAST(QUOTENAME(c.name) + CASE WHEN ic.is_descending_key = 1 THEN ' DESC' ELSE ' ASC' END AS NVARCHAR(MAX)), ', ')
               WITHIN GROUP (ORDER BY ic.key_ordinal) AS key_list
    FROM sys.index_columns AS ic
    JOIN sys.columns AS c ON c.object_id = ic.object_id AND c.column_id = ic.column_id
    WHERE ic.object_id = @id AND ic.is_included_column = 0 AND ic.key_ordinal > 0
    GROUP BY ic.object_id, ic.index_id
) AS keys ON keys.object_id = i.object_id AND keys.index_id = i.index_id
LEFT JOIN (
    SELECT ic.object_id, ic.index_id,
           STRING_AGG(CAST(QUOTENAME(c.name) AS NVARCHAR(MAX)), ', ')
               WITHIN GROUP (ORDER BY ic.index_column_id) AS include_list
    FROM sys.index_columns AS ic
    JOIN sys.columns AS c ON c.object_id = ic.object_id AND c.column_id = ic.column_id
    WHERE ic.object_id = @id AND ic.is_included_column = 1
    GROUP BY ic.object_id, ic.index_id
) AS incl ON incl.object_id = i.object_id AND incl.index_id = i.index_id
WHERE i.object_id = @id AND i.index_id > 0
ORDER BY i.index_id;
