package bamlfuzz

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/invakid404/baml-rest/bamlutils"
)

// WalkResult captures the two JSON renderings the oracle paths need
// plus the per-case provenance metadata.
type WalkResult struct {
	// MockLLMContent is what mockllm replays as the LLM's textual
	// response. Optional fields with shape "absent" are omitted
	// from the parent object's key set; shape "null" emits an
	// explicit JSON null.
	MockLLMContent json.RawMessage
	// Expected is the schema-normalized parsed value: every
	// optional field — present, absent, or explicit-null — appears
	// at its key, with absent and null both rendered as JSON null.
	// Equivalently: this is what the BAML parser is contracted to
	// produce when it consumes MockLLMContent.
	Expected json.RawMessage
	// Metadata is per-case provenance the failure envelope embeds.
	Metadata CaseMetadata
}

// Walk renders a (schema, root-value) pair through the LLM-output
// and schema-normalized-output paths. The root type is the schema's
// effective root (FuzzSchema.EffectiveRoot): when the schema sets
// RootType the value walks that type directly; otherwise the value
// is treated as the named RootClass instance, preserving the v1
// behaviour for existing replay artifacts.
//
// Field iteration follows the class's declaration order so the
// emitted JSON is byte-stable across runs at the same seed.
// Per-instance map values inside the value tree iterate in the
// FuzzValue.MapEntries slice order verbatim — the walker no longer
// sorts map keys, which lets the strict map key-order assertion in
// order.go's walkType(KindMap) actually exercise non-lexicographic
// orders end-to-end. Union values are rendered via the recorded
// VariantIndex / Variant fields; the walker fails closed when a
// union value lacks a recorded choice.
func Walk(schema FuzzSchema, value FuzzValue) (WalkResult, error) {
	meta := CaseMetadata{
		OptionalShapes:  make(map[string]string),
		RecursionDepths: make(map[string]int),
		UnionChoices:    make(map[string]UnionChoice),
	}
	state := &walkState{schema: schema, meta: &meta, depths: make(map[string]int)}
	root := schema.EffectiveRoot()
	var mockBuf, expectBuf bytes.Buffer
	if err := state.renderValueMock(&mockBuf, root, value, ""); err != nil {
		return WalkResult{}, err
	}
	if err := state.renderValueExpected(&expectBuf, root, value); err != nil {
		return WalkResult{}, err
	}
	return WalkResult{
		MockLLMContent: append(json.RawMessage(nil), mockBuf.Bytes()...),
		Expected:       append(json.RawMessage(nil), expectBuf.Bytes()...),
		Metadata:       meta,
	}, nil
}

// walkState is the per-Walk mutable bag — schema lookups, metadata
// accumulators, and the per-class recursion-depth counter. Counters
// are incremented when entering a class instance and decremented on
// exit so the meta.RecursionDepths reports the peak depth observed.
type walkState struct {
	schema FuzzSchema
	meta   *CaseMetadata
	depths map[string]int
}

func (s *walkState) renderClassMock(buf *bytes.Buffer, val FuzzValue, path string) error {
	cls, ok := s.schema.FindClass(val.ClassName)
	if !ok {
		return fmt.Errorf("walk: unknown class %q in value at %q", val.ClassName, path)
	}
	s.depths[cls.Name]++
	if s.meta.RecursionDepths[cls.Name] < s.depths[cls.Name] {
		s.meta.RecursionDepths[cls.Name] = s.depths[cls.Name]
	}
	defer func() { s.depths[cls.Name]-- }()

	buf.WriteByte('{')
	first := true
	for _, prop := range cls.Properties {
		fv, ok := val.LookupField(prop.Name)
		if !ok {
			return fmt.Errorf("walk: missing field %q on class %q value at %q", prop.Name, cls.Name, path)
		}
		fpath := path + "." + prop.Name
		if prop.Type.Kind == KindOptional {
			s.meta.OptionalShapes[fpath] = string(fv.OptionalShape)
			if fv.OptionalShape == OptionalAbsent {
				continue
			}
		}
		if !first {
			buf.WriteByte(',')
		}
		first = false
		keyBytes, err := json.Marshal(prop.Name)
		if err != nil {
			return err
		}
		buf.Write(keyBytes)
		buf.WriteByte(':')
		if err := s.renderValueMock(buf, prop.Type, fv, fpath); err != nil {
			return err
		}
	}
	buf.WriteByte('}')
	return nil
}

func (s *walkState) renderClassExpected(buf *bytes.Buffer, val FuzzValue) error {
	cls, ok := s.schema.FindClass(val.ClassName)
	if !ok {
		return fmt.Errorf("walk: unknown class %q in expected", val.ClassName)
	}
	s.depths[cls.Name]++
	if s.meta.RecursionDepths[cls.Name] < s.depths[cls.Name] {
		s.meta.RecursionDepths[cls.Name] = s.depths[cls.Name]
	}
	defer func() { s.depths[cls.Name]-- }()
	buf.WriteByte('{')
	for i, prop := range cls.Properties {
		fv, ok := val.LookupField(prop.Name)
		if !ok {
			return fmt.Errorf("walk: missing field %q on class %q in expected", prop.Name, cls.Name)
		}
		if i > 0 {
			buf.WriteByte(',')
		}
		keyBytes, err := json.Marshal(prop.Name)
		if err != nil {
			return err
		}
		buf.Write(keyBytes)
		buf.WriteByte(':')
		if err := s.renderValueExpected(buf, prop.Type, fv); err != nil {
			return err
		}
	}
	buf.WriteByte('}')
	return nil
}

func (s *walkState) renderValueMock(buf *bytes.Buffer, t FuzzType, val FuzzValue, path string) error {
	switch t.Kind {
	case KindString:
		return writeJSON(buf, val.String)
	case KindInt:
		buf.WriteString(strconv.FormatInt(val.Int, 10))
		return nil
	case KindFloat:
		return writeJSON(buf, val.Float)
	case KindBool:
		if val.Bool {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
		return nil
	case KindNull:
		buf.WriteString("null")
		return nil
	case KindLiteral:
		return writeLiteral(buf, t.Literal)
	case KindEnumRef:
		return writeJSON(buf, val.Enum)
	case KindOptional:
		switch val.OptionalShape {
		case OptionalNull:
			buf.WriteString("null")
			return nil
		case OptionalPresent:
			if val.Inner == nil || t.Inner == nil {
				return fmt.Errorf("walk: present optional missing inner at %q", path)
			}
			return s.renderValueMock(buf, *t.Inner, *val.Inner, path)
		case OptionalAbsent:
			// Outside a class-field context the parent
			// container (list/map) cannot omit the slot; absent
			// degrades to null so list/map indices stay aligned
			// with the schema. The walker keeps the slot in
			// FuzzValue tree so callers can still tell what
			// happened by reading OptionalShape.
			buf.WriteString("null")
			return nil
		default:
			return fmt.Errorf("walk: unknown optional shape %q at %q", val.OptionalShape, path)
		}
	case KindList:
		if t.Inner == nil {
			return fmt.Errorf("walk: list type missing inner at %q", path)
		}
		buf.WriteByte('[')
		for i, item := range val.Items {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := s.renderValueMock(buf, *t.Inner, item, fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
		return nil
	case KindMap:
		if t.Inner == nil {
			return fmt.Errorf("walk: map type missing inner at %q", path)
		}
		buf.WriteByte('{')
		for i, entry := range val.MapEntries {
			if i > 0 {
				buf.WriteByte(',')
			}
			keyBytes, err := json.Marshal(entry.Key)
			if err != nil {
				return err
			}
			buf.Write(keyBytes)
			buf.WriteByte(':')
			if err := s.renderValueMock(buf, *t.Inner, entry.Value, fmt.Sprintf("%s[%q]", path, entry.Key)); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
		return nil
	case KindClassRef:
		return s.renderClassMock(buf, val, path)
	case KindUnion:
		if val.Kind != KindUnion || val.Variant == nil {
			return fmt.Errorf("walk: union value missing selected variant at %q", path)
		}
		if val.VariantIndex < 0 || val.VariantIndex >= len(t.Variants) {
			return fmt.Errorf("walk: union variant index %d out of range [0,%d) at %q", val.VariantIndex, len(t.Variants), path)
		}
		s.recordUnionChoice(path, t, val)
		// Step the path with ":v" before recursing so nested unions
		// (the picked arm contains another KindUnion) get distinct
		// UnionChoices entries; without the step the inner choice
		// would overwrite the outer one. The normalizer and order
		// checker use the same suffix.
		return s.renderValueMock(buf, t.Variants[val.VariantIndex], *val.Variant, path+":v")
	default:
		return fmt.Errorf("walk: unknown kind %q at %q", t.Kind, path)
	}
}

func (s *walkState) renderValueExpected(buf *bytes.Buffer, t FuzzType, val FuzzValue) error {
	switch t.Kind {
	case KindString:
		return writeJSON(buf, val.String)
	case KindInt:
		buf.WriteString(strconv.FormatInt(val.Int, 10))
		return nil
	case KindFloat:
		return writeJSON(buf, val.Float)
	case KindBool:
		if val.Bool {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
		return nil
	case KindNull:
		buf.WriteString("null")
		return nil
	case KindLiteral:
		return writeLiteral(buf, t.Literal)
	case KindEnumRef:
		return writeJSON(buf, val.Enum)
	case KindOptional:
		if val.OptionalShape == OptionalPresent {
			if val.Inner == nil || t.Inner == nil {
				return fmt.Errorf("expected: present optional missing inner")
			}
			return s.renderValueExpected(buf, *t.Inner, *val.Inner)
		}
		// Absent and null collapse to JSON null.
		buf.WriteString("null")
		return nil
	case KindList:
		if t.Inner == nil {
			return fmt.Errorf("expected: list type missing inner")
		}
		buf.WriteByte('[')
		for i, item := range val.Items {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := s.renderValueExpected(buf, *t.Inner, item); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
		return nil
	case KindMap:
		if t.Inner == nil {
			return fmt.Errorf("expected: map type missing inner")
		}
		buf.WriteByte('{')
		for i, entry := range val.MapEntries {
			if i > 0 {
				buf.WriteByte(',')
			}
			keyBytes, err := json.Marshal(entry.Key)
			if err != nil {
				return err
			}
			buf.Write(keyBytes)
			buf.WriteByte(':')
			if err := s.renderValueExpected(buf, *t.Inner, entry.Value); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
		return nil
	case KindClassRef:
		return s.renderClassExpected(buf, val)
	case KindUnion:
		if val.Kind != KindUnion || val.Variant == nil {
			return fmt.Errorf("expected: union value missing selected variant")
		}
		if val.VariantIndex < 0 || val.VariantIndex >= len(t.Variants) {
			return fmt.Errorf("expected: union variant index %d out of range [0,%d)", val.VariantIndex, len(t.Variants))
		}
		return s.renderValueExpected(buf, t.Variants[val.VariantIndex], *val.Variant)
	default:
		return fmt.Errorf("expected: unknown kind %q", t.Kind)
	}
}

func (s *walkState) recordUnionChoice(path string, t FuzzType, val FuzzValue) {
	choice := UnionChoice{
		Index:        val.VariantIndex,
		Kind:         t.Variants[val.VariantIndex].Kind,
		Ref:          t.Variants[val.VariantIndex].Ref,
		VariantCount: len(t.Variants),
	}
	s.meta.UnionChoices[path] = choice
}

// writeJSON marshals `v` and writes the result into buf. For floats
// and strings it relies on encoding/json's escaping + canonical
// number rendering so output stays parser-clean.
func writeJSON(buf *bytes.Buffer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	buf.Write(b)
	return nil
}

func writeLiteral(buf *bytes.Buffer, lit *FuzzLiteral) error {
	if lit == nil {
		return fmt.Errorf("literal type missing payload")
	}
	switch lit.Kind {
	case LiteralString:
		return writeJSON(buf, lit.String)
	case LiteralInt:
		buf.WriteString(strconv.FormatInt(lit.Int, 10))
		return nil
	case LiteralBool:
		if lit.Bool {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
		return nil
	default:
		return fmt.Errorf("unknown literal kind %q", lit.Kind)
	}
}

// NormalizeMockToExpected re-parses `mock` and walks the schema's
// class graph to insert JSON null for every optional field that the
// LLM omitted. The returned bytes are the canonical-equivalent of
// the walker's Expected output: class instance keys follow schema
// declaration order, and map keys retain the mock's wire order —
// which, given a well-formed mock, mirrors FuzzValue.MapEntries
// insertion order. This is the invariant the tests assert: walking
// (mock) -> normalize -> bytes-equal walker's expected.
//
// Union schemas require union-choice metadata to disambiguate which
// arm produced the mock; call NormalizeMockToExpectedWithChoices
// directly when the schema contains KindUnion. This entry point
// fails closed on encountering a union without recorded choices.
func NormalizeMockToExpected(schema FuzzSchema, mock json.RawMessage, rootClass string) (json.RawMessage, error) {
	return NormalizeMockToExpectedWithChoices(schema, mock, rootClass, nil)
}

// NormalizeMockToExpectedWithChoices is the union-aware variant. The
// `choices` map mirrors CaseMetadata.UnionChoices: each entry tells
// the normalizer which arm produced the JSON at that path.
//
// Normalization is driven by the schema's effective root type
// (FuzzSchema.EffectiveRoot): when the schema sets RootType the
// payload is normalized against that type directly, so raw-root
// union/list/map/scalar schemas round-trip correctly. When RootType
// is nil the effective root is a class ref pointing at RootClass,
// keeping the v1 behaviour. The `rootClass` parameter is honoured
// only as a fallback for callers that supply it before RootType was
// added to the IR.
func NormalizeMockToExpectedWithChoices(schema FuzzSchema, mock json.RawMessage, rootClass string, choices map[string]UnionChoice) (json.RawMessage, error) {
	raw, err := decodePreserveOrder(mock)
	if err != nil {
		return nil, fmt.Errorf("normalize: parse mock: %w", err)
	}
	root := effectiveNormalizeRoot(schema, rootClass)
	out, err := normalizeValue(schema, root, raw, "", choices)
	if err != nil {
		return nil, err
	}
	return marshalSchemaOrdered(schema, root, out, choices)
}

// decodePreserveOrder decodes JSON into an `any`-shaped tree where
// every object node is a *bamlutils.OrderedMap[any] keyed in wire
// order. Arrays become []any whose elements are recursively decoded
// the same way; scalars and nulls decode through encoding/json's
// default rules. The order-preserving carrier is what lets the
// normalizer emit map values in the mock's wire order, so the
// round-trip against the walker's MapEntries insertion order is
// byte-stable.
func decodePreserveOrder(data json.RawMessage) (any, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("empty JSON payload")
	}
	switch trimmed[0] {
	case '{':
		var raw bamlutils.OrderedMap[json.RawMessage]
		if err := raw.UnmarshalJSON(data); err != nil {
			return nil, err
		}
		out := &bamlutils.OrderedMap[any]{}
		var firstErr error
		raw.Range(func(k string, v json.RawMessage) bool {
			child, err := decodePreserveOrder(v)
			if err != nil {
				firstErr = fmt.Errorf("%s: %w", k, err)
				return false
			}
			if err := out.Set(k, child); err != nil {
				firstErr = err
				return false
			}
			return true
		})
		if firstErr != nil {
			return nil, firstErr
		}
		return out, nil
	case '[':
		var rawArr []json.RawMessage
		if err := json.Unmarshal(data, &rawArr); err != nil {
			return nil, err
		}
		arr := make([]any, len(rawArr))
		for i, r := range rawArr {
			v, err := decodePreserveOrder(r)
			if err != nil {
				return nil, fmt.Errorf("[%d]: %w", i, err)
			}
			arr[i] = v
		}
		return arr, nil
	default:
		var v any
		if err := json.Unmarshal(data, &v); err != nil {
			return nil, err
		}
		return v, nil
	}
}

// effectiveNormalizeRoot picks the FuzzType to normalize and re-emit
// against. RootType wins when set (so raw roots work); otherwise the
// `rootClass` argument is used to stay compatible with callers that
// still hand a class name through. Falling further back to
// schema.EffectiveRoot covers schemas where both RootType is nil and
// the caller passed an empty rootClass.
func effectiveNormalizeRoot(schema FuzzSchema, rootClass string) FuzzType {
	if schema.RootType != nil {
		return *schema.RootType
	}
	if rootClass != "" {
		return FuzzType{Kind: KindClassRef, Ref: rootClass}
	}
	return schema.EffectiveRoot()
}

func normalizeClass(schema FuzzSchema, className string, raw any, basePath string, choices map[string]UnionChoice) (*bamlutils.OrderedMap[any], error) {
	cls, ok := schema.FindClass(className)
	if !ok {
		return nil, fmt.Errorf("normalize: unknown class %q", className)
	}
	obj, ok := raw.(*bamlutils.OrderedMap[any])
	if !ok {
		return nil, fmt.Errorf("normalize: class %q expected JSON object, got %T", className, raw)
	}
	out := &bamlutils.OrderedMap[any]{}
	for _, prop := range cls.Properties {
		fv, present := obj.Get(prop.Name)
		if !present {
			// Absent optionals normalize to null; absent non-
			// optionals are a contract violation upstream of the
			// walker.
			if prop.Type.Kind != KindOptional {
				return nil, fmt.Errorf("normalize: required field %q on %q missing from mock", prop.Name, className)
			}
			if err := out.Set(prop.Name, nil); err != nil {
				return nil, err
			}
			continue
		}
		nv, err := normalizeValue(schema, prop.Type, fv, basePath+"."+prop.Name, choices)
		if err != nil {
			return nil, fmt.Errorf("normalize: field %q on %q: %w", prop.Name, className, err)
		}
		if err := out.Set(prop.Name, nv); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func normalizeValue(schema FuzzSchema, t FuzzType, raw any, path string, choices map[string]UnionChoice) (any, error) {
	switch t.Kind {
	case KindString, KindInt, KindFloat, KindBool, KindLiteral, KindEnumRef:
		return raw, nil
	case KindNull:
		return nil, nil
	case KindOptional:
		if raw == nil {
			return nil, nil
		}
		if t.Inner == nil {
			return nil, fmt.Errorf("optional missing inner")
		}
		return normalizeValue(schema, *t.Inner, raw, path, choices)
	case KindList:
		if raw == nil {
			return nil, nil
		}
		arr, ok := raw.([]any)
		if !ok {
			return nil, fmt.Errorf("list expected array, got %T", raw)
		}
		out := make([]any, len(arr))
		for i, item := range arr {
			nv, err := normalizeValue(schema, *t.Inner, item, fmt.Sprintf("%s[%d]", path, i), choices)
			if err != nil {
				return nil, fmt.Errorf("list[%d]: %w", i, err)
			}
			out[i] = nv
		}
		return out, nil
	case KindMap:
		if raw == nil {
			return nil, nil
		}
		obj, ok := raw.(*bamlutils.OrderedMap[any])
		if !ok {
			return nil, fmt.Errorf("map expected object, got %T", raw)
		}
		out := &bamlutils.OrderedMap[any]{}
		var firstErr error
		obj.Range(func(k string, v any) bool {
			nv, err := normalizeValue(schema, *t.Inner, v, fmt.Sprintf("%s[%q]", path, k), choices)
			if err != nil {
				firstErr = fmt.Errorf("map[%q]: %w", k, err)
				return false
			}
			if err := out.Set(k, nv); err != nil {
				firstErr = err
				return false
			}
			return true
		})
		if firstErr != nil {
			return nil, firstErr
		}
		return out, nil
	case KindClassRef:
		return normalizeClass(schema, t.Ref, raw, path, choices)
	case KindUnion:
		choice, ok := choices[path]
		if !ok {
			return nil, fmt.Errorf("normalize: union at %q has no recorded choice", path)
		}
		if choice.Index < 0 || choice.Index >= len(t.Variants) {
			return nil, fmt.Errorf("normalize: union choice index %d out of range at %q", choice.Index, path)
		}
		return normalizeValue(schema, t.Variants[choice.Index], raw, path+":v", choices)
	default:
		return nil, fmt.Errorf("normalize: unknown kind %q", t.Kind)
	}
}

// marshalSchemaOrdered re-emits a normalized value as JSON with
// class-instance keys in schema declaration order and inner map
// keys in the carrier's recorded insertion order. The carrier is
// the *bamlutils.OrderedMap[any] tree produced by normalizeClass /
// normalizeValue (which themselves consume the decodePreserveOrder
// output, so wire order from the mock flows through unchanged).
// Union arms inside the type tree resolve through `choices`. The
// `root` FuzzType drives dispatch directly, so raw-root
// union/list/map/scalar schemas are walked as-is without going
// through the class helper.
func marshalSchemaOrdered(schema FuzzSchema, root FuzzType, normalized any, choices map[string]UnionChoice) (json.RawMessage, error) {
	var buf bytes.Buffer
	if err := writeOrderedValue(&buf, schema, root, normalized, "", choices); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeOrderedClass(buf *bytes.Buffer, schema FuzzSchema, className string, v any, path string, choices map[string]UnionChoice) error {
	cls, ok := schema.FindClass(className)
	if !ok {
		return fmt.Errorf("ordered: unknown class %q", className)
	}
	obj, ok := v.(*bamlutils.OrderedMap[any])
	if !ok {
		return fmt.Errorf("ordered: class %q expected object, got %T", className, v)
	}
	buf.WriteByte('{')
	for i, prop := range cls.Properties {
		if i > 0 {
			buf.WriteByte(',')
		}
		keyBytes, err := json.Marshal(prop.Name)
		if err != nil {
			return err
		}
		buf.Write(keyBytes)
		buf.WriteByte(':')
		fv, _ := obj.Get(prop.Name)
		if err := writeOrderedValue(buf, schema, prop.Type, fv, path+"."+prop.Name, choices); err != nil {
			return err
		}
	}
	buf.WriteByte('}')
	return nil
}

func writeOrderedValue(buf *bytes.Buffer, schema FuzzSchema, t FuzzType, v any, path string, choices map[string]UnionChoice) error {
	switch t.Kind {
	case KindOptional:
		if v == nil {
			buf.WriteString("null")
			return nil
		}
		return writeOrderedValue(buf, schema, *t.Inner, v, path, choices)
	case KindList:
		if v == nil {
			buf.WriteString("null")
			return nil
		}
		arr, ok := v.([]any)
		if !ok {
			return fmt.Errorf("list expected array, got %T", v)
		}
		buf.WriteByte('[')
		for i, item := range arr {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeOrderedValue(buf, schema, *t.Inner, item, fmt.Sprintf("%s[%d]", path, i), choices); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
		return nil
	case KindMap:
		if v == nil {
			buf.WriteString("null")
			return nil
		}
		// Emit map values in the carrier's recorded insertion order
		// (the OrderedMap[any] tree threaded through from the mock
		// via decodePreserveOrder). This matches the walker's
		// FuzzValue.MapEntries slice-order emission, so the
		// (walk → normalize) round-trip is byte-stable.
		obj, ok := v.(*bamlutils.OrderedMap[any])
		if !ok {
			return fmt.Errorf("map expected object, got %T", v)
		}
		buf.WriteByte('{')
		i := 0
		var firstErr error
		obj.Range(func(k string, mv any) bool {
			if i > 0 {
				buf.WriteByte(',')
			}
			i++
			kb, err := json.Marshal(k)
			if err != nil {
				firstErr = err
				return false
			}
			buf.Write(kb)
			buf.WriteByte(':')
			if err := writeOrderedValue(buf, schema, *t.Inner, mv, fmt.Sprintf("%s[%q]", path, k), choices); err != nil {
				firstErr = err
				return false
			}
			return true
		})
		if firstErr != nil {
			return firstErr
		}
		buf.WriteByte('}')
		return nil
	case KindClassRef:
		return writeOrderedClass(buf, schema, t.Ref, v, path, choices)
	case KindUnion:
		choice, ok := choices[path]
		if !ok {
			return fmt.Errorf("ordered: union at %q has no recorded choice", path)
		}
		if choice.Index < 0 || choice.Index >= len(t.Variants) {
			return fmt.Errorf("ordered: union choice index %d out of range at %q", choice.Index, path)
		}
		return writeOrderedValue(buf, schema, t.Variants[choice.Index], v, path+":v", choices)
	default:
		// Primitives, literals, enums: emit through encoding/json.
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		buf.Write(b)
		return nil
	}
}

