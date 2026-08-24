# ADR: `projectdescriptor` ownership and versioning (D1)

**Status:** proposed (M0). **Decision needed by:** M1. **Recommended default
below is ready to build against.**

## Context

The native lane needs one neutral, whole-project input contract shared by the
source frontend (the `.baml` walk) and the native codegen backend. Today's
passive descriptors are **per-function sidecars**: `promptdescriptor.Function`
(one function's prompt + args + return bundle) and `schemadescriptor.Bundle`
(one method's ordered output type graph). Neither describes the whole project —
complete inputs, method class, stream carrier, all client/strategy relations, or
codegen requirements (scope §2 "Build or substantially refactor").

Two properties of the existing descriptors are the reason they are safe
generated-code boundaries and must be preserved by anything new:

- **They import no root/internal runtime.** `schemadescriptor` imports *nothing*
  (not even stdlib); `promptdescriptor` imports only `bamlutils/bamlparser` and
  `schemadescriptor` (`bamlutils/promptdescriptor/descriptor.go:36-39`). The
  lowering translator lives on the internal side and depends on the descriptor
  one-directionally (`internal/schema/static_descriptor.go`).
- **Order is data, never Go-map order.** Every plural field is an ordered slice;
  `schemadescriptor`'s doc calls order "load-bearing … BAML's render_output_format
  hoist order" (`descriptor.go:48-51`).

## Decision (recommended default — D1)

Create **one dependency-light, passive package** — working name
`bamlutils/projectdescriptor` — that **composes, never duplicates**,
`promptdescriptor.Function` and `schemadescriptor.Bundle`. It lives in the
`bamlutils` module (beside the two it composes), imports no root/internal
runtime, and keeps generator mechanics out of the data package (a reader/
validator is a separate consumer). It carries a **monotonic** top-level
`Version` and orders all data explicitly.

> **M0 ships the ADR only.** No `projectdescriptor` package is created in M0
> (that is M1). The manifest freezes the *planned* initial version
> (`descriptor_versions.project_descriptor_planned = 1`) and the composition
> rule; the shape below is the design M1 implements.

### Capability / decline code ownership (source of truth + validation path)

The `Capability`/`Decline` codes carried in `Project.Diagnostics` are **opaque,
stable strings** — `projectdescriptor` defines the *carrier* (a
`CapabilityCode string` newtype and the `Capability`/`Decline` records), never
the *value catalogue* and never any behavior keyed on a value. This keeps D1's
boundary intact: `projectdescriptor` imports neither `nativeserve/admission` nor
the internal feature packages, and it does not duplicate their taxonomy.

The value catalogue has one **checked-in source of truth for M0**:
`internal/codegenspine/manifest.json` (`capabilities`, `declines`). That file is
dependency-light (an embedded JSON + a passive Go mirror), so any consumer can
read it without pulling a runtime into its graph. Ownership by layer:

- **Producers of the codes at runtime** stay where they already live and remain
  the authoritative *emitters*: `nativeserve/admission` (`Stage`/`Reason`),
  `internal/nativeprompt`/`internal/nativebody` (`Feature` keys). M0 does not
  move them.
- **The frozen catalogue** (which codes exist, their `kind`/`proven_status`) is
  the manifest. `manifest_test.go` is the validation path: it grounds the
  descriptor versions, the endpoint semantics, the error taxonomy, and the
  capability fixtures against the live tree, so the catalogue cannot silently
  drift from the code that emits it.
- **M1's `projectdescriptor`** consumes codes as opaque strings and, where it
  needs to *validate* a code is in-catalogue, reads the manifest (or an M1-time
  Go mirror generated from it) — it never imports the emitting packages. If a
  future milestone wants a single Go constant set, generate it *from* the
  manifest into a dependency-light package (e.g. `bamlutils/projectdescriptor/
  capabilitycode`) rather than re-exporting `admission`'s symbols.

This is the answer to "who owns the stable string, and how is it validated":
**the manifest owns the catalogue; `manifest_test.go` validates it against live
code; `projectdescriptor` carries codes opaquely and reads the catalogue, never
the emitters.**

### Minimum shape (from scope §1.3)

```text
Project
  Version                int        // monotonic; fail-closed exact-match fence
  SourceUnits            []SourceUnit    // ordered stable source identities
  Types                  []TypeDef       // ordered; references schemadescriptor nodes
  Methods                []Method        // ordered
  Clients                []Client        // ordered
  RetryPolicies          []RetryPolicy   // ordered
  Strategies             []Strategy      // ordered
  Templates              []TemplateString
  Diagnostics            []Decline       // ordered capability/decline records

Method
  Name                   string          // canonical
  Class                  MethodClass      // e.g. static-unary / dynamic / stream-capable
  Args                   []Argument       // ordered; input type graph
  Return                 schemadescriptor.Bundle   // COMPOSED, not copied
  Stream                 *schemadescriptor.Bundle  // optional stream carrier
  Prompt                 promptdescriptor.Function reference/content   // COMPOSED
  DefaultClient          string
  RequiredCapabilities   []CapabilityCode // stream, media kinds, dynamic types, checks/asserts

Client
  Name, Provider         string
  Model                  promptdescriptor.ClientModel   // provenance: literal/env/dynamic
  Options                []promptdescriptor.ClientOption // ordered, typed
  Retry                  reference
  BodyAffecting vs Transport split           // reuse promptdescriptor.ClientConfig split

Strategy
  Kind                   fallback | round_robin
  Children               []string           // ordered
  StartSeed              (round-robin)
  Retry                  reference
  ValidationState

Capability / Decline
  Code                   CapabilityCode      // stable, machine-readable
  Detail                 string
```

`Return`/`Stream` are literally `schemadescriptor.Bundle` values and `Prompt`
composes `promptdescriptor.Function`; the project contract references those nodes
and lets each consumer apply its own supported profile (scope §1.3). The full
`schemadescriptor` type vocabulary — class, enum, map, tuple, arrow, union,
recursive alias, literal, media, optional/list, `StreamingBehavior{needed,
done,state}`, and `Constraint{level: check|assert}` — is reused verbatim; see
[03-public-compatibility-list.md](03-public-compatibility-list.md) §4.

### Versioning discipline

- **`Version` is a single integer, monotonic, and enforced by an exact-equality
  fail-closed fence** — the established pattern. `schemadescriptor.Version = 1`
  and `promptdescriptor.Version = 3` are each checked `!= Version → error` at
  every consumer (`internal/schema/static_descriptor.go:55`,
  `internal/nativeprompt/static_render.go:218-230`,
  `nativeserve/admission/static.go:640-642`, producing
  `descriptor_version_mismatch`). `projectdescriptor.Version` follows the same
  rule: a stale consumer must **reject**, never under-read.
- **One contract owner reviews every descriptor version change** (D11). A bump
  is coordinated across all consumers in lockstep — the same discipline
  `promptdescriptor`'s history documents (v1→v2 added `ClientConfig`, v2→v3 added
  `InputValues`; `promptdescriptor/descriptor.go:46-66`).
- **Composition means the composed versions travel inside `Project`.** A
  `Project` carries `schemadescriptor.Bundle` values that already stamp
  `Bundle.Version`; the project fence and the bundle fence are independent gates,
  both fail-closed. Do not flatten or re-stamp composed bundles.

### Secrets (scope §5 risk 12)

Prompt descriptors already can embed raw prompt bytes and literal client options
into generated source/binary (`cmd/introspect/main.go` prompt emission).
`projectdescriptor` inherits the same obligation: never log/metric-format
descriptor values, prefer `env` provenance (`ClientModel.Provenance ∈
{literal, env, dynamic}`, `promptdescriptor/descriptor.go:262-278`), and consider
redacted diagnostics / non-JSON in-memory forms for secret values.

## Consequences

- M1 can implement `ProjectDescriptor v1` with **only** the fields the first
  concrete slice needs (one static unary/text-only method class; scope §4),
  composing the two existing descriptors, and grow it additively.
- Because it imports no runtime, generated native code can depend on it without
  pulling BAML/CFFI into the graph — the property the rank-1 native-only proof
  requires.
- The lowering/validation logic stays on the consumer side (as
  `FromStaticDescriptor` does today), keeping the data package inert.

## Alternatives considered

- **Extend `promptdescriptor` in place.** Rejected: it is a per-function prompt
  mirror; widening it to whole-project inputs/clients/strategies would overload a
  stable contract and entangle prompt-versioning with project-versioning.
- **Emit generated Go structs as the contract (status quo of the generated
  lane).** Rejected: that is exactly the reflection-over-generated-values input
  model the native lane exists to replace (scope §1.1).
