#!/bin/sh
# Keep cleanup outside the Go test process: testing's timeout skips t.Cleanup.
set -u

cleanup() {
    : "${ASQ_TEST_RUN_ID:?set ASQ_TEST_RUN_ID to the run to clean}"
    ids=$(podman ps -aq --filter "label=io.argosql.test=$ASQ_TEST_RUN_ID") || return
    if [ -n "$ids" ]; then
        podman rm --force --time 0 $ids
    fi
}

if [ "${1:-}" = --cleanup ]; then
    cleanup
    exit $?
fi

if [ -z "${ASQ_TEST_RUN_ID:-}" ]; then
    run_dir=$(mktemp -d) || exit 1
    ASQ_TEST_RUN_ID="asq-${run_dir##*/}"
    rmdir "$run_dir" || exit 1
fi
export ASQ_TEST_RUN_ID
printf 'integration run: %s\n' "$ASQ_TEST_RUN_ID"
finish() {
    status=$?
    trap - EXIT
    cleanup || { [ "$status" -ne 0 ] || status=1; }
    exit "$status"
}
trap finish EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
"$@"
