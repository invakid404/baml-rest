package llmhttp

import (
	"context"
	"errors"
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
	// fasthttp's HostClient normalises header names by default; our
	// template disables that so header casing matches what the caller
	// sent — important for providers that treat header names case-
	// sensitively. net/http's Server canonicalises on parse (every
	// key becomes Canonical-Header-Key), so an httptest.Server-based
	// assertion can't observe the actual on-wire casing. Use a raw
	// net.Listener and parse the request bytes directly.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	rawHeaders := make(chan []string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		// Read the full request preamble (ends with CRLF CRLF). Cap
		// the read so a malformed client can't hang the test.
		buf := make([]byte, 4096)
		n := 0
		for n < len(buf) {
			_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
			m, err := conn.Read(buf[n:])
			n += m
			if err != nil {
				break
			}
			if idx := strings.Index(string(buf[:n]), "\r\n\r\n"); idx >= 0 {
				break
			}
		}
		lines := strings.Split(string(buf[:n]), "\r\n")
		// lines[0] is the request line; lines[1..] are headers until
		// the empty line. Forward just the header lines.
		var hdrs []string
		for _, line := range lines[1:] {
			if line == "" {
				break
			}
			hdrs = append(hdrs, line)
		}
		rawHeaders <- hdrs
		// Write a minimal response so the client's Execute returns
		// cleanly rather than with a read error.
		_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 2\r\n\r\n{}"))
	}()

	c := withFastClient(t, nil)
	_, err = c.Execute(context.Background(), &Request{
		URL:    "http://" + ln.Addr().String(),
		Method: "POST",
		Headers: map[string]string{
			"Authorization": "Bearer test",
			"x-custom":      "v1",
			"X-MixedCase":   "v2",
		},
		Body: `{}`,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	hdrs := <-rawHeaders

	findHeader := func(name string) (string, bool) {
		prefix := name + ":"
		for _, h := range hdrs {
			if strings.HasPrefix(h, prefix) {
				return strings.TrimSpace(h[len(prefix):]), true
			}
		}
		return "", false
	}

	// Canonical-case header should arrive with its original casing.
	if v, ok := findHeader("Authorization"); !ok || v != "Bearer test" {
		t.Errorf("Authorization header missing or wrong: got %q present=%v; raw lines: %q", v, ok, hdrs)
	}
	// Lowercase header must arrive lowercase on the wire, not
	// canonicalised to "X-Custom".
	if v, ok := findHeader("x-custom"); !ok || v != "v1" {
		t.Errorf("x-custom should preserve lower-case on wire: got %q present=%v; raw lines: %q", v, ok, hdrs)
	}
	// MixedCase should also round-trip verbatim.
	if v, ok := findHeader("X-MixedCase"); !ok || v != "v2" {
		t.Errorf("X-MixedCase should preserve casing on wire: got %q present=%v; raw lines: %q", v, ok, hdrs)
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

// --- Streaming under ClientModeFastHTTP routes through net/http (Stage 1) ---
//
// Option A (#475 follow-up) routes ExecuteStream through net/http
// unconditionally, even when BAML_REST_HTTP_CLIENT=fasthttp resolves the
// origin to fasthttp for unary Execute. These tests drive ExecuteStream with
// withFastClient (which forces ClientModeFastHTTP) and assert the streaming
// contracts hold on the net/http path.

// countingRoundTripper wraps an http.RoundTripper and counts invocations so a
// test can prove the net/http transport actually served the request even when
// the client is pinned to fasthttp mode.
type countingRoundTripper struct {
	rt    http.RoundTripper
	count atomic.Int64
}

func (c *countingRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	c.count.Add(1)
	return c.rt.RoundTrip(r)
}

// TestExecuteStream_FastHTTPModeUsesNetHTTP is the definitive proof of Option
// A: with ClientModeFastHTTP set, ExecuteStream must still flow through the
// injected net/http RoundTripper (fasthttp does not use http.Client at all).
func TestExecuteStream_FastHTTPModeUsesNetHTTP(t *testing.T) {
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

	crt := &countingRoundTripper{rt: server.Client().Transport}
	c := withFastClient(t, &http.Client{Transport: crt})

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
	if crt.count.Load() == 0 {
		t.Fatal("net/http RoundTripper was never called — ExecuteStream did not route through net/http under fasthttp mode")
	}
}

// TestExecuteStream_FastHTTPModeHTTPError verifies the non-2xx error envelope
// (capped *HTTPError) is preserved on the forced net/http stream path.
func TestExecuteStream_FastHTTPModeHTTPError(t *testing.T) {
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
	if !strings.Contains(httpErr.Body, "unauthorized") {
		t.Errorf("body did not include server message: %q", httpErr.Body)
	}
}

// TestExecuteStream_FastHTTPModeMissingContentType verifies a response with no
// Content-Type is rejected with the "missing Content-Type" diagnostic on the
// forced net/http path (net/http does not synthesise a default content type).
func TestExecuteStream_FastHTTPModeMissingContentType(t *testing.T) {
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

// TestExecuteStream_FastHTTPModeContextCancellation verifies a mid-stream ctx
// cancel unblocks the stream promptly on the forced net/http path.
func TestExecuteStream_FastHTTPModeContextCancellation(t *testing.T) {
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

// truncatedChunkedHandler drops the conn mid-response without sending the
// zero-size chunk terminator. Shared between the HTTP and HTTPS truncation
// tests. http.ErrAbortHandler tells Go's http server to close the conn without
// the terminating chunk — simulating a provider that crashes mid-response.
func truncatedChunkedHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(200)
	flusher, _ := w.(http.Flusher)
	fmt.Fprint(w, "data: hello\n\n")
	flusher.Flush()
	panic(http.ErrAbortHandler)
}

// TestExecuteStream_TruncatedChunkedHTTP verifies that a mid-stream connection
// drop on a chunked response (no "0\r\n\r\n" terminator) surfaces as
// io.ErrUnexpectedEOF on Errc. net/http's stdlib chunked reader detects the
// missing terminator natively — no fasthttp tail-tracking needed.
func TestExecuteStream_TruncatedChunkedHTTP(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(truncatedChunkedHandler))
	defer server.Close()

	c := withFastClient(t, server.Client())
	resp, err := c.ExecuteStream(context.Background(), &Request{URL: server.URL, Method: "POST", Body: `{}`})
	if err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}
	defer resp.Close()

	for range resp.Events {
	}
	streamErr := <-resp.Errc
	if streamErr == nil {
		t.Fatal("expected an error for truncated chunked response, got nil")
	}
	if !errors.Is(streamErr, io.ErrUnexpectedEOF) {
		t.Errorf("expected io.ErrUnexpectedEOF, got %v", streamErr)
	}
}

// TestExecuteStream_TruncatedChunkedHTTPS covers the same truncation case over
// TLS via httptest.NewTLSServer — proving stdlib TLS+chunked handling surfaces
// the same io.ErrUnexpectedEOF (replaces the old fasthttp TLS tail test).
func TestExecuteStream_TruncatedChunkedHTTPS(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(truncatedChunkedHandler))
	defer server.Close()

	c := withFastClient(t, server.Client())
	resp, err := c.ExecuteStream(context.Background(), &Request{URL: server.URL, Method: "POST", Body: `{}`})
	if err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}
	defer resp.Close()

	for range resp.Events {
	}
	streamErr := <-resp.Errc
	if streamErr == nil {
		t.Fatal("expected an error for truncated HTTPS chunked response, got nil")
	}
	if !errors.Is(streamErr, io.ErrUnexpectedEOF) {
		t.Errorf("expected io.ErrUnexpectedEOF, got %v", streamErr)
	}
}

// TestExecuteStream_CleanChunkedHTTP sanity-checks that a normal SSE stream
// with a proper chunked terminator ends with a nil Errc (no false truncation).
func TestExecuteStream_CleanChunkedHTTP(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		flusher, _ := w.(http.Flusher)
		fmt.Fprint(w, "data: ok\n\n")
		flusher.Flush()
		// Handler returns normally -> Go's server sends the 0-chunk terminator.
	}))
	defer server.Close()

	c := withFastClient(t, server.Client())
	resp, err := c.ExecuteStream(context.Background(), &Request{URL: server.URL, Method: "POST", Body: `{}`})
	if err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}
	defer resp.Close()

	var events []sseclient.Event
	for ev := range resp.Events {
		events = append(events, ev)
	}
	if err := <-resp.Errc; err != nil {
		t.Fatalf("unexpected stream err: %v", err)
	}
	if len(events) != 1 || events[0].Data != "ok" {
		t.Errorf("unexpected events: %+v", events)
	}
}

// TestExecuteStream_CleanChunkedHTTPS is the HTTPS counterpart: a clean TLS SSE
// stream must not be mis-flagged as truncated.
func TestExecuteStream_CleanChunkedHTTPS(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		flusher, _ := w.(http.Flusher)
		fmt.Fprint(w, "data: ok\n\n")
		flusher.Flush()
	}))
	defer server.Close()

	c := withFastClient(t, server.Client())
	resp, err := c.ExecuteStream(context.Background(), &Request{URL: server.URL, Method: "POST", Body: `{}`})
	if err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}
	defer resp.Close()

	var events []sseclient.Event
	for ev := range resp.Events {
		events = append(events, ev)
	}
	if err := <-resp.Errc; err != nil {
		t.Fatalf("unexpected stream err: %v", err)
	}
	if len(events) != 1 || events[0].Data != "ok" {
		t.Errorf("unexpected events: %+v", events)
	}
}
