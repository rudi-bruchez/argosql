-- login.sql — create the `argosql` login and grant what the tool needs.
--
-- Edit the password below, uncomment the tier you want, run the whole script
-- against the database you intend to diagnose, as sysadmin or equivalent.
-- Run it again once per database: the GRANTs below are database-scoped.
--
-- DO NOT COMMIT this file after typing a real password into it.
--
-- argosql never stores the password. It reads it from the environment variable
-- named by `password_env` in the profile:
--
--   profiles:
--     prod:
--       host: <server>
--       database: <database>
--       username: argosql
--       password_env: ASQ_PASSWORD


-- 1. The login and the database user.

CREATE LOGIN argosql WITH PASSWORD = 'CHANGE_ME_BEFORE_RUNNING', CHECK_POLICY = ON;
GO

CREATE USER argosql FOR LOGIN argosql;
GO


-- 2. Tier Q. Query Store only: enough for `info` and every `qs` command.
--    This is the starting point. Move up only when a command reports exit code
--    4, which names the permission it probed.

GRANT CONNECT TO argosql;
GRANT VIEW DATABASE STATE TO argosql;
GO


-- 3. Tier I. Adds object and index inspection: definitions, sizes, statistics,
--    missing indexes. Uncomment to grant.
--
--    VIEW DEFINITION exposes the text of procedures, functions, views and
--    triggers, including every comment and literal they contain. That is the
--    real cost of this tier.

-- GRANT VIEW DEFINITION TO argosql;
-- GO

--    On SQL Server 2022 and later ONLY, tier I also needs these two. They do
--    not exist before 2022 and granting them there raises an error, so leave
--    them commented on 2019.

-- GRANT VIEW DATABASE PERFORMANCE STATE TO argosql;
-- GRANT VIEW SECURITY DEFINITION TO argosql;
-- GO


-- 4. Tier S. Adds instance-wide observation. This is the one grant that reaches
--    beyond the database you named: under it the login can observe activity in
--    databases you did not select. Uncomment ONE line, matching your version.

-- GRANT VIEW SERVER STATE TO argosql;              -- SQL Server 2019
-- GRANT VIEW SERVER PERFORMANCE STATE TO argosql;  -- SQL Server 2022 and later
-- GO


-- 5. What this script deliberately does NOT grant.
--
--    No SELECT on your tables, on any schema, table or column. Checked against
--    the tool's own SQL: every statement argosql issues reads a system catalog
--    view or a dynamic management view, never a row of your data.
--
--    One consequence, so it does not surprise you: argosql probes whether the
--    caller may SELECT an object before reporting on it, and under this script
--    that probe answers denied. The answer is accurate, and it does not stop a
--    command that only needs catalog metadata.
--
--    No db_datareader, db_owner, sysadmin or any other fixed role, and no write
--    permission of any kind. argosql issues no INSERT, UPDATE, DELETE, ALTER,
--    CREATE or DROP, and creates no object on the server.


-- 6. Verification. 1 means allowed, 0 means not granted.
--    Run as is: the columns for tiers you did not grant should read 0.

EXECUTE AS USER = 'argosql';

SELECT
    [CONNECT]             = HAS_PERMS_BY_NAME(NULL, 'DATABASE', 'CONNECT'),
    [VIEW DATABASE STATE] = HAS_PERMS_BY_NAME(NULL, 'DATABASE', 'VIEW DATABASE STATE'),
    [VIEW DEFINITION]     = HAS_PERMS_BY_NAME(NULL, 'DATABASE', 'VIEW DEFINITION');

REVERT;
GO

-- On SQL Server 2022 and later, uncomment to check the two 2022-only grants.
-- Before 2022 these return NULL, which means the permission does not exist on
-- this version, not that it was refused.

-- EXECUTE AS USER = 'argosql';
-- SELECT
--     [VIEW DATABASE PERFORMANCE STATE] = HAS_PERMS_BY_NAME(NULL, 'DATABASE', 'VIEW DATABASE PERFORMANCE STATE'),
--     [VIEW SECURITY DEFINITION]        = HAS_PERMS_BY_NAME(NULL, 'DATABASE', 'VIEW SECURITY DEFINITION');
-- REVERT;
-- GO
