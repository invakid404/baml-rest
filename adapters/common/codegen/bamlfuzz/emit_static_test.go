package bamlfuzz

import (
	"strings"
	"testing"
)

func TestLowerToBamlSource_ScalarKinds(t *testing.T) {
	schema := AnalyzeGraph(FuzzSchema{
		Classes: []FuzzClass{{Name: "Root", Properties: []FuzzProperty{
			{Name: "s", Type: FuzzType{Kind: KindString}},
			{Name: "i", Type: FuzzType{Kind: KindInt}},
			{Name: "f", Type: FuzzType{Kind: KindFloat}},
			{Name: "b", Type: FuzzType{Kind: KindBool}},
			{Name: "n", Type: FuzzType{Kind: KindNull}},
		}}},
		RootClass: "Root",
	})
	got, err := LowerToBamlSource(schema, "C01")
	if err != nil {
		t.Fatalf("LowerToBamlSource: %v", err)
	}

	wantSubs := []string{
		"class Root_C01 {",
		"  s string",
		"  i int",
		"  f float",
		"  b bool",
		"  n null",
		"function FuzzFn_C01(input: string) -> Root_C01 {",
		"  client TestClient",
	}
	for _, sub := range wantSubs {
		if !strings.Contains(got.Source, sub) {
			t.Errorf("missing substring %q in source:\n%s", sub, got.Source)
		}
	}
	if got.FunctionName != "FuzzFn_C01" {
		t.Errorf("FunctionName: got %q want FuzzFn_C01", got.FunctionName)
	}
	if got.RootClass != "Root_C01" {
		t.Errorf("RootClass: got %q want Root_C01", got.RootClass)
	}
	if len(got.ClassNames) != 1 || got.ClassNames[0] != "Root_C01" {
		t.Errorf("ClassNames: got %v want [Root_C01]", got.ClassNames)
	}
	if len(got.EnumNames) != 0 {
		t.Errorf("EnumNames: got %v want []", got.EnumNames)
	}
}

func TestLowerToBamlSource_Optional(t *testing.T) {
	schema := AnalyzeGraph(FuzzSchema{
		Classes: []FuzzClass{{Name: "Root", Properties: []FuzzProperty{
			{Name: "maybe", Type: FuzzType{Kind: KindOptional, Inner: &FuzzType{Kind: KindString}}},
		}}},
		RootClass: "Root",
	})
	got, err := LowerToBamlSource(schema, "C02")
	if err != nil {
		t.Fatalf("LowerToBamlSource: %v", err)
	}
	if !strings.Contains(got.Source, "  maybe (string)?") {
		t.Errorf("optional spelling missing:\n%s", got.Source)
	}
}

func TestLowerToBamlSource_List(t *testing.T) {
	schema := AnalyzeGraph(FuzzSchema{
		Classes: []FuzzClass{{Name: "Root", Properties: []FuzzProperty{
			{Name: "items", Type: FuzzType{Kind: KindList, Inner: &FuzzType{Kind: KindInt}}},
		}}},
		RootClass: "Root",
	})
	got, err := LowerToBamlSource(schema, "C03")
	if err != nil {
		t.Fatalf("LowerToBamlSource: %v", err)
	}
	if !strings.Contains(got.Source, "  items (int)[]") {
		t.Errorf("list spelling missing:\n%s", got.Source)
	}
}

func TestLowerToBamlSource_Map(t *testing.T) {
	key := FuzzType{Kind: KindString}
	inner := FuzzType{Kind: KindBool}
	schema := AnalyzeGraph(FuzzSchema{
		Classes: []FuzzClass{{Name: "Root", Properties: []FuzzProperty{
			{Name: "flags", Type: FuzzType{Kind: KindMap, Key: &key, Inner: &inner}},
		}}},
		RootClass: "Root",
	})
	got, err := LowerToBamlSource(schema, "C04")
	if err != nil {
		t.Fatalf("LowerToBamlSource: %v", err)
	}
	if !strings.Contains(got.Source, "  flags map<string, bool>") {
		t.Errorf("map spelling missing:\n%s", got.Source)
	}
}

func TestLowerToBamlSource_EnumRef(t *testing.T) {
	schema := AnalyzeGraph(FuzzSchema{
		Classes: []FuzzClass{{Name: "Root", Properties: []FuzzProperty{
			{Name: "status", Type: FuzzType{Kind: KindEnumRef, Ref: "Status"}},
		}}},
		Enums: []FuzzEnum{{Name: "Status", Values: []string{"ACTIVE", "INACTIVE"}}},
		RootClass: "Root",
	})
	got, err := LowerToBamlSource(schema, "C05")
	if err != nil {
		t.Fatalf("LowerToBamlSource: %v", err)
	}
	wantSubs := []string{
		"enum Status_C05 {",
		"  ACTIVE",
		"  INACTIVE",
		"  status Status_C05",
	}
	for _, sub := range wantSubs {
		if !strings.Contains(got.Source, sub) {
			t.Errorf("missing %q in source:\n%s", sub, got.Source)
		}
	}
	if len(got.EnumNames) != 1 || got.EnumNames[0] != "Status_C05" {
		t.Errorf("EnumNames: got %v", got.EnumNames)
	}
}

func TestLowerToBamlSource_ClassRef(t *testing.T) {
	schema := AnalyzeGraph(FuzzSchema{
		Classes: []FuzzClass{
			{Name: "Root", Properties: []FuzzProperty{
				{Name: "inner", Type: FuzzType{Kind: KindClassRef, Ref: "Inner"}},
			}},
			{Name: "Inner", Properties: []FuzzProperty{
				{Name: "label", Type: FuzzType{Kind: KindString}},
			}},
		},
		RootClass: "Root",
	})
	got, err := LowerToBamlSource(schema, "C06")
	if err != nil {
		t.Fatalf("LowerToBamlSource: %v", err)
	}
	if !strings.Contains(got.Source, "  inner Root_C06") && !strings.Contains(got.Source, "  inner Inner_C06") {
		t.Errorf("class ref spelling missing:\n%s", got.Source)
	}
	if !strings.Contains(got.Source, "  inner Inner_C06") {
		t.Errorf("class ref should target Inner_C06, got:\n%s", got.Source)
	}
}

func TestLowerToBamlSource_LiteralKinds(t *testing.T) {
	schema := AnalyzeGraph(FuzzSchema{
		Classes: []FuzzClass{{Name: "Root", Properties: []FuzzProperty{
			{Name: "kind", Type: FuzzType{Kind: KindLiteral, Literal: &FuzzLiteral{Kind: LiteralString, String: "widget"}}},
			{Name: "count", Type: FuzzType{Kind: KindLiteral, Literal: &FuzzLiteral{Kind: LiteralInt, Int: 42}}},
			{Name: "active", Type: FuzzType{Kind: KindLiteral, Literal: &FuzzLiteral{Kind: LiteralBool, Bool: true}}},
		}}},
		RootClass: "Root",
	})
	got, err := LowerToBamlSource(schema, "C07")
	if err != nil {
		t.Fatalf("LowerToBamlSource: %v", err)
	}
	wantSubs := []string{
		`  kind "widget"`,
		"  count 42",
		"  active true",
	}
	for _, sub := range wantSubs {
		if !strings.Contains(got.Source, sub) {
			t.Errorf("missing %q in source:\n%s", sub, got.Source)
		}
	}
}

// TestLowerToBamlSource_LiteralStringEscapes pins the strconv.Quote
// escaping contract — the emitted .baml must remain syntactically
// valid when a literal value carries quotes, backslashes, or non-
// ASCII characters.
func TestLowerToBamlSource_LiteralStringEscapes(t *testing.T) {
	schema := AnalyzeGraph(FuzzSchema{
		Classes: []FuzzClass{{Name: "Root", Properties: []FuzzProperty{
			{Name: "quoted", Type: FuzzType{Kind: KindLiteral, Literal: &FuzzLiteral{Kind: LiteralString, String: `with "quote"`}}},
		}}},
		RootClass: "Root",
	})
	got, err := LowerToBamlSource(schema, "C08")
	if err != nil {
		t.Fatalf("LowerToBamlSource: %v", err)
	}
	if !strings.Contains(got.Source, `  quoted "with \"quote\""`) {
		t.Errorf("literal escape missing:\n%s", got.Source)
	}
}

// TestLowerToBamlSource_SelfRef pins the contract the dynamic emitter
// gates against and the static emitter accepts: a class whose property
// type tree can reach itself must lower successfully on the static
// path.
func TestLowerToBamlSource_SelfRef(t *testing.T) {
	self := FuzzType{Kind: KindClassRef, Ref: "Tree"}
	listSelf := FuzzType{Kind: KindList, Inner: &self}
	schema := AnalyzeGraph(FuzzSchema{
		Classes: []FuzzClass{{Name: "Tree", Properties: []FuzzProperty{
			{Name: "value", Type: FuzzType{Kind: KindString}},
			{Name: "children", Type: FuzzType{Kind: KindOptional, Inner: &listSelf}},
		}}},
		RootClass: "Tree",
	})
	if !schema.HasSelfRef {
		t.Fatalf("expected HasSelfRef=true, got false")
	}
	got, err := LowerToBamlSource(schema, "C09")
	if err != nil {
		t.Fatalf("LowerToBamlSource self-ref: %v", err)
	}
	wantSubs := []string{
		"class Tree_C09 {",
		"  value string",
		"  children ((Tree_C09)[])?",
		"function FuzzFn_C09(input: string) -> Tree_C09 {",
	}
	for _, sub := range wantSubs {
		if !strings.Contains(got.Source, sub) {
			t.Errorf("missing %q in source:\n%s", sub, got.Source)
		}
	}
}

// TestLowerToBamlSource_MutualCycle pins that static lowering accepts
// the dynamic-gated A->B->A shape — only the dynamic TypeBuilder has
// the upstream cgo crash for mutual cycles.
func TestLowerToBamlSource_MutualCycle(t *testing.T) {
	schema := AnalyzeGraph(FuzzSchema{
		Classes: []FuzzClass{
			{Name: "A", Properties: []FuzzProperty{
				{Name: "name", Type: FuzzType{Kind: KindString}},
				{Name: "b", Type: FuzzType{Kind: KindOptional, Inner: &FuzzType{Kind: KindClassRef, Ref: "B"}}},
			}},
			{Name: "B", Properties: []FuzzProperty{
				{Name: "kind", Type: FuzzType{Kind: KindString}},
				{Name: "a", Type: FuzzType{Kind: KindOptional, Inner: &FuzzType{Kind: KindClassRef, Ref: "A"}}},
			}},
		},
		RootClass: "A",
	})
	if !schema.HasMutualCycle {
		t.Fatalf("expected HasMutualCycle=true")
	}
	got, err := LowerToBamlSource(schema, "C10")
	if err != nil {
		t.Fatalf("LowerToBamlSource mutual-cycle: %v", err)
	}
	wantSubs := []string{
		"class A_C10 {",
		"class B_C10 {",
		"  b (B_C10)?",
		"  a (A_C10)?",
		"function FuzzFn_C10(input: string) -> A_C10 {",
	}
	for _, sub := range wantSubs {
		if !strings.Contains(got.Source, sub) {
			t.Errorf("missing %q in source:\n%s", sub, got.Source)
		}
	}
}

// TestLowerToBamlSource_NullRequired covers the edge case the scope
// pins: a class with KindNull as a required (non-optional) field type
// must lower to `null` directly.
func TestLowerToBamlSource_NullRequired(t *testing.T) {
	schema := AnalyzeGraph(FuzzSchema{
		Classes: []FuzzClass{{Name: "Root", Properties: []FuzzProperty{
			{Name: "nothing", Type: FuzzType{Kind: KindNull}},
		}}},
		RootClass: "Root",
	})
	got, err := LowerToBamlSource(schema, "C11")
	if err != nil {
		t.Fatalf("LowerToBamlSource null-required: %v", err)
	}
	if !strings.Contains(got.Source, "  nothing null") {
		t.Errorf("KindNull spelling missing:\n%s", got.Source)
	}
}

// TestLowerToBamlSource_DeterministicOutput pins that lowering the
// same schema twice yields byte-identical source. The replay/failure
// envelope contract depends on this.
func TestLowerToBamlSource_DeterministicOutput(t *testing.T) {
	schema := AnalyzeGraph(FuzzSchema{
		Classes: []FuzzClass{
			{Name: "Root", Properties: []FuzzProperty{
				{Name: "a", Type: FuzzType{Kind: KindString}},
				{Name: "b", Type: FuzzType{Kind: KindOptional, Inner: &FuzzType{Kind: KindClassRef, Ref: "Inner"}}},
			}},
			{Name: "Inner", Properties: []FuzzProperty{
				{Name: "x", Type: FuzzType{Kind: KindInt}},
			}},
		},
		Enums: []FuzzEnum{{Name: "E", Values: []string{"X", "Y"}}},
		RootClass: "Root",
	})
	a, err := LowerToBamlSource(schema, "CDET")
	if err != nil {
		t.Fatalf("first lower: %v", err)
	}
	b, err := LowerToBamlSource(schema, "CDET")
	if err != nil {
		t.Fatalf("second lower: %v", err)
	}
	if a.Source != b.Source {
		t.Errorf("non-deterministic output:\n--a--\n%s\n--b--\n%s", a.Source, b.Source)
	}
}

func TestLowerToBamlSource_InvalidCaseID(t *testing.T) {
	schema := AnalyzeGraph(FuzzSchema{
		Classes:   []FuzzClass{{Name: "Root", Properties: []FuzzProperty{{Name: "x", Type: FuzzType{Kind: KindString}}}}},
		RootClass: "Root",
	})
	tests := []string{"", "0bad", "has-dash", "has space", "has.dot", "1leading"}
	for _, id := range tests {
		_, err := LowerToBamlSource(schema, id)
		if err == nil {
			t.Errorf("expected error for caseID %q, got nil", id)
		}
	}
}

func TestLowerToBamlSource_MissingRoot(t *testing.T) {
	schema := FuzzSchema{
		Classes:   []FuzzClass{{Name: "Other"}},
		RootClass: "Missing",
	}
	_, err := LowerToBamlSource(schema, "C00")
	if err == nil {
		t.Fatalf("expected error for missing root class")
	}
	if !strings.Contains(err.Error(), "root class") {
		t.Errorf("error should mention root class, got %v", err)
	}
}

func TestLowerToBamlSource_UnknownClassRef(t *testing.T) {
	schema := FuzzSchema{
		Classes: []FuzzClass{{Name: "Root", Properties: []FuzzProperty{
			{Name: "missing_ref", Type: FuzzType{Kind: KindClassRef, Ref: "Ghost"}},
		}}},
		RootClass: "Root",
	}
	_, err := LowerToBamlSource(schema, "C12")
	if err == nil {
		t.Fatalf("expected error for unknown class ref")
	}
}
