# Out-of-`go.work` first-party pin follow-up

This file is the TRACKED record of whether the five first-party pseudo-version
selections below point at a **master** commit. **They now DO.** They name
`062871154d95`, the current `master` tip, and the post-squash re-pin runbook in the last
section has been **PERFORMED** — this change is it.

The state it repaired was real, not hypothetical. The pins named `83dde65a20f1` on
`feat/debaml-parse-union`; #689 squash-merged that branch as `cf03786a1fac` and the branch
was deleted, which flattened `83dde65a20f1` (and the two source commits beneath it,
`d43120c9de32` and `939f5d7ff1f6`) out of history. From that moment the five selections
named a commit that resolves to NOTHING, so master's own packaged worker could not resolve
the parser it ships — the exact Slice 7.1b failure this record exists to prevent. Every
box in "Definition of done" is now ticked.

It is proof material, not documentation. `TestFirstPartyPinFollowupIsTracked`
(`cmd/build/nativeworker_pins_test.go`) parses it on every ordinary `go test ./...`,
cross-checks it against the real `go.mod` files, and — wherever a `master` ref is
resolvable — requires the recorded status to agree with actual master-reachability. So
the follow-up cannot be quietly forgotten: once the re-pin lands on master the guard goes
RED until this file is flipped, and if the pins move without this file moving with them
it goes red immediately. That is what forced this change: with the pins moved to master
and this file still reading `OUTSTANDING`, the ANCESTRY clause fails.

`nativeserve/go.mod`'s BUMP RULE header states the general rule. This file is the
CONCRETE, per-change instance of it, which is what the generic comment cannot be.

```
STATUS: RESOLVED
PINNED-COMMIT: 062871154d95
PINNED-STAMP: 20260825064313
REACHABLE-FROM: master
SLICE: de-BAML /parse UNION burn-down — array union_variant_hint, defaultable-collection class union arms, null-into-composite-union decline, worker-boundary float spelling
PR: #689 squash-merged to master as cf03786a1fac; these pins name master tip 062871154d95 (M1 #691 on top of it); re-pinned by THIS change
```

## Why the pins point at MASTER — and at the TIP, not at #689's squash alone

`062871154d95` is the current `master` tip. It carries the NATIVE SAP change —
`internal/debaml`'s union coercion — because #689's squash `cf03786a1fac` put it on master
and the M1 codegen spine (#691), which landed on top, did not touch `internal/debaml`. The
packaged serve core is a direct caller of that package, so the pinned root module decides
what a native claim actually emits; pinning it to a commit that lacks the union parser
would ship a serve core whose answers disagree with this tree's.

**Why the TIP rather than the squash commit `cf03786a1fac`.** The runbook's step 0 says
"the master squash-merge commit of this PR", which was written when nothing had landed on
top. Something has: M1 (#691) changed `bamlutils/embed.go` and
`bamlutils/projectdescriptor/descriptor.go` — and `bamlutils` is TWO of these five pinned
selections. Pinning to `cf03786a1fac` would therefore resolve a `bamlutils` that master has
since moved past, so the packaged worker would build against a sibling that is not the one
this tree ships. The tip is the only commit whose root AND `bamlutils` are both current,
and it is what the standing "the pins always name the LATEST source" rule requires. M1 was
otherwise pin- and tar-neutral: `internal/debaml` untouched, and
`cmd/build/nativeworker_module.tar` not in its diff at all.

Three checks establish that the pinned tip really carries the union parser, rather than
assuming it from the merge graph — run against the RESOLVED module in the module cache,
not the worktree:

- the resolved root module's `internal/debaml` contains `checkUnionClassField`
  (`parse.go`), `jsonFloatFormat` (`coerce.go`) and `union_variant_hint`
  (`alias_coerce.go`, `coerce.go`, `pair_guard.go`) in NON-test sources;
- `diff -rq` between the resolved module's `internal/debaml` and this checkout's reports
  no differences at all — what a consumer downloads IS what this tree ships;
- `83dde65a20f1` is confirmed NOT reachable from `master`, which is what made the previous
  pins orphaned rather than merely stale.

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
`nativeserve.New` against the branch under review. Its `nativeserve/go.mod` now names
`062871154d95`, a MASTER commit, so — unlike every branch-only cut of this record — that
resolution does not depend on any branch continuing to exist. The `push` run on master is
still the one that proves durability end to end, and it can now pass: the durable delivery
state is what this change establishes.

## The five pinned selections

Every one of them moves together. They are directory-`replace`d for local
development, so only the version STRINGS reach MVS — which is precisely why a partial
bump is invisible until the out-of-work packaging build fails with
`updates to go.mod needed`.

| # | file | module | current selection |
| --- | --- | --- | --- |
| 1 | `nativeserve/go.mod` | `github.com/invakid404/baml-rest` | `v0.0.0-20260825064313-062871154d95` |
| 2 | `nativeserve/go.mod` | `github.com/invakid404/baml-rest/bamlutils` | `v0.0.49-0.20260825064313-062871154d95` |
| 3 | `nativeserve/go.mod` | `github.com/invakid404/baml-rest/worker` | `v0.0.49-0.20260825064313-062871154d95` |
| 4 | `internal/nativebody/nanollmprepare/go.mod` | `github.com/invakid404/baml-rest/bamlutils` | `v0.0.49-0.20260825064313-062871154d95` |
| 5 | `internal/nativebody/nanollmprepare/go.mod` | `github.com/invakid404/baml-rest/worker` | `v0.0.49-0.20260825064313-062871154d95` |

`internal/nativebody/nanollmprepare/go.mod`'s `github.com/invakid404/baml-rest v0.0.48`
is deliberately NOT in this list: it is a released tag, not a pseudo-version tracking a
commit, and the module directory-replaces it.

## What was done for THIS re-pin (the executed runbook)

Steps 0-6 of the runbook below, in order, against master tip `062871154d95`:

0. **Stamp resolved by Go, off the origin — never hand-computed.**
   `GOWORK=off GOPRIVATE=github.com/invakid404/baml-rest go mod download -json <mod>@062871154d95`
   was run for the root, `bamlutils` and `worker`, and Go returned
   `v0.0.0-20260825064313-062871154d95` for the root and
   `v0.0.49-0.20260825064313-062871154d95` for the other two — each keeping its own base
   version. Those strings are used verbatim.
1. **All five selections re-pointed together**, off the orphaned `83dde65a20f1`. The edit
   touched only `require` lines: `nanollmprepare`'s deliberate `baml-rest v0.0.48` (a
   released TAG, not a pseudo-version) is untouched, and so is every SHA inside the
   historical prose.
2. **Both `// PIN-STATUS` markers flipped** `OUTSTANDING` -> `RESOLVED`, one per manifest.
3. **Both mirrored narratives rewritten** to master-durable, with the branch-only
   sentences demoted into each manifest's `HISTORICAL, SUPERSEDED` paragraph.
4. **This file rewritten** — fenced record, opening claim, selections table, section
   headings and the checklist below.
5. **Tar regenerated** (`go run ./cmd/build/gen-nativeworker-src`), which is required
   because it embeds both manifests. Master's committed tar was FIRST proven
   self-consistent: on the pristine tip the generator reproduced it byte-identically at
   sha256 `30fb5759...4b8b6f94` — #689's union tar, which M1 did not touch — so the only
   delta this change writes into it is the pin state from steps 1-3.
6. **Gates re-run** — see "Definition of done".

A note on TERMINOLOGY, because the two vocabularies differ. The machine-readable marker
takes exactly `OUTSTANDING` or `RESOLVED`: `pinFollowupViolations` rejects anything else,
and the ANCESTRY clause compares master-reachability against those two literals. "Durable"
is the PROSE word for the same state; `RESOLVED` is what the guards read, and the runbook's
own definition of done has always spelled it that way.

## What was done for the original UNION bump (HISTORICAL — this is what was re-pinned)

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

**If further SOURCE commits are pushed during review**, steps 1-5 must be
REPEATED against the new tip so the pins always name the LATEST source: a pin naming an
earlier commit describes a serve core the tree no longer ships. That rule is why this
re-pin names the master TIP and not #689's squash commit. (The pin/tar commit
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
`83dde65a20f1` to the branch tip was the pin/tar manifest set itself — already carved out
above — plus that one integration test file and this record. NOTHING a packaged module
compiles changed, so the five selections STAYED on `83dde65a20f1` for the rest of the
branch's life. Anything touching a packaged-reachable package re-bumps as usual. The same
rule is what put THIS re-pin on the master tip rather than on #689's squash commit: M1 did
touch `bamlutils`, so it IS a source commit by this definition.

## The follow-up — PERFORMED (the post-squash re-pin RUNBOOK, as executed)

**This HAS now been done: this change is it**, and "What was done for THIS re-pin" above
records each step with its evidence. The runbook is kept in full and in the imperative
because it is the reusable procedure for the NEXT slice that has to pin to a branch
commit — the sequence has been executed five times now (#678, #682, #684, #687 and this
one), and every time the value came from following it literally rather than from
remembering it.

What made it MANDATORY and IMMEDIATE here was not a rule but a broken tree: #689's squash
flattened `83dde65a20f1` away and the branch was deleted, so until this change landed the
five selections named a commit that resolves to nothing.

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

- [x] all five selections name the master commit `062871154d95` (the TIP — see "Why the
      pins point at MASTER"), each with its correct base version
- [x] both `// PIN-STATUS` markers say `RESOLVED`
- [x] both mirrored manifest narratives say master-durable, with the branch-only text
      demoted to `HISTORICAL, SUPERSEDED`
- [x] this file says `STATUS: RESOLVED`, `REACHABLE-FROM: master`, with the new
      commit/stamp and an updated selections table
- [x] `cmd/build/nativeworker_module.tar` regenerated
- [x] tar freshness, `./cmd/build/...`, dynclient regen-idempotence and `nativeserve-goget`
      all green

Precedent: #677 -> #678, #681 -> #682, #683 -> #684 and #686 -> #687 are the four prior
instances of this runbook being executed correctly; Slice 7.1b (#655) is what skipping it
costs — a branch pin went red on `nativeserve-goget` the moment the branch was deleted.

EVERY box above is now ticked. The serve core is pinned to a MASTER commit
(`062871154d95`), so it survives branch deletion by construction and an external consumer
resolves the same union parser this tree ships. This file reads `STATUS: RESOLVED`, which
is what `TestFirstPartyPinFollowupIsTracked`'s ANCESTRY clause requires now that the pinned
commit is master-reachable — the same guard that would have gone RED had the pins been
moved without this record moving with them.

The record stays in the tree rather than being deleted: the next slice that must pin to a
branch commit starts by flipping this file back to `OUTSTANDING` and re-running the runbook
above.
