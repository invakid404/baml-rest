# Decisions ADR — D1–D11

Recorded for owner review at M0. Each decision carries the Codex **recommended
default** so work need not stop for open-ended design; the owner can override any
of them. Source: the codegen-spine scoping document §6 (a planning artifact
tracked outside the repository and attached to PR #688). **Two are flagged ⚑ as
architecturally significant and want explicit sign-off: D2 and D6.**

| ⚑ | Decision | Recommended default | Needed by | Status |
|:--:|---|---|---|---|
| ⚑ | **D2. Retained endpoint / public API matrix** | Freeze the five endpoints, options, output/error envelopes, and the Go names actually public; permit internal layout change. | M0/M3 | **awaiting sign-off** |
|  | **D1. Descriptor ownership / API** | New versioned passive package beside `promptdescriptor`/`schemadescriptor`; compose them; keep generator mechanics out of the data package. | M1 | recommended |
|  | **D3. Source compiler strictness** | Whole native build fails on invalid/unreadable source or a missing retained method; per-method capability declines only for transition builds, and only when recorded in the manifest. | M2 | recommended |
|  | **D4. TypeBuilder scope** | Implement ordered `DynamicTypes`; parse `BamlSnippets` with `bamlparser` if retained, else reject explicitly with a stable error — never silently ignore. | M5 | recommended |
|  | **D5. Adapter matrix** | One native descriptor/runtime ABI; keep 0.204/0.215/0.219 adapters solely for generated rollback/oracle; do not reproduce their BAML-version differences natively. | M2/M4 | recommended |
| ⚑ | **D6. Oracle placement** | Prefer a **separable provider/subprocess** so the native-only artifact can prove a zero-CFFI graph; production may pick in-process temporarily for cost, but registration/init must stay independent. | M4/M9 | **awaiting sign-off** |
|  | **D7. Rollout granularity** | Whole method + mode-capability before claim; no per-type or mid-attempt mixing. Typed pre-socket decline may fall back during transition; post-send failure never does. | M4/M6 | recommended |
|  | **D8. Parity authority** | Public HTTP/plugin JSON bytes and typed error metadata are authoritative; struct layout / generated source text need compatibility only where a downstream Go consumer is explicitly retained. | M3/M7 | recommended |
|  | **D9. Carrier package layout** | Generate `types`/`stream_types`-compatible packages initially to minimize consumer churn; **no** CFFI codec methods; rename only after rank-1 closure. | M3 | recommended |
|  | **D10. Build command boundary** | Grow `cmd/introspect` for M1 (it owns the proven native source walk); extract a dedicated `cmd/sourcegen` only after the descriptor is stable; native codegen consumes an artifact/API, never generated Go reflection. | M1/M2 | recommended |
|  | **D11. Owners** | Source/descriptor owner M0–M2; carrier/codegen owner M1 fixture + M3–M5; serving-bridge owner M6/M8; parser-family owner M7; build/release owner inert M9 in parallel; one contract owner reviews all descriptor version changes. | scheduling | recommended |

## ⚑ D2 — Retained endpoint / public API matrix (flagged)

**Why it is architecturally significant:** everything M3–M9 must reproduce is
defined by *what counts as contract here*. Freeze too little and the native lane
drifts observably; freeze too much (e.g. generated struct layout, the fiber-vs-chi
split, transition-diagnostic `X-BAML-Path*` headers) and internal refactors are
blocked forever.

**Recommended freeze** (full detail:
[01-retained-endpoint-matrix.md](01-retained-endpoint-matrix.md),
[03-public-compatibility-list.md](03-public-compatibility-list.md)):

- **In contract:** the five endpoint paths + `_dynamic` segment; per-method
  routing (method name = path segment); the request field names and
  `__baml_options__` field set; `CallWithRawResponse` fields; NDJSON/SSE frame
  `type`+field vocabulary; content-negotiation types; first-match client-registry
  semantics; the `apierror.Response` envelope and 11-code taxonomy with typed
  `provider_error` details; the `worker.Runtime` seam (frozen at 4 methods).
- **Not in contract:** generated input/output Go struct layout (only its JSON
  projection); which server implements a route; worker protocol/pool/adapter
  internals; `X-BAML-Path` / `X-BAML-Path-Reason` (transition observability only).

**Owner question:** confirm the "not in contract" list — especially that the
transition-diagnostic headers and the generated struct layout are explicitly
**not** frozen, and that the chi-server unary-only asymmetry is intended contract.

## ⚑ D6 — Oracle placement (flagged)

**Why it is architecturally significant:** the rank-1 spine proof is a
**native-only build/test profile in which the BAML oracle leg is absent** and the
graph carries no `baml-cli`, generated `baml_client`, BAML runtime,
`dynclient/baml-patched`, or CFFI symbol (scope §5 state D). If the oracle is
in-process, a production binary that legitimately links CFFI *for the oracle* can
mask an accidental native import of generated types or init globals — the exact
failure the proof exists to catch.

**Recommended default:** make the oracle a **separable provider**, ideally an
**isolated subprocess**, so:

- native registration/init never depends on oracle initialization;
- two graph assertions become possible (M9): the production-transition graph may
  reach CFFI **only** through the oracle provider; the native-only proof graph
  reaches none.

Production may choose in-process temporarily for cost/latency, but only if
registration/init independence is preserved. Decide this **before M4** (it shapes
the `CompositeRuntime` provider seam) without folding rank-6 oracle removal into
this scope.

**Owner question:** approve "separable provider, subprocess preferred" as the
target, and confirm that an in-process oracle, if used in production, must still
keep registration/init independent.

## Where owner confirmation is wanted before M6+

- **D6 (oracle topology)** before M4 — it fixes the `CompositeRuntime` seam.
- **D4 (`BamlSnippets`)** before M5 — parse-natively vs reject-explicitly is a
  product-telemetry call ("are raw snippets retained?"); the native lane must not
  silently ignore them (`bamlutils/interfaces.go:600`).
- **D2 (this matrix)** at M3 latest — the carrier generator freezes the JSON
  shapes it must round-trip against.
- **D7 (fallback granularity)** at M6 — "typed pre-socket decline may fall back,
  post-send never does" governs the request/transport bridge and must be locked
  before native transport starts.
- **D8 (retained Go consumers)** at M7 — whether any downstream Go consumer of
  the generated `types` packages is retained (beyond JSON parity) determines how
  much struct-layout compatibility M3/M7 owe.

## Open item — error-detail redaction (security / privacy)

Not a numbered decision, but flagged for the same owner track. `provider_error`
`details.body` (raw upstream body) and the `internal_error` `stacktrace` detail
are **unredacted by existing design** (developer-tool posture,
`worker/errors.go:78`). Freezing them as public contract would lock the exposure
in. **Recommended:** keep the developer-tool default but add a deployment flag to
redact/gate `body`/`stacktrace` for untrusted-facing deployments, with a
regression test asserting the gated mode exposes neither — **implemented in the
milestone that owns the error path (M6+ serving cutover), not in M0**, which
makes no runtime change. Settle before native serving cuts over. See
[03-public-compatibility-list.md](03-public-compatibility-list.md) §3.

None of D1–D11 needs to be resolved to *start* M1 except the D1 minimum
(descriptor ownership) — which is recommended and buildable as written.
