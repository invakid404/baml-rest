#!/usr/bin/env bash
# testbucket-render.sh — replay a `go test -json` stream on stdin as the plain
# test log `go test` would have printed.
#
# `go test` has no dual-output mode: with -json the console gets NDJSON instead
# of test output, which would be a real regression for anyone reading a failing
# CI job. Both timing-capture paths therefore keep the machine-readable stream
# for `testbucket ingest` and pipe it through here for humans:
#
#   * scripts/testbucket-capture.sh, which wraps one `go test` invocation, and
#   * the bucketed workflow's test job, which runs a whole plan-emitted script
#     whose invocations already tee their own events.
#
# Reading only `output` events reproduces the original log byte for byte,
# because that is exactly what those events carry.
#
# This filter never changes an exit status: callers are responsible for taking
# the status of the command on the LEFT of the pipe (PIPESTATUS[0]), because a
# happy renderer must never make a red test run look green.
set -uo pipefail

if command -v jq >/dev/null 2>&1; then
  exec jq -j --unbuffered 'select(.Action == "output") | .Output'
fi

# Without jq the raw stream is ugly but complete. Degrading beats failing: this
# is instrumentation, and it must not be the reason a test job goes red.
echo "testbucket-render: jq not found; passing the raw -json stream through" >&2
exec cat
