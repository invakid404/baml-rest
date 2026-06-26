#!/usr/bin/env bash
#
# bamlfuzz-boundary-guard_test.sh
#
# Fixture tests for bamlfuzz-boundary-guard.sh. Runs the guard against saved
# `go test -fuzz` log snippets (scripts/testdata/bamlfuzz-guard/) and asserts
# the tolerate-vs-propagate decision for each. Pure bash; no Docker, no Go —
# runnable in CI directly.
#
# Each rejection fixture isolates a single failing condition (it otherwise
# carries the tolerable #526 boundary shape) so a regression in any one
# guard check is caught individually.
set -uo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
guard="${here}/bamlfuzz-boundary-guard.sh"
fixtures="${here}/testdata/bamlfuzz-guard"

pass=0
fail=0

# run_case <name> <logfile> <go_exit> <want_guard_exit> <target> <artifact_mode> [expect_loud]
#   artifact_mode: "empty" (clean dir)
#                | "with-artifact[:<subdir>]" (a <subdir>/_artifacts file exists;
#                                              subdir defaults to "dynamic")
#   artifact_mode "missing-root" -> point the guard at a nonexistent root
#   expect_loud:   non-empty -> require the tolerated BAMLFUZZ: line on STDOUT
#                  (stdout/stderr are captured separately so this enforces the
#                  guard's stdout-only contract for the loud tolerated line)
#   fuzztime (arg 8): configured fuzztime passed to the guard (default 15m)
run_case() {
  local name="$1" logf="$2" code="$3" want="$4" tgt="$5" artmode="$6" loud="${7:-}" fz="${8:-15m}"
  local artroot out errf outf got ok=1 subdir

  artroot="$(mktemp -d)"
  outf="$(mktemp)"
  errf="$(mktemp)"
  # Clean up this case's temp files/dir on return — even on a mid-run failure,
  # so the harness stays correct (and leak-free) if errexit is ever enabled.
  # shellcheck disable=SC2064  # expand artroot/outf/errf now, by design
  trap "rm -rf '$artroot' '$outf' '$errf'" RETURN

  case "$artmode" in
    missing-root)
      # Hand the guard a path that does not exist (F4 fail-closed check).
      rm -rf "$artroot"
      artroot="${artroot}/does-not-exist"
      ;;
    with-artifact*)
      subdir="${artmode#with-artifact}"
      subdir="${subdir#:}"
      subdir="${subdir:-dynamic}"
      mkdir -p "${artroot}/${subdir}/_artifacts"
      printf '{}' > "${artroot}/${subdir}/_artifacts/fuzz.json"
      ;;
  esac

  # Capture stdout and stderr SEPARATELY so the loud-line assertion can be
  # made against stdout only (the guard echoes the tolerated line to stdout;
  # propagate diagnostics go to stderr). Invoke as a non-aborting conditional
  # so a nonzero guard exit never trips errexit if it is ever enabled.
  if "$guard" "${fixtures}/${logf}" "$code" "$tgt" "$fz" "$artroot" >"$outf" 2>"$errf"; then
    got=0
  else
    got=$?
  fi

  [ "$got" -eq "$want" ] || ok=0
  if [ -n "$loud" ]; then
    grep -qF "BAMLFUZZ: tolerated Go native fuzz coordinator deadline" "$outf" || ok=0
  fi

  if [ "$ok" -eq 1 ]; then
    echo "ok   - ${name} (exit ${got})"
    pass=$((pass + 1))
  else
    echo "FAIL - ${name}: got exit ${got}, want ${want}"
    out="$(cat "$outf" "$errf")"
    while IFS= read -r diag_line; do
      echo "       | ${diag_line}"
    done <<<"$out"
    fail=$((fail + 1))
  fi
  # temp files/dir removed by the RETURN trap set above
}

# N2: an artifact_root containing an UNREADABLE subtree (with an _artifacts
# file inside) makes the find scan error. The guard must FAIL CLOSED (propagate
# nonzero), not silently conclude "no replay envelope" and tolerate.
#
# This is permission-based: under a uid that bypasses permissions (root, as in
# some container CI) chmod 000 does not make find error, so the scan-failure
# path is unreachable. We detect that and SKIP rather than emit a false pass —
# the assertion still runs in the normal (non-root) case, including GitHub's
# ubuntu-latest runners.
run_unreadable_subtree_case() {
  local name="$1"
  local artroot outf errf got ok=1
  artroot="$(mktemp -d)"
  outf="$(mktemp)"
  errf="$(mktemp)"
  # shellcheck disable=SC2064  # expand now, by design; chmod restores perms first
  trap "chmod -R u+rwx '$artroot' 2>/dev/null; rm -rf '$artroot' '$outf' '$errf'" RETURN

  mkdir -p "${artroot}/sub/dynamic/_artifacts"
  printf '{}' > "${artroot}/sub/dynamic/_artifacts/fuzz.json"
  chmod 000 "${artroot}/sub"

  # If find can still traverse the 000 subtree, permissions aren't enforced for
  # this uid — the scan-failure path can't be exercised, so skip.
  if find "${artroot}" -type d -name _artifacts -print >/dev/null 2>&1; then
    echo "skip - ${name} (find did not error; permissions not enforced, likely root)"
    return
  fi

  if "$guard" "${fixtures}/boundary_526.log" 1 FuzzBamlfuzzDynamic 15m "${artroot}" >"$outf" 2>"$errf"; then
    got=0
  else
    got=$?
  fi

  [ "$got" -ne 0 ] || ok=0   # MUST propagate (nonzero), never tolerate
  if [ "$ok" -eq 1 ]; then
    echo "ok   - ${name} (exit ${got})"
    pass=$((pass + 1))
  else
    echo "FAIL - ${name}: got exit ${got}, want nonzero (fail closed)"
    while IFS= read -r diag_line; do
      echo "       | ${diag_line}"
    done <<<"$(cat "$outf" "$errf")"
    fail=$((fail + 1))
  fi
}

# --- tolerate ---------------------------------------------------------------
run_case "green go test passes through"            green.log             0 0 FuzzBamlfuzzDynamic            empty
run_case "#526 boundary tolerated (Dynamic)"       boundary_526.log      1 0 FuzzBamlfuzzDynamic            empty loud
run_case "#526 boundary tolerated (Invalid cell)"  boundary_invalid.log  1 0 FuzzBamlfuzzInvalidJSONCoercion empty loud

# --- propagate: reproducible input / parity / oracle markers ---------------
run_case "reproducible crasher propagates"         failing_input.log     1 1 FuzzBamlfuzzDynamic            empty
run_case "replay: marker propagates"               replay.log            1 1 FuzzBamlfuzzDynamic            empty
run_case "write replay artifact: propagates"       write_replay.log      1 1 FuzzBamlfuzzDynamic            empty
run_case "dynclient errored: propagates"           dynclient_errored.log 1 1 FuzzBamlfuzzDynamic            empty
run_case "REST errored propagates"                 rest_errored.log      1 1 FuzzBamlfuzzDynamic            empty
run_case "register scenario: propagates"           register_scenario.log 1 1 FuzzBamlfuzzDynamic            empty
run_case "semantic mismatch (≠) propagates"        semantic_neq.log      1 1 FuzzBamlfuzzDynamic            empty
# Invalid-oracle harness/transport failure (invalid_test.go:370/403). The
# "harness failure:" marker must force propagation even when it surfaces at
# the boundary (near-boundary case where the worker result was not dropped).
run_case "harness-failure marker propagates"       harness_failure_marker.log 1 1 FuzzBamlfuzzInvalidDynamic empty

# --- propagate: budget / boundary-shape violations -------------------------
run_case "CDE before full budget propagates"       cde_before_budget.log 1 1 FuzzBamlfuzzDynamic            empty
run_case "missing (0/sec) shape propagates"        no_zerosec.log        1 1 FuzzBamlfuzzDynamic            empty
run_case "execs changed at boundary propagates"    execs_changed.log     1 1 FuzzBamlfuzzDynamic            empty
# F3: a progress line AFTER the (0/sec) line means fuzzing kept going — the
# (0/sec) was not the terminal completed-budget shape. Fail closed.
run_case "progress after (0/sec) propagates"       progress_after_zerosec.log 1 1 FuzzBamlfuzzDynamic       empty

# --- fractional / malformed fuzztime (Go-duration parser) ------------------
# Proven fail-open hole: FUZZTIME=15.5m (=930s) with elapsed 15m1s (=901s) —
# 901 < 930, the budget did NOT complete, so it must PROPAGATE. The old
# truncating parser read 15.5m as 300s and wrongly tolerated.
run_case "fractional fuzztime short run propagates" boundary_526.log    1 1 FuzzBamlfuzzDynamic            empty "" 15.5m
# No regression: integer fuzztime with elapsed exactly at / past budget.
run_case "15m vs 15m0s tolerates"                  boundary_15m0s.log   1 0 FuzzBamlfuzzDynamic            empty loud 15m
run_case "15m vs 15m1s tolerates"                  boundary_526.log     1 0 FuzzBamlfuzzDynamic            empty loud 15m
# Fractional fuzztime that genuinely completed: 14.5m (=870s) vs 15m1s (901s).
run_case "fractional fuzztime completed tolerates" boundary_526.log     1 0 FuzzBamlfuzzDynamic            empty loud 14.5m
# Malformed fuzztime must fail closed.
run_case "malformed fuzztime propagates"           boundary_526.log     1 1 FuzzBamlfuzzDynamic            empty "" 15x
# Negative fuzztime is meaningless / invalid -> fail closed (not parsed as +15m).
run_case "negative fuzztime propagates"            boundary_526.log     1 1 FuzzBamlfuzzDynamic            empty "" -15m

# --- propagate: structural / state violations ------------------------------
run_case "multiple FAIL lines propagate"           multi_fail.log        1 1 FuzzBamlfuzzDynamic            empty
run_case "FAIL for a different target propagates"  wrong_target.log      1 1 FuzzBamlfuzzDynamic            empty
run_case "replay artifact on disk propagates"      boundary_526.log      1 1 FuzzBamlfuzzDynamic            with-artifact
# The TRUE boundary-drop case for the invalid harness paths: the worker
# result (and any marker) is dropped, so the log is the pure #526 shape with
# NO oracle marker — but failAndDumpInvalid wrote an _artifacts file that
# survives on disk. The artifact check is the durable backstop here.
run_case "invalid artifact (boundary drop) propagates" boundary_invalid.log 1 1 FuzzBamlfuzzInvalidJSONCoercion with-artifact:invalid_json_coercion
# F4: a missing artifact root means we can't confirm "no replay envelope" —
# fail closed instead of falling through to tolerate.
run_case "missing artifact root propagates"        boundary_526.log      1 1 FuzzBamlfuzzDynamic            missing-root
# N2: an unreadable subtree makes the artifact scan error -> fail closed.
run_unreadable_subtree_case "unreadable artifact subtree propagates"
# F2: a non-1 go test exit code is propagated verbatim (not coerced to 1).
run_case "non-1 go test exit propagates"           no_zerosec.log        2 2 FuzzBamlfuzzDynamic            empty

echo "---"
echo "passed=${pass} failed=${fail}"
[ "$fail" -eq 0 ]
