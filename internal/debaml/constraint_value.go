package debaml

import (
	"bytes"
	"encoding/json"
	"fmt"
	"iter"
	"strconv"
	"strings"

	mjvalue "github.com/mitsuhiko/minijinja/minijinja-go/v2/value"
)

// ConstraintValueKind tags the BAML value domain a constraint predicate sees.
//
// It is a 1:1 mirror of BAML v0.223's BamlValue
// (engine/baml-lib/baml-types/src/baml_value.rs:21-32):
//
//	String(String), Int(i64), Float(f64), Bool(bool),
//	Map(BamlMap<String, BamlValue>), List(Vec<BamlValue>),
//	Media(BamlMedia), Enum(String, String),
//	Class(String, BamlMap<String, BamlValue>), Null
//
// The two "named" variants (Enum, Class) carry a type name that the value
// model deliberately DISCARDS on the way into the expression engine — see
// [ConstraintValue.toMinijinja].
type ConstraintValueKind uint8

const (
	ConstraintKindNull ConstraintValueKind = iota
	ConstraintKindBool
	ConstraintKindInt
	ConstraintKindFloat
	ConstraintKindString
	ConstraintKindList
	ConstraintKindMap
	ConstraintKindClass
	ConstraintKindEnum
	ConstraintKindMedia
)

// String names the kind for diagnostics.
func (k ConstraintValueKind) String() string {
	switch k {
	case ConstraintKindNull:
		return "null"
	case ConstraintKindBool:
		return "bool"
	case ConstraintKindInt:
		return "int"
	case ConstraintKindFloat:
		return "float"
	case ConstraintKindString:
		return "string"
	case ConstraintKindList:
		return "list"
	case ConstraintKindMap:
		return "map"
	case ConstraintKindClass:
		return "class"
	case ConstraintKindEnum:
		return "enum"
	case ConstraintKindMedia:
		return "media"
	default:
		return "unknown"
	}
}

// ConstraintEntry is one ordered key/value pair of a map or class value.
//
// BAML's BamlMap is an indexmap::IndexMap, so both map values (input key order)
// and class values (schema field order) are INSERTION-ORDERED, and that order
// is observable inside a predicate (`this|list`, `this|join`, `this.keys()`).
// A Go map cannot carry it, so the entries are a slice.
type ConstraintEntry struct {
	Key   string
	Value ConstraintValue
}

// ConstraintMediaContent is the serde-tagged content arm of a BAML media value
// (engine/baml-lib/baml-types/src/media.rs:41-46 BamlMediaContent). Tag is the
// externally-tagged serde variant name — exactly "File", "Url" or "Base64" —
// and Fields are that variant's struct fields in declaration order.
type ConstraintMediaContent struct {
	Tag    string
	Fields []ConstraintEntry
}

// ConstraintMedia is a BAML media value
// (engine/baml-lib/baml-types/src/media.rs:32-39 BamlMedia).
//
// UNREACHABLE TODAY, and therefore NOT differential-proven: every media output
// shape is rejected before parsing by schema.Bundle.ValidateOutput, so no media
// value can reach a constraint predicate on the native path. It is modelled here
// only so the value domain is complete and a future media slice has a defined
// starting point. See the media note on [ConstraintValue.toMinijinja].
type ConstraintMedia struct {
	// MediaType is the serde variant name of BamlMediaType — "Image", "Audio",
	// "Pdf" or "Video" (media.rs:6-12; the derived Serialize emits the VARIANT
	// name, not the lowercased Display form).
	MediaType string
	// MimeType is the optional explicit mime type; nil serializes as none.
	MimeType *string
	Content  ConstraintMediaContent
}

// ConstraintValue is the native mirror of BAML's BamlValue: the value domain a
// constraint predicate's `this` is drawn from. Exactly one payload field is
// meaningful per kind:
//
//   - ConstraintKindNull:   none
//   - ConstraintKindBool:   b
//   - ConstraintKindInt:    i
//   - ConstraintKindFloat:  f
//   - ConstraintKindString: s
//   - ConstraintKindList:   list
//   - ConstraintKindMap:    entries
//   - ConstraintKindClass:  name + entries
//   - ConstraintKindEnum:   name (enum type) + s (variant name)
//   - ConstraintKindMedia:  media
//
// The zero value is a valid ConstraintKindNull.
type ConstraintValue struct {
	kind    ConstraintValueKind
	b       bool
	i       int64
	f       float64
	s       string
	name    string
	list    []ConstraintValue
	entries []ConstraintEntry
	media   *ConstraintMedia
}

// Kind reports which BamlValue variant this value models.
func (v ConstraintValue) Kind() ConstraintValueKind { return v.kind }

// TypeName is the class or enum name of a ConstraintKindClass /
// ConstraintKindEnum value, and empty for every other kind.
//
// BamlValue carries this name (baml_value.rs:29-30) and the evaluator seam
// DISCARDS it — a predicate sees a mapping or a string, never a named type — so
// nothing in this file reads it. It is retained so the model stays a lossless
// mirror of BamlValue, and because the serving slice needs it: the Checked<T>
// carrier and BAML's assert-failure text are both keyed by the declared type.

func (v ConstraintValue) TypeName() string {
	switch v.kind {
	case ConstraintKindClass, ConstraintKindEnum:
		return v.name
	default:
		return ""
	}
}

// NullValue builds BamlValue::Null.
func NullValue() ConstraintValue { return ConstraintValue{kind: ConstraintKindNull} }

// BoolValue builds BamlValue::Bool.
func BoolValue(b bool) ConstraintValue { return ConstraintValue{kind: ConstraintKindBool, b: b} }

// IntValue builds BamlValue::Int (i64).
func IntValue(i int64) ConstraintValue { return ConstraintValue{kind: ConstraintKindInt, i: i} }

// FloatValue builds BamlValue::Float (f64).
func FloatValue(f float64) ConstraintValue { return ConstraintValue{kind: ConstraintKindFloat, f: f} }

// StringValue builds BamlValue::String.
func StringValue(s string) ConstraintValue { return ConstraintValue{kind: ConstraintKindString, s: s} }

// ListValue builds BamlValue::List. The slice is retained as given (order is
// significant); a nil slice is an empty list, distinct from a null.
func ListValue(items []ConstraintValue) ConstraintValue {
	if items == nil {
		items = []ConstraintValue{}
	}
	return ConstraintValue{kind: ConstraintKindList, list: items}
}

// MapValue builds BamlValue::Map from insertion-ordered entries.
func MapValue(entries []ConstraintEntry) ConstraintValue {
	return ConstraintValue{kind: ConstraintKindMap, entries: entries}
}

// ClassValue builds BamlValue::Class(name, fields) from field-ordered entries.
// The class name is carried for diagnostics only — it is invisible to the
// expression, exactly as in BAML.
func ClassValue(name string, fields []ConstraintEntry) ConstraintValue {
	return ConstraintValue{kind: ConstraintKindClass, name: name, entries: fields}
}

// EnumValue builds BamlValue::Enum(enumName, variant). Only the variant name
// reaches the expression.
func EnumValue(enumName, variant string) ConstraintValue {
	return ConstraintValue{kind: ConstraintKindEnum, name: enumName, s: variant}
}

// MediaValue builds BamlValue::Media. See [ConstraintMedia] — unreachable today.
func MediaValue(m ConstraintMedia) ConstraintValue {
	return ConstraintValue{kind: ConstraintKindMedia, media: &m}
}

// toMinijinja converts a native constraint value into the minijinja value a
// predicate evaluates against.
//
// AUTHORITY. BAML v0.223 reaches the engine through
// `minijinja::Value::from_serialize(this)` (jinja_helpers.rs:88), i.e. through
// BamlValue's serde Serialize impl (baml_value.rs:42-56) — NOT through the
// `From<BamlValue> for minijinja::Value` in baml-types/src/minijinja.rs:17-41,
// which is the PROMPT renderer's conversion. The two agree on every reachable
// arm:
//
//	String   -> string
//	Int      -> i64
//	Float    -> f64
//	Bool     -> bool
//	Map      -> mapping, INSERTION-ORDERED (IndexMap)
//	List     -> sequence, recursing
//	Enum     -> the bare variant NAME string (the enum type is dropped, and the
//	            @alias is NOT used — v0.223 serializes BamlValue::Enum(_, v) as v)
//	Class    -> mapping of its fields in schema order (the class name is dropped)
//	Null     -> serialize_none -> minijinja none
//
// and differ on exactly one: Media. Serialize emits the BamlMedia serde
// document (a mapping); minijinja.rs wraps it in a custom object that renders
// as the magic media marker. Because `evaluate_predicate` goes through
// from_serialize, this reproduces the SERIALIZE arm. Media is unreachable
// today (see [ConstraintMedia]), so neither arm is differential-proven.
//
// MAP REPRESENTATION. minijinja-Go's native mapping (value.FromMap over a Go
// map) iterates its keys SORTED (value/value.go Iter), which would silently
// reorder `this|list`, `this|join`, `this|first` and `this.keys()` relative to
// BAML's insertion order. Maps and classes are therefore projected as an
// ordered mapping OBJECT ([orderedMapping]) whose iteration is the BAML order.
// The one construct that costs is `in` over a mapping — see [orderedMapping].
func (v ConstraintValue) toMinijinjaMode(mode mappingMode) mjvalue.Value {
	switch v.kind {
	case ConstraintKindNull:
		return mjvalue.None()
	case ConstraintKindBool:
		return mjvalue.FromBool(v.b)
	case ConstraintKindInt:
		return mjvalue.FromInt(v.i)
	case ConstraintKindFloat:
		return mjvalue.FromFloat(v.f)
	case ConstraintKindString:
		return mjvalue.FromString(v.s)
	case ConstraintKindEnum:
		// BamlValue::Enum(_, v) => serializer.serialize_str(v): the enum TYPE is
		// dropped and the value becomes a plain string. This is also why the
		// BoundaryML value_cmp fork (which routes `==` on a live enum OBJECT
		// through the variant name) does not bear on constraints: by the time a
		// predicate sees it, an enum IS a string on both sides.
		return mjvalue.FromString(v.s)
	case ConstraintKindList:
		items := make([]mjvalue.Value, len(v.list))
		for i, item := range v.list {
			items[i] = item.toMinijinjaMode(mode)
		}
		return mjvalue.FromSlice(items)
	case ConstraintKindMap, ConstraintKindClass:
		return newMapping(v.entries, mode)
	default:
		// ConstraintKindMedia never reaches here: renderConstraint refuses a
		// media-bearing value before conversion (see hasMedia). Undefined is the
		// fail-safe for any kind added without updating this switch.
		return mjvalue.Undefined()
	}
}

// orderedEntry is one already-converted mapping entry.
type orderedEntry struct {
	key string
	val mjvalue.Value
}

// orderedMapping is the minijinja mapping BAML's Map and Class values become.
//
// WHY AN OBJECT. minijinja-Go's native mapping is a Go map, and every path that
// enumerates one (value.Value.Iter) sorts the keys. BAML's mappings are
// IndexMaps whose insertion order is observable inside a predicate, so a Go map
// would silently reorder `this|list`, `this|join(",")`, `this|first`,
// `this|map(...)` and `this.keys()`. This object reports ObjectReprMap (so
// `is mapping`, Kind and the map filters still see a mapping) and enumerates in
// BAML order.
//
// KNOWN COST — `in` over a mapping. minijinja-Go evaluates the `in` operator as
// value.Value.Contains (value/ops.go:431-455), which type-switches on the
// CONCRETE payload (string / []Value / map[string]Value) and has no object arm.
// `"k" in this` over a mapping therefore evaluates to false here while BAML
// returns true. It is not reconcilable inside the pinned engine (Contains is not
// an extension point and the payload type cannot be forged), and the native
// mapping alternative merely trades it for silently reordered iteration. The
// differential harness pins this divergence explicitly
// (constraintoracle.TestConstraintExpressionDifferential), and a future
// admission slice must DECLINE `in` whose right operand is a mapping rather
// than serve its result. Membership over lists and strings — the common form —
// is unaffected: those stay native minijinja-Go values.
type orderedMapping struct {
	index map[string]mjvalue.Value
	// keys is the BAML order; index is the lookup. Both are written once at
	// construction and never mutated, so the value is safe to share.
	keys []string
}

// newMapping projects a BAML map/class under the requested representation. The
// two differ only in which faithfulness they buy — see the profile notes in
// constraint_profile.go; renderConstraint requires both to agree before it
// trusts an answer.
func newMapping(entries []ConstraintEntry, mode mappingMode) mjvalue.Value {
	if mode == mappingNative {
		// minijinja-Go's own mapping: membership (`in`) and equality are
		// faithful, iteration is SORTED. A duplicate key keeps the last binding,
		// which cannot arise from a BAML IndexMap.
		plain := make(map[string]mjvalue.Value, len(entries))
		for _, e := range entries {
			plain[e.Key] = e.Value.toMinijinjaMode(mode)
		}
		return mjvalue.FromMap(plain)
	}
	converted := make([]orderedEntry, len(entries))
	for i, e := range entries {
		converted[i] = orderedEntry{key: e.Key, val: e.Value.toMinijinjaMode(mode)}
	}
	return newOrderedMappingValues(converted)
}

func newOrderedMappingValues(entries []orderedEntry) mjvalue.Value {
	m := &orderedMapping{
		index: make(map[string]mjvalue.Value, len(entries)),
		keys:  make([]string, 0, len(entries)),
	}
	for _, e := range entries {
		// IndexMap::insert on an existing key REPLACES the value and keeps the
		// original position. BAML's own maps cannot carry a duplicate key (they
		// arrive as an IndexMap already), so this only guards a malformed caller
		// — but it guards it the way BAML's container would.
		if _, seen := m.index[e.key]; !seen {
			m.keys = append(m.keys, e.key)
		}
		m.index[e.key] = e.val
	}
	return mjvalue.FromObject(m)
}

// GetAttr resolves `this.key`. An absent key is undefined, matching a minijinja
// mapping (and BAML, whose undefined attribute is likewise not an error).
func (m *orderedMapping) GetAttr(name string) mjvalue.Value {
	if v, ok := m.index[name]; ok {
		return v
	}
	return mjvalue.Undefined()
}

// GetItem resolves `this["key"]`, and is also what the pycompat `get` method
// projects onto.
func (m *orderedMapping) GetItem(key mjvalue.Value) mjvalue.Value {
	s, ok := key.AsString()
	if !ok {
		return mjvalue.Undefined()
	}
	return m.GetAttr(s)
}

// ObjectRepr makes this a mapping for Kind(), `is mapping` and the map filters.
func (m *orderedMapping) ObjectRepr() mjvalue.ObjectRepr { return mjvalue.ObjectReprMap }

// Keys enumerates in BAML insertion order — the whole point of this type.
func (m *orderedMapping) Keys() []string { return m.keys }

// Iterate yields the KEYS in BAML order, which is how minijinja iterates a
// mapping.
func (m *orderedMapping) Iterate() iter.Seq[mjvalue.Value] {
	return func(yield func(mjvalue.Value) bool) {
		for _, k := range m.keys {
			if !yield(mjvalue.FromString(k)) {
				return
			}
		}
	}
}

// ObjectLen backs `this|length`.
func (m *orderedMapping) ObjectLen() int { return len(m.keys) }

// Map exposes the mapping to value.Value.AsMap, which backs equality
// (value/ops.go Equal), `|items` and `|dictsort`. Order is lost through this
// seam by construction — a Go map has none — which is exactly why iteration
// goes through Keys/Iterate instead.
func (m *orderedMapping) Map() map[string]mjvalue.Value { return m.index }

// String makes the mapping a fmt.Stringer.
//
// This is load-bearing, not cosmetic: minijinja-Go's value.Value.String() has
// no object arm and falls through to fmt.Sprintf("%v", data)
// (value/value.go:947-948), which for a plain Go struct pointer would render
// `&{...}`. Implementing fmt.Stringer makes that fallback resolve to the
// minijinja mapping rendering, so `this|string`, `this|join(...)`, `~`
// concatenation and the formatter all agree with BAML instead of leaking Go's
// struct syntax.
func (m *orderedMapping) String() string { return m.ObjectString() }

// ObjectString renders the mapping the way minijinja-Go renders its native one
// (value/value.go String), but in BAML order rather than sorted.
func (m *orderedMapping) ObjectString() string {
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range m.keys {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(strconv.Quote(k))
		b.WriteString(": ")
		b.WriteString(m.index[k].Repr())
	}
	b.WriteByte('}')
	return b.String()
}

// CallMethod implements the minijinja-contrib pycompat map methods BAML
// installs via set_unknown_method_callback (jinja_helpers.rs:34), ported from
// minijinja-contrib 2.16.0 pycompat.rs:276-318 — `keys`, `values`, `items` and
// `get`. minijinja-Go v2.16.0 has no environment-level unknown-method hook, so
// the only place these can live is on the objects the value model owns; the
// string and sequence halves of pycompat are therefore NOT available and error
// out (fail-closed) rather than answer differently. See the doc comment on
// [EvaluateConstraint].
func (m *orderedMapping) CallMethod(_ mjvalue.State, name string, args []mjvalue.Value, kwargs map[string]mjvalue.Value) (mjvalue.Value, error) {
	if len(kwargs) > 0 {
		return mjvalue.Undefined(), errUnknownKwargs(name)
	}
	switch name {
	case "keys":
		if len(args) != 0 {
			return mjvalue.Undefined(), errArgCount(name, 0, len(args))
		}
		out := make([]mjvalue.Value, 0, len(m.keys))
		for _, k := range m.keys {
			out = append(out, mjvalue.FromString(k))
		}
		return mjvalue.FromSlice(out), nil
	case "values":
		if len(args) != 0 {
			return mjvalue.Undefined(), errArgCount(name, 0, len(args))
		}
		out := make([]mjvalue.Value, 0, len(m.keys))
		for _, k := range m.keys {
			out = append(out, m.index[k])
		}
		return mjvalue.FromSlice(out), nil
	case "items":
		if len(args) != 0 {
			return mjvalue.Undefined(), errArgCount(name, 0, len(args))
		}
		out := make([]mjvalue.Value, 0, len(m.keys))
		for _, k := range m.keys {
			out = append(out, mjvalue.FromSlice([]mjvalue.Value{mjvalue.FromString(k), m.index[k]}))
		}
		return mjvalue.FromSlice(out), nil
	case "get":
		// pycompat.rs:309-315 — `get(key)` / `get(key, default)`; a missing key
		// with no default yields none (Value::from(())), not undefined.
		if len(args) < 1 || len(args) > 2 {
			return mjvalue.Undefined(), errArgCount(name, 1, len(args))
		}
		if s, ok := args[0].AsString(); ok {
			if v, found := m.index[s]; found {
				return v, nil
			}
		}
		if len(args) == 2 {
			return args[1], nil
		}
		return mjvalue.None(), nil
	default:
		return mjvalue.Undefined(), mjvalue.ErrUnknownMethod
	}
}

// MarshalJSON serializes a constraint value exactly as BAML's
// `impl serde::Serialize for BamlValue` does (baml_value.rs:42-56):
//
//	String   -> serialize_str
//	Int      -> serialize_i64
//	Float    -> serialize_f64
//	Bool     -> serialize_bool
//	Map      -> the IndexMap, in INSERTION order
//	List     -> the Vec
//	Media    -> the BamlMedia document
//	Enum     -> serialize_str of the VARIANT name (the enum type is dropped)
//	Class    -> the field IndexMap, in SCHEMA order (the class name is dropped)
//	Null     -> serialize_none
//
// This is the same serialization `Value::from_serialize` walks to build the
// minijinja value, so it is the direct statement of the value model, and the
// differential harness uses it to prove native modelled the same document stock
// BAML decoded (constraintoracle: the `Checked.Value` comparison). Key ORDER is
// preserved here even though encoding/json would sort a Go map, because order is
// part of the model — see [orderedMapping].
func (v ConstraintValue) MarshalJSON() ([]byte, error) {
	switch v.kind {
	case ConstraintKindNull:
		return []byte("null"), nil
	case ConstraintKindBool:
		return json.Marshal(v.b)
	case ConstraintKindInt:
		return json.Marshal(v.i)
	case ConstraintKindFloat:
		return json.Marshal(v.f)
	case ConstraintKindString, ConstraintKindEnum:
		return json.Marshal(v.s)
	case ConstraintKindList:
		return json.Marshal(v.list)
	case ConstraintKindMap, ConstraintKindClass:
		return marshalOrderedEntries(v.entries)
	case ConstraintKindMedia:
		return v.media.MarshalJSON()
	default:
		return nil, fmt.Errorf("debaml: cannot marshal constraint value of kind %s", v.kind)
	}
}

// MarshalJSON serializes a media value as its BamlMedia serde document; see
// [ConstraintMedia] (unreachable today).
func (m *ConstraintMedia) MarshalJSON() ([]byte, error) {
	if m == nil {
		return []byte("null"), nil
	}
	var mime ConstraintValue
	if m.MimeType != nil {
		mime = StringValue(*m.MimeType)
	}
	return marshalOrderedEntries([]ConstraintEntry{
		{Key: "media_type", Value: StringValue(m.MediaType)},
		{Key: "mime_type", Value: mime},
		{Key: "content", Value: MapValue([]ConstraintEntry{
			{Key: m.Content.Tag, Value: MapValue(m.Content.Fields)},
		})},
	})
}

// marshalOrderedEntries writes an object literal in entry order. encoding/json
// sorts Go map keys, so the entries cannot be handed to it as a map.
func marshalOrderedEntries(entries []ConstraintEntry) ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, e := range entries {
		if i > 0 {
			b.WriteByte(',')
		}
		key, err := json.Marshal(e.Key)
		if err != nil {
			return nil, err
		}
		b.Write(key)
		b.WriteByte(':')
		val, err := json.Marshal(e.Value)
		if err != nil {
			return nil, err
		}
		b.Write(val)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}
