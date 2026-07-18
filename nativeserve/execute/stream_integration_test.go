//go:build nanollm_integration

package execute

// De-BAML Phase 7B native stream EXECUTION proofs against a REAL loopback SSE
// server (not a mock RoundTripper) driven by a REAL nanollm client. Gated by
// nanollm_integration.
//
// The one-request/one-socket assertions count REAL ACCEPTED TCP CONNECTIONS via
// the server's ConnState hook (StateNew), so a nanollm retry/fallback regression
// or a second RoundTrip shows up as a real second connection — never merely a
// mock call count. Every base is a literal 127.0.0.1 loopback URL and every
// credential is a literal fake.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/internal/nativebody"
	"github.com/invakid404/baml-rest/internal/nativeprompt"
	nanollm "github.com/viktordanov/nanollm-ffi/go"
)

const (
	streamAlias  = "7b-openai"
	streamTarget = "7b-openai-target"
	streamAPIKey = "sk-7b-fake"
)

func strptr(s string) *string { return &s }

// streamServer is a real loopback SSE server that counts accepted TCP
// connections and captures the last request body/path.
type streamServer struct {
	srv   *httptest.Server
	conns atomic.Int64

	mu       sync.Mutex
	lastBody []byte
	lastPath string
}

// newStreamServer starts a loopback server whose handler is h. conns counts
// StateNew transitions — one per physical TCP connection (keep-alive is disabled
// on the exact stream transport, so one request == one connection).
func newStreamServer(t *testing.T, h func(w http.ResponseWriter, r *http.Request, s *streamServer)) *streamServer {
	t.Helper()
	s := &streamServer{}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.lastBody = body
		s.lastPath = r.URL.Path
		s.mu.Unlock()
		h(w, r, s)
	}))
	srv.Config.ConnState = func(_ net.Conn, st http.ConnState) {
		if st == http.StateNew {
			s.conns.Add(1)
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)
	s.srv = srv
	return s
}

func (s *streamServer) capturedBody() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastBody
}

// newStreamClient builds a real nanollm client with one OpenAI alias bound to the
// loopback base, no ambient env, and maxRetries same-model retries.
func newStreamClient(t *testing.T, baseURL string, maxRetries int) *nanollm.Client {
	t.Helper()
	c, err := nanollm.New(nanollm.Config{
		Models: []nanollm.ModelConfig{{
			Name:       streamAlias,
			Model:      "openai/" + streamTarget,
			APIKey:     streamAPIKey,
			BaseURL:    baseURL + "/v1",
			MaxRetries: maxRetries,
		}},
		UseProcessEnv: false,
	})
	if err != nil {
		t.Fatalf("nanollm.New: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// unaryStreamBody builds the UNARY canonical body (model+messages) the executor
// hands DoStream; the engine injects the streaming suffix during Prepare.
func unaryStreamBody(t *testing.T) []byte {
	t.Helper()
	rendered := &nativeprompt.RenderedPrompt{
		Kind: nativeprompt.KindChat,
		Messages: []nativeprompt.RenderedMessage{
			{Role: "system", Parts: []nativeprompt.Part{{Text: strptr("You are concise.")}}},
			{Role: "user", Parts: []nativeprompt.Part{{Text: strptr("say hi")}}},
		},
	}
	body, err := nativebody.BuildOpenAIChat(rendered, nativebody.ClientIntent{
		Provider:    nativebody.ProviderOpenAI,
		TargetModel: streamTarget,
		ModelAlias:  streamAlias,
	})
	if err != nil {
		t.Fatalf("BuildOpenAIChat: %v", err)
	}
	return body.Bytes()
}

// expectedStreamPlan prepares the streaming plan on the real client and lifts it
// into the neutral exact-attempt carrier the exact stream client compares against.
func expectedStreamPlan(t *testing.T, c *nanollm.Client, body []byte) *llmhttp.ExactAttemptRequest {
	t.Helper()
	prep, err := c.Prepare(nanollm.Request{Model: streamAlias, Body: body, Type: nanollm.ChatCompletion, Stream: true})
	if err != nil {
		t.Fatalf("Prepare(stream): %v", err)
	}
	if !prep.Meta.Stream || prep.ResponseFormat != nanollm.FormatSSE {
		t.Fatalf("prepared plan is not a stream plan: stream=%v format=%q", prep.Meta.Stream, prep.ResponseFormat)
	}
	return &llmhttp.ExactAttemptRequest{
		Method:      prep.Method,
		URL:         prep.URL,
		Headers:     headerFields(prep.Headers),
		Body:        prep.Body,
		BodyPresent: true,
	}
}

// writeSSE writes one SSE data event and flushes so the client reads incrementally.
func writeSSE(w http.ResponseWriter, data string) {
	fmt.Fprintf(w, "data: %s\n\n", data)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

const (
	chunkHel     = `{"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"Hel"},"finish_reason":null}]}`
	chunkLo      = `{"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":"stop"}]}`
	chunkUsage   = `{"id":"c","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":4,"completion_tokens":2,"total_tokens":6}}`
	chunkReason  = `{"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"reasoning_content":"think"},"finish_reason":null}]}`
	chunkEmptyRl = `{"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`
)

// TestRunStreamOneSocketHappyPath is the core proof: one physical native request
// (exactly one accepted TCP connection) yields normalized deltas + usage, and the
// bytes the server received are the admitted plan byte-for-byte.
func TestRunStreamOneSocketHappyPath(t *testing.T) {
	s := newStreamServer(t, func(w http.ResponseWriter, r *http.Request, s *streamServer) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		writeSSE(w, chunkEmptyRl) // role-only: no spurious partial
		writeSSE(w, chunkHel)
		writeSSE(w, chunkLo)
		writeSSE(w, chunkUsage) // usage-only: no spurious partial
		writeSSE(w, "[DONE]")
	})
	client := newStreamClient(t, s.srv.URL, 0)
	body := unaryStreamBody(t)
	expected := expectedStreamPlan(t, client, body)

	var claims, headers, firstBodies atomic.Int64
	var parseable, raw strings.Builder
	res, err := RunStream(context.Background(), StreamConfig{
		Client:            client,
		Request:           nanollm.Request{Model: streamAlias, Body: body, Type: nanollm.ChatCompletion},
		Expected:          expected,
		OnClaim:           func() { claims.Add(1) },
		OnResponseHeaders: func() { headers.Add(1) },
		OnFirstBody:       func() { firstBodies.Add(1) },
		EmitDelta: func(d StreamDelta) error {
			parseable.WriteString(d.ParseableDelta)
			raw.WriteString(d.RawDelta)
			if d.ReasoningDelta != "" {
				t.Errorf("unexpected reasoning delta with IncludeReasoning=false: %q", d.ReasoningDelta)
			}
			return nil
		},
	})
	if err != nil {
		t.Fatalf("RunStream: %v", err)
	}

	if got := s.conns.Load(); got != 1 {
		t.Errorf("accepted TCP connections = %d, want exactly 1", got)
	}
	if claims.Load() != 1 {
		t.Errorf("OnClaim fired %d times, want 1", claims.Load())
	}
	if headers.Load() != 1 || firstBodies.Load() != 1 {
		t.Errorf("liveness callbacks = headers:%d firstBody:%d, want 1/1", headers.Load(), firstBodies.Load())
	}
	if parseable.String() != "Hello" || raw.String() != "Hello" {
		t.Errorf("reassembled parseable=%q raw=%q, want Hello/Hello", parseable.String(), raw.String())
	}
	if res.PartialCount != 2 {
		t.Errorf("PartialCount = %d, want 2 (role-only + usage-only emit nothing)", res.PartialCount)
	}
	if res.Usage == nil || res.Usage.PromptTokens != 4 || res.Usage.CompletionTokens != 2 {
		t.Errorf("usage = %+v, want prompt=4 completion=2", res.Usage)
	}

	// The bytes the real server received ARE the admitted plan (exact request).
	if got := s.capturedBody(); string(got) != string(expected.Body) {
		t.Errorf("server received body != admitted plan body\n got: %s\nwant: %s", got, expected.Body)
	}
}

// TestRunStreamReasoningOptIn proves reasoning_content is surfaced only when the
// caller opts in, and never leaks into the parseable/raw channels.
func TestRunStreamReasoningOptIn(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request, s *streamServer) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		writeSSE(w, chunkReason)
		writeSSE(w, chunkHel)
		writeSSE(w, "[DONE]")
	}

	for _, include := range []bool{true, false} {
		include := include
		t.Run(fmt.Sprintf("include=%v", include), func(t *testing.T) {
			s := newStreamServer(t, handler)
			client := newStreamClient(t, s.srv.URL, 0)
			body := unaryStreamBody(t)
			expected := expectedStreamPlan(t, client, body)

			var reasoning, parseable strings.Builder
			_, err := RunStream(context.Background(), StreamConfig{
				Client:           client,
				Request:          nanollm.Request{Model: streamAlias, Body: body, Type: nanollm.ChatCompletion},
				Expected:         expected,
				IncludeReasoning: include,
				EmitDelta: func(d StreamDelta) error {
					reasoning.WriteString(d.ReasoningDelta)
					parseable.WriteString(d.ParseableDelta)
					return nil
				},
			})
			if err != nil {
				t.Fatalf("RunStream: %v", err)
			}
			if parseable.String() != "Hel" {
				t.Errorf("parseable = %q, want Hel (reasoning never enters parseable)", parseable.String())
			}
			wantReasoning := ""
			if include {
				wantReasoning = "think"
			}
			if reasoning.String() != wantReasoning {
				t.Errorf("reasoning = %q, want %q", reasoning.String(), wantReasoning)
			}
			if s.conns.Load() != 1 {
				t.Errorf("connections = %d, want 1", s.conns.Load())
			}
		})
	}
}

// TestRunStreamProviderStatusErrorD11 proves a stream-start non-2xx is preserved
// as nanollm's *ProviderStatusError (D11) — status + bounded native body — never
// normalized into the uniform envelope, and ProviderStatusHTTPError maps it to
// the public HTTPError. Exactly one connection.
func TestRunStreamProviderStatusErrorD11(t *testing.T) {
	const errBody = `{"error":{"message":"7b-overloaded","type":"overloaded_error"}}`
	s := newStreamServer(t, func(w http.ResponseWriter, r *http.Request, s *streamServer) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(503)
		io.WriteString(w, errBody)
	})
	client := newStreamClient(t, s.srv.URL, 0)
	body := unaryStreamBody(t)
	expected := expectedStreamPlan(t, client, body)

	var claims atomic.Int64
	res, err := RunStream(context.Background(), StreamConfig{
		Client:    client,
		Request:   nanollm.Request{Model: streamAlias, Body: body, Type: nanollm.ChatCompletion},
		Expected:  expected,
		OnClaim:   func() { claims.Add(1) },
		EmitDelta: func(StreamDelta) error { t.Fatal("EmitDelta must not fire on a stream-start non-2xx"); return nil },
	})
	if err == nil {
		t.Fatalf("RunStream returned nil error on a 503, want a terminal ProviderStatusError")
	}
	if res != nil {
		t.Errorf("res = %+v, want nil on a start failure", res)
	}
	if s.conns.Load() != 1 {
		t.Errorf("connections = %d, want exactly 1 (the single 503)", s.conns.Load())
	}
	if claims.Load() != 1 {
		t.Errorf("OnClaim fired %d times, want 1 (the socket was claimed)", claims.Load())
	}

	var te *TerminalError
	if !errors.As(err, &te) || te.Phase != StreamPhaseStatus {
		t.Fatalf("terminal error = %v, want phase=status", err)
	}
	var pse *nanollm.ProviderStatusError
	if !errors.As(err, &pse) {
		t.Fatalf("error does not preserve *nanollm.ProviderStatusError (D11): %v", err)
	}
	if pse.Status != 503 || !strings.Contains(string(pse.Body), "7b-overloaded") {
		t.Errorf("ProviderStatusError = {%d, %s}, want 503 + native body", pse.Status, pse.Body)
	}
	// The native body must NOT be the uniform envelope.
	if strings.Contains(string(pse.Body), "server_error") || strings.Contains(string(pse.Body), `"details"`) {
		t.Errorf("D11 body looks like the uniform envelope, must be native: %s", pse.Body)
	}
	// Mapping to the public HTTPError preserves the real status + bounded body.
	he, ok := ProviderStatusHTTPError(err)
	if !ok || he.StatusCode != 503 || !strings.Contains(he.Body, "7b-overloaded") {
		t.Errorf("ProviderStatusHTTPError = (%v, %v), want 503 + native body", he, ok)
	}
}

// TestRunStreamInvalidContentType proves a 2xx whose Content-Type is not
// text/event-stream is a typed protocol terminal — one connection (the socket was
// claimed), never a decode of a non-SSE body.
func TestRunStreamInvalidContentType(t *testing.T) {
	s := newStreamServer(t, func(w http.ResponseWriter, r *http.Request, s *streamServer) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		io.WriteString(w, `{"not":"sse"}`)
	})
	client := newStreamClient(t, s.srv.URL, 0)
	body := unaryStreamBody(t)
	expected := expectedStreamPlan(t, client, body)

	_, err := RunStream(context.Background(), StreamConfig{
		Client:    client,
		Request:   nanollm.Request{Model: streamAlias, Body: body, Type: nanollm.ChatCompletion},
		Expected:  expected,
		EmitDelta: func(StreamDelta) error { return nil },
	})
	if err == nil {
		t.Fatal("RunStream returned nil error on a non-SSE 2xx, want a typed protocol terminal")
	}
	if s.conns.Load() != 1 {
		t.Errorf("connections = %d, want 1 (2xx headers were received)", s.conns.Load())
	}
	var te *TerminalError
	if !errors.As(err, &te) || te.Phase != StreamPhaseProtocol {
		t.Fatalf("terminal error = %v, want phase=protocol", err)
	}
	var cte *llmhttp.ExactStreamContentTypeError
	if !errors.As(err, &cte) {
		t.Fatalf("error does not carry *ExactStreamContentTypeError: %v", err)
	}
}

// TestRunStreamForcedSecondRoundTripZeroSecondSocket proves the one-shot gate:
// even when the engine WOULD retry (MaxRetries=1 on a retryable 500), the exact
// client refuses the second RoundTrip with ZERO second socket — exactly one
// accepted connection, and the refusal surfaces as ErrExactStreamSecondRoundTrip.
func TestRunStreamForcedSecondRoundTripZeroSecondSocket(t *testing.T) {
	s := newStreamServer(t, func(w http.ResponseWriter, r *http.Request, s *streamServer) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(500)
		io.WriteString(w, `{"error":{"message":"boom"}}`)
	})
	client := newStreamClient(t, s.srv.URL, 1) // same-model retry budget of 1
	body := unaryStreamBody(t)
	expected := expectedStreamPlan(t, client, body)

	_, err := RunStream(context.Background(), StreamConfig{
		Client:    client,
		Request:   nanollm.Request{Model: streamAlias, Body: body, Type: nanollm.ChatCompletion},
		Expected:  expected,
		EmitDelta: func(StreamDelta) error { return nil },
	})
	if err == nil {
		t.Fatal("RunStream returned nil error, want a terminal (refused second RoundTrip)")
	}
	if got := s.conns.Load(); got != 1 {
		t.Fatalf("accepted TCP connections = %d, want exactly 1 (the second RoundTrip must open ZERO sockets)", got)
	}
	if !errors.Is(err, llmhttp.ErrExactStreamSecondRoundTrip) {
		t.Fatalf("error is not ErrExactStreamSecondRoundTrip: %v", err)
	}
	var te *TerminalError
	if !errors.As(err, &te) || te.Phase != StreamPhaseProtocol {
		t.Errorf("terminal phase = %v, want protocol", te)
	}
}

// TestRunStreamPlanMismatchZeroSocket proves an admitted-plan drift is caught with
// ZERO sockets (before the claim): OnClaim never fires and no connection opens.
func TestRunStreamPlanMismatchZeroSocket(t *testing.T) {
	s := newStreamServer(t, func(w http.ResponseWriter, r *http.Request, s *streamServer) {
		t.Error("server was reached; a plan mismatch must open zero sockets")
		w.WriteHeader(200)
	})
	client := newStreamClient(t, s.srv.URL, 0)
	body := unaryStreamBody(t)
	expected := expectedStreamPlan(t, client, body)
	// Tamper the expected plan body so DoStream's re-prepared request no longer
	// matches — a zero-socket mismatch.
	expected.Body = append([]byte(nil), expected.Body...)
	expected.Body[len(expected.Body)-1] = ' '

	var claims atomic.Int64
	_, err := RunStream(context.Background(), StreamConfig{
		Client:    client,
		Request:   nanollm.Request{Model: streamAlias, Body: body, Type: nanollm.ChatCompletion},
		Expected:  expected,
		OnClaim:   func() { claims.Add(1) },
		EmitDelta: func(StreamDelta) error { return nil },
	})
	if err == nil {
		t.Fatal("RunStream returned nil error on a plan mismatch, want a zero-socket terminal")
	}
	if s.conns.Load() != 0 {
		t.Errorf("accepted connections = %d, want 0 (mismatch is pre-claim)", s.conns.Load())
	}
	if claims.Load() != 0 {
		t.Errorf("OnClaim fired %d times, want 0 (mismatch is pre-claim)", claims.Load())
	}
	var mm *llmhttp.ExactStreamPlanMismatchError
	if !errors.As(err, &mm) {
		t.Fatalf("error does not carry *ExactStreamPlanMismatchError: %v", err)
	}
}

// TestRunStreamEmitErrorStopsReading proves an EmitDelta error stops reading
// immediately (terminal, never retried): one connection, phase=emit.
func TestRunStreamEmitErrorStopsReading(t *testing.T) {
	s := newStreamServer(t, func(w http.ResponseWriter, r *http.Request, s *streamServer) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		writeSSE(w, chunkHel)
		writeSSE(w, chunkLo)
		writeSSE(w, "[DONE]")
	})
	client := newStreamClient(t, s.srv.URL, 0)
	body := unaryStreamBody(t)
	expected := expectedStreamPlan(t, client, body)

	stopErr := errors.New("orchestrator asked to stop")
	var emits atomic.Int64
	res, err := RunStream(context.Background(), StreamConfig{
		Client:   client,
		Request:  nanollm.Request{Model: streamAlias, Body: body, Type: nanollm.ChatCompletion},
		Expected: expected,
		EmitDelta: func(StreamDelta) error {
			emits.Add(1)
			return stopErr
		},
	})
	if err == nil {
		t.Fatal("RunStream returned nil error, want the EmitDelta terminal")
	}
	if emits.Load() != 1 {
		t.Errorf("EmitDelta fired %d times, want 1 (reading stops on the first error)", emits.Load())
	}
	if res == nil || res.PartialCount != 0 {
		t.Errorf("res = %+v, want PartialCount 0 (the failed delta is not counted)", res)
	}
	var te *TerminalError
	if !errors.As(err, &te) || te.Phase != StreamPhaseEmit {
		t.Fatalf("terminal error = %v, want phase=emit", err)
	}
	if !errors.Is(err, stopErr) {
		t.Errorf("error does not wrap the EmitDelta cause")
	}
	if s.conns.Load() != 1 {
		t.Errorf("connections = %d, want 1", s.conns.Load())
	}
}

// TestRunStreamCancellationTerminal proves caller cancellation mid-stream is a
// prompt terminal classified as cancel, wrapping the context error, with no retry.
func TestRunStreamCancellationTerminal(t *testing.T) {
	release := make(chan struct{})
	s := newStreamServer(t, func(w http.ResponseWriter, r *http.Request, s *streamServer) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		writeSSE(w, chunkHel)
		// Block until the client cancels (or the test releases), holding the stream
		// open so the next client Read observes the cancellation.
		select {
		case <-r.Context().Done():
		case <-release:
		}
	})
	t.Cleanup(func() { close(release) })
	client := newStreamClient(t, s.srv.URL, 0)
	body := unaryStreamBody(t)
	expected := expectedStreamPlan(t, client, body)

	ctx, cancel := context.WithCancel(context.Background())
	var emits atomic.Int64
	res, err := RunStream(ctx, StreamConfig{
		Client:   client,
		Request:  nanollm.Request{Model: streamAlias, Body: body, Type: nanollm.ChatCompletion},
		Expected: expected,
		EmitDelta: func(StreamDelta) error {
			// Cancel after consuming the first delta; the next Read unblocks terminal.
			if emits.Add(1) == 1 {
				cancel()
			}
			return nil
		},
	})
	defer cancel()
	if err == nil {
		t.Fatal("RunStream returned nil error after cancellation, want a cancel terminal")
	}
	var te *TerminalError
	if !errors.As(err, &te) || te.Phase != StreamPhaseCancel {
		t.Fatalf("terminal error = %v, want phase=cancel", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error does not wrap context.Canceled: %v", err)
	}
	if res == nil || res.PartialCount != 1 {
		t.Errorf("res = %+v, want PartialCount 1 (one delta before cancel)", res)
	}
	if s.conns.Load() != 1 {
		t.Errorf("connections = %d, want 1", s.conns.Load())
	}
}

// TestRunStreamNilGuards proves the pre-socket config guards reject bad wiring
// before any network work (a caller/wiring bug, never a claimed failure).
func TestRunStreamNilGuards(t *testing.T) {
	s := newStreamServer(t, func(w http.ResponseWriter, r *http.Request, s *streamServer) {
		t.Error("server was reached; a config guard must fail before any socket")
	})
	client := newStreamClient(t, s.srv.URL, 0)
	body := unaryStreamBody(t)
	good := StreamConfig{
		Client:    client,
		Request:   nanollm.Request{Model: streamAlias, Body: body, Type: nanollm.ChatCompletion},
		Expected:  &llmhttp.ExactAttemptRequest{Method: "POST", URL: "http://x/y", Body: body, BodyPresent: true},
		EmitDelta: func(StreamDelta) error { return nil },
	}

	cases := map[string]func(c *StreamConfig){
		"nil client":    func(c *StreamConfig) { c.Client = nil },
		"nil expected":  func(c *StreamConfig) { c.Expected = nil },
		"nil emitDelta": func(c *StreamConfig) { c.EmitDelta = nil },
		"empty body":    func(c *StreamConfig) { c.Request.Body = nil },
	}
	for name, mutate := range cases {
		cfg := good
		mutate(&cfg)
		if _, err := RunStream(context.Background(), cfg); err == nil {
			t.Errorf("%s: RunStream accepted an invalid config", name)
		}
	}
	if s.conns.Load() != 0 {
		t.Errorf("config guards opened %d sockets, want 0", s.conns.Load())
	}
}
