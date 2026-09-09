-- info.sql resolves the server/database identity the engine itself
-- reports for this connection: SERVERPROPERTY, DB_NAME(), USER_NAME(),
-- and the connected database's compatibility level. It never reads
-- config.Profile or the connection string: the design spec requires
-- info to carry "no connection string or secrets", and every value
-- below instead comes back from the engine, on this session, after
-- connection.
--
-- compatibility_level comes from sys.databases, not from
-- DATABASEPROPERTYEX: measured against a live SQL Server 2022
-- instance, DATABASEPROPERTYEX(DB_NAME(), 'Compatibility Level') is not
-- one of that function's documented properties at all and returns
-- NULL unconditionally - Microsoft's own property list names
-- 'Collation', 'Edition', 'Version' and others, but no compatibility
-- level. sys.databases.compatibility_level is the documented column
-- for it, and it is always visible for the database the current
-- connection is in.
--
-- SERVERPROPERTY returns sql_variant, which internal/output.ScanRow
-- has no case for (it decides every ambiguous conversion from
-- *sql.ColumnType.DatabaseTypeName(), never from the driver's Go type
-- alone, and sql_variant is not a type that function recognizes).
-- Every SERVERPROPERTY value is explicitly converted to nvarchar here
-- so ScanRow sees NVARCHAR, never sql_variant.
SELECT
    CONVERT(nvarchar(128), SERVERPROPERTY('ServerName'))     AS server,
    DB_NAME()                                                AS [database],
    USER_NAME()                                              AS principal,
    CONVERT(nvarchar(128), SERVERPROPERTY('ProductVersion')) AS product_version,
    CONVERT(nvarchar(128), SERVERPROPERTY('Edition'))        AS edition,
    CAST(d.compatibility_level AS int)                       AS compatibility_level
FROM sys.databases AS d
WHERE d.database_id = DB_ID();
