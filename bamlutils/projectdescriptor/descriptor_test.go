package projectdescriptor

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils/promptdescriptor"
	"github.com/invakid404/baml-rest/bamlutils/schemadescriptor"
)

// sampleProject is a minimal valid v1 project: one admitted static-unary method
// whose composed sub-descriptors are the JSON-clean node types this package
// reuses. It exercises the composition (Return Bundle, ResolvedValueType arg,
// ModelProvenance) without any bamlparser AST.
func sampleProject() Project {
	return Project{
		Version:                 Version,
		PromptDescriptorVersion: promptdescriptor.Version,
		SchemaVersion:           schemadescriptor.Version,
		Methods: []Method{
			{
				Name:   "Greet",
				Class:  ClassStaticUnary,
				Prompt: "Say hello to {{ name }}",
				Args: []Argument{
					{Name: "name", Type: promptdescriptor.ResolvedValueType{Kind: promptdescriptor.ValueString}},
				},
				Client:   "GPT4",
				Provider: "openai",
				Model:    Model{Value: "gpt-4o", Provenance: promptdescriptor.ModelProvenanceLiteral},
				Return: schemadescriptor.Bundle{
					Version: schemadescriptor.Version,
					Method:  "Greet",
					Target:  schemadescriptor.Type{Kind: schemadescriptor.TypePrimitive, Primitive: schemadescriptor.PrimitiveString},
				},
				RequiredCapabilities: []CapabilityCode{"static_method", "final_call", "provider_openai", "single_leaf_client"},
			},
		},
		Diagnostics: []Decline{
			{Method: "StreamsAudio", Code: "media_audio", Detail: "input arg is audio"},
		},
	}
}

func TestVersionIsOne(t *testing.T) {
	if Version != 1 {
		t.Fatalf("Version = %d, want 1 (M1 seed)", Version)
	}
}

func TestValidateAcceptsSample(t *testing.T) {
	p := sampleProject()
	if err := p.Validate(); err != nil {
		t.Fatalf("Validate(sample) = %v, want nil", err)
	}
}

func TestValidateFencesVersions(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Project)
	}{
		{"project version", func(p *Project) { p.Version = Version + 1 }},
		{"prompt-descriptor version", func(p *Project) { p.PromptDescriptorVersion = promptdescriptor.Version + 1 }},
		{"schema version", func(p *Project) { p.SchemaVersion = schemadescriptor.Version + 1 }},
		{"method return version", func(p *Project) { p.Methods[0].Return.Version = schemadescriptor.Version + 1 }},
		{"empty method class", func(p *Project) { p.Methods[0].Class = "" }},
		{"empty diagnostic code", func(p *Project) { p.Diagnostics[0].Code = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := sampleProject()
			tc.mutate(&p)
			if err := p.Validate(); err == nil {
				t.Fatalf("Validate mutated %q = nil, want error (fail-closed fence)", tc.name)
			}
		})
	}
}

// TestJSONRoundTrip proves the descriptor is a faithful cross-module artifact:
// marshal → unmarshal is value-identical (no field carries transient parse
// state), so the introspect producer and the codegen consumer see the same data.
func TestJSONRoundTrip(t *testing.T) {
	p := sampleProject()
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back Project
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(p, back) {
		t.Fatalf("round-trip mismatch:\n before: %+v\n after:  %+v", p, back)
	}
	// Deterministic: marshaling twice yields identical bytes.
	raw2, _ := json.Marshal(back)
	if string(raw) != string(raw2) {
		t.Fatalf("marshal is not deterministic:\n %s\n %s", raw, raw2)
	}
}
