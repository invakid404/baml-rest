//go:build integration

package testutil

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/goccy/go-json"
)

// BAMLRestClient is an HTTP client for testing baml-rest endpoints.
type BAMLRestClient struct {
	baseURL string
	http    *http.Client
}

// NewBAMLRestClient creates a new client for the baml-rest server.
func NewBAMLRestClient(baseURL string) *BAMLRestClient {
	return &BAMLRestClient{
		baseURL: strings.TrimSuffix(baseURL, "/"),
		http:    &http.Client{Timeout: 2 * time.Minute},
	}
}

// CallRequest represents a request to /call or /call-with-raw endpoints.
type CallRequest struct {
	Method  string         // Method name
	Input   map[string]any // Input parameters
	Options *BAMLOptions   // Optional BAML options (client registry, type builder)
}

// BAMLOptions contains optional configuration for BAML method calls.
type BAMLOptions struct {
	ClientRegistry *ClientRegistry `json:"client_registry,omitempty"`
	TypeBuilder    *TypeBuilder    `json:"type_builder,omitempty"`
}

// ClientRegistry allows overriding client configuration.
type ClientRegistry struct {
	Primary string            `json:"primary"`
	Clients []*ClientProperty `json:"clients"`
}

// ClientProperty defines a client configuration.
type ClientProperty struct {
	Name        string         `json:"name"`
	Provider    string         `json:"provider"`
	RetryPolicy *string        `json:"retry_policy,omitempty"`
	Options     map[string]any `json:"options,omitempty"`
}

// TypeBuilder allows injecting dynamic types.
type TypeBuilder struct {
	BAMLSnippets []string `json:"baml_snippets,omitempty"`
}

// CallResponse represents a response from /call endpoint.
type CallResponse struct {
	StatusCode int
	Body       json.RawMessage
	Error      string
}

// CallWithRawResponse represents a response from /call-with-raw endpoint.
type CallWithRawResponse struct {
	StatusCode int
	Data       json.RawMessage `json:"data"`
	Raw        string          `json:"raw"`
	Error      string
}

// Call executes a /call/{method} request.
func (c *BAMLRestClient) Call(ctx context.Context, req CallRequest) (*CallResponse, error) {
	body, err := buildRequestBody(req.Input, req.Options)
	if err != nil {
		return nil, err
	}

	url := fmt.Sprintf("%s/call/%s", c.baseURL, req.Method)
	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	result := &CallResponse{
		StatusCode: resp.StatusCode,
	}

	if resp.StatusCode >= 400 {
		result.Error = string(respBody)
	} else {
		result.Body = respBody
	}

	return result, nil
}

// CallWithRaw executes a /call-with-raw/{method} request.
func (c *BAMLRestClient) CallWithRaw(ctx context.Context, req CallRequest) (*CallWithRawResponse, error) {
	body, err := buildRequestBody(req.Input, req.Options)
	if err != nil {
		return nil, err
	}

	url := fmt.Sprintf("%s/call-with-raw/%s", c.baseURL, req.Method)
	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	result := &CallWithRawResponse{
		StatusCode: resp.StatusCode,
	}

	if resp.StatusCode >= 400 {
		result.Error = string(respBody)
	} else {
		if err := json.Unmarshal(respBody, result); err != nil {
			return nil, fmt.Errorf("failed to unmarshal response: %w", err)
		}
	}

	return result, nil
}

// StreamEvent represents a single SSE event from /stream endpoints.
type StreamEvent struct {
	Event string          // Event type (e.g., "message", "reset", "error")
	Data  json.RawMessage // Event data
	Raw   string          // For stream-with-raw, the raw LLM output at this point
}

// ParseStreamEvent parses a stream event's data into the target.
func (e *StreamEvent) ParseData(target any) error {
	return json.Unmarshal(e.Data, target)
}

// Stream executes a /stream/{method} request and returns a channel of events.
func (c *BAMLRestClient) Stream(ctx context.Context, req CallRequest) (<-chan StreamEvent, <-chan error) {
	return c.streamRequest(ctx, fmt.Sprintf("%s/stream/%s", c.baseURL, req.Method), req)
}

// StreamWithRaw executes a /stream-with-raw/{method} request and returns a channel of events.
func (c *BAMLRestClient) StreamWithRaw(ctx context.Context, req CallRequest) (<-chan StreamEvent, <-chan error) {
	return c.streamRequest(ctx, fmt.Sprintf("%s/stream-with-raw/%s", c.baseURL, req.Method), req)
}

func (c *BAMLRestClient) streamRequest(ctx context.Context, url string, req CallRequest) (<-chan StreamEvent, <-chan error) {
	events := make(chan StreamEvent)
	errs := make(chan error, 1)

	go func() {
		defer close(events)
		defer close(errs)

		body, err := buildRequestBody(req.Input, req.Options)
		if err != nil {
			errs <- err
			return
		}

		httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
		if err != nil {
			errs <- err
			return
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Accept", "text/event-stream")

		resp, err := c.http.Do(httpReq)
		if err != nil {
			errs <- err
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode >= 400 {
			body, _ := io.ReadAll(resp.Body)
			errs <- fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
			return
		}

		if err := parseSSE(ctx, resp.Body, events); err != nil {
			errs <- err
		}
	}()

	return events, errs
}

// ParseRequest represents a request to /parse endpoint.
type ParseRequest struct {
	Method  string       // Method name
	Raw     string       // Raw LLM output to parse
	Options *BAMLOptions // Optional BAML options
}

// ParseResponse represents a response from /parse endpoint.
type ParseResponse struct {
	StatusCode int
	Data       json.RawMessage
	Error      string
}

// Parse executes a /parse/{method} request.
func (c *BAMLRestClient) Parse(ctx context.Context, req ParseRequest) (*ParseResponse, error) {
	input := map[string]any{"raw": req.Raw}
	body, err := buildRequestBody(input, req.Options)
	if err != nil {
		return nil, err
	}

	url := fmt.Sprintf("%s/parse/%s", c.baseURL, req.Method)
	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	result := &ParseResponse{
		StatusCode: resp.StatusCode,
	}

	if resp.StatusCode >= 400 {
		result.Error = string(respBody)
	} else {
		result.Data = respBody
	}

	return result, nil
}

// Health checks the /health endpoint.
func (c *BAMLRestClient) Health(ctx context.Context) error {
	url := fmt.Sprintf("%s/health", c.baseURL)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("health check failed: %d %s", resp.StatusCode, string(body))
	}

	return nil
}

// KillWorkerResult represents the response from /_debug/kill-worker.
type KillWorkerResult struct {
	Status        string `json:"status"`
	WorkerID      int    `json:"worker_id"`
	InFlightCount int    `json:"in_flight_count"`
	GotFirstByte  []bool `json:"got_first_byte"`
	Error         string `json:"error,omitempty"`
}

// KillWorker calls the /_debug/kill-worker endpoint to kill a worker mid-request.
// This is only available in debug builds.
func (c *BAMLRestClient) KillWorker(ctx context.Context) (*KillWorkerResult, error) {
	url := fmt.Sprintf("%s/_debug/kill-worker", c.baseURL)
	req, err := http.NewRequestWithContext(ctx, "POST", url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var result KillWorkerResult
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response: %w", err)
	}

	return &result, nil
}

// ConfigResult represents the response from /_debug/config.
type ConfigResult struct {
	Status              string `json:"status"`
	FirstByteTimeoutMs  int64  `json:"first_byte_timeout_ms"`
	Error               string `json:"error,omitempty"`
}

// SetFirstByteTimeout calls the /_debug/config endpoint to configure the first byte timeout.
// This is only available in debug builds.
func (c *BAMLRestClient) SetFirstByteTimeout(ctx context.Context, timeoutMs int64) (*ConfigResult, error) {
	reqBody := map[string]any{
		"first_byte_timeout_ms": timeoutMs,
	}
	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	url := fmt.Sprintf("%s/_debug/config", c.baseURL)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var result ConfigResult
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response: %w", err)
	}

	return &result, nil
}

// GoroutinesResult represents the response from /_debug/goroutines.
type GoroutinesResult struct {
	Status        string                   `json:"status"`
	TotalCount    int                      `json:"total_count"`
	Filter        string                   `json:"filter,omitempty"`
	MatchCount    int                      `json:"match_count,omitempty"`
	MatchedStacks []string                 `json:"matched_stacks,omitempty"`
	Stacks        string                   `json:"stacks,omitempty"`
	Workers       []WorkerGoroutinesResult `json:"workers,omitempty"`
	Error         string                   `json:"error,omitempty"`
}

// WorkerGoroutinesResult represents goroutine data from a single worker.
type WorkerGoroutinesResult struct {
	WorkerID      int      `json:"worker_id"`
	TotalCount    int      `json:"total_count,omitempty"`
	MatchCount    int      `json:"match_count,omitempty"`
	MatchedStacks []string `json:"matched_stacks,omitempty"`
	Error         string   `json:"error,omitempty"`
}

// GetGoroutines fetches goroutine information from baml-rest.
// If filter is provided, only goroutines matching those patterns are counted.
// Filter should be a comma-separated list of patterns (e.g., "pool.,workerplugin.").
func (c *BAMLRestClient) GetGoroutines(ctx context.Context, filter string) (*GoroutinesResult, error) {
	url := fmt.Sprintf("%s/_debug/goroutines", c.baseURL)
	if filter != "" {
		url += "?filter=" + filter
	}

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var result GoroutinesResult
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response: %w", err)
	}

	return &result, nil
}

func buildRequestBody(input map[string]any, opts *BAMLOptions) ([]byte, error) {
	body := make(map[string]any)
	for k, v := range input {
		body[k] = v
	}

	if opts != nil {
		body["__baml_options__"] = opts
	}

	return json.Marshal(body)
}

func parseSSE(ctx context.Context, r io.Reader, events chan<- StreamEvent) error {
	scanner := bufio.NewScanner(r)
	var currentEvent StreamEvent
	var dataBuffer bytes.Buffer

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		line := scanner.Text()

		if line == "" {
			// Empty line signals end of event
			if dataBuffer.Len() > 0 {
				currentEvent.Data = dataBuffer.Bytes()

				// For stream-with-raw, extract the raw field
				var rawData struct {
					Data json.RawMessage `json:"data"`
					Raw  string          `json:"raw"`
				}
				if err := json.Unmarshal(currentEvent.Data, &rawData); err == nil && rawData.Raw != "" {
					currentEvent.Raw = rawData.Raw
					currentEvent.Data = rawData.Data
				}

				events <- currentEvent
				currentEvent = StreamEvent{}
				dataBuffer.Reset()
			}
			continue
		}

		if strings.HasPrefix(line, "event:") {
			currentEvent.Event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		} else if strings.HasPrefix(line, "data:") {
			data := strings.TrimPrefix(line, "data:")
			if dataBuffer.Len() > 0 {
				dataBuffer.WriteByte('\n')
			}
			dataBuffer.WriteString(data)
		}
	}

	// Handle any remaining data
	if dataBuffer.Len() > 0 {
		currentEvent.Data = dataBuffer.Bytes()
		events <- currentEvent
	}

	return scanner.Err()
}

// CreateTestClient creates a client registry that points to the mock LLM server.
func CreateTestClient(mockLLMURL string, scenarioID string) *ClientRegistry {
	return &ClientRegistry{
		Primary: "TestClient",
		Clients: []*ClientProperty{
			{
				Name:     "TestClient",
				Provider: "openai",
				Options: map[string]any{
					"model":    scenarioID,
					"base_url": mockLLMURL,
					"api_key":  "test-key",
				},
			},
		},
	}
}
