# Release Procedure

> **DO NOT SKIP RELEASE-PREP.** Tagging before the first-party `require`
> statements have been bumped to `v0.0.X` in the merged commit will cause
> `scripts/release-go-tags.sh` to reject the Go module tags, and recovery
> requires force-moving the product tag (already pushed and possibly already
> attached to a GitHub Release) — see #308.

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

The seven published modules have first-party `require` edges between
each other. During day-to-day development they point at
`v0.0.0-00010101000000-000000000000` placeholders and resolve through
local `replace` directives in each `go.mod`. Before the product tag
is cut, those placeholders must be rewritten to the actual `v0.0.N`
that is about to be released, so that `go get` from a downstream
consumer resolves through real tags rather than pseudo-versions.

Affected files and edges:

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

Copy-pasteable rewrite (substitute the release number into `VERSION`
before running):

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
```

Notes:

- **Do not remove the local `replace` directives.** They are ignored
  by downstream consumers when these modules are imported as
  dependencies, and they keep local development ergonomic (the
  workspace builds without needing tags to exist first). Keeping
  them in the release commit is intentional.
- **Do not rewrite the server-only `adapters/adapter_v*` modules.**
  They are not part of the dynclient release graph and are not in
  PR C's tag set. Their existing pins are managed by `scripts/sync.sh`
  and the BAML pin-matrix verifier.

## Step 2 — Verify and commit

```sh
go build ./...
go vet ./...
go test ./... -count=1
(cd dynclient && GOWORK=off go test ./... -count=1)
git status
git commit -am "chore(release): prepare Go module versions for 0.0.N"
```

The extra `GOWORK=off` step inside `dynclient/` verifies that the
module also builds and tests against the rewritten requires when the
workspace is not in play — which is the configuration downstream
consumers will see.

## Step 3 — Push to master

Push the release-prep commit:

```sh
git push origin master
```

## Step 4 — Create and push the product tag

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

## Step 7 — External smoke test

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
