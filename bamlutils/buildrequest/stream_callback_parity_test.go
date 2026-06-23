package buildrequest

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/bamlutils/sse"
	"github.com/invakid404/baml-rest/bamlutils/sseclient"
)

// accumulated mirrors tryOneStreamChild's per-channel accumulation so the two
// transport+extraction paths can be compared field-by-field.
type accumulated struct {
	parseable string
	raw       string
	reasoning string
}

// accumulateViaChannel consumes the server's SSE stream the OLD way: the
// channel-based ExecuteStream + sse.ExtractDeltaPartsFromText on the
// (string) Event.Data.
func accumulateViaChannel(t *testing.T, client *llmhttp.Client, url, provider string, includeReasoning bool) (accumulated, error) {
	t.Helper()
	resp, err := client.ExecuteStream(context.Background(), &llmhttp.Request{URL: url, Method: "POST", Body: `{}`})
	if err != nil {
		return accumulated{}, err
	}
	defer resp.Close()

	var acc accumulated
	for ev := range resp.Events {
		delta, err := sse.ExtractDeltaPartsFromText(provider, ev.Data, includeReasoning)
		if err != nil {
			return acc, err
		}
		acc.raw += delta.Raw
		acc.parseable += delta.Parseable
		acc.reasoning += delta.Reasoning
	}
	if err := <-resp.Errc; err != nil {
		return acc, err
	}
	return acc, nil
}

// accumulateViaCallback consumes the server's SSE stream the NEW way: the
// synchronous ExecuteStreamCallback + sse.ExtractDeltaPartsFromBytes on the
// (byte) EventBytes.Data — never materializing a durable Event.Data string.
func accumulateViaCallback(t *testing.T, client *llmhttp.Client, url, provider string, includeReasoning bool) (accumulated, error) {
	t.Helper()
	handle, err := client.ExecuteStreamCallback(context.Background(), &llmhttp.Request{URL: url, Method: "POST", Body: `{}`})
	if err != nil {
		return accumulated{}, err
	}
	defer handle.Close()

	var acc accumulated
	var cbErr error
	streamErr := handle.ForEach(context.Background(), func(ev sseclient.EventBytes) error {
		delta, err := sse.ExtractDeltaPartsFromBytes(provider, ev.Data, includeReasoning)
		if err != nil {
			cbErr = err
			return sseclient.ErrStopScan
		}
		acc.raw += delta.Raw
		acc.parseable += delta.Parseable
		acc.reasoning += delta.Reasoning
		return nil
	})
	if cbErr != nil {
		return acc, cbErr
	}
	return acc, streamErr
}

// makeAnthropicThinkingServer emits interleaved thinking_delta and text_delta
// events plus a [DONE]-style terminal, to exercise reasoning parity.
func makeAnthropicThinkingServer(thinking, text []string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\"}\n\n")
		for _, th := range thinking {
			fmt.Fprintf(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"%s\"}}\n\n", th)
		}
		for _, tx := range text {
			fmt.Fprintf(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"%s\"}}\n\n", tx)
		}
		fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}))
}

// makeGoogleMultiPartServer emits a single event whose parts[] mixes a
// thought-tagged part and answer parts, exercising multi-part parity.
func makeGoogleMultiPartServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, "data: {\"candidates\":[{\"content\":{\"parts\":["+
			"{\"text\":\"reasoning here\",\"thought\":true},"+
			"{\"text\":\"answer one\"},"+
			"{\"text\":\" answer two\"}]}}]}\n\n")
		fmt.Fprint(w, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\" tail\"}]}}]}\n\n")
	}))
}

func TestStreamCallbackParity_AllProviders(t *testing.T) {
	type scenario struct {
		name             string
		provider         string
		server           *httptest.Server
		includeReasoning bool
	}

	scenarios := []scenario{
		{"openai", "openai", makeOpenAIServer([]string{"Hello", " world", "!"}), false},
		{"azure", "azure-openai", makeOpenAIServer([]string{"Az", "ure"}), false},
		{"ollama", "ollama", makeOpenAIServer([]string{"oll", "ama"}), false},
		{"openrouter", "openrouter", makeOpenAIServer([]string{"open", "router"}), false},
		{"anthropic_text", "anthropic", makeAnthropicServer([]string{"Hello", " from", " Claude"}), false},
		{"anthropic_thinking_off", "anthropic", makeAnthropicThinkingServer([]string{"plan ", "more"}, []string{"final ", "answer"}), false},
		{"anthropic_thinking_on", "anthropic", makeAnthropicThinkingServer([]string{"plan ", "more"}, []string{"final ", "answer"}), true},
		{"openai_responses", "openai-responses", makeOpenAIResponsesServer([]string{"Hello", " responses"}), false},
		{"google_simple", "google-ai", makeGoogleAIServer([]string{"Hello", " Gemini"}), false},
		{"vertex_simple", "vertex-ai", makeGoogleAIServer([]string{"Vertex", " resp"}), false},
		{"google_multipart_off", "google-ai", makeGoogleMultiPartServer(), false},
		{"google_multipart_on", "google-ai", makeGoogleMultiPartServer(), true},
	}

	for _, sc := range scenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			defer sc.server.Close()
			client := llmhttp.NewClient(nil)

			chanAcc, chanErr := accumulateViaChannel(t, client, sc.server.URL, sc.provider, sc.includeReasoning)
			cbAcc, cbErr := accumulateViaCallback(t, client, sc.server.URL, sc.provider, sc.includeReasoning)

			if (chanErr == nil) != (cbErr == nil) {
				t.Fatalf("terminal error mismatch: channel=%v callback=%v", chanErr, cbErr)
			}
			if chanAcc != cbAcc {
				t.Errorf("accumulation mismatch (includeReasoning=%v):\n channel=%#v\ncallback=%#v",
					sc.includeReasoning, chanAcc, cbAcc)
			}
			// Sanity: the callback path actually accumulated something.
			if cbAcc.parseable == "" && cbAcc.reasoning == "" {
				t.Errorf("callback path accumulated nothing for %s", sc.name)
			}
		})
	}
}

// TestStreamCallbackParity_MidStreamTruncation verifies both paths surface a
// terminal error on an aborted chunked stream (io.ErrUnexpectedEOF), so the
// orchestrator's retry gating behaves identically.
func TestStreamCallbackParity_MidStreamTruncation(t *testing.T) {
	makeServer := func() *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			flusher, _ := w.(http.Flusher)
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n")
			if flusher != nil {
				flusher.Flush()
			}
			// Abort mid-stream: panic with ErrAbortHandler drops the chunked
			// framing without a terminating zero chunk.
			panic(http.ErrAbortHandler)
		}))
	}

	server := makeServer()
	defer server.Close()
	client := llmhttp.NewClient(nil)

	_, chanErr := accumulateViaChannel(t, client, server.URL, "openai", false)
	_, cbErr := accumulateViaCallback(t, client, server.URL, "openai", false)

	if chanErr == nil || cbErr == nil {
		t.Fatalf("expected truncation errors, got channel=%v callback=%v", chanErr, cbErr)
	}
	// Both should mention the same body-read failure shape.
	if !strings.Contains(chanErr.Error(), "failed to read response body") ||
		!strings.Contains(cbErr.Error(), "failed to read response body") {
		t.Errorf("expected both to be body-read errors:\n channel=%v\ncallback=%v", chanErr, cbErr)
	}
}
