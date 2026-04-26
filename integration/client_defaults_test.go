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
//     deployment default lets cache_control survive into the outbound body.
//     Skipped on the BuildRequest leg — see the skip message for rationale.
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
		if UseBuildRequest {
			t.Skip("cache_control survives BAML's allowed_role_metadata filter on " +
				"the classic runtime path but is dropped by the BuildRequest " +
				"serializer. Upstream TODOs: " +
				"baml_language/crates/sys_llm/src/build_request/openai.rs:100 and " +
				"baml_language/crates/sys_llm/src/build_request/anthropic.rs:91. " +
				"Remove this skip when those TODOs are resolved.")
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
			UseBuildRequest: UseBuildRequest,
			RuntimeEnv: map[string]string{
				"BAML_REST_CLIENT_DEFAULTS": `{"client_defaults":{"options":{"allowed_role_metadata":["cache_control"]}}}`,
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

		scenarioID := "test-client-defaults-positive"
		registerCacheScenario(t, mockClient, scenarioID)

		// Deliberately omit allowed_role_metadata from the caller's options;
		// the deployment default must inject it at the worker.
		registry := &testutil.ClientRegistry{
			Primary: "TestClient",
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
		if !strings.Contains(string(body), `"cache_control"`) {
			t.Fatalf("expected cache_control in outbound body, got:\n%s", body)
		}
	})

	t.Run("negative_no_default_drops_cache_control", func(t *testing.T) {
		// Shared TestEnv has no BAML_REST_CLIENT_DEFAULTS, so this reproduces
		// the pre-fix behavior: BAML defaults allowed_role_metadata to None
		// and silently strips cache_control before serialization.
		scenarioID := "test-client-defaults-negative"

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		registerCacheScenario(t, MockClient, scenarioID)

		registry := &testutil.ClientRegistry{
			Primary: "TestClient",
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
