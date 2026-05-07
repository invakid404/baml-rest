#!/usr/bin/env bash
set -euo pipefail

# sync.sh — the equivalent of `go work sync` for a repo whose
# adapter_v* and adapters/common modules sit outside the workspace on
# purpose.
#
# Each adapters/adapter_vX_Y_Z module pins a specific
# github.com/boundaryml/baml version on purpose; including them in
# go.work would make MVS bump every adapter to the highest BAML
# version (see go.work for the full rationale). adapters/common is
# excluded for the same reason: it directly requires BAML and each
# adapter has a local `replace ../common` that propagates whatever
# version common pins.
#
# This script:
#   1. Runs `go work sync` for the workspace members so their go.mod
#      files stay reconciled the way they always were.
#   2. Runs `GOWORK=off go mod tidy` in adapters/common. common is no
#      longer in the workspace, so `go work sync` won't refresh its
#      indirect requires; tidy it explicitly with the per-module
#      go.mod as the build list so its BAML pin stays at the
#      lowest-adapter version.
#   3. Walks adapters/adapter_v*/go.mod and runs `GOWORK=off go mod
#      tidy` in each one. Tidying with GOWORK=off forces Go to use
#      that adapter's own go.mod for the build list, so MVS only ever
#      sees the single BAML version that adapter pins. Indirect
#      requires get refreshed without disturbing the BAML pin.
#   4. Runs cmd/verify-adapter-pins to assert that every pinned
#      module's go.mod still resolves the canonical BAML version.
#
# Usage:
#   scripts/sync.sh                  # workspace + every adapter + verify
#   scripts/sync.sh --only-workspace # skip per-adapter tidy + verify
#   scripts/sync.sh --only-adapters  # skip `go work sync`
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

# Walk up from the script's own location to find the repo root (the
# directory containing go.work). This lets the script be invoked from
# anywhere — including via a symlink — without the user worrying about
# the working directory.
script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$script_dir"
while [ "$repo_root" != "/" ] && [ ! -f "$repo_root/go.work" ]; do
    repo_root="$(dirname "$repo_root")"
done
if [ ! -f "$repo_root/go.work" ]; then
    echo "ERROR: no go.work found above $script_dir" >&2
    exit 1
fi
cd "$repo_root"

# common_dir and adapter_dirs are populated up-front so the same lists
# drive the tidy loop and the pin-only path. Using nullglob means a
# layout change that drops every adapter directory is a clean no-op
# rather than a failed glob expansion.
common_dir="adapters/common"
shopt -s nullglob
adapter_dirs=(adapters/adapter_v*/)
shopt -u nullglob

verify_pins() {
    echo
    echo "Verifying BAML pin matrix..."
    go run ./cmd/verify-adapter-pins
}

if $check_pins_only; then
    verify_pins
    exit 0
fi

if ! $only_adapters; then
    echo "Running \`go work sync\`..."
    go work sync
fi

if $only_workspace; then
    echo "Done."
    exit 0
fi

# adapters/common is no longer a workspace member, so `go work sync`
# above no longer refreshes it. Tidy it explicitly before the adapter
# loop so the adapters' `replace ../common` directives see an
# up-to-date common/go.mod with its BAML pin still at the canonical
# lowest-adapter version.
if [ -f "$common_dir/go.mod" ]; then
    echo "Tidying $common_dir with GOWORK=off..."
    (cd "$common_dir" && GOWORK=off go mod tidy)
fi

count=0
for dir in "${adapter_dirs[@]}"; do
    dir="${dir%/}"
    if [ ! -f "$dir/go.mod" ]; then
        continue
    fi
    if [ "$count" -eq 0 ]; then
        echo "Tidying adapter module(s) with GOWORK=off..."
    fi
    printf '  %s ... ' "$dir"
    (cd "$dir" && GOWORK=off go mod tidy)
    echo "ok"
    count=$((count + 1))
done

if [ "$count" -eq 0 ]; then
    echo "No adapter version modules to tidy."
    # Still verify pins — the common module has a pin to assert even
    # in a layout with zero versioned adapters.
    verify_pins
    exit 0
fi

verify_pins

echo
if $only_adapters; then
    echo "Done. Synced $count adapter(s) + common."
else
    echo "Done. Synced workspace + common + $count adapter(s)."
fi
