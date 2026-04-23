package llmhttp

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/invakid404/baml-rest/bamlutils/sseclient"
)

// withFastClient returns a Client pinned to the fasthttp backend for every
// origin. Tests use this to drive the fasthttp code paths deterministically
// without relying on an ALPN probe against a TLS endpoint.
func withFastClient(t *testing.T, http *http.Client) *Client {
	t.Helper()
	t.Setenv(EnvVarClientMode, "fasthttp")
	return NewClient(http)
}

func TestFastExecute_Success(t *testing.T) {
	responseJSON := `{"ok":true}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		fmt.Fprint(w, responseJSON)
	}))
	defer server.Close()

	c := withFastClient(t, server.Client())
	resp, err := c.Execute(context.Background(), &Request{URL: server.URL, Method: "POST", Body: `{}`}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("status %d, want 200", resp.StatusCode)
	}
	if resp.Body != responseJSON {
		t.Errorf("body %q, want %q", resp.Body, responseJSON)
	}
	if resp.Headers.Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type missing or wrong: %q", resp.Headers.Get("Content-Type"))
	}
}

func TestFastExecute_HTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		fmt.Fprint(w, `{"error":"rate limited"}`)
	}))
	defer server.Close()

	c := withFastClient(t, server.Client())
	_, err := c.Execute(context.Background(), &Request{URL: server.URL, Method: "POST", Body: `{}`}, nil)
	if err == nil {
		t.Fatal("expected error on 429")
	}
	httpErr, ok := err.(*HTTPError)
	if !ok {
		t.Fatalf("expected *HTTPError, got %T", err)
	}
	if httpErr.StatusCode != 429 {
		t.Errorf("status %d, want 429", httpErr.StatusCode)
	}
	if !strings.Contains(httpErr.Body, "rate limited") {
		t.Errorf("body did not include server message: %q", httpErr.Body)
	}
}

func TestFastExecute_OnSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer server.Close()

	c := withFastClient(t, server.Client())
	var called atomic.Bool
	_, err := c.Execute(context.Background(), &Request{URL: server.URL, Method: "POST", Body: `{}`},
		func() { called.Store(true) })
	if err != nil {
		t.Fatal(err)
	}
	if !called.Load() {
		t.Error("onSuccess was not fired on 2xx")
	}
}

func TestFastExecute_OnSuccessNotFiredOn5xx(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		fmt.Fprint(w, `{"error":"boom"}`)
	}))
	defer server.Close()

	c := withFastClient(t, server.Client())
	var called atomic.Bool
	_, err := c.Execute(context.Background(), &Request{URL: server.URL, Method: "POST", Body: `{}`},
		func() { called.Store(true) })
	if err == nil {
		t.Fatal("expected 500 error")
	}
	if called.Load() {
		t.Error("onSuccess must not fire on non-2xx")
	}
}

func TestFastExecute_OversizedResponse(t *testing.T) {
	large := strings.Repeat("x", MaxResponseBodyBytes+256)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		fmt.Fprint(w, large)
	}))
	defer server.Close()

	c := withFastClient(t, server.Client())
	_, err := c.Execute(context.Background(), &Request{URL: server.URL, Method: "POST", Body: `{}`}, nil)
	if err == nil {
		t.Fatal("expected size-limit error")
	}
	if !strings.Contains(err.Error(), "exceeds maximum size") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestFastExecute_HeadersForwardedCasePreserved(t *testing.T) {
	// fasthttp defaults to header-name normalisation; our HostClient
	// template disables it so header casing matches what the caller sent —
	// important for providers that treat header names case-sensitively.
	seen := make(chan http.Header, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer server.Close()

	c := withFastClient(t, server.Client())
	_, err := c.Execute(context.Background(), &Request{
		URL:    server.URL,
		Method: "POST",
		Headers: map[string]string{
			"Authorization": "Bearer test",
			"x-custom":      "v1",
		},
		Body: `{}`,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	h := <-seen
	if h.Get("Authorization") != "Bearer test" {
		t.Errorf("Authorization not forwarded: %q", h.Get("Authorization"))
	}
	// Go's http server normalises keys to canonical form on read; the
	// point here is just that the value round-tripped.
	if h.Get("X-Custom") != "v1" {
		t.Errorf("x-custom not forwarded: %q", h.Get("X-Custom"))
	}
}

func TestFastExecuteStream_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		flusher, _ := w.(http.Flusher)
		fmt.Fprint(w, "data: hello\n\n")
		flusher.Flush()
		fmt.Fprint(w, "data: world\n\n")
		flusher.Flush()
	}))
	defer server.Close()

	c := withFastClient(t, server.Client())
	resp, err := c.ExecuteStream(context.Background(), &Request{URL: server.URL, Method: "POST", Body: `{}`})
	if err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}
	defer resp.Close()

	var got []sseclient.Event
	for ev := range resp.Events {
		got = append(got, ev)
	}
	if err := <-resp.Errc; err != nil {
		t.Fatalf("stream err: %v", err)
	}
	if len(got) != 2 || got[0].Data != "hello" || got[1].Data != "world" {
		t.Errorf("unexpected events: %+v", got)
	}
}

func TestFastExecuteStream_HTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		fmt.Fprint(w, `{"error":"unauthorized"}`)
	}))
	defer server.Close()

	c := withFastClient(t, server.Client())
	_, err := c.ExecuteStream(context.Background(), &Request{URL: server.URL, Method: "POST", Body: `{}`})
	if err == nil {
		t.Fatal("expected error on 401")
	}
	httpErr, ok := err.(*HTTPError)
	if !ok {
		t.Fatalf("expected *HTTPError, got %T", err)
	}
	if httpErr.StatusCode != 401 {
		t.Errorf("status %d, want 401", httpErr.StatusCode)
	}
}

func TestFastExecuteStream_ContextCancellation(t *testing.T) {
	// Server flushes one event then blocks until r.Context() fires. The
	// client cancels after 200ms; the stream must unblock and the events
	// channel must close within the test budget — requires the ctx watcher
	// to force-close the underlying conn (not just release it to the pool).
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		flusher, _ := w.(http.Flusher)
		fmt.Fprint(w, "data: first\n\n")
		flusher.Flush()
		<-r.Context().Done()
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	c := withFastClient(t, server.Client())
	resp, err := c.ExecuteStream(ctx, &Request{URL: server.URL, Method: "POST", Body: `{}`})
	if err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}
	defer resp.Close()

	deadline := time.After(5 * time.Second)
	var events []sseclient.Event
drainLoop:
	for {
		select {
		case ev, ok := <-resp.Events:
			if !ok {
				break drainLoop
			}
			events = append(events, ev)
		case <-deadline:
			t.Fatal("events channel did not close after ctx cancellation")
		}
	}

	if len(events) < 1 {
		t.Error("expected at least the first event before cancellation")
	}
}

func TestFastExecuteStream_MissingContentType(t *testing.T) {
	// Regression guard: fasthttp synthesises a default Content-Type of
	// text/plain; charset=utf-8 when the server sends none; our streaming
	// backend opts that default out so the "missing" error path works.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header()["Content-Type"] = nil
		w.WriteHeader(200)
		w.Write([]byte("raw bytes"))
	}))
	defer server.Close()

	c := withFastClient(t, server.Client())
	_, err := c.ExecuteStream(context.Background(), &Request{URL: server.URL, Method: "POST", Body: `{}`})
	if err == nil {
		t.Fatal("expected error for missing Content-Type")
	}
	if !strings.Contains(err.Error(), "missing Content-Type") {
		t.Errorf("expected missing Content-Type error, got: %v", err)
	}
}

func TestFastExecute_ConnectionRefused(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	c := withFastClient(t, nil)
	_, err = c.Execute(context.Background(), &Request{URL: "http://" + addr, Method: "POST", Body: `{}`}, nil)
	if err == nil {
		t.Fatal("expected connection-refused error")
	}
}

// TestFastStreamReader_CloseIdempotent ensures repeated Close calls don't
// panic or re-release the fasthttp pool entries.
func TestFastStreamReader_CloseIdempotent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, "data: ok\n\n")
	}))
	defer server.Close()

	c := withFastClient(t, server.Client())
	resp, err := c.ExecuteStream(context.Background(), &Request{URL: server.URL, Method: "POST", Body: `{}`})
	if err != nil {
		t.Fatal(err)
	}
	// Drain
	for range resp.Events {
	}
	<-resp.Errc

	resp.Close()
	resp.Close() // must not panic / race
	resp.Close()
}

// assert at compile time that fastStreamReader fulfils io.ReadCloser so the
// signature change would be caught by tests rather than at a caller.
var _ io.ReadCloser = (*fastStreamReader)(nil)
