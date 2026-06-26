package schema

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
)

// mustSchema decodes a DynamicOutputSchema from the wire JSON, exercising
// the real input surface (UnmarshalJSON preserves order and rejects
// duplicate keys) rather than hand-building OrderedMaps.
func mustSchema(t *testing.T, raw string) *bamlutils.DynamicOutputSchema {
	t.Helper()
	var s bamlutils.DynamicOutputSchema
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		t.Fatalf("decode schema fixture: %v", err)
	}
	return &s
}

func mustBuild(t *testing.T, raw string) *Bundle {
	t.Helper()
	b, err := FromDynamicOutputSchema(mustSchema(t, raw), BuildOptions{})
	if err != nil {
		t.Fatalf("FromDynamicOutputSchema: %v", err)
	}
	if err := b.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	return b
}

func TestBuildPrimitivesAndComposites(t *testing.T) {
	b := mustBuild(t, `{
		"properties": {
			"name":   {"type": "string"},
			"age":    {"type": "int", "description": "years"},
			"score":  {"type": "float"},
			"active": {"type": "bool"},
			"void":   {"type": "null"},
			"tags":   {"type": "list", "items": {"type": "string"}},
			"meta":   {"type": "map", "keys": {"type": "string"}, "values": {"type": "int"}},
			"either": {"type": "union", "oneOf": [{"type": "string"}, {"type": "int"}]},
			"maybe":  {"type": "optional", "inner": {"type": "string"}},
			"label":  {"type": "string", "alias": "Label"}
		}
	}`)

	if b.Target.Kind != TypeClass || b.Target.Name != dynamicOutputClassName {
		t.Fatalf("target = %+v, want class %q", b.Target, dynamicOutputClassName)
	}
	cls, ok := b.FindClass(dynamicOutputClassName, NonStreaming)
	if !ok {
		t.Fatal("synthetic class missing")
	}

	// Field order must match declaration order.
	wantOrder := []string{"name", "age", "score", "active", "void", "tags", "meta", "either", "maybe", "label"}
	if len(cls.Fields) != len(wantOrder) {
		t.Fatalf("field count = %d, want %d", len(cls.Fields), len(wantOrder))
	}
	for i, name := range wantOrder {
		if cls.Fields[i].Name.Name != name {
			t.Errorf("field[%d] = %q, want %q", i, cls.Fields[i].Name.Name, name)
		}
	}

	// optional must lower to a nullable union, NOT an optional kind.
	maybe, _ := cls.Field("maybe")
	if maybe.Type.Kind != TypeUnion || maybe.Type.Union == nil || !maybe.Type.Union.Nullable {
		t.Errorf("optional did not lower to nullable union: %+v", maybe.Type)
	}
	if len(maybe.Type.Union.Variants) != 1 || maybe.Type.Union.Variants[0].Primitive != PrimitiveString {
		t.Errorf("nullable union variants = %+v, want [string]", maybe.Type.Union.Variants)
	}

	// Description and alias lower onto the field, not the type.
	age, _ := cls.Field("age")
	if age.Description == nil || *age.Description != "years" {
		t.Errorf("age description = %v, want years", age.Description)
	}
	label, _ := cls.Field("label")
	if label.Name.RenderedName() != "Label" {
		t.Errorf("label rendered name = %q, want Label", label.Name.RenderedName())
	}
}

func TestBuildLiterals(t *testing.T) {
	b := mustBuild(t, `{
		"properties": {
			"s": {"type": "literal_string", "value": "hello"},
			"i": {"type": "literal_int", "value": 42},
			"b": {"type": "literal_bool", "value": true}
		}
	}`)
	cls, _ := b.FindClass(dynamicOutputClassName, NonStreaming)

	s, _ := cls.Field("s")
	if s.Type.Kind != TypeLiteral || s.Type.Literal.Kind != LiteralString || s.Type.Literal.String != "hello" {
		t.Errorf("literal string = %+v", s.Type)
	}
	i, _ := cls.Field("i")
	if i.Type.Kind != TypeLiteral || i.Type.Literal.Kind != LiteralInt || i.Type.Literal.Int != 42 {
		t.Errorf("literal int = %+v", i.Type)
	}
	bl, _ := cls.Field("b")
	if bl.Type.Kind != TypeLiteral || bl.Type.Literal.Kind != LiteralBool || !bl.Type.Literal.Bool {
		t.Errorf("literal bool = %+v", bl.Type)
	}
}

func TestBuildRefsClassesEnums(t *testing.T) {
	b := mustBuild(t, `{
		"classes": {
			"Address": {
				"description": "a postal address",
				"properties": {
					"street": {"type": "string"},
					"zip": {"type": "optional", "inner": {"type": "string"}}
				}
			}
		},
		"enums": {
			"Status": {
				"alias": "State",
				"values": [
					{"name": "ACTIVE", "alias": "active", "description": "is active"},
					{"name": "INACTIVE", "skip": true}
				]
			}
		},
		"properties": {
			"address": {"ref": "Address"},
			"status": {"ref": "Status"}
		}
	}`)

	cls, _ := b.FindClass(dynamicOutputClassName, NonStreaming)
	addr, _ := cls.Field("address")
	if addr.Type.Kind != TypeClass || addr.Type.Name != "Address" || addr.Type.Mode != NonStreaming || !addr.Type.Dynamic {
		t.Errorf("address ref = %+v, want dynamic class Address", addr.Type)
	}
	status, _ := cls.Field("status")
	if status.Type.Kind != TypeEnum || status.Type.Name != "Status" || !status.Type.Dynamic {
		t.Errorf("status ref = %+v, want dynamic enum Status", status.Type)
	}

	// Referenced definitions resolve, making the bundle self-contained.
	if _, ok := b.FindClass("Address", NonStreaming); !ok {
		t.Error("Address class not in bundle")
	}
	enum, ok := b.FindEnum("Status")
	if !ok {
		t.Fatal("Status enum not in bundle")
	}
	if enum.Name.RenderedName() != "State" {
		t.Errorf("enum alias = %q, want State", enum.Name.RenderedName())
	}
	// Skipped values (INACTIVE) are dropped, mirroring BAML: only the
	// non-skipped ACTIVE survives.
	if len(enum.Values) != 1 {
		t.Fatalf("enum values = %d, want 1 (skipped value dropped)", len(enum.Values))
	}
	if enum.Values[0].Name.Name != "ACTIVE" {
		t.Errorf("surviving value = %q, want ACTIVE", enum.Values[0].Name.Name)
	}
}

// TestBuildSkipDropsAliasedAndDescribedValue is a regression for R1: a
// skipped value is dropped even when it carries an alias and description,
// so a hidden category never reaches OutputFormatContent.
func TestBuildSkipDropsAliasedAndDescribedValue(t *testing.T) {
	b := mustBuild(t, `{
		"enums": {
			"E": {"values": [
				{"name": "KEEP"},
				{"name": "HIDE", "alias": "hidden", "description": "should vanish", "skip": true}
			]}
		},
		"properties": {"e": {"ref": "E"}}
	}`)
	enum, _ := b.FindEnum("E")
	if len(enum.Values) != 1 || enum.Values[0].Name.Name != "KEEP" {
		t.Fatalf("values = %+v, want only KEEP", enum.Values)
	}
	if _, ok := enum.Value("HIDE"); ok {
		t.Error("skipped HIDE must not be indexed")
	}
	if _, ok := enum.ValueByRenderedName("hidden"); ok {
		t.Error("skipped value's alias must not be indexed")
	}
}

// TestBuildEnumNilDefinitionFails covers C3: a null enum definition is
// malformed input and construction fails closed rather than inventing an
// empty enum.
func TestBuildEnumNilDefinitionFails(t *testing.T) {
	_, err := FromDynamicOutputSchema(mustSchema(t, `{
		"enums": {"E": null},
		"properties": {"x": {"type": "string"}}
	}`), BuildOptions{})
	if err == nil || !strings.Contains(err.Error(), `enum "E": nil definition`) {
		t.Fatalf("expected nil enum definition error, got %v", err)
	}
}

// TestBuildEnumNilValueFails covers the nil-value follow-up: a null entry
// in an enum's values is malformed and fails closed rather than silently
// shrinking the enum.
func TestBuildEnumNilValueFails(t *testing.T) {
	_, err := FromDynamicOutputSchema(mustSchema(t, `{
		"enums": {"E": {"values": [{"name": "A"}, null]}},
		"properties": {"e": {"ref": "E"}}
	}`), BuildOptions{})
	if err == nil || !strings.Contains(err.Error(), `enum "E": value at index 1 is nil`) {
		t.Fatalf("expected nil enum value error, got %v", err)
	}
}

func TestBuildSyntheticClassIsFirst(t *testing.T) {
	b := mustBuild(t, `{
		"classes": {"Address": {"properties": {"city": {"type": "string"}}}},
		"properties": {"address": {"ref": "Address"}}
	}`)
	if b.Classes[0].Name.Name != dynamicOutputClassName {
		t.Errorf("class[0] = %q, want synthetic first", b.Classes[0].Name.Name)
	}
	if b.Classes[1].Name.Name != "Address" {
		t.Errorf("class[1] = %q, want Address", b.Classes[1].Name.Name)
	}
}

// TestBuildRecursiveClassViaRef shows a self-referential class realised
// through the dynamic ref surface: List -> next: List. Validation must
// terminate (recursion-safe traversal).
func TestBuildRecursiveClassViaRef(t *testing.T) {
	b := mustBuild(t, `{
		"classes": {
			"Node": {
				"properties": {
					"value": {"type": "int"},
					"next": {"type": "optional", "inner": {"ref": "Node"}}
				}
			}
		},
		"properties": {"head": {"ref": "Node"}}
	}`)
	node, ok := b.FindClass("Node", NonStreaming)
	if !ok {
		t.Fatal("Node class missing")
	}
	next, _ := node.Field("next")
	if next.Type.Kind != TypeUnion || !next.Type.Union.Nullable {
		t.Fatalf("next = %+v, want nullable union", next.Type)
	}
	inner := next.Type.Union.Variants[0]
	if inner.Kind != TypeClass || inner.Name != "Node" {
		t.Errorf("recursive ref = %+v, want class Node", inner)
	}
}

func TestBuildUnionWithExplicitNullNormalises(t *testing.T) {
	b := mustBuild(t, `{
		"properties": {
			"x": {"type": "union", "oneOf": [{"type": "string"}, {"type": "null"}, {"type": "int"}]}
		}
	}`)
	cls, _ := b.FindClass(dynamicOutputClassName, NonStreaming)
	x, _ := cls.Field("x")
	if x.Type.Union == nil || !x.Type.Union.Nullable {
		t.Fatalf("explicit null variant did not set Nullable: %+v", x.Type)
	}
	for _, v := range x.Type.Union.Variants {
		if v.Kind == TypePrimitive && v.Primitive == PrimitiveNull {
			t.Error("null primitive leaked into union variants")
		}
	}
	if len(x.Type.Union.Variants) != 2 {
		t.Errorf("variants = %d, want 2 (null extracted)", len(x.Type.Union.Variants))
	}
}

// fieldType builds a single-property schema and returns the lowered type
// of property "x", for focused type-shape assertions.
func fieldType(t *testing.T, propJSON string) Type {
	t.Helper()
	b := mustBuild(t, `{"properties":{"x":`+propJSON+`}}`)
	cls, _ := b.FindClass(dynamicOutputClassName, NonStreaming)
	f, ok := cls.Field("x")
	if !ok {
		t.Fatal("field x missing")
	}
	return f.Type
}

// TestBuildUnionSimplification covers R2: dynamic unions are normalised
// exactly as BAML's TypeIR::union(...).simplify() — single-choice
// collapse, nested-union flattening with null hoisting, first-seen
// deduplication, and all-null collapse.
func TestBuildUnionSimplification(t *testing.T) {
	t.Run("singleton collapses to the choice", func(t *testing.T) {
		ty := fieldType(t, `{"type":"union","oneOf":[{"type":"string"}]}`)
		if ty.Kind != TypePrimitive || ty.Primitive != PrimitiveString {
			t.Fatalf("got %+v, want bare string (single-choice union collapses)", ty)
		}
	})
	t.Run("nested optional flattens and hoists null", func(t *testing.T) {
		ty := fieldType(t, `{"type":"union","oneOf":[{"type":"optional","inner":{"type":"string"}},{"type":"int"}]}`)
		if ty.Kind != TypeUnion || ty.Union == nil || !ty.Union.Nullable {
			t.Fatalf("got %+v, want nullable union", ty)
		}
		if len(ty.Union.Variants) != 2 {
			t.Fatalf("variants = %d, want 2 (string,int flattened out of the nested optional)", len(ty.Union.Variants))
		}
		for _, v := range ty.Union.Variants {
			if v.Kind == TypeUnion {
				t.Error("nested union was not flattened")
			}
			if isNull(v) {
				t.Error("null leaked into variants instead of Nullable")
			}
		}
	})
	t.Run("duplicates removed preserving first-seen order", func(t *testing.T) {
		ty := fieldType(t, `{"type":"union","oneOf":[{"type":"string"},{"type":"string"},{"type":"int"}]}`)
		if ty.Kind != TypeUnion || ty.Union == nil || len(ty.Union.Variants) != 2 {
			t.Fatalf("got %+v, want 2 deduped variants", ty)
		}
		if ty.Union.Variants[0].Primitive != PrimitiveString || ty.Union.Variants[1].Primitive != PrimitiveInt {
			t.Errorf("order = %+v, want [string,int]", ty.Union.Variants)
		}
	})
	t.Run("duplicate non-null collapses to single", func(t *testing.T) {
		ty := fieldType(t, `{"type":"union","oneOf":[{"type":"string"},{"type":"string"}]}`)
		if ty.Kind != TypePrimitive || ty.Primitive != PrimitiveString {
			t.Fatalf("got %+v, want bare string", ty)
		}
	})
	t.Run("all-null collapses to null primitive", func(t *testing.T) {
		ty := fieldType(t, `{"type":"union","oneOf":[{"type":"null"},{"type":"null"}]}`)
		if ty.Kind != TypePrimitive || ty.Primitive != PrimitiveNull {
			t.Fatalf("got %+v, want null primitive", ty)
		}
	})
	t.Run("single non-null plus null stays optional-of-one", func(t *testing.T) {
		ty := fieldType(t, `{"type":"union","oneOf":[{"type":"string"},{"type":"null"}]}`)
		if ty.Kind != TypeUnion || ty.Union == nil || !ty.Union.Nullable || len(ty.Union.Variants) != 1 {
			t.Fatalf("got %+v, want optional-of-one union", ty)
		}
	})
}

// TestBuildLiteralIntOutOfRange covers C4: a JSON integer literal beyond
// int64 range is rejected, not silently cast to a wrong value.
func TestBuildLiteralIntOutOfRange(t *testing.T) {
	_, err := FromDynamicOutputSchema(mustSchema(t, `{
		"properties": {"x": {"type": "literal_int", "value": 1e19}}
	}`), BuildOptions{})
	if err == nil || !strings.Contains(err.Error(), "out of int64 range") {
		t.Fatalf("expected out-of-range int64 error, got %v", err)
	}
}

// TestBuildLiteralIntInexact covers the precision follow-up: a literal_int
// that is within int64 range but beyond ±2^53 may already be a rounded
// approximation after JSON decoding, so it is rejected rather than stored
// as a wrong value. 9e18 < 2^63 (in range) but > 2^53 (inexact).
func TestBuildLiteralIntInexact(t *testing.T) {
	_, err := FromDynamicOutputSchema(mustSchema(t, `{
		"properties": {"x": {"type": "literal_int", "value": 9e18}}
	}`), BuildOptions{})
	if err == nil || !strings.Contains(err.Error(), "cannot be represented exactly") {
		t.Fatalf("expected inexact-int error, got %v", err)
	}
}

func TestBuildWithResolver(t *testing.T) {
	resolver := func(name string) (RefKind, bool) {
		if name == "ExternalEnum" {
			return RefEnum, true
		}
		return "", false
	}
	b, err := FromDynamicOutputSchema(mustSchema(t, `{
		"properties": {"e": {"ref": "ExternalEnum"}}
	}`), BuildOptions{Resolver: resolver})
	if err != nil {
		t.Fatalf("build with resolver: %v", err)
	}
	cls, _ := b.FindClass(dynamicOutputClassName, NonStreaming)
	e, _ := cls.Field("e")
	if e.Type.Kind != TypeEnum || e.Type.Name != "ExternalEnum" || e.Type.Dynamic {
		t.Errorf("resolver ref = %+v, want static enum ExternalEnum", e.Type)
	}
	// Without the external definition, the bundle is not self-contained:
	// Validate must flag the dangling reference. This documents the
	// resolver/self-contained gap rather than papering over it.
	if err := b.Validate(); err == nil {
		t.Error("Validate should reject a resolver-classified external ref absent its definition")
	}
}
