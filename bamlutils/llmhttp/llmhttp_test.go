package llmhttp

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/invakid404/baml-rest/bamlutils/sseclient"
)

func TestExecuteStreamSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\" world\"}}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	client := NewClient(server.Client())
	resp, err := client.ExecuteStream(context.Background(), &Request{
		URL:     server.URL,
		Method:  "POST",
		Headers: map[string]string{"Content-Type": "application/json"},
		Body:    `{"model":"gpt-4","stream":true}`,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Close()

	if resp.StatusCode != 200 {
		t.Errorf("expected status 200, got %d", resp.StatusCode)
	}

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

func TestExecuteStreamHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		fmt.Fprint(w, `{"error":{"message":"rate limited"}}`)
	}))
	defer server.Close()

	client := NewClient(server.Client())
	_, err := client.ExecuteStream(context.Background(), &Request{
		URL:    server.URL,
		Method: "POST",
		Body:   `{}`,
	})

	if err == nil {
		t.Fatal("expected error for 429 response")
	}

	httpErr, ok := err.(*HTTPError)
	if !ok {
		t.Fatalf("expected *HTTPError, got %T", err)
	}
	if httpErr.StatusCode != 429 {
		t.Errorf("expected status 429, got %d", httpErr.StatusCode)
	}
	if !strings.Contains(httpErr.Body, "rate limited") {
		t.Errorf("expected body to contain 'rate limited', got %q", httpErr.Body)
	}
}

func TestExecuteStreamServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		fmt.Fprint(w, "internal server error")
	}))
	defer server.Close()

	client := NewClient(server.Client())
	_, err := client.ExecuteStream(context.Background(), &Request{
		URL:    server.URL,
		Method: "POST",
		Body:   `{}`,
	})

	if err == nil {
		t.Fatal("expected error for 500 response")
	}

	httpErr, ok := err.(*HTTPError)
	if !ok {
		t.Fatalf("expected *HTTPError, got %T", err)
	}
	if httpErr.StatusCode != 500 {
		t.Errorf("expected status 500, got %d", httpErr.StatusCode)
	}
}

func TestExecuteStreamContextCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		flusher, ok := w.(http.Flusher)
		if !ok {
			return
		}
		fmt.Fprint(w, "data: first\n\n")
		flusher.Flush()
		// Block forever after first event
		<-r.Context().Done()
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	client := NewClient(server.Client())
	resp, err := client.ExecuteStream(ctx, &Request{
		URL:    server.URL,
		Method: "POST",
		Body:   `{}`,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Close()

	var events []sseclient.Event
	// Drain events with a safety timeout to avoid hanging if channel doesn't close
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-resp.Events:
			if !ok {
				goto drained
			}
			events = append(events, ev)
		case <-deadline:
			t.Fatal("timed out waiting for events channel to close")
		}
	}
drained:

	// Should get at least the first event
	if len(events) < 1 {
		t.Error("expected at least 1 event before cancellation")
	}

	// Stream error should be context-related
	streamErr := <-resp.Errc
	if streamErr == nil {
		t.Log("stream ended without error (acceptable if connection was closed cleanly)")
	}
}

func TestExecuteStreamHeadersForwarded(t *testing.T) {
	headersCh := make(chan http.Header, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headersCh <- r.Header.Clone()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, "data: ok\n\n")
	}))
	defer server.Close()

	client := NewClient(server.Client())
	resp, err := client.ExecuteStream(context.Background(), &Request{
		URL:    server.URL,
		Method: "POST",
		Headers: map[string]string{
			"Authorization": "Bearer sk-test-123",
			"X-Custom":      "custom-value",
			"Content-Type":  "application/json",
		},
		Body: `{}`,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Close()

	for range resp.Events {
	}
	<-resp.Errc

	receivedHeaders := <-headersCh
	if receivedHeaders.Get("Authorization") != "Bearer sk-test-123" {
		t.Errorf("Authorization header not forwarded: %q", receivedHeaders.Get("Authorization"))
	}
	if receivedHeaders.Get("X-Custom") != "custom-value" {
		t.Errorf("X-Custom header not forwarded: %q", receivedHeaders.Get("X-Custom"))
	}
}

func TestExecuteStreamConnectionRefused(t *testing.T) {
	// Grab a free port, then close the listener so the port is guaranteed closed
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	client := NewClient(nil)
	_, err = client.ExecuteStream(context.Background(), &Request{
		URL:    "http://" + addr,
		Method: "POST",
		Body:   `{}`,
	})

	if err == nil {
		t.Fatal("expected error for connection refused")
	}
}

func TestExecuteStreamInvalidURL(t *testing.T) {
	client := NewClient(nil)
	_, err := client.ExecuteStream(context.Background(), &Request{
		URL:    "://invalid",
		Method: "POST",
	})

	if err == nil {
		t.Fatal("expected error for invalid URL")
	}
}

func TestExecuteStreamEmptyBody(t *testing.T) {
	bodyCh := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 1024)
		n, _ := r.Body.Read(buf)
		bodyCh <- string(buf[:n])
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, "data: ok\n\n")
	}))
	defer server.Close()

	client := NewClient(server.Client())
	resp, err := client.ExecuteStream(context.Background(), &Request{
		URL:    server.URL,
		Method: "POST",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Close()

	for range resp.Events {
	}
	<-resp.Errc

	receivedBody := <-bodyCh
	if receivedBody != "" {
		t.Errorf("expected empty body, got %q", receivedBody)
	}
}

func TestDefaultClient(t *testing.T) {
	if DefaultClient == nil {
		t.Fatal("DefaultClient should not be nil")
	}
	if DefaultClient.httpClient == nil {
		t.Fatal("DefaultClient.httpClient should not be nil")
	}
}

func TestRedactHeaders(t *testing.T) {
	headers := map[string]string{
		"Authorization":   "Bearer sk-secret-key",
		"Content-Type":    "application/json",
		"X-Api-Key":       "my-api-key",
		"X-Custom-Header": "safe-value",
		"Cookie":          "session=abc123",
		"X-Auth-Token":    "token-value",
		"X-Request-Id":    "req-123",
	}

	redacted := RedactHeaders(headers)

	// Should be redacted
	if redacted["Authorization"] != "REDACTED" {
		t.Errorf("Authorization should be redacted, got %q", redacted["Authorization"])
	}
	if redacted["X-Api-Key"] != "REDACTED" {
		t.Errorf("X-Api-Key should be redacted, got %q", redacted["X-Api-Key"])
	}
	if redacted["Cookie"] != "REDACTED" {
		t.Errorf("Cookie should be redacted, got %q", redacted["Cookie"])
	}
	if redacted["X-Auth-Token"] != "REDACTED" {
		t.Errorf("X-Auth-Token should be redacted, got %q", redacted["X-Auth-Token"])
	}

	// Should NOT be redacted
	if redacted["Content-Type"] != "application/json" {
		t.Errorf("Content-Type should not be redacted, got %q", redacted["Content-Type"])
	}
	if redacted["X-Custom-Header"] != "safe-value" {
		t.Errorf("X-Custom-Header should not be redacted, got %q", redacted["X-Custom-Header"])
	}
	if redacted["X-Request-Id"] != "req-123" {
		t.Errorf("X-Request-Id should not be redacted, got %q", redacted["X-Request-Id"])
	}
}

func TestRedactHeadersEmpty(t *testing.T) {
	redacted := RedactHeaders(map[string]string{})
	if len(redacted) != 0 {
		t.Errorf("expected empty map, got %d entries", len(redacted))
	}
}

func TestHTTPErrorString(t *testing.T) {
	err := &HTTPError{StatusCode: 429, Body: `{"error":"rate limited"}`}
	s := err.Error()
	if !strings.Contains(s, "429") {
		t.Errorf("error string should contain status code: %q", s)
	}
	if !strings.Contains(s, "rate limited") {
		t.Errorf("error string should contain body: %q", s)
	}
}

func TestHTTPErrorStringEmptyBody(t *testing.T) {
	err := &HTTPError{StatusCode: 500}
	s := err.Error()
	if !strings.Contains(s, "500") {
		t.Errorf("error string should contain status code: %q", s)
	}
}

func TestNewClientNil(t *testing.T) {
	c := NewClient(nil)
	if c.httpClient != http.DefaultClient {
		t.Error("NewClient(nil) should use http.DefaultClient")
	}
}

func TestNewClientCustom(t *testing.T) {
	custom := &http.Client{Timeout: 5 * time.Second}
	c := NewClient(custom)
	if c.httpClient != custom {
		t.Error("NewClient should use the provided client")
	}
}

func TestExecuteStreamMissingContentType(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Return 200 with no Content-Type header.
		// Must call WriteHeader before writing body to prevent Go's
		// auto-detection from setting Content-Type based on body content.
		w.Header()["Content-Type"] = nil // explicitly remove
		w.WriteHeader(200)
		w.Write([]byte("raw bytes"))
	}))
	defer server.Close()

	client := NewClient(server.Client())
	_, err := client.ExecuteStream(context.Background(), &Request{
		URL:    server.URL,
		Method: "POST",
		Body:   `{}`,
	})

	if err == nil {
		t.Fatal("expected error for missing Content-Type")
	}
	if !strings.Contains(err.Error(), "missing Content-Type") {
		t.Errorf("error should mention missing Content-Type: %v", err)
	}
}

// ======== Execute (non-streaming) tests ========

func TestExecuteSuccess(t *testing.T) {
	responseJSON := `{"choices":[{"message":{"content":"Hello world"}}]}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		fmt.Fprint(w, responseJSON)
	}))
	defer server.Close()

	client := NewClient(server.Client())
	resp, err := client.Execute(context.Background(), &Request{
		URL:     server.URL,
		Method:  "POST",
		Headers: map[string]string{"Content-Type": "application/json"},
		Body:    `{"model":"gpt-4","stream":false}`,
	}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if resp.StatusCode != 200 {
		t.Errorf("expected status 200, got %d", resp.StatusCode)
	}
	if resp.Body != responseJSON {
		t.Errorf("expected body %q, got %q", responseJSON, resp.Body)
	}
	if resp.Headers.Get("Content-Type") != "application/json" {
		t.Errorf("expected Content-Type application/json, got %q", resp.Headers.Get("Content-Type"))
	}
}

func TestExecuteHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		fmt.Fprint(w, `{"error":{"message":"rate limited"}}`)
	}))
	defer server.Close()

	client := NewClient(server.Client())
	_, err := client.Execute(context.Background(), &Request{
		URL:    server.URL,
		Method: "POST",
		Body:   `{}`,
	}, nil)

	if err == nil {
		t.Fatal("expected error for 429 response")
	}

	httpErr, ok := err.(*HTTPError)
	if !ok {
		t.Fatalf("expected *HTTPError, got %T", err)
	}
	if httpErr.StatusCode != 429 {
		t.Errorf("expected status 429, got %d", httpErr.StatusCode)
	}
	if !strings.Contains(httpErr.Body, "rate limited") {
		t.Errorf("expected body to contain 'rate limited', got %q", httpErr.Body)
	}
}

func TestExecuteServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		fmt.Fprint(w, "internal server error")
	}))
	defer server.Close()

	client := NewClient(server.Client())
	_, err := client.Execute(context.Background(), &Request{
		URL:    server.URL,
		Method: "POST",
		Body:   `{}`,
	}, nil)

	if err == nil {
		t.Fatal("expected error for 500 response")
	}

	httpErr, ok := err.(*HTTPError)
	if !ok {
		t.Fatalf("expected *HTTPError, got %T", err)
	}
	if httpErr.StatusCode != 500 {
		t.Errorf("expected status 500, got %d", httpErr.StatusCode)
	}
}

func TestExecuteContextCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Write response headers but delay the body. The client gets past
		// httpClient.Do() but blocks in io.ReadAll on the body. When the
		// context expires, the body read returns with a context error.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Short timeout to allow server.Close() to complete promptly.
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	client := NewClient(server.Client())
	_, err := client.Execute(ctx, &Request{
		URL:    server.URL,
		Method: "POST",
		Body:   `{}`,
	}, nil)
	if err == nil {
		t.Fatal("expected error after context timeout")
	}
}

func TestExecuteHeadersForwarded(t *testing.T) {
	headersCh := make(chan http.Header, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headersCh <- r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer server.Close()

	client := NewClient(server.Client())
	_, err := client.Execute(context.Background(), &Request{
		URL:    server.URL,
		Method: "POST",
		Headers: map[string]string{
			"Authorization": "Bearer sk-test-123",
			"X-Custom":      "custom-value",
			"Content-Type":  "application/json",
		},
		Body: `{}`,
	}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	receivedHeaders := <-headersCh
	if receivedHeaders.Get("Authorization") != "Bearer sk-test-123" {
		t.Errorf("Authorization header not forwarded: %q", receivedHeaders.Get("Authorization"))
	}
	if receivedHeaders.Get("X-Custom") != "custom-value" {
		t.Errorf("X-Custom header not forwarded: %q", receivedHeaders.Get("X-Custom"))
	}
}

func TestExecuteConnectionRefused(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	client := NewClient(nil)
	_, err = client.Execute(context.Background(), &Request{
		URL:    "http://" + addr,
		Method: "POST",
		Body:   `{}`,
	}, nil)

	if err == nil {
		t.Fatal("expected error for connection refused")
	}
}

func TestExecuteNilClient(t *testing.T) {
	var c *Client
	_, err := c.Execute(context.Background(), &Request{
		URL:    "http://localhost",
		Method: "POST",
	}, nil)
	if err == nil {
		t.Fatal("expected error for nil client")
	}
	if !strings.Contains(err.Error(), "nil client") {
		t.Errorf("expected nil client error, got: %v", err)
	}
}

func TestExecuteNilContext(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer server.Close()

	client := NewClient(server.Client())
	//nolint:staticcheck // deliberately passing nil context to test the guard
	resp, err := client.Execute(nil, &Request{
		URL:    server.URL,
		Method: "POST",
		Body:   `{}`,
	}, nil)
	if err != nil {
		t.Fatalf("expected nil context to be handled gracefully, got error: %v", err)
	}
	if resp.Body != `{"ok":true}` {
		t.Errorf("unexpected body: %q", resp.Body)
	}
}

func TestExecuteNilRequest(t *testing.T) {
	client := NewClient(nil)
	_, err := client.Execute(context.Background(), nil, nil)
	if err == nil {
		t.Fatal("expected error for nil request")
	}
}

func TestExecuteInvalidURL(t *testing.T) {
	client := NewClient(nil)
	_, err := client.Execute(context.Background(), &Request{
		URL:    "://invalid",
		Method: "POST",
	}, nil)
	if err == nil {
		t.Fatal("expected error for invalid URL")
	}
}

func TestExecuteEmptyBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		fmt.Fprint(w, `{"result":"ok"}`)
	}))
	defer server.Close()

	client := NewClient(server.Client())
	resp, err := client.Execute(context.Background(), &Request{
		URL:    server.URL,
		Method: "POST",
	}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Body != `{"result":"ok"}` {
		t.Errorf("unexpected body: %q", resp.Body)
	}
}

func TestExecuteOversizedResponseError(t *testing.T) {
	// A response body exceeding MaxResponseBodyBytes must produce an error,
	// not a silently truncated successful response.
	largeBody := strings.Repeat("x", MaxResponseBodyBytes+1024)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		fmt.Fprint(w, largeBody)
	}))
	defer server.Close()

	client := NewClient(server.Client())
	_, err := client.Execute(context.Background(), &Request{
		URL:    server.URL,
		Method: "POST",
		Body:   `{}`,
	}, nil)
	if err == nil {
		t.Fatal("expected error for oversized response")
	}
	if !strings.Contains(err.Error(), "exceeds maximum size") {
		t.Errorf("expected size limit error, got: %v", err)
	}
}

func TestExecuteExactlyAtLimit(t *testing.T) {
	// A response body exactly at MaxResponseBodyBytes should succeed.
	exactBody := strings.Repeat("x", MaxResponseBodyBytes)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		fmt.Fprint(w, exactBody)
	}))
	defer server.Close()

	client := NewClient(server.Client())
	resp, err := client.Execute(context.Background(), &Request{
		URL:    server.URL,
		Method: "POST",
		Body:   `{}`,
	}, nil)
	if err != nil {
		t.Fatalf("unexpected error for body at limit: %v", err)
	}
	if len(resp.Body) != MaxResponseBodyBytes {
		t.Errorf("expected body length %d, got %d", MaxResponseBodyBytes, len(resp.Body))
	}
}

func TestExecuteErrorBodyTruncated(t *testing.T) {
	// Error body > 4KB should be truncated in the HTTPError.
	largeError := strings.Repeat("e", 8192)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		fmt.Fprint(w, largeError)
	}))
	defer server.Close()

	client := NewClient(server.Client())
	_, err := client.Execute(context.Background(), &Request{
		URL:    server.URL,
		Method: "POST",
		Body:   `{}`,
	}, nil)

	if err == nil {
		t.Fatal("expected error")
	}
	httpErr, ok := err.(*HTTPError)
	if !ok {
		t.Fatalf("expected *HTTPError, got %T", err)
	}
	if len(httpErr.Body) != MaxErrorBodyBytes {
		t.Errorf("expected error body truncated to %d, got %d", MaxErrorBodyBytes, len(httpErr.Body))
	}
}

// deadlineCapture is an http.RoundTripper that records the request context's
// deadline before forwarding to the underlying transport. This lets tests
// inspect the deadline that Execute() set on the client side (the server's
// r.Context() does NOT inherit the client's deadline).
type deadlineCapture struct {
	rt          http.RoundTripper
	deadline    time.Time
	hasDeadline bool
}

func (d *deadlineCapture) RoundTrip(req *http.Request) (*http.Response, error) {
	d.deadline, d.hasDeadline = req.Context().Deadline()
	return d.rt.RoundTrip(req)
}

func TestExecuteDefaultTimeoutInjected(t *testing.T) {
	// Force the net/http dispatch path: this test intercepts requests via
	// an http.RoundTripper, which only observes traffic on the net/http
	// backend. The fasthttp backend applies DefaultCallTimeout through
	// DoDeadline instead and is covered separately.
	t.Setenv(EnvVarClientMode, "nethttp")

	// Verify that Execute() injects DefaultCallTimeout when the caller
	// provides context.Background() (no deadline). We intercept the
	// outgoing HTTP request to check the context's deadline directly.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer server.Close()

	capture := &deadlineCapture{rt: server.Client().Transport}
	client := NewClient(&http.Client{Transport: capture})

	before := time.Now()
	resp, err := client.Execute(context.Background(), &Request{
		URL:    server.URL,
		Method: "POST",
		Body:   `{}`,
	}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Body != `{"ok":true}` {
		t.Errorf("unexpected body: %q", resp.Body)
	}

	if !capture.hasDeadline {
		t.Fatal("Execute() did not inject a deadline on context.Background()")
	}

	// The injected deadline should be approximately DefaultCallTimeout from
	// when Execute() was called. Allow generous slack for test scheduling.
	remaining := capture.deadline.Sub(before)
	if remaining < DefaultCallTimeout-5*time.Second || remaining > DefaultCallTimeout+5*time.Second {
		t.Errorf("injected deadline remaining %v not close to DefaultCallTimeout %v", remaining, DefaultCallTimeout)
	}
}

func TestExecuteCallerDeadlinePreserved(t *testing.T) {
	// Same reasoning as TestExecuteDefaultTimeoutInjected: forced to
	// net/http so the RoundTripper-based deadline capture works. The
	// fasthttp path passes the caller's deadline to DoDeadline directly.
	t.Setenv(EnvVarClientMode, "nethttp")

	// When the caller provides a context with a deadline, Execute() must
	// NOT override it with DefaultCallTimeout.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer server.Close()

	capture := &deadlineCapture{rt: server.Client().Transport}
	client := NewClient(&http.Client{Transport: capture})

	callerTimeout := 10 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), callerTimeout)
	defer cancel()

	before := time.Now()
	resp, err := client.Execute(ctx, &Request{
		URL:    server.URL,
		Method: "POST",
		Body:   `{}`,
	}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Body != `{"ok":true}` {
		t.Errorf("unexpected body: %q", resp.Body)
	}

	if !capture.hasDeadline {
		t.Fatal("expected a deadline on the request context")
	}

	// The deadline should reflect the caller's 10s timeout, not DefaultCallTimeout (5m).
	// Check both directions: not extended beyond the caller's deadline, and not
	// shortened below it (minus generous slack for test scheduling).
	remaining := capture.deadline.Sub(before)
	if remaining > callerTimeout+time.Second {
		t.Errorf("deadline remaining %v exceeds caller timeout %v — was overridden by default", remaining, callerTimeout)
	}
	if remaining < callerTimeout-2*time.Second {
		t.Errorf("deadline remaining %v is much less than caller timeout %v — deadline was shortened", remaining, callerTimeout)
	}
}

func TestExecuteOnSuccessCallbackFired(t *testing.T) {
	// Verify onSuccess fires on 2xx responses.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer server.Close()

	called := false
	client := NewClient(server.Client())
	resp, err := client.Execute(context.Background(), &Request{
		URL:    server.URL,
		Method: "POST",
		Body:   `{}`,
	}, func() { called = true })
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !called {
		t.Fatal("onSuccess callback was not fired on 2xx response")
	}
	if resp.Body != `{"ok":true}` {
		t.Errorf("unexpected body: %q", resp.Body)
	}
}

func TestExecuteOnSuccessFiresBeforeBodyRead(t *testing.T) {
	// Verify onSuccess fires after 2xx status but BEFORE the body is fully
	// read. The handler records whether the callback channel was signaled
	// before writing the body. After Execute() returns, we assert the
	// handler observed the callback before body send.
	callbackFired := make(chan struct{})
	callbackSeenBeforeBody := make(chan bool, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Check if callback fired before we send the body.
		// Use a short timeout: if the callback hasn't fired within 2s,
		// the body is sent anyway but we record that fact.
		select {
		case <-callbackFired:
			callbackSeenBeforeBody <- true
		case <-time.After(2 * time.Second):
			callbackSeenBeforeBody <- false
		}
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer server.Close()

	client := NewClient(server.Client())
	resp, err := client.Execute(context.Background(), &Request{
		URL:    server.URL,
		Method: "POST",
		Body:   `{}`,
	}, func() { close(callbackFired) })
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Body != `{"ok":true}` {
		t.Errorf("unexpected body: %q", resp.Body)
	}

	// Assert the handler saw the callback BEFORE it sent the body.
	if seen := <-callbackSeenBeforeBody; !seen {
		t.Fatal("onSuccess callback did not fire before the response body was sent — ordering violation")
	}
}

func TestExecuteOnSuccessNotFiredOnError(t *testing.T) {
	// Verify onSuccess is NOT fired on non-2xx responses.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		fmt.Fprint(w, `{"error":"fail"}`)
	}))
	defer server.Close()

	called := false
	client := NewClient(server.Client())
	_, err := client.Execute(context.Background(), &Request{
		URL:    server.URL,
		Method: "POST",
		Body:   `{}`,
	}, func() { called = true })
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
	if called {
		t.Fatal("onSuccess callback should not fire on non-2xx response")
	}
}
