#!/usr/bin/env bash
#
# bamlfuzz-boundary-guard.sh
#
# Decide whether a FAILED native `go test -fuzz` run is the benign Go fuzz
# *coordinator* -fuzztime boundary deadline (issue #526) and may be
# tolerated, or a real failure that must propagate.
#
# Background (issue #526): when `-fuzztime` elapses, Go's fuzz coordinator
# creates its own context.WithTimeout(ctx, fuzztime). If a worker is still
# inside / returning from an exec at the exact budget boundary, the
# coordinator can return context.DeadlineExceeded while blocked on the
# worker RPC. The testing package then prints that bare error and marks the
# fuzz target FAILED — even though no crasher input and no oracle replay
# envelope were produced. That is a Go-stdlib fuzzing boundary artifact, not
# a reproducible failure: there is no saved input to retry and no parity
# mismatch to investigate.
#
# This guard tolerates ONLY that exact shape. It is target-parameterized so
# it works for every native-fuzz cell (FuzzBamlfuzzDynamic,
# FuzzBamlfuzzInvalidDynamic, FuzzBamlfuzzInvalidJSONCoercion). It can NEVER
# mask:
#   * a reproducible input  -> rejected by `Failing input written to`
#   * a parity mismatch     -> rejected by replay/repro + semantic markers
#   * a real oracle deadline-> rejected by oracle-body error markers
#                              (those route through failAndDump, which always
#                               prints repro:/replay:)
#   * a hang / deadlock     -> never reaches the completed-budget boundary
#                              shape (unchanged execs + (0/sec) at fuzztime);
#                              `go test -timeout` / the job timeout still fire
#
# Usage:
#   bamlfuzz-boundary-guard.sh <logfile> <go_test_exit_code> <target> <fuzztime> [artifact_root]
#
# Exit 0  -> go test passed, OR the failure is the tolerable #526 boundary
#            (a loud BAMLFUZZ: line is printed in that case).
# Exit !0 -> a real failure; the original go-test exit code is propagated.
#
# Kept as a standalone script (not inline workflow shell) so it is testable
# against fixed sample logs — see bamlfuzz-boundary-guard_test.sh.
set -uo pipefail

log="${1:?logfile required}"
exit_code="${2:?go test exit code required}"
target="${3:?target required}"
fuzztime="${4:?fuzztime required}"
artifact_root="${5:-adapters/common/codegen/testdata/bamlfuzz}"

# A passing run is nothing to tolerate.
if [ "$exit_code" -eq 0 ]; then
  exit 0
fi

# Refuse to tolerate: surface the reason on stderr and propagate the
# original go-test exit code so the CI step fails on the real failure.
propagate() {
  echo "BAMLFUZZ-GUARD: NOT tolerating native-fuzz failure (${1}); propagating exit ${exit_code}." >&2
  exit "$exit_code"
}

if [ ! -f "$log" ]; then
  propagate "log file ${log} is missing"
fi

# ---------------------------------------------------------------------------
# (1) Exactly one test failed, and it is our target. A second FAIL line (or a
#     FAIL for some other test) means more than the coordinator boundary went
#     wrong — never tolerate that. This also enforces "suppress at most one
#     boundary error per invocation".
# ---------------------------------------------------------------------------
fail_count="$(grep -c -- '--- FAIL: ' "$log" || true)"
if [ "${fail_count:-0}" -ne 1 ]; then
  propagate "expected exactly one '--- FAIL:' line, found ${fail_count:-0}"
fi
if ! grep -qF -- "--- FAIL: ${target} (" "$log"; then
  propagate "the single FAIL line is not for target ${target}"
fi

# ---------------------------------------------------------------------------
# (2) A BARE 'context deadline exceeded' line — the coordinator's own printed
#     error (testing/fuzz.go prints the non-crasher error verbatim on its own
#     line). An oracle-body deadline is embedded inside a longer message
#     (e.g. "dynclient errored: ... context deadline exceeded") and is caught
#     by the marker scan below; a whole-line match here distinguishes the two.
# ---------------------------------------------------------------------------
if ! grep -qxE '[[:space:]]*context deadline exceeded[[:space:]]*' "$log"; then
  propagate "no bare 'context deadline exceeded' line"
fi

# ---------------------------------------------------------------------------
# (3) No oracle-body / replay / crasher markers anywhere. Each marker means a
#     real, attributable failure that MUST surface. failAndDump and
#     failAndDumpInvalid (integration/bamlfuzz_dynamic_test.go,
#     integration/bamlfuzz_invalid_test.go) always print `repro:` and either
#     `replay:` or `write replay artifact:`, so those three alone catch every
#     oracle-body failure for all native-fuzz targets; the rest are
#     defense-in-depth against the specific failure texts.
# ---------------------------------------------------------------------------
reject_markers=(
  'Failing input written to'   # Go fuzz wrote a reproducible crasher input
  'repro:'                     # every failAndDump / failAndDumpInvalid path prints this
  'replay:'                    # ditto (artifact write succeeded)
  'write replay artifact:'     # ditto (artifact write failed)
  'dynclient errored:'         # oracle-body dynclient error
  'REST errored'               # oracle-body REST error ("REST errored (status N): ...")
  'register scenario:'         # mock scenario registration error
  'schema key order mismatch'  # preserve-order parity failure
  'schema order'               # schema-order unsupported / error
  ' diff:'                     # semantic-diff decode error (expected_vs_* diff:)
  '≠'                          # semantic mismatch (expected ≠ dynclient, dynclient ≠ REST, ...)
)
for m in "${reject_markers[@]}"; do
  if grep -qF -- "$m" "$log"; then
    propagate "log contains oracle/crasher marker: ${m}"
  fi
done

# ---------------------------------------------------------------------------
# (4) The fuzz budget actually COMPLETED in the boundary shape: a
#     'fuzz: elapsed:' progress line at/after the configured fuzztime, with
#     (0/sec) and an exec count unchanged from the prior progress line. A real
#     per-input stall or a hang never reaches this completed-budget shape.
# ---------------------------------------------------------------------------
elapsed_lines=()
while IFS= read -r line; do
  elapsed_lines+=("$line")
done < <(grep -E 'fuzz: elapsed:' "$log" || true)

if [ "${#elapsed_lines[@]}" -lt 2 ]; then
  propagate "fewer than two 'fuzz: elapsed:' progress lines"
fi

# Convert a Go duration string (e.g. 15m, 15m0s, 15m1s, 1h0m0s, 30s) to
# whole seconds. Fractional seconds are floored; only integer progress
# durations appear on `fuzz: elapsed:` lines.
dur_to_secs() {
  local d="$1" total=0
  [[ "$d" =~ ([0-9]+)h ]] && total=$(( total + BASH_REMATCH[1] * 3600 ))
  [[ "$d" =~ ([0-9]+)m ]] && total=$(( total + BASH_REMATCH[1] * 60 ))
  [[ "$d" =~ ([0-9]+)s ]] && total=$(( total + BASH_REMATCH[1] ))
  echo "$total"
}
exec_of()    { sed -E 's/.*execs: ([0-9]+).*/\1/' <<<"$1"; }
elapsed_of() { sed -E 's/.*elapsed: ([^,]+),.*/\1/' <<<"$1"; }

# Last progress line carrying the (0/sec) boundary marker.
boundary_idx=-1
for i in "${!elapsed_lines[@]}"; do
  if [[ "${elapsed_lines[$i]}" == *"(0/sec)"* ]]; then
    boundary_idx="$i"
  fi
done
if [ "$boundary_idx" -lt 1 ]; then
  propagate "no '(0/sec)' boundary progress line preceded by another progress line"
fi

boundary_line="${elapsed_lines[$boundary_idx]}"
prev_line="${elapsed_lines[$((boundary_idx - 1))]}"
b_exec="$(exec_of "$boundary_line")"
p_exec="$(exec_of "$prev_line")"
if [ "$b_exec" != "$p_exec" ]; then
  propagate "exec count changed at the (0/sec) boundary (${p_exec} -> ${b_exec}); not a completed-budget stall"
fi

want_secs="$(dur_to_secs "$fuzztime")"
have_secs="$(dur_to_secs "$(elapsed_of "$boundary_line")")"
if [ "${want_secs:-0}" -le 0 ]; then
  propagate "could not parse fuzztime '${fuzztime}'"
fi
if [ "${have_secs:-0}" -lt "$want_secs" ]; then
  propagate "boundary elapsed ${have_secs}s < configured fuzztime ${want_secs}s; budget did not complete"
fi

# ---------------------------------------------------------------------------
# (5) No replay artifact was written under any <target>/_artifacts/ dir. The
#     workflow uploads these on failure; their absence confirms no oracle case
#     produced an envelope (parity mismatches always write one before failing).
# ---------------------------------------------------------------------------
if [ -d "$artifact_root" ]; then
  while IFS= read -r d; do
    [ -n "$d" ] || continue
    if [ -n "$(find "$d" -type f -print -quit 2>/dev/null)" ]; then
      propagate "replay artifact present under ${d}"
    fi
  done < <(find "$artifact_root" -type d -name _artifacts 2>/dev/null || true)
fi

# All conditions held: this is the issue #526 coordinator-boundary artifact.
echo "BAMLFUZZ: tolerated Go native fuzz coordinator deadline at fuzztime boundary; no crasher or replay envelope produced; see issue #526."
exit 0
