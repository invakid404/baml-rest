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
		Capabilities: []MethodCapability{
			{Method: "Greet", Admitted: true, Class: ClassStaticUnary, Required: []CapabilityCode{"static_method", "final_call", "provider_openai", "single_leaf_client"}},
			{Method: "StreamsAudio", Admitted: false, Blocked: "media_audio"},
		},
	}
}

func TestVersionIsTwo(t *testing.T) {
	if Version != 2 {
		t.Fatalf("Version = %d, want 2 (M2 whole-project descriptor)", Version)
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

// TestValidateCapabilityManifest proves Validate enforces the one-record-per-
// retained-method capability contract M4 relies on: every violation fails closed.
func TestValidateCapabilityManifest(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Project)
	}{
		{"absent capability record", func(p *Project) {
			p.Capabilities = p.Capabilities[:1] // drop StreamsAudio's record
		}},
		{"duplicate capability record", func(p *Project) {
			p.Capabilities = append(p.Capabilities, p.Capabilities[0])
		}},
		{"capability for unknown method", func(p *Project) {
			p.Capabilities = append(p.Capabilities, MethodCapability{Method: "Ghost", Admitted: false, Blocked: "media_audio"})
		}},
		{"admitted/declined conflict", func(p *Project) {
			p.Capabilities[0].Admitted = false // Greet is admitted
			p.Capabilities[0].Class = ""
			p.Capabilities[0].Blocked = "checks"
		}},
		{"required-capabilities disagreement", func(p *Project) {
			p.Capabilities[0].Required = []CapabilityCode{"static_method"} // method has four
		}},
		{"method in both admitted and declined", func(p *Project) {
			// Greet is admitted (Methods); also declining it must fail (sets disjoint).
			p.Diagnostics = append(p.Diagnostics, Decline{Method: "Greet", Code: "checks"})
		}},
		{"duplicate method name", func(p *Project) {
			p.Methods = append(p.Methods, p.Methods[0]) // two "Greet" in Methods
		}},
		{"duplicate diagnostic method", func(p *Project) {
			p.Diagnostics = append(p.Diagnostics, Decline{Method: "StreamsAudio", Code: "media_pdf"})
		}},
		{"admitted class mismatch", func(p *Project) {
			p.Capabilities[0].Class = "some_other_class" // method's class is static_unary
		}},
		{"declined record carries class", func(p *Project) {
			p.Capabilities[1].Class = ClassStaticUnary // StreamsAudio is declined
		}},
		{"declined record carries required", func(p *Project) {
			p.Capabilities[1].Required = []CapabilityCode{"static_method"}
		}},
		{"declined blocked code disagrees with diagnostic code", func(p *Project) {
			// StreamsAudio's diagnostic Code is media_audio; a different blocked code
			// is two conflicting reasons for one decline.
			p.Capabilities[1].Blocked = "checks"
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := sampleProject()
			tc.mutate(&p)
			if err := p.Validate(); err == nil {
				t.Fatalf("Validate mutated %q = nil, want error (capability contract)", tc.name)
			}
		})
	}
}

// TestValidateTemplateUniqueness proves Validate rejects a duplicate Template.Name
// (and accepts distinct names).
func TestValidateTemplateUniqueness(t *testing.T) {
	p := sampleProject()
	p.Templates = []Template{{Name: "Header", Body: "a"}, {Name: "Header", Body: "b"}}
	if err := p.Validate(); err == nil {
		t.Fatal("Validate accepted duplicate template names, want error")
	}
	p.Templates = []Template{{Name: "Footer", Body: "a"}, {Name: "Header", Body: "b"}}
	if err := p.Validate(); err != nil {
		t.Fatalf("Validate rejected distinct template names: %v", err)
	}
}

// TestValidateCollectionUniqueness proves Validate rejects a duplicate name in
// each whole-project collection (clients, retry policies, strategies) — closing
// the name-uniqueness class across the descriptor — and accepts distinct names.
func TestValidateCollectionUniqueness(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Project)
	}{
		{"duplicate client", func(p *Project) {
			p.Clients = []Client{{Config: promptdescriptor.ClientConfig{Name: "C"}}, {Config: promptdescriptor.ClientConfig{Name: "C"}}}
		}},
		{"duplicate retry_policy", func(p *Project) {
			p.RetryPolicies = []RetryPolicy{{Name: "R"}, {Name: "R"}}
		}},
		{"duplicate strategy", func(p *Project) {
			p.Strategies = []Strategy{
				{Name: "S", Kind: StrategyFallback, Children: []string{"a"}},
				{Name: "S", Kind: StrategyFallback, Children: []string{"b"}},
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := sampleProject()
			tc.mutate(&p)
			if err := p.Validate(); err == nil {
				t.Fatalf("Validate accepted %q, want error (name uniqueness)", tc.name)
			}
		})
	}

	// Distinct names across all three collections validate cleanly.
	p := sampleProject()
	p.Clients = []Client{{Config: promptdescriptor.ClientConfig{Name: "A"}}, {Config: promptdescriptor.ClientConfig{Name: "B"}}}
	p.RetryPolicies = []RetryPolicy{{Name: "R1"}, {Name: "R2"}}
	p.Strategies = []Strategy{
		{Name: "S1", Kind: StrategyFallback, Children: []string{"a"}},
		{Name: "S2", Kind: StrategyRoundRobin, Children: []string{"b"}},
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("Validate rejected distinct collection names: %v", err)
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
