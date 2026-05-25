package bamlfuzz

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// TestUnionIRJSONRoundTrip pins on-disk format stability for the
// new KindUnion + UnionChoices fields. Failure here means an existing
// replay artifact (corpus or envelope) cannot be re-loaded after a
// rename or tag drift.
func TestUnionIRJSONRoundTrip(t *testing.T) {
	original := FuzzType{
		Kind: KindUnion,
		Variants: []FuzzType{
			{Kind: KindString},
			{Kind: KindInt},
			{Kind: KindNull},
		},
	}
	encoded, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded FuzzType
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(original, decoded) {
		t.Fatalf("FuzzType round trip mismatch\noriginal: %#v\ndecoded:  %#v", original, decoded)
	}

	val := FuzzValue{
		Kind:         KindUnion,
		VariantIndex: 2,
		Variant:      &FuzzValue{Kind: KindNull},
	}
	encVal, err := json.Marshal(val)
	if err != nil {
		t.Fatalf("marshal value: %v", err)
	}
	var decVal FuzzValue
	if err := json.Unmarshal(encVal, &decVal); err != nil {
		t.Fatalf("unmarshal value: %v", err)
	}
	if !reflect.DeepEqual(val, decVal) {
		t.Fatalf("FuzzValue round trip mismatch\noriginal: %#v\ndecoded:  %#v", val, decVal)
	}

	choice := UnionChoice{Index: 2, Kind: KindNull, VariantCount: 3}
	encChoice, err := json.Marshal(choice)
	if err != nil {
		t.Fatalf("marshal choice: %v", err)
	}
	var decChoice UnionChoice
	if err := json.Unmarshal(encChoice, &decChoice); err != nil {
		t.Fatalf("unmarshal choice: %v", err)
	}
	if !reflect.DeepEqual(choice, decChoice) {
		t.Fatalf("UnionChoice round trip mismatch\noriginal: %#v\ndecoded:  %#v", choice, decChoice)
	}
}

// TestAnalyzeGraphFollowsUnionsForClassRefs asserts a self-ref that
// only reaches the class through a union variant still flips
// HasSelfRef. Without union-aware descent in collectClassRefs the
// graph analyzer would miss it.
func TestAnalyzeGraphFollowsUnionsForClassRefs(t *testing.T) {
	self := FuzzType{Kind: KindClassRef, Ref: "FuzzClass0"}
	uni := FuzzType{
		Kind: KindUnion,
		Variants: []FuzzType{
			{Kind: KindString},
			{Kind: KindOptional, Inner: &self},
		},
	}
	schema := FuzzSchema{
		Classes: []FuzzClass{{
			Name: "FuzzClass0",
			Properties: []FuzzProperty{
				{Name: "f", Type: uni},
			},
		}},
		RootClass: "FuzzClass0",
	}
	got := AnalyzeGraph(schema)
	if !got.HasSelfRef {
		t.Errorf("HasSelfRef should fire for self-ref through union variant, got %+v", got)
	}
	if !got.HasUnion {
		t.Errorf("HasUnion should be true, got false")
	}
}

// TestAnalyzeGraphHasUnionDescendsNestedWrappers makes sure unions
// nested inside list / map / optional wrappers still flip HasUnion.
func TestAnalyzeGraphHasUnionDescendsNestedWrappers(t *testing.T) {
	inner := FuzzType{
		Kind: KindUnion,
		Variants: []FuzzType{
			{Kind: KindString},
			{Kind: KindInt},
		},
	}
	schema := FuzzSchema{
		Classes: []FuzzClass{{
			Name: "Root",
			Properties: []FuzzProperty{
				{Name: "f", Type: FuzzType{Kind: KindList, Inner: &inner}},
			},
		}},
		RootClass: "Root",
	}
	got := AnalyzeGraph(schema)
	if !got.HasUnion {
		t.Errorf("HasUnion should be true for union inside list, got false")
	}
}

// TestAnalyzeGraphHasUnionFalseForUnionFree asserts the flag isn't
// stamped when no KindUnion appears anywhere.
func TestAnalyzeGraphHasUnionFalseForUnionFree(t *testing.T) {
	schema := FuzzSchema{
		Classes: []FuzzClass{{
			Name: "Root",
			Properties: []FuzzProperty{
				{Name: "f", Type: FuzzType{Kind: KindString}},
			},
		}},
		RootClass: "Root",
	}
	got := AnalyzeGraph(schema)
	if got.HasUnion {
		t.Errorf("HasUnion should be false for union-free schema, got true")
	}
}

// TestAnalyzeGraphRawRootRequiresDynamicSkip pins the contract that a
// non-class effective root flips RequiresDynamicSkip — the dynamic
// emitter rejects raw roots with ErrDynamicRootTypeUnsupported.
func TestAnalyzeGraphRawRootRequiresDynamicSkip(t *testing.T) {
	root := FuzzType{Kind: KindString}
	schema := FuzzSchema{
		Classes:   []FuzzClass{{Name: "Root", Properties: []FuzzProperty{{Name: "f", Type: FuzzType{Kind: KindString}}}}},
		RootClass: "Root",
		RootType:  &root,
	}
	got := AnalyzeGraph(schema)
	if !got.RequiresDynamicSkip {
		t.Errorf("RequiresDynamicSkip should be true for raw root type, got false")
	}
}

// TestWalkUnionRendersPickedArm asserts the walker emits the JSON
// for the chosen variant and records the choice in metadata.
func TestWalkUnionRendersPickedArm(t *testing.T) {
	schema := FuzzSchema{
		Classes: []FuzzClass{{
			Name: "Root",
			Properties: []FuzzProperty{
				{Name: "u", Type: FuzzType{
					Kind: KindUnion,
					Variants: []FuzzType{
						{Kind: KindString},
						{Kind: KindInt},
					},
				}},
			},
		}},
		RootClass: "Root",
	}
	value := FuzzValue{
		Kind: KindClassRef, ClassName: "Root",
		Fields: []FuzzFieldValue{
			{Name: "u", Value: FuzzValue{
				Kind: KindUnion, VariantIndex: 1,
				Variant: &FuzzValue{Kind: KindInt, Int: 42},
			}},
		},
	}
	res, err := Walk(schema, value)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	want := `{"u":42}`
	if string(res.MockLLMContent) != want {
		t.Errorf("mock mismatch\nwant: %s\ngot:  %s", want, string(res.MockLLMContent))
	}
	if string(res.Expected) != want {
		t.Errorf("expected mismatch\nwant: %s\ngot:  %s", want, string(res.Expected))
	}
	choice, ok := res.Metadata.UnionChoices[".u"]
	if !ok {
		t.Fatalf("missing union choice for .u")
	}
	if choice.Index != 1 || choice.Kind != KindInt || choice.VariantCount != 2 {
		t.Errorf("choice: got %+v", choice)
	}
}

// TestWalkTNullUnionEmitsExplicitNull asserts T|null implemented as a
// union with a KindNull variant emits JSON null when the null arm is
// picked, separately from KindOptional's absent-key semantics.
func TestWalkTNullUnionEmitsExplicitNull(t *testing.T) {
	schema := FuzzSchema{
		Classes: []FuzzClass{{
			Name: "Root",
			Properties: []FuzzProperty{
				{Name: "x", Type: FuzzType{
					Kind: KindUnion,
					Variants: []FuzzType{
						{Kind: KindString},
						{Kind: KindNull},
					},
				}},
			},
		}},
		RootClass: "Root",
	}
	value := FuzzValue{
		Kind: KindClassRef, ClassName: "Root",
		Fields: []FuzzFieldValue{
			{Name: "x", Value: FuzzValue{
				Kind: KindUnion, VariantIndex: 1,
				Variant: &FuzzValue{Kind: KindNull},
			}},
		},
	}
	res, err := Walk(schema, value)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	want := `{"x":null}`
	if string(res.MockLLMContent) != want {
		t.Errorf("T|null with null arm picked should emit explicit null in mock\nwant: %s\ngot:  %s", want, string(res.MockLLMContent))
	}
}

// TestWalkUnionMissingChoiceFailsClosed asserts the walker rejects a
// union value with no Variant set rather than silently skipping the
// node.
func TestWalkUnionMissingChoiceFailsClosed(t *testing.T) {
	schema := FuzzSchema{
		Classes: []FuzzClass{{
			Name: "Root",
			Properties: []FuzzProperty{
				{Name: "u", Type: FuzzType{
					Kind: KindUnion,
					Variants: []FuzzType{
						{Kind: KindString},
						{Kind: KindInt},
					},
				}},
			},
		}},
		RootClass: "Root",
	}
	value := FuzzValue{
		Kind: KindClassRef, ClassName: "Root",
		Fields: []FuzzFieldValue{
			{Name: "u", Value: FuzzValue{Kind: KindUnion, VariantIndex: 0, Variant: nil}},
		},
	}
	_, err := Walk(schema, value)
	if err == nil {
		t.Fatal("expected error for union value without selected variant, got nil")
	}
}

// TestNormalizeUnionRequiresChoice asserts the normalizer fails
// closed when a union node is reached without a recorded choice.
func TestNormalizeUnionRequiresChoice(t *testing.T) {
	schema := FuzzSchema{
		Classes: []FuzzClass{{
			Name: "Root",
			Properties: []FuzzProperty{
				{Name: "u", Type: FuzzType{
					Kind: KindUnion,
					Variants: []FuzzType{
						{Kind: KindString},
						{Kind: KindInt},
					},
				}},
			},
		}},
		RootClass: "Root",
	}
	_, err := NormalizeMockToExpected(schema, json.RawMessage(`{"u":"x"}`), "Root")
	if err == nil {
		t.Fatal("expected error for union without choice, got nil")
	}
	if !strings.Contains(err.Error(), "union") {
		t.Errorf("error should mention union, got %v", err)
	}
}

// TestEmitDynamicLowersUnionAsOneOf asserts the dynamic emitter
// converts a KindUnion to type=union with OneOf populated.
func TestEmitDynamicLowersUnionAsOneOf(t *testing.T) {
	schema := FuzzSchema{
		Classes: []FuzzClass{{
			Name: "Root",
			Properties: []FuzzProperty{
				{Name: "u", Type: FuzzType{
					Kind: KindUnion,
					Variants: []FuzzType{
						{Kind: KindString},
						{Kind: KindNull},
					},
				}},
			},
		}},
		RootClass: "Root",
	}
	out, err := LowerToDynamicSchema(schema)
	if err != nil {
		t.Fatalf("lower: %v", err)
	}
	prop, ok := out.Properties.Get("u")
	if !ok {
		t.Fatalf("missing root property u")
	}
	if prop.Type != "union" {
		t.Errorf("expected type=union, got %q", prop.Type)
	}
	if len(prop.OneOf) != 2 {
		t.Fatalf("expected 2 variants in OneOf, got %d", len(prop.OneOf))
	}
	if prop.OneOf[0].Type != "string" || prop.OneOf[1].Type != "null" {
		t.Errorf("variants: got %q, %q", prop.OneOf[0].Type, prop.OneOf[1].Type)
	}
}

// TestEmitDynamicRejectsRawRoot asserts the dynamic emitter refuses a
// non-class effective root with ErrDynamicRootTypeUnsupported, which
// wraps ErrDynamicSchemaUnsupported.
func TestEmitDynamicRejectsRawRoot(t *testing.T) {
	root := FuzzType{Kind: KindString}
	schema := FuzzSchema{
		Classes:   []FuzzClass{{Name: "Root", Properties: []FuzzProperty{{Name: "f", Type: FuzzType{Kind: KindString}}}}},
		RootClass: "Root",
		RootType:  &root,
	}
	_, err := LowerToDynamicSchema(schema)
	if !errors.Is(err, ErrDynamicRootTypeUnsupported) {
		t.Fatalf("expected ErrDynamicRootTypeUnsupported, got %v", err)
	}
	if !errors.Is(err, ErrDynamicSchemaUnsupported) {
		t.Errorf("ErrDynamicRootTypeUnsupported should wrap ErrDynamicSchemaUnsupported, got %v", err)
	}
}

// TestEmitStaticUnionPipeSyntax pins the static pipe-with-parens
// spelling at three nesting positions: a top-level field, inside
// optional, and inside list. The function-return position uses the
// unparenthesised pipe form.
func TestEmitStaticUnionPipeSyntax(t *testing.T) {
	uni := FuzzType{
		Kind: KindUnion,
		Variants: []FuzzType{
			{Kind: KindString},
			{Kind: KindInt},
		},
	}
	schema := FuzzSchema{
		Classes: []FuzzClass{{
			Name: "Root",
			Properties: []FuzzProperty{
				{Name: "field", Type: uni},
				{Name: "opt", Type: FuzzType{Kind: KindOptional, Inner: &uni}},
				{Name: "lst", Type: FuzzType{Kind: KindList, Inner: &uni}},
				{Name: "tnull", Type: FuzzType{
					Kind: KindUnion,
					Variants: []FuzzType{
						{Kind: KindString},
						{Kind: KindNull},
					},
				}},
			},
		}},
		RootClass: "Root",
	}
	src, err := LowerToBamlSource(schema, "TestC")
	if err != nil {
		t.Fatalf("lower: %v", err)
	}
	// Top-level field union: appears inline with parens (typeSpelling
	// always wraps unions in parens to keep precedence with the
	// surrounding `(...)?` / `(...)[]` wrappers).
	if !strings.Contains(src.Source, "field (string | int)") {
		t.Errorf("missing field union spelling, got source:\n%s", src.Source)
	}
	// Optional-around-union: outer parens for the union, `?` after.
	if !strings.Contains(src.Source, "opt ((string | int))?") {
		t.Errorf("missing optional-around-union spelling, got source:\n%s", src.Source)
	}
	// List-of-union: outer parens for the union, `[]` after.
	if !strings.Contains(src.Source, "lst ((string | int))[]") {
		t.Errorf("missing list-of-union spelling, got source:\n%s", src.Source)
	}
	// T|null variant.
	if !strings.Contains(src.Source, "tnull (string | null)") {
		t.Errorf("missing T|null spelling, got source:\n%s", src.Source)
	}
}

// TestEmitStaticSingleArmUnionEmitsBareVariant asserts the static
// emitter renders a single-arm union (only reachable through the
// shrink-collapse pass) as the bare variant. Emitting a one-operand
// pipe (`string | `) would be invalid BAML.
func TestEmitStaticSingleArmUnionEmitsBareVariant(t *testing.T) {
	uni := FuzzType{
		Kind:     KindUnion,
		Variants: []FuzzType{{Kind: KindString}},
	}
	schema := FuzzSchema{
		Classes: []FuzzClass{{
			Name: "Root",
			Properties: []FuzzProperty{
				{Name: "f", Type: uni},
			},
		}},
		RootClass: "Root",
	}
	src, err := LowerToBamlSource(schema, "TestC")
	if err != nil {
		t.Fatalf("lower: %v", err)
	}
	if strings.Contains(src.Source, "|") {
		t.Errorf("single-arm union should not emit pipe, got source:\n%s", src.Source)
	}
	if !strings.Contains(src.Source, "f string") {
		t.Errorf("single-arm union should render as bare variant, got source:\n%s", src.Source)
	}
}

// TestEmitStaticRawTopLevelUnion asserts the static emitter
// generates a function return type from FuzzSchema.RootType when
// the effective root is a raw union (no class wrapper).
func TestEmitStaticRawTopLevelUnion(t *testing.T) {
	root := FuzzType{
		Kind: KindUnion,
		Variants: []FuzzType{
			{Kind: KindString},
			{Kind: KindInt},
		},
	}
	schema := FuzzSchema{
		Classes:  []FuzzClass{},
		Enums:    []FuzzEnum{},
		RootType: &root,
	}
	src, err := LowerToBamlSource(schema, "TestC")
	if err != nil {
		t.Fatalf("lower: %v", err)
	}
	if !strings.Contains(src.Source, "-> string | int {") {
		t.Errorf("expected '-> string | int {' in source, got:\n%s", src.Source)
	}
}

// TestValueGenTerminatesThroughUnionRecursion is a rapid-driven
// safety test: union variants can contain class refs that, when
// chosen, would recurse into the same class. The value generator
// must terminate via the per-class depth cap.
func TestValueGenTerminatesThroughUnionRecursion(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		schema := StaticSchemaGen().Draw(rt, "schema")
		value := ValueGen(schema).Draw(rt, "value")
		if _, err := Walk(schema, value); err != nil {
			rt.Fatalf("walk failed: %v\nschema: %s", err, schemaDumpJSON(schema))
		}
	})
}

// TestValueGenRespectsUnionVariantBounds asserts the schema
// generator stays within [MinUnionVariants, MaxUnionVariants] for
// every union it draws.
func TestValueGenRespectsUnionVariantBounds(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		schema := StaticSchemaGen().Draw(rt, "schema")
		for _, cls := range schema.Classes {
			for _, prop := range cls.Properties {
				checkUnionBounds(rt, prop.Type)
			}
		}
		if schema.RootType != nil {
			checkUnionBounds(rt, *schema.RootType)
		}
	})
}

func checkUnionBounds(rt *rapid.T, t FuzzType) {
	switch t.Kind {
	case KindUnion:
		if len(t.Variants) < MinUnionVariants || len(t.Variants) > MaxUnionVariants {
			rt.Fatalf("union arm count %d out of bounds [%d, %d]", len(t.Variants), MinUnionVariants, MaxUnionVariants)
		}
		for _, v := range t.Variants {
			checkUnionBounds(rt, v)
		}
	case KindOptional, KindList, KindMap:
		if t.Inner != nil {
			checkUnionBounds(rt, *t.Inner)
		}
	}
}

// TestCoupledCaseGenProducesWalkableCases is a rapid-driven smoke
// test that CoupledCaseGen always returns a (schema, value) pair
// whose Walk output round-trips through normalize. Catches a future
// regression where the Move B collapse pass leaves the value
// inconsistent with the rewritten schema.
func TestCoupledCaseGenProducesWalkableCases(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		cc := CoupledCaseGen(StaticSchemaGen()).Draw(rt, "case")
		normalized, err := NormalizeMockToExpectedWithChoices(cc.Schema, cc.Walk.MockLLMContent, cc.Schema.RootClass, cc.Walk.Metadata.UnionChoices)
		if err != nil {
			rt.Fatalf("normalize: %v\nschema: %s", err, schemaDumpJSON(cc.Schema))
		}
		if string(normalized) != string(cc.Walk.Expected) {
			rt.Fatalf("round-trip mismatch\nnormalized: %s\nexpected:   %s",
				string(normalized), string(cc.Walk.Expected))
		}
	})
}

// TestCollapseUnionsToPickedReplacesConsistentChoices pins the Move
// B contract: when every value visit through a class field's union
// picks the same arm, the rewritten schema has the union removed at
// that position and the value tree drops the wrapper.
func TestCollapseUnionsToPickedReplacesConsistentChoices(t *testing.T) {
	uni := FuzzType{
		Kind: KindUnion,
		Variants: []FuzzType{
			{Kind: KindString},
			{Kind: KindInt},
		},
	}
	schema := FuzzSchema{
		Classes: []FuzzClass{{
			Name: "Root",
			Properties: []FuzzProperty{
				{Name: "u", Type: uni},
			},
		}},
		RootClass: "Root",
	}
	value := FuzzValue{
		Kind: KindClassRef, ClassName: "Root",
		Fields: []FuzzFieldValue{
			{Name: "u", Value: FuzzValue{
				Kind: KindUnion, VariantIndex: 0,
				Variant: &FuzzValue{Kind: KindString, String: "hi"},
			}},
		},
	}
	newSchema, newValue, err := collapseUnionsToPicked(schema, value)
	if err != nil {
		t.Fatalf("collapse: %v", err)
	}
	cls, ok := newSchema.FindClass("Root")
	if !ok {
		t.Fatal("Root class missing after collapse")
	}
	if cls.Properties[0].Type.Kind != KindString {
		t.Errorf("expected u to collapse to string, got %q", cls.Properties[0].Type.Kind)
	}
	if newValue.Fields[0].Value.Kind != KindString {
		t.Errorf("value union wrapper should be stripped, got kind %q", newValue.Fields[0].Value.Kind)
	}
	if newValue.Fields[0].Value.String != "hi" {
		t.Errorf("expected value 'hi', got %q", newValue.Fields[0].Value.String)
	}
}

// schemaDumpJSON is a test helper for printing schemas in failure
// messages. The dump is best-effort: a marshal error degrades to an
// empty string rather than failing the test.
func schemaDumpJSON(s FuzzSchema) string {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return ""
	}
	return string(b)
}
