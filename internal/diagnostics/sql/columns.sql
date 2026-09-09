-- columns.sql lists @id's columns in ordinal order (sys.columns.column_id),
-- each with its base type name, declared length/precision/scale,
-- nullability, identity and computed flags, and the definition text of
-- a default constraint or of a computed column, when either applies -
-- LEFT JOINed, so a column with neither still gets a row, with NULL in
-- whichever of the two does not apply to it.
--
-- max_length is returned exactly as sys.columns stores it: SQL Server's
-- own byte length for the type (-1 means MAX, unconditionally, for
-- every type it applies to), never divided by two for nchar/nvarchar's
-- double-byte characters - that conversion is a display decision the
-- command's own help documents explicitly (design spec's Unicode
-- length clause), not a second column this query invents.
SELECT
    c.column_id AS ordinal,
    c.name,
    ty.name AS type_name,
    c.max_length,
    c.precision,
    c.scale,
    c.is_nullable,
    c.is_identity,
    c.is_computed,
    dc.definition AS default_definition,
    cc.definition AS computed_definition
FROM sys.columns AS c
JOIN sys.types AS ty ON ty.user_type_id = c.user_type_id
LEFT JOIN sys.default_constraints AS dc
    ON dc.parent_object_id = c.object_id AND dc.parent_column_id = c.column_id
LEFT JOIN sys.computed_columns AS cc
    ON cc.object_id = c.object_id AND cc.column_id = c.column_id
WHERE c.object_id = @id
ORDER BY c.column_id;
