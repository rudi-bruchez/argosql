-- stats.sql lists @id's statistics, one row per visible sys.stats row
-- (design spec: "stats list starts with visible sys.stats rows"), with
-- their ordered key columns pre-aggregated exactly like indexes.sql's
-- own keys/includes (STRING_AGG before any join that would otherwise
-- multiply rows), and OUTER APPLY sys.dm_db_stats_properties for the
-- update/sampling facts that function alone can read.
--
-- OUTER APPLY, never CROSS APPLY: design spec, "uses OUTER APPLY
-- sys.dm_db_stats_properties, preserving statistics with inaccessible
-- properties." sys.dm_db_stats_properties itself returns an empty
-- rowset, not an error, when the current principal lacks SELECT on the
-- statistic's columns (see Microsoft's own documented behavior) - an
-- OUTER APPLY keeps the sys.stats row in that case, with every
-- properties column NULL, rather than dropping it.
--
-- properties_stats_id (p.stats_id, re-selected from the APPLY side
-- rather than trusted from st.stats_id) is the ONE column Go's
-- availability decision may key on (fix 1's A1, measured on a real
-- engine): modification_counter and last_updated can BOTH legitimately
-- be NULL on a properties row that genuinely exists - a statistic on
-- an empty table, or one whose blob was never built, reads
-- p.stats_id NOT NULL with p.modification_counter and p.last_updated
-- both NULL. Before this fix, Go kept modification_counter IS NULL as
-- its own signal for "no properties row at all", which is a different,
-- narrower fact: the function returns NO row only when permission is
-- insufficient (object_id/stats_id here are always valid, pulled from
-- sys.stats itself, so every other documented empty-rowset cause is
-- already excluded) - never merely because the blob has no content
-- yet. properties_stats_id IS NULL is the one fact that actually means
-- "this principal could not read this statistic's properties at all".
--
-- rows/rows_sampled/last_updated/modification_counter come from the
-- APPLY; auto_created/user_created/filter_definition come straight from
-- sys.stats, which this principal must already see to reach this row
-- at all.
SELECT
    st.stats_id,
    st.name,
    cols.column_list AS columns,
    p.rows,
    p.rows_sampled,
    p.last_updated,
    p.modification_counter,
    st.auto_created,
    st.user_created,
    st.filter_definition AS filter,
    p.stats_id AS properties_stats_id
FROM sys.stats AS st
LEFT JOIN (
    SELECT sc.object_id, sc.stats_id,
           STRING_AGG(CAST(QUOTENAME(c.name) AS NVARCHAR(MAX)), ', ')
               WITHIN GROUP (ORDER BY sc.stats_column_id) AS column_list
    FROM sys.stats_columns AS sc
    JOIN sys.columns AS c ON c.object_id = sc.object_id AND c.column_id = sc.column_id
    WHERE sc.object_id = @id
    GROUP BY sc.object_id, sc.stats_id
) AS cols ON cols.object_id = st.object_id AND cols.stats_id = st.stats_id
OUTER APPLY sys.dm_db_stats_properties(st.object_id, st.stats_id) AS p
WHERE st.object_id = @id
ORDER BY st.stats_id;
