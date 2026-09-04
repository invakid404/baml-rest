# Out-of-`go.work` first-party pin follow-up

This file is the TRACKED record of whether the five first-party pseudo-version
selections below point at a **master** commit. **They DO NOT.** They name
`a9882f60b5b2` on `feat/debaml-m3e-a`, the branch SOURCE commit that carries the M3e-A
guarded-tree change (the spine STREAM substrate: `nativeserve/spine.StreamExecutor` +
`StreamRegistration` + the stream-native `NewWorkerRuntime`,
`nativeserve/admission.AdmitStaticSpineStreamClaim` with its unexported lane policy, and
`nanollmprepare`'s stream-capable native-only worker). No master commit carries it yet,
so the pins are **BRANCH-ONLY** and this record is **STATUS: OUTSTANDING**. The
post-squash re-pin runbook in the last section is the MANDATORY, IMMEDIATE follow-up
once the PR merges.

M3e-A changes non-test source under BOTH guarded trees — `nativeserve` (the BAML-free
spine stream executor embedding the frozen `UnaryExecutor`, the normalized single
registration classifier, and the new `AdmitStaticSpineStreamClaim` entry beside the
unchanged legacy `AdmitStaticStreamClaim`) and `internal/nativebody/nanollmprepare` (the
native-only registry stub's stream-capable contract, the booted stream e2e, the widened
dependency gate, and the isolated go.mod pin bump) — plus the opaque worker tar and
root-module build/generation code. The pins must therefore name a commit that carries
THIS change: an external consumer resolving nativeserve off the branch tip needs a
snapshot whose five first-party selections are self-consistent with the M3e-A source.
The M3e-A nativeserve source DEPENDS on NEW `bamlutils` symbols — the neutral
`NativeSpineStreamExecutor` / `NativeSpineStreamBinding` / `NativeSpineStreamResult`
contract and the extracted BAML-free `buildrequest.StreamCadence` — so a consumer
resolving a PRE-M3e-A `bamlutils` fails to compile this module: the lockstep is a build
precondition, not cosmetic, and the set must move together.

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
PINNED-COMMIT: a9882f60b5b2
PINNED-STAMP: 20260904095707
REACHABLE-FROM: feat/debaml-m3e-a
SLICE: M3e-A — spine STREAM substrate: make the exact five-arm JSON cohort stream-capable through the BAML-free native-only worker (additive ClassStaticStream descriptor v3 + two carriers over one union; neutral bamlutils stream contract; extracted BAML-free buildrequest.StreamCadence; nativeserve/admission AdmitStaticSpineStreamClaim; nativeserve/spine StreamExecutor + StreamRegistration + stream-native NewWorkerRuntime; generated unaryCandidates/streamCandidates split; no standard-worker serving change)
PR: pending — post-squash re-pin to the M3e-A master squash commit is OWED
```

## Why the pins name the M3e-A branch source commit

`a9882f60b5b2` is the branch commit that carries the guarded-tree change. The packaged
tar (`cmd/build/nativeworker_module.tar`) embeds both out-of-work modules' source AND
their go.mods, so the pins the tar ships are the pins an external `nativeserve-goget`
consumer resolves. Pinning `nativeserve` / `nanollmprepare` to a PRE-M3e-A commit would
ship a snapshot whose bamlutils lacks the `NativeSpineStreamExecutor` contract and the
`buildrequest.StreamCadence` the M3e-A nativeserve source links, so the packaged worker
could not be assembled from it. The pins name the commit that has the source, and the
selection is a lockstep set.

They name a BRANCH commit only because no master commit carries M3e-A yet. The Slice
7.1b failure (#655) is what skipping the post-squash re-pin costs — a branch pin went red
on `nativeserve-goget` the moment the branch was deleted — which is why re-pinning to the
master squash commit is the MANDATORY, IMMEDIATE follow-up after merge (the runbook
below).

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
| 1 | `nativeserve/go.mod` | `github.com/invakid404/baml-rest` | `v0.0.0-20260904095707-a9882f60b5b2` |
| 2 | `nativeserve/go.mod` | `github.com/invakid404/baml-rest/bamlutils` | `v0.0.49-0.20260904095707-a9882f60b5b2` |
| 3 | `nativeserve/go.mod` | `github.com/invakid404/baml-rest/worker` | `v0.0.49-0.20260904095707-a9882f60b5b2` |
| 4 | `internal/nativebody/nanollmprepare/go.mod` | `github.com/invakid404/baml-rest/bamlutils` | `v0.0.49-0.20260904095707-a9882f60b5b2` |
| 5 | `internal/nativebody/nanollmprepare/go.mod` | `github.com/invakid404/baml-rest/worker` | `v0.0.49-0.20260904095707-a9882f60b5b2` |

`internal/nativebody/nanollmprepare/go.mod`'s `github.com/invakid404/baml-rest v0.0.48`
is deliberately NOT in this list: it is a released tag, not a pseudo-version tracking a
commit, and the module directory-replaces it.

## What was done for THIS branch pin (the executed steps)

The bump below points all five selections at the M3e-A branch SOURCE commit and sets the
record `OUTSTANDING`; the post-squash master re-pin is OWED.

1. **All five selections re-pointed together** to `a9882f60b5b2` (Go-formula stamp
   `20260904095707`). The edit touched only `require` lines; `nanollmprepare`'s
   deliberate `baml-rest v0.0.48` (a released TAG) is untouched, and so is every SHA
   inside the historical prose.
2. **Both `// PIN-STATUS` markers flipped** from `RESOLVED` to `OUTSTANDING`, one per
   manifest.
3. **Both mirrored narratives rewritten** to BRANCH-ONLY naming the M3e-A branch source
   commit `a9882f60b5b2`, with the U1c master `56d5473a1bdb` sentences demoted to
   HISTORICAL.
4. **This file updated** — fenced record (`OUTSTANDING`, the M3e-A branch commit/stamp,
   `REACHABLE-FROM: feat/debaml-m3e-a`), opening claim, selections table, this section.
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

## The follow-up — OWED (the post-squash re-pin RUNBOOK, to run after merge)

**This is NOT yet done for M3e-A: the pins are BRANCH-ONLY** at `a9882f60b5b2`. What
makes the post-squash re-pin MANDATORY and IMMEDIATE after merge is the same failure mode
as always: a squash flattens the branch source commit out of history and the branch is
deleted, so until the re-pin lands the five selections would name a commit that resolves
to nothing. The ordered steps below are the runbook the ORCHESTRATOR follows once the
M3e-A PR squash-merges.

### 0. Get the durable commit and its stamp — from Go, not by hand

Take the SHA of the **master squash-merge commit** of the M3e-A PR (not the branch tip,
which the squash flattens away) and confirm the SHA-to-stamp pair Go itself computes:

```bash
SHA=<master squash-merge of the M3e-A PR>
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

DONE for the M3e-A BRANCH pin (THIS change):

- [x] point all five selections at the M3e-A SOURCE commit `a9882f60b5b2` (Go-formula
      stamp `20260904095707`), each with its correct base version
- [x] set both `// PIN-STATUS` markers + this file to `OUTSTANDING`,
      `REACHABLE-FROM: feat/debaml-m3e-a`
- [x] rewrite both narratives to branch-only; demote the U1c `56d5473a1bdb` text to
      `HISTORICAL, SUPERSEDED`
- [x] `cmd/build/nativeworker_module.tar` regenerated and
      `internal/codegenspine/guard.json` re-baselined
- [x] tar freshness, `./cmd/build/...` (incl. `TestFirstPartyPinFollowupIsTracked`),
      codegenspine guard green

OWED post-squash for the M3e-A MASTER re-pin (the ORCHESTRATOR's job, after the PR
squash-merges):

- [ ] re-point all five selections to the M3e-A MASTER squash commit (Go-formula stamp
      from step 0), each with its correct base version
- [ ] flip both `// PIN-STATUS` markers + this file to `RESOLVED`,
      `REACHABLE-FROM: master`
- [ ] rewrite both narratives to master-durable naming the master squash commit; demote
      the branch-only `a9882f60b5b2` text to `HISTORICAL, SUPERSEDED`
- [ ] `cmd/build/nativeworker_module.tar` regenerated and
      `internal/codegenspine/guard.json` re-baselined
- [ ] pin/tar/guard gates green (`TestFirstPartyPinFollowupIsTracked` sees a
      master-reachable pin ⇒ RESOLVED); `nativeserve-goget` runs in CI against master

Precedent: #677 → #678, #681 → #682, #683 → #684, #686 → #687, #689 → #692, #703,
U1's #708 → #709, U1b's #711 post-squash re-pin, and U1c's #713 post-squash re-pin are
the instances of this runbook being executed correctly; Slice 7.1b (#655) is what
skipping it costs — a branch pin went red on `nativeserve-goget` the moment the branch
was deleted.
