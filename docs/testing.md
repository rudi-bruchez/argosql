# Testing

## Unit tests

```sh
go test ./...
go test -race ./...
go vet ./...
go vet -tags=integration ./...
gofmt -l .
```

All five, or `make check`, run with no container, no network, and no
configuration file. They cover every package except the real-server behavior that
`tests/integration` exists for.

## Integration tests

The `tests/integration` package carries the build tag `integration` and requires
`ASQ_TEST_IMAGE`, naming a local Podman image such as
`mcr.microsoft.com/mssql/server:2022-latest`. Without the tag, `go test ./...` does not
even see this package: it is excluded, not skipped. With the tag and no image named, the
suite fails explicitly, with a message naming the missing variable, rather than skipping
silently.

```sh
podman pull mcr.microsoft.com/mssql/server:2022-latest   # once, if not already local
make integration-2022
make integration-2019
```

Equivalently, by hand:

```sh
ASQ_TEST_IMAGE=mcr.microsoft.com/mssql/server:2022-latest \
  sh tests/integration/run.sh go test ./tests/integration -tags=integration -count=1 -timeout=15m -v
```

Every container the suite creates is labeled `io.argosql.test=<run ID>`. The Make
integration targets use `tests/integration/run.sh`, which prints and exports
`ASQ_TEST_RUN_ID`, runs the test in a child process, then removes remaining containers
with that exact label. It preserves the test's exit status; a cleanup failure after a
successful test makes the wrapper fail. CI also runs `make integration-cleanup` with
`if: always()` and the same ID after the test step.

A direct `go test` timeout panics inside `testing` and bypasses `t.Cleanup`. It can
leave a container running indefinitely. Use the wrapper, including for filtered runs.
If the wrapper itself is killed with `SIGKILL`, the host stops, or Podman cleanup
fails, recover manually using the ID printed before the tests:

```sh
ASQ_TEST_RUN_ID='<exact run ID>' make integration-cleanup
```

The manual target refuses an empty ID. To inspect survivors without removing them:

```sh
podman ps -a --filter 'label=io.argosql.test=<exact run ID>'
```

Never select containers by name or omit the run's label value. The delivered `asq`
binary never creates containers; this cleanup belongs only to the integration harness.
`TestTimeoutCleanup` exercises a real child test timeout with one disposable container,
checks that exit code `2` survives the wrapper, and requires zero survivors by label.
Run it separately with `-run '^TestTimeoutCleanup$' -timeout=60s -v` through the wrapper.

### Running the full suite without exhausting memory

The full `tests/integration` suite, run in one `go test` invocation, was observed on one
development machine to be killed by the kernel for memory exhaustion once swap filled,
before the run could finish. The same suite, split into five separate `go test`
invocations by `-run`, each covering a disjoint group of top-level tests, completed in
full in about seven minutes of cumulative test time, one container at a time. A single
long-running process that starts and tears down dozens of containers in sequence
accumulates memory that a process exit releases; five shorter processes do not carry that
accumulation across the boundary.

The five historical groups cover 55 top-level test functions (verified with
`grep -rh '^func Test' tests/integration/*.go | grep -v TestMain | grep -v TestTimeoutCleanup | wc -l`;
`TestMain` itself never prints a `=== RUN` line, and `TestTimeoutCleanup` is excluded
because it is an additional test run separately, as described above: without that
exclusion the same command returns 56 and no longer reproduces the number quoted here). The five
groups below, run as five separate invocations of the same image, partition that count
exactly: 8, 10, 10, 20, and 7, summing to 55.

```sh
IMG=mcr.microsoft.com/mssql/server:2022-latest

ASQ_TEST_IMAGE=$IMG sh tests/integration/run.sh go test ./tests/integration -tags=integration -count=1 -timeout=15m -v \
  -run '^(TestContainerCarriesRunLabel|TestCleanupRemovesByID|TestSignalInterruptRemovesContainer|TestFixtureQueryStoreFlush|TestLabRunOnlyInjectsRequestedSecret|TestErrors|TestSessionTLS|TestSessionLockConflict)$'

ASQ_TEST_IMAGE=$IMG sh tests/integration/run.sh go test ./tests/integration -tags=integration -count=1 -timeout=15m -v \
  -run '^(TestInfo|TestStatus|TestTop|TestQueryExport|TestQueryNotFound|TestQueryStoreUnavailableAndOffWithHistory|TestQueryNoExecutionsInWindowKeepsPlanRow|TestQueryParentModuleVisibility|TestQueryExportLongUnicodeTextByteIdentity|TestQueryPlansSyntheticAggregation)$'

ASQ_TEST_IMAGE=$IMG sh tests/integration/run.sh go test ./tests/integration -tags=integration -count=1 -timeout=15m -v \
  -run '^(TestPlanSummaryWarningAttributeForm|TestPlanExport|TestPlanNotFound|TestPlanMismatch|TestPlanSummary|TestPlanSucceedsWhileQueryStoreOff|TestPlanUnavailableForNullQueryPlan|TestExports|TestScanRowMeasuredGoTypes|TestConvertCellAgainstRealServer)$'

ASQ_TEST_IMAGE=$IMG sh tests/integration/run.sh go test ./tests/integration -tags=integration -count=1 -timeout=15m -v \
  -run '^(TestEncryptedModule|TestObjects|TestObjCodePermissionDenied|TestObjCodeOnNonModuleObject|TestWrongObjectTypeRejected|TestColumnsAndIndexesPropertiesMaskedBySelectOnly|TestObjTableSizeTableSurviveMissingViewDatabaseState|TestIdxListIgnoresPartitioningColumn|TestSizeTableExposesTotals|TestIdxListHandlesWideIndex|TestSize|TestSizeAllocationsOrderMatchesIndependentUnpivot|TestRegisteredTableOrderMatchesExecution|TestObjTableColumnsAndIndexes|TestIdxListOnHeapExcludesIndexZero|TestPartialStatistics|TestIndexDMV|TestStatsMixedAvailabilityPrincipal|TestPermissions|TestEveryProbeIsWellFormed)$'

ASQ_TEST_IMAGE=$IMG sh tests/integration/run.sh go test ./tests/integration -tags=integration -count=1 -timeout=15m -v \
  -run '^(TestPrincipalMatrix|TestHelpNeverConnects|TestTLSValidCertificate|TestTLSWrongHostCertificate|TestTLSExpiredCertificate|TestTLSHandshakeErrorTextAssumptionHoldsForGoMssqldbV1_11_0|TestWorkflow)$'
```

After each invocation, count its `=== RUN` lines (`grep -c '^=== RUN'` on the `-v`
output) and add the five counts. The sum must be at least 55: more than 55 is expected
and correct, since several of these top-level tests (`TestPrincipalMatrix` in particular)
run subtests that each print their own `=== RUN` line; fewer than 55 means one of the
five `-run` patterns above matched nothing, typically because a test was renamed after
this page was written, and the fix is to update the pattern here, never to treat a short
count as a passing run.

Run these five one at a time, never concurrently: two `go test` processes both holding
the memory-exhaustion problem above in reserve is not a smaller version of it, it is the
same problem with a second cause.

### SQL Server 2025

2025 is not part of the validated release matrix. `make smoke-2025` runs only
`TestWorkflow`, the end-to-end reference-command sequence, against a 2025 image; it does
not run the matrix, TLS, or fault-injection suites that 2019 and 2022 carry. A pass here
is evidence of basic compatibility, not of release support.

### What a run actually proves, and how to read it

Every test above that opens a real connection logs one identity line through
`logEngineIdentity` (`tests/integration/podman_test.go`), visible with `go test -v`:

```
engine identity: image=<resolved image ID> major_version=<19|22> product_version=<SERVERPROPERTY('ProductVersion')> go=<toolchain> driver=go-mssqldb@<version>
```

The image ID is the actual image digest the container was started from, resolved at
container creation, never the mutable `:2022-latest` tag; `product_version` is the full
dotted engine version read directly from the running container with
`SERVERPROPERTY('ProductVersion')`, not inferred from the tag either. A claim of "tested
on SQL Server 2022" is only reproducible alongside this line: without the image ID and
the product version it names, nobody else can start the same engine build this run
actually exercised.

## TLS

`tests/integration/tls_test.go` provisions its own disposable containers with
deliberately crafted certificates (valid, wrong host, expired) and proves that
`trust_server_certificate` and `ca_file` actually drive certificate validation, not
merely that a connection with code 3 happened for some unrelated reason. It runs as part
of the fifth tranche above, under whichever image `ASQ_TEST_IMAGE` names; TLS behavior
itself does not vary between SQL Server 2019 and 2022, so running it once per release is
enough, unlike the version-sensitive suites in the other tranches.

## Platforms, authentication, and certificate validation: what this build has tested

| Dimension | Tested | Not tested |
| --- | --- | --- |
| Client OS/architecture | Linux amd64 (this is where `tests/integration` runs) | Windows amd64: the binary cross-compiles and is built by CI on `windows-latest`, which also runs `go test ./...` and a binary smoke test (`asq help`, and a missing `--config` reporting exit code 2) natively on Windows; none of that reaches a real SQL Server over the network from a Windows client. Cross-compiling the binary is not a substitute for this and is never reported as one. |
| Authentication | SQL authentication, the only mode this project implements | Integrated/Entra/Kerberos authentication: out of scope for this release, not merely untested |
| Certificate validation | `encrypt=true`, `trust_server_certificate=true` (the default, including when the profile omits the field): encrypted transport, no chain or hostname check. `trust_server_certificate=false` with a valid matching certificate, a wrong-host certificate (code 3), and an expired certificate (code 3), each against a disposable container with a purpose-built certificate. | `trust_server_certificate=false` against a certificate issued by a real public or enterprise CA, and against the OS trust store specifically rather than a `ca_file`: the TLS suite supplies its own certificates and never touches either. |

### Testing a real Windows-to-SQL-Server connection

This is a manual procedure, run by a person with both a Windows machine and a reachable
SQL Server, not something this project's CI or `tests/integration` suite executes. It is
documented here, rather than built as a conditional test, because its absence from a
given run must be visible as exactly that: a coverage gap, not something a cross-compiled
binary quietly stands in for.

1. Provision a dedicated fixture database and a single-purpose principal (the `Q` tier
   from [docs/permissions.md](docs/permissions.md) is enough for a basic connectivity
   check) on a SQL Server the Windows machine can reach over the network.
2. Write a profile file naming that principal, and set its password in an environment
   variable private to that session, never the one named `ASQ_WINDOWS_TEST_CONFIG`
   itself, which names the profile file's path, not a secret:

   ```powershell
   $env:ASQ_WINDOWS_TEST_CONFIG = "C:\path\to\windows-fixture.yaml"
   $env:ASQ_WINDOWS_PASSWORD = "<the fixture principal's password>"
   ```

3. Run the reference workflow against it with the Windows binary:

   ```powershell
   .\asq.exe --config $env:ASQ_WINDOWS_TEST_CONFIG --ctx windows-fixture --db AppDB qs status
   ```

A release report that wants to claim "Windows connectivity tested" names the exit code
this produced and the engine identity it connected to; a release report that cannot run
this states plainly that Windows network coverage is 0, the way this implementation's own
report does.

## Release readiness

A release is ready when the full workflow completes with output that stays within its
byte budget and artifacts that are complete and reachable afterward, every required
check above passes, and the documentation states the tested platform, authentication and
certificate-validation matrix, exactly as in the table above. Token savings are this
project's goal, not a published number: no percentage or order-of-magnitude claim appears
anywhere in this documentation until it has been measured against a representative
equivalent export, and that measurement has not been made yet.

The "bounded output, complete artifacts" half of that is a concrete check, not a
restatement of the tests passing:

```sh
asq --ctx client --db AppDB qs query 4821 --format json --out-dir ./release-check > out.json
wc -c out.json                                             # must be <= 32768
jq -r '.manifest_path' out.json | xargs cat                # must be present, complete, and readable
```

This is the check to run against a live server as part of a release; it has not been run
in this documentation task, since no diagnostic code changed here, and this project's own
convention is to replay only the suites a code change actually affects, never the whole
integration suite for a documentation-only change.
