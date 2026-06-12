package bamlfuzz

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"github.com/invakid404/baml-rest/adapters/common/codegen/internal/testharness"
	"github.com/invakid404/baml-rest/bamlutils"
)

// GeneratorVersion is the on-the-wire version stamp embedded in replay
// artifacts. Bumped when the generator semantics change in a way that
// invalidates older replays. 0.2.0 introduces KindUnion as a
// first-class IR node plus CaseMetadata.UnionChoices; older v1
// replays decode cleanly (the new fields default to zero values).
const GeneratorVersion = "0.2.0"

// DynamicFailureEnvelope captures the full context surrounding a
// disagreement between the three dynamic-oracle legs (expected /
// dynclient / REST). Release gates: semantic equality always, and
// schema-aware key order at every class instance when
// PreserveSchemaOrder is true (see SchemaOrderDiff). Strict order
// mismatches and schemas the order checker cannot resolve (e.g.
// missing union-choice metadata) are both release-blocking; the
// diagnostic lands on OrderWarning before the envelope is dumped.
// Per-arm union choices travel in Metadata.UnionChoices so a
// developer reading the envelope can see exactly which variant the
// value walker selected at each union node.
type DynamicFailureEnvelope struct {
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
	ReplayPath          string                         `json:"replay_path"`
	Reproduction        string                         `json:"reproduction"`
	Metadata            CaseMetadata                   `json:"metadata"`
}

// RawFailureEnvelope captures the full context around a disagreement on
// the /call-with-raw oracle. It mirrors DynamicFailureEnvelope's parsed
// `data` capture (the with-raw legs still diff their flattened output
// against the walker's Expected) and adds the raw-channel evidence: the
// extracted output text each leg returned. The raw channel is asserted
// with byte equality against MockLLMContent (it is pre-parse wire text,
// not JSON), so DynclientRaw / RESTRaw travel verbatim for forensics.
type RawFailureEnvelope struct {
	GeneratorVersion    string     `json:"generator_version"`
	GeneratedAt         string     `json:"generated_at"`
	RapidSeed           int64      `json:"rapid_seed"`
	CaseIndex           int        `json:"case_index"`
	CaseName            string     `json:"case_name"`
	OracleMode          OracleMode `json:"oracle_mode"`
	PreserveSchemaOrder bool       `json:"preserve_schema_order"`
	Schema              FuzzSchema `json:"schema"`
	// Value is the generated value tree the walker rendered MockLLMContent
	// and Expected from. Captured alongside Schema + RapidSeed so the
	// artifact can standalone-reconstruct the failing case even for fuzz
	// cases, where RapidSeed is 0 and the seed alone is insufficient.
	Value             FuzzValue                      `json:"value"`
	DynamicSchema     *bamlutils.DynamicOutputSchema `json:"dynamic_schema,omitempty"`
	DynamicSkipReason string                         `json:"dynamic_skip_reason,omitempty"`
	MockLLMScenarioID string                         `json:"mockllm_scenario_id"`
	MockLLMContent    json.RawMessage                `json:"mockllm_content"`
	Expected          json.RawMessage                `json:"expected"`

	// Parsed `data` payloads from each with-raw leg.
	DynclientOutput     json.RawMessage `json:"dynclient_output,omitempty"`
	DynclientError      string          `json:"dynclient_error,omitempty"`
	DynclientPanic      string          `json:"dynclient_panic,omitempty"`
	DynclientPanicStack string          `json:"dynclient_panic_stack,omitempty"`
	RESTStatus          int             `json:"rest_status,omitempty"`
	RESTBody            json.RawMessage `json:"rest_body,omitempty"`
	RESTError           string          `json:"rest_error,omitempty"`
	RESTPanic           string          `json:"rest_panic,omitempty"`
	RESTPanicStack      string          `json:"rest_panic_stack,omitempty"`

	// Raw-channel capture: the extracted output text each leg returned.
	// Asserted equal to MockLLMContent (and to each other) via string
	// equality, not SemanticDiff.
	DynclientRaw string `json:"dynclient_raw,omitempty"`
	RESTRaw      string `json:"rest_raw,omitempty"`

	// PlainCallOutput is the parsed `data` from the plain /call dynclient
	// leg, captured for the with-raw ⊇ plain-call anchor (A4).
	PlainCallOutput json.RawMessage `json:"plain_call_output,omitempty"`
	PlainCallError  string          `json:"plain_call_error,omitempty"`

	SemanticDiff []SemanticDiffEntry `json:"semantic_diff,omitempty"`
	RawMismatch  []string            `json:"raw_mismatch,omitempty"`
	ReplayPath   string              `json:"replay_path"`
	Reproduction string              `json:"reproduction"`
	Metadata     CaseMetadata        `json:"metadata"`
}

// WriteRawReplayArtifact writes a RawFailureEnvelope to `dir` as a JSON
// file. Same on-disk format as WriteReplayArtifact (2-space indent,
// deterministic basename via sanitizeArtifactBasename); envelope.ReplayPath
// is stamped with the resulting path so the t.Errorf message can point at
// the artifact.
func WriteRawReplayArtifact(dir string, envelope *RawFailureEnvelope) (string, error) {
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

// ReasoningFailureEnvelope captures the full context around a
// disagreement on the reasoning-channel oracle. It mirrors
// RawFailureEnvelope's parsed-`data` capture (the with-raw legs still
// diff their flattened output against the walker's Expected) and adds the
// reasoning-channel evidence: each leg is driven twice — once with
// __baml_options__.include_reasoning on and once off — so the envelope
// records both flag states. The reasoning "expected" is ThinkingInput
// (the fed thinking string), not a walker output: BAML passes reasoning
// through verbatim, so the oracle proves cross-path preservation and
// content/reasoning separation, not reasoning parsing.
type ReasoningFailureEnvelope struct {
	GeneratorVersion    string     `json:"generator_version"`
	GeneratedAt         string     `json:"generated_at"`
	RapidSeed           int64      `json:"rapid_seed"`
	CaseIndex           int        `json:"case_index"`
	CaseName            string     `json:"case_name"`
	OracleMode          OracleMode `json:"oracle_mode"`
	PreserveSchemaOrder bool       `json:"preserve_schema_order"`
	Schema              FuzzSchema `json:"schema"`
	// Value is the generated value tree the walker rendered MockLLMContent
	// and Expected from, captured so the artifact can standalone-replay a
	// failing case (fuzz cases have RapidSeed 0).
	Value             FuzzValue                      `json:"value"`
	DynamicSchema     *bamlutils.DynamicOutputSchema `json:"dynamic_schema,omitempty"`
	DynamicSkipReason string                         `json:"dynamic_skip_reason,omitempty"`
	MockLLMScenarioID string                         `json:"mockllm_scenario_id"`
	MockLLMContent    json.RawMessage                `json:"mockllm_content"`
	Expected          json.RawMessage                `json:"expected"`

	// ThinkingInput is the fuzzed thinking string fed to the Anthropic mock
	// (Scenario.Thinking). It is the reasoning channel's "expected" value —
	// every leg's reasoning must equal it under include_reasoning=true.
	ThinkingInput string `json:"thinking_input"`

	// Unary dynclient leg, both flag states. The "On" fields come from the
	// include_reasoning=true run; the "Off" fields from the flag-absent run.
	DynclientReasoningOn  string          `json:"dynclient_reasoning_on,omitempty"`
	DynclientReasoningOff string          `json:"dynclient_reasoning_off,omitempty"`
	DynclientDataOn       json.RawMessage `json:"dynclient_data_on,omitempty"`
	DynclientDataOff      json.RawMessage `json:"dynclient_data_off,omitempty"`
	DynclientRawOn        string          `json:"dynclient_raw_on,omitempty"`
	DynclientRawOff       string          `json:"dynclient_raw_off,omitempty"`
	DynclientErrorOn      string          `json:"dynclient_error_on,omitempty"`
	DynclientErrorOff     string          `json:"dynclient_error_off,omitempty"`
	// Unary panic capture, split by flag state so the include_reasoning=true
	// and =false runs of the same leg cannot clobber each other's evidence.
	DynclientPanic         string `json:"dynclient_panic,omitempty"`
	DynclientPanicStack    string `json:"dynclient_panic_stack,omitempty"`
	DynclientPanicOff      string `json:"dynclient_panic_off,omitempty"`
	DynclientPanicStackOff string `json:"dynclient_panic_stack_off,omitempty"`

	// Unary REST /call-with-raw/_dynamic leg, both flag states.
	RESTReasoningOn   string          `json:"rest_reasoning_on,omitempty"`
	RESTReasoningOff  string          `json:"rest_reasoning_off,omitempty"`
	RESTDataOn        json.RawMessage `json:"rest_data_on,omitempty"`
	RESTDataOff       json.RawMessage `json:"rest_data_off,omitempty"`
	RESTRawOn         string          `json:"rest_raw_on,omitempty"`
	RESTRawOff        string          `json:"rest_raw_off,omitempty"`
	RESTStatusOn      int             `json:"rest_status_on,omitempty"`
	RESTStatusOff     int             `json:"rest_status_off,omitempty"`
	RESTErrorOn       string          `json:"rest_error_on,omitempty"`
	RESTErrorOff      string          `json:"rest_error_off,omitempty"`
	RESTPanic         string          `json:"rest_panic,omitempty"`
	RESTPanicStack    string          `json:"rest_panic_stack,omitempty"`
	RESTPanicOff      string          `json:"rest_panic_off,omitempty"`
	RESTPanicStackOff string          `json:"rest_panic_stack_off,omitempty"`

	// Streaming legs: the cumulative reasoning at the last frame and the
	// final-frame data, per transport and flag state. Both transports run
	// in both flag states (R4 must hold on every path); the "Off" fields
	// hold the include_reasoning=false run, whose reasoning must be empty
	// and whose final data must still be present and correct.
	StreamDynclientReasoning    string          `json:"stream_dynclient_reasoning,omitempty"`
	StreamDynclientReasoningOff string          `json:"stream_dynclient_reasoning_off,omitempty"`
	StreamDynclientFinal        json.RawMessage `json:"stream_dynclient_final,omitempty"`
	StreamDynclientFinalOff     json.RawMessage `json:"stream_dynclient_final_off,omitempty"`
	StreamRESTReasoning         string          `json:"stream_rest_reasoning,omitempty"`
	StreamRESTReasoningOff      string          `json:"stream_rest_reasoning_off,omitempty"`
	StreamRESTFinal             json.RawMessage `json:"stream_rest_final,omitempty"`
	StreamRESTFinalOff          json.RawMessage `json:"stream_rest_final_off,omitempty"`

	// Streaming transport/drain errors, per transport and flag state. Like
	// the panic slots, these are distinct per leg×flag so a later stream
	// leg's error never overwrites an earlier one's in the shared envelope.
	StreamDynclientError    string `json:"stream_dynclient_error,omitempty"`
	StreamDynclientErrorOff string `json:"stream_dynclient_error_off,omitempty"`
	StreamRESTError         string `json:"stream_rest_error,omitempty"`
	StreamRESTErrorOff      string `json:"stream_rest_error_off,omitempty"`

	// Streaming panic capture, per transport and flag state. Distinct from
	// the unary DynclientPanic/RESTPanic slots (and from each other) because
	// a single case drives the unary legs plus four stream legs into one
	// envelope; sharing slots would let a later leg's panic overwrite an
	// earlier leg's evidence in the replay artifact. The unsuffixed fields
	// hold the include_reasoning=true run, the "Off" fields the false run.
	StreamDynclientPanic         string `json:"stream_dynclient_panic,omitempty"`
	StreamDynclientPanicStack    string `json:"stream_dynclient_panic_stack,omitempty"`
	StreamDynclientPanicOff      string `json:"stream_dynclient_panic_off,omitempty"`
	StreamDynclientPanicStackOff string `json:"stream_dynclient_panic_stack_off,omitempty"`
	StreamRESTPanic              string `json:"stream_rest_panic,omitempty"`
	StreamRESTPanicStack         string `json:"stream_rest_panic_stack,omitempty"`
	StreamRESTPanicOff           string `json:"stream_rest_panic_off,omitempty"`
	StreamRESTPanicStackOff      string `json:"stream_rest_panic_stack_off,omitempty"`

	SemanticDiff      []SemanticDiffEntry `json:"semantic_diff,omitempty"`
	ReasoningMismatch []string            `json:"reasoning_mismatch,omitempty"`
	ReplayPath        string              `json:"replay_path"`
	Reproduction      string              `json:"reproduction"`
	Metadata          CaseMetadata        `json:"metadata"`
}

// WriteReasoningReplayArtifact writes a ReasoningFailureEnvelope to `dir`
// as a JSON file. Same on-disk format as WriteRawReplayArtifact (2-space
// indent, deterministic basename); envelope.ReplayPath is stamped with
// the resulting path so the t.Errorf message can point at the artifact.
func WriteReasoningReplayArtifact(dir string, envelope *ReasoningFailureEnvelope) (string, error) {
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

// SemanticDiffEntry names one path-level disagreement between two of the
// oracle legs. `Side` identifies which legs disagree
// ("expected_vs_dynclient", "expected_vs_rest", "dynclient_vs_rest").
type SemanticDiffEntry struct {
	Side string `json:"side"`
	Path string `json:"path"`
	Got  any    `json:"got"`
	Want any    `json:"want"`
}

// StaticFailureEnvelope captures the full context around a failure in
// the static prompt oracle. It mirrors DynamicFailureEnvelope on the
// common header fields and adds the static-specific lowering + build
// metadata listed in scope D8. Release gates: semantic equality
// always, and schema-aware key order at every class instance when
// PreserveSchemaOrder is true. Strict order mismatches and schemas
// the order checker cannot resolve are both release-blocking; the
// diagnostic lands on OrderWarning before the envelope is dumped.
//
// BuildError populates when the integration build fails for the case;
// after single-case isolation it identifies the offending case alone.
// RESTStatus / RESTBody / RESTError populate when the build succeeds
// and /call/<FunctionName> runs.
type StaticFailureEnvelope struct {
	GeneratorVersion    string              `json:"generator_version"`
	GeneratedAt         string              `json:"generated_at"`
	RapidSeed           int64               `json:"rapid_seed"`
	CaseIndex           int                 `json:"case_index"`
	CaseName            string              `json:"case_name"`
	OracleMode          OracleMode          `json:"oracle_mode"`
	PreserveSchemaOrder bool                `json:"preserve_schema_order"`
	Schema              FuzzSchema          `json:"schema"`
	BamlSource          string              `json:"baml_source,omitempty"`
	FunctionName        string              `json:"function_name,omitempty"`
	ClassNames          []string            `json:"class_names,omitempty"`
	EnumNames           []string            `json:"enum_names,omitempty"`
	HasSelfRef          bool                `json:"has_self_ref"`
	BuildSourcePath     string              `json:"build_source_path,omitempty"`
	MockLLMScenarioID   string              `json:"mockllm_scenario_id,omitempty"`
	MockLLMContent      json.RawMessage     `json:"mockllm_content,omitempty"`
	Expected            json.RawMessage     `json:"expected,omitempty"`
	BuildError          string              `json:"build_error,omitempty"`
	RESTStatus          int                 `json:"rest_status,omitempty"`
	RESTBody            json.RawMessage     `json:"rest_body,omitempty"`
	RESTError           string              `json:"rest_error,omitempty"`
	RESTPanic           string              `json:"rest_panic,omitempty"`
	RESTPanicStack      string              `json:"rest_panic_stack,omitempty"`
	SemanticDiff        []SemanticDiffEntry `json:"semantic_diff,omitempty"`
	OrderWarning        []string            `json:"order_warning,omitempty"`
	ReplayPath          string              `json:"replay_path"`
	Reproduction        string              `json:"reproduction"`
	Metadata            CaseMetadata        `json:"metadata"`
}

// WriteReplayArtifact writes the failure envelope to `dir` as a JSON
// file and returns the resulting `filepath.Join(dir, basename+".json")`
// (which is absolute exactly when `dir` is). The basename is derived
// from envelope.CaseName via sanitizeArtifactBasename: a safe CaseName
// is used as-is, anything that fails testharness.CheckReplayName
// (empty, separator-bearing, traversal segment, absolute, drive
// prefix) falls back to `case_<CaseIndex>`. The envelope's ReplayPath
// field is stamped with the resulting path so the t.Errorf message
// points the developer directly at the artifact.
//
// Content is rendered with 2-space indent for human readability.
// `dir` is created if it does not already exist.
func WriteReplayArtifact(dir string, envelope *DynamicFailureEnvelope) (string, error) {
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

// WriteStaticReplayArtifact writes a StaticFailureEnvelope to `dir`
// as a JSON file. Same on-disk format as WriteReplayArtifact (2-space
// indent, deterministic basename via sanitizeArtifactBasename);
// envelope.ReplayPath is stamped with the resulting path so the
// t.Errorf message can point at the artifact.
func WriteStaticReplayArtifact(dir string, envelope *StaticFailureEnvelope) (string, error) {
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

// sanitizeArtifactBasename returns a safe basename for the replay
// artifact filename. CaseName comes from on-disk corpus / replay JSON
// and a hand-edited file with separators, traversal segments, or an
// absolute / drive-prefixed path could otherwise escape the artifact
// directory. testharness.CheckReplayName is the shared contract; on
// any violation (including empty) the basename falls back to
// case_<index> so the failure envelope still lands on disk.
func sanitizeArtifactBasename(caseName string, caseIndex int) string {
	if err := testharness.CheckReplayName(caseName); err != nil {
		return fmt.Sprintf("case_%d", caseIndex)
	}
	return caseName
}

// SemanticEqual reports whether two JSON byte slices are equal modulo
// map-key ordering. Slices, scalars, and nulls compare by value; objects
// are compared as key sets. This is the v1 hard-failure oracle for the
// dynamic three-way test.
//
// Either input being empty (len == 0) is a decode error — two missing
// oracle payloads must not silently equate. Callers wrap the error as
// `decode a: %w` / `decode b: %w` so the failure log identifies which
// side was empty.
func SemanticEqual(a, b json.RawMessage) (bool, error) {
	av, err := decodeAny(a)
	if err != nil {
		return false, fmt.Errorf("decode a: %w", err)
	}
	bv, err := decodeAny(b)
	if err != nil {
		return false, fmt.Errorf("decode b: %w", err)
	}
	return deepEqualSemantic(av, bv, nullExpectedVsActual), nil
}

// SemanticDiff returns the list of path-level disagreements between two
// JSON blobs. An empty result means the two are SemanticEqual.
//
// `side` is opaque to the comparator — it is copied verbatim into each
// emitted SemanticDiffEntry so callers can label which oracle leg pair
// they're comparing.
//
// SemanticDiff is for expected-vs-actual comparisons: `a` is the
// expected/reference side and the boundaryml/baml#3690 null-key
// tolerance is applied only to extra null keys on the actual side `b`
// (see the null-tolerance block lower in this file). For actual-vs-actual
// parity comparisons where either leg may independently carry the leak,
// use SemanticDiffParity.
//
// Either input being empty (len == 0) is a decode error — same contract
// as SemanticEqual. Callers wrap with `decode a: %w` / `decode b: %w`
// so the failure log identifies which side was empty.
func SemanticDiff(side string, a, b json.RawMessage) ([]SemanticDiffEntry, error) {
	return semanticDiff(side, a, b, nullExpectedVsActual)
}

// SemanticDiffParity is the symmetric variant of SemanticDiff for
// actual-vs-actual parity comparisons (e.g. dynclient_vs_rest), where
// both legs are BAML-generated and either side may independently carry a
// boundaryml/baml#3690 leaked null key. An extra null-valued key on
// either side is tolerated; every other disagreement is still reported.
func SemanticDiffParity(side string, a, b json.RawMessage) ([]SemanticDiffEntry, error) {
	return semanticDiff(side, a, b, nullParity)
}

// SemanticDiffWithSchema is the schema-aware variant of SemanticDiff
// used by the static prompt oracle. On top of SemanticDiff's
// expected-vs-actual semantics (including the boundaryml/baml#3690
// null-key tolerance) it forgives a string-literal escape-level
// mismatch: BAML's static codegen is inconsistent about literal string
// values containing special characters — for some positions (e.g. a
// top-level class-field literal) it echoes the source-escaped token
// (`\"quoted\"`), while for others (e.g. a deeply nested union arm) it
// returns the decoded value (`"quoted"`). The oracle's expected output
// always carries the decoded value (see normalize.go's writeLiteral).
// When the schema types a field as a string literal, the two forms are
// treated as equal so the position-dependent escaping does not flag a
// false disagreement.
//
// `choices` mirrors CaseMetadata.UnionChoices and lets the literal-path
// collector descend through union arms; a missing entry simply stops the
// descent (the unresolved union arm contributes no literal paths), which
// is safe because an unresolved-arm literal would not be tolerated.
//
// The tolerance is narrow: it applies ONLY at JSON paths the schema
// types as a string literal. A plain `string` field that happens to
// contain quotes is unaffected and still diffs normally.
func SemanticDiffWithSchema(side string, schema FuzzSchema, choices map[string]UnionChoice, a, b json.RawMessage) ([]SemanticDiffEntry, error) {
	av, err := decodeAny(a)
	if err != nil {
		return nil, fmt.Errorf("decode a: %w", err)
	}
	bv, err := decodeAny(b)
	if err != nil {
		return nil, fmt.Errorf("decode b: %w", err)
	}
	lit := make(map[string]bool)
	root := schema.EffectiveRoot()
	// Walk both sides: class-field literal positions are schema-fixed, but
	// list indices / map keys come from the value, so collecting from both
	// payloads ensures a literal path present on either side is recognized.
	collectLiteralStringPaths(lit, schema, choices, "$", "", root, av)
	collectLiteralStringPaths(lit, schema, choices, "$", "", root, bv)
	var out []SemanticDiffEntry
	diffAny(&out, side, "$", av, bv, nullExpectedVsActual, lit)
	return out, nil
}

func semanticDiff(side string, a, b json.RawMessage, mode nullMode) ([]SemanticDiffEntry, error) {
	av, err := decodeAny(a)
	if err != nil {
		return nil, fmt.Errorf("decode a: %w", err)
	}
	bv, err := decodeAny(b)
	if err != nil {
		return nil, fmt.Errorf("decode b: %w", err)
	}
	var out []SemanticDiffEntry
	diffAny(&out, side, "$", av, bv, mode, nil)
	return out, nil
}

// collectLiteralStringPaths records, into `out`, every JSON path (in
// diffAny's path convention) whose schema type resolves to a
// KindLiteral / LiteralString. It walks the schema type guided by the
// decoded value `v` so list indices and map keys come from the actual
// data. Two path strings are tracked in parallel: `jsonPath` (rooted at
// "$", `.key` for every object member — diffAny treats class and map
// nodes identically) and `choicePath` (rooted at "", `[%q]` for map
// keys plus a `:v` step into each union arm — the CaseMetadata.UnionChoices
// key convention shared with normalize.go and order.go). Only the
// jsonPath form is emitted; the choicePath form exists solely to look up
// union choices.
func collectLiteralStringPaths(out map[string]bool, schema FuzzSchema, choices map[string]UnionChoice, jsonPath, choicePath string, t FuzzType, v any) {
	switch t.Kind {
	case KindLiteral:
		if t.Literal != nil && t.Literal.Kind == LiteralString {
			out[jsonPath] = true
		}
	case KindOptional:
		if t.Inner == nil || v == nil {
			return
		}
		collectLiteralStringPaths(out, schema, choices, jsonPath, choicePath, *t.Inner, v)
	case KindClassRef:
		cls, ok := schema.FindClass(t.Ref)
		if !ok {
			return
		}
		m, ok := v.(map[string]any)
		if !ok {
			return
		}
		for _, prop := range cls.Properties {
			cv, ok := m[prop.Name]
			if !ok {
				continue
			}
			collectLiteralStringPaths(out, schema, choices, jsonPath+"."+prop.Name, choicePath+"."+prop.Name, prop.Type, cv)
		}
	case KindList:
		if t.Inner == nil {
			return
		}
		arr, ok := v.([]any)
		if !ok {
			return
		}
		for i, e := range arr {
			collectLiteralStringPaths(out, schema, choices, fmt.Sprintf("%s[%d]", jsonPath, i), fmt.Sprintf("%s[%d]", choicePath, i), *t.Inner, e)
		}
	case KindMap:
		if t.Inner == nil {
			return
		}
		m, ok := v.(map[string]any)
		if !ok {
			return
		}
		for k, mv := range m {
			collectLiteralStringPaths(out, schema, choices, jsonPath+"."+k, fmt.Sprintf("%s[%q]", choicePath, k), *t.Inner, mv)
		}
	case KindUnion:
		choice, ok := choices[choicePath]
		if !ok {
			return
		}
		if choice.Index < 0 || choice.Index >= len(t.Variants) {
			return
		}
		collectLiteralStringPaths(out, schema, choices, jsonPath, choicePath+":v", t.Variants[choice.Index], v)
	}
}

// literalEscapeEquivalent reports whether two string values are equal
// modulo source-escaping of a string literal: one side is the
// strconv.Quote body (the quoted form without its surrounding double
// quotes) of the other. This forgives BAML's position-dependent literal
// echo, where one oracle leg carries the decoded value (`"quoted"`) and
// the other the source-escaped token (`\"quoted\"`). It is symmetric, so
// it covers both expected=decoded/actual=escaped and the reverse. The
// match is exact in both directions, so it only suppresses a genuine
// escape relationship — never two unrelated strings.
func literalEscapeEquivalent(a, b string) bool {
	return escapeBody(a) == b || escapeBody(b) == a
}

// escapeBody returns strconv.Quote(s) with its surrounding double quotes
// stripped — i.e. the Go source-escaped rendering of s without the outer
// delimiters. strconv.Quote never returns a string shorter than two
// characters (the two delimiters), so the slice bounds are always valid.
func escapeBody(s string) string {
	quoted := strconv.Quote(s)
	return quoted[1 : len(quoted)-1]
}

// DetectOrderWarning compares the top-level key order of two JSON
// objects and returns a list of human-readable mismatches. The
// comparison is shallow (top-level only); new strict-mode callers
// should use SchemaOrderDiff, which is schema-aware and recurses
// through nested classes. DetectOrderWarning is retained for legacy
// callers that don't have access to a FuzzSchema.
//
// Both inputs must be JSON objects; non-object inputs return nil with no
// warning (semantic equality still catches structural disagreements).
func DetectOrderWarning(label string, a, b json.RawMessage) []string {
	keysA, errA := topLevelKeys(a)
	keysB, errB := topLevelKeys(b)
	if errA != nil || errB != nil {
		return nil
	}
	if len(keysA) != len(keysB) {
		return []string{fmt.Sprintf("%s: top-level key count differs (a=%d b=%d)", label, len(keysA), len(keysB))}
	}
	var out []string
	for i := range keysA {
		if keysA[i] != keysB[i] {
			out = append(out, fmt.Sprintf("%s[%d]: %q vs %q", label, i, keysA[i], keysB[i]))
		}
	}
	return out
}

func decodeAny(b json.RawMessage) (any, error) {
	if len(b) == 0 {
		// Empty raw message is treated as a hard decode error so
		// SemanticEqual / SemanticDiff can't silently equate two
		// missing payloads. An oracle leg that produced no output is a
		// real failure (the leg errored, or the response body was lost
		// somewhere); the comparator should surface that as a decode
		// error and let the test print which side was empty.
		return nil, fmt.Errorf("empty JSON payload")
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, err
	}
	return v, nil
}

// Workaround for boundaryml/baml#3690: BAML's Go codegen serializes
// null-valued optional fields from a class union arm even when the
// active arm is a map or a different class, so a payload like
// {"k0": -26} comes back as {"Fuzz_field_0": null, "k0": -26}. The
// comparators below tolerate this leaked null key. The bug only ever
// ADDS keys to a BAML-generated output, so the tolerance direction
// depends on what is being compared (selected via nullMode):
//
//   - nullExpectedVsActual: `a` is the expected/reference side and `b`
//     is the actual output. Only an extra null key present in `b` but
//     absent from `a` is suppressed; a null key the expected side
//     carries that the actual side dropped is a genuine missing field
//     and still fails.
//   - nullParity: both sides are BAML-generated (actual-vs-actual), so
//     either may independently carry the leak. An extra null key on
//     either side is suppressed.
//
// This is a comparison-only relaxation — it never touches the serialized
// output. Remove when the upstream fix lands and the leaked null keys
// stop appearing on the wire.
type nullMode int

const (
	nullExpectedVsActual nullMode = iota
	nullParity
)

// withoutExtraNullKeys returns a view of `src` excluding keys that are
// null-valued there and absent from the reference `ref`. Null keys that
// `ref` also carries are kept so genuine null-vs-null agreement still
// participates. The caller chooses which side(s) to strip, encoding the
// asymmetric / parity tolerance described above.
func withoutExtraNullKeys(src, ref map[string]any) map[string]any {
	out := make(map[string]any, len(src))
	for k, v := range src {
		if v == nil {
			if _, ok := ref[k]; !ok {
				continue
			}
		}
		out[k] = v
	}
	return out
}

func deepEqualSemantic(a, b any, mode nullMode) bool {
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok {
			return false
		}
		// The actual side (b) is always stripped of leaked null keys. In
		// parity mode the expected side (a) is stripped too; otherwise a
		// is authoritative so a missing field still fails.
		bf := withoutExtraNullKeys(bv, av)
		af := av
		if mode == nullParity {
			af = withoutExtraNullKeys(av, bv)
		}
		if len(af) != len(bf) {
			return false
		}
		for k, v := range af {
			rv, present := bf[k]
			if !present {
				return false
			}
			if !deepEqualSemantic(v, rv, mode) {
				return false
			}
		}
		return true
	case []any:
		bv, ok := b.([]any)
		if !ok {
			return false
		}
		if len(av) != len(bv) {
			return false
		}
		for i := range av {
			if !deepEqualSemantic(av[i], bv[i], mode) {
				return false
			}
		}
		return true
	case float64:
		bv, ok := b.(float64)
		if !ok {
			return false
		}
		return av == bv
	case string:
		bv, ok := b.(string)
		if !ok {
			return false
		}
		return av == bv
	case bool:
		bv, ok := b.(bool)
		if !ok {
			return false
		}
		return av == bv
	case nil:
		return b == nil
	default:
		return false
	}
}

// diffAny appends path-level disagreements between `a` and `b` to `out`.
// `lit` (nil for non-schema-aware callers) is the set of JSON paths the
// schema types as a string literal; at those paths a source-escape-level
// string mismatch is tolerated (see literalEscapeEquivalent).
func diffAny(out *[]SemanticDiffEntry, side, path string, a, b any, mode nullMode, lit map[string]bool) {
	if deepEqualSemantic(a, b, mode) {
		return
	}
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok {
			*out = append(*out, SemanticDiffEntry{Side: side, Path: path, Got: a, Want: b})
			return
		}
		keys := make(map[string]struct{}, len(av)+len(bv))
		for k := range av {
			keys[k] = struct{}{}
		}
		for k := range bv {
			keys[k] = struct{}{}
		}
		sortedKeys := make([]string, 0, len(keys))
		for k := range keys {
			sortedKeys = append(sortedKeys, k)
		}
		sort.Strings(sortedKeys)
		for _, k := range sortedKeys {
			av1, ok1 := av[k]
			bv1, ok2 := bv[k]
			if !ok1 || !ok2 {
				// boundaryml/baml#3690 workaround: a leaked null-valued
				// key present on only one side is not a real
				// disagreement. An extra null on the actual side (b) is
				// always tolerated; in parity mode an extra null on the
				// a side is too. A non-null one-sided key, or (outside
				// parity) a null key the expected side carries that
				// actual dropped, is still reported.
				if !ok1 && bv1 == nil {
					continue
				}
				if mode == nullParity && !ok2 && av1 == nil {
					continue
				}
				*out = append(*out, SemanticDiffEntry{Side: side, Path: path + "." + k, Got: av1, Want: bv1})
				continue
			}
			diffAny(out, side, path+"."+k, av1, bv1, mode, lit)
		}
	case []any:
		bv, ok := b.([]any)
		if !ok || len(av) != len(bv) {
			*out = append(*out, SemanticDiffEntry{Side: side, Path: path, Got: a, Want: b})
			return
		}
		for i := range av {
			diffAny(out, side, fmt.Sprintf("%s[%d]", path, i), av[i], bv[i], mode, lit)
		}
	case string:
		// At a schema string-literal path, a source-escape-level mismatch
		// between the two legs is BAML's position-dependent literal echo,
		// not a real disagreement. Reached only when the strings already
		// differ (deepEqualSemantic short-circuited equal values above).
		if bv, ok := b.(string); ok && lit[path] && literalEscapeEquivalent(av, bv) {
			return
		}
		*out = append(*out, SemanticDiffEntry{Side: side, Path: path, Got: a, Want: b})
	default:
		*out = append(*out, SemanticDiffEntry{Side: side, Path: path, Got: a, Want: b})
	}
}

// topLevelKeys returns the keys of a JSON object in wire (source) order.
// Decoding through bamlutils.OrderedMap preserves insertion order, which
// is what the order-warning detector compares.
func topLevelKeys(b json.RawMessage) ([]string, error) {
	if len(b) == 0 {
		return nil, fmt.Errorf("empty")
	}
	var om bamlutils.OrderedMap[json.RawMessage]
	if err := om.UnmarshalJSON(b); err != nil {
		return nil, err
	}
	return om.Keys(), nil
}
