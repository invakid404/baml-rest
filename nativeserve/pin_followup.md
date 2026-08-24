# Out-of-`go.work` first-party pin follow-up

This file is the TRACKED record of whether the five first-party pseudo-version
selections below point at a **master** commit. **Right now they do.** The `/parse`
burn-down batch 1 change was squash-merged to master as `05102106c569` (#686), and all
five selections have been re-pinned from the now-orphaned branch source `251b09219943`
to that master commit. The post-squash re-pin runbook in the last section has therefore
been **PERFORMED**: the tar was regenerated and the probes re-run. This record is the
durable delivery state — the serve core is pinned to a commit that survives the branch
deletion.

It is proof material, not documentation. `TestFirstPartyPinFollowupIsTracked`
(`cmd/build/nativeworker_pins_test.go`) parses it on every ordinary `go test ./...`,
cross-checks it against the real `go.mod` files, and — wherever a `master` ref is
resolvable — requires the recorded status to agree with actual master-reachability. So
the follow-up cannot be quietly forgotten: once the re-pin lands on master the guard goes
RED until this file is flipped, and if the pins move without this file moving with them
it goes red immediately.

`nativeserve/go.mod`'s BUMP RULE header states the general rule. This file is the
CONCRETE, per-change instance of it, which is what the generic comment cannot be.

```
STATUS: RESOLVED
PINNED-COMMIT: 05102106c569
PINNED-STAMP: 20260824191050
REACHABLE-FROM: master
SLICE: de-BAML /parse burn-down batch 1 — native-side absent-optional null + BAML TypeBuilder field order, lenient map keys
PR: #686 (master squash 05102106c569024f80eb2592b0059fb7431c220a)
```

## Why the pins now point at master

`05102106c569` is the master squash-merge of the burn-down batch 1 change (#686). It
carries the NATIVE SAP change itself — `internal/debaml`'s class and map coercion — and the
packaged serve core is a direct caller of it, so the pinned root module decides what a
native claim actually emits.

**This bump carries real cross-module behaviour, not just a version string.** The
functional diff is small, but what it changes is what the packaged worker RETURNS:

- `nativeserve/canary/serve.go` passes `debaml.Parse` to `execute.DynamicParse` as the
  response parser for the native dynamic serve lane, and `canary/serve_static.go` /
  `canary/serve_static_shadow.go` call `debaml.ParseStaticBundleUnaryCall` /
  `debaml.ParseStaticBundle` for the static lanes. Batch 1 changes both of those answers:
  a coerced class now emits EVERY declared field, spelling an absent optional as an
  explicit `null` (which is what BAML emits and what the static absent-optional
  normalizer was already written for), and a map key that matches no enum value or
  string-literal arm is KEPT under its original string instead of declining the whole
  map;
- `internal/nativebody/nanollmprepare` runs the same parser (`shadow/response.go` sets
  `Parse: debaml.Parse`) and its `cmd/worker` entrypoint is the binary the
  booted-artifact proofs boot — including the packaged `/parse` route proof #685 added;
- the `worker` module, pinned here in lockstep, is where the new direct-parse
  field-order pass lives (`worker/direct_parse_schema_order.go`): it declares the schema
  in the order BAML's TypeBuilder will actually be populated in, which is what makes the
  native and BAML payloads byte-comparable at the worker boundary at all.

A consumer resolving a PRE-batch-1 pin got a serve core whose native SAP was shaped for
the previous worker-boundary contract — omitting absent optionals, declining a whole map on
a key miss — underneath a manifest describing this one. That is not a cosmetic
disagreement: it is a difference in the bytes a native claim serves, which is exactly the
fact an operator reads this manifest to establish. The re-pin closes that gap: the pinned
commit is now the master commit whose SAP the tree ships.

The pins could not point at master until this change was squash-merged: no master commit
carried the changed SAP. The branch was based on master `f2989cf149c7` (#685), so
`251b09219943` was a descendant of the then-current master — but a descendant is not
master, and the squash flattened it out of history just the same, which is why re-pinning
to the master squash commit was the mandatory immediate follow-up. The Slice 7.1b (#655)
incident is the precedent for what happens if that re-pin is skipped — a branch pin went
red on `nativeserve-goget` once the branch was deleted — and
\#677 → #678, #681 → #682 and #683 → #684 are the three most recent instances of doing it
correctly; this change (#686) is the fourth.

**What a green `nativeserve-goget` proves now — and on which run.** The check runs in two
lanes that prove different things. On a `pull_request` it resolves the PR HEAD SHA, so a
green PR run establishes only that an external consumer can resolve, build and run
`nativeserve.New` against the branch under review, whose `nativeserve/go.mod` in turn names
`05102106c569`, the master commit. The recorded proof of MASTER durability is instead the
`push` run on master: now that these selections sit on the master squash commit, that lane
resolves a SHA the squash cannot flatten away. That is what makes this the durable delivery
state rather than a branch snapshot.

## The five pinned selections

Every one of them moves together. They are directory-`replace`d for local
development, so only the version STRINGS reach MVS — which is precisely why a partial
bump is invisible until the out-of-work packaging build fails with
`updates to go.mod needed`.

| # | file | module | current selection |
| --- | --- | --- | --- |
| 1 | `nativeserve/go.mod` | `github.com/invakid404/baml-rest` | `v0.0.0-20260824191050-05102106c569` |
| 2 | `nativeserve/go.mod` | `github.com/invakid404/baml-rest/bamlutils` | `v0.0.49-0.20260824191050-05102106c569` |
| 3 | `nativeserve/go.mod` | `github.com/invakid404/baml-rest/worker` | `v0.0.49-0.20260824191050-05102106c569` |
| 4 | `internal/nativebody/nanollmprepare/go.mod` | `github.com/invakid404/baml-rest/bamlutils` | `v0.0.49-0.20260824191050-05102106c569` |
| 5 | `internal/nativebody/nanollmprepare/go.mod` | `github.com/invakid404/baml-rest/worker` | `v0.0.49-0.20260824191050-05102106c569` |

`internal/nativebody/nanollmprepare/go.mod`'s `github.com/invakid404/baml-rest v0.0.48`
is deliberately NOT in this list: it is a released tag, not a pseudo-version tracking a
commit, and the module directory-replaces it.

## What was done for THIS re-pin

1. Took the SHA of the burn-down batch 1 master squash-merge commit and its committer
   timestamp in UTC — `05102106c569` / `20260824191050`. The stamp was not hand-trusted:
   `GOWORK=off GOPRIVATE=github.com/invakid404/baml-rest go mod download -json
   <mod>@05102106c569` was run for the root, `bamlutils` and `worker` off the origin AFTER
   the squash landed, and Go's own stamp↔SHA computation returned exactly the
   pseudo-versions recorded above (root `v0.0.0-20260824191050-05102106c569`;
   `bamlutils`/`worker` `v0.0.49-0.20260824191050-05102106c569`).
2. Moved **all five** selections above from the orphaned branch source `251b09219943`
   to that master commit, each keeping its base version — `v0.0.0-<stamp>-<sha12>` for the
   root module, `v0.0.49-0.<stamp>-<sha12>` for `bamlutils` and `worker`. First-party
   modules are filesystem-`replace`d, so `go.sum` needed no change; this was a string
   replace, not a `go get`.
3. Flipped this file and BOTH `// PIN-STATUS` markers (`nativeserve/go.mod`,
   `internal/nativebody/nanollmprepare/go.mod`) to `RESOLVED`, plus BOTH mirrored
   manifest NARRATIVES, so the machine-readable pin state, the prose and this record all
   agree; the branch-only text was demoted to `HISTORICAL, SUPERSEDED`.
4. Regenerated the packaged worker source, which embeds both manifests:
   `go run ./cmd/build/gen-nativeworker-src`.
5. Re-ran the gates.

## The follow-up — PERFORMED (the post-squash re-pin RUNBOOK)

**This has been done.** The ordered steps below were carried out immediately after the
change squash-merged to master. A pre-squash pseudo-version is not the final delivery
state; this record no longer names one. The steps are kept here as the audit trail and as
the template the next such re-pin follows.

### 0. Got the durable commit and its stamp — from Go, not by hand

Took the SHA of the **master squash-merge commit** of this PR (not the branch tip, which
the squash flattens away) and confirmed the SHA-to-stamp pair Go itself computes:

```bash
SHA=05102106c569                   # master squash-merge of #686
for m in github.com/invakid404/baml-rest \
         github.com/invakid404/baml-rest/bamlutils \
         github.com/invakid404/baml-rest/worker; do
  GOWORK=off GOPRIVATE=github.com/invakid404/baml-rest GOFLAGS= \
    go mod download -json "$m@$SHA" | grep '"Version"'
done
```

The versions this printed were used verbatim. The `<stamp>` was NOT hand-computed: a
timestamp off by one second yields a pseudo-version that resolves to nothing, and the
failure surfaces far from the edit.

Two BASE forms, each selection keeping its own base:

- root module: `v0.0.0-<stamp>-<sha12>`
- `bamlutils` and `worker`: `v0.0.49-0.<stamp>-<sha12>` (a DOT after the `-0`, not a dash)

### 1. Re-pointed all FIVE selections, together

The five are listed in "The five pinned selections" above. All were moved in one edit:

| file | modules re-pointed |
| --- | --- |
| `nativeserve/go.mod` | `github.com/invakid404/baml-rest`, `.../bamlutils`, `.../worker` |
| `internal/nativebody/nanollmprepare/go.mod` | `.../bamlutils`, `.../worker` |

A partial bump is invisible locally — both modules directory-`replace` these paths, so
only the version STRINGS reach MVS — and fails later in the out-of-work packaging build
with `updates to go.mod needed`.

### 2. Flipped BOTH machine-readable markers

Set the `// PIN-STATUS:` line in **each** manifest from `OUTSTANDING` to `RESOLVED`:

- `nativeserve/go.mod`
- `internal/nativebody/nanollmprepare/go.mod`

`cmd/build`'s `TestPackagedManifestsMatchTheTrackedPins` requires each manifest to carry
exactly ONE marker and to agree with this file's `STATUS`, inside the packaged tar as well
as in the tree.

### 3. Flipped BOTH mirrored manifest NARRATIVES

Each manifest also carries a prose paragraph describing where the pins stand:
`NOTE (de-BAML /parse burn-down batch 1): ...` in `nativeserve/go.mod`, and
`RIGHT NOW they are ...` in `internal/nativebody/nanollmprepare/go.mod`. BOTH were rewritten
to say the pins are now MASTER-durable and to name the master squash commit
`05102106c569`, and the branch-only sentences were demoted into the
`HISTORICAL, SUPERSEDED` paragraph beneath them.

This step is not cosmetic and no test covers it: the markers are what the GUARDS read, the
narratives are what a HUMAN reads, and a narrative saying "BRANCH-ONLY" under a marker
saying `RESOLVED` tells a reviewer the opposite of the truth. That exact drift happened
once — the S1 text was left in place after the S2 bump — which is why both manifests now
carry "Do not treat this comment as the authority".

### 4. Updated THIS file

- the recorded status to `RESOLVED`
- the pinned commit and stamp to the new values from step 0 (`05102106c569` /
  `20260824191050`)
- `REACHABLE-FROM:` to `master`
- `PR:` to the merged PR number and the master squash SHA
- the opening paragraph ("**Right now they do.**") to say they DO, and that the
  follow-up has been **performed**; this section's heading and tense rewritten to match
- the "current selection" column of the five-selections table to the new versions

### 5. Regenerated the packaged worker source

```bash
go run ./cmd/build/gen-nativeworker-src
```

The tar embeds BOTH manifests, so it carries the pins, the markers and the narratives from
steps 1-3. Skipping this would leave the shipped tar describing a pin state the tree no
longer has.

### 6. Re-ran the gates — all four

```bash
GOWORK=off go test -run TestNativeWorkerModuleTarIsFresh ./cmd/build/   # tar freshness
go test ./cmd/build/...                                                # incl. TestFirstPartyPinFollowupIsTracked
go run ./cmd/regenerate-dynclient && git status --porcelain             # must print NOTHING
```

`TestFirstPartyPinFollowupIsTracked` is what enforces steps 1-4 against each other and
against master-ancestry: once the pins are on a master commit it stays RED until this file
says `RESOLVED`, which it now does.

Then the **`nativeserve-goget` external-consumer probe**, against the MASTER SHA. It must
run from a genuinely external module — no checkout, no `replace`, no workspace — so the
module and its program were MATERIALIZED first; `go get` / `go build` / `go run` in a
bare directory do nothing:

```bash
# The resolved master squash commit. Derived rather than pasted so this block stays
# copy-paste-runnable in a fresh shell, independent of Step 0 — run it from an
# UPDATED checkout right after the merge, where master's tip IS that commit.
SHA="$(git rev-parse --verify master^{commit})"
work="$(mktemp -d)" && cd "$work"
go mod init external.consumer/probe
cat > main.go <<'EOF'
package main

import (
	"fmt"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/invakid404/baml-rest/nativeserve"
)

func main() {
	fn, err := nativeserve.New(prometheus.NewRegistry())
	if err != nil {
		panic(err)
	}
	if fn == nil {
		panic("nativeserve.New returned a nil serve func")
	}
	fmt.Println("ok: nativeserve.New resolved + built as an external consumer")
}
EOF
export CGO_ENABLED=1 GOPRIVATE=github.com/invakid404/baml-rest
GOWORK=off GOFLAGS= go get "github.com/invakid404/baml-rest/nativeserve@$SHA"
GOWORK=off GOFLAGS= go build ./...
GOWORK=off GOFLAGS= go run ./...
```

`CGO_ENABLED=1` is required: `nativeserve` links `nanollm-ffi`, so the probe exercises the
documented CGO build recipe rather than a stub. `GOPRIVATE` keeps the first-party modules
resolving straight from the origin, because a fresh master commit is not in the checksum
database yet. The probe's own `go.mod` resolved the NEW pseudo-versions from step 0, not
the branch ones.

### Definition of done

- [x] all five selections name the master squash commit, each with its correct base version
- [x] both `// PIN-STATUS` markers say `RESOLVED`
- [x] both mirrored manifest narratives say master-durable, with the branch-only text
      demoted to `HISTORICAL, SUPERSEDED`
- [x] this file says `STATUS: RESOLVED`, `REACHABLE-FROM: master`, with the new
      commit/stamp and an updated selections table
- [x] `cmd/build/nativeworker_module.tar` regenerated
- [x] tar freshness, `./cmd/build/...`, dynclient regen-idempotence and `nativeserve-goget`
      all green

Precedent: #677 -> #678, #681 -> #682 and #683 -> #684 are the three prior instances of
this runbook being executed correctly, and #686 (this change) is the fourth; Slice 7.1b
(#655) is what skipping it costs — a branch pin went red on `nativeserve-goget` the moment
the branch was deleted.

Every box above is ticked: this record is the tracked statement that the serve core is now
pinned to the master squash commit (`05102106c569`), which survives the branch deletion,
and that the post-squash re-pin owed after the merge has been performed.
