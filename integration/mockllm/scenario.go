//go:build integration

package mockllm

import (
	"sync"
)

// Scenario defines a mock LLM response configuration.
// The scenario ID is matched against the model name in requests.
type Scenario struct {
	// ID is the scenario identifier, matched against the "model" field in requests
	ID string `json:"id"`

	// Provider determines the response format ("openai", "anthropic", etc.)
	// Currently only "openai" is supported
	Provider string `json:"provider"`

	// Content is the actual LLM response content to return
	Content string `json:"content"`

	// ChunkSize is the number of characters per SSE chunk (0 = single non-streaming response)
	ChunkSize int `json:"chunk_size"`

	// InitialDelayMs is the delay before the first chunk (simulates "thinking")
	InitialDelayMs int `json:"initial_delay_ms"`

	// InitialDelayMsPerRequest allows different delays for each request (0-indexed).
	// If specified, the Nth request to this scenario uses InitialDelayMsPerRequest[N].
	// If N >= len(InitialDelayMsPerRequest), uses the last value in the slice.
	// If empty/nil, falls back to InitialDelayMs.
	InitialDelayMsPerRequest []int `json:"initial_delay_ms_per_request,omitempty"`

	// ChunkDelayMs is the base delay between chunks
	ChunkDelayMs int `json:"chunk_delay_ms"`

	// ChunkJitterMs is random jitter added to ChunkDelayMs (0-N ms)
	ChunkJitterMs int `json:"chunk_jitter_ms"`

	// FailAfter causes the response to fail after N chunks (0 = don't fail)
	FailAfter int `json:"fail_after,omitempty"`

	// FailureMode determines how to fail: "timeout", "500", "disconnect"
	FailureMode string `json:"failure_mode,omitempty"`
}

// ScenarioStore provides thread-safe storage for test scenarios.
type ScenarioStore struct {
	mu            sync.RWMutex
	scenarios     map[string]*Scenario
	requestCounts map[string]int // tracks request count per scenario ID
}

// NewScenarioStore creates a new empty scenario store.
func NewScenarioStore() *ScenarioStore {
	return &ScenarioStore{
		scenarios:     make(map[string]*Scenario),
		requestCounts: make(map[string]int),
	}
}

// Register adds or updates a scenario.
func (s *ScenarioStore) Register(scenario *Scenario) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scenarios[scenario.ID] = scenario
}

// Get retrieves a scenario by ID.
func (s *ScenarioStore) Get(id string) (*Scenario, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	scenario, ok := s.scenarios[id]
	return scenario, ok
}

// GetAndAdvance retrieves a scenario by ID and returns the effective InitialDelayMs
// for this request, then increments the request counter.
// If InitialDelayMsPerRequest is configured, uses that based on request count,
// otherwise falls back to InitialDelayMs.
func (s *ScenarioStore) GetAndAdvance(id string) (*Scenario, int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	scenario, ok := s.scenarios[id]
	if !ok {
		return nil, 0, false
	}

	// Get current request count and increment
	reqCount := s.requestCounts[id]
	s.requestCounts[id] = reqCount + 1

	// Calculate effective initial delay
	effectiveDelay := scenario.InitialDelayMs
	if len(scenario.InitialDelayMsPerRequest) > 0 {
		if reqCount < len(scenario.InitialDelayMsPerRequest) {
			effectiveDelay = scenario.InitialDelayMsPerRequest[reqCount]
		} else {
			// Use last value for subsequent requests
			effectiveDelay = scenario.InitialDelayMsPerRequest[len(scenario.InitialDelayMsPerRequest)-1]
		}
	}

	return scenario, effectiveDelay, true
}

// Delete removes a scenario by ID.
func (s *ScenarioStore) Delete(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, existed := s.scenarios[id]
	delete(s.scenarios, id)
	delete(s.requestCounts, id)
	return existed
}

// Clear removes all scenarios.
func (s *ScenarioStore) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scenarios = make(map[string]*Scenario)
	s.requestCounts = make(map[string]int)
}

// List returns all registered scenarios.
func (s *ScenarioStore) List() []*Scenario {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*Scenario, 0, len(s.scenarios))
	for _, scenario := range s.scenarios {
		result = append(result, scenario)
	}
	return result
}
