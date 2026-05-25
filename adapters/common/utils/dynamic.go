package utils

import (
	"fmt"
	"reflect"

	"github.com/boundaryml/baml/engine/language_client_go/baml_go/serde"
	"github.com/invakid404/baml-rest/bamlutils"
)

// orderedFieldRanger is the structural interface the dynamic-order
// patched BAML runtime emits for ordered field carriers
// (serde.OrderedFields and the DynamicClass.Fields it now uses).
// Detecting the shape by interface keeps adapters/common
// source-compatible with the unpatched BAML module standalone modules
// pin — the type only resolves at runtime when the patched fork is in
// the link graph.
type orderedFieldRanger interface {
	Len() int
	Range(func(string, any) bool)
}

// orderedAnyRanger captures the broader shape any typed
// `bamlutils.OrderedMap[T]` exposes through its RangeAny method (issue
// #366). Typed static maps stay generic at the call site; this
// interface lets UnwrapDynamicValue recognise them without knowing the
// value type parameter. orderedFieldRanger above only matches the
// `OrderedMap[any]` specialisation that the patched serde produces;
// orderedAnyRanger picks up typed maps so generated code returning
// `baml.OrderedMap[Foo]` recursively unwraps into the JSON shape the
// rest of baml-rest expects.
type orderedAnyRanger interface {
	Len() int
	RangeAny(func(string, any) bool)
}

func UnwrapDynamicValue(value any) any {
	if value == nil {
		return nil
	}

	rv := reflect.ValueOf(value)
	if rv.Kind() == reflect.Ptr && rv.IsNil() {
		return nil
	}

	// Detect typed ordered maps (bamlutils.OrderedMap[T]) before the
	// any-specialised orderedFieldRanger probe, so typed static-map
	// fields (issue #366) recurse here and surface as ordered
	// OrderedMap[any] values with preserved insertion order. Typed
	// `OrderedMap[T]` exposes both Range(string, T) and
	// RangeAny(string, any); the latter is the structurally stable
	// shape across all V parameters.
	if ordered, ok := value.(orderedAnyRanger); ok {
		out := bamlutils.OrderedMap[any]{}
		ordered.RangeAny(func(key string, v any) bool {
			_ = out.Set(key, UnwrapDynamicValue(v))
			return true
		})
		return out
	}

	// Detect the patched ordered field carrier before the typed
	// serde.DynamicClass cases below, so an ordered DynamicClass.Fields
	// passed through recursion (or a top-level OrderedFields handed
	// back by DecodeToOrderedValue) lands here first and survives as a
	// bamlutils.OrderedMap[any] with preserved insertion order.
	if ordered, ok := value.(orderedFieldRanger); ok {
		out := bamlutils.OrderedMap[any]{}
		ordered.Range(func(key string, v any) bool {
			_ = out.Set(key, UnwrapDynamicValue(v))
			return true
		})
		return out
	}

	// Check pointer types
	if class, ok := value.(*serde.DynamicClass); ok {
		return UnwrapDynamicValue(class.Fields)
	}

	if enum, ok := value.(*serde.DynamicEnum); ok {
		return UnwrapDynamicValue(enum.Value)
	}

	if union, ok := value.(*serde.DynamicUnion); ok {
		return UnwrapDynamicValue(union.Value)
	}

	// Check value types (BAML returns these as values instead of pointers
	// when running through the modern CFFI shape)
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

	// Handle other slice types via reflection (BAML may return typed slices)
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

	// Handle typed maps via reflection (e.g. map[string]serde.DynamicClass for
	// `map<string, ref Class>`). Without this, typed maps fall through and
	// JSON-marshal the value type's exported fields, leaking the internal
	// {Name, Fields} / {Name, Value} wrappers.
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

	// Dereference a non-nil pointer to recurse on the pointee. This catches
	// shapes BAML produces for `optional(...)` (e.g. *[]serde.DynamicClass),
	// where the pointer wraps a typed slice/map whose elements need unwrapping.
	if rv.Kind() == reflect.Ptr {
		return UnwrapDynamicValue(rv.Elem().Interface())
	}

	return value
}

// stringifyMapKey converts a reflected map key to its string form. Dynamic
// outputs are string-keyed in practice; the %v fallback covers typed string
// aliases and any non-string comparable keys without panicking.
func stringifyMapKey(k reflect.Value) string {
	if k.Kind() == reflect.String {
		return k.String()
	}
	return fmt.Sprintf("%v", k.Interface())
}
