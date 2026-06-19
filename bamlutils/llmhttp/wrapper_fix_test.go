package llmhttp

// Targeted tests for the fasthttp unary wrapper fix (#475): the buffered fast
// lane, the synchronous streamed-header drain, the externally-cancellable lane,
// and isolation of the streaming path. These complement the existing contract
// tests (TestExecuteOnSuccessFiresBeforeBodyRead, TestExecuteExactlyAtLimit,
// TestFastExecute_OversizedResponse, etc.).

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/invakid404/baml-rest/bamlutils/sseclient"
)

// TestFastBufferedLaneSizeLimit pins exact MaxResponseBodyBytes semantics on
// the buffered fast lane (background ctx + nil onSuccess): a body exactly at
// the limit succeeds, one byte over is rejected with the size-limit error
// (fasthttp's MaxResponseBodySize maps to the same message as the net/http and
// streamed paths).
func TestFastBufferedLaneSizeLimit(t *testing.T) {
	cases := []struct {
		name    string
		size    int
		wantErr bool
	}{
		{"exactly at limit", MaxResponseBodyBytes, false},
		{"one over limit", MaxResponseBodyBytes + 1, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := strings.Repeat("x", tc.size)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(200)
				fmt.Fprint(w, body)
			}))
			defer server.Close()

			c := withFastClient(t, server.Client())
			// Background ctx (no Done) + nil onSuccess selects the buffered lane.
			resp, err := c.Execute(context.Background(), &Request{URL: server.URL, Method: "POST", Body: `{}`}, nil)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected size-limit error")
				}
				if !strings.Contains(err.Error(), "exceeds maximum size") {
					t.Errorf("expected size-limit error, got: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for body at limit: %v", err)
			}
			if len(resp.Body) != tc.size {
				t.Errorf("expected body length %d, got %d", tc.size, len(resp.Body))
			}
		})
	}
}

// TestFastBufferedLaneNoPerRequestGoroutine asserts the buffered fast lane is
// fully synchronous: a batch of sequential Execute calls leaves no residual
// goroutines (the io.ReadAll body-read goroutine and the DoDeadline goroutine
// are both gone for this lane).
func TestFastBufferedLaneNoPerRequestGoroutine(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer server.Close()

	c := withFastClient(t, server.Client())
	req := &Request{URL: server.URL, Method: "POST", Body: `{}`}

	// Warm up the pool + protocol cache so steady-state goroutines are settled.
	for i := 0; i < 20; i++ {
		if _, err := c.Execute(context.Background(), req, nil); err != nil {
			t.Fatalf("warmup failed: %v", err)
		}
	}
	runtime.GC()
	time.Sleep(50 * time.Millisecond)
	before := runtime.NumGoroutine()

	for i := 0; i < 200; i++ {
		resp, err := c.Execute(context.Background(), req, nil)
		if err != nil {
			t.Fatalf("request %d failed: %v", i, err)
		}
		if resp.Body != `{"ok":true}` {
			t.Fatalf("request %d body mismatch: %q", i, resp.Body)
		}
	}
	runtime.GC()
	time.Sleep(50 * time.Millisecond)
	after := runtime.NumGoroutine()

	// A synchronous lane must not grow the goroutine count across 200 requests.
	// Allow a tiny slack for unrelated runtime/test goroutines.
	if after > before+2 {
		t.Errorf("buffered lane appears to spawn per-request goroutines: before=%d after=%d", before, after)
	}
}

// TestFastStreamedLaneOnSuccessBeforeBody verifies the streamed-header lane
// (onSuccess != nil) still fires onSuccess after 2xx headers but before the
// body is read, on the fasthttp backend.
func TestFastStreamedLaneOnSuccessBeforeBody(t *testing.T) {
	callbackFired := make(chan struct{})
	seenBeforeBody := make(chan bool, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		select {
		case <-callbackFired:
			seenBeforeBody <- true
		case <-time.After(2 * time.Second):
			seenBeforeBody <- false
		}
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer server.Close()

	c := withFastClient(t, server.Client())
	resp, err := c.Execute(context.Background(), &Request{URL: server.URL, Method: "POST", Body: `{}`},
		func() { close(callbackFired) })
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Body != `{"ok":true}` {
		t.Errorf("unexpected body: %q", resp.Body)
	}
	if seen := <-seenBeforeBody; !seen {
		t.Fatal("onSuccess did not fire before the body on the streamed fasthttp lane")
	}
}

// TestFastCancellableLaneReturnsPromptlyOnCancel pins that a no-deadline
// context.WithCancel unary request on the fasthttp backend returns promptly
// when the caller cancels mid-flight — well before DefaultCallTimeout — even
// though fasthttp's DoDeadline cannot itself be aborted. This is the
// externally-cancellable lane's whole reason for keeping the goroutine race.
func TestFastCancellableLaneReturnsPromptlyOnCancel(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Hang until the test releases, so the only way Execute returns is the
		// caller's cancellation.
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	defer close(release)

	c := withFastClient(t, server.Client())
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := c.Execute(ctx, &Request{URL: server.URL, Method: "POST", Body: `{}`}, nil)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected cancellation error")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("Execute did not return promptly on cancel: took %v", elapsed)
	}
}

// TestExecuteStreamStillStreamsAfterUnaryHost confirms ExecuteStream keeps
// using the streaming host (StreamResponseBody=true) and parses SSE events
// after the unary buffered host was added to the cache entry.
func TestExecuteStreamStillStreamsAfterUnaryHost(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, "data: {\"delta\":\"a\"}\n\n")
		fmt.Fprint(w, "data: {\"delta\":\"b\"}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	c := withFastClient(t, server.Client())
	resp, err := c.ExecuteStream(context.Background(), &Request{
		URL:     server.URL,
		Method:  "POST",
		Headers: map[string]string{"Content-Type": "application/json"},
		Body:    `{"stream":true}`,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Close()

	var events []sseclient.Event
	for ev := range resp.Events {
		events = append(events, ev)
	}
	if streamErr := <-resp.Errc; streamErr != nil {
		t.Fatalf("unexpected stream error: %v", streamErr)
	}
	if len(events) != 3 {
		t.Fatalf("expected 3 events, got %d", len(events))
	}
	if events[2].Data != "[DONE]" {
		t.Errorf("expected [DONE], got %q", events[2].Data)
	}
}

// TestFastBufferedNon2xxHugeBody pins B2: on the buffered fast lane, a non-2xx
// response whose body exceeds MaxResponseBodyBytes must still surface as an
// *HTTPError with the body capped to MaxErrorBodyBytes — NOT a
// "response body exceeds maximum size" error. (The buffered host's
// MaxResponseBodySize is only an OOM backstop; the real limits are enforced in
// code after the status is known.)
func TestFastBufferedNon2xxHugeBody(t *testing.T) {
	hugeError := strings.Repeat("e", MaxResponseBodyBytes+4096) // > MaxResponseBodyBytes
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		fmt.Fprint(w, hugeError)
	}))
	defer server.Close()

	c := withFastClient(t, server.Client())
	// Background ctx + nil onSuccess → buffered lane.
	_, err := c.Execute(context.Background(), &Request{URL: server.URL, Method: "POST", Body: `{}`}, nil)
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
	if strings.Contains(err.Error(), "exceeds maximum size") {
		t.Fatalf("non-2xx huge body must not become a size-limit error: %v", err)
	}
	httpErr, ok := err.(*HTTPError)
	if !ok {
		t.Fatalf("expected *HTTPError, got %T: %v", err, err)
	}
	if httpErr.StatusCode != 500 {
		t.Errorf("expected status 500, got %d", httpErr.StatusCode)
	}
	if len(httpErr.Body) != MaxErrorBodyBytes {
		t.Errorf("expected error body capped to %d, got %d", MaxErrorBodyBytes, len(httpErr.Body))
	}
}

// firstThenNormalServer returns a server whose FIRST response is `first`
// (status firstStatus) and every subsequent response is the normal small JSON
// body with 200. Used to verify a partial/oversized first read doesn't corrupt
// the next request reusing the same pooled connection.
func firstThenNormalServer(t *testing.T, firstStatus int, first, normal string) *httptest.Server {
	t.Helper()
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(firstStatus)
			fmt.Fprint(w, first)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		fmt.Fprint(w, normal)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// assertCleanConnReuse fires `reps` sequential requests on the same client and
// asserts each returns the normal body — i.e. the connection pool was not
// poisoned by a prior partial read. Sequential single requests reuse the same
// pooled conn, so a dirty-pooled conn would surface here as a parse error or a
// garbage body (B1).
func assertCleanConnReuse(t *testing.T, c *Client, url, normal string, onSuccess func(), reps int) {
	t.Helper()
	for i := 0; i < reps; i++ {
		resp, err := c.Execute(context.Background(), &Request{URL: url, Method: "POST", Body: `{}`}, onSuccess)
		if err != nil {
			t.Fatalf("follow-up request %d failed (dirty pooled conn?): %v", i, err)
		}
		if resp.Body != normal {
			t.Fatalf("follow-up request %d got corrupted body (dirty pooled conn?): %q", i, resp.Body)
		}
	}
}

// TestFastStreamedConnReuseAfterOversize pins B1 for the streamed lane's
// oversized-body path: after an oversized 2xx response trips the size limit
// (LimitReader stops before EOF), the connection must be DISCARDED, not pooled,
// so the next request doesn't read leftover body bytes.
func TestFastStreamedConnReuseAfterOversize(t *testing.T) {
	normal := `{"ok":true}`
	oversized := strings.Repeat("x", MaxResponseBodyBytes+1024)
	server := firstThenNormalServer(t, 200, oversized, normal)

	c := withFastClient(t, server.Client())
	onSuccess := func() {} // non-nil → streamed lane

	_, err := c.Execute(context.Background(), &Request{URL: server.URL, Method: "POST", Body: `{}`}, onSuccess)
	if err == nil || !strings.Contains(err.Error(), "exceeds maximum size") {
		t.Fatalf("expected size-limit error on oversized response, got: %v", err)
	}
	assertCleanConnReuse(t, c, server.URL, normal, onSuccess, 5)
}

// TestFastStreamedConnReuseAfterCappedError pins B1 for the streamed lane's
// non-2xx diagnostic path: an error body larger than MaxErrorBodyBytes is read
// only up to the cap (partial), so the connection must be DISCARDED, not pooled.
func TestFastStreamedConnReuseAfterCappedError(t *testing.T) {
	normal := `{"ok":true}`
	largeError := strings.Repeat("e", MaxErrorBodyBytes*4) // > the 4KB diagnostic cap
	server := firstThenNormalServer(t, 500, largeError, normal)

	c := withFastClient(t, server.Client())
	onSuccess := func() {} // non-nil → streamed lane

	_, err := c.Execute(context.Background(), &Request{URL: server.URL, Method: "POST", Body: `{}`}, onSuccess)
	httpErr, ok := err.(*HTTPError)
	if !ok {
		t.Fatalf("expected *HTTPError, got %T: %v", err, err)
	}
	if len(httpErr.Body) != MaxErrorBodyBytes {
		t.Errorf("expected error body capped to %d, got %d", MaxErrorBodyBytes, len(httpErr.Body))
	}
	assertCleanConnReuse(t, c, server.URL, normal, onSuccess, 5)
}

// TestFastStreamedConnReuseAfterErrorBodyReadError pins B1 for the non-2xx
// diagnostic path when the body read ERRORS before EOF (not just exceeds the
// cap): the first response declares a Content-Length larger than the bytes it
// writes and then closes the connection, so reading the error body fails
// mid-stream. The connection must be discarded, not pooled, so a subsequent
// request on the same client is not corrupted.
func TestFastStreamedConnReuseAfterErrorBodyReadError(t *testing.T) {
	normal := `{"ok":true}`
	var n atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			// Declare more body bytes than we write: net/http then closes the
			// connection when the handler returns short, so the client's body
			// read hits EOF before the declared length — a partial read via the
			// error branch (not the cap branch).
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Length", "1024")
			w.WriteHeader(500)
			_, _ = w.Write([]byte(`{"error":"boom"`)) // < 1024 bytes
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		fmt.Fprint(w, normal)
	}))
	defer server.Close()

	c := withFastClient(t, server.Client())
	onSuccess := func() {} // non-nil → streamed lane

	// The first request errors (5xx with a short-then-closed body). Whatever the
	// exact surfaced error, the contract under test is that it must not poison
	// the pool for the next request.
	_, _ = c.Execute(context.Background(), &Request{URL: server.URL, Method: "POST", Body: `{}`}, onSuccess)
	assertCleanConnReuse(t, c, server.URL, normal, onSuccess, 5)
}

// TestFastBufferedLaneConcurrent stresses the buffered lane under concurrency
// to surface any pooled-buffer reuse race (the fastBodyBufPool path is only hit
// by the streamed lane, but the buffered lane shares fasthttp's response pool).
func TestFastBufferedLaneConcurrent(t *testing.T) {
	want := `{"id":"x","choices":[{"message":{"content":"hello world"}}]}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		fmt.Fprint(w, want)
	}))
	defer server.Close()

	c := withFastClient(t, server.Client())
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				resp, err := c.Execute(context.Background(), &Request{URL: server.URL, Method: "POST", Body: `{}`}, nil)
				if err != nil {
					errs <- err
					return
				}
				if resp.Body != want {
					errs <- fmt.Errorf("body mismatch: %q", resp.Body)
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent buffered Execute failed: %v", err)
	}
}
