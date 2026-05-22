package bamlfuzz

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/invakid404/baml-rest/adapters/common/codegen/internal/testharness"
	"github.com/invakid404/baml-rest/bamlutils"
)

// GeneratorVersion is the on-the-wire version stamp embedded in replay
// artifacts. Bumped when the generator semantics change in a way that
// invalidates older replays.
const GeneratorVersion = "0.1.0"

// DynamicFailureEnvelope captures the full context surrounding a
// disagreement between the three dynamic-oracle legs (expected /
// dynclient / REST). v1 release-gates on semantic equality only; order
// mismatches are recorded under OrderWarning and don't fail the test.
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
	RESTStatus          int                            `json:"rest_status,omitempty"`
	RESTBody            json.RawMessage                `json:"rest_body,omitempty"`
	RESTError           string                         `json:"rest_error,omitempty"`
	SemanticDiff        []SemanticDiffEntry            `json:"semantic_diff,omitempty"`
	OrderWarning        []string                       `json:"order_warning,omitempty"`
	ReplayPath          string                         `json:"replay_path"`
	Reproduction        string                         `json:"reproduction"`
	Metadata            CaseMetadata                   `json:"metadata"`
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
	return deepEqualSemantic(av, bv), nil
}

// SemanticDiff returns the list of path-level disagreements between two
// JSON blobs. An empty result means the two are SemanticEqual.
//
// `side` is opaque to the comparator — it is copied verbatim into each
// emitted SemanticDiffEntry so callers can label which oracle leg pair
// they're comparing.
//
// Either input being empty (len == 0) is a decode error — same contract
// as SemanticEqual. Callers wrap with `decode a: %w` / `decode b: %w`
// so the failure log identifies which side was empty.
func SemanticDiff(side string, a, b json.RawMessage) ([]SemanticDiffEntry, error) {
	av, err := decodeAny(a)
	if err != nil {
		return nil, fmt.Errorf("decode a: %w", err)
	}
	bv, err := decodeAny(b)
	if err != nil {
		return nil, fmt.Errorf("decode b: %w", err)
	}
	var out []SemanticDiffEntry
	diffAny(&out, side, "$", av, bv)
	return out, nil
}

// DetectOrderWarning compares the top-level key order of two JSON
// objects and returns a list of human-readable mismatches. The
// comparison is shallow (top-level only) because the v1 strict-order
// follow-up will revisit nested objects.
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

func deepEqualSemantic(a, b any) bool {
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok {
			return false
		}
		if len(av) != len(bv) {
			return false
		}
		for k, v := range av {
			rv, present := bv[k]
			if !present {
				return false
			}
			if !deepEqualSemantic(v, rv) {
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
			if !deepEqualSemantic(av[i], bv[i]) {
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

func diffAny(out *[]SemanticDiffEntry, side, path string, a, b any) {
	if deepEqualSemantic(a, b) {
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
				*out = append(*out, SemanticDiffEntry{Side: side, Path: path + "." + k, Got: av1, Want: bv1})
				continue
			}
			diffAny(out, side, path+"."+k, av1, bv1)
		}
	case []any:
		bv, ok := b.([]any)
		if !ok || len(av) != len(bv) {
			*out = append(*out, SemanticDiffEntry{Side: side, Path: path, Got: a, Want: b})
			return
		}
		for i := range av {
			diffAny(out, side, fmt.Sprintf("%s[%d]", path, i), av[i], bv[i])
		}
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
