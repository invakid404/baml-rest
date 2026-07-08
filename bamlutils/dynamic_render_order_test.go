package bamlutils

import (
	"reflect"
	"testing"
)

// TestNormalizeSchemaFieldOrderForRender_SortsRootAndNested pins the
// preserve_schema_order=false render normalization: the root properties
// and every class's properties are re-ordered alphabetically, while the
// class-declaration order (which does not affect the rendered prompt) is
// left in wire order.
func TestNormalizeSchemaFieldOrderForRender_SortsRootAndNested(t *testing.T) {
	schema := &DynamicOutputSchema{
		Properties: MustOrderedMap(
			OrderedKV("delta", &DynamicProperty{Type: "string"}),
			OrderedKV("alpha", &DynamicProperty{Type: "int"}),
			OrderedKV("hotel", &DynamicProperty{Type: "list", Items: &DynamicTypeSpec{Type: "string"}}),
			OrderedKV("charlie", &DynamicProperty{Ref: "Mailbox"}),
			OrderedKV("bravo", &DynamicProperty{Type: "string"}),
			OrderedKV("golf", &DynamicProperty{Ref: "Profile"}),
		),
		Classes: MustOrderedMap(
			OrderedKV("Profile", &DynamicClass{
				Properties: MustOrderedMap(
					OrderedKV("zulu", &DynamicProperty{Type: "string"}),
					OrderedKV("alpha", &DynamicProperty{Type: "int"}),
					OrderedKV("status", &DynamicProperty{Type: "string"}),
					OrderedKV("preference", &DynamicProperty{Type: "string"}),
				),
			}),
			OrderedKV("Mailbox", &DynamicClass{
				Properties: MustOrderedMap(
					OrderedKV("zip", &DynamicProperty{Type: "string"}),
					OrderedKV("city", &DynamicProperty{Type: "string"}),
					OrderedKV("street", &DynamicProperty{Type: "string"}),
				),
			}),
		),
		Enums: MustOrderedMap(
			OrderedKV("Status", &DynamicEnum{Values: []*DynamicEnumValue{{Name: "PENDING"}, {Name: "ACTIVE"}}}),
		),
	}

	// Snapshot the original wire order so we can assert the clone does not
	// mutate the caller-owned schema.
	wantRootWire := []string{"delta", "alpha", "hotel", "charlie", "bravo", "golf"}
	wantProfileWire := []string{"zulu", "alpha", "status", "preference"}

	got := normalizeSchemaFieldOrderForRender(schema)

	if diff := got.Properties.Keys(); !reflect.DeepEqual(diff, []string{"alpha", "bravo", "charlie", "delta", "golf", "hotel"}) {
		t.Errorf("root properties not sorted: got %v", diff)
	}

	profile, ok := got.Classes.Get("Profile")
	if !ok {
		t.Fatal("Profile class missing from normalized schema")
	}
	if diff := profile.Properties.Keys(); !reflect.DeepEqual(diff, []string{"alpha", "preference", "status", "zulu"}) {
		t.Errorf("Profile properties not sorted: got %v", diff)
	}

	mailbox, ok := got.Classes.Get("Mailbox")
	if !ok {
		t.Fatal("Mailbox class missing from normalized schema")
	}
	if diff := mailbox.Properties.Keys(); !reflect.DeepEqual(diff, []string{"city", "street", "zip"}) {
		t.Errorf("Mailbox properties not sorted: got %v", diff)
	}

	// Class declaration order is not part of the rendered prompt, so it is
	// carried through in wire order rather than sorted.
	if diff := got.Classes.Keys(); !reflect.DeepEqual(diff, []string{"Profile", "Mailbox"}) {
		t.Errorf("class order should be preserved as wire order: got %v", diff)
	}

	// Enums are carried through unchanged.
	if diff := got.Enums.Keys(); !reflect.DeepEqual(diff, []string{"Status"}) {
		t.Errorf("enums should be preserved: got %v", diff)
	}

	// The caller-owned schema must be untouched.
	if diff := schema.Properties.Keys(); !reflect.DeepEqual(diff, wantRootWire) {
		t.Errorf("original root properties mutated: got %v want %v", diff, wantRootWire)
	}
	origProfile, _ := schema.Classes.Get("Profile")
	if diff := origProfile.Properties.Keys(); !reflect.DeepEqual(diff, wantProfileWire) {
		t.Errorf("original Profile properties mutated: got %v want %v", diff, wantProfileWire)
	}
}

// TestNormalizeSchemaFieldOrderForRender_NilAndEmpty exercises the
// defensive edges: a nil schema returns nil, and a schema with no classes
// keeps an empty Classes map (no spurious allocation).
func TestNormalizeSchemaFieldOrderForRender_NilAndEmpty(t *testing.T) {
	if got := normalizeSchemaFieldOrderForRender(nil); got != nil {
		t.Errorf("nil schema should return nil, got %v", got)
	}

	flat := &DynamicOutputSchema{
		Properties: MustOrderedMap(
			OrderedKV("gamma", &DynamicProperty{Type: "string"}),
			OrderedKV("alpha", &DynamicProperty{Type: "string"}),
		),
	}
	got := normalizeSchemaFieldOrderForRender(flat)
	if diff := got.Properties.Keys(); !reflect.DeepEqual(diff, []string{"alpha", "gamma"}) {
		t.Errorf("flat root properties not sorted: got %v", diff)
	}
	if got.Classes.Len() != 0 {
		t.Errorf("expected no classes, got %d", got.Classes.Len())
	}
}

// TestNormalizeSchemaFieldOrderForRender_NilClassValue verifies a nil
// class definition is carried through as nil rather than panicking, since
// buildFields/FromDynamicOutputSchema surface that as a construction error
// downstream rather than here.
func TestNormalizeSchemaFieldOrderForRender_NilClassValue(t *testing.T) {
	schema := &DynamicOutputSchema{
		Properties: MustOrderedMap(OrderedKV("a", &DynamicProperty{Type: "string"})),
		Classes:    MustOrderedMap(OrderedKV("Broken", (*DynamicClass)(nil))),
	}
	got := normalizeSchemaFieldOrderForRender(schema)
	v, ok := got.Classes.Get("Broken")
	if !ok || v != nil {
		t.Errorf("nil class value should be carried through as nil, got ok=%v v=%v", ok, v)
	}
}
