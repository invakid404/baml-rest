package buildrequest

import (
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
)

// TestIsCallProviderSupportedWithConfig_DisableIsPerCall pins that two
// distinct configs route the same provider differently: the config
// with DisableCallBuildRequest set declines every provider, while a
// fresh config with the flag clear keeps accepting supported providers.
// Without the per-config split the env-cached helper would force both
// callers onto the same answer.
func TestIsCallProviderSupportedWithConfig_DisableIsPerCall(t *testing.T) {
	t.Parallel()

	disabled := bamlutils.BuildRequestConfig{DisableCallBuildRequest: true}
	enabled := bamlutils.BuildRequestConfig{DisableCallBuildRequest: false}

	// "openai" lives in callSupportedProviders — under the enabled
	// config it must be accepted; under the disabled config it must
	// be rejected. A regression that read from the env cache would
	// produce the same answer for both rows.
	if IsCallProviderSupportedWithConfig("openai", disabled) {
		t.Errorf("expected openai to be rejected when DisableCallBuildRequest=true")
	}
	if !IsCallProviderSupportedWithConfig("openai", enabled) {
		t.Errorf("expected openai to be accepted when DisableCallBuildRequest=false")
	}
	// Unknown providers stay unsupported on both configs — pinning
	// that DisableCallBuildRequest is the only knob the helper reads
	// from the config.
	if IsCallProviderSupportedWithConfig("not-a-provider", enabled) {
		t.Errorf("expected unknown provider to remain unsupported on enabled config")
	}
}

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

// TestEnvConfig_OnlyDisableCallBuildRequestIsEnvDriven proves EnvConfig
// reads only the surviving env-driven field. After #537 the
// BuildRequest route gate is retired, so EnvConfig carries just
// DisableCallBuildRequest, sourced from the same env-cached helper every
// caller depends on.
func TestEnvConfig_OnlyDisableCallBuildRequestIsEnvDriven(t *testing.T) {
	t.Parallel()

	got := EnvConfig()
	if got.DisableCallBuildRequest != disableCallBuildRequest() {
		t.Errorf("EnvConfig.DisableCallBuildRequest = %v, disableCallBuildRequest() = %v",
			got.DisableCallBuildRequest, disableCallBuildRequest())
	}
}
