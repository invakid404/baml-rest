// Package llmhttp provides a thin HTTP client wrapper for executing LLM
// streaming requests. It converts a generic request specification (URL,
// method, headers, body) into an HTTP request, sends it, and returns the
// response as a stream of SSE events.
//
// This package does NOT depend on the BAML SDK. The conversion from
// baml.HTTPRequest to the types in this package is done in the generated
// adapter code.
package llmhttp

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/invakid404/baml-rest/bamlutils/sseclient"
)

// Request describes an HTTP request to send to an LLM provider.
// This is a plain struct — the adapter codegen converts baml.HTTPRequest
// into this type.
type Request struct {
	URL     string
	Method  string
	Headers map[string]string
	Body    string // Raw request body (JSON string)
}

// StreamResponse represents an active streaming HTTP connection to an LLM
// provider. The caller must call Close() when done reading events.
type StreamResponse struct {
	// StatusCode is the HTTP response status code.
	StatusCode int

	// Headers are the HTTP response headers.
	Headers http.Header

	// Events is the channel of SSE events parsed from the response body.
	// It is closed when the stream ends (EOF or error).
	Events <-chan sseclient.Event

	// Errc receives exactly one value (the terminal error) when the event
	// stream ends. nil means clean EOF.
	Errc <-chan error

	// body holds the response body for cleanup.
	body io.ReadCloser
}

// Close releases the HTTP connection and interrupts the SSE parser goroutine.
// Callers should drain the Events channel before calling Close to avoid losing
// buffered events. It is safe to call multiple times.
func (s *StreamResponse) Close() {
	if s == nil || s.body == nil {
		return
	}
	s.body.Close()
	s.body = nil
}

// Client wraps an *http.Client for making LLM streaming requests.
// Use DefaultClient for a pre-configured client, or create one with
// NewClient for custom settings.
type Client struct {
	httpClient *http.Client
}

// NewClient creates a new LLM HTTP client with the given http.Client.
// If httpClient is nil, http.DefaultClient is used.
func NewClient(httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{httpClient: httpClient}
}

// DefaultClient is a Client using http.DefaultClient. It is safe for
// concurrent use. Go's default transport provides connection pooling,
// HTTP/2 for TLS, and reasonable timeouts.
var DefaultClient = NewClient(nil)

// ExecuteStream sends the given request and returns a StreamResponse with
// SSE events parsed from the response body.
//
// The request is expected to be for a streaming LLM endpoint (the body
// should contain "stream": true or equivalent). The response is expected
// to be an SSE stream (Content-Type: text/event-stream).
//
// On success, the caller must call StreamResponse.Close() when done.
// On error (non-2xx status, connection failure), an error is returned and
// no cleanup is needed.
func (c *Client) ExecuteStream(ctx context.Context, req *Request) (*StreamResponse, error) {
	httpReq, err := buildHTTPRequest(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("llmhttp: failed to build request: %w", err)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("llmhttp: request failed: %w", err)
	}

	// Check for non-success status codes
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, &HTTPError{
			StatusCode: resp.StatusCode,
			Body:       string(body),
		}
	}

	// Start SSE parsing on the response body
	events, errc := sseclient.Stream(ctx, resp.Body)

	return &StreamResponse{
		StatusCode: resp.StatusCode,
		Headers:    resp.Header,
		Events:     events,
		Errc:       errc,
		body:       resp.Body,
	}, nil
}

// HTTPError represents a non-2xx HTTP response from the LLM provider.
type HTTPError struct {
	StatusCode int
	Body       string
}

func (e *HTTPError) Error() string {
	if e.Body != "" {
		return fmt.Sprintf("llmhttp: HTTP %d: %s", e.StatusCode, e.Body)
	}
	return fmt.Sprintf("llmhttp: HTTP %d", e.StatusCode)
}

// buildHTTPRequest converts a Request into a standard *http.Request.
func buildHTTPRequest(ctx context.Context, req *Request) (*http.Request, error) {
	if req == nil {
		return nil, fmt.Errorf("llmhttp: nil request")
	}

	var body io.Reader
	if req.Body != "" {
		body = strings.NewReader(req.Body)
	}

	httpReq, err := http.NewRequestWithContext(ctx, req.Method, req.URL, body)
	if err != nil {
		return nil, err
	}

	for k, v := range req.Headers {
		httpReq.Header.Set(k, v)
	}

	return httpReq, nil
}

// SensitiveHeaderKeys contains header name substrings that indicate sensitive
// values which should be redacted in logs and error messages. Matches are
// case-insensitive.
var SensitiveHeaderKeys = []string{
	"authorization",
	"cookie",
	"key",
	"secret",
	"token",
	"credential",
	"session",
	"auth",
}

// RedactHeaders returns a copy of headers with sensitive values replaced by
// "REDACTED". Use this when logging or including headers in error messages.
func RedactHeaders(headers map[string]string) map[string]string {
	redacted := make(map[string]string, len(headers))
	for k, v := range headers {
		lower := strings.ToLower(k)
		isSensitive := false
		for _, sensitive := range SensitiveHeaderKeys {
			if strings.Contains(lower, sensitive) {
				isSensitive = true
				break
			}
		}
		if isSensitive {
			redacted[k] = "REDACTED"
		} else {
			redacted[k] = v
		}
	}
	return redacted
}
