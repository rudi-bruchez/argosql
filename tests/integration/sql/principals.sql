-- principals.sql creates the three SQL logins this suite's permission
-- tests run as, following the exact bundles the design spec defines:
--   Q (Query Store)        : CONNECT, VIEW DATABASE STATE
--   I (inspection)         : Q, plus VIEW DEFINITION and SELECT on dbo,
--                            plus (2022 only) VIEW DATABASE PERFORMANCE
--                            STATE and VIEW SECURITY DEFINITION
--   S (instance diagnostics): I, plus the instance-level state permission
-- Each is an independent SQL login/user: SQL Server has no login
-- inheritance, so I and S repeat everything Q and I already hold rather
-- than building on a shared role.
--
-- Passwords are never literals in this file: permissions_test.go
-- substitutes the __Q_PASSWORD__ / __I_PASSWORD__ / __S_PASSWORD__
-- tokens below for harness-generated random passwords (see
-- randomPassword in podman_test.go) before executing any of this script,
-- exactly like NewLab already does for the container's own sa password.
--
-- This file has two halves, split at the marker line below: everything
-- above it runs against a connection to master (CREATE LOGIN and GRANT
-- on a server-level permission are both server-principal operations, not
-- bound to any one database); everything below runs against AppDB
-- (CREATE USER and every database-scoped GRANT/DENY). permissions_test.go
-- applies each half through a single held connection, so unlike
-- bootstrap.sql's pool-distributed batches, a later batch in the same
-- half can rely on an earlier one in that half having already committed
-- on the same physical connection.
--
-- The instance-level permission is chosen at run time from the server's
-- own major version, inside the script itself, rather than from two
-- separate scripts or a Go-side conditional: VIEW SERVER PERFORMANCE
-- STATE does not exist as a grantable permission on major 15 (SQL Server
-- 2019), so granting it unconditionally would fail that engine outright.

IF SUSER_ID(N'asq_test_q') IS NOT NULL DROP LOGIN asq_test_q;
IF SUSER_ID(N'asq_test_i') IS NOT NULL DROP LOGIN asq_test_i;
IF SUSER_ID(N'asq_test_s') IS NOT NULL DROP LOGIN asq_test_s;
GO

CREATE LOGIN asq_test_q WITH PASSWORD = N'__Q_PASSWORD__', CHECK_POLICY = OFF;
GO

CREATE LOGIN asq_test_i WITH PASSWORD = N'__I_PASSWORD__', CHECK_POLICY = OFF;
GO

CREATE LOGIN asq_test_s WITH PASSWORD = N'__S_PASSWORD__', CHECK_POLICY = OFF;
GO

DECLARE @major int = CAST(SERVERPROPERTY('ProductMajorVersion') AS int);
DECLARE @stmt nvarchar(max) = CASE
    WHEN @major >= 16 THEN N'GRANT VIEW SERVER PERFORMANCE STATE TO asq_test_s;'
    ELSE N'GRANT VIEW SERVER STATE TO asq_test_s;'
END;
EXEC(@stmt);
GO

-- == APPDB ==

IF USER_ID(N'asq_test_q') IS NOT NULL DROP USER asq_test_q;
IF USER_ID(N'asq_test_i') IS NOT NULL DROP USER asq_test_i;
IF USER_ID(N'asq_test_s') IS NOT NULL DROP USER asq_test_s;
GO

CREATE USER asq_test_q FOR LOGIN asq_test_q;
CREATE USER asq_test_i FOR LOGIN asq_test_i;
CREATE USER asq_test_s FOR LOGIN asq_test_s;
GO

GRANT CONNECT TO asq_test_q, asq_test_i, asq_test_s;
GRANT VIEW DATABASE STATE TO asq_test_q, asq_test_i, asq_test_s;
GO

GRANT VIEW DEFINITION TO asq_test_i, asq_test_s;
GRANT SELECT ON SCHEMA::dbo TO asq_test_i, asq_test_s;
GO

-- Design spec, line 149: for size collection through
-- sys.dm_db_partition_stats, I (and S, which is I plus the instance
-- permission) must additionally hold VIEW DATABASE PERFORMANCE STATE and
-- VIEW SECURITY DEFINITION on 2022, "granted explicitly in the 2022
-- fixture". Both are new in SQL Server 2022 (major 16) and do not exist
-- as grantable permissions on 2019 (major 15) at all - granting either
-- there is a SQL error, not a no-op - so this is gated exactly like the
-- instance permission above, inside the script itself.
DECLARE @major2022 int = CAST(SERVERPROPERTY('ProductMajorVersion') AS int);
IF @major2022 >= 16
BEGIN
    EXEC('GRANT VIEW DATABASE PERFORMANCE STATE TO asq_test_i, asq_test_s;');
    EXEC('GRANT VIEW SECURITY DEFINITION TO asq_test_i, asq_test_s;');
END
GO

-- Restricted carries an explicit DENY for I and S even though neither was
-- ever granted SELECT on it: the point of this fixture (see
-- Restricted.Secret in objects.sql) is that HAS_PERMS_BY_NAME reports
-- Denied identically whether access was never granted or was explicitly
-- denied - it does not expose DENY as a distinct state, and this
-- package's Probe must not invent one either.
DENY SELECT ON SCHEMA::Restricted TO asq_test_i, asq_test_s;
GO

-- dbo.DeniedDefinitionProc (task 13, objects.sql): kept as a
-- documented negative result (fix-0). Measured against a real engine:
-- DENY VIEW DEFINITION on this object removes its sys.objects row
-- entirely for I/S, regardless of the GRANT EXECUTE below - DENY wins
-- over metadata visibility itself, not merely over reading the
-- definition text. This does NOT build obj code's permission_denied
-- fixture; see dbo.ExecuteOnlyProc below for the grant that does.
GRANT EXECUTE ON dbo.DeniedDefinitionProc TO asq_test_i, asq_test_s;
DENY VIEW DEFINITION ON dbo.DeniedDefinitionProc TO asq_test_i, asq_test_s;
GO

-- dbo.ExecuteOnlyProc (fix-0): obj code's REAL confirmed
-- permission_denied fixture (design spec line 204). Q holds no VIEW
-- DEFINITION anywhere (unlike I/S, which get it database-wide above) -
-- GRANT EXECUTE alone, with no DENY at all, is enough for the object
-- to stay visible in sys.objects while sys.sql_modules.definition
-- reads NULL and HAS_PERMS_BY_NAME(...,'VIEW DEFINITION') reads 0 for
-- Q. Measured directly (objects_test.go's TestObjCodePermissionDenied).
GRANT EXECUTE ON dbo.ExecuteOnlyProc TO asq_test_q;
GO
