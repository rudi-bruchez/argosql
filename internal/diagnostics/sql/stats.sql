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
-- properties column NULL, rather than dropping it. Go decides
-- properties_status from that NULL (see stats.go): available when
-- modification_counter is present, otherwise a permission probe
-- decides permission_denied versus unavailable, never a guess from the
-- NULL alone.
--
-- rows/rows_sampled/last_updated/modification_counter come from the
-- APPLY; auto_created/user_created/filter_definition come straight from
-- sys.stats, which this principal must already see to reach this row
-- at all. last_updated can legitimately be NULL even when the
-- properties row itself exists (design spec: "Null last_updated can
-- legitimately mean that no statistics blob exists") - that is why Go
-- keys the available/unavailable decision on modification_counter,
-- never on last_updated.
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
    st.filter_definition AS filter
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
