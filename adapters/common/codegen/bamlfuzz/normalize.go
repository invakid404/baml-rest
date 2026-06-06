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
	// Expected is the schema-normalized parsed value that matches
	// what the BAML parser produces: present optionals carry their
	// inner value, explicit-null optionals appear as JSON null. An
	// absent optional reached through a deterministic (non-union)
	// path appears as JSON null, matching the static path and the
	// dynamic path's InjectAbsentOptionals post-processing. An absent
	// optional reached through a union arm is omitted entirely: the
	// runtime re-derives the active arm structurally and can land on a
	// different arm than the recorded one, so it never injects the
	// null and the key stays off the wire.
	Expected json.RawMessage
	// Metadata is per-case provenance the failure envelope embeds.
	Metadata CaseMetadata
}

// WalkOption configures optional Walk behaviour.
type WalkOption func(*walkState)

// WithPreserveSchemaOrder is a legacy option that previously
// controlled whether absent optional fields appeared in Expected
// output. Since the dynamic path now always injects absent optionals
// as null (matching the static path), this option is accepted but
// ignored — absent optionals always appear as JSON null.
func WithPreserveSchemaOrder(v bool) WalkOption {
	return func(s *walkState) {}
}

// WithStaticLiteralEcho makes the Expected output mirror how the static
// (BAML-source) oracle path round-trips a `literal<string>` field whose
// value contains characters that strconv.Quote escapes (embedded
// double-quotes or backslashes).
//
// Upstream BAML quirk (boundaryml/baml, reproduced on 0.219.0 and
// 0.222.0): a string literal type is written into .baml source via
// strconv.Quote (emit_static.go's literalSpelling), e.g. the value
// `"quoted"` becomes the source token `"\"quoted\""`. BAML stores the
// raw token body — `\"quoted\"`, with the source escapes left intact —
// as the literal's value and echoes that back verbatim. The parsed
// REST response for the field is therefore `\"quoted\"`, not the
// decoded `"quoted"`. This affects only the static path: the dynamic
// oracle hands the raw Go value to TypeBuilder.LiteralString
// (emit_dynamic.go), so no source token is ever parsed and the value
// round-trips unescaped.
//
// With this option the Expected string-literal value is the body of
// strconv.Quote(value) (surrounding quotes stripped), matching BAML's
// actual output so the static oracle does not flag a spurious diff.
// This is a comparison-only adjustment scoped to the static oracle;
// generated code and serialization are untouched. Remove when the
// upstream fix lands.
func WithStaticLiteralEcho() WalkOption {
	return func(s *walkState) { s.staticLiteralEcho = true }
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
func Walk(schema FuzzSchema, value FuzzValue, opts ...WalkOption) (WalkResult, error) {
	meta := CaseMetadata{
		OptionalShapes:  make(map[string]string),
		RecursionDepths: make(map[string]int),
		UnionChoices:    make(map[string]UnionChoice),
	}
	state := &walkState{schema: schema, meta: &meta, depths: make(map[string]int)}
	for _, opt := range opts {
		opt(state)
	}
	root := schema.EffectiveRoot()
	var mockBuf, expectBuf bytes.Buffer
	if err := state.renderValueMock(&mockBuf, root, value, ""); err != nil {
		return WalkResult{}, err
	}
	if err := state.renderValueExpected(&expectBuf, root, value, false); err != nil {
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
	// staticLiteralEcho selects how a string literal renders into the
	// Expected output. See WithStaticLiteralEcho for the upstream-BAML
	// quirk it works around.
	staticLiteralEcho bool
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

// renderClassExpected renders a class instance into the expected
// (schema-normalized) JSON. `inUnion` is true when this class was
// reached through one or more union arms: see renderValueExpected's
// KindOptional/KindUnion handling for why absent optionals are
// omitted rather than nulled in that case.
func (s *walkState) renderClassExpected(buf *bytes.Buffer, val FuzzValue, inUnion bool) error {
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
	first := true
	for _, prop := range cls.Properties {
		fv, ok := val.LookupField(prop.Name)
		if !ok {
			return fmt.Errorf("walk: missing field %q on class %q in expected", prop.Name, cls.Name)
		}
		// Inside a union arm an absent optional produces no key at all.
		// The runtime's InjectAbsentOptionals re-derives the active
		// union arm structurally from the wire value, and that
		// derivation can land on a different arm than the one that
		// actually generated the value (or one without this optional),
		// so the null is never injected and the key stays off the wire.
		// Outside a union the deterministic inject-null contract still
		// holds, so absent optionals there render as JSON null below.
		if inUnion && prop.Type.Kind == KindOptional && fv.OptionalShape == OptionalAbsent {
			continue
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
		if err := s.renderValueExpected(buf, prop.Type, fv, inUnion); err != nil {
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

// renderValueExpected renders one value into the expected JSON.
// `inUnion` propagates whether the value sits beneath a union arm so
// renderClassExpected can omit (rather than null) absent optionals
// there. Descending into a union variant sets it for the subtree; it
// never resets, since once the runtime's arm derivation can diverge
// every absent optional below is unreliable.
func (s *walkState) renderValueExpected(buf *bytes.Buffer, t FuzzType, val FuzzValue, inUnion bool) error {
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
		if s.staticLiteralEcho {
			return writeLiteralStaticEcho(buf, t.Literal)
		}
		return writeLiteral(buf, t.Literal)
	case KindEnumRef:
		return writeJSON(buf, val.Enum)
	case KindOptional:
		if val.OptionalShape == OptionalPresent {
			if val.Inner == nil || t.Inner == nil {
				return fmt.Errorf("expected: present optional missing inner")
			}
			return s.renderValueExpected(buf, *t.Inner, *val.Inner, inUnion)
		}
		// Absent and null collapse to JSON null. (An absent optional
		// inside a union arm is omitted by renderClassExpected before it
		// ever reaches here, so this null is only ever the explicit-null
		// shape or a non-union absent optional.)
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
			if err := s.renderValueExpected(buf, *t.Inner, item, inUnion); err != nil {
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
			if err := s.renderValueExpected(buf, *t.Inner, entry.Value, inUnion); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
		return nil
	case KindClassRef:
		return s.renderClassExpected(buf, val, inUnion)
	case KindUnion:
		if val.Kind != KindUnion || val.Variant == nil {
			return fmt.Errorf("expected: union value missing selected variant")
		}
		if val.VariantIndex < 0 || val.VariantIndex >= len(t.Variants) {
			return fmt.Errorf("expected: union variant index %d out of range [0,%d)", val.VariantIndex, len(t.Variants))
		}
		return s.renderValueExpected(buf, t.Variants[val.VariantIndex], *val.Variant, true)
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

// writeLiteralStaticEcho renders a literal into the Expected output the
// way the static (BAML-source) oracle path observes it round-tripped.
// Only string literals differ from writeLiteral: their value is echoed
// as the body of its strconv.Quote spelling rather than the decoded
// value. See WithStaticLiteralEcho for the upstream-BAML quirk this
// mirrors. Int and bool literals are unaffected and delegate to
// writeLiteral.
func writeLiteralStaticEcho(buf *bytes.Buffer, lit *FuzzLiteral) error {
	if lit == nil {
		return fmt.Errorf("literal type missing payload")
	}
	if lit.Kind == LiteralString {
		return writeJSON(buf, bamlStaticStringLiteralEcho(lit.String))
	}
	return writeLiteral(buf, lit)
}

// bamlStaticStringLiteralEcho returns a string-literal value as the
// static BAML path echoes it: the body of strconv.Quote(s) with the
// surrounding quotes stripped. For values without escape-worthy
// characters (e.g. "alpha", "") this equals s; for values BAML's source
// grammar escapes (embedded `"` or `\`) it carries the escaped source
// token BAML fails to decode. See WithStaticLiteralEcho.
func bamlStaticStringLiteralEcho(s string) string {
	quoted := strconv.Quote(s)
	return quoted[1 : len(quoted)-1]
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
// class graph to produce the canonical-equivalent of the walker's
// Expected output: class instance keys follow schema declaration
// order, and map keys retain the mock's wire order. An absent optional
// field is emitted as JSON null outside a union arm and omitted within
// one — see WalkResult.Expected and renderClassExpected.
//
// Union schemas require union-choice metadata to disambiguate which
// arm produced the mock; call NormalizeMockToExpectedWithChoices
// directly when the schema contains KindUnion. This entry point
// fails closed on encountering a union without recorded choices.
func NormalizeMockToExpected(schema FuzzSchema, mock json.RawMessage, rootClass string) (json.RawMessage, error) {
	return NormalizeMockToExpectedWithChoices(schema, mock, rootClass, nil, true)
}

// NormalizeMockToExpectedWithChoices is the union-aware variant. The
// `choices` map mirrors CaseMetadata.UnionChoices: each entry tells
// the normalizer which arm produced the JSON at that path. The
// `preserve` parameter is accepted for API compatibility; absent
// optional fields are emitted as JSON null outside a union arm and
// omitted within one (see NormalizeMockToExpected).
//
// Normalization is driven by the schema's effective root type
// (FuzzSchema.EffectiveRoot): when the schema sets RootType the
// payload is normalized against that type directly, so raw-root
// union/list/map/scalar schemas round-trip correctly. When RootType
// is nil the effective root is a class ref pointing at RootClass,
// keeping the v1 behaviour. The `rootClass` parameter is honoured
// only as a fallback for callers that supply it before RootType was
// added to the IR.
func NormalizeMockToExpectedWithChoices(schema FuzzSchema, mock json.RawMessage, rootClass string, choices map[string]UnionChoice, preserve bool) (json.RawMessage, error) {
	raw, err := decodePreserveOrder(mock)
	if err != nil {
		return nil, fmt.Errorf("normalize: parse mock: %w", err)
	}
	root := effectiveNormalizeRoot(schema, rootClass)
	out, err := normalizeValue(schema, root, raw, "", choices, preserve, false)
	if err != nil {
		return nil, err
	}
	return marshalSchemaOrdered(schema, root, out, choices, preserve)
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

// normalizeClass builds the order-preserving carrier for a class
// instance. `inUnion` reports whether this class was reached through a
// union arm: an absent optional is then left out of the carrier
// entirely (so the re-emitter omits it), mirroring renderClassExpected
// and the runtime's union-arm-derived InjectAbsentOptionals behaviour.
// Outside a union the missing optional is recorded as nil so it
// re-emits as JSON null.
func normalizeClass(schema FuzzSchema, className string, raw any, basePath string, choices map[string]UnionChoice, preserve, inUnion bool) (*bamlutils.OrderedMap[any], error) {
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
			if prop.Type.Kind != KindOptional {
				return nil, fmt.Errorf("normalize: required field %q on %q missing from mock", prop.Name, className)
			}
			if inUnion {
				continue
			}
			if err := out.Set(prop.Name, nil); err != nil {
				return nil, err
			}
			continue
		}
		nv, err := normalizeValue(schema, prop.Type, fv, basePath+"."+prop.Name, choices, preserve, inUnion)
		if err != nil {
			return nil, fmt.Errorf("normalize: field %q on %q: %w", prop.Name, className, err)
		}
		if err := out.Set(prop.Name, nv); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func normalizeValue(schema FuzzSchema, t FuzzType, raw any, path string, choices map[string]UnionChoice, preserve, inUnion bool) (any, error) {
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
		return normalizeValue(schema, *t.Inner, raw, path, choices, preserve, inUnion)
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
			nv, err := normalizeValue(schema, *t.Inner, item, fmt.Sprintf("%s[%d]", path, i), choices, preserve, inUnion)
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
			nv, err := normalizeValue(schema, *t.Inner, v, fmt.Sprintf("%s[%q]", path, k), choices, preserve, inUnion)
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
		return normalizeClass(schema, t.Ref, raw, path, choices, preserve, inUnion)
	case KindUnion:
		choice, ok := choices[path]
		if !ok {
			return nil, fmt.Errorf("normalize: union at %q has no recorded choice", path)
		}
		if choice.Index < 0 || choice.Index >= len(t.Variants) {
			return nil, fmt.Errorf("normalize: union choice index %d out of range at %q", choice.Index, path)
		}
		return normalizeValue(schema, t.Variants[choice.Index], raw, path+":v", choices, preserve, true)
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
func marshalSchemaOrdered(schema FuzzSchema, root FuzzType, normalized any, choices map[string]UnionChoice, preserve bool) (json.RawMessage, error) {
	var buf bytes.Buffer
	if err := writeOrderedValue(&buf, schema, root, normalized, "", choices, preserve, false); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// writeOrderedClass re-emits a class instance in schema declaration
// order. `inUnion` reports whether this class sits beneath a union arm.
// A declared optional missing from the carrier re-emits as JSON null
// outside a union (the inject-null contract); inside a union it is
// omitted, matching normalizeClass / renderClassExpected and the
// runtime's union-arm-derived InjectAbsentOptionals behaviour.
func writeOrderedClass(buf *bytes.Buffer, schema FuzzSchema, className string, v any, path string, choices map[string]UnionChoice, preserve, inUnion bool) error {
	cls, ok := schema.FindClass(className)
	if !ok {
		return fmt.Errorf("ordered: unknown class %q", className)
	}
	obj, ok := v.(*bamlutils.OrderedMap[any])
	if !ok {
		return fmt.Errorf("ordered: class %q expected object, got %T", className, v)
	}
	buf.WriteByte('{')
	first := true
	for _, prop := range cls.Properties {
		fv, present := obj.Get(prop.Name)
		if !present && prop.Type.Kind == KindOptional {
			if inUnion {
				continue
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
			buf.WriteString(":null")
			continue
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
		if err := writeOrderedValue(buf, schema, prop.Type, fv, path+"."+prop.Name, choices, preserve, inUnion); err != nil {
			return err
		}
	}
	buf.WriteByte('}')
	return nil
}

func writeOrderedValue(buf *bytes.Buffer, schema FuzzSchema, t FuzzType, v any, path string, choices map[string]UnionChoice, preserve, inUnion bool) error {
	switch t.Kind {
	case KindOptional:
		if v == nil {
			buf.WriteString("null")
			return nil
		}
		return writeOrderedValue(buf, schema, *t.Inner, v, path, choices, preserve, inUnion)
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
			if err := writeOrderedValue(buf, schema, *t.Inner, item, fmt.Sprintf("%s[%d]", path, i), choices, preserve, inUnion); err != nil {
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
			if err := writeOrderedValue(buf, schema, *t.Inner, mv, fmt.Sprintf("%s[%q]", path, k), choices, preserve, inUnion); err != nil {
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
		return writeOrderedClass(buf, schema, t.Ref, v, path, choices, preserve, inUnion)
	case KindUnion:
		choice, ok := choices[path]
		if !ok {
			return fmt.Errorf("ordered: union at %q has no recorded choice", path)
		}
		if choice.Index < 0 || choice.Index >= len(t.Variants) {
			return fmt.Errorf("ordered: union choice index %d out of range at %q", choice.Index, path)
		}
		return writeOrderedValue(buf, schema, t.Variants[choice.Index], v, path+":v", choices, preserve, true)
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		buf.Write(b)
		return nil
	}
}
