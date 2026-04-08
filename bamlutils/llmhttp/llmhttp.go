// Package llmhttp provides a thin HTTP client wrapper for executing LLM
// requests (both streaming and non-streaming). It converts a generic request
// specification (URL, method, headers, body) into an HTTP request, sends it,
// and returns either a stream of SSE events or the complete response body.
//
// This package does NOT depend on the BAML SDK. The conversion from
// baml.HTTPRequest to the types in this package is done in the generated
// adapter code.
package llmhttp

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/invakid404/baml-rest/bamlutils/sseclient"
	"github.com/invakid404/baml-rest/bamlutils/urlrewrite"
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

// Response represents a completed non-streaming HTTP response from an LLM
// provider. Unlike StreamResponse, the full body has already been read.
type Response struct {
	// StatusCode is the HTTP response status code.
	StatusCode int

	// Headers are the HTTP response headers.
	Headers http.Header

	// Body is the complete response body as a string.
	Body string
}

// MaxResponseBodyBytes is the maximum response body size that Execute() will
// read. This prevents unbounded memory consumption from misconfigured
// endpoints. 16 MiB is generous for any LLM completion.
const MaxResponseBodyBytes = 16 << 20 // 16 MiB

// MaxErrorBodyBytes is the maximum response body read for non-2xx error
// responses. Error bodies are included in HTTPError for diagnostics but
// do not need the full MaxResponseBodyBytes allowance.
const MaxErrorBodyBytes = 4096

// Client wraps an *http.Client for making LLM requests (streaming and
// non-streaming). Use DefaultClient for a pre-configured client, or create
// one with NewClient for custom settings.
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

// defaultLLMTransport is an HTTP transport tuned for LLM provider traffic.
//
// The BuildRequest path moves outbound HTTP calls from the BAML Rust runtime
// into Go. Go's http.DefaultTransport has MaxIdleConnsPerHost=2, which is
// far too low for a worker process that funnels many concurrent requests to
// the same small set of provider hosts (api.openai.com, api.anthropic.com,
// etc.). With the default, almost every request under concurrent load creates
// a new TCP+TLS connection (~100-300ms handshake overhead per request).
//
// The values below mirror http.DefaultTransport (Go 1.26) with connection
// pool sizes raised for the LLM provider traffic pattern: many concurrent
// requests to few hosts.
var defaultLLMTransport = &http.Transport{
	Proxy: http.ProxyFromEnvironment,
	DialContext: (&net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}).DialContext,
	ForceAttemptHTTP2: true,
	TLSClientConfig: &tls.Config{
		MinVersion: tls.VersionTLS12,
	},
	TLSHandshakeTimeout:   10 * time.Second,
	MaxIdleConns:          256,
	MaxIdleConnsPerHost:   64,
	IdleConnTimeout:       90 * time.Second,
	ExpectContinueTimeout: 1 * time.Second,
}

// DefaultClient is a Client with a transport tuned for LLM provider traffic.
// It is safe for concurrent use. See defaultLLMTransport for details on
// why the defaults differ from http.DefaultTransport.
var DefaultClient = NewClient(&http.Client{Transport: defaultLLMTransport})

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
	if c == nil || c.httpClient == nil {
		return nil, fmt.Errorf("llmhttp: nil client")
	}

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
		body, _ := io.ReadAll(io.LimitReader(resp.Body, MaxErrorBodyBytes))
		resp.Body.Close()
		return nil, &HTTPError{
			StatusCode: resp.StatusCode,
			Body:       string(body),
		}
	}

	// Validate Content-Type for SSE — reject anything that isn't explicitly
	// text/event-stream. Missing Content-Type is also rejected to fail closed
	// against proxy error pages or misconfigured servers. NDJSON is not
	// accepted because the SSE parser requires data: framing.
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	if !strings.Contains(ct, "text/event-stream") {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, MaxErrorBodyBytes))
		resp.Body.Close()
		if ct == "" {
			return nil, fmt.Errorf("llmhttp: missing Content-Type header (expected text/event-stream): %s", string(body))
		}
		return nil, fmt.Errorf("llmhttp: unexpected Content-Type %q (expected text/event-stream): %s", ct, string(body))
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

// DefaultCallTimeout is the maximum time Execute() will wait for a
// non-streaming LLM response when the caller's context has no deadline.
// This prevents a stalled provider from pinning a worker indefinitely
// after the early heartbeat has satisfied the pool's hung detector.
//
// The value is deliberately generous: most non-streaming completions finish
// in under 60 s, but complex prompts with large output can take longer.
// Callers that need a tighter or looser bound should set their own deadline
// on the context passed to Execute().
const DefaultCallTimeout = 5 * time.Minute

// Execute sends the given request and returns the complete response.
//
// Unlike ExecuteStream, this reads the entire response body (up to
// MaxResponseBodyBytes) and returns it as a string. The request is expected
// to be for a non-streaming LLM endpoint (the body should contain
// "stream": false or equivalent). No Content-Type validation is performed
// since non-streaming responses are typically application/json.
//
// If the provided context has no deadline, Execute wraps it with
// DefaultCallTimeout so that a stalled upstream cannot block forever.
// Callers that supply their own deadline are not affected.
//
// The optional onSuccess callback, if non-nil, is invoked after the HTTP
// response returns a 2xx status but before the response body is read.
// This allows the caller to emit a liveness signal (e.g. heartbeat) once
// a real provider response has been confirmed, without waiting for the
// full body to be buffered. This is important because body reads can be
// slow for large responses and the caller may have external timeouts
// (e.g. pool hung detection) that need a signal before the body is fully
// available.
//
// On error (non-2xx status, connection failure, timeout), an error is returned.
func (c *Client) Execute(ctx context.Context, req *Request, onSuccess func()) (*Response, error) {
	if c == nil || c.httpClient == nil {
		return nil, fmt.Errorf("llmhttp: nil client")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	// Enforce a deadline on the outbound call if the caller didn't set one.
	// The HTTP client is shared with ExecuteStream (which must not have a
	// fixed timeout), so the timeout is applied per-request via context
	// rather than on the http.Client itself.
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, DefaultCallTimeout)
		defer cancel()
	}

	httpReq, err := buildHTTPRequest(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("llmhttp: failed to build request: %w", err)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("llmhttp: request failed: %w", err)
	}
	defer resp.Body.Close()

	// Check HTTP status before reading the full body. For error responses,
	// read only a small diagnostic body (4KB, same as ExecuteStream) to
	// avoid tying up the worker on a large or slow error body. The full
	// MaxResponseBodyBytes limit is reserved for successful responses only.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, MaxErrorBodyBytes))
		return nil, &HTTPError{
			StatusCode: resp.StatusCode,
			Body:       string(errBody),
		}
	}

	// Signal that a real 2xx response has been received from the provider.
	// Fired before body read so the caller can emit liveness signals
	// (e.g. heartbeat) without waiting for the full body to buffer.
	if onSuccess != nil {
		onSuccess()
	}

	// Read successful response body with size limit to prevent unbounded
	// memory usage. Read MaxResponseBodyBytes+1 to detect truncation: if
	// we get exactly that many bytes, the response exceeded the limit.
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBodyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("llmhttp: failed to read response body: %w", err)
	}

	if int64(len(body)) > MaxResponseBodyBytes {
		return nil, fmt.Errorf("llmhttp: response body exceeds maximum size (%d bytes)", MaxResponseBodyBytes)
	}

	return &Response{
		StatusCode: resp.StatusCode,
		Headers:    resp.Header,
		Body:       string(body),
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
// If URL rewrite rules are configured (via BAML_REST_BASE_URL_REWRITES),
// the request URL is rewritten before the HTTP request is created.
func buildHTTPRequest(ctx context.Context, req *Request) (*http.Request, error) {
	if req == nil {
		return nil, fmt.Errorf("llmhttp: nil request")
	}

	// Apply URL rewrite rules (catch-all for BuildRequest path)
	url := req.URL
	if rules := urlrewrite.GlobalRules(); len(rules) > 0 {
		url = urlrewrite.ApplyToURL(url, rules)
	}

	var body io.Reader
	if req.Body != "" {
		body = strings.NewReader(req.Body)
	}

	httpReq, err := http.NewRequestWithContext(ctx, req.Method, url, body)
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
