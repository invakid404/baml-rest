package bamlfuzz

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
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
// FuzzValue.MapEntries order (also generator-determined), so those
// too are stable. Union values are rendered via the recorded
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
		entries := sortedMapEntries(val.MapEntries)
		for i, entry := range entries {
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
		entries := sortedMapEntries(val.MapEntries)
		for i, entry := range entries {
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
// the walker's Expected output, modulo encoding/json's map-key
// sorting (top-level class field order still follows the schema).
//
// This is the invariant the tests assert: walking (mock) ->
// normalize -> bytes-equal walker's expected.
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
func NormalizeMockToExpectedWithChoices(schema FuzzSchema, mock json.RawMessage, rootClass string, choices map[string]UnionChoice) (json.RawMessage, error) {
	var raw any
	if err := json.Unmarshal(mock, &raw); err != nil {
		return nil, fmt.Errorf("normalize: parse mock: %w", err)
	}
	out, err := normalizeClass(schema, rootClass, raw, "", choices)
	if err != nil {
		return nil, err
	}
	return marshalSchemaOrdered(schema, rootClass, out, choices)
}

func normalizeClass(schema FuzzSchema, className string, raw any, basePath string, choices map[string]UnionChoice) (map[string]any, error) {
	cls, ok := schema.FindClass(className)
	if !ok {
		return nil, fmt.Errorf("normalize: unknown class %q", className)
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("normalize: class %q expected JSON object, got %T", className, raw)
	}
	out := make(map[string]any, len(cls.Properties))
	for _, prop := range cls.Properties {
		fv, present := obj[prop.Name]
		if !present {
			// Absent optionals normalize to null; absent non-
			// optionals are a contract violation upstream of the
			// walker.
			if prop.Type.Kind != KindOptional {
				return nil, fmt.Errorf("normalize: required field %q on %q missing from mock", prop.Name, className)
			}
			out[prop.Name] = nil
			continue
		}
		nv, err := normalizeValue(schema, prop.Type, fv, basePath+"."+prop.Name, choices)
		if err != nil {
			return nil, fmt.Errorf("normalize: field %q on %q: %w", prop.Name, className, err)
		}
		out[prop.Name] = nv
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
		obj, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("map expected object, got %T", raw)
		}
		out := make(map[string]any, len(obj))
		for k, v := range obj {
			nv, err := normalizeValue(schema, *t.Inner, v, fmt.Sprintf("%s[%q]", path, k), choices)
			if err != nil {
				return nil, fmt.Errorf("map[%q]: %w", k, err)
			}
			out[k] = nv
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
// class-instance keys in schema declaration order. Inner map keys
// retain encoding/json's alphabetical order — they're not class
// instances, so there's no schema-defined ordering to preserve.
// Union arms inside the type tree resolve through `choices`.
func marshalSchemaOrdered(schema FuzzSchema, rootClass string, normalized any, choices map[string]UnionChoice) (json.RawMessage, error) {
	var buf bytes.Buffer
	if err := writeOrderedClass(&buf, schema, rootClass, normalized, "", choices); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeOrderedClass(buf *bytes.Buffer, schema FuzzSchema, className string, v any, path string, choices map[string]UnionChoice) error {
	cls, ok := schema.FindClass(className)
	if !ok {
		return fmt.Errorf("ordered: unknown class %q", className)
	}
	obj, ok := v.(map[string]any)
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
		if err := writeOrderedValue(buf, schema, prop.Type, obj[prop.Name], path+"."+prop.Name, choices); err != nil {
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
		// Sort the keys ourselves so the byte-stream matches the
		// walker's lexicographic emission order. encoding/json's
		// map encoder also sorts alphabetically, but going through
		// it would re-emit any class-instance map without the
		// schema-ordered field layout we need at outer levels.
		obj, ok := v.(map[string]any)
		if !ok {
			return fmt.Errorf("map expected object, got %T", v)
		}
		keys := sortedKeys(obj)
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			kb, err := json.Marshal(k)
			if err != nil {
				return err
			}
			buf.Write(kb)
			buf.WriteByte(':')
			if err := writeOrderedValue(buf, schema, *t.Inner, obj[k], fmt.Sprintf("%s[%q]", path, k), choices); err != nil {
				return err
			}
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

// sortedMapEntries returns a copy of entries sorted lexicographically
// by Key. encoding/json's map encoder uses the same comparison, so
// emitting in sorted order keeps walker output byte-identical to a
// post-normalize round-trip through encoding/json.
func sortedMapEntries(entries []FuzzMapEntry) []FuzzMapEntry {
	out := make([]FuzzMapEntry, len(entries))
	copy(out, entries)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1].Key > out[j].Key; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// Insertion sort (n ≤ 3 in v1).
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j-1] > keys[j]; j-- {
			keys[j-1], keys[j] = keys[j], keys[j-1]
		}
	}
	return keys
}
