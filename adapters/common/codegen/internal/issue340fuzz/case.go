package issue340fuzz

import "encoding/json"

// OracleMode names the oracle the test harness runs against an
// OracleCase. The two modes correspond to the two emission paths in
// the v1 plan.
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
)

// CaseMetadata is per-case provenance that's useful to a developer
// staring at a fuzz failure. None of these fields are load-bearing
// for replay determinism (the seed + schema + value are); they exist
// so the failure envelope can describe the shape choices the
// walker made.
type CaseMetadata struct {
	// OptionalShapes maps a JSON-path-style key (e.g.
	// ".F340_field_2.F340_field_0") to "present" / "absent" /
	// "null". Recorded for every optional field encountered.
	OptionalShapes map[string]string `json:"optional_shapes,omitempty"`
	// RecursionDepths maps a class name to the maximum self-ref
	// depth the value walker entered for that class. Populated for
	// static cases with self-ref edges; zero for dynamic-safe cases.
	RecursionDepths map[string]int `json:"recursion_depths,omitempty"`
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
