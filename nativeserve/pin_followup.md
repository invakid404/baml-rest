# Out-of-`go.work` first-party pin follow-up

This file is the TRACKED record of whether the five first-party pseudo-version
selections below point at a **master** commit. **They do NOT.** They name `f3fcdbc2595d`,
the branch SOURCE commit on `feat/debaml-widen-scalar-list-inputs` that carries the
scalar-LIST input widening (the standard worker's default-serving population now also
admits methods whose inputs mix the existing required scalars with required single-level
lists of non-nullable primitives — `string[]`, `int[]`, `bool[]` — keeping the exact
five-arm `JSON` return and every existing oracle). No master commit carries that change
yet, so the pins are **BRANCH-ONLY** and this record is **STATUS: OUTSTANDING**. The
post-squash re-pin runbook in the last section is **OWED**: it is this branch pin's
mandatory, immediate follow-up once the PR squash-merges.

The scalar-list widening changes non-test source under BOTH guarded trees —
`nativeserve` (the registration input predicate in `nativeserve/spine`, the single
population owner every constructor shares) and `internal/nativebody/nanollmprepare` (the
cohort documentation on the standard worker's serve factories and on the two bounded
population metric labels) — plus the opaque worker tar and root-module build/generation
code. The pins must therefore name a commit that carries THIS change: an external consumer
resolving `nativeserve` needs a snapshot whose five first-party selections are
self-consistent with the widened source, or the packaged worker serves the pre-widening
population while the tar claims otherwise.

Unlike U1s, this change introduces NO new `bamlutils` symbol — the neutral contracts are
unchanged, so a consumer resolving a pre-widening `bamlutils` still compiles. The lockstep
here is therefore the repository's delivery convention (one self-consistent snapshot),
not a link-time precondition. It is not optional for that reason: a partial bump is
invisible until the out-of-work packaging build fails.

It is proof material, not documentation. `TestFirstPartyPinFollowupIsTracked`
(`cmd/build/nativeworker_pins_test.go`) parses it on every ordinary `go test ./...`,
cross-checks it against the real `go.mod` files (LOCKSTEP: all five agree;
FRESHNESS: the recorded commit/stamp match the require directives), and — wherever a
`master` ref is resolvable — requires the recorded status to agree with actual
master-reachability (ANCESTRY: a branch-only commit is NOT master-reachable, so the
status must be `OUTSTANDING`; a master-reachable one must read `RESOLVED`). So the
follow-up cannot be quietly forgotten: while the pins name a branch commit this file
must read `OUTSTANDING` — which is the state recorded below — and once the re-pin lands
on master it must be flipped to `RESOLVED`.

`nativeserve/go.mod`'s BUMP RULE header states the general rule. This file is the
CONCRETE, per-change instance of it, which is what the generic comment cannot be.

```text
STATUS: OUTSTANDING
PINNED-COMMIT: f3fcdbc2595d
PINNED-STAMP: 20260907164835
REACHABLE-FROM: feat/debaml-widen-scalar-list-inputs
SLICE: de-BAML cohort widening (Rank 1) — required scalar-LIST inputs: widen the default-native serving population so the standard BAML+nanollm worker also default-serves methods whose inputs mix the existing required scalars with required single-level lists of non-nullable primitives (string[]/int[]/bool[]), keeping the exact five-arm JSON return and reusing every existing oracle (nativeserve/spine's registration input predicate, one precise two-level shape check; the shared classifier widens /call, /stream, /stream-with-raw and the native-only runtime together)
PR: #719 — branch-only pin; post-squash re-pin to the merged master commit is OWED
```

## Why the pins name a branch commit

`f3fcdbc2595d` is the branch SOURCE commit on `feat/debaml-widen-scalar-list-inputs` that
carries the guarded-tree change. The packaged tar (`cmd/build/nativeworker_module.tar`)
embeds both out-of-work modules' source AND their go.mods, so the pins the tar ships are
the pins an external `nativeserve-goget` consumer resolves. Pinning to a PRE-widening
commit would ship a snapshot whose `nativeserve` still refuses a scalar-list input, so the
packaged worker's serving population would not be the one this change proves. The pins
name the commit that has the source.

They name a BRANCH commit only because no master commit carries the widening yet. The
Slice 7.1b failure (#655) is what skipping the post-squash re-pin costs — a branch pin
went red on `nativeserve-goget` the moment the branch was deleted — which is why the
re-pin to the master squash commit is the MANDATORY, IMMEDIATE follow-up after merge (the
runbook below, now OWED).

## The five pinned selections

Every one of them moves together. They are directory-`replace`d for local
development, so only the version STRINGS reach MVS — which is precisely why a partial
bump is invisible until the out-of-work packaging build fails with
`updates to go.mod needed`.

| # | file | module | current selection |
| --- | --- | --- | --- |
| 1 | `nativeserve/go.mod` | `github.com/invakid404/baml-rest` | `v0.0.0-20260907164835-f3fcdbc2595d` |
| 2 | `nativeserve/go.mod` | `github.com/invakid404/baml-rest/bamlutils` | `v0.0.49-0.20260907164835-f3fcdbc2595d` |
| 3 | `nativeserve/go.mod` | `github.com/invakid404/baml-rest/worker` | `v0.0.49-0.20260907164835-f3fcdbc2595d` |
| 4 | `internal/nativebody/nanollmprepare/go.mod` | `github.com/invakid404/baml-rest/bamlutils` | `v0.0.49-0.20260907164835-f3fcdbc2595d` |
| 5 | `internal/nativebody/nanollmprepare/go.mod` | `github.com/invakid404/baml-rest/worker` | `v0.0.49-0.20260907164835-f3fcdbc2595d` |

`internal/nativebody/nanollmprepare/go.mod`'s `github.com/invakid404/baml-rest v0.0.48`
is deliberately NOT in this list: it is a released tag, not a pseudo-version tracking a
commit, and the module directory-replaces it.

## What was done for THIS branch pin (the executed steps)

The pin below points all five selections at the widening's BRANCH SOURCE commit and sets
the record `OUTSTANDING`; the post-squash re-pin to master is now OWED.

1. **All five selections re-pointed together** to `f3fcdbc2595d` (Go-formula stamp
   `20260907164835`, taken from `go mod download -json` against the pushed branch commit
   rather than hand-computed). The edit touched only `require` lines; `nanollmprepare`'s
   deliberate `baml-rest v0.0.48` (a released TAG) and `workerplugin v0.0.48` are
   untouched, and so is every SHA inside the historical prose.
2. **Both `// PIN-STATUS` markers flipped** from `RESOLVED` to `OUTSTANDING`, one per
   manifest.
3. **Both mirrored narratives rewritten** to branch-only naming `f3fcdbc2595d`, with the
   ExecBridge-U1s master squash `5d1a7d8b1d0b` demoted to `HISTORICAL, SUPERSEDED`.
4. **This file updated** — fenced record (`OUTSTANDING`, the branch commit/stamp,
   `REACHABLE-FROM: feat/debaml-widen-scalar-list-inputs`), opening claim, selections
   table, this section.
5. **Tar regenerated** (`go run ./cmd/build/gen-nativeworker-src`), which is required
   because it embeds both manifests, followed by the codegen-spine guard re-baseline
   (`go test ./internal/codegenspine/ -run TestSourceGuard -update-codegenspine-guard`)
   — the guard hashes the tar and the five pins, so it re-baselines in the same change.
6. **Gates re-run** — pin lockstep, packaged-manifest identity, tar freshness, source
   guard.

A note on TERMINOLOGY, because the two vocabularies differ. The machine-readable
marker takes exactly `OUTSTANDING` or `RESOLVED`: `pinFollowupViolations` rejects
anything else, and the ANCESTRY clause compares master-reachability against those two
literals. "Durable" is the PROSE word for the `RESOLVED` state; `RESOLVED` is what the
guards read.

## The follow-up — OWED (the post-squash re-pin RUNBOOK, to run after merge)

**This is NOT done yet: the pins are BRANCH-ONLY** at `f3fcdbc2595d`. What makes the
post-squash re-pin MANDATORY and IMMEDIATE after merge is the same failure mode as
always: a squash flattens the branch source commit out of history and the branch is
deleted, so until the re-pin lands the five selections name a commit that resolves to
nothing. The ordered steps below are the runbook the ORCHESTRATOR must follow the moment
the widening PR squash-merges.

Both manifests point here by the NUMBERED STEPS below rather than by this heading, because
the heading tracks owed-vs-performed and is rewritten every slice.

### 0. Get the durable commit and its stamp — from Go, not by hand

Take the SHA of the **master squash-merge commit** of the slice's PR (not the branch tip,
which the squash flattens away) and confirm the SHA-to-stamp pair Go itself computes:

```bash
SHA=<master squash-merge of the slice's PR>
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
`NOTE (M3e-A ...)` in `nativeserve/go.mod`, and `RIGHT NOW they are ...` in
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

DONE for the scalar-list widening BRANCH pin (THIS change):

- [x] point all five selections at the widening's branch SOURCE commit `f3fcdbc2595d`
      (Go-formula stamp `20260907164835`), each with its correct base version
- [x] set both `// PIN-STATUS` markers + this file to `OUTSTANDING`,
      `REACHABLE-FROM: feat/debaml-widen-scalar-list-inputs`
- [x] rewrite both narratives to branch-only; demote the U1s `5d1a7d8b1d0b` text to
      `HISTORICAL, SUPERSEDED`
- [x] `cmd/build/nativeworker_module.tar` regenerated and
      `internal/codegenspine/guard.json` re-baselined
- [x] tar freshness, `./cmd/build/...` (incl. `TestFirstPartyPinFollowupIsTracked`),
      codegenspine guard green

OWED post-squash for the scalar-list widening MASTER re-pin (the ORCHESTRATOR's job):

- [ ] re-point all five selections to the widening's MASTER squash commit, each with its
      correct base version
- [ ] flip both `// PIN-STATUS` markers + this file to `RESOLVED`,
      `REACHABLE-FROM: master`
- [ ] rewrite both narratives to master-durable naming that squash commit; demote the
      branch-only `f3fcdbc2595d` text to `HISTORICAL, SUPERSEDED`
- [ ] `cmd/build/nativeworker_module.tar` regenerated and
      `internal/codegenspine/guard.json` re-baselined
- [ ] pin/tar/guard gates green (`TestFirstPartyPinFollowupIsTracked` will then see a
      master-reachable pin ⇒ RESOLVED); `nativeserve-goget` runs in CI against master

Precedent: #677 → #678, #681 → #682, #683 → #684, #686 → #687, #689 → #692, #703,
U1's #708 → #709, U1b's #711 post-squash re-pin, U1c's #713 post-squash re-pin,
M3e-A's #715 post-squash re-pin, and U1s's #717 post-squash re-pin are the
instances of this runbook being executed correctly; Slice 7.1b (#655) is what skipping it costs — a branch pin went red
on `nativeserve-goget` the moment the branch was deleted.
