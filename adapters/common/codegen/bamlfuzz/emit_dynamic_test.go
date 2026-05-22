package bamlfuzz

import (
	"errors"
	"strings"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
)

func TestLowerToDynamicSchema_RejectsSelfRef(t *testing.T) {
	cls := FuzzClass{Name: "FuzzClass0", Properties: []FuzzProperty{
		{Name: "Fuzz_field_0", Type: FuzzType{Kind: KindOptional, Inner: &FuzzType{Kind: KindClassRef, Ref: "FuzzClass0"}}},
	}}
	schema := AnalyzeGraph(FuzzSchema{
		Classes:   []FuzzClass{cls},
		RootClass: "FuzzClass0",
	})
	if !schema.HasSelfRef {
		t.Fatalf("expected self-ref schema, got HasSelfRef=false")
	}

	_, err := LowerToDynamicSchema(schema)
	if !errors.Is(err, ErrDynamicSelfRefUnsupported) {
		t.Fatalf("expected ErrDynamicSelfRefUnsupported, got %v", err)
	}
	if !errors.Is(err, ErrDynamicSchemaUnsupported) {
		t.Errorf("ErrDynamicSelfRefUnsupported should wrap ErrDynamicSchemaUnsupported, got %v", err)
	}
}

// TestLowerToDynamicSchema_RecomputesGraphFlags pins the M1 invariant:
// the lowering must NOT trust the stamped Has*/Requires* booleans on
// the input — those are documented as non-authoritative, and a
// hand-edited corpus or replay artifact could carry stale values.
// LowerToDynamicSchema runs AnalyzeGraph internally so a self-ref or
// mutual-cycle schema with stale `has_self_ref: false` is still caught.
func TestLowerToDynamicSchema_RecomputesGraphFlags(t *testing.T) {
	t.Run("stale self-ref flag", func(t *testing.T) {
		cls := FuzzClass{Name: "Stale", Properties: []FuzzProperty{
			{Name: "self", Type: FuzzType{Kind: KindOptional, Inner: &FuzzType{Kind: KindClassRef, Ref: "Stale"}}},
		}}
		// Construct WITHOUT calling AnalyzeGraph and explicitly stamp
		// the flags to false to model a malicious/stale replay file.
		schema := FuzzSchema{
			Classes:             []FuzzClass{cls},
			RootClass:           "Stale",
			HasSelfRef:          false,
			RequiresDynamicSkip: false,
		}
		_, err := LowerToDynamicSchema(schema)
		if !errors.Is(err, ErrDynamicSelfRefUnsupported) {
			t.Fatalf("expected ErrDynamicSelfRefUnsupported via recomputation, got %v", err)
		}
	})

	t.Run("stale mutual-cycle flag", func(t *testing.T) {
		schema := FuzzSchema{
			Classes: []FuzzClass{
				{Name: "A", Properties: []FuzzProperty{
					{Name: "b", Type: FuzzType{Kind: KindOptional, Inner: &FuzzType{Kind: KindClassRef, Ref: "B"}}},
				}},
				{Name: "B", Properties: []FuzzProperty{
					{Name: "a", Type: FuzzType{Kind: KindOptional, Inner: &FuzzType{Kind: KindClassRef, Ref: "A"}}},
				}},
			},
			RootClass:      "A",
			HasMutualCycle: false,
		}
		_, err := LowerToDynamicSchema(schema)
		if !errors.Is(err, ErrDynamicMutualCycleUnsupported) {
			t.Fatalf("expected ErrDynamicMutualCycleUnsupported via recomputation, got %v", err)
		}
		if !errors.Is(err, ErrDynamicSchemaUnsupported) {
			t.Errorf("ErrDynamicMutualCycleUnsupported should wrap ErrDynamicSchemaUnsupported, got %v", err)
		}
	})
}

func TestLowerToDynamicSchema_PreservesOrder(t *testing.T) {
	schema := AnalyzeGraph(FuzzSchema{
		Classes: []FuzzClass{
			{
				Name: "Root",
				Properties: []FuzzProperty{
					{Name: "zeta", Type: FuzzType{Kind: KindString}},
					{Name: "alpha", Type: FuzzType{Kind: KindInt}},
					{Name: "mu", Type: FuzzType{Kind: KindClassRef, Ref: "Inner"}},
				},
			},
			{
				Name: "Inner",
				Properties: []FuzzProperty{
					{Name: "third", Type: FuzzType{Kind: KindBool}},
					{Name: "first", Type: FuzzType{Kind: KindFloat}},
					{Name: "second", Type: FuzzType{Kind: KindEnumRef, Ref: "Status"}},
				},
			},
		},
		Enums:     []FuzzEnum{{Name: "Status", Values: []string{"ON", "OFF"}}},
		RootClass: "Root",
	})

	out, err := LowerToDynamicSchema(schema)
	if err != nil {
		t.Fatalf("LowerToDynamicSchema: %v", err)
	}

	wantRoot := []string{"zeta", "alpha", "mu"}
	if got := out.Properties.Keys(); !equalStrings(got, wantRoot) {
		t.Errorf("root property order: got %v want %v", got, wantRoot)
	}

	innerCls, ok := out.Classes.Get("Inner")
	if !ok {
		t.Fatalf("Inner class missing from lowered schema")
	}
	wantInner := []string{"third", "first", "second"}
	if got := innerCls.Properties.Keys(); !equalStrings(got, wantInner) {
		t.Errorf("Inner property order: got %v want %v", got, wantInner)
	}

	if _, ok := out.Enums.Get("Status"); !ok {
		t.Errorf("Status enum missing from lowered schema")
	}
}

func TestLowerToDynamicSchema_AllKinds(t *testing.T) {
	schema := AnalyzeGraph(FuzzSchema{
		Classes: []FuzzClass{
			{
				Name: "Root",
				Properties: []FuzzProperty{
					{Name: "s", Type: FuzzType{Kind: KindString}},
					{Name: "i", Type: FuzzType{Kind: KindInt}},
					{Name: "f", Type: FuzzType{Kind: KindFloat}},
					{Name: "b", Type: FuzzType{Kind: KindBool}},
					{Name: "n", Type: FuzzType{Kind: KindNull}},
					{Name: "ls", Type: FuzzType{Kind: KindLiteral, Literal: &FuzzLiteral{Kind: LiteralString, String: "hi"}}},
					{Name: "li", Type: FuzzType{Kind: KindLiteral, Literal: &FuzzLiteral{Kind: LiteralInt, Int: 42}}},
					{Name: "lb", Type: FuzzType{Kind: KindLiteral, Literal: &FuzzLiteral{Kind: LiteralBool, Bool: true}}},
					{Name: "opt", Type: FuzzType{Kind: KindOptional, Inner: &FuzzType{Kind: KindString}}},
					{Name: "lst", Type: FuzzType{Kind: KindList, Inner: &FuzzType{Kind: KindString}}},
					{Name: "mp", Type: FuzzType{
						Kind:  KindMap,
						Key:   &FuzzType{Kind: KindString},
						Inner: &FuzzType{Kind: KindInt},
					}},
					{Name: "cref", Type: FuzzType{Kind: KindClassRef, Ref: "Other"}},
					{Name: "eref", Type: FuzzType{Kind: KindEnumRef, Ref: "E"}},
				},
			},
			{Name: "Other", Properties: []FuzzProperty{{Name: "x", Type: FuzzType{Kind: KindString}}}},
		},
		Enums:     []FuzzEnum{{Name: "E", Values: []string{"A"}}},
		RootClass: "Root",
	})

	out, err := LowerToDynamicSchema(schema)
	if err != nil {
		t.Fatalf("LowerToDynamicSchema: %v", err)
	}

	checks := []struct {
		name string
		want bamlutils.DynamicProperty
	}{
		{"s", bamlutils.DynamicProperty{Type: "string"}},
		{"i", bamlutils.DynamicProperty{Type: "int"}},
		{"f", bamlutils.DynamicProperty{Type: "float"}},
		{"b", bamlutils.DynamicProperty{Type: "bool"}},
		{"n", bamlutils.DynamicProperty{Type: "null"}},
		{"ls", bamlutils.DynamicProperty{Type: "literal_string", Value: "hi"}},
		{"li", bamlutils.DynamicProperty{Type: "literal_int", Value: int64(42)}},
		{"lb", bamlutils.DynamicProperty{Type: "literal_bool", Value: true}},
	}
	for _, c := range checks {
		got, ok := out.Properties.Get(c.name)
		if !ok {
			t.Errorf("property %q missing", c.name)
			continue
		}
		if got.Type != c.want.Type || got.Value != c.want.Value {
			t.Errorf("property %q: got type=%q value=%v, want type=%q value=%v",
				c.name, got.Type, got.Value, c.want.Type, c.want.Value)
		}
	}

	opt, _ := out.Properties.Get("opt")
	if opt.Type != "optional" || opt.Inner == nil || opt.Inner.Type != "string" {
		t.Errorf("optional inner: got %+v", opt)
	}
	lst, _ := out.Properties.Get("lst")
	if lst.Type != "list" || lst.Items == nil || lst.Items.Type != "string" {
		t.Errorf("list items: got %+v", lst)
	}
	mp, _ := out.Properties.Get("mp")
	if mp.Type != "map" || mp.Keys == nil || mp.Keys.Type != "string" || mp.Values == nil || mp.Values.Type != "int" {
		t.Errorf("map keys/values: got %+v", mp)
	}
	cref, _ := out.Properties.Get("cref")
	if cref.Type != "" || cref.Ref != "Other" {
		t.Errorf("class ref: got type=%q ref=%q, want type=\"\" ref=Other", cref.Type, cref.Ref)
	}
	eref, _ := out.Properties.Get("eref")
	if eref.Type != "" || eref.Ref != "E" {
		t.Errorf("enum ref: got type=%q ref=%q, want type=\"\" ref=E", eref.Type, eref.Ref)
	}
}

func TestLowerToDynamicSchema_ValidatesAgainstBamlutils(t *testing.T) {
	schema := AnalyzeGraph(FuzzSchema{
		Classes: []FuzzClass{
			{Name: "Root", Properties: []FuzzProperty{
				{Name: "name", Type: FuzzType{Kind: KindString}},
				{Name: "ref", Type: FuzzType{Kind: KindClassRef, Ref: "Inner"}},
				{Name: "status", Type: FuzzType{Kind: KindEnumRef, Ref: "Status"}},
				{Name: "tags", Type: FuzzType{Kind: KindList, Inner: &FuzzType{Kind: KindString}}},
				{Name: "score", Type: FuzzType{Kind: KindOptional, Inner: &FuzzType{Kind: KindInt}}},
				{Name: "meta", Type: FuzzType{
					Kind:  KindMap,
					Key:   &FuzzType{Kind: KindString},
					Inner: &FuzzType{Kind: KindString},
				}},
			}},
			{Name: "Inner", Properties: []FuzzProperty{
				{Name: "field", Type: FuzzType{Kind: KindString}},
			}},
		},
		Enums:     []FuzzEnum{{Name: "Status", Values: []string{"ON", "OFF"}}},
		RootClass: "Root",
	})

	out, err := LowerToDynamicSchema(schema)
	if err != nil {
		t.Fatalf("LowerToDynamicSchema: %v", err)
	}
	dt := &bamlutils.DynamicTypes{Classes: out.Classes, Enums: out.Enums}
	if err := dt.Validate(); err != nil {
		t.Fatalf("lowered schema failed bamlutils.Validate: %v", err)
	}
}

func TestLowerToDynamicSchema_MissingRootClass(t *testing.T) {
	_, err := LowerToDynamicSchema(FuzzSchema{RootClass: "Missing"})
	if err == nil {
		t.Fatal("expected error for missing root class, got nil")
	}
	if !strings.Contains(err.Error(), "Missing") {
		t.Errorf("error %q does not mention missing class name", err)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
