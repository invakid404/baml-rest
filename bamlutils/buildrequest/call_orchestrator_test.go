package buildrequest

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/bamlutils/retry"
)

// makeJSONServer creates a mock LLM server returning a non-streaming JSON response.
func makeJSONServer(statusCode int, body string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(statusCode)
		fmt.Fprint(w, body)
	}))
}

func makeBuildCallRequest(serverURL string) BuildCallRequestFunc {
	return func(ctx context.Context) (*llmhttp.Request, error) {
		return &llmhttp.Request{
			URL:    serverURL,
			Method: "POST",
			Body:   `{"model":"gpt-4","stream":false}`,
		}, nil
	}
}

func identityParseFinal(_ context.Context, text string) (any, error) {
	return text, nil
}

func TestRunCallOrchestration_Success(t *testing.T) {
	server := makeJSONServer(200, `{"choices":[{"message":{"content":"Hello world"}}]}`)
	defer server.Close()

	client := llmhttp.NewClient(server.Client())
	out := make(chan bamlutils.StreamResult, 100)

	config := &CallConfig{
		Provider: "openai",
		NeedsRaw: false,
	}

	err := RunCallOrchestration(
		context.Background(), out, config, client,
		makeBuildCallRequest(server.URL),
		identityParseFinal,
		ExtractResponseContent,
		newTestResult,
	)
	close(out)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var results []bamlutils.StreamResult
	for r := range out {
		results = append(results, r)
	}

	// Expect heartbeat + final
	if len(results) != 2 {
		t.Fatalf("expected 2 results (heartbeat + final), got %d", len(results))
	}
	if results[0].Kind() != bamlutils.StreamResultKindHeartbeat {
		t.Errorf("expected first result to be heartbeat, got %v", results[0].Kind())
	}
	if results[1].Kind() != bamlutils.StreamResultKindFinal {
		t.Errorf("expected second result to be final, got %v", results[1].Kind())
	}
	if results[1].Final() != "Hello world" {
		t.Errorf("expected final 'Hello world', got %v", results[1].Final())
	}
	if results[1].Raw() != "" {
		t.Errorf("expected empty raw (NeedsRaw=false), got %q", results[1].Raw())
	}
}

func TestRunCallOrchestration_WithRaw(t *testing.T) {
	server := makeJSONServer(200, `{"choices":[{"message":{"content":"test output"}}]}`)
	defer server.Close()

	client := llmhttp.NewClient(server.Client())
	out := make(chan bamlutils.StreamResult, 100)

	config := &CallConfig{
		Provider: "openai",
		NeedsRaw: true,
	}

	err := RunCallOrchestration(
		context.Background(), out, config, client,
		makeBuildCallRequest(server.URL),
		identityParseFinal,
		ExtractResponseContent,
		newTestResult,
	)
	close(out)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var results []bamlutils.StreamResult
	for r := range out {
		results = append(results, r)
	}

	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	final := results[1]
	if final.Kind() != bamlutils.StreamResultKindFinal {
		t.Fatalf("expected final result, got %v", final.Kind())
	}
	if final.Raw() != "test output" {
		t.Errorf("expected raw 'test output', got %q", final.Raw())
	}
}

func TestRunCallOrchestration_HTTPError(t *testing.T) {
	server := makeJSONServer(429, `{"error":{"message":"rate limited"}}`)
	defer server.Close()

	client := llmhttp.NewClient(server.Client())
	out := make(chan bamlutils.StreamResult, 100)

	config := &CallConfig{
		Provider: "openai",
	}

	_ = RunCallOrchestration(
		context.Background(), out, config, client,
		makeBuildCallRequest(server.URL),
		identityParseFinal,
		ExtractResponseContent,
		newTestResult,
	)
	close(out)

	var results []bamlutils.StreamResult
	for r := range out {
		results = append(results, r)
	}

	// Should get only an error result (no heartbeat since Execute() failed
	// and the heartbeat only fires after a successful HTTP response).
	if len(results) != 1 {
		t.Fatalf("expected 1 result (error), got %d", len(results))
	}
	if results[0].Kind() != bamlutils.StreamResultKindError {
		t.Fatalf("expected error result, got %v", results[0].Kind())
	}
	if results[0].Error() == nil {
		t.Fatal("expected non-nil error")
	}
	if !strings.Contains(results[0].Error().Error(), "429") {
		t.Errorf("expected error to mention 429, got: %v", results[0].Error())
	}
}

func TestRunCallOrchestration_WithRetry(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempt := int(attempts.Add(1))
		if attempt <= 2 {
			w.WriteHeader(500)
			fmt.Fprint(w, `{"error":"temporary"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		fmt.Fprint(w, `{"choices":[{"message":{"content":"retry success"}}]}`)
	}))
	defer server.Close()

	client := llmhttp.NewClient(server.Client())
	out := make(chan bamlutils.StreamResult, 100)

	config := &CallConfig{
		Provider:    "openai",
		RetryPolicy: &retry.Policy{MaxRetries: 3, Strategy: &retry.ConstantDelay{DelayMs: 1}},
	}

	err := RunCallOrchestration(
		context.Background(), out, config, client,
		makeBuildCallRequest(server.URL),
		identityParseFinal,
		ExtractResponseContent,
		newTestResult,
	)
	close(out)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var results []bamlutils.StreamResult
	for r := range out {
		results = append(results, r)
	}

	// Should succeed on 3rd attempt: heartbeat + final
	hasFinal := false
	for _, r := range results {
		if r.Kind() == bamlutils.StreamResultKindFinal {
			hasFinal = true
			if r.Final() != "retry success" {
				t.Errorf("expected 'retry success', got %v", r.Final())
			}
		}
	}
	if !hasFinal {
		t.Fatal("expected a final result after retry")
	}

	if attempts.Load() != 3 {
		t.Errorf("expected 3 attempts, got %d", attempts.Load())
	}
}

func TestRunCallOrchestration_RetryExhausted(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(500)
		fmt.Fprint(w, `{"error":"always fails"}`)
	}))
	defer server.Close()

	client := llmhttp.NewClient(server.Client())
	out := make(chan bamlutils.StreamResult, 100)

	maxRetries := 2
	config := &CallConfig{
		Provider:    "openai",
		RetryPolicy: &retry.Policy{MaxRetries: maxRetries, Strategy: &retry.ConstantDelay{DelayMs: 1}},
	}

	_ = RunCallOrchestration(
		context.Background(), out, config, client,
		makeBuildCallRequest(server.URL),
		identityParseFinal,
		ExtractResponseContent,
		newTestResult,
	)
	close(out)

	// Verify exact attempt count: initial + maxRetries
	expectedAttempts := int32(maxRetries + 1)
	if got := attempts.Load(); got != expectedAttempts {
		t.Errorf("expected %d attempts (1 initial + %d retries), got %d", expectedAttempts, maxRetries, got)
	}

	// Should get an error after exhausting retries
	hasError := false
	for r := range out {
		if r.Kind() == bamlutils.StreamResultKindError {
			hasError = true
		}
	}
	if !hasError {
		t.Fatal("expected error result after exhausting retries")
	}
}

func TestRunCallOrchestration_ContextCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Simulate slow response
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	client := llmhttp.NewClient(server.Client())
	out := make(chan bamlutils.StreamResult, 100)

	config := &CallConfig{
		Provider: "openai",
	}

	_ = RunCallOrchestration(
		ctx, out, config, client,
		makeBuildCallRequest(server.URL),
		identityParseFinal,
		ExtractResponseContent,
		newTestResult,
	)
	close(out)

	// On context cancellation, neither final nor error results should be emitted
	for r := range out {
		switch r.Kind() {
		case bamlutils.StreamResultKindFinal:
			t.Fatal("should not get final result after cancellation")
		case bamlutils.StreamResultKindError:
			t.Fatal("should not get error result after cancellation")
		}
	}
}

func TestRunCallOrchestration_CancellationDuringParse(t *testing.T) {
	// If the context is cancelled during parseFinal (after a successful HTTP
	// response), no error result should be emitted — same suppression
	// behavior as the streaming orchestrator.
	server := makeJSONServer(200, `{"choices":[{"message":{"content":"ok"}}]}`)
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())

	client := llmhttp.NewClient(server.Client())
	out := make(chan bamlutils.StreamResult, 100)

	config := &CallConfig{
		Provider: "openai",
	}

	_ = RunCallOrchestration(
		ctx, out, config, client,
		makeBuildCallRequest(server.URL),
		func(_ context.Context, text string) (any, error) {
			// Cancel the context during parsing
			cancel()
			return nil, ctx.Err()
		},
		ExtractResponseContent,
		newTestResult,
	)
	close(out)

	for r := range out {
		switch r.Kind() {
		case bamlutils.StreamResultKindFinal:
			t.Fatal("should not get final result after cancellation during parse")
		case bamlutils.StreamResultKindError:
			t.Fatal("should not get error result after cancellation during parse")
		}
	}
}

func TestRunCallOrchestration_UnsupportedProvider(t *testing.T) {
	out := make(chan bamlutils.StreamResult, 100)
	config := &CallConfig{
		Provider: "aws-bedrock",
	}

	err := RunCallOrchestration(
		context.Background(), out, config, nil,
		func(ctx context.Context) (*llmhttp.Request, error) {
			return &llmhttp.Request{URL: "http://localhost", Method: "POST"}, nil
		},
		identityParseFinal,
		ExtractResponseContent,
		newTestResult,
	)

	if err == nil {
		t.Fatal("expected error for unsupported provider")
	}
	if !strings.Contains(err.Error(), "unsupported") {
		t.Errorf("expected unsupported provider error, got: %v", err)
	}
}

func TestRunCallOrchestration_EmptyProvider(t *testing.T) {
	out := make(chan bamlutils.StreamResult, 100)
	config := &CallConfig{
		Provider: "",
	}

	err := RunCallOrchestration(
		context.Background(), out, config, nil,
		func(ctx context.Context) (*llmhttp.Request, error) {
			return nil, nil
		},
		identityParseFinal,
		ExtractResponseContent,
		newTestResult,
	)

	if err == nil {
		t.Fatal("expected error for empty provider")
	}
}

func TestRunCallOrchestration_BuildRequestError(t *testing.T) {
	out := make(chan bamlutils.StreamResult, 100)
	config := &CallConfig{
		Provider: "openai",
	}

	_ = RunCallOrchestration(
		context.Background(), out, config, llmhttp.DefaultClient,
		func(ctx context.Context) (*llmhttp.Request, error) {
			return nil, fmt.Errorf("build request failed")
		},
		identityParseFinal,
		ExtractResponseContent,
		newTestResult,
	)
	close(out)

	var results []bamlutils.StreamResult
	for r := range out {
		results = append(results, r)
	}

	if len(results) != 1 {
		t.Fatalf("expected 1 error result, got %d", len(results))
	}
	if results[0].Kind() != bamlutils.StreamResultKindError {
		t.Fatalf("expected error, got %v", results[0].Kind())
	}
	if !strings.Contains(results[0].Error().Error(), "build request failed") {
		t.Errorf("unexpected error: %v", results[0].Error())
	}
}

func TestRunCallOrchestration_ParseFinalError(t *testing.T) {
	server := makeJSONServer(200, `{"choices":[{"message":{"content":"unparseable"}}]}`)
	defer server.Close()

	client := llmhttp.NewClient(server.Client())
	out := make(chan bamlutils.StreamResult, 100)

	config := &CallConfig{
		Provider: "openai",
	}

	_ = RunCallOrchestration(
		context.Background(), out, config, client,
		makeBuildCallRequest(server.URL),
		func(_ context.Context, text string) (any, error) {
			return nil, fmt.Errorf("parse failed for: %s", text)
		},
		ExtractResponseContent,
		newTestResult,
	)
	close(out)

	var results []bamlutils.StreamResult
	for r := range out {
		results = append(results, r)
	}

	hasError := false
	for _, r := range results {
		if r.Kind() == bamlutils.StreamResultKindError {
			hasError = true
			if !strings.Contains(r.Error().Error(), "parse failed") {
				t.Errorf("unexpected error: %v", r.Error())
			}
		}
	}
	if !hasError {
		t.Fatal("expected parse error result")
	}
}

func TestRunCallOrchestration_Anthropic(t *testing.T) {
	body := `{"content":[{"type":"text","text":"Anthropic response"}],"stop_reason":"end_turn"}`
	server := makeJSONServer(200, body)
	defer server.Close()

	client := llmhttp.NewClient(server.Client())
	out := make(chan bamlutils.StreamResult, 100)

	config := &CallConfig{
		Provider: "anthropic",
		NeedsRaw: true,
	}

	err := RunCallOrchestration(
		context.Background(), out, config, client,
		makeBuildCallRequest(server.URL),
		identityParseFinal,
		ExtractResponseContent,
		newTestResult,
	)
	close(out)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var results []bamlutils.StreamResult
	for r := range out {
		results = append(results, r)
	}

	hasFinal := false
	for _, r := range results {
		if r.Kind() == bamlutils.StreamResultKindFinal {
			hasFinal = true
			if r.Final() != "Anthropic response" {
				t.Errorf("expected 'Anthropic response', got %v", r.Final())
			}
			if r.Raw() != "Anthropic response" {
				t.Errorf("expected raw 'Anthropic response', got %q", r.Raw())
			}
		}
	}
	if !hasFinal {
		t.Fatal("expected final result")
	}
}

func TestRunCallOrchestration_AnthropicThinkingSplit(t *testing.T) {
	// End-to-end test proving the parseable/raw split is wired correctly
	// through RunCallOrchestration. The mock server returns a response with
	// both thinking and text blocks. parseFinal should receive ONLY the
	// text content, while Raw() should contain thinking + text.
	body := `{"content":[{"type":"thinking","thinking":"Step 1: reason..."},{"type":"text","text":"The answer is 42"}],"stop_reason":"end_turn"}`
	server := makeJSONServer(200, body)
	defer server.Close()

	client := llmhttp.NewClient(server.Client())
	out := make(chan bamlutils.StreamResult, 100)

	config := &CallConfig{
		Provider: "anthropic",
		NeedsRaw: true,
	}

	// Track what parseFinal actually received
	var parseFinalInput string
	captureParseFinal := func(_ context.Context, text string) (any, error) {
		parseFinalInput = text
		return text, nil
	}

	err := RunCallOrchestration(
		context.Background(), out, config, client,
		makeBuildCallRequest(server.URL),
		captureParseFinal,
		ExtractResponseContent,
		newTestResult,
	)
	close(out)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify parseFinal received only the text content (no thinking)
	if parseFinalInput != "The answer is 42" {
		t.Errorf("parseFinal received %q, expected only 'The answer is 42' (no thinking)", parseFinalInput)
	}

	// Verify the final result's Raw() contains both thinking + text
	var results []bamlutils.StreamResult
	for r := range out {
		results = append(results, r)
	}

	hasFinal := false
	for _, r := range results {
		if r.Kind() == bamlutils.StreamResultKindFinal {
			hasFinal = true
			expectedRaw := "Step 1: reason...The answer is 42"
			if r.Raw() != expectedRaw {
				t.Errorf("Raw() = %q, expected %q (thinking + text)", r.Raw(), expectedRaw)
			}
			if r.Final() != "The answer is 42" {
				t.Errorf("Final() = %v, expected 'The answer is 42'", r.Final())
			}
		}
	}
	if !hasFinal {
		t.Fatal("no final result found")
	}
}

func TestRunCallOrchestration_GoogleAI(t *testing.T) {
	body := `{"candidates":[{"content":{"parts":[{"text":"Google response"}],"role":"model"}}]}`
	server := makeJSONServer(200, body)
	defer server.Close()

	client := llmhttp.NewClient(server.Client())
	out := make(chan bamlutils.StreamResult, 100)

	config := &CallConfig{
		Provider: "google-ai",
		NeedsRaw: true,
	}

	err := RunCallOrchestration(
		context.Background(), out, config, client,
		makeBuildCallRequest(server.URL),
		identityParseFinal,
		ExtractResponseContent,
		newTestResult,
	)
	close(out)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for r := range out {
		if r.Kind() == bamlutils.StreamResultKindFinal {
			if r.Final() != "Google response" {
				t.Errorf("expected 'Google response', got %v", r.Final())
			}
			return
		}
	}
	t.Fatal("no final result")
}

func TestRunCallOrchestration_HeartbeatOnlyOnce(t *testing.T) {
	server := makeJSONServer(200, `{"choices":[{"message":{"content":"ok"}}]}`)
	defer server.Close()

	client := llmhttp.NewClient(server.Client())
	out := make(chan bamlutils.StreamResult, 100)

	config := &CallConfig{
		Provider: "openai",
	}

	_ = RunCallOrchestration(
		context.Background(), out, config, client,
		makeBuildCallRequest(server.URL),
		identityParseFinal,
		ExtractResponseContent,
		newTestResult,
	)
	close(out)

	heartbeats := 0
	for r := range out {
		if r.Kind() == bamlutils.StreamResultKindHeartbeat {
			heartbeats++
		}
	}
	if heartbeats != 1 {
		t.Errorf("expected exactly 1 heartbeat, got %d", heartbeats)
	}
}

func TestRunCallOrchestration_RetryHeartbeats(t *testing.T) {
	// Heartbeats are emitted both on retry (to maintain liveness during
	// backoff) and on success (via the onSuccess callback). With 2 failed
	// attempts and 1 success: 2 onRetry heartbeats + 1 onSuccess heartbeat.
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempt := int(attempts.Add(1))
		if attempt <= 2 {
			w.WriteHeader(500)
			fmt.Fprint(w, `{"error":"temporary"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		fmt.Fprint(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer server.Close()

	client := llmhttp.NewClient(server.Client())
	out := make(chan bamlutils.StreamResult, 100)

	config := &CallConfig{
		Provider:    "openai",
		RetryPolicy: &retry.Policy{MaxRetries: 3, Strategy: &retry.ConstantDelay{DelayMs: 1}},
	}

	err := RunCallOrchestration(
		context.Background(), out, config, client,
		makeBuildCallRequest(server.URL),
		identityParseFinal,
		ExtractResponseContent,
		newTestResult,
	)
	close(out)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var results []bamlutils.StreamResult
	for r := range out {
		results = append(results, r)
	}

	// 2 onRetry heartbeats (before backoff sleep) + 1 onSuccess heartbeat
	// (after 2xx on 3rd attempt) + 1 final result = 4 results minimum.
	heartbeats := 0
	hasFinal := false
	for _, r := range results {
		switch r.Kind() {
		case bamlutils.StreamResultKindHeartbeat:
			heartbeats++
		case bamlutils.StreamResultKindFinal:
			hasFinal = true
		}
	}

	if !hasFinal {
		t.Fatal("expected a final result")
	}
	if heartbeats < 3 {
		t.Errorf("expected at least 3 heartbeats (2 onRetry + 1 onSuccess), got %d", heartbeats)
	}
}

func TestIsCallProviderSupported(t *testing.T) {
	supported := []string{"openai", "openai-generic", "azure-openai", "ollama", "openrouter", "anthropic", "google-ai", "vertex-ai"}
	for _, p := range supported {
		if !IsCallProviderSupported(p) {
			t.Errorf("expected %q to be supported", p)
		}
	}

	unsupported := []string{"aws-bedrock", "openai-responses", "unknown", ""}
	for _, p := range unsupported {
		if IsCallProviderSupported(p) {
			t.Errorf("expected %q to be unsupported", p)
		}
	}
}

func TestRunCallOrchestration_NilHTTPClient(t *testing.T) {
	// Pass nil httpClient to verify the nil → DefaultClient fallback path
	// works end-to-end. The test server uses plain HTTP, so DefaultClient's
	// default transport can connect to it directly.
	server := makeJSONServer(200, `{"choices":[{"message":{"content":"from default client"}}]}`)
	defer server.Close()

	out := make(chan bamlutils.StreamResult, 100)
	config := &CallConfig{
		Provider: "openai",
	}

	_ = RunCallOrchestration(
		context.Background(), out, config, nil,
		makeBuildCallRequest(server.URL),
		identityParseFinal,
		ExtractResponseContent,
		newTestResult,
	)
	close(out)

	// Verify the response comes from the actual server, not just "no panic".
	hasFinal := false
	for r := range out {
		if r.Kind() == bamlutils.StreamResultKindFinal {
			hasFinal = true
			if r.Final() != "from default client" {
				t.Errorf("expected 'from default client', got %v", r.Final())
			}
		}
	}
	if !hasFinal {
		t.Fatal("expected final result from nil-client fallback path")
	}
}
