# Out-of-`go.work` first-party pin follow-up

This file is the TRACKED record of whether the five first-party pseudo-version
selections below point at a **master** commit, and — while they do not — exactly what
has to be done about it.

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
PINNED-COMMIT: 4168895ed76d
PINNED-STAMP: 20260814142858
REACHABLE-FROM: feat/debaml-slice72c3-admission-cutover
SLICE: de-BAML 7.2c-3 — six-operator direct-comparison admission cutover
PR: 672
```

## Why the pins are branch-only right now

`nativeserve/admission`'s return-shape gate delegates to the root module's
`debaml.IsAdmittedStaticCheckedFamily` and spells no fingerprint of its own. Slice
7.2c-3 widened the predicate that function answers from `this > I` to the six direct
comparisons `this OP I`. **No symbol changed**, so a consumer resolving an older pin
still COMPILES — it simply keeps declining five of the six operators, i.e. ships a serve
core that silently under-claims against the root it was released with. The pin is what
carries the cutover to a released worker, which is why it had to move in the same change.

It cannot yet point at master: the master commit that will carry the widening is the
squash-merge of PR #672, and it does not exist until the merge happens. `4168895ed76d`
is the branch commit that carries the change; it is reachable only from
`feat/debaml-slice72c3-admission-cutover` and will be flattened out of history by the
squash. This is the exact failure mode `nativeserve/go.mod`'s BUMP RULE records from
Slice 7.1b (#655): the branch pin went red on `nativeserve-goget` once the branch was
deleted.

**What the current green `nativeserve-goget` proves, and what it does not.** It proves an
external consumer can resolve, build and run `nativeserve.New` against the branch tip
today — the module graph and every transitive requirement are correct. It does **not**
prove master-durable release, because branch reachability disappears with the branch.

## The five pinned selections

Every one of them must move together. They are directory-`replace`d for local
development, so only the version STRINGS reach MVS — which is precisely why a partial
bump is invisible until the out-of-work packaging build fails with
`updates to go.mod needed`.

| # | file | module | current selection |
| --- | --- | --- | --- |
| 1 | `nativeserve/go.mod` | `github.com/invakid404/baml-rest` | `v0.0.0-20260814142858-4168895ed76d` |
| 2 | `nativeserve/go.mod` | `github.com/invakid404/baml-rest/bamlutils` | `v0.0.49-0.20260814142858-4168895ed76d` |
| 3 | `nativeserve/go.mod` | `github.com/invakid404/baml-rest/worker` | `v0.0.49-0.20260814142858-4168895ed76d` |
| 4 | `internal/nativebody/nanollmprepare/go.mod` | `github.com/invakid404/baml-rest/bamlutils` | `v0.0.49-0.20260814142858-4168895ed76d` |
| 5 | `internal/nativebody/nanollmprepare/go.mod` | `github.com/invakid404/baml-rest/worker` | `v0.0.49-0.20260814142858-4168895ed76d` |

`internal/nativebody/nanollmprepare/go.mod`'s `github.com/invakid404/baml-rest v0.0.48`
is deliberately NOT in this list: it is a released tag, not a pseudo-version tracking a
commit, and the module directory-replaces it.

## The follow-up, to be performed IMMEDIATELY after the squash-merge

1. Take the master SHA of the squash-merge commit and its committer timestamp in UTC
   (`git log -1 --format='%H %cd' --date=format-local:%Y%m%d%H%M%S master`, with
   `TZ=UTC`).
2. Re-pin **all five** selections above to that commit, keeping each one's base version
   unchanged — `v0.0.0-<stamp>-<sha12>` for the root module, `v0.0.49-0.<stamp>-<sha12>`
   for `bamlutils` and `worker`.
3. Update this file: set `PINNED-COMMIT`/`PINNED-STAMP` to the new values, set
   `REACHABLE-FROM: master`, and set `STATUS: RESOLVED`.
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

Until step 3 is done, `STATUS` stays `OUTSTANDING` and this record is the tracked
statement that the released serve core is not yet pinned to a durable commit.
