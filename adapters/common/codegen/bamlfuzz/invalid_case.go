package bamlfuzz

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/invakid404/baml-rest/bamlutils"
)

// InvalidOracleMode names the invalid-input oracle the test harness
// runs against an InvalidOracleCase. v1 covers two axes; both share the
// dynamic /call/_dynamic surface and the dynclient in-process path.
type InvalidOracleMode string

const (
	// InvalidDynamicSchema (Axis B): a valid base schema is lowered
	// then perturbed via one targeted mutation so the resulting
	// dynamic output_schema should be rejected by both dynclient and
	// REST. The oracle is OutcomeBothReject.
	InvalidDynamicSchema InvalidOracleMode = "invalid_dynamic_schema"
	// InvalidJSONCoercion (Axis C): a valid base schema, valid lowered
	// dynamic output_schema, but the mock LLM JSON response is
	// perturbed with one targeted edit. The oracle is OutcomeConditional:
	// both surfaces succeed → SemanticDiff empty; at least one errors
	// → both must error. Error text is not compared (the pre-flight
	// determinism check showed dynclient and REST emit divergent
	// surface-layer prefixes around an identical BAML diagnostic core).
	InvalidJSONCoercion InvalidOracleMode = "invalid_json_coercion"
)

// ExpectedOutcome describes the assertion shape the oracle applies to a
// case. Kept small — finer-grained classifications (RejectAtValidate vs
// RejectAtRuntime) are tempting but would require BAML to expose a
// stable error-class taxonomy, which it does not.
type ExpectedOutcome string

const (
	// OutcomeBothReject: both dynclient and REST must return a non-nil
	// error (REST: status >= 400). Used by Axis B.
	OutcomeBothReject ExpectedOutcome = "both_reject"
	// OutcomeConditional: when both surfaces succeed the two outputs
	// must SemanticDiff equal; when at least one errors both must
	// error. Used by Axis C, where lenient coercion may legitimately
	// turn a perturbed mock into a valid parse on both surfaces.
	OutcomeConditional ExpectedOutcome = "conditional"
)

// Invalid-schema mutation kinds applied to a valid lowered
// DynamicOutputSchema. Each kind names one targeted edit to one root
// property; the rest of the schema stays untouched so the rapid
// shrinker can isolate the offending mutation when a regression
// surfaces.
//
// MutationUnknownRef from the scoping catalogue is intentionally
// absent: empirical validation at BAML 0.222.0 shows that swapping a
// root property's ref for an undeclared name does NOT trigger
// rejection. Both dynclient and REST consistently return `{}` (the
// runtime treats the unresolved ref as a vacuous class), which makes
// the input invalid by intent but not by behaviour — incompatible
// with Axis B's both-reject oracle. Routing unknown-ref into a
// different oracle (no-crash + body-parity) is a follow-up worth
// considering.
const (
	MutationBothTypeAndRef = "both_type_and_ref"
	MutationMisspelledType = "misspelled_type"
	MutationDropItems      = "drop_items"
	MutationDropInner      = "drop_inner"
	MutationDropKeysValues = "drop_keys_values"
	MutationDropOneOf      = "drop_oneof"
	MutationOverDepth      = "over_depth"
)

// JSON mutation kinds applied to a mock LLM response (Axis C). Each
// kind names one targeted edit to the JSON object the mockllm replays
// as the LLM's textual response.
const (
	JSONMutationDropKey       = "drop_key"
	JSONMutationRetypeScalar  = "retype_scalar"
	JSONMutationAddUnknownKey = "add_unknown_key"
	JSONMutationSwapEnum      = "swap_enum"
	JSONMutationWrapInArray   = "wrap_in_array"
)

// InvalidOracleCase is the materialized fuzz case for the invalid-input
// oracles. Parallel to OracleCase (case.go) but drops the Expected
// parsed-answer field (there is no "right" answer for invalid input)
// and carries the mutation descriptor + expected outcome the oracle
// asserts.
//
// Schema + Value describe the valid base from which the case was
// derived. For Axis B the mutation is applied during lowering by
// LowerInvalidToDynamicSchema; for Axis C the schema lowers cleanly and
// the perturbation lives in MockLLMContent.
type InvalidOracleCase struct {
	Name string `json:"name"`
	Seed int64  `json:"seed"`
	// CaseIndex is the position of the case within its rapid batch.
	// Used to derive distinct artifact filenames per case.
	CaseIndex int               `json:"case_index"`
	Mode      InvalidOracleMode `json:"mode"`
	Mutation  string            `json:"mutation"`
	// PreserveSchemaOrder mirrors the dynamic /call/_dynamic
	// preserve_schema_order request field. Axis C's success quadrant
	// runs the schema-aware key-order oracle when true; both axes pass
	// the value through verbatim to dynclient.Request so the surface
	// pipeline exercises whatever order-emission code path the bit
	// selects.
	PreserveSchemaOrder bool            `json:"preserve_schema_order"`
	ExpectedOutcome     ExpectedOutcome `json:"expected_outcome"`
	Schema              FuzzSchema      `json:"schema"`
	Value               FuzzValue       `json:"value"`
	MockLLMContent      json.RawMessage `json:"mock_llm_content"`
	Metadata            CaseMetadata    `json:"metadata"`
}

// InvalidFailureEnvelope captures the full context around an oracle
// disagreement for an InvalidOracleCase. ActualOutcome records what
// each surface actually did (succeed / error) so a developer reading
// the artifact can see the divergence without re-running the case.
type InvalidFailureEnvelope struct {
	GeneratorVersion    string                         `json:"generator_version"`
	GeneratedAt         string                         `json:"generated_at"`
	RapidSeed           int64                          `json:"rapid_seed"`
	CaseIndex           int                            `json:"case_index"`
	CaseName            string                         `json:"case_name"`
	OracleMode          InvalidOracleMode              `json:"oracle_mode"`
	Mutation            string                         `json:"mutation"`
	PreserveSchemaOrder bool                           `json:"preserve_schema_order"`
	ExpectedOutcome     ExpectedOutcome                `json:"expected_outcome"`
	Schema              FuzzSchema                     `json:"schema"`
	DynamicSchema       *bamlutils.DynamicOutputSchema `json:"dynamic_schema,omitempty"`
	LoweringError       string                         `json:"lowering_error,omitempty"`
	MockLLMScenarioID   string                         `json:"mockllm_scenario_id,omitempty"`
	MockLLMContent      json.RawMessage                `json:"mock_llm_content,omitempty"`
	DynclientOutput     json.RawMessage                `json:"dynclient_output,omitempty"`
	DynclientError      string                         `json:"dynclient_error,omitempty"`
	DynclientPanic      string                         `json:"dynclient_panic,omitempty"`
	DynclientPanicStack string                         `json:"dynclient_panic_stack,omitempty"`
	RESTStatus          int                            `json:"rest_status,omitempty"`
	RESTBody            json.RawMessage                `json:"rest_body,omitempty"`
	RESTError           string                         `json:"rest_error,omitempty"`
	RESTPanic           string                         `json:"rest_panic,omitempty"`
	RESTPanicStack      string                         `json:"rest_panic_stack,omitempty"`
	SemanticDiff        []SemanticDiffEntry            `json:"semantic_diff,omitempty"`
	OrderWarning        []string                       `json:"order_warning,omitempty"`
	ActualOutcome       string                         `json:"actual_outcome,omitempty"`
	ReplayPath          string                         `json:"replay_path"`
	Reproduction        string                         `json:"reproduction"`
	Metadata            CaseMetadata                   `json:"metadata"`
}

// WriteInvalidReplayArtifact writes the invalid-case failure envelope
// to `dir` as a JSON file and returns the resulting path. Mirrors the
// dynamic / static writers in envelope.go — same basename sanitisation,
// same 2-space-indent layout, ReplayPath stamped in place so the
// failure message can point at the artifact.
func WriteInvalidReplayArtifact(dir string, envelope *InvalidFailureEnvelope) (string, error) {
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
