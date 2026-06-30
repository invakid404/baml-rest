package debaml

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/internal/schema"
)

// coerceFlags accumulates whether a coercion produced ANY BAML score-bearing
// condition native CLAIMS (and is therefore NOT a zero-score "clean" success):
// a SubstringMatch (enum/literal/map-key substring, cost 2), an ExtraKey
// (unmatched class input key, cost 1 each), or an ObjectToMap (every object→map,
// cost 1). It is threaded only where cleanliness matters — the NULLABLE-union
// claim, where BAML's null arm competes by scoring (DefaultButHadValue, cost
// 110): only a clean (score-0) non-null arm provably beats null without native
// having to compute scores. A nil receiver means "don't track" (the top-level
// and non-nullable paths), so threading it adds no overhead there.
type coerceFlags struct {
	flagged bool
}

// flag marks the coercion as non-clean. Nil-safe so untracked paths are free.
func (f *coerceFlags) flag() {
	if f != nil {
		f.flagged = true
	}
}

// isFlagged reports whether any score-bearing condition was recorded.
func (f *coerceFlags) isFlagged() bool { return f != nil && f.flagged }

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
// Coercion cut-line (Mcoerce-a): native now matches enum values, string
// literals, class field keys, and map keys through BAML's actual match_string
// (trim / accent+ligature fold / punctuation strip / case-insensitive /
// substring — see match_string.go), so fuzzy matches BAML accepts are CLAIMED
// byte-exact. The remaining leniencies stay STRICT and DECLINE: numeric/bool
// string parsing, float→int rounding, non-string stringification (ObjectToString
// / JsonToString), single-to-array wrapping, single-field implied-key /
// inferred-object absorption, partial list/map skips, and class default-fill —
// all deferred to Mcoerce-b/c/d. Where native's match fails but BAML may
// leniently SUCCEED (or null-default) — or where native cannot determine BAML's
// exact success/failure — coercion DECLINES (ErrDeBAMLParseUnsupported → fall
// back to BAML), so native is never "more capable" than BAML.
//
// Native CLAIMS a coercion error only where BAML also errors: a non-object
// where a MULTI-field class is required (coerceClass typeMismatch), or a
// match_string substring TIE (StrMatchOneFromMany — coerceEnum/coerceLiteral
// ambiguousMatch). A required field still unmatched after fuzzy key matching
// DECLINES — BAML may default-fill or hard-fail and native cannot tell which.
// Union claims count LENIENT per-variant successes and claim only the lone
// winner; two-plus successes are BAML pick_best (M3) and DECLINE. A consequence
// is that native still declines some inputs BAML would itself reject (e.g. a
// required field with no fuzzy key match, {version:3} for literal 2, a
// non-integer for an int) — behavior is identical via fallback, and precise
// claim-parity for those is deferred to the rest of the Mcoerce milestone
// (#546). See coercePrimitive / coerceList / coerceEnum / coerceLiteral /
// coerceClass for the per-kind boundary.
func coerce(b *schema.Bundle, t schema.Type, input value, f *coerceFlags) (json.RawMessage, error) {
	switch t.Kind {
	case schema.TypePrimitive:
		// Primitives match strictly in Mcoerce-a (exact JSON type), so they
		// never add a score-bearing flag — no need to thread f.
		return coercePrimitive(t.Primitive, input)
	case schema.TypeLiteral:
		return coerceLiteral(t.Literal, input, f)
	case schema.TypeEnum:
		return coerceEnum(b, t.Name, input, f)
	case schema.TypeClass:
		return coerceClass(b, t.Name, t.Mode, input, f)
	case schema.TypeList:
		return coerceList(b, t.Elem, input, f)
	case schema.TypeMap:
		return coerceMap(b, t.Key, t.Value, input, f)
	case schema.TypeUnion:
		return coerceUnionSafe(b, t.Union, input, f)
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

// coerceLiteral coerces a value to a literal target. String literals route
// through BAML's actual match_string (Mcoerce-a) — trim / fold / strip /
// case-insensitive / substring — emitting the canonical literal (not the
// fuzzy raw input); a substring tie is a CLAIMED error and a no-match
// DECLINES. Int and bool literals still match EXACTLY: BAML rounds/parses
// them (string→int, float→int), which is Mcoerce-b, so a non-exact int/bool
// DECLINES rather than claiming a mismatch BAML would not produce. A
// non-string input to a string literal also DECLINES (BAML's single-key
// object ObjectToPrimitive extraction is Mcoerce-d).
func coerceLiteral(lit *schema.LiteralValue, input value, f *coerceFlags) (json.RawMessage, error) {
	if lit == nil {
		return nil, fmt.Errorf("debaml: literal type missing value")
	}
	switch lit.Kind {
	case schema.LiteralString:
		if input.kind != valString {
			return nil, declineCoerce("literal string", input)
		}
		matched, outcome, viaSub := matchString(input.strV, []matchCandidate{{name: lit.String, validValues: []string{lit.String}}}, true)
		switch outcome {
		case matchOne:
			if viaSub {
				f.flag() // SubstringMatch (cost 2)
			}
			return marshalJSON(matched)
		case matchAmbiguous:
			// A single candidate cannot tie; defensive.
			return nil, ambiguousMatch(fmt.Sprintf("literal string %q", lit.String), input.strV)
		default:
			return nil, unsupported(fmt.Sprintf("literal string %q: %q matches no value (BAML errors or null-defaults)", lit.String, input.strV))
		}
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

// coerceEnum coerces a string value to an enum target via BAML's actual
// match_string (Mcoerce-a): trim / accent+ligature fold / punctuation strip
// / case-insensitive / substring, against each value's rendered name,
// description, and "rendered: description" form (enumMatchCandidates). The
// canonical real name of the matched value is emitted. A substring TIE is a
// CLAIMED error (StrMatchOneFromMany — BAML errors before emitting), while a
// no-match DECLINES: BAML errors in a required position but null-defaults in
// an optional one (DefaultButHadValue), and native cannot tell which apart
// without scoring, so it falls back. A non-string input also DECLINES — BAML
// stringifies it via jsonish Value Display (ObjectToString), whose exact
// reproduction is Mcoerce-b/d. An enum absent from the lowered bundle is a
// broken schema, kept as a claimed error.
func coerceEnum(b *schema.Bundle, name string, input value, f *coerceFlags) (json.RawMessage, error) {
	if input.kind != valString {
		return nil, declineCoerce("enum target", input)
	}
	e, ok := b.FindEnum(name)
	if !ok {
		return nil, fmt.Errorf("debaml: unknown enum %q", name)
	}
	matched, outcome, viaSub := matchString(input.strV, enumMatchCandidates(e), true)
	switch outcome {
	case matchOne:
		if viaSub {
			f.flag() // SubstringMatch (cost 2)
		}
		// matched is the candidate name = the value's canonical real name.
		return marshalJSON(matched)
	case matchAmbiguous:
		return nil, ambiguousMatch(fmt.Sprintf("enum %q", name), input.strV)
	default:
		return nil, unsupported(fmt.Sprintf("enum %q: %q matches no value (BAML errors or null-defaults)", name, input.strV))
	}
}

// enumMatchCandidates builds the (real_name, valid_values) candidate set
// BAML's enum coercer matches against (coerce_enum.rs:14): each value's
// rendered name, plus — when it has a non-empty trimmed description — that
// description and the "rendered: description" form. The candidate NAME is
// the canonical real name, which match_string returns and coerceEnum emits.
func enumMatchCandidates(e *schema.EnumDef) []matchCandidate {
	cands := make([]matchCandidate, 0, len(e.Values))
	for i := range e.Values {
		v := &e.Values[i]
		rendered := v.Name.RenderedName()
		var vals []string
		if v.Description != nil {
			if d := strings.TrimSpace(*v.Description); d != "" {
				vals = []string{rendered, d, rendered + ": " + d}
			}
		}
		if vals == nil {
			vals = []string{rendered}
		}
		cands = append(cands, matchCandidate{name: v.Name.Name, validValues: vals})
	}
	return cands
}

// coerceClass coerces an object value into a class, emitting fields in
// schema declaration order. Input keys are matched to fields by BAML's
// actual no-substring match_string (Mcoerce-a, via matchesStringToString),
// reproducing coerce_class.rs: each input key is assigned to the FIRST field
// whose rendered name it matches (case-insensitive / punctuation-stripped /
// accent-folded — but NOT substring); when two input keys match the same
// field the FIRST keeps it (update_map's "keep first", coerce_class.rs:548);
// extra/unknown keys are ignored (ExtraKey, not emitted). It CLAIMS when the
// input is an object and every required field is matched and coerces;
// otherwise it DECLINES, because BAML's class coercer is leniently broader
// than native and native cannot tell whether BAML would succeed:
//
//   - A required field still unmatched after fuzzy key matching → DECLINE:
//     BAML may fill a type default (list/map/null/union) or hard-fail
//     (coerce_class.rs:342/field_type.rs:288), which native cannot reproduce.
//   - A SINGLE-field class with a non-object input, or an object NONE of
//     whose keys match the lone field → DECLINE: BAML absorbs the value into
//     the one field via implied-key / inferred-object, or fills a default
//     for an empty object (coerce_class.rs:224/295/313) — all Mcoerce-d.
//
// A MULTI-field class with a NON-OBJECT input stays CLAIMED (typeMismatch):
// BAML hard-fails turning a scalar into a multi-field object too, so the
// differential checks error parity. A claimed child error (e.g. an enum
// substring tie) propagates so the differential checks error parity; lenient
// structural coercion (defaults, implied keys) is deferred to Mcoerce-d. Any
// EXTRA input key (ExtraKey, cost 1) and any flagged child flag cf, so a
// nullable-union claim can require this class arm to be clean.
func coerceClass(b *schema.Bundle, name string, mode schema.StreamingMode, input value, cf *coerceFlags) (json.RawMessage, error) {
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

	// Assign input keys to fields the way BAML does: iterate input keys in
	// order, and for each map it to the FIRST field whose rendered name it
	// fuzzily matches (no substring). When two input keys match the same field,
	// the FIRST keeps it (update_map "keep first") — later duplicates are
	// ignored, NOT treated as extras. An input key matching no field is an
	// extra (ExtraKey). This input-key-first order — rather than scanning
	// fields for a matching key — is what makes one key resolve to a single
	// field, avoiding a key being assigned to two fold-equal fields at once.
	assigned := make([]value, len(cls.Fields))
	present := make([]bool, len(cls.Fields))
	foundAny := false
	hasExtra := false
	for i := range input.objV {
		key := input.objV[i].key
		matchedField := -1
		for j := range cls.Fields {
			if matchesStringToString(key, cls.Fields[j].Name.RenderedName()) {
				matchedField = j
				break
			}
		}
		switch {
		case matchedField < 0:
			hasExtra = true // unmatched input key -> ExtraKey
		case present[matchedField]:
			// Duplicate match for an already-filled field: keep first, ignore.
		default:
			assigned[matchedField] = input.objV[i].val
			present[matchedField] = true
			foundAny = true
		}
	}
	if singleField && !foundAny {
		// No input key matched the lone field: BAML tries implied-key /
		// inferred-object on the whole object, or fills a default for an empty
		// object — native cannot reproduce either, so DECLINE (Mcoerce-d).
		return nil, unsupported(fmt.Sprintf("single-field class %q: lone field unmatched (BAML implied-key/default)", name))
	}
	if hasExtra {
		cf.flag() // ExtraKey (cost 1 each) -> not a clean zero-score class
	}

	var buf bytes.Buffer
	buf.WriteByte('{')
	first := true
	for i := range cls.Fields {
		f := &cls.Fields[i]
		if !present[i] {
			if isOptional(f.Type) {
				// Absent optional: omit it. The downstream
				// InjectAbsentOptionals pass inserts the null, identically
				// for the native and BAML paths.
				continue
			}
			// A required field with no key match even after fuzzy matching:
			// BAML may fill a type default (field_type.rs:288) or hard-fail,
			// and native cannot tell which, so it DECLINES (fall back to BAML)
			// rather than claim a missing-required error that would be WRONG
			// when BAML defaults. Default-filling parity is Mcoerce-d.
			return nil, unsupported(fmt.Sprintf("class %q: required field %q has no key match (BAML may default/error)", name, f.Name.RenderedName()))
		}
		child, err := coerce(b, f.Type, assigned[i], cf)
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

func coerceList(b *schema.Bundle, elem *schema.Type, input value, cf *coerceFlags) (json.RawMessage, error) {
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
		// A clean array adds no list-level flag; element flags (e.g. a
		// substring enum) propagate through cf so a nullable list arm can
		// require all elements clean.
		child, err := coerce(b, *elem, input.arrV[i], cf)
		if err != nil {
			if !declinableChildError(err) {
				return nil, err // hard/invariant failure: propagate, never mask.
			}
			// A bad element (decline sentinel or value-verdict mismatch): BAML
			// records ArrayItemParseError and SKIPS it, returning a partial list
			// that still succeeds — native cannot reproduce that partial result,
			// so it DECLINES the whole list rather than claim a list error where
			// BAML would just drop the element. Partial-list parity is Mcoerce-c.
			return nil, unsupported(fmt.Sprintf("list element %d: %v (BAML skips bad items via ArrayItemParseError)", i, err))
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
func coerceMap(b *schema.Bundle, keyT, valT *schema.Type, input value, cf *coerceFlags) (json.RawMessage, error) {
	if keyT == nil || valT == nil {
		return nil, fmt.Errorf("debaml: map type missing key or value")
	}
	if input.kind != valObject {
		// Not a hard type error: BAML's object/map coercion + scoring can
		// still produce a (partial/flagged) map from a non-object, so decline
		// rather than claim a mismatch BAML would not produce.
		return nil, declineCoerce("map target", input)
	}
	// Every object→map carries ObjectToMap (cost 1), and BAML scores a map by
	// its OWN flags only — the value scores do NOT propagate (score.rs:21). So
	// a map is NEVER a zero-score "clean" arm: flag cf, and coerce values with
	// no accumulator since their flags don't affect the map's score.
	cf.flag()

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

		child, err := coerce(b, *valT, f.val, nil)
		if err != nil {
			if !declinableChildError(err) {
				return nil, err // hard/invariant failure: propagate, never mask.
			}
			// A bad value (decline sentinel or value-verdict mismatch): BAML
			// records MapValueParseError and SKIPS this entry, returning a
			// partial map — native cannot reproduce that, so decline the whole
			// map. Partial-map parity is Mcoerce-c.
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
// key type via BAML's actual match_string (Mcoerce-a), returning nil on a
// clean accept and a DECLINE sentinel otherwise. checkSupportedMapKey has
// already restricted the key type to the four legal shapes, so the default
// arm is defensive. BAML coerces the key with the key type's coercer
// (coerce_map.rs:163) and, on success, inserts the entry under the ORIGINAL
// input key string — so the accepted key is never rewritten, and the only
// thing that matters here is whether the key coercion SUCCEEDS:
//
//   - string primitive: accept any key verbatim.
//   - enum: accept on a clean match_string match (substring enabled); a
//     no-match or substring TIE declines (BAML skips the entry via
//     MapKeyParseError, returning a partial map — Mcoerce-c).
//   - string literal: accept on a clean match_string match (substring enabled).
//   - union of string literals: accept when AT LEAST ONE arm matches. The
//     map output is the original key regardless of which arm BAML's union
//     pick_best selects, so multiple matches do not need scoring here.
//
// Any non-clean key DECLINES the whole map, because BAML would skip just that
// entry and return a partial map, which native does not yet reproduce
// (Mcoerce-c). The accepted key is NOT rewritten — coerceMap emits the
// original input key string, matching BAML's insertion of the raw object key.
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
		// The key's own match flags do not propagate to the map score (BAML
		// discards the coerced key, inserting the original string), so ignore
		// the substring bit — only success/failure matters.
		if _, outcome, _ := matchString(key, enumMatchCandidates(e), true); outcome == matchOne {
			return nil
		}
		return unsupported(fmt.Sprintf("map key %q: no clean enum %q match (BAML skips via MapKeyParseError)", key, keyT.Name))
	case schema.TypeLiteral:
		if keyT.Literal == nil || keyT.Literal.Kind != schema.LiteralString {
			return unsupported("map key literal must be a string literal")
		}
		if _, outcome, _ := matchString(key, []matchCandidate{{name: keyT.Literal.String, validValues: []string{keyT.Literal.String}}}, true); outcome == matchOne {
			return nil
		}
		return unsupported(fmt.Sprintf("map key %q: no clean literal %q match (BAML skips via MapKeyParseError)", key, keyT.Literal.String))
	case schema.TypeUnion:
		// A union-of-string-literals key coerces through the union coercer,
		// which succeeds when ANY literal arm matches; the map then inserts the
		// original key, so the winning arm is irrelevant.
		for _, lit := range flattenStringLiterals(keyT) {
			if _, outcome, _ := matchString(key, []matchCandidate{{name: lit, validValues: []string{lit}}}, true); outcome == matchOne {
				return nil
			}
		}
		return unsupported(fmt.Sprintf("map key %q: matches no string-literal-union arm (BAML skips via MapKeyParseError)", key))
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

// coerceUnionSafe coerces a value against a union, claiming native JSON ONLY
// where it can PROVE BAML also resolves to exactly one clean zero-score
// winner — otherwise it DECLINES (ErrDeBAMLParseUnsupported → fall back). Two
// traps shape the claim:
//
//   - LENIENCY (M2c): native's per-variant verdicts must mirror BAML's lenient
//     ones, and exactly one variant must succeed; two-plus is BAML pick_best
//     SCORING (M3) and declines.
//   - THE NULL ARM SCORES (F1): for a NULLABLE union with NON-null input, BAML
//     includes the null arm in scoring — any non-null value coerces to null
//     with DefaultButHadValue cost 110 (coerce_union.rs:129,
//     coerce_primitive.rs:116). So the chosen non-null arm wins only if its
//     score < 110; if it carries ≥110 worth of flags BAML returns NULL. Native
//     does not score, so in a nullable context it claims the non-null arm ONLY
//     when that arm is CLEAN (zero score-bearing flags) — which trivially beats
//     null's 110. A flagged arm (extra keys, substring match, object→map, …)
//     DECLINES (it might be an M3 scored win for null or another arm).
//
// The claim is limited to:
//
//  1. JSON-null input + nullable union → null immediately (BAML's null fast path).
//  2. nullable single-non-null union (the optional shape) → non-null input
//     coerces through the lone variant ONLY when that coercion is clean.
//  3. non-null input + a multi-variant SAFE FAMILY (homogeneous literal union or
//     flat disjoint-key class union) where exactly one variant succeeds — and,
//     when the union is nullable, that winner is clean.
//
// cf carries the winner's cleanliness up to an OUTER nullable context (a flagged
// arm makes an enclosing optional non-clean too).
func coerceUnionSafe(b *schema.Bundle, u *schema.UnionType, input value, cf *coerceFlags) (json.RawMessage, error) {
	if u == nil {
		return nil, fmt.Errorf("debaml: union type missing payload")
	}
	// Case 1: nullable null fast path (any nullable union).
	if input.kind == valNull {
		if !u.Nullable {
			return nil, typeMismatch("non-nullable union", input)
		}
		return json.RawMessage("null"), nil
	}
	// Case 2: optional — a single non-null variant (always the nullable shape,
	// since a non-nullable single variant collapses to the bare type in
	// simplifyUnion). Coerce the lone arm into a LOCAL accumulator: an arm
	// FAILURE means BAML null-defaults (cost 110) — native can't reproduce that
	// — and a FLAGGED arm might also lose to null by score, so claim ONLY a
	// clean success.
	if len(u.Variants) == 1 {
		arm := &coerceFlags{}
		out, err := coerce(b, u.Variants[0], input, arm)
		if err != nil {
			if !declinableChildError(err) {
				return nil, err // hard/invariant failure: propagate, never mask.
			}
			return nil, unsupported(fmt.Sprintf("optional single-arm union: non-null arm did not cleanly succeed (BAML may null-default): %v", err))
		}
		if arm.isFlagged() {
			return nil, unsupported("optional single-arm union: non-null arm is not a clean zero-score match (null arm competes by scoring → M3)")
		}
		return out, nil
	}
	// Case 3: non-null input against a multi-variant union. Re-prove the
	// non-null arm set is a safe family (this also rejects non-null input to a
	// nullable-but-unsafe union, which the gate permitted only for case 1).
	if err := checkSupportedUnionShape(b, u); err != nil {
		return nil, err
	}
	return coerceUnionSafeMulti(b, u.Variants, input, u.Nullable, cf)
}

// coerceUnionSafeMulti dispatches a proven-safe multi-variant union to its
// family's value-level guard. requireClean is the union's Nullable flag: when
// true the lone winner must be a clean zero-score success (the null arm
// competes by scoring). checkSupportedUnionShape has already proven the variant
// set is one of the two homogeneous families, so the else branch is the flat
// class union.
func coerceUnionSafeMulti(b *schema.Bundle, variants []schema.Type, input value, requireClean bool, cf *coerceFlags) (json.RawMessage, error) {
	if allLiteralVariants(variants) {
		return coerceLiteralUnion(variants, input, requireClean, cf)
	}
	return coerceFlatClassUnion(b, variants, input, requireClean, cf)
}

// resolveArmFlags applies the lone winner's local flags. When requireClean (a
// nullable union, where BAML's 110-cost null arm competes), a flagged winner
// DECLINES. Otherwise a flagged winner is still claimed but its flags propagate
// to the caller's accumulator, so an enclosing nullable context sees it.
func resolveArmFlags(requireClean bool, arm, cf *coerceFlags) error {
	if !arm.isFlagged() {
		return nil
	}
	if requireClean {
		return unsupported("nullable union: lone non-null arm is not a clean zero-score match (null arm competes by scoring → M3)")
	}
	cf.flag()
	return nil
}

// coerceLiteralUnion claims a homogeneous literal union when EXACTLY one
// variant leniently coerces (Mcoerce-a's no-over-claim rule). It counts
// per-variant successes through coerceLiteral itself — so string literals
// are evaluated with the actual match_string (a fuzzy/substring hit counts),
// while int/bool literals stay exact (their lenient numeric coercion is
// Mcoerce-b). Two-plus lenient successes mean BAML would run scored pick_best
// — e.g. input "foobar" substring-matching both "foo" and "bar" arms even
// though the literal VALUES were proven match-disjoint at the gate — so
// native DECLINES (M3). Zero successes decline too. When requireClean (nullable
// union), a winner that only substring-matched (SubstringMatch flag) declines —
// the null arm could score lower. The single winner is re-coerced through
// coerceLiteral so the emitted form matches the single-literal path exactly.
func coerceLiteralUnion(variants []schema.Type, input value, requireClean bool, cf *coerceFlags) (json.RawMessage, error) {
	matched := -1
	count := 0
	for i := range variants {
		_, err := coerceLiteral(variants[i].Literal, input, nil)
		switch {
		case err == nil:
			matched = i
			count++
		case !declinableChildError(err):
			return nil, err // hard/invariant failure in an arm: propagate.
		}
		// A declinable error just means this arm did not match; keep counting.
	}
	if count != 1 {
		return nil, unsupported(fmt.Sprintf("literal union: %d lenient matches for %s input (need exactly 1; 2+ is BAML pick_best = M3)", count, input.kind.String()))
	}
	arm := &coerceFlags{}
	out, err := coerceLiteral(variants[matched].Literal, input, arm)
	if err != nil {
		return nil, err
	}
	if err := resolveArmFlags(requireClean, arm, cf); err != nil {
		return nil, err
	}
	return out, nil
}

// coerceFlatClassUnion claims a flat disjoint-key class union when EXACTLY
// one variant class leniently coerces (Mcoerce-a's no-over-claim rule). It
// counts per-variant successes through coerceClass itself — which now matches
// field keys fuzzily and ignores extra keys — so a class succeeds when all
// its required fields are matched and coerce, regardless of extras. The
// variant classes were proven flat / >=2-required-field / disjoint-key by
// checkSupportedUnionShape, so an input carrying one class's full field set
// cannot satisfy another (every other arm is missing >=2 disjoint required
// fields it cannot fill) — unless the input ALSO carries a second arm's full
// field set, which makes BOTH succeed and forces BAML's scored pick_best, so
// native DECLINES (M3). Zero successes decline too. When requireClean (nullable
// union), a winner carrying ExtraKey flags (extra input keys) or any flagged
// child DECLINES — BAML's null arm (110) could outscore it. A child coercion
// error (sentinel OR claimed) just means that variant did not succeed and is
// swallowed by the count; only the lone winner is emitted.
func coerceFlatClassUnion(b *schema.Bundle, variants []schema.Type, input value, requireClean bool, cf *coerceFlags) (json.RawMessage, error) {
	if input.kind != valObject {
		// BAML can infer a single-field class from a scalar / imply a key;
		// these classes are multi-field so a non-object is never a clean arm,
		// but it is not a hard type error either, so decline.
		return nil, declineCoerce("class union", input)
	}
	matched := -1
	count := 0
	for i := range variants {
		_, err := coerceClass(b, variants[i].Name, variants[i].Mode, input, nil)
		switch {
		case err == nil:
			matched = i
			count++
		case !declinableChildError(err):
			return nil, err // hard/invariant failure in an arm: propagate.
		}
		// A declinable error just means this arm did not match; keep counting.
	}
	if count != 1 {
		return nil, unsupported(fmt.Sprintf("class union: %d variant classes coerce cleanly (need exactly 1; 2+ is BAML pick_best = M3)", count))
	}
	arm := &coerceFlags{}
	out, err := coerceClass(b, variants[matched].Name, variants[matched].Mode, input, arm)
	if err != nil {
		return nil, err
	}
	if err := resolveArmFlags(requireClean, arm, cf); err != nil {
		return nil, err
	}
	return out, nil
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

// mismatchError marks a CLAIMED value-level coercion verdict — a hard JSON-type
// mismatch (typeMismatch) or a match_string substring tie (ambiguousMatch) —
// for which BAML produces a comparable error on the SAME value. At the top
// level it propagates as a claimed parse failure (the differential checks BAML
// also errors); but inside a container/union child-wrapper it is DECLINABLE,
// because BAML would skip the entry (partial list/map) or score it (union),
// which native does not reproduce, so the wrapper falls back instead. It is
// deliberately NOT the ErrDeBAMLParseUnsupported sentinel, so it stays a claim
// at the seam (declinableChildError identifies it for the wrappers).
type mismatchError struct{ msg string }

func (e *mismatchError) Error() string { return e.msg }

// declinableChildError reports whether a child coercion error should make a
// container/union wrapper DECLINE (fall back) rather than propagate. Only two
// classes decline: the ErrDeBAMLParseUnsupported fallback sentinel (native
// stricter than BAML, so BAML may still succeed/skip/score), and a value-verdict
// mismatchError (BAML skips the entry or scores the value). EVERY OTHER error is
// a HARD failure — an unknown enum/class ref, a missing type payload, a marshal
// failure, an unexpected kind: a native bug or schema-invariant violation that
// must PROPAGATE so the native-vs-BAML differential surfaces it (per the seam
// contract: anything but ErrDeBAMLParseUnsupported is a claimed error), rather
// than being silently masked as a BAML fallback.
func declinableChildError(err error) bool {
	if errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
		return true
	}
	var me *mismatchError
	return errors.As(err, &me)
}

// typeMismatch reports a conservative JSON-type mismatch as a CLAIMED
// coercion error. Used where BAML would also fail (e.g. a scalar/array
// where a class object is required), so the differential checks error
// parity rather than masking it behind a fallback.
func typeMismatch(want string, input value) error {
	return &mismatchError{msg: fmt.Sprintf("debaml: expected %s, got %s", want, input.kind.String())}
}

// ambiguousMatch reports a match_string substring TIE (StrMatchOneFromMany)
// as a CLAIMED coercion error: BAML's matcher errors before emitting any of
// the tied variants, so native claims the same error rather than arbitrarily
// picking one. Distinct from declineCoerce — the differential checks BAML
// also errors here (error parity), so this must NOT be the fallback sentinel.
// Safe to claim only outside an optional arm; coerceUnionSafe's case 2
// converts it to a DECLINE where BAML would null-default instead.
func ambiguousMatch(target, input string) error {
	return &mismatchError{msg: fmt.Sprintf("debaml: %s: %q ambiguously substring-matches multiple candidates (BAML StrMatchOneFromMany)", target, input)}
}

// declineCoerce reports a coercion mismatch as a DECLINE
// (ErrDeBAMLParseUnsupported → fall back to BAML), for the cases where
// native's strict matching is narrower than BAML's lenient coercers, so a
// native "failure" is exactly where BAML would still succeed. Distinct from
// typeMismatch, which claims an error BAML would also produce.
func declineCoerce(target string, input value) error {
	return unsupported(fmt.Sprintf("%s: got %s (native stricter than BAML)", target, input.kind.String()))
}
