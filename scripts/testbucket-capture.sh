#!/usr/bin/env bash
# testbucket-capture.sh — run one `go test` invocation, keep its machine-readable
# event stream for `cmd/testbucket ingest`, and still print a readable test log.
#
# Usage:
#   scripts/testbucket-capture.sh <events-file> [go test args...]
#
# The whole point is that adding timing capture must not make a failing CI job
# harder to read. `go test` has no dual-output mode: with -json the console
# gets NDJSON instead of test output, which would be a real regression for
# anyone debugging a failure. So the stream is tee'd to the events file and
# then rendered back to human form by replaying its `output` events, which is
# byte-identical to what plain `go test` would have printed.
#
# The exit status is `go test`'s own, not the renderer's — a green renderer
# must never mask a red test run.
set -uo pipefail

if [ "$#" -lt 1 ]; then
  echo "usage: $0 <events-file> [go test args...]" >&2
  exit 2
fi

events_file=$1
shift

mkdir -p "$(dirname "$events_file")"

# Rendering lives in testbucket-render.sh so the bucketed workflow's test job,
# which runs whole plan-emitted scripts rather than single invocations, replays
# its stream exactly the same way.
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

go test -json "$@" | tee -a "$events_file" | bash "$here/testbucket-render.sh"
status=${PIPESTATUS[0]}

exit "$status"
