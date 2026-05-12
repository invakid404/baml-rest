//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/invakid404/baml-rest/integration/mockllm"
	"github.com/invakid404/baml-rest/integration/testutil"
)

// These tests pin the legacy CallStream+OnTick path's conservative
// string-prefix classifier end-to-end. The classifier itself is a
// fall-through inside cmd/worker.classifyBAMLError that only fires
// when no typed BuildRequest surface matched — so on the
// BuildRequest-mode CI leg these tests would silently validate PR 2's
// typed branch instead of the new prefix arms. forcing legacy mode via
// BAML_REST_USE_BUILD_REQUEST=false (consumed by TestMain into the
// shared TestEnv) is the only way to actually exercise the new code.
//
// The unit tests in cmd/worker/error_classify_test.go cover the prefix
// matrix in isolation; these integration tests verify the wiring from
// BAML's FFI string through the worker bridge to the HTTP envelope's
// code / details fields.

// requireLegacyMode skips the calling test when the shared TestEnv is
// running BuildRequest. Mirrors the existing TestLegacyMode_* pattern
// in roundrobin_overrides_test.go.
func requireLegacyMode(t *testing.T) {
	t.Helper()
	if ActuallyBuildRequest() {
		t.Skip("legacy classifier test; requires BAML_REST_USE_BUILD_REQUEST=false")
	}
}

// TestLegacyClassification_ParseErrorFromProse pins that when the LLM
// returns prose with no parseable JSON envelope, the legacy path's
// "Parsing error: " prefix is classified as parse_error rather than
// the worker_error default. This is the prompt-not-doing-its-job case
// from the brief — the most common reason a /call request fails on a
// schema'd method.
func TestLegacyClassification_ParseErrorFromProse(t *testing.T) {
	requireLegacyMode(t)
	forEachUnaryClient(t, func(t *testing.T, client *testutil.BAMLRestClient) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		// Non-streaming, no failure mode — the upstream returns 200 with
		// prose content. BAML's ValidationError fires post-LLM when it
		// can't coerce the prose into the Person schema.
		opts := setupNonStreamingScenario(t, "legacy-parse-error-prose",
			"I think the person you're asking about is probably John, but I'm not totally sure about the details.")

		resp, err := client.Call(ctx, testutil.CallRequest{
			Method:  "GetPerson",
			Input:   map[string]any{"description": "John Doe, 30, developer"},
			Options: opts,
		})
		if err != nil {
			t.Fatalf("Call failed: %v", err)
		}

		if resp.StatusCode != 500 {
			t.Fatalf("expected status 500, got %d: %s", resp.StatusCode, resp.Error)
		}
		if resp.ErrorCode != "parse_error" {
			t.Errorf("ErrorCode: got %q, want parse_error (legacy classifier didn't engage); body=%s",
				resp.ErrorCode, resp.Error)
		}
		// Sanity check the underlying message survives so consumers
		// branching on Error text still get the BAML envelope.
		if !strings.Contains(resp.Error, "Parsing error:") {
			t.Errorf("Error message lost the 'Parsing error:' envelope: %q", resp.Error)
		}
	})
}

// TestLegacyClassification_ParseErrorFromProseCallWithRaw mirrors the
// /call case for /call-with-raw — same code path, but the response
// envelope is otherwise different on success. Pins that the error
// branch shares the classifier wiring.
func TestLegacyClassification_ParseErrorFromProseCallWithRaw(t *testing.T) {
	requireLegacyMode(t)
	forEachUnaryClient(t, func(t *testing.T, client *testutil.BAMLRestClient) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		opts := setupNonStreamingScenario(t, "legacy-parse-error-prose-raw",
			"Apologies — I cannot answer that without more context.")

		resp, err := client.CallWithRaw(ctx, testutil.CallRequest{
			Method:  "GetPerson",
			Input:   map[string]any{"description": "Jane Doe"},
			Options: opts,
		})
		if err != nil {
			t.Fatalf("CallWithRaw failed: %v", err)
		}

		if resp.StatusCode != 500 {
			t.Fatalf("expected status 500, got %d: %s", resp.StatusCode, resp.Error)
		}
		if resp.ErrorCode != "parse_error" {
			t.Errorf("ErrorCode: got %q, want parse_error; body=%s", resp.ErrorCode, resp.Error)
		}
	})
}

// TestLegacyClassification_ProviderHTTPError pins that an upstream
// non-2xx HTTP response becomes provider_error and that the
// status_code detail is parsed from BAML's
// `LLM client "X" failed with status code: NNN` envelope.
//
// Uses a single-leaf TestClient so there's no fallback retry — the
// provider error reaches the bridge directly. mockllm's
// FailAfter=1+FailureMode="500" returns HTTP 500 before any body, so
// BAML's ClientHttpError display path fires.
func TestLegacyClassification_ProviderHTTPError(t *testing.T) {
	requireLegacyMode(t)
	forEachUnaryClient(t, func(t *testing.T, client *testutil.BAMLRestClient) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		waitForHealthy(t, 30*time.Second)
		scenarioID := "legacy-provider-error-500"

		scenario := &mockllm.Scenario{
			ID:          scenarioID,
			Provider:    "openai",
			Content:     "should not see this",
			FailAfter:   1,
			FailureMode: "500",
		}
		registerCtx, registerCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer registerCancel()
		if err := MockClient.RegisterScenario(registerCtx, scenario); err != nil {
			t.Fatalf("RegisterScenario: %v", err)
		}

		opts := &testutil.BAMLOptions{
			ClientRegistry: testutil.CreateTestClient(TestEnv.MockLLMInternal, scenarioID),
		}

		resp, err := client.Call(ctx, testutil.CallRequest{
			Method:  "GetGreeting",
			Input:   map[string]any{"name": "World"},
			Options: opts,
		})
		if err != nil {
			t.Fatalf("Call failed: %v", err)
		}

		if resp.StatusCode != 500 {
			t.Fatalf("expected status 500 (host wraps provider failures); got %d: %s",
				resp.StatusCode, resp.Error)
		}
		if resp.ErrorCode != "provider_error" {
			t.Fatalf("ErrorCode: got %q, want provider_error (legacy classifier didn't match the LLM-client prefix); body=%s",
				resp.ErrorCode, resp.Error)
		}

		// Status code extraction is best-effort; pinning it here ensures
		// the prefix parser stays in sync with BAML's envelope when the
		// runtime version bumps.
		if len(resp.ErrorDetails) == 0 {
			t.Fatalf("ErrorDetails missing — expected {\"status_code\":500}")
		}
		var details struct {
			StatusCode int `json:"status_code"`
		}
		if err := json.Unmarshal(resp.ErrorDetails, &details); err != nil {
			t.Fatalf("failed to unmarshal ErrorDetails %s: %v", resp.ErrorDetails, err)
		}
		if details.StatusCode != 500 {
			t.Errorf("details.status_code: got %d, want 500 (raw details=%s)",
				details.StatusCode, resp.ErrorDetails)
		}
	})
}

// TestLegacyClassification_ParseEndpointGarbage pins that /parse on a
// garbage input also goes through the legacy classifier (via
// cmd/worker.workerImpl.Parse) and emits parse_error from the
// "Parsing error: " prefix. Without this, /parse-only consumers would
// silently rely on cmd/serve/error.go's worker_error→parse_error
// fallback rewrite for the same outcome — useful as back-compat, but
// the classifier wiring is the contract this PR adds.
func TestLegacyClassification_ParseEndpointGarbage(t *testing.T) {
	requireLegacyMode(t)
	forEachUnaryClient(t, func(t *testing.T, client *testutil.BAMLRestClient) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		// Garbage input — not even close to a Person envelope. BAML's
		// validator returns "Parsing error: Failed to parse LLM
		// response: ...".
		resp, err := client.Parse(ctx, testutil.ParseRequest{
			Method: "GetPerson",
			Raw:    "this is just prose with no JSON anywhere",
		})
		if err != nil {
			t.Fatalf("Parse failed: %v", err)
		}

		if resp.StatusCode != 500 {
			t.Fatalf("expected status 500, got %d: %s", resp.StatusCode, resp.Error)
		}
		if resp.ErrorCode != "parse_error" {
			t.Errorf("ErrorCode: got %q, want parse_error; body=%s", resp.ErrorCode, resp.Error)
		}
	})
}
