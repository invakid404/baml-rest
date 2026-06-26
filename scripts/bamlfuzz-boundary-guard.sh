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
  'harness failure:'           # invalid-oracle harness/transport failure (now also dumps an artifact)
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

# Parse a full Go duration string to integer NANOSECONDS, supporting
# FRACTIONAL units (e.g. 15.5m, 59.9s, 0.5h, 1h2.5m) as well as the integer
# forms Go fuzz prints (15m, 15m0s, 15m1s, 1h0m0s, 30s). Prints the ns value
# and exits 0; exits 1 on an empty, malformed, or partially-consumed string so
# the caller can fail closed. Go durations are integer nanoseconds and stay
# well under 2^53, so the double math is exact for any realistic fuzztime.
# A whole-string parser is required: the previous unanchored h/m/s substring
# match mis-read 15.5m as 300s, which fail-OPEN tolerated a short run (#526
# follow-up). awk handles the float math portably (no bc dependency).
parse_go_duration_ns() {
  awk -v s="$1" '
    BEGIN {
      if (s == "") exit 1
      c = substr(s, 1, 1)
      if (c == "+" || c == "-") s = substr(s, 2)
      if (s == "") exit 1
      total = 0.0; found = 0
      while (length(s) > 0) {
        if (match(s, /^([0-9]+(\.[0-9]*)?|\.[0-9]+)/) == 0) exit 1
        num = substr(s, 1, RLENGTH) + 0
        s = substr(s, RLENGTH + 1)
        if      (substr(s, 1, 2) == "ns") { mult = 1;      s = substr(s, 3) }
        else if (substr(s, 1, 2) == "us") { mult = 1e3;    s = substr(s, 3) }
        else if (substr(s, 1, 2) == "ms") { mult = 1e6;    s = substr(s, 3) }
        else if (substr(s, 1, 1) == "s")  { mult = 1e9;    s = substr(s, 2) }
        else if (substr(s, 1, 1) == "m")  { mult = 60e9;   s = substr(s, 2) }
        else if (substr(s, 1, 1) == "h")  { mult = 3600e9; s = substr(s, 2) }
        else exit 1
        total += num * mult; found = 1
      }
      if (!found) exit 1
      printf "%.0f\n", total
    }'
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
# The (0/sec) line MUST be the terminal progress line: any further
# 'fuzz: elapsed:' line after it means fuzzing kept going, so this was not the
# completed-budget shutdown shape. Fail closed.
if [ "$boundary_idx" -ne $(( ${#elapsed_lines[@]} - 1 )) ]; then
  propagate "(0/sec) line at index ${boundary_idx} is not the final progress line ($(( ${#elapsed_lines[@]} - 1 ))); fuzzing continued after it"
fi

boundary_line="${elapsed_lines[$boundary_idx]}"
prev_line="${elapsed_lines[$((boundary_idx - 1))]}"
b_exec="$(exec_of "$boundary_line")"
p_exec="$(exec_of "$prev_line")"
if [ "$b_exec" != "$p_exec" ]; then
  propagate "exec count changed at the (0/sec) boundary (${p_exec} -> ${b_exec}); not a completed-budget stall"
fi

elapsed_tok="$(elapsed_of "$boundary_line")"
# Parse both to exact nanoseconds; a parse failure fails closed (propagate),
# never silently zero. Comparing in nanoseconds (not truncated seconds) keeps
# a fractional fuzztime like 15.5m from fail-OPEN tolerating a 15m1s run.
if ! want_ns="$(parse_go_duration_ns "$fuzztime")"; then
  propagate "could not parse configured fuzztime '${fuzztime}'"
fi
if ! have_ns="$(parse_go_duration_ns "$elapsed_tok")"; then
  propagate "could not parse boundary elapsed '${elapsed_tok}'"
fi
if [ "$want_ns" -le 0 ]; then
  propagate "configured fuzztime '${fuzztime}' is non-positive"
fi
if [ "$have_ns" -lt "$want_ns" ]; then
  propagate "boundary elapsed ${elapsed_tok} (${have_ns}ns) < configured fuzztime ${fuzztime} (${want_ns}ns); budget did not complete"
fi

# ---------------------------------------------------------------------------
# (5) No replay artifact was written under any <target>/_artifacts/ dir. The
#     workflow uploads these on failure; their absence confirms no oracle case
#     produced an envelope (parity mismatches always write one before failing).
#     A missing artifact root means we cannot make that determination, so fail
#     closed — do NOT fall through to tolerate. Preexisting _artifacts subdirs
#     are NOT required: the failure writers create them on demand.
# ---------------------------------------------------------------------------
if [ ! -d "$artifact_root" ]; then
  propagate "artifact root ${artifact_root} is missing; cannot confirm no replay envelope"
fi
while IFS= read -r d; do
  [ -n "$d" ] || continue
  if [ -n "$(find "$d" -type f -print -quit 2>/dev/null)" ]; then
    propagate "replay artifact present under ${d}"
  fi
done < <(find "$artifact_root" -type d -name _artifacts 2>/dev/null || true)

# All conditions held: this is the issue #526 coordinator-boundary artifact.
echo "BAMLFUZZ: tolerated Go native fuzz coordinator deadline at fuzztime boundary; no crasher or replay envelope produced; see issue #526."
exit 0
