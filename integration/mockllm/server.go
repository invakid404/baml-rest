//go:build integration

package mockllm

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/goccy/go-json"
	"github.com/gofiber/fiber/v3"
)

// Server is a mock LLM server for integration testing.
type Server struct {
	store *ScenarioStore
	app   *fiber.App
	addr  string
	debug bool
}

// NewServer creates a new mock LLM server.
func NewServer(addr string) *Server {
	s := &Server{
		store: NewScenarioStore(),
		addr:  addr,
		debug: os.Getenv("MOCK_LLM_DEBUG") == "1",
	}

	app := fiber.New()

	// Admin API
	app.Post("/_admin/scenarios", s.handleRegisterScenario)
	app.Delete("/_admin/scenarios", s.handleClearScenarios)
	app.Delete("/_admin/scenarios/:id", s.handleDeleteScenario)
	app.Get("/_admin/scenarios", s.handleListScenarios)
	app.Get("/_admin/health", s.handleHealth)

	app.Get("/_admin/scenarios/:id/last-request", s.handleGetLastRequest)
	app.Get("/_admin/scenarios/:id/request-count", s.handleGetRequestCount)
	app.Post("/_admin/scenarios/:id/reset-count", s.handleResetRequestCount)

	// OpenAI-compatible endpoints
	app.Post("/v1/chat/completions", s.handleChatCompletions)
	app.Post("/chat/completions", s.handleChatCompletions)

	// Anthropic Messages API endpoint. The Anthropic provider in BAML appends
	// "/v1/messages" to the configured base_url, so client base_url should be
	// the bare scheme://host of the mock server (e.g. http://mockllm:8080).
	app.Post("/v1/messages", s.handleAnthropicMessages)

	s.app = app

	return s
}

// Store returns the scenario store for direct manipulation in tests.
func (s *Server) Store() *ScenarioStore {
	return s.store
}

// Start starts the server in the background.
func (s *Server) Start() error {
	go func() {
		if err := s.app.Listen(s.addr, fiber.ListenConfig{DisableStartupMessage: true}); err != nil {
			log.Printf("Mock LLM server error: %v", err)
		}
	}()
	return nil
}

// Shutdown gracefully shuts down the server.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.app.ShutdownWithContext(ctx)
}

// Addr returns the server's address.
func (s *Server) Addr() string {
	return s.addr
}

func (s *Server) log(format string, args ...any) {
	if s.debug {
		log.Printf("[mockllm] "+format, args...)
	}
}

// Admin handlers

func (s *Server) handleRegisterScenario(c fiber.Ctx) error {
	var scenario Scenario
	if err := json.Unmarshal(c.Body(), &scenario); err != nil {
		return c.Status(fiber.StatusBadRequest).SendString(fmt.Sprintf("invalid JSON: %v", err))
	}

	if scenario.ID == "" {
		return c.Status(fiber.StatusBadRequest).SendString("scenario ID is required")
	}

	if scenario.Provider == "" {
		scenario.Provider = "openai"
	}

	s.store.Register(&scenario)
	s.log("registered scenario: %s (provider=%s, content_len=%d)", scenario.ID, scenario.Provider, len(scenario.Content))

	return c.Status(fiber.StatusCreated).JSON(map[string]string{"status": "created", "id": scenario.ID})
}

func (s *Server) handleClearScenarios(c fiber.Ctx) error {
	s.store.Clear()
	s.log("cleared all scenarios")
	return c.Status(fiber.StatusOK).JSON(map[string]string{"status": "cleared"})
}

func (s *Server) handleDeleteScenario(c fiber.Ctx) error {
	id := c.Params("id")
	if id == "" {
		return c.Status(fiber.StatusBadRequest).SendString("scenario ID is required")
	}

	if s.store.Delete(id) {
		s.log("deleted scenario: %s", id)
		return c.Status(fiber.StatusOK).JSON(map[string]string{"status": "deleted", "id": id})
	}

	return c.Status(fiber.StatusNotFound).SendString("scenario not found")
}

func (s *Server) handleListScenarios(c fiber.Ctx) error {
	scenarios := s.store.List()
	return c.JSON(scenarios)
}

func (s *Server) handleHealth(c fiber.Ctx) error {
	return c.Status(fiber.StatusOK).JSON(map[string]string{"status": "ok"})
}

func (s *Server) handleGetLastRequest(c fiber.Ctx) error {
	id := c.Params("id")
	if id == "" {
		return c.Status(fiber.StatusBadRequest).SendString("scenario ID is required")
	}

	req, ok := s.store.GetLastRequest(id)
	if !ok {
		return c.Status(fiber.StatusNotFound).SendString("no request captured for scenario")
	}

	c.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
	return c.Send(req.Body)
}

func (s *Server) handleGetRequestCount(c fiber.Ctx) error {
	id := c.Params("id")
	if id == "" {
		return c.Status(fiber.StatusBadRequest).SendString("scenario ID is required")
	}
	count, ok := s.store.GetRequestCountIfExists(id)
	if !ok {
		return c.Status(fiber.StatusNotFound).SendString("scenario not found")
	}
	return c.JSON(fiber.Map{"count": count})
}

func (s *Server) handleResetRequestCount(c fiber.Ctx) error {
	id := c.Params("id")
	if id == "" {
		return c.Status(fiber.StatusBadRequest).SendString("scenario ID is required")
	}
	if !s.store.ResetRequestCountIfExists(id) {
		return c.Status(fiber.StatusNotFound).SendString("scenario not found")
	}
	return c.SendStatus(fiber.StatusNoContent)
}

// LLM endpoint handlers

type chatCompletionsRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
	Stream   bool          `json:"stream"`
}

type chatMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"` // Can be string or array of content parts
}

func (m *chatMessage) GetContent() string {
	// Try to unmarshal as string first
	var str string
	if err := json.Unmarshal(m.Content, &str); err == nil {
		return str
	}

	// Try to unmarshal as array of content parts
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(m.Content, &parts); err == nil {
		var texts []string
		for _, part := range parts {
			if part.Type == "text" {
				texts = append(texts, part.Text)
			}
		}
		return strings.Join(texts, "")
	}

	return ""
}

func (s *Server) handleChatCompletions(c fiber.Ctx) error {
	body := append([]byte(nil), c.Body()...)

	var req chatCompletionsRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return c.Status(fiber.StatusBadRequest).SendString(fmt.Sprintf("invalid JSON: %v", err))
	}

	s.log("chat completions request: model=%s, stream=%v", req.Model, req.Stream)

	return s.dispatchScenario(c, body, req.Model, req.Stream)
}

// anthropicMessagesRequest is the minimal Anthropic Messages API request
// shape needed for scenario routing. We only use Model (for scenario lookup)
// and Stream; everything else is consumed by the response shape provided by
// AnthropicProvider so we don't deserialize it here.
type anthropicMessagesRequest struct {
	Model  string `json:"model"`
	Stream bool   `json:"stream"`
}

func (s *Server) handleAnthropicMessages(c fiber.Ctx) error {
	body := append([]byte(nil), c.Body()...)

	var req anthropicMessagesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return c.Status(fiber.StatusBadRequest).SendString(fmt.Sprintf("invalid JSON: %v", err))
	}

	s.log("anthropic messages request: model=%s, stream=%v", req.Model, req.Stream)

	return s.dispatchScenario(c, body, req.Model, req.Stream)
}

// dispatchScenario is the shared scenario lookup + provider dispatch path
// used by both the OpenAI and Anthropic endpoints. The endpoint-specific
// handlers stay narrow (just request decoding) and delegate the rest here.
func (s *Server) dispatchScenario(c fiber.Ctx, body []byte, model string, stream bool) error {
	// Look up scenario by model name and get effective delay for this request
	scenario, effectiveDelay, ok := s.store.GetAndAdvance(model)
	if !ok {
		s.log("scenario not found for model: %s", model)
		return c.Status(fiber.StatusNotFound).SendString(fmt.Sprintf("no scenario registered for model: %s", model))
	}

	// Capture request body for test inspection
	s.store.CaptureRequest(model, body)

	s.log("effective initial delay for this request: %dms", effectiveDelay)

	provider, err := GetProvider(scenario.Provider)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).SendString(err.Error())
	}

	// Immediate failure: FailAfter=1 with FailureMode="500" returns HTTP 500
	// BEFORE the response body (streaming or otherwise) starts. This is needed
	// because once SSE streaming begins, the HTTP status is already 200 and
	// cannot be changed — a mid-stream disconnect is not reliably treated as
	// an error by all LLM runtimes.
	if scenario.FailAfter > 0 && scenario.FailAfter <= 1 && scenario.FailureMode == "500" {
		// Honour effectiveDelay so the failure timing is consistent with the
		// rest of the mock scenario model (e.g. for timeout-based retry tests).
		if effectiveDelay > 0 {
			reqCtx := c.Context()
			if requestCtx := c.RequestCtx(); requestCtx != nil {
				reqCtx = requestCtx
			}
			if err := waitForDurationOrCancel(reqCtx, time.Duration(effectiveDelay)*time.Millisecond); err != nil {
				return err
			}
		}
		s.log("immediate 500 failure for scenario %s", scenario.ID)
		return c.Status(fiber.StatusInternalServerError).SendString("Internal Server Error")
	}

	if stream {
		return s.handleStreamingResponse(c, scenario, provider, effectiveDelay)
	}

	return s.handleNonStreamingResponse(c, scenario, provider, effectiveDelay)
}

func (s *Server) handleStreamingResponse(c fiber.Ctx, scenario *Scenario, provider Provider, effectiveDelay int) error {
	c.Set(fiber.HeaderContentType, provider.ContentType(true))
	c.Set(fiber.HeaderCacheControl, "no-cache")
	c.Set(fiber.HeaderConnection, "keep-alive")
	c.Set("X-Accel-Buffering", "no")

	streamParentCtx := c.Context()
	if reqCtx := c.RequestCtx(); reqCtx != nil {
		streamParentCtx = reqCtx
	}

	return c.SendStreamWriter(func(w *bufio.Writer) {
		streamCtx, cancel := context.WithCancel(streamParentCtx)
		defer cancel()

		if err := StreamResponse(streamCtx, w, scenario, provider, effectiveDelay); err != nil {
			s.log("streaming error: %v", err)
		}
	})
}

func waitForDurationOrCancel(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}

	timer := time.NewTimer(d)
	defer func() {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}()

	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Server) handleNonStreamingResponse(c fiber.Ctx, scenario *Scenario, provider Provider, effectiveDelay int) error {
	reqCtx := c.Context()
	if requestCtx := c.RequestCtx(); requestCtx != nil {
		reqCtx = requestCtx
	}

	// Apply initial delay for non-streaming too
	if err := waitForDurationOrCancel(reqCtx, time.Duration(effectiveDelay)*time.Millisecond); err != nil {
		return err
	}

	// Check for failure
	if scenario.FailAfter > 0 && scenario.FailAfter <= 1 {
		switch scenario.FailureMode {
		case "500":
			return c.Status(fiber.StatusInternalServerError).SendString("Internal Server Error")
		case "timeout":
			if err := waitForDurationOrCancel(reqCtx, 10*time.Minute); err != nil {
				return err
			}
			return nil
		}
	}

	// Anthropic with scenario.Thinking emits a leading thinking content block
	// before the text block. The base Provider interface stays text-only; the
	// thinking-aware shape is reached through the concrete Anthropic type.
	var (
		data []byte
		err  error
	)
	if anthropic, ok := provider.(*AnthropicProvider); ok && scenario.Thinking != "" {
		data, err = anthropic.FormatNonStreamingWithThinking(scenario.Content, scenario.Thinking)
	} else {
		data, err = provider.FormatNonStreaming(scenario.Content)
	}
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).SendString(fmt.Sprintf("failed to format response: %v", err))
	}

	c.Set(fiber.HeaderContentType, provider.ContentType(false))
	return c.Send(data)
}

// Client is an HTTP client for interacting with the mock server's admin API.
type Client struct {
	baseURL string
	http    *http.Client
}

// NewClient creates a new admin client for the mock server.
func NewClient(baseURL string) *Client {
	return &Client{
		baseURL: strings.TrimSuffix(baseURL, "/"),
		http:    &http.Client{Timeout: 10 * time.Second},
	}
}

// RegisterScenario registers a scenario with the mock server.
func (c *Client) RegisterScenario(ctx context.Context, scenario *Scenario) error {
	data, err := json.Marshal(scenario)
	if err != nil {
		return fmt.Errorf("failed to marshal scenario: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/_admin/scenarios", strings.NewReader(string(data)))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(body))
	}

	return nil
}

// ClearScenarios removes all registered scenarios.
func (c *Client) ClearScenarios(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, "DELETE", c.baseURL+"/_admin/scenarios", nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(body))
	}

	return nil
}

// DeleteScenario removes a single scenario by ID.
// Returns nil if the scenario was deleted or did not exist.
func (c *Client) DeleteScenario(ctx context.Context, scenarioID string) error {
	escapedID := url.PathEscape(scenarioID)
	req, err := http.NewRequestWithContext(ctx, "DELETE", fmt.Sprintf("%s/_admin/scenarios/%s", c.baseURL, escapedID), nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	// Accept both 200 (deleted) and 404 (already absent) as success.
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNotFound {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(body))
	}

	return nil
}

// GetLastRequest returns the last request body received for a scenario.
func (c *Client) GetLastRequest(ctx context.Context, scenarioID string) ([]byte, error) {
	escapedID := url.PathEscape(scenarioID)
	req, err := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("%s/_admin/scenarios/%s/last-request", c.baseURL, escapedID), nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(body))
	}

	return body, nil
}

// GetRequestCount returns the number of requests the mock server received for a scenario.
func (c *Client) GetRequestCount(ctx context.Context, scenarioID string) (int, error) {
	escapedID := url.PathEscape(scenarioID)
	req, err := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("%s/_admin/scenarios/%s/request-count", c.baseURL, escapedID), nil)
	if err != nil {
		return 0, fmt.Errorf("failed to create request: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return 0, fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(body))
	}
	var result struct {
		Count int `json:"count"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return 0, fmt.Errorf("failed to decode response: %w", err)
	}
	return result.Count, nil
}

// ResetRequestCount resets the request counter for a scenario to zero.
func (c *Client) ResetRequestCount(ctx context.Context, scenarioID string) error {
	escapedID := url.PathEscape(scenarioID)
	req, err := http.NewRequestWithContext(ctx, "POST", fmt.Sprintf("%s/_admin/scenarios/%s/reset-count", c.baseURL, escapedID), nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

// Health checks if the mock server is healthy.
func (c *Client) Health(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, "GET", c.baseURL+"/_admin/health", nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health check failed: status %d", resp.StatusCode)
	}

	return nil
}
