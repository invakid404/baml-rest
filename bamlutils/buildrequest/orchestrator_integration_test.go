package buildrequest

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/bamlutils/retry"
	"github.com/invakid404/baml-rest/bamlutils/sse"
)

// ============================================================================
// Provider format tests — verify delta extraction for each provider format
// ============================================================================

func makeAnthropicServer(chunks []string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		// message_start
		fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg-test\"}}\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		// content_block_start
		fmt.Fprint(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		// content deltas
		for _, chunk := range chunks {
			fmt.Fprintf(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"%s\"}}\n\n", chunk)
			if flusher != nil {
				flusher.Flush()
			}
		}
		// message_stop
		fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}))
}

func makeGoogleAIServer(chunks []string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		for _, chunk := range chunks {
			fmt.Fprintf(w, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"%s\"}],\"role\":\"model\"}}]}\n\n", chunk)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
}

func makeOpenAIResponsesServer(chunks []string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		// response.created
		fmt.Fprint(w, "data: {\"type\":\"response.created\"}\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		// output_text deltas
		for _, chunk := range chunks {
			fmt.Fprintf(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"%s\"}\n\n", chunk)
			if flusher != nil {
				flusher.Flush()
			}
		}
		// response.completed
		fmt.Fprint(w, "data: {\"type\":\"response.completed\"}\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}))
}

func runOrchestrationCollect(t *testing.T, serverURL string, provider string, chunks []string, retryPolicy *retry.Policy) ([]*testResult, error) {
	t.Helper()
	client := llmhttp.NewClient(nil) // Uses the default test server client
	// For httptest servers we need to use the server's client
	return runOrchestrationCollectWithClient(t, serverURL, provider, retryPolicy, client)
}

func runOrchestrationCollectWithClient(t *testing.T, serverURL string, provider string, retryPolicy *retry.Policy, client *llmhttp.Client) ([]*testResult, error) {
	t.Helper()
	out := make(chan bamlutils.StreamResult, 100)

	config := &StreamConfig{
		Provider:      provider,
		NeedsPartials: true,
		NeedsRaw:      true,
		RetryPolicy:   retryPolicy,
	}

	err := RunStreamOrchestration(
		context.Background(), out, config, client,
		func(ctx context.Context, clientOverride string) (*llmhttp.Request, error) {
			return &llmhttp.Request{URL: serverURL, Method: "POST", Body: `{}`}, nil
		},
		func(ctx context.Context, accumulated string) (any, error) { return accumulated, nil },
		func(ctx context.Context, accumulated string) (any, error) { return accumulated, nil },
		newTestResult,
	)
	close(out)

	var results []*testResult
	for r := range out {
		results = append(results, r.(*testResult))
	}
	return results, err
}

func TestProvider_OpenAI(t *testing.T) {
	server := makeOpenAIServer([]string{"Hello", " world", "!"})
	defer server.Close()

	results, err := runOrchestrationCollect(t, server.URL, "openai", nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	assertHasFinal(t, results, "Hello world!")
}

func TestProvider_Anthropic(t *testing.T) {
	server := makeAnthropicServer([]string{"Hello", " from", " Claude"})
	defer server.Close()

	results, err := runOrchestrationCollect(t, server.URL, "anthropic", nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	assertHasFinal(t, results, "Hello from Claude")
}

func TestProvider_GoogleAI(t *testing.T) {
	server := makeGoogleAIServer([]string{"Hello", " from", " Gemini"})
	defer server.Close()

	results, err := runOrchestrationCollect(t, server.URL, "google-ai", nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	assertHasFinal(t, results, "Hello from Gemini")
}

func TestProvider_VertexAI(t *testing.T) {
	server := makeGoogleAIServer([]string{"Vertex", " response"})
	defer server.Close()

	results, err := runOrchestrationCollect(t, server.URL, "vertex-ai", nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	assertHasFinal(t, results, "Vertex response")
}

func TestProvider_OpenAIResponses(t *testing.T) {
	server := makeOpenAIResponsesServer([]string{"Hello", " responses"})
	defer server.Close()

	results, err := runOrchestrationCollect(t, server.URL, "openai-responses", nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	assertHasFinal(t, results, "Hello responses")
}

func TestProvider_AzureOpenAI(t *testing.T) {
	server := makeOpenAIServer([]string{"Azure", " OpenAI"})
	defer server.Close()

	results, err := runOrchestrationCollect(t, server.URL, "azure-openai", nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	assertHasFinal(t, results, "Azure OpenAI")
}

func TestProvider_Ollama(t *testing.T) {
	server := makeOpenAIServer([]string{"Ollama", " response"})
	defer server.Close()

	results, err := runOrchestrationCollect(t, server.URL, "ollama", nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	assertHasFinal(t, results, "Ollama response")
}

// ============================================================================
// Error scenario tests
// ============================================================================

func TestError_HTTP429RateLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(429)
		fmt.Fprint(w, `{"error":{"message":"Rate limit exceeded","type":"rate_limit_error"}}`)
	}))
	defer server.Close()

	results, err := runOrchestrationCollectWithClient(t, server.URL, "openai", nil, llmhttp.NewClient(server.Client()))
	if err != nil {
		t.Fatalf("unexpected error from orchestration: %v", err)
	}

	assertHasError(t, results, "429")
}

func TestError_HTTP500ServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		fmt.Fprint(w, `{"error":{"message":"Internal server error"}}`)
	}))
	defer server.Close()

	results, err := runOrchestrationCollectWithClient(t, server.URL, "openai", nil, llmhttp.NewClient(server.Client()))
	if err != nil {
		t.Fatalf("unexpected error from orchestration: %v", err)
	}

	assertHasError(t, results, "500")
}

func TestError_HTTP503ServiceUnavailable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		fmt.Fprint(w, "Service Unavailable")
	}))
	defer server.Close()

	results, err := runOrchestrationCollectWithClient(t, server.URL, "openai", nil, llmhttp.NewClient(server.Client()))
	if err != nil {
		t.Fatalf("unexpected error from orchestration: %v", err)
	}

	assertHasError(t, results, "503")
}

func TestError_MidStreamDisconnect(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		flusher, ok := w.(http.Flusher)
		if !ok {
			return
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\n\n")
		flusher.Flush()
		// Close connection abruptly (simulate disconnect) by panicking the handler
		panic(http.ErrAbortHandler)
	}))
	defer server.Close()

	results, err := runOrchestrationCollectWithClient(t, server.URL, "openai", nil, llmhttp.NewClient(server.Client()))
	if err != nil {
		t.Fatalf("unexpected error from orchestration: %v", err)
	}

	// Should have an error result (either stream error or empty response)
	assertHasErrorResult(t, results)
}

func TestError_MalformedSSE(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		// Valid chunk followed by malformed JSON
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\n\n")
		fmt.Fprint(w, "data: {invalid json\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\" world\"}}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	results, err := runOrchestrationCollectWithClient(t, server.URL, "openai", nil, llmhttp.NewClient(server.Client()))
	if err != nil {
		t.Fatalf("unexpected error from orchestration: %v", err)
	}

	// Should still produce a final result despite the malformed event
	// (extractDeltaFromText returns empty string for malformed JSON via gjson)
	assertHasFinal(t, results, "Hello world")
}

func TestEmptyCompletion_LetParseFinalDecide(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		// Only send [DONE] with no content chunks — empty completion
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	client := llmhttp.NewClient(server.Client())
	out := make(chan bamlutils.StreamResult, 100)

	// parseFinal accepts empty string and returns "empty" as a valid result
	config := &StreamConfig{
		Provider:      "openai",
		NeedsPartials: true,
		NeedsRaw:      true,
	}

	RunStreamOrchestration(
		context.Background(), out, config, client,
		func(ctx context.Context, clientOverride string) (*llmhttp.Request, error) {
			return &llmhttp.Request{URL: server.URL, Method: "POST", Body: `{}`}, nil
		},
		func(ctx context.Context, accumulated string) (any, error) { return accumulated, nil },
		func(ctx context.Context, accumulated string) (any, error) {
			// Accept empty string as valid
			return "parsed:" + accumulated, nil
		},
		newTestResult,
	)
	close(out)

	// Should have a final result, NOT an error
	var gotFinal bool
	for r := range out {
		tr := r.(*testResult)
		if tr.kind == bamlutils.StreamResultKindFinal {
			gotFinal = true
			if tr.final != "parsed:" {
				t.Errorf("expected final='parsed:', got %q", tr.final)
			}
		}
		if tr.kind == bamlutils.StreamResultKindError {
			t.Errorf("should not get error for empty completion, got: %v", tr.err)
		}
	}
	if !gotFinal {
		t.Error("expected a final result for empty completion")
	}
}

// ============================================================================
// Retry tests
// ============================================================================

func TestRetry_SuccessAfterFailures(t *testing.T) {
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
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"success\"}}]}\n\n")
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

	var results []*testResult
	for r := range out {
		results = append(results, r.(*testResult))
	}

	if int(attempts.Load()) != 3 {
		t.Errorf("expected 3 attempts, got %d", attempts.Load())
	}

	// Verify reset signals were emitted for retries
	var resets int
	for _, r := range results {
		if r.reset {
			resets++
		}
	}
	if resets != 2 {
		t.Errorf("expected 2 reset signals, got %d", resets)
	}

	assertHasFinal(t, results, "success")
}

func TestRetry_AllAttemptsExhausted(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		fmt.Fprint(w, "always fails")
	}))
	defer server.Close()

	client := llmhttp.NewClient(server.Client())
	out := make(chan bamlutils.StreamResult, 100)

	config := &StreamConfig{
		Provider: "openai",
		RetryPolicy: &retry.Policy{
			MaxRetries: 2,
			Strategy:   &retry.ConstantDelay{DelayMs: 1},
		},
	}

	RunStreamOrchestration(
		context.Background(), out, config, client,
		func(ctx context.Context, clientOverride string) (*llmhttp.Request, error) {
			return &llmhttp.Request{URL: server.URL, Method: "POST", Body: `{}`}, nil
		},
		func(ctx context.Context, accumulated string) (any, error) { return nil, nil },
		func(ctx context.Context, accumulated string) (any, error) { return nil, nil },
		newTestResult,
	)
	close(out)

	var results []*testResult
	for r := range out {
		results = append(results, r.(*testResult))
	}

	assertHasErrorResult(t, results)
}

func TestRetry_ExponentialBackoff(t *testing.T) {
	var attempts atomic.Int32
	var mu sync.Mutex
	var timestamps []time.Time
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		timestamps = append(timestamps, time.Now())
		mu.Unlock()
		n := attempts.Add(1)
		if n < 3 {
			w.WriteHeader(429)
			fmt.Fprint(w, "rate limited")
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
			Strategy: &retry.ExponentialBackoff{
				DelayMs:    50,
				Multiplier: 2.0,
				MaxDelayMs: 1000,
			},
		},
	}

	RunStreamOrchestration(
		context.Background(), out, config, client,
		func(ctx context.Context, clientOverride string) (*llmhttp.Request, error) {
			return &llmhttp.Request{URL: server.URL, Method: "POST", Body: `{}`}, nil
		},
		func(ctx context.Context, accumulated string) (any, error) { return accumulated, nil },
		func(ctx context.Context, accumulated string) (any, error) { return accumulated, nil },
		newTestResult,
	)
	close(out)

	for range out {
	}

	if int(attempts.Load()) != 3 {
		t.Errorf("expected 3 attempts, got %d", attempts.Load())
	}

	// Verify delays increase (exponential backoff)
	mu.Lock()
	ts := timestamps
	mu.Unlock()
	if len(ts) >= 3 {
		delay1 := ts[1].Sub(ts[0])
		delay2 := ts[2].Sub(ts[1])
		// Second delay should be roughly 2x the first (with some tolerance)
		if delay2 < delay1 {
			t.Errorf("expected increasing delays, got delay1=%v, delay2=%v", delay1, delay2)
		}
	}
}

// ============================================================================
// Heartbeat and stream lifecycle tests
// ============================================================================

func TestHeartbeat_SentOnce(t *testing.T) {
	server := makeOpenAIServer([]string{"a", "b", "c"})
	defer server.Close()

	client := llmhttp.NewClient(server.Client())
	out := make(chan bamlutils.StreamResult, 100)

	config := &StreamConfig{
		Provider:      "openai",
		NeedsPartials: true,
	}

	RunStreamOrchestration(
		context.Background(), out, config, client,
		func(ctx context.Context, clientOverride string) (*llmhttp.Request, error) {
			return &llmhttp.Request{URL: server.URL, Method: "POST", Body: `{}`}, nil
		},
		func(ctx context.Context, accumulated string) (any, error) { return accumulated, nil },
		func(ctx context.Context, accumulated string) (any, error) { return accumulated, nil },
		newTestResult,
	)
	close(out)

	var heartbeats int
	for r := range out {
		if r.Kind() == bamlutils.StreamResultKindHeartbeat {
			heartbeats++
		}
	}

	if heartbeats != 1 {
		t.Errorf("expected exactly 1 heartbeat, got %d", heartbeats)
	}
}

func TestStreamResultOrder(t *testing.T) {
	server := makeOpenAIServer([]string{"Hello", " world"})
	defer server.Close()

	client := llmhttp.NewClient(server.Client())
	out := make(chan bamlutils.StreamResult, 100)

	config := &StreamConfig{
		Provider:      "openai",
		NeedsPartials: true,
		NeedsRaw:      true,
	}

	RunStreamOrchestration(
		context.Background(), out, config, client,
		func(ctx context.Context, clientOverride string) (*llmhttp.Request, error) {
			return &llmhttp.Request{URL: server.URL, Method: "POST", Body: `{}`}, nil
		},
		func(ctx context.Context, accumulated string) (any, error) { return accumulated, nil },
		func(ctx context.Context, accumulated string) (any, error) { return accumulated, nil },
		newTestResult,
	)
	close(out)

	var results []*testResult
	for r := range out {
		results = append(results, r.(*testResult))
	}

	// Verify order: heartbeat first, then partials, then final
	if len(results) < 3 {
		t.Fatalf("expected at least 3 results (heartbeat + partial + final), got %d", len(results))
	}

	if results[0].kind != bamlutils.StreamResultKindHeartbeat {
		t.Errorf("first result should be heartbeat, got kind %d", results[0].kind)
	}

	lastResult := results[len(results)-1]
	if lastResult.kind != bamlutils.StreamResultKindFinal {
		t.Errorf("last result should be final, got kind %d", lastResult.kind)
	}

	// All middle results should be stream partials
	for i := 1; i < len(results)-1; i++ {
		if results[i].kind != bamlutils.StreamResultKindStream {
			t.Errorf("result[%d] should be stream, got kind %d", i, results[i].kind)
		}
	}
}

func TestRawAccumulation(t *testing.T) {
	server := makeOpenAIServer([]string{"a", "b", "c"})
	defer server.Close()

	client := llmhttp.NewClient(server.Client())
	out := make(chan bamlutils.StreamResult, 100)

	config := &StreamConfig{
		Provider:      "openai",
		NeedsPartials: true,
		NeedsRaw:      true,
	}

	RunStreamOrchestration(
		context.Background(), out, config, client,
		func(ctx context.Context, clientOverride string) (*llmhttp.Request, error) {
			return &llmhttp.Request{URL: server.URL, Method: "POST", Body: `{}`}, nil
		},
		func(ctx context.Context, accumulated string) (any, error) { return accumulated, nil },
		func(ctx context.Context, accumulated string) (any, error) { return accumulated, nil },
		newTestResult,
	)
	close(out)

	// Each partial should have a raw delta, and final should have full text
	var partialRaws []string
	var finalRaw string
	for r := range out {
		tr := r.(*testResult)
		if tr.kind == bamlutils.StreamResultKindStream {
			partialRaws = append(partialRaws, tr.raw)
		}
		if tr.kind == bamlutils.StreamResultKindFinal {
			finalRaw = tr.raw
		}
	}

	if finalRaw != "abc" {
		t.Errorf("expected final raw 'abc', got %q", finalRaw)
	}

	// All partials should have non-empty raw deltas
	for i, raw := range partialRaws {
		if raw == "" {
			t.Errorf("partial[%d] raw should not be empty", i)
		}
	}
}

// ============================================================================
// Bedrock fallback test
// ============================================================================

func TestBedrock_NotSupported(t *testing.T) {
	if IsProviderSupported("aws-bedrock") {
		t.Error("aws-bedrock should NOT be supported by BuildRequest path")
	}
}

func TestProvider_UnknownFallsBack(t *testing.T) {
	// Unknown/future providers must NOT be supported — they should fall
	// back to the legacy path instead of entering BuildRequest and failing.
	unknowns := []string{
		"some-future-provider",
		"custom-provider",
		"mistral",
		"cohere",
		"",
	}
	for _, p := range unknowns {
		if IsProviderSupported(p) {
			t.Errorf("unknown provider %q should NOT be supported", p)
		}
	}
}

func TestProvider_WhitelistMatchesExtractor(t *testing.T) {
	// Every supported provider must be recognized by ExtractDeltaFromText.
	// This ensures IsProviderSupported and ExtractDeltaFromText stay in sync.
	supported := []string{
		"openai", "openai-generic", "azure-openai", "ollama", "openrouter",
		"openai-responses", "anthropic", "google-ai", "vertex-ai",
	}
	for _, p := range supported {
		if !IsProviderSupported(p) {
			t.Errorf("provider %q should be supported", p)
		}
	}

	// Round-trip: verify each supported provider is actually handled by ExtractDeltaFromText
	sampleChunks := map[string]string{
		"openai":           `{"choices":[{"delta":{"content":"test"}}]}`,
		"openai-generic":   `{"choices":[{"delta":{"content":"test"}}]}`,
		"azure-openai":     `{"choices":[{"delta":{"content":"test"}}]}`,
		"ollama":           `{"choices":[{"delta":{"content":"test"}}]}`,
		"openrouter":       `{"choices":[{"delta":{"content":"test"}}]}`,
		"openai-responses": `{"type":"response.output_text.delta","delta":"test"}`,
		"anthropic":        `{"type":"content_block_delta","delta":{"type":"text_delta","text":"test"}}`,
		"google-ai":        `{"candidates":[{"content":{"parts":[{"text":"test"}]}}]}`,
		"vertex-ai":        `{"candidates":[{"content":{"parts":[{"text":"test"}]}}]}`,
	}
	for provider, chunk := range sampleChunks {
		delta, err := sse.ExtractDeltaFromText(provider, chunk, false)
		if err != nil {
			t.Errorf("ExtractDeltaFromText(%q) returned error: %v", provider, err)
		}
		if delta != "test" {
			t.Errorf("ExtractDeltaFromText(%q) = %q, want %q", provider, delta, "test")
		}
	}
}

// ============================================================================
// Helpers
// ============================================================================

func assertHasFinal(t *testing.T, results []*testResult, expectedRaw string) {
	t.Helper()
	for _, r := range results {
		if r.kind == bamlutils.StreamResultKindFinal {
			if r.raw != expectedRaw {
				t.Errorf("expected final raw %q, got %q", expectedRaw, r.raw)
			}
			return
		}
	}
	t.Errorf("no final result found in %d results", len(results))
}

func assertHasError(t *testing.T, results []*testResult, containsText string) {
	t.Helper()
	for _, r := range results {
		if r.kind == bamlutils.StreamResultKindError {
			if r.err == nil {
				t.Error("error result should have non-nil error")
				return
			}
			if !strings.Contains(r.err.Error(), containsText) {
				t.Errorf("error should contain %q, got: %v", containsText, r.err)
			}
			return
		}
	}
	t.Errorf("no error result found in %d results", len(results))
}

func assertHasErrorResult(t *testing.T, results []*testResult) {
	t.Helper()
	for _, r := range results {
		if r.kind == bamlutils.StreamResultKindError {
			return
		}
	}
	t.Errorf("no error result found in %d results", len(results))
}
