package bamlutils

import "testing"

func inject(t *testing.T, raw string, schema *DynamicOutputSchema) string {
	t.Helper()
	out, err := InjectAbsentOptionals([]byte(raw), schema)
	if err != nil {
		t.Fatalf("InjectAbsentOptionals: %v", err)
	}
	return string(out)
}

func TestInjectAbsentOptionals_UnionClassArmWithAbsentOptional(t *testing.T) {
	t.Parallel()

	// Class A has a required property "id" and an optional property "note".
	// The union arm matcher must recognise {"id":1} as matching A even
	// though "note" is absent — that's exactly what we're trying to inject.
	aProps := MustOrderedMap(
		OrderedKV("id", &DynamicProperty{Type: "int"}),
		OrderedKV("note", &DynamicProperty{Type: "optional", Inner: &DynamicTypeSpec{Type: "string"}}),
	)
	classes := MustOrderedMap(
		OrderedKV("A", &DynamicClass{Properties: aProps}),
	)
	props := MustOrderedMap(
		OrderedKV("v", &DynamicProperty{
			Type: "union",
			OneOf: []*DynamicTypeSpec{
				{Ref: "A"},
				{Type: "string"},
			},
		}),
	)
	s := &DynamicOutputSchema{Properties: props, Classes: classes}
	got := inject(t, `{"v":{"id":1}}`, s)
	want := `{"v":{"id":1,"note":null}}`
	if got != want {
		t.Fatalf("union class arm with absent optional:\n got %s\nwant %s", got, want)
	}
}

func TestInjectAbsentOptionals_UnionClassArmAllPresent(t *testing.T) {
	t.Parallel()

	// When the optional property IS present, it should be preserved as-is.
	aProps := MustOrderedMap(
		OrderedKV("id", &DynamicProperty{Type: "int"}),
		OrderedKV("note", &DynamicProperty{Type: "optional", Inner: &DynamicTypeSpec{Type: "string"}}),
	)
	classes := MustOrderedMap(
		OrderedKV("A", &DynamicClass{Properties: aProps}),
	)
	props := MustOrderedMap(
		OrderedKV("v", &DynamicProperty{
			Type: "union",
			OneOf: []*DynamicTypeSpec{
				{Ref: "A"},
				{Type: "string"},
			},
		}),
	)
	s := &DynamicOutputSchema{Properties: props, Classes: classes}
	got := inject(t, `{"v":{"id":1,"note":"hi"}}`, s)
	want := `{"v":{"id":1,"note":"hi"}}`
	if got != want {
		t.Fatalf("union class arm all present:\n got %s\nwant %s", got, want)
	}
}

func TestInjectAbsentOptionals_UnionClassArmMissingRequired(t *testing.T) {
	t.Parallel()

	// When a required property is missing, the class arm should NOT match,
	// and the value should pass through unchanged (matched by the scalar arm).
	aProps := MustOrderedMap(
		OrderedKV("id", &DynamicProperty{Type: "int"}),
		OrderedKV("note", &DynamicProperty{Type: "optional", Inner: &DynamicTypeSpec{Type: "string"}}),
	)
	classes := MustOrderedMap(
		OrderedKV("A", &DynamicClass{Properties: aProps}),
	)
	props := MustOrderedMap(
		OrderedKV("v", &DynamicProperty{
			Type: "union",
			OneOf: []*DynamicTypeSpec{
				{Ref: "A"},
				{Type: "string"},
			},
		}),
	)
	s := &DynamicOutputSchema{Properties: props, Classes: classes}
	got := inject(t, `{"v":{"note":"hello"}}`, s)
	want := `{"v":{"note":"hello"}}`
	if got != want {
		t.Fatalf("union class arm missing required:\n got %s\nwant %s", got, want)
	}
}

func TestInjectAbsentOptionals_UnionTwoClassArmsDisambiguation(t *testing.T) {
	t.Parallel()

	// Two class arms: A has {id, note?}, B has {title, desc?}.
	// Input matches B (has "title"), so B's absent "desc" gets injected.
	aProps := MustOrderedMap(
		OrderedKV("id", &DynamicProperty{Type: "int"}),
		OrderedKV("note", &DynamicProperty{Type: "optional", Inner: &DynamicTypeSpec{Type: "string"}}),
	)
	bProps := MustOrderedMap(
		OrderedKV("title", &DynamicProperty{Type: "string"}),
		OrderedKV("desc", &DynamicProperty{Type: "optional", Inner: &DynamicTypeSpec{Type: "string"}}),
	)
	classes := MustOrderedMap(
		OrderedKV("A", &DynamicClass{Properties: aProps}),
		OrderedKV("B", &DynamicClass{Properties: bProps}),
	)
	props := MustOrderedMap(
		OrderedKV("v", &DynamicProperty{
			Type: "union",
			OneOf: []*DynamicTypeSpec{
				{Ref: "A"},
				{Ref: "B"},
			},
		}),
	)
	s := &DynamicOutputSchema{Properties: props, Classes: classes}
	got := inject(t, `{"v":{"title":"x"}}`, s)
	want := `{"v":{"title":"x","desc":null}}`
	if got != want {
		t.Fatalf("union two-class disambiguation:\n got %s\nwant %s", got, want)
	}
}

func TestInjectAbsentOptionals_MapClassRefInjectsInsideValues(t *testing.T) {
	t.Parallel()

	// map<string, FuzzClass2> where FuzzClass2 has one optional property.
	// Injection must recurse into each map VALUE — a value missing the
	// optional gets it injected INSIDE that value — and must NOT inject
	// the class's properties at the map container level.
	f2 := MustOrderedMap(
		OrderedKV("id", &DynamicProperty{Type: "int"}),
		OrderedKV("note", &DynamicProperty{Type: "optional", Inner: &DynamicTypeSpec{Type: "string"}}),
	)
	classes := MustOrderedMap(
		OrderedKV("FuzzClass2", &DynamicClass{Properties: f2}),
	)
	props := MustOrderedMap(
		OrderedKV("m", &DynamicProperty{
			Type:   "map",
			Keys:   &DynamicTypeSpec{Type: "string"},
			Values: &DynamicTypeSpec{Ref: "FuzzClass2"},
		}),
	)
	s := &DynamicOutputSchema{Properties: props, Classes: classes}
	// k0 already has note; kA is missing it.
	got := inject(t, `{"m":{"k0":{"id":1,"note":"x"},"kA":{"id":2}}}`, s)
	want := `{"m":{"k0":{"id":1,"note":"x"},"kA":{"id":2,"note":null}}}`
	if got != want {
		t.Fatalf("map class-ref injects inside values:\n got %s\nwant %s", got, want)
	}
}

func TestInjectAbsentOptionals_MapPropertyWithStrayRefNotTreatedAsClass(t *testing.T) {
	t.Parallel()

	// Regression for the nightly-envelope shape: a map<string, ClassRef>
	// property whose spec ALSO carries the value class's name in Ref.
	// injectType must take the map path (recurse into values), not the
	// Ref/class path — otherwise FuzzClass2's optional gets injected at
	// the map container level as a spurious sibling of the map keys.
	f2 := MustOrderedMap(
		OrderedKV("Fuzz_field_0", &DynamicProperty{Type: "optional", Inner: &DynamicTypeSpec{Type: "int"}}),
	)
	classes := MustOrderedMap(
		OrderedKV("FuzzClass2", &DynamicClass{Properties: f2}),
	)
	props := MustOrderedMap(
		OrderedKV("Fuzz_field_0", &DynamicProperty{
			Type:   "map",
			Ref:    "FuzzClass2", // stray ref carried alongside the map type
			Keys:   &DynamicTypeSpec{Type: "string"},
			Values: &DynamicTypeSpec{Ref: "FuzzClass2"},
		}),
	)
	s := &DynamicOutputSchema{Properties: props, Classes: classes}
	// Every map value already has the optional present → nothing to inject.
	in := `{"Fuzz_field_0":{"k0":{"Fuzz_field_0":0},"kA":{"Fuzz_field_0":-42}}}`
	got := inject(t, in, s)
	if got != in {
		t.Fatalf("map property with stray ref must not inject at container level:\n got %s\nwant %s", got, in)
	}
}

func TestInjectAbsentOptionals_TopLevelAbsentOptional(t *testing.T) {
	t.Parallel()

	// Baseline: top-level absent optional gets injected (no union involved).
	props := MustOrderedMap(
		OrderedKV("id", &DynamicProperty{Type: "int"}),
		OrderedKV("note", &DynamicProperty{Type: "optional", Inner: &DynamicTypeSpec{Type: "string"}}),
	)
	s := &DynamicOutputSchema{Properties: props}
	got := inject(t, `{"id":1}`, s)
	want := `{"id":1,"note":null}`
	if got != want {
		t.Fatalf("top-level absent optional:\n got %s\nwant %s", got, want)
	}
}
