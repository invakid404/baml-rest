package buildrequest

import (
	"testing"
)

// TestBuildLegacyMetadataPlanForClient_SupportedSingleProviderNoRetiredReason
// pins that the BuildRequest route gate is gone (#537): a legacy-path
// plan for a supported single provider with no other classification
// reason MUST leave PathReason empty and never synthesize the retired
// PathReasonBuildRequestDisabled. A regression that re-introduced the
// route gate would surface here as a stray "buildrequest-disabled"
// reason on a perfectly valid handler.
func TestBuildLegacyMetadataPlanForClient_SupportedSingleProviderNoRetiredReason(t *testing.T) {
	t.Parallel()

	plan := BuildLegacyMetadataPlanForClient(
		nil,        // no runtime registry
		"MyClient", // effective client name
		"openai",   // introspected provider (call-supported)
		nil,        // no fallback chains
		nil,        // no introspected providers
		IsProviderSupported,
		nil, // no retry policy
	)
	if plan == nil {
		t.Fatal("expected plan")
	}
	if plan.PathReason != "" {
		t.Errorf("expected empty PathReason for a supported single provider, got %q", plan.PathReason)
	}
	if plan.PathReason == PathReasonBuildRequestDisabled {
		t.Errorf("retired PathReasonBuildRequestDisabled must never be emitted")
	}
}
