# Codegen spine — M0 contract freeze

This directory is the **M0 (contract freeze)** deliverable of the rank-1 native
codegen spine: a second, descriptor-driven native lane built beside the
generated-BAML lane (full scope: the codegen-spine scoping document — a planning
artifact tracked outside the repository and attached to PR #688; its decisions
are distilled, with `file:line` grounding, into the documents here). M0 ships
**no** descriptor package, **no** codegen, and **no** runtime change — those are
M1+. It freezes the contract the native lane must reproduce and records the
design decisions M1–M9 build against, grounded in the code that exists today.

M0 is a **P (pin/tar-independent) slice**: it touches none of
`internal/debaml/**`, `nativeserve/**`, or `internal/nativebody/nanollmprepare/**`,
bumps no version pin, and does not regenerate `cmd/build/nativeworker_module.tar`.
A source guard enforces this (see below).

## Documents

| Doc | Deliverable | Owner decision |
|---|---|---|
| [01-retained-endpoint-matrix.md](01-retained-endpoint-matrix.md) | Retained endpoint / method / type / client / option matrix | **D2** ⚑ |
| [02-descriptor-ownership-adr.md](02-descriptor-ownership-adr.md) | `projectdescriptor` ownership / versioning ADR | **D1** |
| [03-public-compatibility-list.md](03-public-compatibility-list.md) | Exact public Go/JSON + typed-error compatibility list | D2, D8 |
| [04-capability-decline-taxonomy.md](04-capability-decline-taxonomy.md) | Native capability / decline taxonomy | D3, D7 |
| [05-decisions-adr.md](05-decisions-adr.md) | Decisions ADR D1–D11 with recommended defaults | all |

⚑ = **architecturally significant, flagged for explicit owner sign-off.** The
two flagged decisions are **D2** (exactly what endpoint/public-API surface is
frozen as contract) and **D6** (oracle placement — recommend a separable
provider/subprocess so the native-only artifact can prove a zero-CFFI graph).
See [05-decisions-adr.md](05-decisions-adr.md) for both and for where owner
confirmation is wanted before M6+.

## The machine-checked half

The prose above is paired with a checked-in, machine-validated manifest so the
freeze cannot silently rot:

- `internal/codegenspine/manifest.json` — the frozen enumerable contract
  (retained endpoints, the 11-code error taxonomy, descriptor versions, the
  capability/decline taxonomy, and the grounding fixture set).
- `internal/codegenspine/manifest_test.go` — validates the manifest **against
  the live tree**: descriptor versions against `schemadescriptor.Version` /
  `promptdescriptor.Version`; the error code list (order-sensitive) against
  `internal/apierror.AllCodes()`; the envelope fields against the JSON tags of
  `apierror.Response` and `bamlutils.BamlOptions`; the four call/stream
  endpoints' raw/partial semantics against the live `bamlutils.StreamMode`
  predicates; and every representative `.baml` fixture is parsed with the
  production `bamlparser` and asserted to genuinely exhibit its capability
  category (static/dynamic, final/stream, strategies, media, checks, TypeBuilder).
- `internal/codegenspine/guard.json` + `guard_test.go` — the pin/tar-independence
  **source guard**: the native-worker tar is byte-identical, the five first-party
  pseudo-version pins are unmoved, and the three collision-path trees
  (`internal/debaml`, `nativeserve`, `internal/nativebody/nanollmprepare`) are
  byte-frozen. Regenerate the baseline (a later, intentional S slice only) with:

  ```sh
  go test ./internal/codegenspine/ -run TestSourceGuard -update-codegenspine-guard
  ```

M0 establishes the manifest **shape** and the **fixture set**. The scope's
eventual rule — *the build fails if a retained endpoint is missing from both the
native registry and an allowed transition fallback* — is wired in a later
milestone once the native registry exists (M4). The `proven_status` field on
each capability (`proven` / `transitional` / `declined`) is the seam that rule
will read.

## Grounding, not aspiration

Every claim in these documents cites `file:line` in the tree at base master
`d1f2526e2e7c`. Where a shape is a *plan* (the future `projectdescriptor`
package, the future native registry), it is labelled as such. The reused
contracts — `bamlutils/schemadescriptor`, `bamlutils/promptdescriptor`,
`internal/nativeschema`, `internal/apierror`, `worker.Runtime`,
`bamlutils.StreamingMethod`/`ParseMethod`, `nativeserve/admission` — are cited,
not reinvented.
