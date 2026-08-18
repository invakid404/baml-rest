# Out-of-`go.work` first-party pin follow-up

This file is the TRACKED record of whether the five first-party pseudo-version
selections below point at a **master** commit, and — while they do not — exactly what
has to be done about it. **Right now they do not.** The serving-cutover S1 change moved
all five to its own branch commit, so this record is `OUTSTANDING` and the follow-up in
the last section has **not** been performed yet.

It is proof material, not documentation. `TestFirstPartyPinFollowupIsTracked`
(`cmd/build/nativeworker_pins_test.go`) parses it on every ordinary `go test ./...`,
cross-checks it against the real `go.mod` files, and — wherever a `master` ref is
resolvable — requires `STATUS` to agree with actual master-reachability. So the
follow-up cannot be quietly forgotten: once the re-pin lands on master the guard goes
RED until this file is flipped, and if the pins move without this file moving with them
it goes red immediately.

`nativeserve/go.mod`'s BUMP RULE header states the general rule. This file is the
CONCRETE, per-change instance of it, which is what the generic comment cannot be.

```
STATUS: OUTSTANDING
PINNED-COMMIT: 7cb69af02b36
PINNED-STAMP: 20260819085842
REACHABLE-FROM: feat/debaml-s1-cohort-admission
SLICE: de-BAML serving cutover S1 — default-deny cohort admission + privacy-safe inventory + bounded telemetry
PR: 675 (branch pin; re-pin to the master squash commit immediately after merge)
```

## Why the pins are branch-only

`7cb69af02b36` is the S1 cutover commit. It installs the DEFAULT-DENY surface/cohort
gate in `nativeserve/admission`, folds every exported telemetry recorder's label
arguments onto their closed sets, adds the config-load configuration inventory, and adds
the direct-parse observation seam.

**This bump carries real cross-module wiring, not just a version string.** S1 changes
BOTH sides of the graph:

- the root/worker side gains the neutral seam — `bamlutils.NativeDirectParseObserveFunc`,
  the `worker/parse.go` call site, and the `workerboot` factory option;
- `nativeserve` supplies the implementation that consumes it
  (`nativeserve.NewDirectParseObserve`), and the packaged worker installs it.

So a consumer resolving the OLD pin gets a `nativeserve` written against a root that
does not have the seam, plus — more quietly — a serve core with **no cohort gate at
all** (admission WIDER than the one reviewed) whose exported recorders still accept
unbounded, secret-shaped label values. Earlier slices' bumps were about a behaviour that
would silently under-claim; this one is about a released worker missing an entire
admission gate.

It cannot yet point at master: the master commit that will carry all of this is the
squash-merge of this change, and it does not exist until the merge happens.
`7cb69af02b36` is reachable only from `feat/debaml-s1-cohort-admission` and will be
flattened out of history by the squash — the exact failure mode the BUMP RULE records
from Slice 7.1b (#655), where a branch pin went red on `nativeserve-goget` once the
branch was deleted.

**What a green `nativeserve-goget` proves now, and what it does not.** It proves an
external consumer can resolve, build and run `nativeserve.New` against the branch tip —
the module graph and every transitive requirement are correct. It does **not** prove
master-durable release, because branch reachability disappears with the branch. Closing
that gap is the follow-up below, and it is mandatory.

## The five pinned selections

Every one of them must move together. They are directory-`replace`d for local
development, so only the version STRINGS reach MVS — which is precisely why a partial
bump is invisible until the out-of-work packaging build fails with
`updates to go.mod needed`.

| # | file | module | current selection |
| --- | --- | --- | --- |
| 1 | `nativeserve/go.mod` | `github.com/invakid404/baml-rest` | `v0.0.0-20260819085842-7cb69af02b36` |
| 2 | `nativeserve/go.mod` | `github.com/invakid404/baml-rest/bamlutils` | `v0.0.49-0.20260819085842-7cb69af02b36` |
| 3 | `nativeserve/go.mod` | `github.com/invakid404/baml-rest/worker` | `v0.0.49-0.20260819085842-7cb69af02b36` |
| 4 | `internal/nativebody/nanollmprepare/go.mod` | `github.com/invakid404/baml-rest/bamlutils` | `v0.0.49-0.20260819085842-7cb69af02b36` |
| 5 | `internal/nativebody/nanollmprepare/go.mod` | `github.com/invakid404/baml-rest/worker` | `v0.0.49-0.20260819085842-7cb69af02b36` |

`internal/nativebody/nanollmprepare/go.mod`'s `github.com/invakid404/baml-rest v0.0.48`
is deliberately NOT in this list: it is a released tag, not a pseudo-version tracking a
commit, and the module directory-replaces it.

## The follow-up — REQUIRED, NOT YET PERFORMED

Every step below is still outstanding. It must be carried out against the master
squash-merge commit of this change, immediately after that merge happens. The
#672 → #673 sequence is the required precedent; a branch-only pseudo-version is not a
durable release.

1. Take the master SHA of the squash-merge commit and its committer timestamp in UTC
   (`git log -1 --format='%H %cd' --date=format-local:%Y%m%d%H%M%S master`, with
   `TZ=UTC`).
2. Re-pin **all five** selections above to that commit, keeping each one's base version
   unchanged — `v0.0.0-<stamp>-<sha12>` for the root module, `v0.0.49-0.<stamp>-<sha12>`
   for `bamlutils` and `worker`.
3. Update this file: set `PINNED-COMMIT`/`PINNED-STAMP` to the new values, set
   `REACHABLE-FROM: master`, set `STATUS: RESOLVED`, and rewrite this section's heading
   and tense to record that it has been performed.
4. Regenerate the packaged worker source, which embeds both manifests:
   `go run ./cmd/build/gen-nativeworker-src`.
5. Re-run the gates, all of which must be green:
   - `GOWORK=off go test -run TestNativeWorkerModuleTarIsFresh ./cmd/build/` — tar freshness;
   - `go test ./cmd/build/...` — including `TestFirstPartyPinFollowupIsTracked`, which is
     what enforces steps 2–3 against each other and against master-ancestry;
   - `go run ./cmd/regenerate-dynclient` then confirm an EMPTY diff — regen idempotence;
   - the `nativeserve-goget` external-consumer probe against the **master** SHA:
     in an empty module, with no checkout / no `replace` / no workspace,
     `GOWORK=off GOFLAGS= GOPRIVATE=github.com/invakid404/baml-rest go get
     github.com/invakid404/baml-rest/nativeserve@<master-sha>` then `go build ./...` then
     `go run ./...`.

Until step 3 is done this record stays `OUTSTANDING` and is the tracked statement that
the released serve core is not yet pinned to a durable commit — and, for this change
specifically, that a consumer resolving the previous pin gets a serve core WITHOUT the
default-deny cohort gate, WITHOUT the exported-recorder label folds, and written against
a root that lacks the direct-parse observation seam.
