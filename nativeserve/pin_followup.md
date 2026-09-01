# Out-of-`go.work` first-party pin follow-up

This file is the TRACKED record of whether the five first-party pseudo-version
selections below point at a **master** commit. **They DO.** They name
`7ddbb39fd3db`, the MASTER squash-merge commit of PR #708 that carries the
ExecBridge-U1 guarded-tree change (a production native unary executor / population bridge).
That commit is on master, so the pins are **MASTER-DURABLE** and this record is
**STATUS: RESOLVED**. The post-squash re-pin runbook in the last section has been
**PERFORMED** — this is the durable delivery.

For historical context: U1 changes non-test source under the
guarded `nativeserve` tree (a new BAML-free spine unary serve lane), and that serve core
is built on the NEW neutral contract in `bamlutils` — `NativeSpineUnaryExecutor` /
`NativeSpineUnaryBinding` and the tri-state result. (The scalar-descriptor reconstruction
the runtime also needs now lives INSIDE `nativeserve/spine` itself, as the unexported
`reconstructFunction`, so the production serve package stays a thin lane free of the
codegen toolchain — it is carried by the pinned commit, not the pinned root.) A consumer
resolving a PRE-U1 pin gets a `bamlutils` that lacks that contract, so the pinned commit
MUST carry the U1 source — which the master squash commit `7ddbb39fd3db` does. The pins
briefly named the branch commit `f35489fc2463` while no master commit carried the change;
the re-pin to the master squash commit is what made delivery durable, and it is now PERFORMED.

It is proof material, not documentation. `TestFirstPartyPinFollowupIsTracked`
(`cmd/build/nativeworker_pins_test.go`) parses it on every ordinary `go test ./...`,
cross-checks it against the real `go.mod` files (LOCKSTEP: all five agree; FRESHNESS: the
recorded commit/stamp match the require directives), and — wherever a `master` ref is
resolvable — requires the recorded status to agree with actual master-reachability
(ANCESTRY: a branch-only commit is NOT master-reachable, so the status must be `OUTSTANDING`).
So the follow-up cannot be quietly forgotten: while the pins name a branch commit this file
must read `OUTSTANDING`, and once the re-pin lands on master it must be flipped to `RESOLVED`.

`nativeserve/go.mod`'s BUMP RULE header states the general rule. This file is the
CONCRETE, per-change instance of it, which is what the generic comment cannot be.

```text
STATUS: RESOLVED
PINNED-COMMIT: 7ddbb39fd3db
PINNED-STAMP: 20260901085119
REACHABLE-FROM: master
SLICE: ExecBridge-U1 — production native unary executor / population bridge for the exact five-arm direct JSON recursive alias (neutral bamlutils binding/executor contract, emitted scalar projector + strict decoder, nativeserve/spine's unexported reconstructFunction, nativeserve/spine executor + nativeserve/admission.AdmitStaticSpineClaim); no generated BAML or CFFI on the emitted/runtime path
PR: #708 (squash-merged to master as 7ddbb39fd3db); all five pins re-pointed to that master commit — the post-squash re-pin is PERFORMED
```

## Why the pins name the U1 master commit

`7ddbb39fd3db` is the MASTER squash-merge commit of PR #708 that carries the U1
guarded-tree change. The packaged serve core is built directly on top of the NEW U1 contract
in `bamlutils`, so the pinned modules decide whether the serve core even compiles:
`nativeserve/spine` imports `bamlutils.NativeSpineUnaryExecutor` / `NativeSpineUnaryBinding`
(the neutral binding/executor contract + tri-state result, new in this slice), and
`nativeserve/admission.AdmitStaticSpineClaim` reuses the shared static admission building
blocks minus the BAML plan-compare oracle. The scalar-descriptor reconstruction the runtime
also needs is NOT a root symbol — it lives inside `nativeserve/spine` itself (the unexported
`reconstructFunction`), so it rides in the pinned commit rather than the pinned root, and keeps
the production serve package free of the codegen toolchain. Pinning `bamlutils` to any PRE-U1
commit would resolve a module that lacks the new contract, so the out-of-work packaging build
(`-mod=readonly`) and the external consumer would both fail to compile the serve core. The
pins named the branch source commit `f35489fc2463` only while no master commit carried the U1
source; now that PR #708 has squash-merged, the pins name the master commit `7ddbb39fd3db` and
delivery is durable — the post-squash re-pin has been PERFORMED.

The emitted/runtime spine path itself contains no generated BAML and no BAML CFFI — proven
mechanically by `go list -deps` over every emitted hermetic module
(`internal/nativespinefixture`, `internal/nativespinejsonfixture`) and the production bridge
package (`nativeserve/spine`), rejecting `baml_client` / `github.com/boundaryml/baml` /
`dynclient/baml-patched` / `language_client_go`. The nanollm FFI is the intended native
provider engine and is permitted; it is not BAML CFFI.

**What a green `nativeserve-goget` proves on which run.** On a `pull_request` it resolves the
PR HEAD SHA; on `push` to master it resolves the master tip. Now that the pins name the MASTER
commit `7ddbb39fd3db`, the `push`-to-master durability run is meaningful: it resolves the five
pins from origin off the master tip, which survives the deletion of `feat/debaml-execbridge-u1`.
A branch-only pin going red on the master `push` run the instant the branch is deleted is the
Slice 7.1b failure this record exists to prevent, and the reason this re-pin was mandatory and
immediate post-merge — it has now been PERFORMED.

## The five pinned selections

Every one of them moves together. They are directory-`replace`d for local
development, so only the version STRINGS reach MVS — which is precisely why a partial
bump is invisible until the out-of-work packaging build fails with
`updates to go.mod needed`.

| # | file | module | current selection |
| --- | --- | --- | --- |
| 1 | `nativeserve/go.mod` | `github.com/invakid404/baml-rest` | `v0.0.0-20260901085119-7ddbb39fd3db` |
| 2 | `nativeserve/go.mod` | `github.com/invakid404/baml-rest/bamlutils` | `v0.0.49-0.20260901085119-7ddbb39fd3db` |
| 3 | `nativeserve/go.mod` | `github.com/invakid404/baml-rest/worker` | `v0.0.49-0.20260901085119-7ddbb39fd3db` |
| 4 | `internal/nativebody/nanollmprepare/go.mod` | `github.com/invakid404/baml-rest/bamlutils` | `v0.0.49-0.20260901085119-7ddbb39fd3db` |
| 5 | `internal/nativebody/nanollmprepare/go.mod` | `github.com/invakid404/baml-rest/worker` | `v0.0.49-0.20260901085119-7ddbb39fd3db` |

`internal/nativebody/nanollmprepare/go.mod`'s `github.com/invakid404/baml-rest v0.0.48`
is deliberately NOT in this list: it is a released tag, not a pseudo-version tracking a
commit, and the module directory-replaces it.

## What was done for THIS master re-pin (the executed steps)

The runbook below is the post-squash re-pin to a MASTER commit. It has now been executed
against the master squash commit `7ddbb39fd3db`, flipping the record to `RESOLVED`. (U1's
BRANCH pin earlier executed the same steps against the branch source commit `f35489fc2463`
and left the record `OUTSTANDING`; that is the state this re-pin supersedes.)

0. **Stamp resolved by Go, off the origin — never hand-computed.** With PR #708 squash-merged
   to master `7ddbb39fd3db`, `GOWORK=off GOPRIVATE=github.com/invakid404/baml-rest GOFLAGS=
   go mod download -json <mod>@7ddbb39fd3db` was run for the root, `bamlutils` and `worker`,
   and Go returned `v0.0.0-20260901085119-7ddbb39fd3db` for the root and
   `v0.0.49-0.20260901085119-7ddbb39fd3db` for the other two — each keeping its own base
   version. Those strings are used verbatim.
1. **All five selections re-pointed together** to `7ddbb39fd3db`. The edit touched only
   `require` lines: `nanollmprepare`'s deliberate `baml-rest v0.0.48` (a released TAG, not a
   pseudo-version) is untouched, and so is every SHA inside the historical prose.
2. **Both `// PIN-STATUS` markers flipped** `OUTSTANDING` -> `RESOLVED`, one per manifest.
3. **Both mirrored narratives rewritten** to MASTER-DURABLE, with the prior branch-only U1
   sentences demoted into each manifest's `HISTORICAL, SUPERSEDED` paragraph.
4. **This file rewritten** — fenced record (`RESOLVED`, the new commit/stamp,
   `REACHABLE-FROM: master`), opening claim, selections table, this section, and the
   Definition-of-done checklist below.
5. **Tar regenerated** (`go run ./cmd/build/gen-nativeworker-src`), which is required because
   it embeds both manifests, followed by the codegen-spine guard re-baseline
   (`go test ./internal/codegenspine/ -run TestSourceGuard -update-codegenspine-guard`) — the
   guard hashes the tar and the five pins, so it re-baselines in the same change.
6. **Gates re-run** — see "Definition of done".

A note on TERMINOLOGY, because the two vocabularies differ. The machine-readable marker
takes exactly `OUTSTANDING` or `RESOLVED`: `pinFollowupViolations` rejects anything else,
and the ANCESTRY clause compares master-reachability against those two literals. "Durable"
is the PROSE word for the `RESOLVED` state; `RESOLVED` is what the guards read.

## The follow-up — PERFORMED (the post-squash re-pin RUNBOOK, executed after merge)

**This HAS now been done for U1: the pins are MASTER-DURABLE** at `7ddbb39fd3db` and the
re-pin to the master squash commit has been PERFORMED. "What was done for THIS master re-pin"
above records the steps that were executed against the master commit; the runbook here is that
same procedure, kept in full and in the imperative because it is the reusable sequence — it has
been executed eight times now (#678, #682, #684, #687, #692, #703, #708 and — post-merge — this
one, PR #709), and every time the value came from following it literally rather than from
remembering it.

What makes it MANDATORY and IMMEDIATE after merge is the same failure mode as always: a squash
flattens the branch source commit out of history and the branch is deleted, so until the
re-pin lands the five selections would name a commit that resolves to nothing.

### 0. Get the durable commit and its stamp — from Go, not by hand

Take the SHA of the **master squash-merge commit** of the PR (not the branch tip, which the
squash flattens away) and confirm the SHA-to-stamp pair Go itself computes:

```bash
SHA=<master squash-merge of the PR>
for m in github.com/invakid404/baml-rest \
         github.com/invakid404/baml-rest/bamlutils \
         github.com/invakid404/baml-rest/worker; do
  GOWORK=off GOPRIVATE=github.com/invakid404/baml-rest GOFLAGS= \
    go mod download -json "$m@$SHA" | grep '"Version"'
done
```

Use the versions this prints verbatim. Do NOT hand-compute the `<stamp>`: a timestamp off
by one second yields a pseudo-version that resolves to nothing, and the failure surfaces
far from the edit.

Two BASE forms, each selection keeping its own base:

- root module: `v0.0.0-<stamp>-<sha12>`
- `bamlutils` and `worker`: `v0.0.49-0.<stamp>-<sha12>` (a DOT after the `-0`, not a dash)

### 1. Re-point all FIVE selections, together

The five are listed in "The five pinned selections" above. Move them in one edit:

| file | modules to re-point |
| --- | --- |
| `nativeserve/go.mod` | `github.com/invakid404/baml-rest`, `.../bamlutils`, `.../worker` |
| `internal/nativebody/nanollmprepare/go.mod` | `.../bamlutils`, `.../worker` |

A partial bump is invisible locally — both modules directory-`replace` these paths, so
only the version STRINGS reach MVS — and fails later in the out-of-work packaging build
with `updates to go.mod needed`.

### 2. Flip BOTH machine-readable markers

Set the `// PIN-STATUS:` line in **each** manifest from `OUTSTANDING` to `RESOLVED`:

- `nativeserve/go.mod`
- `internal/nativebody/nanollmprepare/go.mod`

`cmd/build`'s `TestPackagedManifestsMatchTheTrackedPins` requires each manifest to carry
exactly ONE marker and to agree with this file's `STATUS`, inside the packaged tar as well
as in the tree.

### 3. Flip BOTH mirrored manifest NARRATIVES

Each manifest also carries a prose paragraph describing where the pins stand:
`NOTE (de-BAML /parse UNION-RESIDUAL batch 2): ...` in `nativeserve/go.mod`, and
`RIGHT NOW they are ...` in `internal/nativebody/nanollmprepare/go.mod`. BOTH must be
rewritten to say the pins are MASTER-durable and to name the master squash commit, and the
branch-only sentences demoted into the `HISTORICAL, SUPERSEDED` paragraph beneath them.

This step is not cosmetic and no test covers it: the markers are what the GUARDS read, the
narratives are what a HUMAN reads, and a narrative saying "BRANCH-ONLY" under a marker
saying `RESOLVED` tells a reviewer the opposite of the truth. That exact drift happened
once — the S1 text was left in place after the S2 bump — which is why both manifests now
carry "Do not treat this comment as the authority".

### 4. Update THIS file

- the recorded status to `RESOLVED`
- the pinned commit and stamp to the new values from step 0
- `REACHABLE-FROM:` to `master`
- `PR:` to the merged PR number and the master squash SHA
- the opening paragraph to say they DO, and that the follow-up has been **performed**; this
  section's heading and tense rewritten to match
- the "current selection" column of the five-selections table to the new versions

### 5. Regenerate the packaged worker source (and re-baseline the guard)

```bash
go run ./cmd/build/gen-nativeworker-src
go test ./internal/codegenspine/ -run TestSourceGuard -update-codegenspine-guard
```

The tar embeds BOTH manifests, so it carries the pins, the markers and the narratives from
steps 1-3; the codegen-spine guard hashes the tar and the five pins, so it re-baselines in
the same change. Skipping either would leave the shipped tar / the guard describing a pin
state the tree no longer has.

### 6. Re-run the gates — all four

```bash
GOWORK=off go test -run TestNativeWorkerModuleTarIsFresh ./cmd/build/   # tar freshness
go test ./cmd/build/...                                                # incl. TestFirstPartyPinFollowupIsTracked
go run ./cmd/regenerate-dynclient && git status --porcelain             # must print NOTHING
```

`TestFirstPartyPinFollowupIsTracked` is what enforces steps 1-4 against each other and
against master-ancestry: once the pins are on a master commit it stays RED until this file
says `RESOLVED`.

Then the **`nativeserve-goget` external-consumer probe**, against the master tip that CARRIES
STEPS 1-5 — i.e. AFTER the re-pin has landed. It must run from a genuinely external module —
no checkout, no `replace`, no workspace — under `CGO_ENABLED=1
GOPRIVATE=github.com/invakid404/baml-rest`, `go get github.com/invakid404/baml-rest/nativeserve@<master>`
then `go build ./... && go run ./...`; CHECK the probe's own `go.mod` resolves the NEW
pseudo-versions from step 0.

### Definition of done

DONE post-squash for the U1 MASTER re-pin (this change):

- [x] re-point all five selections to the MASTER squash commit `7ddbb39fd3db` (Go-resolved
      stamp `20260901085119`), each with its correct base version
- [x] flip both `// PIN-STATUS` markers + this file to `RESOLVED`, `REACHABLE-FROM: master`
- [x] rewrite both narratives to master-durable; demote the branch-only U1 text to
      `HISTORICAL, SUPERSEDED`
- [x] `cmd/build/nativeworker_module.tar` regenerated and `internal/codegenspine/guard.json`
      re-baselined
- [x] tar freshness, `./cmd/build/...` (incl. `TestFirstPartyPinFollowupIsTracked`),
      codegenspine guard green
- [x] `nativeserve-goget` resolves all five pins from origin off the master commit

Superseded — DONE earlier for the U1 BRANCH pin:

- [x] all five selections named the branch source commit `f35489fc2463` (STATUS: OUTSTANDING),
      the mid-slice state this re-pin replaces

Precedent: #677 → #678, #681 → #682, #683 → #684, #686 → #687, #689 → #692 and #703 are the
prior instances of this runbook being executed correctly; Slice 7.1b (#655) is what skipping
it costs — a branch pin went red on `nativeserve-goget` the moment the branch was deleted.

The pins now name the MASTER commit `7ddbb39fd3db`, so this file reads `STATUS: RESOLVED`,
which is what `TestFirstPartyPinFollowupIsTracked`'s ANCESTRY clause requires (a
master-reachable commit must be `RESOLVED`). The record stays in the tree as the durable
delivery.
