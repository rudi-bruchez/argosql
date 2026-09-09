-- objects.sql creates the fixture objects permissions_test.go resolves
-- and probes against, beyond the bolt/nut/washer Widgets table
-- bootstrap.sql already provides:
--
--   dbo.Orders          a plain two-part name, the one the task 8 brief
--                        and TestEveryProbeIsWellFormed both use.
--   dbo.[Order.Detail]  an identifier that itself contains a literal dot,
--                        so resolving "dbo.[Order.Detail]" only works if
--                        the two-part splitter honors bracket quoting
--                        rather than splitting on every dot it sees.
--   dbo.EncryptedProc   a WITH ENCRYPTION module: its definition text is
--                        unreadable even to sysadmin, but its catalog row
--                        (object_id, schema, name, type) is not - Resolve
--                        reads only that row, so encryption must not
--                        affect it.
--   Restricted.Secret   a table under a schema that principals.sql denies
--                        SELECT on to I and S, to prove a probed SELECT
--                        permission reports Denied under an explicit DENY
--                        exactly as it would under a plain absence of
--                        grant - the two are indistinguishable through
--                        HAS_PERMS_BY_NAME and this package does not
--                        pretend otherwise.
--
-- Applied by permissions_test.go against lab.Admin, the same AppDB pool
-- applyBootstrap uses: every connection in that pool already defaults to
-- AppDB (set from the DSN, not from a USE), so - like bootstrap.sql -
-- nothing here depends on a USE persisting across batches that may land
-- on different pooled physical connections. Must run before
-- principals.sql, whose DENY statement targets the Restricted schema
-- this script creates.

IF OBJECT_ID(N'AppDB.dbo.Orders') IS NULL
BEGIN
    CREATE TABLE AppDB.dbo.Orders (
        OrderId INT IDENTITY(1,1) PRIMARY KEY,
        CustomerName NVARCHAR(100) NOT NULL,
        Total DECIMAL(10,2) NOT NULL DEFAULT 0
    );
END
GO

IF NOT EXISTS (SELECT 1 FROM AppDB.dbo.Orders)
BEGIN
    INSERT INTO AppDB.dbo.Orders (CustomerName, Total) VALUES
        (N'Alpha', 10.00),
        (N'Beta', 20.00);
END
GO

IF OBJECT_ID(N'AppDB.dbo.[Order.Detail]') IS NULL
BEGIN
    CREATE TABLE AppDB.dbo.[Order.Detail] (
        DetailId INT IDENTITY(1,1) PRIMARY KEY,
        Note NVARCHAR(100) NOT NULL
    );
END
GO

-- CREATE/DROP PROCEDURE take at most a schema-qualified name, never a
-- database prefix (unlike CREATE TABLE above): this connection's default
-- database is already AppDB (set from the DSN), so dbo.EncryptedProc
-- alone is unambiguous.
IF OBJECT_ID(N'AppDB.dbo.EncryptedProc') IS NOT NULL
    DROP PROCEDURE dbo.EncryptedProc;
GO

CREATE PROCEDURE dbo.EncryptedProc
WITH ENCRYPTION
AS
BEGIN
    SELECT 1 AS Placeholder;
END
GO

IF SCHEMA_ID(N'Restricted') IS NULL
    EXEC(N'CREATE SCHEMA Restricted');
GO

IF OBJECT_ID(N'AppDB.Restricted.Secret') IS NULL
BEGIN
    CREATE TABLE AppDB.Restricted.Secret (
        SecretId INT IDENTITY(1,1) PRIMARY KEY,
        Value NVARCHAR(100) NOT NULL
    );
END
GO
