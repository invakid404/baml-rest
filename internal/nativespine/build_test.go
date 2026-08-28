package nativespine

import (
	"reflect"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils/projectdescriptor"
	"github.com/invakid404/baml-rest/bamlutils/promptdescriptor"
	sd "github.com/invakid404/baml-rest/bamlutils/schemadescriptor"
)

// TestFixtureDescriptor is the golden-descriptor proof: from the representative
// .baml, the admitted method carries the right name, argument order/names/types,
// prompt bytes, client/provider/model provenance, and return schema; and the
// declined methods carry the right stable codes.
func TestFixtureDescriptor(t *testing.T) {
	p, err := BuildFromSource(M1FixtureSources)
	if err != nil {
		t.Fatalf("BuildFromSource: %v", err)
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if p.Version != projectdescriptor.Version || p.PromptDescriptorVersion != promptdescriptor.Version || p.SchemaVersion != sd.Version {
		t.Fatalf("versions = {%d,%d,%d}", p.Version, p.PromptDescriptorVersion, p.SchemaVersion)
	}

	if len(p.Methods) != 1 {
		t.Fatalf("want 1 admitted method, got %d", len(p.Methods))
	}
	m := p.Methods[0]
	if m.Name != "Greet" || m.Class != projectdescriptor.ClassStaticUnary {
		t.Fatalf("method = %q/%q", m.Name, m.Class)
	}
	if m.Prompt != "Greet {{ name }}. Formal: {{ formal }}" {
		t.Fatalf("prompt bytes = %q", m.Prompt)
	}
	if m.Client != "GPT4" || m.Provider != "openai" {
		t.Fatalf("client/provider = %q/%q", m.Client, m.Provider)
	}
	if m.Model.Value != "gpt-4o" || m.Model.Provenance != promptdescriptor.ModelProvenanceLiteral {
		t.Fatalf("model = %+v", m.Model)
	}
	wantArgs := []projectdescriptor.Argument{
		{Name: "name", Type: promptdescriptor.ResolvedValueType{Kind: promptdescriptor.ValueString}},
		{Name: "formal", Type: promptdescriptor.ResolvedValueType{Kind: promptdescriptor.ValueBool}},
	}
	if !reflect.DeepEqual(m.Args, wantArgs) {
		t.Fatalf("args = %+v, want %+v", m.Args, wantArgs)
	}
	if m.Return.Method != "Greet" || m.Return.Target.Kind != sd.TypeClass || m.Return.Target.Name != "Greeting" {
		t.Fatalf("return target = %+v", m.Return.Target)
	}
	if len(m.Return.Classes) != 1 || m.Return.Classes[0].Name.Name != "Greeting" || len(m.Return.Classes[0].Fields) != 2 {
		t.Fatalf("return classes = %+v", m.Return.Classes)
	}

	// Declines: exactly one per category, with the expected codes.
	wantDeclines := map[string]projectdescriptor.CapabilityCode{
		"AnthropicGreet": DeclineProviderNotOpenAI,
		"EnvGreet":       DeclineModelNotLiteral,
		"FallbackGreet":  DeclineStrategyFallback,
		"ScoreName":      DeclineChecks,
	}
	got := map[string]projectdescriptor.CapabilityCode{}
	for _, d := range p.Diagnostics {
		got[d.Method] = d.Code
		if d.Detail == "" {
			t.Errorf("decline %q has empty detail", d.Method)
		}
	}
	if !reflect.DeepEqual(got, wantDeclines) {
		t.Fatalf("declines = %+v, want %+v", got, wantDeclines)
	}
}

// TestDescriptorDeterministic proves the descriptor is stable across repeated
// runs and independent of source-file ordering (methods sorted by name).
func TestDescriptorDeterministic(t *testing.T) {
	a, err := BuildFromSource(M1FixtureSources)
	if err != nil {
		t.Fatal(err)
	}
	b, err := BuildFromSource(M1FixtureSources)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatal("descriptor is not deterministic across repeated runs")
	}
	// Same content under a different file-name set (order-independence): rename
	// the files so the walk order differs but the declarations are identical.
	shuffled := map[string]string{
		"zzz_functions.baml": M1FixtureSources["functions.baml"],
		"aaa_clients.baml":   M1FixtureSources["clients.baml"],
		"mmm_types.baml":     M1FixtureSources["types.baml"],
	}
	c, err := BuildFromSource(shuffled)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, c) {
		t.Fatal("descriptor depends on source-file ordering")
	}
}

// TestClassifyOutputSchema exercises the schema-walk declines directly with
// synthetic bundles, covering categories the .baml fixture does not.
func TestClassifyOutputSchema(t *testing.T) {
	prim := func(p sd.PrimitiveKind) sd.Type { return sd.Type{Kind: sd.TypePrimitive, Primitive: p} }
	cases := []struct {
		name   string
		bundle sd.Bundle
		want   projectdescriptor.CapabilityCode
	}{
		{"clean primitive", sd.Bundle{Target: prim(sd.PrimitiveString)}, ""},
		{"optional primitive", sd.Bundle{Target: sd.Type{Kind: sd.TypeUnion, Union: &sd.UnionType{Nullable: true, Variants: []sd.Type{prim(sd.PrimitiveString)}}}}, ""},
		{"media image target", sd.Bundle{Target: sd.Type{Kind: sd.TypePrimitive, Primitive: sd.PrimitiveMedia, Media: sd.MediaImage}}, DeclineMediaImage},
		{"media audio in class field", sd.Bundle{
			Target: sd.Type{Kind: sd.TypeClass, Name: "C"},
			Classes: []sd.ClassDef{{Name: sd.Name{Name: "C"}, Fields: []sd.ClassField{
				{Name: sd.Name{Name: "a"}, Type: sd.Type{Kind: sd.TypePrimitive, Primitive: sd.PrimitiveMedia, Media: sd.MediaAudio}},
			}}},
		}, DeclineMediaAudio},
		{"dynamic type", sd.Bundle{Target: sd.Type{Kind: sd.TypeClass, Name: "C", Dynamic: true}}, DeclineSchemaDynamicClass},
		{"check constraint", sd.Bundle{
			Target:  sd.Type{Kind: sd.TypeClass, Name: "C"},
			Classes: []sd.ClassDef{{Name: sd.Name{Name: "C"}, Constraints: []sd.Constraint{{Level: sd.ConstraintCheck, Expression: "x"}}}},
		}, DeclineChecks},
		{"assert constraint on target meta", sd.Bundle{Target: sd.Type{Kind: sd.TypePrimitive, Primitive: sd.PrimitiveString, Meta: sd.TypeMeta{Constraints: []sd.Constraint{{Level: sd.ConstraintAssert, Expression: "x"}}}}}, DeclineAsserts},
		// M3b: a multi-arm union of supported arms is now ADMITTED (it lowers to a
		// discriminated carrier). A single-nullable-variant union stays M3a's *T.
		{"multi-variant union of supported arms", sd.Bundle{Target: sd.Type{Kind: sd.TypeUnion, Union: &sd.UnionType{Variants: []sd.Type{prim(sd.PrimitiveString), prim(sd.PrimitiveInt)}}}}, ""},
		{"multi-variant nullable union", sd.Bundle{Target: sd.Type{Kind: sd.TypeUnion, Union: &sd.UnionType{Nullable: true, Variants: []sd.Type{prim(sd.PrimitiveString), prim(sd.PrimitiveInt)}}}}, ""},
		// M3b: string/int/bool literals admit (standalone and as union arms).
		{"standalone string literal", sd.Bundle{Target: sd.Type{Kind: sd.TypeLiteral, Literal: &sd.LiteralValue{Kind: sd.LiteralString, String: "active"}}}, ""},
		{"standalone int literal", sd.Bundle{Target: sd.Type{Kind: sd.TypeLiteral, Literal: &sd.LiteralValue{Kind: sd.LiteralInt, Int: 7}}}, ""},
		{"standalone bool literal", sd.Bundle{Target: sd.Type{Kind: sd.TypeLiteral, Literal: &sd.LiteralValue{Kind: sd.LiteralBool, Bool: true}}}, ""},
		{"same-base literal union", sd.Bundle{Target: sd.Type{Kind: sd.TypeUnion, Union: &sd.UnionType{Variants: []sd.Type{
			{Kind: sd.TypeLiteral, Literal: &sd.LiteralValue{Kind: sd.LiteralString, String: "a"}},
			{Kind: sd.TypeLiteral, Literal: &sd.LiteralValue{Kind: sd.LiteralString, String: "b"}},
		}}}}, ""},
		// A literal with no value is malformed — decline, never admit-then-fail.
		{"nil-payload literal", sd.Bundle{Target: sd.Type{Kind: sd.TypeLiteral, Literal: nil}}, DeclineUnsupportedOutputShape},
		// A union does NOT launder an unsupported arm (media) into admission.
		{"union with media arm", sd.Bundle{Target: sd.Type{Kind: sd.TypeUnion, Union: &sd.UnionType{Variants: []sd.Type{
			prim(sd.PrimitiveString),
			{Kind: sd.TypePrimitive, Primitive: sd.PrimitiveMedia, Media: sd.MediaImage},
		}}}}, DeclineMediaImage},
		// A zero-arm union is malformed — decline rather than emit an arm-less carrier.
		{"zero-arm union", sd.Bundle{Target: sd.Type{Kind: sd.TypeUnion, Union: &sd.UnionType{Variants: nil}}}, DeclineUnsupportedOutputShape},
		// A NON-nullable single-arm union is malformed for a carrier (not an
		// optional-of-one, not multi-arm) — decline, keeping admission == emission
		// (the emitter's buildCarrierPlan errors on len<2). Not constructible via the
		// descriptor builder, but the classifier must still fail closed.
		{"non-nullable single-arm union", sd.Bundle{Target: sd.Type{Kind: sd.TypeUnion, Union: &sd.UnionType{Nullable: false, Variants: []sd.Type{prim(sd.PrimitiveString)}}}}, DeclineUnsupportedOutputShape},
		// M3a admits a STRING-keyed map (value within the carrier profile); a
		// non-string-keyed map or a value outside the profile still declines.
		{"string-keyed map", sd.Bundle{Target: sd.Type{Kind: sd.TypeMap, Key: ptr(prim(sd.PrimitiveString)), Value: ptr(prim(sd.PrimitiveString))}}, ""},
		{"int-keyed map", sd.Bundle{Target: sd.Type{Kind: sd.TypeMap, Key: ptr(prim(sd.PrimitiveInt)), Value: ptr(prim(sd.PrimitiveString))}}, DeclineUnsupportedOutputShape},
		// A string-keyed map with NO value type must FAIL CLOSED at classification —
		// otherwise it is admitted here (walkType(nil) succeeds) and schemaGoType
		// rejects it only at emit time (admit-then-fail).
		{"string-keyed map, nil value type", sd.Bundle{Target: sd.Type{Kind: sd.TypeMap, Key: ptr(prim(sd.PrimitiveString)), Value: nil}}, DeclineUnsupportedOutputShape},
		{"string-keyed map of media value", sd.Bundle{Target: sd.Type{Kind: sd.TypeMap, Key: ptr(prim(sd.PrimitiveString)), Value: ptr(sd.Type{Kind: sd.TypePrimitive, Primitive: sd.PrimitiveMedia, Media: sd.MediaImage})}}, DeclineMediaImage},
		{"recursive classes", sd.Bundle{Target: sd.Type{Kind: sd.TypeClass, Name: "C"}, RecursiveClasses: []string{"C"}}, DeclineUnsupportedOutputShape},
		// M3b: an aliased output field/enum member is now ADMITTED — the alias is
		// ingress-only metadata and is IGNORED; the emitter serves the CANONICAL
		// key/value (empirically confirmed, scope §2). Even an exotic alias admits.
		{"aliased class field", sd.Bundle{
			Target: sd.Type{Kind: sd.TypeClass, Name: "C"},
			Classes: []sd.ClassDef{{Name: sd.Name{Name: "C"}, Fields: []sd.ClassField{
				{Name: sd.Name{Name: "confidence", Alias: strptr("score")}, Type: prim(sd.PrimitiveFloat)},
			}}},
		}, ""},
		{"exotic aliased class field", sd.Bundle{
			Target: sd.Type{Kind: sd.TypeClass, Name: "C"},
			Classes: []sd.ClassDef{{Name: sd.Name{Name: "C"}, Fields: []sd.ClassField{
				{Name: sd.Name{Name: "confidence", Alias: strptr("")}, Type: prim(sd.PrimitiveFloat)},
			}}},
		}, ""},
		{"aliased enum member", sd.Bundle{
			Target: sd.Type{Kind: sd.TypeEnum, Name: "E"},
			Enums: []sd.EnumDef{{Name: sd.Name{Name: "E"}, Values: []sd.EnumValue{
				{Name: sd.Name{Name: "RED", Alias: strptr("rouge")}},
			}}},
		}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, detail := classifyOutputSchema(tc.bundle)
			if code != tc.want {
				t.Fatalf("code = %q (%s), want %q", code, detail, tc.want)
			}
		})
	}
}

func ptr(t sd.Type) *sd.Type  { return &t }
func strptr(s string) *string { return &s }
