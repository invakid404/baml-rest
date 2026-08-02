#!/usr/bin/env bash
#
# check-host-zero-nanollm_test.sh
#
# Regression tests for the nanollm-match helpers in check-host-zero-nanollm.sh.
# Pure bash; no Docker, no Go — runnable in CI directly.
#
# The guarded scenario (the whole reason these tests exist): a forbidden nanollm
# reference that appears EARLY in a value larger than a pipe buffer. The previous
# `printf … | grep -q` form let `grep -q` exit on the early match and SIGPIPE the
# `printf`; under `set -o pipefail` the pipeline status went non-zero and the
# `if` MISSED the match — a fail-open in the host-isolation gate. These tests
# assert the current here-string helpers detect it, and demonstrate that the old
# pipeline form did not.

set -uo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# Source the guard for its helpers only (the BASH_SOURCE guard keeps main() from
# running). This also sets `set -o pipefail`, so these tests exercise the exact
# shell mode the SIGPIPE bug depended on.
# shellcheck source=/dev/null
source "${here}/check-host-zero-nanollm.sh"
set +e            # let the harness capture helper return codes itself
set -o pipefail   # keep pipefail on — it is what the regression turns on

failed=0

# A nanollm reference followed by WAY more than a pipe buffer (~64 KiB) of
# trailing data. This is the exact shape that SIGPIPE-flipped the old check.
big_filler="$(head -c 200000 </dev/zero | tr '\0' 'x')"
big_syms="nanollm_startup_symbol"$'\n'"${big_filler}"
big_deps="${NANOLLM_PREFIX}"$'\n'"${big_filler}"

# assert_rc <name> <want-rc> <fn> <args...>
#   want-rc 0 = helper must report a match/error (gate would FAIL); 1 = clean
assert_rc() {
    local name="$1" want="$2" fn="$3" got=0
    shift 3
    "$fn" "$@" || got=$?
    if [ "${got}" -eq "${want}" ]; then
        echo "ok:   ${name}"
    else
        echo "FAIL: ${name} (want rc=${want}, got rc=${got})"
        failed=1
    fi
}

# --- clean inputs report clean (rc 1) --------------------------------------
assert_rc "graph: clean input is clean"        1 graph_has_nanollm   "github.com/boundaryml/baml v0.223.0"
assert_rc "symbols: clean input is clean"      1 symbols_have_nanollm "$(head -c 100000 </dev/zero | tr '\0' 'q')"

# --- present references are detected (rc 0) --------------------------------
assert_rc "graph: prefix detected"             0 graph_has_nanollm   "x ${NANOLLM_PREFIX}/go y"
assert_rc "symbols: literal detected"          0 symbols_have_nanollm "_cgo_nanollm_version"
assert_rc "symbols: case-insensitive detected" 0 symbols_have_nanollm "NANOLLM_ffi_init"

# --- THE regression: early match + >64 KiB trailing data is still detected --
assert_rc "graph: early match + big trailing detected"   0 graph_has_nanollm   "${big_deps}"
assert_rc "symbols: early match + big trailing detected" 0 symbols_have_nanollm "${big_syms}"

# --- root-manifest scan: fail-closed on match AND on grep error -------------
# manifest_has_ref reads a FILE; besides match/clean it must treat a grep error
# (exit >1) as a failure, so a `grep -q` that errors cannot read as "no match".
tmp_manifest="$(mktemp)"
printf 'require %s v0.4.3\n' "${NANOLLM_PREFIX}/go" >"${tmp_manifest}"
assert_rc "manifest: nanollm reference detected" 0 manifest_has_ref "${NANOLLM_PREFIX}" "${tmp_manifest}"
printf 'require github.com/boundaryml/baml v0.223.0\n' >"${tmp_manifest}"
assert_rc "manifest: clean file is clean"        1 manifest_has_ref "${NANOLLM_PREFIX}" "${tmp_manifest}"
rm -f "${tmp_manifest}"
# grep error path: a nonexistent path makes grep exit 2 (not 1) on both GNU and
# BSD grep -> must fail closed (rc 0), NOT be read as "no match" (rc 1). This is
# the exact fail-open a bare `if grep -q PATTERN FILE` would have.
assert_rc "manifest: grep error fails closed"    0 manifest_has_ref "${NANOLLM_PREFIX}" "/nonexistent/host-zero-check/does-not-exist-$$" 2>/dev/null

# --- graph_has_ref: the THREE-valued presence/absence helper used by check 5 --
# Same here-string contract as graph_has_nanollm, so it inherits the same
# match-position independence; these assertions pin all three statuses plus the
# early-match-with-large-trailing-data shape the whole file exists for.
assert_rc "graph_has_ref: present pattern detected" 0 graph_has_ref \
    "github.com/invakid404/baml-rest/internal/bamlprofile" \
    "github.com/invakid404/baml-rest/internal/nativeprompt"$'\n'"github.com/invakid404/baml-rest/internal/bamlprofile"
assert_rc "graph_has_ref: absent pattern is clean"   1 graph_has_ref \
    "github.com/mitsuhiko/minijinja" \
    "github.com/invakid404/minijinja-go/v2"$'\n'"github.com/invakid404/baml-rest/internal/bamlprofile"
assert_rc "graph_has_ref: early match + big trailing detected" 0 graph_has_ref \
    "github.com/boundaryml/baml" \
    "github.com/boundaryml/baml"$'\n'"${big_filler}"
# A near-miss must NOT match: the fork path must not satisfy a check for the
# retired pre-fork engine (they share no prefix, but pin it so a future
# substring-y pattern edit is caught).
assert_rc "graph_has_ref: fork is not the retired engine" 1 graph_has_ref \
    "github.com/mitsuhiko/minijinja/minijinja-go/v2" \
    "github.com/invakid404/minijinja-go/v2"

# --- check 5 verdict: MUTATION tests of render_path_violation ----------------
# The presence half of check 5 has no negative evidence unless something proves
# it can FAIL. These feed render_path_violation synthetic dependency listings —
# the healthy one, then one mutation per way the render path could go wrong — so
# a future edit that makes the check unfalsifiable (as naming ./internal/
# bamlprofile on the `go list` command line once did) is caught here.
#
# render_path_violation echoes its reason; silence it so the harness output stays
# one line per assertion.
render_path_violation_quiet() { render_path_violation "$1" >/dev/null; }

healthy_deps="github.com/invakid404/baml-rest/internal/nativeprompt
github.com/invakid404/baml-rest/internal/bamlprofile
github.com/invakid404/minijinja-go/v2
github.com/invakid404/minijinja-go/v2/value
strings"

# Mutations are built with the REAL grep, before the mocked-error block below.
deps_without_profile="$(grep -Fv 'baml-rest/internal/bamlprofile' <<<"${healthy_deps}")"
deps_without_fork="$(grep -Fv 'invakid404/minijinja-go' <<<"${healthy_deps}")"

assert_rc "render_path: healthy listing is clean"            1 render_path_violation_quiet "${healthy_deps}"
assert_rc "render_path: missing bamlprofile edge is caught"  0 render_path_violation_quiet "${deps_without_profile}"
assert_rc "render_path: missing minijinja fork is caught"    0 render_path_violation_quiet "${deps_without_fork}"
assert_rc "render_path: stock BAML is caught"                0 render_path_violation_quiet \
    "${healthy_deps}"$'\n'"github.com/boundaryml/baml/engine/language_client_go/baml_go"
assert_rc "render_path: retired pre-fork engine is caught"   0 render_path_violation_quiet \
    "${healthy_deps}"$'\n'"github.com/mitsuhiko/minijinja/minijinja-go/v2"
assert_rc "render_path: nanollm is caught"                   0 render_path_violation_quiet \
    "${healthy_deps}"$'\n'"${NANOLLM_PREFIX}/go"
assert_rc "render_path: profileoracle is caught"             0 render_path_violation_quiet \
    "${healthy_deps}"$'\n'"github.com/invakid404/baml-rest/internal/bamlprofile/profileoracle"

# --- a grep EXECUTION error must fail BOTH halves of check 5 -----------------
# graph_has_ref is the one helper used for REQUIRED references too, so it cannot
# collapse "errored" into "present" the way the forbidden-only helpers do: that
# would make a broken scan look like the required edge is there. Shadow grep with
# a function that exits 2 (a real grep error status) and assert the distinct rc 2
# propagates into a gate failure.
#
# render_path_violation has TWO `*` (scan-errored) arms, one per loop, and they
# must be exercised SEPARATELY: a blanket mock errors on the first required
# package and returns before the forbidden loop ever runs, so it proves nothing
# about the forbidden arm. Hence a blanket mock below, then a SELECTIVE one.
#
# assert_reason additionally pins WHICH arm fired. Without it both mutations
# would just be "rc 0", indistinguishable from any other violation, and the
# forbidden-arm test could pass while never leaving the required loop.
assert_reason() {
    local name="$1" want_rc="$2" needle="$3" deps="$4" got=0 out
    out="$(render_path_violation "${deps}")" || got=$?
    if [ "${got}" -eq "${want_rc}" ] && [[ "${out}" == *"${needle}"* ]]; then
        echo "ok:   ${name}"
    else
        echo "FAIL: ${name} (want rc=${want_rc} reason containing '${needle}'; got rc=${got} reason='${out}')"
        failed=1
    fi
}

# (a) blanket error -> the REQUIRED loop's `*` arm fires first.
grep() { return 2; }
assert_rc     "graph_has_ref: scan error is its own status (2)" 2 graph_has_ref "anything" "irrelevant"
assert_reason "render_path: scan error fails the required half" 0 \
    "the scan for required package github.com/invakid404/baml-rest/internal/bamlprofile errored" \
    "${healthy_deps}"
unset -f grep

# (b) SELECTIVE error -> every required package resolves normally, so the
# required loop completes, and only a FORBIDDEN pattern errors. This is the only
# way to reach the second `*` arm. `command grep` bypasses the shadowing function
# for every other pattern; graph_has_ref passes the pattern as the last argument
# and the subject on stdin, both of which pass straight through.
grep() {
    local pat="${@: -1}"
    if [ "${pat}" = "github.com/boundaryml/baml" ]; then
        return 2 # simulate a grep execution failure for ONE forbidden pattern
    fi
    command grep "$@"
}
# Sanity: with the selective mock in place a required package still resolves, so
# a failure below really is the forbidden arm and not a broken mock.
assert_rc     "graph_has_ref: selective mock leaves required lookups intact" 0 graph_has_ref \
    "github.com/invakid404/baml-rest/internal/bamlprofile" "${healthy_deps}"
assert_rc     "graph_has_ref: selective mock errors on the chosen pattern"   2 graph_has_ref \
    "github.com/boundaryml/baml" "${healthy_deps}"
assert_reason "render_path: scan error fails the forbidden half" 0 \
    "the scan for forbidden package github.com/boundaryml/baml errored" \
    "${healthy_deps}"
unset -f grep

# The real grep must be back, or every assertion after this point is meaningless.
assert_rc "graph_has_ref: real grep restored after the mock"      1 graph_has_ref "absent-pattern" "clean input"

# --- documentation: the OLD pipeline form fails open on the same input ------
# Run the retired construct so a future refactor back to it is visibly wrong.
# Informational: not counted toward pass/fail (its miss depends on pipe-buffer
# size), but it should print MISSED on any buffer < 200 KiB.
old_form_rc=0
if printf '%s\n' "${big_syms}" | grep -qi nanollm; then old_form_rc=0; else old_form_rc=1; fi
if [ "${old_form_rc}" -ne 0 ]; then
    echo "note: retired 'printf | grep -qi' form MISSED the early match (rc=${old_form_rc}) — the fail-open this fix removes"
else
    echo "note: retired 'printf | grep -qi' form happened to catch it here (buffer >= 200 KiB); the here-string form is match-position-independent regardless"
fi

echo "---"
if [ "${failed}" -eq 0 ]; then
    echo "PASS: nanollm-match helpers fail closed on matches (incl. large trailing data)"
else
    echo "FAIL: one or more match-helper assertions failed"
fi
[ "${failed}" -eq 0 ]
