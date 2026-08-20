# Out-of-`go.work` first-party pin follow-up

This file is the TRACKED record of whether the five first-party pseudo-version
selections below point at a **master** commit, and — while they do not — exactly what
has to be done about it. **Right now they do.** The serving-cutover S2 change moved all
five to its own branch commit; that change has since squash-merged as master
`c676a4dacc90` (#677), all five have been re-pinned to it, and the follow-up in the last
section has been **performed**.

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
PINNED-COMMIT: c676a4dacc90
PINNED-STAMP: 20260820091105
REACHABLE-FROM: master
SLICE: de-BAML serving cutover S2 — native-capable worker as the standard deployable artifact
PR: 677 (squash-merged as master c676a4dacc90; all five re-pinned to it post-merge)
```

## Why the pins were branch-only, and no longer are

`c676a4dacc90` is the master squash-merge of the S2 cutover change. It makes the
nanollmprepare-based native-capable worker the STANDARD deployable artifact, adds the
root-side artifact attestation the packaged worker's startup depends on, and — the part
that makes this a packaged-module change rather than a root-only one — repairs the
flag-off path of the isolated module's `cmd/worker-shadow` entrypoint.

**This bump carries real cross-module behaviour, not just a version string.** S2 splits
across both sides of the graph:

- the root side gains `internal/artifactprofile` and the `internal/workerboot`
  attestation: the running worker derives its artifact profile from its own linked
  capability, cross-checks it against the build's `-ldflags` stamp, re-derives the
  release artifact ID from the stamped inputs, and REFUSES TO SERVE on a contradiction;
- the packaged side (`internal/nativebody/nanollmprepare`) supplies what that
  attestation reads. `cmd/worker-shadow`'s flag-off branch used to pass a zero
  `workerboot.Options`, so a binary the build stamps `native_capable` derived
  `baml_only`, contradicted its own stamp and EXITED before serving any BAML —
  `BAML_REST_USE_DEBAML=false`, the one global kill switch, was an outage on that
  artifact. It now advertises its static build capability on the flag-off path, while
  still executing zero nanollm FFI.

So a consumer resolving the OLD pin gets a root with no artifact attestation at all
underneath a packaged worker written for one: the profile/ID signal that S2's
wrong-artifact alert reads would simply not exist, and the flag-off repair above would
have no counterpart to be correct against. Earlier slices' bumps were about behaviour
that would silently under-claim; this one is about a released worker whose kill switch
and whose release identity both live across the boundary.

Before the merge the pins could not point at master: the master commit carrying all of
this was the squash-merge of the change itself, and it did not exist until the merge
happened. The pre-merge selections named `e38f7effd633`, reachable only from
`feat/debaml-s2-standard-artifact` and flattened out of history by the squash — the exact
failure mode the BUMP RULE records from Slice 7.1b (#655), where a branch pin went red on
`nativeserve-goget` once the branch was deleted. This re-pin closes that window.

**What a green `nativeserve-goget` proves now.** With the selections on
`c676a4dacc90` it proves an external consumer can resolve, build and run
`nativeserve.New` against a MASTER commit — the module graph and every transitive
requirement are correct, and the reachability that backs them does not disappear with a
branch. That is the master-durable release the branch pin could not claim.

## The five pinned selections

Every one of them must move together. They are directory-`replace`d for local
development, so only the version STRINGS reach MVS — which is precisely why a partial
bump is invisible until the out-of-work packaging build fails with
`updates to go.mod needed`.

| # | file | module | current selection |
| --- | --- | --- | --- |
| 1 | `nativeserve/go.mod` | `github.com/invakid404/baml-rest` | `v0.0.0-20260820091105-c676a4dacc90` |
| 2 | `nativeserve/go.mod` | `github.com/invakid404/baml-rest/bamlutils` | `v0.0.49-0.20260820091105-c676a4dacc90` |
| 3 | `nativeserve/go.mod` | `github.com/invakid404/baml-rest/worker` | `v0.0.49-0.20260820091105-c676a4dacc90` |
| 4 | `internal/nativebody/nanollmprepare/go.mod` | `github.com/invakid404/baml-rest/bamlutils` | `v0.0.49-0.20260820091105-c676a4dacc90` |
| 5 | `internal/nativebody/nanollmprepare/go.mod` | `github.com/invakid404/baml-rest/worker` | `v0.0.49-0.20260820091105-c676a4dacc90` |

`internal/nativebody/nanollmprepare/go.mod`'s `github.com/invakid404/baml-rest v0.0.48`
is deliberately NOT in this list: it is a released tag, not a pseudo-version tracking a
commit, and the module directory-replaces it.

## The follow-up — PERFORMED

Every step below has been carried out against `c676a4dacc90`, the master squash-merge
commit of the S2 change, immediately after that merge. The #672 → #673 sequence was the
required precedent, and #675 → #676 the most recent instance of it; a branch-only
pseudo-version is not a durable release, and this change is what makes the S2 pins
durable.

1. Took the master SHA of the squash-merge commit and its committer timestamp in UTC —
   `c676a4dacc90` / `20260820091105`. The stamp was not hand-trusted: `GOWORK=off
   GOPRIVATE=github.com/invakid404/baml-rest go mod download -json <mod>@c676a4dacc90`
   was run for the root, `bamlutils` and `worker` off the origin, and Go's own
   stamp↔SHA computation returned exactly the pseudo-versions recorded above.
2. Re-pinned **all five** selections above to that commit, each keeping its base version
   — `v0.0.0-<stamp>-<sha12>` for the root module, `v0.0.49-0.<stamp>-<sha12>` for
   `bamlutils` and `worker`.
3. Updated this file: `PINNED-COMMIT`/`PINNED-STAMP` set to the new values, the record
   marked as reachable from `master` and resolved, and this section's heading and tense
   rewritten to record that it has been performed.
4. Regenerated the packaged worker source, which embeds both manifests:
   `go run ./cmd/build/gen-nativeworker-src`.
5. Re-ran the gates, all green:
   - `GOWORK=off go test -run TestNativeWorkerModuleTarIsFresh ./cmd/build/` — tar freshness;
   - `go test ./cmd/build/...` — including `TestFirstPartyPinFollowupIsTracked`, which is
     what enforces steps 2–3 against each other and against master-ancestry;
   - `go run ./cmd/regenerate-dynclient` followed by an EMPTY diff — regen idempotence;
   - the `nativeserve-goget` external-consumer probe against the **master** SHA:
     in an empty module, with no checkout / no `replace` / no workspace,
     `GOWORK=off GOFLAGS= GOPRIVATE=github.com/invakid404/baml-rest go get
     github.com/invakid404/baml-rest/nativeserve@c676a4dacc90` then `go build ./...` then
     `go run ./...`.

With step 3 done this record is the tracked statement that the released serve core IS
pinned to a durable master commit — one that carries the root-side artifact attestation,
the release-artifact-ID re-derivation, and the `cmd/worker-shadow` flag-off repair the
packaged worker ships. The next slice that moves these five re-opens this record.
