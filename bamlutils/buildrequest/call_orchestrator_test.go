package buildrequest

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
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
	return func(ctx context.Context, clientOverride string) (*llmhttp.Request, error) {
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
		func(ctx context.Context, clientOverride string) (*llmhttp.Request, error) {
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
		func(ctx context.Context, clientOverride string) (*llmhttp.Request, error) {
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
		func(ctx context.Context, clientOverride string) (*llmhttp.Request, error) {
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

func TestRunCallOrchestration_RetryDoesNotEmitRetryHeartbeats(t *testing.T) {
	// Only a real 2xx response should emit a heartbeat. Failed attempts must
	// not emit retry heartbeats, otherwise the pool's first-byte detector is
	// satisfied before any provider response has actually succeeded.
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

	// Exactly 1 heartbeat: onSuccess after the 2xx response on the 3rd attempt.
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
	if heartbeats != 1 {
		t.Errorf("expected exactly 1 heartbeat (onSuccess only), got %d", heartbeats)
	}
}

func TestRunCallOrchestration_RetryRebuildsRequest(t *testing.T) {
	// Verify that buildRequest is called on every retry attempt, not just
	// once before the retry loop. This matters for retry policies that may
	// route through different models/providers — BAML's Request API can
	// return a different HTTP request on each invocation.
	var buildCalls atomic.Int32
	var attempts atomic.Int32

	// Two servers simulating different "providers" that the retry rotates to.
	server1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		fmt.Fprint(w, `{"error":"server1 fails"}`)
	}))
	defer server1.Close()

	server2 := makeJSONServer(200, `{"choices":[{"message":{"content":"server2 ok"}}]}`)
	defer server2.Close()

	// buildRequest returns different URLs on each call, simulating a
	// retry policy that rotates across providers.
	buildFn := func(ctx context.Context, clientOverride string) (*llmhttp.Request, error) {
		call := int(buildCalls.Add(1))
		url := server1.URL
		if call > 1 {
			url = server2.URL
		}
		return &llmhttp.Request{
			URL:    url,
			Method: "POST",
			Body:   `{"model":"test","stream":false}`,
		}, nil
	}

	// Use server2's client which can reach both localhost servers.
	client := llmhttp.NewClient(server2.Client())
	out := make(chan bamlutils.StreamResult, 100)

	config := &CallConfig{
		Provider:    "openai",
		RetryPolicy: &retry.Policy{MaxRetries: 3, Strategy: &retry.ConstantDelay{DelayMs: 1}},
	}

	// Track actual HTTP attempts via a wrapper that counts.
	originalBuildFn := buildFn
	buildFn = func(ctx context.Context, clientOverride string) (*llmhttp.Request, error) {
		attempts.Add(1)
		return originalBuildFn(ctx, clientOverride)
	}

	err := RunCallOrchestration(
		context.Background(), out, config, client,
		buildFn,
		identityParseFinal,
		ExtractResponseContent,
		newTestResult,
	)
	close(out)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// buildRequest must be called on each attempt, not just once.
	if got := buildCalls.Load(); got != 2 {
		t.Errorf("expected exactly 2 buildRequest calls (one per attempt), got %d", got)
	}

	// The first attempt hits server1 (500), retry rebuilds and hits server2 (200).
	hasFinal := false
	for r := range out {
		if r.Kind() == bamlutils.StreamResultKindFinal {
			hasFinal = true
			if r.Final() != "server2 ok" {
				t.Errorf("expected 'server2 ok', got %v", r.Final())
			}
		}
	}
	if !hasFinal {
		t.Fatal("expected a final result from server2 after retry rotation")
	}
}

func TestIsCallProviderSupported(t *testing.T) {
	supported := []string{"openai", "openai-generic", "azure-openai", "ollama", "openrouter", "openai-responses", "anthropic", "google-ai", "vertex-ai"}
	for _, p := range supported {
		if !IsCallProviderSupported(p) {
			t.Errorf("expected %q to be supported", p)
		}
	}

	unsupported := []string{"aws-bedrock", "unknown", ""}
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

func TestRunCallOrchestration_FallbackChain(t *testing.T) {
	// Simulate a fallback chain: first child (openai-shaped) returns 500,
	// second child (anthropic-shaped) returns 200. The orchestrator walks
	// the entire chain per retry attempt: OpenAI fails, Anthropic succeeds.
	var attempts atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempt := int(attempts.Add(1))
		if attempt == 1 {
			w.WriteHeader(500)
			fmt.Fprint(w, `{"error":"openai down"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		fmt.Fprint(w, `{"content":[{"type":"text","text":"anthropic ok"}]}`)
	}))
	defer server.Close()

	client := llmhttp.NewClient(server.Client())
	out := make(chan bamlutils.StreamResult, 100)

	config := &CallConfig{
		Provider:      "",
		RetryPolicy:   &retry.Policy{MaxRetries: 3, Strategy: &retry.ConstantDelay{DelayMs: 1}},
		FallbackChain: []string{"OpenAIClient", "AnthropicClient"},
		ClientProviders: map[string]string{
			"OpenAIClient":    "openai",
			"AnthropicClient": "anthropic",
		},
	}

	var overrides []string
	buildFn := func(ctx context.Context, clientOverride string) (*llmhttp.Request, error) {
		overrides = append(overrides, clientOverride)
		return &llmhttp.Request{
			URL:    server.URL,
			Method: "POST",
			Body:   `{"stream":false}`,
		}, nil
	}

	err := RunCallOrchestration(
		context.Background(), out, config, client,
		buildFn,
		identityParseFinal,
		ExtractResponseContent,
		newTestResult,
	)
	close(out)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Chain walk: OpenAI (fail) → Anthropic (success), done in first attempt
	if want := []string{"OpenAIClient", "AnthropicClient"}; !slices.Equal(overrides, want) {
		t.Errorf("expected override sequence %v, got %v", want, overrides)
	}

	// Verify the result was extracted with the anthropic provider
	hasFinal := false
	for r := range out {
		if r.Kind() == bamlutils.StreamResultKindFinal {
			hasFinal = true
			if r.Final() != "anthropic ok" {
				t.Errorf("expected 'anthropic ok', got %v", r.Final())
			}
		}
	}
	if !hasFinal {
		t.Fatal("expected a final result from anthropic fallback")
	}
}

func TestRunCallOrchestration_FallbackChainWithRaw(t *testing.T) {
	// Same setup as TestRunCallOrchestration_FallbackChain but with
	// NeedsRaw=true — verifies the raw response text propagates
	// through the fallback path.
	var attempts atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempt := int(attempts.Add(1))
		if attempt == 1 {
			w.WriteHeader(500)
			fmt.Fprint(w, `{"error":"primary down"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		fmt.Fprint(w, `{"id":"chatcmpl-test","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"fallback ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
	}))
	defer server.Close()

	client := llmhttp.NewClient(server.Client())
	out := make(chan bamlutils.StreamResult, 100)

	config := &CallConfig{
		Provider:      "",
		RetryPolicy:   &retry.Policy{MaxRetries: 3, Strategy: &retry.ConstantDelay{DelayMs: 1}},
		NeedsRaw:      true,
		FallbackChain: []string{"PrimaryClient", "SecondaryClient"},
		ClientProviders: map[string]string{
			"PrimaryClient":   "openai",
			"SecondaryClient": "openai",
		},
	}

	buildFn := func(ctx context.Context, clientOverride string) (*llmhttp.Request, error) {
		return &llmhttp.Request{
			URL:    server.URL,
			Method: "POST",
			Body:   `{"stream":false}`,
		}, nil
	}

	err := RunCallOrchestration(
		context.Background(), out, config, client,
		buildFn,
		identityParseFinal,
		ExtractResponseContent,
		newTestResult,
	)
	close(out)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var hasFinal bool
	for r := range out {
		if r.Kind() == bamlutils.StreamResultKindFinal {
			hasFinal = true
			if r.Final() != "fallback ok" {
				t.Errorf("expected 'fallback ok', got %v", r.Final())
			}
			if r.Raw() != "fallback ok" {
				t.Errorf("expected Raw()='fallback ok', got %q", r.Raw())
			}
		}
	}
	if !hasFinal {
		t.Fatal("expected a final result from fallback")
	}
}

func TestRunCallOrchestration_FallbackChainExtractionFailure(t *testing.T) {
	// First child returns a valid 200 but with a body that the extraction
	// function rejects. The chain walk advances to the second child within
	// the same retry attempt: Bad(extraction fail) → Good(200 success).
	var attempts atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempt := int(attempts.Add(1))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		if attempt == 1 {
			// Malformed — not valid OpenAI/Anthropic content
			fmt.Fprint(w, `{"garbage": true}`)
			return
		}
		// Valid Anthropic-shaped response on second attempt
		fmt.Fprint(w, `{"content":[{"type":"text","text":"recovered"}]}`)
	}))
	defer server.Close()

	client := llmhttp.NewClient(server.Client())
	out := make(chan bamlutils.StreamResult, 100)

	config := &CallConfig{
		Provider:      "",
		RetryPolicy:   &retry.Policy{MaxRetries: 3, Strategy: &retry.ConstantDelay{DelayMs: 1}},
		FallbackChain: []string{"BadClient", "GoodClient"},
		ClientProviders: map[string]string{
			"BadClient":  "openai",
			"GoodClient": "anthropic",
		},
	}

	buildFn := func(ctx context.Context, clientOverride string) (*llmhttp.Request, error) {
		return &llmhttp.Request{URL: server.URL, Method: "POST", Body: `{}`}, nil
	}

	err := RunCallOrchestration(
		context.Background(), out, config, client,
		buildFn,
		identityParseFinal,
		ExtractResponseContent,
		newTestResult,
	)
	close(out)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	hasFinal := false
	for r := range out {
		if r.Kind() == bamlutils.StreamResultKindFinal {
			hasFinal = true
			if r.Final() != "recovered" {
				t.Errorf("expected 'recovered', got %v", r.Final())
			}
		}
	}
	if !hasFinal {
		t.Fatal("expected final result after extraction failure retry")
	}
	if got := int(attempts.Load()); got != 2 {
		t.Errorf("expected exactly 2 attempts (1 extraction failure + 1 success), got %d", got)
	}
}
