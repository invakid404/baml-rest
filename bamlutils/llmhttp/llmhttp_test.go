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
