#!/usr/bin/env bash
#
# testbucket-capture_test.sh
#
# Regression tests for scripts/testbucket-capture.sh, the timing capture that
# Phase B attaches to the existing unit-test jobs.
#
# The guarded property (the whole reason these tests exist): the capture wraps
# `go test` in a pipeline, and a pipeline's exit status is its LAST command's.
# Without an explicit PIPESTATUS read, a failing test run would be reported
# through the renderer's happy exit and the unit-test job would go GREEN on a
# red suite. That is a fail-open in the one job whose entire purpose is to be
# red when the tests are. These tests pin the failure path first.
#
# Pure bash + the Go toolchain; no network.
set -uo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
capture="$here/testbucket-capture.sh"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

fails=0
ok()  { printf '  ok   %s\n' "$1"; }
bad() { printf '  FAIL %s\n' "$1"; fails=$((fails + 1)); }

# want/deny run a predicate and report it. Written as if/then rather than the
# shorter `pred && ok || bad`, which shellcheck rightly flags (SC2015): there
# the failure branch also runs whenever the SUCCESS branch fails, so a test
# harness written that way can report a pass and a fail for the same check.
want() { local label=$1; shift; if "$@"; then ok "$label"; else bad "$label"; fi; }
deny() { local label=$1; shift; if "$@"; then bad "$label"; else ok "$label"; fi; }

# A throwaway module with one passing and one failing test, so both paths are
# exercised against the real toolchain rather than a mock.
mkdir -p "$work/mod"
cat > "$work/mod/go.mod" <<'EOF'
module example.com/capture

go 1.26.5
EOF
cat > "$work/mod/pass_test.go" <<'EOF'
package capture

import "testing"

func TestPasses(t *testing.T) { t.Log("hello from the passing test") }
EOF
cat > "$work/mod/fail_test.go" <<'EOF'
package capture

import "testing"

func TestFails(t *testing.T) { t.Fatal("deliberately red") }
EOF

echo "== a green run stays green, prints a readable log, and captures events =="
events="$work/green.ndjson"
out=$( (cd "$work/mod" && GOWORK=off bash "$capture" "$events" -count=1 -run TestPasses ./...) 2>&1 )
status=$?
want "exit status 0 (got $status)" test "$status" -eq 0
want "log is human-readable (has an 'ok' line)" grep -q '^ok ' <<<"$out"
want "test output reached the log" grep -q 'hello from the passing test' <<<"$out"
deny "no raw NDJSON in the human log" grep -q '^{' <<<"$out"
want "events file written" test -s "$events"
want "events file holds go test -json events" grep -q '"Action":"pass"' "$events"

echo "== a RED run must exit non-zero: the pipeline must not mask it =="
events="$work/red.ndjson"
out=$( (cd "$work/mod" && GOWORK=off bash "$capture" "$events" -count=1 -run TestFails ./...) 2>&1 )
status=$?
want "exit status $status is non-zero (a zero here is a fail-open capture)" test "$status" -ne 0
want "the failure reason reached the log" grep -q 'deliberately red' <<<"$out"
want "the failure is in the event stream" grep -q '"Action":"fail"' "$events"

echo "== a build failure must exit non-zero too =="
cat > "$work/mod/broken_test.go" <<'EOF'
package capture

func Broken() { this is not go }
EOF
events="$work/broken.ndjson"
(cd "$work/mod" && GOWORK=off bash "$capture" "$events" -count=1 ./...) >/dev/null 2>&1
status=$?
want "exit status $status is non-zero" test "$status" -ne 0
rm -f "$work/mod/broken_test.go"

echo "== appending: two invocations accumulate into one events file =="
events="$work/both.ndjson"
(cd "$work/mod" && GOWORK=off bash "$capture" "$events" -count=1 -run TestPasses ./...) >/dev/null 2>&1
before=$(wc -l < "$events")
(cd "$work/mod" && GOWORK=off bash "$capture" "$events" -count=1 -run TestPasses ./...) >/dev/null 2>&1
after=$(wc -l < "$events")
want "second invocation appended ($before -> $after lines)" test "$after" -gt "$before"

echo "== the events directory is created on demand =="
events="$work/nested/deeper/events.ndjson"
(cd "$work/mod" && GOWORK=off bash "$capture" "$events" -count=1 -run TestPasses ./...) >/dev/null 2>&1
want "nested events path created" test -s "$events"

echo "== usage error without an events file =="
bash "$capture" >/dev/null 2>&1
status=$?
want "exit status 2 (got $status)" test "$status" -eq 2

if [ "$fails" -ne 0 ]; then
  printf '\n%d check(s) FAILED\n' "$fails"
  exit 1
fi
printf '\nall checks passed\n'
