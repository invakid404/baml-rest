package llmhttp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"syscall"
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
	// ExecuteStream unconditionally routes through net/http (openStream ->
	// http.Client.Do), so inject the refusal into the net/http RoundTripper.
	// The explicit ClientModeNetHTTP documents the contract and keeps this
	// connection-failure test off any environment-driven construction. Using
	// a synthetic refusal (not a just-freed ephemeral port) makes it
	// deterministic — see errTestConnectionRefused.
	client := NewClientWithOptions(ClientOptions{
		Mode:          ClientModeNetHTTP,
		NetHTTPClient: connectionRefusedHTTPClient(),
	})
	_, err := client.ExecuteStream(context.Background(), &Request{
		URL:    "http://connection-refused.invalid/",
		Method: "POST",
		Body:   `{}`,
	})
	requireConnectionRefused(t, err)
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
	defer resp.Release()

	if resp.StatusCode != 200 {
		t.Errorf("expected status 200, got %d", resp.StatusCode)
	}
	if resp.BodyString() != responseJSON {
		t.Errorf("expected body %q, got %q", responseJSON, resp.BodyString())
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
	// Pin ClientModeNetHTTP so executeBorrow skips the fast cache entry and
	// reaches c.httpClient.Do — Auto mode would resolve a plain-http origin to
	// fasthttp, so this generic test would otherwise never exercise the unary
	// net/http path. The refusal is injected into the RoundTripper (no socket),
	// making it deterministic instead of racing a just-freed ephemeral port.
	client := NewClientWithOptions(ClientOptions{
		Mode:          ClientModeNetHTTP,
		NetHTTPClient: connectionRefusedHTTPClient(),
	})
	_, err := client.Execute(context.Background(), &Request{
		URL:    "http://connection-refused.invalid/",
		Method: "POST",
		Body:   `{}`,
	}, nil)
	requireConnectionRefused(t, err)
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
	defer resp.Release()
	if resp.BodyString() != `{"ok":true}` {
		t.Errorf("unexpected body: %q", resp.BodyString())
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
	defer resp.Release()
	if resp.BodyString() != `{"result":"ok"}` {
		t.Errorf("unexpected body: %q", resp.BodyString())
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
	defer resp.Release()
	if len(resp.BodyString()) != MaxResponseBodyBytes {
		t.Errorf("expected body length %d, got %d", MaxResponseBodyBytes, len(resp.BodyString()))
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
	defer resp.Release()
	if resp.BodyString() != `{"ok":true}` {
		t.Errorf("unexpected body: %q", resp.BodyString())
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
	defer resp.Release()
	if resp.BodyString() != `{"ok":true}` {
		t.Errorf("unexpected body: %q", resp.BodyString())
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
	defer resp.Release()
	if !called {
		t.Fatal("onSuccess callback was not fired on 2xx response")
	}
	if resp.BodyString() != `{"ok":true}` {
		t.Errorf("unexpected body: %q", resp.BodyString())
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
	defer resp.Release()
	if resp.BodyString() != `{"ok":true}` {
		t.Errorf("unexpected body: %q", resp.BodyString())
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

// roundTripperFunc lets a test return a synthetic *http.Response with a
// body of its choosing — useful for driving ExecuteStream over a body
// reader that emits valid SSE bytes and then a typed transport error.
type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// errTestConnectionRefused is the private sentinel the connection-refused
// tests inject into their transport seams. It wraps syscall.ECONNREFUSED so
// the production classifier maps it to TransportFlakeConnectionRefused, and
// its own identity lets the assertions prove the configured seam — not an
// unrelated DNS/proxy/socket error — was what surfaced. These tests inject a
// synthetic refusal instead of dialing a just-freed ephemeral port: closing a
// port-0 listener and then dialing it is a TOCTOU race (a concurrent test
// server can bind the released port and answer 2xx, making Execute return a
// nil error), which is exactly what flaked under -race -count=100.
var errTestConnectionRefused = fmt.Errorf(
	"test-injected connection refusal: %w", syscall.ECONNREFUSED,
)

// connectionRefusedHTTPClient returns an *http.Client whose RoundTripper
// always fails with errTestConnectionRefused, exercising the net/http Execute
// and ExecuteStream paths without touching the network.
func connectionRefusedHTTPClient() *http.Client {
	return &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return nil, errTestConnectionRefused
	})}
}

// connectionRefusedDial is a fasthttp DialFuncWithTimeout that always fails
// with errTestConnectionRefused, exercising the fasthttp backend's dial path
// without touching the network.
func connectionRefusedDial(_ string, _ time.Duration) (net.Conn, error) {
	return nil, errTestConnectionRefused
}

// requireConnectionRefused asserts that err is the fully classified
// connection-refused transport error: it originated from the injected
// sentinel, carries syscall.ECONNREFUSED and the ErrTransportFlake umbrella,
// and is a *TransportError categorised as TransportFlakeConnectionRefused.
func requireConnectionRefused(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected connection-refused error")
	}
	if !errors.Is(err, errTestConnectionRefused) {
		t.Fatalf("error did not come from the injected refusal: %v", err)
	}
	if !errors.Is(err, syscall.ECONNREFUSED) {
		t.Fatalf("errors.Is(err, syscall.ECONNREFUSED) = false: %v", err)
	}
	if !errors.Is(err, ErrTransportFlake) {
		t.Fatalf("errors.Is(err, ErrTransportFlake) = false: %v", err)
	}
	var te *TransportError
	if !errors.As(err, &te) {
		t.Fatalf("error is not *TransportError: %T: %v", err, err)
	}
	if te.Category != TransportFlakeConnectionRefused {
		t.Fatalf("category = %v, want %v", te.Category, TransportFlakeConnectionRefused)
	}
}

// scriptedReader emits a fixed prefix on its first Read calls and then
// returns finalErr once the prefix has been drained. Used to simulate a
// streaming body that delivers a complete SSE event before the transport
// connection drops mid-stream.
type scriptedReader struct {
	prefix []byte
	pos    int
	err    error
	closed bool
}

func (s *scriptedReader) Read(p []byte) (int, error) {
	if s.pos < len(s.prefix) {
		n := copy(p, s.prefix[s.pos:])
		s.pos += n
		return n, nil
	}
	return 0, s.err
}

func (s *scriptedReader) Close() error {
	s.closed = true
	return nil
}

func TestExecuteStream_MidStreamTransportFlakeClassified(t *testing.T) {
	// Force the net/http dispatch path so the custom RoundTripper is the
	// only thing that observes the request — the cache's ALPN probe must
	// not run against a fake URL.
	t.Setenv(EnvVarClientMode, "nethttp")

	transportErr := &net.OpError{Op: "read", Err: &os.SyscallError{Syscall: "read", Err: syscall.ECONNRESET}}

	rt := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		body := &scriptedReader{
			prefix: []byte("data: first\n\ndata: [DONE]\n\n"),
			err:    transportErr,
		}
		return &http.Response{
			Status:     "200 OK",
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       body,
			Request:    r,
		}, nil
	})

	client := NewClient(&http.Client{Transport: rt})
	resp, err := client.ExecuteStream(context.Background(), &Request{
		URL:    "http://example.invalid/",
		Method: "POST",
		Body:   `{}`,
	})
	if err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}
	defer resp.Close()

	var events []sseclient.Event
	for ev := range resp.Events {
		events = append(events, ev)
	}
	if len(events) < 1 {
		t.Fatal("expected at least one event before the transport drop")
	}

	streamErr := <-resp.Errc
	if streamErr == nil {
		t.Fatal("expected non-nil terminal error after a mid-stream ECONNRESET")
	}
	if !errors.Is(streamErr, ErrTransportFlake) {
		t.Errorf("errors.Is(err, ErrTransportFlake) = false; want true; err = %v", streamErr)
	}
	if !errors.Is(streamErr, syscall.ECONNRESET) {
		t.Errorf("errors.Is(err, syscall.ECONNRESET) = false; want true; err = %v", streamErr)
	}
}

func TestExecuteStream_TruncationStaysContentIntegrity(t *testing.T) {
	// io.ErrUnexpectedEOF (chunked truncation) must NOT be reclassified
	// as ErrTransportFlake — the body-read site uses
	// staleConnTeardownAcceptable=false, and the wrapper must mirror
	// that. Truncation is a content-integrity failure, not a transport
	// flake.
	t.Setenv(EnvVarClientMode, "nethttp")

	rt := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		body := &scriptedReader{
			prefix: []byte("data: partial\n\n"),
			err:    io.ErrUnexpectedEOF,
		}
		return &http.Response{
			Status:     "200 OK",
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       body,
			Request:    r,
		}, nil
	})

	client := NewClient(&http.Client{Transport: rt})
	resp, err := client.ExecuteStream(context.Background(), &Request{
		URL:    "http://example.invalid/",
		Method: "POST",
		Body:   `{}`,
	})
	if err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}
	defer resp.Close()

	for range resp.Events {
	}
	streamErr := <-resp.Errc
	if streamErr == nil {
		t.Fatal("expected non-nil terminal error for truncated body")
	}
	if errors.Is(streamErr, ErrTransportFlake) {
		t.Errorf("io.ErrUnexpectedEOF must not match ErrTransportFlake; err = %v", streamErr)
	}
	if !errors.Is(streamErr, io.ErrUnexpectedEOF) {
		t.Errorf("errors.Is(err, io.ErrUnexpectedEOF) = false; want true; err = %v", streamErr)
	}
}
