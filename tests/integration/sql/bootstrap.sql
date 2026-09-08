-- Fixture database for argosql's integration suite. Applied once per
-- container by fixture_test.go's applyBootstrap, running the whole script
-- against a connection whose current database is master: every object
-- below is named with the AppDB. prefix rather than reached through a USE
-- statement, because database/sql may hand each batch to a different
-- pooled physical connection and a USE issued in one batch is not
-- guaranteed to still be in effect for the next.
--
-- No container this harness creates is ever reused across runs, so the
-- guards below (DB_ID/OBJECT_ID checks) exist only to make a single
-- accidental re-run harmless, not to support incremental application.
--
-- Batches are separated by a line containing only "GO", the same
-- convention sqlcmd uses; fixture_test.go's batch splitter looks for
-- exactly that. CREATE DATABASE and the two ALTER DATABASE SET QUERY_STORE
-- statements each need their own batch: SQL Server rejects CREATE DATABASE
-- combined with other statements in one batch, and a QUERY_STORE state
-- change is not guaranteed visible to a statement that follows it in the
-- same batch.

IF DB_ID(N'AppDB') IS NULL
BEGIN
    CREATE DATABASE AppDB;
END
GO

ALTER DATABASE AppDB SET QUERY_STORE = ON;
GO

ALTER DATABASE AppDB SET QUERY_STORE
(OPERATION_MODE = READ_WRITE, QUERY_CAPTURE_MODE = ALL,
 INTERVAL_LENGTH_MINUTES = 1, DATA_FLUSH_INTERVAL_SECONDS = 60);
GO

IF OBJECT_ID(N'AppDB.dbo.Widgets') IS NULL
BEGIN
    CREATE TABLE AppDB.dbo.Widgets (
        Id INT IDENTITY(1,1) PRIMARY KEY,
        Name NVARCHAR(100) NOT NULL,
        Quantity INT NOT NULL DEFAULT 0
    );
END
GO

IF NOT EXISTS (SELECT 1 FROM AppDB.dbo.Widgets)
BEGIN
    INSERT INTO AppDB.dbo.Widgets (Name, Quantity) VALUES
        (N'bolt', 100),
        (N'nut', 250),
        (N'washer', 500);
END
GO
