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
		// boundaryml/baml#3690 workaround: a key present and null on one
		// side but absent on the other is treated as equivalent, so the
		// leaked optional field does not break equality.
		{"null vs missing", `{"a":null}`, `{}`, true},
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
// workaround: when the only difference between two payloads is a key
// that is present (and null-valued) on one side but absent on the other,
// SemanticDiff reports no disagreement.
func TestSemanticDiff_ToleratesExtraNullKeys(t *testing.T) {
	cases := []struct {
		name string
		a, b string
	}{
		{"extra null on b", `{"k0":-26}`, `{"Fuzz_field_0":null,"k0":-26}`},
		{"extra null on a", `{"Fuzz_field_0":null,"k0":-26}`, `{"k0":-26}`},
		{"null on both", `{"f":null,"k0":-26}`, `{"f":null,"k0":-26}`},
		{"nested extra null", `{"o":{"k":1}}`, `{"o":{"leak":null,"k":1}}`},
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
// #3690 workaround only suppresses extra null keys: a missing non-null
// key or an extra non-null key still produces a diff entry.
func TestSemanticDiff_RealDifferencesSurviveNullTolerance(t *testing.T) {
	cases := []struct {
		name string
		a, b string
	}{
		{"extra non-null key on b", `{"k0":-26}`, `{"extra":1,"k0":-26}`},
		{"missing non-null key on b", `{"a":1,"b":2}`, `{"a":1}`},
		{"null vs non-null value", `{"a":null}`, `{"a":1}`},
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
