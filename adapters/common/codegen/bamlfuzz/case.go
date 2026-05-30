package bamlfuzz

import "encoding/json"

// OracleMode names the oracle the test harness runs against an
// OracleCase. Each mode corresponds to one emission/transport path the
// same generated case is driven through.
type OracleMode string

const (
	// OracleDynamicThreeWay: lower the schema through the dynamic
	// emitter, drive both dynclient.DynamicCall and the REST
	// /call/_dynamic endpoint, then three-way diff against the
	// walker's expected JSON.
	OracleDynamicThreeWay OracleMode = "dynamic_three_way"
	// OracleStaticPrompt: lower the schema through the static
	// emitter into a temp baml_src/, boot the integration server,
	// drive REST /call/<GeneratedFunction>, and diff against the
	// walker's expected JSON.
	OracleStaticPrompt OracleMode = "static_prompt"
	// OracleDynamicStreaming: drive the same lowered dynamic case
	// through the streaming surfaces (dynclient DynamicStream iterator
	// + REST /stream/_dynamic over SSE and NDJSON), collect each leg's
	// final frame, and assert it equals the walker's expected JSON and
	// the unary parse. Partial frames are not asserted on in v1 — only
	// the terminal frame, which is chunk-timing invariant.
	OracleDynamicStreaming OracleMode = "dynamic_streaming"
)

// CaseMetadata is per-case provenance that's useful to a developer
// staring at a fuzz failure. None of these fields are load-bearing
// for replay determinism (the seed + schema + value are); they exist
// so the failure envelope can describe the shape choices the
// walker made.
type CaseMetadata struct {
	// OptionalShapes maps a JSON-path-style key (e.g.
	// ".Fuzz_field_2.Fuzz_field_0") to "present" / "absent" /
	// "null". Recorded for every optional field encountered.
	OptionalShapes map[string]string `json:"optional_shapes,omitempty"`
	// RecursionDepths maps a class name to the peak instance-entry
	// depth the value walker reached for that class. The walker
	// increments a counter on each class-instance entry and records
	// the maximum, so every class encountered during the walk gets an
	// entry — even single-visit classes in a dynamic-safe DAG, which
	// report depth 1. Self-ref and mutual-cycle schemas report higher
	// peaks up to MaxValueRecursion.
	RecursionDepths map[string]int `json:"recursion_depths,omitempty"`
	// UnionChoices maps a JSON-path-style key to the union-variant
	// choice the value walker recorded at that path. Populated for
	// every KindUnion node encountered. The order checker and the
	// failure envelope read this to explain which arm was exercised;
	// callers reading union choices fail closed when an entry is
	// missing.
	UnionChoices map[string]UnionChoice `json:"union_choices,omitempty"`
}

// UnionChoice records which arm of a KindUnion was selected at one
// JSON path. Index is the zero-based slot in FuzzType.Variants;
// Kind / Ref describe the selected arm's discriminator so a
// developer can interpret the envelope without re-deriving the
// schema. VariantCount lets a consumer detect a single-arm shrink
// state (where the union collapsed to a bare type during shrinking).
type UnionChoice struct {
	Index        int          `json:"index"`
	Kind         FuzzTypeKind `json:"kind"`
	Ref          string       `json:"ref,omitempty"`
	VariantCount int          `json:"variant_count"`
}

// OracleCase is the fully-materialized fuzz case downstream PRs ship
// into mockllm + the test harness. PR-A produces these; PR-B and
// PR-C consume them.
type OracleCase struct {
	Name                string          `json:"name"`
	Seed                int64           `json:"seed"`
	CaseIndex           int             `json:"case_index"`
	Mode                OracleMode      `json:"mode"`
	PreserveSchemaOrder bool            `json:"preserve_schema_order"`
	Schema              FuzzSchema      `json:"schema"`
	Value               FuzzValue       `json:"value"`
	MockLLMContent      json.RawMessage `json:"mock_llm_content"`
	Expected            json.RawMessage `json:"expected"`
	Metadata            CaseMetadata    `json:"metadata"`
}
