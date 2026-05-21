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
# skipping it forces a destructive tag move to recover (the product
# tag has to be force-moved to a fresh commit, which rewrites
# already-published release history). RELEASE.md captures the full
# failure-mode taxonomy under "What goes wrong if you skip Step 1".
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
# without this, root's go.mod drifts behind every release and the
# Renovate bot opens redundant first-party bump PRs to close the gap
# instead). Resolved eagerly so the same list drives both the preflight
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
# before the "prev → version" banner below) so the ordering check
# and backfill-mode detection below can compare against it.
prev_version="$(git tag --list '[0-9]*.[0-9]*.[0-9]*' --sort=-v:refname | head -1 || true)"
if [ -z "$prev_version" ]; then
    echo "ERROR: no previous X.Y.Z tag found; release-prep.sh is meant for incremental releases" >&2
    echo "       — for the very first release, perform the rewrites manually per RELEASE.md" >&2
    exit 1
fi

# Bash 3.2-compatible "a < b" comparison for X.Y.Z semver. Used by the
# ordering check below; we can't lean on `sort -V` because macOS BSD
# sort doesn't ship it. `10#$x` forces decimal so a leading zero
# anywhere in the version (already caught by the earlier shape
# regex, but defense-in-depth) doesn't trigger octal interpretation.
semver_lt() {
    local a_major a_minor a_patch b_major b_minor b_patch
    IFS=. read -r a_major a_minor a_patch <<< "$1"
    IFS=. read -r b_major b_minor b_patch <<< "$2"

    if [ $((10#$a_major)) -lt $((10#$b_major)) ]; then return 0; fi
    if [ $((10#$a_major)) -gt $((10#$b_major)) ]; then return 1; fi
    if [ $((10#$a_minor)) -lt $((10#$b_minor)) ]; then return 0; fi
    if [ $((10#$a_minor)) -gt $((10#$b_minor)) ]; then return 1; fi
    [ $((10#$a_patch)) -lt $((10#$b_patch)) ]
}

# Ordering check: refuse if version is older than the latest product
# tag, regardless of whether the older tag exists in the local clone.
# Without this, a typo like `release-prep.sh 0.0.45` in a shallow or
# tag-pruned clone (where 0.0.45 isn't fetched) would slip past the
# tag-existence branch below and proceed to downgrade root's
# first-party requires. The semver comparison fires first so the
# operator gets a single clear error, not whatever cascade `go mod
# tidy` would produce against the wrong version.
if semver_lt "$version" "$prev_version"; then
    echo "ERROR: ${version} is older than the latest product tag (${prev_version})." >&2
    echo "       Running release-prep for an older version would attempt to downgrade root's" >&2
    echo "       first-party requires below the released set." >&2
    echo "       — to bump forward, pass the next version after ${prev_version}" >&2
    echo "       — to recover a botched ${version}, skip it and prep the next version cleanly" >&2
    exit 1
fi

# Tag-existence semantics, simplified now that semver_lt above already
# rejected version < prev_version:
# - version > prev_version → normal forward release-prep (no message,
#   just continue).
# - version equals the latest product tag → backfill mode. Every
#   published-module edit will be a no-op (modules are already at
#   expected_version from the prior run), but the root `go mod tidy`
#   step at the end can still pull root's first-party requires forward
#   to match the release that already shipped. This is the post-Step-6
#   rerun path that updates root once the per-module tags reach the
#   module proxy — no separate maintainer script required. Allow it
#   with a NOTE so the operator sees what the script is doing.
backfill_mode=false
if [ "$version" = "$prev_version" ]; then
    backfill_mode=true
    echo "NOTE: ${version} is already the latest product tag — running in backfill mode."
    echo "      Published-module edits will be no-ops (already at ${expected_version});"
    echo "      the root \`go mod tidy\` step will pull root's first-party requires"
    echo "      forward to ${expected_version}."
    echo
fi

if [ "$backfill_mode" = true ]; then
    echo "release-prep: ${prev_version} (backfill)"
else
    echo "release-prep: ${prev_version} → ${version}"
fi
echo

# Apply the 11 explicit go mod edit rewrites across the five
# published modules that have first-party requires. The set matches
# the manual copy-paste form documented under RELEASE.md "Step 1 —
# Release-prep require rewrites".
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
# release. We handle that explicitly below after sync.sh runs so the
# automation matches the manual procedure that's worked in practice.
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
# references must move forward at each release. Without this step,
# every release `go mod tidy` in root would do the bump organically
# (eventually), and Renovate sees the lag in the meantime and opens
# redundant first-party bump PRs that duplicate what tidy produces
# anyway. Doing it here as part of release-prep keeps the released
# tree self-consistent and lets the Renovate config disable first-
# party deps without losing anything.
#
# Backfill-mode-only: this step is gated on backfill_mode because
# tidy needs the new product tag to exist on the module proxy. Root
# has no local replace for dynclient/baml-patched (intentional —
# go.work's inline notes explain why the patched fork stays outside
# the workspace), so a forward-mode run for an unreleased version
# would crash here with "unknown revision
# dynclient/baml-patched/v0.0.X". The canonical flow is forward prep
# (this step skipped) → tag → release-go-tags → re-run release-prep
# in backfill mode (this step fires) → commit root. The "Next steps"
# output in forward mode below points the operator at the re-run.
if [ "$backfill_mode" = true ]; then
    echo
    echo "Tidying root module..."
    go mod tidy
fi

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
#
# Gated on backfill_mode for the same reason as the tidy step above:
# in forward mode root intentionally stays at the previous release
# (the tidy step that would advance it is skipped), so a forward
# validation against the new expected_version would report every
# root first-party require as "stale" — a false-positive specific
# to the forward flow. Backfill mode does run tidy first, so the
# validation here is meaningful: it catches anything tidy left
# behind (notably, a pseudo-version sneaking onto a published
# module in root, which release_dep_allows_pseudo now rejects).
if [ "$backfill_mode" = true ]; then
    echo "  ./go.mod (root)"
    if ! release_validate_module_requires "(root)" "$expected_version" true < go.mod; then
        echo "ERROR: root go.mod still has first-party requires that don't match ${expected_version}" >&2
        validation_failed=1
    fi
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
    # The post-`No changes` hint depends on where in the release flow
    # the operator is. In forward mode, the only reason this branch
    # fires is that someone already landed the Step 1 rewrites
    # manually (or via a previous run) — they still need to push +
    # tag + release-go-tags. In backfill mode, "no changes" means root
    # is already current with the released module set (e.g. the
    # operator ran backfill twice, or root was tidied out of band);
    # there's nothing left to do for this release.
    if [ "$backfill_mode" = true ]; then
        echo "(Backfill is a no-op — root already tracks ${expected_version}; nothing to commit.)"
    else
        echo "(Run scripts/release-go-tags.sh ${version} after tagging,"
        echo " then re-run scripts/release-prep.sh ${version} for the Step 7 root backfill.)"
    fi
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

# Next-steps guidance — split by mode because forward and backfill
# land in different places in the release flow:
# - Forward prep is the Step 1 commit; the next chapter is RELEASE.md
#   Steps 3-6 (push to master, tag, release-go-tags). The forward
#   output also points at Step 7 so the operator knows the post-tag
#   backfill rerun is a real step, not a workaround.
# - Backfill prep is the Step 7 commit; tags already exist, no further
#   release-go-tags pass, no GitHub Release to publish. The maintainer
#   just commits + pushes via a regular PR. The commit-message shape
#   differs (`backfill root for ${version}` vs `prepare Go module
#   versions for ${version}`) so it's clear in `git log` which
#   release-prep run produced the commit.
echo
if [ "$backfill_mode" = true ]; then
    echo "Next steps (RELEASE.md Step 7 — post-tag root backfill):"
else
    echo "Next steps (RELEASE.md Step 1 — forward release-prep):"
fi
echo "  1. Verify the build:  go build ./... && go vet ./... && go test ./... -count=1"
echo "                        (cd dynclient && GOWORK=off go test ./... -count=1)"
echo "  2. Review the diff:   git diff --cached -- ${candidate_paths[*]}"
# The `-- <paths>` pathspec on the commit line tells git to commit
# the working-tree content of those paths only, ignoring any
# unrelated changes the maintainer may have staged separately. The
# commit is safe to copy-paste even when other staged work exists.
if [ "$backfill_mode" = true ]; then
    echo "  3. Commit:            git commit -m \"chore(release): backfill root for ${version}\" \\"
    echo "                            -- ${candidate_paths[*]}"
    echo "  4. Push to master via a regular PR. No new tag, no GitHub Release —"
    echo "     root is excluded from the published tag set (see RELEASE.md Tag conventions),"
    echo "     so this commit just keeps root's go.mod / go.sum tracking the released set."
else
    echo "  3. Commit:            git commit -m \"chore(release): prepare Go module versions for ${version}\" \\"
    echo "                            -- ${candidate_paths[*]}"
    echo "  4. Push to master and continue from RELEASE.md Step 3."
    # The root tidy step was skipped in forward mode (it needs the
    # new product tag on the module proxy). After tagging in Steps 4
    # / 6 of RELEASE.md, re-running this script picks up backfill
    # mode and produces the root go.mod / go.sum bump as a separate
    # commit on top of the release-go-tags work. Surface that here so
    # the operator doesn't have to remember the two-phase flow from
    # context.
    echo
    echo "  After Step 6 (release-go-tags.sh ${version}), re-run \`scripts/release-prep.sh ${version}\`"
    echo "  per RELEASE.md Step 7 to backfill root's first-party requires. That second run"
    echo "  executes \`go mod tidy\` in the repo root, which needs the new product tag to be"
    echo "  reachable on the module proxy."
fi
