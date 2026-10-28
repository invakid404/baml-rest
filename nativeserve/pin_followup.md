# Out-of-`go.work` first-party pin follow-up

This file is the TRACKED record of whether the five first-party pseudo-version
selections below point at a **master** commit. **They DO.** They name `707ac0119657`,
the MASTER squash-merge commit of the `/call-with-raw` widening PR (#721), which carries
the guarded-tree change (the standard worker's live-oracle spine lane now also
default-serves `/call-with-raw` for the exact-JSON cohort, through the raw-aware reference
oracle `nativeserve/staticoracle.ResolveReference`, with that lane's one send made through
the request's ordinary `llmhttp` client). That commit is master-reachable, so the pins are
**MASTER-DURABLE** and this record is **STATUS: RESOLVED**. The post-squash re-pin runbook
in the last section has been **PERFORMED** — it was the merged change's mandatory
follow-up, now complete.

The `/call-with-raw` widening changes non-test source under BOTH guarded trees —
`nativeserve` (the live-oracle lane's lane-scoped raw admission in `nativeserve/admission`,
the reference oracle in `nativeserve/staticoracle`, `nativeserve/spine.CallWithOracle`, and
the ordinary-client attempt `nativeserve/execute.RunOrdinaryAttempt`) and
`internal/nativebody/nanollmprepare` (the `standardspineoracle` composite and the
`cmd/worker` documentation) — plus `bamlutils`, the opaque worker tar and root-module
build/generation code. The pins therefore name a commit that carries THIS change: an
external consumer resolving `nativeserve` needs a snapshot whose five first-party
selections are self-consistent with the widened source.

Like U1s and unlike the scalar-list widening, this change ADDS `bamlutils` symbols that
`nativeserve/spine` links: `NativeStaticInvocation.HTTPClient` and the `ProviderRequests`,
`ChannelsCompared`, `AssistantMatch`, `RawMatch`, `ReasoningMatch`, `CanonicalBytesMatch`
and `NativeFailureRecovered` fields of `NativeSpineUnaryOracleObservations`. A consumer
resolving the pre-change `bamlutils` (`880a3c4d693e`) does not compile `nativeserve/spine`,
so the lockstep here is a link-time precondition, not only the repository's delivery
convention (one self-consistent snapshot). A partial bump is still invisible until the
out-of-work packaging build fails.

It is proof material, not documentation. `TestFirstPartyPinFollowupIsTracked`
(`cmd/build/nativeworker_pins_test.go`) parses it on every ordinary `go test ./...`,
cross-checks it against the real `go.mod` files (LOCKSTEP: all five agree;
FRESHNESS: the recorded commit/stamp match the require directives), and — wherever a
`master` ref is resolvable — requires the recorded status to agree with actual
master-reachability (ANCESTRY: a branch-only commit is NOT master-reachable, so the
status must be `OUTSTANDING`; a master-reachable one must read `RESOLVED`). So the
follow-up cannot be quietly forgotten: while the pins name a branch commit this file
must read `OUTSTANDING`, and once the re-pin lands on master it must be flipped to
`RESOLVED` — which is the state recorded below.

`nativeserve/go.mod`'s BUMP RULE header states the general rule. This file is the
CONCRETE, per-change instance of it, which is what the generic comment cannot be.

```text
STATUS: RESOLVED
PINNED-COMMIT: 707ac0119657
PINNED-STAMP: 20261028122515
REACHABLE-FROM: master
SLICE: de-BAML /call-with-raw widening — default-serve /call-with-raw on the live-oracle spine lane for the exact-JSON cohort via a raw-aware reference oracle (nativeserve/staticoracle.ResolveReference: BAML's extractor over the original provider body plus a BAML-only parse; native wins only on exact assistant/raw/reasoning, structured/order and canonical-bytes agreement, else the whole reference envelope), with raw admission scoped to that lane in nativeserve/admission and the lane's one send made through the request's ordinary llmhttp client (nativeserve/execute.RunOrdinaryAttempt)
PR: #721 — squash-merged to master as 707ac0119657; post-squash re-pin to the /call-with-raw master squash commit PERFORMED
```

## Why the pins name the /call-with-raw master squash commit

`707ac0119657` is the master squash-merge commit of PR #721 that carries the guarded-tree
change. The packaged tar (`cmd/build/nativeworker_module.tar`) embeds both out-of-work
modules' source AND their go.mods, so the pins the tar ships are the pins an external
`nativeserve-goget` consumer resolves. Pinning to a PRE-change commit would ship a snapshot
whose `bamlutils` lacks the fields `nativeserve/spine` now links, so that consumer could not
build `nativeserve/spine` at all. The pins name the commit that has the source, and the
selection is a lockstep set.

During PR #721's review the five never moved: they stayed at the master-durable
`880a3c4d693e` (#719's squash), and the guard baseline recorded the re-pin as owed. So this
re-pin went master to master, with no branch pin to repair. It was still the MANDATORY,
IMMEDIATE follow-up after merge, because until it landed master's `nativeserve` named a
`bamlutils` that predates the fields it links. The Slice 7.1b failure (#655) is the general
cost of a pin left behind — a branch pin went red on `nativeserve-goget` the moment the
branch was deleted — and the runbook below is how it is avoided.

## The five pinned selections

Every one of them moves together. They are directory-`replace`d for local
development, so only the version STRINGS reach MVS — which is precisely why a partial
bump is invisible until the out-of-work packaging build fails with
`updates to go.mod needed`.

| # | file | module | current selection |
| --- | --- | --- | --- |
| 1 | `nativeserve/go.mod` | `github.com/invakid404/baml-rest` | `v0.0.0-20261028122515-707ac0119657` |
| 2 | `nativeserve/go.mod` | `github.com/invakid404/baml-rest/bamlutils` | `v0.0.49-0.20261028122515-707ac0119657` |
| 3 | `nativeserve/go.mod` | `github.com/invakid404/baml-rest/worker` | `v0.0.49-0.20261028122515-707ac0119657` |
| 4 | `internal/nativebody/nanollmprepare/go.mod` | `github.com/invakid404/baml-rest/bamlutils` | `v0.0.49-0.20261028122515-707ac0119657` |
| 5 | `internal/nativebody/nanollmprepare/go.mod` | `github.com/invakid404/baml-rest/worker` | `v0.0.49-0.20261028122515-707ac0119657` |

`internal/nativebody/nanollmprepare/go.mod`'s `github.com/invakid404/baml-rest v0.0.48`
is deliberately NOT in this list: it is a released tag, not a pseudo-version tracking a
commit, and the module directory-replaces it.

## What was done for THIS master re-pin (the executed steps)

The re-pin below points all five selections at the `/call-with-raw` widening's MASTER
squash commit; the record was already `RESOLVED` and stays so.

1. **All five selections re-pointed together** to `707ac0119657` (Go-formula stamp
   `20261028122515`, taken from `go mod download -json` rather than hand-computed). The
   edit touched only `require` lines; `nanollmprepare`'s deliberate `baml-rest v0.0.48`
   (a released TAG) and `workerplugin v0.0.48` are untouched, and so is every SHA inside
   the historical prose.
2. **Both `// PIN-STATUS` markers left at `RESOLVED`**: the previous pins were already
   master-durable, so there was nothing to flip.
3. **Both mirrored narratives rewritten** to MASTER-DURABLE naming the `/call-with-raw`
   master squash commit `707ac0119657`, with the scalar-list widening's `880a3c4d693e`
   sentences demoted to `HISTORICAL, SUPERSEDED`.
4. **This file updated** — fenced record (the master commit/stamp, `SLICE:`, `PR:`),
   opening claim, selections table, this section.
5. **Tar regenerated** (`go run ./cmd/build/gen-nativeworker-src`), which is required
   because it embeds both manifests, followed by the codegen-spine guard re-baseline
   (`go test ./internal/codegenspine/ -run TestSourceGuard -update-codegenspine-guard`)
   — the guard hashes the tar and the five pins, so it re-baselines in the same change,
   and its provenance note now names `707ac0119657` for the `/call-with-raw` entry.
6. **Gates re-run** — pin lockstep, packaged-manifest identity, tar freshness, source
   guard, out-of-work build.

A note on TERMINOLOGY, because the two vocabularies differ. The machine-readable
marker takes exactly `OUTSTANDING` or `RESOLVED`: `pinFollowupViolations` rejects
anything else, and the ANCESTRY clause compares master-reachability against those two
literals. "Durable" is the PROSE word for the `RESOLVED` state; `RESOLVED` is what the
guards read.

## The follow-up — PERFORMED (the post-squash re-pin RUNBOOK, executed after merge)

**This has been done for the `/call-with-raw` widening: the pins are MASTER-DURABLE** at
`707ac0119657`. For a branch pin, what makes the post-squash re-pin MANDATORY and IMMEDIATE
after merge is the same failure mode as always: a squash flattens the branch source commit
out of history and the branch is deleted, so until the re-pin lands the five selections
would name a commit that resolves to nothing. `/call-with-raw` never branch-pinned; its
re-pin was owed because the previous master pin predates the `bamlutils` fields
`nativeserve/spine` links. The ordered steps below are the runbook the ORCHESTRATOR
followed once PR #721 squash-merged (step 2 was a no-op); it is retained as the executed
record and as the template for the next slice.

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

DONE post-squash for the scalar-list widening MASTER re-pin (the prior change, after PR
#719 squash-merged; superseded by the re-pin below):

- [x] re-point all five selections to the widening MASTER squash commit `880a3c4d693e`
      (Go-formula stamp `20260907202119`), each with its correct base version
- [x] flip both `// PIN-STATUS` markers + this file to `RESOLVED`,
      `REACHABLE-FROM: master`
- [x] rewrite both narratives to master-durable naming `880a3c4d693e`; demote the
      branch-only `c44e054441e6` text to `HISTORICAL, SUPERSEDED`
- [x] `cmd/build/nativeworker_module.tar` regenerated and
      `internal/codegenspine/guard.json` re-baselined
- [x] pin/tar/guard gates green (`TestFirstPartyPinFollowupIsTracked` now sees a
      master-reachable pin ⇒ RESOLVED); `nativeserve-goget` runs in CI against master

DONE post-squash for the `/call-with-raw` widening MASTER re-pin (THIS change, after PR
#721 squash-merged):

- [x] re-point all five selections to the `/call-with-raw` MASTER squash commit
      `707ac0119657` (Go-formula stamp `20261028122515`), each with its correct base version
- [x] keep both `// PIN-STATUS` markers + this file at `RESOLVED`,
      `REACHABLE-FROM: master` (the previous pins were already master-durable)
- [x] rewrite both narratives to master-durable naming `707ac0119657`; demote the
      scalar-list widening's `880a3c4d693e` text to `HISTORICAL, SUPERSEDED`
- [x] `cmd/build/nativeworker_module.tar` regenerated and
      `internal/codegenspine/guard.json` re-baselined (its provenance note names
      `707ac0119657`)
- [x] pin/tar/guard gates green (`TestFirstPartyPinFollowupIsTracked` sees a
      master-reachable pin ⇒ RESOLVED); `nativeserve-goget` runs in CI against master

Precedent: #677 → #678, #681 → #682, #683 → #684, #686 → #687, #689 → #692, #703,
U1's #708 → #709, U1b's #711 post-squash re-pin, U1c's #713 post-squash re-pin,
M3e-A's #715 post-squash re-pin, U1s's #717 post-squash re-pin, the scalar-list
widening's #719 post-squash re-pin, and the `/call-with-raw` widening's #721 master-to-master
re-pin (this change) are the
instances of this runbook being executed correctly; Slice 7.1b (#655) is what skipping it costs — a branch pin went red
on `nativeserve-goget` the moment the branch was deleted.
