package debaml

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/invakid404/baml-rest/internal/schema"
)

// coerce converts an ordered value (decoded strict, or via the
// conservative fixing pass) into the flattened dynamic output JSON for
// type t, returning a json.RawMessage. checkSupported has already rejected
// every out-of-scope kind, so coerce only handles the M2a cut-line; an
// unexpected kind is a claimed coercion error.
//
// Field and enum-value names follow BAML's rendered/canonical split: input
// keys are matched by rendered name (the alias the model is shown), and
// output keys use the canonical name — the form the downstream
// FlattenDynamicOutput / InjectAbsentOptionals / ReorderDynamicOutputBySchema
// pipeline keys on. Class fields are emitted in schema declaration order so
// that order pass remains the authority.
//
// Coercion cut-line (DELIBERATE, M2a): native matches types STRICTLY, while
// BAML's coercers are lenient (parse numeric strings, round float→int,
// stringify non-strings, fuzzy-match enums/literals via match_string, wrap
// singletons into arrays, absorb a scalar into a single-field class). Where
// native's strict match fails but BAML may leniently SUCCEED — or where
// native cannot determine BAML's exact success/failure — coercion DECLINES
// (ErrDeBAMLParseUnsupported → fall back to BAML), so native is never "more
// capable" than BAML. Native CLAIMS a coercion error only for the mismatch
// BAML also hard-rejects: a non-object where a MULTI-field class is required
// (coerceClass). A required field with no EXACT key match instead DECLINES —
// BAML may fuzzy-match the key via match_string or hard-fail, and native
// cannot tell which apart (deferred to Mcoerce) — while a class whose every
// required field is exact-matched is claimed. A consequence is that native
// also declines some inputs BAML would itself reject (e.g. {color:"MAUVE"}
// with no enum match, {version:3} for literal 2, a non-integer for an int,
// or a genuinely-absent required field) — behavior is
// identical via fallback, and precise claim-parity for those (porting BAML's
// match_string for keys/enums/literals + lenient numeric/structural coercion)
// is deferred to the Mcoerce milestone (#546). See coercePrimitive /
// coerceList / coerceEnum / coerceLiteral / coerceClass for the per-kind
// boundary.
func coerce(b *schema.Bundle, t schema.Type, input value) (json.RawMessage, error) {
	switch t.Kind {
	case schema.TypePrimitive:
		return coercePrimitive(t.Primitive, input)
	case schema.TypeLiteral:
		return coerceLiteral(t.Literal, input)
	case schema.TypeEnum:
		return coerceEnum(b, t.Name, input)
	case schema.TypeClass:
		return coerceClass(b, t.Name, t.Mode, input)
	case schema.TypeList:
		return coerceList(b, t.Elem, input)
	case schema.TypeMap:
		return coerceMap(b, t.Key, t.Value, input)
	case schema.TypeUnion:
		return coerceUnion(b, t.Union, input)
	default:
		return nil, fmt.Errorf("debaml: cannot coerce type kind %q", t.Kind)
	}
}

// coercePrimitive coerces a value to a primitive target. Native primitive
// matching is intentionally STRICT (exact JSON type, integer-exact ints),
// whereas BAML's primitive coercers are lenient: they stringify non-string
// JSON, parse numeric strings, round float→int, and so on. A strict native
// failure is therefore exactly where BAML would still succeed, so every
// mismatch / non-exact value DECLINES (ErrDeBAMLParseUnsupported → fall
// back to BAML) rather than claiming a hard error BAML would not produce.
// Porting BAML's full lenient numeric/string coercer is a later milestone;
// declining keeps parity in the meantime (BAML yields the correct coerced
// value via fallback).
func coercePrimitive(p schema.PrimitiveKind, input value) (json.RawMessage, error) {
	switch p {
	case schema.PrimitiveString:
		if input.kind != valString {
			return nil, declineCoerce("string target", input)
		}
		return marshalJSON(input.strV)
	case schema.PrimitiveInt:
		if input.kind != valNumber {
			return nil, declineCoerce("int target", input)
		}
		if _, err := input.numV.Int64(); err != nil {
			// Fractional, out-of-range, or exponent forms BAML rounds/parses.
			return nil, unsupported(fmt.Sprintf("int target: %s not an exact integer", input.numV.String()))
		}
		return json.RawMessage(input.numV.String()), nil
	case schema.PrimitiveFloat:
		if input.kind != valNumber {
			return nil, declineCoerce("float target", input)
		}
		if _, err := input.numV.Float64(); err != nil {
			return nil, unsupported(fmt.Sprintf("float target: %s not representable", input.numV.String()))
		}
		return json.RawMessage(input.numV.String()), nil
	case schema.PrimitiveBool:
		if input.kind != valBool {
			return nil, declineCoerce("bool target", input)
		}
		return marshalJSON(input.boolV)
	case schema.PrimitiveNull:
		if input.kind != valNull {
			return nil, declineCoerce("null target", input)
		}
		return json.RawMessage("null"), nil
	default:
		return nil, fmt.Errorf("debaml: unsupported primitive %q", p)
	}
}

// coerceLiteral coerces a value to a literal target. Native matching is
// EXACT, whereas BAML's literal coercion routes string literals through the
// fuzzy match_string helper (trim / strip-punctuation / case-insensitive /
// substring) and rounds/parses for int and bool literals. A non-exact
// native match is therefore where BAML's lenient matcher would still
// succeed, so any mismatch DECLINES (fall back to BAML) rather than
// claiming an error BAML would not produce.
func coerceLiteral(lit *schema.LiteralValue, input value) (json.RawMessage, error) {
	if lit == nil {
		return nil, fmt.Errorf("debaml: literal type missing value")
	}
	switch lit.Kind {
	case schema.LiteralString:
		if input.kind != valString || input.strV != lit.String {
			return nil, unsupported(fmt.Sprintf("literal string %q: no exact match (BAML fuzzy-matches)", lit.String))
		}
		return marshalJSON(input.strV)
	case schema.LiteralInt:
		if input.kind != valNumber {
			return nil, declineCoerce("literal int", input)
		}
		n, err := input.numV.Int64()
		if err != nil || n != lit.Int {
			return nil, unsupported(fmt.Sprintf("literal int %d: no exact match (BAML rounds/parses)", lit.Int))
		}
		return json.RawMessage(input.numV.String()), nil
	case schema.LiteralBool:
		if input.kind != valBool || input.boolV != lit.Bool {
			return nil, unsupported(fmt.Sprintf("literal bool %v: no exact match", lit.Bool))
		}
		return marshalJSON(input.boolV)
	default:
		return nil, fmt.Errorf("debaml: unknown literal kind %q", lit.Kind)
	}
}

// coerceEnum coerces a value to an enum target by EXACT rendered-name (or
// alias) match. BAML's enum coercion routes through the fuzzy match_string
// helper (trim / strip-punctuation / case-insensitive / accent-removal /
// substring), so a value with no exact native match may still match in
// BAML. To avoid claiming a mismatch BAML would not produce, a non-string
// input or a no-exact-match value DECLINES (fall back to BAML). An enum
// referenced but absent from the lowered bundle is a broken schema, kept as
// a claimed error.
func coerceEnum(b *schema.Bundle, name string, input value) (json.RawMessage, error) {
	if input.kind != valString {
		return nil, declineCoerce("enum target", input)
	}
	e, ok := b.FindEnum(name)
	if !ok {
		return nil, fmt.Errorf("debaml: unknown enum %q", name)
	}
	v, ok := e.ValueByRenderedName(input.strV)
	if !ok {
		return nil, unsupported(fmt.Sprintf("enum %q: %q not an exact value (BAML fuzzy-matches)", name, input.strV))
	}
	// Emit the canonical enum value name, matching BAML's enum coercion.
	return marshalJSON(v.Name.Name)
}

// coerceClass coerces an object value into a class, emitting fields in
// schema declaration order. It CLAIMS only when native is confident it
// matches BAML's structure: the input is an object and every required field
// is matched by an EXACT key. Otherwise it DECLINES, because BAML's class
// coercer is leniently broader than native's strict matching and native
// cannot tell whether BAML would succeed:
//
//   - A required field with no EXACT key match → DECLINE: BAML matches field
//     keys fuzzily (coerce_class.rs → match_string: case-insensitive /
//     punctuation-stripped / substring), so {"Name":...} may match `name`.
//   - A SINGLE-field class with a non-object input, or an object whose lone
//     field key is absent → DECLINE: BAML absorbs the value into the one
//     field via implied-key / inferred-object (coerce_class.rs:224/295/300).
//
// A MULTI-field class with a NON-OBJECT input stays CLAIMED (typeMismatch):
// BAML hard-fails turning a scalar into a multi-field object too, so the
// differential checks error parity. Extra/unknown input keys are ignored on
// both sides (native iterates only schema fields). Precise key-matching and
// lenient structural coercion are deferred to the Mcoerce milestone (#546).
func coerceClass(b *schema.Bundle, name string, mode schema.StreamingMode, input value) (json.RawMessage, error) {
	cls, ok := b.FindClass(name, mode)
	if !ok {
		return nil, fmt.Errorf("debaml: unknown class %q", name)
	}
	singleField := len(cls.Fields) == 1
	if input.kind != valObject {
		if singleField {
			return nil, declineCoerce("single-field class (BAML implied-key)", input)
		}
		return nil, typeMismatch("object", input)
	}
	if singleField {
		if _, present := lookupField(input.objV, cls.Fields[0].Name); !present {
			return nil, unsupported(fmt.Sprintf("debaml: single-field class %q: lone field absent (BAML implied-key may coerce the object)", name))
		}
	}

	var buf bytes.Buffer
	buf.WriteByte('{')
	first := true
	for i := range cls.Fields {
		f := &cls.Fields[i]
		val, present := lookupField(input.objV, f.Name)
		if !present {
			if isOptional(f.Type) {
				// Absent optional: omit it. The downstream
				// InjectAbsentOptionals pass inserts the null, identically
				// for the native and BAML paths.
				continue
			}
			// A required field with no EXACT key match: BAML matches field
			// keys fuzzily (coerce_class.rs → match_string: case-insensitive
			// / punctuation-stripped / substring), so it may coerce a
			// differently-cased or near key that native's exact lookup misses
			// (e.g. {"Name":...} → name) — or it may truly hard-fail. Native
			// cannot tell which without match_string, so it DECLINES (fall
			// back to BAML) rather than claim a missing-required error that
			// would be WRONG when BAML fuzzy-matches. Precise key-matching
			// parity is deferred to the Mcoerce milestone (#546).
			return nil, unsupported(fmt.Sprintf("class %q: required field %q has no exact key match (BAML may fuzzy-match keys)", name, f.Name.RenderedName()))
		}
		child, err := coerce(b, f.Type, val)
		if err != nil {
			return nil, err
		}
		if !first {
			buf.WriteByte(',')
		}
		first = false
		key, err := marshalJSON(f.Name.Name)
		if err != nil {
			return nil, err
		}
		buf.Write(key)
		buf.WriteByte(':')
		buf.Write(child)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

func coerceList(b *schema.Bundle, elem *schema.Type, input value) (json.RawMessage, error) {
	if elem == nil {
		return nil, fmt.Errorf("debaml: list type missing element")
	}
	if input.kind != valArray {
		// BAML wraps a non-array value into a one-element array; native is
		// stricter, so decline rather than claim a mismatch BAML would not
		// produce.
		return nil, declineCoerce("list target", input)
	}
	var buf bytes.Buffer
	buf.WriteByte('[')
	for i := range input.arrV {
		child, err := coerce(b, *elem, input.arrV[i])
		if err != nil {
			return nil, err
		}
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.Write(child)
	}
	buf.WriteByte(']')
	return buf.Bytes(), nil
}

// coerceMap coerces a JSON object into a map, emitting entries in INPUT key
// order (the opposite of coerceClass's schema order: a map's keys are data,
// a class's fields are the schema). It CLAIMS only the clean-map subset and
// DECLINES everything BAML would represent as a partial/scored map — because
// BAML's map coercer does NOT hard-fail on a bad entry: it records a
// score-bearing MapKeyParseError / MapValueParseError and SKIPS that entry,
// returning a partial map. Native cannot reproduce that partial result, and
// must never claim a clean map where BAML would return a flagged/partial
// one, so any uncertainty DECLINES (ErrDeBAMLParseUnsupported → fall back):
//
//   - Non-object input → DECLINE: BAML has object/map coercion + scoring
//     outside this clean subset (it is not a hard type error).
//   - A key with no EXACT match (matchMapKey) → DECLINE: BAML's enum/literal
//     key coercion is fuzzy (match_string), and a no-match key is skipped
//     with a flag rather than failing the map. Fuzzy key matching is Mcoerce.
//   - ANY value coercion error (sentinel OR claimed) → DECLINE the WHOLE
//     map: BAML attaches MapValueParseError and skips just that entry, so a
//     native hard-claim here would diverge from BAML's partial success.
//   - A duplicate output key → DECLINE: BAML's IndexMap insert/overwrite
//     ordering for duplicates is unproven, so native does not claim it.
//
// Accepted entries are keyed by the ORIGINAL input key string (never the
// canonical enum/literal form, matching coerce_map.rs:165-174), and an empty
// object is a clean empty map. A class value is emitted by coerceClass in
// schema order, nested inside the input-ordered map.
func coerceMap(b *schema.Bundle, keyT, valT *schema.Type, input value) (json.RawMessage, error) {
	if keyT == nil || valT == nil {
		return nil, fmt.Errorf("debaml: map type missing key or value")
	}
	if input.kind != valObject {
		// Not a hard type error: BAML's object/map coercion + scoring can
		// still produce a (partial/flagged) map from a non-object, so decline
		// rather than claim a mismatch BAML would not produce.
		return nil, declineCoerce("map target", input)
	}

	var buf bytes.Buffer
	buf.WriteByte('{')
	seen := make(map[string]struct{}, len(input.objV))
	first := true
	for i := range input.objV {
		f := &input.objV[i]
		if err := matchMapKey(b, *keyT, f.key); err != nil {
			return nil, err
		}
		if _, dup := seen[f.key]; dup {
			return nil, unsupported(fmt.Sprintf("map duplicate key %q (BAML duplicate insert/overwrite ordering unproven)", f.key))
		}
		seen[f.key] = struct{}{}

		child, err := coerce(b, *valT, f.val)
		if err != nil {
			// A bad value: BAML records MapValueParseError and SKIPS this
			// entry, returning a partial map — native cannot reproduce that, so
			// decline the whole map (regardless of whether the child error was
			// a sentinel decline or a claimed mismatch).
			return nil, unsupported(fmt.Sprintf("map value for key %q: %v (BAML skips bad entries via MapValueParseError)", f.key, err))
		}

		if !first {
			buf.WriteByte(',')
		}
		first = false
		key, err := marshalJSON(f.key)
		if err != nil {
			return nil, err
		}
		buf.Write(key)
		buf.WriteByte(':')
		buf.Write(child)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// matchMapKey validates an input object key string against the declared map
// key type by EXACT match only, returning nil on a clean accept and a
// DECLINE sentinel otherwise. checkSupportedMapKey has already restricted
// the key type to the four legal shapes, so the default arm is defensive.
//
//   - string primitive: accept any key verbatim.
//   - enum: require an EXACT rendered-name/alias match (the same exact path
//     coerceEnum uses); BAML's full enum key coercion is fuzzy (match_string)
//     so a non-exact key is Mcoerce, not a native claim.
//   - string literal: require exact string equality.
//   - union of string literals: require EXACTLY ONE flattened literal equal
//     to the key (no match, or a duplicate-literal ambiguity, declines).
//
// The accepted key is NOT rewritten — coerceMap emits the original input key
// string, matching BAML's insertion of the raw object key.
func matchMapKey(b *schema.Bundle, keyT schema.Type, key string) error {
	switch keyT.Kind {
	case schema.TypePrimitive:
		if keyT.Primitive == schema.PrimitiveString {
			return nil
		}
		return unsupported(fmt.Sprintf("map key primitive %q", keyT.Primitive))
	case schema.TypeEnum:
		e, ok := b.FindEnum(keyT.Name)
		if !ok {
			return fmt.Errorf("debaml: unknown enum %q", keyT.Name)
		}
		if _, ok := e.ValueByRenderedName(key); !ok {
			return unsupported(fmt.Sprintf("map key %q: no exact enum %q match (BAML fuzzy-matches)", key, keyT.Name))
		}
		return nil
	case schema.TypeLiteral:
		if keyT.Literal == nil || keyT.Literal.Kind != schema.LiteralString {
			return unsupported("map key literal must be a string literal")
		}
		if key != keyT.Literal.String {
			return unsupported(fmt.Sprintf("map key %q: no exact literal %q match (BAML fuzzy-matches)", key, keyT.Literal.String))
		}
		return nil
	case schema.TypeUnion:
		matches := 0
		for _, lit := range flattenStringLiterals(keyT) {
			if lit == key {
				matches++
			}
		}
		if matches != 1 {
			return unsupported(fmt.Sprintf("map key %q: %d exact string-literal-union matches (need exactly 1; BAML fuzzy-matches)", key, matches))
		}
		return nil
	default:
		return unsupported(fmt.Sprintf("map key kind %q", keyT.Kind))
	}
}

// flattenStringLiterals collects, in declaration order, every string-literal
// value reachable from a (recursively nested) union — the candidate set a
// union-of-string-literals map key is matched against. checkSupportedMapKey
// has already proven t is a string-literal union, so non-literal/non-union
// members are not expected and are skipped defensively.
func flattenStringLiterals(t schema.Type) []string {
	if t.Union == nil {
		return nil
	}
	var out []string
	for i := range t.Union.Variants {
		v := &t.Union.Variants[i]
		switch v.Kind {
		case schema.TypeLiteral:
			if v.Literal != nil && v.Literal.Kind == schema.LiteralString {
				out = append(out, v.Literal.String)
			}
		case schema.TypeUnion:
			out = append(out, flattenStringLiterals(*v)...)
		}
	}
	return out
}

// coerceUnion handles the only union shape M1/M2a supports: an optional —
// a nullable union with exactly one non-null variant. A JSON null becomes
// null; anything else coerces against the lone variant. checkSupported has
// already rejected every other union, so this is reached only for
// optionals.
func coerceUnion(b *schema.Bundle, u *schema.UnionType, input value) (json.RawMessage, error) {
	if u == nil {
		return nil, fmt.Errorf("debaml: union type missing payload")
	}
	if input.kind == valNull {
		if !u.Nullable {
			return nil, typeMismatch("non-nullable union", input)
		}
		return json.RawMessage("null"), nil
	}
	if len(u.Variants) != 1 {
		return nil, fmt.Errorf("debaml: general union scoring is unsupported")
	}
	return coerce(b, u.Variants[0], input)
}

// lookupField returns the input value for a class field, matched by the
// rendered name ONLY (the alias when present, else the canonical name) —
// the same key BAML's jsonish class coercer matches against
// (name.rendered_name()). An aliased field is therefore NOT matched by its
// canonical name: BAML treats the canonical key as an extra field and the
// rendered key as missing, and the native path must agree to stay
// drift-free. For a field with no alias, RenderedName()==Name, so this is
// unchanged. Duplicate input keys resolve last-wins, matching the
// encoding/json map decode the M1 path used. The comma-ok form
// distinguishes an absent key from a present null value.
func lookupField(obj []field, name schema.Name) (value, bool) {
	rendered := name.RenderedName()
	var found value
	ok := false
	for i := range obj {
		if obj[i].key == rendered {
			found = obj[i].val
			ok = true
		}
	}
	return found, ok
}

// isOptional reports whether t is an optional (a nullable union), the only
// type for which an absent field is tolerated rather than a parse error.
func isOptional(t schema.Type) bool {
	return t.Kind == schema.TypeUnion && t.Union != nil && t.Union.Nullable
}

// marshalJSON encodes v to compact JSON without HTML escaping, so emitted
// strings read naturally. (Comparison against BAML is semantic — both
// sides are decoded before diffing — so escaping never affects parity.)
func marshalJSON(v any) (json.RawMessage, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return json.RawMessage(bytes.TrimRight(buf.Bytes(), "\n")), nil
}

// typeMismatch reports a conservative JSON-type mismatch as a CLAIMED
// coercion error. Used where BAML would also fail (e.g. a scalar/array
// where a class object is required), so the differential checks error
// parity rather than masking it behind a fallback.
func typeMismatch(want string, input value) error {
	return fmt.Errorf("debaml: expected %s, got %s", want, input.kind.String())
}

// declineCoerce reports a coercion mismatch as a DECLINE
// (ErrDeBAMLParseUnsupported → fall back to BAML), for the cases where
// native's strict matching is narrower than BAML's lenient coercers, so a
// native "failure" is exactly where BAML would still succeed. Distinct from
// typeMismatch, which claims an error BAML would also produce.
func declineCoerce(target string, input value) error {
	return unsupported(fmt.Sprintf("%s: got %s (native stricter than BAML)", target, input.kind.String()))
}
