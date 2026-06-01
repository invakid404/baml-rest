package bamlfuzz

// FuzzOptionalShape encodes which of the three legitimate LLM
// shapes an optional field takes. Absent optionals are omitted
// from expected output when preserve_schema_order is false; when
// true they appear as JSON null (matching BAML's behaviour of
// emitting all schema keys). Null optionals always appear as
// explicit JSON null. The distinction matters for both the mock
// (what the LLM emits) and the expected output (what the parser
// produces).
type FuzzOptionalShape string

const (
	// OptionalPresent: the optional carries a non-null inner value.
	OptionalPresent FuzzOptionalShape = "present"
	// OptionalAbsent: the optional's key is omitted entirely from
	// the parent object. The parser sees no key.
	OptionalAbsent FuzzOptionalShape = "absent"
	// OptionalNull: the optional's key is present with an explicit
	// JSON null payload. Parser sees the key, parses to null.
	OptionalNull FuzzOptionalShape = "null"
)

// FuzzMapEntry is one key/value pair inside a KindMap value.
// Stored as a slice (not a map) so generation order is preserved
// and emitted JSON is stable across runs.
type FuzzMapEntry struct {
	Key   string    `json:"key"`
	Value FuzzValue `json:"value"`
}

// FuzzFieldValue binds a property name to its concrete value for a
// class instance. Order mirrors the class's property declaration
// order so the value walker can stream-emit JSON in schema order.
type FuzzFieldValue struct {
	Name  string    `json:"name"`
	Value FuzzValue `json:"value"`
}

// FuzzValue is a concrete value conforming to some FuzzType. Kind
// dispatches which payload fields are meaningful:
//   - KindString / KindInt / KindFloat / KindBool: matching scalar
//     field.
//   - KindLiteral: the scalar field matching the literal's kind.
//   - KindEnumRef: Enum.
//   - KindOptional: OptionalShape, plus Inner when present.
//   - KindList: Items (may be empty).
//   - KindMap: MapEntries (may be empty).
//   - KindClassRef: ClassName + Fields (one per class property, in
//     declaration order).
//   - KindUnion: VariantIndex names the FuzzType.Variants slot the
//     value chose; Variant carries the generated value for that
//     arm. A KindUnion value MUST always wrap a Variant — the walker,
//     emitters, and order checker fail closed when it does not.
type FuzzValue struct {
	Kind FuzzTypeKind `json:"kind"`

	String string  `json:"string,omitempty"`
	Int    int64   `json:"int,omitempty"`
	Float  float64 `json:"float,omitempty"`
	Bool   bool    `json:"bool,omitempty"`
	Enum   string  `json:"enum,omitempty"`

	OptionalShape FuzzOptionalShape `json:"optional_shape,omitempty"`
	Inner         *FuzzValue        `json:"inner,omitempty"`

	Items []FuzzValue `json:"items,omitempty"`

	MapEntries []FuzzMapEntry `json:"map_entries,omitempty"`

	ClassName string           `json:"class_name,omitempty"`
	Fields    []FuzzFieldValue `json:"fields,omitempty"`

	VariantIndex int        `json:"variant_index,omitempty"`
	Variant      *FuzzValue `json:"variant,omitempty"`
}

// LookupField returns the FuzzValue for the named property on a
// class-instance FuzzValue. Linear scan — field counts bounded at
// 5 in v1.
func (v FuzzValue) LookupField(name string) (FuzzValue, bool) {
	for _, fv := range v.Fields {
		if fv.Name == name {
			return fv.Value, true
		}
	}
	return FuzzValue{}, false
}
