package baml

import (
	"reflect"

	"github.com/invakid404/baml-rest/dynclient/baml-patched/engine/language_client_go/baml_go/serde"
	"github.com/invakid404/baml-rest/dynclient/baml-patched/engine/language_client_go/baml_go/shared"
	"github.com/invakid404/baml-rest/dynclient/baml-patched/engine/language_client_go/pkg/cffi"
)

func EncodeClass(name string, fields map[string]any, dynamicFields *map[string]any) (*cffi.HostValue, error) {
	return serde.EncodeClass(name, fields, dynamicFields)
}

func EncodeEnum(name string, value string, is_dynamic bool) (*cffi.HostValue, error) {
	return serde.EncodeEnum(name, value, is_dynamic)
}

func EncodeValue(value any) (*cffi.HostValue, error) {
	return serde.EncodeValue(value)
}

func Decode(holder *cffi.CFFIValueHolder) reflect.Value {
	raw_decoded_data, _ := serde.Decode(holder, typeMap)
	return raw_decoded_data
}

func DecodeToValue(holder *cffi.CFFIValueHolder) any {
	raw_decoded_data, goType := serde.Decode(holder, typeMap)

	if !raw_decoded_data.IsValid() {
		return nil
	}

	// If int, bool, float, string, return the value directly
	if goType == reflect.TypeOf(int64(0)) {
		return raw_decoded_data.Int()
	}
	if goType == reflect.TypeOf(float64(0)) {
		return raw_decoded_data.Float()
	}
	if goType == reflect.TypeOf(false) {
		return raw_decoded_data.Bool()
	}
	return raw_decoded_data.Interface()
}

func BAMLTESTINGONLY_InternalEncode(value any) (*cffi.HostValue, error) {
	return serde.EncodeValue(value)
}

type TypeMap = serde.TypeMap
type Checked[T any] = shared.Checked[T]
type StreamState[T any] = shared.StreamState[T]
type StreamingStateType = shared.StreamStateType
type DynamicUnion = serde.DynamicUnion
type DynamicClass = serde.DynamicClass
type DynamicEnum = serde.DynamicEnum

const (
	StreamStatePending	= shared.StreamStatePending
	StreamStateIncomplete	= shared.StreamStateIncomplete
	StreamStateComplete	= shared.StreamStateComplete
)

// OrderedFields is the public alias for the ordered field map
// the patched runtime uses inside DynamicClass and CFFI map values.
// Generated @@dynamic clients reference this through baml.OrderedFields
// so dynamic outputs surface insertion order to baml-rest.
type OrderedFields = serde.OrderedFields

// NewOrderedFields allocates an empty OrderedFields with the
// supplied capacity hint.
func NewOrderedFields(capacity int) OrderedFields {
	return serde.NewOrderedFields(capacity)
}

// DecodeToOrderedValue is the order-preserving counterpart to
// DecodeToValue. Generated @@dynamic clients call this for each
// LLM-added property so nested dynamic class and CFFI map values
// retain CFFI insertion order before baml-rest assembles the
// response.
func DecodeToOrderedValue(holder *cffi.CFFIValueHolder) any {
	return serde.DecodeToOrderedValue(holder, typeMap)
}

// EncodeClassOrdered mirrors EncodeClass but accepts the ordered
// field map generated clients now use for DynamicProperties. The
// underlying serde.EncodeClass call still takes a map[string]any, so
// the helper materialises a plain map by iterating in insertion
// order — encode semantics do not depend on map order, but the
// iteration shape keeps the helper trivially auditable.
func EncodeClassOrdered(name string, fields map[string]any, dynamicFields *OrderedFields) (*cffi.HostValue, error) {
	if dynamicFields == nil {
		return serde.EncodeClass(name, fields, nil)
	}
	flattened := make(map[string]any, dynamicFields.Len())
	dynamicFields.Range(func(k string, v any) bool {
		flattened[k] = v
		return true
	})
	return serde.EncodeClass(name, fields, &flattened)
}

// OrderedMap is the public alias for the generic ordered map carrier
// used by statically-typed map<string, T> fields in the generated
// client. Generated code spells it `baml.OrderedMap[T]`; the
// value type stays concrete while iteration order matches BAML's CFFI
// emission order.
type OrderedMap[V any] = serde.OrderedMap[V]

// OrderedKV constructs a single ordered-map entry. Provided so
// generated initialisers can build `baml.OrderedMap[T]` literals
// without importing serde directly.
func OrderedKV[V any](key string, value V) serde.OrderedEntry[V] {
	return serde.OrderedKV(key, value)
}

// NewOrderedMap builds a typed ordered map from entries in source
// order. Returns the wrapped serde error on duplicate keys.
func NewOrderedMap[V any](entries ...serde.OrderedEntry[V]) (OrderedMap[V], error) {
	return serde.NewOrderedMap(entries...)
}
