package utils

import (
	"reflect"
	"testing"

	"github.com/boundaryml/baml/engine/language_client_go/baml_go/serde"
)

// TestUnwrapDynamicValue_OptionalListRefClass covers the shape BAML returns
// for `optional(list(ref Class))`: a non-nil *[]serde.DynamicClass pointing at
// a typed slice of dynamic classes (with a nested DynamicEnum field). Without
// the pointer-deref + typed-slice handling, the pointer falls through and
// JSON-marshals the wrappers, leaking {Name, Fields} / {Name, Value}.
func TestUnwrapDynamicValue_OptionalListRefClass(t *testing.T) {
	inner := []serde.DynamicClass{
		{
			Name: "BlockingIssueDetails",
			Fields: map[string]any{
				"description": "payment required",
				"type": serde.DynamicEnum{
					Name:  "BlockingIssueType",
					Value: "Payment_Required",
				},
			},
		},
	}
	got := UnwrapDynamicValue(&inner)

	want := []any{
		map[string]any{
			"description": "payment required",
			"type":        "Payment_Required",
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("optional(list(ref Class)) unwrap mismatch\n got: %#v\nwant: %#v", got, want)
	}
}

// TestUnwrapDynamicValue_OptionalListRefEnum covers `optional(list(ref Enum))`:
// a non-nil *[]serde.DynamicEnum. Each element must unwrap to its plain string
// value, not leak the {Name, Value} wrapper.
func TestUnwrapDynamicValue_OptionalListRefEnum(t *testing.T) {
	inner := []serde.DynamicEnum{
		{Name: "Status", Value: "ACTIVE"},
		{Name: "Status", Value: "PENDING"},
	}
	got := UnwrapDynamicValue(&inner)

	want := []any{"ACTIVE", "PENDING"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("optional(list(ref Enum)) unwrap mismatch\n got: %#v\nwant: %#v", got, want)
	}
}

// TestUnwrapDynamicValue_MapStringRefClass covers `map<string, ref Class>`:
// a typed map whose values are serde.DynamicClass. The reflective Map handler
// must recurse into each value and unwrap to plain map[string]any.
func TestUnwrapDynamicValue_MapStringRefClass(t *testing.T) {
	in := map[string]serde.DynamicClass{
		"i1": {
			Name: "Issue",
			Fields: map[string]any{
				"description": "first",
				"type": serde.DynamicEnum{
					Name:  "IssueType",
					Value: "Payment_Required",
				},
			},
		},
	}
	got := UnwrapDynamicValue(in)

	want := map[string]any{
		"i1": map[string]any{
			"description": "first",
			"type":        "Payment_Required",
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("map<string, ref Class> unwrap mismatch\n got: %#v\nwant: %#v", got, want)
	}
}

// TestUnwrapDynamicValue_MapStringRefEnum covers `map<string, ref Enum>`:
// a typed map whose values are serde.DynamicEnum. Each value must unwrap to a
// plain string.
func TestUnwrapDynamicValue_MapStringRefEnum(t *testing.T) {
	in := map[string]serde.DynamicEnum{
		"a": {Name: "Status", Value: "ACTIVE"},
		"b": {Name: "Status", Value: "PENDING"},
	}
	got := UnwrapDynamicValue(in)

	want := map[string]any{"a": "ACTIVE", "b": "PENDING"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("map<string, ref Enum> unwrap mismatch\n got: %#v\nwant: %#v", got, want)
	}
}

func TestUnwrapDynamicValue_OptionalMapStringRefClass(t *testing.T) {
	inner := map[string]serde.DynamicClass{
		"i1": {
			Name: "Issue",
			Fields: map[string]any{
				"description": "from optional map",
				"type": serde.DynamicEnum{
					Name:  "IssueType",
					Value: "Payment_Required",
				},
			},
		},
	}
	got := UnwrapDynamicValue(&inner)

	want := map[string]any{
		"i1": map[string]any{
			"description": "from optional map",
			"type":        "Payment_Required",
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("optional(map<string, ref Class>) unwrap mismatch\n got: %#v\nwant: %#v", got, want)
	}
}

func TestUnwrapDynamicValue_ListMapStringRefEnum(t *testing.T) {
	in := []map[string]serde.DynamicEnum{
		{
			"a": {Name: "Status", Value: "ACTIVE"},
			"b": {Name: "Status", Value: "PENDING"},
		},
	}
	got := UnwrapDynamicValue(in)

	want := []any{
		map[string]any{"a": "ACTIVE", "b": "PENDING"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("list(map<string, ref Enum>) unwrap mismatch\n got: %#v\nwant: %#v", got, want)
	}
}

func TestUnwrapDynamicValue_MapStringListRefClass(t *testing.T) {
	in := map[string][]serde.DynamicClass{
		"open": {
			{
				Name: "Issue",
				Fields: map[string]any{
					"description": "nested list",
					"type": serde.DynamicEnum{
						Name:  "IssueType",
						Value: "Payment_Required",
					},
				},
			},
		},
	}
	got := UnwrapDynamicValue(in)

	want := map[string]any{
		"open": []any{
			map[string]any{
				"description": "nested list",
				"type":        "Payment_Required",
			},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("map<string, list(ref Class)> unwrap mismatch\n got: %#v\nwant: %#v", got, want)
	}
}

func TestUnwrapDynamicValue_DynamicUnionMapStringRefEnum(t *testing.T) {
	in := serde.DynamicUnion{
		Value: map[string]serde.DynamicEnum{
			"a": {Name: "Status", Value: "ACTIVE"},
			"b": {Name: "Status", Value: "PENDING"},
		},
	}
	got := UnwrapDynamicValue(in)

	want := map[string]any{"a": "ACTIVE", "b": "PENDING"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("union(map<string, ref Enum>) unwrap mismatch\n got: %#v\nwant: %#v", got, want)
	}
}

func TestUnwrapDynamicValue_NonStringMapKeyFallback(t *testing.T) {
	in := map[int]serde.DynamicEnum{
		7: {Name: "Status", Value: "ACTIVE"},
	}
	got := UnwrapDynamicValue(in)

	want := map[string]any{"7": "ACTIVE"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("non-string map key fallback mismatch\n got: %#v\nwant: %#v", got, want)
	}
}

// TestUnwrapDynamicValue_NilPointer ensures the pre-existing nil-pointer
// short-circuit still wins over the new pointer-deref branch.
func TestUnwrapDynamicValue_NilPointer(t *testing.T) {
	var p *[]serde.DynamicClass
	if got := UnwrapDynamicValue(p); got != nil {
		t.Fatalf("nil *[]DynamicClass should unwrap to nil, got %#v", got)
	}
}

func TestUnwrapDynamicValue_PointerToTypedNilSlicePreservesNil(t *testing.T) {
	var inner []serde.DynamicClass
	got := UnwrapDynamicValue(&inner)
	if !isNilLike(got) {
		t.Fatalf("pointer to typed nil slice should preserve nil semantics, got %#v", got)
	}
}

func TestUnwrapDynamicValue_TypedNilMapPreservesNil(t *testing.T) {
	var in map[string]serde.DynamicClass
	got := UnwrapDynamicValue(in)
	if !isNilLike(got) {
		t.Fatalf("typed nil map should preserve nil semantics, got %#v", got)
	}
}

func isNilLike(value any) bool {
	if value == nil {
		return true
	}
	rv := reflect.ValueOf(value)
	switch rv.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Ptr, reflect.Slice:
		return rv.IsNil()
	default:
		return false
	}
}
