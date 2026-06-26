package schema

import (
	"strings"
	"testing"
)

// TestBuildFailures covers golden construction failures: unresolved
// references, reserved/ambiguous names, and malformed type specs surface
// as errors from FromDynamicOutputSchema (construction validates as it
// lowers).
func TestBuildFailures(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantSub string
	}{
		{
			name:    "unresolved ref",
			raw:     `{"properties": {"x": {"ref": "Missing"}}}`,
			wantSub: `unresolved reference "Missing"`,
		},
		{
			name:    "reserved synthetic class name",
			raw:     `{"classes": {"Baml_Rest_DynamicOutput": {"properties": {"a": {"type": "string"}}}}, "properties": {"a": {"type": "string"}}}`,
			wantSub: "reserved by baml-rest",
		},
		{
			name:    "class and enum same name",
			raw:     `{"classes": {"Foo": {"properties": {"a": {"type": "string"}}}}, "enums": {"Foo": {"values": [{"name": "A"}]}}, "properties": {"f": {"ref": "Foo"}}}`,
			wantSub: "both a class and an enum",
		},
		{
			name:    "both type and ref",
			raw:     `{"properties": {"x": {"type": "string", "ref": "Foo"}}}`,
			wantSub: "cannot have both 'type' and 'ref'",
		},
		{
			name:    "unknown type",
			raw:     `{"properties": {"x": {"type": "bogus"}}}`,
			wantSub: `unknown type "bogus"`,
		},
		{
			name:    "list missing items",
			raw:     `{"properties": {"x": {"type": "list"}}}`,
			wantSub: "'list' requires 'items'",
		},
		{
			name:    "literal_int with non-integer",
			raw:     `{"properties": {"x": {"type": "literal_int", "value": "nope"}}}`,
			wantSub: "'literal_int' requires an integer",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := FromDynamicOutputSchema(mustSchema(t, tt.raw), BuildOptions{})
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tt.wantSub)
			}
			if !strings.Contains(err.Error(), tt.wantSub) {
				t.Fatalf("error = %q, want substring %q", err.Error(), tt.wantSub)
			}
		})
	}
}

// TestAmbiguousAliasFails covers two fields rendering to the same name
// within a class — the rendered-name index rejects it during
// construction's RebuildIndexes.
func TestAmbiguousAliasFails(t *testing.T) {
	_, err := FromDynamicOutputSchema(mustSchema(t, `{
		"properties": {
			"a": {"type": "string", "alias": "shared"},
			"b": {"type": "string", "alias": "shared"}
		}
	}`), BuildOptions{})
	if err == nil || !strings.Contains(err.Error(), "duplicate rendered field name") {
		t.Fatalf("expected duplicate rendered field name error, got %v", err)
	}
}

func TestDuplicateClassModeKeyFails(t *testing.T) {
	b := &Bundle{
		Target: Type{Kind: TypeClass, Name: "A", Mode: NonStreaming},
		Classes: []ClassDef{
			{Name: Name{Name: "A"}, Mode: NonStreaming, Fields: []ClassField{{Name: Name{Name: "x"}, Type: Type{Kind: TypePrimitive, Primitive: PrimitiveString}}}},
			{Name: Name{Name: "A"}, Mode: NonStreaming, Fields: []ClassField{{Name: Name{Name: "y"}, Type: Type{Kind: TypePrimitive, Primitive: PrimitiveString}}}},
		},
	}
	if err := b.Validate(); err == nil || !strings.Contains(err.Error(), "duplicate class") {
		t.Fatalf("expected duplicate class error, got %v", err)
	}
}

func TestDuplicateEnumNameFails(t *testing.T) {
	b := &Bundle{
		Target: Type{Kind: TypeClass, Name: "A", Mode: NonStreaming},
		Classes: []ClassDef{
			{Name: Name{Name: "A"}, Mode: NonStreaming, Fields: []ClassField{{Name: Name{Name: "x"}, Type: Type{Kind: TypeEnum, Name: "E"}}}},
		},
		Enums: []EnumDef{
			{Name: Name{Name: "E"}, Values: []EnumValue{{Name: Name{Name: "A"}}}},
			{Name: Name{Name: "E"}, Values: []EnumValue{{Name: Name{Name: "B"}}}},
		},
	}
	if err := b.Validate(); err == nil || !strings.Contains(err.Error(), "duplicate enum name") {
		t.Fatalf("expected duplicate enum name error, got %v", err)
	}
}

func TestUnresolvedRefInHandBuiltBundle(t *testing.T) {
	b := &Bundle{
		Target: Type{Kind: TypeClass, Name: "Root", Mode: NonStreaming},
		Classes: []ClassDef{
			{Name: Name{Name: "Root"}, Mode: NonStreaming, Fields: []ClassField{
				{Name: Name{Name: "x"}, Type: Type{Kind: TypeClass, Name: "Ghost", Mode: NonStreaming}},
			}},
		},
	}
	if err := b.Validate(); err == nil || !strings.Contains(err.Error(), `unresolved class reference "Ghost"`) {
		t.Fatalf("expected unresolved class reference error, got %v", err)
	}
}

func TestUnionNullVariantRejected(t *testing.T) {
	b := &Bundle{
		Target: Type{Kind: TypeClass, Name: "Root", Mode: NonStreaming},
		Classes: []ClassDef{
			{Name: Name{Name: "Root"}, Mode: NonStreaming, Fields: []ClassField{
				{Name: Name{Name: "x"}, Type: Type{Kind: TypeUnion, Union: &UnionType{Variants: []Type{
					{Kind: TypePrimitive, Primitive: PrimitiveString},
					{Kind: TypePrimitive, Primitive: PrimitiveNull},
				}}}},
			}},
		},
	}
	if err := b.Validate(); err == nil || !strings.Contains(err.Error(), "null primitive") {
		t.Fatalf("expected null primitive union error, got %v", err)
	}
}

func TestInvalidMapKeyRejected(t *testing.T) {
	b := &Bundle{
		Target: Type{Kind: TypeClass, Name: "Root", Mode: NonStreaming},
		Classes: []ClassDef{
			{Name: Name{Name: "Root"}, Mode: NonStreaming, Fields: []ClassField{
				{Name: Name{Name: "m"}, Type: Type{
					Kind:  TypeMap,
					Key:   &Type{Kind: TypePrimitive, Primitive: PrimitiveInt},
					Value: &Type{Kind: TypePrimitive, Primitive: PrimitiveString},
				}},
			}},
		},
	}
	if err := b.Validate(); err == nil || !strings.Contains(err.Error(), "map key must be") {
		t.Fatalf("expected map key error, got %v", err)
	}
}

func TestValidMapKeysAccepted(t *testing.T) {
	keys := []Type{
		{Kind: TypePrimitive, Primitive: PrimitiveString},
		{Kind: TypeEnum, Name: "E"},
		{Kind: TypeLiteral, Literal: &LiteralValue{Kind: LiteralString, String: "k"}},
		{Kind: TypeUnion, Union: &UnionType{Variants: []Type{
			{Kind: TypePrimitive, Primitive: PrimitiveString},
			{Kind: TypeEnum, Name: "E"},
		}}},
	}
	for i, k := range keys {
		key := k
		b := &Bundle{
			Target: Type{Kind: TypeClass, Name: "Root", Mode: NonStreaming},
			Enums:  []EnumDef{{Name: Name{Name: "E"}, Values: []EnumValue{{Name: Name{Name: "A"}}}}},
			Classes: []ClassDef{
				{Name: Name{Name: "Root"}, Mode: NonStreaming, Fields: []ClassField{
					{Name: Name{Name: "m"}, Type: Type{Kind: TypeMap, Key: &key, Value: &Type{Kind: TypePrimitive, Primitive: PrimitiveString}}},
				}},
			},
		}
		if err := b.Validate(); err != nil {
			t.Errorf("key[%d] %+v: unexpected error %v", i, k, err)
		}
	}
}

// TestOutputProfileRejectsUnsupportedKinds covers the output-usable
// profile: tuple, arrow, top, and media pass structural Validate but are
// rejected by ValidateOutput, matching where BAML's renderer/parser
// error or panic.
func TestOutputProfileRejectsUnsupportedKinds(t *testing.T) {
	tests := []struct {
		name    string
		field   Type
		wantSub string
	}{
		{"tuple", Type{Kind: TypeTuple, Items: []Type{{Kind: TypePrimitive, Primitive: PrimitiveString}}}, "tuple is not usable"},
		{"arrow", Type{Kind: TypeArrow, Arrow: &ArrowType{Return: Type{Kind: TypePrimitive, Primitive: PrimitiveString}}}, "arrow is not usable"},
		{"top", Type{Kind: TypeTop}, "top is not usable"},
		{"media", Type{Kind: TypePrimitive, Primitive: PrimitiveMedia}, "media is not usable"},
		{"nested media in list", Type{Kind: TypeList, Elem: &Type{Kind: TypePrimitive, Primitive: PrimitiveMedia}}, "media is not usable"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			field := tt.field
			b := &Bundle{
				Target: Type{Kind: TypeClass, Name: "Root", Mode: NonStreaming},
				Classes: []ClassDef{
					{Name: Name{Name: "Root"}, Mode: NonStreaming, Fields: []ClassField{
						{Name: Name{Name: "f"}, Type: field},
					}},
				},
			}
			if err := b.Validate(); err != nil {
				t.Fatalf("structural Validate should pass, got %v", err)
			}
			if err := b.ValidateOutput(); err == nil || !strings.Contains(err.Error(), tt.wantSub) {
				t.Fatalf("ValidateOutput error = %v, want substring %q", err, tt.wantSub)
			}
		})
	}
}

// TestRecursiveAliasValidates exercises a structural recursive alias and
// recursive-class set at the model level (the dynamic input surface does
// not yet produce these), confirming traversal resolves their references
// and terminates.
func TestRecursiveAliasValidates(t *testing.T) {
	b := &Bundle{
		Target: Type{Kind: TypeRecursiveAlias, Name: "JSON"},
		StructuralRecursiveAliases: []RecursiveAliasDef{
			{Name: "JSON", Target: Type{Kind: TypeUnion, Union: &UnionType{Variants: []Type{
				{Kind: TypePrimitive, Primitive: PrimitiveString},
				{Kind: TypeList, Elem: &Type{Kind: TypeRecursiveAlias, Name: "JSON"}},
				{Kind: TypeMap, Key: &Type{Kind: TypePrimitive, Primitive: PrimitiveString}, Value: &Type{Kind: TypeRecursiveAlias, Name: "JSON"}},
			}}}},
		},
	}
	if err := b.Validate(); err != nil {
		t.Fatalf("recursive alias bundle should validate, got %v", err)
	}
	if err := b.ValidateOutput(); err != nil {
		t.Fatalf("recursive alias bundle should pass output profile, got %v", err)
	}
}

func TestRecursiveClassSetResolves(t *testing.T) {
	b := &Bundle{
		Target: Type{Kind: TypeClass, Name: "Node", Mode: NonStreaming},
		Classes: []ClassDef{
			{Name: Name{Name: "Node"}, Mode: NonStreaming, Fields: []ClassField{
				{Name: Name{Name: "next"}, Type: makeNullable(Type{Kind: TypeClass, Name: "Node", Mode: NonStreaming})},
			}},
		},
		RecursiveClasses: []string{"Node"},
	}
	if err := b.Validate(); err != nil {
		t.Fatalf("recursive class bundle should validate, got %v", err)
	}

	b.RecursiveClasses = []string{"Ghost"}
	if err := b.Validate(); err == nil || !strings.Contains(err.Error(), `unresolved class "Ghost"`) {
		t.Fatalf("expected unresolved recursive class error, got %v", err)
	}
}
