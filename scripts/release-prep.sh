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

# The set of paths the script may stage at the end. The five
# directly-edited published modules, pool, every existing
# adapters/adapter_v*/go.mod (which sync.sh refreshes), and root's
# go.mod / go.sum (which the final `go mod tidy` step rewrites to
# pull root's first-party requires forward to the released tag set —
# closes the gap that surfaced after 0.0.47 as Renovate PRs #330 and
# #331). Resolved eagerly so the same list drives both the preflight
# and the stage-at-end pass and the two paths can't drift.
candidate_paths=(
    dynclient/go.mod
    adapters/common/go.mod
    introspected/go.mod
    worker/go.mod
    workerplugin/go.mod
    pool/go.mod
    go.mod
    go.sum
)
shopt -s nullglob
for dir in adapters/adapter_v*/; do
    [ -f "${dir}go.mod" ] || continue
    candidate_paths+=("${dir%/}/go.mod")
done
shopt -u nullglob

# Preflight: refuse to run if any candidate path has uncommitted
# modifications, untracked content, or staged changes. The script
# ends by `git add`-ing these paths, so pre-existing work-in-progress
# in one of them would get scooped into the release-prep commit by
# accident.
dirty_paths=()
for path in "${candidate_paths[@]}"; do
    if [ -n "$(git status --porcelain -- "$path")" ]; then
        dirty_paths+=("$path")
    fi
done
if [ "${#dirty_paths[@]}" -gt 0 ]; then
    echo "ERROR: release-prep would stage paths that already have uncommitted changes:" >&2
    for path in "${dirty_paths[@]}"; do
        echo "    $path" >&2
    done
    echo "       Commit or stash those changes first; the script stages every go.mod it" >&2
    echo "       rewrites and won't run while any of those paths are dirty." >&2
    exit 1
fi

# Detect the most recent product tag. The `[0-9]*` glob excludes
# subdir-prefixed module tags like `dynclient/v0.0.46` so only product
# tags rank. `--sort=-v:refname` returns them in descending semver
# order so `head -1` is the latest. Computed up here (rather than just
# before the "prev → version" banner below) so the tag-existence
# branch below can compare against it for backfill-mode detection.
prev_version="$(git tag --list '[0-9]*.[0-9]*.[0-9]*' --sort=-v:refname | head -1 || true)"
if [ -z "$prev_version" ]; then
    echo "ERROR: no previous X.Y.Z tag found; release-prep.sh is meant for incremental releases" >&2
    echo "       — for the very first release, perform the rewrites manually per RELEASE.md" >&2
    exit 1
fi

# Tag-existence semantics:
# - version is not a product tag → normal forward release-prep (no
#   message, just continue).
# - version equals the latest product tag → backfill mode. Every
#   published-module edit will be a no-op (modules are already at
#   expected_version from the prior run), but the root `go mod tidy`
#   step at the end can still pull root's first-party requires forward
#   to match the release that already shipped. This is the path that
#   closes the #330/#331 gap on 0.0.47 without needing a one-off
#   maintainer script. Allow it with a NOTE so the operator sees what
#   the script is doing.
# - version is some older product tag → almost certainly a typo. A run
#   here would attempt to downgrade root's first-party requires below
#   the released set. Refuse so the operator catches the mistake
#   before any file changes.
backfill_mode=false
if git tag --list "$version" | grep -qx "$version"; then
    if [ "$version" = "$prev_version" ]; then
        backfill_mode=true
        echo "NOTE: ${version} is already the latest product tag — running in backfill mode."
        echo "      Published-module edits will be no-ops (already at ${expected_version});"
        echo "      the root \`go mod tidy\` step will pull root's first-party requires"
        echo "      forward to ${expected_version}."
        echo
    else
        echo "ERROR: ${version} is already a release tag, and not the latest one (${prev_version})." >&2
        echo "       Running release-prep for an older tag would attempt to downgrade root's" >&2
        echo "       first-party requires below the released set." >&2
        echo "       — to bump forward, pass the next version after ${prev_version}" >&2
        echo "       — to recover a botched ${version}, skip it and prep the next version cleanly" >&2
        exit 1
    fi
fi

if [ "$backfill_mode" = true ]; then
    echo "release-prep: ${prev_version} (backfill)"
else
    echo "release-prep: ${prev_version} → ${version}"
fi
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

# Root module: tidy so root's first-party requires (and the indirect
# dynclient/baml-patched require + its go.sum entries) move forward to
# match the released module set. Root is deliberately excluded from
# the published-tag set (see release-lib.sh release_required_modules),
# but its own go.mod still references the published modules — those
# references must move forward at each release. Skipping this step is
# what produced Renovate PRs #330 and #331 against 0.0.47: every
# release `go mod tidy` in root would do this organically, so Renovate
# saw the lag and opened cosmetic-stale bumps that duplicated what
# tidy would do anyway. Doing it here as part of release-prep keeps
# the released tree self-consistent without needing the bot to fill
# the gap.
echo
echo "Tidying root module..."
go mod tidy

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
# Root: the workspace-only modules (adapter_v*, dynclient, pool) sit
# at pseudo-versions in root by design — they're never published with
# a tag, and root resolves them through local `replace` directives in
# the same go.mod. Pass allow_pseudo=true so the validator treats those
# pseudos as clean while still catching v0.0.<N≠expected> stales for
# the published modules root depends on (adapters/common, bamlutils,
# introspected, worker, workerplugin, and the indirect baml-patched).
echo "  ./go.mod (root)"
if ! release_validate_module_requires "(root)" "$expected_version" true < go.mod; then
    echo "ERROR: root go.mod still has first-party requires that don't match ${expected_version}" >&2
    validation_failed=1
fi
if [ "$validation_failed" -ne 0 ]; then
    echo
    echo "ERROR: release-prep validation failed — fix the modules above and re-run" >&2
    exit 1
fi
echo "Validation OK."

# Stage the rewritten go.mod files so the maintainer's next step is
# straight `git commit`. We do NOT commit — the maintainer still
# runs the RELEASE.md Step 2 verification (`go build`, `go vet`,
# `go test`, `GOWORK=off go test` inside dynclient) before
# committing manually.
#
# Iterating over candidate_paths (the same list the preflight
# verified clean) keeps the staging targeted: even if some unrelated
# tool dirtied a file between preflight and now, we won't sweep it
# up — only paths we knew we were going to touch get staged.
echo
echo "Staging release-prep changes..."
staged_any=0
for path in "${candidate_paths[@]}"; do
    # Skip paths git reports as unchanged so we don't churn the
    # index with a no-op `git add` on files where every first-party
    # require was already at expected_version.
    if [ -n "$(git diff --name-only -- "$path")" ]; then
        git add -- "$path"
        staged_any=1
    fi
done

if [ "$staged_any" -eq 0 ]; then
    echo "No changes — every first-party require was already at ${expected_version}."
    echo "(Run scripts/release-go-tags.sh ${version} after tagging.)"
    exit 0
fi

echo
echo "Staged files:"
# Scope the summary to candidate paths so any pre-existing staged
# work the maintainer had before running release-prep isn't listed
# under our header (and, more importantly, isn't mistaken as part
# of the release-prep commit by a copy-paste of the commit line
# below).
git diff --cached --name-only -- "${candidate_paths[@]}" | sed 's/^/  /'

echo
echo "Next steps (per RELEASE.md):"
echo "  1. Verify the build:  go build ./... && go vet ./... && go test ./... -count=1"
echo "                        (cd dynclient && GOWORK=off go test ./... -count=1)"
echo "  2. Review the diff:   git diff --cached -- ${candidate_paths[*]}"
# The `-- <paths>` pathspec on the commit line tells git to commit
# the working-tree content of those paths only, ignoring any
# unrelated changes the maintainer may have staged separately. The
# commit is safe to copy-paste even when other staged work exists.
echo "  3. Commit:            git commit -m \"chore(release): prepare Go module versions for ${version}\" \\"
echo "                            -- ${candidate_paths[*]}"
echo "  4. Push to master and continue from RELEASE.md Step 3."
