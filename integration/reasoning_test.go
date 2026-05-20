//go:build integration

package integration

// Integration tests for the IncludeReasoning opt-in covering the
// /call-with-raw and /stream-with-raw endpoints against the mock Anthropic
// provider. These exercise the full plumbing chain — JSON →
// BamlOptions.IncludeReasoning → Adapter.SetIncludeReasoning →
// StreamConfig/CallConfig → extractors → HTTP envelope — that the unit
// tests below the orchestrator boundary can't cover end-to-end.
//
// Each public scenario is exercised in two flag states:
//
//   - "default" (omitted from the request body) — expected: reasoning
//     channel empty; raw text-only.
//   - "opt_in" (true) — expected: reasoning channel carries the provider's
//     thinking text; raw stays text-only by construction.
//
// Note on a third state: BamlOptions.IncludeReasoning is a plain `bool`
// with `json:"include_reasoning,omitempty"`, so an explicit `false`
// serializes identically to "field omitted". On the worker side a missing
// field decodes to the same zero value, so "explicit false" and "default"
// are not integration-observable as distinct cases.
//
// A dedicated parseable-invariant test runs the same input through both
// flag values and asserts the parsed Data is byte-identical. This codifies
// the structural guarantee that flipping the flag can never leak reasoning
// into the parser; the gate is on the reasoning channel writes only.

import (
	"context"
	"strings"
	"testing"
	"time"

	stdjson "encoding/json"

	"github.com/bytedance/sonic"
	"github.com/invakid404/baml-rest/integration/mockllm"
	"github.com/invakid404/baml-rest/integration/testutil"
)

const (
	// reasoningTestContent is the text content the mock returns. Kept as a
	// well-formed JSON object so GetSimple can parse it; the parser only
	// sees this content (never the thinking) regardless of the flag value.
	reasoningTestContent = `{"message": "hello"}`

	// reasoningTestThinking is the thinking-block content the mock emits
	// before the text content. Deliberately chosen to look JSON-ish so a
	// regression that fed it into the parser would corrupt resp.Data with
	// `"answer"` instead of `"hello"` — making any leak loud and trivial
	// to detect.
	reasoningTestThinking = `Let me think about this. The answer should be: {"message": "answer"}`
)

// setupAnthropicReasoningScenario registers an Anthropic-provider scenario
// with both text content and thinking content, returning BAMLOptions ready
// for use against /call-with-raw or /stream-with-raw.
//
// includeReasoning maps to BamlOptions.IncludeReasoning verbatim; callers
// pass *bool so a nil value omits the field from the JSON entirely
// (matching the "default" flag-absent shape).
func setupAnthropicReasoningScenario(t *testing.T, scenarioID, content, thinking string, streaming bool, includeReasoning *bool) *testutil.BAMLOptions {
	t.Helper()

	waitForHealthy(t, 30*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	scenario := &mockllm.Scenario{
		ID:             scenarioID,
		Provider:       "anthropic",
		Content:        content,
		Thinking:       thinking,
		InitialDelayMs: 50,
	}
	if streaming {
		scenario.ChunkSize = 8
		scenario.ChunkDelayMs = 5
	}

	if err := MockClient.RegisterScenario(ctx, scenario); err != nil {
		t.Fatalf("Failed to register scenario: %v", err)
	}

	opts := &testutil.BAMLOptions{
		ClientRegistry: testutil.CreateAnthropicTestClient(TestEnv.MockLLMInternal, scenarioID),
	}
	if includeReasoning != nil {
		opts.IncludeReasoning = *includeReasoning
	}
	return opts
}

func boolPtr(v bool) *bool { return &v }

func assertParsedMessage(t *testing.T, data stdjson.RawMessage, want string) {
	t.Helper()
	var parsed struct {
		Message string `json:"message"`
	}
	if err := sonic.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("Failed to unmarshal parsed data: %v (raw bytes: %s)", err, string(data))
	}
	if parsed.Message != want {
		t.Errorf("Parsed message: got %q, want %q (full data: %s)", parsed.Message, want, string(data))
	}
}

// TestCallWithRaw_AnthropicReasoning_DefaultExcludes verifies the default
// (flag-absent) /call-with-raw behavior: reasoning is empty, raw is
// text-only.
func TestCallWithRaw_AnthropicReasoning_DefaultExcludes(t *testing.T) {
	forEachUnaryClient(t, func(t *testing.T, client *testutil.BAMLRestClient) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		opts := setupAnthropicReasoningScenario(
			t,
			"test-anthropic-reason-call-default",
			reasoningTestContent,
			reasoningTestThinking,
			false,
			nil,
		)

		resp, err := client.CallWithRaw(ctx, testutil.CallRequest{
			Method:  "GetSimple",
			Input:   map[string]any{"input": "anything"},
			Options: opts,
		})
		if err != nil {
			t.Fatalf("CallWithRaw failed: %v", err)
		}
		if resp.StatusCode != 200 {
			t.Fatalf("Expected status 200, got %d: %s", resp.StatusCode, resp.Error)
		}

		if resp.Raw != reasoningTestContent {
			t.Errorf("default flag: raw got %q, want %q", resp.Raw, reasoningTestContent)
		}
		if resp.Reasoning != "" {
			t.Errorf("default flag: reasoning got %q, want empty", resp.Reasoning)
		}
		assertParsedMessage(t, resp.Data, "hello")
	})
}

// TestCallWithRaw_AnthropicReasoning_OptInPopulatesReasoning verifies the
// opt-in (flag=true) /call-with-raw behavior: raw stays text-only;
// reasoning carries the provider's thinking text on its dedicated channel.
func TestCallWithRaw_AnthropicReasoning_OptInPopulatesReasoning(t *testing.T) {
	forEachUnaryClient(t, func(t *testing.T, client *testutil.BAMLRestClient) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		opts := setupAnthropicReasoningScenario(
			t,
			"test-anthropic-reason-call-optin",
			reasoningTestContent,
			reasoningTestThinking,
			false,
			boolPtr(true),
		)

		resp, err := client.CallWithRaw(ctx, testutil.CallRequest{
			Method:  "GetSimple",
			Input:   map[string]any{"input": "anything"},
			Options: opts,
		})
		if err != nil {
			t.Fatalf("CallWithRaw failed: %v", err)
		}
		if resp.StatusCode != 200 {
			t.Fatalf("Expected status 200, got %d: %s", resp.StatusCode, resp.Error)
		}

		if resp.Raw != reasoningTestContent {
			t.Errorf("opt-in: raw got %q, want %q (raw is text-only by construction)", resp.Raw, reasoningTestContent)
		}
		if resp.Reasoning != reasoningTestThinking {
			t.Errorf("opt-in: reasoning got %q, want %q", resp.Reasoning, reasoningTestThinking)
		}
		assertParsedMessage(t, resp.Data, "hello")
	})
}

// TestCallWithRaw_AnthropicReasoning_ParseableInvariant runs the same
// scenario through both flag values and asserts the parsed Data is
// byte-identical.
func TestCallWithRaw_AnthropicReasoning_ParseableInvariant(t *testing.T) {
	forEachUnaryClient(t, func(t *testing.T, client *testutil.BAMLRestClient) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		optsDefault := setupAnthropicReasoningScenario(
			t,
			"test-anthropic-reason-invariant-default",
			reasoningTestContent,
			reasoningTestThinking,
			false,
			nil,
		)
		optsOptIn := setupAnthropicReasoningScenario(
			t,
			"test-anthropic-reason-invariant-optin",
			reasoningTestContent,
			reasoningTestThinking,
			false,
			boolPtr(true),
		)

		respDefault, err := client.CallWithRaw(ctx, testutil.CallRequest{
			Method:  "GetSimple",
			Input:   map[string]any{"input": "anything"},
			Options: optsDefault,
		})
		if err != nil {
			t.Fatalf("default CallWithRaw failed: %v", err)
		}
		if respDefault.StatusCode != 200 {
			t.Fatalf("default: expected status 200, got %d: %s", respDefault.StatusCode, respDefault.Error)
		}

		respOptIn, err := client.CallWithRaw(ctx, testutil.CallRequest{
			Method:  "GetSimple",
			Input:   map[string]any{"input": "anything"},
			Options: optsOptIn,
		})
		if err != nil {
			t.Fatalf("opt-in CallWithRaw failed: %v", err)
		}
		if respOptIn.StatusCode != 200 {
			t.Fatalf("opt-in: expected status 200, got %d: %s", respOptIn.StatusCode, respOptIn.Error)
		}

		// Sanity: reasoning diverges across flag values.
		if respDefault.Reasoning == respOptIn.Reasoning {
			t.Fatalf("reasoning values unexpectedly identical between flag states: %q", respDefault.Reasoning)
		}
		// Raw must be byte-identical across the flag (text-only in both).
		if respDefault.Raw != respOptIn.Raw {
			t.Errorf("raw diverged across flag values: default=%q opt-in=%q (raw should be flag-invariant)", respDefault.Raw, respOptIn.Raw)
		}

		if string(respDefault.Data) != string(respOptIn.Data) {
			t.Errorf("parseable invariant violated:\n  default Data: %s\n  opt-in  Data: %s", respDefault.Data, respOptIn.Data)
		}
	})
}

// streamWithRawResult is the accumulated outcome of a /stream-with-raw run.
type streamWithRawResult struct {
	lastRaw            string
	lastReasoning      string
	finalEvent         *testutil.StreamEvent
	rawSnapshots       []string
	reasoningSnapshots []string
	dataEvents         []stdjson.RawMessage
}

// streamWithRawTransport selects the wire format under test. Both
// transports run against the same /stream-with-raw endpoint; the server
// negotiates SSE vs NDJSON via the Accept header. Coverage across both
// is required because PR #242 changed both wire formats and the
// integration testutil has separate parseSSE / parseNDJSON decoders for
// each — a regression that drops or swaps reasoning on one path would
// otherwise slip through.
type streamWithRawTransport int

const (
	streamWithRawTransportSSE streamWithRawTransport = iota
	streamWithRawTransportNDJSON
)

func (tp streamWithRawTransport) name() string {
	switch tp {
	case streamWithRawTransportNDJSON:
		return "ndjson"
	default:
		return "sse"
	}
}

func runStreamWithRaw(t *testing.T, ctx context.Context, opts *testutil.BAMLOptions) streamWithRawResult {
	return runStreamWithRawTransport(t, ctx, opts, streamWithRawTransportSSE)
}

func runStreamWithRawTransport(t *testing.T, ctx context.Context, opts *testutil.BAMLOptions, transport streamWithRawTransport) streamWithRawResult {
	t.Helper()

	req := testutil.CallRequest{
		Method:  "GetSimple",
		Input:   map[string]any{"input": "anything"},
		Options: opts,
	}

	var events <-chan testutil.StreamEvent
	var errs <-chan error
	switch transport {
	case streamWithRawTransportNDJSON:
		events, errs = BAMLClient.StreamWithRawNDJSON(ctx, req)
	default:
		events, errs = BAMLClient.StreamWithRaw(ctx, req)
	}

	var result streamWithRawResult
	for {
		select {
		case event, ok := <-events:
			if !ok {
				if result.finalEvent == nil {
					t.Fatalf("[%s] stream closed without a terminal event; raw snapshots: %d, reasoning snapshots: %d, data events: %d", transport.name(), len(result.rawSnapshots), len(result.reasoningSnapshots), len(result.dataEvents))
				}
				return result
			}
			if event.IsMetadata() {
				continue
			}
			if event.IsFinal() {
				ev := event
				result.finalEvent = &ev
			}
			if event.Raw != "" {
				result.lastRaw = event.Raw
				result.rawSnapshots = append(result.rawSnapshots, event.Raw)
			}
			if event.Reasoning != "" {
				result.lastReasoning = event.Reasoning
				result.reasoningSnapshots = append(result.reasoningSnapshots, event.Reasoning)
			}
			if len(event.Data) > 0 && string(event.Data) != "null" {
				result.dataEvents = append(result.dataEvents, event.Data)
			}
		case err, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			if err != nil {
				t.Fatalf("[%s] Stream error: %v", transport.name(), err)
			}
		case <-ctx.Done():
			t.Fatal("Context cancelled")
		}
	}
}

// TestStreamWithRaw_AnthropicReasoning_DefaultExcludes verifies the default
// /stream-with-raw behavior: per-event reasoning values are always empty
// and the cumulative raw at end-of-stream equals the text content.
func TestStreamWithRaw_AnthropicReasoning_DefaultExcludes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	opts := setupAnthropicReasoningScenario(
		t,
		"test-anthropic-reason-stream-default",
		reasoningTestContent,
		reasoningTestThinking,
		true,
		nil,
	)

	result := runStreamWithRaw(t, ctx, opts)

	if result.lastRaw != reasoningTestContent {
		t.Errorf("default stream: final cumulative raw got %q, want %q", result.lastRaw, reasoningTestContent)
	}
	if len(result.reasoningSnapshots) != 0 {
		t.Errorf("default stream: expected no reasoning snapshots, got %d", len(result.reasoningSnapshots))
	}

	for i, snap := range result.rawSnapshots {
		if containsAny(snap, "answer", "Let me think") {
			t.Errorf("default stream: raw snapshot %d leaked thinking content: %q", i, snap)
		}
	}

	assertParsedMessage(t, result.finalEvent.Data, "hello")
}

// TestStreamWithRaw_AnthropicReasoning_OptInPopulatesReasoning verifies the
// opt-in /stream-with-raw behavior: the final cumulative reasoning carries
// the thinking text and raw stays text-only.
func TestStreamWithRaw_AnthropicReasoning_OptInPopulatesReasoning(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	opts := setupAnthropicReasoningScenario(
		t,
		"test-anthropic-reason-stream-optin",
		reasoningTestContent,
		reasoningTestThinking,
		true,
		boolPtr(true),
	)

	result := runStreamWithRaw(t, ctx, opts)

	if result.lastRaw != reasoningTestContent {
		t.Errorf("opt-in stream: final cumulative raw got %q, want %q (raw is text-only)", result.lastRaw, reasoningTestContent)
	}
	if result.lastReasoning != reasoningTestThinking {
		t.Errorf("opt-in stream: final cumulative reasoning got %q, want %q", result.lastReasoning, reasoningTestThinking)
	}

	const reasoningOnlyMarker = "answer"
	sawMarker := false
	for _, snap := range result.reasoningSnapshots {
		if containsAny(snap, reasoningOnlyMarker) {
			sawMarker = true
			break
		}
	}
	if !sawMarker {
		t.Errorf("opt-in stream: expected reasoning to contain %q somewhere; got snapshots: %v", reasoningOnlyMarker, result.reasoningSnapshots)
	}

	// Raw must NOT contain the thinking text under any flag value.
	for i, snap := range result.rawSnapshots {
		if containsAny(snap, reasoningOnlyMarker, "Let me think") {
			t.Errorf("opt-in stream: raw snapshot %d leaked thinking content: %q", i, snap)
		}
	}

	assertParsedMessage(t, result.finalEvent.Data, "hello")
}

// TestStreamWithRaw_AnthropicReasoning_ParseableInvariant_PerEvent codifies
// the strongest version of the parseable invariant: under opt-in,
// reasoning reaches the wire's reasoning channel BUT no per-event Data
// value ever reflects thinking-derived JSON.
func TestStreamWithRaw_AnthropicReasoning_ParseableInvariant_PerEvent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	opts := setupAnthropicReasoningScenario(
		t,
		"test-anthropic-reason-stream-parseable-invariant",
		reasoningTestContent,
		reasoningTestThinking,
		true,
		boolPtr(true),
	)

	result := runStreamWithRaw(t, ctx, opts)

	if len(result.dataEvents) == 0 {
		t.Fatal("opt-in stream parseable invariant: no Data events observed")
	}

	for i, data := range result.dataEvents {
		var parsed struct {
			Message string `json:"message"`
		}
		if err := sonic.Unmarshal(data, &parsed); err != nil {
			continue
		}
		if parsed.Message == "answer" {
			t.Errorf("opt-in stream Data event %d leaked thinking-derived value: parsed message %q (raw data: %s)", i, parsed.Message, string(data))
		}
	}

	const reasoningOnlyMarker = "answer"
	sawMarker := false
	for _, snap := range result.reasoningSnapshots {
		if containsAny(snap, reasoningOnlyMarker) {
			sawMarker = true
			break
		}
	}
	if !sawMarker {
		t.Fatalf("opt-in stream parseable invariant: expected reasoning to contain %q somewhere; got snapshots: %v", reasoningOnlyMarker, result.reasoningSnapshots)
	}
}

// TestStreamWithRawNDJSON_AnthropicReasoning_FlagContract mirrors the SSE
// flag-off / flag-on contract across the NDJSON transport. PR #242
// changed both wire formats (NDJSONEvent gains a `reasoning` field on
// the server, parseNDJSON decodes it on the client); without dedicated
// NDJSON coverage a regression that drops or swaps reasoning on the
// NDJSON path slips through.
//
// Subtest grouping (rather than a separate test function) keeps the
// fixture, marker constants, and reasoning-channel contract assertions
// in one place — both subtests assert exactly the same shape, just
// against different transports.
func TestStreamWithRawNDJSON_AnthropicReasoning_FlagContract(t *testing.T) {
	t.Run("default_excludes", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		opts := setupAnthropicReasoningScenario(
			t,
			"test-anthropic-reason-stream-ndjson-default",
			reasoningTestContent,
			reasoningTestThinking,
			true,
			nil,
		)

		result := runStreamWithRawTransport(t, ctx, opts, streamWithRawTransportNDJSON)

		if result.lastRaw != reasoningTestContent {
			t.Errorf("ndjson default: final cumulative raw got %q, want %q", result.lastRaw, reasoningTestContent)
		}
		if len(result.reasoningSnapshots) != 0 {
			t.Errorf("ndjson default: expected no reasoning snapshots, got %d", len(result.reasoningSnapshots))
		}
		for i, snap := range result.rawSnapshots {
			if containsAny(snap, "answer", "Let me think") {
				t.Errorf("ndjson default: raw snapshot %d leaked thinking content: %q", i, snap)
			}
		}
		assertParsedMessage(t, result.finalEvent.Data, "hello")
	})

	t.Run("opt_in_populates_reasoning", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		opts := setupAnthropicReasoningScenario(
			t,
			"test-anthropic-reason-stream-ndjson-optin",
			reasoningTestContent,
			reasoningTestThinking,
			true,
			boolPtr(true),
		)

		result := runStreamWithRawTransport(t, ctx, opts, streamWithRawTransportNDJSON)

		if result.lastRaw != reasoningTestContent {
			t.Errorf("ndjson opt-in: final cumulative raw got %q, want %q (raw is text-only)", result.lastRaw, reasoningTestContent)
		}
		if result.lastReasoning != reasoningTestThinking {
			t.Errorf("ndjson opt-in: final cumulative reasoning got %q, want %q", result.lastReasoning, reasoningTestThinking)
		}

		const reasoningOnlyMarker = "answer"
		sawMarker := false
		for _, snap := range result.reasoningSnapshots {
			if containsAny(snap, reasoningOnlyMarker) {
				sawMarker = true
				break
			}
		}
		if !sawMarker {
			t.Errorf("ndjson opt-in: expected reasoning to contain %q somewhere; got snapshots: %v", reasoningOnlyMarker, result.reasoningSnapshots)
		}

		// Raw must NOT contain the thinking text under any flag value.
		for i, snap := range result.rawSnapshots {
			if containsAny(snap, reasoningOnlyMarker, "Let me think") {
				t.Errorf("ndjson opt-in: raw snapshot %d leaked thinking content: %q", i, snap)
			}
		}
		assertParsedMessage(t, result.finalEvent.Data, "hello")
	})
}

// containsAny returns true if s contains any of the given substrings.
func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if sub == "" {
			continue
		}
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
