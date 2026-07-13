// Package execute holds two gated, TEST-ONLY de-BAML nanollm suites.
//
// Slice 2's FUNCTIONAL-SEND suite proves that a baml-rest-native OpenAI chat body
// (built once by internal/nativebody.BuildOpenAIChat) round-trips through
// nanollm's REAL executor — Do/DoStream, across the CGO/Rust translation core —
// to a provider-native HTTP endpoint and back to an OpenAI-shaped caller result
// (send_integration_test.go, over the go-mocklm harness in
// mocklm_integration_test.go).
//
// Slice 6b's PREPARED-REQUEST adapter (attempt.go + prepared_*_test.go) proves
// the OTHER, Option-B send path: a fresh, Phase-5-admitted nanollm.PreparedRequest
// executed ONCE through baml-rest's own exact transport primitive (bamlutils/
// llmhttp ExactExecutor — ordered headers, raw bytes, one RoundTrip), then
// nanollm.TranslateResponse(prep.Meta.ModelAlias, ...) -> OpenAI assistant-text
// extraction -> internal/debaml.Parse (native SAP). It uses NO Do/DoStream, no
// fallback, and no same-model retry — exactly one attempt per call. Its
// wire-fidelity oracle is a baml-rest-owned loopback capture server
// (prepared_capture_test.go); prepared_mocklm_test.go reuses the same Slice-2
// go-mocklm subprocess for a provider-native validity check.
//
// Every nanollm-touching file in the package — the adapter attempt.go and all
// the _test.go files — carries `//go:build nanollm_integration`. This file
// (doc.go) is the deliberate exception: it is the sole UNTAGGED, IMPORT-FREE
// source, so that default, tagless tooling (`go build ./...` / `go vet ./...` /
// editors) sees a normal, entirely nanollm-free and go-mocklm-free package with
// exactly one non-test source file — never an empty package, and never a reason
// to resolve, fetch, or link the cgo nanollm-ffi archive or the go-mocklm tool.
//
// Invariants this package upholds (asserted concretely by the tagged tests):
//
//   - TEST-ONLY: there is no production code here and nothing outside the gated
//     _test.go files imports nanollm or go-mocklm. No serving/worker/transport
//     package depends on this package.
//
//   - LOOPBACK-ONLY: every Do/DoStream call runs through a supplied http.Client
//     whose transport has Proxy=nil and a dial guard that rejects any non-
//     loopback host, and every configured BaseURL is a literal 127.0.0.1 URL.
//     Credentials are literal fakes and process-env secret resolution is
//     disabled (nanollm.Config.UseProcessEnv=false). A typo'd BaseURL fails the
//     dial guard rather than escaping to a public provider; there are no real
//     provider keys and no public egress.
//
//   - NO BAML: this package pulls in NO BAML runtime. The request body is the
//     output of baml-rest's own native builder (nativebody), not a BAML CFFI
//     call, so the suite carries none of the `integration`-tag CFFI dependency
//     the sibling P5 differential legs need — it is `nanollm_integration` only.
//
// See the module go.mod and ../doc.go for why this lives in a separate,
// out-of-workspace module, and .github/workflows/nanollm-send.yml for the
// advisory CI lane that runs it.
package execute
