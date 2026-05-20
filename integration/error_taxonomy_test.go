//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	stdjson "encoding/json"

	"github.com/bytedance/sonic"
	"github.com/invakid404/baml-rest/bamlutils"
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

// requireLegacyParseEnvelope skips the calling test when BAML's
// runtime loses successful-response parse failures behind a generic
// no-result fallback. On BAML v0.214,
// types/response.rs::result_with_constraints_content returns a
// terminal `This should never happen - Please report this error to
// our team with BAML_LOG=info enabled so we can improve this error
// message` anyhow!() string for the LLMResponse::Success-with-no-
// parsed-result case — the underlying `Parsing error: ...` envelope
// is already discarded, so there's nothing for the legacy classifier
// to anchor on. v0.218 and later surface the stable validation-error
// envelope. Tests that assert end-to-end on parse_error from a live
// LLM call therefore require >= 0.218.
//
// Provider-error tests (TestLegacyClassification_ProviderHTTPError)
// and the /parse-endpoint test (TestLegacyClassification_
// ParseEndpointGarbage) don't go through
// result_with_constraints_content — the former hits the LLMFailure
// branch which still surfaces ClientHttpError on 0.214, and /parse
// invokes BAML's parser directly on raw text rather than processing
// an LLMResponse. Both keep running across the whole BAML matrix.
func requireLegacyParseEnvelope(t *testing.T) {
	t.Helper()
	if !bamlutils.IsVersionAtLeast(BAMLVersion, "0.218.0") {
		t.Skipf("requires BAML >= 0.218 for the stable `Parsing error:` envelope; got %s", BAMLVersion)
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
	requireLegacyParseEnvelope(t)
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
// branch shares the classifier wiring AND (per #256) that the
// accumulated raw text flows through to ErrorDetails.raw so consumers
// don't need to regex-scrape BAML's "Parsing error: ..." free-form
// message.
func TestLegacyClassification_ParseErrorFromProseCallWithRaw(t *testing.T) {
	requireLegacyMode(t)
	requireLegacyParseEnvelope(t)
	forEachUnaryClient(t, func(t *testing.T, client *testutil.BAMLRestClient) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		const unparseable = "Apologies — I cannot answer that without more context."
		opts := setupNonStreamingScenario(t, "legacy-parse-error-prose-raw", unparseable)

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
		// #256: legacy CallStream+OnTick error envelopes must carry
		// details.raw with the accumulator's text at the time of
		// failure. Without this, consumers have to regex BAML's
		// free-form error string for "\nRaw Response: ..." to recover
		// the same diagnostic.
		if len(resp.ErrorDetails) == 0 {
			t.Fatalf("ErrorDetails missing — expected details.raw with the unparseable prose")
		}
		var details struct {
			Raw string `json:"raw"`
		}
		if err := sonic.Unmarshal(resp.ErrorDetails, &details); err != nil {
			t.Fatalf("failed to unmarshal ErrorDetails %s: %v", resp.ErrorDetails, err)
		}
		if details.Raw == "" {
			t.Fatalf("ErrorDetails.raw is empty; expected the unparseable prose (raw details=%s)",
				resp.ErrorDetails)
		}
		if !strings.Contains(details.Raw, unparseable) {
			t.Errorf("ErrorDetails.raw must contain the mock prose %q; got %q",
				unparseable, details.Raw)
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
		if err := sonic.Unmarshal(resp.ErrorDetails, &details); err != nil {
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

// requireBuildRequestMode skips the calling test when the shared
// TestEnv is NOT running BuildRequest. Inverse of requireLegacyMode —
// pairs the legacy classifier coverage above with BuildRequest-path
// coverage so the details.raw contract (#256) has end-to-end pinning
// on both orchestrators.
func requireBuildRequestMode(t *testing.T) {
	t.Helper()
	if !ActuallyBuildRequest() {
		t.Skip("BuildRequest classifier test; requires BAML_REST_USE_BUILD_REQUEST=true on BAML >= 0.219")
	}
}

// TestBuildRequestClassification_ParseErrorFromProseCallWithRaw is the
// BuildRequest analogue of TestLegacyClassification_ParseErrorFromProseCallWithRaw
// above. Per #256, accumulated raw is threaded through the BuildRequest
// non-streaming orchestrator's final-parse failure site so /call-with-raw
// error envelopes carry details.raw the same as the legacy path.
//
// Asserts the new contract: when the BuildRequest non-streaming path
// fails at final-parse on prose that doesn't fit the schema, the error
// envelope carries ErrorCode=parse_error AND details.raw with the
// unparseable prose verbatim — driven by the rawCarryingError wrap at
// the parseFinal failure site in bamlutils/buildrequest/call_orchestrator.go.
func TestBuildRequestClassification_ParseErrorFromProseCallWithRaw(t *testing.T) {
	requireBuildRequestMode(t)
	requireLegacyParseEnvelope(t)
	forEachUnaryClient(t, func(t *testing.T, client *testutil.BAMLRestClient) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		const unparseable = "I'm sorry — I cannot satisfy that request."
		opts := setupNonStreamingScenario(t, "buildrequest-parse-error-prose-raw", unparseable)

		resp, err := client.CallWithRaw(ctx, testutil.CallRequest{
			Method:  "GetPerson",
			Input:   map[string]any{"description": "Some person"},
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
		if len(resp.ErrorDetails) == 0 {
			t.Fatalf("ErrorDetails missing — expected details.raw with the unparseable prose")
		}
		var details struct {
			Raw string `json:"raw"`
		}
		if err := sonic.Unmarshal(resp.ErrorDetails, &details); err != nil {
			t.Fatalf("failed to unmarshal ErrorDetails %s: %v", resp.ErrorDetails, err)
		}
		if details.Raw == "" {
			t.Fatalf("ErrorDetails.raw is empty; expected the unparseable prose (raw details=%s)",
				resp.ErrorDetails)
		}
		if !strings.Contains(details.Raw, unparseable) {
			t.Errorf("ErrorDetails.raw must contain the mock prose %q; got %q",
				unparseable, details.Raw)
		}
	})
}

// TestBuildRequestClassification_ParseErrorFromProseStreamWithRaw pins
// the streaming analogue of the call-with-raw test above. The
// streaming BuildRequest orchestrator wraps its final-parse failure
// with newRawError(parseErr, rawAccumulated.String()) at the
// failure site so the worker bridge can attach details.raw to the
// terminal error event the SSE/NDJSON client observes.
//
// The error event payload is {"error":..., "code":..., "details":...}
// — same shape as the unary endpoint's body — so the assertion shape
// matches the call-with-raw test above except we read it from a
// stream event rather than an HTTP body.
func TestBuildRequestClassification_ParseErrorFromProseStreamWithRaw(t *testing.T) {
	requireBuildRequestMode(t)
	requireLegacyParseEnvelope(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const unparseable = "Sorry — your question is outside my training."
	opts := setupScenario(t, "buildrequest-parse-error-prose-stream-raw", unparseable)

	events, errs := BAMLClient.StreamWithRaw(ctx, testutil.CallRequest{
		Method:  "GetPerson",
		Input:   map[string]any{"description": "Anyone"},
		Options: opts,
	})

	// Collect events until the channel closes or an error arrives.
	var errEvent *testutil.StreamEvent
	for events != nil || errs != nil {
		select {
		case ev, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			if ev.IsError() {
				// Copy so the value survives the loop iteration —
				// IsError frames are terminal but the channel is
				// drained until close.
				e := ev
				errEvent = &e
			}
		case err, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			if err != nil {
				t.Fatalf("stream errored: %v", err)
			}
		case <-ctx.Done():
			t.Fatalf("ctx cancelled before terminal error: %v", ctx.Err())
		}
	}

	if errEvent == nil {
		t.Fatal("expected a terminal error event from the parse failure")
	}

	// The error frame's Data field is the JSON envelope
	// `{"error":..., "code":..., "details":...}` (see
	// SSEStreamWriterPublisher.PublishError / NDJSONEvent).
	var payload struct {
		Error   string          `json:"error"`
		Code    string          `json:"code"`
		Details stdjson.RawMessage `json:"details"`
	}
	if err := sonic.Unmarshal(errEvent.Data, &payload); err != nil {
		t.Fatalf("failed to unmarshal error event Data %s: %v", errEvent.Data, err)
	}
	if payload.Code != "parse_error" {
		t.Errorf("error event code: got %q, want parse_error; data=%s", payload.Code, errEvent.Data)
	}
	if len(payload.Details) == 0 {
		t.Fatalf("error event missing details.raw; data=%s", errEvent.Data)
	}
	var details struct {
		Raw string `json:"raw"`
	}
	if err := sonic.Unmarshal(payload.Details, &details); err != nil {
		t.Fatalf("failed to unmarshal error event details %s: %v", payload.Details, err)
	}
	if details.Raw == "" {
		t.Fatalf("error event details.raw is empty; details=%s", payload.Details)
	}
	if !strings.Contains(details.Raw, unparseable) {
		t.Errorf("details.raw must contain the mock prose %q; got %q", unparseable, details.Raw)
	}
}
