package bamlfuzz

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
)

// schemaRootOnly builds a single-class schema with the given declared
// property order. Inner types are all plain strings so the helper
// stays focused on the root-level class node tests.
func schemaRootOnly(props ...string) FuzzSchema {
	cls := FuzzClass{Name: "Root"}
	for _, name := range props {
		cls.Properties = append(cls.Properties, FuzzProperty{
			Name: name,
			Type: FuzzType{Kind: KindString},
		})
	}
	return FuzzSchema{Classes: []FuzzClass{cls}, RootClass: "Root"}
}

func optionalOf(inner FuzzType) FuzzType {
	return FuzzType{Kind: KindOptional, Inner: &inner}
}

func listOf(inner FuzzType) FuzzType {
	return FuzzType{Kind: KindList, Inner: &inner}
}

func mapOf(inner FuzzType) FuzzType {
	return FuzzType{Kind: KindMap, Key: &FuzzType{Kind: KindString}, Inner: &inner}
}

func classRef(name string) FuzzType {
	return FuzzType{Kind: KindClassRef, Ref: name}
}

func TestSchemaOrderDiff_TopLevelSwapFails(t *testing.T) {
	schema := schemaRootOnly("a", "b", "c")
	exp := json.RawMessage(`{"a":"1","b":"2","c":"3"}`)
	got := json.RawMessage(`{"b":"2","a":"1","c":"3"}`)
	diffs, err := SchemaOrderDiff("expected_vs_rest", schema, exp, got)
	if err != nil {
		t.Fatalf("SchemaOrderDiff: %v", err)
	}
	if len(diffs) != 1 {
		t.Fatalf("expected 1 diff, got %d (%v)", len(diffs), diffs)
	}
	d := diffs[0]
	if d.Side != "expected_vs_rest" || d.Path != "$" {
		t.Errorf("entry head: %+v", d)
	}
	wantExp := []string{"a", "b", "c"}
	wantGot := []string{"b", "a", "c"}
	if !keysEqual(d.Expected, wantExp) || !keysEqual(d.Actual, wantGot) {
		t.Errorf("entry keys: got expected=%v actual=%v, want expected=%v actual=%v",
			d.Expected, d.Actual, wantExp, wantGot)
	}
}

func TestSchemaOrderDiff_IdenticalOrderHasNoDiff(t *testing.T) {
	schema := schemaRootOnly("a", "b", "c")
	exp := json.RawMessage(`{"a":"1","b":"2","c":"3"}`)
	got := json.RawMessage(`{"a":"1","b":"2","c":"3"}`)
	diffs, err := SchemaOrderDiff("side", schema, exp, got)
	if err != nil {
		t.Fatalf("SchemaOrderDiff: %v", err)
	}
	if len(diffs) != 0 {
		t.Errorf("expected no diffs, got %v", diffs)
	}
}

func TestSchemaOrderDiff_NestedClassMismatchAtChildPath(t *testing.T) {
	inner := FuzzClass{Name: "Inner", Properties: []FuzzProperty{
		{Name: "x", Type: FuzzType{Kind: KindInt}},
		{Name: "y", Type: FuzzType{Kind: KindString}},
	}}
	outer := FuzzClass{Name: "Root", Properties: []FuzzProperty{
		{Name: "before", Type: FuzzType{Kind: KindString}},
		{Name: "inner", Type: classRef("Inner")},
		{Name: "after", Type: FuzzType{Kind: KindString}},
	}}
	schema := FuzzSchema{Classes: []FuzzClass{outer, inner}, RootClass: "Root"}

	exp := json.RawMessage(`{"before":"b","inner":{"x":1,"y":"hi"},"after":"a"}`)
	got := json.RawMessage(`{"before":"b","inner":{"y":"hi","x":1},"after":"a"}`)
	diffs, err := SchemaOrderDiff("side", schema, exp, got)
	if err != nil {
		t.Fatalf("SchemaOrderDiff: %v", err)
	}
	if len(diffs) != 1 {
		t.Fatalf("expected 1 diff, got %d (%v)", len(diffs), diffs)
	}
	if diffs[0].Path != "$.inner" {
		t.Errorf("nested path: got %q want %q", diffs[0].Path, "$.inner")
	}
}

func TestSchemaOrderDiff_ListOfClassMismatchAtIndexedPath(t *testing.T) {
	inner := FuzzClass{Name: "Inner", Properties: []FuzzProperty{
		{Name: "x", Type: FuzzType{Kind: KindInt}},
		{Name: "y", Type: FuzzType{Kind: KindInt}},
	}}
	outer := FuzzClass{Name: "Root", Properties: []FuzzProperty{
		{Name: "items", Type: listOf(classRef("Inner"))},
	}}
	schema := FuzzSchema{Classes: []FuzzClass{outer, inner}, RootClass: "Root"}

	exp := json.RawMessage(`{"items":[{"x":1,"y":2},{"x":3,"y":4}]}`)
	got := json.RawMessage(`{"items":[{"y":2,"x":1},{"x":3,"y":4}]}`)
	diffs, err := SchemaOrderDiff("side", schema, exp, got)
	if err != nil {
		t.Fatalf("SchemaOrderDiff: %v", err)
	}
	if len(diffs) != 1 {
		t.Fatalf("expected 1 diff, got %d (%v)", len(diffs), diffs)
	}
	if diffs[0].Path != "$.items[0]" {
		t.Errorf("list path: got %q want %q", diffs[0].Path, "$.items[0]")
	}
}

func TestSchemaOrderDiff_OptionalPresentRecursesAndFlagsInnerSwap(t *testing.T) {
	inner := FuzzClass{Name: "Inner", Properties: []FuzzProperty{
		{Name: "x", Type: FuzzType{Kind: KindInt}},
		{Name: "y", Type: FuzzType{Kind: KindString}},
	}}
	outer := FuzzClass{Name: "Root", Properties: []FuzzProperty{
		{Name: "maybe", Type: optionalOf(classRef("Inner"))},
	}}
	schema := FuzzSchema{Classes: []FuzzClass{outer, inner}, RootClass: "Root"}

	exp := json.RawMessage(`{"maybe":{"x":1,"y":"a"}}`)
	got := json.RawMessage(`{"maybe":{"y":"a","x":1}}`)
	diffs, err := SchemaOrderDiff("side", schema, exp, got)
	if err != nil {
		t.Fatalf("SchemaOrderDiff: %v", err)
	}
	if len(diffs) != 1 {
		t.Fatalf("expected 1 diff, got %d (%v)", len(diffs), diffs)
	}
	if diffs[0].Path != "$.maybe" {
		t.Errorf("optional path: got %q want %q", diffs[0].Path, "$.maybe")
	}
}

func TestSchemaOrderDiff_OptionalNullDoesNotRecurse(t *testing.T) {
	inner := FuzzClass{Name: "Inner", Properties: []FuzzProperty{
		{Name: "x", Type: FuzzType{Kind: KindInt}},
		{Name: "y", Type: FuzzType{Kind: KindString}},
	}}
	outer := FuzzClass{Name: "Root", Properties: []FuzzProperty{
		{Name: "maybe", Type: optionalOf(classRef("Inner"))},
	}}
	schema := FuzzSchema{Classes: []FuzzClass{outer, inner}, RootClass: "Root"}

	exp := json.RawMessage(`{"maybe":null}`)
	got := json.RawMessage(`{"maybe":null}`)
	diffs, err := SchemaOrderDiff("side", schema, exp, got)
	if err != nil {
		t.Fatalf("SchemaOrderDiff: %v", err)
	}
	if len(diffs) != 0 {
		t.Errorf("expected no diffs on both-null, got %v", diffs)
	}

	// One side null, one side present: structural mismatch is owned by
	// SemanticDiff; the order walker must not synthesize a class-order
	// entry from the optional dispatch.
	exp = json.RawMessage(`{"maybe":null}`)
	got = json.RawMessage(`{"maybe":{"y":"a","x":1}}`)
	diffs, err = SchemaOrderDiff("side", schema, exp, got)
	if err != nil {
		t.Fatalf("SchemaOrderDiff: %v", err)
	}
	if len(diffs) != 0 {
		t.Errorf("expected no order entries for null-vs-object, got %v", diffs)
	}
}

// TestSchemaOrderDiff_MapKeyReorderFlagged pins the map insertion-order
// policy: FuzzValue.MapEntries carries the request's insertion order;
// the wire must echo it back when preserve_schema_order=true.
func TestSchemaOrderDiff_MapKeyReorderFlagged(t *testing.T) {
	outer := FuzzClass{Name: "Root", Properties: []FuzzProperty{
		{Name: "by_id", Type: mapOf(FuzzType{Kind: KindInt})},
	}}
	schema := FuzzSchema{Classes: []FuzzClass{outer}, RootClass: "Root"}

	exp := json.RawMessage(`{"by_id":{"a":1,"b":2,"c":3}}`)
	got := json.RawMessage(`{"by_id":{"c":3,"a":1,"b":2}}`)
	diffs, err := SchemaOrderDiff("side", schema, exp, got)
	if err != nil {
		t.Fatalf("SchemaOrderDiff: %v", err)
	}
	if len(diffs) != 1 {
		t.Fatalf("expected 1 diff, got %d (%v)", len(diffs), diffs)
	}
	if diffs[0].Path != "$.by_id" {
		t.Errorf("path: got %q want %q", diffs[0].Path, "$.by_id")
	}
}

func TestSchemaOrderDiff_MapValueClassChecked(t *testing.T) {
	inner := FuzzClass{Name: "Inner", Properties: []FuzzProperty{
		{Name: "x", Type: FuzzType{Kind: KindInt}},
		{Name: "y", Type: FuzzType{Kind: KindString}},
	}}
	outer := FuzzClass{Name: "Root", Properties: []FuzzProperty{
		{Name: "by_id", Type: mapOf(classRef("Inner"))},
	}}
	schema := FuzzSchema{Classes: []FuzzClass{outer, inner}, RootClass: "Root"}

	exp := json.RawMessage(`{"by_id":{"a":{"x":1,"y":"hi"},"b":{"x":2,"y":"yo"}}}`)
	got := json.RawMessage(`{"by_id":{"a":{"y":"hi","x":1},"b":{"x":2,"y":"yo"}}}`)
	diffs, err := SchemaOrderDiff("side", schema, exp, got)
	if err != nil {
		t.Fatalf("SchemaOrderDiff: %v", err)
	}
	if len(diffs) != 1 {
		t.Fatalf("expected 1 diff, got %d (%v)", len(diffs), diffs)
	}
	if diffs[0].Path != `$.by_id["a"]` {
		t.Errorf("map value path: got %q want %q", diffs[0].Path, `$.by_id["a"]`)
	}
}

func TestSchemaOrderDiff_MissingExtraKeysDoNotPanic(t *testing.T) {
	schema := schemaRootOnly("a", "b", "c")
	exp := json.RawMessage(`{"a":"1","b":"2","c":"3"}`)
	got := json.RawMessage(`{"a":"1","c":"3"}`)
	diffs, err := SchemaOrderDiff("side", schema, exp, got)
	if err != nil {
		t.Fatalf("SchemaOrderDiff: %v", err)
	}
	if len(diffs) != 1 {
		t.Fatalf("expected 1 diff (length mismatch surfaces key-list inequality), got %d", len(diffs))
	}
	if diffs[0].Path != "$" {
		t.Errorf("path: %q", diffs[0].Path)
	}
}

// TestSchemaOrderDiff_UnionWithoutChoiceReturnsUnsupported pins the
// union contract: when the order walker reaches a KindUnion node
// without a matching UnionChoices entry, SchemaOrderDiff returns
// ErrSchemaOrderUnsupported. The integration oracles promote this
// to a hard failure (see ErrSchemaOrderUnsupported's doc comment).
func TestSchemaOrderDiff_UnionWithoutChoiceReturnsUnsupported(t *testing.T) {
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
	_, err := SchemaOrderDiff("side", schema,
		json.RawMessage(`{"u":"x"}`),
		json.RawMessage(`{"u":"x"}`),
	)
	if !errors.Is(err, ErrSchemaOrderUnsupported) {
		t.Errorf("got err=%v, want errors.Is(err, ErrSchemaOrderUnsupported)", err)
	}
}

// TestSchemaOrderDiff_UnionWithChoiceWalksArm pins the positive
// path: given a recorded UnionChoices entry the walker descends into
// the named arm and continues the order check from there.
func TestSchemaOrderDiff_UnionWithChoiceWalksArm(t *testing.T) {
	schema := FuzzSchema{
		Classes: []FuzzClass{
			{
				Name: "Root",
				Properties: []FuzzProperty{
					{Name: "u", Type: FuzzType{
						Kind: KindUnion,
						Variants: []FuzzType{
							{Kind: KindClassRef, Ref: "Inner"},
							{Kind: KindString},
						},
					}},
				},
			},
			{
				Name: "Inner",
				Properties: []FuzzProperty{
					{Name: "a", Type: FuzzType{Kind: KindInt}},
					{Name: "b", Type: FuzzType{Kind: KindInt}},
				},
			},
		},
		RootClass: "Root",
	}
	// The metadata key `.u` names the union node itself — the field
	// `u` on Root. Suffixed `:v` segments only appear for paths that
	// live INSIDE the picked arm (e.g. a nested union below `u` would
	// be at `.u:v...`); the outer union's own choice lives at the
	// unsuffixed field path.
	choices := map[string]UnionChoice{
		".u": {Index: 0, Kind: KindClassRef, Ref: "Inner", VariantCount: 2},
	}
	diffs, err := SchemaOrderDiffWithChoices("side", schema,
		json.RawMessage(`{"u":{"a":1,"b":2}}`),
		json.RawMessage(`{"u":{"b":2,"a":1}}`),
		choices,
	)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(diffs) != 1 {
		t.Fatalf("expected 1 diff in the Inner class, got %d", len(diffs))
	}
}

// TestSchemaOrderDiff_UnionStaleVariantCountFailsClosed asserts the
// order walker bails when the recorded UnionChoice.VariantCount does
// not match the schema's current variant slice. Stale corpus/replay
// metadata after a schema-side variant reorder must surface as
// ErrSchemaOrderUnsupported, not silent traversal of the wrong arm.
func TestSchemaOrderDiff_UnionStaleVariantCountFailsClosed(t *testing.T) {
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
	choices := map[string]UnionChoice{
		// Stale: schema has 2 arms, choice claims 3.
		".u": {Index: 0, Kind: KindString, VariantCount: 3},
	}
	_, err := SchemaOrderDiffWithChoices("side", schema,
		json.RawMessage(`{"u":"x"}`),
		json.RawMessage(`{"u":"x"}`),
		choices,
	)
	if !errors.Is(err, ErrSchemaOrderUnsupported) {
		t.Errorf("stale variant count should be unsupported, got %v", err)
	}
}

// TestSchemaOrderDiff_UnionStaleKindFailsClosed asserts the order
// walker bails when the recorded UnionChoice.Kind disagrees with the
// kind of the selected variant in the current schema.
func TestSchemaOrderDiff_UnionStaleKindFailsClosed(t *testing.T) {
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
	choices := map[string]UnionChoice{
		// Stale: index 0 is string in the schema but the metadata
		// records it as int (the variants were reordered).
		".u": {Index: 0, Kind: KindInt, VariantCount: 2},
	}
	_, err := SchemaOrderDiffWithChoices("side", schema,
		json.RawMessage(`{"u":"x"}`),
		json.RawMessage(`{"u":"x"}`),
		choices,
	)
	if !errors.Is(err, ErrSchemaOrderUnsupported) {
		t.Errorf("stale arm kind should be unsupported, got %v", err)
	}
}

// TestSchemaOrderDiff_UnionStaleRefFailsClosed asserts the order
// walker bails when the recorded UnionChoice.Ref differs from the
// selected variant's class/enum ref name. A class rename or swap
// upstream of the corpus would otherwise let the walker traverse the
// wrong target.
func TestSchemaOrderDiff_UnionStaleRefFailsClosed(t *testing.T) {
	schema := FuzzSchema{
		Classes: []FuzzClass{
			{
				Name: "Root",
				Properties: []FuzzProperty{
					{Name: "u", Type: FuzzType{
						Kind: KindUnion,
						Variants: []FuzzType{
							{Kind: KindClassRef, Ref: "Inner"},
							{Kind: KindString},
						},
					}},
				},
			},
			{
				Name: "Inner",
				Properties: []FuzzProperty{
					{Name: "a", Type: FuzzType{Kind: KindInt}},
				},
			},
		},
		RootClass: "Root",
	}
	choices := map[string]UnionChoice{
		// Stale: schema points the class-ref arm at "Inner" but the
		// metadata still names "Other" from a prior schema.
		".u": {Index: 0, Kind: KindClassRef, Ref: "Other", VariantCount: 2},
	}
	_, err := SchemaOrderDiffWithChoices("side", schema,
		json.RawMessage(`{"u":{"a":1}}`),
		json.RawMessage(`{"u":{"a":1}}`),
		choices,
	)
	if !errors.Is(err, ErrSchemaOrderUnsupported) {
		t.Errorf("stale arm ref should be unsupported, got %v", err)
	}
}

func TestSchemaOrderDiff_MalformedJSONIsDecodeError(t *testing.T) {
	schema := schemaRootOnly("a", "b")
	if _, err := SchemaOrderDiff("side", schema,
		json.RawMessage(`not json`),
		json.RawMessage(`{"a":1,"b":2}`)); err == nil {
		t.Error("malformed expected: want error, got nil")
	} else if !strings.Contains(err.Error(), "decode expected") {
		t.Errorf("malformed expected: error should mention decode expected, got %v", err)
	}
	if _, err := SchemaOrderDiff("side", schema,
		json.RawMessage(`{"a":1,"b":2}`),
		json.RawMessage(`{"a":1`)); err == nil {
		t.Error("malformed actual: want error, got nil")
	} else if !strings.Contains(err.Error(), "decode actual") {
		t.Errorf("malformed actual: error should mention decode actual, got %v", err)
	}
}

func TestSchemaOrderDiff_DuplicateKeysAreDecodeError(t *testing.T) {
	schema := schemaRootOnly("a", "b")
	_, err := SchemaOrderDiff("side", schema,
		json.RawMessage(`{"a":1,"a":2,"b":3}`),
		json.RawMessage(`{"a":1,"b":2}`),
	)
	if err == nil {
		t.Fatal("duplicate keys: want error, got nil")
	}
	if !errors.Is(err, bamlutils.ErrOrderedMapDuplicateKey) {
		t.Errorf("duplicate keys: err should wrap ErrOrderedMapDuplicateKey, got %v", err)
	}
}

func TestSchemaOrderDiff_EmptyPayloadIsDecodeError(t *testing.T) {
	schema := schemaRootOnly("a", "b")
	if _, err := SchemaOrderDiff("side", schema, json.RawMessage{}, json.RawMessage(`{"a":1,"b":2}`)); err == nil {
		t.Error("empty expected: want error, got nil")
	}
	if _, err := SchemaOrderDiff("side", schema, json.RawMessage(`{"a":1,"b":2}`), json.RawMessage{}); err == nil {
		t.Error("empty actual: want error, got nil")
	}
}

func TestSchemaOrderDiff_RejectsMissingRootClass(t *testing.T) {
	// Schema lists no RootClass.
	schema := FuzzSchema{Classes: []FuzzClass{{Name: "Root"}}}
	if _, err := SchemaOrderDiff("side", schema, []byte(`{}`), []byte(`{}`)); err == nil {
		t.Error("missing RootClass: want error, got nil")
	}

	// RootClass set but the class isn't declared.
	schema = FuzzSchema{RootClass: "Ghost"}
	if _, err := SchemaOrderDiff("side", schema, []byte(`{}`), []byte(`{}`)); err == nil {
		t.Error("undeclared RootClass: want error, got nil")
	}
}

func TestFormatSchemaOrderDiffs(t *testing.T) {
	if FormatSchemaOrderDiffs(nil) != nil {
		t.Error("nil input must produce nil output")
	}
	if FormatSchemaOrderDiffs([]SchemaOrderDiffEntry{}) != nil {
		t.Error("empty input must produce nil output")
	}
	out := FormatSchemaOrderDiffs([]SchemaOrderDiffEntry{
		{Side: "expected_vs_rest", Path: "$.inner", Expected: []string{"a", "b"}, Actual: []string{"b", "a"}},
	})
	if len(out) != 1 {
		t.Fatalf("expected 1 line, got %d", len(out))
	}
	if !strings.Contains(out[0], "expected_vs_rest") || !strings.Contains(out[0], "$.inner") {
		t.Errorf("line does not embed side+path: %q", out[0])
	}
}

// TestSchemaOrderDiffMapInsertionOrderMismatch pins the D1 map policy
// directly: insertion-order swap surfaces a single diff entry at the
// map node's own path.
func TestSchemaOrderDiffMapInsertionOrderMismatch(t *testing.T) {
	schema := FuzzSchema{
		Classes: []FuzzClass{{
			Name: "Root",
			Properties: []FuzzProperty{
				{Name: "m", Type: FuzzType{
					Kind:  KindMap,
					Key:   &FuzzType{Kind: KindString},
					Inner: &FuzzType{Kind: KindString},
				}},
			},
		}},
		RootClass: "Root",
	}
	exp := json.RawMessage(`{"m":{"kA":"a","kB":"b"}}`)
	got := json.RawMessage(`{"m":{"kB":"b","kA":"a"}}`)
	diffs, err := SchemaOrderDiff("side", schema, exp, got)
	if err != nil {
		t.Fatalf("SchemaOrderDiff: %v", err)
	}
	if len(diffs) != 1 {
		t.Fatalf("expected 1 diff, got %d (%v)", len(diffs), diffs)
	}
	if diffs[0].Path != "$.m" {
		t.Errorf("path: got %q want %q", diffs[0].Path, "$.m")
	}
	if !keysEqual(diffs[0].Expected, []string{"kA", "kB"}) {
		t.Errorf("expected keys: got %v want %v", diffs[0].Expected, []string{"kA", "kB"})
	}
	if !keysEqual(diffs[0].Actual, []string{"kB", "kA"}) {
		t.Errorf("actual keys: got %v want %v", diffs[0].Actual, []string{"kB", "kA"})
	}
}

// TestSchemaOrderDiffMapWithNestedClassPropagates pins that map-key
// order and nested-class-property order are asserted independently
// at distinct paths.
func TestSchemaOrderDiffMapWithNestedClassPropagates(t *testing.T) {
	schema := FuzzSchema{
		Classes: []FuzzClass{
			{
				Name: "Root",
				Properties: []FuzzProperty{
					{Name: "m", Type: mapOf(classRef("Inner"))},
				},
			},
			{
				Name: "Inner",
				Properties: []FuzzProperty{
					{Name: "a", Type: FuzzType{Kind: KindInt}},
					{Name: "b", Type: FuzzType{Kind: KindInt}},
				},
			},
		},
		RootClass: "Root",
	}

	// Variant 1: map order kept, inner class order swapped → one
	// diff at $.m["kA"] for the class.
	exp := json.RawMessage(`{"m":{"kA":{"a":1,"b":2}}}`)
	got := json.RawMessage(`{"m":{"kA":{"b":2,"a":1}}}`)
	diffs, err := SchemaOrderDiff("side", schema, exp, got)
	if err != nil {
		t.Fatalf("variant 1: SchemaOrderDiff: %v", err)
	}
	if len(diffs) != 1 {
		t.Fatalf("variant 1: expected 1 diff, got %d (%v)", len(diffs), diffs)
	}
	if diffs[0].Path != `$.m["kA"]` {
		t.Errorf("variant 1: path: got %q want %q", diffs[0].Path, `$.m["kA"]`)
	}

	// Variant 2: map order swapped, inner class order kept → one
	// diff at $.m for the map.
	exp = json.RawMessage(`{"m":{"kA":{"a":1,"b":2},"kB":{"a":3,"b":4}}}`)
	got = json.RawMessage(`{"m":{"kB":{"a":3,"b":4},"kA":{"a":1,"b":2}}}`)
	diffs, err = SchemaOrderDiff("side", schema, exp, got)
	if err != nil {
		t.Fatalf("variant 2: SchemaOrderDiff: %v", err)
	}
	if len(diffs) != 1 {
		t.Fatalf("variant 2: expected 1 diff, got %d (%v)", len(diffs), diffs)
	}
	if diffs[0].Path != "$.m" {
		t.Errorf("variant 2: path: got %q want %q", diffs[0].Path, "$.m")
	}
}

// TestSchemaOrderDiffMapSameOrderPasses asserts identical insertion
// order on both sides produces no diff entries and no error.
func TestSchemaOrderDiffMapSameOrderPasses(t *testing.T) {
	schema := FuzzSchema{
		Classes: []FuzzClass{{
			Name: "Root",
			Properties: []FuzzProperty{
				{Name: "m", Type: mapOf(FuzzType{Kind: KindString})},
			},
		}},
		RootClass: "Root",
	}
	exp := json.RawMessage(`{"m":{"kA":"a","kB":"b"}}`)
	got := json.RawMessage(`{"m":{"kA":"a","kB":"b"}}`)
	diffs, err := SchemaOrderDiff("side", schema, exp, got)
	if err != nil {
		t.Fatalf("SchemaOrderDiff: %v", err)
	}
	if len(diffs) != 0 {
		t.Errorf("expected no diffs, got %v", diffs)
	}
}

// TestSchemaOrderDiffMapStructuralDisagreementSilent pins that the
// walker silently bails when the two sides disagree on shape at a map
// node — SemanticDiff is the gate for structural mismatches.
func TestSchemaOrderDiffMapStructuralDisagreementSilent(t *testing.T) {
	schema := FuzzSchema{
		Classes: []FuzzClass{{
			Name: "Root",
			Properties: []FuzzProperty{
				{Name: "m", Type: mapOf(FuzzType{Kind: KindString})},
			},
		}},
		RootClass: "Root",
	}
	exp := json.RawMessage(`{"m":{"k":"v"}}`)
	got := json.RawMessage(`{"m":null}`)
	diffs, err := SchemaOrderDiff("side", schema, exp, got)
	if err != nil {
		t.Fatalf("SchemaOrderDiff: %v", err)
	}
	if len(diffs) != 0 {
		t.Errorf("structural disagreement should be silent, got %v", diffs)
	}
}

// TestSchemaOrderDiffUnionMissingChoiceReturnsUnsupported pins that
// a union-rooted schema with zero recorded choices still returns
// ErrSchemaOrderUnsupported. Exercises the hard-fail wiring end to
// end: any future regression that strips a choice from the metadata
// map will surface here.
func TestSchemaOrderDiffUnionMissingChoiceReturnsUnsupported(t *testing.T) {
	schema := FuzzSchema{
		RootType: &FuzzType{
			Kind: KindUnion,
			Variants: []FuzzType{
				{Kind: KindString},
				{Kind: KindInt},
			},
		},
	}
	_, err := SchemaOrderDiff("side", schema,
		json.RawMessage(`"x"`),
		json.RawMessage(`"x"`),
	)
	if !errors.Is(err, ErrSchemaOrderUnsupported) {
		t.Errorf("got err=%v, want errors.Is(err, ErrSchemaOrderUnsupported)", err)
	}
}

// TestSchemaOrderDiff_ToleratesExtraNullKeysInClass pins the
// boundaryml/baml#3690 workaround at a class node: an extra null-valued
// key on the actual side that expected does not carry must not register
// as a key-order mismatch.
func TestSchemaOrderDiff_ToleratesExtraNullKeysInClass(t *testing.T) {
	schema := schemaRootOnly("a", "b")
	exp := json.RawMessage(`{"a":"1","b":"2"}`)
	got := json.RawMessage(`{"a":"1","b":"2","Fuzz_field_0":null}`)
	diffs, err := SchemaOrderDiffWithChoices("side", schema, exp, got, nil)
	if err != nil {
		t.Fatalf("SchemaOrderDiff: %v", err)
	}
	if len(diffs) != 0 {
		t.Errorf("expected no diffs for leaked null key, got %v", diffs)
	}
}

// TestSchemaOrderDiff_NullKeyMissingFromActualStillDiffs pins the
// asymmetry: the workaround only forgives extra null keys ON the actual
// side. A null-valued key the expected side carries that the actual side
// dropped is a genuine missing field and still surfaces a diff.
func TestSchemaOrderDiff_NullKeyMissingFromActualStillDiffs(t *testing.T) {
	schema := schemaRootOnly("a", "b")
	exp := json.RawMessage(`{"a":"1","b":"2","leak":null}`)
	got := json.RawMessage(`{"a":"1","b":"2"}`)
	diffs, err := SchemaOrderDiffWithChoices("side", schema, exp, got, nil)
	if err != nil {
		t.Fatalf("SchemaOrderDiff: %v", err)
	}
	if len(diffs) != 1 {
		t.Fatalf("expected 1 diff for null key missing from actual, got %d (%v)", len(diffs), diffs)
	}
}

// TestSchemaOrderDiff_ExtraNonNullKeyInClassStillDiffs asserts the
// workaround is scoped to null keys: an extra non-null key remains an
// order mismatch.
func TestSchemaOrderDiff_ExtraNonNullKeyInClassStillDiffs(t *testing.T) {
	schema := schemaRootOnly("a", "b")
	exp := json.RawMessage(`{"a":"1","b":"2"}`)
	got := json.RawMessage(`{"a":"1","b":"2","extra":"3"}`)
	diffs, err := SchemaOrderDiffWithChoices("side", schema, exp, got, nil)
	if err != nil {
		t.Fatalf("SchemaOrderDiff: %v", err)
	}
	if len(diffs) != 1 {
		t.Fatalf("expected 1 diff for extra non-null key, got %d (%v)", len(diffs), diffs)
	}
}

// TestSchemaOrderDiff_ToleratesExtraNullKeysInMap pins the workaround at
// a map node: the leaked null key on the actual side is dropped before
// the insertion-order comparison.
func TestSchemaOrderDiff_ToleratesExtraNullKeysInMap(t *testing.T) {
	schema := FuzzSchema{
		Classes: []FuzzClass{{
			Name: "Root",
			Properties: []FuzzProperty{
				{Name: "m", Type: mapOf(FuzzType{Kind: KindInt})},
			},
		}},
		RootClass: "Root",
	}
	exp := json.RawMessage(`{"m":{"k0":-26}}`)
	got := json.RawMessage(`{"m":{"Fuzz_field_0":null,"k0":-26}}`)
	diffs, err := SchemaOrderDiffWithChoices("side", schema, exp, got, nil)
	if err != nil {
		t.Fatalf("SchemaOrderDiff: %v", err)
	}
	if len(diffs) != 0 {
		t.Errorf("expected no diffs for leaked null map key, got %v", diffs)
	}

	// An extra non-null map key remains a mismatch.
	got = json.RawMessage(`{"m":{"extra":1,"k0":-26}}`)
	diffs, err = SchemaOrderDiffWithChoices("side", schema, exp, got, nil)
	if err != nil {
		t.Fatalf("SchemaOrderDiff: %v", err)
	}
	if len(diffs) != 1 {
		t.Fatalf("expected 1 diff for extra non-null map key, got %d (%v)", len(diffs), diffs)
	}

	// Asymmetry: a null map key the expected side carries that the
	// actual side dropped is a genuine missing entry and still diffs.
	expMissing := json.RawMessage(`{"m":{"leak":null,"k0":-26}}`)
	gotMissing := json.RawMessage(`{"m":{"k0":-26}}`)
	diffs, err = SchemaOrderDiffWithChoices("side", schema, expMissing, gotMissing, nil)
	if err != nil {
		t.Fatalf("SchemaOrderDiff: %v", err)
	}
	if len(diffs) != 1 {
		t.Fatalf("expected 1 diff for null map key missing from actual, got %d (%v)", len(diffs), diffs)
	}
}

// TestSchemaOrderDiffWithChoices_UnionMapArmToleratesLeakedNull mirrors
// the exact boundaryml/baml#3690 shape: a `class | map<string,int>`
// union whose active map arm comes back with a leaked null field from
// the class arm. The order walker descends into the map arm and the
// leaked key is tolerated.
func TestSchemaOrderDiffWithChoices_UnionMapArmToleratesLeakedNull(t *testing.T) {
	schema := FuzzSchema{
		Classes: []FuzzClass{{
			Name: "FuzzClass1",
			Properties: []FuzzProperty{
				{Name: "Fuzz_field_0", Type: optionalOf(FuzzType{Kind: KindFloat})},
			},
		}},
		RootType: &FuzzType{
			Kind: KindUnion,
			Variants: []FuzzType{
				classRef("FuzzClass1"),
				mapOf(FuzzType{Kind: KindInt}),
			},
		},
	}
	choices := map[string]UnionChoice{
		"": {Index: 1, Kind: KindMap, VariantCount: 2},
	}
	exp := json.RawMessage(`{"k0":-26}`)
	got := json.RawMessage(`{"Fuzz_field_0":null,"k0":-26}`)
	diffs, err := SchemaOrderDiffWithChoices("side", schema, exp, got, choices)
	if err != nil {
		t.Fatalf("SchemaOrderDiff: %v", err)
	}
	if len(diffs) != 0 {
		t.Errorf("expected no diffs for leaked null key in map arm, got %v", diffs)
	}
}

// TestSchemaOrderDiffParity_ToleratesExtraNullEitherSide pins the
// symmetric #3690 order tolerance for actual-vs-actual parity checks: a
// leaked null key on either side is dropped before the key-order
// comparison, so it does not register as a mismatch. The asymmetric
// variant flags an a-side-only null (see
// TestSchemaOrderDiff_NullKeyMissingFromActualStillDiffs); parity does
// not.
func TestSchemaOrderDiffParity_ToleratesExtraNullEitherSide(t *testing.T) {
	schema := schemaRootOnly("a", "b")
	cases := []struct {
		name   string
		a, b   string
		parity bool
		want   int
	}{
		{"null on a only, parity tolerates", `{"a":"1","b":"2","leak":null}`, `{"a":"1","b":"2"}`, true, 0},
		{"null on b only, parity tolerates", `{"a":"1","b":"2"}`, `{"a":"1","b":"2","leak":null}`, true, 0},
		{"null on a only, asymmetric flags", `{"a":"1","b":"2","leak":null}`, `{"a":"1","b":"2"}`, false, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var (
				diffs []SchemaOrderDiffEntry
				err   error
			)
			if c.parity {
				diffs, err = SchemaOrderDiffParityWithChoices("side", schema, json.RawMessage(c.a), json.RawMessage(c.b), nil)
			} else {
				diffs, err = SchemaOrderDiffWithChoices("side", schema, json.RawMessage(c.a), json.RawMessage(c.b), nil)
			}
			if err != nil {
				t.Fatalf("SchemaOrderDiff: %v", err)
			}
			if len(diffs) != c.want {
				t.Errorf("expected %d diffs, got %d (%v)", c.want, len(diffs), diffs)
			}
		})
	}
}

// TestSchemaOrderDiffParity_ExtraNonNullKeyStillDiffs asserts the parity
// order tolerance is scoped to null keys: an extra non-null key on either
// side remains a mismatch.
func TestSchemaOrderDiffParity_ExtraNonNullKeyStillDiffs(t *testing.T) {
	schema := schemaRootOnly("a", "b")
	exp := json.RawMessage(`{"a":"1","b":"2","extra":"3"}`)
	got := json.RawMessage(`{"a":"1","b":"2"}`)
	diffs, err := SchemaOrderDiffParityWithChoices("side", schema, exp, got, nil)
	if err != nil {
		t.Fatalf("SchemaOrderDiff: %v", err)
	}
	if len(diffs) != 1 {
		t.Fatalf("expected 1 diff for extra non-null key, got %d (%v)", len(diffs), diffs)
	}
}

func keysEqual(a, b []string) bool {
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
