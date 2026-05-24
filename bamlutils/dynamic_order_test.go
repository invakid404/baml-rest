package bamlutils

import (
	"strings"
	"testing"
)

// reorder is the per-test convenience wrapper that fails fast on
// errors so the body assertions can stay focused on the byte shape.
func reorder(t *testing.T, raw string, schema *DynamicOutputSchema) string {
	t.Helper()
	out, err := ReorderDynamicOutputBySchema([]byte(raw), schema)
	if err != nil {
		t.Fatalf("ReorderDynamicOutputBySchema: %v", err)
	}
	return string(out)
}

func schemaFlat(t *testing.T, entries ...OrderedEntry[*DynamicProperty]) *DynamicOutputSchema {
	t.Helper()
	props, err := NewOrderedMap(entries...)
	if err != nil {
		t.Fatalf("NewOrderedMap: %v", err)
	}
	return &DynamicOutputSchema{Properties: props}
}

func TestReorderDynamicOutputBySchema_TopLevel(t *testing.T) {
	t.Parallel()
	s := schemaFlat(t,
		OrderedKV("c", &DynamicProperty{Type: "string"}),
		OrderedKV("a", &DynamicProperty{Type: "int"}),
		OrderedKV("b", &DynamicProperty{Type: "bool"}),
	)
	got := reorder(t, `{"a":1,"b":true,"c":"x"}`, s)
	if got != `{"c":"x","a":1,"b":true}` {
		t.Fatalf("top-level reorder: got %s", got)
	}
}

func TestReorderDynamicOutputBySchema_Nested(t *testing.T) {
	t.Parallel()
	addrProps, _ := NewOrderedMap(
		OrderedKV("street", &DynamicProperty{Type: "string"}),
		OrderedKV("city", &DynamicProperty{Type: "string"}),
	)
	classes, _ := NewOrderedMap(
		OrderedKV("Address", &DynamicClass{Properties: addrProps}),
	)
	props, _ := NewOrderedMap(
		OrderedKV("name", &DynamicProperty{Type: "string"}),
		OrderedKV("addr", &DynamicProperty{Ref: "Address"}),
	)
	s := &DynamicOutputSchema{Properties: props, Classes: classes}
	got := reorder(t, `{"addr":{"city":"Sofia","street":"Vitosha"},"name":"X"}`, s)
	want := `{"name":"X","addr":{"street":"Vitosha","city":"Sofia"}}`
	if got != want {
		t.Fatalf("nested reorder: got %s want %s", got, want)
	}
}

func TestReorderDynamicOutputBySchema_OptionalClassPresent(t *testing.T) {
	t.Parallel()
	addrProps, _ := NewOrderedMap(
		OrderedKV("street", &DynamicProperty{Type: "string"}),
		OrderedKV("city", &DynamicProperty{Type: "string"}),
	)
	classes, _ := NewOrderedMap(
		OrderedKV("Address", &DynamicClass{Properties: addrProps}),
	)
	props, _ := NewOrderedMap(
		OrderedKV("addr", &DynamicProperty{
			Type:  "optional",
			Inner: &DynamicTypeSpec{Ref: "Address"},
		}),
	)
	s := &DynamicOutputSchema{Properties: props, Classes: classes}
	got := reorder(t, `{"addr":{"city":"Sofia","street":"Vitosha"}}`, s)
	want := `{"addr":{"street":"Vitosha","city":"Sofia"}}`
	if got != want {
		t.Fatalf("optional present: got %s want %s", got, want)
	}
}

func TestReorderDynamicOutputBySchema_OptionalClassNull(t *testing.T) {
	t.Parallel()
	addrProps, _ := NewOrderedMap(
		OrderedKV("street", &DynamicProperty{Type: "string"}),
	)
	classes, _ := NewOrderedMap(
		OrderedKV("Address", &DynamicClass{Properties: addrProps}),
	)
	props, _ := NewOrderedMap(
		OrderedKV("addr", &DynamicProperty{
			Type:  "optional",
			Inner: &DynamicTypeSpec{Ref: "Address"},
		}),
	)
	s := &DynamicOutputSchema{Properties: props, Classes: classes}
	got := reorder(t, `{"addr":null}`, s)
	if got != `{"addr":null}` {
		t.Fatalf("optional null: got %s", got)
	}
}

func TestReorderDynamicOutputBySchema_ListOfClass(t *testing.T) {
	t.Parallel()
	itemProps, _ := NewOrderedMap(
		OrderedKV("y", &DynamicProperty{Type: "int"}),
		OrderedKV("x", &DynamicProperty{Type: "int"}),
	)
	classes, _ := NewOrderedMap(
		OrderedKV("Point", &DynamicClass{Properties: itemProps}),
	)
	props, _ := NewOrderedMap(
		OrderedKV("points", &DynamicProperty{
			Type:  "list",
			Items: &DynamicTypeSpec{Ref: "Point"},
		}),
	)
	s := &DynamicOutputSchema{Properties: props, Classes: classes}
	got := reorder(t, `{"points":[{"x":1,"y":2},{"x":3,"y":4}]}`, s)
	want := `{"points":[{"y":2,"x":1},{"y":4,"x":3}]}`
	if got != want {
		t.Fatalf("list of class: got %s want %s", got, want)
	}
}

func TestReorderDynamicOutputBySchema_MapOfClass(t *testing.T) {
	t.Parallel()
	addrProps, _ := NewOrderedMap(
		OrderedKV("street", &DynamicProperty{Type: "string"}),
		OrderedKV("city", &DynamicProperty{Type: "string"}),
	)
	classes, _ := NewOrderedMap(
		OrderedKV("Address", &DynamicClass{Properties: addrProps}),
	)
	props, _ := NewOrderedMap(
		OrderedKV("addrs", &DynamicProperty{
			Type:   "map",
			Keys:   &DynamicTypeSpec{Type: "string"},
			Values: &DynamicTypeSpec{Ref: "Address"},
		}),
	)
	s := &DynamicOutputSchema{Properties: props, Classes: classes}
	// Input has "z" first then "a" - the map keys must stay in input
	// order while each value's class properties get reordered.
	got := reorder(t, `{"addrs":{"z":{"city":"Z","street":"ZZ"},"a":{"city":"A","street":"AA"}}}`, s)
	want := `{"addrs":{"z":{"street":"ZZ","city":"Z"},"a":{"street":"AA","city":"A"}}}`
	if got != want {
		t.Fatalf("map of class: got %s want %s", got, want)
	}
}

func TestReorderDynamicOutputBySchema_MapOfScalar(t *testing.T) {
	t.Parallel()
	props, _ := NewOrderedMap(
		OrderedKV("scores", &DynamicProperty{
			Type:   "map",
			Keys:   &DynamicTypeSpec{Type: "string"},
			Values: &DynamicTypeSpec{Type: "int"},
		}),
	)
	s := &DynamicOutputSchema{Properties: props}
	got := reorder(t, `{"scores":{"z":1,"a":2,"m":3}}`, s)
	if got != `{"scores":{"z":1,"a":2,"m":3}}` {
		t.Fatalf("map of scalar: got %s", got)
	}
}

func TestReorderDynamicOutputBySchema_MissingRoot(t *testing.T) {
	t.Parallel()
	_, err := ReorderDynamicOutputBySchema([]byte(`{}`), nil)
	if err == nil {
		t.Fatal("expected error for nil schema")
	}
	empty := &DynamicOutputSchema{}
	_, err = ReorderDynamicOutputBySchema([]byte(`{}`), empty)
	if err == nil {
		t.Fatal("expected error for empty Properties")
	}
}

func TestReorderDynamicOutputBySchema_MissingClassRef(t *testing.T) {
	t.Parallel()
	props, _ := NewOrderedMap(
		OrderedKV("addr", &DynamicProperty{Ref: "Address"}),
	)
	s := &DynamicOutputSchema{Properties: props}
	_, err := ReorderDynamicOutputBySchema([]byte(`{"addr":{"x":1}}`), s)
	if err == nil {
		t.Fatal("expected error for missing class ref")
	}
	if !strings.Contains(err.Error(), "Address") {
		t.Fatalf("expected error to name the missing class, got: %v", err)
	}
}

func TestReorderDynamicOutputBySchema_Idempotent(t *testing.T) {
	t.Parallel()
	addrProps, _ := NewOrderedMap(
		OrderedKV("street", &DynamicProperty{Type: "string"}),
		OrderedKV("city", &DynamicProperty{Type: "string"}),
	)
	classes, _ := NewOrderedMap(
		OrderedKV("Address", &DynamicClass{Properties: addrProps}),
	)
	props, _ := NewOrderedMap(
		OrderedKV("name", &DynamicProperty{Type: "string"}),
		OrderedKV("addr", &DynamicProperty{Ref: "Address"}),
	)
	s := &DynamicOutputSchema{Properties: props, Classes: classes}
	first := reorder(t, `{"addr":{"city":"Sofia","street":"Vitosha"},"name":"X"}`, s)
	second := reorder(t, first, s)
	if first != second {
		t.Fatalf("not idempotent: first=%s second=%s", first, second)
	}
}

func TestReorderDynamicOutputBySchema_UnionFirstClassMatch(t *testing.T) {
	t.Parallel()
	aProps, _ := NewOrderedMap(
		OrderedKV("name", &DynamicProperty{Type: "string"}),
		OrderedKV("id", &DynamicProperty{Type: "int"}),
	)
	bProps, _ := NewOrderedMap(
		OrderedKV("title", &DynamicProperty{Type: "string"}),
		OrderedKV("uid", &DynamicProperty{Type: "int"}),
	)
	classes, _ := NewOrderedMap(
		OrderedKV("A", &DynamicClass{Properties: aProps}),
		OrderedKV("B", &DynamicClass{Properties: bProps}),
	)
	props, _ := NewOrderedMap(
		OrderedKV("v", &DynamicProperty{
			Type: "union",
			OneOf: []*DynamicTypeSpec{
				{Ref: "A"},
				{Ref: "B"},
			},
		}),
	)
	s := &DynamicOutputSchema{Properties: props, Classes: classes}
	got := reorder(t, `{"v":{"id":1,"name":"x"}}`, s)
	if got != `{"v":{"name":"x","id":1}}` {
		t.Fatalf("union first class match: got %s", got)
	}
}

func TestReorderDynamicOutputBySchema_UnionSecondClassMatch(t *testing.T) {
	t.Parallel()
	aProps, _ := NewOrderedMap(
		OrderedKV("name", &DynamicProperty{Type: "string"}),
		OrderedKV("id", &DynamicProperty{Type: "int"}),
	)
	bProps, _ := NewOrderedMap(
		OrderedKV("title", &DynamicProperty{Type: "string"}),
		OrderedKV("uid", &DynamicProperty{Type: "int"}),
	)
	classes, _ := NewOrderedMap(
		OrderedKV("A", &DynamicClass{Properties: aProps}),
		OrderedKV("B", &DynamicClass{Properties: bProps}),
	)
	props, _ := NewOrderedMap(
		OrderedKV("v", &DynamicProperty{
			Type: "union",
			OneOf: []*DynamicTypeSpec{
				{Ref: "A"},
				{Ref: "B"},
			},
		}),
	)
	s := &DynamicOutputSchema{Properties: props, Classes: classes}
	got := reorder(t, `{"v":{"uid":5,"title":"y"}}`, s)
	if got != `{"v":{"title":"y","uid":5}}` {
		t.Fatalf("union second class match: got %s", got)
	}
}

func TestReorderDynamicOutputBySchema_UnionClassPlusScalarRuntimeScalar(t *testing.T) {
	t.Parallel()
	aProps, _ := NewOrderedMap(
		OrderedKV("name", &DynamicProperty{Type: "string"}),
		OrderedKV("id", &DynamicProperty{Type: "int"}),
	)
	classes, _ := NewOrderedMap(
		OrderedKV("A", &DynamicClass{Properties: aProps}),
	)
	props, _ := NewOrderedMap(
		OrderedKV("v", &DynamicProperty{
			Type: "union",
			OneOf: []*DynamicTypeSpec{
				{Ref: "A"},
				{Type: "string"},
			},
		}),
	)
	s := &DynamicOutputSchema{Properties: props, Classes: classes}
	got := reorder(t, `{"v":"hello"}`, s)
	if got != `{"v":"hello"}` {
		t.Fatalf("union class+scalar runtime scalar: got %s", got)
	}
}

func TestReorderDynamicOutputBySchema_UnionWithOptionalNull(t *testing.T) {
	t.Parallel()
	aProps, _ := NewOrderedMap(
		OrderedKV("name", &DynamicProperty{Type: "string"}),
		OrderedKV("id", &DynamicProperty{Type: "int"}),
	)
	classes, _ := NewOrderedMap(
		OrderedKV("A", &DynamicClass{Properties: aProps}),
	)
	props, _ := NewOrderedMap(
		OrderedKV("v", &DynamicProperty{
			Type: "union",
			OneOf: []*DynamicTypeSpec{
				{Type: "optional", Inner: &DynamicTypeSpec{Ref: "A"}},
				{Type: "string"},
			},
		}),
	)
	s := &DynamicOutputSchema{Properties: props, Classes: classes}
	got := reorder(t, `{"v":null}`, s)
	if got != `{"v":null}` {
		t.Fatalf("union with optional null: got %s", got)
	}
}

func TestReorderDynamicOutputBySchema_UnionNoMatch(t *testing.T) {
	t.Parallel()
	aProps, _ := NewOrderedMap(
		OrderedKV("name", &DynamicProperty{Type: "string"}),
		OrderedKV("id", &DynamicProperty{Type: "int"}),
	)
	classes, _ := NewOrderedMap(
		OrderedKV("A", &DynamicClass{Properties: aProps}),
	)
	props, _ := NewOrderedMap(
		OrderedKV("v", &DynamicProperty{
			Type: "union",
			OneOf: []*DynamicTypeSpec{
				{Ref: "A"},
				{Type: "int"},
			},
		}),
	)
	s := &DynamicOutputSchema{Properties: props, Classes: classes}
	// Object missing required class properties; not an int — neither
	// variant matches. Pass-through, no error.
	in := `{"v":{"unrelated":"value","other":42}}`
	out, err := ReorderDynamicOutputBySchema([]byte(in), s)
	if err != nil {
		t.Fatalf("expected no error on union no-match, got %v", err)
	}
	if string(out) != `{"v":{"unrelated":"value","other":42}}` {
		t.Fatalf("union no-match should pass through: got %s", out)
	}
}

func TestReorderDynamicOutputBySchema_UnionAmbiguousFirstWins(t *testing.T) {
	t.Parallel()
	// Two classes share keys; the input matches both. First-declared
	// must win, even though both A and B are structurally valid.
	aProps, _ := NewOrderedMap(
		OrderedKV("id", &DynamicProperty{Type: "int"}),
	)
	bProps, _ := NewOrderedMap(
		OrderedKV("id", &DynamicProperty{Type: "int"}),
	)
	classes, _ := NewOrderedMap(
		OrderedKV("A", &DynamicClass{Properties: aProps}),
		OrderedKV("B", &DynamicClass{Properties: bProps}),
	)
	props, _ := NewOrderedMap(
		OrderedKV("v", &DynamicProperty{
			Type: "union",
			OneOf: []*DynamicTypeSpec{
				{Ref: "A"},
				{Ref: "B"},
			},
		}),
	)
	s := &DynamicOutputSchema{Properties: props, Classes: classes}
	// Both A and B match — first wins. Output is the reordered A view
	// (extra keys appended after declared id).
	got := reorder(t, `{"v":{"extra":"x","id":1}}`, s)
	if got != `{"v":{"id":1,"extra":"x"}}` {
		t.Fatalf("ambiguous union: got %s", got)
	}
}

func TestReorderDynamicOutputBySchema_SelfReferentialLinkedList(t *testing.T) {
	t.Parallel()
	nodeProps, _ := NewOrderedMap(
		OrderedKV("value", &DynamicProperty{Type: "int"}),
		OrderedKV("next", &DynamicProperty{
			Type:  "optional",
			Inner: &DynamicTypeSpec{Ref: "Node"},
		}),
	)
	classes, _ := NewOrderedMap(
		OrderedKV("Node", &DynamicClass{Properties: nodeProps}),
	)
	props, _ := NewOrderedMap(
		OrderedKV("head", &DynamicProperty{Ref: "Node"}),
	)
	s := &DynamicOutputSchema{Properties: props, Classes: classes}
	got := reorder(t, `{"head":{"next":{"next":null,"value":2},"value":1}}`, s)
	want := `{"head":{"value":1,"next":{"value":2,"next":null}}}`
	if got != want {
		t.Fatalf("self-ref: got %s want %s", got, want)
	}
}

func TestReorderDynamicOutputBySchema_MutualRecursion(t *testing.T) {
	t.Parallel()
	aProps, _ := NewOrderedMap(
		OrderedKV("name", &DynamicProperty{Type: "string"}),
		OrderedKV("b", &DynamicProperty{
			Type:  "optional",
			Inner: &DynamicTypeSpec{Ref: "B"},
		}),
	)
	bProps, _ := NewOrderedMap(
		OrderedKV("label", &DynamicProperty{Type: "string"}),
		OrderedKV("a", &DynamicProperty{
			Type:  "optional",
			Inner: &DynamicTypeSpec{Ref: "A"},
		}),
	)
	classes, _ := NewOrderedMap(
		OrderedKV("A", &DynamicClass{Properties: aProps}),
		OrderedKV("B", &DynamicClass{Properties: bProps}),
	)
	props, _ := NewOrderedMap(
		OrderedKV("root", &DynamicProperty{Ref: "A"}),
	)
	s := &DynamicOutputSchema{Properties: props, Classes: classes}
	got := reorder(t, `{"root":{"b":{"a":{"b":null,"name":"deep"},"label":"L"},"name":"top"}}`, s)
	want := `{"root":{"name":"top","b":{"label":"L","a":{"name":"deep","b":null}}}}`
	if got != want {
		t.Fatalf("mutual recursion: got %s want %s", got, want)
	}
}

func TestReorderDynamicOutputBySchema_UnknownInputKeysPreservedAfterDeclared(t *testing.T) {
	t.Parallel()
	s := schemaFlat(t,
		OrderedKV("a", &DynamicProperty{Type: "string"}),
		OrderedKV("b", &DynamicProperty{Type: "int"}),
	)
	got := reorder(t, `{"zeta":99,"a":"x","extra":true,"b":1}`, s)
	want := `{"a":"x","b":1,"zeta":99,"extra":true}`
	if got != want {
		t.Fatalf("unknown keys preserved: got %s want %s", got, want)
	}
}

func TestReorderDynamicOutputBySchema_DuplicateKeysError(t *testing.T) {
	t.Parallel()
	s := schemaFlat(t,
		OrderedKV("a", &DynamicProperty{Type: "string"}),
	)
	_, err := ReorderDynamicOutputBySchema([]byte(`{"a":"x","a":"y"}`), s)
	if err == nil {
		t.Fatal("expected error for duplicate keys in input")
	}
}

func TestReorderDynamicOutputBySchema_MalformedJSONError(t *testing.T) {
	t.Parallel()
	s := schemaFlat(t,
		OrderedKV("a", &DynamicProperty{Type: "string"}),
	)
	_, err := ReorderDynamicOutputBySchema([]byte(`not json`), s)
	if err == nil {
		t.Fatal("expected error for malformed JSON")
	}
}
