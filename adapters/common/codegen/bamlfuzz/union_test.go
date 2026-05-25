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

// TestAnalyzeGraphRootTypeRefsDoNotPollutePropertyGraph pins that a
// RootType referencing the root class does not flip HasSelfRef when
// the class's own property tree has no self edge. The two fields can
// coexist (RootType wins for emission, RootClass stays for replay
// compatibility), so root-reachable refs must not be folded into the
// class's direct-edge set.
func TestAnalyzeGraphRootTypeRefsDoNotPollutePropertyGraph(t *testing.T) {
	rootRef := FuzzType{Kind: KindClassRef, Ref: "Root"}
	rootUnion := FuzzType{
		Kind: KindUnion,
		Variants: []FuzzType{
			rootRef,
			{Kind: KindString},
		},
	}
	schema := FuzzSchema{
		Classes: []FuzzClass{{
			Name: "Root",
			Properties: []FuzzProperty{
				{Name: "f", Type: FuzzType{Kind: KindInt}},
			},
		}},
		RootClass: "Root",
		RootType:  &rootUnion,
	}
	got := AnalyzeGraph(schema)
	if got.HasSelfRef {
		t.Errorf("HasSelfRef should stay false when Root's properties have no self edge, got true")
	}
	if got.HasMutualCycle {
		t.Errorf("HasMutualCycle should be false here, got true")
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
// union value with no Variant set; silently skipping the node would
// hide an upstream invariant break.
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

// TestNormalizeRawRootUnion asserts the normalizer round-trips a raw
// top-level union (RootClass == "", RootType set). Without EffectiveRoot
// dispatch the normalizer would try to read the payload as a class
// and bail with "unknown class".
func TestNormalizeRawRootUnion(t *testing.T) {
	root := FuzzType{
		Kind: KindUnion,
		Variants: []FuzzType{
			{Kind: KindString},
			{Kind: KindInt},
		},
	}
	schema := FuzzSchema{RootType: &root}
	choices := map[string]UnionChoice{
		"": {Index: 1, Kind: KindInt, VariantCount: 2},
	}
	out, err := NormalizeMockToExpectedWithChoices(schema, json.RawMessage(`42`), "", choices)
	if err != nil {
		t.Fatalf("normalize raw root union: %v", err)
	}
	if string(out) != "42" {
		t.Errorf("normalize raw root union\nwant: 42\ngot:  %s", string(out))
	}
}

// TestNormalizeRawRootList asserts the normalizer handles a raw
// top-level list root by dispatching off EffectiveRoot.
func TestNormalizeRawRootList(t *testing.T) {
	elem := FuzzType{Kind: KindInt}
	root := FuzzType{Kind: KindList, Inner: &elem}
	schema := FuzzSchema{RootType: &root}
	out, err := NormalizeMockToExpectedWithChoices(schema, json.RawMessage(`[1,2,3]`), "", nil)
	if err != nil {
		t.Fatalf("normalize raw root list: %v", err)
	}
	if string(out) != "[1,2,3]" {
		t.Errorf("normalize raw root list\nwant: [1,2,3]\ngot:  %s", string(out))
	}
}

// TestNormalizeRawRootMap asserts the normalizer handles a raw
// top-level map root.
func TestNormalizeRawRootMap(t *testing.T) {
	key := FuzzType{Kind: KindString}
	val := FuzzType{Kind: KindInt}
	root := FuzzType{Kind: KindMap, Key: &key, Inner: &val}
	schema := FuzzSchema{RootType: &root}
	out, err := NormalizeMockToExpectedWithChoices(schema, json.RawMessage(`{"a":1,"b":2}`), "", nil)
	if err != nil {
		t.Fatalf("normalize raw root map: %v", err)
	}
	// Map keys sort alphabetically.
	want := `{"a":1,"b":2}`
	if string(out) != want {
		t.Errorf("normalize raw root map\nwant: %s\ngot:  %s", want, string(out))
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

// TestEmitDynamicSingleArmUnionEmitsBareVariant asserts the dynamic
// emitter collapses a single-arm union to its bare variant at both
// top-level and nested positions. Matches the static emitter's
// behaviour so hand-written or replay schemas that survived through
// the Move B shrink-collapse pass with a single-arm union lower as
// the bare variant; a one-element OneOf would not be legal BAML.
func TestEmitDynamicSingleArmUnionEmitsBareVariant(t *testing.T) {
	topLevelUni := FuzzType{
		Kind:     KindUnion,
		Variants: []FuzzType{{Kind: KindString}},
	}
	nestedUni := FuzzType{
		Kind:     KindUnion,
		Variants: []FuzzType{{Kind: KindInt}},
	}
	schema := FuzzSchema{
		Classes: []FuzzClass{{
			Name: "Root",
			Properties: []FuzzProperty{
				{Name: "top", Type: topLevelUni},
				{Name: "lst", Type: FuzzType{Kind: KindList, Inner: &nestedUni}},
			},
		}},
		RootClass: "Root",
	}
	out, err := LowerToDynamicSchema(schema)
	if err != nil {
		t.Fatalf("lower: %v", err)
	}
	top, ok := out.Properties.Get("top")
	if !ok {
		t.Fatal("missing top property")
	}
	if top.Type != "string" {
		t.Errorf("expected bare string at top, got type %q", top.Type)
	}
	if len(top.OneOf) != 0 {
		t.Errorf("expected no OneOf entries at top, got %d", len(top.OneOf))
	}
	lst, ok := out.Properties.Get("lst")
	if !ok {
		t.Fatal("missing lst property")
	}
	if lst.Type != "list" {
		t.Fatalf("expected list, got %q", lst.Type)
	}
	if lst.Items == nil || lst.Items.Type != "int" {
		t.Errorf("expected nested single-arm union to collapse to int, got %#v", lst.Items)
	}
	if lst.Items != nil && len(lst.Items.OneOf) != 0 {
		t.Errorf("expected no nested OneOf entries, got %d", len(lst.Items.OneOf))
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

// TestSchemaGenDoesNotProduceLiteralInt is a rapid-driven invariant:
// no `KindLiteral` of kind `LiteralInt` may appear anywhere in a
// drawn schema. BAML's grammar does not accept integer literals as
// union variants (`field (0 | bool | 42)` is rejected as "not a valid
// field or attribute definition"), and the static emitter regularly
// surfaces them inside unions where they break the BAML CLI build.
// Pin the generator's literal-kind set so a future reintroduction of
// `LiteralInt` cannot silently reproduce the CI failure class.
func TestSchemaGenDoesNotProduceLiteralInt(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		schema := StaticSchemaGen().Draw(rt, "schema")
		for _, cls := range schema.Classes {
			for _, prop := range cls.Properties {
				checkNoLiteralInt(rt, prop.Type)
			}
		}
		if schema.RootType != nil {
			checkNoLiteralInt(rt, *schema.RootType)
		}
	})
}

func checkNoLiteralInt(rt *rapid.T, t FuzzType) {
	switch t.Kind {
	case KindLiteral:
		if t.Literal != nil && t.Literal.Kind == LiteralInt {
			rt.Fatalf("literal-int %d drawn; BAML rejects integer literals as union variants", t.Literal.Int)
		}
	case KindUnion:
		for _, v := range t.Variants {
			checkNoLiteralInt(rt, v)
		}
	case KindOptional, KindList, KindMap:
		if t.Inner != nil {
			checkNoLiteralInt(rt, *t.Inner)
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

// TestCollapseUnionsToPickedListAllSameCollapses pins that a list of
// union elements collapses when every element picks the same arm.
// The Move B internal path uses ":l" (aggregate, no index) so
// observations across list positions merge.
func TestCollapseUnionsToPickedListAllSameCollapses(t *testing.T) {
	uni := FuzzType{
		Kind: KindUnion,
		Variants: []FuzzType{
			{Kind: KindString},
			{Kind: KindInt},
		},
	}
	listOfUnion := FuzzType{Kind: KindList, Inner: &uni}
	schema := FuzzSchema{
		Classes: []FuzzClass{{
			Name: "Root",
			Properties: []FuzzProperty{
				{Name: "items", Type: listOfUnion},
			},
		}},
		RootClass: "Root",
	}
	mkArm := func(s string) FuzzValue {
		return FuzzValue{Kind: KindUnion, VariantIndex: 0, Variant: &FuzzValue{Kind: KindString, String: s}}
	}
	value := FuzzValue{
		Kind: KindClassRef, ClassName: "Root",
		Fields: []FuzzFieldValue{
			{Name: "items", Value: FuzzValue{Kind: KindList, Items: []FuzzValue{
				mkArm("a"), mkArm("b"), mkArm("c"),
			}}},
		},
	}
	newSchema, newValue, err := collapseUnionsToPicked(schema, value)
	if err != nil {
		t.Fatalf("collapse: %v", err)
	}
	cls, _ := newSchema.FindClass("Root")
	if cls.Properties[0].Type.Kind != KindList {
		t.Fatalf("expected list kind, got %q", cls.Properties[0].Type.Kind)
	}
	if cls.Properties[0].Type.Inner == nil || cls.Properties[0].Type.Inner.Kind != KindString {
		t.Errorf("expected list<string> after collapse, got inner %#v", cls.Properties[0].Type.Inner)
	}
	for i, item := range newValue.Fields[0].Value.Items {
		if item.Kind != KindString {
			t.Errorf("list[%d]: expected string, got kind %q", i, item.Kind)
		}
	}
}

// TestCollapseUnionsToPickedListMixedChoicesDoesNotCollapse pins that
// mixed arm picks across list elements leave the union shape intact.
// Merging at the element-type position invalidates the plan when
// elements disagree.
func TestCollapseUnionsToPickedListMixedChoicesDoesNotCollapse(t *testing.T) {
	uni := FuzzType{
		Kind: KindUnion,
		Variants: []FuzzType{
			{Kind: KindString},
			{Kind: KindInt},
		},
	}
	listOfUnion := FuzzType{Kind: KindList, Inner: &uni}
	schema := FuzzSchema{
		Classes: []FuzzClass{{
			Name: "Root",
			Properties: []FuzzProperty{
				{Name: "items", Type: listOfUnion},
			},
		}},
		RootClass: "Root",
	}
	value := FuzzValue{
		Kind: KindClassRef, ClassName: "Root",
		Fields: []FuzzFieldValue{
			{Name: "items", Value: FuzzValue{Kind: KindList, Items: []FuzzValue{
				{Kind: KindUnion, VariantIndex: 0, Variant: &FuzzValue{Kind: KindString, String: "a"}},
				{Kind: KindUnion, VariantIndex: 1, Variant: &FuzzValue{Kind: KindInt, Int: 7}},
			}}},
		},
	}
	newSchema, newValue, err := collapseUnionsToPicked(schema, value)
	if err != nil {
		t.Fatalf("collapse: %v", err)
	}
	cls, _ := newSchema.FindClass("Root")
	if cls.Properties[0].Type.Inner == nil || cls.Properties[0].Type.Inner.Kind != KindUnion {
		t.Errorf("mixed list picks should preserve union, got inner %#v", cls.Properties[0].Type.Inner)
	}
	for i, item := range newValue.Fields[0].Value.Items {
		if item.Kind != KindUnion {
			t.Errorf("list[%d]: expected union wrapper preserved, got kind %q", i, item.Kind)
		}
	}
}

// TestCollapseUnionsToPickedMapValueCollapses pins that a map<string,
// union> collapses when every value picks the same arm.
func TestCollapseUnionsToPickedMapValueCollapses(t *testing.T) {
	uni := FuzzType{
		Kind: KindUnion,
		Variants: []FuzzType{
			{Kind: KindString},
			{Kind: KindInt},
		},
	}
	key := FuzzType{Kind: KindString}
	mapOfUnion := FuzzType{Kind: KindMap, Key: &key, Inner: &uni}
	schema := FuzzSchema{
		Classes: []FuzzClass{{
			Name: "Root",
			Properties: []FuzzProperty{
				{Name: "m", Type: mapOfUnion},
			},
		}},
		RootClass: "Root",
	}
	mkArm := func(s string) FuzzValue {
		return FuzzValue{Kind: KindUnion, VariantIndex: 0, Variant: &FuzzValue{Kind: KindString, String: s}}
	}
	value := FuzzValue{
		Kind: KindClassRef, ClassName: "Root",
		Fields: []FuzzFieldValue{
			{Name: "m", Value: FuzzValue{Kind: KindMap, MapEntries: []FuzzMapEntry{
				{Key: "a", Value: mkArm("x")},
				{Key: "b", Value: mkArm("y")},
			}}},
		},
	}
	newSchema, newValue, err := collapseUnionsToPicked(schema, value)
	if err != nil {
		t.Fatalf("collapse: %v", err)
	}
	cls, _ := newSchema.FindClass("Root")
	if cls.Properties[0].Type.Inner == nil || cls.Properties[0].Type.Inner.Kind != KindString {
		t.Errorf("expected map<string,string> after collapse, got inner %#v", cls.Properties[0].Type.Inner)
	}
	for _, e := range newValue.Fields[0].Value.MapEntries {
		if e.Value.Kind != KindString {
			t.Errorf("map[%q]: expected string, got kind %q", e.Key, e.Value.Kind)
		}
	}
}

// TestCollapseUnionsToPickedNestedUnionInsideMixedOuter pins that an
// inner union nested inside a non-collapsed outer union still
// collapses when the inner choice is consistent within the variant it
// lives in. Move B's path scheme distinguishes ":v0" from ":v1" so
// nested unions are not merged across distinct outer variants.
func TestCollapseUnionsToPickedNestedUnionInsideMixedOuter(t *testing.T) {
	innerUni := FuzzType{
		Kind: KindUnion,
		Variants: []FuzzType{
			{Kind: KindString},
			{Kind: KindBool},
		},
	}
	outer := FuzzType{
		Kind: KindUnion,
		Variants: []FuzzType{
			innerUni,
			{Kind: KindInt},
		},
	}
	// `items` is a list so we get two visits — one picking outer
	// variant 0 (the nested-union arm) and one picking outer variant 1
	// (an int). The outer choice disagrees → outer stays. Inside
	// variant 0 the inner-union choice is consistent across the single
	// visit there, so it collapses to its single arm.
	listOuter := FuzzType{Kind: KindList, Inner: &outer}
	schema := FuzzSchema{
		Classes: []FuzzClass{{
			Name: "Root",
			Properties: []FuzzProperty{
				{Name: "items", Type: listOuter},
			},
		}},
		RootClass: "Root",
	}
	innerArm := FuzzValue{
		Kind: KindUnion, VariantIndex: 0,
		Variant: &FuzzValue{Kind: KindString, String: "x"},
	}
	value := FuzzValue{
		Kind: KindClassRef, ClassName: "Root",
		Fields: []FuzzFieldValue{
			{Name: "items", Value: FuzzValue{Kind: KindList, Items: []FuzzValue{
				{Kind: KindUnion, VariantIndex: 0, Variant: &innerArm},
				{Kind: KindUnion, VariantIndex: 1, Variant: &FuzzValue{Kind: KindInt, Int: 9}},
			}}},
		},
	}
	newSchema, _, err := collapseUnionsToPicked(schema, value)
	if err != nil {
		t.Fatalf("collapse: %v", err)
	}
	cls, _ := newSchema.FindClass("Root")
	listType := cls.Properties[0].Type
	if listType.Kind != KindList || listType.Inner == nil {
		t.Fatalf("expected list type, got %#v", listType)
	}
	outerType := *listType.Inner
	if outerType.Kind != KindUnion || len(outerType.Variants) != 2 {
		t.Fatalf("outer union should be preserved, got %#v", outerType)
	}
	if outerType.Variants[0].Kind != KindString {
		t.Errorf("nested inner union inside outer variant 0 should have collapsed to string, got %#v", outerType.Variants[0])
	}
	if outerType.Variants[1].Kind != KindInt {
		t.Errorf("outer variant 1 should still be int, got %#v", outerType.Variants[1])
	}
}

// schemaDumpJSON is a test helper for printing schemas in failure
// messages. The dump is best-effort: a marshal error degrades to an
// empty string; the helper does not fail the test on its own.
func schemaDumpJSON(s FuzzSchema) string {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return ""
	}
	return string(b)
}

// TestCollapseUnionsToPickedStripsWrappersForCollapsedNestedUnions
// pins the Move B value-rewrite contract for the multi-level union
// shape that surfaced as TestBamlfuzzDynamicOracle/rapid/preserve_off
// /case_3: a 3-level union where the outer arm is consistent across
// observations (collapses), and at least one nested union arm is
// visited by a single observation and collapses too. The pre-fix
// rewrite walked the post-collapse schema using `v.VariantIndex` as
// the descent key, which kept stale wrappers in place — the walker
// then dispatched on a non-union schema kind (e.g. enum_ref) with a
// union-shaped value whose payload field was the zero value, emitting
// the wrapper's zero ("", 0, false) — not the actual leaf payload at
// the bottom of the value tree.
//
// The contract this test pins: after collapseUnionsToPicked returns,
// the rewritten value's structural kinds must align with the rewritten
// schema. No union wrapper must survive at a position where the new
// schema's type is no longer a union, regardless of how many union
// levels the original schema had.
func TestCollapseUnionsToPickedStripsWrappersForCollapsedNestedUnions(t *testing.T) {
	// Schema modelled after the kD branch of the case_3 schema:
	// FuzzClass1.Fuzz_field_0 = union[union[union[null, enum, lit-false],
	//                                          union[bool, lit-false],
	//                                          union[bool, bool, int]],
	//                                  union[<other shapes>]]
	// Reduced to the minimal shape that exercises the bug: a 3-level
	// union where the outer's arm 0 is a middle union, and the middle's
	// arm 1 is a sub-union whose first arm is enum_ref.
	enum := FuzzEnum{Name: "E", Values: []string{"E_V0"}}
	subA := FuzzType{Kind: KindUnion, Variants: []FuzzType{
		{Kind: KindBool},
		{Kind: KindLiteral, Literal: &FuzzLiteral{Kind: LiteralBool, Bool: false}},
	}}
	subB := FuzzType{Kind: KindUnion, Variants: []FuzzType{
		{Kind: KindEnumRef, Ref: enum.Name},
		{Kind: KindBool},
	}}
	subC := FuzzType{Kind: KindUnion, Variants: []FuzzType{
		{Kind: KindString},
		{Kind: KindInt},
	}}
	middle := FuzzType{Kind: KindUnion, Variants: []FuzzType{subA, subB, subC}}
	otherOuterArm := FuzzType{Kind: KindString}
	outer := FuzzType{Kind: KindUnion, Variants: []FuzzType{middle, otherOuterArm}}

	// Wrap in a map so two visits can take different middle arms,
	// disagreeing at the middle level (so middle stays a 3-arm union)
	// while still agreeing at the outer level (so outer collapses to
	// its arm 0 = middle).
	keyT := FuzzType{Kind: KindString}
	mapT := FuzzType{Kind: KindMap, Key: &keyT, Inner: &outer}

	schema := FuzzSchema{
		Classes: []FuzzClass{{
			Name: "Root",
			Properties: []FuzzProperty{
				{Name: "m", Type: mapT},
			},
		}},
		Enums:     []FuzzEnum{enum},
		RootClass: "Root",
	}

	// kA visit: outer=0, middle=0 (subA), inner=1 (lit-false). All
	// three wrappers present, leaf is the literal payload.
	kA := FuzzValue{
		Kind: KindUnion, VariantIndex: 0,
		Variant: &FuzzValue{
			Kind: KindUnion, VariantIndex: 0,
			Variant: &FuzzValue{
				Kind: KindUnion, VariantIndex: 1,
				Variant: &FuzzValue{Kind: KindLiteral, Bool: false},
			},
		},
	}
	// kD visit: outer=0, middle=1 (subB), inner=0 (enum_ref). This
	// reproduces the case_3 failure shape: the inner enum_ref slot
	// retained a union wrapper after rewrite, and the walker emitted
	// "" — not "E_V0" — at that position.
	kD := FuzzValue{
		Kind: KindUnion, VariantIndex: 0,
		Variant: &FuzzValue{
			Kind: KindUnion, VariantIndex: 1,
			Variant: &FuzzValue{
				Kind: KindUnion, VariantIndex: 0,
				Variant: &FuzzValue{Kind: KindEnumRef, Enum: "E_V0"},
			},
		},
	}

	value := FuzzValue{
		Kind: KindClassRef, ClassName: "Root",
		Fields: []FuzzFieldValue{{
			Name: "m",
			Value: FuzzValue{Kind: KindMap, MapEntries: []FuzzMapEntry{
				{Key: "kA", Value: kA},
				{Key: "kD", Value: kD},
			}},
		}},
	}

	newSchema, newValue, err := collapseUnionsToPicked(schema, value)
	if err != nil {
		t.Fatalf("collapse: %v", err)
	}

	// The new schema's m.inner should be the middle union with its
	// three arms rewritten: subA, subB, subC each collapsed (single
	// observation per arm), so the new middle is union[lit-false,
	// enum_ref, string] (visited inner arms collapsed; arm 0 sub-arm
	// 1 was kA's pick → lit-false; arm 1 sub-arm 0 was kD's pick →
	// enum_ref; arm 2 was never visited so stays a string|int union).
	cls, _ := newSchema.FindClass("Root")
	mapType := cls.Properties[0].Type
	if mapType.Kind != KindMap || mapType.Inner == nil {
		t.Fatalf("expected map<string, T>, got %#v", mapType)
	}
	collapsedOuter := *mapType.Inner
	if collapsedOuter.Kind != KindUnion {
		t.Fatalf("expected outer union preserved as a union, got %q\nschema: %s",
			collapsedOuter.Kind, schemaDumpJSON(newSchema))
	}
	if len(collapsedOuter.Variants) != 3 {
		t.Fatalf("collapsed outer should expose middle's 3 arms, got %d: %s",
			len(collapsedOuter.Variants), schemaDumpJSON(newSchema))
	}
	if got := collapsedOuter.Variants[0].Kind; got != KindLiteral {
		t.Errorf("arm 0 (kA's middle arm = subA collapsed via inner=1) should be lit-bool, got %q", got)
	}
	if got := collapsedOuter.Variants[1].Kind; got != KindEnumRef {
		t.Errorf("arm 1 (kD's middle arm = subB collapsed via inner=0) should be enum_ref, got %q", got)
	}
	// arm 2 (subC) was never visited so it stays as the original
	// 2-arm union of unrelated kinds.
	if got := collapsedOuter.Variants[2].Kind; got != KindUnion {
		t.Errorf("arm 2 (never visited) should stay as a union, got %q", got)
	}

	// Now the load-bearing assertion: the rewritten map entries must
	// align with the rewritten schema — kA's value at arm 0 must
	// dispatch as KindLiteral, kD's value at arm 1 must dispatch as
	// KindEnumRef, and crucially the inner slots must carry the leaf
	// payload, not a stale union wrapper.
	if err := assertValueAlignsWithSchema(newSchema, newSchema.EffectiveRoot(), newValue); err != nil {
		t.Fatalf("rewritten value not aligned with new schema: %v\nschema: %s\nvalue: %s",
			err, schemaDumpJSON(newSchema), valueDumpJSON(newValue))
	}

	// And the walked output must reflect the leaf payloads (no ""
	// from a wrapper's zero-value Enum/Bool/Int).
	walked, err := Walk(newSchema, newValue)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	got := string(walked.Expected)
	if !strings.Contains(got, `"E_V0"`) {
		t.Errorf("expected enum value E_V0 in walked output, got %s", got)
	}
	if strings.Contains(got, `""`) {
		t.Errorf("walked output contains \"\" — wrapper stripping regressed: %s", got)
	}
}

// TestCoupledCaseGenValueShapeMatchesSchema is the property-level
// counterpart to the focused regression test above: across a large
// number of random draws, the post-collapse value's structural shape
// must align with the post-collapse schema. The original round-trip
// test (TestCoupledCaseGenProducesWalkableCases) only checked that
// MockLLMContent and Expected normalized to each other, which is
// satisfied even when both are equally wrong — a value-wrapper bug
// renders both via the same broken walker, so the round-trip survives
// while the actual emitted JSON is structurally meaningless. This
// test compares against the schema directly to catch that class of
// drift.
func TestCoupledCaseGenValueShapeMatchesSchema(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		cc := CoupledCaseGen(StaticSchemaGen()).Draw(rt, "case")
		if err := assertValueAlignsWithSchema(cc.Schema, cc.Schema.EffectiveRoot(), cc.Value); err != nil {
			rt.Fatalf("value/schema misalignment: %v\nschema: %s\nvalue: %s",
				err, schemaDumpJSON(cc.Schema), valueDumpJSON(cc.Value))
		}
	})
}

// assertValueAlignsWithSchema walks a FuzzType and FuzzValue in
// lockstep and reports the first kind mismatch. The check is strict:
// every wrapper (optional/list/map/union/class_ref) in the schema must
// have a matching wrapper in the value, and every primitive/literal/
// enum schema slot must NOT have a union value sitting in it. The
// latter is the failure shape the case_3 bug surfaced.
func assertValueAlignsWithSchema(schema FuzzSchema, t FuzzType, v FuzzValue) error {
	switch t.Kind {
	case KindString, KindInt, KindFloat, KindBool, KindNull, KindLiteral, KindEnumRef:
		if v.Kind != t.Kind {
			return &alignError{path: "<scalar>", want: t.Kind, got: v.Kind}
		}
		return nil
	case KindUnion:
		if v.Kind != KindUnion {
			return &alignError{path: "<union>", want: KindUnion, got: v.Kind}
		}
		if v.Variant == nil {
			return errors.New("union value missing Variant")
		}
		if v.VariantIndex < 0 || v.VariantIndex >= len(t.Variants) {
			return errors.New("union variant index out of range")
		}
		return assertValueAlignsWithSchema(schema, t.Variants[v.VariantIndex], *v.Variant)
	case KindOptional:
		if v.Kind != KindOptional {
			return &alignError{path: "<optional>", want: KindOptional, got: v.Kind}
		}
		if v.OptionalShape == OptionalPresent && v.Inner != nil && t.Inner != nil {
			return assertValueAlignsWithSchema(schema, *t.Inner, *v.Inner)
		}
		return nil
	case KindList:
		if v.Kind != KindList {
			return &alignError{path: "<list>", want: KindList, got: v.Kind}
		}
		if t.Inner == nil {
			return nil
		}
		for i, item := range v.Items {
			if err := assertValueAlignsWithSchema(schema, *t.Inner, item); err != nil {
				return &alignError{path: "list[" + itoa(i) + "]", inner: err}
			}
		}
		return nil
	case KindMap:
		if v.Kind != KindMap {
			return &alignError{path: "<map>", want: KindMap, got: v.Kind}
		}
		if t.Inner == nil {
			return nil
		}
		for _, e := range v.MapEntries {
			if err := assertValueAlignsWithSchema(schema, *t.Inner, e.Value); err != nil {
				return &alignError{path: "map[" + e.Key + "]", inner: err}
			}
		}
		return nil
	case KindClassRef:
		if v.Kind != KindClassRef {
			return &alignError{path: "<class>", want: KindClassRef, got: v.Kind}
		}
		if v.ClassName != t.Ref {
			return &alignError{
				path:    "<class>",
				want:    KindClassRef,
				got:     v.Kind,
				wantRef: t.Ref,
				gotRef:  v.ClassName,
			}
		}
		cls, ok := schema.FindClass(t.Ref)
		if !ok {
			return errors.New("class ref " + t.Ref + " not in schema")
		}
		for _, prop := range cls.Properties {
			fv, ok := v.LookupField(prop.Name)
			if !ok {
				continue
			}
			if err := assertValueAlignsWithSchema(schema, prop.Type, fv); err != nil {
				return &alignError{path: t.Ref + "." + prop.Name, inner: err}
			}
		}
		return nil
	}
	return nil
}

type alignError struct {
	path    string
	want    FuzzTypeKind
	got     FuzzTypeKind
	wantRef string
	gotRef  string
	inner   error
}

func (a *alignError) Error() string {
	if a.inner != nil {
		return a.path + ": " + a.inner.Error()
	}
	if a.wantRef != "" || a.gotRef != "" {
		return a.path + ": want kind " + string(a.want) + " ref " + a.wantRef +
			", got " + string(a.got) + " ref " + a.gotRef
	}
	return a.path + ": want kind " + string(a.want) + ", got " + string(a.got)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := false
	if i < 0 {
		neg = true
		i = -i
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		b[pos] = '-'
	}
	return string(b[pos:])
}

func valueDumpJSON(v FuzzValue) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return ""
	}
	return string(b)
}
