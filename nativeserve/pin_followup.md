# Out-of-`go.work` first-party pin follow-up

This file is the TRACKED record of whether the five first-party pseudo-version
selections below point at a **master** commit. **Right now they do not.** The `/parse`
burn-down batch 1 change moved all five to its own BRANCH commit `251b09219943` on
`feat/debaml-parse-burndown-1`, because the native SAP behaviour the packaged serve core
runs was changed by that very change and no master commit carries it yet. The post-squash
re-pin runbook in the last section is therefore **OWED**: the moment this change
squash-merges to master, all five must be re-pinned to the squash commit, the tar
regenerated, and the probes re-run. A pre-squash pseudo-version is not the final delivery
state, and this record says so rather than letting a reader assume otherwise.

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
STATUS: OUTSTANDING
PINNED-COMMIT: 251b09219943
PINNED-STAMP: 20260824164954
REACHABLE-FROM: feat/debaml-parse-burndown-1
SLICE: de-BAML /parse burn-down batch 1 — native-side absent-optional null + BAML TypeBuilder field order, lenient map keys
PR: pending (branch feat/debaml-parse-burndown-1; post-squash re-pin OWED)
```

## Why the pins are branch-only

`251b09219943` is the burn-down batch 1 source commit. It changes the NATIVE SAP itself —
`internal/debaml`'s class and map coercion — and the packaged serve core is a direct
caller of it, so the pinned root module decides what a native claim actually emits.

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

A consumer resolving the OLD pin gets a serve core whose native SAP is shaped for the
previous worker-boundary contract — omitting absent optionals, declining a whole map on a
key miss — underneath a manifest describing this one. That is not a cosmetic
disagreement: it is a difference in the bytes a native claim serves, which is exactly the
fact an operator reads this manifest to establish.

The pins cannot point at master until this change is squash-merged: no master commit
carries the changed SAP. The branch is based on master `f2989cf149c7` (#685), so
`251b09219943` is a descendant of the current master — but a descendant is not master,
and the squash will flatten it out of history just the same, which is why re-pinning to
the master squash commit is the mandatory immediate follow-up. The Slice 7.1b (#655)
incident is the precedent for what happens if that re-pin is skipped — a branch pin went
red on `nativeserve-goget` once the branch was deleted — and
\#677 → #678, #681 → #682 and #683 → #684 are the three most recent instances of doing it
correctly.

**What a green `nativeserve-goget` proves now — and on which run.** The check runs in two
lanes that prove different things. On a `pull_request` it resolves the PR HEAD SHA, so a
green PR run establishes only BRANCH durability — that an external consumer can resolve,
build and run `nativeserve.New` against the branch under review, whose `nativeserve/go.mod`
in turn names `251b09219943`, another commit on that same branch. The recorded proof of
MASTER durability is instead the `push` run on master, and while these selections sit on a
branch commit even THAT lane can only prove branch-durability: the module graph it tests
still resolves a SHA the squash will flatten away. A green PR run must NOT be read as a
master proof. That is the whole reason the re-pin runbook below is mandatory rather than
tidy-up — the day `feat/debaml-parse-burndown-1` is deleted, an unfixed pin goes red.

## The five pinned selections

Every one of them must move together. They are directory-`replace`d for local
development, so only the version STRINGS reach MVS — which is precisely why a partial
bump is invisible until the out-of-work packaging build fails with
`updates to go.mod needed`.

| # | file | module | current selection |
| --- | --- | --- | --- |
| 1 | `nativeserve/go.mod` | `github.com/invakid404/baml-rest` | `v0.0.0-20260824164954-251b09219943` |
| 2 | `nativeserve/go.mod` | `github.com/invakid404/baml-rest/bamlutils` | `v0.0.49-0.20260824164954-251b09219943` |
| 3 | `nativeserve/go.mod` | `github.com/invakid404/baml-rest/worker` | `v0.0.49-0.20260824164954-251b09219943` |
| 4 | `internal/nativebody/nanollmprepare/go.mod` | `github.com/invakid404/baml-rest/bamlutils` | `v0.0.49-0.20260824164954-251b09219943` |
| 5 | `internal/nativebody/nanollmprepare/go.mod` | `github.com/invakid404/baml-rest/worker` | `v0.0.49-0.20260824164954-251b09219943` |

`internal/nativebody/nanollmprepare/go.mod`'s `github.com/invakid404/baml-rest v0.0.48`
is deliberately NOT in this list: it is a released tag, not a pseudo-version tracking a
commit, and the module directory-replaces it.

## What was done for THIS bump

1. Took the branch SHA of the burn-down batch 1 source commit and its committer timestamp
   in UTC — `251b09219943` / `20260824164954`. The stamp was not hand-trusted: `GOWORK=off
   GOPRIVATE=github.com/invakid404/baml-rest go mod download -json <mod>@251b09219943`
   was run for the root, `bamlutils` and `worker` off the origin AFTER the source commit
   was pushed, and Go's own stamp↔SHA computation returned exactly the pseudo-versions
   recorded above.
2. Moved **all five** selections above to that commit, each keeping its base version
   — `v0.0.0-<stamp>-<sha12>` for the root module, `v0.0.49-0.<stamp>-<sha12>` for
   `bamlutils` and `worker`.
3. Updated this file and BOTH `// PIN-STATUS` markers (`nativeserve/go.mod`,
   `internal/nativebody/nanollmprepare/go.mod`) to `OUTSTANDING`, plus BOTH mirrored
   manifest NARRATIVES, so the machine-readable pin state, the prose and this record all
   agree. (The five move together on every revision of this change and never separately.
   A rebase and an edit both rewrite the source commit, so either re-opens this record —
   the pins name a commit, not a change.)
4. Regenerated the packaged worker source, which embeds both manifests:
   `go run ./cmd/build/gen-nativeworker-src`.
5. Re-ran the gates.

## The follow-up — OWED (the post-squash re-pin RUNBOOK)

**This has NOT been done yet, and it is not optional.** Carry out the ordered steps below
immediately after this change squash-merges to master. A pre-squash pseudo-version is not
the final delivery state; this record names one right now and must stop doing so.
Nothing here is optional and nothing here is inferable — an earlier instance of this
runbook left two steps implicit (flipping the mirrored manifest NARRATIVES, and
materializing the external probe's module and program), which is why they are numbered
steps below.

### 0. Get the durable commit and its stamp — from Go, not by hand

Take the SHA of the **master squash-merge commit** of this PR (not the branch tip, which
the squash flattens away) and confirm the SHA-to-stamp pair Go itself computes:

```bash
SHA=<master-squash-sha>            # e.g. from `git log --first-parent master`
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

Expect two BASE forms, and keep each selection's own base:

- root module: `v0.0.0-<stamp>-<sha12>`
- `bamlutils` and `worker`: `v0.0.49-0.<stamp>-<sha12>` (a DOT after the `-0`, not a dash)

### 1. Re-point all FIVE selections, together

The five are listed in "The five pinned selections" above. Move all of them in one edit:

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

Each manifest also carries a prose paragraph describing where the pins currently stand:
`NOTE (de-BAML /parse burn-down batch 1): these pins are BRANCH-ONLY ...` in
`nativeserve/go.mod`, and `RIGHT NOW they are BRANCH-ONLY: ...` in
`internal/nativebody/nanollmprepare/go.mod`. Rewrite BOTH to say the pins are now
MASTER-durable and to name the master squash commit, and demote the branch-only sentences
into the `HISTORICAL, SUPERSEDED` paragraph beneath them.

This step is not cosmetic and no test covers it: the markers are what the GUARDS read, the
narratives are what a HUMAN reads, and a narrative saying "BRANCH-ONLY" under a marker
saying `RESOLVED` tells a reviewer the opposite of the truth. That exact drift happened
once — the S1 text was left in place after the S2 bump — which is why both manifests now
carry "Do not treat this comment as the authority".

### 4. Update THIS file

- `STATUS:` to `RESOLVED`
- `PINNED-COMMIT:` / `PINNED-STAMP:` to the new values from step 0
- `REACHABLE-FROM:` to `master`
- `PR:` to the merged PR number and the master squash SHA
- the opening paragraph ("**Right now they do not.**") to say they DO, and that the
  follow-up has been **performed**; rewrite this section's heading and tense to match
- the "current selection" column of the five-selections table to the new versions

### 5. Regenerate the packaged worker source

```bash
go run ./cmd/build/gen-nativeworker-src
```

The tar embeds BOTH manifests, so it carries the pins, the markers and the narratives from
steps 1-3. Skip this and the shipped tar describes a pin state the tree no longer has.

### 6. Re-run the gates — all four

```bash
GOWORK=off go test -run TestNativeWorkerModuleTarIsFresh ./cmd/build/   # tar freshness
go test ./cmd/build/...                                                # incl. TestFirstPartyPinFollowupIsTracked
go run ./cmd/regenerate-dynclient && git status --porcelain             # must print NOTHING
```

`TestFirstPartyPinFollowupIsTracked` is what enforces steps 1-4 against each other and
against master-ancestry: once the pins are on a master commit it goes RED until this file
says `RESOLVED`.

Then the **`nativeserve-goget` external-consumer probe**, against the MASTER SHA. It must
run from a genuinely external module — no checkout, no `replace`, no workspace — so the
module and its program have to be MATERIALIZED first; `go get` / `go build` / `go run` in a
bare directory do nothing:

```bash
# The resolved master squash commit. Derived rather than pasted so this block stays
# copy-paste-runnable in a fresh shell, independent of Step 0 — run it from an
# UPDATED checkout right after the merge, where master's tip IS that commit.
# (Until the re-pin lands, PINNED-COMMIT above is a BRANCH sha; probing that proves
# branch durability only, which is the whole reason this step exists.)
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
database yet. Confirm the probe's own `go.mod` resolved the NEW pseudo-versions from step
0, not the branch ones.

### Definition of done

- [ ] all five selections name the master squash commit, each with its correct base version
- [ ] both `// PIN-STATUS` markers say `RESOLVED`
- [ ] both mirrored manifest narratives say master-durable, with the branch-only text
      demoted to `HISTORICAL, SUPERSEDED`
- [ ] this file says `STATUS: RESOLVED`, `REACHABLE-FROM: master`, with the new
      commit/stamp and an updated selections table
- [ ] `cmd/build/nativeworker_module.tar` regenerated
- [ ] tar freshness, `./cmd/build/...`, dynclient regen-idempotence and `nativeserve-goget`
      all green

Precedent: #677 -> #678, #681 -> #682 and #683 -> #684 are the three most recent instances
of this runbook being executed correctly; Slice 7.1b (#655) is what skipping it costs — a
branch pin went red on `nativeserve-goget` the moment the branch was deleted.

No box above is ticked yet: this record is the tracked statement that the serve core is
currently pinned to a BRANCH commit (`251b09219943`), which the squash-merge will flatten
out of history, and that re-pointing it at the resulting master commit is owed.
