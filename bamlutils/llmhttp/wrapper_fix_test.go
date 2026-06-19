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
