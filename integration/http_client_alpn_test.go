//go:build integration

package integration

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bytedance/sonic"
	"github.com/invakid404/baml-rest/integration/mockllm"
	"github.com/invakid404/baml-rest/integration/testutil"
)

// TestHTTPClientALPNSelection is the real ALPN-selection coverage the
// http-client matrix axis was added for (#485). The shared mock serves an
// HTTPS/h2 lane on :8443 alongside the plaintext :8080 lane; this test points
// the BuildRequest path at the HTTPS lane and asserts which transport the
// worker's llmhttp backend actually negotiated, observed as the HTTP protocol
// version the mock received:
//
//   - auto    → ALPN probe negotiates h2 → net/http  → server sees HTTP/2.0
//   - nethttp → forced net/http (speaks h2)          → server sees HTTP/2.0
//   - fasthttp→ forced fasthttp (no ALPN, h1 only)   → server sees HTTP/1.1
//
// Against plaintext http:// the auto selector short-circuits to fasthttp, so
// auto and fasthttp were indistinguishable; over HTTPS auto genuinely takes
// the ALPN → h2 → net/http branch, de-duplicating auto vs fasthttp.
//
// Gated on the BuildRequest path because only it dispatches LLM calls through
// the Go llmhttp client (BAML_REST_HTTP_CLIENT is a no-op on the legacy
// CallStream path, which uses the BAML runtime's own HTTP stack and would not
// trust the mock's self-signed cert). The versioned matrix only runs the
// auto/fasthttp arms on BuildRequest cells anyway.
func TestHTTPClientALPNSelection(t *testing.T) {
	if !ActuallyBuildRequest() {
		t.Skip("http-client selection only applies to the BuildRequest path (llmhttp); skipping on legacy CallStream")
	}
	if TestEnv.MockLLMInternalTLS == "" {
		t.Skip("mock HTTPS/h2 lane not configured")
	}

	// The host BAML_REST_HTTP_CLIENT (CI http-client axis) is forwarded into
	// the container, so it also names the backend the worker resolved. Empty
	// or any non-"fasthttp" value resolves to auto/nethttp, both of which
	// speak h2.
	mode := strings.ToLower(strings.TrimSpace(os.Getenv(testutil.HTTPClientSelectorEnvVar)))
	wantProto := "HTTP/2.0"
	if mode == "fasthttp" {
		wantProto = "HTTP/1.1"
	}

	waitForHealthy(t, 30*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const scenarioID = "alpn-selection"
	scenario := &mockllm.Scenario{
		ID:             scenarioID,
		Provider:       "openai",
		Content:        "Hello, World!",
		ChunkSize:      0, // non-streaming: the fiber adaptor on the TLS lane buffers responses
		InitialDelayMs: 0,
	}
	if err := MockClient.RegisterScenario(ctx, scenario); err != nil {
		t.Fatalf("Failed to register scenario: %v", err)
	}

	resp, err := BAMLClient.Call(ctx, testutil.CallRequest{
		Method: "GetGreeting",
		Input:  map[string]any{"name": "World"},
		Options: &testutil.BAMLOptions{
			// Point the client at the HTTPS/h2 lane so the llmhttp backend
			// runs its real TLS/ALPN decision instead of the plaintext
			// fasthttp short-circuit.
			ClientRegistry: testutil.CreateTestClient(TestEnv.MockLLMInternalTLS, scenarioID),
		},
	})
	if err != nil {
		t.Fatalf("Call over HTTPS mock failed: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("Expected status 200, got %d: %s", resp.StatusCode, resp.Error)
	}
	var result string
	if err := sonic.Unmarshal(resp.Body, &result); err != nil {
		t.Fatalf("Failed to unmarshal response: %v", err)
	}
	if result != "Hello, World!" {
		t.Errorf("Expected 'Hello, World!', got %q", result)
	}

	// Confirm the request really went through the BuildRequest/llmhttp layer
	// (not legacy), otherwise the protocol assertion would not reflect the
	// http-client selection.
	testutil.AssertHeaderEquals(t, resp.Headers, testutil.HeaderBAMLPath, "buildrequest")

	gotProto, err := MockClient.GetLastProtocol(ctx, scenarioID)
	if err != nil {
		t.Fatalf("Failed to read recorded protocol from mock: %v", err)
	}
	if gotProto != wantProto {
		t.Fatalf("http-client=%q: mock observed %s, want %s (auto/nethttp negotiate h2 via ALPN → net/http; fasthttp uses http/1.1)",
			mode, gotProto, wantProto)
	}
}
