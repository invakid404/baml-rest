#!/usr/bin/env bash
set -euo pipefail

# sync.sh — the equivalent of `go work sync` for a repo whose
# adapter_v* modules sit outside the workspace on purpose.
#
# Each adapters/adapter_vX_Y_Z module pins a specific
# github.com/boundaryml/baml version on purpose; including them in
# go.work would make MVS bump every adapter to the highest BAML version
# (see go.work for the full rationale).
#
# This script:
#   1. Runs `go work sync` for the workspace members so their go.mod
#      files stay reconciled the way they always were.
#   2. Walks adapters/adapter_v*/go.mod and runs `GOWORK=off go mod
#      tidy` in each one. Tidying with GOWORK=off forces Go to use that
#      adapter's own go.mod for the build list, so MVS only ever sees
#      the single BAML version that adapter pins. Indirect requires get
#      refreshed without disturbing the BAML pin.
#
# Usage:
#   scripts/sync.sh                  # workspace + every adapter
#   scripts/sync.sh --only-workspace # skip per-adapter tidy
#   scripts/sync.sh --only-adapters  # skip `go work sync`
#   scripts/sync.sh -h | --help      # print this header

usage() {
    sed -n '/^# sync\.sh/,/^$/p' "$0" | sed 's/^# \{0,1\}//'
}

only_workspace=false
only_adapters=false

for arg in "$@"; do
    case "$arg" in
        --only-workspace) only_workspace=true ;;
        --only-adapters)  only_adapters=true ;;
        -h|--help)        usage; exit 0 ;;
        *) echo "ERROR: unknown flag: $arg" >&2; usage >&2; exit 2 ;;
    esac
done

if $only_workspace && $only_adapters; then
    echo "ERROR: --only-workspace and --only-adapters are mutually exclusive" >&2
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

if ! $only_adapters; then
    echo "Running \`go work sync\`..."
    go work sync
fi

if $only_workspace; then
    echo "Done."
    exit 0
fi

shopt -s nullglob
adapter_dirs=(adapters/adapter_v*/)
shopt -u nullglob

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
    exit 0
fi

echo
if $only_adapters; then
    echo "Done. Synced $count adapter(s)."
else
    echo "Done. Synced workspace + $count adapter(s)."
fi
