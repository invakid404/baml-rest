package adapter

import (
	"fmt"
	"reflect"

	"github.com/boundaryml/baml/engine/language_client_go/baml_go/serde"
	"github.com/boundaryml/baml/engine/language_client_go/pkg/cffi"
)

// DynamicValue is a wrapper type that implements InternalBamlSerializer
// to properly handle dynamic values (like nested maps) that would otherwise
// fail serialization because interface{} doesn't implement InternalBamlSerializer.
type DynamicValue struct {
	Value any
}

// InternalBamlSerializer is a marker method required by the interface.
func (d DynamicValue) InternalBamlSerializer() {}

// Encode serializes the wrapped value using our custom encoding that properly
// handles interface{} values which BAML's EncodeValue fails to process.
func (d DynamicValue) Encode() (*cffi.CFFIValueHolder, error) {
	return encodeAnyValue(d.Value)
}

// Type returns the field type for this dynamic value, which is "any" type.
func (d DynamicValue) Type() (*cffi.CFFIFieldTypeHolder, error) {
	return anyFieldType(), nil
}

// Ensure DynamicValue implements InternalBamlSerializer
var _ serde.InternalBamlSerializer = DynamicValue{}

// anyFieldType returns the CFFI field type for "any"
func anyFieldType() *cffi.CFFIFieldTypeHolder {
	return &cffi.CFFIFieldTypeHolder{
		Type: &cffi.CFFIFieldTypeHolder_AnyType{
			AnyType: &cffi.CFFIFieldTypeAny{},
		},
	}
}

// encodeAnyValue encodes any Go value to a CFFIValueHolder, properly handling
// interface{} values that BAML's encodeValue fails to process.
func encodeAnyValue(value any) (*cffi.CFFIValueHolder, error) {
	if value == nil {
		return &cffi.CFFIValueHolder{
			Type:  anyFieldType(),
			Value: &cffi.CFFIValueHolder_NullValue{},
		}, nil
	}

	// Handle DynamicValue by recursively encoding its inner value
	if dv, ok := value.(DynamicValue); ok {
		return encodeAnyValue(dv.Value)
	}

	rv := reflect.ValueOf(value)

	// Unwrap interface values - this is the key fix for BAML's bug
	for rv.Kind() == reflect.Interface {
		if rv.IsNil() {
			return &cffi.CFFIValueHolder{
				Type:  anyFieldType(),
				Value: &cffi.CFFIValueHolder_NullValue{},
			}, nil
		}
		rv = rv.Elem()
	}

	// Handle pointers
	for rv.Kind() == reflect.Ptr {
		if rv.IsNil() {
			return &cffi.CFFIValueHolder{
				Type:  anyFieldType(),
				Value: &cffi.CFFIValueHolder_NullValue{},
			}, nil
		}
		rv = rv.Elem()
	}

	// Now encode based on the actual concrete type
	switch rv.Kind() {
	case reflect.String:
		return &cffi.CFFIValueHolder{
			Type: anyFieldType(),
			Value: &cffi.CFFIValueHolder_StringValue{
				StringValue: rv.String(),
			},
		}, nil

	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return &cffi.CFFIValueHolder{
			Type: anyFieldType(),
			Value: &cffi.CFFIValueHolder_IntValue{
				IntValue: rv.Int(),
			},
		}, nil

	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return &cffi.CFFIValueHolder{
			Type: anyFieldType(),
			Value: &cffi.CFFIValueHolder_IntValue{
				IntValue: int64(rv.Uint()),
			},
		}, nil

	case reflect.Float32, reflect.Float64:
		return &cffi.CFFIValueHolder{
			Type: anyFieldType(),
			Value: &cffi.CFFIValueHolder_FloatValue{
				FloatValue: rv.Float(),
			},
		}, nil

	case reflect.Bool:
		return &cffi.CFFIValueHolder{
			Type: anyFieldType(),
			Value: &cffi.CFFIValueHolder_BoolValue{
				BoolValue: rv.Bool(),
			},
		}, nil

	case reflect.Slice, reflect.Array:
		return encodeAnyList(rv)

	case reflect.Map:
		return encodeAnyMap(rv)

	default:
		return nil, fmt.Errorf("unsupported type for dynamic encoding: %s (kind: %s)", rv.Type(), rv.Kind())
	}
}

// encodeAnyList encodes a slice or array to a CFFIValueHolder
func encodeAnyList(rv reflect.Value) (*cffi.CFFIValueHolder, error) {
	length := rv.Len()
	values := make([]*cffi.CFFIValueHolder, length)

	for i := 0; i < length; i++ {
		elem := rv.Index(i)
		encoded, err := encodeAnyValue(elem.Interface())
		if err != nil {
			return nil, fmt.Errorf("encoding list element %d: %w", i, err)
		}
		values[i] = encoded
	}

	return &cffi.CFFIValueHolder{
		Type: anyFieldType(),
		Value: &cffi.CFFIValueHolder_ListValue{
			ListValue: &cffi.CFFIValueList{
				Values:    values,
				ValueType: anyFieldType(),
			},
		},
	}, nil
}

// encodeAnyMap encodes a map to a CFFIValueHolder
func encodeAnyMap(rv reflect.Value) (*cffi.CFFIValueHolder, error) {
	if rv.Type().Key().Kind() != reflect.String {
		return nil, fmt.Errorf("map key type must be string, got %s", rv.Type().Key().Kind())
	}

	entries := make([]*cffi.CFFIMapEntry, 0, rv.Len())

	for _, key := range rv.MapKeys() {
		mapValue := rv.MapIndex(key)
		valueHolder, err := encodeAnyValue(mapValue.Interface())
		if err != nil {
			return nil, fmt.Errorf("encoding map value for key '%s': %w", key.String(), err)
		}
		entries = append(entries, &cffi.CFFIMapEntry{
			Key:   key.String(),
			Value: valueHolder,
		})
	}

	return &cffi.CFFIValueHolder{
		Type: anyFieldType(),
		Value: &cffi.CFFIValueHolder_MapValue{
			MapValue: &cffi.CFFIValueMap{
				Entries:   entries,
				KeyType:   anyFieldType(),
				ValueType: anyFieldType(),
			},
		},
	}, nil
}

// WrapMapValues recursively wraps all values in a map with DynamicValue
// to ensure proper serialization of nested maps and other dynamic values.
func WrapMapValues(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}

	result := make(map[string]any, len(m))
	for k, v := range m {
		result[k] = wrapValue(v)
	}
	return result
}

// wrapValue wraps a single value, recursively handling maps and slices.
func wrapValue(v any) any {
	if v == nil {
		return nil
	}

	rv := reflect.ValueOf(v)

	// Unwrap interface values first
	for rv.Kind() == reflect.Interface {
		if rv.IsNil() {
			return nil
		}
		rv = rv.Elem()
	}

	switch rv.Kind() {
	case reflect.Map:
		// Check if it's a map with string keys
		if rv.Type().Key().Kind() == reflect.String {
			// Create a new map with wrapped values
			newMap := make(map[string]any, rv.Len())
			for _, key := range rv.MapKeys() {
				newMap[key.String()] = wrapValue(rv.MapIndex(key).Interface())
			}
			return DynamicValue{Value: newMap}
		}
		// For non-string key maps, wrap as-is
		return DynamicValue{Value: rv.Interface()}

	case reflect.Slice, reflect.Array:
		// Wrap each element in the slice
		length := rv.Len()
		newSlice := make([]any, length)
		for i := 0; i < length; i++ {
			newSlice[i] = wrapValue(rv.Index(i).Interface())
		}
		return DynamicValue{Value: newSlice}

	case reflect.Ptr:
		if rv.IsNil() {
			return nil
		}
		// Wrap the pointed-to value
		return wrapValue(rv.Elem().Interface())

	default:
		// For primitive types (string, int, float, bool), wrap them directly
		return DynamicValue{Value: rv.Interface()}
	}
}
