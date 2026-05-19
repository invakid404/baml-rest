//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/integration/mockllm"
	"github.com/invakid404/baml-rest/integration/testutil"
)

// TestClientDefaults_AllowedRoleMetadata exercises the
// BAML_REST_CLIENT_DEFAULTS feature end-to-end. Two sub-tests:
//
//  1. positive: a dedicated container with the env var set. The caller's
//     ClientRegistry deliberately omits allowed_role_metadata; we verify the
//     deployment default lets cache_control survive into the outbound body on
//     both runtime paths (classic CallStream/OnTick and BuildRequest). The
//     recursive helper validates the captured wire body so alias-leak shapes
//     such as cache_control.cache_type do not pass.
//
//  2. negative: reuses the shared TestEnv (no BAML_REST_CLIENT_DEFAULTS).
//     Same request shape; asserts cache_control is dropped. Reproduces the
//     bug this feature is fixing.
func TestClientDefaults_AllowedRoleMetadata(t *testing.T) {
	// Dynamic endpoints are the carrier for message-level metadata.
	if !bamlutils.IsVersionAtLeast(BAMLVersion, "0.215.0") {
		t.Skip("Skipping: dynamic endpoints require BAML >= 0.215.0")
	}

	t.Run("positive_default_preserves_cache_control", func(t *testing.T) {
		// Older BAML BuildRequest serializers dropped message-level
		// metadata for some providers. BAML v0.222 preserves
		// cache_control for the dynamic template shape used here after
		// the class-free literal alias-bypass landed for #304, so this
		// leg runs against both runtime paths. If a future supported
		// BAML version regresses, narrow the skip to that version
		// rather than reinstating a blanket UseBuildRequest skip — the
		// blanket form would mask the openai-generic BuildRequest path
		// the user originally reported on.

		setupCtx, setupCancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer setupCancel()

		opts := matrixSetupOptions()
		opts.RuntimeEnv = map[string]string{
			"BAML_REST_CLIENT_DEFAULTS": `{"client_defaults":{"options":{"allowed_role_metadata":["cache_control"]}}}`,
		}
		env, err := testutil.Setup(setupCtx, opts)
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

		scenarioID := "test-client-defaults-positive"
		registerCacheScenario(t, mockClient, scenarioID)

		// Deliberately omit allowed_role_metadata from the caller's options;
		// the deployment default must inject it at the worker.
		registry := &testutil.ClientRegistry{
			Primary: testutil.StringPtr("TestClient"),
			Clients: []*testutil.ClientProperty{
				{
					Name:     "TestClient",
					Provider: testutil.StringPtr("openai-generic"),
					Options: map[string]any{
						"model":    scenarioID,
						"base_url": env.MockLLMInternal,
						"api_key":  "test-key",
					},
				},
			},
		}

		callCtx, callCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer callCancel()

		resp, err := bamlClient.DynamicCall(callCtx, testutil.DynamicRequest{
			Messages: []testutil.DynamicMessage{
				{
					Role:    "user",
					Content: "Does cache_control survive?",
					Metadata: &testutil.DynamicMessageMetadata{
						CacheControl: &testutil.DynamicCacheControl{Type: "ephemeral"},
					},
				},
			},
			ClientRegistry: registry,
			OutputSchema: &testutil.DynamicOutputSchema{
				Properties: map[string]*testutil.DynamicProperty{
					"answer": {Type: "string"},
				},
			},
		})
		if err != nil {
			t.Fatalf("DynamicCall failed: %v", err)
		}
		if resp.StatusCode != 200 {
			t.Fatalf("expected 200, got %d: %s", resp.StatusCode, resp.Error)
		}

		inspectCtx, inspectCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer inspectCancel()
		body, err := mockClient.GetLastRequest(inspectCtx, scenarioID)
		if err != nil {
			t.Fatalf("GetLastRequest failed: %v", err)
		}
		// Recursive validation: exactly one well-formed
		// cache_control:{type:ephemeral} on the outbound body. The
		// previous strings.Contains gate accepted the
		// `cache_control:{"cache_type":"ephemeral"}` shape from the
		// upstream BAML jinja-runtime alias bug — the recursive helper
		// catches that and other leak variants.
		assertExactlyOneEphemeralCacheControl(t, body)
	})

	t.Run("negative_no_default_drops_cache_control", func(t *testing.T) {
		// Shared TestEnv has no BAML_REST_CLIENT_DEFAULTS, so this
		// reproduces BAML's default behavior: allowed_role_metadata
		// defaults to None and cache_control is silently stripped
		// before serialization.
		scenarioID := "test-client-defaults-negative"

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		registerCacheScenario(t, MockClient, scenarioID)

		registry := &testutil.ClientRegistry{
			Primary: testutil.StringPtr("TestClient"),
			Clients: []*testutil.ClientProperty{
				{
					Name:     "TestClient",
					Provider: testutil.StringPtr("openai-generic"),
					Options: map[string]any{
						"model":    scenarioID,
						"base_url": TestEnv.MockLLMInternal,
						"api_key":  "test-key",
					},
				},
			},
		}

		resp, err := BAMLClient.DynamicCall(ctx, testutil.DynamicRequest{
			Messages: []testutil.DynamicMessage{
				{
					Role:    "user",
					Content: "Does cache_control survive?",
					Metadata: &testutil.DynamicMessageMetadata{
						CacheControl: &testutil.DynamicCacheControl{Type: "ephemeral"},
					},
				},
			},
			ClientRegistry: registry,
			OutputSchema: &testutil.DynamicOutputSchema{
				Properties: map[string]*testutil.DynamicProperty{
					"answer": {Type: "string"},
				},
			},
		})
		if err != nil {
			t.Fatalf("DynamicCall failed: %v", err)
		}
		if resp.StatusCode != 200 {
			t.Fatalf("expected 200, got %d: %s", resp.StatusCode, resp.Error)
		}

		inspectCtx, inspectCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer inspectCancel()
		body, err := MockClient.GetLastRequest(inspectCtx, scenarioID)
		if err != nil {
			t.Fatalf("GetLastRequest failed: %v", err)
		}
		if strings.Contains(string(body), `"cache_control"`) {
			t.Fatalf("expected cache_control absent from outbound body, got:\n%s", body)
		}
	})
}

// registerCacheScenario registers a minimal openai-shaped scenario whose
// response body matches the dynamic output schema used by the test.
func registerCacheScenario(t *testing.T, client *mockllm.Client, scenarioID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := client.RegisterScenario(ctx, &mockllm.Scenario{
		ID:        scenarioID,
		Provider:  "openai",
		Content:   `{"answer": "yes"}`,
		ChunkSize: 0,
	})
	if err != nil {
		t.Fatalf("RegisterScenario: %v", err)
	}
}
