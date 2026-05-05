package buildrequest

import (
	"context"
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
	kind     bamlutils.StreamResultKind
	stream   any
	final    any
	raw      string
	err      error
	reset    bool
	metadata *bamlutils.Metadata
}

func (r *testResult) Kind() bamlutils.StreamResultKind { return r.kind }
func (r *testResult) Stream() any                      { return r.stream }
func (r *testResult) Final() any                       { return r.final }
func (r *testResult) Error() error                     { return r.err }
func (r *testResult) Raw() string                      { return r.raw }
func (r *testResult) Reset() bool                      { return r.reset }
func (r *testResult) Metadata() *bamlutils.Metadata    { return r.metadata }
func (r *testResult) Release()                         {}

func newTestResult(kind bamlutils.StreamResultKind, stream, final any, raw string, err error, reset bool) bamlutils.StreamResult {
	return &testResult{kind: kind, stream: stream, final: final, raw: raw, err: err, reset: reset}
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
	if partials < 1 {
		t.Errorf("expected at least 1 partial, got %d", partials)
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
	if nonResetPartials == 0 {
		t.Errorf("expected at least one non-reset partial (testResult with kind == bamlutils.StreamResultKindStream and reset == false); got 0")
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
	supported := []string{"openai", "openai-generic", "azure-openai", "anthropic", "google-ai", "vertex-ai", "ollama", "openrouter", "openai-responses"}
	for _, p := range supported {
		if !IsProviderSupported(p) {
			t.Errorf("expected provider %q to be supported", p)
		}
	}

	unsupported := []string{"aws-bedrock"}
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
	// Default (IncludeThinkingInRaw=false, BAML-aligned): thinking_delta
	// events are dropped from the extractor entirely. They produce no Stream
	// kind result (delta.Raw is empty), so partials count is 2 (text deltas
	// only), and final raw equals parseable.
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
		Provider:             "anthropic",
		NeedsPartials:        true,
		NeedsRaw:             true,
		IncludeThinkingInRaw: false,
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
	var partialStreams []any
	var finalResult *testResult
	for r := range out {
		tr := r.(*testResult)
		if tr.kind == bamlutils.StreamResultKindStream {
			partialRaws = append(partialRaws, tr.raw)
			partialStreams = append(partialStreams, tr.stream)
		}
		if tr.kind == bamlutils.StreamResultKindFinal {
			finalResult = tr
		}
	}

	if want := []string{"The answer", " is 42"}; len(partialRaws) != len(want) || partialRaws[0] != want[0] || partialRaws[1] != want[1] {
		t.Fatalf("partial raws = %v, want %v", partialRaws, want)
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
}

func TestRunStreamOrchestration_AnthropicThinkingPreservesRawStream_OptIn(t *testing.T) {
	// Opt-in (IncludeThinkingInRaw=true): thinking_delta events accumulate
	// into the raw stream alongside text deltas. Parseable still excludes
	// thinking content (the BAML parser must never see it).
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
		Provider:             "anthropic",
		NeedsPartials:        true,
		NeedsRaw:             true,
		IncludeThinkingInRaw: true,
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
	var partialStreams []any
	var finalResult *testResult
	for r := range out {
		tr := r.(*testResult)
		if tr.kind == bamlutils.StreamResultKindStream {
			partialRaws = append(partialRaws, tr.raw)
			partialStreams = append(partialStreams, tr.stream)
		}
		if tr.kind == bamlutils.StreamResultKindFinal {
			finalResult = tr
		}
	}

	if want := []string{"Step 1: reason...", "The answer", " Step 2: refine...", " is 42"}; len(partialRaws) != len(want) || partialRaws[0] != want[0] || partialRaws[1] != want[1] || partialRaws[2] != want[2] || partialRaws[3] != want[3] {
		t.Fatalf("partial raws = %v, want %v", partialRaws, want)
	}
	if partialStreams[0] != nil || partialStreams[2] != nil {
		t.Fatalf("thinking partials should be raw-only, got streams %v", partialStreams)
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
	if finalResult.raw != "Step 1: reason...The answer Step 2: refine... is 42" {
		t.Fatalf("final raw = %q", finalResult.raw)
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

func TestResolveFallbackChain_AllSupported(t *testing.T) {
	fallbackChains := map[string][]string{
		"MyFallback": {"ClientA", "ClientB"},
	}
	clientProviders := map[string]string{
		"MyFallback": "baml-fallback",
		"ClientA":    "openai",
		"ClientB":    "anthropic",
	}

	adapter := &mockAdapter{Context: context.Background()}
	chain, providers, legacyChildren := ResolveFallbackChain(
		adapter, "MyFallback", fallbackChains, clientProviders,
		func(p string) bool { return p == "openai" || p == "anthropic" },
	)

	if len(chain) != 2 {
		t.Fatalf("expected chain length 2, got %d", len(chain))
	}
	if providers["ClientA"] != "openai" || providers["ClientB"] != "anthropic" {
		t.Errorf("unexpected providers: %v", providers)
	}
	if len(legacyChildren) != 0 {
		t.Errorf("expected empty legacyChildren for all-supported chain, got %v", legacyChildren)
	}
}

func TestResolveFallbackChain_MixedSupport(t *testing.T) {
	// Chain [openai, aws-bedrock, anthropic] with only openai + anthropic
	// supported by BuildRequest. Expect a 3-element chain back with
	// legacyChildren marking the aws-bedrock child.
	fallbackChains := map[string][]string{
		"MyFallback": {"ClientA", "ClientB", "ClientC"},
	}
	clientProviders := map[string]string{
		"MyFallback": "baml-fallback",
		"ClientA":    "openai",
		"ClientB":    "aws-bedrock", // unsupported
		"ClientC":    "anthropic",
	}

	adapter := &mockAdapter{Context: context.Background()}
	chain, providers, legacyChildren := ResolveFallbackChain(
		adapter, "MyFallback", fallbackChains, clientProviders,
		func(p string) bool { return p == "openai" || p == "anthropic" },
	)

	if len(chain) != 3 {
		t.Fatalf("expected chain length 3 for mixed-mode, got %d", len(chain))
	}
	if providers["ClientA"] != "openai" || providers["ClientB"] != "aws-bedrock" || providers["ClientC"] != "anthropic" {
		t.Errorf("unexpected providers: %v", providers)
	}
	if !legacyChildren["ClientB"] {
		t.Errorf("expected ClientB to be marked legacy, got legacyChildren=%v", legacyChildren)
	}
	if legacyChildren["ClientA"] || legacyChildren["ClientC"] {
		t.Errorf("expected only ClientB to be legacy, got %v", legacyChildren)
	}
}

func TestResolveFallbackChain_AllUnsupported(t *testing.T) {
	// When every child is unsupported, ResolveFallbackChain returns nil so
	// the caller routes the whole chain through the existing legacy path.
	fallbackChains := map[string][]string{
		"MyFallback": {"ClientA", "ClientB"},
	}
	clientProviders := map[string]string{
		"MyFallback": "baml-fallback",
		"ClientA":    "aws-bedrock",
		"ClientB":    "aws-bedrock",
	}

	adapter := &mockAdapter{Context: context.Background()}
	chain, providers, legacyChildren := ResolveFallbackChain(
		adapter, "MyFallback", fallbackChains, clientProviders,
		func(p string) bool { return p == "openai" || p == "anthropic" },
	)

	if chain != nil || providers != nil || legacyChildren != nil {
		t.Errorf("expected nil results when every child is unsupported, got chain=%v providers=%v legacy=%v", chain, providers, legacyChildren)
	}
}

func TestResolveFallbackChain_DuplicateLegacyChildren(t *testing.T) {
	// A chain listing the same unsupported client twice is still "all
	// legacy" from a routing standpoint — there is no supported child for
	// BuildRequest to drive, so the resolver must return nil so the whole
	// chain routes through the legacy path. The previous implementation
	// compared len(legacyChildren) (a set, 1) against len(chain)
	// (positions, 2) and incorrectly reported this as a mixed chain.
	fallbackChains := map[string][]string{
		"MyFallback": {"BedrockA", "BedrockA"},
	}
	clientProviders := map[string]string{
		"MyFallback": "baml-fallback",
		"BedrockA":   "aws-bedrock",
	}

	adapter := &mockAdapter{Context: context.Background()}
	chain, providers, legacyChildren := ResolveFallbackChain(
		adapter, "MyFallback", fallbackChains, clientProviders,
		func(p string) bool { return p == "openai" || p == "anthropic" },
	)

	if chain != nil || providers != nil || legacyChildren != nil {
		t.Errorf("expected nil results when every chain position is legacy (duplicates included), got chain=%v providers=%v legacy=%v", chain, providers, legacyChildren)
	}
}

func TestResolveFallbackChain_EmptyProviderFallsBack(t *testing.T) {
	// A child with a missing provider is fatal — we can't route an unknown
	// provider through either BuildRequest or legacy reliably, so the
	// whole chain should fall back to the legacy path.
	fallbackChains := map[string][]string{
		"MyFallback": {"ClientA", "ClientB"},
	}
	clientProviders := map[string]string{
		"MyFallback": "baml-fallback",
		"ClientA":    "openai",
		// ClientB intentionally missing
	}

	adapter := &mockAdapter{Context: context.Background()}
	chain, providers, legacyChildren := ResolveFallbackChain(
		adapter, "MyFallback", fallbackChains, clientProviders,
		func(p string) bool { return p == "openai" || p == "anthropic" },
	)

	if chain != nil || providers != nil || legacyChildren != nil {
		t.Errorf("expected nil results when a child's provider is empty, got chain=%v providers=%v legacy=%v", chain, providers, legacyChildren)
	}
}

func TestResolveFallbackChain_RuntimeOverrideChild(t *testing.T) {
	fallbackChains := map[string][]string{
		"MyFallback": {"ClientA", "ClientB"},
	}
	// Introspected: ClientA=openai, ClientB=anthropic
	clientProviders := map[string]string{
		"MyFallback": "baml-fallback",
		"ClientA":    "openai",
		"ClientB":    "anthropic",
	}

	// Runtime override changes ClientB from anthropic to google-ai
	adapter := &mockAdapter{
		Context: context.Background(),
		originalRegistry: &bamlutils.ClientRegistry{
			Clients: []*bamlutils.ClientProperty{
				{Name: "ClientB", Provider: "google-ai"},
			},
		},
	}
	chain, providers, legacyChildren := ResolveFallbackChain(
		adapter, "MyFallback", fallbackChains, clientProviders,
		func(p string) bool { return p == "openai" || p == "anthropic" || p == "google-ai" },
	)

	if len(chain) != 2 {
		t.Fatalf("expected chain length 2, got %d", len(chain))
	}
	// ClientB should use the runtime override, not the introspected value
	if providers["ClientB"] != "google-ai" {
		t.Errorf("expected ClientB provider 'google-ai' (runtime override), got %q", providers["ClientB"])
	}
	if providers["ClientA"] != "openai" {
		t.Errorf("expected ClientA provider 'openai' (introspected), got %q", providers["ClientA"])
	}
	if len(legacyChildren) != 0 {
		t.Errorf("expected no legacy children, got %v", legacyChildren)
	}
}

func TestResolveFallbackChain_RuntimeOverrideMakesLegacy(t *testing.T) {
	// All introspected providers are supported; runtime override flips one
	// child to aws-bedrock, turning this into a mixed-mode chain rather
	// than falling back entirely to the legacy path.
	fallbackChains := map[string][]string{
		"MyFallback": {"ClientA", "ClientB"},
	}
	clientProviders := map[string]string{
		"MyFallback": "baml-fallback",
		"ClientA":    "openai",
		"ClientB":    "anthropic",
	}

	adapter := &mockAdapter{
		Context: context.Background(),
		originalRegistry: &bamlutils.ClientRegistry{
			Clients: []*bamlutils.ClientProperty{
				{Name: "ClientB", Provider: "aws-bedrock"},
			},
		},
	}
	chain, providers, legacyChildren := ResolveFallbackChain(
		adapter, "MyFallback", fallbackChains, clientProviders,
		func(p string) bool { return p == "openai" || p == "anthropic" },
	)

	if len(chain) != 2 {
		t.Fatalf("expected chain length 2 for mixed-mode, got %d", len(chain))
	}
	if !legacyChildren["ClientB"] {
		t.Errorf("expected ClientB to be legacy after runtime override, got %v", legacyChildren)
	}
	if legacyChildren["ClientA"] {
		t.Errorf("expected ClientA to remain BuildRequest-driven, got %v", legacyChildren)
	}
	if providers["ClientB"] != "aws-bedrock" {
		t.Errorf("expected ClientB provider 'aws-bedrock' in providers map, got %q", providers["ClientB"])
	}
}

func TestResolveFallbackChain_RuntimeStrategyOverrideReordersChildren(t *testing.T) {
	fallbackChains := map[string][]string{
		"MyFallback": {"ClientA", "ClientB"},
	}
	clientProviders := map[string]string{
		"MyFallback": "baml-fallback",
		"ClientA":    "openai",
		"ClientB":    "anthropic",
	}

	adapter := &mockAdapter{
		Context: context.Background(),
		originalRegistry: &bamlutils.ClientRegistry{
			Clients: []*bamlutils.ClientProperty{
				{
					Name:     "MyFallback",
					Provider: "baml-fallback",
					Options: map[string]any{
						"strategy": []any{"ClientB", "ClientA"},
					},
				},
			},
		},
	}

	chain, providers, legacyChildren := ResolveFallbackChain(
		adapter, "MyFallback", fallbackChains, clientProviders,
		func(p string) bool { return p == "openai" || p == "anthropic" },
	)

	if len(chain) != 2 {
		t.Fatalf("expected chain length 2, got %d", len(chain))
	}
	if chain[0] != "ClientB" || chain[1] != "ClientA" {
		t.Fatalf("expected runtime strategy order [ClientB ClientA], got %v", chain)
	}
	if providers["ClientB"] != "anthropic" || providers["ClientA"] != "openai" {
		t.Errorf("unexpected providers: %v", providers)
	}
	if len(legacyChildren) != 0 {
		t.Errorf("expected no legacy children for all-supported chain, got %v", legacyChildren)
	}
}

func TestResolveFallbackChain_RuntimeStrategyOverrideWithoutIntrospectedChain(t *testing.T) {
	fallbackChains := map[string][]string{}
	clientProviders := map[string]string{
		"MyFallback": "baml-fallback",
		"ClientA":    "openai",
		"ClientB":    "anthropic",
	}

	adapter := &mockAdapter{
		Context: context.Background(),
		originalRegistry: &bamlutils.ClientRegistry{
			Clients: []*bamlutils.ClientProperty{
				{
					Name:     "MyFallback",
					Provider: "baml-fallback",
					Options: map[string]any{
						"strategy": "strategy [ClientB, ClientA]",
					},
				},
			},
		},
	}

	chain, providers, legacyChildren := ResolveFallbackChain(
		adapter, "MyFallback", fallbackChains, clientProviders,
		func(p string) bool { return p == "openai" || p == "anthropic" },
	)

	if len(chain) != 2 {
		t.Fatalf("expected runtime-defined chain length 2, got %d", len(chain))
	}
	if chain[0] != "ClientB" || chain[1] != "ClientA" {
		t.Fatalf("expected runtime strategy order [ClientB ClientA], got %v", chain)
	}
	if providers["ClientB"] != "anthropic" || providers["ClientA"] != "openai" {
		t.Errorf("unexpected providers: %v", providers)
	}
	if len(legacyChildren) != 0 {
		t.Errorf("expected no legacy children for all-supported chain, got %v", legacyChildren)
	}
}

func TestResolveFallbackChain_RuntimeQuotedStringStrategyOverride(t *testing.T) {
	fallbackChains := map[string][]string{}
	clientProviders := map[string]string{
		"MyFallback": "baml-fallback",
		"ClientA":    "openai",
		"ClientB":    "anthropic",
	}

	adapter := &mockAdapter{
		Context: context.Background(),
		originalRegistry: &bamlutils.ClientRegistry{
			Clients: []*bamlutils.ClientProperty{
				{
					Name:     "MyFallback",
					Provider: "baml-fallback",
					Options: map[string]any{
						"strategy": `strategy ["ClientB", "ClientA"]`,
					},
				},
			},
		},
	}

	chain, providers, legacyChildren := ResolveFallbackChain(
		adapter, "MyFallback", fallbackChains, clientProviders,
		func(p string) bool { return p == "openai" || p == "anthropic" },
	)

	if len(chain) != 2 {
		t.Fatalf("expected runtime-defined chain length 2, got %d", len(chain))
	}
	if chain[0] != "ClientB" || chain[1] != "ClientA" {
		t.Fatalf("expected runtime strategy order [ClientB ClientA], got %v", chain)
	}
	if providers["ClientB"] != "anthropic" || providers["ClientA"] != "openai" {
		t.Errorf("unexpected providers: %v", providers)
	}
	if len(legacyChildren) != 0 {
		t.Errorf("expected no legacy children for all-supported chain, got %v", legacyChildren)
	}
}

func TestResolveFallbackChain_RuntimeOverrideMakesAllLegacy(t *testing.T) {
	// Runtime override flips every child to an unsupported provider →
	// full legacy fallback (nil results), matching the all-unsupported case.
	fallbackChains := map[string][]string{
		"MyFallback": {"ClientA", "ClientB"},
	}
	clientProviders := map[string]string{
		"MyFallback": "baml-fallback",
		"ClientA":    "openai",
		"ClientB":    "anthropic",
	}

	adapter := &mockAdapter{
		Context: context.Background(),
		originalRegistry: &bamlutils.ClientRegistry{
			Clients: []*bamlutils.ClientProperty{
				{Name: "ClientA", Provider: "aws-bedrock"},
				{Name: "ClientB", Provider: "aws-bedrock"},
			},
		},
	}
	chain, providers, legacyChildren := ResolveFallbackChain(
		adapter, "MyFallback", fallbackChains, clientProviders,
		func(p string) bool { return p == "openai" || p == "anthropic" },
	)

	if chain != nil || providers != nil || legacyChildren != nil {
		t.Errorf("expected nil when every child is legacy, got chain=%v providers=%v legacy=%v", chain, providers, legacyChildren)
	}
}

func TestResolveFallbackChain_NotFallback(t *testing.T) {
	fallbackChains := map[string][]string{}
	clientProviders := map[string]string{"GPT4": "openai"}

	adapter := &mockAdapter{Context: context.Background()}
	chain, providers, legacyChildren := ResolveFallbackChain(
		adapter, "GPT4", fallbackChains, clientProviders,
		func(p string) bool { return true },
	)

	if chain != nil || providers != nil || legacyChildren != nil {
		t.Errorf("expected nil for non-fallback client, got chain=%v providers=%v legacy=%v", chain, providers, legacyChildren)
	}
}

func TestResolveFallbackChain_RoundRobinChildSkipped(t *testing.T) {
	// ResolveFallbackChain only classifies baml-fallback parents. A
	// baml-roundrobin parent returns nil,nil,nil — not because RR is
	// "legacy-only" globally (top-level RR is resolved upstream by
	// ResolveEffectiveClient for modern adapters with SupportsWithClient),
	// but because this helper is the fallback-chain classifier. RR is
	// handled either by the upstream resolver (modern) or by BAML's
	// runtime under the legacy path (older adapters / nested RR children
	// inside a fallback chain).
	fallbackChains := map[string][]string{
		"MyRoundRobin": {"ClientA", "ClientB"},
	}
	clientProviders := map[string]string{
		"MyRoundRobin": "baml-roundrobin",
		"ClientA":      "openai",
		"ClientB":      "anthropic",
	}

	adapter := &mockAdapter{Context: context.Background()}
	chain, providers, legacyChildren := ResolveFallbackChain(
		adapter, "MyRoundRobin", fallbackChains, clientProviders,
		func(p string) bool { return true },
	)

	if chain != nil || providers != nil || legacyChildren != nil {
		t.Errorf("expected nil for baml-roundrobin parent (classified elsewhere), got chain=%v providers=%v legacy=%v", chain, providers, legacyChildren)
	}
}

func TestResolveFallbackChain_FallbackWithRRChild_SurfacesReason(t *testing.T) {
	// A fallback chain with a baml-roundrobin child still runs, but
	// the PathReason surfaces the composition so operators can
	// distinguish centralised top-level RR from per-worker nested
	// RR. The RR child lands on the legacy child list
	// (IsProviderSupported returns false for baml-roundrobin), and
	// BAML's runtime handles its rotation on each worker
	// independently. Centralised unwrapping of RR children inside
	// fallback chains is deferred.
	fallbackChains := map[string][]string{
		"MyFallback": {"OpenAIClient", "InnerRR"},
		"InnerRR":    {"GoogleA", "GoogleB"},
	}
	clientProviders := map[string]string{
		"MyFallback":   "baml-fallback",
		"OpenAIClient": "openai",
		"InnerRR":      "baml-roundrobin",
		"GoogleA":      "google-ai",
		"GoogleB":      "google-ai",
	}

	chain, providers, legacy, reason := ResolveFallbackChainForClientWithReason(
		nil, "MyFallback", fallbackChains, clientProviders,
		func(p string) bool {
			return p == "openai" || p == "google-ai" || p == "anthropic"
		},
	)

	if chain == nil {
		t.Fatalf("expected chain to resolve (RR child is mixed-legacy, not abort), got nil with reason=%q", reason)
	}
	if reason != PathReasonFallbackRoundRobinChildLegacy {
		t.Fatalf("reason: got %q, want %q", reason, PathReasonFallbackRoundRobinChildLegacy)
	}
	if !legacy["InnerRR"] {
		t.Errorf("InnerRR must be marked legacy (BAML runtime handles RR rotation per-worker), legacyChildren=%v", legacy)
	}
	if legacy["OpenAIClient"] {
		t.Errorf("OpenAIClient is a supported provider — must not be on legacy list, legacyChildren=%v", legacy)
	}
	if providers["InnerRR"] != "baml-roundrobin" {
		t.Errorf("providers[InnerRR]: got %q, want baml-roundrobin", providers["InnerRR"])
	}
}

func TestResolveFallbackChain_FallbackWithRRChild_AliasSpellings(t *testing.T) {
	// Resolver-level coverage for the non-canonical RR spellings the
	// alias classifier must fold before deciding whether the child
	// contributes PathReasonFallbackRoundRobinChildLegacy. The
	// canonical "baml-roundrobin" form is exercised by
	// TestResolveFallbackChain_FallbackWithRRChild above; this table
	// pins the two alias forms (BAML upstream's hyphenated
	// "baml-round-robin" and the bare "round-robin" shorthand).
	// Both alias spellings must hit the resolver's RR-child path.
	// (Metadata-plan-level coverage of all three spellings exists in
	// TestBuildFallbackChainPlan_ReasonPlumbsFromResolver.)
	cases := []struct {
		name     string
		provider string
	}{
		{name: "baml-round-robin", provider: "baml-round-robin"},
		{name: "round-robin", provider: "round-robin"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fallbackChains := map[string][]string{
				"MyFallback": {"A", "InnerRR"},
			}
			clientProviders := map[string]string{
				"MyFallback": "baml-fallback",
				"A":          "openai",
				"InnerRR":    tc.provider,
			}

			_, providers, _, reason := ResolveFallbackChainForClientWithReason(
				nil, "MyFallback", fallbackChains, clientProviders,
				func(p string) bool { return p == "openai" },
			)
			if reason != PathReasonFallbackRoundRobinChildLegacy {
				t.Fatalf("reason with alias %q: got %q, want %q", tc.provider, reason, PathReasonFallbackRoundRobinChildLegacy)
			}
			// providers map must surface the canonical "baml-roundrobin"
			// spelling regardless of which alias the caller's
			// clientProviders used. ResolveClientProvider applies
			// normalizeStrategyProvider, but a regression that dropped
			// the normalisation on one path could still yield the
			// right reason while leaking the alias here.
			if got := providers["InnerRR"]; got != "baml-roundrobin" {
				t.Errorf("providers[InnerRR] with alias %q: got %q, want baml-roundrobin (canonical)", tc.provider, got)
			}
		})
	}
}

// TestResolveFallbackChain_NestedInvalidStrategyOverride pins the
// invalid-nested preflight: a fallback chain whose nested RR child
// has a present-but-malformed runtime `strategy` override must abort
// chain resolution with PathReasonInvalidStrategyOverride. The caller
// (codegen `len(chain) > 0` gate) then routes the whole request to
// top-level legacy, where BAML's eager registry parse emits the
// canonical ensure_strategy error against the full runtime registry.
//
// Without this preflight, the mixed-mode legacy callback for InnerRR
// would forward the invalid entry into BAML inside the per-child
// callback, BAML's rejection would be re-classified as an ordinary
// fallback child failure, and a later successful sibling could
// silently mask the operator's misconfiguration — exactly the
// validation suppression the top-level invalid-override fallthrough
// was designed to prevent.
func TestResolveFallbackChain_NestedInvalidStrategyOverride(t *testing.T) {
	fallbackChains := map[string][]string{
		"MyFallback": {"FastLeaf", "InnerRR"},
		"InnerRR":    {"StaticBlue", "StaticGreen"},
	}
	clientProviders := map[string]string{
		"MyFallback": "baml-fallback",
		"FastLeaf":   "openai",
		"InnerRR":    "baml-roundrobin",
	}

	reg := &bamlutils.ClientRegistry{
		Clients: []*bamlutils.ClientProperty{
			// "garbage" parses to an empty chain — present but invalid.
			{Name: "InnerRR", Provider: "round-robin", ProviderSet: true,
				Options: map[string]any{"strategy": "garbage"}},
		},
	}

	chain, providers, legacy, reason := ResolveFallbackChainForClientWithReason(
		reg, "MyFallback", fallbackChains, clientProviders,
		func(p string) bool { return p == "openai" },
	)

	if chain != nil {
		t.Fatalf("expected nil chain for invalid nested strategy override, got %v", chain)
	}
	if reason != PathReasonInvalidStrategyOverride {
		t.Errorf("reason: got %q, want %q", reason, PathReasonInvalidStrategyOverride)
	}
	if providers != nil {
		t.Errorf("providers must be nil when chain is rejected, got %v", providers)
	}
	if legacy != nil {
		t.Errorf("legacyChildren must be nil when chain is rejected, got %v", legacy)
	}
}

// TestResolveFallbackChain_NestedInvalidStartOverride pins the same
// invalid-nested preflight for the `start` option. Mirrors the
// top-level invalid-start route (TestRoundRobinOverrides_InvalidStart-
// RoutesToLegacy) so the contract is consistent between top-level RR
// and nested-RR-inside-fallback compositions: an invalid `start`
// surfaces upstream's canonical ensure_int error rather than silently
// using the static child config.
func TestResolveFallbackChain_NestedInvalidStartOverride(t *testing.T) {
	fallbackChains := map[string][]string{
		"MyFallback": {"FastLeaf", "InnerRR"},
		"InnerRR":    {"StaticBlue", "StaticGreen"},
	}
	clientProviders := map[string]string{
		"MyFallback": "baml-fallback",
		"FastLeaf":   "openai",
		"InnerRR":    "baml-roundrobin",
	}

	reg := &bamlutils.ClientRegistry{
		Clients: []*bamlutils.ClientProperty{
			// String "1" — present-but-invalid for a numeric option.
			{Name: "InnerRR", Provider: "round-robin", ProviderSet: true,
				Options: map[string]any{
					"strategy": []any{"StaticBlue", "StaticGreen"},
					"start":    "1",
				}},
		},
	}

	chain, _, _, reason := ResolveFallbackChainForClientWithReason(
		reg, "MyFallback", fallbackChains, clientProviders,
		func(p string) bool { return p == "openai" },
	)

	if chain != nil {
		t.Fatalf("expected nil chain for invalid nested start override, got %v", chain)
	}
	if reason != PathReasonInvalidRoundRobinStartOverride {
		t.Errorf("reason: got %q, want %q", reason, PathReasonInvalidRoundRobinStartOverride)
	}
}

// TestResolveFallbackChain_NestedInvalidProviderOverride pins the
// preflight's provider-precedence path: a present-empty `provider`
// on a nested strategy-parent child surfaces
// PathReasonInvalidProviderOverride, mirroring the top-level
// classifier's switch precedence. Operators reading
// X-BAML-Path-Reason then see the same reason whether the invalid
// override targets the request's top-level client or a nested
// fallback chain child.
func TestResolveFallbackChain_NestedInvalidProviderOverride(t *testing.T) {
	fallbackChains := map[string][]string{
		"MyFallback": {"FastLeaf", "InnerRR"},
		"InnerRR":    {"StaticBlue", "StaticGreen"},
	}
	clientProviders := map[string]string{
		"MyFallback": "baml-fallback",
		"FastLeaf":   "openai",
		"InnerRR":    "baml-roundrobin",
	}

	reg := &bamlutils.ClientRegistry{
		Clients: []*bamlutils.ClientProperty{
			// Provider explicitly set to "" — operator typo or hostile
			// input. Forwarding "" into BAML's CFFI is rejected
			// in ClientProvider::from_str.
			{Name: "InnerRR", Provider: "", ProviderSet: true},
		},
	}

	chain, _, _, reason := ResolveFallbackChainForClientWithReason(
		reg, "MyFallback", fallbackChains, clientProviders,
		func(p string) bool { return p == "openai" },
	)

	if chain != nil {
		t.Fatalf("expected nil chain for invalid nested provider override, got %v", chain)
	}
	if reason != PathReasonInvalidProviderOverride {
		t.Errorf("reason: got %q, want %q", reason, PathReasonInvalidProviderOverride)
	}
}

// TestResolveFallbackChain_NestedValidOverrideStillResolves pins
// negative coverage: a nested strategy-parent child WITH a valid
// runtime override (changed strategy children, valid start, valid
// provider) must still resolve normally with the existing
// PathReasonFallbackRoundRobinChildLegacy. The preflight's
// "any nested override → legacy" alternative would lose mixed-mode
// orchestration for legitimate runtime overrides; this test guards
// against that drift.
func TestResolveFallbackChain_NestedValidOverrideStillResolves(t *testing.T) {
	fallbackChains := map[string][]string{
		"MyFallback": {"FastLeaf", "InnerRR"},
		"InnerRR":    {"StaticBlue", "StaticGreen"},
	}
	clientProviders := map[string]string{
		"MyFallback": "baml-fallback",
		"FastLeaf":   "openai",
		"InnerRR":    "baml-roundrobin",
	}

	reg := &bamlutils.ClientRegistry{
		Clients: []*bamlutils.ClientProperty{
			// Valid: redirects InnerRR's children at runtime.
			{Name: "InnerRR", Provider: "round-robin", ProviderSet: true,
				Options: map[string]any{
					"strategy": []any{"TenantLeaf"},
					"start":    0,
				}},
			{Name: "TenantLeaf", Provider: "openai", ProviderSet: true},
		},
	}

	chain, _, legacy, reason := ResolveFallbackChainForClientWithReason(
		reg, "MyFallback", fallbackChains, clientProviders,
		func(p string) bool { return p == "openai" },
	)

	if chain == nil {
		t.Fatalf("expected chain to resolve for valid nested override, got nil with reason=%q", reason)
	}
	if reason != PathReasonFallbackRoundRobinChildLegacy {
		t.Errorf("reason: got %q, want %q", reason, PathReasonFallbackRoundRobinChildLegacy)
	}
	if !legacy["InnerRR"] {
		t.Errorf("InnerRR must remain on the mixed-mode legacy child list (BAML runtime handles RR rotation), legacyChildren=%v", legacy)
	}
}

// TestResolveFallbackChain_TransitiveInvalidStartOverride pins the
// transitive scope of the invalid-nested preflight: a fallback chain
// whose immediate strategy-parent child is itself a fallback that
// transitively reaches an RR with an invalid runtime `start` must
// short-circuit to top-level legacy with
// PathReasonInvalidRoundRobinStartOverride. Without transitive scope,
// the deep RR's malformed entry would be carried into the mixed-mode
// per-child legacy callback's scoped registry, BAML's eager parse
// would reject it inside InnerFallback's callback, RunStream-/
// CallOrchestration would re-classify the rejection as an ordinary
// fallback child failure, and the LaterLeaf sibling could silently
// answer the request with 200 — masking the misconfiguration.
//
// The LaterLeaf sibling is load-bearing: it's the trailing valid
// child that would have absorbed the deep failure under the
// immediate-only preflight. Its presence forces this test to depend
// on transitive walking specifically.
func TestResolveFallbackChain_TransitiveInvalidStartOverride(t *testing.T) {
	fallbackChains := map[string][]string{
		"MyOuterFallback": {"InnerFallback", "LaterLeaf"},
		"InnerFallback":   {"DeeperRR"},
		"DeeperRR":        {"LeafX", "LeafY"},
	}
	clientProviders := map[string]string{
		"MyOuterFallback": "baml-fallback",
		"InnerFallback":   "baml-fallback",
		"DeeperRR":        "baml-roundrobin",
		"LeafX":           "openai",
		"LeafY":           "openai",
		"LaterLeaf":       "openai",
	}

	reg := &bamlutils.ClientRegistry{
		Clients: []*bamlutils.ClientProperty{
			// DeeperRR is two strategy-parent levels below the
			// outer chain's immediate child (InnerFallback).
			// Numeric string fails BAML's i32 ensure_int contract.
			{Name: "DeeperRR", Provider: "round-robin", ProviderSet: true,
				Options: map[string]any{
					"strategy": []any{"LeafX", "LeafY"},
					"start":    "1",
				}},
		},
	}

	chain, _, _, reason := ResolveFallbackChainForClientWithReason(
		reg, "MyOuterFallback", fallbackChains, clientProviders,
		func(p string) bool { return p == "openai" },
	)

	if chain != nil {
		t.Fatalf("expected nil chain (deep invalid start should short-circuit), got %v", chain)
	}
	if reason != PathReasonInvalidRoundRobinStartOverride {
		t.Errorf("reason: got %q, want %q (transitive walk should catch deep invalid start)",
			reason, PathReasonInvalidRoundRobinStartOverride)
	}
}

// TestResolveFallbackChain_TransitiveInvalidStrategyOverride is the
// strategy-side counterpart to TransitiveInvalidStartOverride. Same
// composition (Outer → InnerFallback → DeeperRR + LaterLeaf), but the
// deep invalid override is a malformed `strategy` value rather than
// `start`. Asserts the transitive walk catches it with
// PathReasonInvalidStrategyOverride.
func TestResolveFallbackChain_TransitiveInvalidStrategyOverride(t *testing.T) {
	fallbackChains := map[string][]string{
		"MyOuterFallback": {"InnerFallback", "LaterLeaf"},
		"InnerFallback":   {"DeeperRR"},
		"DeeperRR":        {"LeafX", "LeafY"},
	}
	clientProviders := map[string]string{
		"MyOuterFallback": "baml-fallback",
		"InnerFallback":   "baml-fallback",
		"DeeperRR":        "baml-roundrobin",
		"LeafX":           "openai",
		"LeafY":           "openai",
		"LaterLeaf":       "openai",
	}

	reg := &bamlutils.ClientRegistry{
		Clients: []*bamlutils.ClientProperty{
			// "garbage" parses to an empty chain — present but invalid
			// per InspectStrategyOverride's three-state classification.
			{Name: "DeeperRR", Provider: "round-robin", ProviderSet: true,
				Options: map[string]any{"strategy": "garbage"}},
		},
	}

	chain, _, _, reason := ResolveFallbackChainForClientWithReason(
		reg, "MyOuterFallback", fallbackChains, clientProviders,
		func(p string) bool { return p == "openai" },
	)

	if chain != nil {
		t.Fatalf("expected nil chain (deep invalid strategy should short-circuit), got %v", chain)
	}
	if reason != PathReasonInvalidStrategyOverride {
		t.Errorf("reason: got %q, want %q (transitive walk should catch deep invalid strategy)",
			reason, PathReasonInvalidStrategyOverride)
	}
}

// TestResolveFallbackChain_TransitivePresentEmptyProviderOverride
// pins the provider-side transitive case. The deep override is a
// present-empty `provider:""` — operator typo or hostile input that
// BAML's CFFI rejects in ClientProvider::from_str. Same Outer →
// InnerFallback → DeeperRR + LaterLeaf composition; LaterLeaf would
// mask the failure under immediate-only scope.
//
// Per-node precedence in the transitive walk surfaces this case as
// PathReasonInvalidProviderOverride (provider checked before
// strategy/start at each visited node), matching
// ResolveProviderWithReason's top-level switch.
func TestResolveFallbackChain_TransitivePresentEmptyProviderOverride(t *testing.T) {
	fallbackChains := map[string][]string{
		"MyOuterFallback": {"InnerFallback", "LaterLeaf"},
		"InnerFallback":   {"DeeperRR"},
		"DeeperRR":        {"LeafX", "LeafY"},
	}
	clientProviders := map[string]string{
		"MyOuterFallback": "baml-fallback",
		"InnerFallback":   "baml-fallback",
		"DeeperRR":        "baml-roundrobin",
		"LeafX":           "openai",
		"LeafY":           "openai",
		"LaterLeaf":       "openai",
	}

	reg := &bamlutils.ClientRegistry{
		Clients: []*bamlutils.ClientProperty{
			// Provider explicitly set to empty string — present but
			// invalid. Without the runtime override the introspected
			// provider (baml-roundrobin) would classify normally.
			{Name: "DeeperRR", Provider: "", ProviderSet: true},
		},
	}

	chain, _, _, reason := ResolveFallbackChainForClientWithReason(
		reg, "MyOuterFallback", fallbackChains, clientProviders,
		func(p string) bool { return p == "openai" },
	)

	if chain != nil {
		t.Fatalf("expected nil chain (deep invalid provider should short-circuit), got %v", chain)
	}
	if reason != PathReasonInvalidProviderOverride {
		t.Errorf("reason: got %q, want %q (transitive walk should catch deep present-empty provider)",
			reason, PathReasonInvalidProviderOverride)
	}
}

// TestResolveFallbackChain_TransitiveCycleSafe pins that a self-
// cyclic strategy graph (InnerFallback → InnerFallback) does not
// infinite-loop the transitive walk. The visited set in
// FindInvalidReachableStrategyOverride is the guard. With no
// invalid override anywhere in the graph the chain must resolve
// normally — empty reason and a non-empty chain.
//
// Mixed-mode composition (FastLeaf + InnerFallback) keeps the chain
// drivable: InnerFallback is a strategy parent so it lands on
// legacyChildren, but a chain with at least one BR-supported
// sibling escapes the legacyPositions == len(chain) short-circuit
// and is returned to the caller.
func TestResolveFallbackChain_TransitiveCycleSafe(t *testing.T) {
	fallbackChains := map[string][]string{
		"MyOuterFallback": {"FastLeaf", "InnerFallback"},
		"InnerFallback":   {"InnerFallback"},
	}
	clientProviders := map[string]string{
		"MyOuterFallback": "baml-fallback",
		"FastLeaf":        "openai",
		"InnerFallback":   "baml-fallback",
	}

	chain, providers, legacy, reason := ResolveFallbackChainForClientWithReason(
		nil, "MyOuterFallback", fallbackChains, clientProviders,
		func(p string) bool { return p == "openai" },
	)

	if len(chain) != 2 {
		t.Fatalf("expected len=2 chain (FastLeaf, InnerFallback), got %d: %v", len(chain), chain)
	}
	if reason != "" {
		t.Errorf("reason: got %q, want empty (cycle without invalid overrides should not surface a path reason)", reason)
	}
	if providers["FastLeaf"] != "openai" {
		t.Errorf("FastLeaf provider: got %q, want openai", providers["FastLeaf"])
	}
	if !legacy["InnerFallback"] {
		t.Errorf("InnerFallback must be on the mixed-mode legacy child list (strategy parent), legacyChildren=%v", legacy)
	}
}

// TestResolveFallbackChain_NestedExplicitRRProviderWithoutStrategy
// pins the chain-preflight contract for the missing-strategy case:
// a nested RR child whose runtime override carries an explicit RR
// provider but no `options.strategy` must route the whole request
// to top-level legacy with PathReasonInvalidStrategyOverride.
// Falling back to the introspected chain would silently dispatch
// a static child while BAML's eager parse rejects the shape — the
// validation-suppression class the preflight rules out.
//
// LaterLeaf is load-bearing: it's the trailing valid sibling that
// would have masked the deep failure under per-child legacy
// dispatch. Its presence means this test depends specifically on
// the transitive walk catching the missing-strategy case.
func TestResolveFallbackChain_NestedExplicitRRProviderWithoutStrategy(t *testing.T) {
	fallbackChains := map[string][]string{
		"MyOuterFallback": {"InnerRR", "LaterLeaf"},
		"InnerRR":         {"StaticBlue", "StaticGreen"},
	}
	clientProviders := map[string]string{
		"MyOuterFallback": "baml-fallback",
		"InnerRR":         "baml-roundrobin",
		"StaticBlue":      "openai",
		"StaticGreen":     "openai",
		"LaterLeaf":       "openai",
	}

	reg := &bamlutils.ClientRegistry{
		Clients: []*bamlutils.ClientProperty{
			// Explicit RR provider, no options.strategy — runtime
			// shape that BAML's eager parse rejects with
			// ensure_strategy.
			{Name: "InnerRR", Provider: "round-robin", ProviderSet: true,
				Options: map[string]any{"temperature": 0.7}},
		},
	}

	chain, _, _, reason := ResolveFallbackChainForClientWithReason(
		reg, "MyOuterFallback", fallbackChains, clientProviders,
		func(p string) bool { return p == "openai" },
	)

	if chain != nil {
		t.Fatalf("expected nil chain (explicit RR provider w/ no strategy should short-circuit), got %v", chain)
	}
	if reason != PathReasonInvalidStrategyOverride {
		t.Errorf("reason: got %q, want %q (transitive walk should catch explicit-provider+no-strategy)",
			reason, PathReasonInvalidStrategyOverride)
	}
}

func TestResolveFallbackChain_FallbackAllowed(t *testing.T) {
	// baml-fallback SHOULD use the BuildRequest path.
	fallbackChains := map[string][]string{
		"MyFallback": {"ClientA", "ClientB"},
	}
	clientProviders := map[string]string{
		"MyFallback": "baml-fallback",
		"ClientA":    "openai",
		"ClientB":    "anthropic",
	}

	adapter := &mockAdapter{Context: context.Background()}
	chain, providers, legacyChildren := ResolveFallbackChain(
		adapter, "MyFallback", fallbackChains, clientProviders,
		func(p string) bool { return p == "openai" || p == "anthropic" },
	)

	if len(chain) != 2 {
		t.Fatalf("expected chain length 2 for baml-fallback, got %d", len(chain))
	}
	if providers["ClientA"] != "openai" || providers["ClientB"] != "anthropic" {
		t.Errorf("unexpected providers: %v", providers)
	}
	if len(legacyChildren) != 0 {
		t.Errorf("expected no legacy children for all-supported chain, got %v", legacyChildren)
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

	// Find the reset signal — there should be at least one between the
	// primary's partial and the secondary's partial/final.
	sawReset := false
	sawStalePartial := false
	errorFrames := 0
	finalVal := ""
	for _, r := range results {
		if r.kind == bamlutils.StreamResultKindStream && r.stream == "stale" {
			sawStalePartial = true
		}
		if r.kind == bamlutils.StreamResultKindStream && r.reset {
			sawReset = true
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
	if sawStalePartial && !sawReset {
		t.Error("primary emitted partial data but no reset signal was sent before the secondary child")
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
	legacyStreamChild := func(ctx context.Context, clientOverride, provider string, needsRaw bool, sendHeartbeat func()) (any, string, error) {
		legacyCalled.Add(1)
		gotOverride = clientOverride
		gotProvider = provider
		gotNeedsRaw = needsRaw
		sendHeartbeat()
		return "legacy ok", "legacy raw", nil
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
	if resets < 1 {
		t.Errorf("expected at least one reset signal between children, got %d", resets)
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
	legacyStreamChild := func(ctx context.Context, clientOverride, provider string, needsRaw bool, sendHeartbeat func()) (any, string, error) {
		legacyCalled.Add(1)
		return nil, "", fmt.Errorf("legacy upstream 500")
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
	if partials < 1 {
		t.Errorf("expected at least one partial from the supported child, got %d", partials)
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
			LegacyStreamChild: func(ctx context.Context, clientOverride, provider string, needsRaw bool, sendHeartbeat func()) (any, string, error) {
				// Call sendHeartbeat multiple times to verify idempotency.
				sendHeartbeat()
				sendHeartbeat()
				sendHeartbeat()
				return "ok", "", nil
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
			LegacyStreamChild: func(ctx context.Context, clientOverride, provider string, needsRaw bool, sendHeartbeat func()) (any, string, error) {
				// Simulate failure before any upstream byte — callback
				// never fires the heartbeat. Pool hung-detection should
				// still be able to kill this request if it hangs long
				// enough; here we just assert no heartbeat leaked.
				return nil, "", fmt.Errorf("upstream 4xx")
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
		return func(ctx context.Context, clientOverride, provider string, needsRaw bool, sendHeartbeat func()) (any, string, error) {
			sendHeartbeat()
			return "final", raw, nil
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
	legacyStreamChild := func(ctx context.Context, clientOverride, provider string, needsRaw bool, sendHeartbeat func()) (any, string, error) {
		if clientOverride != "LegacyChild" {
			t.Errorf("legacy callback got clientOverride=%q, want LegacyChild", clientOverride)
		}
		req := &llmhttp.Request{URL: legacyServer.URL, Method: "POST", Body: `{}`}
		resp, err := httpClient.Execute(ctx, req, sendHeartbeat)
		if err != nil {
			return nil, "", err
		}
		raw := resp.Body
		if !needsRaw {
			raw = ""
		}
		return legacyFinal, raw, nil
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

	if heartbeats < 2 {
		// Supported child emits one on HTTP connect, legacy callback
		// emits a second after its reset. Fewer than two indicates the
		// reset failed to clear heartbeatSent, or the legacy callback
		// never fired sendHeartbeat.
		t.Errorf("expected at least 2 heartbeats (one per child attempt), got %d", heartbeats)
	}
	if partials < 1 {
		t.Errorf("expected at least 1 partial from the supported child stream, got %d", partials)
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
	if heartbeats < 1 {
		t.Errorf("expected >=1 heartbeat from the successful attempt; got %d", heartbeats)
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
	if heartbeats < 2 {
		t.Errorf("expected >=2 heartbeats (one per child), got %d", heartbeats)
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
