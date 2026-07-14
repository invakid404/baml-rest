// Package admission is the de-BAML production dynamic-client mapper plus the
// FULL pre/post-Prepare no-send native admission predicate for one unary OpenAI
// `_dynamic` call. It lives in the out-of-go.work nanollmprepare worker module
// and is UNTAGGED production code: it compiles into the BAML+nanollm worker
// binary (built with GOWORK=off + CGO), links nanollm through the sibling
// nativeworker import, and can never enter the host/root link graph (which stays
// zero-nanollm and CGO-free).
//
// What it does, and deliberately does NOT do:
//
//   - [Admitter.Admit] evaluates the whole native serving predicate — build/flag/
//     route, whole-orchestration-plan, effective dynamic client, prompt+canonical
//     body, and the prepared plan revalidated immediately after nanollm Prepare —
//     and either returns an [Admitted] plan proven up to (but NOT including) the
//     exact-transport RoundTrip, or a stable, secret-free [Decline] to BAML.
//   - It maps the effective one-client dynamic registry into a request-scoped
//     nanollm config with a SEPARATE internal alias and NO ambient/process env,
//     creates the engine, and safely Closes it before returning — the plan is
//     bytes, so the no-send path never needs the engine to outlive Admit.
//   - It NEVER opens a socket, NEVER calls nanollm Do/DoStream, and NEVER performs
//     an exact-transport RoundTrip. It changes NO serving behaviour: nothing in
//     the generated adapter, handler, or orchestrator calls it — the orchestrator's
//     native child-attempt callback stays nil/hard-off, so a native-capable worker
//     still serves 100% BAML. Admit is exercised only by the gated admission/mapper
//     tests.
//
// Parity-decline is the load-bearing invariant: when ANY layer cannot be proven,
// Admit declines to BAML instead of admitting. A decline is an observable routing
// decision recorded on the worker registry ([Metrics]) with a fixed-enum stage +
// reason — never a user-visible error while BAML is available, and never a
// free-form or secret-bearing label.
package admission
