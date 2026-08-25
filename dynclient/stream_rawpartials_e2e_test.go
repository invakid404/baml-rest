package dynclient

// Faithful end-to-end reproduction of the customer's raw-stream bug, run
// against the REAL BAML runtime + a real httptest OpenAI-compatible SSE
// server — the exact shape of /tmp/shared/customer-repro/repro/main.go.
//
// A one-property class schema ({value: string}) plus a plain-prose model
// response makes BAML's ParseStream/Parse return a root-coercion error for
// every partial and for the final. On clean 0.0.48 this delivered NO live
// raw partials and hard-failed the whole call. After the hotfix:
//
//   - live raw partials arrive as the prose streams (always-on), and
//   - with WithSoftFinalParse(), the final-parse miss completes
//     successfully carrying the full accumulated raw.
//
// Unlike the rest of client_test.go (which injects a fakeRuntime so the
// native BAML CFFI is never loaded), this test needs the real runtime. It
// SKIPS cleanly when the native library is unavailable, so hermetic CI
// without a cached BAML lib is never broken by it.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// e2eProseChunks is the customer's exact plain-text stream.
var e2eProseChunks = []string{"Here is ", "plain prose, ", "not structured JSON."}

// realRuntimeClient builds a dynclient backed by the real BAML runtime.
// If the native runtime cannot initialize (missing/unavailable CFFI), the
// test is skipped rather than failed — keeping this E2E opt-in to
// environments where BAML is present.
func realRuntimeClient(t *testing.T, opts ...Option) *Client {
	t.Helper()
	var (
		c   *Client
		err error
	)
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Skipf("skipping real-runtime E2E: BAML runtime unavailable: %v", r)
			}
		}()
		c, err = New(opts...)
	}()
	if err != nil {
		t.Skipf("skipping real-runtime E2E: dynclient.New: %v", err)
	}
	return c
}

// serveProseSSE starts a local OpenAI-compatible SSE server that streams
// each chunk as one chat.completion.chunk content delta, then [DONE].
func serveProseSSE(chunks []string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		flusher, _ := w.(http.Flusher)
		for i, chunk := range chunks {
			payload, err := json.Marshal(map[string]any{
				"id":     "e2e-repro",
				"object": "chat.completion.chunk",
				"model":  "local-fake-model",
				"choices": []any{map[string]any{
					"index": i,
					"delta": map[string]any{"content": chunk},
				}},
			})
			if err != nil {
				return
			}
			fmt.Fprintf(w, "data: %s\n\n", payload)
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

// proseRequest builds the customer's {value: string} request pointed at the
// local SSE server.
func proseRequest(serverURL string) Request {
	primary := "LocalFakeProvider"
	prompt := "Return the requested direct text. Do not return a JSON envelope."
	return Request{
		Messages: []Message{{Role: "user", TextContent: &prompt}},
		ClientRegistry: &ClientRegistry{
			Primary: &primary,
			Clients: []*ClientProperty{{
				Name:     primary,
				Provider: "openai",
				Options: map[string]any{
					"model":    "local-fake-model",
					"base_url": serverURL,
					"api_key":  "unused",
				},
			}},
		},
		OutputSchema: &OutputSchema{
			Properties: MustOrderedMap(OrderedKV("value", &Property{Type: "string"})),
		},
	}
}

// collectRawStream drives a DynamicStreamRaw stream to completion, returning
// the accumulated-raw value of each partial that carried raw text, whether a
// final event arrived (and its raw), and any terminal error from Next().
func collectRawStream(t *testing.T, stream *Stream) (rawPartials []string, sawFinal bool, finalRaw string, terminalErr error) {
	t.Helper()
	for {
		ev, err := stream.Next()
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			terminalErr = err
			return
		}
		switch ev.Kind {
		case EventPartial:
			if ev.Raw != "" {
				rawPartials = append(rawPartials, ev.Raw)
			}
		case EventFinal:
			sawFinal = true
			finalRaw = ev.Raw
		}
	}
}

// TestDynamicStreamRaw_ProsePartials_RealRuntime is the faithful E2E lock.
// It reproduces the customer bundle exactly and asserts the fixed behavior
// end-to-end through the real BAML runtime.
func TestDynamicStreamRaw_ProsePartials_RealRuntime(t *testing.T) {
	full := strings.Join(e2eProseChunks, "")

	// Fix (1), always-on: live raw partials must reach the caller as the
	// prose streams, even though BAML rejects every partial parse. The
	// default strict final parse still errors at the end (the customer's
	// terminal coerce error) — but only AFTER the live raw arrived.
	t.Run("live_raw_partials_even_when_final_parse_fails", func(t *testing.T) {
		server := serveProseSSE(e2eProseChunks)
		defer server.Close()

		c := realRuntimeClient(t, WithUseBuildRequest(true))
		stream, err := c.DynamicStreamRaw(context.Background(), proseRequest(server.URL))
		if err != nil {
			t.Fatalf("DynamicStreamRaw: %v", err)
		}
		defer stream.Close()

		rawPartials, sawFinal, _, terminalErr := collectRawStream(t, stream)

		if len(rawPartials) == 0 {
			t.Fatalf("expected live raw partials as the prose streams, got none " +
				"(regression: raw suppressed by structured-parse gating)")
		}
		if last := rawPartials[len(rawPartials)-1]; last != full {
			t.Errorf("accumulated raw reached %q, want the full prose %q", last, full)
		}
		// Default (no WithSoftFinalParse): the final structured parse
		// misses and surfaces as a terminal error, matching the customer's
		// observed final coerce error — the strict contract is unchanged.
		if terminalErr == nil {
			t.Errorf("expected a terminal final-parse error under default strict mode (sawFinal=%v)", sawFinal)
		} else if !strings.Contains(terminalErr.Error(), "coerce") &&
			!strings.Contains(terminalErr.Error(), "parse final result") {
			t.Logf("terminal error (informational): %v", terminalErr)
		}
	})

	// Fix (2), opt-in: WithSoftFinalParse turns the final-parse miss into a
	// successful raw-only final carrying the full accumulated raw.
	t.Run("WithSoftFinalParse_yields_successful_raw_final", func(t *testing.T) {
		server := serveProseSSE(e2eProseChunks)
		defer server.Close()

		c := realRuntimeClient(t, WithUseBuildRequest(true), WithSoftFinalParse())
		stream, err := c.DynamicStreamRaw(context.Background(), proseRequest(server.URL))
		if err != nil {
			t.Fatalf("DynamicStreamRaw: %v", err)
		}
		defer stream.Close()

		rawPartials, sawFinal, finalRaw, terminalErr := collectRawStream(t, stream)

		if terminalErr != nil {
			t.Fatalf("WithSoftFinalParse: expected clean completion, got terminal error: %v", terminalErr)
		}
		if len(rawPartials) == 0 {
			t.Errorf("expected live raw partials as the prose streams, got none")
		}
		if !sawFinal {
			t.Fatalf("WithSoftFinalParse: expected a successful final event")
		}
		if finalRaw != full {
			t.Errorf("final raw = %q, want the full prose %q", finalRaw, full)
		}
	})
}
