#!/usr/bin/env bash
set -euo pipefail

# release-go-tags.sh — create the subdir-prefixed Go module tags for a
# product release.
#
# The product release tag is `X.Y.Z` (no `v` prefix) and is created and
# pushed by hand as part of the normal release flow (see RELEASE.md).
# This script is the second step: it walks the set of nested Go modules
# that we publish, validates that the product-tag commit is in a clean
# release-prep state, and then creates and pushes the matching
# subdir-prefixed Go module tags, for example:
#
#   dynclient/vX.Y.Z
#   bamlutils/vX.Y.Z
#   ...
#
# Validation runs against the product-tag commit (NOT the current
# worktree). Each required module's go.mod is read with
# `git show <tag>:<path>/go.mod`. Every first-party require
# (github.com/invakid404/baml-rest/...) must already point at
# `v${version}`. Pseudo-versions (`v0.0.0-...`) and any
# non-`v0.0.<positive-integer>` require are rejected so we never tag a
# commit whose modules would resolve through stale pseudo-versions.
#
# Tag creation is idempotent: tags that already exist at the right SHA
# (locally or on origin) are skipped; tags that exist at a different
# SHA fail loudly.
#
# Usage:
#   scripts/release-go-tags.sh [--dry-run] X.Y.Z
#
# --dry-run validates the product-tag commit and prints the tags that
# would be created and pushed without touching local or remote refs.

usage() {
    sed -n '/^# release-go-tags\.sh/,/^$/p' "$0" | sed 's/^# \{0,1\}//'
}

dry_run=false
version=""

for arg in "$@"; do
    case "$arg" in
        --dry-run) dry_run=true ;;
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
# rather than the generic "doesn't match X.Y.Z" complaint.
if [[ "$version" == v* ]]; then
    echo "ERROR: product release tags use no 'v' prefix; pass '${version#v}' instead of '${version}'" >&2
    exit 2
fi

if [[ ! "$version" =~ ^[0-9]+[.][0-9]+[.][0-9]+$ ]]; then
    echo "ERROR: version must match X.Y.Z (got: '${version}')" >&2
    exit 2
fi

# Walk up from the script's own location to find the repo root, the
# same way scripts/sync.sh does, so this script can be invoked from
# anywhere without callers worrying about the working directory.
script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./release-lib.sh
source "$script_dir/release-lib.sh"

repo_root="$(release_find_repo_root "$script_dir" ".git")" || {
    echo "ERROR: no .git found above $script_dir" >&2
    exit 1
}
cd "$repo_root"

# Required module set + first-party prefix come from release-lib.sh
# so the two release scripts can't drift apart on which modules are
# in the published tag set.
required_modules=("${release_required_modules[@]}")
first_party_prefix="$release_first_party_prefix"
expected_version="v${version}"

# Validate that the product tag exists both locally and on origin
# before doing anything else. The local check resolves the SHA; the
# remote check makes sure we don't push module tags that point at a
# commit nobody else can fetch.
if ! git rev-parse --verify --quiet "${version}^{commit}" >/dev/null; then
    echo "ERROR: product tag '${version}' does not exist locally; create and push it first (see RELEASE.md)" >&2
    exit 1
fi
if ! git ls-remote --exit-code --tags origin "refs/tags/${version}" >/dev/null; then
    echo "ERROR: product tag '${version}' is not on origin; push it first (see RELEASE.md)" >&2
    exit 1
fi
release_sha="$(git rev-parse "${version}^{commit}")"

# Phase 1: validate every required module at the product-tag commit.
# We accumulate failures so the operator sees every broken module in
# one pass rather than re-running the script after each fix.
echo "Validating release-prep state at ${version} (${release_sha})..."
validation_failed=0
for module_path in "${required_modules[@]}"; do
    if ! git cat-file -e "${version}:${module_path}/go.mod" 2>/dev/null; then
        echo "ERROR: product tag ${version} does not contain required module ${module_path}/go.mod" >&2
        validation_failed=1
        continue
    fi
    echo "  ${module_path}/go.mod"
    # Process substitution (not a pipe) so the validator runs in the
    # current shell — required for any state it sets to persist.
    if ! release_validate_module_requires "$module_path" "$expected_version" \
            < <(git show "${version}:${module_path}/go.mod"); then
        echo "ERROR: ${module_path}/go.mod has first-party requires that must be rewritten to ${expected_version} before tagging" >&2
        validation_failed=1
    fi
done
if [ "$validation_failed" -ne 0 ]; then
    echo "ERROR: release-prep validation failed; fix the modules above, retag, and re-run" >&2
    exit 1
fi
echo "Validation OK."

# Phase 2: figure out which tags need to be created / pushed.
# `to_create` holds tags we'll create locally then push, `skipped`
# holds tags that already point at release_sha somewhere (local or
# remote). Anything pointing at a *different* SHA aborts immediately.
to_create=()
skipped=()

for module_path in "${required_modules[@]}"; do
    tag="${module_path}/${expected_version}"

    local_sha=""
    if local_sha="$(git rev-parse --verify --quiet "refs/tags/${tag}")"; then
        :
    else
        local_sha=""
    fi

    if [ -n "$local_sha" ]; then
        if [ "$local_sha" != "$release_sha" ]; then
            echo "ERROR: local tag ${tag} points at ${local_sha}, expected ${release_sha}" >&2
            exit 1
        fi
        skipped+=("$tag")
        continue
    fi

    # `git ls-remote` exits 0 with empty stdout when the ref doesn't
    # exist, so we can't rely on the exit code alone — check stdout.
    remote_line="$(git ls-remote --tags origin "refs/tags/${tag}" || true)"
    if [ -n "$remote_line" ]; then
        remote_sha="${remote_line%%[[:space:]]*}"
        if [ "$remote_sha" != "$release_sha" ]; then
            echo "ERROR: origin tag ${tag} points at ${remote_sha}, expected ${release_sha}" >&2
            exit 1
        fi
        skipped+=("$tag")
        continue
    fi

    to_create+=("$tag")
done

# Phase 3: act (or pretend to, in dry-run).
created=()
if [ "${#to_create[@]}" -gt 0 ]; then
    if $dry_run; then
        echo
        echo "[dry-run] would create and push:"
        for tag in "${to_create[@]}"; do
            echo "  ${tag} -> ${release_sha}"
        done
    else
        for tag in "${to_create[@]}"; do
            echo "Creating ${tag} -> ${release_sha}"
            git tag "${tag}" "${release_sha}"
            created+=("$tag")
        done
        echo "Pushing ${#created[@]} tag(s) to origin..."
        git push origin "${created[@]}"
    fi
fi

# Final summary.
echo
echo "Summary:"
echo "  product tag: ${version} (${release_sha})"
if $dry_run; then
    echo "  mode: dry-run (no tags created or pushed)"
fi
if [ "${#created[@]}" -gt 0 ]; then
    echo "  created: ${#created[@]}"
    for tag in "${created[@]}"; do echo "    ${tag}"; done
fi
if [ "${#to_create[@]}" -gt 0 ] && $dry_run; then
    echo "  would create: ${#to_create[@]}"
    for tag in "${to_create[@]}"; do echo "    ${tag}"; done
fi
if [ "${#skipped[@]}" -gt 0 ]; then
    echo "  skipped (already at ${release_sha}): ${#skipped[@]}"
    for tag in "${skipped[@]}"; do echo "    ${tag}"; done
fi
if [ "${#created[@]}" -eq 0 ] && [ "${#to_create[@]}" -eq 0 ]; then
    echo "  no-op: all ${#required_modules[@]} Go module tags already point at ${release_sha}"
fi
