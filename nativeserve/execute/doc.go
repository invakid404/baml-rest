// Package execute is the de-BAML native SEND engine: its PRODUCTION surface is
// the Slice-6b prepared-request executor in attempt.go, plus two gated,
// TEST-ONLY nanollm suites.
//
// PRODUCTION (attempt.go, UNTAGGED): [RunAttempt] / [ConsumeResponse] execute a
// fresh, Phase-5-admitted nanollm.PreparedRequest EXACTLY ONCE through baml-rest's
// own exact transport primitive (bamlutils/llmhttp ExactExecutor — ordered
// headers, raw bytes, one RoundTrip), then run
// nanollm.TranslateResponse(prep.Meta.ModelAlias, ...) -> OpenAI assistant-text
// extraction -> internal/debaml.Parse (native SAP). No Do/DoStream, no fallback,
// no same-model retry — exactly one attempt per call. This is the single native
// RoundTrip owner the serve implementation
// [github.com/invakid404/baml-rest/nativeserve/canary] claims and drives; it links
// nanollm (CGO), so — like the rest of this module — it is kept out of the host /
// default build graph at the MODULE level (nativeserve is out-of-go.work), NOT by
// a build tag. doc.go is a plain package comment (no longer load-bearing for a
// non-empty package, since attempt.go is an untagged source).
//
// TEST-ONLY, `//go:build nanollm_integration` (all *_test.go):
//
//   - Slice 2's FUNCTIONAL-SEND suite proves a baml-rest-native OpenAI chat body
//     (built once by internal/nativebody.BuildOpenAIChat) round-trips through
//     nanollm's REAL executor — Do/DoStream, across the CGO/Rust translation core
//     — to a provider-native HTTP endpoint and back to an OpenAI-shaped caller
//     result (send_integration_test.go, over the go-mocklm harness in
//     mocklm_integration_test.go).
//   - Slice 6b's PREPARED-REQUEST tests exercise attempt.go end to end: their
//     wire-fidelity oracle is a baml-rest-owned loopback capture server
//     (prepared_capture_test.go); prepared_mocklm_test.go reuses the same Slice-2
//     go-mocklm subprocess for a provider-native validity check; and
//     consume_response_test.go covers the response mapping.
//
// Invariants this package upholds (asserted concretely by the tagged tests):
//
//   - LOOPBACK-ONLY (tests): every Do/DoStream call runs through a supplied
//     http.Client whose transport has Proxy=nil and a dial guard that rejects any
//     non-loopback host, and every configured BaseURL is a literal 127.0.0.1 URL.
//     Credentials are literal fakes and process-env secret resolution is disabled
//     (nanollm.Config.UseProcessEnv=false). A typo'd BaseURL fails the dial guard
//     rather than escaping to a public provider; there are no real provider keys
//     and no public egress.
//
//   - NO BAML: this package pulls in NO BAML runtime. The request body is the
//     output of baml-rest's own native builder (nativebody), not a BAML CFFI
//     call, so the functional-send suite carries none of the `integration`-tag
//     CFFI dependency the sibling P5 differential legs need — it is
//     `nanollm_integration` only. (The production attempt.go likewise imports no
//     BAML runtime; internal/debaml.Parse is the native SAP, not a BAML call.)
//
// See the module go.mod and ../doc.go for why this lives in a separate,
// out-of-workspace module, and .github/workflows/nanollm-send.yml for the
// CI lane that runs the gated suites.
package execute
