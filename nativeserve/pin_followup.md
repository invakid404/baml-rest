# Out-of-`go.work` first-party pin follow-up

This file is the TRACKED record of whether the five first-party pseudo-version
selections below point at a **master** commit. **They DO.** They name
`56d5473a1bdb`, the MASTER squash-merge commit of the ExecBridge-U1c PR (#713), which
carries the ExecBridge-U1c guarded-tree change (the live-oracle standard composite:
`nativeserve/spine.CallWithOracle` + `NewPopulationExecutor`,
`nativeserve/admission.AdmitStaticSpineOracleClaim`, the `nativeserve/staticoracle`
same-response core, and `nanollmprepare`'s `standardspineoracle` composite + generated
`NewExecutor`). That commit is master-reachable, so the pins are **MASTER-DURABLE** and
this record is **STATUS: RESOLVED**. The post-squash re-pin runbook in the last section
has been PERFORMED — it was the U1c branch pin's mandatory follow-up, now complete.

U1c changes non-test source under BOTH guarded trees — `nativeserve` (the
oracle-capable spine `CallWithOracle` + `NewPopulationExecutor` sharing `Call`'s
core, the live-oracle admission entry `AdmitStaticSpineOracleClaim`, and the
factored same-response `nativeserve/staticoracle` core both the canary static serve
path and the spine oracle reuse) and `internal/nativebody/nanollmprepare` (the
standard-only `standardspineoracle` composite, the generated aggregate's `NewExecutor`,
the `serveProfileOptions()` factory swap, and the isolated go.mod pin bump) — plus the
opaque worker tar and root-module build/generation code. The pins must therefore name a
commit that carries THIS change: an external consumer resolving nativeserve off the
branch tip needs a snapshot whose five first-party selections are self-consistent with
the U1c source. Unlike U1b, the U1c nativeserve source DEPENDS on a NEW `bamlutils`
symbol — the neutral `NativeSpineUnaryOracleExecutor` / `NativeSpineUnaryOracleResult`
contract — so a consumer resolving a PRE-U1c `bamlutils` fails to compile this module:
the lockstep is a build precondition, not cosmetic, and the set must move together.

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
STATUS: RESOLVED
PINNED-COMMIT: 56d5473a1bdb
PINNED-STAMP: 20260903134144
REACHABLE-FROM: master
SLICE: ExecBridge-U1c — live-oracle standard composite: default-select the exact U1 structural population through the generated spine under a BAML plan-compare + same-bytes oracle (nativeserve/spine CallWithOracle + NewPopulationExecutor sharing Call's core; nativeserve/admission AdmitStaticSpineOracleClaim; nativeserve/staticoracle same-response core; nanollmprepare standardspineoracle composite + generated NewExecutor + serveProfileOptions factory swap; profile-neutral debamlnativespinegenerated tag compiled into both native-capable workers)
PR: #713 — squash-merged to master as 56d5473a1bdb; post-squash re-pin to the U1c master squash commit PERFORMED
```

## Why the pins name the U1c master squash commit

`56d5473a1bdb` is the master squash-merge commit of PR #713 that carries the
guarded-tree change. The packaged tar (`cmd/build/nativeworker_module.tar`) embeds both
out-of-work modules' source AND their go.mods, so the pins the tar ships are the pins an
external `nativeserve-goget` consumer resolves. Pinning `nativeserve` / `nanollmprepare`
to a PRE-U1c commit would ship a snapshot whose bamlutils lacks the
`NativeSpineUnaryOracleExecutor` contract the U1c nativeserve source links, so the
packaged worker could not be assembled from it. The pins name the commit that has the
source, and the selection is a lockstep set.

They named a BRANCH commit (`ae3900c1a0ff`) only while no master commit carried U1c; now
that PR #713 has squash-merged, all five are re-pinned to the master squash. The Slice
7.1b failure (#655) is what skipping this post-squash re-pin would have cost — a branch
pin went red on `nativeserve-goget` the moment the branch was deleted — which is why the
re-pin to the master squash commit was the MANDATORY, IMMEDIATE follow-up after merge
(the runbook below, now performed).

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
| 1 | `nativeserve/go.mod` | `github.com/invakid404/baml-rest` | `v0.0.0-20260903134144-56d5473a1bdb` |
| 2 | `nativeserve/go.mod` | `github.com/invakid404/baml-rest/bamlutils` | `v0.0.49-0.20260903134144-56d5473a1bdb` |
| 3 | `nativeserve/go.mod` | `github.com/invakid404/baml-rest/worker` | `v0.0.49-0.20260903134144-56d5473a1bdb` |
| 4 | `internal/nativebody/nanollmprepare/go.mod` | `github.com/invakid404/baml-rest/bamlutils` | `v0.0.49-0.20260903134144-56d5473a1bdb` |
| 5 | `internal/nativebody/nanollmprepare/go.mod` | `github.com/invakid404/baml-rest/worker` | `v0.0.49-0.20260903134144-56d5473a1bdb` |

`internal/nativebody/nanollmprepare/go.mod`'s `github.com/invakid404/baml-rest v0.0.48`
is deliberately NOT in this list: it is a released tag, not a pseudo-version tracking a
commit, and the module directory-replaces it.

## What was done for THIS master re-pin (the executed steps)

The re-pin below points all five selections at the U1c MASTER squash commit and sets the
record `RESOLVED`; the branch pin's OWED follow-up is now PERFORMED.

1. **All five selections re-pointed together** to `56d5473a1bdb` (Go-formula stamp
   `20260903134144`). The edit touched only `require` lines; `nanollmprepare`'s
   deliberate `baml-rest v0.0.48` (a released TAG) is untouched, and so is every SHA
   inside the historical prose.
2. **Both `// PIN-STATUS` markers flipped** from `OUTSTANDING` to `RESOLVED`, one per
   manifest.
3. **Both mirrored narratives rewritten** to MASTER-DURABLE naming the U1c master squash
   commit `56d5473a1bdb`, with the branch-only `ae3900c1a0ff` sentences demoted to
   HISTORICAL.
4. **This file updated** — fenced record (`RESOLVED`, the U1c master commit/stamp,
   `REACHABLE-FROM: master`), opening claim, selections table, this section.
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

## The follow-up — PERFORMED (the post-squash re-pin RUNBOOK, executed after merge)

**This has been done for U1c: the pins are MASTER-DURABLE** at `56d5473a1bdb`. What made
the post-squash re-pin MANDATORY and IMMEDIATE after merge is the same failure mode as
always: a squash flattens the branch source commit out of history and the branch is
deleted, so until the re-pin lands the five selections would name a commit that resolves
to nothing. The ordered steps below are the runbook the ORCHESTRATOR followed once the
U1c PR (#713) squash-merged; it is retained as the executed record and as the template
for the next slice.

### 0. Get the durable commit and its stamp — from Go, not by hand

Take the SHA of the **master squash-merge commit** of the U1c PR (not the branch tip,
which the squash flattens away) and confirm the SHA-to-stamp pair Go itself computes:

```bash
SHA=<master squash-merge of the U1c PR>
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

DONE for the U1c BRANCH pin (the prior change, superseded by the master re-pin below):

- [x] point all five selections at the U1c SOURCE commit `ae3900c1a0ff` (Go-formula
      stamp `20260903095054`), each with its correct base version
- [x] set both `// PIN-STATUS` markers + this file to `OUTSTANDING`,
      `REACHABLE-FROM: feat/debaml-execbridge-u1c`
- [x] rewrite both narratives to branch-only; demote the U1b `8fe27577082c` text to
      `HISTORICAL, SUPERSEDED`
- [x] `cmd/build/nativeworker_module.tar` regenerated and
      `internal/codegenspine/guard.json` re-baselined
- [x] tar freshness, `./cmd/build/...` (incl. `TestFirstPartyPinFollowupIsTracked`),
      codegenspine guard green

DONE post-squash for the U1c MASTER re-pin (THIS change, after PR #713 squash-merged):

- [x] re-point all five selections to the U1c MASTER squash commit `56d5473a1bdb`
      (Go-formula stamp `20260903134144`), each with its correct base version
- [x] flip both `// PIN-STATUS` markers + this file to `RESOLVED`,
      `REACHABLE-FROM: master`
- [x] rewrite both narratives to master-durable naming `56d5473a1bdb`; demote the
      branch-only `ae3900c1a0ff` text to `HISTORICAL, SUPERSEDED`
- [x] `cmd/build/nativeworker_module.tar` regenerated and
      `internal/codegenspine/guard.json` re-baselined
- [x] pin/tar/guard gates green (`TestFirstPartyPinFollowupIsTracked` now sees a
      master-reachable pin ⇒ RESOLVED); `nativeserve-goget` runs in CI against master

Precedent: #677 → #678, #681 → #682, #683 → #684, #686 → #687, #689 → #692, #703,
U1's #708 → #709, U1b's #711 post-squash re-pin, and U1c's #713 post-squash re-pin (this
change) are the instances of this runbook being executed correctly; Slice 7.1b (#655) is
what skipping it costs — a branch pin went red on `nativeserve-goget` the moment the
branch was deleted.
