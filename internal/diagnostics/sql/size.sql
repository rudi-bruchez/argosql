-- size.sql builds "size table"'s allocations rows, one row per
-- (index_id, partition_number, allocation_type) actually present for
-- @id, by unpivoting sys.dm_db_partition_stats' three used/reserved
-- page-count pairs (in_row, LOB, row_overflow) rather than joining
-- anything: In-row, LOB and row-overflow are a breakdown of the same
-- totals table.sql/this query's own used_page_count/reserved_page_count
-- would give, never additional totals to sum a second time (design
-- spec line 174). A join here, rather than three independent SELECTs
-- UNIONed, would multiply this DMV's own one-row-per-partition shape
-- by the number of categories - exactly the double counting the design
-- spec forbids.
--
-- A category with zero used AND zero reserved pages for a given
-- (index_id, partition_number) is left out of its branch entirely - a
-- plain heap or clustered table with no LOB/overflow data never gets
-- LOB_DATA/ROW_OVERFLOW_DATA rows at all, so "the categories a table
-- actually uses" is exactly this query's result, never a fixed three
-- rows per partition regardless of whether it uses them.
--
-- allocation_type is spelled exactly as sys.allocation_units.type_desc
-- would (IN_ROW_DATA, LOB_DATA, ROW_OVERFLOW_DATA): a columnstore
-- index's compressed segments, which the engine itself stores as
-- LOB_DATA, are labeled LOB_DATA here too, like any other LOB
-- allocation - never invented as a fourth category, and never
-- described in this program's output as solely user LOB columns
-- (design spec: "columnstore allocations retain the DMV's LOB category
-- and must not be described as solely user LOB columns").
--
-- Ordered by index_id, then partition_number, then allocation_type -
-- the design spec's own declared row order for allocation rows.
SELECT index_id, partition_number, 'IN_ROW_DATA' AS allocation_type,
       in_row_used_page_count AS used_pages, in_row_reserved_page_count AS reserved_pages
FROM sys.dm_db_partition_stats
WHERE object_id = @id AND (in_row_used_page_count > 0 OR in_row_reserved_page_count > 0)
UNION ALL
SELECT index_id, partition_number, 'LOB_DATA',
       lob_used_page_count, lob_reserved_page_count
FROM sys.dm_db_partition_stats
WHERE object_id = @id AND (lob_used_page_count > 0 OR lob_reserved_page_count > 0)
UNION ALL
SELECT index_id, partition_number, 'ROW_OVERFLOW_DATA',
       row_overflow_used_page_count, row_overflow_reserved_page_count
FROM sys.dm_db_partition_stats
WHERE object_id = @id AND (row_overflow_used_page_count > 0 OR row_overflow_reserved_page_count > 0)
ORDER BY index_id, partition_number, allocation_type;
