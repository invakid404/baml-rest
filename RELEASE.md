# Release Procedure

> **DO NOT SKIP RELEASE-PREP (Step 1).** The product tag MUST be created from
> a commit where every first-party `require` statement in the published Go
> modules has already been rewritten to `v0.0.X`. Tagging before that point
> causes `scripts/release-go-tags.sh` to reject the tag set, and the only
> recovery paths are destructive (force-moving an already-pushed tag, possibly
> already attached to a GitHub Release) or skipping the broken version
> entirely. See [What goes wrong if you skip Step 1](#what-goes-wrong-if-you-skip-step-1)
> and #308.

This document is the maintainer-facing guide for cutting a baml-rest
release. It covers both the existing product release (binary + Docker)
and the additive subdir-prefixed Go module tags that let downstream
Go consumers import `github.com/invakid404/baml-rest/dynclient` and
the other published nested modules at a stable version.

## Tag conventions

- Product release tags are `0.0.N`, with **no `v` prefix**. This
  matches every prior release in `git tag -l` and the existing GitHub
  release flow.
- Do **not** change the product tag scheme. Renaming product tags to
  `v0.0.N` would orphan every prior release and break any
  external automation that watches the `0.0.*` tag stream.
- Go module tags are **additive**, subdir-prefixed tags like
  `dynclient/v0.0.N`. They follow Go's nested-module convention
  (subdir prefix + a `v`-prefixed semver) so `go get` can resolve the
  nested modules without conflating them with the product tag.
- The Go module tag set is exactly the seven modules listed under
  [Step 6](#step-6--create-go-module-tags). The repo root, `pool`,
  and the server-only `adapters/adapter_v*` modules are deliberately
  excluded — see PR #289 for the rationale.

## Step 1 — Release-prep require rewrites

**This is the first numbered step on purpose.** The product tag in
Step 4 MUST point at a commit where this step has already landed.
Tagging before the rewrites have merged is the failure mode #308
captures — see [What goes wrong if you skip Step 1](#what-goes-wrong-if-you-skip-step-1).

The seven published modules have first-party `require` edges between
each other. During day-to-day development they point at
`v0.0.0-00010101000000-000000000000` placeholders and resolve through
local `replace` directives in each `go.mod`. Before the product tag
is cut, those placeholders must be rewritten to the actual `v0.0.N`
that is about to be released, so that `go get` from a downstream
consumer resolves through real tags rather than pseudo-versions.

Run the helper:

```sh
./scripts/release-prep.sh 0.0.N
```

This:

1. Validates `0.0.N` is well-formed and not already tagged.
2. Rewrites all eleven first-party `require` edges across the five
   published modules listed below.
3. Runs `scripts/sync.sh` to refresh the workspace and the
   per-adapter pin matrix.
4. Bumps `pool/go.mod`'s first-party requires explicitly — `go work
   sync` propagates `bamlutils` to `pool` but does not propagate
   `workerplugin` for the same release, a quirk both #319 and #321
   had to fix by hand.
5. Re-validates the worktree against the same first-party-requires
   check `scripts/release-go-tags.sh` runs at tag time, so a botched
   prep fails BEFORE the tag exists rather than after.
6. Leaves the changes staged but uncommitted and prints a suggested
   commit message. The maintainer reviews and commits manually — see
   [Step 2](#step-2--verify-and-commit).

Affected files and edges (the script edits these for you; the list
is here for reference if you need to audit the diff):

- `dynclient/go.mod`
  - `adapters/common`
  - `bamlutils`
  - `dynclient/baml-patched`
  - `introspected`
  - `worker`
  - `workerplugin`
- `adapters/common/go.mod`
  - `bamlutils`
  - `introspected`
- `introspected/go.mod`
  - `bamlutils`
- `worker/go.mod`
  - `bamlutils`
  - `workerplugin`
- `workerplugin/go.mod`
  - `bamlutils`

The script's behavior matches the manual rewrite that #319 and #321
used. If you need to perform the edits by hand for any reason (e.g.
the helper is broken on your platform), substitute the release
number into `VERSION` and run:

```sh
VERSION=0.0.N

(cd dynclient && \
  go mod edit \
    -require=github.com/invakid404/baml-rest/adapters/common@v${VERSION} \
    -require=github.com/invakid404/baml-rest/bamlutils@v${VERSION} \
    -require=github.com/invakid404/baml-rest/dynclient/baml-patched@v${VERSION} \
    -require=github.com/invakid404/baml-rest/introspected@v${VERSION} \
    -require=github.com/invakid404/baml-rest/worker@v${VERSION} \
    -require=github.com/invakid404/baml-rest/workerplugin@v${VERSION})

(cd adapters/common && \
  go mod edit \
    -require=github.com/invakid404/baml-rest/bamlutils@v${VERSION} \
    -require=github.com/invakid404/baml-rest/introspected@v${VERSION})

(cd introspected && \
  go mod edit \
    -require=github.com/invakid404/baml-rest/bamlutils@v${VERSION})

(cd worker && \
  go mod edit \
    -require=github.com/invakid404/baml-rest/bamlutils@v${VERSION} \
    -require=github.com/invakid404/baml-rest/workerplugin@v${VERSION})

(cd workerplugin && \
  go mod edit \
    -require=github.com/invakid404/baml-rest/bamlutils@v${VERSION})

./scripts/sync.sh

(cd pool && \
  go mod edit \
    -require=github.com/invakid404/baml-rest/bamlutils@v${VERSION} \
    -require=github.com/invakid404/baml-rest/workerplugin@v${VERSION})
```

Notes:

- **Do not remove the local `replace` directives.** They are ignored
  by downstream consumers when these modules are imported as
  dependencies, and they keep local development ergonomic (the
  workspace builds without needing tags to exist first). Keeping
  them in the release commit is intentional.
- **Do not rewrite the server-only `adapters/adapter_v*` modules
  by hand.** They are not part of the dynclient release graph and
  are not in the published tag set. Their indirect first-party
  requires are managed by `scripts/sync.sh` and the BAML pin-matrix
  verifier — `release-prep.sh` invokes both for you.

### What goes wrong if you skip Step 1

Tagging the product version on a commit that still has stale
first-party requires is the documented failure mode behind #308.
The symptom and recovery cost are worth spelling out because the
recovery is destructive:

- **Symptom.** `scripts/release-go-tags.sh 0.0.N` fails Phase 1
  validation against the freshly-pushed product tag. Every
  first-party `require` line in the published modules is still at
  `v0.0.<N-1>` (or further back). The script prints the offending
  lines per module and emits a recovery hint identifying the shape
  as "release-prep was skipped".
- **Recovery option (a) — force-move the tag.** Create the
  release-prep commit on master, then `git tag -f 0.0.N
  <new-sha>` and `git push --force origin 0.0.N`. This rewrites
  published release history: any GitHub Release attached to the
  tag is silently re-pointed (or worse, orphaned), downstream
  consumers who already cached the original SHA see a divergent
  history, and any external automation watching the `0.0.*` tag
  stream may double-fire. This option is destructive and requires
  explicit maintainer authorization each time.
- **Recovery option (b) — skip the version.** Land the release-prep
  commit on master and tag `0.0.N+1` from it instead, leaving the
  broken `0.0.N` tag in place (or deleting it cleanly before anyone
  consumes it). This burns a version number but avoids touching
  published history.

The whole point of Step 1 being mechanical (one script invocation)
and Step 4 having its "DO NOT tag before Step 1 has merged" guard
is to make this failure mode unreachable in the normal flow.

## Step 2 — Verify and commit

```sh
go build ./...
go vet ./...
go test ./... -count=1
(cd dynclient && GOWORK=off go test ./... -count=1)
```

The extra `GOWORK=off` step inside `dynclient/` verifies that the
module also builds and tests against the rewritten requires when the
workspace is not in play — which is the configuration downstream
consumers will see.

Then use the `git diff --cached -- <paths>` and `git commit -m
"..." -- <paths>` lines that `release-prep.sh` printed at the end
of Step 1 to review and commit. Those commands are the single
source of truth for the release-prep commit shape; both restrict
their pathspec to the rewritten go.mod files so that any unrelated
work you happened to have staged in the same checkout is left out
of the release commit. **Don't substitute `git commit -am ...`
for the printed line** — `-a` would sweep up every modified
tracked file, including any WIP the script intentionally left
alone.

## Step 3 — Push to master

Push the release-prep commit:

```sh
git push origin master
```

## Step 4 — Create and push the product tag

> **DO NOT tag the product version until the release-prep commit
> from Step 1 has merged to `master`.** The Go module tag set
> created in Step 6 must point at a commit where every first-party
> `require` is already at `v0.0.N`. If you tag from an earlier
> commit, Step 6 will reject the tag set and the only recoveries
> are destructive — see
> [What goes wrong if you skip Step 1](#what-goes-wrong-if-you-skip-step-1).

```sh
git tag 0.0.N
git push origin 0.0.N
```

This is the canonical product release tag. The release-go-tags script
in Step 6 refuses to run until this tag exists both locally and on
`origin`.

## Step 5 — Publish the GitHub Release

Publish a GitHub Release from tag `0.0.N`. This preserves the
existing binary and Docker image release flow — none of that pipeline
is changed by the Go module tags added in this document.

## Step 6 — Create Go module tags

```sh
./scripts/release-go-tags.sh 0.0.N
```

The script:

1. Validates that `0.0.N` exists locally and on `origin`.
2. Reads each of the seven module `go.mod` files **at the product-tag
   commit** (not the current worktree) and confirms every first-party
   require is exactly `v0.0.N`. Pseudo-versions (`v0.0.0-…`) and any
   `v0.0.<positive-int>` value that does not match the release fail
   validation with the module path and offending require lines.
3. For each of the seven module paths, creates the local Go module
   tag pointing at the product-tag commit and pushes them together to
   `origin`.

The script is idempotent: tags that already exist at the right SHA
(locally or on `origin`) are skipped; tags that exist at a different
SHA fail loudly. Re-running after a partial push is safe.

`--dry-run` validates and prints the tags that would be created and
pushed without touching any refs.

The full Go module tag set for release `0.0.N`:

| Module | Tag |
| --- | --- |
| `github.com/invakid404/baml-rest/adapters/common` | `adapters/common/v0.0.N` |
| `github.com/invakid404/baml-rest/bamlutils` | `bamlutils/v0.0.N` |
| `github.com/invakid404/baml-rest/dynclient` | `dynclient/v0.0.N` |
| `github.com/invakid404/baml-rest/dynclient/baml-patched` | `dynclient/baml-patched/v0.0.N` |
| `github.com/invakid404/baml-rest/introspected` | `introspected/v0.0.N` |
| `github.com/invakid404/baml-rest/worker` | `worker/v0.0.N` |
| `github.com/invakid404/baml-rest/workerplugin` | `workerplugin/v0.0.N` |

## Step 7 — Backfill root first-party requires

```sh
./scripts/release-prep.sh 0.0.N
```

This is the same script as Step 1, but now `0.0.N` is the latest
product tag (just pushed in Step 4 / tagged out per-module in Step 6)
so the script switches into **backfill mode**. Every published-module
edit is a no-op — those `go.mod` files were already rewritten in
Step 1 — so the only real work is a `go mod tidy` in the repo root.
That tidy pulls root's first-party requires (and the indirect
`dynclient/baml-patched` require + its `go.sum` entries) forward to
the just-released module set.

The tidy step has to run after Step 6 because root has no local
`replace` for `dynclient/baml-patched` (intentional — see `go.work`'s
inline notes on why the patched fork stays outside the workspace).
With no replace in play, `go mod tidy` resolves baml-patched through
the module proxy, which means the new per-module tag from Step 6
must already be reachable. Running this step alongside Step 1, before
the tag exists, crashes tidy with `unknown revision
dynclient/baml-patched/v0.0.N`.

Commit + push the resulting `go.mod` / `go.sum` changes as a regular
PR — the script's "Next steps" output prints the canonical commit
line (`chore(release): backfill root for 0.0.N`). No new product
tag, no GitHub Release, no `release-go-tags.sh` rerun: root is
deliberately excluded from the published tag set (see
[Tag conventions](#tag-conventions)), so this commit just keeps
root's own `go.mod` / `go.sum` tracking the released module set.

Skipping this step is harmless for the release itself (the per-module
tags from Step 6 are what downstream consumers resolve against), but
it leaves root referencing the previous release and Renovate will
open redundant first-party bump PRs to close the gap — see #330 /
#331 against 0.0.47 for the symptom this step exists to prevent.

## Step 8 — External smoke test

Confirm the published Go module tags resolve cleanly for a fresh
downstream consumer:

```sh
tmp=$(mktemp -d)
cd "$tmp"
go mod init smoke
GOWORK=off go get github.com/invakid404/baml-rest/dynclient@v0.0.N
GOWORK=off go list -deps github.com/invakid404/baml-rest/dynclient
```

The `GOWORK=off` is important — without it, a stray
`~/go.work` covering this repo would resolve `dynclient` through
the local workspace instead of through the just-published tag, and
the smoke test would silently pass even if the tag chain were broken.

## Consuming dynclient

Downstream Go consumers add dynclient with:

```sh
go get github.com/invakid404/baml-rest/dynclient@v0.0.N
```

The other six published modules (`adapters/common`, `bamlutils`,
`dynclient/baml-patched`, `introspected`, `worker`, `workerplugin`)
are tagged together so that anything reachable from `dynclient`'s
require graph resolves at the same release.
