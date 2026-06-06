package bamlfuzz

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSemanticEqual(t *testing.T) {
	cases := []struct {
		name string
		a, b string
		want bool
	}{
		{"identical object", `{"a":1,"b":2}`, `{"b":2,"a":1}`, true},
		{"different value", `{"a":1}`, `{"a":2}`, false},
		{"missing key", `{"a":1,"b":2}`, `{"a":1}`, false},
		{"nested order", `{"x":{"a":1,"b":2}}`, `{"x":{"b":2,"a":1}}`, true},
		{"array order matters", `[1,2,3]`, `[3,2,1]`, false},
		// boundaryml/baml#3690 workaround is asymmetric: an extra null
		// key on the actual side (b) is tolerated, but a null key the
		// expected side (a) carries that actual dropped is a genuine
		// missing field and still fails.
		{"null missing from actual", `{"a":null}`, `{}`, false},
		{"extra null on actual", `{}`, `{"a":null}`, true},
		{"both null", `null`, `null`, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := SemanticEqual([]byte(c.a), []byte(c.b))
			if err != nil {
				t.Fatalf("SemanticEqual: %v", err)
			}
			if got != c.want {
				t.Errorf("SemanticEqual(%s, %s) = %v, want %v", c.a, c.b, got, c.want)
			}
		})
	}
}

func TestSemanticEqual_RejectsEmptyPayload(t *testing.T) {
	// Both empty: the comparator MUST NOT report equality on two
	// missing payloads — that hides real "leg produced no output"
	// regressions in the dynamic oracle.
	if _, err := SemanticEqual([]byte{}, []byte{}); err == nil {
		t.Error("SemanticEqual(empty, empty) returned nil error; want decode error")
	}
	if _, err := SemanticEqual([]byte{}, []byte(`{"a":1}`)); err == nil {
		t.Error("SemanticEqual(empty, nonempty) returned nil error; want decode error")
	}
	if _, err := SemanticEqual([]byte(`{"a":1}`), []byte{}); err == nil {
		t.Error("SemanticEqual(nonempty, empty) returned nil error; want decode error")
	}
}

func TestSemanticDiff_RejectsEmptyPayload(t *testing.T) {
	if _, err := SemanticDiff("side", []byte{}, []byte(`{"a":1}`)); err == nil {
		t.Error("SemanticDiff(empty, nonempty) returned nil error; want decode error")
	}
	if _, err := SemanticDiff("side", []byte(`{"a":1}`), []byte{}); err == nil {
		t.Error("SemanticDiff(nonempty, empty) returned nil error; want decode error")
	}
}

func TestSemanticDiff_FindsDifferences(t *testing.T) {
	diff, err := SemanticDiff("a_vs_b",
		[]byte(`{"x":1,"y":{"a":true,"b":"hi"}}`),
		[]byte(`{"x":2,"y":{"a":true,"b":"bye"},"z":null}`))
	if err != nil {
		t.Fatalf("SemanticDiff: %v", err)
	}
	if len(diff) == 0 {
		t.Fatal("expected differences, got none")
	}
	for _, d := range diff {
		if d.Side != "a_vs_b" {
			t.Errorf("entry side=%q want %q", d.Side, "a_vs_b")
		}
	}
}

// TestSemanticDiff_ToleratesExtraNullKeys pins the boundaryml/baml#3690
// workaround: when the only difference is a null-valued key present on
// the actual side (b) but absent from the expected side (a), SemanticDiff
// reports no disagreement. The tolerance is asymmetric — a is expected.
func TestSemanticDiff_ToleratesExtraNullKeys(t *testing.T) {
	cases := []struct {
		name string
		a, b string
	}{
		{"extra null on actual", `{"k0":-26}`, `{"Fuzz_field_0":null,"k0":-26}`},
		{"null on both", `{"f":null,"k0":-26}`, `{"f":null,"k0":-26}`},
		{"nested extra null on actual", `{"o":{"k":1}}`, `{"o":{"leak":null,"k":1}}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			diff, err := SemanticDiff("side", []byte(c.a), []byte(c.b))
			if err != nil {
				t.Fatalf("SemanticDiff: %v", err)
			}
			if len(diff) != 0 {
				t.Errorf("expected no diff entries, got %v", diff)
			}
		})
	}
}

// TestSemanticDiff_RealDifferencesSurviveNullTolerance asserts the
// #3690 workaround only suppresses extra null keys on the actual side:
// an extra non-null key, a missing non-null key, or a null key the
// expected side carries that actual dropped still produces a diff entry.
func TestSemanticDiff_RealDifferencesSurviveNullTolerance(t *testing.T) {
	cases := []struct {
		name string
		a, b string
	}{
		{"extra non-null key on actual", `{"k0":-26}`, `{"extra":1,"k0":-26}`},
		{"missing non-null key on actual", `{"a":1,"b":2}`, `{"a":1}`},
		{"null vs non-null value", `{"a":null}`, `{"a":1}`},
		{"null key missing from actual", `{"a":null,"k":1}`, `{"k":1}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			diff, err := SemanticDiff("side", []byte(c.a), []byte(c.b))
			if err != nil {
				t.Fatalf("SemanticDiff: %v", err)
			}
			if len(diff) == 0 {
				t.Errorf("expected a diff entry, got none")
			}
		})
	}
}

// TestSemanticDiffParity_ToleratesExtraNullEitherSide pins the symmetric
// #3690 tolerance used for actual-vs-actual parity comparisons: a leaked
// null key on either side is forgiven, since both legs are BAML-generated
// and may independently carry the leak.
func TestSemanticDiffParity_ToleratesExtraNullEitherSide(t *testing.T) {
	cases := []struct {
		name string
		a, b string
	}{
		{"same extra null both sides", `{"f":null,"k0":-26}`, `{"f":null,"k0":-26}`},
		{"extra null on a only", `{"f":null,"k0":-26}`, `{"k0":-26}`},
		{"extra null on b only", `{"k0":-26}`, `{"f":null,"k0":-26}`},
		{"different leaked null each side", `{"fa":null,"k0":-26}`, `{"fb":null,"k0":-26}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			diff, err := SemanticDiffParity("side", []byte(c.a), []byte(c.b))
			if err != nil {
				t.Fatalf("SemanticDiffParity: %v", err)
			}
			if len(diff) != 0 {
				t.Errorf("expected no diff entries, got %v", diff)
			}
		})
	}
}

// TestSemanticDiffParity_RealDifferencesStillDiff asserts the parity
// variant only forgives extra null keys: an extra non-null key on either
// side, or a differing value, still produces a diff entry.
func TestSemanticDiffParity_RealDifferencesStillDiff(t *testing.T) {
	cases := []struct {
		name string
		a, b string
	}{
		{"extra non-null key on a", `{"extra":1,"k0":-26}`, `{"k0":-26}`},
		{"extra non-null key on b", `{"k0":-26}`, `{"extra":1,"k0":-26}`},
		{"differing value", `{"k0":-26}`, `{"k0":-27}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			diff, err := SemanticDiffParity("side", []byte(c.a), []byte(c.b))
			if err != nil {
				t.Fatalf("SemanticDiffParity: %v", err)
			}
			if len(diff) == 0 {
				t.Errorf("expected a diff entry, got none")
			}
		})
	}
}

// literalClassSchema is a single-class schema whose Fuzz_field_0 is a
// string literal (value `"quoted"`, i.e. the two-quote token) and whose
// Fuzz_field_1 is a plain string — used to pin that the literal-escape
// tolerance fires only at the literal-typed path.
func literalClassSchema() FuzzSchema {
	return FuzzSchema{
		Classes: []FuzzClass{{
			Name: "FuzzClass0",
			Properties: []FuzzProperty{
				{Name: "Fuzz_field_0", Type: FuzzType{Kind: KindLiteral, Literal: &FuzzLiteral{Kind: LiteralString, String: `"quoted"`}}},
				{Name: "Fuzz_field_1", Type: FuzzType{Kind: KindString}},
			},
		}},
		RootClass: "FuzzClass0",
	}
}

// decoded / escaped are the two wire forms BAML's static codegen emits
// for the same `"quoted"` literal depending on the literal's position in
// the type tree: the decoded value (`"quoted"`) vs the source-escaped
// token (`\"quoted\"`). Both are valid JSON strings; they decode to
// different Go strings, which is why the comparison layer must reconcile
// them when the schema says the field is a literal.
const (
	decodedLiteralObj = `{"Fuzz_field_0":"\"quoted\"","Fuzz_field_1":"plain"}`
	escapedLiteralObj = `{"Fuzz_field_0":"\\\"quoted\\\"","Fuzz_field_1":"plain"}`
)

// TestSemanticDiffWithSchema_ToleratesLiteralEscape pins the
// position-dependent literal-escape tolerance: when the schema types a
// field as a string literal, the decoded form (`"quoted"`) and the
// source-escaped form (`\"quoted\"`) are treated as equal in BOTH
// directions. This reproduces nightly #9 (BAML returns escaped, oracle
// expects decoded) and the symmetric case.
func TestSemanticDiffWithSchema_ToleratesLiteralEscape(t *testing.T) {
	schema := literalClassSchema()
	cases := []struct {
		name string
		a, b string
	}{
		{"expected decoded, actual escaped", decodedLiteralObj, escapedLiteralObj},
		{"expected escaped, actual decoded", escapedLiteralObj, decodedLiteralObj},
		{"both decoded", decodedLiteralObj, decodedLiteralObj},
		{"both escaped", escapedLiteralObj, escapedLiteralObj},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			diff, err := SemanticDiffWithSchema("expected_vs_rest", schema, nil, []byte(c.a), []byte(c.b))
			if err != nil {
				t.Fatalf("SemanticDiffWithSchema: %v", err)
			}
			if len(diff) != 0 {
				t.Errorf("expected no diff entries, got %v", diff)
			}
		})
	}
}

// TestSemanticDiffWithSchema_LiteralEscapeIsNarrow asserts the tolerance
// is confined to literal-typed string fields: an escape-level mismatch on
// a plain string field still diffs, and a genuinely different literal
// value still diffs.
func TestSemanticDiffWithSchema_LiteralEscapeIsNarrow(t *testing.T) {
	schema := literalClassSchema()
	cases := []struct {
		name string
		a, b string
	}{
		// Fuzz_field_1 is a plain string; an escape-level difference there
		// is a real disagreement, not the literal echo.
		{
			"plain string escape diff still diffs",
			`{"Fuzz_field_0":"\"quoted\"","Fuzz_field_1":"\"q\""}`,
			`{"Fuzz_field_0":"\"quoted\"","Fuzz_field_1":"\\\"q\\\""}`,
		},
		// A literal value that genuinely differs (not an escape relation)
		// must still diff.
		{
			"different literal value still diffs",
			`{"Fuzz_field_0":"\"quoted\"","Fuzz_field_1":"plain"}`,
			`{"Fuzz_field_0":"different","Fuzz_field_1":"plain"}`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			diff, err := SemanticDiffWithSchema("expected_vs_rest", schema, nil, []byte(c.a), []byte(c.b))
			if err != nil {
				t.Fatalf("SemanticDiffWithSchema: %v", err)
			}
			if len(diff) == 0 {
				t.Errorf("expected a diff entry, got none")
			}
		})
	}
}

// TestSemanticDiffWithSchema_LiteralUnderUnion exercises the choices-driven
// descent: a literal that sits in a union arm is reconciled only when the
// recorded UnionChoice resolves the arm. Without the choice the union is
// unresolved, no literal path is collected, and the escape mismatch diffs.
func TestSemanticDiffWithSchema_LiteralUnderUnion(t *testing.T) {
	schema := FuzzSchema{
		Classes: []FuzzClass{{
			Name: "FuzzClass0",
			Properties: []FuzzProperty{{
				Name: "Fuzz_field_0",
				Type: FuzzType{Kind: KindUnion, Variants: []FuzzType{
					{Kind: KindLiteral, Literal: &FuzzLiteral{Kind: LiteralString, String: `"quoted"`}},
					{Kind: KindInt},
				}},
			}},
		}},
		RootClass: "FuzzClass0",
		HasUnion:  true,
	}
	decoded := `{"Fuzz_field_0":"\"quoted\""}`
	escaped := `{"Fuzz_field_0":"\\\"quoted\\\""}`
	choices := map[string]UnionChoice{
		".Fuzz_field_0": {Index: 0, Kind: KindLiteral, VariantCount: 2},
	}

	diff, err := SemanticDiffWithSchema("expected_vs_rest", schema, choices, []byte(decoded), []byte(escaped))
	if err != nil {
		t.Fatalf("SemanticDiffWithSchema: %v", err)
	}
	if len(diff) != 0 {
		t.Errorf("with union choice resolving the literal arm, expected no diff, got %v", diff)
	}

	// Without the choice the union arm is unresolved, so the literal path is
	// not collected and the escape mismatch is reported as a real diff.
	diff, err = SemanticDiffWithSchema("expected_vs_rest", schema, nil, []byte(decoded), []byte(escaped))
	if err != nil {
		t.Fatalf("SemanticDiffWithSchema: %v", err)
	}
	if len(diff) == 0 {
		t.Error("without a union choice the unresolved-arm literal must still diff, got none")
	}
}

func TestDetectOrderWarning(t *testing.T) {
	warns := DetectOrderWarning("top",
		[]byte(`{"a":1,"b":2,"c":3}`),
		[]byte(`{"b":2,"a":1,"c":3}`))
	if len(warns) == 0 {
		t.Fatal("expected order warnings, got none")
	}

	none := DetectOrderWarning("top",
		[]byte(`{"a":1,"b":2}`),
		[]byte(`{"a":1,"b":2}`))
	if len(none) != 0 {
		t.Errorf("expected no warnings, got %v", none)
	}
}

func TestSanitizeArtifactBasename(t *testing.T) {
	cases := []struct {
		name  string
		index int
		want  string
	}{
		{"safe_name", 0, "safe_name"},
		{"case_demo", 7, "case_demo"},
		{"with-dashes_and_under", 3, "with-dashes_and_under"},
		// Unsafe inputs all fall back to case_<index>.
		{"", 4, "case_4"},
		{".", 5, "case_5"},
		{"..", 6, "case_6"},
		{"sub/name", 7, "case_7"},
		{`sub\name`, 8, "case_8"},
		{"/abs", 9, "case_9"},
		{"C:foo", 10, "case_10"},
		{"a/../b", 11, "case_11"},
	}
	for _, c := range cases {
		got := sanitizeArtifactBasename(c.name, c.index)
		if got != c.want {
			t.Errorf("sanitizeArtifactBasename(%q, %d) = %q, want %q", c.name, c.index, got, c.want)
		}
	}
}

func TestWriteReplayArtifact_SanitizesUnsafeCaseName(t *testing.T) {
	dir := t.TempDir()
	env := &DynamicFailureEnvelope{
		CaseName:  "../escape",
		CaseIndex: 42,
		Expected:  json.RawMessage(`{"k":"v"}`),
	}
	path, err := WriteReplayArtifact(dir, env)
	if err != nil {
		t.Fatalf("WriteReplayArtifact: %v", err)
	}
	if got, want := filepath.Base(path), "case_42.json"; got != want {
		t.Errorf("basename: got %q want %q (raw=%q)", got, want, path)
	}
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		t.Fatalf("filepath.Rel: %v", err)
	}
	if strings.HasPrefix(rel, "..") {
		t.Errorf("artifact escaped dir: rel=%q dir=%q path=%q", rel, dir, path)
	}
}

func TestWriteStaticReplayArtifact_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	env := &StaticFailureEnvelope{
		CaseName:       "static_demo",
		CaseIndex:      3,
		OracleMode:     OracleStaticPrompt,
		BamlSource:     "class Demo {}",
		FunctionName:   "FuzzFn_C03",
		ClassNames:     []string{"Demo_C03"},
		HasSelfRef:     true,
		MockLLMContent: json.RawMessage(`{"k":"v"}`),
		Expected:       json.RawMessage(`{"k":"v"}`),
	}
	path, err := WriteStaticReplayArtifact(dir, env)
	if err != nil {
		t.Fatalf("WriteStaticReplayArtifact: %v", err)
	}
	if got, want := filepath.Base(path), "static_demo.json"; got != want {
		t.Errorf("path basename: got %q want %q", got, want)
	}
	if env.ReplayPath != path {
		t.Errorf("envelope.ReplayPath: got %q want %q", env.ReplayPath, path)
	}
	if env.GeneratorVersion == "" {
		t.Error("envelope.GeneratorVersion not stamped")
	}
	if env.GeneratedAt == "" {
		t.Error("envelope.GeneratedAt not stamped")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var roundtrip StaticFailureEnvelope
	if err := json.Unmarshal(data, &roundtrip); err != nil {
		t.Fatalf("Unmarshal envelope: %v", err)
	}
	if roundtrip.CaseName != env.CaseName {
		t.Errorf("CaseName round-trip: got %q want %q", roundtrip.CaseName, env.CaseName)
	}
	if roundtrip.FunctionName != env.FunctionName {
		t.Errorf("FunctionName round-trip: got %q want %q", roundtrip.FunctionName, env.FunctionName)
	}
	if roundtrip.BamlSource != env.BamlSource {
		t.Errorf("BamlSource round-trip mismatch")
	}
	if !roundtrip.HasSelfRef {
		t.Error("HasSelfRef round-trip lost")
	}
}

func TestWriteStaticReplayArtifact_SanitizesUnsafeCaseName(t *testing.T) {
	dir := t.TempDir()
	env := &StaticFailureEnvelope{
		CaseName:  "../escape",
		CaseIndex: 9,
	}
	path, err := WriteStaticReplayArtifact(dir, env)
	if err != nil {
		t.Fatalf("WriteStaticReplayArtifact: %v", err)
	}
	if got, want := filepath.Base(path), "case_9.json"; got != want {
		t.Errorf("basename: got %q want %q (raw=%q)", got, want, path)
	}
}

func TestWriteReplayArtifact_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	env := &DynamicFailureEnvelope{
		CaseName:            "case_demo",
		CaseIndex:           7,
		OracleMode:          OracleDynamicThreeWay,
		PreserveSchemaOrder: true,
		MockLLMContent:      json.RawMessage(`{"k":"v"}`),
		Expected:            json.RawMessage(`{"k":"v"}`),
	}
	path, err := WriteReplayArtifact(dir, env)
	if err != nil {
		t.Fatalf("WriteReplayArtifact: %v", err)
	}
	if got, want := filepath.Base(path), "case_demo.json"; got != want {
		t.Errorf("path basename: got %q want %q", got, want)
	}
	if env.ReplayPath != path {
		t.Errorf("envelope.ReplayPath: got %q want %q", env.ReplayPath, path)
	}
	if env.GeneratorVersion == "" {
		t.Error("envelope.GeneratorVersion not stamped")
	}
	if env.GeneratedAt == "" {
		t.Error("envelope.GeneratedAt not stamped")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var roundtrip DynamicFailureEnvelope
	if err := json.Unmarshal(data, &roundtrip); err != nil {
		t.Fatalf("Unmarshal envelope: %v", err)
	}
	if roundtrip.CaseName != env.CaseName {
		t.Errorf("CaseName round-trip: got %q want %q", roundtrip.CaseName, env.CaseName)
	}
}
