package schema

import (
	"encoding/json"
	"reflect"
	"testing"
)

func ptr(s string) *string { return &s }

// sampleBundle exercises every type kind and lookup the bundle owns, so
// the round-trip and index tests cover the whole model surface.
func sampleBundle(t *testing.T) *Bundle {
	t.Helper()
	b := &Bundle{
		Target: Type{Kind: TypeClass, Name: "Baml_Rest_DynamicOutput", Mode: NonStreaming, Dynamic: true},
		Enums: []EnumDef{
			{
				Name: Name{Name: "Status"},
				Values: []EnumValue{
					{Name: Name{Name: "ACTIVE", Alias: ptr("active")}, Description: ptr("is active")},
					{Name: Name{Name: "INACTIVE"}},
				},
			},
		},
		Classes: []ClassDef{
			{
				Name: Name{Name: "Baml_Rest_DynamicOutput"},
				Mode: NonStreaming,
				Fields: []ClassField{
					{Name: Name{Name: "name"}, Type: Type{Kind: TypePrimitive, Primitive: PrimitiveString}},
					{Name: Name{Name: "status"}, Type: Type{Kind: TypeEnum, Name: "Status", Dynamic: true}},
					{Name: Name{Name: "address"}, Type: Type{Kind: TypeClass, Name: "Address", Mode: NonStreaming, Dynamic: true}},
					{Name: Name{Name: "tags"}, Type: Type{Kind: TypeList, Elem: &Type{Kind: TypePrimitive, Primitive: PrimitiveString}}},
					{Name: Name{Name: "scores"}, Type: Type{
						Kind:  TypeMap,
						Key:   &Type{Kind: TypePrimitive, Primitive: PrimitiveString},
						Value: &Type{Kind: TypePrimitive, Primitive: PrimitiveInt},
					}},
					{Name: Name{Name: "nickname", Alias: ptr("nick")}, Type: makeOptional(Type{Kind: TypePrimitive, Primitive: PrimitiveString})},
					{Name: Name{Name: "kind"}, Type: Type{Kind: TypeUnion, Union: &UnionType{Variants: []Type{
						{Kind: TypeLiteral, Literal: &LiteralValue{Kind: LiteralString, String: "a"}},
						{Kind: TypeLiteral, Literal: &LiteralValue{Kind: LiteralInt, Int: 7}},
					}}}},
				},
			},
			{
				Name:        Name{Name: "Address"},
				Description: ptr("a postal address"),
				Mode:        NonStreaming,
				Fields: []ClassField{
					{Name: Name{Name: "city"}, Type: Type{Kind: TypePrimitive, Primitive: PrimitiveString}},
				},
			},
		},
	}
	if err := b.RebuildIndexes(); err != nil {
		t.Fatalf("RebuildIndexes: %v", err)
	}
	return b
}

func TestRoundTripPreservesOrderAndStructure(t *testing.T) {
	orig := sampleBundle(t)

	data, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got Bundle
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := got.RebuildIndexes(); err != nil {
		t.Fatalf("rebuild after unmarshal: %v", err)
	}

	// Exported state must survive the round trip exactly. Compare a fresh
	// copy of orig with cleared indexes so reflect.DeepEqual only weighs
	// the serialized fields.
	wantExported := *orig
	wantExported.enumByName = nil
	wantExported.classByKey = nil
	wantExported.aliasByName = nil
	for i := range wantExported.Enums {
		wantExported.Enums[i].valueByName = nil
		wantExported.Enums[i].valueByRenderedName = nil
	}
	for i := range wantExported.Classes {
		wantExported.Classes[i].fieldByName = nil
		wantExported.Classes[i].fieldByRenderedName = nil
	}

	gotExported := got
	gotExported.enumByName = nil
	gotExported.classByKey = nil
	gotExported.aliasByName = nil
	for i := range gotExported.Enums {
		gotExported.Enums[i].valueByName = nil
		gotExported.Enums[i].valueByRenderedName = nil
	}
	for i := range gotExported.Classes {
		gotExported.Classes[i].fieldByName = nil
		gotExported.Classes[i].fieldByRenderedName = nil
	}

	if !reflect.DeepEqual(wantExported, gotExported) {
		t.Fatalf("round trip mismatch:\n got: %+v\nwant: %+v", gotExported, wantExported)
	}

	// Re-marshalling the decoded bundle must be byte-identical: order is
	// stable through the slices.
	data2, err := json.Marshal(&got)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	if string(data) != string(data2) {
		t.Fatalf("re-marshal not byte-stable:\n first: %s\nsecond: %s", data, data2)
	}
}

func TestRebuildIndexesIsDeterministic(t *testing.T) {
	b := sampleBundle(t)

	enum1 := b.enumByName
	class1 := b.classByKey

	if err := b.RebuildIndexes(); err != nil {
		t.Fatalf("second RebuildIndexes: %v", err)
	}
	if !reflect.DeepEqual(enum1, b.enumByName) {
		t.Errorf("enumByName not deterministic:\n first: %v\nsecond: %v", enum1, b.enumByName)
	}
	if !reflect.DeepEqual(class1, b.classByKey) {
		t.Errorf("classByKey not deterministic:\n first: %v\nsecond: %v", class1, b.classByKey)
	}
}

func TestLookups(t *testing.T) {
	b := sampleBundle(t)

	if _, ok := b.FindClass("Baml_Rest_DynamicOutput", NonStreaming); !ok {
		t.Error("FindClass synthetic: not found")
	}
	if _, ok := b.FindClass("Baml_Rest_DynamicOutput", Streaming); ok {
		t.Error("FindClass should be mode-keyed: streaming variant must not exist")
	}
	if _, ok := b.FindEnum("Status"); !ok {
		t.Error("FindEnum Status: not found")
	}

	cls, ok := b.FindClass("Baml_Rest_DynamicOutput", NonStreaming)
	if !ok {
		t.Fatal("synthetic class missing")
	}
	// Rendered-name index resolves the aliased field by its alias, not its
	// canonical name.
	if _, ok := cls.FieldByRenderedName("nick"); !ok {
		t.Error("FieldByRenderedName(nick): not found")
	}
	if _, ok := cls.FieldByRenderedName("nickname"); ok {
		t.Error("FieldByRenderedName should use rendered name: canonical must miss when aliased")
	}
	if _, ok := cls.Field("nickname"); !ok {
		t.Error("Field(nickname): canonical lookup should succeed")
	}

	enum, _ := b.FindEnum("Status")
	if _, ok := enum.ValueByRenderedName("active"); !ok {
		t.Error("ValueByRenderedName(active): not found")
	}
	if _, ok := enum.Value("ACTIVE"); !ok {
		t.Error("Value(ACTIVE): canonical lookup should succeed")
	}
}

func TestRenderedName(t *testing.T) {
	if got := (Name{Name: "X"}).RenderedName(); got != "X" {
		t.Errorf("nil alias: got %q want X", got)
	}
	if got := (Name{Name: "X", Alias: ptr("y")}).RenderedName(); got != "y" {
		t.Errorf("alias: got %q want y", got)
	}
}

func TestMetaOmittedWhenZero(t *testing.T) {
	data, err := json.Marshal(Type{Kind: TypePrimitive, Primitive: PrimitiveString})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got := string(data); got != `{"kind":"primitive","primitive":"string"}` {
		t.Errorf("bare type carries meta/stream noise: %s", got)
	}

	withMeta := Type{
		Kind:      TypePrimitive,
		Primitive: PrimitiveString,
		Meta:      TypeMeta{Stream: StreamingBehavior{Done: true}},
	}
	data, err = json.Marshal(withMeta)
	if err != nil {
		t.Fatalf("marshal meta: %v", err)
	}
	if got := string(data); got != `{"kind":"primitive","meta":{"stream":{"done":true}},"primitive":"string"}` {
		t.Errorf("meta not serialized as expected: %s", got)
	}
}
