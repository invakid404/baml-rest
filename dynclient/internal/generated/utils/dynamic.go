// Package utils is the hand-written companion to
// dynclient/internal/generated. The codegen emitters in
// adapters/common/codegen reference UnwrapDynamicValue under
// `<SelfPkg>/utils`, so the generated adapter.go in this tree
// calls into this package.
//
// The body mirrors adapters/common/utils.UnwrapDynamicValue. It is
// not a re-export because the type assertions against serde.Dynamic*
// types must use the patched BAML fork's type identities — sharing
// the function with adapters/common would resolve those assertions
// against the upstream BAML module pinned by adapters/common's
// independent go.mod, and the cross-module identities would never
// match at runtime.
package utils

import (
	"fmt"
	"reflect"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/dynclient/baml-patched/engine/language_client_go/baml_go/serde"
)

// orderedFieldRanger is the structural interface the dynamic-order
// patched BAML runtime emits for ordered field carriers
// (serde.OrderedFields and the DynamicClass.Fields it now uses).
// Detection here mirrors adapters/common/utils so the two unwrap
// implementations behave identically against the patched fork's
// ordered shapes.
type orderedFieldRanger interface {
	Len() int
	Range(func(string, any) bool)
}

// orderedAnyRanger captures the broader shape any typed
// bamlutils.OrderedMap[T] exposes through its RangeAny method
// (issue #366). Typed static maps stay generic at the call site; this
// interface lets UnwrapDynamicValue recognise them without knowing
// the value type parameter.
type orderedAnyRanger interface {
	Len() int
	RangeAny(func(string, any) bool)
}

// UnwrapDynamicValue collapses BAML's serde wrapper types (DynamicClass /
// DynamicEnum / DynamicUnion) into plain Go values so the JSON encoder
// emits the BAML-shaped payload instead of the wrapper's internal
// fields. The traversal handles both pointer and value forms (BAML
// returns either depending on CFFI shape), homogeneous maps and slices
// of wrappers, and recursive pointer types BAML produces for optional
// fields.
func UnwrapDynamicValue(value any) any {
	if value == nil {
		return nil
	}

	rv := reflect.ValueOf(value)
	if rv.Kind() == reflect.Ptr && rv.IsNil() {
		return nil
	}

	// Typed static-map carrier (issue #366) — handled before the
	// any-specialised orderedFieldRanger probe so a typed
	// baml.OrderedMap[T] field surfaces as an ordered OrderedMap[any]
	// in the unwrapped output. The JSON encoder otherwise reaches the
	// unexported keys/vals fields and emits an empty object.
	if ordered, ok := value.(orderedAnyRanger); ok {
		out := bamlutils.OrderedMap[any]{}
		ordered.RangeAny(func(key string, v any) bool {
			_ = out.Set(key, UnwrapDynamicValue(v))
			return true
		})
		return out
	}

	if ordered, ok := value.(orderedFieldRanger); ok {
		out := bamlutils.OrderedMap[any]{}
		ordered.Range(func(key string, v any) bool {
			_ = out.Set(key, UnwrapDynamicValue(v))
			return true
		})
		return out
	}

	if class, ok := value.(*serde.DynamicClass); ok {
		return UnwrapDynamicValue(class.Fields)
	}
	if enum, ok := value.(*serde.DynamicEnum); ok {
		return UnwrapDynamicValue(enum.Value)
	}
	if union, ok := value.(*serde.DynamicUnion); ok {
		return UnwrapDynamicValue(union.Value)
	}

	if class, ok := value.(serde.DynamicClass); ok {
		return UnwrapDynamicValue(class.Fields)
	}
	if enum, ok := value.(serde.DynamicEnum); ok {
		return UnwrapDynamicValue(enum.Value)
	}
	if union, ok := value.(serde.DynamicUnion); ok {
		return UnwrapDynamicValue(union.Value)
	}

	if anyMap, ok := value.(map[string]any); ok {
		if anyMap == nil {
			return value
		}
		result := make(map[string]any, len(anyMap))
		for k, v := range anyMap {
			result[k] = UnwrapDynamicValue(v)
		}
		return result
	}

	if anySlice, ok := value.([]any); ok {
		if anySlice == nil {
			return value
		}
		result := make([]any, len(anySlice))
		for i, v := range anySlice {
			result[i] = UnwrapDynamicValue(v)
		}
		return result
	}

	if rv.Kind() == reflect.Slice {
		if rv.IsNil() {
			return value
		}
		result := make([]any, rv.Len())
		for i := 0; i < rv.Len(); i++ {
			result[i] = UnwrapDynamicValue(rv.Index(i).Interface())
		}
		return result
	}

	if rv.Kind() == reflect.Map {
		if rv.IsNil() {
			return value
		}
		result := make(map[string]any, rv.Len())
		iter := rv.MapRange()
		for iter.Next() {
			key := stringifyMapKey(iter.Key())
			result[key] = UnwrapDynamicValue(iter.Value().Interface())
		}
		return result
	}

	if rv.Kind() == reflect.Ptr {
		return UnwrapDynamicValue(rv.Elem().Interface())
	}

	return value
}

func stringifyMapKey(k reflect.Value) string {
	if k.Kind() == reflect.String {
		return k.String()
	}
	return fmt.Sprintf("%v", k.Interface())
}
