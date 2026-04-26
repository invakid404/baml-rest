//go:build integration

package integration

// Integration tests for the IncludeThinkingInRaw opt-in covering the
// /call-with-raw and /stream-with-raw endpoints against the mock Anthropic
// provider. These exercise the full plumbing chain — JSON →
// BamlOptions.IncludeThinkingInRaw → Adapter.SetIncludeThinkingInRaw →
// StreamConfig/CallConfig → extractors → HTTP envelope — that the unit
// tests below the orchestrator boundary can't cover end-to-end.
//
// Each public scenario is exercised in three flag states:
//
//   - "default" (omitted from the request body) — expected: thinking absent
//     from raw, matching upstream BAML's RawLLMResponse() text-only contract.
//   - "explicit_false" — expected: identical to default. Codifies that
//     setting the field to false must not differ from omitting it.
//   - "opt_in" (true) — expected: thinking text included in raw, parseable
//     unchanged.
//
// A dedicated parseable-invariant test runs the same input through both the
// default and opt-in flag values and asserts the parsed Data is byte-identical
// across both. This codifies the structural guarantee that flipping the flag
// can never leak thinking into the parser; the gate is on the rawSB / Raw
// writes only, never on parseableSB / Parseable.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/invakid404/baml-rest/integration/mockllm"
	"github.com/invakid404/baml-rest/integration/testutil"
)

const (
	// thinkingTestContent is the text content the mock returns. Kept as a
	// well-formed JSON object so GetSimple can parse it; the parser only sees
	// this content (never the thinking) regardless of the flag value, by
	// construction.
	thinkingTestContent = `{"message": "hello"}`

	// thinkingTestThinking is the thinking-block content the mock emits
	// before the text content. Deliberately chosen to look JSON-ish so a
	// regression that accidentally fed it into the parser would corrupt
	// resp.Data with `"answer"` instead of `"hello"` — making any leak
	// loud and trivially detectable.
	thinkingTestThinking = `Let me think about this. The answer should be: {"message": "answer"}`
)

// setupAnthropicThinkingScenario registers an Anthropic-provider scenario
// with both text content and thinking content, returning BAMLOptions ready
// for use against /call-with-raw or /stream-with-raw.
//
// includeThinkingInRaw maps to BamlOptions.IncludeThinkingInRaw verbatim;
// callers pass *bool so a nil value omits the field from the JSON entirely
// (matching the "default" flag-absent shape).
func setupAnthropicThinkingScenario(t *testing.T, scenarioID, content, thinking string, streaming bool, includeThinkingInRaw *bool) *testutil.BAMLOptions {
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
		// Small chunks so the stream emits multiple deltas per block; this
		// also exercises the IncrementalExtractor's accumulation path the
		// per-event raw assertions depend on.
		scenario.ChunkSize = 8
		scenario.ChunkDelayMs = 5
	}

	if err := MockClient.RegisterScenario(ctx, scenario); err != nil {
		t.Fatalf("Failed to register scenario: %v", err)
	}

	opts := &testutil.BAMLOptions{
		ClientRegistry: testutil.CreateAnthropicTestClient(TestEnv.MockLLMInternal, scenarioID),
	}
	if includeThinkingInRaw != nil {
		opts.IncludeThinkingInRaw = *includeThinkingInRaw
	}
	return opts
}

// boolPtr returns a pointer to v. Used to disambiguate a missing flag (nil)
// from an explicitly-set flag (&true / &false) when constructing BamlOptions.
func boolPtr(v bool) *bool { return &v }

// assertParsedMessage decodes resp.Data and asserts the parsed message matches
// the expected text — never the thinking. Centralized because every
// /call-with-raw test in this file makes the same assertion (the parseable
// guarantee is the whole point of the opt-in design).
func assertParsedMessage(t *testing.T, data json.RawMessage, want string) {
	t.Helper()
	var parsed struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("Failed to unmarshal parsed data: %v (raw bytes: %s)", err, string(data))
	}
	if parsed.Message != want {
		t.Errorf("Parsed message: got %q, want %q (full data: %s)", parsed.Message, want, string(data))
	}
}

// TestCallWithRaw_AnthropicThinking_DefaultExcludes verifies the default
// (flag-absent) /call-with-raw behavior: raw contains the text only,
// matching upstream BAML's RawLLMResponse() contract. Thinking is dropped
// across the JSON → adapter → orchestrator → extractor chain.
func TestCallWithRaw_AnthropicThinking_DefaultExcludes(t *testing.T) {
	forEachUnaryClient(t, func(t *testing.T, client *testutil.BAMLRestClient) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		opts := setupAnthropicThinkingScenario(
			t,
			"test-anthropic-think-call-default",
			thinkingTestContent,
			thinkingTestThinking,
			false, /*streaming*/
			nil,   /*flag absent*/
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

		if resp.Raw != thinkingTestContent {
			t.Errorf("default flag: raw got %q, want %q (thinking should be excluded)", resp.Raw, thinkingTestContent)
		}
		assertParsedMessage(t, resp.Data, "hello")
	})
}

// TestCallWithRaw_AnthropicThinking_OptInIncludes verifies the opt-in
// (flag=true) /call-with-raw behavior: raw includes both the thinking and
// text content, with thinking emitted before text (matching the wire order
// of the Anthropic content blocks).
func TestCallWithRaw_AnthropicThinking_OptInIncludes(t *testing.T) {
	forEachUnaryClient(t, func(t *testing.T, client *testutil.BAMLRestClient) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		opts := setupAnthropicThinkingScenario(
			t,
			"test-anthropic-think-call-optin",
			thinkingTestContent,
			thinkingTestThinking,
			false, /*streaming*/
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

		want := thinkingTestThinking + thinkingTestContent
		if resp.Raw != want {
			t.Errorf("opt-in: raw got %q, want %q", resp.Raw, want)
		}
		// Parseable invariant — thinking, even though it contains a JSON-ish
		// fragment that mentions a different message value, must NOT confuse
		// the parser.
		assertParsedMessage(t, resp.Data, "hello")
	})
}

// TestCallWithRaw_AnthropicThinking_ParseableInvariant runs the same
// scenario through both the default and opt-in flag values and asserts the
// parsed Data is byte-identical. This codifies the structural guarantee that
// IncludeThinkingInRaw can never affect what the BAML parser sees — the gate
// is on raw writes only, not parseable.
func TestCallWithRaw_AnthropicThinking_ParseableInvariant(t *testing.T) {
	forEachUnaryClient(t, func(t *testing.T, client *testutil.BAMLRestClient) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		// Use distinct scenario IDs so the request-count counter doesn't
		// interfere across invocations; the underlying mock content is
		// identical.
		optsDefault := setupAnthropicThinkingScenario(
			t,
			"test-anthropic-think-invariant-default",
			thinkingTestContent,
			thinkingTestThinking,
			false,
			nil,
		)
		optsOptIn := setupAnthropicThinkingScenario(
			t,
			"test-anthropic-think-invariant-optin",
			thinkingTestContent,
			thinkingTestThinking,
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

		// Sanity checks the raw values diverge as expected — confirms the
		// flag actually had an effect, so the Data equality below is a
		// meaningful invariant check rather than a tautology.
		if string(respDefault.Raw) == string(respOptIn.Raw) {
			t.Fatalf("raw values unexpectedly identical between flag states: %q", respDefault.Raw)
		}

		if string(respDefault.Data) != string(respOptIn.Data) {
			t.Errorf("parseable invariant violated:\n  default Data: %s\n  opt-in  Data: %s", respDefault.Data, respOptIn.Data)
		}
	})
}

// streamWithRawResult is the accumulated outcome of a /stream-with-raw run,
// used by both the default and opt-in stream tests to share the same event
// loop while keeping per-test assertions narrow.
type streamWithRawResult struct {
	lastRaw      string                // last non-empty Raw observed; cumulative by orchestrator design
	finalEvent   *testutil.StreamEvent // the "final" event (if seen), carries fully validated Data
	rawSnapshots []string              // every non-empty Raw observed, in order
	dataEvents   []json.RawMessage     // every Data observed (intermediate + final), in order
}

// runStreamWithRaw drives a /stream-with-raw call to completion, returning
// the accumulated raw snapshots and the final event for caller assertions.
func runStreamWithRaw(t *testing.T, ctx context.Context, opts *testutil.BAMLOptions) streamWithRawResult {
	t.Helper()

	events, errs := BAMLClient.StreamWithRaw(ctx, testutil.CallRequest{
		Method:  "GetSimple",
		Input:   map[string]any{"input": "anything"},
		Options: opts,
	})

	var result streamWithRawResult
	for {
		select {
		case event, ok := <-events:
			if !ok {
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
			if len(event.Data) > 0 && string(event.Data) != "null" {
				result.dataEvents = append(result.dataEvents, event.Data)
			}
		case err, ok := <-errs:
			if !ok {
				// errs closed by the streaming goroutine; nil out the
				// channel so this select arm blocks forever and we
				// continue draining events without spinning. The events
				// arm's !ok check below is what terminates the loop.
				errs = nil
				continue
			}
			if err != nil {
				t.Fatalf("Stream error: %v", err)
			}
		case <-ctx.Done():
			t.Fatal("Context cancelled")
		}
	}
}

// TestStreamWithRaw_AnthropicThinking_DefaultExcludes verifies the default
// (flag-absent) /stream-with-raw behavior: per-event raw values exclude
// thinking deltas, and the cumulative raw at end-of-stream equals the text
// content. Thinking SSE events emit no per-event raw at all, since the
// orchestrator skips StreamResultKindStream when delta.Raw == "".
func TestStreamWithRaw_AnthropicThinking_DefaultExcludes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	opts := setupAnthropicThinkingScenario(
		t,
		"test-anthropic-think-stream-default",
		thinkingTestContent,
		thinkingTestThinking,
		true, /*streaming*/
		nil,
	)

	result := runStreamWithRaw(t, ctx, opts)

	if result.lastRaw != thinkingTestContent {
		t.Errorf("default stream: final cumulative raw got %q, want %q", result.lastRaw, thinkingTestContent)
	}

	// No per-event raw snapshot should ever contain the thinking text — even
	// on intermediate snapshots that show partial accumulations. Any leak is
	// a regression in the gating logic.
	for i, snap := range result.rawSnapshots {
		// A leaked thinking delta would manifest as a raw snapshot that
		// either starts with a thinking byte (snap[0] is the first byte of
		// thinking content) or contains a substring of the thinking text
		// that isn't also in the text content.
		if containsAny(snap, "answer", "Let me think") {
			t.Errorf("default stream: raw snapshot %d leaked thinking content: %q", i, snap)
		}
	}

	if result.finalEvent != nil {
		assertParsedMessage(t, result.finalEvent.Data, "hello")
	}
}

// TestStreamWithRaw_AnthropicThinking_OptInIncludes verifies the opt-in
// (flag=true) /stream-with-raw behavior: the final cumulative raw equals
// thinking + text concatenated, and at least one observable raw event
// surfaces thinking content alongside text content.
//
// The path-specific timing of when thinking content first appears in
// per-event raw differs between the BuildRequest and legacy paths:
//
//   - BuildRequest emits per-event raw deltas as the orchestrator parses
//     each SSE frame, so intermediate snapshots can be a strict prefix of
//     thinking text before any text deltas arrive.
//   - Legacy uses the IncrementalExtractor + ParseStream loop. The
//     extractor maintains separate parseable (text-only) and raw
//     (text + thinking under opt-in) buffers. ParseStream is fed the
//     parseable buffer, so early thinking-only ticks emit a raw-only
//     partial without parsed Data. The thinking content reaches the wire
//     incrementally as those raw-only partials. At end-of-stream a
//     suffix-splice reconciliation at
//     adapters/common/codegen/codegen.go:4144-4197 compares
//     extractor.ParseableFull (text-only the extractor saw) against
//     fl.RawLLMResponse() (BAML's authoritative text-only): if the
//     latter extends the former as a prefix, the missing text suffix
//     is appended to extractor.RawFull, recovering any text lost to a
//     stale-SSE-view race while preserving accumulated thinking.
//
// Both paths converge to the same final cumulative raw, which is what the
// /with-raw contract guarantees. The test asserts that, plus a uniqueness
// check on a thinking-only substring to confirm the flag actually had an
// effect on observable raw output (rather than the final assertion
// accidentally matching for some other reason).
func TestStreamWithRaw_AnthropicThinking_OptInIncludes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	opts := setupAnthropicThinkingScenario(
		t,
		"test-anthropic-think-stream-optin",
		thinkingTestContent,
		thinkingTestThinking,
		true,
		boolPtr(true),
	)

	result := runStreamWithRaw(t, ctx, opts)

	want := thinkingTestThinking + thinkingTestContent
	if result.lastRaw != want {
		t.Errorf("opt-in stream: final cumulative raw got %q, want %q", result.lastRaw, want)
	}

	// thinkingOnlyMarker is a substring that appears in the thinking text
	// but never in the text content, so observing it in raw confirms the
	// opt-in flag took effect end-to-end. "answer" is in thinking
	// ('{"message": "answer"}') but not in content ('{"message": "hello"}').
	const thinkingOnlyMarker = "answer"
	sawThinkingMarker := false
	for _, snap := range result.rawSnapshots {
		if containsAny(snap, thinkingOnlyMarker) {
			sawThinkingMarker = true
			break
		}
	}
	if !sawThinkingMarker {
		t.Errorf("opt-in stream: expected raw to contain %q somewhere; got snapshots: %v", thinkingOnlyMarker, result.rawSnapshots)
	}

	if result.finalEvent != nil {
		// Parseable invariant — even when raw includes thinking, the parsed
		// data must reflect text only.
		assertParsedMessage(t, result.finalEvent.Data, "hello")
	}
}

// TestStreamWithRaw_AnthropicThinking_ParseableInvariant_PerEvent codifies the
// strongest version of the parseable invariant: under opt-in
// (IncludeThinkingInRaw=true), thinking content reaches the wire's raw
// buffer, BUT no per-event Data value (intermediate or final) ever reflects
// thinking-derived JSON.
//
// This is the regression test for the legacy-path bug: prior to the
// IncrementalExtractor parseable/raw split, the legacy code fed
// extractor.Full() (which under opt-in contained text + thinking) directly
// to ParseStream. The thinking text used here contains a complete
// `{"message": "answer"}` JSON object — if it were fed to the parser, an
// intermediate Data event would unmarshal to message="answer" instead of
// "hello". The test exercises the BAML parser end-to-end against thinking
// content that would unambiguously corrupt parsing, so any regression
// reintroducing the leak fails loudly.
//
// The assertion runs over every Data event the stream emits, not just the
// final, because the failure mode primarily manifests in intermediate
// partials.
func TestStreamWithRaw_AnthropicThinking_ParseableInvariant_PerEvent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	opts := setupAnthropicThinkingScenario(
		t,
		"test-anthropic-think-stream-parseable-invariant",
		thinkingTestContent,
		thinkingTestThinking,
		true,
		boolPtr(true),
	)

	result := runStreamWithRaw(t, ctx, opts)

	if len(result.dataEvents) == 0 {
		t.Fatal("opt-in stream parseable invariant: no Data events observed; cannot verify per-event invariant")
	}

	for i, data := range result.dataEvents {
		var parsed struct {
			Message string `json:"message"`
		}
		if err := json.Unmarshal(data, &parsed); err != nil {
			// Intermediate partials may not be fully-formed; the parser
			// emits whatever shape it can. A partial that doesn't have a
			// message field at all is fine — that's not a leak. Skip.
			continue
		}
		if parsed.Message == "answer" {
			t.Errorf("opt-in stream Data event %d leaked thinking-derived value: parsed message %q (raw data: %s)", i, parsed.Message, string(data))
		}
	}

	// Confirm raw still carries thinking (i.e. the flag actually had an
	// effect; otherwise the per-event invariant above is a tautology).
	const thinkingOnlyMarker = "answer"
	sawThinkingMarker := false
	for _, snap := range result.rawSnapshots {
		if containsAny(snap, thinkingOnlyMarker) {
			sawThinkingMarker = true
			break
		}
	}
	if !sawThinkingMarker {
		t.Fatalf("opt-in stream parseable invariant: expected raw to contain %q somewhere; got snapshots: %v (sanity check failed — flag not honored)", thinkingOnlyMarker, result.rawSnapshots)
	}
}

// containsAny returns true if s contains any of the given substrings. Tiny
// helper to keep the leak-detection assertions readable. Empty subs are
// skipped (rather than matching every string per strings.Contains' semantics)
// so callers can pass conditionally-empty markers without changing the test's
// outcome.
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
