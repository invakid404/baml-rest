package buildrequest

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
)

// TestRunCallOrchestration_ByteExtractorSeam pins the injection contract for
// the net/http unary lane (regression guard for #492): the byte copy-skip
// path must never silently bypass a caller-injected extractor.
//
//   - When a byte extractor is supplied, it is the one actually invoked on a
//     net/http response (resp.BodyBytes != nil) and its output flows through.
//   - When NO byte extractor is supplied, the injected STRING extractor is
//     still honored on the net/http lane (it is NOT bypassed) — only the
//     allocation behavior changes, never which extractor runs.
//
// Both sub-tests drive a real net/http llmhttp client against an httptest
// server, so resp.BodyBytes is populated and the byte routing branch is
// genuinely exercised (not the fasthttp fallback).
func TestRunCallOrchestration_ByteExtractorSeam(t *testing.T) {
	const body = `{"choices":[{"message":{"content":"wire content"}}]}`

	t.Run("custom byte extractor is invoked on net/http", func(t *testing.T) {
		server := makeJSONServer(200, body)
		defer server.Close()
		client := llmhttp.NewClientWithOptions(llmhttp.ClientOptions{Mode: llmhttp.ClientModeNetHTTP, NetHTTPClient: server.Client()})

		var byteCalled, stringCalled atomic.Bool
		var gotBody atomic.Value // []byte the byte extractor saw

		byteExtractor := func(provider string, b []byte, includeReasoning bool) (string, string, string, error) {
			byteCalled.Store(true)
			gotBody.Store(append([]byte(nil), b...))
			return "byte-parseable", "byte-raw", "", nil
		}
		stringExtractor := func(provider, responseBody string, includeReasoning bool) (string, string, string, error) {
			stringCalled.Store(true)
			return "string-parseable", "string-raw", "", nil
		}

		out := make(chan bamlutils.StreamResult, 16)
		err := RunCallOrchestration(
			context.Background(), out,
			&CallConfig{Provider: "openai", NeedsRaw: true},
			client,
			makeBuildCallRequest(server.URL),
			identityParseFinal,
			stringExtractor,
			byteExtractor,
			nil,
			newTestResult,
		)
		close(out)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if !byteCalled.Load() {
			t.Error("byte extractor was NOT invoked on the net/http lane (copy-skip path not taken)")
		}
		if stringCalled.Load() {
			t.Error("string extractor was invoked even though a byte extractor was supplied")
		}
		if gb, _ := gotBody.Load().([]byte); string(gb) != body {
			t.Errorf("byte extractor received %q, want the full response body %q", gb, body)
		}

		final := lastFinal(t, out)
		if final.Final() != "byte-parseable" {
			t.Errorf("final = %v, want byte extractor output 'byte-parseable'", final.Final())
		}
		if final.Raw() != "byte-raw" {
			t.Errorf("raw = %q, want byte extractor output 'byte-raw'", final.Raw())
		}
	})

	t.Run("string extractor honored on net/http when no byte extractor", func(t *testing.T) {
		server := makeJSONServer(200, body)
		defer server.Close()
		client := llmhttp.NewClientWithOptions(llmhttp.ClientOptions{Mode: llmhttp.ClientModeNetHTTP, NetHTTPClient: server.Client()})

		var stringCalled atomic.Bool
		stringExtractor := func(provider, responseBody string, includeReasoning bool) (string, string, string, error) {
			stringCalled.Store(true)
			if responseBody != body {
				t.Errorf("string extractor received %q, want %q", responseBody, body)
			}
			return "custom-string-parseable", "custom-string-raw", "", nil
		}

		out := make(chan bamlutils.StreamResult, 16)
		err := RunCallOrchestration(
			context.Background(), out,
			&CallConfig{Provider: "openai", NeedsRaw: true},
			client,
			makeBuildCallRequest(server.URL),
			identityParseFinal,
			stringExtractor,
			nil, nil, // no byte extractor — the injected string extractor must still run
			newTestResult,
		)
		close(out)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if !stringCalled.Load() {
			t.Fatal("custom string extractor was NOT honored on the net/http lane (byte path silently bypassed injection)")
		}
		final := lastFinal(t, out)
		if final.Final() != "custom-string-parseable" {
			t.Errorf("final = %v, want custom string extractor output", final.Final())
		}
		if final.Raw() != "custom-string-raw" {
			t.Errorf("raw = %q, want custom string extractor output", final.Raw())
		}
	})
}

// lastFinal drains out and returns the terminal StreamResultKindFinal,
// failing if none was emitted.
func lastFinal(t *testing.T, out <-chan bamlutils.StreamResult) bamlutils.StreamResult {
	t.Helper()
	var final bamlutils.StreamResult
	var found bool
	for r := range out {
		if r.Kind() == bamlutils.StreamResultKindError {
			t.Fatalf("unexpected error result: %v", r.Error())
		}
		if r.Kind() == bamlutils.StreamResultKindFinal {
			final = r
			found = true
		}
	}
	if !found {
		t.Fatal("no final result emitted")
	}
	return final
}
