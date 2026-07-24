package debaml

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/internal/debaml/bamlunicode"
	"github.com/invakid404/baml-rest/internal/schema"
)

// candKind classifies a coerced value the way BAML's BamlValueWithFlags variants
// do (types.rs), so pickBest can apply its list / class / scalar-vs-composite
// special ordering.
type candKind uint8

const (
	candScalar candKind = iota // String / Int / Float / Bool — non-composite
	candEnum                   // Enum — non-composite
	candNull                   // Null — non-composite
	candList                   // composite
	candMap                    // composite
	candClass                  // composite
)

// isComposite mirrors BamlValueWithFlags::is_composite (types.rs): list / map /
// class are composite; scalar / enum / null are not.
func (k candKind) isComposite() bool {
	return k == candList || k == candMap || k == candClass
}

// coerceFlags accumulates the BAML SCORE MODEL (M3) for one coerced subtree,
// plus the pick_best discriminators the union / array-to-singular selection
// needs. It replaces the pre-M3 boolean "flagged" signal with a real score:
//
//   - score: the types.rs INHERENT BamlValueWithFlags::score() of this subtree —
//     the sum of this node's OWN score-bearing flag weights PLUS every child
//     score (list = own + item scores; class = own + field scores; map = own +
//     each key conditions + value score). Lower wins in a union / pick_best.
//     NOT the score.rs trait model (which multiplies list/class children by 10
//     and scores a map by own flags only); the union and pick_best call sites
//     use the inherent model. A nil receiver means "don't track" (top-level /
//     standalone), so add / foldChild are nil-safe and free there.
//   - uncertain: the verdict depended on something native cannot prove equals
//     BAML. Its ONE remaining leaf cause is a non-integer number spelling whose
//     jsonish Value Display native cannot reproduce byte-identically
//     (displayStringification/displayNumber). (#555 Slice 2 removed the former
//     non-ASCII case-fold cause: match_string now folds through bamlunicode,
//     byte-identical to Rust str::to_lowercase, so a case-fold verdict is
//     native's to CLAIM, never uncertain.) A HARD decline signal: any uncertain
//     arm declines the WHOLE union, and the collection classifiers decline the
//     whole list/map on an uncertain child. This flag may still acquire OTHER
//     non-Unicode causes in future, so it is NOT removed wholesale — only the
//     Unicode leaf cause was dropped.
//
// The remaining fields describe the TOP-LEVEL node this accumulator coerced (its
// OWN conditions and kind) for pick_best's special ordering. They are set by the
// node's own coercer and are NOT folded up from children (foldChild folds only
// score + uncertain). At a union arm the arm's own accumulator carries them; a
// nested child's descriptors are set but ignored by its parent.
type coerceFlags struct {
	score     int
	uncertain bool

	// targetIsUnion mirrors BAML's `target` being a TypeIR::Union at THIS coercion
	// (coerce_union.rs passes union_target down to each arm's coerce). It is an
	// INPUT signal, not an output discriminator (never snapshotted by toCandidate /
	// absorb): its ONLY effect is that a primitive array-to-singular's inner
	// pick_best adds UnionMatch (score 0) instead of FirstMatch (score 1) to the
	// winning item — so `int` as a union arm on [1,2] scores 1 (UnionMatch+FirstMatch)
	// and beats a `string` arm's JsonToString (2), while the SAME `int` target
	// standalone scores 2 (FirstMatch+FirstMatch). Set true by the direct union-arm
	// coercions (coerceUnionSafe case 2 + coerceUnionSafeMulti phase 2).
	targetIsUnion bool

	kind candKind

	// list discriminators (this list's OWN conditions):
	singleToArray     bool // produced by SingleToArray (non-array wrap)
	itemsEmpty        bool // emitted zero items
	arrayItemErrors   int  // count of ArrayItemParseError flags on the list
	firstItemMarkdown bool // first item carries ObjectFromMarkdown (native: never)

	// scalar discriminators (this value's OWN conditions):
	hasJsonToString bool // a string coerced from a non-string via JsonToString
	hasFirstMatch   bool // an array-to-singular / pick_best FirstMatch winner
	hasUnionMatch   bool // a union winner (carries UnionMatch, score 0)

	// class discriminators:
	classPropCount     int
	classAllDefault    bool // every emitted field came from a default/missing path
	classSingleImplied bool // single field, a string coerced via ImpliedKey
}

// add records w points of this node's OWN score-bearing conditions (a specific
// Flag weight, e.g. FloatToInt=1, JsonToString=2, DefaultButHadValue=110).
// Nil-safe so untracked top-level / standalone callers pay nothing.
func (f *coerceFlags) add(w int) {
	if f != nil {
		f.score += w
	}
}

// foldChild folds a CHILD subtree's total score into this node — the types.rs
// composite scoring (list/class/map = own + child scores) — and propagates the
// child's uncertainty. Nil-safe on the receiver; child is always non-nil at the
// call sites. Descriptor bits are NOT folded (they describe each node's own
// top-level conditions).
func (f *coerceFlags) foldChild(child *coerceFlags) {
	if f != nil {
		f.score += child.score
	}
	if child.isUncertain() {
		f.markUncertain()
	}
}

// absorb folds a selected candidate (a union / array-to-singular winner) into
// this accumulator: its total score PLUS its top-level kind and pick_best
// discriminators, so the winner competes correctly when THIS accumulator is
// itself a field / element / arm of an outer scored context. It does NOT force
// UnionMatch (setWinnerCf adds that for union winners); an array-to-singular
// winner carries only its own FirstMatch. Nil-safe.
func (f *coerceFlags) absorb(w candidate) {
	if f == nil {
		return
	}
	f.score += w.score
	f.kind = w.kind
	f.singleToArray = w.singleToArray
	f.itemsEmpty = w.itemsEmpty
	f.arrayItemErrors = w.arrayItemErrors
	f.firstItemMarkdown = w.firstItemMarkdown
	f.hasJsonToString = w.hasJsonToString
	f.hasFirstMatch = w.hasFirstMatch
	f.hasUnionMatch = w.hasUnionMatch
	f.classPropCount = w.classPropCount
	f.classAllDefault = w.classAllDefault
	f.classSingleImplied = w.classSingleImplied
}

// markUncertain records a verdict native cannot prove matches BAML — today only
// a non-integer number display (displayStringification/displayNumber); the
// non-ASCII case-fold cause was removed in #555 Slice 2. Callers DECLINE (never
// claim, never proven-skip).
func (f *coerceFlags) markUncertain() {
	if f != nil {
		f.uncertain = true
	}
}

// isUncertain reports whether a number-display uncertainty was recorded (the
// sole remaining cause after #555 Slice 2 dropped the Unicode case-fold cause).
func (f *coerceFlags) isUncertain() bool { return f != nil && f.uncertain }

// isFlagged reports whether any score-bearing condition was recorded — i.e. the
// value is not a clean zero-score success. Every native score-bearing flag has
// weight >= 1, so this is exactly score > 0. Retained as a readable predicate for
// the leaf/structural unit tests that assert a conversion is score-bearing.
func (f *coerceFlags) isFlagged() bool { return f != nil && f.score > 0 }

// setKind records this node's BAML value kind (nil-safe).
func (f *coerceFlags) setKind(k candKind) {
	if f != nil {
		f.kind = k
	}
}

// toCandidate snapshots this arm accumulator into a pickBest candidate carrying
// the emitted output, the inherent score, and the special-ordering discriminators.
// originIndex is the arm's position in BAML's iter_include_null() order (so the
// index tiebreak reproduces BAML's res indices even with excluded error arms).
// Nil-safe (like add / foldChild / setKind): a nil receiver yields a zero-scored
// candidate. Today every selectUnionArms caller passes a fresh non-nil
// accumulator, so this is API-consistency hardening only.
func (f *coerceFlags) toCandidate(out json.RawMessage, originIndex int) candidate {
	c := candidate{output: out, originIndex: originIndex}
	if f == nil {
		return c
	}
	c.kind = f.kind
	c.score = f.score
	c.singleToArray = f.singleToArray
	c.itemsEmpty = f.itemsEmpty
	c.arrayItemErrors = f.arrayItemErrors
	c.firstItemMarkdown = f.firstItemMarkdown
	c.hasJsonToString = f.hasJsonToString
	c.hasFirstMatch = f.hasFirstMatch
	c.hasUnionMatch = f.hasUnionMatch
	c.classPropCount = f.classPropCount
	c.classAllDefault = f.classAllDefault
	c.classSingleImplied = f.classSingleImplied
	return c
}

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
// Coercion cut-line (Mcoerce-b): on top of Mcoerce-a's match_string parity
// (enum values, string literals, class field keys, map keys via trim /
// accent+ligature fold / punctuation strip / case-insensitive / substring —
// see match_string.go), native now ports BAML's lenient PRIMITIVE and LITERAL
// numeric/bool/null coercers: numeric-string parsing (trim + trailing-comma
// trim, i64 / u64-wrap / f64 / fraction / extracted-number regex), float→int
// rounding (half-away-from-zero, saturating cast), string→bool (casefold +
// match_string), and non-null→null defaulting — plus int/bool LITERALS by
// primitive-coerce-then-compare. Each score-bearing conversion (FloatToInt,
// StringToFloat, StringToBool, DefaultButHadValue) flags the coerceFlags
// accumulator so the nullable-union clean-only rule holds. Mcoerce-c adds native
// LIST parity (coerceList / coerceArray.rs): non-array SINGLE-TO-ARRAY wrapping,
// PARTIAL array skips of PROVEN-parse-error items, and empty-list-on-singleton-
// failure — each score-bearing (SingleToArray, ArrayItemParseError, child flags)
// so the nullable-union clean-only rule still holds. Mcoerce-c also adds native
// MAP parity (coerceMap / coerce_map.rs): object→map ObjectToMap flagging,
// VALUE-then-KEY coercion, VALUE partial entry skips of PROVEN value parse
// errors (MapValueParseError), and ORIGINAL input key strings in input order.
// KEYS do not partial-skip in native: any non-clean enum/literal/literal-union
// key verdict declines the WHOLE map (no MapKeyParseError skip), because the
// dynamic bridge keeps non-matching keys leniently. ObjectToMap and value
// partial skips are score-bearing, so the nullable-union clean-only rule still
// holds. Mcoerce-d PR 1 adds native STRINGIFICATION + literal EXTRACTION: a
// primitive STRING target stringifies a NON-null non-string via jsonish Value
// Display (displayValue) and marks JsonToString (score 2); ENUM and string
// LITERAL targets stringify a non-string the same way and mark ObjectToString
// (score 2) before running match_string; and every LITERAL kind first tries a
// single-key-object ObjectToPrimitive extraction (score 2) of a number / bool /
// string inner value. These are score-bearing (JsonToString / ObjectToString /
// ObjectToPrimitive all flag the accumulator) so the nullable-union clean-only
// rule holds, and the leaf-level collection classifiers keep the newly-succeeding
// string/enum/literal children instead of declining the whole collection (a
// direct string←null child is now a proven ArrayItemParseError/MapValueParseError
// skip). Mcoerce-d PR 2 adds native STRUCTURAL CLASS coercion (coerceClass /
// coerce_class.rs): input-key→field assignment (first-match, keep-first
// duplicates, ExtraKey extras), single-field OBJECT implied-key and SCALAR/null
// inferred-object absorption (ImpliedKey / InferedObject), missing-optional null
// fill (OptionalDefaultFromNoValue), and required-field DEFAULTS via
// TypeIR::default_value (defaultValue — list→[], map→{}, null→null, first
// defaultable union arm; DefaultFromNoValue) plus the present-map-non-object
// default {} (DefaultButHadUnparseableValue). Each new flag is score-bearing so
// the nullable-union clean-only rule holds, and a class child that now succeeds
// through these paths is KEPT in a list/map instead of declining the whole
// collection. ARRAY input to a class still DECLINES (coerce_array_to_singular /
// pick_best = M3), as do broadened union families (Mcoerce-d PR 3). Where
// native's coercion fails but BAML may leniently SUCCEED (or null-default) — or
// where native cannot determine BAML's exact success/failure — coercion DECLINES
// (ErrDeBAMLParseUnsupported → fall back to BAML), so native is never "more
// capable" than BAML.
//
// Native CLAIMS a coercion error only where BAML also errors: a match_string
// substring TIE (StrMatchOneFromMany — coerceEnum / coerceLiteral
// ambiguousMatch). A class missing a required NON-defaultable field DECLINES
// (BAML errors, but native falls back to stay strictly non-over-claiming), as
// does ARRAY input to a class (M3 array-to-singular). Union claims count LENIENT
// per-variant successes and claim only the lone winner; two-plus successes are
// BAML pick_best (M3) and DECLINE. A consequence is that native still declines
// some inputs BAML would itself reject or coerce differently (a required field
// with no key match, a present field native's coercer is stricter on, an
// implied-key/inferred coercion native declines, an int-LITERAL value mismatch
// after rounding) — behavior is identical via fallback, and precise claim-parity
// for those is deferred to the rest of the Mcoerce milestone (#546). See
// coercePrimitive / coerceList / coerceEnum / coerceLiteral / coerceClass for the
// per-kind boundary.
// cctx is the path-local pair-guard context (see pair_guard.go), threaded through
// EVERY final recursive descent so an admitted recursive-class family coerces under
// BAML's (ClassKey, value) circular-reference guard with no depth cap. It is nil for
// leaf callers and the stream path (which never carries a recursive class).
func coerce(b *schema.Bundle, t schema.Type, input value, f *coerceFlags, cctx *coerceCtx) (json.RawMessage, error) {
	switch t.Kind {
	case schema.TypePrimitive:
		// Primitives are lenient in Mcoerce-b (numeric-string parse, float→int
		// rounding, string→bool, non-null→null default), each of which adds a
		// score-bearing flag, so f is threaded through for the nullable-union
		// clean-only rule.
		return coercePrimitive(t.Primitive, input, f)
	case schema.TypeLiteral:
		return coerceLiteral(t.Literal, input, f)
	case schema.TypeEnum:
		return coerceEnum(b, t.Name, input, f)
	case schema.TypeClass:
		return coerceClass(b, t.Name, t.Mode, input, f, cctx)
	case schema.TypeList:
		return coerceList(b, t.Elem, input, f, cctx)
	case schema.TypeMap:
		return coerceMap(b, t.Key, t.Value, input, f, cctx)
	case schema.TypeUnion:
		return coerceUnionSafe(b, t.Union, input, f, cctx)
	case schema.TypeRecursiveAlias:
		// De-BAML Phase 3a: the admitted structural-recursive alias (JSON). The
		// alias-specific EXACT scored path (alias_coerce.go) reproduces BAML's
		// coerce_alias -> coerce_union (null -> [], IndexMap map order, broad-union
		// scoring/hint); the FINAL bytes are SORTED-public (marshalPublic). Reached
		// ONLY for the admitted family (the fingerprint forbids an alias inside any
		// other bundle), so a top-level nil f is the common case; a non-nil enclosing
		// accumulator folds the alias result's inherent score.
		av, af, _, err := aliasCoerceValue(b, input, cctx)
		if err != nil {
			return nil, err
		}
		if af != nil {
			f.foldChild(af)
			f.setKind(af.kind)
		}
		return av.marshalPublic()
	default:
		return nil, fmt.Errorf("debaml: cannot coerce type kind %q", t.Kind)
	}
}

// coercePrimitive coerces a value to a primitive target. Mcoerce-b ports
// BAML's lenient numeric/bool/null coercers (coerce_primitive.rs) so native
// claims the same conversions BAML would:
//
//   - int:   JSON number (i64 / u64-wrap / float→int round) and string
//     (trim+comma-trim then i64 / u64-wrap / float→int / fraction / extracted
//     number). Float/fraction/extracted paths add FloatToInt (score 1).
//   - float: JSON number and string (f64 / i64 / u64 / fraction / extracted).
//     The extracted-number path adds StringToFloat (score 1).
//   - bool:  JSON bool, casefold "true"/"false", or a match_string true/false
//     substring hit — the string paths add StringToBool (score 1).
//   - null:  JSON null is clean; any non-null value defaults to null with
//     DefaultButHadValue (score 110).
//   - string: Mcoerce-d ports coerce_string's non-string stringification: a
//     JSON string is emitted unchanged, a NON-null non-string is rendered via
//     jsonish Value Display (displayValue) and marked JsonToString (score 2),
//     and a JSON null still DECLINES (BAML error_unexpected_null — null is
//     never stringified; an optional string null-defaults, which native cannot
//     score).
//
// Every score-bearing conversion marks f (nil-safe) so the nullable-union
// clean-only rule can require the winning non-null arm to be clean.
func coercePrimitive(p schema.PrimitiveKind, input value, f *coerceFlags) (json.RawMessage, error) {
	switch p {
	case schema.PrimitiveString:
		return coercePrimitiveString(input, f)
	case schema.PrimitiveInt:
		return coercePrimitiveInt(input, f)
	case schema.PrimitiveFloat:
		return coercePrimitiveFloat(input, f)
	case schema.PrimitiveBool:
		return coercePrimitiveBool(input, f)
	case schema.PrimitiveNull:
		return coercePrimitiveNull(input, f)
	default:
		return nil, fmt.Errorf("debaml: unsupported primitive %q", p)
	}
}

// coercePrimitiveInt emits the coerced integer as a JSON number, marking f
// when the value came from a float/fraction/extracted path (FloatToInt). JSON
// ARRAY input routes through coerce_array_to_singular (M3d): each item is
// coerced back through coercePrimitiveInt, pickBest selects the winner, and
// FirstMatch scoring is applied (coerceScalarArrayToSingular).
func coercePrimitiveInt(input value, f *coerceFlags) (json.RawMessage, error) {
	if input.kind == valArray {
		return coerceScalarArrayToSingular(input.arrV, f, coercePrimitiveInt, provenNumericArrayItemError)
	}
	n, flagged, err := coerceIntValue(input)
	if err != nil {
		return nil, err
	}
	f.setKind(candScalar)
	if flagged {
		f.add(1) // FloatToInt (score 1)
	}
	return json.RawMessage(strconv.FormatInt(n, 10)), nil
}

// coercePrimitiveFloat emits the coerced float as a JSON number, marking f
// when the value came from the extracted-number path (StringToFloat). JSON
// ARRAY input routes through coerce_array_to_singular (M3d).
func coercePrimitiveFloat(input value, f *coerceFlags) (json.RawMessage, error) {
	if input.kind == valArray {
		return coerceScalarArrayToSingular(input.arrV, f, coercePrimitiveFloat, provenNumericArrayItemError)
	}
	out, flagged, err := coerceFloatValue(input)
	if err != nil {
		return nil, err
	}
	f.setKind(candScalar)
	if flagged {
		f.add(1) // StringToFloat (score 1)
	}
	return out, nil
}

// coercePrimitiveBool emits the coerced bool, marking f when it came from a
// string (StringToBool). The string→bool match_string fold is now proven
// (bamlunicode), so a miss/tie is a CLAIMED decline, never a Unicode-uncertain
// one. JSON ARRAY input routes through coerce_array_to_singular (M3d).
func coercePrimitiveBool(input value, f *coerceFlags) (json.RawMessage, error) {
	if input.kind == valArray {
		return coerceScalarArrayToSingular(input.arrV, f, coercePrimitiveBool, provenBoolArrayItemError)
	}
	b, flagged, err := coerceBoolValue(input)
	if err != nil {
		return nil, err
	}
	f.setKind(candScalar)
	if flagged {
		f.add(1) // StringToBool (score 1)
	}
	return boolRaw(b), nil
}

// coerceScalarArrayToSingular is the shared array-input path for the primitive
// int/float/bool coercers (coerce_primitive.rs: a JSON Array routes through
// coerce_array_to_singular over the SAME per-kind coercer). It maps each item
// through coerceItem into a fresh accumulator, runs coerceArrayToSingular
// (pickBest + FirstMatch), and folds the winner into f (score, kind scalar,
// hasFirstMatch) so a nullable / outer-union scoring decision sees the exact
// array-to-singular score. The winner's own bytes are emitted directly.
func coerceScalarArrayToSingular(items []value, f *coerceFlags, coerceItem func(value, *coerceFlags) (json.RawMessage, error), itemProvenErr func(item value) bool) (json.RawMessage, error) {
	// BAML threads the current `target` into coerce_array_to_singular; when this
	// primitive is a UNION ARM the target is the union, so the inner pick_best adds
	// UnionMatch (0) not FirstMatch (1). Propagate that into the recursive item
	// coercions too (a nested-array item's array-to-singular has the same target).
	tu := f != nil && f.targetIsUnion
	w, err := coerceArrayToSingular(items, tu, func(item value) (json.RawMessage, *coerceFlags, error) {
		cf := &coerceFlags{targetIsUnion: tu}
		out, e := coerceItem(item, cf)
		return out, cf, e
	}, itemProvenErr)
	if err != nil {
		return nil, err
	}
	f.absorb(w)
	return w.output, nil
}

// provenNumericArrayItemError reports whether coerce_int / coerce_float PROVABLY
// errors on item — the item-error whitelist for an int/float array-to-singular
// (identical to provenListItemError's numeric arm). A string is proven only when
// NONE of BAML's own numeric strategies parse it (bamlStringNumberFails, which is
// Rust-faithful — native's coercers are stricter); an object/bool/null is
// error_unexpected_type; a number always coerces and a nested array recurses.
func provenNumericArrayItemError(item value) bool {
	switch item.kind {
	case valString:
		return bamlStringNumberFails(item.strV)
	case valObject, valBool, valNull:
		return true
	default:
		return false // valNumber coerces; valArray recurses (nested array-to-singular).
	}
}

// provenBoolArrayItemError reports whether coerce_bool PROVABLY errors on item —
// the item-error whitelist for a bool array-to-singular. It is reached only after
// native's bool coercer FAILED; the string→bool match_string fold is now proven
// (bamlunicode), so a STRING miss/tie is a CERTAIN match_string failure equal to
// BAML's error_unexpected_type, and an object/number/null is error_unexpected_type;
// a bool coerces and a nested array recurses.
func provenBoolArrayItemError(item value) bool {
	switch item.kind {
	case valString, valObject, valNumber, valNull:
		return true
	default:
		return false // valBool coerces; valArray recurses.
	}
}

// coercePrimitiveNull emits JSON null. A non-null input defaults to null with
// DefaultButHadValue (score 110), marking f. In a nullable-union scoring
// decision the implicit null arm is handled by coerceUnionSafe, not here — this
// is the standalone primitive-null target BAML always resolves to null.
func coercePrimitiveNull(input value, f *coerceFlags) (json.RawMessage, error) {
	f.setKind(candNull)
	if input.kind != valNull {
		f.add(110) // DefaultButHadValue (score 110)
	}
	return json.RawMessage("null"), nil
}

// coercePrimitiveString ports coerce_string (coerce_primitive.rs:132). A JSON
// string is emitted unchanged (score 0). A NON-null, NON-string value is
// stringified via jsonish Value Display (displayValue) and carries JsonToString
// (score 2) — but ONLY when native can prove the display byte-identical to BAML;
// a display carrying a non-integer number spelling native cannot reproduce (see
// displayNumber) DECLINES and marks f uncertain. A JSON null is NOT stringified:
// BAML errors (error_unexpected_null) and native DECLINES — a required string is
// a hard error and an optional string null-defaults, neither of which native
// reproduces without scoring. (BAML's AnyOf parser-meta path is not modeled by
// native's value kinds, so it never arises here.)
func coercePrimitiveString(input value, f *coerceFlags) (json.RawMessage, error) {
	f.setKind(candScalar)
	switch input.kind {
	case valString:
		return marshalJSON(input.strV)
	case valNull:
		// BAML error_unexpected_null — null is never stringified.
		return nil, declineCoerce("string target (null)", input)
	default:
		s, err := displayStringification(input, f, "string target")
		if err != nil {
			return nil, err
		}
		// Score-bearing JsonToString (added by displayStringification); record the
		// discriminator pick_best's scalar-vs-composite rule keys on (a string cast
		// from JSON is devalued against a composite arm).
		if f != nil {
			f.hasJsonToString = true
		}
		return marshalJSON(s)
	}
}

// displayStringification is the shared non-null/non-string stringification guard
// behind both JsonToString (coercePrimitiveString) and ObjectToString
// (stringForMatch). It renders the jsonish Value Display of input (displayValue)
// and enforces number-display certainty: if the display carries a number spelling
// native cannot prove byte-identical to BAML's serde_json Display (displayNumber),
// it marks f uncertain and DECLINES; otherwise it marks the score-bearing flag
// (JsonToString / ObjectToString both reduce to coerceFlags.flagged) and returns
// the raw Display string. It does NOT handle string passthrough, null, JSON
// marshaling, or match_string — those stay in each caller. uncertainContext names
// the target for the decline message.
func displayStringification(input value, f *coerceFlags, uncertainContext string) (string, error) {
	s, certain := displayValue(input)
	if !certain {
		f.markUncertain()
		return "", unsupported(uncertainContext + ": number spelling display not provably identical to BAML's serde_json Display (Mcoerce-d number-display parity)")
	}
	f.add(2) // JsonToString / ObjectToString (score 2)
	return s, nil
}

// displayValue renders v the way BAML's jsonish::Value Display impl does
// (jsonish/value.rs:195) — the string BAML feeds to JsonToString (primitive
// string, coercePrimitiveString) and ObjectToString (enum / string-literal
// match_string, stringForMatch). This is NOT JSON serialization: keys and
// nested strings are UNQUOTED. The forms, per value.rs:
//
//   - number: BAML's serde_json::Number Display (see displayNumber) — native
//     renders it byte-identically ONLY for a provably-canonical integer token
//     and marks a NON-integer spelling UNCERTAIN;
//   - bool:   "true" / "false";
//   - null:   "null";
//   - string: the raw string, unquoted (a nested string is not re-quoted);
//   - object: "{k: v, k2: v2}" — unquoted keys, ": " key/value separator,
//     ", " between entries, in input order, values recursively displayed;
//   - array:  "[v, v2]" — ", " between elements, recursively displayed.
//
// It returns the rendered string plus a certain flag: certain is false when the
// display contains ANY number spelling native cannot prove equals BAML's
// serde_json f64 Display (see displayNumber), propagated up through nested
// object/array trees. Callers MUST DECLINE (never claim, never proven-skip) when
// certain is false. The caller JSON-marshals a certain result (marshalJSON,
// HTML-escaping disabled) so the display string becomes the emitted JSON string.
func displayValue(v value) (s string, certain bool) {
	switch v.kind {
	case valNull:
		return "null", true
	case valBool:
		if v.boolV {
			return "true", true
		}
		return "false", true
	case valNumber:
		return displayNumber(v.numV)
	case valString:
		return v.strV, true
	case valArray:
		var b strings.Builder
		b.WriteByte('[')
		certain = true
		for i := range v.arrV {
			if i > 0 {
				b.WriteString(", ")
			}
			es, ec := displayValue(v.arrV[i])
			certain = certain && ec
			b.WriteString(es)
		}
		b.WriteByte(']')
		return b.String(), certain
	case valObject:
		var b strings.Builder
		b.WriteByte('{')
		certain = true
		for i := range v.objV {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(v.objV[i].key)
			b.WriteString(": ")
			vs, vc := displayValue(v.objV[i].val)
			certain = certain && vc
			b.WriteString(vs)
		}
		b.WriteByte('}')
		return b.String(), certain
	default:
		return "", false
	}
}

// displayNumber renders a json.Number the way BAML's serde_json::Number Display
// does, but returns certain=true ONLY when native can PROVE byte-identity.
//
// BAML's jsonish engine does NOT enable serde_json's arbitrary_precision, so a
// jsonish::Value::Number stores a serde_json::Number as i64 / u64 / f64 and its
// Display prints the CANONICAL form, NOT the input token:
//
//   - an INTEGER magnitude in i64/u64 range prints its canonical decimal digits;
//     serde normalizes a leading '+' and leading zeros ("+5"/"007" -> "5"/"7");
//   - NEGATIVE ZERO in integer notation ("-0"/"-00") is stored by serde as f64
//     -0.0 and displays "-0.0", NOT the integer "0";
//   - every OTHER form — a fraction, an exponent (5e0), a redundant spelling
//     (5.00), or an integer too large for u64 (serde f64 fallback) — is rendered
//     via serde's f64 Display (ryu shortest round-trip, displayFloat64).
//
// native reproduces ALL of these byte-for-byte — the ryu float formatting is
// LIVE-CAPTURED 0-mismatch vs BAML over 2060 finals (a structured boundary sweep
// plus 2000 randomized f64 incl. subnormals; the corpus 5e0 -> "5.0" is one). The
// certain return is now true for every f64-parseable number token; it is false
// only for a token strconv cannot parse as f64 at all, which the jsonish number
// scanner never emits.
func displayNumber(n json.Number) (string, bool) {
	s := string(n)
	if !strings.ContainsAny(s, ".eE") {
		// Integer notation. serde stores negative zero as f64 -0.0 ("-0" -> "-0.0");
		// any other i64/u64-range integer prints its canonical decimal (sign and
		// leading zeros normalized). A magnitude past u64 falls to the f64 path.
		if isNegZeroToken(s) {
			return "-0.0", true
		}
		if v, err := strconv.ParseInt(s, 10, 64); err == nil {
			return strconv.FormatInt(v, 10), true
		}
		if v, err := strconv.ParseUint(s, 10, 64); err == nil {
			return strconv.FormatUint(v, 10), true
		}
	}
	// Fraction / exponent / redundant spelling / past-u64 integer: serde_json's
	// f64 Display (ryu). displayFloat64 reproduces it byte-for-byte.
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return s, false // not f64-parseable (never emitted by the number scanner).
	}
	return displayFloat64(f), true
}

// isNegZeroToken reports whether s is an integer-notation negative zero ("-0",
// "-00", …), which serde_json stores as f64 -0.0 and Displays as "-0.0".
func isNegZeroToken(s string) bool {
	if len(s) < 2 || s[0] != '-' {
		return false
	}
	for i := 1; i < len(s); i++ {
		if s[i] != '0' {
			return false
		}
	}
	return true
}

// displayFloat64 reproduces serde_json's f64 Display (ryu shortest round-trip):
// the shortest decimal digits that round-trip to f (Go's strconv 'e' shortest
// yields the SAME digits as ryu) reformatted per ryu's fixed/scientific rule —
// scientific when the leading-digit power of ten is < -5 or >= 16, else fixed with
// a trailing ".0" when integer-valued. The bounds and syntax (no '+', no zero-pad,
// no exponent in fixed form) are LIVE-CAPTURED 0-mismatch vs BAML over 2060 tokens
// (structured boundary sweep + 2000 randomized f64 incl. subnormals/huge). f is
// finite: displayNumber only reaches this for a strconv-parseable token, and a
// non-finite string never parses through the jsonish number scanner.
func displayFloat64(f float64) string {
	e := strconv.FormatFloat(f, 'e', -1, 64) // "d(.ddd)?e±XX"
	neg := strings.HasPrefix(e, "-")
	e = strings.TrimPrefix(e, "-")
	mantExp := strings.SplitN(e, "e", 2)
	digits := strings.TrimRight(strings.Replace(mantExp[0], ".", "", 1), "0")
	if digits == "" {
		digits = "0"
	}
	exp10, _ := strconv.Atoi(mantExp[1]) // power of ten of the LEADING digit
	var out string
	switch {
	case exp10 < -5 || exp10 >= 16:
		out = digits[:1]
		if len(digits) > 1 {
			out += "." + digits[1:]
		}
		out += "e" + strconv.Itoa(exp10)
	case exp10 >= 0:
		if exp10+1 >= len(digits) {
			out = digits + strings.Repeat("0", exp10+1-len(digits)) + ".0"
		} else {
			out = digits[:exp10+1] + "." + digits[exp10+1:]
		}
	default:
		out = "0." + strings.Repeat("0", -exp10-1) + digits
	}
	if neg {
		out = "-" + out
	}
	return out
}

// stringForMatch reproduces match_string's value→string prelude
// (match_string.rs:47): a JSON string is used verbatim (score 0); a NON-null
// non-string is stringified via jsonish Value Display and marks ObjectToString
// (score 2); a JSON null is BAML error_unexpected_null and DECLINES (null is
// never matched — a required target errors, an optional one null-defaults, and
// native cannot tell which without scoring). A non-string whose Display carries
// a number spelling native cannot prove equals BAML's (displayValue certain=false)
// also DECLINES and marks f uncertain, so match_string is never run against an
// unprovable stringification. It is the shared prelude for enum and string-literal
// coercion (coerceEnum / coerceLiteral). BAML's AnyOf parser-meta path is not
// modeled by native's value kinds.
func stringForMatch(input value, f *coerceFlags) (string, error) {
	switch input.kind {
	case valString:
		return input.strV, nil
	case valNull:
		return "", declineCoerce("match_string target (null)", input)
	default:
		return displayStringification(input, f, "match_string target")
	}
}

// coerceIntValue ports coerce_int (coerce_primitive.rs). It returns the coerced
// i64, whether a FloatToInt-class conversion happened (score-bearing), and an
// error. A JSON number tries i64, then u64 (Rust wrap cast), then f64 (round
// half-away-from-zero, saturating cast). A string is trimmed then comma-trimmed
// and tried as i64, u64, f64, fraction, and finally the extracted-number regex.
func coerceIntValue(input value) (n int64, flagged bool, err error) {
	switch input.kind {
	case valNumber:
		return intFromNumeric(input.numV.String(), input)
	case valString:
		t := trimNumericString(input.strV)
		if v, fl, e := intFromNumeric(t, input); e == nil {
			return v, fl, nil
		}
		if v, ok := floatFromMaybeFraction(t); ok {
			n, fin := i64FromF64RoundOk(v)
			if !fin {
				return 0, false, declineCoerce("int target (non-finite fraction)", input)
			}
			return n, true, nil // FloatToInt
		}
		if v, ok := floatFromCommaSeparated(t); ok {
			n, fin := i64FromF64RoundOk(v)
			if !fin {
				return 0, false, declineCoerce("int target (non-finite extracted)", input)
			}
			return n, true, nil // FloatToInt
		}
		return 0, false, declineCoerce("int target", input)
	default:
		// Array→singular (coerce_array_to_singular) is Mcoerce-c; every other
		// kind is a BAML error_unexpected_type but native declines conservatively.
		return 0, false, declineCoerce("int target", input)
	}
}

// intFromNumeric parses a numeric token (a JSON number's text, or a
// trimmed/comma-trimmed string) as int the way BAML's number arm does: i64,
// then u64 wrapped to i64, then f64 rounded (FloatToInt). It returns a non-nil
// error only when none of the three parse — the string arm then falls through
// to the fraction / extracted-number strategies.
func intFromNumeric(s string, input value) (int64, bool, error) {
	if v, ok := parseI64Rust(s); ok {
		return v, false, nil
	}
	if v, ok := parseU64Rust(s); ok {
		return int64(v), false, nil // Rust u64 as i64 (two's-complement wrap)
	}
	if v, ok := parseF64Rust(s); ok {
		// BAML's coerce_int applies Rust's `f64 as i64` cast, which is TOTAL and
		// saturating: a finite out-of-range magnitude clamps to i64::MIN/MAX and a
		// NON-finite value clamps too (+Inf -> i64::MAX, -Inf -> i64::MIN, NaN -> 0).
		// i64FromF64Round already implements exactly this cast, so native reproduces
		// BAML byte-exact (LIVE-CAPTURED: "inf"/"+inf"/"Infinity"/"INF"/"1e30" -> MAX,
		// "-inf"/"-Infinity" -> MIN, "nan"/"NaN" -> 0; corpus 90_int_inf / 91_int_nan).
		return i64FromF64Round(v), true, nil // FloatToInt (saturating cast)
	}
	return 0, false, declineCoerce("int target", input)
}

// coerceFloatValue ports coerce_float (coerce_primitive.rs), returning the
// emitted JSON number, whether the extracted-number path (StringToFloat) was
// used, and an error. A JSON number emits its exact token (as_f64 always
// succeeds for a valid number). A string is trimmed then comma-trimmed and
// tried as f64, i64, u64, fraction (all clean), then extracted (StringToFloat).
func coerceFloatValue(input value) (json.RawMessage, bool, error) {
	switch input.kind {
	case valNumber:
		if _, ok := parseF64Rust(input.numV.String()); ok {
			return json.RawMessage(input.numV.String()), false, nil
		}
		return nil, false, declineCoerce("float target", input)
	case valString:
		t := trimNumericString(input.strV)
		if v, ok := parseF64Rust(t); ok {
			return emitFloat(v, false, input) // non-finite ("inf"/"nan") declines
		}
		if v, ok := parseI64Rust(t); ok {
			return emitFloat(float64(v), false, input)
		}
		if v, ok := parseU64Rust(t); ok {
			return emitFloat(float64(v), false, input)
		}
		if v, ok := floatFromMaybeFraction(t); ok {
			return emitFloat(v, false, input)
		}
		if v, ok := floatFromCommaSeparated(t); ok {
			return emitFloat(v, true, input) // StringToFloat
		}
		return nil, false, declineCoerce("float target", input)
	default:
		return nil, false, declineCoerce("float target", input)
	}
}

// boolMatchCandidates is the true/false candidate set coerce_bool passes to
// match_string (coerce_primitive.rs:366) when the casefold check misses.
var boolMatchCandidates = []matchCandidate{
	{name: "true", validValues: []string{"true", "True", "TRUE"}},
	{name: "false", validValues: []string{"false", "False", "FALSE"}},
}

// coerceBoolValue ports coerce_bool (coerce_primitive.rs). It returns the bool,
// whether a StringToBool conversion happened, and an error. A JSON bool is clean;
// a string is first tested casefold-exact against "true"/"false", then via
// match_string (substring enabled) — whose own flags (SubstringMatch) are
// DROPPED: a substring hit still adds only StringToBool (score 1). The
// match_string fold is now proven (bamlunicode == Rust str::to_lowercase), so a
// non-ASCII string bool verdict is native's to CLAIM (no uncertainty return).
func coerceBoolValue(input value) (b bool, flagged bool, err error) {
	switch input.kind {
	case valBool:
		return input.boolV, false, nil
	case valString:
		// ASCII-only fast path: "true"/"false" spell out of ASCII letters, and no
		// non-ASCII scalar lowercases (simple OR full) to a bare ASCII t/r/u/e, so
		// strings.ToLower here can never turn a non-ASCII input into "true"/"false"
		// — a non-ASCII bool string always falls through to the bamlunicode-backed
		// match_string below, which carries the full-Unicode parity. (Leaving this
		// ASCII lower is deliberate; swapping it changes nothing observable.)
		switch strings.ToLower(input.strV) {
		case "true":
			return true, true, nil // StringToBool
		case "false":
			return false, true, nil // StringToBool
		}
		matched, outcome, _ := matchString(input.strV, boolMatchCandidates, true)
		if outcome == matchOne {
			switch matched {
			case "true":
				return true, true, nil // StringToBool (NOT SubstringMatch)
			case "false":
				return false, true, nil
			}
		}
		// match_string no-match or substring TIE (StrMatchOneFromMany): BAML's
		// coerce_bool errors; for a required bool it errors, for an optional one
		// it null-defaults, and native cannot tell which without scoring — decline.
		return false, false, declineCoerce("bool target", input)
	default:
		return false, false, declineCoerce("bool target", input)
	}
}

// boolRaw emits a bool as its JSON literal.
func boolRaw(b bool) json.RawMessage {
	if b {
		return json.RawMessage("true")
	}
	return json.RawMessage("false")
}

// floatRaw emits f as a JSON number using the shortest round-trip form, or
// reports ok=false for a NON-FINITE value (NaN, +Inf, -Inf) — which has no
// valid JSON number spelling, so the caller must DECLINE rather than emit an
// invalid token like "NaN"/"+Inf". The differential compares numeric VALUE
// (both sides decode to float64), so the byte spelling (50 vs 50.0 vs
// scientific) never affects parity — only the decoded value must equal BAML's.
func floatRaw(f float64) (json.RawMessage, bool) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return nil, false
	}
	return json.RawMessage(strconv.FormatFloat(f, 'g', -1, 64)), true
}

// emitFloat wraps floatRaw for coerceFloatValue's string paths: a finite value
// emits (carrying the given StringToFloat flag), while a NON-FINITE value
// DECLINES — BAML's Float(NaN/±Inf) has no valid JSON number form, so native
// falls back rather than claim invalid output.
func emitFloat(v float64, flagged bool, input value) (json.RawMessage, bool, error) {
	out, ok := floatRaw(v)
	if !ok {
		return nil, false, declineCoerce("float target (non-finite)", input)
	}
	return out, flagged, nil
}

// trimNumericString mirrors BAML's shared numeric-string preprocessing:
// s.trim() then s.trim_end_matches(',') — whitespace-trim, THEN strip every
// trailing comma (no second whitespace trim).
func trimNumericString(s string) string {
	return strings.TrimRight(strings.TrimSpace(s), ",")
}

// parseI64Rust mirrors s.parse::<i64>(): base-10, optional leading sign, no
// underscores (Go ParseInt base 10 rejects them, matching Rust).
func parseI64Rust(s string) (int64, bool) {
	n, err := strconv.ParseInt(s, 10, 64)
	return n, err == nil
}

// parseU64Rust mirrors s.parse::<u64>(): base-10 digits with an optional
// leading '+' (Rust unsigned FromStr accepts '+', Go ParseUint does not, so a
// single leading '+' is stripped first).
func parseU64Rust(s string) (uint64, bool) {
	if strings.HasPrefix(s, "+") {
		s = s[1:]
	}
	n, err := strconv.ParseUint(s, 10, 64)
	return n, err == nil
}

// parseF64Rust mirrors s.parse::<f64>() (Rust's FromStr for f64): decimal /
// exponent / leading-dot / signed forms, plus the case-insensitive "inf" /
// "infinity" / "nan" special values. Go's strconv.ParseFloat is a SUPERSET —
// it also accepts two spellings Rust REJECTS: digit-group underscores
// ("1_000") and hex floats ("0x1p4"). Both are rejected here BEFORE
// ParseFloat, so native never CLAIMS a numeric string BAML's coerce_int /
// coerce_float would decline (a parity over-claim). A ParseFloat range error
// (overflow, e.g. "1e400") also declines — Rust yields Ok(inf) there, but
// native conservatively falls back rather than guess the dynamic bridge's
// handling of a non-finite value.
//
// NOTE: a valid "inf" / "nan" spelling returns the non-finite value with
// ok=true — the caller then rejects it: the int path via i64FromF64RoundOk and
// every FLOAT-emitting path via floatRaw both DECLINE non-finite (a NaN / ±Inf
// has no valid JSON number form, and native does not claim BAML's saturation
// against the dynamic bridge).
func parseF64Rust(s string) (float64, bool) {
	if strings.IndexByte(s, '_') >= 0 {
		return 0, false // Rust f64 parse rejects digit-group underscores.
	}
	t := s
	if len(t) > 0 && (t[0] == '+' || t[0] == '-') {
		t = t[1:]
	}
	if strings.HasPrefix(t, "0x") || strings.HasPrefix(t, "0X") {
		return 0, false // Rust f64 parse rejects hex-float syntax.
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return f, true
}

// i64FromF64Round rounds f half-away-from-zero (Rust f64::round) then applies
// Rust's saturating float→int cast: NaN→0, values ≥ 2^63 saturate to
// math.MaxInt64, values < -2^63 saturate to math.MinInt64. (Go's own
// float→int conversion is implementation-defined out of range, so the bounds
// are checked explicitly.)
func i64FromF64Round(f float64) int64 {
	r := math.Round(f) // ties away from zero, matching Rust f64::round
	switch {
	case math.IsNaN(r):
		return 0
	case r >= 9223372036854775808.0: // 2^63 (i64::MAX + 1)
		return math.MaxInt64
	case r < -9223372036854775808.0: // -2^63 (i64::MIN, exactly representable)
		return math.MinInt64
	default:
		return int64(r)
	}
}

// i64FromF64RoundOk applies i64FromF64Round but reports ok=false for a
// NON-FINITE input (NaN / ±Inf). BAML would saturate it via `round() as i64`
// (inf→i64::MAX, nan→0), but native conservatively DECLINES the whole int
// coercion rather than claim against the dynamic bridge's non-finite handling
// — a parity-safe under-claim (fall back to BAML) captured in the corpus. A
// FINITE value out of i64 range (e.g. 1e19) still saturates and returns
// ok=true, matching BAML exactly.
func i64FromF64RoundOk(f float64) (int64, bool) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return i64FromF64Round(f), true
}

// floatFromMaybeFraction ports float_from_maybe_fraction: split on the FIRST
// '/', parse each side (trimmed) as f64, and divide when the denominator is
// non-zero.
func floatFromMaybeFraction(s string) (float64, bool) {
	i := strings.IndexByte(s, '/')
	if i < 0 {
		return 0, false
	}
	num, ok1 := parseF64Rust(strings.TrimSpace(s[:i]))
	den, ok2 := parseF64Rust(strings.TrimSpace(s[i+1:]))
	if !ok1 || !ok2 || den == 0.0 {
		return 0, false
	}
	return num / den, true
}

// extractedNumberRe ports float_from_comma_separated's number regex verbatim:
// unanchored, an optional sign and currency prefix, then a comma-grouped /
// decimal / plain / leading-dot mantissa with an optional exponent. It is
// deliberately case-sensitive on 'e' (matching the Rust literal).
var extractedNumberRe = regexp.MustCompile(`([-+]?)\$?(?:\d+(?:,\d+)*(?:\.\d+)?|\d+\.\d+|\d+|\.\d+)(?:e[-+]?\d+)?`)

// currencySymbolRe matches any Unicode currency symbol (\p{Sc}), removed from
// the extracted number before parsing.
var currencySymbolRe = regexp.MustCompile(`\p{Sc}`)

// floatFromCommaSeparated ports float_from_comma_separated: require EXACTLY one
// regex match in the whole string, strip its commas, then strip Unicode
// currency symbols, and parse the remainder as f64. Percent signs are never
// part of the match (so "50%" → 50.0, not 0.5) and multiple numbers yield no
// result.
func floatFromCommaSeparated(s string) (float64, bool) {
	ms := extractedNumberRe.FindAllString(s, -1)
	if len(ms) != 1 {
		return 0, false
	}
	withoutCommas := strings.ReplaceAll(ms[0], ",", "")
	withoutCurrency := currencySymbolRe.ReplaceAllString(withoutCommas, "")
	return parseF64Rust(withoutCurrency)
}

// coerceLiteral coerces a value to a literal target. String literals route
// through BAML's actual match_string (Mcoerce-a) — trim / fold / strip /
// case-insensitive / substring — emitting the canonical literal (not the
// fuzzy raw input); a substring tie is a CLAIMED error and a no-match
// DECLINES. Int and bool literals are LENIENT in Mcoerce-b: they run the
// primitive int/bool coercer and compare the coerced value to the literal
// (coerce_literal.rs:110-135), so string→int, float→int, and string→bool all
// resolve exactly as BAML would. On a value MISMATCH after a successful
// primitive coercion native DECLINES (falls back) rather than claiming BAML's
// error-vs-default choice, keeping the conservative Mcoerce boundary.
//
// BAML's coerce_literal runs a single-key-object ObjectToPrimitive extraction
// BEFORE every literal kind (coerce_literal.rs:90): an object with EXACTLY ONE
// key whose inner value is a number / bool / string is unwrapped, the literal is
// re-coerced against that inner value, and on success ObjectToPrimitive (score
// 2) is added (the key name is ignored). An inner object / array / null gets NO
// extraction and falls through to the normal per-kind path. This is Mcoerce-d;
// a JSON null still DECLINES (BAML error_unexpected_null before any extraction).
func coerceLiteral(lit *schema.LiteralValue, input value, f *coerceFlags) (json.RawMessage, error) {
	if lit == nil {
		return nil, fmt.Errorf("debaml: literal type missing value")
	}
	f.setKind(candScalar) // a literal coerces to a String/Int/Bool value — non-composite.
	// Single-key-object ObjectToPrimitive prelude (coerce_literal.rs:90). Only a
	// number / bool / string inner value is extracted; the recursion re-runs this
	// coercer on that scalar (so the prelude never re-fires) and PROPAGATES an
	// inner mismatch — BAML does NOT fall back to matching the whole object.
	if input.kind == valObject && len(input.objV) == 1 {
		if inner := input.objV[0].val; inner.kind == valNumber || inner.kind == valBool || inner.kind == valString {
			out, err := coerceLiteral(lit, inner, f)
			if err != nil {
				return nil, err
			}
			f.add(2) // ObjectToPrimitive (score 2)
			return out, nil
		}
	}
	switch lit.Kind {
	case schema.LiteralString:
		// A NON-null non-string stringifies via jsonish Value Display and marks
		// ObjectToString (score 2, Mcoerce-d) before matching; a JSON null
		// DECLINES (stringForMatch). A multi-key object (or an object whose lone
		// inner value is an object/array/null, which the prelude skipped) is
		// stringified whole here.
		str, err := stringForMatch(input, f)
		if err != nil {
			return nil, err
		}
		matched, outcome, viaSub := matchString(str, []matchCandidate{{name: lit.String, validValues: []string{lit.String}}}, true)
		switch outcome {
		case matchOne:
			if viaSub {
				f.add(2) // SubstringMatch (cost 2)
			}
			return marshalJSON(matched)
		case matchAmbiguous:
			// A single candidate cannot tie; defensive.
			return nil, ambiguousMatch(fmt.Sprintf("literal string %q", lit.String), str)
		default:
			// A no-match through the COMPLETE match_string (native reproduces every
			// strategy for the already-stringified input) is a PROVABLE BAML error:
			// coerce_string/match_string errors this literal. In a union arm that is
			// an excluded candidate (not a native-can't-prove decline); at a
			// standalone position it still declines (fall back) — provenError wraps
			// ErrDeBAMLParseUnsupported, so top-level/container disposition is
			// unchanged. (A higher-level optional's null-default is the union's job,
			// not this leaf's.)
			return nil, provenError(fmt.Sprintf("literal string %q: %q matches no value (BAML errors)", lit.String, str))
		}
	case schema.LiteralInt:
		// A multi-key object (or single-key object with an object/array/null
		// inner) not consumed by the prelude falls to coerceIntValue, whose
		// default arm DECLINES it (BAML coerce_int error_unexpected_type).
		n, flagged, err := coerceIntValue(input)
		if err != nil {
			if !declinableChildError(err) {
				return nil, err // hard/invariant failure: propagate.
			}
			return nil, unsupported(fmt.Sprintf("literal int %d: primitive int coercion declined: %v", lit.Int, err))
		}
		if n != lit.Int {
			// The primitive int coercion SUCCEEDED but the value is not the literal:
			// a PROVABLE BAML error (coerce_literal compares equal and errors). In a
			// union arm that is an excluded candidate; at a standalone position it
			// still declines (provenError wraps the fallback sentinel).
			return nil, provenError(fmt.Sprintf("literal int %d: coerced to %d (value mismatch → BAML errors)", lit.Int, n))
		}
		if flagged {
			f.add(1) // FloatToInt (score 1) — preserved from the primitive coercer.
		}
		return json.RawMessage(strconv.FormatInt(n, 10)), nil
	case schema.LiteralBool:
		b, flagged, err := coerceBoolValue(input)
		if err != nil {
			if !declinableChildError(err) {
				return nil, err // hard/invariant failure: propagate.
			}
			return nil, unsupported(fmt.Sprintf("literal bool %v: primitive bool coercion declined: %v", lit.Bool, err))
		}
		if b != lit.Bool {
			// Primitive bool coercion SUCCEEDED but the value is not the literal: a
			// PROVABLE BAML error (excluded in a union arm; declines standalone).
			return nil, provenError(fmt.Sprintf("literal bool %v: coerced to %v (value mismatch → BAML errors)", lit.Bool, b))
		}
		if flagged {
			f.add(1) // StringToBool (score 1) — preserved from the primitive coercer.
		}
		return boolRaw(b), nil
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
// without scoring, so it falls back. A NON-null non-string input is stringified
// via jsonish Value Display and marked ObjectToString (score 2, Mcoerce-d,
// stringForMatch) before matching — so an object/array/number/bool that
// substring-matches a value coerces to that value's canonical name; a JSON null
// DECLINES (never matched). An enum absent from the lowered bundle is a broken
// schema, kept as a claimed error.
func coerceEnum(b *schema.Bundle, name string, input value, f *coerceFlags) (json.RawMessage, error) {
	e, ok := b.FindEnum(name)
	if !ok {
		return nil, fmt.Errorf("debaml: unknown enum %q", name)
	}
	f.setKind(candEnum)
	str, err := stringForMatch(input, f)
	if err != nil {
		return nil, err
	}
	matched, outcome, viaSub := matchString(str, enumMatchCandidates(e), true)
	switch outcome {
	case matchOne:
		if viaSub {
			f.add(2) // SubstringMatch (cost 2)
		}
		// matched is the candidate name = the value's canonical real name.
		return marshalJSON(matched)
	case matchAmbiguous:
		return nil, ambiguousMatch(fmt.Sprintf("enum %q", name), str)
	default:
		// A no-match through the COMPLETE match_string is a PROVABLE BAML error
		// (coerce_enum errors); excluded in a union arm, declines standalone.
		return nil, provenError(fmt.Sprintf("enum %q: %q matches no value (BAML errors)", name, str))
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
			// Rust .trim() (Unicode 17.0.0 White_Space) via bamlunicode, matching
			// BAML's coerce_enum candidate construction — not Go's unicode.IsSpace.
			if d := bamlunicode.TrimSpace(*v.Description); d != "" {
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

// coerceClass ports BAML's structural class coercer (coerce_class.rs, the
// non-array subset) so native reproduces BAML's class value BYTE-for-BYTE.
// Fields are emitted in SCHEMA declaration order under their CANONICAL names;
// input keys are matched to fields by BAML's no-substring match_string
// (matchesStringToString) — each key to the FIRST field whose rendered name it
// fuzzily matches, first key keeps a field (update_map "keep first"), unmatched
// keys are extras (ExtraKey, cost 1, not emitted). On top of field assignment it
// ports (Mcoerce-d):
//
//   - SINGLE-field OBJECT implied-key (coerce_class.rs:224): when NO key matched
//     the lone field and the object is non-empty, the WHOLE object is coerced
//     into that field (ImpliedKey, cost 2) instead of flagging ExtraKey.
//   - SINGLE-field SCALAR/null inferred-object (coerce_class.rs:295): a
//     non-object non-array value is coerced into the lone field (ImpliedKey cost
//     2 on the child + InferedObject cost 0 on the class).
//   - Missing OPTIONAL field → null (OptionalDefaultFromNoValue cost 1 + Pending);
//     native OMITS it (InjectAbsentOptionals re-adds the null downstream) but
//     flags the score so a nullable-union arm stays non-clean.
//   - Missing required field → TypeIR::default_value (defaultValue): list→[],
//     map→{}, primitive-null→null, first-defaultable-union-arm, tuple (all cost
//     100 DefaultFromNoValue); a non-defaultable missing required field is a
//     BAML error_missing_required_field.
//   - Present required MAP field with a NON-object value → the map default {}
//     (DefaultButHadUnparseableValue cost 2): coerce_map errors on a non-object
//     (error_unexpected_type), a PROVEN failure whose default is deterministic.
//
// It CLAIMS a class SUCCESS when every field resolves to a value native can
// prove equals BAML's, PROPAGATES a claimed child error (an enum/literal
// substring tie — BAML errors that non-defaultable field and so the class), and
// DECLINES (fall back) otherwise:
//
//   - ARRAY input → DECLINE: BAML runs coerce_array_to_singular / pick_best over
//     the items (M3); native does not model the scored selection.
//   - A required NON-defaultable field is absent → DECLINE: BAML errors, but
//     native falls back to stay strictly non-over-claiming (mirroring the
//     pre-Mcoerce-d conservative choice).
//   - A field native's coercer merely DECLINED (native stricter, so BAML may
//     leniently succeed or null-default) or an implied-key/inferred coercion that
//     declined → DECLINE: native cannot prove BAML's field value.
//
// Every score-bearing flag (ExtraKey / ImpliedKey / OptionalDefaultFromNoValue /
// DefaultFromNoValue / DefaultButHadUnparseableValue) and any flagged child folds
// into cf, so a nullable-union claim can require this class arm to be clean.
func coerceClass(b *schema.Bundle, name string, mode schema.StreamingMode, input value, cf *coerceFlags, cctx *coerceCtx) (json.RawMessage, error) {
	cls, ok := b.FindClass(name, mode)
	if !ok {
		return nil, fmt.Errorf("debaml: unknown class %q", name)
	}
	// BAML's Class::coerce circular-reference guard (coerce_class.rs), BEFORE the
	// array/implied work: a repeated (ClassKey, value) on the active COERCE path is a
	// circular reference → BAML's parsing error. Derive a CHILD context carrying this
	// pair for every descent below, so siblings inherit the ancestor path but never
	// poison each other. No depth cap. (cctx nil-safe: an empty context never fires.)
	key := schema.ClassKey{Name: name, Mode: mode}
	if cctx.coerceHas(key, input) {
		return nil, errCircularReference(name)
	}
	child := cctx.enterCoerce(key, input)
	// ARRAY input → coerce_array_to_singular over the items coerced INTO the class
	// (coerce_class.rs). M3d claims only the MULTI-field all-required-flat-leaf shape
	// (coerceClassArray); every other shape DECLINES (see coerceClassArray).
	if input.kind == valArray {
		return coerceClassArray(b, cls, name, mode, input.arrV, cf, child)
	}
	cf.setKind(candClass)
	nF := len(cls.Fields)
	singleField := nF == 1

	// Per-field resolution: out[i] holds the emitted bytes when filled[i]. A
	// still-unfilled required NON-defaultable field sets missingRequired (BAML
	// errors; native DECLINES, see below); a field native cannot resolve to
	// BAML's exact value sets indeterminate (fall back).
	out := make([]json.RawMessage, nF)
	filled := make([]bool, nF)
	missingRequired := false
	indeterminate := false
	// pick_best class discriminators: hasRealField is set when any field takes a
	// real input value (matched / implied / inferred) — so classAllDefault (every
	// emitted field came from a default/missing path) is its negation.
	// impliedString records a single-field string filled via ImpliedKey (the
	// class-vs-union devalue rule keys on it). Both are score-neutral; they feed
	// pickBest only, and are unreachable in M3a's flat/single-arm families (where
	// class-vs-class never fires), so they are computed best-effort for M3c.
	hasRealField := false
	impliedString := false

	// resolveMatched coerces a matched/implied/inferred field value into field i,
	// folding a clean child's flags on success and classifying a failure: a
	// CLAIMED mismatch and a PROVABLE field parse error both make BAML error the
	// whole class (a proven class error); a PROVEN map-non-object failure fills the
	// {} default; every other declinable failure is INDETERMINATE (native may be
	// stricter than BAML); a hard failure propagates.
	resolveMatched := func(i int, v value) error {
		childF := &coerceFlags{}
		o, err := coerce(b, cls.Fields[i].Type, v, childF, child)
		if err == nil {
			out[i] = o
			filled[i] = true
			hasRealField = true
			cf.foldChild(childF) // class score = own + field value scores (types.rs)
			return nil
		}
		if !declinableChildError(err) {
			return err // hard/invariant failure: propagate, never mask.
		}
		if isClaimedMismatch(err) {
			// A CLAIMED field error (enum/literal substring tie StrMatchOneFromMany,
			// a non-nullable-union null) — BAML PROVABLY errors this field, and only
			// NON-defaultable field types produce it, so BAML errors the whole class
			// (no array candidate for non-array input). Propagate so native claims
			// the same error (the differential checks status parity).
			return err
		}
		if cls.Fields[i].Type.Kind == schema.TypeMap && v.kind != valObject {
			// coerce_map on a non-object is error_unexpected_type (coerce_map.rs),
			// so BAML fills the map default {} with DefaultButHadUnparseableValue.
			out[i] = json.RawMessage("{}")
			filled[i] = true
			hasRealField = true
			cf.add(2) // DefaultButHadUnparseableValue (cost 2)
			return nil
		}
		if childF.isUncertain() {
			// A nested child carried number-display uncertainty native cannot
			// reproduce byte-exact (displayNumber) — the field might coerce (BAML
			// keeps) or not, so native cannot classify the class: INDETERMINATE (a
			// scored union arm declines the whole union). (#555 Slice 2 removed the
			// former case-fold cause; only number-display reaches here now.)
			cf.markUncertain()
			indeterminate = true
			return nil
		}
		if provenClassFieldError(b, cls.Fields[i].Type, v, err) {
			// The value is a PROVABLE BAML parse error for a REQUIRED NON-defaultable
			// field (a bad numeric string / wrong-kind scalar / null-into-string, OR
			// an int/bool literal value mismatch), so BAML records Some(Err) for that
			// field and PROVABLY errors the whole class (a present-but-unparseable
			// required field, no array candidate for non-array input). Return a proven
			// class error so a scored union arm is EXCLUDED (BAML selects the OTHER
			// arm) rather than declining the whole union.
			return provenError(fmt.Sprintf("class %q: required field %q provably fails to coerce (BAML errors the class)", name, cls.Fields[i].Name.Name))
		}
		indeterminate = true
		return nil
	}

	// coerceImplied coerces v into the lone field (implied-key / inferred-object),
	// filling it with ImpliedKey (cost 2) on success. A CLAIMED failure propagates
	// (the lone non-defaultable field errors → BAML errors the class); an unproven
	// declinable failure records indeterminate (native cannot prove BAML's
	// ExtraKey+default/error path).
	coerceImplied := func(v value) error {
		childF := &coerceFlags{}
		o, err := coerce(b, cls.Fields[0].Type, v, childF, child)
		if err == nil {
			out[0] = o
			filled[0] = true
			hasRealField = true
			cf.foldChild(childF) // class score = own + field value scores (types.rs)
			cf.add(2)            // ImpliedKey (cost 2); InferedObject is cost 0 (no flag).
			// A single-field STRING filled via ImpliedKey is the class-vs-union
			// devalue subject (pick_best); the field type is a string primitive, so
			// the emitted value is a JSON string carrying ImpliedKey.
			if singleField && cls.Fields[0].Type.Kind == schema.TypePrimitive && cls.Fields[0].Type.Primitive == schema.PrimitiveString {
				impliedString = true
			}
			return nil
		}
		if !declinableChildError(err) {
			return err
		}
		if isClaimedMismatch(err) {
			return err // claimed field error → BAML errors the class; propagate.
		}
		if childF.isUncertain() {
			cf.markUncertain()
			indeterminate = true
			return nil
		}
		if provenClassFieldError(b, cls.Fields[0].Type, v, err) {
			// The implied/inferred value PROVABLY fails to coerce into the lone
			// REQUIRED NON-defaultable field, so BAML errors the class. Return a
			// proven class error so a scored union arm is EXCLUDED, not declined.
			return provenError(fmt.Sprintf("class %q: implied field %q provably fails to coerce (BAML errors the class)", name, cls.Fields[0].Name.Name))
		}
		indeterminate = true
		return nil
	}

	switch input.kind {
	case valObject:
		// Assign input keys to fields in INPUT order: each key to the FIRST field
		// it fuzzily matches (no substring); a key matching an already-filled
		// field is a duplicate (keep first, ignored, NOT an extra); a key matching
		// no field is an extra. Input-key-first order (rather than scanning fields
		// for a matching key) is what resolves one key to a single field.
		matched := make([]bool, nF)
		assigned := make([]value, nF)
		foundAny := false
		extraCount := 0
		for i := range input.objV {
			key := input.objV[i].key
			mf := -1
			for j := range cls.Fields {
				// The field-key fold is now proven (bamlunicode == BAML's
				// match_string), so a non-ASCII key assignment is native's to CLAIM.
				if matchesStringToString(key, cls.Fields[j].Name.RenderedName()) {
					mf = j
					break
				}
			}
			switch {
			case mf < 0:
				extraCount++
			case matched[mf]:
				// Duplicate match for an already-filled field: keep first, ignore.
			default:
				assigned[mf] = input.objV[i].val
				matched[mf] = true
				foundAny = true
			}
		}
		for j := range cls.Fields {
			if matched[j] {
				if err := resolveMatched(j, assigned[j]); err != nil {
					return nil, err
				}
			}
		}
		switch {
		case singleField && !foundAny && extraCount > 0:
			// No key matched the lone field: coerce the whole object into it
			// (ImpliedKey, no ExtraKey). A declinable failure → indeterminate.
			if err := coerceImplied(input); err != nil {
				return nil, err
			}
		case extraCount > 0:
			cf.add(extraCount) // ExtraKey (cost 1 EACH) — not a clean zero-score class.
		}
	default:
		// Scalar / null (non-object, non-array). A single-field class absorbs the
		// value into its lone field (inferred-object); a multi-field class assigns
		// nothing (BAML's Some(x) arm is a no-op for >1 field) — every field falls
		// to the default/missing pass below.
		if singleField {
			if err := coerceImplied(input); err != nil {
				return nil, err
			}
		}
	}

	if indeterminate {
		// A field native could not resolve to BAML's exact value (a non-proven
		// coercion decline, or an implied-key/inferred decline): fall back so
		// BAML's lenient success / null-default / scored choice stands.
		return nil, unsupported(fmt.Sprintf("class %q: a field could not be resolved to BAML's value (deferred lenient success/default/scoring)", name))
	}

	// Defaults / missing pass for every still-unfilled field (BAML's "check what
	// we have / what we need").
	for i := range cls.Fields {
		if filled[i] {
			continue
		}
		ft := cls.Fields[i].Type
		if isOptional(ft) {
			// Absent optional → null (OptionalDefaultFromNoValue + Pending). Native
			// omits it (InjectAbsentOptionals adds the null downstream, identically
			// for both legs) but scores it for the nullable-union comparison.
			cf.add(1) // OptionalDefaultFromNoValue (cost 1)
			continue
		}
		if d, ok := defaultValue(ft); ok {
			out[i] = d
			filled[i] = true
			cf.add(100) // DefaultFromNoValue (cost 100)
			continue
		}
		missingRequired = true
	}

	if missingRequired {
		// A required non-defaultable field is absent: BAML has no array candidate
		// for non-array input, so it PROVABLY errors (error_missing_required_field).
		// At a standalone / container position native DECLINES (falls back) rather
		// than claim the error — provenError wraps the fallback sentinel, so that
		// disposition is unchanged. Inside a UNION arm the categorizer recognizes
		// the proven error and EXCLUDES this arm from scoring (it is a real BAML
		// failure, not a native-can't-prove decline), so e.g. a Book | Car | null
		// union whose Car arm misses both its required fields scores just Book vs
		// null. The class-with-all-required-flat-leaf SCALAR list/map SKIP case is
		// still identified independently by provenListItemError.
		return nil, provenError(fmt.Sprintf("class %q: required non-defaultable field missing (BAML errors)", name))
	}

	// pick_best class discriminators, now that field resolution is complete.
	propCount := 0
	for i := range cls.Fields {
		if filled[i] {
			propCount++
		}
	}
	if cf != nil {
		cf.classPropCount = propCount
		cf.classAllDefault = !hasRealField // every emitted field came from a default/missing path
		cf.classSingleImplied = impliedString
	}

	var buf bytes.Buffer
	buf.WriteByte('{')
	first := true
	for i := range cls.Fields {
		if !filled[i] {
			continue // absent optional (omitted; InjectAbsentOptionals adds null).
		}
		if !first {
			buf.WriteByte(',')
		}
		first = false
		key, err := marshalJSON(cls.Fields[i].Name.Name)
		if err != nil {
			return nil, err
		}
		buf.Write(key)
		buf.WriteByte(':')
		buf.Write(out[i])
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// coerceClassArray ports coerce_class.rs's ARRAY-input branch for the ONLY class
// shape native claims: a MULTI-field class every field of which is a REQUIRED FLAT
// LEAF (classAllRequiredFlatLeaf — the same shape the union gate admits). For that
// shape, coerce_class runs coerce_array_to_singular over the items (each coerced
// INTO the class) and, because no field has a default_value, array input NEVER
// builds a completed_instance to compete (completed_cls = [array-to-singular
// candidate] only) — so the whole class result IS the array-to-singular winner plus
// FirstMatch. Native reproduces exactly that: coerceArrayToSingular over the items
// (each item → coerce(class, item)), folded into cf (kind class, FirstMatch, the
// item's own score). An object item that fully coerces is Ok; a scalar / partial /
// missing-required item is a PROVEN coerce_class error (coerceClass returns
// provenError), excluded from ranking; an indeterminate item declines the whole
// array-to-singular.
//
// Every OTHER class shape DECLINES (deferred, conservative):
//   - a SINGLE-field class array is an implied-key-vs-array-to-singular pick_best
//     COMPETITION (coerce_class.rs adds a second implied-key candidate) native does
//     not model;
//   - a class with any DEFAULTABLE / OPTIONAL / nested field would build a competing
//     completed_instance (defaults fill the missing required fields), whose scoring
//     against the array-to-singular candidate native does not model.
func coerceClassArray(b *schema.Bundle, cls *schema.ClassDef, name string, mode schema.StreamingMode, items []value, cf *coerceFlags, cctx *coerceCtx) (json.RawMessage, error) {
	if !classAllRequiredFlatLeaf(cls) {
		// Single-field (implied-key competition) or a defaultable/optional/nested
		// field (completed_instance competition) — both DECLINE (M3d+).
		return nil, unsupported(fmt.Sprintf("class %q: array-to-singular only claims a multi-field all-required-flat-leaf class (single-field implied-key / defaultable-field competition deferred)", name))
	}
	classType := schema.Type{Kind: schema.TypeClass, Name: name, Mode: mode}
	tu := cf != nil && cf.targetIsUnion
	w, err := coerceArrayToSingular(items, tu, func(item value) (json.RawMessage, *coerceFlags, error) {
		itemCf := &coerceFlags{targetIsUnion: tu}
		out, e := coerce(b, classType, item, itemCf, cctx)
		return out, itemCf, e
	}, nil)
	if err != nil {
		return nil, err
	}
	cf.absorb(w) // kind class + FirstMatch + item score
	return w.output, nil
}

// defaultValue ports TypeIR::default_value (field_type.rs:288) for the subset
// native supports: the value BAML fills for a class field with no usable input.
// It returns the default's JSON bytes and whether the type is DEFAULTABLE:
//
//   - list  → []
//   - map   → {}
//   - primitive null → null
//   - union → the FIRST defaultable arm from iter_include_null (non-null variants
//     in declaration order, then null appended when the union is optional),
//     resolved recursively.
//   - tuple → the list of every element's default, only when ALL elements have
//     defaults (native rejects tuple at the gate, so this stays unreachable but
//     faithful).
//
// Non-defaultable (ok=false): enum, literal, class, recursive alias, and the
// primitive scalars string/int/float/bool — a missing required field of those is
// a BAML error_missing_required_field. BAML only uses a default that passes its
// asserts; native rejects every constrained type at the gate, so all defaults
// here are unconditional.
func defaultValue(t schema.Type) (json.RawMessage, bool) {
	switch t.Kind {
	case schema.TypeList:
		return json.RawMessage("[]"), true
	case schema.TypeMap:
		return json.RawMessage("{}"), true
	case schema.TypePrimitive:
		if t.Primitive == schema.PrimitiveNull {
			return json.RawMessage("null"), true
		}
		return nil, false
	case schema.TypeUnion:
		if t.Union == nil {
			return nil, false
		}
		for i := range t.Union.Variants {
			if d, ok := defaultValue(t.Union.Variants[i]); ok {
				return d, true
			}
		}
		if t.Union.Nullable {
			return json.RawMessage("null"), true // the appended null arm
		}
		return nil, false
	case schema.TypeTuple:
		var buf bytes.Buffer
		buf.WriteByte('[')
		for i := range t.Items {
			d, ok := defaultValue(t.Items[i])
			if !ok {
				return nil, false
			}
			if i > 0 {
				buf.WriteByte(',')
			}
			buf.Write(d)
		}
		buf.WriteByte(']')
		return buf.Bytes(), true
	default:
		// enum, literal, class, recursive_alias, primitive scalar, arrow, top.
		return nil, false
	}
}

// coerceList coerces a value into a list, porting BAML's coerce_array
// (coerce_array.rs) so native reproduces BAML's PARTIAL-list result byte-for-byte.
// A list coercion always SUCCEEDS: child failures fold into list-level flags, not
// a list error.
//
//   - ARRAY input: each element is coerced in input order. A KEPT child's coerced
//     output is appended in accepted order; a child that is a PROVEN BAML parse
//     error (provenListItemError) is DROPPED — BAML records ArrayItemParseError(i)
//     and continues — so an all-bad array becomes [] and [good,bad,good] becomes
//     [good,good].
//   - NON-ARRAY input: BAML wraps the value as a single implied element
//     (SingleToArray). A successful inner coercion yields [child]; a PROVEN inner
//     parse error yields an EMPTY list (SingleToArray + ArrayItemParseError(0)),
//     NOT a parse failure.
//
// THE CRUX (over-claim guard): a child failure is SKIPPED only when it is a
// PROVEN BAML parse error for that exact child target+input. A child native
// merely DECLINED (an ErrDeBAMLParseUnsupported that is NOT a proven error) may be
// a DEFERRED Mcoerce-d SUCCESS (e.g. list<string> with a number → JsonToString),
// so native DECLINES THE WHOLE LIST there rather than skip an item BAML would
// keep. A number-display-uncertain child likewise declines the whole list. A
// HARD/invariant child error propagates unchanged. See coerceListChild /
// provenListItemError for the exact safe-skip vs must-decline split.
//
// Score-bearing flags (SingleToArray, each ArrayItemParseError skip, and a kept
// child's own flags) are threaded into cf so the nullable-union clean-only rule
// (coerceUnionSafe) sees a flagged list arm and declines it against the scored
// null arm — Mcoerce-c does not score collection arms against null.
func coerceList(b *schema.Bundle, elem *schema.Type, input value, cf *coerceFlags, cctx *coerceCtx) (json.RawMessage, error) {
	if elem == nil {
		return nil, fmt.Errorf("debaml: list type missing element")
	}
	cf.setKind(candList)
	var buf bytes.Buffer
	buf.WriteByte('[')
	kept := 0     // emitted items (itemsEmpty = kept == 0)
	errCount := 0 // ArrayItemParseError count (pick_best list-vs-list discriminator)

	if input.kind != valArray {
		// Non-array → BAML wraps it as one implied element (SingleToArray, score 1).
		cf.add(1)
		if cf != nil {
			cf.singleToArray = true
		}
		out, keep, err := coerceListChild(b, *elem, input, cf, cctx)
		if err != nil {
			return nil, err
		}
		if keep {
			buf.Write(out)
			kept++
		} else {
			// The implied element is a proven parse error → ArrayItemParseError(0)
			// (score 1+0=1): the list is the EMPTY list, which still SUCCEEDS.
			cf.add(1)
			errCount++
		}
		buf.WriteByte(']')
		if cf != nil {
			cf.itemsEmpty = kept == 0
			cf.arrayItemErrors = errCount
		}
		return buf.Bytes(), nil
	}

	first := true
	for i := range input.arrV {
		out, keep, err := coerceListChild(b, *elem, input.arrV[i], cf, cctx)
		if err != nil {
			return nil, err
		}
		if !keep {
			// Proven BAML parse error → ArrayItemParseError(i) (score 1+i): SKIP.
			cf.add(1 + i)
			errCount++
			continue
		}
		if !first {
			buf.WriteByte(',')
		}
		first = false
		buf.Write(out)
		kept++
	}
	buf.WriteByte(']')
	if cf != nil {
		cf.itemsEmpty = kept == 0
		cf.arrayItemErrors = errCount
	}
	return buf.Bytes(), nil
}

// coerceListChild coerces one list element (array item or implied singleton).
// It returns exactly one of:
//
//   - (out, true, nil):  the child coerced; append out (its flags merged into cf).
//   - (nil, false, nil): the child is a PROVEN BAML parse error; the caller SKIPS
//     it (BAML records ArrayItemParseError and the list still succeeds).
//   - (nil, false, err): the whole list must FAIL — err is either a HARD/invariant
//     error to propagate, or an ErrDeBAMLParseUnsupported DECLINE because the
//     child could be a DEFERRED Mcoerce-d success or carried number-display
//     uncertainty, so native falls back rather than skip an item BAML might keep.
func coerceListChild(b *schema.Bundle, elem schema.Type, item value, cf *coerceFlags, cctx *coerceCtx) (json.RawMessage, bool, error) {
	childF := &coerceFlags{}
	out, err := coerce(b, elem, item, childF, cctx)
	if err == nil {
		// Kept: the child value's TOTAL score counts toward the list score
		// (types.rs List score = own conditions + sum of item scores), so fold
		// it in (and propagate any child uncertainty).
		cf.foldChild(childF)
		return out, true, nil
	}
	if !declinableChildError(err) {
		return nil, false, err // hard/invariant failure: propagate, never mask.
	}
	if childF.isUncertain() {
		// A nested child carried number-display uncertainty native cannot reproduce
		// byte-exact (displayNumber) — the child might coerce (BAML keeps) or not,
		// and native cannot tell which, so DECLINE the whole list. (#555 Slice 2
		// removed the former case-fold cause; only number-display reaches here now.)
		cf.markUncertain()
		return nil, false, unsupported(fmt.Sprintf("list element %s→%s: number-display uncertainty (cannot prove skip vs keep)", item.kind, elem.Kind))
	}
	if provenListItemError(b, elem, item) {
		return nil, false, nil // PROVEN parse error → BAML skips via ArrayItemParseError.
	}
	// A declinable error that is NOT a proven parse error: BAML may still SUCCEED
	// via a deferred Mcoerce-d path (JsonToString / ObjectToString /
	// ObjectToPrimitive / implied-key / default-fill / array-to-singular / union
	// scoring), so native cannot skip it — DECLINE the whole list (fall back).
	return nil, false, unsupported(fmt.Sprintf("list element %s→%s: child declined but not a proven BAML parse error (deferred Mcoerce-d success possible): %v", item.kind, elem.Kind, err))
}

// provenListItemError reports whether BAML PROVABLY errors coercing item to the
// list element type elem — the ONLY case where BAML records ArrayItemParseError
// and drops the item, so native may skip it. It is a WHITELIST called ONLY after
// native's own coercion FAILED with a declinable error, so for match_string-based
// targets (bool/enum/string-literal from a STRING) — whose fold is now proven
// (bamlunicode) — a native failure already equals a BAML failure. For NUMERIC targets
// native is STRICTER than BAML (inf/overflow/hex/underscore), so a string is
// proven only when BAML's own parse strategies are re-checked
// (bamlStringNumberFails); an input kind BAML rejects with error_unexpected_type
// is proven; an ARRAY defers to coerce_array_to_singular (pick_best, M3) and a
// class/map/union child (or object into a numeric/literal target) defers to
// Mcoerce-d, so those are NOT proven and DECLINE the whole list. For a STRING
// target (Mcoerce-d) a non-null non-string now SUCCEEDS via JsonToString and
// never reaches here; only a direct NULL is proven (error_unexpected_null).
func provenListItemError(b *schema.Bundle, elem schema.Type, item value) bool {
	switch elem.Kind {
	case schema.TypePrimitive:
		switch elem.Primitive {
		case schema.PrimitiveInt, schema.PrimitiveFloat:
			switch item.kind {
			case valString:
				return bamlStringNumberFails(item.strV)
			case valObject, valBool, valNull:
				return true // BAML coerce_int/coerce_float: error_unexpected_type.
			default:
				// valNumber: BAML always coerces a number; valArray:
				// coerce_array_to_singular (pick_best, M3) — both DEFERRED.
				return false
			}
		case schema.PrimitiveBool:
			switch item.kind {
			case valString:
				// Reached only after a native bool coercion FAILED: a CERTAIN
				// match_string miss/tie (the fold is proven, bamlunicode), which
				// BAML's coerce_bool turns into error_unexpected_type.
				return true
			case valObject, valNumber, valNull:
				return true // error_unexpected_type (BAML never coerces number→bool).
			default:
				return false // valBool: success; valArray: array-to-singular (M3).
			}
		case schema.PrimitiveString:
			// A NON-null non-string now stringifies (JsonToString, Mcoerce-d) and
			// SUCCEEDS at the leaf, so it never reaches here. Only a NULL reaches
			// here: BAML coerce_string on Null is error_unexpected_null → a proven
			// ArrayItemParseError skip (an array item is never null-defaulted).
			return item.kind == valNull
		default:
			// null target: coercePrimitiveNull always succeeds (any value
			// null-defaults), so a null-target child never fails into here.
			return false
		}
	case schema.TypeEnum:
		// A non-string enum input stringifies (ObjectToString, Mcoerce-d). A
		// STRING native rejected is a CERTAIN match_string miss/tie (the fold is
		// proven, bamlunicode), which coerce_enum turns into an error in a required
		// list-element position.
		return item.kind == valString
	case schema.TypeLiteral:
		// Only a STRING literal from a STRING is a proven match_string failure;
		// int/bool literals (value mismatch / exhausted parse) and object/scalar
		// extraction defer to Mcoerce-d.
		return elem.Literal != nil && elem.Literal.Kind == schema.LiteralString && item.kind == valString
	case schema.TypeClass:
		// A multi-field class whose fields are ALL required flat leaves has no
		// default_value for any field and is not single-field, so a genuine SCALAR
		// input can be neither inferred-object-absorbed nor default-filled — every
		// required field stays unfilled → BAML error_missing_required_field. That
		// is the PROVEN skip this whitelist establishes (coerceClass itself only
		// DECLINES that case — see its missingRequired branch — so the list/map
		// skip verdict is decided HERE, not by a claimed class error). Everything
		// else is NOT proven: a single-field class (Mcoerce-d inferred-object /
		// implied-key) or a class with any defaultable (list/map/optional/
		// defaultable-union) field now SUCCEEDS through coerceClass and is KEPT
		// before reaching here (or DECLINES as indeterminate → whole-list fallback);
		// a valObject may coerce or default; an ARRAY element defers to
		// coerce_array_to_singular (pick_best, M3).
		cls, ok := b.FindClass(elem.Name, elem.Mode)
		if !ok {
			return false
		}
		if !classAllRequiredFlatLeaf(cls) {
			return false
		}
		switch item.kind {
		case valString, valNumber, valBool, valNull:
			return true
		default:
			return false // valObject: coerce succeeds/defaults/declines; valArray: M3.
		}
	default:
		// map / list / union children: a map non-object, a nested list, or a
		// union needing scoring are all DEFERRED, never proven parse errors.
		return false
	}
}

// provenClassFieldError reports whether a present class field of type fieldT
// whose coercion FAILED with err PROVABLY makes BAML error the whole class. Two
// conditions must both hold:
//
//   - the field is REQUIRED and NON-DEFAULTABLE (not optional and has no
//     TypeIR::default_value). BAML records such a present-but-unparseable field as
//     an unparsed_required field and errors the class (no array candidate for
//     non-array input). A DEFAULTABLE field is DEFAULTED on failure, not errored —
//     an optional field → null, a list/map/null/defaultable-union → its default —
//     so excluding an arm whose field BAML would default would OVER-claim the
//     other arm; those return false (native stays indeterminate → whole-union
//     decline). (A required map field with a non-object value is the {} default,
//     handled by resolveMatched before this call.)
//   - BAML provably errors the field coercion: either err is already a proven
//     BAML error (isProvenBamlError — a literal/enum no-match, an int/bool LITERAL
//     value mismatch, a missing sub-field), OR the value is a proven-error SHAPE
//     the leaf coercer reports as a plain decline (provenListItemError — a bad
//     numeric string, a wrong-kind scalar, a null into a string).
//
// The isProvenBamlError(err) arm is why an int/bool LITERAL field mismatch (which
// coerceLiteral flags as provenErr but provenListItemError does not recognize)
// now correctly excludes just that arm.
func provenClassFieldError(b *schema.Bundle, fieldT schema.Type, v value, err error) bool {
	if isOptional(fieldT) {
		return false
	}
	if _, ok := defaultValue(fieldT); ok {
		return false // a defaultable field is DEFAULTED on failure, not errored.
	}
	return isProvenBamlError(err) || provenListItemError(b, fieldT, v)
}

// classAllRequiredFlatLeaf reports whether cls is a multi-field class every field
// of which is a REQUIRED flat leaf (primitive scalar / literal / enum — the
// isFlatLeafField set, which excludes optionals, lists, maps, unions and nested
// classes). Such a class has no field with a BAML default_value, so a genuine
// SCALAR input can be neither absorbed (single-field implied-key needs len==1)
// nor default-filled, guaranteeing a missing-required-field error — the only
// class shape safe to treat as a proven list-item skip.
func classAllRequiredFlatLeaf(cls *schema.ClassDef) bool {
	if len(cls.Fields) < 2 {
		return false
	}
	for i := range cls.Fields {
		if !isFlatLeafField(cls.Fields[i].Type) {
			return false
		}
	}
	return true
}

// bamlStringNumberFails reports whether BAML's coerce_int / coerce_float STRING
// path (coerce_primitive.rs) would DEFINITELY error on s — i.e. NONE of Rust's
// strategies yields a number: i64, u64, f64 (INCLUDING inf/nan/overflow, which
// Rust parses to a non-finite f64 BAML then saturates), a single fraction, or a
// single extracted comma/currency number. Native's own numeric coercers are
// STRICTER (they reject non-finite/overflow/underscore/hex), so a native failure
// does NOT prove a BAML failure; only this Rust-faithful re-check does, and only
// a TRUE result makes a bad numeric string a skippable list item. (A single-i64/
// -u64 string is always f64-parseable, so rustF64Parseable subsumes all three.)
func bamlStringNumberFails(s string) bool {
	t := trimNumericString(s) // BAML: s.trim().trim_end_matches(',')
	if rustF64Parseable(t) {
		return false
	}
	if bamlFractionParses(t) {
		return false
	}
	if bamlCommaNumberParses(t) {
		return false
	}
	return true
}

// rustF64Parseable reports whether Rust's <str>::parse::<f64>() would return Ok
// for s — the predicate BAML's numeric coercers use. It mirrors parseF64Rust's
// REJECTIONS (digit-group underscores and hex-float syntax, which Go's ParseFloat
// accepts but Rust does not) but, UNLIKE parseF64Rust, treats an OVERFLOW as a
// parse SUCCESS: Rust returns Ok(±inf) for "1e400" while Go returns ErrRange, and
// "inf"/"nan" parse to non-finite values both accept. Native's own coercers
// DECLINE those non-finite results, but for classifying a list item native needs
// BAML's verdict — and BAML SUCCEEDS (saturating) — so they must NOT count as
// proven parse errors.
func rustF64Parseable(s string) bool {
	if s == "" {
		return false
	}
	if strings.IndexByte(s, '_') >= 0 {
		return false // Rust f64 parse rejects digit-group underscores.
	}
	t := s
	if t[0] == '+' || t[0] == '-' {
		t = t[1:]
	}
	if strings.HasPrefix(t, "0x") || strings.HasPrefix(t, "0X") {
		return false // Rust f64 parse rejects hex-float syntax.
	}
	if _, err := strconv.ParseFloat(s, 64); err == nil {
		return true
	} else if errors.Is(err, strconv.ErrRange) {
		return true // overflow → Rust yields Ok(±inf).
	}
	return false
}

// bamlFractionParses mirrors float_from_maybe_fraction (coerce_primitive.rs):
// split on the FIRST '/', both trimmed sides parse as Rust f64, denominator
// non-zero. (BAML then divides; the exact quotient is irrelevant here — a success
// means BAML coerces the string, so it is NOT a proven error.)
func bamlFractionParses(s string) bool {
	i := strings.IndexByte(s, '/')
	if i < 0 {
		return false
	}
	num := strings.TrimSpace(s[:i])
	den := strings.TrimSpace(s[i+1:])
	if !rustF64Parseable(num) || !rustF64Parseable(den) {
		return false
	}
	// denominator != 0.0 (BAML's guard). A den that overflows to ±inf is != 0.
	d, err := strconv.ParseFloat(den, 64)
	if err != nil && !errors.Is(err, strconv.ErrRange) {
		return false
	}
	return d != 0.0
}

// bamlCommaNumberParses mirrors float_from_comma_separated (coerce_primitive.rs):
// require EXACTLY one extracted-number regex match, strip its commas and Unicode
// currency symbols, then parse the remainder as Rust f64.
func bamlCommaNumberParses(s string) bool {
	ms := extractedNumberRe.FindAllString(s, -1)
	if len(ms) != 1 {
		return false
	}
	withoutCommas := strings.ReplaceAll(ms[0], ",", "")
	withoutCurrency := currencySymbolRe.ReplaceAllString(withoutCommas, "")
	return rustF64Parseable(withoutCurrency)
}

// coerceMap coerces a JSON object into a map, porting BAML's coerce_map
// (coerce_map.rs) so native reproduces BAML's PARTIAL-map result byte-for-byte.
// A map coercion of an OBJECT always SUCCEEDS: a bad VALUE folds into a
// score-bearing MapValueParseError and the ENTRY is SKIPPED, not a map error.
// Entries are emitted in INPUT key order (the opposite of coerceClass's schema
// order: a map's keys are data, a class's fields are the schema), keyed by the
// ORIGINAL input key string (never the canonical enum/literal form, matching
// coerce_map.rs:165-174) — so an all-bad-value map becomes {} and
// {"b":good,"bad":bad,"a":good} becomes {"b":...,"a":...}.
//
// Per BAML each entry coerces the VALUE first, then the KEY:
//
//   - value coercion PROVEN-errors → MapValueParseError: SKIP the whole entry
//     WITHOUT coercing the key (coerceMapValueChild / provenMapValueError).
//   - value succeeds but the KEY does not cleanly coerce → DECLINE the whole map
//     (coerceMapKey). Unlike a value, a KEY never yields a native partial skip:
//     the DYNAMIC bridge's enum/literal/literal-union key coercion is LENIENT —
//     it accepts a NON-MATCHING key and KEEPS the ORIGINAL string (live-captured:
//     {"A":x,"C":z} over enum {A,B} or over "A"|"B" → the FULL map, not a partial
//     one), so a key miss is a DEFERRED Mcoerce-d keep, not a provable skip.
//   - both succeed → insert (original key string, coerced value).
//
// THE CRUX (over-claim guard, mirroring coerceList): an entry's VALUE is SKIPPED
// only when its failure is a PROVEN BAML parse error for that exact value
// target+input. A value native merely DECLINED (an ErrDeBAMLParseUnsupported that
// is NOT a proven error) may be a DEFERRED Mcoerce-d SUCCESS (e.g.
// map<string,string> with a NUMBER value → JsonToString), so native DECLINES THE
// WHOLE MAP there rather than skip an entry BAML would keep. A number-display-
// uncertain value likewise declines the whole map. (A KEY match is now proven —
// bamlunicode — so a non-ASCII key is native's to CLAIM, no longer a decline.)
//
// Native still DECLINES (ErrDeBAMLParseUnsupported → fall back) on:
//   - Non-object input: BAML has object/map coercion + scoring outside this
//     subset (it is not a hard type error).
//   - A DUPLICATE original input key: BAML's IndexMap insert/overwrite ordering
//     for duplicates is unproven, and a skipped duplicate must not be reasoned
//     safe, so ANY duplicate declines the whole map before emitting.
//
// Every object→map carries ObjectToMap (cost 1), so a map is NEVER a zero-score
// arm; its INHERENT score (types.rs, the model union/pick_best use) is own
// conditions plus each accepted entry's key conditions (empty, 0) plus value
// score. M3 scores a non-null map arm against the null arm (DefaultButHadValue
// 110) rather than the pre-M3 clean-only decline.
func coerceMap(b *schema.Bundle, keyT, valT *schema.Type, input value, cf *coerceFlags, cctx *coerceCtx) (json.RawMessage, error) {
	if keyT == nil || valT == nil {
		return nil, fmt.Errorf("debaml: map type missing key or value")
	}
	if input.kind != valObject {
		// Not a hard type error: BAML's object/map coercion + scoring can
		// still produce a (partial/flagged) map from a non-object, so decline
		// rather than claim a mismatch BAML would not produce.
		return nil, declineCoerce("map target", input)
	}
	// Any DUPLICATE original input key declines the WHOLE map up-front (BAML's
	// duplicate insert/overwrite ordering is unproven). Scanning ALL keys before
	// emitting means a duplicate whose entry would otherwise SKIP still declines
	// — native never reasons that a skipped duplicate leaves the rest safe.
	seen := make(map[string]struct{}, len(input.objV))
	for i := range input.objV {
		k := input.objV[i].key
		if _, dup := seen[k]; dup {
			return nil, unsupported(fmt.Sprintf("map duplicate key %q (BAML duplicate insert/overwrite ordering unproven)", k))
		}
		seen[k] = struct{}{}
	}

	// Every object→map carries ObjectToMap (cost 1); score it before iterating.
	cf.setKind(candMap)
	cf.add(1)

	var buf bytes.Buffer
	buf.WriteByte('{')
	first := true
	for i := range input.objV {
		f := &input.objV[i]
		// VALUE first (BAML coerce_map order): a proven value error skips the
		// entry WITHOUT coercing the key.
		child, keepVal, err := coerceMapValueChild(b, *valT, f.val, cf, cctx)
		if err != nil {
			return nil, err // hard/invariant error, or decline-the-whole-map.
		}
		if !keepVal {
			cf.add(1) // MapValueParseError (cost 1) → skip entry.
			continue
		}
		// KEY second: the original key string coerced against the key type. A key
		// that does not cleanly coerce DECLINES the whole map (the dynamic bridge
		// keeps non-matching keys leniently — a miss is Mcoerce-d, never a skip).
		if err := coerceMapKey(b, *keyT, f.key, cf); err != nil {
			return nil, err
		}
		// Both succeeded: insert the ORIGINAL input key string + coerced value.
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

// coerceMapValueChild coerces one map VALUE. Like coerceListChild it returns
// exactly one of:
//
//   - (out, true, nil):  the value coerced; insert the entry with out.
//   - (nil, false, nil): the value is a PROVEN BAML parse error; the caller SKIPS
//     the entry (BAML records MapValueParseError and the map still succeeds).
//   - (nil, false, err): the whole map must FAIL — a HARD/invariant error, or an
//     ErrDeBAMLParseUnsupported DECLINE because the value could be a DEFERRED
//     Mcoerce-d success or carried number-display uncertainty, so native falls
//     back rather than skip an entry BAML might keep.
//
// A map's INHERENT score (types.rs) is own conditions plus, per accepted entry,
// the key conditions (inserted EMPTY, score 0) plus the VALUE score — so a kept
// value's total score DOES fold into the map here (M3 drops the pre-M3
// "map own flags only" score.rs shortcut). The child's number-display uncertainty
// is still propagated.
func coerceMapValueChild(b *schema.Bundle, valT schema.Type, val value, cf *coerceFlags, cctx *coerceCtx) (json.RawMessage, bool, error) {
	childF := &coerceFlags{}
	out, err := coerce(b, valT, val, childF, cctx)
	if err == nil {
		cf.foldChild(childF) // map score += accepted value score (key conds are empty=0)
		return out, true, nil
	}
	if !declinableChildError(err) {
		return nil, false, err // hard/invariant failure: propagate, never mask.
	}
	if childF.isUncertain() {
		// A nested value carried number-display uncertainty native cannot reproduce
		// byte-exact (displayNumber) — it might coerce (BAML keeps) or not, so
		// DECLINE the whole map. (#555 Slice 2 removed the former case-fold cause.)
		cf.markUncertain()
		return nil, false, unsupported(fmt.Sprintf("map value %s→%s: number-display uncertainty (cannot prove skip vs keep)", val.kind, valT.Kind))
	}
	if provenMapValueError(b, valT, val) {
		return nil, false, nil // PROVEN parse error → BAML skips via MapValueParseError.
	}
	// A declinable error that is NOT a proven parse error: BAML may still SUCCEED
	// via a deferred Mcoerce-d path (JsonToString / ObjectToString /
	// ObjectToPrimitive / implied-key / default-fill / array-to-singular / union
	// scoring), so native cannot skip it — DECLINE the whole map (fall back).
	return nil, false, unsupported(fmt.Sprintf("map value %s→%s: child declined but not a proven BAML parse error (deferred Mcoerce-d success possible): %v", val.kind, valT.Kind, err))
}

// provenMapValueError reports whether BAML PROVABLY errors coercing a map VALUE
// child — the ONLY case native may SKIP the entry (MapValueParseError). A map
// value is coerced on the raw child with a fresh scope exactly like a list
// ELEMENT (both invoke the value type's coercer on the child, and both wrap a
// failure in a *ParseError flag and skip), so the proven-parse-error whitelist
// is IDENTICAL: it delegates to provenListItemError. See that function for the
// per-kind safe-skip vs must-decline split (numeric-string re-derivation via
// bamlStringNumberFails, error_unexpected_type kinds, certain match_string miss,
// multi-field all-required-flat-leaf class ← scalar; object/array/union/nested-
// collection values DEFER and decline the whole map instead of skipping).
func provenMapValueError(b *schema.Bundle, valT schema.Type, val value) bool {
	return provenListItemError(b, valT, val)
}

// coerceMapKey coerces one map KEY: the ORIGINAL input key STRING wrapped as a
// JSONish string and coerced against the key type (BAML coerce_map.rs:163). It
// returns nil on a clean ACCEPT (the entry is inserted under the ORIGINAL key
// string, never the canonical enum/literal form) and a DECLINE sentinel
// otherwise. checkSupportedMapKey has already restricted the key type to the
// four legal shapes (string primitive / enum / string literal / non-nullable
// union of string literals), so the default arm is defensive.
//
// Unlike a map VALUE, a KEY has NO native partial-skip path: a key that does not
// cleanly match DECLINES THE WHOLE MAP (Mcoerce-d), never a per-entry skip. The
// live-captured DYNAMIC behavior is that enum / string-literal / literal-union
// map keys are LENIENT — a NON-MATCHING key is ACCEPTED and inserted under its
// ORIGINAL string, so {"A":x,"C":z} over enum {A,B} (map_bad_enum_key) or over
// "A"|"B" (map_literal_key_partial_bad_key) yields the FULL map, not a partial
// one. Native cannot SKIP a missed key (BAML keeps it) nor reproduce that lenient
// keep (that is Mcoerce-d structural leniency), so it declines the whole map on
// ANY non-clean key. Per key type:
//
//   - string primitive: any key coerces → ACCEPT.
//   - enum / string literal / string-literal union: a clean match_string match
//     (any arm, for a union) → ACCEPT; a certain MISS declines the whole map
//     (dynamic lenient keep). The match_string fold is now proven (bamlunicode ==
//     Rust str::to_lowercase), so a non-ASCII key match/miss is native's to CLAIM
//     — there is no case-fold-uncertain branch (#555 Slice 2).
//
// cf is retained for the scored-context threading convention every coerce* helper
// follows (and so a future NON-Unicode key uncertainty could propagate to an
// enclosing union counter). Key coercion produces no uncertainty today, so cf is
// never marked here; no claimable union admits a map arm anyway (parse.go's safe
// families are literal/class only).
func coerceMapKey(b *schema.Bundle, keyT schema.Type, key string, cf *coerceFlags) error {
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
		// Only success/failure matters (BAML discards the coerced key, inserting
		// the original string), so ignore the substring bit.
		_, outcome, _ := matchString(key, enumMatchCandidates(e), true)
		if outcome == matchOne {
			return nil
		}
		// A missed enum key is a DEFERRED lenient keep (dynamic enum keys accept
		// non-members → full map), not a skip, so decline the whole map.
		return unsupported(fmt.Sprintf("map key %q: no clean enum %q match (dynamic keeps non-members → Mcoerce-d decline)", key, keyT.Name))
	case schema.TypeLiteral:
		if keyT.Literal == nil || keyT.Literal.Kind != schema.LiteralString {
			return unsupported("map key literal must be a string literal")
		}
		_, outcome, _ := matchString(key, []matchCandidate{{name: keyT.Literal.String, validValues: []string{keyT.Literal.String}}}, true)
		if outcome == matchOne {
			return nil
		}
		// A missed literal key is a DEFERRED lenient keep, not a skip → decline.
		return unsupported(fmt.Sprintf("map key %q: no clean literal %q match (dynamic keeps non-matches → Mcoerce-d decline)", key, keyT.Literal.String))
	case schema.TypeUnion:
		// Only a non-nullable union of string literals is a legal map key
		// (the same invariant the parse gate proves via isStringLiteralUnionType
		// / checkSupportedMapKey). flattenStringLiterals is intentionally lossy —
		// it silently drops any non-literal arm — so assert the invariant here
		// too, defensively, since coerceMapKey is unit/future-callable without the
		// gate: a mixed union must NOT be treated as its string-literal subset.
		if !isStringLiteralUnionType(keyT) {
			return unsupported("map key union must be a non-nullable union of string literals")
		}
		// A union-of-string-literals key coerces through BAML's union coercer,
		// which accepts when ANY arm matches; the map then inserts the ORIGINAL
		// key, so the winning arm is irrelevant. The fold is now proven
		// (bamlunicode == Rust str::to_lowercase), so a non-ASCII arm match
		// (e.g. key "É" vs literal "é", matched in the case-fold pass) is native's
		// to CLAIM. Scan the arms: the FIRST clean match accepts the key.
		for _, lit := range flattenStringLiterals(keyT) {
			if _, outcome, _ := matchString(key, []matchCandidate{{name: lit, validValues: []string{lit}}}, true); outcome == matchOne {
				return nil
			}
		}
		// No arm matched: a DEFERRED lenient keep (dynamic keeps non-matching
		// literal-union keys → full map), not a skip → decline the whole map.
		return unsupported(fmt.Sprintf("map key %q: matches no string-literal-union arm (dynamic keeps non-matches → Mcoerce-d decline)", key))
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

// candidate is one coerced union / array-to-singular arm ready for pickBest: its
// emitted output plus the inherent score and the special-ordering discriminators.
// originIndex is the arm's position in BAML's iter_include_null() order, so the
// index tiebreak reproduces BAML's res indices even though EXCLUDED error arms
// leave gaps in the candidate slice.
type candidate struct {
	output      json.RawMessage
	originIndex int
	kind        candKind
	score       int

	singleToArray     bool
	itemsEmpty        bool
	arrayItemErrors   int
	firstItemMarkdown bool

	hasJsonToString bool
	hasFirstMatch   bool
	hasUnionMatch   bool

	classPropCount     int
	classAllDefault    bool
	classSingleImplied bool
}

// isDefaultList mirrors pick_best's "default" bit: an EMPTY list produced by
// SingleToArray, which the generic order devalues behind a non-default value.
func (c candidate) isDefaultList() bool {
	return c.kind == candList && c.itemsEmpty && c.singleToArray
}

// nullCandidate is BAML's null arm for NON-null input: the value coerces to null
// with DefaultButHadValue (score 110). iter_include_null() appends it LAST, so
// originIndex is the count of preceding non-null variants.
func nullCandidate(originIndex int) candidate {
	return candidate{output: json.RawMessage("null"), originIndex: originIndex, kind: candNull, score: 110}
}

// pickBest ports array_helper::pick_best's SELECTION (array_helper.rs) over the
// Ok candidates and returns the winning candidate's index. It does NOT re-add a
// UnionMatch/FirstMatch flag — native emits the winner's raw output directly
// (those flags carry score 0), and coerceUnionSafe folds the winner's score into
// the caller. targetIsUnion enables the class-vs-union ImpliedKey devalue rule.
//
// It computes the winner by pairwise MINIMUM SCAN rather than sort.SliceStable,
// then VERIFIES the winner ranks <= every other candidate. cmpCandidates mirrors
// BAML's pairwise sort_by closure, whose conditional special cases (list /
// class / scalar-vs-composite) make it INTRANSITIVE on some mixed shapes — e.g.
// an ordinary class (score 111), an all-default class (score 100), and a null
// (score 110) form a cmp cycle. On such a set no candidate is a consistent
// global minimum: Go's sort would be undefined, and BAML's own sort_by result is
// an implementation artifact native cannot reliably reproduce, so native DECLINES
// (fall back to BAML). For every shape M3a actually claims (literal/scalar/null
// and flat-class-union/null, where classAllDefault / classSingleImplied are
// unreachable and no list arms appear) the comparator reduces to (score, index),
// a total order — the verification always passes and the winner is exact.
func pickBest(targetIsUnion bool, cands []candidate) (int, error) {
	if len(cands) == 0 {
		return -1, unsupported("pick_best: empty candidate set")
	}
	if len(cands) == 1 {
		return 0, nil
	}
	// Pairwise minimum scan (well-defined even for an intransitive comparator).
	best := 0
	for i := 1; i < len(cands); i++ {
		if cmpCandidates(targetIsUnion, cands[i], cands[best]) < 0 {
			best = i
		}
	}
	// Verify best is a CONSISTENT global minimum (ranks <= every other candidate).
	// If not, the comparator cycles on this set and there is no well-defined
	// winner native can prove equals BAML's — DECLINE (fall back).
	for i := range cands {
		if i == best {
			continue
		}
		if cmpCandidates(targetIsUnion, cands[best], cands[i]) > 0 {
			return -1, unsupported("pick_best: candidate comparator is not a total order on this set (BAML sort_by is intransitive here) — declining")
		}
	}
	return best, nil
}

// cmpCandidates returns <0 if a should rank before b, >0 after, 0 equal —
// a faithful translation of pick_best's sort_by closure (array_helper.rs). Its
// conditional special cases (list / class / scalar-vs-composite) are pairwise and
// can be INTRANSITIVE on mixed sets (as BAML's own closure is), so pickBest never
// feeds it to a sort; it uses a minimum scan plus a total-order verification and
// declines when the relation cycles. The generic (default, score, index) tail is
// a total order (the index tiebreak breaks all ties).
func cmpCandidates(targetIsUnion bool, a, b candidate) int {
	// Two list candidates (array_helper.rs:74).
	if a.kind == candList && b.kind == candList {
		switch {
		case a.singleToArray && !b.singleToArray:
			return 1 // prefer the non-SingleToArray list (b)
		case !a.singleToArray && b.singleToArray:
			return -1 // prefer a
		default:
			// Same SingleToArray-ness: prefer content NOT from markdown.
			switch {
			case a.firstItemMarkdown && !b.firstItemMarkdown:
				return 1
			case !a.firstItemMarkdown && b.firstItemMarkdown:
				return -1
			}
			// A list empty ONLY because of ArrayItemParseError loses to a
			// no-parse-error list.
			ua, ub := a.arrayItemErrors, b.arrayItemErrors
			switch {
			case ua == 0 && ub > 0 && b.itemsEmpty:
				return -1 // prefer a (no errors) over the empty-error b
			case ua > 0 && ub == 0 && a.itemsEmpty:
				return 1 // prefer b
			}
		}
	}

	// Two class candidates (array_helper.rs:150).
	if a.kind == candClass && b.kind == candClass {
		if targetIsUnion {
			// Devalue a class whose only property is a string with ImpliedKey.
			switch {
			case a.classSingleImplied && !b.classSingleImplied:
				return 1
			case !a.classSingleImplied && b.classSingleImplied:
				return -1
			}
		}
		// Devalue an all-default class behind a non-default one.
		switch {
		case a.classAllDefault && !b.classAllDefault:
			return 1
		case !a.classAllDefault && b.classAllDefault:
			return -1
		}
	}

	// Devalue a non-composite scalar cast from JSON (JsonToString) or picked by
	// array-to-singular (FirstMatch) against a composite (array_helper.rs:214).
	if !a.kind.isComposite() && b.kind.isComposite() && (a.hasJsonToString || a.hasFirstMatch) {
		return 1
	}
	if a.kind.isComposite() && !b.kind.isComposite() && (b.hasJsonToString || b.hasFirstMatch) {
		return -1
	}

	// Generic order: (default, score, index) — non-default before default, then
	// lower score, then lower original index (array_helper.rs:237).
	if a.isDefaultList() != b.isDefaultList() {
		if !a.isDefaultList() {
			return -1
		}
		return 1
	}
	if a.score != b.score {
		if a.score < b.score {
			return -1
		}
		return 1
	}
	if a.originIndex != b.originIndex {
		if a.originIndex < b.originIndex {
			return -1
		}
		return 1
	}
	return 0
}

// coerceArrayToSingular ports array_helper::coerce_array_to_singular
// (array_helper.rs): a NON-array target that receives an ARRAY input maps every
// item through coerceItem, runs array_helper::pick_best over the successful
// items, and adds FirstMatch scoring. It is BAML's "first/best array item wins"
// path, used by the primitive int/float/bool coercers (coerce_primitive.rs) and,
// via the union arms, by any int/float/bool arm on array input.
//
// FirstMatch scoring reproduces BAML exactly: pick_best (when res.len() > 1)
// adds FirstMatch(winnerIdx) to the winner UNLESS it already carries a
// UnionMatch/FirstMatch (score 1), and coerce_array_to_singular then ALWAYS adds
// FirstMatch(0) on success (score 1). So a >1-item array-to-singular winner
// scores its own score + 2, a 1-item winner + 1 — the extra score-bearing flags
// that devalue the scalar against a composite arm in an outer union (pick_best
// scalar-vs-composite rule) and matter against a nullable null arm.
//
// Over-claim guards mirror the list/union collection classifiers:
//   - empty array → BAML pick_best error_unexpected_empty_array, a PROVABLE
//     error (provenError): excluded in a union arm, propagated as a claimed
//     error at a standalone position.
//   - a PROVEN BAML item error is excluded from ranking (i32::MAX), safe.
//   - an item native merely DECLINED (native stricter — could be a DEFERRED
//     BAML success that changes the pick_best winner) or a number-display-UNCERTAIN
//     item DECLINES the whole array-to-singular (fall back).
//   - every item a proven error (and the array non-empty) → BAML merges the
//     errors and errors (provenError).
//
// targetIsUnion enables pick_best's class-vs-union ImpliedKey devalue for a
// class array-to-singular over union arms (unused by the scalar callers, which
// pass false), and selects the UnionMatch-vs-FirstMatch flag for the winner.
//
// itemProvenErr classifies an item that native's coercer DECLINED (a plain
// unsupported, not already a provenErr) as a PROVEN BAML error for that item
// target+input (e.g. a non-numeric string into an int coercer — bamlStringNumberFails),
// so it is EXCLUDED from ranking like BAML's error item rather than declining the
// whole array-to-singular. It mirrors provenListItemError's role for list items;
// a nil predicate means "no item decline is proven" (every non-proven decline
// falls back).
func coerceArrayToSingular(items []value, targetIsUnion bool, coerceItem func(item value) (json.RawMessage, *coerceFlags, error), itemProvenErr func(item value) bool) (candidate, error) {
	n := len(items)
	if n == 0 {
		// pick_best on an empty res → error_unexpected_empty_array (deterministic
		// BAML error). Claim the error / exclude the arm rather than fall back.
		return candidate{}, provenError("array-to-singular: empty array (BAML error_unexpected_empty_array)")
	}
	var cands []candidate
	for i := 0; i < n; i++ {
		out, cf, err := coerceItem(items[i])
		if cf.isUncertain() {
			// The item verdict hinged on number-display native cannot reproduce
			// byte-exact; BAML might keep or drop it, changing the winner → decline
			// the whole thing. (#555 Slice 2 removed the former case-fold cause.)
			return candidate{}, unsupported("array-to-singular item: number-display uncertainty (cannot prove verdict equals BAML)")
		}
		if err == nil {
			cands = append(cands, cf.toCandidate(out, i))
			continue
		}
		if !declinableChildError(err) {
			return candidate{}, err // hard/invariant failure: propagate, never mask.
		}
		if isProvenBamlError(err) || (itemProvenErr != nil && itemProvenErr(items[i])) {
			continue // provable BAML item error: excluded from ranking (i32::MAX).
		}
		// A native-can't-prove decline: BAML may still succeed this item via a
		// deferred path and change the pick_best winner, so native cannot exclude
		// it — DECLINE the whole array-to-singular (fall back).
		return candidate{}, unsupported("array-to-singular item: unsupported/indeterminate (BAML may succeed via a deferred path)")
	}
	if len(cands) == 0 {
		// Non-empty array where every item is a PROVEN BAML error → pick_best has
		// no Ok candidate and BAML error_merge_multiple errors (a claimed error).
		return candidate{}, provenError("array-to-singular: every item provably fails (BAML error_merge_multiple)")
	}
	idx, err := pickBest(targetIsUnion, cands)
	if err != nil {
		return candidate{}, err
	}
	w := cands[idx]
	// pick_best (res.len() > 1) adds a flag to the winner when it lacks both
	// UnionMatch and FirstMatch — UnionMatch (score 0) when the target is a UNION
	// (a union arm's array-to-singular), else FirstMatch (score 1). This is why an
	// `int` union arm on [1,2] scores 1 (UnionMatch+FirstMatch) and can beat a
	// `string` arm's JsonToString (2).
	if n > 1 && !(w.hasUnionMatch || w.hasFirstMatch) {
		if targetIsUnion {
			w.hasUnionMatch = true // UnionMatch (score 0)
		} else {
			w.score++ // FirstMatch (score 1)
			w.hasFirstMatch = true
		}
	}
	// coerce_array_to_singular then ALWAYS adds FirstMatch(0) on success.
	w.score++
	w.hasFirstMatch = true
	return w, nil
}

// unionArm coerces variant i of a union into a FRESH accumulator, returning its
// emitted output, that accumulator (for the score + discriminators), and an error.
type unionArm func(i int) (json.RawMessage, *coerceFlags, error)

// selectUnionArms is the shared scored-selection core for the safe union
// families (coerce_union.rs standard path). It coerces narms arms in
// iter_include_null() order, categorizes each, applies BAML's early
// first-score-0 winner rule, and otherwise runs pickBest over the successful
// candidates (plus the null arm when nullable). Categorization:
//
//   - SUCCESS: a candidate; a score-0 success is the immediate winner (BAML
//     returns it without trying later arms).
//   - PROVEN BAML error (isProvenBamlError): excluded from ranking (i32::MAX) —
//     a real BAML arm failure, safe to skip.
//   - native-can't-prove / uncertain: DECLINE THE WHOLE UNION. Native cannot
//     prove BAML also fails this arm; BAML might succeed it through a deferred
//     path and change the selection, so excluding it would risk an over-claim.
//   - hard/invariant error: propagate.
//
// A non-nullable union with no successful arm declines (BAML merges the errors
// and errors; native falls back — a safe under-claim).
func selectUnionArms(narms int, nullable bool, arm unionArm) (candidate, error) {
	var cands []candidate
	for i := 0; i < narms; i++ {
		out, armF, err := arm(i)
		if armF.isUncertain() {
			// #555 Slice 2 removed the former case-fold cause; only number-display
			// uncertainty reaches here now (still declines the whole union).
			return candidate{}, unsupported("union arm: number-display uncertainty (cannot prove verdict equals BAML)")
		}
		if err == nil {
			c := armF.toCandidate(out, i)
			if c.score == 0 {
				// BAML's early first-winner (coerce_union.rs standard path): the
				// FIRST arm that coerces with score()==0 is returned immediately,
				// BEFORE later arms (or the null arm) are coerced and BEFORE
				// array_helper::pick_best runs. This branch INTENTIONALLY bypasses
				// cmpCandidates/pickBest — a score-0 arm is BAML's winner regardless
				// of any special ordering. It is parity-correct today because every
				// pick_best discriminator that is REACHABLE in M3a's safe families is
				// score-bearing when present (SingleToArray, JsonToString, ImpliedKey,
				// default fills, ArrayItemParseError, ExtraKey), so a score-0 arm
				// carries none of them, and the unreachable ones (FirstMatch,
				// ObjectFromMarkdown, list-vs-list, all-default flat-class) never
				// arise. If future work adds a score-0-capable discriminator or
				// changes any flag weight, RE-AUDIT this branch against BAML's
				// coerce_union before routing score-0 arms through pickBest.
				return c, nil
			}
			cands = append(cands, c)
			continue
		}
		if isProvenBamlError(err) {
			continue // BAML provably errors this arm: excluded from ranking.
		}
		if declinableChildError(err) {
			return candidate{}, unsupported("union arm: unsupported/indeterminate (BAML may succeed via a deferred path)")
		}
		return candidate{}, err // hard/invariant failure: propagate, never mask.
	}
	if nullable {
		cands = append(cands, nullCandidate(narms))
	}
	if len(cands) == 0 {
		return candidate{}, unsupported("union: no arm succeeded (BAML errors)")
	}
	idx, err := pickBest(true, cands)
	if err != nil {
		return candidate{}, err
	}
	return cands[idx], nil
}

// setWinnerCf folds the union WINNER into the enclosing accumulator. The union
// result IS the winner value plus UnionMatch (score 0), so the caller sees the
// winner's score and top-level discriminators (needed only when this union is
// itself a field/element of an outer scored context). Nil-safe.
func setWinnerCf(cf *coerceFlags, w candidate) {
	if cf == nil {
		return
	}
	cf.absorb(w) // score (UnionMatch adds 0) + kind + discriminators
	cf.hasUnionMatch = true
}

// coerceUnionSafe coerces a value against a union, claiming native JSON ONLY
// where it can PROVE BAML's scored selection. M3 replaces the pre-M3 clean-only
// rule with a faithful port of the BAML SCORE MODEL: each arm is coerced and
// scored (types.rs inherent BamlValueWithFlags::score()), the first score-0 arm
// wins immediately, and otherwise array_helper::pick_best chooses the lowest
// (special-ordering, score, index). The claim stays inside the supported union
// families proven by checkSupportedUnionShape:
//
//  1. JSON-null input + nullable union → null immediately (BAML's null fast path).
//  2. nullable single-non-null union (the optional shape) → the lone arm is
//     scored against the NULL arm (DefaultButHadValue, score 110): the arm wins
//     when its score < 110 (e.g. optional int with 1.6 → 2, FloatToInt score 1),
//     null wins when the arm scores > 110 (e.g. an extra-key-heavy class), and a
//     score-110 tie goes to the arm (lower index).
//  3. non-null input + a multi-variant SUPPORTED FAMILY — a scalar/literal/enum
//     leaf union (M3b) or a flat disjoint-key class union — scored via
//     selectUnionArms, including two lenient successes (BAML pick_best) and, when
//     nullable, the null arm.
//
// Over-claim guards: any arm native cannot prove (an ErrDeBAMLParseUnsupported
// that is not a proven BAML error) or any uncertain arm DECLINES THE WHOLE UNION;
// there is no array-to-singular here (an int/float/bool arm declines ARRAY input,
// declining the union). cf carries the winner's score/kind up to an outer scored
// context (a union that is itself a class field / list element).
func coerceUnionSafe(b *schema.Bundle, u *schema.UnionType, input value, cf *coerceFlags, cctx *coerceCtx) (json.RawMessage, error) {
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
	// simplifyUnion). Score the lone arm against the null arm.
	if len(u.Variants) == 1 {
		// Re-prove the lone non-null arm structurally before claiming NON-NULL
		// input. The gate's nullable fast path (checkSupportedType's
		// `if u.Nullable { return nil }`) accepts the schema WITHOUT walking this
		// arm, so a value tree that only checkSupportedType declines — not
		// coerce — would otherwise slip through here. The canonical case is a
		// direct list<multi-arm-union> under an OPTIONAL map value: its
		// isMultiArmUnion guard lives only in checkSupportedType's TypeList arm
		// (the array union_variant_hint decline) and has NO coerce-time
		// equivalent (coerceList/coerceMapValueChild would happily coerce it),
		// so an optional map<string, list<int|string>> would over-claim. This is
		// the SAME structural guard a non-optional field gets at the gate, so a
		// supported arm (optional map<string,string>, optional int, an optional
		// class) still passes and claims; a JSON-null input already returned via
		// case 1, so this reproof only gates the NON-NULL claim.
		if err := checkSupportedType(b, u.Variants[0]); err != nil {
			return nil, err
		}
		w, err := selectUnionArms(1, true, func(i int) (json.RawMessage, *coerceFlags, error) {
			armF := &coerceFlags{targetIsUnion: true}
			out, e := coerce(b, u.Variants[0], input, armF, cctx)
			return out, armF, e
		})
		if err != nil {
			return nil, err
		}
		setWinnerCf(cf, w)
		return w.output, nil
	}
	// Case 3: non-null input against a multi-variant union. Re-prove the
	// non-null arm set is a safe family (this also rejects non-null input to a
	// nullable-but-unsafe union, which the gate permitted only for case 1).
	if err := checkSupportedUnionShape(b, u); err != nil {
		return nil, err
	}
	return coerceUnionSafeMulti(b, u.Variants, input, u.Nullable, cf, cctx)
}

// coerceUnionSafeMulti resolves a proven-safe multi-variant union (a scalar/
// literal/enum leaf family, a required-flat-leaf CLASS family, or any MIX of them —
// checkSupportedUnionShape) reproducing BAML's TWO-PHASE union coercion exactly:
//
//  1. try_cast pass (tryCastUnion): field_type.rs runs self.try_cast BEFORE the
//     lenient coerce for EVERY value ("try_cast is basically a way to exit early");
//     for a union that is try_cast_union over iter_skip_null (the non-null arms in
//     order). The FIRST arm whose STRICT native-type cast succeeds is BAML's
//     immediate winner at score 0 — a scalar/literal/enum by exact native-JSON-type
//     match (tryCastArm), a CLASS by an exact-key object match whose every required
//     flat-leaf field try_casts (tryCastClass). No numeric parse, stringify, fuzzy
//     match, single-key extraction, class implied-key, or default happens here. This
//     is why `int | string` keeps a numeric STRING as a string, a JSON number picks
//     the int/float arm regardless of order, and a class whose full field set is
//     present (fixtures 35/39/42) returns immediately regardless of the other arms.
//  2. lenient coerce pass (only when NO arm try_casts): each arm is coerced through
//     coerce(), which dispatches to the SAME per-kind coercer BAML uses
//     (coercePrimitive / coerceLiteral / coerceEnum / coerceClass), so a lenient
//     conversion (numeric-string parse, float→int round, string→bool, stringify+
//     match, substring/fuzzy hit, single-key extraction, class implied-key /
//     inferred-object / extra-key / default) scores exactly as BAML would.
//     selectUnionArms applies the early first-score-0 rule and otherwise runs
//     array_helper::pick_best over the successes (plus the null arm, score 110,
//     when nullable) — whose list/class/scalar-vs-composite special ordering native
//     reproduces in cmpCandidates.
//
// In the lenient pass a non-matching / value-mismatched / missing-required arm is a
// PROVEN BAML error (excluded from ranking); an arm native's stricter coercer merely
// DECLINED (native-can't-prove — a non-numeric string to an int arm, or ARRAY input
// to an int/float/bool/class arm whose array-to-singular is M3d) or a number-display-
// UNCERTAIN arm declines the WHOLE union, because BAML might succeed it via a
// deferred path and change the winner. The winner's own output is emitted directly
// (in the winner's schema field order for a class).
//
// It SUBSUMES the pre-M3c per-family paths: a scalar/literal/enum-only variant set
// behaves exactly as the M3b scalar-leaf coercer (phase-1 leaf try_cast, phase-2
// per-kind scoring), and an all-class or mixed set adds the class try_cast (phase 1)
// and the class pick_best special ordering (phase 2). The old disjoint-key /
// >=2-field class gate is gone: overlapping and single-field class arms are resolved
// by pick_best rather than declined.
func coerceUnionSafeMulti(b *schema.Bundle, variants []schema.Type, input value, nullable bool, cf *coerceFlags, cctx *coerceCtx) (json.RawMessage, error) {
	// Phase 1: try_cast pass (coerce_union.rs try_cast_union, run by field_type.rs
	// BEFORE the lenient coerce). The FIRST non-null arm whose STRICT native-type
	// cast succeeds is BAML's winner at score 0.
	if w, matched, err := tryCastUnion(b, variants, input, cctx); err != nil {
		return nil, err
	} else if matched {
		setWinnerCf(cf, w)
		return w.output, nil
	}
	// Phase 2: lenient coerce pass (coerce_union.rs standard path) — early
	// first-score-0, else array_helper::pick_best, with the null arm (score 110)
	// when nullable.
	w, err := selectUnionArms(len(variants), nullable, func(i int) (json.RawMessage, *coerceFlags, error) {
		armF := &coerceFlags{targetIsUnion: true}
		out, e := coerce(b, variants[i], input, armF, cctx)
		return out, armF, e
	})
	if err != nil {
		return nil, err
	}
	setWinnerCf(cf, w)
	return w.output, nil
}

// tryCastUnion ports BAML's try_cast_union (coerce_union.rs) for the supported
// union families. field_type.rs runs self.try_cast BEFORE the lenient coerce for
// EVERY value; for a union that is try_cast_union over iter_skip_null (the non-null
// arms in order). Each arm's try_cast (tryCastArm — a scalar/literal/enum by strict
// native-JSON-type match, a CLASS by tryCastClass, a MAP by tryCastMap) succeeds
// ONLY when the input's native shape already matches. It reproduces BAML's two
// exits:
//
//   - the FIRST arm whose try_cast scores 0 (every leaf/class arm is 0-or-no-match;
//     a clean scalar/literal/enum/class) is BAML's IMMEDIATE winner (UnionMatch),
//     returned before later arms and before the lenient pass; and
//   - otherwise, EVERY arm whose try_cast succeeds with a NON-zero score (only a MAP
//     arm — ObjectToMap is score 1) is collected with its UnionMatch pre-added, and
//     array_helper::pick_best chooses among them. This sub-path is why a `Class |
//     map` object with an extra key resolves to the MAP (the class try_cast rejects
//     the extra key, but the map try_cast keeps it): without it, native's lenient
//     pass would wrongly prefer the lower-index class (ExtraKey, also score 1).
//
// The null arm is excluded here (the JSON-null input fast path is in
// coerceUnionSafe; a non-null input never try_casts to null).
//
// Returns (winner, true, nil) on a hit (score-0 immediate OR non-zero pick_best);
// (_, false, nil) when NO arm try_casts (caller runs the lenient pass); (_, false,
// err) only for a value native cannot emit (a JSON number that parses to a
// non-finite float) or a hard/invariant failure (unknown class/enum / malformed arm
// type), which declines/propagates.
func tryCastUnion(b *schema.Bundle, variants []schema.Type, input value, cctx *coerceCtx) (candidate, bool, error) {
	var filtered []candidate
	for i := range variants {
		c, matched, err := tryCastArm(b, variants[i], input, cctx)
		if err != nil {
			return candidate{}, false, err
		}
		if !matched {
			continue
		}
		c.originIndex = i
		c.hasUnionMatch = true
		if c.score == 0 {
			// BAML's first-score-0 fast path: return immediately, before later arms.
			return c, true, nil
		}
		// Non-zero try_cast (a map ObjectToMap): collected for pick_best.
		filtered = append(filtered, c)
	}
	if len(filtered) == 0 {
		return candidate{}, false, nil // no arm try_casts → lenient pass.
	}
	idx, err := pickBest(true, filtered)
	if err != nil {
		return candidate{}, false, err
	}
	return filtered[idx], true, nil
}

// tryCastArm ports the per-kind try_cast (field_type.rs::try_cast dispatch): a
// STRICT native-type match. It returns a candidate (output, kind, score, and the
// pick_best discriminators) and whether the arm matched. Every leaf/class arm
// scores 0 (no score-bearing flag); a MAP arm scores its ObjectToMap (1) plus any
// nested value try_cast scores.
//
//   - string:  a JSON string (verbatim).
//   - int:     a JSON number whose token is an i64-range integer (as_i64 Some) —
//     a float-valued or u64-overflow number does NOT try_cast (it falls to the
//     lenient FloatToInt / u64-wrap coerce pass).
//   - float:   any JSON number (as_f64); a non-finite result (huge exponent) has
//     no JSON spelling, so the arm declines the whole union.
//   - bool:    a JSON boolean.
//   - literal: a JSON value whose native type EQUALS the literal (string by exact
//     ==, int by as_i64 ==, bool by ==) — no stringify/parse/fuzzy.
//   - enum:    a JSON string EXACTLY equal to a value's rendered name (no
//     description forms, no case fold, no substring); emits the canonical name.
//   - class:   an OBJECT whose keys EXACTLY match a subset of the fields with NO
//     extras and every required flat-leaf field try_casting (tryCastClass).
//   - map:     an OBJECT whose every value try_casts (tryCastMap), score 1.
//   - list:    an ARRAY whose every element try_casts (tryCastArray), score 0 (the
//     types.rs list score is the sum of element scores).
//   - union:   the union's try_cast (tryCastUnionArm) — a JSON null against a
//     nullable union, else the first arm whose try_cast wins (recursive). Reached
//     ONLY when a union is a LIST ELEMENT (single-non-null-arm optional) or a MAP
//     VALUE (any supported union), never as a direct union arm (nested unions are
//     flattened by simplifyUnion).
//
// LIST and UNION dispatch are ESSENTIAL, not optional: BAML's TypeIR::try_cast
// dispatches List→try_cast_array and Union→try_cast_union (field_type.rs), and a
// list/union arm can try_cast at score 0 while its LENIENT coerce ALSO scores 0
// without them being the SAME winner — e.g. `list<int> | list<string>` on ["1"]:
// list<int> try_cast FAILS (int try_cast rejects the string "1") but list<int>
// LENIENT-scores-0 (coerce_int parses "1"→1 with no flag), so a hint-less lenient
// pass wrongly early-returns [1] while BAML's try_cast picks list<string>→["1"].
// Skipping list/union try_cast here is exactly that over-claim. Every other kind
// (recursive alias / media / tuple) never appears as a supported union arm.
func tryCastArm(b *schema.Bundle, t schema.Type, input value, cctx *coerceCtx) (candidate, bool, error) {
	switch t.Kind {
	case schema.TypeClass:
		out, kind, matched, err := tryCastClass(b, t.Name, t.Mode, input, cctx)
		if err != nil || !matched {
			return candidate{}, matched, err
		}
		return candidate{output: out, kind: kind, score: 0}, true, nil
	case schema.TypeMap:
		return tryCastMap(b, t.Key, t.Value, input, cctx)
	case schema.TypeList:
		return tryCastArray(b, t.Elem, input, cctx)
	case schema.TypeUnion:
		return tryCastUnionArm(b, t.Union, input, cctx)
	case schema.TypePrimitive:
		switch t.Primitive {
		case schema.PrimitiveString:
			if input.kind == valString {
				out, err := marshalJSON(input.strV)
				return candidate{output: out, kind: candScalar, score: 0}, err == nil, err
			}
		case schema.PrimitiveInt:
			if input.kind == valNumber {
				if v, ok := parseI64Rust(input.numV.String()); ok {
					return candidate{output: json.RawMessage(strconv.FormatInt(v, 10)), kind: candScalar, score: 0}, true, nil
				}
			}
		case schema.PrimitiveFloat:
			if input.kind == valNumber {
				if v, ok := parseF64Rust(input.numV.String()); ok {
					out, fin := floatRaw(v)
					if !fin {
						return candidate{}, false, unsupported("union try_cast: JSON number parses to a non-finite float (no JSON spelling)")
					}
					return candidate{output: out, kind: candScalar, score: 0}, true, nil
				}
			}
		case schema.PrimitiveBool:
			if input.kind == valBool {
				return candidate{output: boolRaw(input.boolV), kind: candScalar, score: 0}, true, nil
			}
		}
	case schema.TypeLiteral:
		if t.Literal == nil {
			return candidate{}, false, fmt.Errorf("debaml: literal type missing value")
		}
		switch t.Literal.Kind {
		case schema.LiteralString:
			if input.kind == valString && input.strV == t.Literal.String {
				out, err := marshalJSON(input.strV)
				return candidate{output: out, kind: candScalar, score: 0}, err == nil, err
			}
		case schema.LiteralInt:
			if input.kind == valNumber {
				if v, ok := parseI64Rust(input.numV.String()); ok && v == t.Literal.Int {
					return candidate{output: json.RawMessage(strconv.FormatInt(v, 10)), kind: candScalar, score: 0}, true, nil
				}
			}
		case schema.LiteralBool:
			if input.kind == valBool && input.boolV == t.Literal.Bool {
				return candidate{output: boolRaw(input.boolV), kind: candScalar, score: 0}, true, nil
			}
		}
	case schema.TypeEnum:
		if input.kind == valString {
			e, ok := b.FindEnum(t.Name)
			if !ok {
				return candidate{}, false, fmt.Errorf("debaml: unknown enum %q", t.Name)
			}
			for i := range e.Values {
				if e.Values[i].Name.RenderedName() == input.strV {
					out, err := marshalJSON(e.Values[i].Name.Name)
					return candidate{output: out, kind: candEnum, score: 0}, err == nil, err
				}
			}
		}
	}
	return candidate{}, false, nil
}

// tryCastMap ports try_cast_map (coerce_map.rs): a JSON OBJECT whose every VALUE
// try_casts (recursively, fail-fast on the first that does not) becomes a map
// carrying ObjectToMap (score 1) plus any nested value try_cast scores. Unlike a
// leaf/class arm this is a NON-zero try_cast, so it does NOT short-circuit the union
// at score 0 — tryCastUnion collects it and pick_best chooses. try_cast_map does NOT
// validate keys (the union gate restricts map arms to STRING keys, where every key
// is valid, so this is faithful); a non-object input, or any value that does not
// try_cast, returns not-matched (the arm falls to the lenient partial-map coerce).
// A DUPLICATE input key returns not-matched (BAML's IndexMap overwrite order is
// unproven — coerceMap declines duplicates too), so the arm falls to the lenient
// pass, which also declines the whole map, declining the union (safe under-claim).
// Entries are emitted in INPUT key order under their ORIGINAL key strings.
func tryCastMap(b *schema.Bundle, keyT, valT *schema.Type, input value, cctx *coerceCtx) (candidate, bool, error) {
	if keyT == nil || valT == nil {
		return candidate{}, false, fmt.Errorf("debaml: map type missing key or value")
	}
	if input.kind != valObject {
		return candidate{}, false, nil
	}
	seen := make(map[string]struct{}, len(input.objV))
	for i := range input.objV {
		if _, dup := seen[input.objV[i].key]; dup {
			return candidate{}, false, nil // duplicate key: fall to lenient (declines).
		}
		seen[input.objV[i].key] = struct{}{}
	}
	score := 1 // ObjectToMap
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i := range input.objV {
		e := &input.objV[i]
		vc, matched, err := tryCastArm(b, *valT, e.val, cctx)
		if err != nil {
			return candidate{}, false, err
		}
		if !matched {
			return candidate{}, false, nil // fail-fast → lenient partial-map coerce.
		}
		score += vc.score // nested value try_cast score (e.g. a nested map's ObjectToMap)
		if i > 0 {
			buf.WriteByte(',')
		}
		key, err := marshalJSON(e.key)
		if err != nil {
			return candidate{}, false, err
		}
		buf.Write(key)
		buf.WriteByte(':')
		buf.Write(vc.output)
	}
	buf.WriteByte('}')
	return candidate{output: buf.Bytes(), kind: candMap, score: score}, true, nil
}

// tryCastArray ports try_cast_array (coerce_array.rs): a JSON ARRAY whose EVERY
// element strict-try-casts (fail-fast on the first that does not) becomes a list
// with NO own flags, so its score is the SUM of element try_cast scores (0 for a
// clean array; nonzero only when an element is itself a map/nested-list — e.g. a
// list<map<...>> element's ObjectToMap). An empty array try_casts to the empty
// list (score 0). A non-array input, or any element that does not try_cast,
// returns not-matched (the arm falls to the lenient coerceList pass).
//
// The gate DECLINES a list whose element is a MULTI-ARM union (checkSupportedType),
// so tryCastArray never sees the array union_variant_hint's per-element cross-hint:
// its admitted element shapes (scalar / literal / enum / class / map / nested list /
// single-non-null-arm optional) each try_cast independently, and BAML's hint is a
// no-op for them (a non-union element carries no UnionMatch; a single-arm-optional's
// only hint is its lone arm), so ignoring the hint here is faithful.
func tryCastArray(b *schema.Bundle, elem *schema.Type, input value, cctx *coerceCtx) (candidate, bool, error) {
	if elem == nil {
		return candidate{}, false, fmt.Errorf("debaml: list type missing element")
	}
	if input.kind != valArray {
		return candidate{}, false, nil
	}
	score := 0
	var buf bytes.Buffer
	buf.WriteByte('[')
	for i := range input.arrV {
		ec, matched, err := tryCastArm(b, *elem, input.arrV[i], cctx)
		if err != nil {
			return candidate{}, false, err
		}
		if !matched {
			return candidate{}, false, nil // fail-fast → lenient coerceList.
		}
		score += ec.score // types.rs list score = own (0) + sum(element scores)
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.Write(ec.output)
	}
	buf.WriteByte(']')
	return candidate{output: buf.Bytes(), kind: candList, score: score, itemsEmpty: len(input.arrV) == 0}, true, nil
}

// tryCastUnionArm ports try_cast_union (coerce_union.rs) for a union that is a
// nested value/element (a MAP value or a single-non-null-arm optional LIST element)
// — never a direct union arm (nested unions are flattened by simplifyUnion). It
// reproduces try_cast_union's two exits: a JSON null against a NULLABLE union
// try_casts to null (score 0), and otherwise the non-null arms are tried in order
// via tryCastUnion (first score-0 wins; a non-zero map arm is pick_best'd). A
// non-null input to a union with no try_casting arm returns not-matched (the caller
// — tryCastMap / tryCastArray — fails fast to the lenient pass).
func tryCastUnionArm(b *schema.Bundle, u *schema.UnionType, input value, cctx *coerceCtx) (candidate, bool, error) {
	if u == nil {
		return candidate{}, false, fmt.Errorf("debaml: union type missing payload")
	}
	if input.kind == valNull {
		if u.Nullable {
			// try_cast_union's null fast path: a Null result with empty conditions (score 0).
			return candidate{output: json.RawMessage("null"), kind: candNull, score: 0, hasUnionMatch: true}, true, nil
		}
		return candidate{}, false, nil
	}
	return tryCastUnion(b, u.Variants, input, cctx)
}

// tryCastClass ports Class::try_cast (ir_ref/coerce_class.rs) — the STRICT phase-1
// class coercion. It matches ONLY a JSON OBJECT whose keys EXACTLY equal (by
// rendered name — a case-sensitive BamlMap key lookup, NOT the lenient fuzzy
// matches_string_to_string coerce uses) a subset of the class fields with NO extra
// key, where every present field's value try_casts (recursively, via tryCastArm)
// and every field is present. The M3c gate (checkUnionClassVariant) guarantees all
// fields are REQUIRED flat leaves, so there are no optional fills and a class
// try_cast that matches always scores 0 (own conditions empty, every field try_cast
// score 0) — hence it is BAML's immediate union winner, and no non-zero try_cast
// score arises for the caller to pick_best over.
//
// Returns (out, candClass, true, nil) on a strict match (emitted in SCHEMA field
// order under canonical field names, byte-identical to the class's clean lenient
// coerce output); (nil, candClass, false, nil) when the object does not strictly
// match (extra key, missing field, or a field that does not try_cast — the caller
// falls to the lenient phase, or the next arm); (nil, _, false, err) only for a
// hard/invariant failure (unknown class, or a malformed field arm type).
func tryCastClass(b *schema.Bundle, name string, mode schema.StreamingMode, input value, cctx *coerceCtx) (json.RawMessage, candKind, bool, error) {
	cls, ok := b.FindClass(name, mode)
	if !ok {
		return nil, 0, false, fmt.Errorf("debaml: unknown class %q", name)
	}
	if input.kind != valObject {
		// Class try_cast handles objects only (a scalar/array/null never try_casts
		// to a class; those are the lenient inferred-object / array-to-singular
		// paths, not the strict cast).
		return nil, candClass, false, nil
	}
	// BAML's Class::try_cast circular-reference guard (ir_ref/coerce_class.rs), on the
	// SEPARATE try_cast active set, after the object check and before field recursion:
	// a repeated (ClassKey, value) on the active TRY_CAST path is a normal strict
	// NO-MATCH (None) — NOT a claimed error, unlike the coerce set. Derive a child
	// try_cast context carrying this pair for the field recursion below.
	key := schema.ClassKey{Name: name, Mode: mode}
	if cctx.tryCastHas(key, input) {
		return nil, candClass, false, nil
	}
	child := cctx.enterTryCast(key, input)
	nF := len(cls.Fields)
	assigned := make([]value, nF)
	present := make([]bool, nF)
	for i := range input.objV {
		key := input.objV[i].key
		mf := -1
		for j := range cls.Fields {
			// EXACT rendered-name match (BAML's fill_result.get_mut(k)); the gate
			// rejects duplicate rendered names, so the first match is unique.
			if cls.Fields[j].Name.RenderedName() == key {
				mf = j
				break
			}
		}
		if mf < 0 {
			// try_cast rejects objects with ANY extra key (stricter matching).
			return nil, candClass, false, nil
		}
		if present[mf] {
			continue // duplicate key for an already-set field: keep first.
		}
		present[mf] = true
		assigned[mf] = input.objV[i].val
	}

	var buf bytes.Buffer
	buf.WriteByte('{')
	first := true
	for j := range cls.Fields {
		if !present[j] {
			// The gate guarantees every field is REQUIRED (no optional), so a field
			// with no input key fails the strict cast (BAML returns None; the lenient
			// phase would default/error it instead).
			return nil, candClass, false, nil
		}
		fc, matched, err := tryCastArm(b, cls.Fields[j].Type, assigned[j], child)
		if err != nil {
			return nil, candClass, false, err
		}
		if !matched {
			// A field's value does not strictly cast to its type → the class
			// try_cast fails (BAML returns None; the lenient phase may still coerce).
			return nil, candClass, false, nil
		}
		if !first {
			buf.WriteByte(',')
		}
		first = false
		key, err := marshalJSON(cls.Fields[j].Name.Name)
		if err != nil {
			return nil, candClass, false, err
		}
		buf.Write(key)
		buf.WriteByte(':')
		buf.Write(fc.output)
	}
	buf.WriteByte('}')
	return buf.Bytes(), candClass, true, nil
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

// isClaimedMismatch reports whether err is a CLAIMED value-verdict mismatchError
// (typeMismatch / ambiguousMatch — an error BAML also produces) as opposed to the
// ErrDeBAMLParseUnsupported fallback sentinel. coerceClass uses it to distinguish
// a field failure BAML PROVABLY errors on (propagate as the class error, since
// such a field type is non-defaultable) from one native merely DECLINED (native
// stricter than BAML → indeterminate, fall back).
func isClaimedMismatch(err error) bool {
	var me *mismatchError
	return errors.As(err, &me)
}

// provenErr marks a PROVABLE BAML coercion error native does NOT claim at a
// standalone position — a literal/enum no-match, an int/bool literal value
// mismatch, or a missing required class field — where BAML errors but native
// falls back (an optional at a higher level might null-default, which native
// cannot score). It wraps the ErrDeBAMLParseUnsupported sentinel via Unwrap, so
// declinableChildError and the top-level / container disposition are UNCHANGED
// from the pre-M3 plain unsupported() form. What it adds is a signal for the
// SCORED UNION selection (isProvenBamlError): inside a union arm this is a real
// BAML failure that is EXCLUDED from ranking, as opposed to a native-can't-prove
// decline (plain unsupported) that must decline the whole union.
type provenErr struct{ msg string }

func (e *provenErr) Error() string { return e.msg }

func (e *provenErr) Unwrap() error { return bamlutils.ErrDeBAMLParseUnsupported }

// provenError constructs a provenErr with the fallback sentinel wrapped in, so
// errors.Is(err, ErrDeBAMLParseUnsupported) holds (unchanged fallback behavior).
func provenError(reason string) error {
	return &provenErr{msg: fmt.Sprintf("%s: %s", bamlutils.ErrDeBAMLParseUnsupported.Error(), reason)}
}

// isProvenBamlError reports whether err is a PROVABLE BAML coercion error for a
// union arm — a claimed mismatchError (substring tie / type mismatch) or a
// provenErr (no-match / value-mismatch / missing-required-field). Such an arm is
// EXCLUDED from the scored selection (a real BAML failure). A plain unsupported()
// decline (native stricter, can't prove BAML's outcome) is NOT proven and
// declines the whole union instead.
func isProvenBamlError(err error) bool {
	if isClaimedMismatch(err) {
		return true
	}
	var pe *provenErr
	return errors.As(err, &pe)
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

// --- M4b: streaming (raw_is_done=false) coercion --------------------------
//
// errStreamDeleted is an INTERNAL sentinel (never surfaced to a Parse caller) a
// coerceStream* function returns for a value BAML's semantic streaming DELETES: an
// INCOMPLETE value whose type is done-required, which semantic_streaming.rs
// process_node errors (`must_be_done && state != Complete → Err(IncompleteDoneValue)`).
// It is DISTINCT from bamlutils.ErrDeBAMLParseUnsupported: a deletion is a PROVEN
// BAML behavior native REPRODUCES (drop the list/map child, null-replace the class
// field), whereas the unsupported sentinel means native cannot prove BAML's output
// and must FALL BACK. Each container inspects its child's error:
//
//   - errStreamDeleted            → child deleted (list/map drop, class null-fill);
//   - ErrDeBAMLParseUnsupported   → native cannot prove the child → the WHOLE stream
//     parse falls back (propagated up unchanged);
//   - any other error             → a CLAIMED parse failure, propagated for parity.
//
// It never escapes coerceStream: the top-level target is always the synthetic
// Baml_Rest_DynamicOutput CLASS (a class is never done-required), whose
// coerceStreamClass converts every child deletion into a null field — so a stray
// errStreamDeleted reaching parseStream would be an invariant bug, not a claim.
var errStreamDeleted = errors.New("debaml: stream value deleted by semantic streaming")

// coerceStream coerces a value recovered by the streaming parser
// (streamExtractCandidate / streamFix) into the flattened dynamic output JSON for
// type t. It ports the ANNOTATION-FREE slice of BAML's semantic_streaming.rs
// process_node (M4c) whose observable output for a dynamic schema is an ordinary
// object/list/map/string with NO StreamState wrappers.
//
// The dynamic output bridge (bamlutils.DynamicOutputSchema) carries no streaming-
// annotation channel: BAML's dynamic TypeBuilder cannot attach @stream.done,
// @@stream.done, @stream.not_null, or @stream.with_state to a dynamically-built
// field/class (ClassPropertyBuilder exposes only SetType / description / alias), and
// schema.FromDynamicOutputSchema lowers every class non-streaming with a zero
// StreamingBehavior. So for a dynamic schema BAML's OWN parse-stream also sees no
// annotations: process_node's `must_be_done` collapses to the INTRINSIC required-done
// TYPE TABLE (semantic_streaming.rs required_done) — int / float / bool / enum /
// literal / null / media require done; string / list / map / class / tuple /
// recursive-alias do NOT — and needed_fields (@stream.not_null) is empty, so a class
// never errors from a missing needed field. M4c models exactly that intrinsic
// behavior; any annotation-dependent behavior is NOT representable and stays BAML's
// job (checkNoStreamAnnotations is a defensive gate; a union target still declines).
//
// process_node's KEEP / DELETE / NULL-REPLACE decisions, ported per container:
//
//   - int / float / bool / null / enum / literal / media (DONE-REQUIRED leaf): a
//     COMPLETE value coerces through the SAME final leaf coercer (stream == final for
//     a done value); an INCOMPLETE value is DELETED (errStreamDeleted) — process_node
//     errors it, and the parent drops/null-fills — see coerceStreamDoneLeaf.
//   - string: NOT done-required, emitted as-is whether COMPLETE or INCOMPLETE (a
//     truncated / markdown-recovered string keeps its partial value; the "AnyOf
//     string does not leak" surface). A non-string into a string target declines.
//   - list: coerce each element; KEEP a coerced element, DROP a DELETED element,
//     keeping completed siblings in BAML order — see coerceStreamList.
//   - map: same as list, but a DELETED value drops the WHOLE entry — coerceStreamMap.
//   - class: assign present keys to fields, coerce each; a DELETED field value is
//     REPLACED WITH NULL, a MISSING field is NULL-FILLED (Pending), fields emitted in
//     class-definition order — see coerceStreamClass.
//   - union: DECLINE (mixed/composite union semantic streaming stays fallback — the
//     variant-dependent required_done and array union_variant_hint are not modeled).
//
// A child that DECLINES with ErrDeBAMLParseUnsupported (native cannot prove BAML's
// output) propagates unchanged so the WHOLE stream parse falls back; a deletion is a
// proven behavior native reproduces. Over-decline is safe; over-claim is a bug.
func coerceStream(b *schema.Bundle, t schema.Type, input value) (json.RawMessage, error) {
	switch t.Kind {
	case schema.TypePrimitive:
		if t.Primitive == schema.PrimitiveString {
			if input.kind == valString {
				return marshalJSON(input.strV)
			}
			// A COMPLETE non-string into a string target: BAML stringifies via JsonToString
			// (serde Display) IDENTICALLY in stream and final (LIVE-CAPTURED: a complete
			// {"f":5e0} streams f:"5.0", the same as the final). coercePrimitiveString
			// reproduces the display byte-exact (displayNumber / displayFloat64).
			//
			// §5.9.1 greedy-recovery — bare-scalar pre-comma display. An INCOMPLETE unquoted
			// NUMBER mid-token in a string field is streamed by BAML as its evolving serde/ryu
			// display (`{"f":5`→f:"5", `{"f":5e0`→f:"5.0"), byte-IDENTICAL to the complete
			// display below (semantic streaming does not gate a string target on done).
			// coercePrimitiveString reproduces it byte-exact. An incomplete raw span that is
			// not a valid number arrives as an Incomplete valString and is handled above
			// (marshalJSON of the raw span — e.g. `5e`→"5e"). Any OTHER incomplete non-string
			// (an incomplete bool/null keyword, never emitted into an admitted string field on
			// this matrix) stays declined — a safe under-claim, never a byte-divergent emit.
			if input.incomplete {
				if input.kind == valNumber {
					return coercePrimitiveString(input, &coerceFlags{})
				}
				return nil, unsupported("stream: incomplete non-number/non-string into string field (greedy-recovery)")
			}
			return coercePrimitiveString(input, &coerceFlags{})
		}
		return coerceStreamDoneLeaf(b, t, input)
	case schema.TypeLiteral, schema.TypeEnum:
		return coerceStreamDoneLeaf(b, t, input)
	case schema.TypeClass:
		return coerceStreamClass(b, t.Name, t.Mode, input)
	case schema.TypeList:
		return coerceStreamList(b, t.Elem, input)
	case schema.TypeMap:
		return coerceStreamMap(b, t.Key, t.Value, input)
	case schema.TypeUnion:
		return nil, unsupported("stream: union target (mixed-union semantic streaming deferred)")
	default:
		return nil, unsupported(fmt.Sprintf("stream: type kind %q", t.Kind))
	}
}

// coerceStreamDoneLeaf coerces a DONE-REQUIRED leaf (int / float / bool / null /
// enum / literal / media) in stream mode, porting semantic_streaming.rs
// process_node's done-check for a leaf. An INCOMPLETE value FAILS the check
// (must_be_done && state != Complete → Err(IncompleteDoneValue)): BAML DELETES it,
// so this returns errStreamDeleted and the parent container drops it (list/map) or
// null-replaces it (class). A COMPLETE value is coerced through the SAME final leaf
// coercer — for a done value stream and final coercion are identical — and a final
// DECLINE / proven error DECLINES the whole stream parse (fall back): the leaf sits
// below semantic streaming, so native cannot prove whether BAML's coercion-layer
// skip (ArrayItemParseError) or default-fill would fire, and over-declines instead.
func coerceStreamDoneLeaf(b *schema.Bundle, t schema.Type, input value) (json.RawMessage, error) {
	if input.incomplete {
		return nil, errStreamDeleted
	}
	// The stream lane never carries a recursive class (SupportsNativeStreamBundle
	// keeps its blanket cycle/union decline), so a nil pair-guard context is correct.
	out, err := coerce(b, t, input, &coerceFlags{}, nil)
	if err != nil {
		// §5.9.1 greedy-recovery — invalid-enum partial disposition. A COMPLETE
		// done-required leaf that PROVABLY fails coercion is, in PARTIAL (stream) mode,
		// BAML's partial-unparseable disposition, NOT a whole-parse fallback: the leaf
		// coerces to an error, the class/list/map records the field as unparsed, and the
		// PARTIAL value is emitted with that field nulled (class deletion_nulls) / dropped
		// (list/map), while the FINAL non-stream parse still errors on the required field
		// (LIVE-CAPTURED: {f:E1,last:string} with "NOPE" streams {"f":null,"last":...} then
		// the terminal final ERRORS — same as BAML). Reuse errStreamDeleted (the parent
		// container already null-fills / drops it, identical to an incomplete done value).
		//
		// Scoped to a PROVEN match_string miss — a complete enum or string-literal value
		// from a STRING input, whose fold is now BAML-exact (bamlunicode, #555 Slice 2), so
		// a native coercion failure equals a BAML failure. This is the SAME proven-error
		// whitelist coerceStreamList already uses (provenListItemError) to drop a bad
		// list<enum>/list<litstr> child, so a direct enum FIELD and an enum LIST ELEMENT now
		// share one disposition. An UNPROVEN failure (native stricter than BAML — a numeric
		// overflow, an ambiguous conversion) still DECLINES so the whole stream falls back;
		// never a blanket null on every coercion failure.
		if streamCompleteLeafUnparseable(t, input) {
			return nil, errStreamDeleted
		}
		return nil, unsupported(fmt.Sprintf("stream: done-required leaf did not cleanly coerce: %v", err))
	}
	return out, nil
}

// streamCompleteLeafUnparseable reports whether a COMPLETE leaf value that FAILED
// coercion is a PROVEN BAML non-match native may treat as a partial-unparseable
// deletion (class null / list-map drop) rather than a whole-stream fallback. It is
// deliberately NARROW — a match_string target (enum, string-literal) coerced from a
// STRING, where the fold is proven BAML-exact (bamlunicode) so a native miss is a
// certain BAML miss. Numeric / bool / int-or-bool-literal leaves are NOT included:
// native is stricter than BAML there (inf/overflow/hex/underscore), so a failure is
// not proven and must keep declining. Mirrors provenListItemError's enum/litstr arms
// so a class enum field and a list<enum> element resolve identically.
func streamCompleteLeafUnparseable(t schema.Type, input value) bool {
	if input.kind != valString {
		return false
	}
	switch t.Kind {
	case schema.TypeEnum:
		return true
	case schema.TypeLiteral:
		return t.Literal != nil && t.Literal.Kind == schema.LiteralString
	default:
		return false
	}
}

// coerceStreamClass coerces an object into a class in stream mode, porting the
// annotation-free slice of semantic_streaming.rs process_node's Class arm together
// with the coerce_class.rs field assignment: assign present input keys to fields
// (BAML's input-key-first fuzzy match, keep-first on duplicates, extra keys ignored),
// coerce each present field's value through coerceStream, and emit every field in
// class-definition order. A field's value becomes null in two BAML cases, both an
// explicit null here:
//
//   - MISSING field (no input key matched): NULL-FILLED with BAML's Pending null
//     (fields_needing_null_filler → Null{state: Pending});
//   - DELETED field value (coerceStream returns errStreamDeleted — an incomplete
//     done-required field value process_node errors): REPLACED WITH NULL
//     (deletion_nulls), never dropped — a class field is null-replaced, not removed.
//
// Both survive the downstream InjectAbsentOptionals + reorder identically to BAML's
// flattened output. Because the dynamic bridge carries no @stream.not_null,
// needed_fields is empty, so a class never errors from a missing needed field (that
// MissingNeededFields → whole-class null/drop path is not representable here).
//
// It DECLINES (fall back) for the shapes M4c does not model:
//   - NON-object input (a scalar/array → class implied-key / inferred-object /
//     array-to-singular is deferred);
//   - a class where NO field received a present value — the "open/repaired root
//     object AFTER ≥1 field value is available" guard: a bare `{`, a key with no
//     value, a dangling colon, or an all-extra-keys object stays fallback (a matched
//     field whose value is DELETED still counts as present, so it is claimed);
//   - a present field whose value DECLINES in stream with the unsupported sentinel (a
//     complete leaf that does not cleanly coerce, a deferred conversion) — its
//     decline propagates and the whole class falls back.
//
// A field-key match is now proven (bamlunicode == BAML's match_string), so a
// non-ASCII field key is native's to CLAIM — no field-key uncertainty decline
// (#555 Slice 2).
func coerceStreamClass(b *schema.Bundle, name string, mode schema.StreamingMode, input value) (json.RawMessage, error) {
	cls, ok := b.FindClass(name, mode)
	if !ok {
		return nil, fmt.Errorf("debaml: unknown class %q", name)
	}
	if input.kind != valObject {
		return nil, unsupported(fmt.Sprintf("stream: class %q from %s (implied-key/inferred/array-to-singular deferred)", name, input.kind))
	}
	nF := len(cls.Fields)
	matched := make([]bool, nF)
	assigned := make([]value, nF)
	// Input-key-first assignment: each key to the FIRST field it fuzzily matches
	// (no substring); a key matching an already-filled field is a duplicate
	// (keep-first, ignored); a key matching no field is an extra (ignored).
	for i := range input.objV {
		key := input.objV[i].key
		mf := -1
		for j := range cls.Fields {
			if matchesStringToString(key, cls.Fields[j].Name.RenderedName()) {
				mf = j
				break
			}
		}
		if mf < 0 || matched[mf] {
			continue
		}
		assigned[mf] = input.objV[i].val
		matched[mf] = true
	}
	// An open object with NO matched field value yet — a bare `{`, a dangling key
	// `{"name"`, or a dangling colon `{"name":` — is NOT declined (Phase 7C
	// eligible-prefix gap closure). BAML parse-stream emits the class with EVERY
	// field at its streaming filler for these prefixes (LIVE-CAPTURED: corpus 20
	// open_brace / first_key / colon and corpus 177 key_only all yield the all-filler
	// object, e.g. {"name":null,"age":null}). The field loop below already produces
	// exactly that when no field matched — the empty-object case simply falls through
	// to the SAME all-filler emission the ≥1-field case uses for its missing fields,
	// so native reproduces BAML byte-exact with the identical, proven filler logic
	// (no per-prefix BAML fallback, I6).
	//
	// This is deliberately scoped to an open OBJECT candidate: bare prose or a
	// just-opened fence with no `{` never reach here (streamExtractCandidate declines
	// them at extraction), so BAML's allow_as_string→class recovery for those stays
	// native-declined — a safe under-claim, never mis-emitted as all-null here.

	var buf bytes.Buffer
	buf.WriteByte('{')
	for j := range cls.Fields {
		if j > 0 {
			buf.WriteByte(',')
		}
		key, err := marshalJSON(cls.Fields[j].Name.Name)
		if err != nil {
			return nil, err
		}
		buf.Write(key)
		buf.WriteByte(':')
		if matched[j] {
			o, err := coerceStream(b, cls.Fields[j].Type, assigned[j])
			switch {
			case err == nil:
				buf.Write(o)
			case errors.Is(err, errStreamDeleted):
				// Field value DELETED by semantic streaming (incomplete done-required)
				// → BAML's deletion_nulls: the field is REPLACED WITH NULL, not dropped.
				buf.WriteString("null")
			default:
				// Unsupported → whole class falls back; a claimed error propagates.
				return nil, err
			}
		} else {
			buf.Write(streamMissingFieldFiller(cls.Fields[j].Type))
		}
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// streamMissingFieldFiller returns BAML's streaming filler value for a class field
// absent from the stream, matching semantic_streaming.rs fields_needing_null_filler
// composed with the coercion-layer default fill (coerce_class.rs) — the SAME
// TypeIR::default_value the FINAL missing-field pass uses (defaultValue), differing
// only in that streaming TOLERATES a missing required field rather than erroring:
//
//   - OPTIONAL field → null (OptionalDefaultFromNoValue + Pending);
//   - REQUIRED field with a default_value → that default (list→[], map→{},
//     primitive-null→null, first-defaultable-union-arm, tuple — DefaultFromNoValue +
//     Pending);
//   - REQUIRED non-defaultable field (scalar / enum / literal / class) → null (the
//     Pending null-filler; final parse would error, streaming null-fills).
//
// This is why a missing required list field surfaces as [] and a missing required map
// as {} in BAML parse-stream (LIVE-CAPTURED — see corpus 177), not null. A DELETED
// field (present but incomplete done-required) is null-replaced separately
// (deletion_nulls), never routed here; those types are never defaultable anyway.
func streamMissingFieldFiller(ft schema.Type) json.RawMessage {
	if isOptional(ft) {
		return json.RawMessage("null")
	}
	if d, ok := defaultValue(ft); ok {
		return d
	}
	return json.RawMessage("null")
}

// coerceStreamList coerces an array into a list in stream mode, porting
// semantic_streaming.rs process_node's List arm (filter_map over the children,
// dropping any that error). A list is never done-required, so an unterminated
// (Incomplete) list is fine; each element is coerced through coerceStream and:
//
//   - KEPT (in element order) when it coerces;
//   - DROPPED when it is DELETED by semantic streaming (errStreamDeleted — an
//     incomplete done-required element), keeping completed siblings in BAML order.
//     This is the NUMBERS behavior: int[] streaming `[1,2` keeps 1 and drops the
//     still-building 2.
//
// It DECLINES (fall back) for the cases M4c does not model:
//   - a NON-array input (SingleToArray wrapping is deferred);
//   - an element that returns ErrDeBAMLParseUnsupported — native cannot prove BAML's
//     output for it (a complete leaf that does not cleanly coerce, whose
//     coercion-layer ArrayItemParseError skip-vs-keep native does not model in
//     stream) — so the WHOLE list falls back; a claimed element error propagates.
func coerceStreamList(b *schema.Bundle, elem *schema.Type, input value) (json.RawMessage, error) {
	if elem == nil {
		return nil, fmt.Errorf("debaml: list type missing element")
	}
	if input.kind != valArray {
		return nil, unsupported(fmt.Sprintf("stream: list from %s (SingleToArray deferred)", input.kind))
	}
	var buf bytes.Buffer
	buf.WriteByte('[')
	first := true
	for i := range input.arrV {
		o, err := coerceStream(b, *elem, input.arrV[i])
		if err != nil {
			if errors.Is(err, errStreamDeleted) {
				continue // semantic-streaming deletion: drop the element, keep siblings.
			}
			// A COMPLETE element that does not coerce but is a PROVEN BAML
			// ArrayItemParseError (the SAME whitelist the FINAL list coercer uses to
			// drop it) is DROPPED, keeping siblings — matching BAML's parse-stream, which
			// drops the bad child at the SAME cadence (LIVE-CAPTURED: list<int>
			// `["1",2,"bad",4]` streams [] -> [1] -> [1,2] -> (drop "bad") -> [1,2,4],
			// corpus 94). Only an UNPROVEN unsupported (native cannot tell BAML's
			// keep-vs-drop-vs-default) still declines the whole list; a claimed error
			// still propagates.
			if errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) && provenListItemError(b, *elem, input.arrV[i]) {
				continue
			}
			return nil, err // unproven unsupported → whole list falls back; claimed error → propagate.
		}
		if !first {
			buf.WriteByte(',')
		}
		first = false
		buf.Write(o)
	}
	buf.WriteByte(']')
	return buf.Bytes(), nil
}

// coerceStreamMap coerces an object into a map in stream mode, porting
// semantic_streaming.rs process_node's Map arm (filter_map over the entries,
// dropping any whose VALUE errors — the WHOLE key/value pair is removed, never
// null-kept, unlike a class field). A map is never done-required.
//
// It is restricted to the SIMPLE reachable subset — a STRING key (every key valid)
// and no duplicate keys — where native reproduces BAML's partial map byte-exact.
// A non-string key (enum / literal / string-literal union, whose dynamic lenient
// key-keep / MapKeyParseError semantics are Mcoerce-d / unproven in stream) and a
// duplicate key (insert/overwrite ordering unproven) DECLINE the whole map. Entries
// are emitted in INPUT key order under their ORIGINAL key strings.
//
//   - KEPT: an entry whose value coerces;
//   - DROPPED: an entry whose value is DELETED by semantic streaming
//     (errStreamDeleted — an incomplete done-required value), keeping siblings;
//   - WHOLE map DECLINES: a non-object input, a non-string key, a duplicate key, or a
//     value that returns ErrDeBAMLParseUnsupported / a claimed error.
func coerceStreamMap(b *schema.Bundle, keyT, valT *schema.Type, input value) (json.RawMessage, error) {
	if keyT == nil || valT == nil {
		return nil, fmt.Errorf("debaml: map type missing key or value")
	}
	if input.kind != valObject {
		return nil, unsupported(fmt.Sprintf("stream: map from %s (ObjectToMap of non-object deferred)", input.kind))
	}
	if keyT.Kind != schema.TypePrimitive || keyT.Primitive != schema.PrimitiveString {
		return nil, unsupported("stream: map with non-string key (enum/literal key dynamic-keep unproven in stream)")
	}
	// Any DUPLICATE original key declines the whole map (mirrors coerceMap): BAML's
	// duplicate insert/overwrite ordering is unproven, so native never claims it.
	seen := make(map[string]struct{}, len(input.objV))
	for i := range input.objV {
		if _, dup := seen[input.objV[i].key]; dup {
			return nil, unsupported(fmt.Sprintf("stream: map duplicate key %q (insert/overwrite ordering unproven)", input.objV[i].key))
		}
		seen[input.objV[i].key] = struct{}{}
	}
	var buf bytes.Buffer
	buf.WriteByte('{')
	first := true
	for i := range input.objV {
		f := &input.objV[i]
		o, err := coerceStream(b, *valT, f.val)
		if err != nil {
			if errors.Is(err, errStreamDeleted) {
				continue // value deleted → drop the WHOLE entry, keep siblings.
			}
			return nil, err // unsupported → whole map falls back; claimed error → propagate.
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
		buf.Write(o)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}
