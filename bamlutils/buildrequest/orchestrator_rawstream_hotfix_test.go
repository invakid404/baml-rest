package buildrequest

// Hotfix regression suite: streaming-with-raw must deliver live raw
// partials even when the intermediate structured parse fails, and (opt-in)
// must be able to complete successfully on a final structured-parse miss
// by surfacing the accumulated raw text.
//
// Reproduces the customer bug filed against 0.0.48: a class output schema
// ({value: string}) combined with a plain-prose model response makes BAML's
// ParseStream/Parse return a root-coercion error for every partial and for
// the final. On clean 0.0.48 the orchestrator gates raw-delta emission on
// structured-parse SUCCESS (both stream-child funcs), so no live raw
// partials reach the caller and the whole call hard-fails at the final
// parse. See /tmp/shared/customer-repro/ISSUE.md.
//
// These tests drive the REAL RunStreamOrchestration against REAL httptest
// SSE / AWS-event-stream servers streaming the exact customer prose deltas.
// Only parseStream/parseFinal are injected — they stand in for the pinned
// BAML runtime's root-coercion verdict, which is runtime-originated and not
// the behavior under repair.

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
)

// proseChunks is the customer's exact plain-text stream: none of these
// accumulated prefixes coerce to the requested root class, so BAML's
// ParseStream rejects every one of them.
var proseChunks = []string{"Here is ", "plain prose, ", "not structured JSON."}

// rootCoercionParseStream mimics the pinned BAML runtime's partial-parse
// verdict for a plain-text prefix against a class schema: it always fails.
func rootCoercionParseStream(_ context.Context, accumulated string) (any, error) {
	return nil, fmt.Errorf(
		"Failed to coerce value: [InferedObject(String(%q, Incomplete))]", accumulated)
}

// collectResults drains the buffered result channel into a typed slice.
func collectResults(out chan bamlutils.StreamResult) []*testResult {
	close(out)
	var results []*testResult
	for r := range out {
		results = append(results, r.(*testResult))
	}
	return results
}

// TestRunStreamOrchestration_RawPartialsSurviveParseFailure_SSE is the
// primary lock for fix (1): live raw partials must reach the caller as the
// prose streams, even though every ParseStream call errors. parseFinal is
// made to succeed here to isolate the partial-gating bug from the final-
// parse behavior (covered separately).
//
// FAILS on clean 0.0.48: the SSE stream child only emits a raw delta inside
// the `parseErr == nil && parsed != nil` branch, so a failing ParseStream
// drops every raw partial → 0 stream events instead of 3.
func TestRunStreamOrchestration_RawPartialsSurviveParseFailure_SSE(t *testing.T) {
	server := makeOpenAIServer(proseChunks)
	defer server.Close()

	client := llmhttp.NewClient(server.Client())
	out := make(chan bamlutils.StreamResult, 100)

	config := &StreamConfig{
		Provider:      "openai",
		NeedsPartials: true,
		NeedsRaw:      true,
	}

	err := RunStreamOrchestration(
		context.Background(), out, config, client,
		func(_ context.Context, _ string) (*llmhttp.Request, error) {
			return &llmhttp.Request{URL: server.URL, Method: "POST", Body: `{}`}, nil
		},
		rootCoercionParseStream,
		// Final parse succeeds so the call completes cleanly and the test
		// asserts purely on the live partial stream.
		func(_ context.Context, accumulated string) (any, error) {
			return map[string]any{"value": accumulated}, nil
		},
		newTestResult,
	)
	if err != nil {
		t.Fatalf("RunStreamOrchestration returned error: %v", err)
	}

	results := collectResults(out)
	assertLiveRawPartials(t, results, proseChunks)

	// The completed call must surface a successful final, never an error.
	var finals, streamErrors int
	for _, r := range results {
		switch r.kind {
		case bamlutils.StreamResultKindFinal:
			finals++
		case bamlutils.StreamResultKindError:
			streamErrors++
		}
	}
	if streamErrors != 0 {
		t.Errorf("expected 0 error results, got %d", streamErrors)
	}
	if finals != 1 {
		t.Errorf("expected 1 final result, got %d", finals)
	}
}

// TestRunStreamOrchestration_RawPartialsSurviveParseFailure_Bedrock is the
// same lock for the AWS-event-stream (aws-bedrock) stream child, which
// carries an identical parse-gate. contentBlockDelta text frames stream the
// prose; every ParseStream errors.
//
// FAILS on clean 0.0.48 for the same reason as the SSE case.
func TestRunStreamOrchestration_RawPartialsSurviveParseFailure_Bedrock(t *testing.T) {
	pinnedTime := time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)

	var frames bytes.Buffer
	frames.Write(bedrockStreamFrame(t, "messageStart", []byte(`{"role":"assistant"}`)))
	for _, chunk := range proseChunks {
		// %q emits a valid JSON string for these ASCII prose deltas.
		payload := fmt.Sprintf(`{"delta":{"text":%q}}`, chunk)
		frames.Write(bedrockStreamFrame(t, "contentBlockDelta", []byte(payload)))
	}
	frames.Write(bedrockStreamFrame(t, "contentBlockStop", []byte(`{"contentBlockIndex":0}`)))
	frames.Write(bedrockStreamFrame(t, "messageStop", []byte(`{"stopReason":"end_turn"}`)))

	var sawHeaders atomic.Int32
	var sawPath atomic.Pointer[string]
	var sawMethod atomic.Pointer[string]
	server := newMockBedrockStreamServer(t, frames.Bytes(), &sawHeaders, &sawPath, &sawMethod)
	defer server.Close()

	client := llmhttp.NewClient(server.Client())
	out := make(chan bamlutils.StreamResult, 100)

	config := &StreamConfig{
		Provider:                  "aws-bedrock",
		NeedsPartials:             true,
		NeedsRaw:                  true,
		BuildBedrockStreamRequest: makeBedrockStreamRequestFn(server.URL, pinnedTime),
	}

	err := RunStreamOrchestration(
		context.Background(), out, config, client,
		func(_ context.Context, _ string) (*llmhttp.Request, error) {
			return nil, fmt.Errorf("buildRequest must not be called for aws-bedrock")
		},
		rootCoercionParseStream,
		func(_ context.Context, accumulated string) (any, error) {
			return map[string]any{"value": accumulated}, nil
		},
		newTestResult,
	)
	if err != nil {
		t.Fatalf("RunStreamOrchestration returned error: %v", err)
	}

	results := collectResults(out)
	assertLiveRawPartials(t, results, proseChunks)
}

// TestRunStreamOrchestration_NonRawStrictOnParseFailure_SSE is the scope
// guard: a NON-raw stream (NeedsRaw=false) must be byte-for-byte unchanged
// by the hotfix — no partials emitted while ParseStream fails, and the call
// still hard-fails at the final parse. Passes both before and after the fix.
func TestRunStreamOrchestration_NonRawStrictOnParseFailure_SSE(t *testing.T) {
	server := makeOpenAIServer(proseChunks)
	defer server.Close()

	client := llmhttp.NewClient(server.Client())
	out := make(chan bamlutils.StreamResult, 100)

	config := &StreamConfig{
		Provider:      "openai",
		NeedsPartials: true,
		NeedsRaw:      false, // non-raw structured stream
	}

	_ = RunStreamOrchestration(
		context.Background(), out, config, client,
		func(_ context.Context, _ string) (*llmhttp.Request, error) {
			return &llmhttp.Request{URL: server.URL, Method: "POST", Body: `{}`}, nil
		},
		rootCoercionParseStream,
		rootCoercionParseStream, // final parse also fails
		newTestResult,
	)

	results := collectResults(out)

	var partials, streamErrors, finals int
	for _, r := range results {
		switch {
		case r.kind == bamlutils.StreamResultKindStream && !r.reset:
			partials++
		case r.kind == bamlutils.StreamResultKindError:
			streamErrors++
		case r.kind == bamlutils.StreamResultKindFinal:
			finals++
		}
	}
	if partials != 0 {
		t.Errorf("non-raw stream must emit 0 partials while ParseStream fails, got %d", partials)
	}
	if finals != 0 {
		t.Errorf("non-raw stream must not emit a final on a final-parse miss, got %d", finals)
	}
	if streamErrors != 1 {
		t.Errorf("non-raw stream must hard-fail with 1 error on a final-parse miss, got %d", streamErrors)
	}
}

// runSoftFinalScenario drives a raw stream whose partial AND final parses
// both fail (the full customer scenario), toggling only StreamConfig.
// SoftFinalParse, and returns the collected results.
func runSoftFinalScenario(t *testing.T, softFinalParse bool) []*testResult {
	t.Helper()
	server := makeOpenAIServer(proseChunks)
	defer server.Close()

	client := llmhttp.NewClient(server.Client())
	out := make(chan bamlutils.StreamResult, 100)

	config := &StreamConfig{
		Provider:       "openai",
		NeedsPartials:  true,
		NeedsRaw:       true,
		SoftFinalParse: softFinalParse,
	}

	_ = RunStreamOrchestration(
		context.Background(), out, config, client,
		func(_ context.Context, _ string) (*llmhttp.Request, error) {
			return &llmhttp.Request{URL: server.URL, Method: "POST", Body: `{}`}, nil
		},
		rootCoercionParseStream,
		rootCoercionParseStream, // final parse also fails (root coercion)
		newTestResult,
	)
	return collectResults(out)
}

// TestRunStreamOrchestration_SoftFinalParse_OptIn_ReturnsRawSuccess_SSE is
// the lock for fix (2): with StreamConfig.SoftFinalParse enabled, a final
// structured-parse miss on a raw-wanted stream must complete SUCCESSFULLY,
// surfacing the full accumulated raw text as the result — not a terminal
// error.
//
// FAILS on clean 0.0.48: the field is inert, so the final parse still
// hard-fails (newRawError) → 1 error result, 0 finals.
func TestRunStreamOrchestration_SoftFinalParse_OptIn_ReturnsRawSuccess_SSE(t *testing.T) {
	results := runSoftFinalScenario(t, true /* SoftFinalParse */)

	var finals, streamErrors int
	var finalRaw string
	var finalData any
	for _, r := range results {
		switch r.kind {
		case bamlutils.StreamResultKindFinal:
			finals++
			finalRaw = r.raw
			finalData = r.final
		case bamlutils.StreamResultKindError:
			streamErrors++
		}
	}

	if streamErrors != 0 {
		t.Errorf("SoftFinalParse ON: expected 0 error results, got %d", streamErrors)
	}
	if finals != 1 {
		t.Fatalf("SoftFinalParse ON: expected 1 successful final, got %d", finals)
	}
	if want := strings.Join(proseChunks, ""); finalRaw != want {
		t.Errorf("SoftFinalParse ON: final raw = %q, want full accumulated raw %q", finalRaw, want)
	}
	if finalData != nil {
		t.Errorf("SoftFinalParse ON: final structured data must be nil on a parse miss, got %#v", finalData)
	}
}

// TestRunStreamOrchestration_SoftFinalParse_DefaultStrict_Errors_SSE is the
// default-behavior guard for fix (2): with SoftFinalParse OFF (the default),
// a final structured-parse miss must STILL hard-fail the call. Passes both
// before and after the fix — proving the soft path is strictly opt-in.
func TestRunStreamOrchestration_SoftFinalParse_DefaultStrict_Errors_SSE(t *testing.T) {
	results := runSoftFinalScenario(t, false /* default: strict */)

	var finals, streamErrors int
	for _, r := range results {
		switch r.kind {
		case bamlutils.StreamResultKindFinal:
			finals++
		case bamlutils.StreamResultKindError:
			streamErrors++
		}
	}
	if finals != 0 {
		t.Errorf("SoftFinalParse OFF (default): expected 0 finals on a final-parse miss, got %d", finals)
	}
	if streamErrors != 1 {
		t.Errorf("SoftFinalParse OFF (default): expected 1 terminal error on a final-parse miss, got %d", streamErrors)
	}
}

// TestRunStreamOrchestration_RawPartialsSurviveThrottle_SSE locks the
// throttle-skip case for fix (1): with a large ParseThrottleInterval only the
// first tick parses; the rest are throttled. Raw delivery must not depend on
// parse cadence, so every prose delta's raw must still arrive.
//
// Pre-hardening this failed (the raw-only emit lived inside `if shouldParse`,
// so throttled ticks emitted nothing → 1 partial instead of 3).
func TestRunStreamOrchestration_RawPartialsSurviveThrottle_SSE(t *testing.T) {
	server := makeOpenAIServer(proseChunks)
	defer server.Close()

	client := llmhttp.NewClient(server.Client())
	out := make(chan bamlutils.StreamResult, 100)

	config := &StreamConfig{
		Provider:              "openai",
		NeedsPartials:         true,
		NeedsRaw:              true,
		ParseThrottleInterval: time.Hour, // only the first tick parses
	}

	err := RunStreamOrchestration(
		context.Background(), out, config, client,
		func(_ context.Context, _ string) (*llmhttp.Request, error) {
			return &llmhttp.Request{URL: server.URL, Method: "POST", Body: `{}`}, nil
		},
		rootCoercionParseStream,
		func(_ context.Context, accumulated string) (any, error) {
			return map[string]any{"value": accumulated}, nil
		},
		newTestResult,
	)
	if err != nil {
		t.Fatalf("RunStreamOrchestration returned error: %v", err)
	}

	assertLiveRawPartials(t, collectResults(out), proseChunks)
}

// TestRunStreamOrchestration_SoftFinalParse_HonorsCancellation_SSE locks that
// the soft-final opt-in never swallows a cancellation/deadline: when the final
// parse fails specifically with context.Canceled, the call must fall through
// to the strict path, not return a successful raw final.
func TestRunStreamOrchestration_SoftFinalParse_HonorsCancellation_SSE(t *testing.T) {
	server := makeOpenAIServer(proseChunks)
	defer server.Close()

	client := llmhttp.NewClient(server.Client())
	out := make(chan bamlutils.StreamResult, 100)

	config := &StreamConfig{
		Provider:       "openai",
		NeedsPartials:  true,
		NeedsRaw:       true,
		SoftFinalParse: true,
	}

	_ = RunStreamOrchestration(
		context.Background(), out, config, client,
		func(_ context.Context, _ string) (*llmhttp.Request, error) {
			return &llmhttp.Request{URL: server.URL, Method: "POST", Body: `{}`}, nil
		},
		rootCoercionParseStream,
		// Final parse fails with a cancellation error rather than a
		// structured-parse miss. Soft final must NOT treat this as success.
		func(_ context.Context, _ string) (any, error) { return nil, context.Canceled },
		newTestResult,
	)

	var finals, streamErrors int
	for _, r := range collectResults(out) {
		switch r.kind {
		case bamlutils.StreamResultKindFinal:
			finals++
		case bamlutils.StreamResultKindError:
			streamErrors++
		}
	}
	if finals != 0 {
		t.Errorf("SoftFinalParse must not convert a cancellation into a successful final; got %d finals", finals)
	}
	if streamErrors != 1 {
		t.Errorf("a cancellation final-parse error must take the strict error path; got %d errors", streamErrors)
	}
}

// TestRunStreamOrchestration_SoftFinalParse_ScopedToStreaming_SSE locks the
// streaming-mode boundary for fix (2): NeedsPartials=false mirrors the
// non-streaming CallWithRaw bridge (StreamModeCallWithRaw has NeedsRaw=true,
// NeedsPartials=false). SoftFinalParse must NOT soften that path — it stays
// strict on a final-parse miss.
func TestRunStreamOrchestration_SoftFinalParse_ScopedToStreaming_SSE(t *testing.T) {
	server := makeOpenAIServer(proseChunks)
	defer server.Close()

	client := llmhttp.NewClient(server.Client())
	out := make(chan bamlutils.StreamResult, 100)

	config := &StreamConfig{
		Provider:       "openai",
		NeedsPartials:  false, // non-streaming call bridge shape
		NeedsRaw:       true,
		SoftFinalParse: true,
	}

	_ = RunStreamOrchestration(
		context.Background(), out, config, client,
		func(_ context.Context, _ string) (*llmhttp.Request, error) {
			return &llmhttp.Request{URL: server.URL, Method: "POST", Body: `{}`}, nil
		},
		nil,                     // no parseStream on the call bridge
		rootCoercionParseStream, // final parse fails (root coercion)
		newTestResult,
	)

	var finals, streamErrors int
	for _, r := range collectResults(out) {
		switch r.kind {
		case bamlutils.StreamResultKindFinal:
			finals++
		case bamlutils.StreamResultKindError:
			streamErrors++
		}
	}
	if finals != 0 {
		t.Errorf("SoftFinalParse must not soften a non-streaming (NeedsPartials=false) call; got %d finals", finals)
	}
	if streamErrors != 1 {
		t.Errorf("non-streaming raw call must still hard-fail on a final-parse miss; got %d errors", streamErrors)
	}
}

// assertLiveRawPartials checks that exactly len(want) non-reset stream
// (partial) events arrived, each carrying the corresponding per-delta raw
// text and no structured data (parsed == nil), in order.
func assertLiveRawPartials(t *testing.T, results []*testResult, want []string) {
	t.Helper()
	var gotRaw []string
	for _, r := range results {
		if r.kind == bamlutils.StreamResultKindStream && !r.reset {
			gotRaw = append(gotRaw, r.raw)
			if r.stream != nil {
				t.Errorf("raw-only partial should carry nil structured data, got %#v", r.stream)
			}
		}
	}
	if len(gotRaw) != len(want) {
		t.Fatalf("expected %d live raw partials (one per prose delta), got %d: %q",
			len(want), len(gotRaw), gotRaw)
	}
	for i := range want {
		if gotRaw[i] != want[i] {
			t.Errorf("raw partial %d = %q, want %q", i, gotRaw[i], want[i])
		}
	}
}
