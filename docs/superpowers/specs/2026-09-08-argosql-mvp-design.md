# argosql MVP design

Status: revised after external design review. No implementation is authorized by this document.

Source: [TASKS01.md](../../TASKS01.md). Prepared using the SuperPowers brainstorming workflow. The source is an exploratory conversation, not a validated technical specification; the decisions below resolve its competing suggestions.

Revision of 8 September 2026: corrected against the findings in [the review report](2026-09-08-argosql-mvp-design-review.md), which were established by running the document's own artifacts on SQL Server 2019 RTM-CU32-GDR and 2022 RTM-CU26 with `github.com/microsoft/go-mssqldb v1.11.0` under Go 1.27. Claims that survived that check are marked as measured where the number matters. Two of the five planned reviewers did not run; the closing section names which parts of this revision rest on a single reader.

## Outcome and scope

Deliver a single Go binary, `asq`, that lets a DBA or AI coding agent identify an expensive SQL Server query, inspect its recorded plans, and investigate the referenced objects without flooding the agent's context.

The first successful workflow is:

```sh
asq --ctx client --db AppDB qs status
asq --ctx client --db AppDB qs top --by cpu --hours 24 --top 10
asq --ctx client --db AppDB qs query 4821 --hours 24
asq --ctx client --db AppDB plan 4821 --plan-id 9033 --summary
asq --ctx client --db AppDB obj table dbo.Orders
asq --ctx client --db AppDB idx usage dbo.Orders
asq --ctx client --db AppDB stats list dbo.Orders
```

Proposed release baseline: SQL Server 2019 and 2022; Linux amd64 and Windows amd64 clients; SQL authentication from an environment variable. Other versions, integrated authentication, Kerberos, and Entra require subsequent compatibility work. These are scope choices for review, not claims about driver limitations.

The workflow above runs from a query to the objects it touches: the plan summary lists referenced objects and indexes. The reverse path does not exist in v0.1, and cannot be built from Query Store metadata alone. See the `--object` note under Commands.

## Approaches considered

| Approach | Benefit | Cost |
| --- | --- | --- |
| Generic SQL executor first | Small initial implementation; immediately reuses scripts | Agent must supply correct diagnostic SQL; weak product differentiation |
| Query Store workflow plus object inspection (recommended) | Useful diagnosis from named commands; reusable output and execution core | Requires explicit aggregation and metadata contracts |
| Entire catalogue from TASKS01 | Broad DBA coverage | Regression detection, plan comparison, scripting, and version support each expand validation substantially |

The MVP includes curated diagnostics only. Arbitrary SQL is explicitly deferred despite its prominence in the early discussion. This keeps the first release's public command surface consistent with its diagnostic purpose. This choice needs user review.

## Commands

All database commands require `--ctx` and an explicit database, supplied by `--db` or the named profile. Never silently connect to `master`. Command help includes examples, output units, required permissions, and supported versions.

| Command | Required behavior |
| --- | --- |
| `help --json` | Versioned inventory generated from the command registry, with parameters, defaults, and examples; works offline |
| `info` | Resolved server/database identity, product version, edition, compatibility level, and current principal; no connection string or secrets |
| `qs status` | Desired/actual state, raw and decoded read-only reasons, capture mode, storage usage/limit, retention and interval settings, earliest/latest stored interval |
| `qs top` | Rank queries by total CPU, duration, logical reads, or executions; `--aggregate total\|avg` defaults to total; averages apply to the first three metrics |
| `qs query <query_id>` | Identity, parent object when visible, SQL preview and full-text artifact, per-plan execution counts and averages for the selected window |
| `plan <query_id> --plan-id <id> [--summary]` | Verify plan belongs to query; export complete XML to `.sqlplan`; optional bounded summary described below |
| `obj table <schema.name>` | Ordered columns, types/length/precision/scale, nullability, identity/computed/default metadata; index structure and approximate row count |
| `obj code <schema.name>` | Export visible module definition to `.sql`; return identity and line count; report three distinct states, encrypted, not visible to this principal, and absent, never one NULL for all three |
| `size table <schema.name>` | Approximate row count and allocated/used/reserved space, with index and allocation-type breakdowns that avoid double counting |
| `idx list <schema.name>` | Name/type, ordered keys with direction, included columns, filter, uniqueness, disabled state; shares implementation with table inspection |
| `idx usage <schema.name>` | Seeks/scans/lookups/updates, last-use timestamps, server start time if permitted, and explicit observation-window limitations |
| `idx missing [--table <schema.name>] [--top N]` | Default top 10 suggestions, raw DMV evidence, transparent impact score, and advisory limitations; no CREATE script |
| `stats list <schema.name>` | Ordered columns, update time, row/sample counts, sample percentage, modification counter, auto/user-created flags, and filter; per-row completeness, since the properties are readable only where the principal holds `SELECT` |

`qs top` supports `--object <schema.name>` and `--min-executions N` (default 1). Ranking ties use ascending query ID. `--top` must be 1 to 100. `qs query` lists plans with no executions in the window explicitly, with zero executions and null averages.

`--object` filters on `sys.query_store_query.object_id`, which carries the parent module and nothing else. Measured: it is 0 for every ad-hoc statement, including the auto-parameterized form the engine substitutes, so only a procedure, function or trigger name matches. A table name matches nothing, ever. Help text and the command's own header state this in those terms; the flag must not read as "queries touching this object". Reaching queries from a table requires scanning plan XML for referenced objects, which is deferred.

`qs top` and `qs query` exclude `sys.query_store_query.is_internal_query = 1` by default and say so in the header. Measured: auto-statistics work (StatMan) enters Query Store after a few dozen executions of an ordinary workload, and would otherwise rank against user queries. `--include-internal` restores them. The column exists on both baseline versions.

Do not include a `stale` verdict in v0.1: show the evidence without implying that a universal percentage is SQL Server's update threshold. Missing-index impact is `(user_seeks + user_scans) * avg_total_user_cost * avg_user_impact / 100`; label it a ranking score, not predicted elapsed-time savings. Do not fabricate percentage coverage by existing indexes.

`sys.dm_db_index_usage_stats` and the missing-index DMVs are instance-wide, and `object_id` is unique only inside a database. Every query over them filters on `database_id = DB_ID()`. Measured: a single test instance already returned rows for two databases. `sys.dm_db_missing_index_group_stats`, which carries the four columns of the score, has no `database_id` at all, so the filter travels `_group_stats` to `sys.dm_db_missing_index_groups` to `sys.dm_db_missing_index_details.database_id`. Omitting it reports another database's counters under this database's object names.

`stats list` reports per-statistic completeness. Measured: `sys.dm_db_stats_properties` is gated per statistic on `SELECT` over that statistic's columns, and returns no row and no error when the permission is absent, so the command can list five statistics and read the properties of one. Rows whose properties could not be read carry nulls plus a warning naming the cause, and the completeness metadata records the count. Never present a partially readable statistics list as complete.

## Query Store semantics

`qs top` and `qs query` accept either `--hours N` (positive, default 24) or `--since RFC3339 --until RFC3339`. Reject mixed modes or reversed windows. Resolve relative time once in UTC for the entire command. Include intersecting runtime intervals and disclose the actual interval coverage; boundary intervals are not prorated or claimed to be exact per-execution filtering.

Filter to successful executions first (`execution_type = 0`) and label that filter, then aggregate the surviving rows by plan and interval, then combine plans. Stating the grain as (plan, execution type, interval) was wrong: once the filter is applied, execution type is constant and grouping on it does nothing. An interval can hold several rows for the same plan; measured twice independently, 3076 and 699 executions on one triplet in one run, two rows for fifteen executions in another. Sum those rows, do not collapse them and do not pick one.

Totals for CPU, duration and logical reads are `sum(avg_metric * count_executions)`, and averages are that total divided by summed executions. The `executions` metric has no average column: its total is `sum(count_executions)` and it has no average form, which is why `--aggregate avg` is rejected for it. Convert CPU/duration microseconds to milliseconds; logical reads remain 8-KB page counts. Measured: a statement of 31,493 ms wall clock is recorded as `avg_duration = 31491772`, so the units are as documented. Never average averages without weighting. Microsoft documents active-interval multiplicity and these units in [sys.query_store_runtime_stats](https://learn.microsoft.com/en-us/sql/relational-databases/system-catalog-views/sys-query-store-runtime-stats-transact-sql?view=sql-server-ver16).

`sys.query_store_runtime_stats` carries six more columns on 2022 than on 2019, and `sys.query_store_plan` four more. Embedded SQL names its columns explicitly and never selects a 2022-only column on the 2019 path. One of them is `replica_group_id`. Add it to the aggregation grain so that an instance collecting secondary-replica statistics does not sum replicas into one ranking, but do not join `sys.query_store_replicas` to name the replica: measured on a standalone 2022 instance, `replica_group_id` is 1 for every row while that catalog view is empty, so the join would drop the entire result set on the project's own reference instance. Render the raw group id, and resolve names only when the catalog returns something.

Every Query Store command reads health first. Emit structured warnings for non-READ_WRITE state, capture restrictions, and requested history outside available coverage. READ_ONLY history remains inspectable with a warning. A genuinely empty selection succeeds with zero rows. Permission errors must not be converted to empty results. Query Store health and retention affect interpretation; see Microsoft's [Query Store practices](https://learn.microsoft.com/en-us/sql/relational-databases/performance/best-practice-with-the-query-store?view=sql-server-ver16).

OFF does not mean absent. Measured: with `actual_state_desc = OFF`, the catalog views stay readable, returned 145 runtime rows, raised no error, and the history survived the OFF and back to READ_WRITE round trip intact. So there are two OFF cases and the earlier wording covered only one. OFF with no usable history yields an unavailable result, not an empty ranking. OFF with history succeeds, ranks the frozen history, and carries a warning naming the state and the latest stored interval, because that timestamp is the only honest bound on what the ranking describes. Never present a frozen ranking as current.

`readonly_reason` is 0 whenever the state was set deliberately rather than reached through a limit. Measured on both baseline versions with `OPERATION_MODE = READ_ONLY`. Render zero as the absence of a reason, not as an unknown code and not as a blank cell: the decoded column reads that the store is read-only by configuration. Decode the non-zero value bit by bit; the catalog view offers no decoded column, so the mapping is the CLI's own and is versioned with the help output.

Plan summaries identify the source as Query Store compiled plan XML. Include the statement's estimated cost, up to five operators ranked by estimated subtree cost, referenced objects/indexes, and warning elements actually present. Costs are optimizer estimates; do not sum overlapping subtree costs or infer actual row counts, actual spills, or observed runtime from absent attributes. Unsupported XML elements do not prevent raw export; malformed XML yields a summary error while retaining a successfully exported artifact.

Do not report a statement count. `sys.query_store_plan` stores one plan per statement, not per batch: measured, a three-statement procedure produced three plans, each holding exactly one `StmtSimple`. A statement count would be the constant 1 for every artifact this command can produce. The earlier wording was specified against `sys.dm_exec_query_plan`, which does hold batch plans, and that mistake also sized `internal/plan`: a single-statement document does not need streaming, and the bounded-state requirement is about the operator list, not about the parse. Keep the reader streaming anyway if it costs nothing, but the acceptance criterion is the bounded operator list, not memory during parse.

Query Store plan XML carries no XML declaration. Measured: the text begins at `<ShowPlanXML xmlns=...`. Write the artifact as UTF-8 without BOM and do not synthesize a declaration, which is what the source asked for and what keeps the file out of the UTF-16LE trap that SSMS exports fall into. The general artifact rule below, that a declaration must match the encoding, applies only where a declaration already exists. Open one exported plan in SSMS during slice 2 and record the result: no reviewer settled whether SSMS accepts a declaration-less `.sqlplan`, and the answer decides whether a declaration has to be added after all.

## Output and artifacts

Default output is escaped TSV without alignment padding. `--format json` produces a single versioned JSON envelope containing context, named result sets, typed column descriptors, row arrays, warnings, artifact paths, and completeness metadata. Multiple sections use named tables in both formats. JSONL is deferred.

Default stdout preview: 10 rows per table, 200 Unicode code points per text cell, and 32 KiB total serialized output, with `--preview N`, `--truncate N` and `--no-truncate` overriding the first two. Restoring those three flags is deliberate: the source states configurable truncation as a requirement, and freezing the numbers as constants dropped it silently.

Those three limits can conflict. A command with several named sections, and `obj table` is one, reaches 32 KiB before its last section is serialized. Resolve it by allocation, not by arrival order: reserve the completeness metadata and artifact paths first, divide the remaining budget equally among the sections that have rows, and let a section that does not use its share return the remainder to the others in declared order. A section that loses rows to the budget says so in its own truncation metadata. What must never happen is a section rendering empty because an earlier section spent the budget, since an agent reads that as "no indexes on this table". Section order is the order declared in the command's result contract and does not vary between invocations. JSON must remain valid when bounded. Full tabular output is written to a per-invocation artifact; stdout is the preview. Long SQL text, module bodies, and plans are separate files. Record truncation in machine-readable metadata; the TSV marker `…[+N]` states omitted code points.

TSV encodes null as `\N`, empty text as an empty cell, and escapes backslash, tab, CR, and LF. Escape column names too. Full exports preserve strings and exact decimal values without display rounding; JSON represents bigint and decimal values as strings with SQL type metadata to avoid consumer precision loss. Preview-only float rounding is allowed and documented. Duplicate column labels remain positional.

Artifacts are UTF-8 without BOM. Where a declaration already exists in the source text it must match the encoding; never add one that was not there, and see the plan section for why that case does not arise for Query Store plans. Use a unique invocation directory under the OS cache directory, or `--out-dir`. Directory/file permissions are owner-only where supported; never derive filesystem paths directly from server-supplied names. Use exclusive creation and atomic completion; never overwrite existing files. A manifest records source identity, UTC collection time, schema version, counts, and completeness. No credentials in metadata.

Bound each invocation to 2,000 collected rows across tables and 100 MiB of artifact data. Probe for additional rows before declaring a result complete at the row limit. The previous figure of 10,000 sat far above anything v0.1 can collect, which made exit code 7 dead and the guard decorative: `qs top` is capped at 100, `idx missing` defaults to 10, and the widest legitimate single result is `obj table` on a 1,024-column table. 2,000 clears that case and still catches a collection that has run away, which is what the guard is for. Exit code 7 stays rare by design, so acceptance check 5 constructs a fixture that exceeds the cap rather than waiting for a natural one. Oversized XML or module definitions fail explicitly instead of producing apparently valid truncated source files. Incomplete tabular results remain labeled partial. Artifact write failure is an error; do not advertise a nonexistent or complete file. Cache cleanup is manual in v0.1 and documented.

## Connection and execution

Use a named YAML profile selected by `--ctx`; `--config` can override its default OS user-config location. Profiles contain host, port, optional database, username, and the name of a password environment variable. No password argument or plaintext password property. Do not load `.env` implicitly.

TLS encryption and certificate validation are enabled by the CLI, explicitly, on every connection. This is not inherited: measured with go-mssqldb v1.11.0, a DSN carrying no `encrypt` parameter connects successfully and unencrypted, and `sys.dm_exec_connections.encrypt_option` reads false. The failure mode of forgetting is therefore a working plaintext connection to a client's production server, so the encryption parameter is set in code and is not a profile property that can be omitted. A profile may explicitly opt into trusting a server certificate, with a visible warning. That opt-in is not only a development convenience: a stock SQL Server container presents a self-signed certificate with no IP SAN, and the validated default fails against it with `x509: cannot validate certificate for 127.0.0.1 because it doesn't contain any IP SANs`. Every integration run therefore exercises the trusting path, and the release documentation states which of the two paths the tested matrix covers.

Use Go `database/sql` with `github.com/microsoft/go-mssqldb`, pinned when implementation starts. Acquire one `*sql.Conn` at the start of the invocation, run every statement on it, and close it at the end. This is stronger than "one connection per invocation" and the difference is load bearing: `database/sql` is a pool and the driver implements `driver.SessionResetter`, so a `SET` issued through `db.Exec` is discarded before the next statement. Measured, on one SPID: `SET LOCK_TIMEOUT 5000` through `db.Exec` leaves `@@LOCK_TIMEOUT` at -1, the infinite wait. Limiting the pool to one connection does not help either, which is the trap, since it is the obvious reading of the earlier wording: with `SetMaxOpenConns(1)` the value is still -1 on the same SPID, because the reset happens on every release to the pool and not on every new physical connection. Only the held `*sql.Conn` keeps it, and it loses it the moment it is closed and reacquired.

Session setup runs once on that connection and its effect is asserted, not assumed: read `@@LOCK_TIMEOUT` back before the first diagnostic query and fail the invocation if it is not the configured value. Proposed defaults: connection deadline 5 seconds, overall database-work deadline 30 seconds, lock timeout 5 seconds. `--timeout` accepts 1 to 300 seconds and covers all diagnostic queries in that invocation. Cancellation closes result streams and the connection. There are no automatic query retries.

Embed curated SQL with `go:embed`. Bind values as parameters; map sortable metrics to static SQL fragments. Resolve schema/object names through parameters rather than concatenating executable identifiers. Keep session setup and queries on the same connection. Preserve default isolation; neither READ UNCOMMITTED nor transaction rollback is a security mechanism.

## Permissions and confidentiality

The CLI offers no DML, DDL, maintenance, stored-procedure execution, or arbitrary-query command in this release. A DBA provisions a dedicated principal for it. The earlier claim that this principal is granted only what the diagnostics need, scoped to one database, was measured and is false: the thirteen commands fall into three permission regimes, and two of them reach outside the database.

| Regime | Commands | Grant | Scope |
| --- | --- | --- | --- |
| Query Store | `qs status`, `qs top`, `qs query`, `plan` | `VIEW DATABASE STATE` | One database |
| Server DMV | `idx usage`, `idx missing`, and server start time wherever it is shown | 2019: `VIEW SERVER STATE`. 2022: `VIEW SERVER PERFORMANCE STATE` | Whole instance |
| Object metadata | `obj table`, `obj code`, `size table`, `idx list`, `stats list` | `VIEW DEFINITION` on the objects or schema, plus `SELECT` on a table's columns for that table's statistics properties | One database, per object |

Three consequences the document must carry rather than discover during slice 3.

The server DMV regime is not database scoped and cannot be made so. Measured on both versions, a principal holding only `VIEW DATABASE STATE` is refused with `VIEW SERVER STATE permission was denied` on 2019 and `VIEW SERVER PERFORMANCE STATE permission was denied` on 2022, and `GRANT VIEW SERVER PERFORMANCE STATE` is a syntax error on 2019, so the setup examples differ in scope and not only in wording. Once granted on 2022, the same principal reads `sys.dm_exec_query_stats` for every database on the instance. A DBA is therefore accepting an instance-wide read to obtain two commands out of thirteen. Say so in the setup guidance, and make `idx usage` and `idx missing` degrade to a named, explicit unavailability when the grant is absent rather than appearing broken.

The object metadata regime is in direct tension with the security posture the source recommends. `docs/TASKS01.md` proposes `DENY VIEW ANY DEFINITION` and `DENY SELECT ON SCHEMA::dbo`. Measured under that posture, `OBJECT_ID('dbo.Orders')` returns NULL, so five commands return zero rows and raise no error at all. That is a real trade-off between inspecting objects and withholding business data, and the operator makes it, not the tool. Document both principals: a Query Store only principal that satisfies the source's confidentiality posture, and an inspection principal that additionally holds `VIEW DEFINITION`, with the note that `stats list` needs `SELECT` on top and that granting it also makes `sys.dm_db_stats_histogram` readable, which the MVP defers but no longer withholds.

The matrix above is the starting point, not the finished thing: integration tests establish the rest, and `help` must not ship a plausible permission list ahead of them. Slice 1 ships `help` and the permission work lands in slice 3, so the risk is a confident wrong list written to avoid blocking. Until a command's permission has been measured, its help output says the permission is not yet established rather than guessing.

Do not call a principal safe solely because a few DENY statements exist. Ownership, elevated roles, and permission interactions require review. Microsoft explicitly documents exceptions to DENY precedence in [DENY](https://learn.microsoft.com/en-us/sql/t-sql/statements/deny-transact-sql?view=sql-server-ver16). The CLI cannot make a broadly privileged credential read-only.

Diagnostic access is potentially confidential: query text, compiled parameters, module definitions, and object names may expose sensitive information. Environment variables avoid passwords in arguments but do not hide credentials from an agent with equivalent OS access. A separate credential broker is outside the MVP. Never print raw DSNs or raw connection errors; redact known credential values in application diagnostics. Exported SQL content is intentionally preserved and must be treated as sensitive by the operator.

Index usage counters have reset and visibility limitations. Server uptime is context, not a proven continuous observation start for every index. Missing usage rows are unknown/unobserved, not proof of no reads. Do not recommend dropping an index in v0.1.

## Internal boundaries

| Component | Responsibility |
| --- | --- |
| `cmd/asq`, `internal/cli` | Command registry, argument validation, help, exit codes |
| `internal/config` | Profile parsing, environment resolution, safe diagnostics |
| `internal/sqlserver` | Held `*sql.Conn` lifetime, asserted session settings, deadlines, parameters, version capabilities, and the permission probe |
| `internal/diagnostics` | Embedded SQL and typed command results; no terminal formatting |
| `internal/plan` | Single-statement plan XML summary with a bounded operator list |
| `internal/output` | TSV/JSON serialization, previews, truncation and type rules |
| `internal/artifacts` | Exclusive paths, quotas, full exports and manifests |

Flow: validate input, resolve profile, connect and hold the connection, assert session settings, probe the permissions this command needs, check capabilities and health, execute curated queries, save artifacts, render bounded result. CLI and a future MCP adapter can share typed results, but no MCP abstractions or server are built now.

The permission probe is a named step because SQL Server will not give the answer any other way. Metadata visibility rules do not raise an error when a principal lacks rights on an object: they return nothing. Before running an object command, resolve the name and, if it does not resolve, ask whether the principal holds the permission that would make it visible, so the command can distinguish absent from invisible instead of reporting either as an empty result.

## Errors

Exit codes: 0 successful (including empty results and health warnings); 2 invalid arguments/config; 3 connection/authentication; 4 permission or feature unavailable; 5 execution failure/timeout; 6 artifact or serialization failure; 7 collection limit reached with partial results; 8 requested entity not found; 130 user interruption.

The exit code is the only thing an agent observes at the process boundary, so every outcome named elsewhere in this document maps to exactly one of them. Code 8 is new: the earlier table pushed a missing identifier onto 2, which is for arguments that are malformed rather than merely unmatched.

| Outcome | Code |
| --- | --- |
| Ranking or listing returned, possibly with warnings | 0 |
| Query Store READ_ONLY, or OFF with history: frozen result plus warning | 0 |
| Genuinely empty selection in a healthy window | 0 |
| Query Store OFF or ERROR with no usable history | 4 |
| Command needs a permission the principal lacks | 4 |
| Command needs a version feature the server does not have | 4 |
| `query_id` or `plan_id` not present in this database | 8 |
| `--plan-id` does not belong to the given `query_id` | 8 |
| Object name does not resolve, or is not visible to this principal | 8 |
| Collection cap reached, partial result written | 7 |

Diagnostics go to stderr with a stable code and safe message; retain SQL Server error numbers when available. JSON errors use the same envelope with `ok: false`, an error object, and any explicitly partial artifacts. Never stream a JSON prefix that cannot be completed after an error. Missing required permissions fail the command; optional fields use null plus a warning.

Object visibility needs its own rule because the engine gives no signal. Measured: a principal without rights on an object gets NULL from `OBJECT_ID` and zero rows downstream, with no error, so "permission errors must not be converted to empty results" cannot be satisfied by catching an error that never arrives. Code 8 covers both absent and invisible, and the message says which one the permission probe established, or that the two could not be separated. Never claim nonexistence on the strength of an empty result.

## Validation and release acceptance

### Local Podman environment

Podman inventory verified on 2026-09-08: local SQL Server images are available for `2019-latest`, `2022-latest`, and `2025-latest` from `mcr.microsoft.com/mssql/server`. Existing containers include:

| Container | Image version | Observed state | Host port |
| --- | --- | --- | --- |
| `sqltop-test-2019` | 2019 | Stopped | 11439 |
| `sql2022` | 2022 | Stopped | 11433 |
| `sqltop-test` | 2022 | Stopped | 11433 |
| `sqlgopace-mssql` | 2022 | Stopped | 1433 |
| `sql2025` | 2025 | Running | 11533 |

Use the existing local images to create disposable, project-specific test containers with unique names and available loopback-bound ports. Do not reuse existing database volumes. The two existing containers configured for port 11433 cannot run simultaneously on that binding. Container state and image tags are discovery information, not proof of SQL readiness or exact engine build.

Required integration coverage remains SQL Server 2019 and 2022. Add a SQL Server 2025 compatibility smoke test using its available image; passing that smoke test alone does not establish full release support. Record the image ID/digest and queried engine version in test results rather than relying on mutable `latest` tags.

The test harness must wait for a successful SQL connection, create isolated fixture databases and principals, seed and flush Query Store history, and run CLI checks under each documented diagnostic principal. Keep administrative setup credentials separate from those used by the CLI. Clean up only resources created by the test run.

The inventory checks in the original draft did not start containers or connect to SQL Server; the review that produced this revision did. It created its own disposable containers from the images above, on Podman-assigned loopback ports, ran the statements this document names against 2019 RTM-CU32-GDR and 2022 RTM-CU26, and removed them afterwards. Every claim above marked as measured comes from that run and from no other source. The pre-existing containers listed in the table were neither started nor connected to, which remains the rule.

### Acceptance checks

1. Run the end-to-end workflow against disposable SQL Server 2019 and 2022 databases with seeded Query Store history. Compare rankings and plan counts against independent fixture expectations. Record which TLS path each run used; a stock container forces the trusting one, so a run that does not say which path it took has not established anything about the shipped default.
2. Verify weighted averages with unequal execution counts, multiple plans, duplicate active-interval rows, zero counts, and intersecting boundary intervals. Confirm deterministic ties and displayed units. Assert separately that ranking by `executions` totals `count_executions` rather than a weighted average, and that `--aggregate avg` is refused for it. Assert that an internal query present in the fixture is absent from the default ranking and present under `--include-internal`.
3. Exercise Query Store OFF with no history, OFF with history, READ_ONLY set deliberately, READ_ONLY reached through a limit, empty history, partial retention, missing permissions, missing IDs, and plan/query mismatch. Each must produce the exit code the table in Errors assigns it, and the two OFF cases must not produce the same one. Assert that `readonly_reason` renders as a configured read-only state when it is 0.
4. Round-trip null, empty strings, duplicate labels, tabs/newlines, non-ASCII text, bigint, decimal, and long values through exports. Parse every JSON fixture. Check stdout limits including metadata, with a fixture whose sections together exceed 32 KiB, and assert that no section renders empty because an earlier one spent the budget.
5. Export a plan larger than 80 KiB and a module body larger than the preview cell limit, and compare the artifact byte for byte against the server value. Test large XML, malformed XML, artifact collisions, disk failure, and the collection cap. Open one exported plan in SSMS and record whether a declaration-less UTF-8 `.sqlplan` is accepted.
6. Confirm cancellation and timeouts release resources. Read `@@LOCK_TIMEOUT` from inside the diagnostic session and assert it equals the configured value: a suite that only checks that cancellation works passes with the timeout at -1. Test errors with synthetic passwords and DSNs and assert no credential leakage.
7. Validate each documented principal against the regime it belongs to: the Query Store principal runs the four Query Store commands and fails the other nine with code 4 or 8; the inspection principal runs the object commands; neither can be made to run `idx usage` or `idx missing` without the instance-wide grant. Build the counterexample explicitly: a principal under the source's `DENY VIEW ANY DEFINITION` posture must produce a named not-visible result for the object commands, never a zero-row success. Assert that `stats list` marks statistics whose properties it could not read. Independently attempt writes with every principal and confirm rejection. Include elevated-principal counterexamples in setup guidance.
8. Build and smoke-test Linux and Windows artifacts. Include a concise agent skill describing discovery, health-first diagnosis, artifact handling, and permission limits.

Release is ready when the workflow completes with bounded output and accessible complete artifacts, all required checks pass, and documentation states the tested platform, authentication and certificate-validation matrix. Token savings are a goal, not a numerical claim until measured against representative equivalent output.

## Delivery slices and deferred work

Implement in three dependency-ordered slices after design approval: (1) connection/output/artifacts plus `info` and `qs status`; (2) `qs top`, `qs query`, and plan export/summary; (3) object, size, index, and statistics inspection, packaging, and agent guidance. These are one MVP, not independent expansion projects.

Two things belong to slice 1 that a reading of the slice titles would put later. The held connection with asserted session settings is the first of them: it is not a refinement of the connection code, it is the connection code, and retrofitting it after slice 2 means every diagnostic query written in between ran without a lock timeout. The permission probe is the second, because slice 1 already ships `help`, which must state permissions, and `qs status`, which is the command a DBA runs to find out whether the principal was provisioned correctly.

Deferred: arbitrary `q`, JSONL, integrated/Entra/Kerberos authentication, MCP, broker service, regression/temporal comparison, waits, forced-plan reports, plan diff, script generation, physical index scans, index consolidation/unused verdicts, statistics histograms/staleness verdicts, cross-server comparisons, snapshot history, and automatic maintenance.

Also deferred, and named here because the previous list read as exhaustive while these fell through it: `idx operational`; `obj list`, `obj deps` and column search; `size db` and `size top`; `idx fk-unindexed`; grouping by query shape (`qs hashes`, `qs adhoc`, `--query-hash`, `--plan-hash`, `--sql-handle`); `qs variation`; `config drift`; `qs config-script` and `qs purge-script`; the `--expert` output mode; the `qs top` filters other than `--object` and `--min-executions`, namely `--text-contains`, `--min-duration-ms`, `--min-cpu-ms`, `--min-reads`, `--exclude-object`, `--exclude-text` and `--days`; the ten ranking metrics beyond CPU, duration, logical reads and executions; the distinct query, plan and text counts in `qs status`; memory and visible core counts in `info`; the `--min-uptime` refusal guard, of which v0.1 keeps only the warning; and the reverse lookup from a table to the queries that read it, which needs plan XML scanning rather than Query Store metadata.

Configurable truncation is not deferred. It was in the source, it was dropped without being listed, and `--preview N`, `--truncate N` and `--no-truncate` are back in the MVP under Output and artifacts.

Review focus: confirm the curated-only MVP, the proposed authentication and platform baseline, and the two principals in the permission matrix, which is the decision this revision most changed. Two of five planned reviewers did not run against the original draft; the sections written or rewritten here on the strength of a single reader are the permission matrix, the exit code table and the stdout budget allocation. Once reviewed, convert this design into an implementation plan; do not start coding from this draft.
