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

# The renderer is jq (preinstalled on GitHub-hosted runners). If it is missing
# we degrade to passing the raw stream through rather than failing: an ugly log
# is a far better outcome than a red unit-test job caused by the instrumentation
# that was supposed to be transparent.
if command -v jq >/dev/null 2>&1; then
  render() { jq -j --unbuffered 'select(.Action == "output") | .Output'; }
else
  echo "testbucket-capture: jq not found; emitting the raw -json stream" >&2
  render() { cat; }
fi

go test -json "$@" | tee -a "$events_file" | render
status=${PIPESTATUS[0]}

exit "$status"
