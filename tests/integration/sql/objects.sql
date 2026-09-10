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

-- Task 13's own fixture objects, for obj table/obj code/idx list/size
-- table.
--
-- dbo.DeniedDefinitionProc was built to demonstrate obj code's
-- permission_denied state via an object-level DENY VIEW DEFINITION
-- on top of I/S's own database-wide GRANT VIEW DEFINITION
-- (principals.sql). Measured against a real engine (fix-0): this
-- does NOT produce a visible-but-denied module - DENY VIEW DEFINITION
-- removes the object's sys.objects row entirely for that principal,
-- regardless of any other grant (GRANT EXECUTE included), so Resolve
-- itself fails at code 8 rather than reaching a definition_state at
-- all. Left in place as a documented negative result, not a fixture
-- any test currently exercises for that purpose - see
-- dbo.ExecuteOnlyProc below for the fixture that actually works.
IF OBJECT_ID(N'AppDB.dbo.DeniedDefinitionProc') IS NOT NULL
    DROP PROCEDURE dbo.DeniedDefinitionProc;
GO

CREATE PROCEDURE dbo.DeniedDefinitionProc
AS
BEGIN
    SELECT 1 AS Placeholder;
END
GO

-- dbo.ExecuteOnlyProc: obj code's REAL permission_denied fixture
-- (fix-0). Design spec line 204 says "a confirmed denied definition
-- permission" - "denied" there does not require an explicit DENY: the
-- ABSENCE of a VIEW DEFINITION grant is enough for the permission to
-- be missing. principals.sql grants Q (who holds no VIEW DEFINITION
-- anywhere, schema- or database-wide) EXECUTE on this one procedure
-- and nothing else. Measured against a real engine: EXECUTE alone
-- keeps the object visible in sys.objects (metadata visibility needs
-- only SOME permission, and EXECUTE qualifies here - unlike the
-- DeniedDefinitionProc case above, where VIEW DEFINITION was
-- explicitly denied and removed visibility outright), while
-- sys.sql_modules.definition reads NULL and HAS_PERMS_BY_NAME on VIEW
-- DEFINITION reads 0 (Denied) for that same principal - exactly the
-- state the spec describes.
IF OBJECT_ID(N'AppDB.dbo.ExecuteOnlyProc') IS NOT NULL
    DROP PROCEDURE dbo.ExecuteOnlyProc;
GO

CREATE PROCEDURE dbo.ExecuteOnlyProc
AS
BEGIN
    SELECT 1 AS Placeholder;
END
GO

-- dbo.PlainModule: an ordinary, unencrypted, undenied procedure - the
-- "obj code" matrix fixture (design spec's Q/I/S permission table):
-- unlike dbo.EncryptedProc (encrypted) and dbo.DeniedDefinitionProc
-- (VIEW DEFINITION explicitly denied to I/S), this one's definition is
-- readable by I and S under the plain schema-wide GRANT VIEW
-- DEFINITION principals.sql already gives them, so obj code on it
-- reaches definitionStateAvailable rather than one of the three
-- failure states.
IF OBJECT_ID(N'AppDB.dbo.PlainModule') IS NOT NULL
    DROP PROCEDURE dbo.PlainModule;
GO

CREATE PROCEDURE dbo.PlainModule
AS
BEGIN
    SELECT 1 AS Placeholder;
END
GO

-- dbo.ColumnsFixture exercises columns.sql's default/computed columns
-- and indexes.sql's ordered composite keys (with direction), included
-- columns, a unique filtered index, and a disabled index.
IF OBJECT_ID(N'AppDB.dbo.ColumnsFixture') IS NULL
BEGIN
    CREATE TABLE AppDB.dbo.ColumnsFixture (
        Id INT IDENTITY(1,1) PRIMARY KEY,
        Price DECIMAL(12,4) NOT NULL DEFAULT (0),
        Quantity INT NOT NULL,
        Extended AS (Price * Quantity) PERSISTED,
        Note NVARCHAR(50) NULL DEFAULT (N'n/a')
    );
END
GO

IF NOT EXISTS (SELECT 1 FROM AppDB.dbo.ColumnsFixture)
BEGIN
    INSERT INTO AppDB.dbo.ColumnsFixture (Price, Quantity, Note) VALUES
        (9.99, 3, N'first'),
        (1.50, 100, NULL);
END
GO

IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE object_id = OBJECT_ID(N'AppDB.dbo.ColumnsFixture') AND name = N'IX_ColumnsFixture_Composite')
BEGIN
    CREATE NONCLUSTERED INDEX IX_ColumnsFixture_Composite
        ON AppDB.dbo.ColumnsFixture (Quantity DESC, Price ASC)
        INCLUDE (Note);
END
GO

IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE object_id = OBJECT_ID(N'AppDB.dbo.ColumnsFixture') AND name = N'UX_ColumnsFixture_Note')
BEGIN
    CREATE UNIQUE NONCLUSTERED INDEX UX_ColumnsFixture_Note
        ON AppDB.dbo.ColumnsFixture (Note)
        WHERE Note IS NOT NULL;
END
GO

IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE object_id = OBJECT_ID(N'AppDB.dbo.ColumnsFixture') AND name = N'IX_ColumnsFixture_Disabled')
BEGIN
    CREATE INDEX IX_ColumnsFixture_Disabled ON AppDB.dbo.ColumnsFixture (Quantity);
    ALTER INDEX IX_ColumnsFixture_Disabled ON AppDB.dbo.ColumnsFixture DISABLE;
END
GO

-- dbo.SizeHeap: a heap (no clustered index), for size table's
-- index_id = 0 row-count case and its own IN_ROW_DATA-only allocation.
IF OBJECT_ID(N'AppDB.dbo.SizeHeap') IS NULL
BEGIN
    CREATE TABLE AppDB.dbo.SizeHeap (
        Value INT NOT NULL,
        INDEX IX_SizeHeap_Value NONCLUSTERED (Value)
    );
END
GO

IF NOT EXISTS (SELECT 1 FROM AppDB.dbo.SizeHeap)
BEGIN
    INSERT INTO AppDB.dbo.SizeHeap (Value)
    SELECT TOP (200) ROW_NUMBER() OVER (ORDER BY (SELECT NULL))
    FROM sys.all_objects AS a CROSS JOIN sys.all_objects AS b;
END
GO

-- dbo.SizeEmpty: a real, empty table (clustered PK, zero rows) - size
-- table's "objet vide" case: a legitimate row_count of 0, not an
-- error, and a partition-stats row that exists even with nothing in
-- it.
IF OBJECT_ID(N'AppDB.dbo.SizeEmpty') IS NULL
BEGIN
    CREATE TABLE AppDB.dbo.SizeEmpty (
        Id INT IDENTITY(1,1) PRIMARY KEY,
        Value INT NOT NULL
    );
END
GO

-- dbo.SizeFixture: one clustered index, two nonclustered indexes, and
-- an NVARCHAR(MAX) column filled past the in-row limit plus three
-- VARCHAR(4000) columns whose combined content exceeds 8060 bytes -
-- the single fixture task 13's own brief names for the extension
-- assertion: "sur une table à un index clusterisé, deux non
-- clusterisés et une colonne LOB remplie, la table allocations porte
-- plus d'une ligne [et] ses catégories couvrent les trois valeurs
-- d'allocation_type rencontrées."
IF OBJECT_ID(N'AppDB.dbo.SizeFixture') IS NULL
BEGIN
    CREATE TABLE AppDB.dbo.SizeFixture (
        Id INT IDENTITY(1,1) NOT NULL,
        Category INT NOT NULL,
        Label NVARCHAR(100) NOT NULL,
        Overflow1 VARCHAR(4000) NOT NULL,
        Overflow2 VARCHAR(4000) NOT NULL,
        Overflow3 VARCHAR(4000) NOT NULL,
        Blob NVARCHAR(MAX) NOT NULL,
        CONSTRAINT PK_SizeFixture PRIMARY KEY CLUSTERED (Id)
    );
    CREATE NONCLUSTERED INDEX IX_SizeFixture_Category ON AppDB.dbo.SizeFixture (Category);
    CREATE NONCLUSTERED INDEX IX_SizeFixture_Label ON AppDB.dbo.SizeFixture (Label);
END
GO

IF NOT EXISTS (SELECT 1 FROM AppDB.dbo.SizeFixture)
BEGIN
    INSERT INTO AppDB.dbo.SizeFixture (Category, Label, Overflow1, Overflow2, Overflow3, Blob)
    SELECT TOP (50)
        ROW_NUMBER() OVER (ORDER BY (SELECT NULL)) % 5,
        N'row' + CAST(ROW_NUMBER() OVER (ORDER BY (SELECT NULL)) AS NVARCHAR(10)),
        REPLICATE('a', 4000), REPLICATE('b', 4000), REPLICATE('c', 4000),
        REPLICATE(N'x', 20000)
    FROM sys.all_objects AS a CROSS JOIN sys.all_objects AS b;
END
GO

-- dbo.SizePartitioned: a range-partitioned clustered table, three
-- partitions on the [PRIMARY] filegroup - size table's own
-- partition_number > 1 case.
IF NOT EXISTS (SELECT 1 FROM sys.partition_functions WHERE name = N'AsqSizePF')
BEGIN
    CREATE PARTITION FUNCTION AsqSizePF (INT) AS RANGE LEFT FOR VALUES (100, 200);
END
GO

IF NOT EXISTS (SELECT 1 FROM sys.partition_schemes WHERE name = N'AsqSizePS')
BEGIN
    CREATE PARTITION SCHEME AsqSizePS AS PARTITION AsqSizePF ALL TO ([PRIMARY]);
END
GO

IF OBJECT_ID(N'AppDB.dbo.SizePartitioned') IS NULL
BEGIN
    CREATE TABLE AppDB.dbo.SizePartitioned (
        Bucket INT NOT NULL,
        Value INT NOT NULL,
        CONSTRAINT PK_SizePartitioned PRIMARY KEY CLUSTERED (Bucket, Value)
    ) ON AsqSizePS (Bucket);
END
GO

IF NOT EXISTS (SELECT 1 FROM AppDB.dbo.SizePartitioned)
BEGIN
    INSERT INTO AppDB.dbo.SizePartitioned (Bucket, Value) VALUES
        (10, 1), (10, 2), (150, 1), (150, 2), (250, 1);
END
GO

-- dbo.SizeColumnstore: a rowstore table with a nonclustered columnstore
-- index, forced fully compressed with REORGANIZE - size table's own
-- columnstore case, whose compressed segments the DMV reports as
-- LOB_DATA (see sql/size.sql's own doc comment).
IF OBJECT_ID(N'AppDB.dbo.SizeColumnstore') IS NULL
BEGIN
    CREATE TABLE AppDB.dbo.SizeColumnstore (
        Id INT IDENTITY(1,1) PRIMARY KEY,
        Metric INT NOT NULL
    );
END
GO

IF NOT EXISTS (SELECT 1 FROM AppDB.dbo.SizeColumnstore)
BEGIN
    INSERT INTO AppDB.dbo.SizeColumnstore (Metric)
    SELECT TOP (500) ROW_NUMBER() OVER (ORDER BY (SELECT NULL))
    FROM sys.all_objects AS a CROSS JOIN sys.all_objects AS b;
END
GO

IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE object_id = OBJECT_ID(N'AppDB.dbo.SizeColumnstore') AND name = N'CSI_SizeColumnstore')
BEGIN
    CREATE NONCLUSTERED COLUMNSTORE INDEX CSI_SizeColumnstore ON AppDB.dbo.SizeColumnstore (Metric);
END
GO

ALTER INDEX CSI_SizeColumnstore ON AppDB.dbo.SizeColumnstore REORGANIZE WITH (COMPRESS_ALL_ROW_GROUPS = ON);
GO

-- Task 14's own fixture objects, for idx usage/idx missing/stats list.

-- dbo.UsageFixture: one index actually queried below (a seek on
-- Category), and one index created but never referenced by any
-- statement in this script - "idx usage"'s own never_observed case,
-- as opposed to a fabricated zero (design spec: "Do not include a
-- stale verdict in v0.1"). IX_UsageFixture_NeverQueried is created
-- AFTER the INSERT below, deliberately: measured against a real
-- engine, an index that already existed at INSERT time gets a
-- sys.dm_db_index_usage_stats row from that write alone (user_updates
-- nonzero), even though it is never read - that row's mere existence
-- would make it 'observed' rather than genuinely untouched. Creating
-- it last means no later statement in this script ever writes to or
-- reads through it, so it keeps no DMV row at all.
IF OBJECT_ID(N'dbo.UsageFixture') IS NULL
BEGIN
    CREATE TABLE dbo.UsageFixture (
        Id INT IDENTITY(1,1) PRIMARY KEY,
        Category NVARCHAR(50) NOT NULL,
        Note NVARCHAR(200) NULL
    );
    CREATE NONCLUSTERED INDEX IX_UsageFixture_Category ON dbo.UsageFixture (Category);
    INSERT INTO dbo.UsageFixture (Category, Note) VALUES (N'a', N'x'), (N'b', N'y'), (N'c', N'z');
    CREATE NONCLUSTERED INDEX IX_UsageFixture_NeverQueried ON dbo.UsageFixture (Note);
END
GO

DECLARE @UsageFixtureSeek NVARCHAR(50);
SELECT @UsageFixtureSeek = Category FROM dbo.UsageFixture WHERE Category = N'b';
GO

-- dbo.MissingIndexFixture: an unindexed, selective predicate column
-- (Status), queried below just selectively enough that the optimizer
-- records a real sys.dm_db_missing_index_details suggestion - a
-- covering-index decision the optimizer makes at compile time, not
-- dependent on repeated execution or a Query Store flush.
IF OBJECT_ID(N'dbo.MissingIndexFixture') IS NULL
BEGIN
    CREATE TABLE dbo.MissingIndexFixture (
        Id INT IDENTITY(1,1) PRIMARY KEY,
        Status NVARCHAR(20) NOT NULL,
        Payload NVARCHAR(400) NOT NULL
    );
    INSERT INTO dbo.MissingIndexFixture (Status, Payload)
    SELECT CASE WHEN rn = 1 THEN N'rare' ELSE N'common' END, REPLICATE(N'x', 400)
    FROM (
        SELECT TOP (200000) ROW_NUMBER() OVER (ORDER BY (SELECT NULL)) AS rn
        FROM sys.all_objects AS a CROSS JOIN sys.all_objects AS b
    ) AS n;
END
GO

DECLARE @MissingIndexFixtureHit INT;
SELECT @MissingIndexFixtureHit = Id FROM dbo.MissingIndexFixture WHERE Status = N'rare';
GO

-- dbo.MissingIndexHiddenFromS: the same shape, but a deliberately
-- LESS selective predicate (50 matching rows rather than 1) - fix 1's
-- B2, measured: an identical fixture to MissingIndexFixture produced
-- an IDENTICAL impact_score, which made a reversed ORDER BY
-- undetectable (both rows tied, so no row order is distinguishable
-- from any other). The differing selectivity gives this suggestion a
-- measurably different avg_user_impact/impact_score, so a test can
-- actually tell DESC apart from ASC. principals.sql's own per-object
-- DENY VIEW DEFINITION to asq_test_s makes this object's catalog row
-- invisible to S while leaving its instance-level DMV evidence visible
-- (design spec: "Use left joins for optional local names so metadata
-- visibility cannot silently remove DMV evidence") - the real-engine
-- counterpart to TestMissingCellValuesLeftJoinAndOrder's own
-- fake-driver proof.
IF OBJECT_ID(N'dbo.MissingIndexHiddenFromS') IS NULL
BEGIN
    CREATE TABLE dbo.MissingIndexHiddenFromS (
        Id INT IDENTITY(1,1) PRIMARY KEY,
        Status NVARCHAR(20) NOT NULL,
        Payload NVARCHAR(400) NOT NULL
    );
    INSERT INTO dbo.MissingIndexHiddenFromS (Status, Payload)
    SELECT CASE WHEN rn <= 50 THEN N'rare' ELSE N'common' END, REPLICATE(N'x', 400)
    FROM (
        SELECT TOP (200000) ROW_NUMBER() OVER (ORDER BY (SELECT NULL)) AS rn
        FROM sys.all_objects AS a CROSS JOIN sys.all_objects AS b
    ) AS n;
END
GO

DECLARE @MissingIndexHiddenFromSHit INT;
SELECT @MissingIndexHiddenFromSHit = Id FROM dbo.MissingIndexHiddenFromS WHERE Status = N'rare';
GO

-- dbo.StatsFixture: two independent single-column statistics, so a
-- principal with column-level SELECT on exactly one of them sees
-- mixed availability within the same "stats list" table (design spec
-- line 176: "SELECT on only one statistic's columns (mixed
-- availability)").
--
-- St_Ordered (fix 2's A1) is a SECOND statistic, with two columns
-- declared in an order that differs from their alphabetical order:
-- St_Granted/St_Withheld alone cannot exercise sql/stats.sql's own
-- "columns" STRING_AGG ... WITHIN GROUP (ORDER BY sc.stats_column_id),
-- which carries design spec line 57's "Ordered columns" requirement -
-- a single-column statistic makes that ordering clause unobservable
-- (measured: removing or reversing it changes nothing when there is
-- only ever one column to aggregate), the same shape of defect task 13
-- paid for under a different name (an "across indexes" order asserted
-- against a fixture with only one index).
IF OBJECT_ID(N'dbo.StatsFixture') IS NULL
BEGIN
    CREATE TABLE dbo.StatsFixture (
        Id INT IDENTITY(1,1) PRIMARY KEY,
        Granted NVARCHAR(50) NOT NULL,
        Withheld NVARCHAR(50) NOT NULL
    );
    INSERT INTO dbo.StatsFixture (Granted, Withheld) VALUES (N'a', N'x'), (N'b', N'y'), (N'c', N'z');
    CREATE STATISTICS St_Granted ON dbo.StatsFixture (Granted);
    CREATE STATISTICS St_Withheld ON dbo.StatsFixture (Withheld);
    CREATE STATISTICS St_Ordered ON dbo.StatsFixture (Withheld, Granted);
END
GO
