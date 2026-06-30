package bamlfuzz

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ParseDiffFailureEnvelope captures the full context around a
// native-vs-BAML differential parse disagreement (or a BAML-vs-checked-in
// expectation mismatch). It is the parse-harness analogue of the
// dynamic/static/streaming oracle envelopes: enough context to replay and
// triage one failing parse without re-deriving the case.
//
// One envelope describes one comparison. For a streaming case the
// per-prefix legs each get their own envelope; PrefixIndex / PrefixName
// identify which accumulated prefix this envelope is for (PrefixIndex is
// -1 for a final-parse leg). Raw is the exact text that was parsed.
//
// Expected, when present, is the checked-in `want` the BAML outcome was
// characterized against; ExpectedStatus records its success/error token.
// BAML and Native are the captured ParseOutcomes; the diff slices and the
// human-readable Failures mirror ParseDiffResult.
type ParseDiffFailureEnvelope struct {
	GeneratorVersion    string     `json:"generator_version"`
	GeneratedAt         string     `json:"generated_at"`
	CaseIndex           int        `json:"case_index"`
	CaseName            string     `json:"case_name"`
	OracleMode          OracleMode `json:"oracle_mode"`
	PreserveSchemaOrder bool       `json:"preserve_schema_order"`
	Schema              FuzzSchema `json:"schema"`

	// UnionChoices mirrors the case's union-arm metadata so a replay artifact
	// carries enough context to reproduce the schema-order check (which fails
	// closed on a union path without a matching UnionChoice).
	UnionChoices map[string]UnionChoice `json:"union_choices,omitempty"`

	// Stream is true when the leg was a parse-stream comparison over an
	// accumulated prefix; false for a final parse.
	Stream bool `json:"stream"`
	// PrefixIndex / PrefixName identify the streaming prefix this envelope
	// covers. PrefixIndex is -1 for a final-parse leg.
	PrefixIndex int    `json:"prefix_index"`
	PrefixName  string `json:"prefix_name,omitempty"`
	// Raw is the exact model text that was parsed (full response for a
	// final parse, accumulated prefix for a stream leg).
	Raw string `json:"raw"`

	// ExpectedStatus / Expected hold the checked-in `want` outcome the
	// BAML leg was characterized against, when the comparison was
	// BAML-vs-corpus. Empty for a pure native-vs-BAML diff.
	ExpectedStatus string          `json:"expected_status,omitempty"`
	Expected       json.RawMessage `json:"expected,omitempty"`

	SkippedNative bool         `json:"skipped_native,omitempty"`
	BAML          ParseOutcome `json:"baml"`
	// Native is a pointer so an absent/skipped native leg is omitted from
	// the JSON entirely. A non-pointer ParseOutcome would not be elided by
	// encoding/json (omitempty does not apply to a zero-value struct), and
	// ParseOutcome.Parser is non-omitempty, so a BAML-only failure envelope
	// would otherwise emit a misleading {"parser":""} native stub.
	Native *ParseOutcome `json:"native,omitempty"`

	SemanticDiff []SemanticDiffEntry    `json:"semantic_diff,omitempty"`
	OrderDiff    []SchemaOrderDiffEntry `json:"order_diff,omitempty"`
	Failures     []string               `json:"failures,omitempty"`

	ReplayPath   string `json:"replay_path"`
	Reproduction string `json:"reproduction,omitempty"`
}

// WriteParseDiffReplayArtifact writes a ParseDiffFailureEnvelope to `dir`
// as a JSON file. Same on-disk format as WriteReplayArtifact (2-space
// indent, deterministic basename via sanitizeArtifactBasename);
// envelope.ReplayPath is stamped with the resulting path so the t.Errorf
// message can point at the artifact. For a streaming leg the prefix index
// is folded into the basename so per-prefix failures of one case don't
// clobber each other.
func WriteParseDiffReplayArtifact(dir string, envelope *ParseDiffFailureEnvelope) (string, error) {
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
	if envelope.PrefixIndex >= 0 {
		name = fmt.Sprintf("%s_prefix_%d", name, envelope.PrefixIndex)
	}
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
