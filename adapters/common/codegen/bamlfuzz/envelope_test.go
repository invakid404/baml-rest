package bamlfuzz

import (
	"encoding/json"
	"os"
	"path/filepath"
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
		{"null vs missing", `{"a":null}`, `{}`, false},
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
