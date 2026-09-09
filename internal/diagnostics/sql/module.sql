-- module.sql answers "obj code"'s definition-state question for @id in
-- one round trip, re-joining sys.objects rather than trusting Resolve's
-- earlier read alone: if @id no longer exists there at all, this query
-- returns zero rows, which Code reads as the object having disappeared
-- between resolution and this read (design spec: "Recheck disappearance
-- during collection and report unavailable rather than treating it as
-- an empty definition") - never as an empty, successfully exported
-- module.
--
-- OBJECTPROPERTYEX(..., 'IsEncrypted') is read unconditionally,
-- alongside sys.sql_modules.definition, because it alone among these
-- two facts stays visible to a principal denied VIEW DEFINITION on
-- this exact object: WITH ENCRYPTION's own catalog bit is metadata
-- about the module, not the definition text it protects. This is what
-- lets Code tell "encrypted" (is_encrypted = 1, definition NULL
-- because the engine itself refuses to store it in cleartext for
-- anyone) apart from "visible, unencrypted, but this principal's VIEW
-- DEFINITION permission on it was denied" (is_encrypted = 0, definition
-- NULL) - the second case Code confirms separately with a permission
-- probe, never inferred from this query's NULL alone.
SELECT m.definition, CAST(OBJECTPROPERTYEX(o.object_id, 'IsEncrypted') AS bit) AS is_encrypted
FROM sys.objects AS o
LEFT JOIN sys.sql_modules AS m ON m.object_id = o.object_id
WHERE o.object_id = @id;
