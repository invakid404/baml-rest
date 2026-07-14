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
printf 'require %s v0.3.2\n' "${NANOLLM_PREFIX}/go" >"${tmp_manifest}"
assert_rc "manifest: nanollm reference detected" 0 manifest_has_ref "${NANOLLM_PREFIX}" "${tmp_manifest}"
printf 'require github.com/boundaryml/baml v0.223.0\n' >"${tmp_manifest}"
assert_rc "manifest: clean file is clean"        1 manifest_has_ref "${NANOLLM_PREFIX}" "${tmp_manifest}"
rm -f "${tmp_manifest}"
# grep error path: a nonexistent path makes grep exit 2 (not 1) on both GNU and
# BSD grep -> must fail closed (rc 0), NOT be read as "no match" (rc 1). This is
# the exact fail-open a bare `if grep -q PATTERN FILE` would have.
assert_rc "manifest: grep error fails closed"    0 manifest_has_ref "${NANOLLM_PREFIX}" "/nonexistent/host-zero-check/does-not-exist-$$" 2>/dev/null

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
