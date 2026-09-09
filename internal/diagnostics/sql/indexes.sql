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
-- A heap (index_id = 0) has no matching sys.indexes row at all - it is
-- deliberately absent from this result, exactly like every other
-- index this object does not have; obj table's own row-count query
-- (table.sql) is what still reports a heap's approximate row count.
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
           STRING_AGG(QUOTENAME(c.name) + CASE WHEN ic.is_descending_key = 1 THEN ' DESC' ELSE ' ASC' END, ', ')
               WITHIN GROUP (ORDER BY ic.key_ordinal) AS key_list
    FROM sys.index_columns AS ic
    JOIN sys.columns AS c ON c.object_id = ic.object_id AND c.column_id = ic.column_id
    WHERE ic.object_id = @id AND ic.is_included_column = 0
    GROUP BY ic.object_id, ic.index_id
) AS keys ON keys.object_id = i.object_id AND keys.index_id = i.index_id
LEFT JOIN (
    SELECT ic.object_id, ic.index_id,
           STRING_AGG(QUOTENAME(c.name), ', ')
               WITHIN GROUP (ORDER BY ic.index_column_id) AS include_list
    FROM sys.index_columns AS ic
    JOIN sys.columns AS c ON c.object_id = ic.object_id AND c.column_id = ic.column_id
    WHERE ic.object_id = @id AND ic.is_included_column = 1
    GROUP BY ic.object_id, ic.index_id
) AS incl ON incl.object_id = i.object_id AND incl.index_id = i.index_id
WHERE i.object_id = @id AND i.index_id > 0
ORDER BY i.index_id;
