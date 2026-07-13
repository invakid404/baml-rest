// Package execute holds de-BAML Slice 2's gated, TEST-ONLY functional-send
// suite for nanollm. It proves that a baml-rest-native OpenAI chat body (built
// once by internal/nativebody.BuildOpenAIChat) round-trips through nanollm's
// REAL executor — Do/DoStream, across the CGO/Rust translation core — to a
// provider-native HTTP endpoint and back to an OpenAI-shaped caller result.
//
// The whole suite is build-tagged `//go:build nanollm_integration`; the proof
// lives in the tagged mocklm_integration_test.go (subprocess/admin/loopback
// harness) and send_integration_test.go (the Do parity + DoStream matrix).
// This file is deliberately UNTAGGED and IMPORT-FREE so that default, tagless
// tooling (`go build ./...` / `go vet ./...` / editors) sees a normal, entirely
// nanollm-free and go-mocklm-free package with exactly one non-test source file
// — never an empty package, and never a reason to resolve, fetch, or link the
// cgo nanollm-ffi archive or the go-mocklm tool.
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
