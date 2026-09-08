# argosql MVP design

Status: revised after external design review. No implementation is authorized by this document.

Source: [TASKS01.md](../../TASKS01.md). Prepared using the SuperPowers brainstorming workflow. The source is an exploratory conversation, not a validated technical specification; the decisions below resolve its competing suggestions.

Revision of 8 September 2026: normative contracts below supersede conflicting proposals in [the historical review report](2026-09-08-argosql-mvp-design-review.md). That report records experiments on SQL Server 2019/2022 and the incomplete reviewer panel; its measurements have not been rerun for this edit. The user explicitly selected `TrustServerCertificate=true` as the default. This revision updates the specification only.

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
| `obj code <schema.name>` | Export visible module definition to `.sql`; return identity and line count; use the definition-state contract below; preserve an explicit unresolved state when absence and invisibility cannot be separated |
| `size table <schema.name>` | Approximate row count and allocated/used/reserved space, with index and allocation-type breakdowns that avoid double counting |
| `idx list <schema.name>` | Name/type, ordered keys with direction, included columns, filter, uniqueness, disabled state; shares implementation with table inspection |
| `idx usage <schema.name>` | Seeks/scans/lookups/updates, last-use timestamps, server start time if permitted, and explicit observation-window limitations |
| `idx missing [--table <schema.name>] [--top N]` | Default top 10 suggestions, raw DMV evidence, transparent impact score, and advisory limitations; no CREATE script |
| `stats list <schema.name>` | Ordered columns, update time, row/sample counts, sample percentage, modification counter, auto/user-created flags, and filter; per-row completeness; limited principals need `SELECT` on statistics columns for properties |

`qs top` supports `--object <schema.name>` and `--min-executions N` (integer >= 1, default 1). `--by` defaults to cpu, `--aggregate` to total, and `--top` to 10. Both ranking commands accept --top from 1 to 100. Query and plan IDs are positive signed 64-bit integers. `qs query` lists plans with no executions in the window explicitly, with zero executions and null averages. Syntactically invalid IDs and flag values yield code 2.

`--object` filters on `sys.query_store_query.object_id`, the parent module. Accept procedures, functions and triggers; reject a resolved table or other unsupported object type with code 2. An unresolved name follows the object-visibility contract. Ad-hoc queries with object_id 0 do not match this filter. Help and output label it `parent_module`, never "queries touching this table". Reverse lookup through referenced objects is deferred.

`qs top` excludes `is_internal_query = 1` by default; `--include-internal` includes it. Direct lookup with `qs query <id>` returns an existing internal query and labels `is_internal_query=true`, avoiding a misleading not-found result. The ranking header records its filter.

Do not include a `stale` verdict in v0.1: show the evidence without implying that a universal percentage is SQL Server's update threshold. Missing-index impact is `(user_seeks + user_scans) * avg_total_user_cost * avg_user_impact / 100`; label it a ranking score, not predicted elapsed-time savings. Do not fabricate percentage coverage by existing indexes.

`sys.dm_db_index_usage_stats` is filtered by `database_id = DB_ID()` before joining local object metadata. Missing-index queries join group stats to groups by `group_handle = index_group_handle`, then to details by `index_handle`; filter on `details.database_id = DB_ID()`. Use left joins for optional local names so metadata visibility cannot silently remove DMV evidence. An optional table filter must first resolve through the object-visibility contract.

`stats list` starts with visible `sys.stats` rows and uses `OUTER APPLY sys.dm_db_stats_properties`, preserving statistics with inaccessible properties. Return `properties_status=available|permission_denied|unavailable`; only use permission_denied when a permission check establishes it. Missing properties alone do not prove the cause. Null `last_updated` can legitimately mean that no statistics blob exists. Report unavailable-property counts separately from preview truncation. No readable statistics is a successful empty result only after the target object was resolved. See [statistics properties and permissions](https://learn.microsoft.com/en-us/sql/relational-databases/system-dynamic-management-objects/sys-dm-db-stats-properties-transact-sql?view=sql-server-ver16).

## Query Store semantics

`qs top` and `qs query` accept either `--hours N` (positive integer, default 24) or both `--since RFC3339 --until RFC3339`. Reject mixed modes, timestamp overflow, and since >= until with code 2. Resolve relative time once in UTC for the entire command. The requested window is half-open [since, until); select intervals with start_time < until AND end_time > since. Disclose the requested window and actual interval coverage; boundary intervals are not prorated or claimed to be exact per-execution filtering.

Filter to successful executions (`execution_type = 0`), then sum every runtime row at the plan/interval/replica grain before combining plans. Retaining execution_type in GROUP BY after filtering is redundant but correct. Never deduplicate active-interval rows by selecting one representative row.

Totals for CPU, duration and logical reads are `sum(avg_metric * count_executions)`; averages divide those totals by summed executions, returning null for a zero denominator. For `executions`, total is `sum(count_executions)` and `--aggregate avg` is rejected with code 2. Convert CPU/duration microseconds to milliseconds; logical reads remain 8-KB page counts. Preserve sufficient numeric precision through intermediate aggregation and round only for display. See [runtime statistics](https://learn.microsoft.com/en-us/sql/relational-databases/system-catalog-views/sys-query-store-runtime-stats-transact-sql?view=sql-server-ver16).

Embedded SQL uses explicit column lists and version-specific statements. On 2022, carry runtime `replica_group_id` through every aggregation: `qs top` ranks (query_id, replica_group_id), and `qs query` reports (plan_id, replica_group_id). Ties sort by query_id then replica_group_id; plan rows sort by plan_id then replica_group_id. On 2019 expose a null replica_group_id without selecting the absent column. Never require a matching `sys.query_store_replicas` row to retain results; v0.1 displays raw group IDs without resolving replica names. Multi-replica collection is outside the validated release matrix, but synthetic aggregation tests must prove that different groups are not combined.

Every Query Store command reads health first. Emit structured warnings for non-READ_WRITE state, capture restrictions, and requested history outside available coverage. READ_ONLY history remains inspectable with a warning. A genuinely empty selection succeeds with zero rows. Permission errors must not be converted to empty results. Query Store health and retention affect interpretation; see Microsoft's [Query Store practices](https://learn.microsoft.com/en-us/sql/relational-databases/performance/best-practice-with-the-query-store?view=sql-server-ver16).

`qs status` succeeds with code 0 whenever it can read its required status fields, including OFF, READ_ONLY or ERROR and no history; these states produce warnings. For `qs top` and `qs query`, OFF/READ_ONLY/ERROR with readable runtime history permits analysis with state and coverage warnings. If no runtime history is accessible in these states, return code 4. History existence is assessed independently of the requested window; no matching intervals within retained history returns an empty ranking, not unavailable. `plan` can export a retained plan even if no runtime history remains. With permissions established, a missing query/plan is code 8. Backend SQL errors remain errors, not empty history.

`readonly_reason` is rendered in conjunction with desired and actual state. Zero plus desired=READ_ONLY and actual=READ_ONLY means `configured_read_only`; zero in READ_WRITE means `none`; zero in other states means `no_reason_reported`. Never infer deliberate read-only configuration from zero alone. Decode nonzero values as a bitmask, retain the raw integer, and expose unknown bits numerically.

Plan summaries identify the source as Query Store compiled plan XML. Include the statement's estimated cost, up to five operators ranked by estimated subtree cost, referenced objects/indexes, and warning elements actually present. Costs are optimizer estimates; do not sum overlapping subtree costs or infer actual row counts, actual spills, or observed runtime from absent attributes. Unsupported XML elements do not prevent raw export; malformed XML yields a summary error while retaining a successfully exported artifact.

No statement count is required in the summary. Do not assume that a statement-level plan is small. Parse XML incrementally with a token reader, avoiding a whole-document DOM; retain at most five ranked operators, 100 distinct object/index references, and 100 warning summaries. Order operator ties by statement traversal order then NodeId. Mark capped reference/warning lists as truncated and point to the raw artifact. Accept unknown elements, reject malformed XML with code 5, and preserve a complete raw export when summary generation fails. Driver buffering may retain one SQL value; the parser must not add an unbounded DOM on top of that value.

Export SQL text, definitions and plan XML as UTF-8 without BOM and without line-ending normalization. Do not synthesize an XML declaration. If a source declaration exists and declares another encoding, normalize only its encoding value to UTF-8 and record `encoding_normalized=true`. For the ordinary declaration-free source, compare the artifact bytes to the UTF-8 encoding of the exact SQL value. Do not promise that all possible Query Store plans have no declaration.

## Output and artifacts

Default output is escaped TSV without alignment padding. `--format json` produces a single versioned JSON envelope containing context, named result sets, typed column descriptors, row arrays, warnings, artifact paths, and completeness metadata. Multiple sections use named tables in both formats. JSONL is deferred.

Default preview limits: 10 rows per table, 200 Unicode code points per text cell, and 32,768 bytes for the entire serialized stdout including its final newline. `--preview N` accepts 0–10,000; `--truncate N` accepts 1–10,000; `--no-truncate` disables only the cell-length limit and conflicts with an explicit `--truncate`. Invalid values yield code 2. These flags do not change collection limits or full artifacts.

The byte cap takes precedence over both preview flags. Build a complete bounded response before writing stdout. Reserve table metadata and artifact references first; divide remaining bytes equally among nonempty sections, rounding down. Add whole rows in their defined order without exceeding either the section allocation or preview count. Redistribute unused bytes in declared section order. Never split a serialized row or emit invalid JSON. A row too large to fit, including under `--no-truncate`, may result in zero displayed rows.

Each table reports `rows_collected`, `rows_shown`, `collection_complete`, `preview_complete`, `properties_complete`, and `omitted_reasons` (row_limit, byte_limit, cell_limit, collection_limit, property_unavailable as applicable). Zero shown with collected rows is `preview_omitted`, not `empty`; TSV states this explicitly. Only zero collected rows with complete collection can be called empty. A display limit alone keeps exit code 0. Missing optional properties also keep code 0 with warnings; incomplete collection yields code 7. Full artifacts retain complete cells within collection limits.

Declared table order: `info`: identity; `qs status`: status, coverage; `qs top`: queries; `qs query`: query, plans; `plan --summary`: statement, operators, references, warnings; `obj table`: table, columns, indexes; `obj code`: module; `size table`: table, allocations; `idx list`: indexes; `idx usage`: usage; `idx missing`: suggestions; `stats list`: statistics. Rows use IDs/ordinals ascending unless ranking is specified; allocation rows use index_id, partition_number, allocation type. Full schemas and stable column order are versioned in the implementation's command registry.

If metadata alone exceeds the cap, emit a compact response containing the manifest path and `preview_omitted=true`; retain full metadata in the manifest. If even the compact response cannot fit, return code 6 with a fixed, bounded error envelope and no embedded oversized path. Artifact-writing failure is likewise code 6. Cell-truncation markers use `…[+N]`, where N counts omitted Unicode code points; metadata is authoritative if the literal source contains that marker.

TSV encodes null as `\N`, empty text as an empty cell, and escapes backslash, tab, CR, and LF. Escape column names too. Full exports preserve strings and exact decimal values without display rounding; JSON represents bigint and decimal values as strings with SQL type metadata to avoid consumer precision loss. Preview-only float rounding is allowed and documented. Duplicate column labels remain positional.

Artifacts are UTF-8 without BOM and follow the XML normalization rule above. Use a unique invocation directory under the OS cache directory, or `--out-dir`. Use owner-only access where supported, exclusive file creation, sanitized generated filenames, and atomic completion. Never overwrite existing files. The manifest records context, UTC collection time, schema version, counts, completeness, TLS mode and artifact paths; never credentials. Reserve space for manifest completion within the artifact quota. Cleanup is manual in v0.1.

Bound each invocation to 10,000 collected tabular rows and 100 MiB of artifact data (104,857,600 bytes). The row cap is a defensive ceiling, not an expected workload size; test it with a synthetic collector fixture. Detect an additional row before declaring a result partial at the exact boundary. Intentional SQL TOP selection is complete for that request. The byte cap also applies to single XML/definition values: do not publish truncated source files; return code 7 with an explicit omitted-artifact record. Retain previously completed artifacts as partial invocation results. Stream table exports; never accumulate the full result set solely to produce a preview. A single driver value can be buffered before its size is checked, so artifact quotas do not claim a hard process-memory ceiling.

## Connection and execution

Use a named YAML profile selected by `--ctx`; `--config` overrides `<os.UserConfigDir()>/argosql/config.yaml`. Profiles contain host, port (default 1433), optional database, username, password_env, trust_server_certificate (default true), and optional ca_file. Resolve a relative ca_file against the config directory. Reject unknown fields, plaintext password fields, missing password environment variables, and empty usernames with code 2. `--db` overrides the profile database. Do not load `.env` implicitly or accept raw DSNs.

Explicitly set driver `encrypt=true` on every connection. Set `TrustServerCertificate=true` by default, as requested by the user, including when the profile omits trust_server_certificate. This accepts the server certificate without chain/hostname validation while retaining transport encryption. Report `tls_encryption=required` and `certificate_validation=skipped` in context metadata; this normal default requires no confirmation or repeated warning.

A profile can set `trust_server_certificate: false` to validate the certificate chain, expiry and host name. Use the OS trust store when ca_file is absent; map ca_file to the driver's `certificate` parameter when supplied. Reject ca_file together with trust_server_certificate=true as contradictory configuration. Certificate validation or handshake failure returns code 3; never retry with validation or encryption disabled. Driver parameter semantics are defined by [go-mssqldb v1.11.0](https://github.com/microsoft/go-mssqldb/blob/v1.11.0/README.md#connection-parameters-and-dsn).

Example configuration (the explicit true is optional and shows the default):

```yaml
profiles:
  client:
    host: localhost
    port: 11533
    database: AppDB
    username: asq_diagnostic
    password_env: ASQ_CLIENT_PASSWORD
    trust_server_certificate: true
```

Profile fields are snake_case and case-sensitive. There are no TLS CLI overrides in v0.1. A nonempty ca_file must be readable and parseable; otherwise return code 2 before connecting. Validated connections report `certificate_validation=verified` after the handshake succeeds. Neither mode permits disabling encryption.

Use Go `database/sql` with `github.com/microsoft/go-mssqldb`, pinned when implementation starts. Acquire one `*sql.Conn`, run setup and every diagnostic statement on it, and close it and the owning pool at invocation end. A pool limited to one connection does not substitute for holding a Conn: session reset can occur when a connection returns to the pool. No query may execute through the pool after setup.

Session setup executes SET LOCK_TIMEOUT 5000 on the held connection, then reads @@LOCK_TIMEOUT before the first diagnostic query; a mismatch returns code 5. The 5-second lock timeout is fixed in v0.1. `--timeout` accepts integer seconds from 1 to 300 (default 30); its clock starts before connection acquisition and covers connection, setup, probes, health queries and result consumption. The connection phase also has a 5-second deadline, capped by the remaining overall time. Connection-phase expiry returns 3; expiry after connection returns 5. User interruption returns 130. Close result streams, held connection and pool on every exit. No application-level query retries are allowed; record any pinned-driver retry behavior in connection tests.

Embed curated SQL with `go:embed`. Bind values as parameters; map sortable metrics to static SQL fragments. Resolve schema/object names through parameters rather than concatenating executable identifiers. Keep session setup and queries on the same connection. Preserve default isolation; neither READ UNCOMMITTED nor transaction rollback is a security mechanism.

## Permissions and confidentiality

The CLI exposes curated diagnostics only. Setup examples are explicit sufficient grant bundles, not claims of universally minimal permissions. Grants on an object or schema may expose definitions and data; direct server-DMV permissions cover the instance even when the CLI filters to one database. Signed wrapper modules or a broker could provide narrower access, but neither is part of v0.1.

Define three cumulative fixture principals, without fixed privileged roles, ownership, or write grants:

- `Q` (Query Store): CONNECT to the target database and VIEW DATABASE STATE. On 2022 retain this sufficient bundle; the runtime view documents the narrower VIEW DATABASE PERFORMANCE STATE permission, which is not assumed to cover all other queried views.
- `I` (inspection): Q plus VIEW DEFINITION in the fixture database and SELECT on fixture tables. For size collection through sys.dm_db_partition_stats, validate the database state/definition permissions on 2019 and the corresponding VIEW DATABASE PERFORMANCE STATE / VIEW SECURITY DEFINITION requirements on 2022. Grant these finer permissions explicitly in the 2022 fixture. This profile can read fixture business data.
- `S` (instance diagnostics): I plus VIEW SERVER STATE on 2019 or VIEW SERVER PERFORMANCE STATE on 2022. The instance grant permits more observation than the selected database alone.

The runtime-view permission difference is documented in [sys.query_store_runtime_stats](https://learn.microsoft.com/en-us/sql/relational-databases/system-catalog-views/sys-query-store-runtime-stats-transact-sql?view=sql-server-ver16); size-query requirements are documented in [sys.dm_db_partition_stats](https://learn.microsoft.com/en-us/sql/relational-databases/system-dynamic-management-objects/sys-dm-db-partition-stats-transact-sql?view=sql-server-ver16). Each executed SQL statement must have a version-specific permission assertion in the integration fixtures before release. Help documents sufficient tested bundles and optional capabilities; do not label an untested list as minimal.

The following matrix uses existing fixture objects, healthy Query Store history and valid IDs. All three principals can connect; Q has no object-level grants in the fixture schema. Additional permission grants inherited from public must be excluded by fixture setup.

| Command | Q | I | S |
| --- | --- | --- | --- |
| help --json | 0, offline | 0, offline | 0, offline |
| info | 0 | 0 | 0 |
| qs status | 0 | 0 | 0 |
| qs top | 0 | 0 | 0 |
| qs query | 0; parent name may be unavailable | 0 | 0 |
| plan | 0 | 0 | 0 |
| obj table | 8, not_found_or_not_visible | 0 | 0 |
| obj code | 8, not_found_or_not_visible | 0 | 0 |
| size table | 8, not_found_or_not_visible | 0 | 0 |
| idx list | 8, not_found_or_not_visible | 0 | 0 |
| idx usage | 8 for an unresolved fixture table | 4, instance permission absent | 0 |
| idx missing (no table filter) | 4 | 4 | 0 |
| stats list | 8, not_found_or_not_visible | 0 | 0 |

Permission checks follow target resolution for commands taking object names. This fixes error precedence: idx usage on an unresolved name returns 8 before checking its server permission. info does not require server-start-time access; any added optional start-time field is null with a capability warning when unavailable. obj table may return columns/indexes with unavailable optional row count if size permissions are absent; size table itself requires those permissions.

Size collection uses sys.dm_db_partition_stats at (object_id, index_id, partition_number). Table row count sums row_count only for index_id IN (0,1). Sum used/reserved pages once across relevant partitions and indexes; unused = reserved - used. In-row, LOB and row-overflow are a breakdown of these totals, never additional totals to add again. Label bytes as pages * 8192 and MiB as bytes / 1048576. Allocation type and index are independent breakdown dimensions. Memory-optimized table size is unavailable with code 4 in v0.1; columnstore allocations retain the DMV's LOB category and must not be described as solely user LOB columns. See [partition statistics](https://learn.microsoft.com/en-us/sql/relational-databases/system-dynamic-management-objects/sys-dm-db-partition-stats-transact-sql?view=sql-server-ver16).

Test additional principals separately: object metadata access without SELECT (statistics listed with unavailable properties); SELECT on only one statistic's columns (mixed availability); explicit denial of a required permission; visible module with inaccessible definition; encrypted module; and insufficient visibility for an unresolved name. Runtime diagnostics must not demand a whole recommended bundle where effective narrower grants suffice.

No principal, including Q, is described as guaranteeing confidentiality of business data: Query Store itself may contain literals and compiled parameter values.

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

Object and definition resolution uses the current principal only; it does not elevate permissions. Permission probes return known-allowed, known-denied, or unknown. Preserve unknown instead of converting it to denied.

For object names, resolve the exact schema/name first. A resolved object of the wrong type gives code 2. An unresolved name yields code 8 with `not_found_or_not_visible`, unless sufficient effective metadata visibility demonstrably establishes absence, in which case use `not_found`. Do not infer absence from a database-level permission alone if narrower DENY can hide the object. A probe is not required to separate these cases when the engine cannot.

For obj code, a visible supported module with a non-null definition yields `available` and an artifact. A confirmed encrypted module yields `encrypted`, code 4, no definition artifact. A visible module with a confirmed denied definition permission yields `permission_denied`, code 4. Otherwise a null definition yields `definition_unavailable`, code 4, without inventing its cause. An unresolved module follows code 8 above. Recheck disappearance during collection and report unavailable rather than treating it as an empty definition.

## Errors

Exit codes: 0 successful (including empty results and health warnings); 2 invalid arguments/config; 3 connection/authentication/TLS; 4 permission or feature unavailable; 5 execution failure/timeout; 6 artifact or serialization failure; 7 collection limit reached; 8 requested entity not found or not visible; 130 user interruption.

Exit codes, structured error kinds, and completeness metadata form the machine-readable outcome. Validation precedes connection; object resolution precedes object-specific permission checks; feature/permission checks precede collection. A later output failure uses code 6 even if the collected data was already partial.

| Outcome | Code |
| --- | --- |
| Ranking or listing returned, possibly with warnings | 0 |
| Query Store READ_ONLY, or OFF with history: frozen result plus warning | 0 |
| Genuinely empty selection in a healthy window | 0 |
| qs status with readable state, even OFF/ERROR and no history | 0 |
| qs top/qs query in OFF/READ_ONLY/ERROR with no readable runtime history | 4 |
| Retained plan export without runtime history | 0 |
| Encrypted or unavailable module definition | 4 |
| Command needs a permission the principal lacks | 4 |
| Command needs a version feature the server does not have | 4 |
| `query_id` or `plan_id` not present in this database | 8 |
| `--plan-id` does not belong to the given `query_id` | 8 |
| Object name does not resolve, or is not visible to this principal | 8 |
| Collection cap reached, partial result written | 7 |

Diagnostics go to stderr with a stable code and safe message; retain SQL Server error numbers when available. JSON errors use the same envelope with `ok: false`, an error object, and any explicitly partial artifacts. Never stream a JSON prefix that cannot be completed after an error. Missing required permissions fail the command; optional fields use null plus a warning.

Object commands use the resolution contract above: code 8 may mean `not_found_or_not_visible`, and no client-side permission probe is promised to eliminate that ambiguity. A known-denied permission on an already resolved object is code 4. An empty listing is code 0 only for a resolved target and a complete, accessible selection.

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

The historical review describes disposable-container experiments; this specification edit did not rerun them. Before using those observations as release evidence, preserve reproducible setup, queries, expected results, image identifiers and actual engine versions under tests/integration. Existing containers remain untouched.

### Acceptance checks

1. Run the reference workflow on SQL Server 2019 and 2022 with seeded history under S, then exercise the command/principal matrix exactly. Compare rankings and counts with fixture expectations. Use an administrative test connection only for setup and observation, never as the CLI principal.
2. Verify weighted totals/averages, zero denominators, multiple plans/replica groups, active-interval multiplicity, deterministic ties and units. Test internal-query exclusion in ranking and direct internal-ID lookup. Synthetic rows supplement live fixtures when engine state cannot be reproduced deterministically.
3. Verify status/analysis/plan behavior separately for OFF with/without history, READ_ONLY, ERROR, no matching time window, missing IDs and plan mismatch. Fixture the otherwise nondeterministic ERROR state at the backend boundary. For readonly_reason=0 test READ_WRITE, configured READ_ONLY, and other states independently.
4. Round-trip nulls, empty strings, special characters, Unicode, duplicate labels, bigint and decimal values. Exercise multiple sections exceeding 32 KiB, a single oversized cell with --no-truncate, --preview 0, metadata-only fallback and an oversized manifest path. Assert valid JSON, exact byte caps and explicit preview_omitted versus empty states.
5. Compare ordinary plan and module artifact bytes to the UTF-8 source value, including a plan above 80 KiB. Test declaration normalization separately. Test malformed XML, retained raw export after summary failure, large single-statement plans, bounded operator/reference/warning retention, file collisions, disk failure, and exact/over-limit row and byte boundaries. Instrument parser state or allocations to detect whole-document DOM construction; do not mistake a five-row summary for bounded parsing.
6. Verify @@LOCK_TIMEOUT on the held connection before diagnostics and after a preceding query. Exercise a real conflicting lock, overall deadline expiry, cancellation, stream closure and connection disposal. Check synthetic secrets and DSNs never appear in application diagnostics.
7. Verify partial statistics with OUTER APPLY, known-denied versus unknown causes, encrypted/inaccessible/absent modules, and unknown visibility returning not_found_or_not_visible. Attempt writes under Q, I and S independently and confirm rejection. help must succeed with no database config or credentials; info must succeed without instance DMV permissions.
8. Exercise the default TLS path on stock containers with trust_server_certificate omitted and explicitly true. Verify successful encrypted transport using an administrative observation of the CLI session. Exercise false with a test CA/certificate and matching host (success), unknown issuer (code 3), wrong host (code 3), and expired certificate (code 3). Reject ca_file with true (code 2). Assert explicit driver encrypt=true and absence of fallback after TLS failure. Keep TLS test keys and trust stores isolated from user/system configuration.
9. Build and smoke-test Linux and Windows clients; cross-compilation alone is not a Windows runtime test. Include concise agent guidance for help discovery, health-first diagnosis, artifacts, and permissions.
10. Version test setup SQL, workloads, permission fixtures and TLS fixture generation under tests/integration, plus documented invocation/cleanup commands. Record driver/Go/engine versions and image digest or ID in test outputs. Do not recreate historical transcripts and label them as original evidence.

Release is ready when the workflow completes with bounded output and accessible complete artifacts, all required checks pass, and documentation states the tested platform, authentication and certificate-validation matrix. Token savings are a goal, not a numerical claim until measured against representative equivalent output.

## Delivery slices and deferred work

Implement in three dependency-ordered slices after design approval: (1) connection/output/artifacts plus `info` and `qs status`; (2) `qs top`, `qs query`, and plan export/summary; (3) object, size, index, and statistics inspection, packaging, and agent guidance. These are one MVP, not independent expansion projects.

Slice 1 includes the held connection, asserted session settings, both TLS modes, tri-state permission probes, artifact limits, and output completeness contracts. Define each command’s permission requirements and expected errors alongside its SQL and tests in the slice that introduces it. Release help lists implemented commands only and derives its parameters and tested capabilities from the registry.

Deferred: arbitrary `q`, JSONL, integrated/Entra/Kerberos authentication, MCP, broker service, regression/temporal comparison, waits, forced-plan reports, plan diff, script generation, physical index scans, index consolidation/unused verdicts, statistics histograms/staleness verdicts, cross-server comparisons, snapshot history, and automatic maintenance.

Also deferred: `idx operational`; `obj list`, `obj deps` and column search; `size db` and `size top`; `idx fk-unindexed`; grouping by query shape (`qs hashes`, `qs adhoc`, `--query-hash`, `--plan-hash`, `--sql-handle`); `qs variation`; `config drift`; `qs config-script` and `qs purge-script`; `--expert`; additional ranking filters (`--text-contains`, `--min-duration-ms`, `--min-cpu-ms`, `--min-reads`, `--exclude-object`, `--exclude-text`, `--query-id`, `--plan-id`); `--days`; ranking metrics beyond CPU, duration, logical reads and executions; distinct query/plan/text counts in qs status; memory/core counts in info; `--min-uptime`; `--out` and `--plans-dir` (v0.1 uses --out-dir); and reverse table-to-query lookup. Multi-replica deployment validation is also deferred.

Configurable preview/cell truncation is included under Output and artifacts; it never disables the stdout byte cap.

The next artifact is an implementation plan based on these contracts. The TLS default is a confirmed user decision; it does not require further approval. Historical review findings remain in their report rather than serving as additional, competing requirements.
