package utils

import (
	"reflect"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
)

// fakeOrderedFields is the package-local stand-in for the
// serde.OrderedFields type the patched BAML runtime exposes. Tests
// against adapters/common cannot reference serde.OrderedFields
// directly because the package is built against unpatched upstream
// BAML; the structural interface in UnwrapDynamicValue exists for
// exactly this case.
type fakeOrderedFields struct {
	keys []string
	vals map[string]any
}

func newFakeOrderedFields(entries ...[2]any) *fakeOrderedFields {
	f := &fakeOrderedFields{vals: map[string]any{}}
	for _, e := range entries {
		k := e[0].(string)
		f.keys = append(f.keys, k)
		f.vals[k] = e[1]
	}
	return f
}

func (f *fakeOrderedFields) Len() int { return len(f.keys) }

func (f *fakeOrderedFields) Range(fn func(string, any) bool) {
	for _, k := range f.keys {
		if !fn(k, f.vals[k]) {
			return
		}
	}
}

// TestUnwrapDynamicValue_StructuralOrdered verifies UnwrapDynamicValue
// detects the structural ordered shape (Len + Range) and produces a
// bamlutils.OrderedMap[any] whose insertion order matches Range's
// callback order. The fake is intentionally a different type from
// serde.OrderedFields so the test isolates the interface-based
// detection.
func TestUnwrapDynamicValue_StructuralOrdered(t *testing.T) {
	value := newFakeOrderedFields(
		[2]any{"zebra", "z"},
		[2]any{"alpha", "a"},
		[2]any{"middle", "m"},
	)

	got := UnwrapDynamicValue(value)

	ordered, ok := got.(bamlutils.OrderedMap[any])
	if !ok {
		t.Fatalf("expected bamlutils.OrderedMap[any], got %T", got)
	}
	want := []string{"zebra", "alpha", "middle"}
	if !reflect.DeepEqual(ordered.Keys(), want) {
		t.Fatalf("ordered keys mismatch\n got: %v\nwant: %v", ordered.Keys(), want)
	}
	for _, kv := range ordered.Entries() {
		expected := value.vals[kv.Key]
		if !reflect.DeepEqual(kv.Value, expected) {
			t.Fatalf("value for %s: got %#v want %#v", kv.Key, kv.Value, expected)
		}
	}
}

// TestUnwrapDynamicValue_TypedOrderedMap pins the issue #366 contract:
// a typed bamlutils.OrderedMap[T] (the carrier the static-map pass
// surfaces to generated client fields) is unwrapped into a
// bamlutils.OrderedMap[any] with preserved insertion order. The probe
// uses RangeAny, so the V parameter does not bleed into the
// detection.
func TestUnwrapDynamicValue_TypedOrderedMap(t *testing.T) {
	typed := bamlutils.MustOrderedMap(
		bamlutils.OrderedKV("gamma", "g"),
		bamlutils.OrderedKV("alpha", "a"),
		bamlutils.OrderedKV("beta", "b"),
	)

	got := UnwrapDynamicValue(typed)

	ordered, ok := got.(bamlutils.OrderedMap[any])
	if !ok {
		t.Fatalf("expected OrderedMap[any], got %T", got)
	}
	want := []string{"gamma", "alpha", "beta"}
	if !reflect.DeepEqual(ordered.Keys(), want) {
		t.Fatalf("keys mismatch\n got: %v\nwant: %v", ordered.Keys(), want)
	}
	for _, kv := range ordered.Entries() {
		want, _ := typed.Get(kv.Key)
		if !reflect.DeepEqual(kv.Value, want) {
			t.Fatalf("value for %q: got %#v want %#v", kv.Key, kv.Value, want)
		}
	}
}

// TestUnwrapDynamicValue_TypedOrderedMapNested covers the nested
// `OrderedMap[OrderedMap[T]]` shape generated code produces for
// `map<string, map<string, T>>`. Each level must preserve order and
// surface as OrderedMap[any] under the unwrap.
func TestUnwrapDynamicValue_TypedOrderedMapNested(t *testing.T) {
	inner := bamlutils.MustOrderedMap(
		bamlutils.OrderedKV("z", "Z"),
		bamlutils.OrderedKV("a", "A"),
	)
	outer := bamlutils.MustOrderedMap(
		bamlutils.OrderedKV("second", inner),
		bamlutils.OrderedKV("first", inner),
	)

	got := UnwrapDynamicValue(outer)

	o, ok := got.(bamlutils.OrderedMap[any])
	if !ok {
		t.Fatalf("outer: expected OrderedMap[any], got %T", got)
	}
	if want := []string{"second", "first"}; !reflect.DeepEqual(o.Keys(), want) {
		t.Fatalf("outer keys mismatch\n got: %v\nwant: %v", o.Keys(), want)
	}
	secVal, _ := o.Get("second")
	mid, ok := secVal.(bamlutils.OrderedMap[any])
	if !ok {
		t.Fatalf("nested: expected OrderedMap[any], got %T", secVal)
	}
	if want := []string{"z", "a"}; !reflect.DeepEqual(mid.Keys(), want) {
		t.Fatalf("nested keys mismatch\n got: %v\nwant: %v", mid.Keys(), want)
	}
}

// TestUnwrapDynamicValue_NestedOrdered covers the nested case the
// dynamic integration test exercises: ordered class containing an
// ordered map containing another ordered class. Each level must
// preserve the source order and emit a bamlutils.OrderedMap[any].
func TestUnwrapDynamicValue_NestedOrdered(t *testing.T) {
	innerClass := newFakeOrderedFields(
		[2]any{"delta", 4},
		[2]any{"bravo", 2},
	)
	innerMap := newFakeOrderedFields(
		[2]any{"keyZ", innerClass},
		[2]any{"keyA", "scalar"},
	)
	outerClass := newFakeOrderedFields(
		[2]any{"second", innerMap},
		[2]any{"first", "leading"},
	)

	got := UnwrapDynamicValue(outerClass)

	outer, ok := got.(bamlutils.OrderedMap[any])
	if !ok {
		t.Fatalf("outer: expected OrderedMap[any], got %T", got)
	}
	if want := []string{"second", "first"}; !reflect.DeepEqual(outer.Keys(), want) {
		t.Fatalf("outer keys mismatch\n got: %v\nwant: %v", outer.Keys(), want)
	}
	secondVal, _ := outer.Get("second")
	mid, ok := secondVal.(bamlutils.OrderedMap[any])
	if !ok {
		t.Fatalf("second: expected OrderedMap[any], got %T", secondVal)
	}
	if want := []string{"keyZ", "keyA"}; !reflect.DeepEqual(mid.Keys(), want) {
		t.Fatalf("middle keys mismatch\n got: %v\nwant: %v", mid.Keys(), want)
	}
	zVal, _ := mid.Get("keyZ")
	innerOM, ok := zVal.(bamlutils.OrderedMap[any])
	if !ok {
		t.Fatalf("keyZ: expected OrderedMap[any], got %T", zVal)
	}
	if want := []string{"delta", "bravo"}; !reflect.DeepEqual(innerOM.Keys(), want) {
		t.Fatalf("inner keys mismatch\n got: %v\nwant: %v", innerOM.Keys(), want)
	}
}
