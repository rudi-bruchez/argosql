# Permissions

`asq` never requires a privileged login. Every command probes the exact permission it
needs and reports a refusal as a refusal, never as an absence. The three fixture tiers
below are cumulative starting points, not claims of a universally minimal grant: they are
what this project's own integration suite provisions and tests against
(`tests/integration/sql/principals.sql`, `tests/integration/matrix_test.go`).

`login.sql`, at the root of this repository, is the versioned provisioning script: a
single file with the login, the user, and each tier commented out except the first, meant
to be run once per database as sysadmin or equivalent, then edited up a tier only when a
command actually reports exit code 4 and names the permission it needs.

## The three tiers

Q, Query Store only:

```sql
GRANT CONNECT TO argosql;
GRANT VIEW DATABASE STATE TO argosql;
```

This is enough for `info` and every `qs` command. It grants nothing on any table, view,
procedure, function or trigger: a principal with only this tier cannot read a single row
of business data, and cannot see the text of a single object.

I, inspection, adds object and index metadata:

```sql
GRANT VIEW DEFINITION TO argosql;
```

`VIEW DEFINITION` is the real cost of this tier: it exposes the text of every procedure,
function, view and trigger the principal can otherwise see, including any comment or
literal constant they contain. On SQL Server 2022 and later, two further grants are
needed for table size collection specifically, and only there:

```sql
GRANT VIEW DATABASE PERFORMANCE STATE TO argosql;
GRANT VIEW SECURITY DEFINITION TO argosql;
```

Both of these already come implied on 2022 and later by the two grants named above
(`VIEW DATABASE STATE` implies `VIEW DATABASE PERFORMANCE STATE`; `VIEW DEFINITION`
implies `VIEW SECURITY DEFINITION`), so naming them again in this script is for
discoverability, not because they add a capability the principal would otherwise lack.
They do not exist before 2022: granting them there raises an error.

S, instance diagnostics, adds the one grant that reaches past the selected database:

```sql
GRANT VIEW SERVER STATE TO argosql;              -- SQL Server 2019
GRANT VIEW SERVER PERFORMANCE STATE TO argosql;  -- SQL Server 2022 and later
```

Under this tier the login can observe activity in databases it was never given access
to select. `idx usage` and `idx missing` are the only two commands that need it; every
other command works fully under I.

None of the three tiers grants `SELECT` on application tables beyond what a command's
own metadata probe needs, `db_datareader`, `db_owner`, `sysadmin`, or any write
permission. `asq` issues no `INSERT`, `UPDATE`, `DELETE`, `ALTER`, `CREATE` or `DROP`
against the database it diagnoses.

## What each command needs, and what it does under a narrower tier

| Command | Q | I | S |
| --- | --- | --- | --- |
| `help --json` | 0, offline | 0, offline | 0, offline |
| `info` | 0 | 0 | 0 |
| `qs status` | 0 | 0 | 0 |
| `qs top` | 0 | 0 | 0 |
| `qs query` | 0, parent module name may be unavailable | 0 | 0 |
| `plan` | 0 | 0 | 0 |
| `obj table` | 8, not_found_or_not_visible | 0 | 0 |
| `obj code` | 8, not_found_or_not_visible | 0 | 0 |
| `size table` | 8, not_found_or_not_visible | 0 | 0 |
| `idx list` | 8, not_found_or_not_visible | 0 | 0 |
| `idx usage` | 8 for an unresolved fixture table | 4, instance permission absent | 0 |
| `idx missing` (no `--table`) | 4 | 4 | 0 |
| `stats list` | 8, not_found_or_not_visible | 0 | 0 |

This is the design spec's own matrix, reproduced here because it is what the
implementation is held to. It assumes existing fixture objects and a healthy, reachable
Query Store history; `tests/integration/matrix_test.go` (`TestPrincipalMatrix`) runs this
exact matrix against a real server, for all thirteen commands and all three principals,
as part of this project's integration suite. See [docs/testing.md](docs/testing.md) for
how to run that suite and what its last run actually covered.

A few things the table does not say on its own, because they hold for every row in it:

Object resolution always precedes the object-specific permission check. An object this
principal cannot see at all reports 8, not 4, even where a permission is also missing:
a principal that cannot prove the object exists has no basis to name a denied permission
either.

A `DENY` is not the same event as simply not granting something. `DENY VIEW DEFINITION`
on an object removes that object from `sys.objects` for the denied principal, so the
command reports 8, the same as a name that never existed. A bare `GRANT EXECUTE`, with
no `DENY` at all, leaves the object visible in `sys.objects` while
`sys.sql_modules.definition` reads NULL for it: that is the one path to `obj code`'s
`permission_denied` state, a visible object whose definition is confirmed unreadable,
and it is a different server state than an object that was never granted `VIEW
DEFINITION` at all.

Revoking a grant that is implied by another one does not remove the capability. Microsoft
documents `sys.dm_db_partition_stats` as requiring, on 2022 and later, `VIEW DATABASE
PERFORMANCE STATE` and `VIEW SECURITY DEFINITION`; both are already implied by grants
this project's I tier holds for other reasons, so revoking either one alone, without
touching what implies it, proves nothing about whether the capability was actually
removed.
