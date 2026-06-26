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
#   expect_loud:   non-empty -> require the tolerated BAMLFUZZ: line on stdout
run_case() {
  local name="$1" logf="$2" code="$3" want="$4" tgt="$5" artmode="$6" loud="${7:-}"
  local artroot out got ok=1 subdir

  artroot="$(mktemp -d)"
  case "$artmode" in
    with-artifact*)
      subdir="${artmode#with-artifact}"
      subdir="${subdir#:}"
      subdir="${subdir:-dynamic}"
      mkdir -p "${artroot}/${subdir}/_artifacts"
      printf '{}' > "${artroot}/${subdir}/_artifacts/fuzz.json"
      ;;
  esac

  out="$("$guard" "${fixtures}/${logf}" "$code" "$tgt" "15m" "$artroot" 2>&1)"
  got=$?
  rm -rf "$artroot"

  [ "$got" -eq "$want" ] || ok=0
  if [ -n "$loud" ]; then
    grep -qF "BAMLFUZZ: tolerated Go native fuzz coordinator deadline" <<<"$out" || ok=0
  fi

  if [ "$ok" -eq 1 ]; then
    echo "ok   - ${name} (exit ${got})"
    pass=$((pass + 1))
  else
    echo "FAIL - ${name}: got exit ${got}, want ${want}"
    while IFS= read -r diag_line; do
      echo "       | ${diag_line}"
    done <<<"$out"
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

# --- propagate: structural / state violations ------------------------------
run_case "multiple FAIL lines propagate"           multi_fail.log        1 1 FuzzBamlfuzzDynamic            empty
run_case "FAIL for a different target propagates"  wrong_target.log      1 1 FuzzBamlfuzzDynamic            empty
run_case "replay artifact on disk propagates"      boundary_526.log      1 1 FuzzBamlfuzzDynamic            with-artifact
# The TRUE boundary-drop case for the invalid harness paths: the worker
# result (and any marker) is dropped, so the log is the pure #526 shape with
# NO oracle marker — but failAndDumpInvalid wrote an _artifacts file that
# survives on disk. The artifact check is the durable backstop here.
run_case "invalid artifact (boundary drop) propagates" boundary_invalid.log 1 1 FuzzBamlfuzzInvalidJSONCoercion with-artifact:invalid_json_coercion

echo "---"
echo "passed=${pass} failed=${fail}"
[ "$fail" -eq 0 ]
