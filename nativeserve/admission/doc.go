// Package admission is the de-BAML production dynamic-client mapper plus the
// FULL pre/post-Prepare no-send native admission predicate for one unary OpenAI
// `_dynamic` call. It lives in the out-of-go.work, PUBLIC nativeserve serve-core
// module (#624) and is UNTAGGED production code: it links nanollm directly (via
// its own github.com/viktordanov/nanollm-ffi/go import) and compiles into BOTH the
// BAML+nanollm subprocess worker binary (built with GOWORK=off + CGO) AND an
// in-process dynclient consumer that imports nativeserve. It can never enter the
// host/root link graph (which stays zero-nanollm and CGO-free) because that graph
// never imports nativeserve.
//
// What it does, and deliberately does NOT do:
//
//   - [Admitter.Admit] evaluates the whole native serving predicate — build/flag/
//     route, the DEFAULT-DENY surface/cohort gate, whole-orchestration-plan,
//     effective dynamic client, prompt+canonical body, and the prepared plan
//     revalidated immediately after nanollm Prepare — and either returns an
//     [Admitted] plan proven up to (but NOT including) the exact-transport
//     RoundTrip, or a stable, secret-free [Decline] to BAML.
//   - The surface/cohort gate (cohort.go) is the serving-cutover S1 layer: every
//     lane evaluates it the moment layer 1 establishes the SURFACE and before any
//     native work, and the shipped policy ([ProductionCohortGate]) enrolls NOTHING —
//     so every request declines with (StageCohort, ReasonCohortNotEnrolled) before a
//     socket, and a native-capable artifact is externally equivalent to BAML
//     transport. It is admission EVIDENCE, not a second kill switch:
//     BAML_REST_USE_DEBAML (StageFlag, checked first) remains the complete revert.
//   - The gate's REQUEST-side half is [ResolveConfigIdentity] (identity.go), the
//     serving-cutover S3a layer: it binds a request's EFFECTIVE selected BAML
//     configuration to one bounded opaque fingerprint. The dynamic surface takes its
//     client_registry from the PUBLIC REQUEST BODY, so nothing derived from that
//     registry can establish which configuration a request is running — not even a
//     byte-exact match against the approved shape. Identity therefore comes from the
//     TRUSTED-CONFIGURATION SEAL (bamlutils/trustedconfig.go), which the worker's
//     config load applies to a client the DEPLOYMENT configured and which has no wire
//     representation to forge; the registry facts this resolver checks are narrowing
//     conditions on top of it, never a substitute for it. So it is never a
//     caller-supplied value and never a worker-wide "this process hosts configuration
//     X" assumption. It only NARROWS: a configuration that cannot be proven to be one
//     the deployment sealed gets no identity, and no identity is a cohort-stage
//     decline. With nothing declared (the shipped default) that is every request.
//   - It maps the effective one-client dynamic registry into a request-scoped
//     nanollm config with a SEPARATE internal alias and NO ambient/process env,
//     creates the engine, and safely Closes it before returning — the plan is
//     bytes, so the no-send path never needs the engine to outlive Admit.
//   - [Admitter.Admit] itself NEVER opens a socket, NEVER calls nanollm Do/DoStream,
//     and NEVER performs an exact-transport RoundTrip — it is the no-send predicate,
//     exercised by the gated admission/mapper tests and the shadow comparator.
//     [Admitter.AdmitClaim] wraps it for the SERVE path: it runs the same predicate
//     and, on a full match, CLAIMS a native attempt (keeping the request-scoped
//     engine alive) so the serve implementation ([github.com/invakid404/baml-rest/nativeserve/canary])
//     can perform exactly one RoundTrip. So whether admission leads to native serving
//     is decided by its CALLER: with the umbrella flag OFF (or no serve/shadow
//     callback installed) the generated seam leaves the native child-attempt callback
//     nil, and a native-capable worker — or a dynclient consumer — serves 100% BAML;
//     with the flag ON and the serve callback installed (workerboot.NativeServeFactory
//     or dynclient.WithNativeServeComparator, both via nativeserve.New) an admitted
//     unary `_dynamic` call is served natively.
//
// Parity-decline is the load-bearing invariant: when ANY layer cannot be proven,
// admission declines to BAML instead of admitting/claiming. A decline is an
// observable routing decision recorded on the private registry ([Metrics]) with a
// fixed-enum stage + reason — never a user-visible error while BAML is available,
// and never a free-form or secret-bearing label.
package admission
