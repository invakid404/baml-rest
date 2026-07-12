// Package static holds the de-BAML Phase 5.1 STATIC OpenAI prepared-request
// differential: it proves nanollm's PreparedRequest matches the ACTUAL provider
// request stock BAML v0.223.0 builds (pre-send, via the generated
// Request.<Function>()) for every Phase-4a-admitted static-corpus row —
// test-only, no send, gated.
//
// The proof lives in the triply build-tagged
// prepared_request_integration_test.go
// (`integration && nanollm_prebuilt && nanollm_integration`); this file exists
// only so the package always has a non-test source file when the tags are off.
// Nothing in the default build imports it.
//
// It links the STOCK upstream github.com/boundaryml/baml@v0.223.0 runtime (via
// the checked-in generated fixture client) and must NEVER share a binary with the
// patched-BAML dynamic leg (../dynamic); keeping the two BAML runtimes in separate
// packages/test binaries is deliberate.
package static
