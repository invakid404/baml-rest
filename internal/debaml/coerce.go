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
	"github.com/invakid404/baml-rest/internal/schema"
)

// coerceFlags accumulates two signals a union claim needs:
//
//   - flagged: a BAML score-bearing condition native CLAIMS (and is therefore
//     NOT a zero-score "clean" success): a SubstringMatch (enum/literal/map-key
//     substring, cost 2), an ExtraKey (unmatched class input key, cost 1 each),
//     or an ObjectToMap (every object→map, cost 1). Used by the NULLABLE-union
//     claim, where BAML's null arm competes by scoring (DefaultButHadValue,
//     cost 110): only a clean (score-0) non-null arm provably beats null
//     without native computing scores.
//   - uncertain: the match verdict depended on a non-ASCII case fold native
//     cannot prove equals Rust's str::to_lowercase (caseFoldUncertain). Used by
//     EVERY union claim: if any arm's verdict was uncertain, native cannot
//     trust its per-arm count (a false-rejected leaf would let it claim the
//     wrong arm), so it declines.
//
// A nil receiver means "don't track" (top-level / non-nullable / standalone),
// so threading it is free there.
type coerceFlags struct {
	flagged   bool
	uncertain bool
}

// flag marks the coercion as non-clean. Nil-safe so untracked paths are free.
func (f *coerceFlags) flag() {
	if f != nil {
		f.flagged = true
	}
}

// markUncertain records a non-ASCII case-fold native cannot prove matches BAML.
func (f *coerceFlags) markUncertain() {
	if f != nil {
		f.uncertain = true
	}
}

// isFlagged reports whether any score-bearing condition was recorded.
func (f *coerceFlags) isFlagged() bool { return f != nil && f.flagged }

// isUncertain reports whether a non-ASCII case-fold uncertainty was recorded.
func (f *coerceFlags) isUncertain() bool { return f != nil && f.uncertain }

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
// MAP parity (coerceMap / coerce_map.rs): object→map ObjectToMap flagging, VALUE-
// then-KEY coercion, PARTIAL entry skips of PROVEN value/key parse errors
// (MapValueParseError / MapKeyParseError), and ORIGINAL input key strings in
// input order — each score-bearing (ObjectToMap, the partial errors) so the
// nullable-union clean-only rule still holds. The remaining leniencies stay
// STRICT and DECLINE: non-string stringification (ObjectToString / JsonToString),
// single-key-object ObjectToPrimitive literal extraction, single-field implied-key
// / inferred-object absorption, and class default-fill — all deferred to
// Mcoerce-d. Where native's coercion fails but BAML may leniently SUCCEED (or
// null-default) — or where native cannot determine BAML's exact success/failure
// — coercion DECLINES (ErrDeBAMLParseUnsupported → fall back to BAML), so native
// is never "more capable" than BAML.
//
// Native CLAIMS a coercion error only where BAML also errors: a non-object
// where a MULTI-field class is required (coerceClass typeMismatch), or a
// match_string substring TIE (StrMatchOneFromMany — coerceEnum/coerceLiteral
// ambiguousMatch). A required field still unmatched after fuzzy key matching
// DECLINES — BAML may default-fill or hard-fail and native cannot tell which.
// Union claims count LENIENT per-variant successes and claim only the lone
// winner; two-plus successes are BAML pick_best (M3) and DECLINE. A consequence
// is that native still declines some inputs BAML would itself reject or coerce
// differently (e.g. a required field with no fuzzy key match, an int-LITERAL
// value mismatch after rounding, a single-key object into a literal) — behavior
// is identical via fallback, and precise claim-parity for those is deferred to
// the rest of the Mcoerce milestone (#546). See coercePrimitive / coerceList /
// coerceEnum / coerceLiteral / coerceClass for the per-kind boundary.
func coerce(b *schema.Bundle, t schema.Type, input value, f *coerceFlags) (json.RawMessage, error) {
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
//
// Primitive STRING stays STRICT (only a JSON string coerces): non-string→string
// via JsonToString (score 2) is Mcoerce-d, so a non-string DECLINES. Every
// score-bearing conversion marks f (nil-safe) so the nullable-union clean-only
// rule can require the winning non-null arm to be clean.
func coercePrimitive(p schema.PrimitiveKind, input value, f *coerceFlags) (json.RawMessage, error) {
	switch p {
	case schema.PrimitiveString:
		if input.kind != valString {
			// Non-string → string is JsonToString (score 2), Mcoerce-d.
			return nil, declineCoerce("string target", input)
		}
		return marshalJSON(input.strV)
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
// when the value came from a float/fraction/extracted path (FloatToInt).
func coercePrimitiveInt(input value, f *coerceFlags) (json.RawMessage, error) {
	n, flagged, err := coerceIntValue(input)
	if err != nil {
		return nil, err
	}
	if flagged {
		f.flag() // FloatToInt (score 1)
	}
	return json.RawMessage(strconv.FormatInt(n, 10)), nil
}

// coercePrimitiveFloat emits the coerced float as a JSON number, marking f
// when the value came from the extracted-number path (StringToFloat).
func coercePrimitiveFloat(input value, f *coerceFlags) (json.RawMessage, error) {
	out, flagged, err := coerceFloatValue(input)
	if err != nil {
		return nil, err
	}
	if flagged {
		f.flag() // StringToFloat (score 1)
	}
	return out, nil
}

// coercePrimitiveBool emits the coerced bool, marking f when it came from a
// string (StringToBool). A non-ASCII case-fold uncertainty in the match_string
// fallback marks f and DECLINES (native cannot prove the verdict equals BAML).
func coercePrimitiveBool(input value, f *coerceFlags) (json.RawMessage, error) {
	b, flagged, uncertain, err := coerceBoolValue(input)
	if uncertain {
		f.markUncertain()
	}
	if err != nil {
		return nil, err
	}
	if flagged {
		f.flag() // StringToBool (score 1)
	}
	return boolRaw(b), nil
}

// coercePrimitiveNull emits JSON null. A non-null input defaults to null with
// DefaultButHadValue (score 110), marking f. In a nullable-union scoring
// decision the implicit null arm is handled by coerceUnionSafe, not here — this
// is the standalone primitive-null target BAML always resolves to null.
func coercePrimitiveNull(input value, f *coerceFlags) (json.RawMessage, error) {
	if input.kind != valNull {
		f.flag() // DefaultButHadValue (score 110)
	}
	return json.RawMessage("null"), nil
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
		n, fin := i64FromF64RoundOk(v)
		if !fin {
			return 0, false, declineCoerce("int target (non-finite)", input)
		}
		return n, true, nil // FloatToInt
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
// whether a StringToBool conversion happened, whether a non-ASCII case-fold
// uncertainty was hit in the match_string fallback, and an error. A JSON bool
// is clean; a string is first tested casefold-exact against "true"/"false",
// then via match_string (substring enabled) — whose own flags (SubstringMatch)
// are DROPPED: a substring hit still adds only StringToBool (score 1).
func coerceBoolValue(input value) (b bool, flagged bool, uncertain bool, err error) {
	switch input.kind {
	case valBool:
		return input.boolV, false, false, nil
	case valString:
		switch strings.ToLower(input.strV) {
		case "true":
			return true, true, false, nil // StringToBool
		case "false":
			return false, true, false, nil // StringToBool
		}
		matched, outcome, _, unc := matchString(input.strV, boolMatchCandidates, true)
		if unc {
			return false, false, true, unsupported("bool target: non-ASCII case-fold uncertainty in match_string")
		}
		if outcome == matchOne {
			switch matched {
			case "true":
				return true, true, false, nil // StringToBool (NOT SubstringMatch)
			case "false":
				return false, true, false, nil
			}
		}
		// match_string no-match or substring TIE (StrMatchOneFromMany): BAML's
		// coerce_bool errors; for a required bool it errors, for an optional one
		// it null-defaults, and native cannot tell which without scoring — decline.
		return false, false, false, declineCoerce("bool target", input)
	default:
		return false, false, false, declineCoerce("bool target", input)
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
// BEFORE every literal kind; that prelude is Mcoerce-d, so native does NOT copy
// it — an OBJECT input to an int/bool (or string) literal DECLINES here.
func coerceLiteral(lit *schema.LiteralValue, input value, f *coerceFlags) (json.RawMessage, error) {
	if lit == nil {
		return nil, fmt.Errorf("debaml: literal type missing value")
	}
	switch lit.Kind {
	case schema.LiteralString:
		if input.kind != valString {
			return nil, declineCoerce("literal string", input)
		}
		matched, outcome, viaSub, uncertain := matchString(input.strV, []matchCandidate{{name: lit.String, validValues: []string{lit.String}}}, true)
		if uncertain {
			f.markUncertain()
			return nil, unsupported(fmt.Sprintf("literal string %q: non-ASCII case-fold uncertainty for %q (cannot prove match equals BAML)", lit.String, input.strV))
		}
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
		if input.kind == valObject {
			// Single-key-object ObjectToPrimitive extraction is Mcoerce-d.
			return nil, declineCoerce("literal int (BAML single-key object extraction)", input)
		}
		n, flagged, err := coerceIntValue(input)
		if err != nil {
			if !declinableChildError(err) {
				return nil, err // hard/invariant failure: propagate.
			}
			return nil, unsupported(fmt.Sprintf("literal int %d: primitive int coercion declined: %v", lit.Int, err))
		}
		if n != lit.Int {
			// BAML errors here (required) or null-defaults (optional); native
			// cannot tell which without scoring, so decline (fall back).
			return nil, unsupported(fmt.Sprintf("literal int %d: coerced to %d (value mismatch → BAML error/default)", lit.Int, n))
		}
		if flagged {
			f.flag() // FloatToInt (score 1) — preserved from the primitive coercer.
		}
		return json.RawMessage(strconv.FormatInt(n, 10)), nil
	case schema.LiteralBool:
		if input.kind == valObject {
			return nil, declineCoerce("literal bool (BAML single-key object extraction)", input)
		}
		b, flagged, uncertain, err := coerceBoolValue(input)
		if uncertain {
			f.markUncertain()
			return nil, unsupported(fmt.Sprintf("literal bool %v: non-ASCII case-fold uncertainty (cannot prove verdict equals BAML)", lit.Bool))
		}
		if err != nil {
			if !declinableChildError(err) {
				return nil, err // hard/invariant failure: propagate.
			}
			return nil, unsupported(fmt.Sprintf("literal bool %v: primitive bool coercion declined: %v", lit.Bool, err))
		}
		if b != lit.Bool {
			return nil, unsupported(fmt.Sprintf("literal bool %v: coerced to %v (value mismatch → BAML error/default)", lit.Bool, b))
		}
		if flagged {
			f.flag() // StringToBool (score 1) — preserved from the primitive coercer.
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
	matched, outcome, viaSub, uncertain := matchString(input.strV, enumMatchCandidates(e), true)
	if uncertain {
		// The verdict hinged on a non-ASCII case fold native cannot prove
		// equals BAML's. Mark it (so a union counter declines the whole union)
		// and DECLINE rather than claim a match/no-match that might diverge.
		f.markUncertain()
		return nil, unsupported(fmt.Sprintf("enum %q: non-ASCII case-fold uncertainty for %q (cannot prove match equals BAML)", name, input.strV))
	}
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
	keyUncertain := false
	for i := range input.objV {
		key := input.objV[i].key
		matchedField := -1
		for j := range cls.Fields {
			m, unc := matchesStringToString(key, cls.Fields[j].Name.RenderedName())
			if unc {
				// This key-vs-field comparison hinged on a non-ASCII case fold
				// native cannot prove equals BAML's, so the assignment is
				// untrustworthy.
				keyUncertain = true
				cf.markUncertain()
			}
			if m {
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
	if keyUncertain {
		// A field-key match/no-match could diverge from BAML; DECLINE rather
		// than claim a (possibly wrong) assignment. cf is already marked so an
		// enclosing union counter declines the whole union.
		return nil, unsupported(fmt.Sprintf("class %q: non-ASCII case-fold uncertainty in a field key (cannot prove assignment equals BAML)", name))
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
// keep. A case-fold-uncertain child likewise declines the whole list. A
// HARD/invariant child error propagates unchanged. See coerceListChild /
// provenListItemError for the exact safe-skip vs must-decline split.
//
// Score-bearing flags (SingleToArray, each ArrayItemParseError skip, and a kept
// child's own flags) are threaded into cf so the nullable-union clean-only rule
// (coerceUnionSafe) sees a flagged list arm and declines it against the scored
// null arm — Mcoerce-c does not score collection arms against null.
func coerceList(b *schema.Bundle, elem *schema.Type, input value, cf *coerceFlags) (json.RawMessage, error) {
	if elem == nil {
		return nil, fmt.Errorf("debaml: list type missing element")
	}
	var buf bytes.Buffer
	buf.WriteByte('[')

	if input.kind != valArray {
		// Non-array → BAML wraps it as one implied element (SingleToArray, score 1).
		cf.flag()
		out, keep, err := coerceListChild(b, *elem, input, cf)
		if err != nil {
			return nil, err
		}
		if keep {
			buf.Write(out)
		} else {
			// The implied element is a proven parse error → ArrayItemParseError(0):
			// the list is the EMPTY list, which still SUCCEEDS.
			cf.flag()
		}
		buf.WriteByte(']')
		return buf.Bytes(), nil
	}

	first := true
	for i := range input.arrV {
		out, keep, err := coerceListChild(b, *elem, input.arrV[i], cf)
		if err != nil {
			return nil, err
		}
		if !keep {
			// Proven BAML parse error → ArrayItemParseError(i) (score 1+i): SKIP.
			cf.flag()
			continue
		}
		if !first {
			buf.WriteByte(',')
		}
		first = false
		buf.Write(out)
	}
	buf.WriteByte(']')
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
//     child could be a DEFERRED Mcoerce-d success or its match verdict was
//     case-fold-uncertain, so native falls back rather than skip an item BAML
//     might keep.
func coerceListChild(b *schema.Bundle, elem schema.Type, item value, cf *coerceFlags) (json.RawMessage, bool, error) {
	childF := &coerceFlags{}
	out, err := coerce(b, elem, item, childF)
	if err == nil {
		// Kept: the child value's own flags count toward the list arm's score
		// (types.rs List score = own flags + sum of child scores), so propagate
		// them so an enclosing nullable optional sees a non-clean list arm.
		if childF.isFlagged() {
			cf.flag()
		}
		if childF.isUncertain() {
			cf.markUncertain()
		}
		return out, true, nil
	}
	if !declinableChildError(err) {
		return nil, false, err // hard/invariant failure: propagate, never mask.
	}
	if childF.isUncertain() {
		// The child's match verdict hinged on a non-ASCII case fold native cannot
		// prove equals BAML — it might MATCH (BAML keeps) or MISS (BAML skips), and
		// native cannot tell which, so DECLINE the whole list.
		cf.markUncertain()
		return nil, false, unsupported(fmt.Sprintf("list element %s→%s: non-ASCII case-fold uncertainty (cannot prove skip vs keep)", item.kind, elem.Kind))
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
// native's own coercion FAILED with a declinable error AND was not case-fold-
// uncertain, so for match_string-based targets (bool/enum/string-literal from a
// STRING) a native failure already equals a BAML failure. For NUMERIC targets
// native is STRICTER than BAML (inf/overflow/hex/underscore), so a string is
// proven only when BAML's own parse strategies are re-checked
// (bamlStringNumberFails); an input kind BAML rejects with error_unexpected_type
// is proven; an ARRAY defers to coerce_array_to_singular (pick_best, M3) and a
// class/map/union child (or object into a numeric/literal target) defers to
// Mcoerce-d, so those are NOT proven and DECLINE the whole list.
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
				// Reached only after a native bool coercion FAILED and was NOT
				// case-fold-uncertain: a certain casefold + match_string miss/tie,
				// which BAML's coerce_bool turns into error_unexpected_type.
				return true
			case valObject, valNumber, valNull:
				return true // error_unexpected_type (BAML never coerces number→bool).
			default:
				return false // valBool: success; valArray: array-to-singular (M3).
			}
		default:
			// string / null targets: a native failure is a DEFERRED BAML SUCCESS
			// (JsonToString for string; null defaults for any value), never proven.
			return false
		}
	case schema.TypeEnum:
		// A non-string enum input stringifies (ObjectToString, Mcoerce-d). A
		// STRING native rejected (non-uncertain) is a certain match_string
		// miss/tie, which coerce_enum turns into an error in a required
		// list-element position.
		return item.kind == valString
	case schema.TypeLiteral:
		// Only a STRING literal from a STRING is a proven match_string failure;
		// int/bool literals (value mismatch / exhausted parse) and object/scalar
		// extraction defer to Mcoerce-d.
		return elem.Literal != nil && elem.Literal.Kind == schema.LiteralString && item.kind == valString
	case schema.TypeClass:
		// A multi-field class whose fields are ALL required flat leaves has no
		// default_value for any field, so a genuine SCALAR input leaves every
		// required field unfilled → BAML error_missing_required_field. An ARRAY
		// element defers to coerce_array_to_singular (M3); a single-field class, or
		// a class with any defaultable (list/map/optional) field, defers to
		// Mcoerce-d — so those are NOT proven.
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
			return false // valObject: coerce succeeds/defers; valArray: M3.
		}
	default:
		// map / list / union children: a map non-object, a nested list, or a
		// union needing scoring are all DEFERRED, never proven parse errors.
		return false
	}
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
// WHOLE MAP there rather than skip an entry BAML would keep. A case-fold-
// uncertain value or key likewise declines the whole map.
//
// Native still DECLINES (ErrDeBAMLParseUnsupported → fall back) on:
//   - Non-object input: BAML has object/map coercion + scoring outside this
//     subset (it is not a hard type error).
//   - A DUPLICATE original input key: BAML's IndexMap insert/overwrite ordering
//     for duplicates is unproven, and a skipped duplicate must not be reasoned
//     safe, so ANY duplicate declines the whole map before emitting.
//
// Every object→map carries ObjectToMap (cost 1), and BAML scores a map by its
// OWN flags only — the value scores do NOT propagate (score.rs:21) — so a map is
// NEVER a zero-score "clean" arm: cf is flagged, so a nullable optional map arm
// declines against the scored null arm (the clean-only rule), and Mcoerce-c does
// not score collection arms against null.
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

	// Every object→map carries ObjectToMap (cost 1); flag before iterating.
	cf.flag()

	var buf bytes.Buffer
	buf.WriteByte('{')
	first := true
	for i := range input.objV {
		f := &input.objV[i]
		// VALUE first (BAML coerce_map order): a proven value error skips the
		// entry WITHOUT coercing the key.
		child, keepVal, err := coerceMapValueChild(b, *valT, f.val, cf)
		if err != nil {
			return nil, err // hard/invariant error, or decline-the-whole-map.
		}
		if !keepVal {
			cf.flag() // MapValueParseError (cost 1) → skip entry.
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
//     Mcoerce-d success or its verdict was case-fold-uncertain, so native falls
//     back rather than skip an entry BAML might keep.
//
// A map scores by its OWN flags only (score.rs) — a kept value's own flags do
// NOT propagate to the map score — so, unlike coerceListChild, a flagged value
// does NOT flag cf here (the map is already non-clean via ObjectToMap). The
// child's case-fold uncertainty is still propagated defensively.
func coerceMapValueChild(b *schema.Bundle, valT schema.Type, val value, cf *coerceFlags) (json.RawMessage, bool, error) {
	childF := &coerceFlags{}
	out, err := coerce(b, valT, val, childF)
	if err == nil {
		if childF.isUncertain() {
			cf.markUncertain()
		}
		return out, true, nil
	}
	if !declinableChildError(err) {
		return nil, false, err // hard/invariant failure: propagate, never mask.
	}
	if childF.isUncertain() {
		// The value's match verdict hinged on a non-ASCII case fold native cannot
		// prove equals BAML — it might MATCH (BAML keeps) or MISS (BAML skips), so
		// DECLINE the whole map.
		cf.markUncertain()
		return nil, false, unsupported(fmt.Sprintf("map value %s→%s: non-ASCII case-fold uncertainty (cannot prove skip vs keep)", val.kind, valT.Kind))
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
//     (any arm, for a union) → ACCEPT; a case-fold-UNCERTAIN verdict marks cf and
//     declines; a certain MISS declines the whole map (dynamic lenient keep).
//
// A non-ASCII case-fold uncertainty marks cf (nil-safe) so that — should a map
// ever become reachable as a union arm — the enclosing union counter makes the
// same conservative whole-union decline. Today no claimable union admits a map
// arm (parse.go's safe families are literal/class only), so this is purely
// defensive signal propagation, never an over-claim.
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
		_, outcome, _, uncertain := matchString(key, enumMatchCandidates(e), true)
		if uncertain {
			cf.markUncertain()
			return unsupported(fmt.Sprintf("map key %q: non-ASCII case-fold uncertainty against enum %q", key, keyT.Name))
		}
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
		_, outcome, _, uncertain := matchString(key, []matchCandidate{{name: keyT.Literal.String, validValues: []string{keyT.Literal.String}}}, true)
		if uncertain {
			cf.markUncertain()
			return unsupported(fmt.Sprintf("map key %q: non-ASCII case-fold uncertainty against literal %q", key, keyT.Literal.String))
		}
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
		// key, so the winning arm is irrelevant. Scan ALL arms: only a CERTAIN
		// match accepts the key. An uncertain-only match (matchString can return
		// matchOne together with uncertain when the match is achieved solely in
		// the non-ASCII case-fold pass, e.g. key "É" vs literal "é") must NOT
		// accept — that verdict hinges on a lowercasing native cannot prove
		// equals BAML — so it only records sawUncertain. A later CERTAIN arm
		// still rescues an earlier uncertain one (its match returns before the
		// case-fold attempt, so uncertain is false), which is why scanning all
		// arms — rather than short-circuiting on the first uncertain arm —
		// avoids over-declining.
		accepted := false
		sawUncertain := false
		for _, lit := range flattenStringLiterals(keyT) {
			_, outcome, _, uncertain := matchString(key, []matchCandidate{{name: lit, validValues: []string{lit}}}, true)
			if uncertain {
				sawUncertain = true
				continue
			}
			if outcome == matchOne {
				accepted = true
			}
		}
		if accepted {
			// A clean arm matched; the key is valid regardless of any uncertain
			// arm, and the map inserts the original key — no uncertainty to mark.
			return nil
		}
		if sawUncertain {
			// No clean arm, but an arm's verdict hinged on a non-ASCII case fold
			// native cannot prove equals BAML — decline the map AND propagate the
			// uncertainty (so any enclosing union counter declines conservatively).
			cf.markUncertain()
			return unsupported(fmt.Sprintf("map key %q: non-ASCII case-fold uncertainty against string-literal-union arms", key))
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
//     null's 110. A flagged arm (extra keys, substring match, object→map, or an
//     Mcoerce-b FloatToInt / StringToFloat / StringToBool / DefaultButHadValue
//     conversion) DECLINES (it might be an M3 scored win for null or another
//     arm) — so optional int|null with "123" claims (clean direct parse) while
//     the same with 1.6 declines (FloatToInt).
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
		if arm.isUncertain() {
			// The arm matched, but its verdict hinged on a non-ASCII case fold
			// native cannot prove equals BAML — decline rather than risk a
			// claim BAML would resolve differently (to the arm, or to null).
			return nil, unsupported("optional single-arm union: non-ASCII case-fold uncertainty in the non-null arm")
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
// variant leniently coerces (the no-over-claim rule). It counts per-variant
// successes through coerceLiteral itself — so string literals are evaluated
// with the actual match_string (a fuzzy/substring hit counts), and int/bool
// literals now count their Mcoerce-b lenient coercion too (a numeric string or
// rounded float that equals the literal, a casefold/substring bool). This is
// the M2c union revisit: once int/bool leaves are lenient, a union that had one
// STRICT success can gain a second LENIENT one, so counting through the lenient
// coerceLiteral is load-bearing. Two-plus lenient successes mean BAML would run scored pick_best
// — e.g. input "foobar" substring-matching both "foo" and "bar" arms even
// though the literal VALUES were proven match-disjoint at the gate — so
// native DECLINES (M3). Zero successes decline too. When requireClean (nullable
// union), a winner that only substring-matched (SubstringMatch flag) declines —
// the null arm could score lower. The single winner is re-coerced through
// coerceLiteral so the emitted form matches the single-literal path exactly.
func coerceLiteralUnion(variants []schema.Type, input value, requireClean bool, cf *coerceFlags) (json.RawMessage, error) {
	matched := -1
	count := 0
	unc := &coerceFlags{}
	for i := range variants {
		_, err := coerceLiteral(variants[i].Literal, input, unc)
		switch {
		case err == nil:
			matched = i
			count++
		case !declinableChildError(err):
			return nil, err // hard/invariant failure in an arm: propagate.
		}
		// A declinable error just means this arm did not match; keep counting.
	}
	if unc.isUncertain() {
		// Some arm's match verdict hinged on a non-ASCII case fold native
		// cannot prove equals BAML; a false-rejected arm would let native claim
		// the wrong lone winner, so DECLINE the whole union.
		return nil, unsupported("literal union: non-ASCII case-fold uncertainty in an arm (cannot prove verdict equals BAML)")
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
	unc := &coerceFlags{}
	for i := range variants {
		_, err := coerceClass(b, variants[i].Name, variants[i].Mode, input, unc)
		switch {
		case err == nil:
			matched = i
			count++
		case !declinableChildError(err):
			return nil, err // hard/invariant failure in an arm: propagate.
		}
		// A declinable error just means this arm did not match; keep counting.
	}
	if unc.isUncertain() {
		// Some arm's field-key or leaf match hinged on a non-ASCII case fold
		// native cannot prove equals BAML; a false-rejected arm would let native
		// claim the wrong lone winner, so DECLINE the whole union.
		return nil, unsupported("class union: non-ASCII case-fold uncertainty in an arm (cannot prove verdict equals BAML)")
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
