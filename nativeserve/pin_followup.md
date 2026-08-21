# Out-of-`go.work` first-party pin follow-up

This file is the TRACKED record of whether the five first-party pseudo-version
selections below point at a **master** commit. **Right now they do.** The serving-cutover
S3a change first moved all five to its own BRANCH commit `9f4cfe14e878` on
`feat/debaml-s3a-identity`, because the symbols the packaged worker now links were
introduced by that very change and no master commit carried them yet; PR #681 then
squash-merged that change to master as `2f2e13c6dadb`, and the post-squash re-pin runbook
in the last section has been **performed** — all five now name the master squash commit.

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
PINNED-COMMIT: 2f2e13c6dadb
PINNED-STAMP: 20260821064630
REACHABLE-FROM: master
SLICE: de-BAML serving cutover S3a — trusted effective-config identity resolver at the native admission seam (default-deny)
PR: 681 (master squash-merge 2f2e13c6dadbb198f52b32e52c4c6de86514aeeb; re-pinned to the master squash commit post-merge)
```

## Why the pins were branch-only (now re-pinned to master)

`9f4cfe14e878` is the S3a source commit. It closes the production-wiring gap that would
otherwise make the FIRST enrollment an over-claim: before it, `NewServeFunc` built the
serve `Server` with a ZERO cohort identity and `serveCohortInput` returned that
process-level value, so an enrolled fingerprint would have been stamped onto **every**
request the worker hosts. After it, the dynamic unary serve seam RESOLVES a request's
configuration identity — and resolves it only for a configuration the DEPLOYMENT
configured, never one the caller described.

**This bump carries real cross-module behaviour, not just a version string.** S3a splits
across both sides of the graph:

- the ROOT side gains the TRUSTED-CONFIGURATION SEAL — `bamlutils/trustedconfig.go`
  (the wire-unreachable seal on `ClientProperty` plus the canonical selector digest) and
  `bamlutils/trustedclients` (the deployment's approved-configuration declaration, its
  strict end-of-input parsing, its bounded value-free rejection errors, and the sealing
  pass) — plus the worker config-load pass in `worker/options.go` that applies it, and the
  `workerboot` runtime seam the booted-artifact proof builds its fixture through. That is
  the ONLY thing that can mark a client as deployment-owned;
- the `nativeserve` side gains `admission.ResolveConfigIdentity`, which READS that seal
  and nothing else for identity, and rewires `canary`'s dynamic unary `serveCohortInput`
  onto it. The cohort gate additionally binds an identity to its inventory record's
  provider CLASS and declared surfaces rather than to the opaque bucket alone;
- the packaged side (`internal/nativebody/nanollmprepare`) is what RUNS that seam. Its
  `cmd/worker` entrypoint is also the binary the booted-artifact `/call` proof boots.

A consumer resolving the OLD pin gets a serve core with no seal to read, under a manifest
that claims one: its admission seam could not distinguish the deployment's approved
configuration from any caller-supplied configuration that resembled it. That is the
specific hazard S3a exists to remove, so the lockstep here is carrying behaviour, not a
string.

The pins could not point at master until this change was squash-merged: the master commit that
carries all of this IS that squash-merge (`2f2e13c6dadb`, #681), and it did not exist until
the merge happened. The branch was rebased onto master `5a1459ae291a` (#680), so
`9f4cfe14e878` was a descendant of the then-current master — but a descendant is not
master, and the squash flattened it out of history just the same, which is why re-pinning
to `2f2e13c6dadb` was the mandatory immediate follow-up. The Slice 7.1b (#655) incident is
the precedent for what happens if that re-pin is skipped — a branch pin went red on
`nativeserve-goget` once the branch was deleted — and #675 → #676, #677 → #678 and now #681
are the most recent instances of doing it correctly.

**What a green `nativeserve-goget` proves now.** With the selections on the master squash
commit `2f2e13c6dadb` it proves an external consumer can resolve, build and run
`nativeserve.New` against a MASTER commit — the module graph and every transitive
requirement are correct AND master-durable, so it keeps proving that after the
`feat/debaml-s3a-identity` branch is deleted. While the selections were on the branch
commit `9f4cfe14e878` a green probe proved only branch-resolvability, which is the whole
reason the re-pin runbook below was mandatory.

## The five pinned selections

Every one of them must move together. They are directory-`replace`d for local
development, so only the version STRINGS reach MVS — which is precisely why a partial
bump is invisible until the out-of-work packaging build fails with
`updates to go.mod needed`.

| # | file | module | current selection |
| --- | --- | --- | --- |
| 1 | `nativeserve/go.mod` | `github.com/invakid404/baml-rest` | `v0.0.0-20260821064630-2f2e13c6dadb` |
| 2 | `nativeserve/go.mod` | `github.com/invakid404/baml-rest/bamlutils` | `v0.0.49-0.20260821064630-2f2e13c6dadb` |
| 3 | `nativeserve/go.mod` | `github.com/invakid404/baml-rest/worker` | `v0.0.49-0.20260821064630-2f2e13c6dadb` |
| 4 | `internal/nativebody/nanollmprepare/go.mod` | `github.com/invakid404/baml-rest/bamlutils` | `v0.0.49-0.20260821064630-2f2e13c6dadb` |
| 5 | `internal/nativebody/nanollmprepare/go.mod` | `github.com/invakid404/baml-rest/worker` | `v0.0.49-0.20260821064630-2f2e13c6dadb` |

`internal/nativebody/nanollmprepare/go.mod`'s `github.com/invakid404/baml-rest v0.0.48`
is deliberately NOT in this list: it is a released tag, not a pseudo-version tracking a
commit, and the module directory-replaces it.

## What was done for THIS bump

1. Took the branch SHA of the S3a source commit and its committer timestamp in UTC —
   `9f4cfe14e878` / `20260820173806`. The stamp was not hand-trusted: `GOWORK=off
   GOPRIVATE=github.com/invakid404/baml-rest go mod download -json <mod>@9f4cfe14e878`
   was run for the root, `bamlutils` and `worker` off the origin, and Go's own
   stamp↔SHA computation returned exactly the pseudo-versions recorded above.
2. Moved **all five** selections above to that commit, each keeping its base version
   — `v0.0.0-<stamp>-<sha12>` for the root module, `v0.0.49-0.<stamp>-<sha12>` for
   `bamlutils` and `worker`.
3. Updated this file and BOTH `// PIN-STATUS` markers (`nativeserve/go.mod`,
   `internal/nativebody/nanollmprepare/go.mod`) to `OUTSTANDING`, so the machine-readable
   pin state and this record agree. (The five moved together on every revision of this
   change and never separately. The source SHA moved five times: twice because a cold
   review rewrote the change, once because the branch was REBASED onto master
   `5a1459ae291a` (#680), and twice for bounded bot-feedback cleanups (the second of
   which is the completed re-pin runbook below). A rebase and an
   edit both rewrite the source commit, so either re-opens this record — the pins name a
   commit, not a change.)
4. Regenerated the packaged worker source, which embeds both manifests:
   `go run ./cmd/build/gen-nativeworker-src`.
5. Re-ran the gates.

## The follow-up — PERFORMED (the post-squash re-pin RUNBOOK)

**This has been done for #681's squash-merge `2f2e13c6dadb`.** The ordered steps below are
the runbook that was executed, retained verbatim as the record of exactly what was carried
out and as the template for the next slice. Immediately after the change was squash-merged,
all of the following were done, in this order. A pre-squash pseudo-version is not the final
delivery state. Nothing here is optional and nothing here is inferable — an earlier
instance of this runbook left two steps implicit (flipping the mirrored manifest
NARRATIVES, and materializing the external probe's module and program), which is why they
are numbered steps below.

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
`NOTE (de-BAML serving cutover S3a): these pins are BRANCH-ONLY ...` in
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

- [x] all five selections name the master squash commit, each with its correct base version
- [x] both `// PIN-STATUS` markers say `RESOLVED`
- [x] both mirrored manifest narratives say master-durable, with the branch-only text
      demoted to `HISTORICAL, SUPERSEDED`
- [x] this file says `STATUS: RESOLVED`, `REACHABLE-FROM: master`, with the new
      commit/stamp and an updated selections table
- [x] `cmd/build/nativeworker_module.tar` regenerated
- [x] tar freshness, `./cmd/build/...`, dynclient regen-idempotence and `nativeserve-goget`
      all green

Precedent: #675 -> #676 and #677 -> #678 are the two most recent instances of this runbook
being executed correctly, and #681 (this one) is the latest; Slice 7.1b (#655) is what
skipping it costs — a branch pin went red on `nativeserve-goget` the moment the branch was
deleted.

Step 4 is now done: this record is the tracked statement that the released serve core IS
pinned to the durable master commit `2f2e13c6dadb`.
