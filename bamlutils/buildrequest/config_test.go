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

// TestBuildLegacyMetadataPlanForClientWithConfig_UseBuildRequestFalse
// pins the load-bearing classification: a legacy-path plan built with
// UseBuildRequest=false MUST surface PathReasonBuildRequestDisabled
// regardless of process-level env. The plan-builder's tail-rule reads
// cfg.UseBuildRequest directly; if it ever reverts to the env-cached
// helper, the per-handler classification would silently follow the
// env instead of this handler's config.
func TestBuildLegacyMetadataPlanForClientWithConfig_UseBuildRequestFalse(t *testing.T) {
	t.Parallel()

	cfg := bamlutils.BuildRequestConfig{UseBuildRequest: false}
	// Supported provider on the legacy path with no other reason to
	// classify — the only thing left for PathReason is the
	// BuildRequest-disabled tail.
	plan := BuildLegacyMetadataPlanForClientWithConfig(
		nil,        // no runtime registry
		"MyClient", // effective client name
		"openai",   // introspected provider (call-supported)
		nil,        // no fallback chains
		nil,        // no introspected providers
		IsProviderSupported,
		nil, // no retry policy
		cfg,
	)
	if plan == nil {
		t.Fatal("expected plan")
	}
	if plan.PathReason != PathReasonBuildRequestDisabled {
		t.Errorf("expected PathReason=%q, got %q", PathReasonBuildRequestDisabled, plan.PathReason)
	}
}

// TestBuildLegacyMetadataPlanForClientWithConfig_UseBuildRequestTrue
// is the inverse pin: when the handler's config has UseBuildRequest
// set, the tail rule must leave PathReason empty for a legacy-path
// plan that has no other classification reason. A regression that
// inverted the conditional would surface here as a stray
// "buildrequest-disabled" reason on a perfectly valid handler.
func TestBuildLegacyMetadataPlanForClientWithConfig_UseBuildRequestTrue(t *testing.T) {
	t.Parallel()

	cfg := bamlutils.BuildRequestConfig{UseBuildRequest: true}
	plan := BuildLegacyMetadataPlanForClientWithConfig(
		nil, "MyClient", "openai", nil, nil, IsProviderSupported, nil, cfg,
	)
	if plan == nil {
		t.Fatal("expected plan")
	}
	if plan.PathReason != "" {
		t.Errorf("expected empty PathReason on UseBuildRequest=true with no other reason, got %q", plan.PathReason)
	}
}

// TestEnvConfigMatchesEnvHelpers proves the convenience wrapper
// preserves the same env contract every existing caller already
// depends on. A regression that diverged the two paths would silently
// drift server behaviour between the env-cached helper and the new
// per-instance plumbing.
func TestEnvConfigMatchesEnvHelpers(t *testing.T) {
	t.Parallel()

	got := EnvConfig()
	if got.UseBuildRequest != UseBuildRequest() {
		t.Errorf("EnvConfig.UseBuildRequest = %v, UseBuildRequest() = %v",
			got.UseBuildRequest, UseBuildRequest())
	}
	if got.DisableCallBuildRequest != disableCallBuildRequest() {
		t.Errorf("EnvConfig.DisableCallBuildRequest = %v, disableCallBuildRequest() = %v",
			got.DisableCallBuildRequest, disableCallBuildRequest())
	}
}
