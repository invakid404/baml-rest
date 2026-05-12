package buildrequest

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/bamlutils/retry"
)

// testResult implements bamlutils.StreamResult for testing
type testResult struct {
	kind      bamlutils.StreamResultKind
	stream    any
	final     any
	raw       string
	reasoning string
	err       error
	reset     bool
	metadata  *bamlutils.Metadata
}

func (r *testResult) Kind() bamlutils.StreamResultKind { return r.kind }
func (r *testResult) Stream() any                      { return r.stream }
func (r *testResult) Final() any                       { return r.final }
func (r *testResult) Error() error                     { return r.err }
func (r *testResult) Raw() string                      { return r.raw }
func (r *testResult) Reasoning() string                { return r.reasoning }
func (r *testResult) Reset() bool                      { return r.reset }
func (r *testResult) Metadata() *bamlutils.Metadata    { return r.metadata }
func (r *testResult) Release()                         {}

func newTestResult(kind bamlutils.StreamResultKind, stream, final any, raw, reasoning string, err error, reset bool) bamlutils.StreamResult {
	return &testResult{kind: kind, stream: stream, final: final, raw: raw, reasoning: reasoning, err: err, reset: reset}
}

func makeOpenAIServer(chunks []string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		for _, chunk := range chunks {
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"%s\"}}]}\n\n", chunk)
			if flusher != nil {
				flusher.Flush()
			}
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}))
}

func TestRunStreamOrchestration_Success(t *testing.T) {
	server := makeOpenAIServer([]string{"Hello", " world", "!"})
	defer server.Close()

	client := llmhttp.NewClient(server.Client())
	out := make(chan bamlutils.StreamResult, 100)

	config := &StreamConfig{
		Provider:      "openai",
		NeedsPartials: true,
		NeedsRaw:      true,
	}

	err := RunStreamOrchestration(
		context.Background(),
		out,
		config,
		client,
		func(ctx context.Context, clientOverride string) (*llmhttp.Request, error) {
			return &llmhttp.Request{
				URL:    server.URL,
				Method: "POST",
				Body:   `{}`,
			}, nil
		},
		func(ctx context.Context, accumulated string) (any, error) {
			return accumulated, nil // Just return the accumulated text as the "parsed" result
		},
		func(ctx context.Context, accumulated string) (any, error) {
			return accumulated, nil // Final parse
		},
		newTestResult,
	)

	close(out)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var results []*testResult
	for r := range out {
		results = append(results, r.(*testResult))
	}

	// Should have partials + 1 heartbeat + 1 final
	var heartbeats, partials, finals, errors int
	for _, r := range results {
		switch r.kind {
		case bamlutils.StreamResultKindHeartbeat:
			heartbeats++
		case bamlutils.StreamResultKindStream:
			partials++
		case bamlutils.StreamResultKindFinal:
			finals++
		case bamlutils.StreamResultKindError:
			errors++
		}
	}

	if heartbeats != 1 {
		t.Errorf("expected 1 heartbeat, got %d", heartbeats)
	}
	if finals != 1 {
		t.Errorf("expected 1 final, got %d", finals)
	}
	if errors != 0 {
		t.Errorf("expected 0 errors, got %d", errors)
	}
	if partials != 3 {
		t.Errorf("expected 3 partials (one per SSE delta), got %d", partials)
	}

	// The final result should have the full accumulated text
	lastResult := results[len(results)-1]
	if lastResult.kind != bamlutils.StreamResultKindFinal {
		t.Errorf("last result should be final, got kind %d", lastResult.kind)
	}
	if lastResult.raw != "Hello world!" {
		t.Errorf("expected raw 'Hello world!', got %q", lastResult.raw)
	}
}

func TestRunStreamOrchestration_NoPartials(t *testing.T) {
	server := makeOpenAIServer([]string{"Hello", " world"})
	defer server.Close()

	client := llmhttp.NewClient(server.Client())
	out := make(chan bamlutils.StreamResult, 100)

	config := &StreamConfig{
		Provider:      "openai",
		NeedsPartials: false,
		NeedsRaw:      true,
	}

	err := RunStreamOrchestration(
		context.Background(), out, config, client,
		func(ctx context.Context, clientOverride string) (*llmhttp.Request, error) {
			return &llmhttp.Request{URL: server.URL, Method: "POST", Body: `{}`}, nil
		},
		nil, // No parseStream needed
		func(ctx context.Context, accumulated string) (any, error) {
			return accumulated, nil
		},
		newTestResult,
	)
	close(out)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var results []*testResult
	for r := range out {
		results = append(results, r.(*testResult))
	}

	// With NeedsPartials=false and NeedsRaw=true (StreamModeCallWithRaw),
	// no intermediate partials should be emitted. Raw is accumulated and
	// included in the final result only.
	var streamResults, finals int
	var finalRaw string
	for _, r := range results {
		if r.kind == bamlutils.StreamResultKindStream {
			streamResults++
		}
		if r.kind == bamlutils.StreamResultKindFinal {
			finals++
			finalRaw = r.raw
		}
	}

	if streamResults != 0 {
		t.Errorf("expected 0 intermediate stream results in raw-only mode, got %d", streamResults)
	}
	if finals != 1 {
		t.Errorf("expected 1 final result, got %d", finals)
	}
	if finalRaw != "Hello world" {
		t.Errorf("expected final raw 'Hello world', got %q", finalRaw)
	}
}

func TestRunStreamOrchestration_HTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		fmt.Fprint(w, `{"error":"rate limited"}`)
	}))
	defer server.Close()

	client := llmhttp.NewClient(server.Client())
	out := make(chan bamlutils.StreamResult, 100)

	config := &StreamConfig{Provider: "openai", NeedsPartials: true}

	err := RunStreamOrchestration(
		context.Background(), out, config, client,
		func(ctx context.Context, clientOverride string) (*llmhttp.Request, error) {
			return &llmhttp.Request{URL: server.URL, Method: "POST", Body: `{}`}, nil
		},
		func(ctx context.Context, accumulated string) (any, error) { return nil, nil },
		func(ctx context.Context, accumulated string) (any, error) { return nil, nil },
		newTestResult,
	)
	close(out)

	if err != nil {
		t.Fatalf("unexpected error from orchestration itself: %v", err)
	}

	// Error should be emitted on the channel
	var errorResults int
	for r := range out {
		if r.Kind() == bamlutils.StreamResultKindError {
			errorResults++
			if r.Error() == nil {
				t.Error("error result should have non-nil error")
			}
		}
	}
	if errorResults != 1 {
		t.Errorf("expected 1 error result, got %d", errorResults)
	}
}

func TestRunStreamOrchestration_WithRetry(t *testing.T) {
	// Models a partial response that streamed content before failing
	// validation mid-stream — the realistic scenario the retry-boundary
	// reset marker is for. Each failing attempt emits one valid SSE
	// delta (so trySendPartial fires and sawStreamFrame=true) with
	// content "stale"; parseFinal rejects "stale" and the attempt
	// errors out. The third attempt streams "ok" which parseFinal
	// accepts.
	//
	// The server-emits-stale-then-ok shape models "two failing
	// windows that DID emit partials, retried; reset must fire each
	// time" — reset markers only fire when there's actual partial
	// state for the next window to clear, so the test must produce
	// real partials before failing rather than a 500-before-any-byte.
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		flusher, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		if n < 3 {
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"stale\"}}]}\n\n")
			if flusher != nil {
				flusher.Flush()
			}
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	client := llmhttp.NewClient(server.Client())
	out := make(chan bamlutils.StreamResult, 100)

	config := &StreamConfig{
		Provider:      "openai",
		NeedsPartials: true,
		NeedsRaw:      true,
		RetryPolicy: &retry.Policy{
			MaxRetries: 3,
			Strategy:   &retry.ConstantDelay{DelayMs: 1},
		},
	}

	parseFinal := func(_ context.Context, s string) (any, error) {
		if s == "stale" {
			return nil, fmt.Errorf("rejected stale content")
		}
		return s, nil
	}

	err := RunStreamOrchestration(
		context.Background(), out, config, client,
		func(ctx context.Context, clientOverride string) (*llmhttp.Request, error) {
			return &llmhttp.Request{URL: server.URL, Method: "POST", Body: `{}`}, nil
		},
		func(ctx context.Context, accumulated string) (any, error) { return accumulated, nil },
		parseFinal,
		newTestResult,
	)
	close(out)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if attempts.Load() != 3 {
		t.Errorf("expected 3 attempts, got %d", attempts.Load())
	}

	// Should have reset signals from retries — each failing attempt
	// queued a partial frame before erroring out, so sawStreamFrame
	// was true when the retry callback fired.
	var resets, finals, errors, nonResetPartials int
	for r := range out {
		tr := r.(*testResult)
		// Production emits resets specifically as
		// StreamResultKindStream with reset=true (orchestrator.go
		// reset-emission seams). Counting on `tr.reset` alone would
		// match a hypothetical Final/Error result that carried a
		// stale reset bit — gate strictly so a regression that
		// mis-tags a reset can't slip past as a counter increment.
		if tr.kind == bamlutils.StreamResultKindStream && tr.reset {
			resets++
		}
		if tr.kind == bamlutils.StreamResultKindStream && !tr.reset {
			nonResetPartials++
		}
		if tr.kind == bamlutils.StreamResultKindFinal {
			finals++
		}
		if tr.kind == bamlutils.StreamResultKindError {
			errors++
		}
	}

	// Strict equality, not lower-bound. This is a single-provider
	// retry scenario with exactly 3 attempts (n<3 fails, n==3
	// succeeds), so exactly 2 retry boundaries fire. A
	// duplicate-emission regression — reset fired from both the
	// retry callback and another path, or state not cleared between
	// boundaries — would slip past `resets >= 2` but fail strict
	// equality. Aligns with the integration sibling
	// (orchestrator_integration_test.go:440-448) which also asserts
	// exactly 2.
	if resets != 2 {
		t.Errorf("expected 2 reset signals (one per retry boundary), got %d", resets)
	}
	if finals != 1 {
		t.Errorf("expected 1 final, got %d", finals)
	}
	// RunStreamOrchestration emits StreamResultKindError only after
	// retry.Execute returns an error (whole plan exhausted). A
	// successful retry path that also emits Error frames is a
	// contract regression — error and final should be mutually
	// exclusive on a single attempt's outcome.
	if errors != 0 {
		t.Errorf("expected 0 error frames on a recovered retry; got %d", errors)
	}
	// Without this counter, a regression that emits resets but no real
	// partials (e.g. all queued frames mis-tagged or dropped before
	// dispatch) would silently pass the resets/finals/errors trio.
	// testResult is the local stream sink type; tr.kind is its result
	// classification, tr.reset is the reset bit, and the comparison is
	// against bamlutils.StreamResultKindStream (the non-reset partial
	// shape).
	if nonResetPartials != 3 {
		t.Errorf("expected 3 non-reset partials (one per attempt's delta — 2x 'stale' + 1x 'ok'), got %d", nonResetPartials)
	}
}

func TestRunStreamOrchestration_ContextCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		flusher, ok := w.(http.Flusher)
		if !ok {
			return
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"first\"}}]}\n\n")
		flusher.Flush()
		// Block until client disconnects
		<-r.Context().Done()
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	client := llmhttp.NewClient(server.Client())
	out := make(chan bamlutils.StreamResult, 100)

	config := &StreamConfig{Provider: "openai", NeedsPartials: true}

	RunStreamOrchestration(
		ctx, out, config, client,
		func(ctx context.Context, clientOverride string) (*llmhttp.Request, error) {
			return &llmhttp.Request{URL: server.URL, Method: "POST", Body: `{}`}, nil
		},
		func(ctx context.Context, accumulated string) (any, error) { return accumulated, nil },
		func(ctx context.Context, accumulated string) (any, error) { return accumulated, nil },
		newTestResult,
	)
	close(out)

	// Should have gotten at least a heartbeat and possibly a partial before timeout
	var count int
	for range out {
		count++
	}
	if count == 0 {
		t.Error("expected at least some results before context cancellation")
	}
}

func TestParseBuildRequestEnv(t *testing.T) {
	tests := []struct {
		env      string
		expected bool
	}{
		{"", false},
		{"0", false},
		{"false", false},
		{"no", false},
		{"1", true},
		{"true", true},
		{"yes", true},
		{"on", true},
		{"TRUE", true},
		{"True", true},
	}

	for _, tt := range tests {
		t.Run(tt.env, func(t *testing.T) {
			os.Setenv("BAML_REST_USE_BUILD_REQUEST", tt.env)
			defer os.Unsetenv("BAML_REST_USE_BUILD_REQUEST")

			if got := parseBuildRequestEnv(); got != tt.expected {
				t.Errorf("parseBuildRequestEnv() with env=%q: got %v, want %v", tt.env, got, tt.expected)
			}
		})
	}
}

func TestIsProviderSupported(t *testing.T) {
	supported := []string{
		"openai", "openai-generic", "azure-openai", "anthropic",
		"google-ai", "vertex-ai", "ollama", "openrouter",
		"openai-responses",
		// aws-bedrock streaming — see supportedProviders doc-comment
		// for the dispatch contract.
		"aws-bedrock",
	}
	for _, p := range supported {
		if !IsProviderSupported(p) {
			t.Errorf("expected provider %q to be supported", p)
		}
	}

	// Future/unknown providers stay on the legacy path.
	unsupported := []string{"some-future-provider", "custom-provider", ""}
	for _, p := range unsupported {
		if IsProviderSupported(p) {
			t.Errorf("expected provider %q to be unsupported", p)
		}
	}
}

func TestRunStreamOrchestration_Anthropic(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\"}\n\n")
		fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"Hello\"}}\n\n")
		fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\" world\"}}\n\n")
		fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer server.Close()

	client := llmhttp.NewClient(server.Client())
	out := make(chan bamlutils.StreamResult, 100)

	config := &StreamConfig{
		Provider:      "anthropic",
		NeedsPartials: true,
		NeedsRaw:      true,
	}

	err := RunStreamOrchestration(
		context.Background(), out, config, client,
		func(ctx context.Context, clientOverride string) (*llmhttp.Request, error) {
			return &llmhttp.Request{URL: server.URL, Method: "POST", Body: `{}`}, nil
		},
		func(ctx context.Context, accumulated string) (any, error) { return accumulated, nil },
		func(ctx context.Context, accumulated string) (any, error) { return accumulated, nil },
		newTestResult,
	)
	close(out)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var finalResult *testResult
	for r := range out {
		tr := r.(*testResult)
		if tr.kind == bamlutils.StreamResultKindFinal {
			finalResult = tr
		}
	}

	if finalResult == nil {
		t.Fatal("expected a final result")
	}
	if finalResult.raw != "Hello world" {
		t.Errorf("expected raw 'Hello world', got %q", finalResult.raw)
	}
}

func TestRunStreamOrchestration_AnthropicThinkingUsesAnswerOnlyParsing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\"}\n\n")
		fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"Step 1: reason...\"}}\n\n")
		fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"The answer\"}}\n\n")
		fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\" Step 2: refine...\"}}\n\n")
		fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\" is 42\"}}\n\n")
		fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer server.Close()

	client := llmhttp.NewClient(server.Client())
	out := make(chan bamlutils.StreamResult, 100)

	config := &StreamConfig{
		Provider:      "anthropic",
		NeedsPartials: true,
		NeedsRaw:      false,
	}

	var parseStreamInputs []string
	var parseFinalInput string
	err := RunStreamOrchestration(
		context.Background(), out, config, client,
		func(ctx context.Context, clientOverride string) (*llmhttp.Request, error) {
			return &llmhttp.Request{URL: server.URL, Method: "POST", Body: `{}`}, nil
		},
		func(ctx context.Context, accumulated string) (any, error) {
			parseStreamInputs = append(parseStreamInputs, accumulated)
			return accumulated, nil
		},
		func(ctx context.Context, accumulated string) (any, error) {
			parseFinalInput = accumulated
			return accumulated, nil
		},
		newTestResult,
	)
	close(out)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got, want := parseStreamInputs, []string{"The answer", "The answer is 42"}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("parseStream inputs = %v, want %v", got, want)
	}
	if parseFinalInput != "The answer is 42" {
		t.Fatalf("parseFinal input = %q, want %q", parseFinalInput, "The answer is 42")
	}

	var partials, finals int
	for r := range out {
		tr := r.(*testResult)
		if tr.kind == bamlutils.StreamResultKindStream {
			partials++
			if tr.raw != "" {
				t.Fatalf("expected no raw partial output in non-raw mode, got %q", tr.raw)
			}
		}
		if tr.kind == bamlutils.StreamResultKindFinal {
			finals++
			if tr.final != "The answer is 42" {
				t.Fatalf("final = %v, want %q", tr.final, "The answer is 42")
			}
			if tr.raw != "" {
				t.Fatalf("expected empty final raw in non-raw mode, got %q", tr.raw)
			}
		}
	}

	if partials != 2 {
		t.Fatalf("expected 2 parsed partials (text deltas only), got %d", partials)
	}
	if finals != 1 {
		t.Fatalf("expected 1 final, got %d", finals)
	}
}

func TestRunStreamOrchestration_AnthropicThinkingDropsThinkingFromRaw_Default(t *testing.T) {
	// Default (IncludeReasoning=false, BAML-aligned): thinking_delta events
	// produce no Stream-kind frame and never reach raw or reasoning. Partials
	// count is 2 (text deltas only), and final raw equals parseable. The
	// reasoning channel is empty on every frame.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\"}\n\n")
		fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"Step 1: reason...\"}}\n\n")
		fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"The answer\"}}\n\n")
		fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\" Step 2: refine...\"}}\n\n")
		fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\" is 42\"}}\n\n")
		fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer server.Close()

	client := llmhttp.NewClient(server.Client())
	out := make(chan bamlutils.StreamResult, 100)

	config := &StreamConfig{
		Provider:         "anthropic",
		NeedsPartials:    true,
		NeedsRaw:         true,
		IncludeReasoning: false,
	}

	var parseStreamInputs []string
	var parseFinalInput string
	err := RunStreamOrchestration(
		context.Background(), out, config, client,
		func(ctx context.Context, clientOverride string) (*llmhttp.Request, error) {
			return &llmhttp.Request{URL: server.URL, Method: "POST", Body: `{}`}, nil
		},
		func(ctx context.Context, accumulated string) (any, error) {
			parseStreamInputs = append(parseStreamInputs, accumulated)
			return accumulated, nil
		},
		func(ctx context.Context, accumulated string) (any, error) {
			parseFinalInput = accumulated
			return accumulated, nil
		},
		newTestResult,
	)
	close(out)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got, want := parseStreamInputs, []string{"The answer", "The answer is 42"}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("parseStream inputs = %v, want %v", got, want)
	}
	if parseFinalInput != "The answer is 42" {
		t.Fatalf("parseFinal input = %q, want %q", parseFinalInput, "The answer is 42")
	}

	var partialRaws []string
	var partialReasonings []string
	var partialStreams []any
	var finalResult *testResult
	for r := range out {
		tr := r.(*testResult)
		if tr.kind == bamlutils.StreamResultKindStream {
			partialRaws = append(partialRaws, tr.raw)
			partialReasonings = append(partialReasonings, tr.reasoning)
			partialStreams = append(partialStreams, tr.stream)
		}
		if tr.kind == bamlutils.StreamResultKindFinal {
			finalResult = tr
		}
	}

	if want := []string{"The answer", " is 42"}; len(partialRaws) != len(want) || partialRaws[0] != want[0] || partialRaws[1] != want[1] {
		t.Fatalf("partial raws = %v, want %v", partialRaws, want)
	}
	for i, reasoning := range partialReasonings {
		if reasoning != "" {
			t.Errorf("partial reasoning[%d] = %q, want empty (flag off)", i, reasoning)
		}
	}
	if partialStreams[0] != "The answer" || partialStreams[1] != "The answer is 42" {
		t.Fatalf("text partial streams = %v", partialStreams)
	}
	if finalResult == nil {
		t.Fatal("expected a final result")
	}
	if finalResult.final != "The answer is 42" {
		t.Fatalf("final = %v, want %q", finalResult.final, "The answer is 42")
	}
	if finalResult.raw != "The answer is 42" {
		t.Fatalf("final raw = %q (thinking should be dropped under default flag)", finalResult.raw)
	}
	if finalResult.reasoning != "" {
		t.Fatalf("final reasoning = %q, want empty (flag off)", finalResult.reasoning)
	}
}

func TestRunStreamOrchestration_AnthropicThinkingPreservesRawStream_OptIn(t *testing.T) {
	// Opt-in (IncludeReasoning=true): thinking_delta events populate the
	// reasoning channel via dedicated reasoning-only Stream frames. Parseable
	// and raw still exclude thinking content (raw stays text-only by
	// construction).
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\"}\n\n")
		fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"Step 1: reason...\"}}\n\n")
		fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"The answer\"}}\n\n")
		fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\" Step 2: refine...\"}}\n\n")
		fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\" is 42\"}}\n\n")
		fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer server.Close()

	client := llmhttp.NewClient(server.Client())
	out := make(chan bamlutils.StreamResult, 100)

	config := &StreamConfig{
		Provider:         "anthropic",
		NeedsPartials:    true,
		NeedsRaw:         true,
		IncludeReasoning: true,
	}

	var parseStreamInputs []string
	var parseFinalInput string
	err := RunStreamOrchestration(
		context.Background(), out, config, client,
		func(ctx context.Context, clientOverride string) (*llmhttp.Request, error) {
			return &llmhttp.Request{URL: server.URL, Method: "POST", Body: `{}`}, nil
		},
		func(ctx context.Context, accumulated string) (any, error) {
			parseStreamInputs = append(parseStreamInputs, accumulated)
			return accumulated, nil
		},
		func(ctx context.Context, accumulated string) (any, error) {
			parseFinalInput = accumulated
			return accumulated, nil
		},
		newTestResult,
	)
	close(out)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got, want := parseStreamInputs, []string{"The answer", "The answer is 42"}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("parseStream inputs = %v, want %v", got, want)
	}
	if parseFinalInput != "The answer is 42" {
		t.Fatalf("parseFinal input = %q, want %q", parseFinalInput, "The answer is 42")
	}

	var partialRaws []string
	var partialReasonings []string
	var partialStreams []any
	var finalResult *testResult
	for r := range out {
		tr := r.(*testResult)
		if tr.kind == bamlutils.StreamResultKindStream {
			partialRaws = append(partialRaws, tr.raw)
			partialReasonings = append(partialReasonings, tr.reasoning)
			partialStreams = append(partialStreams, tr.stream)
		}
		if tr.kind == bamlutils.StreamResultKindFinal {
			finalResult = tr
		}
	}

	// Per-event order: thinking, text, thinking, text. Thinking events
	// carry only reasoning (raw is empty, stream is nil); text events
	// carry only raw + parsed stream (reasoning is empty).
	if want := []string{"", "The answer", "", " is 42"}; len(partialRaws) != len(want) || partialRaws[0] != want[0] || partialRaws[1] != want[1] || partialRaws[2] != want[2] || partialRaws[3] != want[3] {
		t.Fatalf("partial raws = %v, want %v", partialRaws, want)
	}
	if want := []string{"Step 1: reason...", "", " Step 2: refine...", ""}; len(partialReasonings) != len(want) || partialReasonings[0] != want[0] || partialReasonings[1] != want[1] || partialReasonings[2] != want[2] || partialReasonings[3] != want[3] {
		t.Fatalf("partial reasonings = %v, want %v", partialReasonings, want)
	}
	if partialStreams[0] != nil || partialStreams[2] != nil {
		t.Fatalf("reasoning partials should be raw-only/reasoning-only, got streams %v", partialStreams)
	}
	if partialStreams[1] != "The answer" || partialStreams[3] != "The answer is 42" {
		t.Fatalf("text partial streams = %v", partialStreams)
	}
	if finalResult == nil {
		t.Fatal("expected a final result")
	}
	if finalResult.final != "The answer is 42" {
		t.Fatalf("final = %v, want %q", finalResult.final, "The answer is 42")
	}
	if finalResult.raw != "The answer is 42" {
		t.Fatalf("final raw = %q (raw is text-only regardless of flag)", finalResult.raw)
	}
	if finalResult.reasoning != "Step 1: reason... Step 2: refine..." {
		t.Fatalf("final reasoning = %q", finalResult.reasoning)
	}
}

func TestRunStreamOrchestration_ParseThrottle(t *testing.T) {
	// Generate many small chunks
	chunks := make([]string, 20)
	for i := range chunks {
		chunks[i] = "x"
	}
	server := makeOpenAIServer(chunks)
	defer server.Close()

	client := llmhttp.NewClient(server.Client())
	out := make(chan bamlutils.StreamResult, 200)

	parseCount := 0
	config := &StreamConfig{
		Provider:              "openai",
		NeedsPartials:         true,
		NeedsRaw:              false,
		ParseThrottleInterval: 50 * time.Millisecond,
	}

	err := RunStreamOrchestration(
		context.Background(), out, config, client,
		func(ctx context.Context, clientOverride string) (*llmhttp.Request, error) {
			return &llmhttp.Request{URL: server.URL, Method: "POST", Body: `{}`}, nil
		},
		func(ctx context.Context, accumulated string) (any, error) {
			parseCount++
			return accumulated, nil
		},
		func(ctx context.Context, accumulated string) (any, error) { return accumulated, nil },
		newTestResult,
	)
	if err != nil {
		t.Fatalf("unexpected orchestration error: %v", err)
	}
	close(out)

	// With throttling, parse should be called fewer times than total chunks
	// (At minimum once for the first event, then throttled)
	if parseCount >= 20 {
		t.Errorf("expected throttled parse calls (< 20), got %d", parseCount)
	}
}

func TestRunStreamOrchestration_ValidatesAllFallbackChildrenUpFront(t *testing.T) {
	out := make(chan bamlutils.StreamResult, 10)

	err := RunStreamOrchestration(
		context.Background(), out,
		&StreamConfig{
			FallbackChain: []string{"PrimaryClient", "MissingClient"},
			ClientProviders: map[string]string{
				"PrimaryClient": "openai",
			},
		},
		nil,
		func(ctx context.Context, clientOverride string) (*llmhttp.Request, error) {
			t.Fatal("buildRequest should not be called when fallback validation fails")
			return nil, nil
		},
		nil,
		nil,
		newTestResult,
	)

	if err == nil {
		t.Fatal("expected validation error for unsupported or missing fallback child provider")
	}
	if want := `for child "MissingClient"`; !strings.Contains(err.Error(), want) {
		t.Fatalf("expected error containing %q, got %q", want, err.Error())
	}

	close(out)
	// With no MetadataPlan / NewMetadataResult on the config the
	// upfront planned-metadata emit is a no-op (the nil-config
	// short-circuit). So this branch pins "no stream output on
	// validation error when metadata isn't wired up". The metadata-
	// wired-up case is covered by
	// TestRunStreamOrchestration_EmitsPlannedMetadataBeforeValidationError
	// below.
	for r := range out {
		t.Fatalf("expected no stream results on upfront validation error, got kind %v", r.Kind())
	}
}

// TestRunStreamOrchestration_EmitsPlannedMetadataBeforeValidationError
// pins that when the orchestrator config supplies MetadataPlan +
// NewMetadataResult, the planned-metadata event MUST be emitted
// before any validation return. Otherwise the only observable signal
// on exactly the failure modes operators most need to diagnose
// (unsupported provider, malformed fallback chain) gets dropped.
func TestRunStreamOrchestration_EmitsPlannedMetadataBeforeValidationError(t *testing.T) {
	out := make(chan bamlutils.StreamResult, 10)

	plan := &bamlutils.Metadata{
		Path:       "buildrequest",
		Client:     "PrimaryClient",
		PathReason: "",
	}

	err := RunStreamOrchestration(
		context.Background(), out,
		&StreamConfig{
			FallbackChain: []string{"PrimaryClient", "MissingClient"},
			ClientProviders: map[string]string{
				"PrimaryClient": "openai",
			},
			MetadataPlan:      plan,
			NewMetadataResult: newTestMetadataResult,
		},
		nil,
		func(ctx context.Context, clientOverride string) (*llmhttp.Request, error) {
			t.Fatal("buildRequest should not be called when fallback validation fails")
			return nil, nil
		},
		nil,
		nil,
		newTestResult,
	)

	if err == nil {
		t.Fatal("expected validation error for unsupported or missing fallback child provider")
	}

	close(out)
	planned, outcome, kinds, metadataPhases := collectMetadata(t, out)
	if planned == nil {
		t.Fatalf("expected one planned metadata result before validation error; got kinds=%v", kinds)
	}
	if outcome != nil {
		t.Errorf("expected no outcome metadata on validation-error path, got %+v", outcome)
	}
	if planned.Phase != bamlutils.MetadataPhasePlanned {
		t.Errorf("planned phase: got %q, want planned", planned.Phase)
	}
	if planned.Client != "PrimaryClient" {
		t.Errorf("planned client: got %q, want PrimaryClient", planned.Client)
	}
	// Pin "exactly one frame, and it's the planned metadata".
	// Allowing any number of trailing frames after the planned event
	// (so long as outcome stayed nil) would let a regression that
	// emitted spurious reset/error/data frames after a synchronous
	// validation return (e.g. a reordering bug that lost the
	// early-return) slip through. Strict frame count + kind + phase
	// checks pin the full validation-error contract.
	if len(kinds) != 1 {
		t.Errorf("expected exactly 1 emitted frame on validation-error path, got %d (kinds=%v)", len(kinds), kinds)
	}
	if len(kinds) >= 1 && kinds[0] != bamlutils.StreamResultKindMetadata {
		t.Errorf("expected sole frame to be metadata; got kind=%v", kinds[0])
	}
	if len(metadataPhases) != 1 || metadataPhases[0] != bamlutils.MetadataPhasePlanned {
		t.Errorf("expected exactly one planned metadata phase; got %v", metadataPhases)
	}
}

func TestRunStreamOrchestration_FallbackChainResetBetweenChildren(t *testing.T) {
	// Primary streams partial data ("stale") and completes normally, but the
	// child still fails because parseFinal rejects that final content.
	// Secondary streams "good" data and completes normally. The test verifies that:
	// 1. A reset signal is emitted between children so downstream
	//    discards the primary's stale partial state.
	// 2. The final output contains only the secondary's data.
	var attempts atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempt := int(attempts.Add(1))
		flusher, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)

		if attempt == 1 {
			// Primary: stream valid chunks that accumulate to "stale",
			// then complete normally. parseFinal rejects "stale" content,
			// causing tryOneStreamChild to return an error after having
			// emitted partial events — exercising the inter-child reset.
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"stale\"}}]}\n\n")
			if flusher != nil {
				flusher.Flush()
			}
			fmt.Fprint(w, "data: [DONE]\n\n")
			if flusher != nil {
				flusher.Flush()
			}
			return
		}
		// Secondary: normal stream that completes.
		for _, chunk := range []string{"good", " data"} {
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"%s\"}}]}\n\n", chunk)
			if flusher != nil {
				flusher.Flush()
			}
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer server.Close()

	client := llmhttp.NewClient(server.Client())
	out := make(chan bamlutils.StreamResult, 100)

	config := &StreamConfig{
		Provider:      "",
		RetryPolicy:   &retry.Policy{MaxRetries: 1, Strategy: &retry.ConstantDelay{DelayMs: 1}},
		NeedsPartials: true,
		FallbackChain: []string{"PrimaryClient", "SecondaryClient"},
		ClientProviders: map[string]string{
			"PrimaryClient":   "openai",
			"SecondaryClient": "openai",
		},
	}

	// Capture the clientOverride sequence so we can assert the
	// fallback-handoff actually targeted SecondaryClient on the second
	// attempt rather than re-running PrimaryClient. The HTTP server keys
	// solely off request count, and buildFn ignores the override, so a
	// regression that retried the primary on the second attempt would
	// still satisfy the reset/final assertions below.
	var (
		overrideMu  sync.Mutex
		overrideSeq []string
	)
	buildFn := func(ctx context.Context, clientOverride string) (*llmhttp.Request, error) {
		overrideMu.Lock()
		overrideSeq = append(overrideSeq, clientOverride)
		overrideMu.Unlock()
		return &llmhttp.Request{URL: server.URL, Method: "POST", Body: `{}`}, nil
	}

	// parseFinal rejects "stale" content so the primary child fails after
	// streaming partial events, forcing the orchestrator to advance to
	// the secondary child.
	parseFinal := func(_ context.Context, s string) (any, error) {
		if s == "stale" {
			return nil, fmt.Errorf("rejected stale content")
		}
		return s, nil
	}

	err := RunStreamOrchestration(
		context.Background(), out, config, client,
		buildFn,
		func(_ context.Context, s string) (any, error) { return s, nil },
		parseFinal,
		newTestResult,
	)
	close(out)

	// Drain results
	var results []*testResult
	for r := range out {
		results = append(results, r.(*testResult))
	}

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	overrideMu.Lock()
	gotOverrides := append([]string(nil), overrideSeq...)
	overrideMu.Unlock()
	wantOverrides := []string{"PrimaryClient", "SecondaryClient"}
	if !reflect.DeepEqual(gotOverrides, wantOverrides) {
		t.Errorf("clientOverride sequence: got %v, want %v (regression: fallback handoff did not switch children)", gotOverrides, wantOverrides)
	}

	// Count reset signals — the in-chain handoff path has exactly one
	// emission site (orchestrator.go:2185-2223), and the retry-boundary
	// reset path (orchestrator.go:2265-2292) is not reached because the
	// full attempt succeeds. So a healthy run has resets == 1; a
	// duplicate reset is a real regression and a missing reset means
	// the secondary's frames stack onto the primary's stale buffer.
	resets := 0
	sawStalePartial := false
	errorFrames := 0
	finalVal := ""
	for _, r := range results {
		if r.kind == bamlutils.StreamResultKindStream && r.stream == "stale" {
			sawStalePartial = true
		}
		if r.kind == bamlutils.StreamResultKindStream && r.reset {
			resets++
		}
		if r.kind == bamlutils.StreamResultKindError {
			errorFrames++
		}
		if r.kind == bamlutils.StreamResultKindFinal {
			finalVal, _ = r.final.(string)
		}
	}

	// Fast-fail if the primary never emitted the stale partial. The
	// reset assertion below is guarded on sawStalePartial; without
	// this fast-fail, a regression that stops the primary from
	// emitting any partial at all would silently skip the reset
	// assertion and give a false green.
	if !sawStalePartial {
		t.Fatal("primary never emitted 'stale' partial to the channel; the reset-between-children invariant cannot be exercised")
	}
	if sawStalePartial && resets != 1 {
		t.Errorf("expected exactly 1 reset signal between primary and secondary; got %d", resets)
	}
	// RunStreamOrchestration emits StreamResultKindError only after
	// retry.Execute returns an error (whole plan exhausted). A
	// successful fallback-handoff path that also emits an Error frame
	// is a contract regression — the reset/final assertions wouldn't
	// catch it on their own.
	if errorFrames != 0 {
		t.Errorf("expected 0 error frames on a recovered fallback handoff; got %d", errorFrames)
	}
	if finalVal != "good data" {
		t.Errorf("expected final='good data', got %q", finalVal)
	}
}

// TestRunStreamOrchestration_MixedChain_LegacySucceedsSecond covers the
// typical mixed-mode flow: a BuildRequest-driven child fails, then a
// legacy child wins via the LegacyStreamChild callback. The orchestrator
// must emit exactly one final (the legacy child's), reset state between
// children, and not emit any legacy-side partials.
func TestRunStreamOrchestration_MixedChain_LegacySucceedsSecond(t *testing.T) {
	// Supported child: streams valid chunks but parseFinal rejects, so the
	// child fails and the orchestrator advances to the legacy child.
	server := makeOpenAIServer([]string{"stale"})
	defer server.Close()

	client := llmhttp.NewClient(server.Client())
	out := make(chan bamlutils.StreamResult, 100)

	var legacyCalled atomic.Int32
	var gotOverride string
	var gotProvider string
	var gotNeedsRaw bool
	legacyStreamChild := func(ctx context.Context, clientOverride, provider string, needsRaw bool, sendHeartbeat func()) (any, string, string, error) {
		legacyCalled.Add(1)
		gotOverride = clientOverride
		gotProvider = provider
		gotNeedsRaw = needsRaw
		sendHeartbeat()
		return "legacy ok", "legacy raw", "", nil
	}

	config := &StreamConfig{
		RetryPolicy:   &retry.Policy{MaxRetries: 0},
		NeedsPartials: true,
		NeedsRaw:      true,
		FallbackChain: []string{"SupportedChild", "LegacyChild"},
		ClientProviders: map[string]string{
			"SupportedChild": "openai",
			"LegacyChild":    "aws-bedrock",
		},
		LegacyChildren:    map[string]bool{"LegacyChild": true},
		LegacyStreamChild: legacyStreamChild,
	}

	parseFinal := func(_ context.Context, s string) (any, error) {
		return nil, fmt.Errorf("supported child rejected")
	}
	err := RunStreamOrchestration(
		context.Background(), out, config, client,
		func(ctx context.Context, clientOverride string) (*llmhttp.Request, error) {
			return &llmhttp.Request{URL: server.URL, Method: "POST", Body: `{}`}, nil
		},
		func(_ context.Context, s string) (any, error) { return s, nil },
		parseFinal,
		newTestResult,
	)
	close(out)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if legacyCalled.Load() != 1 {
		t.Fatalf("expected legacy callback invoked once, got %d", legacyCalled.Load())
	}
	if gotOverride != "LegacyChild" {
		t.Errorf("expected clientOverride 'LegacyChild', got %q", gotOverride)
	}
	if gotProvider != "aws-bedrock" {
		t.Errorf("expected provider 'aws-bedrock', got %q", gotProvider)
	}
	if !gotNeedsRaw {
		t.Errorf("expected needsRaw=true, got false")
	}

	var resets, finals, errors int
	var finalVal any
	var finalRaw string
	for r := range out {
		tr := r.(*testResult)
		if tr.kind == bamlutils.StreamResultKindStream && tr.reset {
			resets++
		}
		if tr.kind == bamlutils.StreamResultKindFinal {
			finals++
			finalVal = tr.final
			finalRaw = tr.raw
		}
		if tr.kind == bamlutils.StreamResultKindError {
			errors++
		}
	}
	if resets != 1 {
		t.Errorf("expected exactly 1 reset signal between children, got %d", resets)
	}
	if finals != 1 {
		t.Fatalf("expected exactly one final, got %d", finals)
	}
	if finalVal != "legacy ok" {
		t.Errorf("expected final='legacy ok', got %v", finalVal)
	}
	if finalRaw != "legacy raw" {
		t.Errorf("expected raw='legacy raw', got %q", finalRaw)
	}
	// RunStreamOrchestration emits StreamResultKindError only after
	// retry.Execute returns an error (whole plan exhausted). A
	// successful legacy-second handoff that also emits an Error
	// frame is a contract regression.
	if errors != 0 {
		t.Errorf("expected 0 error frames on a recovered legacy-second handoff; got %d", errors)
	}
}

// TestRunStreamOrchestration_MixedChain_LegacyFirstFails_SupportedWins
// ensures a legacy-first-fails path lets the supported child take over
// and that the orchestrator emits partials from the supported child only.
func TestRunStreamOrchestration_MixedChain_LegacyFirstFails_SupportedWins(t *testing.T) {
	server := makeOpenAIServer([]string{"good"})
	defer server.Close()

	client := llmhttp.NewClient(server.Client())
	out := make(chan bamlutils.StreamResult, 100)

	var legacyCalled atomic.Int32
	legacyStreamChild := func(ctx context.Context, clientOverride, provider string, needsRaw bool, sendHeartbeat func()) (any, string, string, error) {
		legacyCalled.Add(1)
		return nil, "", "", fmt.Errorf("legacy upstream 500")
	}

	config := &StreamConfig{
		RetryPolicy:   &retry.Policy{MaxRetries: 0},
		NeedsPartials: true,
		FallbackChain: []string{"LegacyChild", "SupportedChild"},
		ClientProviders: map[string]string{
			"LegacyChild":    "aws-bedrock",
			"SupportedChild": "openai",
		},
		LegacyChildren:    map[string]bool{"LegacyChild": true},
		LegacyStreamChild: legacyStreamChild,
	}

	err := RunStreamOrchestration(
		context.Background(), out, config, client,
		func(ctx context.Context, clientOverride string) (*llmhttp.Request, error) {
			return &llmhttp.Request{URL: server.URL, Method: "POST", Body: `{}`}, nil
		},
		func(_ context.Context, s string) (any, error) { return s, nil },
		func(_ context.Context, s string) (any, error) { return s, nil },
		newTestResult,
	)
	close(out)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if legacyCalled.Load() != 1 {
		t.Fatalf("expected legacy callback invoked once, got %d", legacyCalled.Load())
	}

	var partials, finals, errors int
	var finalVal any
	for r := range out {
		tr := r.(*testResult)
		switch tr.kind {
		case bamlutils.StreamResultKindStream:
			if !tr.reset {
				partials++
			}
		case bamlutils.StreamResultKindFinal:
			finals++
			finalVal = tr.final
		case bamlutils.StreamResultKindError:
			errors++
		}
	}
	if partials != 1 {
		t.Errorf("expected exactly 1 partial from the supported child, got %d", partials)
	}
	if finals != 1 {
		t.Fatalf("expected exactly one final, got %d", finals)
	}
	// RunStreamOrchestration emits StreamResultKindError only after
	// retry.Execute returns an error (whole plan exhausted). A
	// successful legacy-first-fails handoff that also emits an Error
	// frame is a contract regression — mirrors the sibling assertion
	// in TestRunStreamOrchestration_MixedChain_SupportedFirstFails_LegacyWins.
	if errors != 0 {
		t.Errorf("expected 0 error frames on a recovered legacy-first-fails handoff; got %d", errors)
	}
	if finalVal != "good" {
		t.Errorf("expected final='good' from supported child, got %v", finalVal)
	}
}

// TestRunStreamOrchestration_MixedChain_LegacyNoCallbackErrors verifies
// up-front validation: if LegacyChildren is non-empty but
// LegacyStreamChild is nil, the orchestrator returns immediately and
// never calls buildRequest.
func TestRunStreamOrchestration_MixedChain_LegacyNoCallbackErrors(t *testing.T) {
	out := make(chan bamlutils.StreamResult, 10)
	err := RunStreamOrchestration(
		context.Background(), out,
		&StreamConfig{
			FallbackChain: []string{"SupportedChild", "LegacyChild"},
			ClientProviders: map[string]string{
				"SupportedChild": "openai",
				"LegacyChild":    "aws-bedrock",
			},
			LegacyChildren: map[string]bool{"LegacyChild": true},
		},
		nil,
		func(ctx context.Context, clientOverride string) (*llmhttp.Request, error) {
			t.Fatal("buildRequest must not be called when LegacyStreamChild is missing")
			return nil, nil
		},
		nil, nil,
		newTestResult,
	)
	if err == nil || !strings.Contains(err.Error(), "LegacyStreamChild") {
		t.Fatalf("expected LegacyStreamChild validation error, got %v", err)
	}
	close(out)
	for r := range out {
		t.Fatalf("expected no results on validation failure, got kind %v", r.Kind())
	}
}

// TestRunStreamOrchestration_MixedChain_LegacyHeartbeat verifies that the
// orchestrator emits a heartbeat when the legacy callback calls
// sendHeartbeat, and only emits it once even if the callback calls it
// multiple times. Also verifies that a legacy callback which never calls
// sendHeartbeat (simulating a fail-before-first-byte upstream) produces
// no heartbeat.
func TestRunStreamOrchestration_MixedChain_LegacyHeartbeat(t *testing.T) {
	t.Run("callback fires heartbeat", func(t *testing.T) {
		out := make(chan bamlutils.StreamResult, 100)
		config := &StreamConfig{
			FallbackChain: []string{"LegacyChild"},
			ClientProviders: map[string]string{
				"LegacyChild": "aws-bedrock",
			},
			LegacyChildren: map[string]bool{"LegacyChild": true},
			LegacyStreamChild: func(ctx context.Context, clientOverride, provider string, needsRaw bool, sendHeartbeat func()) (any, string, string, error) {
				// Call sendHeartbeat multiple times to verify idempotency.
				sendHeartbeat()
				sendHeartbeat()
				sendHeartbeat()
				return "ok", "", "", nil
			},
		}
		err := RunStreamOrchestration(
			context.Background(), out, config, nil,
			func(ctx context.Context, clientOverride string) (*llmhttp.Request, error) {
				t.Fatal("buildRequest must not be called for legacy-only chain")
				return nil, nil
			},
			nil, nil,
			newTestResult,
		)
		close(out)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		var heartbeats, finals int
		for r := range out {
			switch r.Kind() {
			case bamlutils.StreamResultKindHeartbeat:
				heartbeats++
			case bamlutils.StreamResultKindFinal:
				finals++
			}
		}
		if heartbeats != 1 {
			t.Errorf("expected exactly 1 heartbeat (idempotent), got %d", heartbeats)
		}
		if finals != 1 {
			t.Errorf("expected 1 final, got %d", finals)
		}
	})

	t.Run("callback never fires heartbeat", func(t *testing.T) {
		out := make(chan bamlutils.StreamResult, 100)
		config := &StreamConfig{
			RetryPolicy:   &retry.Policy{MaxRetries: 0},
			FallbackChain: []string{"LegacyChild"},
			ClientProviders: map[string]string{
				"LegacyChild": "aws-bedrock",
			},
			LegacyChildren: map[string]bool{"LegacyChild": true},
			LegacyStreamChild: func(ctx context.Context, clientOverride, provider string, needsRaw bool, sendHeartbeat func()) (any, string, string, error) {
				// Simulate failure before any upstream byte — callback
				// never fires the heartbeat. Pool hung-detection should
				// still be able to kill this request if it hangs long
				// enough; here we just assert no heartbeat leaked.
				return nil, "", "", fmt.Errorf("upstream 4xx")
			},
		}
		err := RunStreamOrchestration(
			context.Background(), out, config, nil,
			func(ctx context.Context, clientOverride string) (*llmhttp.Request, error) {
				return nil, nil
			},
			nil, nil,
			newTestResult,
		)
		close(out)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		var heartbeats int
		for r := range out {
			if r.Kind() == bamlutils.StreamResultKindHeartbeat {
				heartbeats++
			}
		}
		if heartbeats != 0 {
			t.Errorf("expected 0 heartbeats when callback never calls sendHeartbeat, got %d", heartbeats)
		}
	})
}

// TestRunStreamOrchestration_MixedChain_LegacyRawPropagation verifies
// that the raw string returned from a winning legacy callback is
// propagated to the final emission only when NeedsRaw is true.
func TestRunStreamOrchestration_MixedChain_LegacyRawPropagation(t *testing.T) {
	legacyChildFn := func(raw string) LegacyStreamChildFunc {
		return func(ctx context.Context, clientOverride, provider string, needsRaw bool, sendHeartbeat func()) (any, string, string, error) {
			sendHeartbeat()
			return "final", raw, "", nil
		}
	}

	t.Run("NeedsRaw true", func(t *testing.T) {
		out := make(chan bamlutils.StreamResult, 100)
		config := &StreamConfig{
			NeedsRaw:      true,
			FallbackChain: []string{"LegacyChild"},
			ClientProviders: map[string]string{
				"LegacyChild": "aws-bedrock",
			},
			LegacyChildren:    map[string]bool{"LegacyChild": true},
			LegacyStreamChild: legacyChildFn("legacy raw text"),
		}
		err := RunStreamOrchestration(
			context.Background(), out, config, nil,
			func(ctx context.Context, clientOverride string) (*llmhttp.Request, error) {
				return nil, nil
			},
			nil, nil,
			newTestResult,
		)
		close(out)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		var finalRaw string
		for r := range out {
			if r.Kind() == bamlutils.StreamResultKindFinal {
				finalRaw = r.Raw()
			}
		}
		if finalRaw != "legacy raw text" {
			t.Errorf("expected raw='legacy raw text', got %q", finalRaw)
		}
	})

	t.Run("NeedsRaw false", func(t *testing.T) {
		out := make(chan bamlutils.StreamResult, 100)
		config := &StreamConfig{
			NeedsRaw:      false,
			FallbackChain: []string{"LegacyChild"},
			ClientProviders: map[string]string{
				"LegacyChild": "aws-bedrock",
			},
			LegacyChildren:    map[string]bool{"LegacyChild": true},
			LegacyStreamChild: legacyChildFn("legacy raw text"),
		}
		err := RunStreamOrchestration(
			context.Background(), out, config, nil,
			func(ctx context.Context, clientOverride string) (*llmhttp.Request, error) {
				return nil, nil
			},
			nil, nil,
			newTestResult,
		)
		close(out)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		var finalRaw string
		for r := range out {
			if r.Kind() == bamlutils.StreamResultKindFinal {
				finalRaw = r.Raw()
			}
		}
		if finalRaw != "" {
			t.Errorf("expected empty raw when NeedsRaw=false, got %q", finalRaw)
		}
	})
}

// TestRunStreamOrchestration_MixedChain_HTTPBackedEndToEnd is the closest
// approximation to a "generated-adapter integration test" we can write
// without adding AWS Bedrock Converse API emulation to the mock LLM (see
// the PR description for the follow-up needed to exercise a real adapter
// with a bedrock child). Both children run against real httptest servers:
//
//   - Supported child: open an SSE stream from an OpenAI-shaped server;
//     the orchestrator extracts deltas and parses a final.
//   - Legacy child: callback mirrors the shape runLegacyChildStream emits
//     — fires sendHeartbeat on the first upstream byte, drains the body,
//     and returns (final, raw, err). This exercises the runtime behavior
//     a real codegen-emitted legacyStreamChildFn would have, including
//     liveness signalling from a real HTTP round-trip.
//
// The test covers the most interesting scenario: supported child fails
// at parseFinal, the orchestrator emits a reset, dispatches to the
// legacy callback, and the legacy child's final propagates with its raw
// text. It checks the observable StreamResult timeline end-to-end.
func TestRunStreamOrchestration_MixedChain_HTTPBackedEndToEnd(t *testing.T) {
	// Supported child (openai SSE) — its body parses into "bad" which the
	// parseFinal closure rejects, so the orchestrator must advance to the
	// legacy child.
	supportedServer := makeOpenAIServer([]string{"bad"})
	defer supportedServer.Close()

	// Legacy child — plain text body, delayed so the heartbeat timing is
	// observable. This mirrors how a real BAML aws-bedrock response would
	// look to runLegacyChildStream: drain the body, extract a raw string,
	// return a final typed value.
	const legacyFinal = "legacy-final-text"
	const legacyRaw = "legacy-raw-payload"
	legacyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Small delay before body to give the test a visible heartbeat
		// window; runLegacyChildStream in the real generated code fires
		// its sendHeartbeat on the first FunctionLog tick, which tracks
		// upstream byte arrival.
		time.Sleep(10 * time.Millisecond)
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(200)
		fmt.Fprint(w, legacyRaw)
	}))
	defer legacyServer.Close()

	httpClient := llmhttp.NewClient(supportedServer.Client())

	// Legacy callback mirrors runLegacyChildStream: make a real HTTP call
	// via llmhttp, fire sendHeartbeat on first byte (via the Execute
	// onSuccess callback), read the body as raw, and synthesize a final.
	legacyStreamChild := func(ctx context.Context, clientOverride, provider string, needsRaw bool, sendHeartbeat func()) (any, string, string, error) {
		if clientOverride != "LegacyChild" {
			t.Errorf("legacy callback got clientOverride=%q, want LegacyChild", clientOverride)
		}
		req := &llmhttp.Request{URL: legacyServer.URL, Method: "POST", Body: `{}`}
		resp, err := httpClient.Execute(ctx, req, sendHeartbeat)
		if err != nil {
			return nil, "", "", err
		}
		raw := resp.Body
		if !needsRaw {
			raw = ""
		}
		return legacyFinal, raw, "", nil
	}

	out := make(chan bamlutils.StreamResult, 100)
	config := &StreamConfig{
		RetryPolicy:   &retry.Policy{MaxRetries: 0},
		NeedsPartials: true,
		NeedsRaw:      true,
		FallbackChain: []string{"SupportedChild", "LegacyChild"},
		ClientProviders: map[string]string{
			"SupportedChild": "openai",
			"LegacyChild":    "aws-bedrock",
		},
		LegacyChildren:    map[string]bool{"LegacyChild": true},
		LegacyStreamChild: legacyStreamChild,
	}

	// parseFinal rejects the supported child's content, forcing fall-over
	// to the legacy child. parseStream is lenient so partials flow
	// normally until the final reject.
	parseFinal := func(_ context.Context, s string) (any, error) {
		if s == "bad" {
			return nil, fmt.Errorf("parse rejected %q", s)
		}
		return s, nil
	}

	err := RunStreamOrchestration(
		context.Background(), out, config, httpClient,
		func(ctx context.Context, clientOverride string) (*llmhttp.Request, error) {
			return &llmhttp.Request{URL: supportedServer.URL, Method: "POST", Body: `{}`}, nil
		},
		func(_ context.Context, s string) (any, error) { return s, nil },
		parseFinal,
		newTestResult,
	)
	close(out)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Walk the full SSE-like timeline and verify ordering/contents.
	// Expected shape:
	//   [heartbeat, supported partials..., reset, heartbeat, final(legacy)]
	// — the supported attempt emits partials and a heartbeat on its HTTP
	// response; the orchestrator then resets before dispatching to the
	// legacy callback, which fires its own heartbeat on first-byte and
	// feeds back a final + raw.
	var (
		heartbeats int
		partials   int
		resets     int
		finals     int
		finalVal   any
		finalRaw   string
	)
	for r := range out {
		tr := r.(*testResult)
		switch tr.kind {
		case bamlutils.StreamResultKindHeartbeat:
			heartbeats++
		case bamlutils.StreamResultKindStream:
			if tr.reset {
				resets++
			} else {
				partials++
			}
		case bamlutils.StreamResultKindFinal:
			finals++
			finalVal = tr.final
			finalRaw = tr.raw
		case bamlutils.StreamResultKindError:
			t.Fatalf("unexpected error result: %v", tr.err)
		}
	}

	if heartbeats != 2 {
		// Supported child emits one on HTTP connect, legacy callback
		// emits a second after its reset. A different count indicates
		// the reset failed to clear heartbeatSent, the legacy callback
		// never fired sendHeartbeat, or a duplicate heartbeat slipped
		// past.
		t.Errorf("expected exactly 2 heartbeats (one per child attempt), got %d", heartbeats)
	}
	if partials != 1 {
		t.Errorf("expected exactly 1 partial from the supported child stream, got %d", partials)
	}
	if resets != 1 {
		t.Errorf("expected exactly 1 reset between children, got %d", resets)
	}
	if finals != 1 {
		t.Fatalf("expected exactly 1 final, got %d", finals)
	}
	if finalVal != legacyFinal {
		t.Errorf("expected final=%q from legacy child, got %v", legacyFinal, finalVal)
	}
	if finalRaw != legacyRaw {
		t.Errorf("expected raw=%q from legacy child, got %q", legacyRaw, finalRaw)
	}
}

// TestRunStreamOrchestration_CallBridge_ShapedLikeUnary simulates the
// call-mode bridge: NeedsPartials=false, NeedsRaw=true. The orchestrator
// consumes SSE, never emits a partial, and produces a single final with the
// accumulated raw. This is the shape pool.Call expects, so the unary handler
// returns the final as if it came from RunCallOrchestration.
func TestRunStreamOrchestration_CallBridge_ShapedLikeUnary(t *testing.T) {
	server := makeOpenAIServer([]string{"Hello", " ", "world"})
	defer server.Close()

	client := llmhttp.NewClient(server.Client())
	out := make(chan bamlutils.StreamResult, 100)

	config := &StreamConfig{
		Provider:      "openai",
		NeedsPartials: false, // call-mode bridge
		NeedsRaw:      true,
	}

	err := RunStreamOrchestration(
		context.Background(), out, config, client,
		func(ctx context.Context, clientOverride string) (*llmhttp.Request, error) {
			return &llmhttp.Request{URL: server.URL, Method: "POST", Body: `{}`}, nil
		},
		nil, // parseStream unused with NeedsPartials=false
		func(ctx context.Context, accumulated string) (any, error) { return accumulated, nil },
		newTestResult,
	)
	close(out)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var partials, finals, heartbeats, errors int
	var finalVal any
	var finalRaw string
	for r := range out {
		tr := r.(*testResult)
		switch tr.kind {
		case bamlutils.StreamResultKindStream:
			partials++
		case bamlutils.StreamResultKindFinal:
			finals++
			finalVal = tr.final
			finalRaw = tr.raw
		case bamlutils.StreamResultKindHeartbeat:
			heartbeats++
		case bamlutils.StreamResultKindError:
			errors++
		}
	}

	if partials != 0 {
		t.Errorf("bridge should emit no partials; got %d", partials)
	}
	if finals != 1 {
		t.Fatalf("expected 1 final, got %d", finals)
	}
	if errors != 0 {
		t.Errorf("expected 0 errors, got %d", errors)
	}
	if heartbeats != 1 {
		t.Errorf("expected 1 heartbeat, got %d", heartbeats)
	}
	if finalVal != "Hello world" {
		t.Errorf("expected final='Hello world', got %v", finalVal)
	}
	if finalRaw != "Hello world" {
		t.Errorf("expected raw='Hello world', got %q", finalRaw)
	}
}

// TestRunStreamOrchestration_NoResetWhenNeedsPartialsFalse_Retry verifies
// that the retry-boundary reset event is suppressed when NeedsPartials=false.
// Context: call modes (StreamModeCall / StreamModeCallWithRaw) bridged
// through this orchestrator have no partial audience downstream, so a reset
// event would falsely flip the pool's gotFirstByte flag before the retry's
// upstream has produced any byte — disabling hung detection.
func TestRunStreamOrchestration_NoResetWhenNeedsPartialsFalse_Retry(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		if n < 2 {
			w.WriteHeader(500)
			fmt.Fprint(w, "internal error")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	client := llmhttp.NewClient(server.Client())
	out := make(chan bamlutils.StreamResult, 100)

	config := &StreamConfig{
		Provider:      "openai",
		NeedsPartials: false, // call-mode bridge
		NeedsRaw:      true,
		RetryPolicy: &retry.Policy{
			MaxRetries: 2,
			Strategy:   &retry.ConstantDelay{DelayMs: 1},
		},
	}

	err := RunStreamOrchestration(
		context.Background(), out, config, client,
		func(ctx context.Context, clientOverride string) (*llmhttp.Request, error) {
			return &llmhttp.Request{URL: server.URL, Method: "POST", Body: `{}`}, nil
		},
		nil,
		func(ctx context.Context, accumulated string) (any, error) { return accumulated, nil },
		newTestResult,
	)
	close(out)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if attempts.Load() != 2 {
		t.Fatalf("expected 2 attempts, got %d", attempts.Load())
	}

	var resets, finals, heartbeats, errors int
	for r := range out {
		tr := r.(*testResult)
		// Strict gate on Stream+reset so a stray reset bit on a Final
		// or Error result can't inflate the counter.
		if tr.kind == bamlutils.StreamResultKindStream && tr.reset {
			resets++
		}
		if tr.kind == bamlutils.StreamResultKindFinal {
			finals++
		}
		if tr.kind == bamlutils.StreamResultKindHeartbeat {
			heartbeats++
		}
		if tr.kind == bamlutils.StreamResultKindError {
			errors++
		}
	}

	if resets != 0 {
		t.Errorf("expected 0 reset events under NeedsPartials=false; got %d", resets)
	}
	if finals != 1 {
		t.Errorf("expected 1 final, got %d", finals)
	}
	// The failing attempt never produces a byte (500 before SSE begins), so
	// its sendHeartbeat never fires. The retry re-arms heartbeatSent; the
	// successful attempt then fires one heartbeat on HTTP connect.
	if heartbeats != 1 {
		t.Errorf("expected exactly 1 heartbeat from the successful attempt; got %d", heartbeats)
	}
	// RunStreamOrchestration emits StreamResultKindError only after
	// retry.Execute returns an error (whole plan exhausted). A
	// recovered retry that also emits Error frames is a contract
	// regression.
	if errors != 0 {
		t.Errorf("expected 0 error frames on a recovered retry; got %d", errors)
	}
}

// TestRunStreamOrchestration_NoResetWhenNeedsPartialsFalse_FallbackChain
// verifies the in-chain reset (between failing children) is also suppressed
// under NeedsPartials=false. Same rationale as the retry variant.
func TestRunStreamOrchestration_NoResetWhenNeedsPartialsFalse_FallbackChain(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempt := int(attempts.Add(1))
		flusher, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		if attempt == 1 {
			// Primary: stream content that parseFinal rejects.
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"stale\"}}]}\n\n")
			if flusher != nil {
				flusher.Flush()
			}
			fmt.Fprint(w, "data: [DONE]\n\n")
			if flusher != nil {
				flusher.Flush()
			}
			return
		}
		// Secondary: stream valid content.
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"good\"}}]}\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer server.Close()

	client := llmhttp.NewClient(server.Client())
	out := make(chan bamlutils.StreamResult, 100)

	config := &StreamConfig{
		Provider:      "",
		RetryPolicy:   &retry.Policy{MaxRetries: 1, Strategy: &retry.ConstantDelay{DelayMs: 1}},
		NeedsPartials: false, // call-mode bridge
		FallbackChain: []string{"PrimaryClient", "SecondaryClient"},
		ClientProviders: map[string]string{
			"PrimaryClient":   "openai",
			"SecondaryClient": "openai",
		},
	}

	parseFinal := func(_ context.Context, s string) (any, error) {
		if s == "stale" {
			return nil, fmt.Errorf("rejected stale content")
		}
		return s, nil
	}

	// Capture the clientOverride sequence so we can assert the
	// fallback-handoff actually targeted SecondaryClient on the second
	// attempt rather than re-running PrimaryClient. The HTTP server keys
	// solely off request count, and buildFn ignores the override, so a
	// regression that retried the primary on the second attempt would
	// still satisfy the resets/finals assertions below.
	var (
		overrideMu  sync.Mutex
		overrideSeq []string
	)

	err := RunStreamOrchestration(
		context.Background(), out, config, client,
		func(ctx context.Context, clientOverride string) (*llmhttp.Request, error) {
			overrideMu.Lock()
			overrideSeq = append(overrideSeq, clientOverride)
			overrideMu.Unlock()
			return &llmhttp.Request{URL: server.URL, Method: "POST", Body: `{}`}, nil
		},
		nil,
		parseFinal,
		newTestResult,
	)
	close(out)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	overrideMu.Lock()
	gotOverrides := append([]string(nil), overrideSeq...)
	overrideMu.Unlock()
	wantOverrides := []string{"PrimaryClient", "SecondaryClient"}
	if !reflect.DeepEqual(gotOverrides, wantOverrides) {
		t.Errorf("clientOverride sequence: got %v, want %v (regression: fallback handoff did not switch children)", gotOverrides, wantOverrides)
	}

	var resets, finals, heartbeats, errors int
	var finalVal any
	for r := range out {
		tr := r.(*testResult)
		// Strict gate on Stream+reset; a stray bit on Final/Error
		// must not show up as a reset.
		if tr.kind == bamlutils.StreamResultKindStream && tr.reset {
			resets++
		}
		if tr.kind == bamlutils.StreamResultKindFinal {
			finals++
			finalVal = tr.final
		}
		if tr.kind == bamlutils.StreamResultKindHeartbeat {
			heartbeats++
		}
		if tr.kind == bamlutils.StreamResultKindError {
			errors++
		}
	}

	if resets != 0 {
		t.Errorf("expected 0 reset events under NeedsPartials=false; got %d", resets)
	}
	if finals != 1 {
		t.Errorf("expected 1 final, got %d", finals)
	}
	if finalVal != "good" {
		t.Errorf("expected final=%q, got %v", "good", finalVal)
	}
	// Both children emit a heartbeat on HTTP connect; the in-chain reset
	// must still clear heartbeatSent.
	if heartbeats != 2 {
		t.Errorf("expected exactly 2 heartbeats (one per child), got %d", heartbeats)
	}
	// RunStreamOrchestration emits StreamResultKindError only after
	// retry.Execute returns an error (whole plan exhausted). A
	// successful fallback handoff that also emits an Error frame is
	// a contract regression.
	if errors != 0 {
		t.Errorf("expected 0 error frames on a recovered fallback handoff; got %d", errors)
	}
}

// TestRunStreamOrchestration_NoResetWhenNoStreamFrame_FallbackChain pins
// the in-chain reset marker contract: it must NOT fire when the
// previous child failed BEFORE successfully queuing any
// StreamResultKindStream frame to the client. NeedsPartials=true is
// the regression class — without the sawStreamFrame gate, every
// NeedsPartials handoff would emit a reset regardless of upstream
// activity, and the pool's first-byte detector would treat that
// reset (a Stream-kind result) as progress for the next child even
// when the prior window produced zero bytes.
//
// The test uses a primary that returns 500 before any SSE byte (so
// trySendPartial never runs and sawStreamFrame stays false) and a
// secondary that succeeds. The channel must not carry a phantom
// reset between the two children.
func TestRunStreamOrchestration_NoResetWhenNoStreamFrame_FallbackChain(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempt := int(attempts.Add(1))
		if attempt == 1 {
			// Primary: 500 before any SSE byte. tryOneStreamChild
			// fails out via httpClient.ExecuteStream's status check;
			// trySendPartial never runs.
			w.WriteHeader(500)
			fmt.Fprint(w, "internal error")
			return
		}
		// Secondary: clean SSE response with one delta.
		flusher, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"good\"}}]}\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer server.Close()

	client := llmhttp.NewClient(server.Client())
	out := make(chan bamlutils.StreamResult, 100)

	config := &StreamConfig{
		Provider:      "",
		RetryPolicy:   &retry.Policy{MaxRetries: 1, Strategy: &retry.ConstantDelay{DelayMs: 1}},
		NeedsPartials: true, // partials wired up — reset only suppressed by the sawStreamFrame gate
		FallbackChain: []string{"PrimaryClient", "SecondaryClient"},
		ClientProviders: map[string]string{
			"PrimaryClient":   "openai",
			"SecondaryClient": "openai",
		},
	}

	// Capture the clientOverride sequence so we can assert the
	// fallback-handoff actually targeted SecondaryClient on the
	// second attempt rather than re-running PrimaryClient. The HTTP
	// server keys solely off request count, and buildRequest ignores
	// the override, so a regression that retried the primary on the
	// second attempt would still satisfy the resets/finals
	// assertions below.
	var (
		overrideMu  sync.Mutex
		overrideSeq []string
	)

	err := RunStreamOrchestration(
		context.Background(), out, config, client,
		func(ctx context.Context, clientOverride string) (*llmhttp.Request, error) {
			overrideMu.Lock()
			overrideSeq = append(overrideSeq, clientOverride)
			overrideMu.Unlock()
			return &llmhttp.Request{URL: server.URL, Method: "POST", Body: `{}`}, nil
		},
		func(_ context.Context, s string) (any, error) { return s, nil },
		func(_ context.Context, s string) (any, error) { return s, nil },
		newTestResult,
	)
	close(out)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	overrideMu.Lock()
	gotOverrides := append([]string(nil), overrideSeq...)
	overrideMu.Unlock()
	wantOverrides := []string{"PrimaryClient", "SecondaryClient"}
	if !reflect.DeepEqual(gotOverrides, wantOverrides) {
		t.Errorf("clientOverride sequence: got %v, want %v (regression: fallback handoff did not switch children)", gotOverrides, wantOverrides)
	}

	var resets, finals, errors int
	for r := range out {
		tr := r.(*testResult)
		// Strict gate on Stream+reset; only a Stream result should
		// carry the reset bit.
		if tr.kind == bamlutils.StreamResultKindStream && tr.reset {
			resets++
		}
		if tr.kind == bamlutils.StreamResultKindFinal {
			finals++
		}
		if tr.kind == bamlutils.StreamResultKindError {
			errors++
		}
	}
	if resets != 0 {
		t.Errorf("expected 0 reset events when prior child queued no stream frames; got %d", resets)
	}
	if finals != 1 {
		t.Errorf("expected 1 final from secondary, got %d", finals)
	}
	// RunStreamOrchestration emits StreamResultKindError only after
	// retry.Execute returns an error (whole plan exhausted). A
	// successful fallback handoff that also emits an Error frame is
	// a contract regression.
	if errors != 0 {
		t.Errorf("expected 0 error frames on a recovered fallback handoff; got %d", errors)
	}
}

// TestRunStreamOrchestration_NoResetWhenNoStreamFrame_Retry is the
// retry-callback sibling of the fallback-chain no-stream-frame test.
// Same shape: first attempt fails before any stream frame, retry
// succeeds; the retry-boundary reset must be suppressed.
func TestRunStreamOrchestration_NoResetWhenNoStreamFrame_Retry(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		if n < 2 {
			w.WriteHeader(500)
			fmt.Fprint(w, "internal error")
			return
		}
		flusher, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer server.Close()

	client := llmhttp.NewClient(server.Client())
	out := make(chan bamlutils.StreamResult, 100)

	config := &StreamConfig{
		Provider:      "openai",
		NeedsPartials: true,
		NeedsRaw:      false,
		RetryPolicy: &retry.Policy{
			MaxRetries: 2,
			Strategy:   &retry.ConstantDelay{DelayMs: 1},
		},
	}

	err := RunStreamOrchestration(
		context.Background(), out, config, client,
		func(ctx context.Context, clientOverride string) (*llmhttp.Request, error) {
			return &llmhttp.Request{URL: server.URL, Method: "POST", Body: `{}`}, nil
		},
		func(_ context.Context, s string) (any, error) { return s, nil },
		func(_ context.Context, s string) (any, error) { return s, nil },
		newTestResult,
	)
	close(out)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if attempts.Load() != 2 {
		t.Fatalf("expected 2 attempts, got %d", attempts.Load())
	}

	var resets, finals, errors int
	for r := range out {
		tr := r.(*testResult)
		// Strict gate on Stream+reset; only a Stream result should
		// carry the reset bit.
		if tr.kind == bamlutils.StreamResultKindStream && tr.reset {
			resets++
		}
		if tr.kind == bamlutils.StreamResultKindFinal {
			finals++
		}
		if tr.kind == bamlutils.StreamResultKindError {
			errors++
		}
	}
	if resets != 0 {
		t.Errorf("expected 0 reset events when prior attempt queued no stream frames; got %d", resets)
	}
	// Also pin success delivery so a regression that suppresses
	// resets AND drops the final can't pass this test by only
	// satisfying the reset-count check. Mirrors the fallback-chain
	// sibling above.
	if finals != 1 {
		t.Errorf("expected 1 final from successful retry, got %d", finals)
	}
	// RunStreamOrchestration emits StreamResultKindError only after
	// retry.Execute returns an error (whole plan exhausted). A
	// recovered retry that also emits Error frames is a contract
	// regression.
	if errors != 0 {
		t.Errorf("expected 0 error frames on a recovered retry; got %d", errors)
	}
}

// TestRunStreamOrchestration_ParseFinalError_CarriesRaw pins the
// details.raw contract from #256 on the streaming SSE path: when the
// path fails at final-parse after the stream completed cleanly, the
// emitted error frame must carry the accumulated raw text via Raw() so
// the worker bridge can surface it on details.raw. Symmetric to the
// call-orchestrator parse-error test.
func TestRunStreamOrchestration_ParseFinalError_CarriesRaw(t *testing.T) {
	chunks := []string{"Sorry,", " I", " can't"}
	server := makeOpenAIServer(chunks)
	defer server.Close()

	client := llmhttp.NewClient(server.Client())
	out := make(chan bamlutils.StreamResult, 100)

	config := &StreamConfig{
		Provider:      "openai",
		NeedsPartials: true,
		NeedsRaw:      true,
	}

	parseFinal := func(_ context.Context, s string) (any, error) {
		// Return a plain parse error. The production orchestrator
		// applies wrapOutputParse() at the final-parse site, then
		// newRawError wraps that with the accumulated raw. This test
		// pins that the resulting chain still satisfies
		// errors.Is(err, ErrOutputParse) — wrapping ErrOutputParse
		// inside the fixture would short-circuit the assertion and
		// hide a regression in the production sentinel wrap.
		return nil, fmt.Errorf("Parsing error: bad field")
	}

	err := RunStreamOrchestration(
		context.Background(), out, config, client,
		func(ctx context.Context, clientOverride string) (*llmhttp.Request, error) {
			return &llmhttp.Request{URL: server.URL, Method: "POST", Body: `{}`}, nil
		},
		func(ctx context.Context, accumulated string) (any, error) { return accumulated, nil },
		parseFinal,
		newTestResult,
	)
	close(out)
	if err != nil {
		t.Fatalf("unexpected error from RunStreamOrchestration: %v", err)
	}

	// Track terminal-frame shape, not just "any error frame
	// exists". A regression that emitted a Final frame alongside
	// (or instead of) the error frame — or multiple terminal
	// frames — would slip past a loose "find one error" check;
	// pinning exactly one error, zero finals, and an error as the
	// last frame catches those. Mirrors the
	// _StreamError_CarriesRaw sibling's terminal-frame contract.
	var errResult bamlutils.StreamResult
	var lastFrame bamlutils.StreamResult
	var errorCount, finalCount int
	for r := range out {
		lastFrame = r
		switch r.Kind() {
		case bamlutils.StreamResultKindError:
			errResult = r
			errorCount++
		case bamlutils.StreamResultKindFinal:
			finalCount++
		}
	}
	if errorCount != 1 {
		t.Fatalf("expected exactly 1 error frame from final-parse failure, got %d", errorCount)
	}
	if finalCount != 0 {
		t.Fatalf("expected 0 Final frames on a final-parse failure, got %d", finalCount)
	}
	if lastFrame == nil || lastFrame.Kind() != bamlutils.StreamResultKindError {
		t.Fatalf("expected last emitted frame to be an Error, got %v", lastFrame)
	}
	const wantRaw = "Sorry, I can't"
	if errResult.Raw() != wantRaw {
		t.Fatalf("expected error.Raw() = %q (accumulated text), got %q", wantRaw, errResult.Raw())
	}
	// errors.Is must walk through both production wraps
	// (wrapOutputParse + rawCarryingError) so the worker bridge's
	// classifyBAMLError still maps this to parse_error. The loose
	// "has Unwrap" shape check this replaces would pass for any
	// wrapped error — proving nothing about sentinel preservation.
	if !errors.Is(errResult.Error(), ErrOutputParse) {
		t.Fatalf("expected error to satisfy ErrOutputParse through the wrapOutputParse + rawCarryingError chain; got %v",
			errResult.Error())
	}
}

// TestRunStreamOrchestration_StreamError_CarriesRaw pins the
// mid-stream-failure variant of the #256 details.raw contract: when
// the transport breaks after partial accumulation, the error frame's
// Raw() carries whatever bytes arrived before the failure.
//
// The test must pin specifically to the stream-error wrap site (where
// resp.Errc is consumed and wrapped with `buildrequest: stream error:
// %w`) rather than the final-parse wrap site that fires on the same
// attempt. Two precautions make that pin sharp:
//
//   - parseFinal returns success on whatever accumulator content survives,
//     so the test cannot escape into the final-parse wrap. If the stream
//     error doesn't fire, the attempt would succeed and emit a Final
//     frame — the assertion would then fail with "no error frame".
//   - the server panics with http.ErrAbortHandler after one valid SSE
//     event, leaving the chunked response without its zero-size
//     terminator. Go's chunked reader surfaces io.ErrUnexpectedEOF on
//     the SSE scanner, which propagates to resp.Errc — the deterministic
//     trigger for the stream-error wrap. parseStream errors can't
//     substitute here: production intentionally ignores partial parse
//     failures (orchestrator.go's `if parseErr == nil && parsed != nil`
//     guard), so a failing parseStream wouldn't terminate the attempt.
//
// Asserts: (1) an error frame is emitted, (2) errResult.Raw() carries
// the pre-truncation accumulator, (3) errResult.Error() is the
// stream-error wrap (not the final-parse wrap).
func TestRunStreamOrchestration_StreamError_CarriesRaw(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		flusher, _ := w.(http.Flusher)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		// Drop the connection without writing the zero-size chunk
		// terminator. Go's chunked reader surfaces this as
		// io.ErrUnexpectedEOF — mirrors the
		// fast_backend_test.go::truncatedChunkedHandler pattern.
		panic(http.ErrAbortHandler)
	}))
	defer server.Close()

	client := llmhttp.NewClient(server.Client())
	out := make(chan bamlutils.StreamResult, 100)

	config := &StreamConfig{
		Provider:      "openai",
		NeedsPartials: true,
		NeedsRaw:      true,
	}

	// Success-shape parseFinal: if execution ever reaches the
	// final-parse branch, the attempt succeeds — the test then fails
	// at the "expected an error frame" assertion. This is what makes
	// the test specifically pin the stream-error wrap rather than
	// accidentally pass via the final-parse wrap.
	parseFinal := func(_ context.Context, accumulated string) (any, error) {
		return accumulated, nil
	}

	err := RunStreamOrchestration(
		context.Background(), out, config, client,
		func(ctx context.Context, clientOverride string) (*llmhttp.Request, error) {
			return &llmhttp.Request{URL: server.URL, Method: "POST", Body: `{}`}, nil
		},
		func(ctx context.Context, accumulated string) (any, error) { return accumulated, nil },
		parseFinal,
		newTestResult,
	)
	// RunStreamOrchestration's contract: after emitting an error frame
	// on the channel, the function itself returns nil. Errors flow via
	// the StreamResult, not the function return — pinning both keeps
	// a regression that routes the failure through the wrong channel
	// from slipping past the channel-only assertions below.
	if err != nil {
		t.Fatalf("unexpected error from RunStreamOrchestration: %v", err)
	}
	close(out)

	var errResult bamlutils.StreamResult
	var sawFinal bool
	for r := range out {
		switch r.Kind() {
		case bamlutils.StreamResultKindError:
			errResult = r
		case bamlutils.StreamResultKindFinal:
			sawFinal = true
		}
	}
	if sawFinal {
		t.Fatal("unexpected Final frame — truncation should have terminated the attempt at the stream-error wrap, not parseFinal")
	}
	if errResult == nil {
		t.Fatal("expected an error result from the truncated chunked response")
	}
	if !strings.Contains(errResult.Raw(), "Hello") {
		t.Fatalf("expected error.Raw() to include the pre-truncation chunk %q; got %q",
			"Hello", errResult.Raw())
	}
	// Pin to the stream-error wrap (orchestrator.go's resp.Errc arm)
	// specifically. A regression that routed the truncation through
	// the final-parse wrap would carry a "failed to parse final
	// result" message instead — same Raw, different prefix.
	if !strings.Contains(errResult.Error().Error(), "buildrequest: stream error") {
		t.Fatalf("expected error message to come from the stream-error wrap; got %q",
			errResult.Error().Error())
	}
}
