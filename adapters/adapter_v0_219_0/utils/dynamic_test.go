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

// TestUnwrapDynamicValue_NilPointer ensures the pre-existing nil-pointer
// short-circuit still wins over the new pointer-deref branch.
func TestUnwrapDynamicValue_NilPointer(t *testing.T) {
	var p *[]serde.DynamicClass
	if got := UnwrapDynamicValue(p); got != nil {
		t.Fatalf("nil *[]DynamicClass should unwrap to nil, got %#v", got)
	}
}
