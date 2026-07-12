// Package planassert holds the SHARED nanollm-plan client + assertions used by
// BOTH de-BAML Phase 5.1 differential legs (../dynamic and ../static), plus the
// literal auth/environment fence constants they agree on.
//
// It exists to de-duplicate the byte-for-byte-identical plan helpers that
// previously lived in each leg. Those helpers USE nanollm (they build a
// nanollm.Client and read a nanollm.PreparedRequest), so they deliberately do
// NOT live in the pure ../testutil package — testutil must stay import-free of
// BOTH nanollm AND every BAML runtime so its normalization/redaction/mutation
// unit tests run UNGATED. planassert imports nanollm (and the pure-Go
// internal/nativebody for its provider constant) but imports NO BAML runtime, so
// it never couples the two legs to a shared BAML binary.
//
// The nanollm-touching helpers live in the build-tagged asserts.go
// (`nanollm_prebuilt && nanollm_integration`); this file carries only the
// dependency-free fence constants and the package doc, so a default, tag-less
// `go build ./...` compiles this package with ZERO nanollm/CGO — preserving the
// root/default-build isolation.
package planassert

// The literal auth/environment fence (scope "Concrete auth/environment fence").
// Both legs share ONE source of truth: the dynamic leg's ClientRegistry and the
// static leg's reused StaticOracleClient fixture both carry these exact literals,
// and the paired nanollm ModelConfig restates them. The alias is deliberately
// distinct from the target so the model-separation proof is meaningful (the alias
// never appears in the JSON body). The `.invalid` base is non-routable: no leg
// ever dials it (the dynamic capture returns locally; the static request is
// pre-send).
const (
	FenceModel   = "fake-static-oracle-model"
	FenceBaseURL = "https://static-oracle.invalid/v1"
	FenceAPIKey  = "fake-static-oracle-key"
	FenceAlias   = "phase5-openai-alias"

	// WantURL is the exact prepared/captured endpoint (fake base + /v1 +
	// /chat/completions) both legs assert.
	WantURL = FenceBaseURL + "/chat/completions"
)
