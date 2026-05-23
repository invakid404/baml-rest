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

if $check_pins_only; then
    verify_pins
    exit 0
fi

if ! $only_adapters; then
    echo "Running \`go work sync\`..."
    go work sync
fi

tidy_standalone_modules

if $only_workspace; then
    echo
    echo "Done. Synced workspace + ${#standalone_dirs[@]} standalone module(s)."
    exit 0
fi

tidy_adapter_version_modules
verify_pins

echo
if $only_adapters; then
    echo "Done. Synced ${#standalone_dirs[@]} standalone module(s) + $adapter_tidy_count adapter version(s)."
else
    echo "Done. Synced workspace + ${#standalone_dirs[@]} standalone module(s) + $adapter_tidy_count adapter version(s)."
fi
