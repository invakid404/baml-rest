package utils

import (
	"fmt"
	"reflect"

	"github.com/boundaryml/baml/engine/language_client_go/baml_go/serde"
)

func UnwrapDynamicValue(value any) any {
	if value == nil {
		return nil
	}

	rv := reflect.ValueOf(value)
	if rv.Kind() == reflect.Ptr && rv.IsNil() {
		return nil
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

	// Check value types (BAML may return these as values instead of pointers)
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
