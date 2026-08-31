# Out-of-`go.work` first-party pin follow-up

This file is the TRACKED record of whether the five first-party pseudo-version
selections below point at a **master** commit. **They do NOT yet.** They name
`8427c6315563`, the branch SOURCE commit on `feat/debaml-execbridge-u1` that carries the
ExecBridge-U1 guarded-tree change (a production native unary executor / population bridge).
No master commit carries that change yet, so the pins are **BRANCH-ONLY** and this record is
**STATUS: OUTSTANDING**. The post-squash re-pin runbook in the last section is **OWED** — the
orchestrator drives it after this PR squash-merges to master.

This is the expected mid-slice state, not a defect: U1 changes non-test source under the
guarded `nativeserve` tree (a new BAML-free spine unary serve lane), and that serve core
is built on the NEW neutral contract in `bamlutils` — `NativeSpineUnaryExecutor` /
`NativeSpineUnaryBinding` and the tri-state result. (The scalar-descriptor reconstruction
the runtime also needs now lives INSIDE `nativeserve/spine` itself, as the unexported
`reconstructFunction`, so the production serve package stays a thin lane free of the
codegen toolchain — it is carried by the branch tip, not the pinned root.) A consumer
resolving a PRE-U1 pin gets a `bamlutils` that lacks that contract, so the pinned commit
MUST carry the U1 source — which the branch commit `8427c6315563` does and no master commit
yet does. A branch-tip pin is not final delivery; the re-pin to the master squash commit is
what makes it durable.

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
STATUS: OUTSTANDING
PINNED-COMMIT: 8427c6315563
PINNED-STAMP: 20260831140315
REACHABLE-FROM: feat/debaml-execbridge-u1 (branch source tip; NOT master)
SLICE: ExecBridge-U1 — production native unary executor / population bridge for the exact five-arm direct JSON recursive alias (neutral bamlutils binding/executor contract, emitted scalar projector + strict decoder, nativeserve/spine's unexported reconstructFunction, nativeserve/spine executor + nativeserve/admission.AdmitStaticSpineClaim); no generated BAML or CFFI on the emitted/runtime path
PR: feat/debaml-execbridge-u1 (this PR); pins name the branch source commit 8427c6315563; re-pin to the master squash commit is OWED post-merge
```

## Why the pins point at the branch source commit (not master yet)

`8427c6315563` is the branch commit on `feat/debaml-execbridge-u1` that carries the U1
guarded-tree change. The packaged serve core is built directly on top of the NEW U1 contract
in `bamlutils`, so the pinned modules decide whether the serve core even compiles:
`nativeserve/spine` imports `bamlutils.NativeSpineUnaryExecutor` / `NativeSpineUnaryBinding`
(the neutral binding/executor contract + tri-state result, new in this slice), and
`nativeserve/admission.AdmitStaticSpineClaim` reuses the shared static admission building
blocks minus the BAML plan-compare oracle. The scalar-descriptor reconstruction the runtime
also needs is NOT a root symbol — it lives inside `nativeserve/spine` itself (the unexported
`reconstructFunction`), so it rides in the branch tip rather than the pinned root, and keeps
the production serve package free of the codegen toolchain. Pinning `bamlutils` to any PRE-U1
commit would resolve a module that lacks the new contract, so the out-of-work packaging build
(`-mod=readonly`) and the external consumer would both fail to compile the serve core. No
MASTER commit carries the U1 source yet, so the pins MUST name the branch source commit; the
re-pin to the eventual master squash commit is what makes delivery durable and is OWED (the
orchestrator drives it).

The emitted/runtime spine path itself contains no generated BAML and no BAML CFFI — proven
mechanically by `go list -deps` over every emitted hermetic module
(`internal/nativespinefixture`, `internal/nativespinejsonfixture`) and the production bridge
package (`nativeserve/spine`), rejecting `baml_client` / `github.com/boundaryml/baml` /
`dynclient/baml-patched` / `language_client_go`. The nanollm FFI is the intended native
provider engine and is permitted; it is not BAML CFFI.

**What a green `nativeserve-goget` proves on which run.** On a `pull_request` it resolves the
PR HEAD SHA; on `push` to master it resolves the master tip. While the pins name a BRANCH
commit, the `pull_request` run is what proves the branch delivery resolves (the PR HEAD's
`nativeserve/go.mod` names `8427c6315563`, which is fetchable as long as the branch exists).
The `push`-to-master durability run only becomes meaningful AFTER the post-squash re-pin names
a master commit — which is exactly the OWED follow-up this record tracks. A branch-only pin
going red on the master `push` run the instant the branch is deleted is the Slice 7.1b failure
this record exists to prevent, and the reason the re-pin is mandatory and immediate post-merge.

## The five pinned selections

Every one of them moves together. They are directory-`replace`d for local
development, so only the version STRINGS reach MVS — which is precisely why a partial
bump is invisible until the out-of-work packaging build fails with
`updates to go.mod needed`.

| # | file | module | current selection |
| --- | --- | --- | --- |
| 1 | `nativeserve/go.mod` | `github.com/invakid404/baml-rest` | `v0.0.0-20260831140315-8427c6315563` |
| 2 | `nativeserve/go.mod` | `github.com/invakid404/baml-rest/bamlutils` | `v0.0.49-0.20260831140315-8427c6315563` |
| 3 | `nativeserve/go.mod` | `github.com/invakid404/baml-rest/worker` | `v0.0.49-0.20260831140315-8427c6315563` |
| 4 | `internal/nativebody/nanollmprepare/go.mod` | `github.com/invakid404/baml-rest/bamlutils` | `v0.0.49-0.20260831140315-8427c6315563` |
| 5 | `internal/nativebody/nanollmprepare/go.mod` | `github.com/invakid404/baml-rest/worker` | `v0.0.49-0.20260831140315-8427c6315563` |

`internal/nativebody/nanollmprepare/go.mod`'s `github.com/invakid404/baml-rest v0.0.48`
is deliberately NOT in this list: it is a released tag, not a pseudo-version tracking a
commit, and the module directory-replaces it.

## What was done for THIS branch pin (the executed steps)

The runbook below is written for the post-squash re-pin to a MASTER commit (the OWED
follow-up). U1's BRANCH pin executed the same steps against the branch source commit
`8427c6315563` instead, and left the record `OUTSTANDING`:

0. **Stamp resolved by Go, off the origin — never hand-computed.** After the source commit was
   pushed to the branch, `GOWORK=off GOPRIVATE=github.com/invakid404/baml-rest GOPROXY=direct
   go mod download -json <mod>@8427c6315563` was run for the root, `bamlutils` and `worker`,
   and Go returned `v0.0.0-20260831140315-8427c6315563` for the root and
   `v0.0.49-0.20260831140315-8427c6315563` for the other two — each keeping its own base
   version. Those strings are used verbatim.
1. **All five selections re-pointed together** to `8427c6315563`. The edit touched only
   `require` lines: `nanollmprepare`'s deliberate `baml-rest v0.0.48` (a released TAG, not a
   pseudo-version) is untouched, and so is every SHA inside the historical prose.
2. **Both `// PIN-STATUS` markers flipped** `RESOLVED` -> `OUTSTANDING`, one per manifest.
3. **Both mirrored narratives rewritten** to BRANCH-ONLY, with the prior master-durable
   batch-2 sentences demoted into each manifest's `HISTORICAL, SUPERSEDED` paragraph.
4. **This file rewritten** — fenced record (`OUTSTANDING`, the new commit/stamp,
   `REACHABLE-FROM` the branch), opening claim, selections table, this section, and the
   Definition-of-done checklist below.
5. **Tar regenerated** (`go run ./cmd/build/gen-nativeworker-src`), which is required because
   it embeds both manifests, followed by the codegen-spine guard re-baseline
   (`go test ./internal/codegenspine/ -run TestSourceGuard -update-codegenspine-guard`) — the
   guard hashes the tar and the five pins, so it re-baselines in the same change.
6. **Gates re-run** — see "Definition of done".

The OWED follow-up (post-squash) re-runs exactly these steps against the master squash commit
and flips the record back to `RESOLVED`; the orchestrator drives it.

A note on TERMINOLOGY, because the two vocabularies differ. The machine-readable marker
takes exactly `OUTSTANDING` or `RESOLVED`: `pinFollowupViolations` rejects anything else,
and the ANCESTRY clause compares master-reachability against those two literals. "Durable"
is the PROSE word for the `RESOLVED` state; `RESOLVED` is what the guards read.

## The follow-up — OWED (the post-squash re-pin RUNBOOK, to execute after merge)

**This has NOT yet been done for U1: the pins are BRANCH-ONLY** at `8427c6315563` and the
re-pin to the eventual master squash commit is OWED (the orchestrator drives it after merge).
"What was done for THIS branch pin" above records the branch-pin steps that were executed; the
runbook here is the post-squash version to run against the master commit. It is kept in full
and in the imperative because it is the reusable procedure — the sequence has been executed
seven times now (#678, #682, #684, #687, #692, #703 and — post-merge — this one), and every
time the value came from following it literally rather than from remembering it.

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

DONE for the U1 BRANCH pin (this change):

- [x] all five selections name the branch source commit `8427c6315563`, each with its correct
      base version
- [x] both `// PIN-STATUS` markers say `OUTSTANDING`
- [x] both mirrored manifest narratives say BRANCH-ONLY, with the prior master-durable batch-2
      text demoted to `HISTORICAL, SUPERSEDED`
- [x] this file says `STATUS: OUTSTANDING`, `REACHABLE-FROM:` the branch, with the new
      commit/stamp and an updated selections table
- [x] `cmd/build/nativeworker_module.tar` regenerated and `internal/codegenspine/guard.json`
      re-baselined
- [x] tar freshness, `./cmd/build/...` (incl. `TestFirstPartyPinFollowupIsTracked`),
      codegenspine guard green

OWED post-squash (the orchestrator drives it after merge):

- [ ] re-point all five selections to the MASTER squash commit (Go-resolved stamp)
- [ ] flip both `// PIN-STATUS` markers + this file to `RESOLVED`, `REACHABLE-FROM: master`
- [ ] rewrite both narratives to master-durable; demote the branch-only text to HISTORICAL
- [ ] regenerate the tar + re-baseline the guard; rerun all gates
- [ ] `nativeserve-goget` green on the master push that carries the re-pin commit

Precedent: #677 → #678, #681 → #682, #683 → #684, #686 → #687, #689 → #692 and #703 are the
prior instances of this runbook being executed correctly; Slice 7.1b (#655) is what skipping
it costs — a branch pin went red on `nativeserve-goget` the moment the branch was deleted.

While the pins name the BRANCH commit `8427c6315563`, this file reads `STATUS: OUTSTANDING`,
which is what `TestFirstPartyPinFollowupIsTracked`'s ANCESTRY clause requires (a branch-only
commit is not master-reachable). The record stays in the tree; the OWED post-squash re-pin
flips it to `RESOLVED`.
