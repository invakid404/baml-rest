# Out-of-`go.work` first-party pin follow-up

This file is the TRACKED record of whether the five first-party pseudo-version
selections below point at a **master** commit. **They now DO.** They name
`7c7bed8291b6`, the master commit that squash-merged the de-BAML `/parse` UNION-RESIDUAL
batch-2 slice (PR #703), and the post-squash re-pin runbook in the last section has been
**PERFORMED** — this change is it.

The state it repaired was real, not hypothetical. During review the pins named the batch-2
BRANCH tip `5fdd679c8784` on `feat/debaml-parse-batch2` (re-bumped there twice as the Codex
and CodeRabbit reviews added guarded-tree changes). PR #703 squash-merged that branch as
`7c7bed8291b6` and the branch was deleted, which flattened `5fdd679c8784` (and every commit
beneath it) out of history. From that moment the five selections named a commit that
resolves to NOTHING, so master's own packaged worker could not resolve the parser it
ships — the exact Slice 7.1b failure this record exists to prevent. Every box in
"Definition of done" is now ticked.

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
STATUS: RESOLVED
PINNED-COMMIT: 7c7bed8291b6
PINNED-STAMP: 20260827134815
REACHABLE-FROM: master
SLICE: de-BAML /parse UNION-RESIDUAL batch 2 — optional-field class-union arm cast (OptionalDefaultFromNoValue score), typed all-arms-failed union verdict (claim / list-drop / class-default-fill by position), proven-union list-element drop, alias nil-safety
PR: #703 squash-merged to master as 7c7bed8291b6; these pins name that master commit; re-pinned by THIS change
```

## Why the pins point at MASTER now

`7c7bed8291b6` is the master commit that carries the batch-2 native SAP — `internal/debaml`'s
union coercion — because it is the squash-merge of PR #703 itself. The packaged serve core is
a direct caller of that package, so the pinned root module decides what a native claim
actually emits; pinning it to a commit that lacks the batch-2 union parser would ship a serve
core whose answers disagree with this tree's. During review the only commit carrying the SAP
was the branch tip; now the squash commit does, and it is a MASTER commit, so the pins survive
branch deletion by construction.

Three checks establish that the pinned commit really carries the batch-2 parser, rather than
assuming it from the merge graph — run against the RESOLVED module in the module cache, not
the worktree:

- the resolved root module's `internal/debaml` contains the batch-2 gate/cast changes
  (`checkUnionClassField`'s single-non-null optional `TypeUnion` case in `parse.go`,
  `unionNoArmVerdict` / `errUnionAllArmsFailed` and the `coerceListChild` union-element drop
  in `coerce.go`) in NON-test sources;
- `diff -rq` between the resolved module's `internal/debaml` and this checkout's reports no
  differences at all — what a consumer downloads IS what this tree ships;
- `5fdd679c8784` (the last branch tip) is confirmed NOT reachable from `master`, which is what
  made the review-phase pins orphaned rather than merely stale.

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

**What a green `nativeserve-goget` proves now — and on which run.** On a `pull_request` it
resolves the PR HEAD SHA; on `push` to master it resolves the master tip. Because
`nativeserve/go.mod` now names `7c7bed8291b6`, a MASTER commit, that resolution does not
depend on any branch continuing to exist — unlike every branch-only cut of this record. The
`push` run on master is the one that proves durability end-to-end, and it can now pass: the
durable delivery state is what this change establishes.

## The five pinned selections

Every one of them moves together. They are directory-`replace`d for local
development, so only the version STRINGS reach MVS — which is precisely why a partial
bump is invisible until the out-of-work packaging build fails with
`updates to go.mod needed`.

| # | file | module | current selection |
| --- | --- | --- | --- |
| 1 | `nativeserve/go.mod` | `github.com/invakid404/baml-rest` | `v0.0.0-20260827134815-7c7bed8291b6` |
| 2 | `nativeserve/go.mod` | `github.com/invakid404/baml-rest/bamlutils` | `v0.0.49-0.20260827134815-7c7bed8291b6` |
| 3 | `nativeserve/go.mod` | `github.com/invakid404/baml-rest/worker` | `v0.0.49-0.20260827134815-7c7bed8291b6` |
| 4 | `internal/nativebody/nanollmprepare/go.mod` | `github.com/invakid404/baml-rest/bamlutils` | `v0.0.49-0.20260827134815-7c7bed8291b6` |
| 5 | `internal/nativebody/nanollmprepare/go.mod` | `github.com/invakid404/baml-rest/worker` | `v0.0.49-0.20260827134815-7c7bed8291b6` |

`internal/nativebody/nanollmprepare/go.mod`'s `github.com/invakid404/baml-rest v0.0.48`
is deliberately NOT in this list: it is a released tag, not a pseudo-version tracking a
commit, and the module directory-replaces it.

## What was done for THIS re-pin (the executed runbook)

Steps 0-6 of the runbook below, in order, against the master squash commit `7c7bed8291b6`:

0. **Stamp resolved by Go, off the origin — never hand-computed.**
   `GOWORK=off GOPRIVATE=github.com/invakid404/baml-rest go list -m -json <mod>@7c7bed8291b6`
   was run for the root, `bamlutils` and `worker`, and Go returned
   `v0.0.0-20260827134815-7c7bed8291b6` for the root and
   `v0.0.49-0.20260827134815-7c7bed8291b6` for the other two — each keeping its own base
   version. Those strings are used verbatim.
1. **All five selections re-pointed together**, off the orphaned branch tip `5fdd679c8784`.
   The edit touched only `require` lines: `nanollmprepare`'s deliberate `baml-rest v0.0.48`
   (a released TAG, not a pseudo-version) is untouched, and so is every SHA inside the
   historical prose.
2. **Both `// PIN-STATUS` markers flipped** `OUTSTANDING` -> `RESOLVED`, one per manifest.
3. **Both mirrored narratives rewritten** to master-durable, with the branch-only sentences
   demoted into each manifest's `HISTORICAL, SUPERSEDED` paragraph.
4. **This file rewritten** — fenced record, opening claim, selections table, section
   headings and the checklist below.
5. **Tar regenerated** (`go run ./cmd/build/gen-nativeworker-src`), which is required because
   it embeds both manifests, followed by the codegen-spine guard re-baseline
   (`go test ./internal/codegenspine/ -run TestSourceGuard -update-codegenspine-guard`) — the
   guard hashes the tar and the five pins, so it re-baselines in the same change.
6. **Gates re-run** — see "Definition of done".

A note on TERMINOLOGY, because the two vocabularies differ. The machine-readable marker
takes exactly `OUTSTANDING` or `RESOLVED`: `pinFollowupViolations` rejects anything else,
and the ANCESTRY clause compares master-reachability against those two literals. "Durable"
is the PROSE word for the `RESOLVED` state; `RESOLVED` is what the guards read.

## The follow-up — PERFORMED (the post-squash re-pin RUNBOOK, as executed)

**This HAS now been done: this change is it**, and "What was done for THIS re-pin" above
records each step. The runbook is kept in full and in the imperative because it is the
reusable procedure for the NEXT slice that has to pin to a branch commit — the sequence has
been executed six times now (#678, #682, #684, #687, #692 and this one), and every time the
value came from following it literally rather than from remembering it.

What made it MANDATORY and IMMEDIATE here was not a rule but a broken tree: PR #703's squash
flattened `5fdd679c8784` away and the branch was deleted, so until this change landed the
five selections named a commit that resolves to nothing.

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

- [x] all five selections name the master commit `7c7bed8291b6`, each with its correct base
      version
- [x] both `// PIN-STATUS` markers say `RESOLVED`
- [x] both mirrored manifest narratives say master-durable, with the branch-only text
      demoted to `HISTORICAL, SUPERSEDED`
- [x] this file says `STATUS: RESOLVED`, `REACHABLE-FROM: master`, with the new
      commit/stamp and an updated selections table
- [x] `cmd/build/nativeworker_module.tar` regenerated and `internal/codegenspine/guard.json`
      re-baselined
- [x] tar freshness, `./cmd/build/...`, dynclient regen-idempotence green (and
      `nativeserve-goget` green on the master push that carries this commit)

Precedent: #677 → #678, #681 → #682, #683 → #684, #686 → #687 and #689 → #692 are the five
prior instances of this runbook being executed correctly; Slice 7.1b (#655) is what skipping
it costs — a branch pin went red on `nativeserve-goget` the moment the branch was deleted.

EVERY box above is now ticked. The serve core is pinned to a MASTER commit
(`7c7bed8291b6`), so it survives branch deletion by construction and an external consumer
resolves the same union parser this tree ships. This file reads `STATUS: RESOLVED`, which is
what `TestFirstPartyPinFollowupIsTracked`'s ANCESTRY clause requires now that the pinned
commit is master-reachable.

The record stays in the tree rather than being deleted: the next slice that must pin to a
branch commit starts by flipping this file back to `OUTSTANDING` and re-running the runbook
above.
