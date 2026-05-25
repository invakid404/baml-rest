package serde

import (
	"bytes"
	"errors"
	"fmt"
	"iter"

	"github.com/bytedance/sonic"
	"github.com/bytedance/sonic/ast"
)

// ErrOrderedMapDuplicateKey is the sentinel returned (wrapped in
// OrderedMapKeyError) when an insert-only operation receives a key that
// already exists in the map. Set and SetFront return it; NewOrderedMap
// and JSON unmarshal surface it for duplicate entries in input data.
// Use errors.Is to detect.
var ErrOrderedMapDuplicateKey = errors.New("serde: ordered map duplicate key")

// ErrOrderedMapMissingKey is the sentinel returned (wrapped in
// OrderedMapKeyError) when Replace is invoked on a key that is not
// already present. Use errors.Is to detect.
var ErrOrderedMapMissingKey = errors.New("serde: ordered map missing key")

// OrderedMapKeyError reports an OrderedMap operation that failed because
// of a key-level invariant (duplicate insert, missing replace). The Key
// field carries the offending key; Unwrap exposes the sentinel
// (ErrOrderedMapDuplicateKey or ErrOrderedMapMissingKey).
type OrderedMapKeyError struct {
	Key string
	Err error
}

func (e *OrderedMapKeyError) Error() string {
	return fmt.Sprintf("%s: %q", e.Err.Error(), e.Key)
}

func (e *OrderedMapKeyError) Unwrap() error {
	return e.Err
}

// OrderedEntry is a (key, value) pair used to construct or iterate an
// OrderedMap with insertion order preserved.
type OrderedEntry[V any] struct {
	Key   string
	Value V
}

// OrderedMap is a map[string]V that remembers insertion order. The zero
// value is usable; methods lazily initialize the underlying storage.
// Insertion is strict: Set, SetFront, NewOrderedMap, and UnmarshalJSON
// all reject duplicate keys. Use Replace to update an existing value
// in-place.
type OrderedMap[V any] struct {
	keys []string
	vals map[string]V
}

// OrderedKV is a small constructor convenience for building
// NewOrderedMap argument lists.
func OrderedKV[V any](key string, value V) OrderedEntry[V] {
	return OrderedEntry[V]{Key: key, Value: value}
}

// NewOrderedMap builds an OrderedMap from entries in source order.
// Returns an OrderedMapKeyError wrapping ErrOrderedMapDuplicateKey on
// the first duplicate key.
func NewOrderedMap[V any](entries ...OrderedEntry[V]) (OrderedMap[V], error) {
	var m OrderedMap[V]
	for _, e := range entries {
		if err := m.Set(e.Key, e.Value); err != nil {
			return OrderedMap[V]{}, err
		}
	}
	return m, nil
}

// MustOrderedMap is the panicking variant of NewOrderedMap, intended
// for tests and trusted package-local construction where a duplicate
// key indicates a programmer bug.
func MustOrderedMap[V any](entries ...OrderedEntry[V]) OrderedMap[V] {
	m, err := NewOrderedMap(entries...)
	if err != nil {
		panic(err)
	}
	return m
}

// Set inserts a new key with value at the end of the order. Returns
// OrderedMapKeyError wrapping ErrOrderedMapDuplicateKey when key is
// already present; the map is left unchanged in that case.
func (m *OrderedMap[V]) Set(key string, value V) error {
	if _, exists := m.vals[key]; exists {
		return &OrderedMapKeyError{Key: key, Err: ErrOrderedMapDuplicateKey}
	}
	if m.vals == nil {
		m.vals = make(map[string]V)
	}
	m.vals[key] = value
	m.keys = append(m.keys, key)
	return nil
}

// Add is an alias for Set; provided for readability in call sites where
// "Add a new entry" reads more naturally than "Set".
func (m *OrderedMap[V]) Add(key string, value V) error {
	return m.Set(key, value)
}

// SetFront inserts a new key with value at index 0. Returns
// OrderedMapKeyError wrapping ErrOrderedMapDuplicateKey when key is
// already present; the map is left unchanged in that case.
func (m *OrderedMap[V]) SetFront(key string, value V) error {
	if _, exists := m.vals[key]; exists {
		return &OrderedMapKeyError{Key: key, Err: ErrOrderedMapDuplicateKey}
	}
	if m.vals == nil {
		m.vals = make(map[string]V)
	}
	m.vals[key] = value
	m.keys = append([]string{key}, m.keys...)
	return nil
}

// Replace updates the value of an existing key in place without
// changing its position. Returns OrderedMapKeyError wrapping
// ErrOrderedMapMissingKey when key is not already present.
func (m *OrderedMap[V]) Replace(key string, value V) error {
	if _, exists := m.vals[key]; !exists {
		return &OrderedMapKeyError{Key: key, Err: ErrOrderedMapMissingKey}
	}
	m.vals[key] = value
	return nil
}

// Delete removes the entry for key. Returns true if the key was present.
func (m *OrderedMap[V]) Delete(key string) bool {
	if _, ok := m.vals[key]; !ok {
		return false
	}
	delete(m.vals, key)
	for i, k := range m.keys {
		if k == key {
			m.keys = append(m.keys[:i], m.keys[i+1:]...)
			break
		}
	}
	return true
}

// Clear empties the map, leaving it in the zero state.
func (m *OrderedMap[V]) Clear() {
	m.keys = nil
	m.vals = nil
}

// Get returns the value for key and a presence flag.
func (m OrderedMap[V]) Get(key string) (V, bool) {
	v, ok := m.vals[key]
	return v, ok
}

// Has reports whether key is present.
func (m OrderedMap[V]) Has(key string) bool {
	_, ok := m.vals[key]
	return ok
}

// Len returns the number of entries.
func (m OrderedMap[V]) Len() int {
	return len(m.keys)
}

// Keys returns a copy of the key slice in insertion order.
func (m OrderedMap[V]) Keys() []string {
	if len(m.keys) == 0 {
		return nil
	}
	out := make([]string, len(m.keys))
	copy(out, m.keys)
	return out
}

// Entries returns a copy of the (key, value) pairs in insertion order.
func (m OrderedMap[V]) Entries() []OrderedEntry[V] {
	if len(m.keys) == 0 {
		return nil
	}
	out := make([]OrderedEntry[V], 0, len(m.keys))
	for _, k := range m.keys {
		out = append(out, OrderedEntry[V]{Key: k, Value: m.vals[k]})
	}
	return out
}

// Range invokes fn for each entry in insertion order. Iteration stops
// when fn returns false.
func (m OrderedMap[V]) Range(fn func(string, V) bool) {
	for _, k := range m.keys {
		if !fn(k, m.vals[k]) {
			return
		}
	}
}

// RangeAny invokes fn for each entry in insertion order with the value
// boxed to any. It exists so non-generic call sites (the dynamic-value
// unwrap pass, the codegen reflection scanner) can iterate ordered maps
// in the same shape they iterate serde.OrderedFields without knowing
// the V parameter. Iteration stops when fn returns false.
func (m OrderedMap[V]) RangeAny(fn func(string, any) bool) {
	for _, k := range m.keys {
		if !fn(k, m.vals[k]) {
			return
		}
	}
}

// All returns a range-over-func iterator over the map's entries in
// insertion order, for `for k, v := range m.All()` ergonomics.
func (m OrderedMap[V]) All() iter.Seq2[string, V] {
	return func(yield func(string, V) bool) {
		for _, k := range m.keys {
			if !yield(k, m.vals[k]) {
				return
			}
		}
	}
}

// Clone returns a shallow copy: the key slice and value map are
// duplicated, but pointed-to values are shared with the original.
func (m OrderedMap[V]) Clone() OrderedMap[V] {
	if len(m.keys) == 0 {
		return OrderedMap[V]{}
	}
	cloned := OrderedMap[V]{
		keys: make([]string, len(m.keys)),
		vals: make(map[string]V, len(m.vals)),
	}
	copy(cloned.keys, m.keys)
	for k, v := range m.vals {
		cloned.vals[k] = v
	}
	return cloned
}

// IsZero reports whether the map has no entries. Used by encoders that
// honor `json:",omitzero"`.
func (m OrderedMap[V]) IsZero() bool {
	return len(m.keys) == 0
}

// MarshalJSON emits a JSON object in insertion order. An empty map
// marshals as "{}"; parent structs that need omission should use
// `json:",omitempty,omitzero"` on the field.
func (m OrderedMap[V]) MarshalJSON() ([]byte, error) {
	if len(m.keys) == 0 {
		return []byte("{}"), nil
	}
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, k := range m.keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		keyBytes, err := sonic.Marshal(k)
		if err != nil {
			return nil, err
		}
		buf.Write(keyBytes)
		buf.WriteByte(':')
		valBytes, err := sonic.Marshal(m.vals[k])
		if err != nil {
			return nil, err
		}
		buf.Write(valBytes)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// UnmarshalJSON decodes a JSON object into the map, preserving wire
// order. Duplicate keys are rejected with an OrderedMapKeyError. JSON
// null clears the map to the zero value.
func (m *OrderedMap[V]) UnmarshalJSON(data []byte) error {
	res, err := unmarshalOrderedMap[V](data, "")
	if err != nil {
		return err
	}
	*m = res
	return nil
}

// unmarshalOrderedMap is the path-qualified decoder backing
// OrderedMap.UnmarshalJSON and the schema-specific decoders in
// dynamic.go. The optional path is prefixed to errors so callers
// retain the existing field-qualified error text quality.
//
// Walks the JSON via sonic's ast.Parser + Properties iterator: the
// iterator yields pairs in source order and preserves duplicate keys
// (sonic's linkedPairs stores every pushed pair without dedup), which
// is the property needed for strict duplicate-key rejection. Per-value
// decoding then hops back through sonic.Unmarshal on the raw substring.
//
// Sonic's ast.Visitor interface uses encoding/json.Number in its
// method signatures, so a direct visitor implementation would re-pin
// the stdlib import we are dropping; the Parser+Properties path is the
// equivalent streaming sonic-native traversal.
func unmarshalOrderedMap[V any](data []byte, path string) (OrderedMap[V], error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return OrderedMap[V]{}, nil
	}

	src := string(data)
	parser := ast.NewParser(src)
	node, errP := parser.Parse()
	if errP != 0 {
		return OrderedMap[V]{}, qualifyErr(path, parser.ExportError(errP))
	}
	if node.TypeSafe() != ast.V_OBJECT {
		return OrderedMap[V]{}, fmt.Errorf("%s%sexpected object", path, sep(path))
	}

	it, err := node.Properties()
	if err != nil {
		return OrderedMap[V]{}, qualifyErr(path, err)
	}

	var out OrderedMap[V]
	var pair ast.Pair
	for it.Next(&pair) {
		if out.Has(pair.Key) {
			keyErr := &OrderedMapKeyError{Key: pair.Key, Err: ErrOrderedMapDuplicateKey}
			if path == "" {
				return OrderedMap[V]{}, keyErr
			}
			// Wrap with %w so the path-qualified branch still surfaces
			// the ErrOrderedMapDuplicateKey sentinel under errors.Is and
			// the *OrderedMapKeyError carrier under errors.As, while
			// preserving the existing path-qualified error prose that
			// callers and tests assert against.
			return OrderedMap[V]{}, fmt.Errorf("%s: duplicate key %q: %w", path, pair.Key, keyErr)
		}
		raw, rawErr := pair.Value.Raw()
		if rawErr != nil {
			return OrderedMap[V]{}, qualifyField(path, pair.Key, rawErr)
		}
		var value V
		if err := sonic.Unmarshal([]byte(raw), &value); err != nil {
			return OrderedMap[V]{}, qualifyField(path, pair.Key, err)
		}
		if err := out.Set(pair.Key, value); err != nil {
			return OrderedMap[V]{}, err
		}
	}

	// Reject trailing bytes after the top-level object. sonic.Valid is
	// strict — it returns false when non-whitespace follows the top-level
	// value. ast.Parser produces a lazy node and doesn't surface its
	// internal position back to the external Parser, so a manual scan
	// from parser.Pos() doesn't work; sonic.Valid is the clean check.
	// This preserves the strict-input contract #318 locked in (stdjson's
	// Decoder was happy to accept `{"a":1} garbage`).
	if !sonic.Valid(data) {
		return OrderedMap[V]{}, qualifyErr(path, errors.New("trailing bytes after top-level object"))
	}

	return out, nil
}

func qualifyErr(path string, err error) error {
	if path == "" {
		return err
	}
	return fmt.Errorf("%s: %w", path, err)
}

func qualifyField(path, key string, err error) error {
	if path == "" {
		return fmt.Errorf("%s: %w", key, err)
	}
	return fmt.Errorf("%s.%s: %w", path, key, err)
}

func sep(path string) string {
	if path == "" {
		return ""
	}
	return ": "
}


// OrderedFields is the OrderedMap specialisation used by serde for
// dynamic class fields and CFFI map values. The alias keeps the
// rest of the serde package free of generic argument noise.
type OrderedFields = OrderedMap[any]

// NewOrderedFields constructs an empty OrderedFields with the
// supplied capacity hint. Mirrors the make(map[string]any, capacity)
// shape the unpatched runtime used to use.
func NewOrderedFields(capacity int) OrderedFields {
	return OrderedFields{
		keys: make([]string, 0, capacity),
		vals: make(map[string]any, capacity),
	}
}

