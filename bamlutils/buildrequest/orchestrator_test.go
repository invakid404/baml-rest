package buildrequest

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
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
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		if n < 3 {
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
		NeedsPartials: true,
		NeedsRaw:      true,
		RetryPolicy: &retry.Policy{
			MaxRetries: 3,
			Strategy:   &retry.ConstantDelay{DelayMs: 1},
		},
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

	if attempts.Load() != 3 {
		t.Errorf("expected 3 attempts, got %d", attempts.Load())
	}

	// Should have reset signals from retries
	var resets, finals int
	for r := range out {
		tr := r.(*testResult)
		if tr.reset {
			resets++
		}
		if tr.kind == bamlutils.StreamResultKindFinal {
			finals++
		}
	}

	if resets < 2 {
		t.Errorf("expected at least 2 reset signals (for 2 retries), got %d", resets)
	}
	if finals != 1 {
		t.Errorf("expected 1 final, got %d", finals)
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
	// upfront planned-metadata emit is a no-op (CodeRabbit verdict-28
	// finding 8 only changes ordering, not the nil-config short-
	// circuit). So this branch still pins "no stream output on
	// validation error when metadata isn't wired up". The metadata-
	// wired-up case is covered by
	// TestRunStreamOrchestration_EmitsPlannedMetadataBeforeValidationError
	// below.
	for r := range out {
		t.Fatalf("expected no stream results on upfront validation error, got kind %v", r.Kind())
	}
}

// TestRunStreamOrchestration_EmitsPlannedMetadataBeforeValidationError
// pins the post-verdict-28 contract (CodeRabbit finding 8): when the
// orchestrator config supplies MetadataPlan + NewMetadataResult, the
// planned-metadata event MUST be emitted before any validation return.
// Pre-fix the emit sat after validation, which silently dropped the
// only observable signal on exactly the failure modes operators most
// need to diagnose (unsupported provider, malformed fallback chain).
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
	planned, outcome, kinds := collectMetadata(t, out)
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
	// Cold-review finding 2 (narrow fix): a fallback chain with a
	// baml-roundrobin child still runs, but the PathReason surfaces the
	// composition so operators can distinguish centralised top-level RR
	// from per-worker nested RR. The RR child lands on the legacy child
	// list (IsProviderSupported returns false for baml-roundrobin), and
	// BAML's runtime handles its rotation on each worker independently.
	// Centralised unwrapping of RR children inside fallback chains is
	// deferred — see cold-review rebuttal / broad fix.
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

func TestResolveFallbackChain_FallbackWithRRChild_HyphenatedSpelling(t *testing.T) {
	// Same as above but with the "baml-round-robin" spelling (BAML
	// upstream) to confirm the alias classifier folds all three RR
	// spellings (baml-roundrobin / baml-round-robin / round-robin)
	// before deciding whether the child contributes the new
	// PathReason.
	fallbackChains := map[string][]string{
		"MyFallback": {"A", "InnerRR"},
	}
	clientProviders := map[string]string{
		"MyFallback": "baml-fallback",
		"A":          "openai",
		"InnerRR":    "baml-round-robin",
	}

	_, _, _, reason := ResolveFallbackChainForClientWithReason(
		nil, "MyFallback", fallbackChains, clientProviders,
		func(p string) bool { return p == "openai" },
	)
	if reason != PathReasonFallbackRoundRobinChildLegacy {
		t.Fatalf("reason with alias spelling: got %q, want %q", reason, PathReasonFallbackRoundRobinChildLegacy)
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

	buildFn := func(ctx context.Context, clientOverride string) (*llmhttp.Request, error) {
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

	// Find the reset signal — there should be at least one between the
	// primary's partial and the secondary's partial/final.
	sawReset := false
	sawStalePartial := false
	finalVal := ""
	for _, r := range results {
		if r.kind == bamlutils.StreamResultKindStream && r.stream == "stale" {
			sawStalePartial = true
		}
		if r.kind == bamlutils.StreamResultKindStream && r.reset {
			sawReset = true
		}
		if r.kind == bamlutils.StreamResultKindFinal {
			finalVal, _ = r.final.(string)
		}
	}

	if sawStalePartial && !sawReset {
		t.Error("primary emitted partial data but no reset signal was sent before the secondary child")
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

	var resets, finals int
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

	var partials, finals int
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
		}
	}
	if partials < 1 {
		t.Errorf("expected at least one partial from the supported child, got %d", partials)
	}
	if finals != 1 {
		t.Fatalf("expected exactly one final, got %d", finals)
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

	var resets, finals, heartbeats int
	for r := range out {
		tr := r.(*testResult)
		if tr.reset {
			resets++
		}
		if tr.kind == bamlutils.StreamResultKindFinal {
			finals++
		}
		if tr.kind == bamlutils.StreamResultKindHeartbeat {
			heartbeats++
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

	err := RunStreamOrchestration(
		context.Background(), out, config, client,
		func(ctx context.Context, clientOverride string) (*llmhttp.Request, error) {
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

	var resets, finals, heartbeats int
	var finalVal any
	for r := range out {
		tr := r.(*testResult)
		if tr.reset {
			resets++
		}
		if tr.kind == bamlutils.StreamResultKindFinal {
			finals++
			finalVal = tr.final
		}
		if tr.kind == bamlutils.StreamResultKindHeartbeat {
			heartbeats++
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
}
