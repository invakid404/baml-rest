//go:build integration

package mockllm

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/goccy/go-json"
)

// Server is a mock LLM server for integration testing.
type Server struct {
	store  *ScenarioStore
	server *http.Server
	debug  bool
}

// NewServer creates a new mock LLM server.
func NewServer(addr string) *Server {
	s := &Server{
		store: NewScenarioStore(),
		debug: os.Getenv("MOCK_LLM_DEBUG") == "1",
	}

	mux := http.NewServeMux()

	// Admin API
	mux.HandleFunc("POST /_admin/scenarios", s.handleRegisterScenario)
	mux.HandleFunc("DELETE /_admin/scenarios", s.handleClearScenarios)
	mux.HandleFunc("DELETE /_admin/scenarios/{id}", s.handleDeleteScenario)
	mux.HandleFunc("GET /_admin/scenarios", s.handleListScenarios)
	mux.HandleFunc("GET /_admin/health", s.handleHealth)

	mux.HandleFunc("GET /_admin/scenarios/{id}/last-request", s.handleGetLastRequest)

	// OpenAI-compatible endpoints
	mux.HandleFunc("POST /v1/chat/completions", s.handleChatCompletions)
	mux.HandleFunc("POST /chat/completions", s.handleChatCompletions)

	s.server = &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	return s
}

// Store returns the scenario store for direct manipulation in tests.
func (s *Server) Store() *ScenarioStore {
	return s.store
}

// Start starts the server in the background.
func (s *Server) Start() error {
	go func() {
		if err := s.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("Mock LLM server error: %v", err)
		}
	}()
	return nil
}

// Shutdown gracefully shuts down the server.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.server.Shutdown(ctx)
}

// Addr returns the server's address.
func (s *Server) Addr() string {
	return s.server.Addr
}

func (s *Server) log(format string, args ...any) {
	if s.debug {
		log.Printf("[mockllm] "+format, args...)
	}
}

// Admin handlers

func (s *Server) handleRegisterScenario(w http.ResponseWriter, r *http.Request) {
	var scenario Scenario
	if err := json.NewDecoder(r.Body).Decode(&scenario); err != nil {
		http.Error(w, fmt.Sprintf("invalid JSON: %v", err), http.StatusBadRequest)
		return
	}

	if scenario.ID == "" {
		http.Error(w, "scenario ID is required", http.StatusBadRequest)
		return
	}

	if scenario.Provider == "" {
		scenario.Provider = "openai"
	}

	s.store.Register(&scenario)
	s.log("registered scenario: %s (provider=%s, content_len=%d)", scenario.ID, scenario.Provider, len(scenario.Content))

	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]string{"status": "created", "id": scenario.ID})
}

func (s *Server) handleClearScenarios(w http.ResponseWriter, r *http.Request) {
	s.store.Clear()
	s.log("cleared all scenarios")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "cleared"})
}

func (s *Server) handleDeleteScenario(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "scenario ID is required", http.StatusBadRequest)
		return
	}

	if s.store.Delete(id) {
		s.log("deleted scenario: %s", id)
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "deleted", "id": id})
	} else {
		http.Error(w, "scenario not found", http.StatusNotFound)
	}
}

func (s *Server) handleListScenarios(w http.ResponseWriter, r *http.Request) {
	scenarios := s.store.List()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(scenarios)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (s *Server) handleGetLastRequest(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "scenario ID is required", http.StatusBadRequest)
		return
	}

	req, ok := s.store.GetLastRequest(id)
	if !ok {
		http.Error(w, "no request captured for scenario", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write(req.Body)
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

func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to read body: %v", err), http.StatusBadRequest)
		return
	}

	var req chatCompletionsRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, fmt.Sprintf("invalid JSON: %v", err), http.StatusBadRequest)
		return
	}

	s.log("chat completions request: model=%s, stream=%v", req.Model, req.Stream)

	// Look up scenario by model name and get effective delay for this request
	scenario, effectiveDelay, ok := s.store.GetAndAdvance(req.Model)
	if !ok {
		s.log("scenario not found for model: %s", req.Model)
		http.Error(w, fmt.Sprintf("no scenario registered for model: %s", req.Model), http.StatusNotFound)
		return
	}

	// Capture request body for test inspection
	s.store.CaptureRequest(req.Model, body)

	s.log("effective initial delay for this request: %dms", effectiveDelay)

	provider, err := GetProvider(scenario.Provider)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if req.Stream {
		s.handleStreamingResponse(w, r, scenario, provider, effectiveDelay)
	} else {
		s.handleNonStreamingResponse(w, scenario, provider, effectiveDelay)
	}
}

func (s *Server) handleStreamingResponse(w http.ResponseWriter, r *http.Request, scenario *Scenario, provider Provider, effectiveDelay int) {
	ctx := r.Context()

	// Use a timeout context to prevent hanging forever
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	if err := StreamResponse(ctx, w, scenario, provider, effectiveDelay); err != nil {
		s.log("streaming error: %v", err)
		// Connection likely already closed, nothing we can do
	}
}

func (s *Server) handleNonStreamingResponse(w http.ResponseWriter, scenario *Scenario, provider Provider, effectiveDelay int) {
	// Apply initial delay for non-streaming too
	if effectiveDelay > 0 {
		time.Sleep(time.Duration(effectiveDelay) * time.Millisecond)
	}

	// Check for failure
	if scenario.FailAfter > 0 && scenario.FailAfter <= 1 {
		switch scenario.FailureMode {
		case "500":
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		case "timeout":
			time.Sleep(10 * time.Minute) // Will likely timeout
			return
		}
	}

	data, err := provider.FormatNonStreaming(scenario.Content)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to format response: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", provider.ContentType(false))
	w.Write(data)
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

// GetLastRequest returns the last request body received for a scenario.
func (c *Client) GetLastRequest(ctx context.Context, scenarioID string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("%s/_admin/scenarios/%s/last-request", c.baseURL, scenarioID), nil)
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
