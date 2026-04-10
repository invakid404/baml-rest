package buildrequest

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/bamlutils/retry"
)

// testResult implements bamlutils.StreamResult for testing
type testResult struct {
	kind   bamlutils.StreamResultKind
	stream any
	final  any
	raw    string
	err    error
	reset  bool
}

func (r *testResult) Kind() bamlutils.StreamResultKind { return r.kind }
func (r *testResult) Stream() any                      { return r.stream }
func (r *testResult) Final() any                       { return r.final }
func (r *testResult) Error() error                     { return r.err }
func (r *testResult) Raw() string                      { return r.raw }
func (r *testResult) Reset() bool                      { return r.reset }
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

func TestRunStreamOrchestration_AnthropicThinkingPreservesRawStream(t *testing.T) {
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
		NeedsRaw:      true,
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
	chain, providers := ResolveFallbackChain(
		adapter, "MyFallback", fallbackChains, clientProviders,
		func(p string) bool { return p == "openai" || p == "anthropic" },
	)

	if len(chain) != 2 {
		t.Fatalf("expected chain length 2, got %d", len(chain))
	}
	if providers["ClientA"] != "openai" || providers["ClientB"] != "anthropic" {
		t.Errorf("unexpected providers: %v", providers)
	}
}

func TestResolveFallbackChain_UnsupportedChild(t *testing.T) {
	fallbackChains := map[string][]string{
		"MyFallback": {"ClientA", "ClientB"},
	}
	clientProviders := map[string]string{
		"MyFallback": "baml-fallback",
		"ClientA":    "openai",
		"ClientB":    "aws-bedrock", // unsupported
	}

	adapter := &mockAdapter{Context: context.Background()}
	chain, providers := ResolveFallbackChain(
		adapter, "MyFallback", fallbackChains, clientProviders,
		func(p string) bool { return p == "openai" || p == "anthropic" },
	)

	if chain != nil || providers != nil {
		t.Errorf("expected nil chain/providers when child has unsupported provider, got chain=%v providers=%v", chain, providers)
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
	chain, providers := ResolveFallbackChain(
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

	chain, providers := ResolveFallbackChain(
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

	chain, providers := ResolveFallbackChain(
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
}

func TestResolveFallbackChain_RuntimeOverrideUnsupported(t *testing.T) {
	fallbackChains := map[string][]string{
		"MyFallback": {"ClientA", "ClientB"},
	}
	clientProviders := map[string]string{
		"MyFallback": "baml-fallback",
		"ClientA":    "openai",
		"ClientB":    "anthropic",
	}

	// Runtime override changes ClientB to an unsupported provider
	adapter := &mockAdapter{
		Context: context.Background(),
		originalRegistry: &bamlutils.ClientRegistry{
			Clients: []*bamlutils.ClientProperty{
				{Name: "ClientB", Provider: "aws-bedrock"},
			},
		},
	}
	chain, providers := ResolveFallbackChain(
		adapter, "MyFallback", fallbackChains, clientProviders,
		func(p string) bool { return p == "openai" || p == "anthropic" },
	)

	// Should fall back to legacy path because runtime override made ClientB unsupported
	if chain != nil || providers != nil {
		t.Errorf("expected nil when runtime override makes child unsupported, got chain=%v providers=%v", chain, providers)
	}
}

func TestResolveFallbackChain_NotFallback(t *testing.T) {
	fallbackChains := map[string][]string{}
	clientProviders := map[string]string{"GPT4": "openai"}

	adapter := &mockAdapter{Context: context.Background()}
	chain, providers := ResolveFallbackChain(
		adapter, "GPT4", fallbackChains, clientProviders,
		func(p string) bool { return true },
	)

	if chain != nil || providers != nil {
		t.Errorf("expected nil for non-fallback client, got chain=%v providers=%v", chain, providers)
	}
}

func TestResolveFallbackChain_RoundRobinGated(t *testing.T) {
	// baml-roundrobin should NOT use the BuildRequest path because the
	// orchestrator has no cross-request state to distribute load.
	fallbackChains := map[string][]string{
		"MyRoundRobin": {"ClientA", "ClientB"},
	}
	clientProviders := map[string]string{
		"MyRoundRobin": "baml-roundrobin",
		"ClientA":      "openai",
		"ClientB":      "anthropic",
	}

	adapter := &mockAdapter{Context: context.Background()}
	chain, providers := ResolveFallbackChain(
		adapter, "MyRoundRobin", fallbackChains, clientProviders,
		func(p string) bool { return true },
	)

	if chain != nil || providers != nil {
		t.Errorf("expected nil for baml-roundrobin (should use legacy path), got chain=%v providers=%v", chain, providers)
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
	chain, providers := ResolveFallbackChain(
		adapter, "MyFallback", fallbackChains, clientProviders,
		func(p string) bool { return p == "openai" || p == "anthropic" },
	)

	if len(chain) != 2 {
		t.Fatalf("expected chain length 2 for baml-fallback, got %d", len(chain))
	}
	if providers["ClientA"] != "openai" || providers["ClientB"] != "anthropic" {
		t.Errorf("unexpected providers: %v", providers)
	}
}

func TestRunStreamOrchestration_FallbackChainResetBetweenChildren(t *testing.T) {
	// Primary streams partial data ("stale") then abruptly closes the
	// connection (simulating a mid-stream failure). Secondary streams
	// "good" data and completes normally. The test verifies that:
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
