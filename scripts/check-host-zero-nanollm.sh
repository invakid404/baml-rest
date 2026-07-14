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
# It proves four things, none of which depend on the embedded worker bytes:
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

echo "== 1/4: root module/workspace files are zero-nanollm =="
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

echo "== 2/4: subprocess host import graph is zero-nanollm =="
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

echo "== 3/4: CGO_ENABLED=0 builds succeed (default + subprocess host) =="
CGO_ENABLED=0 go build ./... || fail "default 'CGO_ENABLED=0 go build ./...' failed (host must be CGO-free)"
HOST_BIN="$(mktemp -d)/host-subprocess"
CGO_ENABLED=0 go build -tags subprocess -o "${HOST_BIN}" ./cmd/serve \
    || fail "'CGO_ENABLED=0 go build -tags subprocess ./cmd/serve' failed (subprocess host must be CGO-free)"
echo "   ok: default and subprocess-host builds link with CGO disabled"

echo "== 4/4: compiled subprocess host has zero nanollm symbols =="
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

echo "PASS: host stays zero-nanollm / CGO-free (worker-only nanollm invariant holds)"
}

# Run the checks only when executed directly; when sourced (by the test), just
# expose fail() and the match helpers.
if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
    main "$@"
fi
