package buildrequest

import (
	"context"
	"strings"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
)

// TestRunCallOrchestrationUsesConfig pins that the per-call
// BuildRequestConfig threads through RunCallOrchestration's
// validation. With DisableCallBuildRequest=true on a known-supported
// provider, the orchestrator must reject the request via the
// IsCallProviderSupportedWithConfig branch instead of using the
// env-cached global. A regression that left IsCallProviderSupported
// in the validator would silently honour the env even when the
// caller passed an explicit config.
func TestRunCallOrchestrationUsesConfig(t *testing.T) {
	t.Parallel()

	out := make(chan bamlutils.StreamResult, 4)
	defer close(out)

	config := &CallConfig{
		Provider:           "openai",
		NeedsRaw:           false,
		BuildRequestConfig: bamlutils.BuildRequestConfig{DisableCallBuildRequest: true},
	}

	err := RunCallOrchestration(
		context.Background(), out, config, nil,
		nil, nil, nil, newTestResult,
	)
	if err == nil {
		t.Fatal("expected rejection when DisableCallBuildRequest=true")
	}
	if !strings.Contains(err.Error(), "unsupported or empty provider") {
		t.Fatalf("unexpected error message: %v", err)
	}
}
