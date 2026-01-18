package utils

import (
	"reflect"

	"github.com/boundaryml/baml/engine/language_client_go/baml_go/serde"
)

func UnwrapDynamicValue(value any) any {
	if value == nil {
		return nil
	}

	if rv := reflect.ValueOf(value); rv.Kind() == reflect.Ptr && rv.IsNil() {
		return nil
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
		result := make(map[string]any, len(anyMap))
		for k, v := range anyMap {
			result[k] = UnwrapDynamicValue(v)
		}
		return result
	}

	if anySlice, ok := value.([]any); ok {
		result := make([]any, len(anySlice))
		for i, v := range anySlice {
			result[i] = UnwrapDynamicValue(v)
		}
		return result
	}

	return value
}
