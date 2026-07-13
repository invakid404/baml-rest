// Package dynamic holds the de-BAML Phase 5.1 DYNAMIC OpenAI prepared-request
// differential: it proves nanollm's PreparedRequest matches the ACTUAL provider
// request BAML v0.223 builds through baml-rest's PATCHED runtime (dynclient), for
// every Phase-4a-admitted dynamic fixture — test-only, no send, gated.
//
// The proof lives in the doubly build-tagged
// prepared_request_integration_test.go
// (`integration && nanollm_integration`); this file exists only so the package
// always has a non-test source file when the tags are off (so `go build ./...`
// never sees an empty package). Nothing in the default build imports it — the
// whole nanollmprepare module is outside go.work and carries the only nanollm
// dependency (the public, cgo nanollm-ffi module).
//
// It links baml-rest's patched BAML runtime (via dynclient/baml-patched) and must
// NEVER share a binary with the stock-BAML static leg (../static); keeping the two
// BAML runtimes in separate packages/test binaries is deliberate.
package dynamic
