#!/usr/bin/env bash
#
# check-host-zero-nanollm.sh — de-BAML cutover Slice 2 host-isolation guard.
#
# Asserts the invariant that survives even when the distributed worker embeds a
# nanollm-linked payload (owner-settled option b): the HOST Go link graph and
# host process contain NO nanollm and require NO CGO. nanollm links ONLY inside
# the worker subprocess, built separately from the out-of-go.work
# internal/nativebody/nanollmprepare module.
#
# It proves five things, none of which depend on the embedded worker bytes:
#
#   1. Root module/workspace files (go.mod, go.sum, go.work, go.work.sum)
#      reference neither nanollm nor go-mocklm.
#   2. The subprocess host import graph (go list -deps) has zero nanollm.
#   3. CGO_ENABLED=0 builds succeed: the default `go build ./...` AND the
#      subprocess host `./cmd/serve` link with cgo disabled.
#   4. The compiled CGO_ENABLED=0 subprocess host has zero nanollm SYMBOLS
#      (go tool nm) — a link-graph assertion, distinct from `strings`, which
#      would legitimately match nanollm text inside an embedded native-worker
#      payload.
#   5. The de-BAML Slice 7.1a prompt-render path — nativeprompt -> bamlprofile
#      -> the pinned pure-Go minijinja fork — is PURE: it reaches the profile and
#      the fork, and reaches NO nanollm, go-mocklm, stock BAML/CFFI, dynclient,
#      test oracle, or the retired pre-fork external engine. Check 2 covers only
#      nanollm on the whole host; this pins the newly wired edge specifically, so
#      a future import that drags the stock runtime back into the render path
#      fails here rather than at a container build.
#
# Run from anywhere in the repo. Exits non-zero on the first violation.
#
# Sourceable: the match helpers below are defined at load time, but the checks
# run only when the script is executed directly (see the BASH_SOURCE guard at the
# end), so check-host-zero-nanollm_test.sh can source it to exercise them.
set -euo pipefail

fail() {
    echo "FAIL: $*" >&2
    exit 1
}

# The prefix github.com/viktordanov/nanollm matches BOTH the isolated test/worker
# module path and the public nanollm-ffi package; go-mocklm is the gated test
# responder. Neither may appear in a root module/workspace file.
NANOLLM_PREFIX="github.com/viktordanov/nanollm"
MOCKLM_PREFIX="github.com/viktordanov/go-mocklm"

# graph_has_nanollm / symbols_have_nanollm test a CAPTURED string for a nanollm
# reference. They return 0 when the gate must FAIL (match found — or grep itself
# errored, which we treat as a match so the gate stays fail-closed) and 1 only
# when the input is confirmed clean.
#
# Why a here-string and not `printf … | grep -q`: a two-command pipeline lets
# `grep -q` close the pipe on an EARLY match, so `printf` takes SIGPIPE (141);
# under `set -o pipefail` the pipeline status is then non-zero and an `if`
# condition would take its FALSE branch — silently MISSING a real match when it
# sits early in a large stream (the exact symbol-table case). A single `grep`
# reading a here-string has no upstream producer to signal, so its exit status is
# purely match/no-match/error. `|| rc=$?` keeps `set -e` from firing on the
# no-match (exit 1). `-F` is a fixed-string match (NANOLLM_PREFIX is a literal).
graph_has_nanollm() {
    local rc=0
    grep -Fq -- "${NANOLLM_PREFIX}" <<<"$1" || rc=$?
    [ "${rc}" -eq 1 ] && return 1
    return 0
}
symbols_have_nanollm() {
    local rc=0
    grep -Fiq -- 'nanollm' <<<"$1" || rc=$?
    [ "${rc}" -eq 1 ] && return 1
    return 0
}

# graph_has_ref <literal-pattern> <captured-string>: the general form of
# graph_has_nanollm, used by check 5 to assert BOTH directions — PRESENCE (an
# expected package must be on the render path) and ABSENCE (a forbidden one must
# not be).
#
# Because it serves both, it is THREE-valued rather than the two-valued
# fail-closed contract of the helpers above:
#
#   0  the pattern is present
#   1  the input is CONFIRMED not to contain it
#   2  the scan itself ERRORED (grep exit >1) — the answer is unknown
#
# The distinct error status is load-bearing. The two-valued helpers can collapse
# "errored" into "present" because they only ever test for FORBIDDEN references,
# where "present" is already the fail-closed answer. Here, collapsing the same
# way would make a grep failure look like a REQUIRED package is present, which
# fails OPEN on the presence half. So callers must treat 2 as a gate failure in
# both loops (see render_path_violation), never as a match or a miss.
graph_has_ref() {
    local rc=0
    grep -Fq -- "$1" <<<"$2" || rc=$?
    case "${rc}" in
        0) return 0 ;;
        1) return 1 ;;
        *) return 2 ;;
    esac
}

# RENDER_PATH_REQUIRED / RENDER_PATH_FORBIDDEN are check 5's two sides.
#
# REQUIRED is what the Slice 7.1a wiring must actually reach. FORBIDDEN is what
# would make the host impure, each with why:
#   nanollm / go-mocklm        -> CGO + a linked Rust archive; worker-only.
#   boundaryml/baml            -> the stock runtime/CFFI; test-oracle-only.
#   baml-rest/dynclient        -> carries the vendored BAML runtime fork.
#   profileoracle/staticoracle -> integration-tagged differential harnesses.
#   mitsuhiko/minijinja        -> the retired pre-fork external engine; the
#                                 cutover must leave no production importer.
RENDER_PATH_REQUIRED=(
    "github.com/invakid404/baml-rest/internal/bamlprofile"
    "github.com/invakid404/minijinja-go/v2"
)
RENDER_PATH_FORBIDDEN=(
    "${NANOLLM_PREFIX}"
    "${MOCKLM_PREFIX}"
    "github.com/boundaryml/baml"
    "github.com/invakid404/baml-rest/dynclient"
    "github.com/invakid404/baml-rest/internal/bamlprofile/profileoracle"
    "github.com/invakid404/baml-rest/internal/nativeprompt/staticoracle"
    "github.com/mitsuhiko/minijinja"
)

# render_path_violation <captured-deps>: check 5's whole verdict, as a pure
# function of an ALREADY-CAPTURED dependency listing. It echoes the reason and
# returns 0 when the gate must FAIL, 1 only when the render path is confirmed
# pure.
#
# It is split out from main() precisely so it can be MUTATION-TESTED:
# check-host-zero-nanollm_test.sh feeds it synthetic listings with the
# nativeprompt -> bamlprofile edge removed, with a forbidden package added, and
# with a mocked grep failure, and asserts each is caught. Without that, the
# presence half of this check has no negative evidence at all — the defect
# reviewers found in its first form, where the listing was produced by naming
# ./internal/bamlprofile on the `go list` command line and so contained it
# whether or not nativeprompt imported it.
render_path_violation() {
    local deps="$1" pkg rc
    for pkg in "${RENDER_PATH_REQUIRED[@]}"; do
        rc=0
        graph_has_ref "${pkg}" "${deps}" || rc=$?
        case "${rc}" in
            0) ;; # present, as required
            1) echo "the render path does NOT reach ${pkg}; the Slice 7.1a wiring is not in place"; return 0 ;;
            *) echo "the scan for required package ${pkg} errored; cannot verify the render path"; return 0 ;;
        esac
    done
    for pkg in "${RENDER_PATH_FORBIDDEN[@]}"; do
        rc=0
        graph_has_ref "${pkg}" "${deps}" || rc=$?
        case "${rc}" in
            0) echo "the render path reaches ${pkg}; it must stay pure Go, CGO-free and stock-BAML-free"; return 0 ;;
            1) ;; # confirmed absent
            *) echo "the scan for forbidden package ${pkg} errored; cannot verify the render path"; return 0 ;;
        esac
    done
    return 1
}

# manifest_has_ref <literal-pattern> <file>: the file-based twin of the helpers
# above, for the root go.mod/go.sum/go.work/go.work.sum scan. Returns 0 when the
# gate must FAIL (pattern present, OR grep ERRORED — e.g. an unreadable file,
# grep exit >1 — which we treat as a failure so the scan stays FAIL-CLOSED) and
# 1 ONLY when the file is confirmed clean. A bare `if grep -q PATTERN FILE` reads
# a grep error (exit >1) as "no match", which would fail open on this host-zero
# boundary check.
manifest_has_ref() {
    local rc=0
    grep -Fq -- "$1" "$2" || rc=$?
    [ "${rc}" -eq 1 ] && return 1
    return 0
}

main() {
    # Resolve repo root from this script's location so it runs from any CWD.
    local SCRIPT_DIR REPO_ROOT
    SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
    REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
    cd "${REPO_ROOT}"

echo "== 1/5: root module/workspace files are zero-nanollm =="
for f in go.mod go.sum go.work go.work.sum; do
    [ -f "$f" ] || continue
    if manifest_has_ref "${NANOLLM_PREFIX}" "$f"; then
        fail "root ${f} references nanollm (${NANOLLM_PREFIX}), or the scan errored; host link graph must stay zero-nanollm"
    fi
    if manifest_has_ref "${MOCKLM_PREFIX}" "$f"; then
        fail "root ${f} references go-mocklm, or the scan errored; gated test tooling must stay out of the root module"
    fi
done
echo "   ok: go.mod/go.sum/go.work/go.work.sum reference no nanollm/go-mocklm"

echo "== 2/5: subprocess host import graph is zero-nanollm =="
# -deps lists the full transitive import set of the subprocess host. It never
# includes the embedded worker's own imports (the worker is an opaque []byte).
#
# Capture the producer's output and check ITS exit status FIRST, so this gate
# fails CLOSED. A bare `go list … | grep -q` used as an `if` condition would
# print success whenever `go list` errored and grep therefore saw no matching
# input — masking a broken validation run.
host_deps="$(CGO_ENABLED=0 go list -deps -tags subprocess ./cmd/serve)" \
    || fail "go list -deps of cmd/serve (subprocess) failed; cannot verify host import isolation"
if graph_has_nanollm "${host_deps}"; then
    fail "cmd/serve (subprocess) transitively imports nanollm"
fi
echo "   ok: cmd/serve (subprocess) has no nanollm in its import graph"

echo "== 3/5: CGO_ENABLED=0 builds succeed (default + subprocess host) =="
CGO_ENABLED=0 go build ./... || fail "default 'CGO_ENABLED=0 go build ./...' failed (host must be CGO-free)"
HOST_BIN="$(mktemp -d)/host-subprocess"
CGO_ENABLED=0 go build -tags subprocess -o "${HOST_BIN}" ./cmd/serve \
    || fail "'CGO_ENABLED=0 go build -tags subprocess ./cmd/serve' failed (subprocess host must be CGO-free)"
echo "   ok: default and subprocess-host builds link with CGO disabled"

echo "== 4/5: compiled subprocess host has zero nanollm symbols =="
# go tool nm reads the Go/link symbol table; nanollm symbols would appear here
# only if the host actually linked the archive. It does NOT surface strings
# inside an embedded []byte payload, so this stays correct for a native-worker
# build too.
#
# Same fail-closed pattern: capture nm's output and check its exit status before
# grepping, so an nm failure surfaces instead of being read as "no symbols".
host_syms="$(go tool nm "${HOST_BIN}")" \
    || fail "go tool nm of the subprocess host failed; cannot verify symbol isolation"
if symbols_have_nanollm "${host_syms}"; then
    fail "subprocess host binary carries nanollm symbols (host must not link nanollm)"
fi
echo "   ok: host binary symbol table has no nanollm symbols"

echo "== 5/5: the nativeprompt -> bamlprofile render path is pure =="
# Slice 7.1a made internal/bamlprofile production-reachable through
# internal/nativeprompt (and embedded it — see .embedignore). This asserts the
# newly reachable edge in BOTH directions: the profile and the fork ARE on it,
# and nothing that would make the host impure is.
#
# The listing is rooted at ./internal/nativeprompt ALONE, deliberately.
# `go list -deps` always prints the packages NAMED on its command line, so adding
# ./internal/bamlprofile there would satisfy the required-package half whether or
# not nativeprompt actually imports it — a vacuous proof. Rooting only at the
# importer means bamlprofile and the fork appear if and only if the edge is real,
# and their own transitive dependencies still ride along for the forbidden half.
#
# `go list -deps` (no -test) is the PRODUCTION import graph, which is the claim
# being made; the stock-BAML oracles under ./profileoracle and ./staticoracle are
# integration-tagged test packages and must never appear here.
render_deps="$(CGO_ENABLED=0 go list -deps ./internal/nativeprompt)" \
    || fail "go list -deps of ./internal/nativeprompt failed; cannot verify prompt-render purity"

if reason="$(render_path_violation "${render_deps}")"; then
    fail "${reason}"
fi
echo "   ok: render path reaches bamlprofile + the minijinja fork, and nothing impure"

echo "PASS: host stays zero-nanollm / CGO-free (worker-only nanollm invariant holds)"
}

# Run the checks only when executed directly; when sourced (by the test), just
# expose fail() and the match helpers.
if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
    main "$@"
fi
