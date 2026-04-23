//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/invakid404/baml-rest/integration/mockllm"
	"github.com/invakid404/baml-rest/integration/testutil"
)

// TestCallBridge_ForcesStreamRequest end-to-end-verifies the bridge that
// routes /call{,-with-raw} through StreamRequest + SSE accumulation when the
// non-streaming Request API is unavailable for the resolved provider.
//
// Production divergence between callSupportedProviders and supportedProviders
// doesn't exist today (both sets are identical), so the test forces the
// bridge by setting BAML_REST_DISABLE_CALL_BUILD_REQUEST=true in a dedicated
// container. Under that flag IsCallProviderSupported returns false for every
// provider — block 1 of the router declines, block 2 (stream modes only) is
// skipped, and the bridge block takes over for call modes.
//
// Checks:
//   - /call returns the parsed data
//   - /call-with-raw returns parsed data + non-empty raw
//   - X-BAML-Build-Request-API == "streamrequest"
//   - Outcome metadata fields (RetryCount, UpstreamDuration) are present
//
// The bridge only makes sense with BuildRequest on, so the test skips on the
// legacy leg where the flag would no-op.
func TestCallBridge_ForcesStreamRequest(t *testing.T) {
	if !ActuallyBuildRequest() {
		t.Skip("bridge requires BuildRequest path; skipping on legacy or pre-0.219 runtimes")
	}

	setupCtx, setupCancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer setupCancel()

	adapterVersion, err := testutil.GetAdapterVersionForBAML(BAMLVersion)
	if err != nil {
		t.Fatalf("Failed to get adapter version: %v", err)
	}
	bamlSrcPath, err := findTestdataPath()
	if err != nil {
		t.Fatalf("Failed to find testdata: %v", err)
	}

	env, err := testutil.Setup(setupCtx, testutil.SetupOptions{
		BAMLSrcPath:     bamlSrcPath,
		BAMLVersion:     BAMLVersion,
		AdapterVersion:  adapterVersion,
		BAMLSource:      BAMLSourcePath,
		UseBuildRequest: true,
		RuntimeEnv: map[string]string{
			// Forces IsCallProviderSupported to return false for every
			// provider, routing /call{,-with-raw} through the stream-
			// accumulation bridge.
			"BAML_REST_DISABLE_CALL_BUILD_REQUEST": "true",
		},
	})
	if err != nil {
		t.Fatalf("Failed to setup dedicated env: %v", err)
	}
	defer func() {
		termCtx, termCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer termCancel()
		if err := env.Terminate(termCtx); err != nil {
			t.Logf("dedicated env Terminate: %v", err)
		}
	}()

	mockClient := mockllm.NewClient(env.MockLLMURL)
	bamlClient := testutil.NewBAMLRestClient(env.BAMLRestURL)

	registerBridgeScenario := func(t *testing.T, scenarioID, content string) *testutil.BAMLOptions {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		// Bridge drives the upstream as streaming (stream: true) so mock
		// must respond with SSE chunks, not a single JSON body.
		scenario := &mockllm.Scenario{
			ID:             scenarioID,
			Provider:       "openai",
			Content:        content,
			ChunkSize:      20,
			InitialDelayMs: 10,
			ChunkDelayMs:   5,
		}
		if err := mockClient.RegisterScenario(ctx, scenario); err != nil {
			t.Fatalf("RegisterScenario: %v", err)
		}
		return &testutil.BAMLOptions{
			ClientRegistry: testutil.CreateTestClient(env.MockLLMInternal, scenarioID),
		}
	}

	t.Run("call_returns_data_via_streamrequest", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		opts := registerBridgeScenario(t, "test-bridge-call", "Hello, bridge!")

		resp, err := bamlClient.Call(ctx, testutil.CallRequest{
			Method:  "GetGreeting",
			Input:   map[string]any{"name": "Bridge"},
			Options: opts,
		})
		if err != nil {
			t.Fatalf("Call failed: %v", err)
		}
		if resp.StatusCode != 200 {
			t.Fatalf("expected 200, got %d: %s", resp.StatusCode, resp.Error)
		}

		var result string
		if err := json.Unmarshal(resp.Body, &result); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if result != "Hello, bridge!" {
			t.Errorf("got %q, want %q", result, "Hello, bridge!")
		}

		// Routing headers: Path stays "buildrequest" — the bridge is
		// still the BuildRequest orchestrator. BuildRequestAPI flips to
		// "streamrequest" so operators can distinguish a bridged /call
		// from a native Request API one.
		testutil.AssertHeaderEquals(t, resp.Headers, testutil.HeaderBAMLPath, "buildrequest")
		testutil.AssertHeaderEquals(t, resp.Headers, testutil.HeaderBAMLBuildRequestAPI, "streamrequest")
		testutil.AssertHeaderEquals(t, resp.Headers, testutil.HeaderBAMLClient, "TestClient")
		testutil.AssertHeaderEquals(t, resp.Headers, testutil.HeaderBAMLWinnerProvider, "openai")
		testutil.AssertHeaderEquals(t, resp.Headers, testutil.HeaderBAMLRetryCount, "0")
		testutil.AssertHeaderPresent(t, resp.Headers, testutil.HeaderBAMLUpstreamDuration)
	})

	t.Run("call_with_raw_returns_data_and_raw_via_streamrequest", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		content := `{"message": "bridged-raw"}`
		opts := registerBridgeScenario(t, "test-bridge-call-with-raw", content)

		resp, err := bamlClient.CallWithRaw(ctx, testutil.CallRequest{
			Method:  "GetSimple",
			Input:   map[string]any{"input": "test"},
			Options: opts,
		})
		if err != nil {
			t.Fatalf("CallWithRaw failed: %v", err)
		}
		if resp.StatusCode != 200 {
			t.Fatalf("expected 200, got %d: %s", resp.StatusCode, resp.Error)
		}

		// Parsed data is the SimpleOutput struct extracted from the SSE-
		// accumulated text.
		var data struct {
			Message string `json:"message"`
		}
		if err := json.Unmarshal(resp.Data, &data); err != nil {
			t.Fatalf("unmarshal data: %v", err)
		}
		if data.Message != "bridged-raw" {
			t.Errorf("data.message: got %q, want %q", data.Message, "bridged-raw")
		}

		// Raw reconstructed from SSE deltas must match the mock content
		// byte-for-byte — SSE accumulation preserves the full wire text.
		if resp.Raw != content {
			t.Errorf("raw: got %q, want %q", resp.Raw, content)
		}

		testutil.AssertHeaderEquals(t, resp.Headers, testutil.HeaderBAMLPath, "buildrequest")
		testutil.AssertHeaderEquals(t, resp.Headers, testutil.HeaderBAMLBuildRequestAPI, "streamrequest")
		testutil.AssertHeaderEquals(t, resp.Headers, testutil.HeaderBAMLClient, "TestClient")
		testutil.AssertHeaderEquals(t, resp.Headers, testutil.HeaderBAMLWinnerProvider, "openai")
		testutil.AssertHeaderEquals(t, resp.Headers, testutil.HeaderBAMLRetryCount, "0")
		testutil.AssertHeaderPresent(t, resp.Headers, testutil.HeaderBAMLUpstreamDuration)
	})

	// Fallback-chain /call under the disable flag: call-side
	// ResolveFallbackChain sees every child as legacy-under-call and
	// returns nil (all-legacy short circuit), so the call block declines;
	// the bridge block re-resolves with IsProviderSupported, accepts the
	// chain with all children as BuildRequest-driven, and the stream
	// orchestrator walks the strategy. Covers the new mixed-chain guard's
	// fall-through composition: call-side refuses → bridge accepts → chain
	// succeeds end-to-end.
	t.Run("fallback_chain_call_bridges_to_streamrequest", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		// Primary returns 500 immediately; secondary succeeds.
		if err := mockClient.RegisterScenario(ctx, &mockllm.Scenario{
			ID:          "fallback-primary",
			Provider:    "openai",
			Content:     "should not see this",
			FailAfter:   1,
			FailureMode: "500",
		}); err != nil {
			t.Fatalf("register primary: %v", err)
		}
		if err := mockClient.RegisterScenario(ctx, &mockllm.Scenario{
			ID:             "fallback-secondary",
			Provider:       "openai",
			Content:        "Hello from secondary!",
			ChunkSize:      20,
			InitialDelayMs: 10,
			ChunkDelayMs:   5,
		}); err != nil {
			t.Fatalf("register secondary: %v", err)
		}

		resp, err := bamlClient.Call(ctx, testutil.CallRequest{
			Method: "GetGreetingFallbackPair",
			Input:  map[string]any{"name": "Bridge"},
		})
		if err != nil {
			t.Fatalf("Call failed: %v", err)
		}
		if resp.StatusCode != 200 {
			t.Fatalf("expected 200, got %d: %s", resp.StatusCode, resp.Error)
		}

		var result string
		if err := json.Unmarshal(resp.Body, &result); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if result != "Hello from secondary!" {
			t.Errorf("got %q, want %q", result, "Hello from secondary!")
		}

		testutil.AssertHeaderEquals(t, resp.Headers, testutil.HeaderBAMLPath, "buildrequest")
		testutil.AssertHeaderEquals(t, resp.Headers, testutil.HeaderBAMLBuildRequestAPI, "streamrequest")
		testutil.AssertHeaderEquals(t, resp.Headers, testutil.HeaderBAMLClient, "TestFallbackPair")
		// Secondary won; surface it as winner-client to distinguish from
		// the strategy name in planned.Client.
		testutil.AssertHeaderEquals(t, resp.Headers, testutil.HeaderBAMLWinnerClient, "FallbackSecondary")
		testutil.AssertHeaderPresent(t, resp.Headers, testutil.HeaderBAMLUpstreamDuration)
	})
}
