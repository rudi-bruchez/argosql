# argosql

`asq` is a single Go binary that diagnoses SQL Server through the Query Store and the
system catalog. It answers the kind of question a DBA or an AI coding agent asks while
chasing a slow query: which queries cost the most, what their recorded plans look like,
what the objects they touch are made of. The output is shaped for a context-limited
reader: previews are bounded, artifacts are written to disk rather than dumped to stdout,
and every response declares what it collected versus what it is showing.

Bounding the output is the goal this project is built around. It has not yet been
measured against an equivalent unbounded export, so this page makes no claim about how
much smaller the bounded output is. What is true today: stdout stays within a fixed byte
budget, previews are capped in rows and in characters per cell, and the full data is
still reachable, as a file, when the preview is not enough. See
[docs/usage.md](docs/usage.md) for how that works in practice.

## What it does, and does not do

The reference workflow, the one `asq` is built around, runs from a query to the objects
it touches:

```sh
asq --ctx client --db AppDB qs status
asq --ctx client --db AppDB qs top --by cpu --hours 24 --top 10
asq --ctx client --db AppDB qs query 4821 --hours 24
asq --ctx client --db AppDB plan 4821 --plan-id 9033 --summary
asq --ctx client --db AppDB obj table dbo.Orders
asq --ctx client --db AppDB idx usage dbo.Orders
asq --ctx client --db AppDB stats list dbo.Orders
```

Every command is a curated, parameterized diagnostic query against Query Store and the
catalog views: there is no arbitrary SQL execution, no maintenance command, no script
generator, and nothing that writes to the database being diagnosed. `asq help --json`
lists every registered command, its flags, the permissions it needs, and the SQL Server
versions it is tested against, from the same registry the binary itself runs on, so the
list can never drift from what the build actually does.

Deferred for now, so they do not surface as a command, a flag, or a silent partial
implementation: arbitrary SQL, JSONL output, integrated/Entra/Kerberos authentication, an
MCP adapter, a credential broker, regression or temporal comparison, wait statistics,
forced-plan reports, plan diffing, script generation, physical index scans, index
consolidation or unused-index verdicts, statistics histograms or staleness verdicts,
cross-server comparison, snapshot history, and automatic maintenance.

## Installing

Build from source with the pinned Go toolchain named in `go.mod`:

```sh
make build
```

This produces `dist/asq-linux-amd64` and `dist/asq-windows-amd64.exe`, both built with
`CGO_ENABLED=0` and `-trimpath`, so either one is a single file with no runtime
dependency beyond the target OS. Copy the one matching your platform wherever your shell
finds binaries, under the name `asq` (or `asq.exe` on Windows).

## Configuring a connection

`asq` reads named profiles from a YAML file, `<your OS config dir>/argosql/config.yaml`
by default, overridable with `--config`. A profile never holds a password directly: it
names the environment variable that does.

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

`trust_server_certificate: true` is the default, including when the line above is
omitted entirely: the connection is still encrypted, but the server certificate is
accepted without chain or hostname validation, the same tradeoff `sqlcmd` and most ad hoc
tooling make by default. Set it to `false`, with an optional `ca_file`, to validate the
certificate against a real chain. There is no way to disable encryption itself. See
[docs/permissions.md](docs/permissions.md) and the provisioning script, `login.sql`, for
what to grant the login this profile names, and
[docs/testing.md](docs/testing.md) for exactly which platform, authentication and
certificate-validation combinations have actually been exercised.

## Limits worth knowing before you rely on this

`asq` holds exactly one database connection for the life of an invocation. A command that
opens a row set and, anywhere in its own code, issues another query on that same
connection before consuming every row blocks forever: no exit code, no error, nothing.
This is not a hypothetical; it was measured once in this project's own development. If a
future command hangs with no output at all, this is the first thing to suspect, not a
network timeout.

The project supports SQL Server 2019 and 2022, SQL authentication only, from Linux amd64
and Windows amd64 clients; SQL Server 2025 receives a smoke test only, not full release
coverage. `docs/testing.md` states exactly which of these combinations this build has
actually been run against, rather than assuming a build target that compiles is a
platform that was tested.

## Documentation

- [docs/usage.md](docs/usage.md): output shapes, artifacts, exit codes, the agent
  workflow.
- [docs/permissions.md](docs/permissions.md): the three permission tiers and what each
  command needs, alongside `login.sql`.
- [docs/testing.md](docs/testing.md): how to reproduce the test suite, the tested
  platform/authentication/certificate-validation matrix, and what this build's release
  evidence actually covers.
- [skills/argosql/SKILL.md](skills/argosql/SKILL.md): the agent skill for using `asq`
  inside a coding session.
