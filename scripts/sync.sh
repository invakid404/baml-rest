#!/usr/bin/env bash
set -euo pipefail

# sync.sh — the equivalent of `go work sync` for a repo whose
# adapter_v* and adapters/common modules sit outside the workspace on
# purpose, with the per-module go.sum maintenance that GOWORK=off
# consumers and CI's standalone-modules matrix need on top.
#
# Each adapters/adapter_vX_Y_Z module pins a specific
# github.com/boundaryml/baml version on purpose; including them in
# go.work would make MVS bump every adapter to the highest BAML
# version (see go.work for the full rationale). adapters/common is
# excluded for the same reason: it directly requires BAML and each
# adapter has a local `replace ../common` that propagates whatever
# version common pins.
#
# `go work sync` reconciles workspace members' go.mod requirements
# against the workspace build list, but it does NOT populate the
# per-module go.sum hashes that an external consumer (or CI's
# standalone-modules matrix, which runs `GOWORK=off go test ./...`
# from inside each published module) needs to build the same module
# outside the workspace. The standalone tidy loop closes that gap by
# running `GOWORK=off go mod tidy` over every entry in
# release_required_modules — the canonical published module set,
# sourced from scripts/release-lib.sh so this script and the release
# tooling agree on what "published" means.
#
# This script:
#   1. Runs `go work sync` for the workspace members so their go.mod
#      files stay reconciled the way they always were.
#   2. Runs `GOWORK=off go mod tidy` in every release_required_modules
#      entry. The loop owns adapters/common (whose BAML pin must stay
#      at the lowest-adapter version, since it's still outside
#      go.work) along with every published Go module, so each
#      standalone go.sum carries the hashes a GOWORK=off build needs.
#   3. Walks adapters/adapter_v*/go.mod and runs `GOWORK=off go mod
#      tidy` in each one. Tidying with GOWORK=off forces Go to use
#      that adapter's own go.mod for the build list, so MVS only ever
#      sees the single BAML version that adapter pins. Indirect
#      requires get refreshed without disturbing the BAML pin.
#   4. Runs cmd/verify-adapter-pins to assert that every pinned
#      module's go.mod still resolves the canonical BAML version.
#
# Usage:
#   scripts/sync.sh                  # workspace + standalone + every adapter + verify
#   scripts/sync.sh --only-workspace # workspace + standalone; skip adapter tidy + verify
#   scripts/sync.sh --only-adapters  # standalone + adapter tidy + verify; skip `go work sync`
#   scripts/sync.sh --check-pins-only# skip every tidy step; only verify
#   scripts/sync.sh -h | --help      # print this header

usage() {
    sed -n '/^# sync\.sh/,/^$/p' "$0" | sed 's/^# \{0,1\}//'
}

only_workspace=false
only_adapters=false
check_pins_only=false

for arg in "$@"; do
    case "$arg" in
        --only-workspace)  only_workspace=true ;;
        --only-adapters)   only_adapters=true ;;
        --check-pins-only) check_pins_only=true ;;
        -h|--help)         usage; exit 0 ;;
        *) echo "ERROR: unknown flag: $arg" >&2; usage >&2; exit 2 ;;
    esac
done

# --check-pins-only is meant to be used by itself; combining it with
# either --only-* flag would silently override one of them, which is
# never what the caller wants.
mode_count=0
$only_workspace  && mode_count=$((mode_count + 1))
$only_adapters   && mode_count=$((mode_count + 1))
$check_pins_only && mode_count=$((mode_count + 1))
if [ "$mode_count" -gt 1 ]; then
    echo "ERROR: --only-workspace, --only-adapters, and --check-pins-only are mutually exclusive" >&2
    exit 2
fi

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./release-lib.sh
source "$script_dir/release-lib.sh"

# Walk up from the script's own location to find the repo root (the
# directory containing go.work). This lets the script be invoked from
# anywhere — including via a symlink — without the user worrying about
# the working directory.
repo_root="$script_dir"
while [ "$repo_root" != "/" ] && [ ! -f "$repo_root/go.work" ]; do
    repo_root="$(dirname "$repo_root")"
done
if [ ! -f "$repo_root/go.work" ]; then
    echo "ERROR: no go.work found above $script_dir" >&2
    exit 1
fi
cd "$repo_root"

# standalone_dirs is the canonical published module set from
# release-lib.sh; adapter_dirs is the workspace-excluded per-version
# adapter set. Both are populated up-front so the same lists drive the
# tidy loops and the pin-only path. nullglob means a layout change
# that drops every adapter directory is a clean no-op, not a failed
# glob expansion.
standalone_dirs=("${release_required_modules[@]}")
shopt -s nullglob
adapter_dirs=(adapters/adapter_v*/)
shopt -u nullglob

verify_pins() {
    echo
    echo "Verifying BAML pin matrix..."
    go run ./cmd/verify-adapter-pins
}

# tidy_standalone_modules runs `GOWORK=off go mod tidy` in every
# published standalone module. A missing go.mod is fatal: this is the
# canonical release module set, and silently skipping an entry would
# hide release/CI drift the moment a module is renamed or moved.
#
# This is one step of a single sync pass; it does not need to be a
# fixed point on its own. The bounded fixed-point loop at the bottom of
# this script re-runs the whole pass (go work sync + this + adapter
# tidies) until the module metadata stops changing, so cross-step
# feedback — a standalone tidy here rewriting a workspace member's
# go.mod, which then changes the next `go work sync` — converges
# instead of leaking a non-fixed-point state (see #473).
tidy_standalone_modules() {
    echo "Tidying standalone-tested module(s) with GOWORK=off..."
    local dir
    for dir in "${standalone_dirs[@]}"; do
        if [ ! -f "$dir/go.mod" ]; then
            echo "ERROR: required standalone module missing go.mod: $dir" >&2
            exit 1
        fi
        printf '  %s ... ' "$dir"
        (cd "$dir" && GOWORK=off go mod tidy)
        echo "ok"
    done
}

# tidy_adapter_version_modules runs `GOWORK=off go mod tidy` in every
# adapters/adapter_v* module and records the count in the global
# adapter_tidy_count so the final status line can report it.
adapter_tidy_count=0
tidy_adapter_version_modules() {
    local count=0
    local dir
    for dir in "${adapter_dirs[@]}"; do
        dir="${dir%/}"
        if [ ! -f "$dir/go.mod" ]; then
            continue
        fi
        if [ "$count" -eq 0 ]; then
            echo "Tidying adapter version module(s) with GOWORK=off..."
        fi
        printf '  %s ... ' "$dir"
        (cd "$dir" && GOWORK=off go mod tidy)
        echo "ok"
        count=$((count + 1))
    done
    if [ "$count" -eq 0 ]; then
        echo "No adapter version modules to tidy."
    fi
    adapter_tidy_count="$count"
}

# module_metadata_paths emits a NUL-delimited list of every Go module
# metadata path the repo treats as sync output — the same set the
# renovate-sync allowlist guards and unit-tests diffs: root go.mod /
# go.sum / go.work / go.work.sum plus every nested */go.mod and
# */go.sum (git pathspec wildcards span '/', so this reaches
# dynclient/baml-patched/go.mod too). Both tracked (--cached) and
# untracked-but-not-ignored (--others) entries are included so a newly
# created go.sum is part of the fingerprint, not invisible to it.
module_metadata_paths() {
    git ls-files -z --cached --others --exclude-standard \
        -- 'go.mod' 'go.sum' 'go.work' 'go.work.sum' '*/go.mod' '*/go.sum'
}

# module_metadata_fingerprint hashes path+content of every module
# metadata file into a single digest. git hash-object is used (rather
# than sha256sum/shasum) because it's available wherever git is —
# macOS and the CI runner alike — and needs no tool-detection. A
# change to any file's content, or the set of files itself, flips the
# digest; that's the convergence signal for the fixed-point loop.
module_metadata_fingerprint() {
    local p
    module_metadata_paths | sort -z | while IFS= read -r -d '' p; do
        printf '%s:' "$p"
        git hash-object "$p" 2>/dev/null || printf 'MISSING'
        printf '\n'
    done | git hash-object --stdin
}

# run_sync_pass performs exactly one sync pass, honoring the --only-*
# flags. This is the body that used to run inline; the fixed-point
# loop below repeats it until the module metadata stops changing.
#
# `go work sync` reconciles workspace members against the workspace
# build list, but the standalone `GOWORK=off go mod tidy` passes that
# follow can themselves rewrite a workspace member's go.mod, which
# changes the inputs for the NEXT `go work sync`. A single pass is
# therefore not guaranteed to be a fixed point (see #473): a Renovate
# dep bump can make pass 1 produce one indirect set and pass 2 reshuffle
# it. Running until convergence makes the published state stable.
run_sync_pass() {
    if ! $only_adapters; then
        echo "Running \`go work sync\`..."
        go work sync
    fi

    tidy_standalone_modules

    if ! $only_workspace; then
        tidy_adapter_version_modules
    fi
}

if $check_pins_only; then
    verify_pins
    exit 0
fi

# Bounded fixed-point loop. Snapshot the metadata fingerprint, run a
# pass, re-snapshot: when a pass leaves the fingerprint unchanged the
# tree is a fixed point and a second full scripts/sync.sh invocation
# would be a no-op (the property #473 needs). The real Renovate cases
# converge on pass 2; the cap guards against an oscillating graph —
# if it hasn't settled by then we fail loudly with the live diff rather
# than committing a non-convergent state or looping forever.
max_passes=5
pass=0
prev_fingerprint="$(module_metadata_fingerprint)"
while true; do
    pass=$((pass + 1))
    echo
    echo "=== sync pass $pass ==="
    run_sync_pass
    new_fingerprint="$(module_metadata_fingerprint)"
    if [ "$new_fingerprint" = "$prev_fingerprint" ]; then
        echo
        echo "Module metadata converged after $pass pass(es)."
        break
    fi
    prev_fingerprint="$new_fingerprint"
    if [ "$pass" -ge "$max_passes" ]; then
        echo "ERROR: scripts/sync.sh did not converge to a fixed point after $max_passes passes." >&2
        echo "Still-changing module metadata:" >&2
        git diff -- 'go.mod' 'go.sum' 'go.work' 'go.work.sum' '*/go.mod' '*/go.sum' >&2 || true
        exit 1
    fi
done

if $only_workspace; then
    echo
    echo "Done. Synced workspace + ${#standalone_dirs[@]} standalone module(s)."
    exit 0
fi

# verify_pins runs ONCE, after convergence — re-running it every pass
# would be wasted work since the pin matrix can only be valid on the
# final, stable go.mod set.
verify_pins

echo
if $only_adapters; then
    echo "Done. Synced ${#standalone_dirs[@]} standalone module(s) + $adapter_tidy_count adapter version(s)."
else
    echo "Done. Synced workspace + ${#standalone_dirs[@]} standalone module(s) + $adapter_tidy_count adapter version(s)."
fi
