# Out-of-`go.work` first-party pin follow-up

This file is the TRACKED record of whether the five first-party pseudo-version
selections below point at a **master** commit. **Right now they do NOT.** They name
`83dde65a20f1`, the LATEST `/parse` UNION burn-down SOURCE commit on
`feat/debaml-parse-union`, because no master commit carries the changed native SAP yet.
The post-squash re-pin runbook in the last section is therefore **OWED** and must be
executed the moment this change squash-merges: the squash will flatten `83dde65a20f1`
out of history and the branch will be deleted, at which point a pin naming it resolves
to nothing.

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
PINNED-COMMIT: 83dde65a20f1
PINNED-STAMP: 20260824225140
REACHABLE-FROM: feat/debaml-parse-union
SLICE: de-BAML /parse UNION burn-down — array union_variant_hint, defaultable-collection class union arms, null-into-composite-union decline, worker-boundary float spelling
PR: (this change; branch feat/debaml-parse-union, source commit 83dde65a20f1)
```

## Why the pins point at a BRANCH commit

`83dde65a20f1` is the union burn-down's own source commit. It carries the NATIVE SAP change
itself — `internal/debaml`'s union coercion — and the packaged serve core is a direct caller
of it, so the pinned root module decides what a native claim actually emits. No MASTER commit
carries that SAP yet, which is the only reason these pins are not master-durable.

**This bump carries real cross-module behaviour, not just a version string.** What it changes
is what the packaged worker RETURNS:

- `nativeserve/canary/serve.go` passes `debaml.Parse` to `execute.DynamicParse` as the
  response parser for the native dynamic serve lane, and `canary/serve_static.go` /
  `canary/serve_static_shadow.go` call `debaml.ParseStaticBundleUnaryCall` /
  `debaml.ParseStaticBundle` for the static lanes. The union burn-down changes those
  answers on four counts:
  - a list whose ELEMENT is a multi-arm union is now COERCED rather than declined, and each
    element is resolved under BAML's cross-element `ctx.union_variant_hint`
    (`coerce_array.rs`) — the previous element's winning arm is tried first and taken
    outright at score 0, which is directly observable (`list<int|float>` over
    `[1.5, 9007199254740993]` emits `9007199254740992`);
  - a class union ARM may now carry a required `list` / string-keyed `map` field: its
    `try_cast` score is summed into the class's, and an ABSENT one is
    `TypeIR::default_value`-filled to `[]` / `{}`, so an all-default arm is a real
    `pick_best` participant instead of a gate decline;
  - a JSON `null` against a NON-nullable union with a composite arm no longer claims
    "non-nullable union rejects null" — BAML's list arm absorbs the null as `[]`, its map
    arm as `{}` — so native DECLINES there instead of out-claiming an error;
  - every emitted float is spelled the way the worker boundary spells it (encoding/json's
    encoder, which `sonic` reproduces) rather than `strconv` `'g'`, which is what makes a
    native claim byte-comparable at all for any magnitude outside `[1e-6, 1e21)`;
  - a REQUIRED class field whose coercion PROVABLY fails and whose type has a
    `TypeIR::default_value` is now DEFAULT-FILLED (`DefaultButHadUnparseableValue`, cost
    2) exactly as `coerce_class` does, so a JSON null against a `string|map<string,int>`
    field serves `{}` natively instead of declining;
  - a class-valued LIST element / MAP value inside a class union arm is held to the
    union-arm class rules, so an arm whose collection carries an out-of-scope class
    DECLINES rather than being admitted by omission.
- `internal/nativebody/nanollmprepare` runs the same parser (`shadow/response.go` sets
  `Parse: debaml.Parse`) and its `cmd/worker` entrypoint is the binary the
  booted-artifact proofs boot — including the packaged `/parse` route proof #685 added;
- the `worker` module, pinned here in lockstep, is where the direct-parse field-order pass
  lives (`worker/direct_parse_schema_order.go`) that makes the native and BAML payloads
  byte-comparable at the worker boundary in the first place.

A consumer resolving a PRE-union pin gets a serve core whose native SAP declines the shapes
this tree now claims, and whose float spelling differs, underneath a manifest describing
this one. That is not a cosmetic disagreement: it is a difference in the bytes a native
claim serves, which is exactly the fact an operator reads this manifest to establish.

The pins CANNOT point at master until this change is squash-merged: no master commit carries
the changed SAP. The branch is based on master `d1f2526e2e7c` (#687), so `83dde65a20f1` is a
descendant of the current master — but a descendant is not master, and the squash will
flatten it out of history just the same, which is why re-pinning to the master squash commit
is the mandatory immediate follow-up. The Slice 7.1b (#655) incident is the precedent for
what happens if that re-pin is skipped — a branch pin went red on `nativeserve-goget` once the
branch was deleted — and \#677 → #678, #681 → #682, #683 → #684 and #686 → #687 are the four
most recent instances of doing it correctly.

**What a green `nativeserve-goget` proves now — and on which run.** The check runs in two
lanes that prove different things. On a `pull_request` it resolves the PR HEAD SHA, so a
green PR run establishes that an external consumer can resolve, build and run
`nativeserve.New` against the branch under review, whose `nativeserve/go.mod` in turn names
`83dde65a20f1` — a commit on the SAME branch, which is resolvable exactly while the branch
lives. That is a branch snapshot, NOT the durable delivery state: the `push` run on master is
what proves durability, and it cannot until the re-pin lands.

## The five pinned selections

Every one of them moves together. They are directory-`replace`d for local
development, so only the version STRINGS reach MVS — which is precisely why a partial
bump is invisible until the out-of-work packaging build fails with
`updates to go.mod needed`.

| # | file | module | current selection |
| --- | --- | --- | --- |
| 1 | `nativeserve/go.mod` | `github.com/invakid404/baml-rest` | `v0.0.0-20260824225140-83dde65a20f1` |
| 2 | `nativeserve/go.mod` | `github.com/invakid404/baml-rest/bamlutils` | `v0.0.49-0.20260824225140-83dde65a20f1` |
| 3 | `nativeserve/go.mod` | `github.com/invakid404/baml-rest/worker` | `v0.0.49-0.20260824225140-83dde65a20f1` |
| 4 | `internal/nativebody/nanollmprepare/go.mod` | `github.com/invakid404/baml-rest/bamlutils` | `v0.0.49-0.20260824225140-83dde65a20f1` |
| 5 | `internal/nativebody/nanollmprepare/go.mod` | `github.com/invakid404/baml-rest/worker` | `v0.0.49-0.20260824225140-83dde65a20f1` |

`internal/nativebody/nanollmprepare/go.mod`'s `github.com/invakid404/baml-rest v0.0.48`
is deliberately NOT in this list: it is a released tag, not a pseudo-version tracking a
commit, and the module directory-replaces it.

## What was done for THIS bump

1. Pushed the LATEST union burn-down SOURCE commit (`83dde65a20f1` — the cold-review
   collection-class restriction, which sits on top of the map/null fix `939f5d7ff1f6`,
   which in turn sits on the original burn-down source `d43120c9de32`) to
   `feat/debaml-parse-union` FIRST, then took its committer timestamp in UTC from Go
   rather than by hand:
   `GOWORK=off GOPRIVATE=github.com/invakid404/baml-rest go mod download -json
   <mod>@83dde65a20f1` was run for the root, `bamlutils` and `worker` off the origin, and
   Go's own stamp↔SHA computation returned exactly the pseudo-versions recorded above
   (root `v0.0.0-20260824225140-83dde65a20f1`; `bamlutils`/`worker`
   `v0.0.49-0.20260824225140-83dde65a20f1`).
2. Moved **all five** selections above to that branch source commit — from the
   post-#687 master commit `05102106c569` originally, and RE-BUMPED off the superseded
   branch source `d43120c9de32`, and again off `939f5d7ff1f6`, as each cold-review fix
   added another source commit — each keeping its base version — `v0.0.0-<stamp>-<sha12>`
   for the root module, `v0.0.49-0.<stamp>-<sha12>` for `bamlutils` and `worker`.
   First-party modules are filesystem-`replace`d, so `go.sum` needed no change; this was a
   string replace, not a `go get`.
3. Flipped this file and BOTH `// PIN-STATUS` markers (`nativeserve/go.mod`,
   `internal/nativebody/nanollmprepare/go.mod`) to `OUTSTANDING`, plus BOTH mirrored
   manifest NARRATIVES, so the machine-readable pin state, the prose and this record all
   agree; the previous master-durable text was demoted to `HISTORICAL, SUPERSEDED`.
4. Regenerated the packaged worker source, which embeds both manifests:
   `go run ./cmd/build/gen-nativeworker-src`.
5. Re-ran the gates.

**If further SOURCE commits are pushed to this branch during review**, steps 1-5 must be
REPEATED against the new tip so the pins always name the LATEST source: a pin naming an
earlier branch commit describes a serve core the tree no longer ships. (The pin/tar commit
itself is not a source commit — it changes only `go.mod` manifests and the tar — so it does
not require re-bumping onto itself.) This has already happened TWICE on this branch: the
cold-review map/null fix landed as a second source commit and the collection-class
restriction as a third, so all five selections were re-bumped `d43120c9de32` ->
`939f5d7ff1f6` -> `83dde65a20f1`, each time with a freshly-resolved stamp.

**What counts as a SOURCE commit, precisely.** "Source" here means content the PACKAGED
serve core resolves through these pins — the root module's non-test packages, `bamlutils`
and `worker`. A commit that touches only files the packaged build can never compile is NOT
one, and re-bumping onto it would be pin churn that proves nothing. The branch has exactly
one such commit, sitting on top of the pins: the parse-recovery disposition-table
registration, which edits only `integration/bamlfuzz_parse_recovery_test.go` (plus this
file) — a `//go:build integration` file in package `integration`, which the tar does not
carry (zero `integration/` entries) and which no packaged module imports. Two checks
establish that rather than asserting it: `go run ./cmd/build/gen-nativeworker-src`
reproduces `cmd/build/nativeworker_module.tar` BYTE-IDENTICALLY (sha256
`30fb5759...4b8b6f94` before and after), and the whole diff from the pinned commit
`83dde65a20f1` to the branch tip is the pin/tar manifest set itself — already carved out
above — plus that one integration test file and this record. NOTHING a packaged module
compiles changed, so the five selections stay on `83dde65a20f1` and the serve core a
consumer resolves is bit-identical to the one this tree ships. Anything touching a
packaged-reachable package re-bumps as usual.

## The follow-up — OWED (the post-squash re-pin RUNBOOK)

**This has NOT been done, and cannot be until this change squash-merges.** The ordered
steps below are MANDATORY and IMMEDIATE once it does: a pre-squash pseudo-version is not a
delivery state, and the moment the branch is deleted a pin naming `83dde65a20f1` resolves
to nothing.

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
`NOTE (de-BAML /parse UNION burn-down): ...` in `nativeserve/go.mod`, and
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
- the opening paragraph ("**Right now they do NOT.**") to say they DO, and that the
  follow-up has been **performed**; this section's heading and tense rewritten to match
- the "current selection" column of the five-selections table to the new versions

### 5. Regenerate the packaged worker source

```bash
go run ./cmd/build/gen-nativeworker-src
```

The tar embeds BOTH manifests, so it carries the pins, the markers and the narratives from
steps 1-3. Skipping this would leave the shipped tar describing a pin state the tree no
longer has.

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
it at the commit whose `nativeserve/go.mod` names the master squash SHA from step 0.

It must run from a genuinely external module — no checkout, no `replace`, no workspace —
so the module and its program were MATERIALIZED first; `go get` / `go build` / `go run` in
a bare directory do nothing:

```bash
# The master tip AFTER the re-pin commit has landed — NOT the squash commit of the
# change being re-pinned. Derived rather than pasted so this block stays
# copy-paste-runnable in a fresh shell; run it from a checkout updated past the re-pin.
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
database yet. CHECK the probe's own `go.mod`: it must resolve the NEW pseudo-versions from
step 0. If it still shows the branch ones, `$SHA` was aimed before the re-pin and the run
proves nothing.

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

Precedent: #677 -> #678, #681 -> #682, #683 -> #684 and #686 -> #687 are the four prior
instances of this runbook being executed correctly; Slice 7.1b (#655) is what skipping it
costs — a branch pin went red on `nativeserve-goget` the moment the branch was deleted.

NOT ONE box above is ticked, and that is the point of this record: the serve core is
currently pinned to a BRANCH commit (`83dde65a20f1`), which the squash will flatten and the
branch deletion will orphan. This file stays `STATUS: OUTSTANDING` — and
`TestFirstPartyPinFollowupIsTracked` stays RED once the pins reach master — until every box
is ticked.
