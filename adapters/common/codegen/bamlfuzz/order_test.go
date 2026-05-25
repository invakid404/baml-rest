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

func TestSchemaOrderDiff_MapKeyReorderingIgnored(t *testing.T) {
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
	if len(diffs) != 0 {
		t.Errorf("map key reorder should be ignored, got %v", diffs)
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
// ErrSchemaOrderUnsupported and the caller falls back to
// semantic-only diagnostics.
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
