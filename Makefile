.PHONY: test race vet build integration-2019 integration-2022 smoke-2025 integration-cleanup

DIST := dist

test:
	go test ./...

race:
	go test -race ./...

vet:
	go vet ./...
	go vet -tags=integration ./...

build:
	mkdir -p $(DIST)
	CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -trimpath -o $(DIST)/asq-linux-amd64     ./cmd/asq
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -o $(DIST)/asq-windows-amd64.exe ./cmd/asq

# The three targets below each run tests/integration (build tag
# "integration") against one locally present SQL Server image. None of
# them pulls: tests/integration/podman_test.go fails explicitly when
# ASQ_TEST_IMAGE names an image podman does not already have, rather than
# reaching the network on a machine that may have none. Pull the image
# yourself first if it is not already local:
#
#   podman pull mcr.microsoft.com/mssql/server:2022-latest
#
# Run one target at a time. Each one starts its own disposable
# containers and removes them on exit; see docs/testing.md for the
# memory-bounded sliced alternative when the full suite does not fit in
# one go test invocation.
integration-2019:
	ASQ_TEST_IMAGE=mcr.microsoft.com/mssql/server:2019-latest \
	  sh tests/integration/run.sh go test ./tests/integration -tags=integration -count=1 -timeout=15m -v

integration-2022:
	ASQ_TEST_IMAGE=mcr.microsoft.com/mssql/server:2022-latest \
	  sh tests/integration/run.sh go test ./tests/integration -tags=integration -count=1 -timeout=15m -v

# SQL Server 2025 is not part of the validated release matrix (CLAUDE.md):
# this runs only the reference end-to-end workflow (TestWorkflow), never
# the full matrix, TLS or fault-injection suites that 2019 and 2022 carry.
# Passing this alone does not establish release support for 2025.
smoke-2025:
	ASQ_TEST_IMAGE=mcr.microsoft.com/mssql/server:2025-latest \
	  sh tests/integration/run.sh go test ./tests/integration -tags=integration -run '^TestWorkflow$$' -count=1 -timeout=15m -v

# Requires the exact ID printed by the wrapper, never a container name.
integration-cleanup:
	sh tests/integration/run.sh --cleanup
