package bamlfuzz

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/invakid404/baml-rest/bamlutils"
)

// StreamingFailureEnvelope captures the full context around a
// disagreement on the streaming dynamic oracle. It mirrors
// DynamicFailureEnvelope's header fields and adds streaming-specific
// capture: the ordered partial sequence and final frame from each leg
// (dynclient iterator, REST /stream/_dynamic over SSE, REST
// /stream/_dynamic over NDJSON), the unary parse used for the
// streaming-vs-unary cross-check, the per-leg event-kind sequences, the
// chunk size mockllm paged the response with, and whether any leg saw a
// reset frame. The partial sequences are the evidence a developer needs
// to reconstruct how a final diverged, so they travel even though v1
// only asserts on the final frame.
//
// Release gates match the unary dynamic oracle: semantic equality of
// every final pairing always, and schema-aware key order on each leg's
// final frame when PreserveSchemaOrder is true. Order diagnostics land
// on OrderWarning before the envelope is dumped.
type StreamingFailureEnvelope struct {
	GeneratorVersion    string                         `json:"generator_version"`
	GeneratedAt         string                         `json:"generated_at"`
	RapidSeed           int64                          `json:"rapid_seed"`
	CaseIndex           int                            `json:"case_index"`
	CaseName            string                         `json:"case_name"`
	OracleMode          OracleMode                     `json:"oracle_mode"`
	PreserveSchemaOrder bool                           `json:"preserve_schema_order"`
	Schema              FuzzSchema                     `json:"schema"`
	DynamicSchema       *bamlutils.DynamicOutputSchema `json:"dynamic_schema,omitempty"`
	DynamicSkipReason   string                         `json:"dynamic_skip_reason,omitempty"`
	MockLLMScenarioID   string                         `json:"mockllm_scenario_id"`
	MockLLMContent      json.RawMessage                `json:"mockllm_content"`
	Expected            json.RawMessage                `json:"expected"`

	// ChunkSize is the mockllm per-chunk byte width used for this case.
	// >0 so the streaming/partial code path is genuinely exercised.
	ChunkSize int `json:"chunk_size"`

	// Per-leg partial sequences (full snapshots with placeholder nulls,
	// not deltas) captured in arrival order.
	DynclientPartials  []json.RawMessage `json:"dynclient_partials,omitempty"`
	RESTPartialsSSE    []json.RawMessage `json:"rest_partials_sse,omitempty"`
	RESTPartialsNDJSON []json.RawMessage `json:"rest_partials_ndjson,omitempty"`

	// Per-leg terminal frames + the unary parse the streaming finals
	// are cross-checked against.
	DynclientFinal  json.RawMessage `json:"dynclient_final,omitempty"`
	RESTFinalSSE    json.RawMessage `json:"rest_final_sse,omitempty"`
	RESTFinalNDJSON json.RawMessage `json:"rest_final_ndjson,omitempty"`
	UnaryParse      json.RawMessage `json:"unary_parse,omitempty"`

	// Ordered event-kind sequence per leg ("partial"/"final"/"reset"/
	// "metadata"/"error"). The "single clean final" check reads these.
	DynclientKinds  []string `json:"dynclient_kinds,omitempty"`
	RESTKindsSSE    []string `json:"rest_kinds_sse,omitempty"`
	RESTKindsNDJSON []string `json:"rest_kinds_ndjson,omitempty"`

	// SawReset is true when any leg emitted a reset frame. With the
	// deterministic single-attempt mock a reset is unexpected and
	// recorded as a finding.
	SawReset bool `json:"saw_reset,omitempty"`

	// Per-leg surface errors. Context/transport errors are harness
	// failures and never reach the envelope; these capture stream-level
	// or HTTP-status errors that are oracle signal.
	DynclientError  string `json:"dynclient_error,omitempty"`
	RESTErrorSSE    string `json:"rest_error_sse,omitempty"`
	RESTErrorNDJSON string `json:"rest_error_ndjson,omitempty"`
	UnaryError      string `json:"unary_error,omitempty"`

	SemanticDiff []SemanticDiffEntry `json:"semantic_diff,omitempty"`
	OrderWarning []string            `json:"order_warning,omitempty"`
	ReplayPath   string              `json:"replay_path"`
	Reproduction string              `json:"reproduction"`
	Metadata     CaseMetadata        `json:"metadata"`
}

// WriteStreamingReplayArtifact writes a StreamingFailureEnvelope to
// `dir` as a JSON file. Same on-disk format as WriteReplayArtifact
// (2-space indent, deterministic basename via sanitizeArtifactBasename);
// envelope.ReplayPath is stamped with the resulting path so the
// t.Errorf message can point at the artifact.
func WriteStreamingReplayArtifact(dir string, envelope *StreamingFailureEnvelope) (string, error) {
	if envelope == nil {
		return "", fmt.Errorf("bamlfuzz: nil envelope")
	}
	if envelope.GeneratorVersion == "" {
		envelope.GeneratorVersion = GeneratorVersion
	}
	if envelope.GeneratedAt == "" {
		envelope.GeneratedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("bamlfuzz: mkdir %s: %w", dir, err)
	}
	name := sanitizeArtifactBasename(envelope.CaseName, envelope.CaseIndex)
	path := filepath.Join(dir, name+".json")
	envelope.ReplayPath = path
	buf, err := json.MarshalIndent(envelope, "", "  ")
	if err != nil {
		return "", fmt.Errorf("bamlfuzz: marshal envelope: %w", err)
	}
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		return "", fmt.Errorf("bamlfuzz: write %s: %w", path, err)
	}
	return path, nil
}
