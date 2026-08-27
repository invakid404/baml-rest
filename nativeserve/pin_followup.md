# Out-of-`go.work` first-party pin follow-up

This file is the TRACKED record of whether the five first-party pseudo-version
selections below point at a **master** commit. **They now do NOT.** They name
`5fdd679c8784`, the latest SOURCE tip of the de-BAML `/parse` UNION-RESIDUAL batch-2 slice
on `feat/debaml-parse-batch2` — a BRANCH commit — and the post-squash re-pin runbook in the
last section is **OWED, not yet performed**.

They were re-bumped here from the initial batch-2 source `c011c7e95993` across two review
rounds, both landing ONLY in guarded trees (`internal/debaml` / `nativeserve`) the
codegen-spine guard hashes: the Codex review of PR #703 added one discriminating unit test
(`TestClassUnion_OptionalField_NoMatchWitness`), and the CodeRabbit review added three
doc/comment/header fixes (a stale go-get provenance header, a reversed-arm test comment, and
an ATX-heading line in this file). Neither round changes any packaged, compiled source — the
test file is `_test.go` and the rest are comments — so the serve core the pins describe is
byte-unchanged; but the standing "pins always name the LATEST branch source" rule moves the
five selections to the tip that carries them, with the tar regenerated and the guard
re-baselined each time so the pinned commit's guarded-tree content stays identical to the
guard-frozen checkout.

This is the expected branch-phase state, entered deliberately: batch 2 edits
`internal/debaml`, the native SAP the packaged serve core resolves through these pins, and
no master commit carries that change yet, so there is no durable master pin to name. The
pins point at the branch source commit for the life of the review; the squash-merge will
flatten that commit out of history, so re-pinning all five to the master squash commit is
the MANDATORY immediate follow-up — the same branch-pin-then-re-pin the four most recent
slices (#682, #684, #687, #692) each performed.

It is proof material, not documentation. `TestFirstPartyPinFollowupIsTracked`
(`cmd/build/nativeworker_pins_test.go`) parses it on every ordinary `go test ./...`,
cross-checks it against the real `go.mod` files (LOCKSTEP: all five agree; FRESHNESS: the
recorded commit/stamp match the require directives), and — wherever a `master` ref is
resolvable — requires the recorded status to agree with actual master-reachability
(ANCESTRY). So the follow-up cannot be quietly forgotten: once the re-pin lands on master
the guard goes RED until this file is flipped to `RESOLVED`, and if the pins move without
this file moving with them it goes red immediately.

`nativeserve/go.mod`'s BUMP RULE header states the general rule. This file is the
CONCRETE, per-change instance of it, which is what the generic comment cannot be.

```
STATUS: OUTSTANDING
PINNED-COMMIT: 5fdd679c8784
PINNED-STAMP: 20260827131055
REACHABLE-FROM: branch feat/debaml-parse-batch2
SLICE: de-BAML /parse UNION-RESIDUAL batch 2 — optional-field class-union arm cast (OptionalDefaultFromNoValue score), typed all-arms-failed union verdict (claim / list-drop / class-default-fill by position), proven-union list-element drop, alias nil-safety
PR: opened, NOT merged (Codex review pending); these pins name the branch source commit 5fdd679c8784 and are re-pinned to the master squash commit as the immediate post-merge follow-up
```

## Why the pins point at the BRANCH source commit for now

`5fdd679c8784` is the batch-2 SOURCE commit — the one that carries the changed native SAP
(`internal/debaml`'s union coercion). The packaged serve core is a direct caller of that
package, so the pinned root module decides what a native claim actually emits; pinning it
to a commit that lacks the batch-2 union parser would ship a serve core whose answers
disagree with this tree's. No master commit carries the change yet, so the branch source
commit is the only commit whose `internal/debaml` is the batch-2 parser — hence the
branch pin, and hence `STATUS: OUTSTANDING`.

**This bump carries real cross-module behaviour, not just a version string.** What it changes
is what the packaged worker RETURNS:

- `nativeserve/canary/serve.go` passes `debaml.Parse` to `execute.DynamicParse` as the
  response parser for the native dynamic serve lane, and `canary/serve_static.go` /
  `canary/serve_static_shadow.go` call `debaml.ParseStaticBundleUnaryCall` /
  `debaml.ParseStaticBundle` for the static lanes. The batch-2 slice changes those answers
  on three counts:
  - a class union ARM may now carry a SINGLE-non-null OPTIONAL field: `Class::try_cast`
    fills an ABSENT optional with a typed `null` at `OptionalDefaultFromNoValue` (score 1)
    and recurses into a PRESENT one, so an arm the gate used to decline is now cast and
    scored (`class_union_optional_field_arm_stays_fallback` and its collection sibling now
    serve natively);
  - an all-arms-failed NON-nullable union is now CLAIMED as BAML's union-no-match error
    instead of the fallback sentinel — a claimed error at the top level
    (`scalar_union_no_match`), consumed as `ArrayItemParseError` at a list element, and
    default-filled at a required class field, by ENCLOSING position;
  - a proven-failing union LIST element is now DROPPED (`ArrayItemParseError`) the way
    `coerce_array` drops it, so `list<string|map<string,int>>` over `[null]` serves `[]`
    natively (`list_union_map_arm_rejects_null_stays_fallback`).
- `internal/nativebody/nanollmprepare` runs the same parser (`shadow/response.go` sets
  `Parse: debaml.Parse`) and its `cmd/worker` entrypoint is the binary the
  booted-artifact proofs boot — including the packaged `/parse` route proof #685 added;
- the `worker` module, pinned here in lockstep, is where the direct-parse field-order pass
  lives (`worker/direct_parse_schema_order.go`) that makes the native and BAML payloads
  byte-comparable at the worker boundary in the first place.

A consumer resolving a PRE-batch-2 pin gets a serve core whose native SAP declines the
union shapes this tree now claims, underneath a manifest describing this one. That is not a
cosmetic disagreement: it is a difference in the bytes a native claim serves, which is
exactly the fact an operator reads this manifest to establish.

The pins CANNOT point at master until this change is squash-merged: no master commit carries
the changed SAP. The branch is based on master `ff3012b160aca` (#702), so `5fdd679c8784` is
a descendant of the current master — but a descendant is not master, and the squash will
flatten it out of history just the same, which is why re-pinning to the master squash commit
is the mandatory immediate follow-up. The Slice 7.1b (#655) incident is the precedent for
what happens if that re-pin is skipped — a branch pin went red on `nativeserve-goget` once
the branch was deleted — and #677 → #678, #681 → #682, #683 → #684, #686 → #687 and
#689 → #692 are the five most recent instances of doing it correctly.

**What a green `nativeserve-goget` proves on which run.** On a `pull_request` it resolves the
PR HEAD SHA, so a green PR run establishes that an external consumer can resolve, build and
run `nativeserve.New` against the branch under review — but its `nativeserve/go.mod` names a
BRANCH commit, so that resolution depends on the branch continuing to exist. The `push` run
on master is the one that proves durability, and it can only pass AFTER the post-squash
re-pin lands.

## The five pinned selections

Every one of them moves together. They are directory-`replace`d for local
development, so only the version STRINGS reach MVS — which is precisely why a partial
bump is invisible until the out-of-work packaging build fails with
`updates to go.mod needed`.

| # | file | module | current selection |
| --- | --- | --- | --- |
| 1 | `nativeserve/go.mod` | `github.com/invakid404/baml-rest` | `v0.0.0-20260827131055-5fdd679c8784` |
| 2 | `nativeserve/go.mod` | `github.com/invakid404/baml-rest/bamlutils` | `v0.0.49-0.20260827131055-5fdd679c8784` |
| 3 | `nativeserve/go.mod` | `github.com/invakid404/baml-rest/worker` | `v0.0.49-0.20260827131055-5fdd679c8784` |
| 4 | `internal/nativebody/nanollmprepare/go.mod` | `github.com/invakid404/baml-rest/bamlutils` | `v0.0.49-0.20260827131055-5fdd679c8784` |
| 5 | `internal/nativebody/nanollmprepare/go.mod` | `github.com/invakid404/baml-rest/worker` | `v0.0.49-0.20260827131055-5fdd679c8784` |

`internal/nativebody/nanollmprepare/go.mod`'s `github.com/invakid404/baml-rest v0.0.48`
is deliberately NOT in this list: it is a released tag, not a pseudo-version tracking a
commit, and the module directory-replaces it.

## What was done for THIS branch cut (the executed branch-pin runbook)

Steps 0-5 of the runbook below, in order, against the batch-2 source commit
`5fdd679c8784`:

0. **Stamp resolved by Go, off the origin — never hand-computed.** The source commit was
   pushed to `feat/debaml-parse-batch2` FIRST, then
   `GOWORK=off GOPRIVATE=github.com/invakid404/baml-rest go mod download -json <mod>@5fdd679c8784`
   (equivalently `go list -m -json <mod>@5fdd679c8784`) was run for the root, `bamlutils`
   and `worker`, and Go returned `v0.0.0-20260827131055-5fdd679c8784` for the root and
   `v0.0.49-0.20260827131055-5fdd679c8784` for the other two — each keeping its own base
   version. Those strings are used verbatim.
1. **All five selections re-pointed together**, off the post-#692 master commit
   `062871154d95`, to the branch source commit. The edit touched only `require` lines:
   `nanollmprepare`'s deliberate `baml-rest v0.0.48` (a released TAG, not a pseudo-version)
   is untouched, and so is every SHA inside the historical prose.
2. **Both `// PIN-STATUS` markers flipped** `RESOLVED` -> `OUTSTANDING`, one per manifest.
3. **Both mirrored narratives rewritten** to branch-only, with the master-durable
   `062871154d95` sentences demoted into each manifest's `HISTORICAL, SUPERSEDED` paragraph.
4. **This file rewritten** — fenced record, opening claim, selections table, section
   headings and the checklist below.
5. **Tar regenerated** (`go run ./cmd/build/gen-nativeworker-src`), which is required
   because it embeds both manifests, followed by the codegen-spine guard re-baseline
   (`go test ./internal/codegenspine/ -run TestSourceGuard -update-codegenspine-guard`) —
   editing `internal/debaml` is a sanctioned guarded-path change, so its tree hash, the
   tar and the five pins all move in the same change.

A note on TERMINOLOGY, because the two vocabularies differ. The machine-readable marker
takes exactly `OUTSTANDING` or `RESOLVED`: `pinFollowupViolations` rejects anything else,
and the ANCESTRY clause compares master-reachability against those two literals. "Durable"
is the PROSE word for the `RESOLVED` state; `RESOLVED` is what the guards read.

## The follow-up — OWED (the post-squash re-pin RUNBOOK, to execute)

**This has NOT yet been done.** The runbook is kept in full and in the imperative because it
is the reusable procedure that MUST run the moment this PR squash-merges — the sequence has
been executed five times now (#678, #682, #684, #687, #692), and every time the value came
from following it literally rather than from remembering it.

What makes it MANDATORY and IMMEDIATE is not a rule but the squash: the merge flattens
`5fdd679c8784` (and its parents) out of history and the branch is deleted, so until the
re-pin lands the five selections would name a commit that resolves to nothing — the exact
Slice 7.1b failure this record exists to prevent.

### 0. Get the durable commit and its stamp — from Go, not by hand

Take the SHA of the **master squash-merge commit** of this PR (not the branch tip, which
the squash flattens away) and confirm the SHA-to-stamp pair Go itself computes:

```bash
SHA=<master squash-merge of this PR>
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
- the opening paragraph ("**They now do NOT.**") to say they DO, and that the
  follow-up has been **performed**; this section's heading and tense rewritten to match
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

Then the **`nativeserve-goget` external-consumer probe**, against the master tip that
CARRIES STEPS 1-5 — i.e. AFTER the re-pin has landed, not immediately after this PR's
squash. This ordering is load-bearing: the squash commit itself still names the BRANCH
pins (steps 1-5 have not run yet at that moment), so a probe aimed at it resolves the
obsolete pseudo-versions and can pass while proving nothing about the durable state. Aim
it at the commit whose `nativeserve/go.mod` names the master squash SHA from step 0. It must
run from a genuinely external module — no checkout, no `replace`, no workspace — under
`CGO_ENABLED=1 GOPRIVATE=github.com/invakid404/baml-rest`.

### Definition of done (of the post-squash re-pin — NOT yet complete)

- [ ] all five selections name the master squash commit, each with its correct base version
- [ ] both `// PIN-STATUS` markers say `RESOLVED`
- [ ] both mirrored manifest narratives say master-durable, with the branch-only text
      demoted to `HISTORICAL, SUPERSEDED`
- [ ] this file says `STATUS: RESOLVED`, `REACHABLE-FROM: master`, with the new
      commit/stamp and an updated selections table
- [ ] `cmd/build/nativeworker_module.tar` regenerated and `internal/codegenspine/guard.json`
      re-baselined
- [ ] tar freshness, `./cmd/build/...`, dynclient regen-idempotence and `nativeserve-goget`
      all green

Precedent: #677 → #678, #681 → #682, #683 → #684, #686 → #687 and #689 → #692 are the five
prior instances of this runbook being executed correctly; Slice 7.1b (#655) is what skipping
it costs — a branch pin went red on `nativeserve-goget` the moment the branch was deleted.

The record stays in the tree rather than being deleted: this branch cut flipped it back to
`OUTSTANDING`, and the post-squash re-pin flips it to `RESOLVED` and ticks every box above.
