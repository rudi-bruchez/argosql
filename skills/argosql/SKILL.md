---
name: argosql
description: Diagnose SQL Server Query Store and object metadata using the asq CLI.
---

Start with `asq help --json`. It lists every command this build actually implements,
with its flags, required permissions and tested versions, generated from the same
registry the binary runs on. Do not assume a command exists, or takes a flag, from
memory or from an earlier session: re-run `help --json` if either is in doubt.

Require an explicit context (`--ctx`) and an explicit database (`--db`, or one already
named in the profile) before running anything else. `asq` never guesses a database and
never falls back to `master`.

Read `qs status` before interpreting any historical metric from `qs top` or `qs query`.
Query Store can be OFF, READ_ONLY, or carrying less retained history than the window a
later command asks for; `qs status` is what says so, and a ranking run without first
checking it can read as current when it is actually frozen or partial.

Follow query IDs to plans and referenced objects: `qs top` to find an expensive
`query_id`, `qs query <id>` to see its plans, `plan <id> --plan-id <id> --summary` to
inspect one of them, then `obj table`, `idx list`, `idx usage`, or `stats list` on the
objects the plan references. There is no reverse lookup from an object back to the
queries that touch it in this release.

Inspect completeness metadata before treating a missing row, a missing property, or a
zero count as absence. `rows_collected`, `collection_complete`, `properties_complete`,
and `omitted_reasons` distinguish "nothing found" from "something here was not
readable", and `properties_status` on `stats list` distinguishes a genuine gap from a
denied permission. A permission refusal is reported as a refusal, with its own exit
code and kind; do not reinterpret it as the object simply not existing.

Load large artifacts (full SQL text, module definitions, plan XML) only when the
preview is not enough. They are files on disk, named in the response's `artifacts` list
and `manifest_path`, not something to request inline by default.

Never suggest forcing a plan, creating an index, dropping an index, or running SQL this
tool did not generate. `asq` is read-only diagnostics over Query Store and the catalog;
none of that is in its scope, and `idx missing` in particular reports raw evidence and a
ranking score, not a recommendation to act on.
