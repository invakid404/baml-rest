package buildrequest

import (
	"context"
	"errors"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"unsafe"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
)

// Stage 2 (#487): the call orchestrator owns the ExecuteBorrowed lifecycle.
// This file pins the copy-before-release property in the normal build;
// call_orchestrator_borrow_debug_test.go pins the per-attempt release ordering
// using the debug-build live-borrow gauge.
//
// Everything that escapes an attempt (raw/reasoning under NeedsRaw, raw on
// parse-failure, the diagnostic body on extraction-failure) must be an OWNED
// copy taken before the per-attempt Release, not an alias of the borrowed
// buffer. Observed deterministically: the escaped string no longer shares a
// backing array with the borrowed body view (different unsafe.StringData) while
// still carrying the correct content. The fasthttp lane is forced so
// resp.BodyString() genuinely aliases pooled transport storage (the net/http
// lane is already owned and would not exercise the borrow).

// forceFastClient returns a client pinned to the fasthttp backend, so unary
// responses are genuinely borrowed (BodyString aliases pooled storage). The
// orchestrator routes the streamed fasthttp lane here (onSuccess is non-nil),
// whose drain buffer is the borrowed storage Release returns to the pool.
func forceFastClient(server *httptest.Server) *llmhttp.Client {
	return llmhttp.NewClientWithOptions(llmhttp.ClientOptions{
		Mode:          llmhttp.ClientModeFastHTTP,
		NetHTTPClient: server.Client(),
	})
}

// strPtr returns the backing-array pointer of s, for aliasing/ownership checks.
func strPtr(s string) unsafe.Pointer {
	return unsafe.Pointer(unsafe.StringData(s))
}

// lastError drains out and returns the terminal StreamResultKindError, failing
// if none was emitted.
func lastError(t *testing.T, out <-chan bamlutils.StreamResult) bamlutils.StreamResult {
	t.Helper()
	var errRes bamlutils.StreamResult
	var found bool
	for r := range out {
		if r.Kind() == bamlutils.StreamResultKindError {
			errRes = r
			found = true
		}
	}
	if !found {
		t.Fatal("no error result emitted")
	}
	return errRes
}

// TestRunCallOrchestration_BorrowEscapesAreOwnedCopies proves that every value
// crossing the attempt boundary is cloned out of the borrowed buffer before the
// per-attempt Release. Each sub-test injects a Stage-3-style extractor that
// returns aliases of the borrowed responseBody; the orchestrator must hand back
// values backed by different storage (and the correct content). The pointer
// check is the teeth: were the clone dropped, the escaped value would still
// share a backing array with the borrowed buffer.
func TestRunCallOrchestration_BorrowEscapesAreOwnedCopies(t *testing.T) {
	const body = `{"choices":[{"message":{"content":"hello world"}}]}`

	t.Run("success raw+reasoning under NeedsRaw", func(t *testing.T) {
		server := makeJSONServer(200, body)
		defer server.Close()
		client := forceFastClient(server)

		const wantReasoning = "think"
		var aliasRaw, aliasReasoning unsafe.Pointer
		extractor := func(provider, responseBody string, includeReasoning bool) (string, string, string, error) {
			// raw aliases the whole borrowed buffer; reasoning aliases a slice of
			// it (both share the borrowed body's backing array).
			raw := responseBody
			reasoning := responseBody[:len(wantReasoning)]
			aliasRaw = strPtr(raw)
			aliasReasoning = strPtr(reasoning)
			return "parsed", raw, reasoning, nil
		}

		out := make(chan bamlutils.StreamResult, 16)
		err := RunCallOrchestration(
			context.Background(), out,
			&CallConfig{Provider: "openai", NeedsRaw: true},
			client, makeBuildCallRequest(server.URL),
			identityParseFinal, extractor, nil, nil, newTestResult,
		)
		close(out)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		final := lastFinal(t, out)

		if final.Raw() != body {
			t.Errorf("raw = %q, want %q", final.Raw(), body)
		}
		if final.Reasoning() != body[:len(wantReasoning)] {
			t.Errorf("reasoning = %q, want %q", final.Reasoning(), body[:len(wantReasoning)])
		}
		if strPtr(final.Raw()) == aliasRaw {
			t.Error("raw was NOT copied before Release — final still aliases the borrowed buffer")
		}
		if strPtr(final.Reasoning()) == aliasReasoning {
			t.Error("reasoning was NOT copied before Release — final still aliases the borrowed buffer")
		}
	})

	t.Run("parse-failure raw", func(t *testing.T) {
		server := makeJSONServer(200, body)
		defer server.Close()
		client := forceFastClient(server)

		var aliasRaw unsafe.Pointer
		extractor := func(provider, responseBody string, includeReasoning bool) (string, string, string, error) {
			raw := responseBody // aliases the borrowed buffer
			aliasRaw = strPtr(raw)
			return "parsed", raw, "", nil
		}
		failParse := func(_ context.Context, _ string) (any, error) {
			return nil, errors.New("boom")
		}

		out := make(chan bamlutils.StreamResult, 16)
		err := RunCallOrchestration(
			context.Background(), out,
			&CallConfig{Provider: "openai", NeedsRaw: true},
			client, makeBuildCallRequest(server.URL),
			failParse, extractor, nil, nil, newTestResult,
		)
		close(out)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		errRes := lastError(t, out)

		if errRes.Raw() != body {
			t.Errorf("parse-failure raw = %q, want the extracted raw %q", errRes.Raw(), body)
		}
		if strPtr(errRes.Raw()) == aliasRaw {
			t.Error("parse-failure raw was NOT copied before Release — it still aliases the borrowed buffer")
		}
	})

	t.Run("real borrowed extractor is routed and its escaping raw is owned", func(t *testing.T) {
		// End-to-end through the REAL ExtractResponseContentBorrowed on the
		// fasthttp borrow lane. The extracted content aliases the borrowed
		// buffer in-attempt; Stage 2 clones raw out before the per-attempt
		// Release. Under -tags llmhttp_debug, Release poisons the recycled
		// buffer, so a missed clone would surface here as a corrupted raw —
		// making this the orchestrator-level use-after-free guard.
		const content = "hello world"
		const body = `{"choices":[{"message":{"content":"hello world"}}]}`
		server := makeJSONServer(200, body)
		defer server.Close()
		client := forceFastClient(server)

		var borrowedCalled, stringCalled atomic.Bool
		borrowedExtractor := func(provider string, b []byte, incl bool) (string, string, string, error) {
			borrowedCalled.Store(true)
			return ExtractResponseContentBorrowed(provider, b, incl)
		}
		stringExtractor := func(provider, responseBody string, incl bool) (string, string, string, error) {
			stringCalled.Store(true)
			return ExtractResponseContent(provider, responseBody, incl)
		}

		out := make(chan bamlutils.StreamResult, 16)
		err := RunCallOrchestration(
			context.Background(), out,
			&CallConfig{Provider: "openai", NeedsRaw: true},
			client, makeBuildCallRequest(server.URL),
			identityParseFinal, stringExtractor, nil, borrowedExtractor, newTestResult,
		)
		close(out)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		final := lastFinal(t, out)

		if !borrowedCalled.Load() {
			t.Error("borrowed extractor was NOT routed on the fasthttp borrow lane")
		}
		if stringCalled.Load() {
			t.Error("string extractor ran even though a borrowed extractor was supplied for the borrow lane")
		}
		// Correct value AND owned: if raw still aliased the borrowed buffer it
		// would read poison garbage under the debug build.
		if final.Raw() != content {
			t.Errorf("raw = %q, want %q (corrupted raw under the debug build means a missed clone-before-Release)", final.Raw(), content)
		}
		if final.Final() != content {
			t.Errorf("final = %v, want %q", final.Final(), content)
		}
	})

	t.Run("extraction-failure body", func(t *testing.T) {
		server := makeJSONServer(200, body)
		defer server.Close()
		client := forceFastClient(server)

		var aliasBody unsafe.Pointer
		extractor := func(provider, responseBody string, includeReasoning bool) (string, string, string, error) {
			aliasBody = strPtr(responseBody) // the borrowed body backing array
			return "", "", "", errors.New("extract boom")
		}

		out := make(chan bamlutils.StreamResult, 16)
		err := RunCallOrchestration(
			context.Background(), out,
			&CallConfig{Provider: "openai", NeedsRaw: true},
			client, makeBuildCallRequest(server.URL),
			identityParseFinal, extractor, nil, nil, newTestResult,
		)
		close(out)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		errRes := lastError(t, out)

		if errRes.Raw() != body {
			t.Errorf("extraction-failure raw = %q, want the full body %q", errRes.Raw(), body)
		}
		if strPtr(errRes.Raw()) == aliasBody {
			t.Error("extraction-failure body was NOT copied before Release — it still aliases the borrowed buffer")
		}
	})
}
