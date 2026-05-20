#!/usr/bin/env bash
set -euo pipefail

# release-prep.sh — one-command release-prep for a baml-rest cut.
#
# Rewrites every first-party `require` line in the published Go
# modules to `v${version}`, then runs `scripts/sync.sh` to refresh
# the workspace + adapter pin matrix, then validates the result
# against the same checks `scripts/release-go-tags.sh` will run at
# tag time. The changes are left staged but uncommitted so the
# maintainer reviews and commits manually (the verify + commit step
# is still owned by RELEASE.md Step 2).
#
# This script exists because the manual release-prep — eleven hand
# -typed `go mod edit` calls plus a `sync.sh` run plus a known
# pool-vs-workerplugin quirk — was the step most often skipped, and
# skipping it forces a destructive tag move to recover. See #308.
#
# Usage:
#   scripts/release-prep.sh X.Y.Z

usage() {
    sed -n '/^# release-prep\.sh/,/^$/p' "$0" | sed 's/^# \{0,1\}//'
}

version=""
for arg in "$@"; do
    case "$arg" in
        -h|--help) usage; exit 0 ;;
        -*)
            echo "ERROR: unknown flag: $arg" >&2
            usage >&2
            exit 2
            ;;
        *)
            if [ -n "$version" ]; then
                echo "ERROR: unexpected extra argument: $arg" >&2
                usage >&2
                exit 2
            fi
            version="$arg"
            ;;
    esac
done

if [ -z "$version" ]; then
    echo "ERROR: missing version argument (expected X.Y.Z)" >&2
    usage >&2
    exit 2
fi

# Reject a leading `v` explicitly so the failure message is specific
# rather than the generic "doesn't match X.Y.Z" complaint, matching
# the convention release-go-tags.sh follows.
if [[ "$version" == v* ]]; then
    echo "ERROR: product release versions use no 'v' prefix; pass '${version#v}' instead of '${version}'" >&2
    exit 2
fi

if [[ ! "$version" =~ ^[0-9]+[.][0-9]+[.][0-9]+$ ]]; then
    echo "ERROR: version must match X.Y.Z (got: '${version}')" >&2
    exit 2
fi

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./release-lib.sh
source "$script_dir/release-lib.sh"

repo_root="$(release_find_repo_root "$script_dir" "go.work")" || {
    echo "ERROR: no go.work found above $script_dir" >&2
    exit 1
}
cd "$repo_root"

expected_version="v${version}"

# Reject if the version is already a release tag — re-running this
# script for an already-cut release silently churns module files
# against a frozen tag, which is never what the operator wants. The
# hint covers the two real use cases: bumping forward (next release)
# or skipping a broken cut (cleanly prepping vN+1 after vN was
# botched).
if git tag --list "$version" | grep -qx "$version"; then
    echo "ERROR: ${version} is already a release tag; this script prepares the *next* release commit" >&2
    echo "       — to bump forward, pass the next version (e.g. the patch after ${version})" >&2
    echo "       — to recover a botched ${version}, skip it and prep the next version cleanly" >&2
    exit 1
fi

# Detect previous version informationally for the status output. Only
# semver-shaped tags are considered, so accidental tags like
# `mistake-0.0.41-rc1` don't poison the lookup. `sort -V` would treat
# `0.0.10` as older than `0.0.2`; git's `-v:refname` understands the
# semver ordering once we strip non-matching tags.
prev_version="$(
    git tag --list '[0-9]*.[0-9]*.[0-9]*' \
        | grep -E '^[0-9]+\.[0-9]+\.[0-9]+$' \
        | sort -V \
        | tail -1 \
        || true
)"
if [ -z "$prev_version" ]; then
    echo "ERROR: no previous X.Y.Z tag found; release-prep.sh is meant for incremental releases" >&2
    echo "       — for the very first release, perform the rewrites manually per RELEASE.md" >&2
    exit 1
fi

echo "release-prep: ${prev_version} → ${version}"
echo

# Apply the 11 explicit go mod edit rewrites across the five
# published modules that have first-party requires. The set matches
# RELEASE.md "Step 1 — Release-prep require rewrites" and the diffs
# from PRs #319 and #321.
#
# Each module's edits are batched into a single `go mod edit` call so
# `go.mod` is only rewritten once per module, matching the manual
# copy-paste form in RELEASE.md.
prefix="$release_first_party_prefix"

run_edit() {
    local module_dir="$1"
    shift
    local requires=()
    local dep
    for dep in "$@"; do
        requires+=("-require=${prefix}/${dep}@${expected_version}")
    done
    echo "  ${module_dir}/go.mod"
    (cd "$module_dir" && go mod edit "${requires[@]}")
}

echo "Rewriting first-party requires to ${expected_version}..."
run_edit dynclient \
    adapters/common \
    bamlutils \
    dynclient/baml-patched \
    introspected \
    worker \
    workerplugin
run_edit adapters/common \
    bamlutils \
    introspected
run_edit introspected \
    bamlutils
run_edit worker \
    bamlutils \
    workerplugin
run_edit workerplugin \
    bamlutils

# Run sync.sh to refresh the workspace + adapter pin matrix. This
# propagates first-party requires to pool/go.mod and the
# adapters/adapter_v*/go.mod files, and refreshes BAML adapter pins.
#
# Known quirk: `go work sync` propagates bamlutils to pool/go.mod but
# does NOT rewrite pool's `workerplugin` require for the same
# release. Both prior release-prep PRs (#319, #321) had to bump it
# manually. We handle that explicitly below after sync.sh runs.
echo
echo "Running scripts/sync.sh..."
"$script_dir/sync.sh"

# Explicit pool bumps — workaround for the `go work sync` quirk
# above. Doing this idempotently means an explicit edit is a no-op
# when sync.sh already wrote the right values, so we never need to
# branch on whether the quirk fired this run.
echo
echo "Pinning pool first-party requires to ${expected_version}..."
run_edit pool \
    bamlutils \
    workerplugin

# Final validation: every required module's first-party requires
# must match v${version}. This is the same shape
# scripts/release-go-tags.sh enforces against the tagged commit, run
# here against the working tree so a botched prep fails BEFORE the
# tag exists rather than after.
echo
echo "Validating release-prep state for ${expected_version}..."
validation_failed=0
for module_path in "${release_required_modules[@]}"; do
    if [ ! -f "${module_path}/go.mod" ]; then
        echo "ERROR: required module ${module_path}/go.mod is missing from the worktree" >&2
        validation_failed=1
        continue
    fi
    echo "  ${module_path}/go.mod"
    if ! release_validate_module_requires "$module_path" "$expected_version" < "${module_path}/go.mod"; then
        echo "ERROR: ${module_path}/go.mod still has first-party requires that don't match ${expected_version}" >&2
        validation_failed=1
    fi
done
if [ "$validation_failed" -ne 0 ]; then
    echo
    echo "ERROR: release-prep validation failed — fix the modules above and re-run" >&2
    exit 1
fi
echo "Validation OK."

# Status summary. We do NOT commit — the maintainer still runs the
# RELEASE.md Step 2 verification (`go build`, `go vet`, `go test`,
# `GOWORK=off go test` inside dynclient) and then commits manually.
echo
changed=()
while IFS= read -r line; do
    [ -n "$line" ] && changed+=("$line")
done < <(git -C "$repo_root" diff --name-only)

if [ "${#changed[@]}" -eq 0 ]; then
    echo "No changes — every first-party require was already at ${expected_version}."
    echo "(Run scripts/release-go-tags.sh ${version} after tagging.)"
    exit 0
fi

echo "Changed files:"
for f in "${changed[@]}"; do
    echo "  ${f}"
done

echo
echo "Next steps (per RELEASE.md):"
echo "  1. Verify the build:  go build ./... && go vet ./... && go test ./... -count=1"
echo "                        (cd dynclient && GOWORK=off go test ./... -count=1)"
echo "  2. Review the diff:   git diff"
echo "  3. Commit:            git commit -am \"chore(release): prepare Go module versions for ${version}\""
echo "  4. Push to master and continue from RELEASE.md Step 3."
