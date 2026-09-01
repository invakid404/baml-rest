# Out-of-`go.work` first-party pin follow-up

This file is the TRACKED record of whether the five first-party pseudo-version
selections below point at a **master** commit. **They DO NOT — yet.** They name
`9a67ca3741a7`, the U1b **SOURCE** commit on `feat/debaml-execbridge-u1b` that
carries the ExecBridge-U1b guarded-tree change (the native-only packaged worker:
a reusable `nativeserve/spine.NewWorkerRuntime` factory + population classifier,
and the BAML-free `nanollmprepare` bootstrap / `cmd/worker-nativeonly` entrypoint).
That commit is NOT on master, so the pins are **BRANCH-ONLY** and this record is
**STATUS: OUTSTANDING**. The post-squash re-pin runbook in the last section is
therefore **OWED** — it is the mandatory follow-up once U1b squash-merges.

U1b changes non-test source under BOTH guarded trees — `nativeserve` (the
reusable native-only runtime factory + the single population classifier + the
promoted pure adapter; `nativeserve/spine` now returns a `worker.Runtime`) and
`internal/nativebody/nanollmprepare` (the `nativeonlyboot` bootstrap, the
`cmd/worker-nativeonly` entrypoint, the committed `nativegenerated/generated_off.go`
stub, and the isolated go.mod pin bump) — plus the opaque worker tar and
root-module build/generation code. The pins must therefore name a commit that
carries THIS change: an external consumer resolving nativeserve off the branch
tip needs a snapshot whose five first-party selections are self-consistent with
the U1b source. The U1b nativeserve source still compiles against the U1
`bamlutils`/`worker` contract (U1b introduces no new root symbol), but the
guarded-tree source moved, so the lockstep set must move with it.

It is proof material, not documentation. `TestFirstPartyPinFollowupIsTracked`
(`cmd/build/nativeworker_pins_test.go`) parses it on every ordinary `go test ./...`,
cross-checks it against the real `go.mod` files (LOCKSTEP: all five agree;
FRESHNESS: the recorded commit/stamp match the require directives), and — wherever a
`master` ref is resolvable — requires the recorded status to agree with actual
master-reachability (ANCESTRY: a branch-only commit is NOT master-reachable, so the
status must be `OUTSTANDING`). So the follow-up cannot be quietly forgotten: while
the pins name a branch commit this file must read `OUTSTANDING`, and once the re-pin
lands on master it must be flipped to `RESOLVED`.

`nativeserve/go.mod`'s BUMP RULE header states the general rule. This file is the
CONCRETE, per-change instance of it, which is what the generic comment cannot be.

```text
STATUS: OUTSTANDING
PINNED-COMMIT: 9a67ca3741a7
PINNED-STAMP: 20260901184028
REACHABLE-FROM: feat/debaml-execbridge-u1b
SLICE: ExecBridge-U1b — native-only packaged worker that boots + serves the ExecBridge-U1 exact-JSON cohort with ZERO BAML/CFFI in its runtime graph (nativeserve/spine.NewWorkerRuntime factory + single population classifier + promoted pure adapter; nanollmprepare nativeonlyboot bootstrap + cmd/worker-nativeonly entrypoint + committed nativegenerated stub; cmd/build --native-only-worker selector + generation join + native-only overlay + dependency gate)
PR: feat/debaml-execbridge-u1b (unmerged; branch-only pin — post-squash re-pin to the U1b master squash commit is OWED)
```

## Why the pins name the U1b branch commit

`9a67ca3741a7` is the U1b SOURCE commit that carries the guarded-tree change. The
packaged tar (`cmd/build/nativeworker_module.tar`) embeds both out-of-work modules'
source AND their go.mods, so the pins the tar ships are the pins an external
`nativeserve-goget` consumer resolves off the branch. Pinning `nativeserve` /
`nanollmprepare` to a PRE-U1b commit would ship a snapshot whose nativeserve lacks
the `NewWorkerRuntime` factory (and whose nanollmprepare lacks `nativeonlyboot` /
`cmd/worker-nativeonly`), so the packaged native-only worker could not be assembled
from it. The pins name the commit that has the source, and the selection is a
lockstep set.

The pins name a BRANCH commit only because no master commit carries U1b yet. The
Slice 7.1b failure (#655) is what skipping the master re-pin costs — a branch pin
went red on `nativeserve-goget` the moment the branch was deleted — which is why the
re-pin to the master squash commit is mandatory and immediate post-merge.

The emitted/runtime spine path itself contains no generated BAML and no BAML CFFI —
proven mechanically by `go list -deps` over the production factory package
(`nativeserve/spine`) and by the whole-command gate over the packaged native-only
command (`TestNativeOnlyWorkerHasNoBAML`), rejecting `baml_client` /
`github.com/boundaryml/baml` / `dynclient` / `language_client_go` / `rootruntime` /
`introspected` / `workerboot` / the root generated package. The nanollm FFI is the
intended native provider engine and is permitted; it is not BAML CFFI.

## The five pinned selections

Every one of them moves together. They are directory-`replace`d for local
development, so only the version STRINGS reach MVS — which is precisely why a partial
bump is invisible until the out-of-work packaging build fails with
`updates to go.mod needed`.

| # | file | module | current selection |
| --- | --- | --- | --- |
| 1 | `nativeserve/go.mod` | `github.com/invakid404/baml-rest` | `v0.0.0-20260901184028-9a67ca3741a7` |
| 2 | `nativeserve/go.mod` | `github.com/invakid404/baml-rest/bamlutils` | `v0.0.49-0.20260901184028-9a67ca3741a7` |
| 3 | `nativeserve/go.mod` | `github.com/invakid404/baml-rest/worker` | `v0.0.49-0.20260901184028-9a67ca3741a7` |
| 4 | `internal/nativebody/nanollmprepare/go.mod` | `github.com/invakid404/baml-rest/bamlutils` | `v0.0.49-0.20260901184028-9a67ca3741a7` |
| 5 | `internal/nativebody/nanollmprepare/go.mod` | `github.com/invakid404/baml-rest/worker` | `v0.0.49-0.20260901184028-9a67ca3741a7` |

`internal/nativebody/nanollmprepare/go.mod`'s `github.com/invakid404/baml-rest v0.0.48`
is deliberately NOT in this list: it is a released tag, not a pseudo-version tracking a
commit, and the module directory-replaces it.

## What was done for THIS branch pin (the executed steps)

The BRANCH pin below points all five selections at the U1b source commit and leaves
the record `OUTSTANDING`; the post-squash re-pin to a MASTER commit (the runbook in
the next section) is the OWED follow-up.

1. **All five selections pointed together** to `9a67ca3741a7`. The edit touched only
   `require` lines; `nanollmprepare`'s deliberate `baml-rest v0.0.48` (a released
   TAG) is untouched, and so is every SHA inside the historical prose.
2. **Both `// PIN-STATUS` markers set** to `OUTSTANDING`, one per manifest.
3. **Both mirrored narratives rewritten** to BRANCH-ONLY, with the prior U1
   `RESOLVED`/`7ddbb39fd3db` sentences demoted into each manifest's
   `HISTORICAL, SUPERSEDED` paragraph.
4. **This file rewritten** — fenced record (`OUTSTANDING`, the U1b commit/stamp,
   `REACHABLE-FROM` naming the branch), opening claim, selections table, this
   section, and the runbook below.
5. **Tar regenerated** (`go run ./cmd/build/gen-nativeworker-src`), which is required
   because it embeds both manifests, followed by the codegen-spine guard re-baseline
   (`go test ./internal/codegenspine/ -run TestSourceGuard -update-codegenspine-guard`)
   — the guard hashes the tar and the five pins, so it re-baselines in the same change.
6. **Gates re-run** — pin lockstep, packaged-manifest identity, tar freshness, source
   guard, out-of-work build.

A note on TERMINOLOGY, because the two vocabularies differ. The machine-readable
marker takes exactly `OUTSTANDING` or `RESOLVED`: `pinFollowupViolations` rejects
anything else, and the ANCESTRY clause compares master-reachability against those two
literals. "Durable" is the PROSE word for the `RESOLVED` state; `RESOLVED` is what the
guards read.

## The follow-up — OWED (the post-squash re-pin RUNBOOK, run after merge)

**This is NOT yet done for U1b: the pins are BRANCH-ONLY** at `9a67ca3741a7`, and the
re-pin to the U1b master squash commit is OWED. What makes it MANDATORY and IMMEDIATE
after merge is the same failure mode as always: a squash flattens the branch source
commit out of history and the branch is deleted, so until the re-pin lands the five
selections would name a commit that resolves to nothing.

### 0. Get the durable commit and its stamp — from Go, not by hand

Take the SHA of the **master squash-merge commit** of the U1b PR (not the branch tip,
which the squash flattens away) and confirm the SHA-to-stamp pair Go itself computes:

```bash
SHA=<master squash-merge of the U1b PR>
for m in github.com/invakid404/baml-rest \
         github.com/invakid404/baml-rest/bamlutils \
         github.com/invakid404/baml-rest/worker; do
  GOWORK=off GOPRIVATE=github.com/invakid404/baml-rest GOFLAGS= \
    go mod download -json "$m@$SHA" | grep '"Version"'
done
```

Use the versions this prints verbatim. Do NOT hand-compute the `<stamp>`: a timestamp
off by one second yields a pseudo-version that resolves to nothing, and the failure
surfaces far from the edit. Two BASE forms, each selection keeping its own base:

- root module: `v0.0.0-<stamp>-<sha12>`
- `bamlutils` and `worker`: `v0.0.49-0.<stamp>-<sha12>` (a DOT after the `-0`, not a dash)

### 1. Re-point all FIVE selections, together

| file | modules to re-point |
| --- | --- |
| `nativeserve/go.mod` | `github.com/invakid404/baml-rest`, `.../bamlutils`, `.../worker` |
| `internal/nativebody/nanollmprepare/go.mod` | `.../bamlutils`, `.../worker` |

A partial bump is invisible locally — both modules directory-`replace` these paths, so
only the version STRINGS reach MVS — and fails later in the out-of-work packaging build
with `updates to go.mod needed`.

### 2. Flip BOTH machine-readable markers

Set the `// PIN-STATUS:` line in **each** manifest from `OUTSTANDING` to `RESOLVED`
(`nativeserve/go.mod` and `internal/nativebody/nanollmprepare/go.mod`).
`cmd/build`'s `TestPackagedManifestsMatchTheTrackedPins` requires each manifest to
carry exactly ONE marker and to agree with this file's status, inside the packaged tar
as well as in the tree.

### 3. Flip BOTH mirrored manifest NARRATIVES

Each manifest carries a prose paragraph describing where the pins stand:
`NOTE (ExecBridge-U1b ...)` in `nativeserve/go.mod`, and `RIGHT NOW they are ...` in
`internal/nativebody/nanollmprepare/go.mod`. BOTH must be rewritten to say the pins are
MASTER-durable and to name the master squash commit, with the branch-only sentences
demoted into the `HISTORICAL, SUPERSEDED` paragraph. No test covers this, but the
markers are what the GUARDS read and the narratives are what a HUMAN reads.

### 4. Update THIS file

- the recorded status to `RESOLVED`, the pinned commit and stamp to the new values
  from step 0, `REACHABLE-FROM:` to `master`, `PR:` to the merged PR number and the
  master squash SHA;
- the opening paragraph to say they DO name a master commit and the follow-up is
  PERFORMED; this section's heading and tense rewritten to match;
- the "current selection" column of the five-selections table to the new versions.

### 5. Regenerate the packaged worker source (and re-baseline the guard)

```bash
go run ./cmd/build/gen-nativeworker-src
go test ./internal/codegenspine/ -run TestSourceGuard -update-codegenspine-guard
```

The tar embeds BOTH manifests, so it carries the pins, markers, and narratives from
steps 1-3; the codegen-spine guard hashes the tar and the five pins, so it re-baselines
in the same change.

### 6. Re-run the gates

```bash
GOWORK=off go test -run TestNativeWorkerModuleTarIsFresh ./cmd/build/   # tar freshness
go test ./cmd/build/...                                                 # incl. TestFirstPartyPinFollowupIsTracked
go run ./cmd/regenerate-dynclient && git status --porcelain             # must print NOTHING
```

`TestFirstPartyPinFollowupIsTracked` is what enforces steps 1-4 against each other and
against master-ancestry: once the pins are on a master commit it stays RED until this
file says `RESOLVED`.

Then the **`nativeserve-goget` external-consumer probe**, against the master tip that
CARRIES steps 1-5 — i.e. AFTER the re-pin has landed. It must run from a genuinely
external module (no checkout, no `replace`, no workspace) under `CGO_ENABLED=1
GOPRIVATE=github.com/invakid404/baml-rest`, `go get github.com/invakid404/baml-rest/nativeserve@<master>`
then `go build ./... && go run ./...`; CHECK the probe's own `go.mod` resolves the NEW
pseudo-versions from step 0.

### Definition of done

DONE for the U1b BRANCH pin (this change):

- [x] point all five selections at the U1b SOURCE commit `9a67ca3741a7` (Go-formula
      stamp `20260901184028`), each with its correct base version
- [x] set both `// PIN-STATUS` markers + this file to `OUTSTANDING`,
      `REACHABLE-FROM: feat/debaml-execbridge-u1b`
- [x] rewrite both narratives to branch-only; demote the U1 `7ddbb39fd3db` text to
      `HISTORICAL, SUPERSEDED`
- [x] `cmd/build/nativeworker_module.tar` regenerated and
      `internal/codegenspine/guard.json` re-baselined
- [x] tar freshness, `./cmd/build/...` (incl. `TestFirstPartyPinFollowupIsTracked`),
      codegenspine guard green

OWED post-squash for the U1b MASTER re-pin:

- [ ] re-point all five selections to the U1b MASTER squash commit, flip both markers
      + this file to `RESOLVED`, `REACHABLE-FROM: master`, regenerate the tar +
      re-baseline the guard, and rerun all gates incl. `nativeserve-goget`

Precedent: #677 → #678, #681 → #682, #683 → #684, #686 → #687, #689 → #692, #703, and
U1's #708 → #709 are the prior instances of this runbook being executed correctly;
Slice 7.1b (#655) is what skipping it costs — a branch pin went red on
`nativeserve-goget` the moment the branch was deleted.
