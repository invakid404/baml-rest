#!/usr/bin/env bash
#
# run-bamlfuzz-native.sh
#
# Run the nightly native `go test -fuzz` invocation for ONE bamlfuzz target,
# teeing the FULL output to both the job log (so the #450 container-log dump
# from TestMain and all fuzz progress stay visible) and a file, then hand the
# result to bamlfuzz-boundary-guard.sh.
#
# The guard tolerates ONLY the Go fuzz coordinator's own -fuzztime boundary
# deadline (issue #526); every other failure shape — reproducible inputs,
# parity mismatches, real oracle deadlines, hangs — propagates unchanged.
#
# This wrapper is ONLY for the native `-fuzz` path. The Static and Streaming
# cells run their oracle under `-run` (no fuzz coordinator, so no #526 shape)
# and must keep invoking `go test` directly.
#
# Required env: BAMLFUZZ_FUNCTION (the fuzz target), FUZZTIME.
# Optional env: BAMLFUZZ_FUZZCACHEDIR (default: the in-tree .fuzzcache),
#               GO_TEST_TIMEOUT (default: 80m).
set -uo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${here}/.." && pwd)"

target="${BAMLFUZZ_FUNCTION:?BAMLFUZZ_FUNCTION must be set}"
fuzztime="${FUZZTIME:?FUZZTIME must be set}"
go_timeout="${GO_TEST_TIMEOUT:-80m}"
fuzzcachedir="${BAMLFUZZ_FUZZCACHEDIR:-${repo_root}/adapters/common/codegen/testdata/bamlfuzz/.fuzzcache}"
artifact_root="${repo_root}/adapters/common/codegen/testdata/bamlfuzz"

logfile="$(mktemp "${TMPDIR:-/tmp}/bamlfuzz-native.XXXXXX")"
echo "run-bamlfuzz-native: target=${target} fuzztime=${fuzztime} fuzzcachedir=${fuzzcachedir}"

# Tee everything to the job output AND the log the guard inspects. Capture the
# FULL pipe status: PIPESTATUS[0] is go test, PIPESTATUS[1] is tee.
set -x
go test -tags=integration,subprocess -v -count=1 -p 1 -timeout "${go_timeout}" \
  -run='^$' \
  -fuzz="^${target}\$" \
  -fuzztime="${fuzztime}" \
  ./integration \
  -test.fuzzcachedir="${fuzzcachedir}" 2>&1 | tee "${logfile}"
pipe=("${PIPESTATUS[@]}")
set +x
go_status="${pipe[0]}"
tee_status="${pipe[1]}"

echo "run-bamlfuzz-native: go test exited ${go_status}, tee exited ${tee_status}"

# Fail closed if tee failed: the log the guard inspects may be truncated or
# missing, so the guard cannot safely decide. Never hand a partial log to the
# guard — propagate a nonzero exit instead.
if [ "${tee_status}" -ne 0 ]; then
  echo "run-bamlfuzz-native: tee failed (status ${tee_status}); the captured log is unreliable — NOT invoking the boundary guard." >&2
  if [ "${go_status}" -ne 0 ]; then
    exit "${go_status}"
  fi
  exit 1
fi

exec "${here}/bamlfuzz-boundary-guard.sh" "${logfile}" "${go_status}" "${target}" "${fuzztime}" "${artifact_root}"
