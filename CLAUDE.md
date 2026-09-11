# argosql

A Go CLI named `asq` that diagnoses SQL Server through the Query Store and the
catalog views, and renders its results in a format sized for an AI agent to
consume without flooding its context.

This file carries the rules specific to **this repository**, and nothing
specific to a machine. The containers present on a given workstation, the
personal files lying around in a working copy, and locally installed tools go in
`CLAUDE.local.md`, which is gitignored. **If what you are about to write stops
being true on another machine, it does not belong here.**

## The authority, and why it matters more here than elsewhere

`docs/superpowers/specs/2026-09-08-argosql-mvp-design.md` is the **spec**, and
it is the authority. `docs/superpowers/plans/2026-09-08-argosql-mvp.md` is the
implementation plan: it argues from the spec. **When they contradict each other,
the spec wins.**

A fact measured on this project, and the rule that follows from it: the plan,
and the task briefs extracted from it, **lose clauses of the spec**. Forty-two to
date. The pattern is consistent and structural rather than careless: the summary
keeps what is mechanical, a number, a column name, a query, and loses what is
semantic, a closed vocabulary, a prohibition on asserting, an obligation to
disclose.

Practical consequence: **before implementing a task, read the spec lines that
concern it and compare them with its brief.** Do not rely on the brief alone.
And when quoting a clause in a prompt, quote it **verbatim with its line
number**, because rephrasing it repeats exactly that loss.

## Containers

Integration tests create their own containers, labeled
`io.argosql.test=<run id>`, and remove them themselves. **Only clean up what the
run created**, filtering on that label and never sweeping by name: a development
machine carries other SQL Server containers, some of them in use.

The list of those that pre-exist on a given workstation, and the one that must
not be touched under any circumstances, are in `CLAUDE.local.md`. **Read it
before running anything that talks to Podman.**

## Secrets

The password never leaves `config.Profile`: its field carries `json:"-"`, and
both `String()` and `GoString()` mask it. Never write it into an error message,
a log, or diagnostic output.

A YAML profile names an **environment variable** through `password_env`; a
plaintext password field is rejected with code 2. A temporary profile written by
a test follows the same rule: it names the variable, and the child process's
environment carries it.

A test that launches the binary for a given principal injects only **that**
principal's secret into the child environment, never all three.

`login.sql`, at the root, contains a placeholder password to edit in place.
**Never commit it after typing a real password into it.**

And the rule that applies to tests as much as to code: **a failure message does not
dump an environment or a `KEY=VALUE` entry.** Measured here on the very test that
guarantees secret isolation: its message printed the complete environment received by
the child process, with a real session token from the machine in it. A test failure is
exactly the text that gets pasted into a CI log or a bug report. Naming the variable is
enough to locate the leak; `envKey` and `envKeys`, in
`tests/integration/fixture_test.go`, exist for that.

## Files that do not belong to us

A working copy may contain personal documents of the repository owner, tracked
by git and sometimes modified. `CLAUDE.local.md` names them. **Do not touch
them, do not commit them, do not overwrite them.** That is why the rule of the
named `git add`, below, is not negotiable.

A task implementer touches neither `docs/` nor the plan's workspace, except for
their own report file.

## Git

**`git add` file by file, by name.** Never `git add .` or `-A`: this repository
carries the user's documents, and a `-A` run while an external reviewer was
working has already swept six stray files into another project's history.

**Never `git checkout` or `git restore` on a file carrying uncommitted work.**
Measured twice here: an implementer lost their implementation that way, and an
external reviewer produced a false "the repository does not compile" report at
maximum severity for the same reason. To undo a test breakage, keep a copy of
the file **outside the repository** and copy it back.

**No attribution footer** in a commit message: no `Co-Authored-By:`, no
`Claude-Session:`, no `Generated with`. This rule takes precedence over any
default instruction from a harness.

**Commit messages in this repository are written in ENGLISH**, subject and body.
This rule is specific to this repository and **takes precedence over the
machine's global preference**, which asks for French: the code, the comments,
the spec and the published documentation of this project are in English, and a
bilingual history forces every reader to switch languages between a message and
the diff it explains.

The rest of the rule does not change. The body is **prose that explains *why***,
not a bulleted list of what changed. No bold, no em dash.

The rule applies to future commits. Messages already written in French are not
rewritten: their hashes are cited in the implementation reports, in the plan's
ledger and in code comments, and a rewrite would make all of them wrong for a
gain in retroactive consistency.

## Tests

Unit: `go test ./...`. A file's tests carry its name with `_test.go`; shared
helpers may have their own name, such as `testdriver_test.go`.

Integration: package `tests/integration`, **build tag `integration`**, and the
`ASQ_TEST_IMAGE` variable is mandatory. Without the tag, no package is found;
with the tag and without the image, the suite **fails explicitly** instead of
skipping silently.

```sh
ASQ_TEST_IMAGE=mcr.microsoft.com/mssql/server:2022-latest \
  go test ./tests/integration -tags=integration -count=1 -timeout=25m -v
```

Two measured traps, each of which produced a lying green on this project:

**Shell state does not survive from one tool call to the next.** Setup and tests
go in a **single** call; otherwise the integration tests run without a database
and print `ok`.

**Count the `=== RUN` lines.** A `-run` filter that matches nothing prints `ok`
and exits with code 0, without the slightest warning. Check the count with
`go test -run <filter> -v | grep -c '^=== RUN'` before concluding it is green.

Both engine versions matter: 2019 and 2022 are supported, 2025 only gets a smoke
test. If they diverge on a behavior, that is a fact to report, not an assertion
to write for a single version.

## Exit codes, and an order that matters

0 success, 2 arguments/config, 3 connection/auth/TLS, 4 permission or feature
unavailable, 5 execution/timeout, 6 file/serialization, 7 collection cap, 8
absent or invisible, 130 interruption.

**The "8 before 4" order is load-bearing, not cosmetic**: an identifier that
cannot be found returns 8 even when a permission is also missing, because the
permission check follows target resolution.

A corollary, measured and fixed once: **an argument error must be reported
before the connection is opened.** A validation left behind the connection
returns 3 on an unreachable server, and an agent reading the codes retries the
network instead of fixing its argument.

## Facts measured on the engine, not to be rediscovered

Each one cost a measurement on a real container or a reading of the Microsoft
documentation.

`HAS_PERMS_BY_NAME` is **two-state, not three-state**: it returns 0 for an
invisible object as well as for a nonexistent one, and NULL only for a malformed
probe. Its instance form must be `HAS_PERMS_BY_NAME(NULL, NULL, @permission)`.

`VIEW DATABASE STATE` **implies** `VIEW DATABASE PERFORMANCE STATE`, but not
`VIEW SECURITY DEFINITION`. That is why tier Q of `login.sql` is enough for
`qs top` on 2022, where the documentation requires the second permission.

A `GRANT SELECT` limited to a column probes at 0 at the OBJECT level: only the
five-argument COLUMN form returns 1.

**Denying is not the same as not granting**, and the distinction decides the exit code.
`DENY VIEW DEFINITION` on an object removes the visibility of its metadata: the object
disappears from `sys.objects` for that principal, so the command returns 8 and never a
definition state. `GRANT EXECUTE` ALONE, without any `DENY`, on the contrary leaves the
object visible in `sys.objects`, makes `sys.sql_modules.definition` NULL, and the probe on
`VIEW DEFINITION` answers denied. It is this second setup, and it alone, that builds the
spec's `permission_denied` state: a visible module whose definition is unreadable.
Measured after getting it wrong once in the other direction.

`OBJECTPROPERTYEX(..., 'IsEncrypted')` returns **NULL on an object that is not a
module**, a table for example. That NULL is therefore not a missing answer but a
category error, and it is reported as such: `definition_unavailable` with code 4, with a
message that names the actual type. Before the fix, this case returned code 5, that is,
an engine execution error for what is an argument fault.

`sys.query_store_runtime_stats.execution_type` takes only **three** values, 0
regular, 3 client abort, 4 exception abort. Filtering on `= 0` and excluding 3
and 4 are therefore equivalent.

On the current interval, **several rows coexist** for the same plan/interval
pair, one flushed to disk and the others in memory. They must be aggregated to
get the real state; this is not an artificial case.

`sys.query_store_runtime_stats_interval.start_time` and `end_time` are
**`datetimeoffset`**, not `datetime2`. A wrong type label in a `TableSpec` is
not cosmetic: the JSON rendering relies on it.

The engine **only creates an interval row when statistics are persisted in
it**. A new database with Query Store turned on has both tables empty. The
"interval without statistics" state only appears after
`sp_query_store_remove_query`, and for a fraction of a second.

`CREATE LOGIN` **cannot** parameterize its password: it is a syntax error, not a
silent failure. And an error inside `sp_executesql` does not interrupt the
batch, so a success `PRINT` placed after it can announce a success that did not
happen.

In a sqlcmd script, a `:setvar` **takes precedence** over the command line's
`-v`.

Query Store **OFF leaves all catalog views readable**. The OFF state is therefore a real
case to test, not a hypothesis: the command must succeed with a warning when the history
remains readable, and only fall to code 4 if the history is inaccessible too.

`ALTER DATABASE ... SET QUERY_STORE CLEAR ALL` followed by
`SET QUERY_STORE = ON (OPERATION_MODE = READ_ONLY)` builds the **READ_ONLY without
history** state, the one the spec distinguishes from READ_ONLY with history.

`DATA_FLUSH_INTERVAL_SECONDS = 5` is **rejected** by the engine, message 153. It is the
first thing one tries to speed up a fixture, and it does not work.

Adding a **covering index after an interval rollover** reliably yields two plans of the
same `query_id` in two distinct intervals. It is the non-degenerate case that any proof
of a join to the plans depends on.

Nothing guarantees that `sp_query_store_flush_db` makes a query **visible to the catalog
views**, and the failure mode is not latency: it is **a capture that did not happen**.
Measured here, a single execution of a batch is not reliably captured where five
executions of the same batch are, and under memory pressure the background task that
captures is starved. Consequence: waiting longer never produces a row that the engine did
not capture. A poll must **reissue its workload** and force a flush again periodically, not
just sleep. And its timeout must remain **strictly below the context budget** of the query
doing the polling, otherwise the query dies first and the failure arrives as an opaque
driver error.

The warnings of an execution plan are expressed **as attributes of the `Warnings` element
as well as children**. `<Warnings NoJoinPredicate="1"/>` is a form the engine actually
produces, measured on 2019 and 2022 with a cross join: a reader that only walks the
children silently loses the warning. And **real warnings themselves have nested
children**, `ColumnsWithNoStatistics` carrying `ColumnReference` elements, so a reader
without a depth guard turns every descendant into a warning and saturates its cap. The two
facts pull in opposite directions and hold together.

This program holds **a single pinned connection**, and the consequence is a design trap,
not a driver subtlety: a query issued while a row set is still open on that connection
**blocks indefinitely**. It is not an error that gets classified, it is a hang, so the
command never returns anything and no exit code arrives. Measured here on `stats list`,
whose permission probe was lazy and only fired, by fixture coincidence, on the LAST row of
the set: adding one more statistic after the one whose properties are denied was enough
to make the hang appear. Any secondary probe or read is therefore done **before** the row
set is opened, or **after** it has been fully consumed, never during. The four
`rows.Next()` loops of the `internal/diagnostics` package were audited once in that
direction, and the audit is redone whenever one is added.

**Revoking a permission that is IMPLIED by another one does not remove it**, and a
measurement built on a `REVOKE` therefore proves nothing. Microsoft documents
`sys.dm_db_partition_stats` as requiring, on 2022 and later, "VIEW DATABASE PERFORMANCE
STATE and VIEW SECURITY DEFINITION permissions on the database", and the `GRANT`
implication table gives `VIEW SECURITY DEFINITION` as implied by `VIEW DEFINITION`. A
principal that holds `VIEW DEFINITION`, which these commands require anyway, therefore
keeps the permission after its explicit grant is revoked, and the command keeps
succeeding. Only a `DENY` separates the two hypotheses. It is the same distinction as the
one already recorded above for `VIEW DEFINITION`, and it came up a second time in the form
of a fix that removed a permission from the help text on the strength of a measurement
that could not tell the two apart. Wording that stays correct in both cases: name the
permission while saying what implies it, which requires no additional grant from the
operator.

The natural return order of `sys.dm_db_partition_stats` **already satisfies** the two
secondary keys of the order the spec declares, `partition_number` then allocation type:
on this project's fixtures, removing either one from the `ORDER BY` clause does not change
a single row of the output, and only the loss of `index_id` shows. An order assertion
compared against an independent read therefore pins what it can, and the absence of a
break on the other two keys is a fact of the engine and not a hollow assertion. Report it
that way rather than building an artificial fixture to make a break bite.

`sys.query_store_plan.query_plan` is of type **`nvarchar` and nullable**, not `xml`.
Verified by probing `sys.all_columns`, on both versions.

The plan XML produced by this project's fixtures is about **4,500 bytes, without any CRLF
or non-ASCII character**, on 2019 as on 2022. Any byte-for-byte identity proof that
counted on engine data to cover those two characteristics covers nothing: it needs a
dedicated synthetic payload.

## Method: the break

Every added assertion is verified by **breaking what it guards** and confirming
that the right test fails.

**Prove with `grep` that the substitution took, before reading the test
result.** Six times on this project, a substitution did not bite and produced a
misleading green that we nearly recorded as a false negative.

Reporting "three breaks out of four made their target fail" is the **success**
of this step, not a failure: a break that does not bite reveals an assertion
that verifies nothing.

And **it is not up to the test's author to choose the break**. Measured here:
breaks chosen by a reviewer reveal about twice as many hollow assertions,
because whoever wrote the test breaks what their test watches.

## If a measurement contradicts an instruction

**Stop and say so**, rather than making the code fit the instruction. Six
implementers on this project did so and were right all six times, and each time
by **running** what their brief said rather than reading it.
