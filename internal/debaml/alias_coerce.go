package debaml

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"

	"github.com/invakid404/baml-rest/internal/schema"
)

// De-BAML Phase 3a/3c (recursive ALIASES) — the alias-specific EXACT scored coercion
// path for the served recursive-alias families (recursive_alias_profile.go): the
// five-arm NON-nullable `JSON` alias and the six-stored-variant NULLABLE `JsonValue`
// alias. Both run the SAME code, parameterized by [recAliasProfile]; the profile — not
// a name comparison — is what selects the two family-specific behaviours (the `float`
// arm and the first-class `null` arm).
//
// This is an ISOLATED faithful port of BAML v0.223's field_type.coerce for a
// TypeIR::RecursiveTypeAlias (coercer/ir_ref/coerce_alias.rs → coerce_union.rs →
// coerce_array.rs / coerce_map.rs), NOT a reuse of the conservative dynamic-safe
// coerceUnionSafe / coerceList / coerceMap. The generic dynamic paths deliberately
// UNDER-claim (decline when unsure) so they never over-claim on the open dynamic
// surface; this path SERVES exactly one proven family, so it reproduces BAML's lenient
// list/map/union byte-for-byte: a list DROPS an errored element (ArrayItemParseError)
// and succeeds, a map inserts value-first with IndexMap overwrite-at-first-position,
// and the non-nullable union coerces null through the list fallback (null -> []).
//
// The three semantics the generic paths get wrong for this family, reproduced here:
//
//   - SCORING: BAML's union / pick_best use the INHERENT types.rs BamlValueWithFlags
//     ::score() (list = own flags + Σ child scores; map = own flags + Σ(key conds +
//     value score); scalar = own flags). This IS native's [coerceFlags] inherent
//     model — reused here — NOT the score.rs WithScore trait (list/class ×10, map own
//     only), which the inherent method shadows at every union/pick_best call site.
//
//   - NULL -> [] (JSON ONLY): JSON is a NON-nullable union, so null does not
//     short-circuit; each arm coerces null (int/string/bool/map error), and the list
//     arm SingleToArray-wraps the null whose element re-enters coerce_alias on the SAME
//     ("JSON", null) pair already on the active coerce set -> a circular-reference error
//     that coerce_array records as ArrayItemParseError and drops -> the EMPTY list wins.
//
//   - NULL -> null (JsonValue ONLY): JsonValue IS nullable, so try_cast_union's
//     OPTIONAL fast path (coerce_union.rs:19-35) returns Null at score 0 BEFORE any
//     non-null arm is tried. It therefore NEVER reaches the list SingleToArray
//     fallback: a JsonValue null is a first-class typed null ([akNull], public `null`,
//     a typed-nil *Union6 in the generated carrier), NOT the JSON `[]` trap. The
//     lenient pass still models BAML's iter_include_null (the six non-null variants
//     plus an implicit null candidate LAST, DefaultButHadValue score 110), so the port
//     stays exact even though a nullable union can only reach it for a NON-null input.
//
//   - INT vs FLOAT (JsonValue ONLY): with both an `int` (arm 0) and a `float` (arm 1)
//     arm, the winner is the FIRST score-0 strict cast — `int` accepts a number only
//     through as_i64, `float` through as_f64 — so `1` selects int while `1.0`/`1.5`
//     select float. Which numbers have as_i64 Some is decided by BAML's TWO number
//     parse paths; alias_number.go documents and reproduces them. The public float
//     bytes are Go json.Marshal of the coerced float64, matching BAML's
//     CFFI-f64 -> Go float64 -> generated Union6.MarshalJSON, so the input's lexical
//     spelling is intentionally lost (`1.0` -> `1`, `3.0` -> `3`).
//
//   - MAP ORDER: coercion retains BAML's IndexMap insertion order with last-value-
//     wins-at-first-position ([orderedAliasMap]); the FINAL /call bytes and the
//     generated Go value are SORTED-public (encoding/json.Marshal of the equivalent
//     Go map[string]any), matching the static callback (Parse.<Method> then
//     json.Marshal on the generated types.JSON union). [aliasValue.marshalPublic]
//     bridges ordered-internal -> sorted-public.
//
// The tagged (kind, value) pair-guard (pair_guard.go) and the union_variant_hint
// (coerceCtx.hint) are threaded through every recursive descent, no depth cap.

// aliasKind tags which arm of the alias union an [aliasValue] holds. The first five
// are the Phase-3a `JSON` arms; [akFloat] and [akNull] are the two Phase-3c
// `JsonValue` additions and are UNREACHABLE for the `JSON` family (which has neither
// a float arm nor a nullable union).
type aliasKind uint8

const (
	akInt aliasKind = iota
	akString
	akBool
	akArray
	akMap
	// akFloat is the JsonValue `float` arm: a Go float64 whose public bytes come from
	// json.Marshal (Go's shortest-round-trip float formatting), byte-identical to the
	// generated Union6.MarshalJSON of BAML's decoded f64.
	akFloat
	// akNull is the JsonValue first-class typed NULL — a PRESENT value that projects
	// to Go nil / JSON `null`, deliberately distinct from an absent value and from the
	// JSON family's null -> [] list fallback.
	akNull
)

// aliasValue is the native ordered internal carrier for a coerced alias value. The
// object arm ([orderedAliasMap]) retains BAML's IndexMap insertion order (with
// overwrite-at-first-position) for the ordered-tree test helper and for coercion
// semantics; the public bytes sort it (see [aliasValue.marshalPublic]).
type aliasValue struct {
	kind aliasKind
	i    int64
	f    float64
	s    string
	b    bool
	arr  []aliasValue
	obj  *orderedAliasMap
}

// orderedAliasMap is the non-generic ordered map carrier (v2 scope §1): entries hold
// insertion order, index is a non-authoritative lookup cache (NEVER the serialization
// authority). A duplicate key OVERWRITES the value at the first key's position
// (BAML IndexMap::insert), it does NOT append or reorder.
type orderedAliasMap struct {
	entries []aliasEntry
	index   map[string]int
}

type aliasEntry struct {
	key string
	val aliasValue
}

func newOrderedAliasMap() *orderedAliasMap {
	return &orderedAliasMap{index: make(map[string]int)}
}

// put inserts key->val with BAML's IndexMap semantics: a new key appends an entry at
// the end (recording its position); a duplicate key REPLACES the value at the first
// occurrence's position without moving it (last-value-wins-in-first-position).
func (m *orderedAliasMap) put(key string, val aliasValue) {
	if i, ok := m.index[key]; ok {
		m.entries[i].val = val
		return
	}
	m.index[key] = len(m.entries)
	m.entries = append(m.entries, aliasEntry{key: key, val: val})
}

// toAny materialises the ordered internal tree into a Go `any`. The object arm becomes
// a map[string]any — whose keys encoding/json.Marshal SORTS lexically, so the public
// bytes are sorted regardless of the internal insertion order. An empty array/map
// stays a non-nil []any{}/map[string]any so it marshals as []/{} (never null).
func (av aliasValue) toAny() any {
	switch av.kind {
	case akInt:
		return av.i
	case akFloat:
		// Public-project the coerced float64 through Go json.Marshal — the SAME encoder
		// the generated Union6.MarshalJSON applies to BAML's decoded f64 pointer — so
		// exponent/boundary spellings match byte-for-byte and the provider's lexical
		// number spelling is intentionally NOT preserved.
		return av.f
	case akNull:
		// A PRESENT typed null: a nil `any` marshals as JSON `null`, both at the root
		// and as a list element / map value.
		return nil
	case akString:
		return av.s
	case akBool:
		return av.b
	case akArray:
		out := make([]any, len(av.arr))
		for i := range av.arr {
			out[i] = av.arr[i].toAny()
		}
		return out
	case akMap:
		m := make(map[string]any, len(av.obj.entries))
		for i := range av.obj.entries {
			m[av.obj.entries[i].key] = av.obj.entries[i].val.toAny()
		}
		return m
	default:
		return nil
	}
}

// marshalPublic emits the BAML-canonical public bytes: encoding/json.Marshal of the
// equivalent Go value — SORTED map keys and HTML escaping (< > & -> < >
// &) — byte-identical to the generated static callback (Parse.<Method> then
// json.Marshal on the generated types.JSON / types.JsonValue union).
func (av aliasValue) marshalPublic() (json.RawMessage, error) {
	return json.Marshal(av.toAny())
}

// errNegativeZeroFloat is the NARROW value-scoped decline for a coerced NEGATIVE ZERO on
// the `JsonValue` float arm. It is the ONE finite f64 the served seam cannot carry
// byte-exactly, and the reason is BAML's own generated union, not native's coercion:
//
//	Union6.UnmarshalJSON tries `json.Unmarshal(data, &variant_Int)` FIRST, and the bytes
//	`-0` unmarshal cleanly into an int64 as 0. So the canonical FinalJSON `-0` — which IS
//	byte-identical to BAML's own json.Marshal of its Float(-0.0) arm — decodes back into
//	the INT arm and re-marshals as `0`, while BAML's CFFI-decoded value stays Float(-0.0)
//	and re-marshals as `-0`. The generated carrier is simply not injective on the sign of
//	zero (every other finite f64 round-trips: an integral float that overflows i64 fails
//	the int attempt and lands on the float arm, and any `.`/`e` spelling does too).
//
// Emitting `-0.0` instead would round-trip, but it would break the load-bearing contract
// that native's FinalJSON is BYTE-IDENTICAL to BAML's Parse+json.Marshal, so the scope's
// rule applies verbatim: do not reshape the emitted bytes to work around a formatting
// boundary — leave the VALUE declined until it has its own proof. A decline here is the
// ordinary pre-claim sentinel: the route falls back to BAML, which produces `-0`.
//
// It is reachable ONLY for the `JsonValue` family (the `JSON` family has no float arm)
// and ONLY for a STRICT-parsed `-0`/`-0.0`; a fixing-parsed `-0` is the INTEGER 0
// (alias_number.go) and serves natively.
var errNegativeZeroFloat = unsupported("alias: coerced negative-zero float (the generated union carrier decodes `-0` back into its int arm, so native cannot prove the served bytes)")

// aliasHasNegativeZero reports whether the coerced tree contains a float arm holding a
// NEGATIVE ZERO anywhere (root, list element, or map value). See [errNegativeZeroFloat].
func aliasHasNegativeZero(av aliasValue) bool {
	switch av.kind {
	case akFloat:
		return av.f == 0 && math.Signbit(av.f)
	case akArray:
		for i := range av.arr {
			if aliasHasNegativeZero(av.arr[i]) {
				return true
			}
		}
	case akMap:
		if av.obj != nil {
			for i := range av.obj.entries {
				if aliasHasNegativeZero(av.obj.entries[i].val) {
					return true
				}
			}
		}
	}
	return false
}

// coerceAliasTree is the ordered-internal entry (also used by the ordered-tree test
// helper): field_type.coerce for the alias — try_cast first (a way to exit early),
// else the lenient coerce.
func coerceAliasTree(b *schema.Bundle, prof recAliasProfile, input value, cctx *coerceCtx) (aliasValue, error) {
	av, _, _, err := aliasCoerceValue(b, prof, input, cctx)
	return av, err
}

// aliasCoerceValue ports field_type.coerce for TypeIR::RecursiveTypeAlias: it runs the
// alias try_cast (an early exit) and, only if that finds no match, the lenient alias
// coerce. It returns the coerced value, its coerceFlags (score + pick_best
// discriminators, for an enclosing scored context), and the winning union-arm index
// (the outermost UnionMatch, carried as the next array sibling's hint). The arm index
// is [aliasArmNone] when the winner is the IMPLICIT null arm of a nullable union —
// that arm has no stored-variant index, so it must never become a sibling hint.
func aliasCoerceValue(b *schema.Bundle, prof recAliasProfile, input value, cctx *coerceCtx) (aliasValue, *coerceFlags, int, error) {
	if av, f, idx, ok, err := tryCastAlias(b, prof, input, cctx); err != nil {
		return aliasValue{}, nil, aliasArmNone, err
	} else if ok {
		return av, f, idx, nil
	}
	return coerceAlias(b, prof, input, cctx)
}

// aliasArmNone is the "no ordered non-null arm" index. It is returned for the implicit
// NULL arm of a nullable union (which is not one of the stored variants) and on error,
// and it is what keeps the null arm from being carried as the next array sibling's
// union_variant_hint.
const aliasArmNone = -1

// aliasVariants resolves the alias to its ordered union arms via
// Bundle.FindRecursiveAlias (the exact fingerprint guarantees the shape: five arms for
// `JSON`, six stored non-null variants for `JsonValue`). The parser profile has already
// proven the alias exists and is well-formed, so a lookup miss or a non-union target is
// an invariant failure.
func aliasVariants(b *schema.Bundle, prof recAliasProfile) ([]schema.Type, error) {
	def, ok := b.FindRecursiveAlias(prof.aliasName)
	if !ok {
		return nil, fmt.Errorf("debaml: recursive alias %q not found", prof.aliasName)
	}
	if def.Target.Kind != schema.TypeUnion || def.Target.Union == nil {
		return nil, fmt.Errorf("debaml: recursive alias %q target is not a union", prof.aliasName)
	}
	return def.Target.Union.Variants, nil
}

// tryCastAlias ports try_cast_alias (ir_ref/coerce_alias.rs): the try_cast pair-guard
// (a repeat on the active TRY_CAST set is an ordinary no-match, None), then resolves
// the alias and try_casts the resolved union. Returns (value, flags, armIdx, matched,
// err); err is only an invariant failure.
func tryCastAlias(b *schema.Bundle, prof recAliasProfile, input value, cctx *coerceCtx) (aliasValue, *coerceFlags, int, bool, error) {
	if cctx.tryCastHasAlias(prof.aliasName, input) {
		return aliasValue{}, nil, aliasArmNone, false, nil
	}
	child := cctx.enterTryCastAlias(prof.aliasName, input)
	variants, err := aliasVariants(b, prof)
	if err != nil {
		return aliasValue{}, nil, aliasArmNone, false, err
	}
	return tryCastAliasUnion(b, prof, variants, input, child)
}

// coerceAlias ports coerce_alias (ir_ref/coerce_alias.rs): the coerce pair-guard (a
// repeat on the active COERCE set is a CLAIMED circular-reference error), then resolves
// the alias and coerces the resolved union.
func coerceAlias(b *schema.Bundle, prof recAliasProfile, input value, cctx *coerceCtx) (aliasValue, *coerceFlags, int, error) {
	if cctx.coerceHasAlias(prof.aliasName, input) {
		return aliasValue{}, nil, aliasArmNone, errCircularReference(prof.aliasName)
	}
	child := cctx.enterCoerceAlias(prof.aliasName, input)
	variants, err := aliasVariants(b, prof)
	if err != nil {
		return aliasValue{}, nil, aliasArmNone, err
	}
	return coerceAliasUnion(b, prof, variants, input, child)
}

// tryCastAliasUnion ports try_cast_union (coerce_union.rs) for an alias union. For a
// NULLABLE union (JsonValue) a JSON null takes the OPTIONAL fast path FIRST — Null at
// score 0, returned before any non-null arm is tried (coerce_union.rs:19-35) — which
// is exactly why JsonValue null is a typed null and never the JSON list fallback. For
// a non-null input (and for the whole non-nullable JSON family) it tries the hint arm
// first (score-0 win), then the non-null arms IN ORDER; the FIRST score-0 arm wins
// immediately; otherwise the (>=1-scored) matches go to pick_best. The arms are input-
// type disjoint (a number reaches int-then-float, and as_i64/as_f64 are mutually
// exclusive by alias_number.go's model), so at most one arm try_casts a given value.
func tryCastAliasUnion(b *schema.Bundle, prof recAliasProfile, variants []schema.Type, input value, cctx *coerceCtx) (aliasValue, *coerceFlags, int, bool, error) {
	// NULLABLE fast path (JsonValue): try_cast_union returns Null with empty conditions
	// BEFORE iterating the non-null options. Score 0, no stored-arm index.
	if prof.nullable && input.kind == valNull {
		return aliasValue{kind: akNull}, &coerceFlags{kind: candNull, hasUnionMatch: true}, aliasArmNone, true, nil
	}
	// Hint fast path: the previous array sibling's winning arm, tried first.
	if cctx.hint != nil && *cctx.hint >= 0 && *cctx.hint < len(variants) {
		if av, f, ok, err := aliasTryCastArm(b, prof, variants[*cctx.hint], input, cctx); err != nil {
			return aliasValue{}, nil, aliasArmNone, false, err
		} else if ok && f.score == 0 {
			return av, f, *cctx.hint, true, nil
		}
	}
	var vals []aliasValue
	var cands []candidate
	for i := range variants {
		av, f, ok, err := aliasTryCastArm(b, prof, variants[i], input, cctx)
		if err != nil {
			return aliasValue{}, nil, aliasArmNone, false, err
		}
		if !ok {
			continue
		}
		if f.score == 0 {
			return av, f, i, true, nil // first score-0 wins immediately
		}
		vals = append(vals, av)
		cands = append(cands, f.toCandidate(nil, i))
	}
	if len(cands) == 0 {
		return aliasValue{}, nil, aliasArmNone, false, nil // no arm try_casts -> lenient pass
	}
	if len(cands) == 1 {
		return vals[0], aliasCandFlags(cands[0]), cands[0].originIndex, true, nil
	}
	idx, err := pickBest(true, cands)
	if err != nil {
		return aliasValue{}, nil, aliasArmNone, false, err
	}
	return vals[idx], aliasCandFlags(cands[idx]), cands[idx].originIndex, true, nil
}

// coerceAliasUnion ports coerce_union (coerce_union.rs) for an alias union: the hint
// arm's coerce (score-0 early win), then each arm in order (first score-0 wins),
// otherwise pick_best over every successful arm. For a NULLABLE union the option list
// is BAML's iter_include_null — the ordered stored variants followed by an IMPLICIT
// NULL option LAST (score 0 for a null input, DefaultButHadValue 110 otherwise) — so
// the port stays exact even though a nullable union reaches this pass only for a
// non-null input (a null already won the try_cast fast path above).
//
// A per-arm error (including a circular-reference from the pair-guard, and an arm
// native declines because it cannot prove BAML's bytes) is EXCLUDED like BAML's Err
// arm — safe for these families because the excluded arm can never be the winner (the
// list arm always succeeds, and a stringified-number arm always loses to the
// int/float arm). The list arm always succeeds, so there is always at least one
// candidate.
func coerceAliasUnion(b *schema.Bundle, prof recAliasProfile, variants []schema.Type, input value, cctx *coerceCtx) (aliasValue, *coerceFlags, int, error) {
	// Hint fast path.
	if cctx.hint != nil && *cctx.hint >= 0 && *cctx.hint < len(variants) {
		if av, f, err := aliasCoerceArm(b, prof, variants[*cctx.hint], input, cctx); err == nil && f.score == 0 && !f.isUncertain() {
			return av, f, *cctx.hint, nil
		}
	}
	var vals []aliasValue
	var arms []int
	var cands []candidate
	for i := range variants {
		av, f, err := aliasCoerceArm(b, prof, variants[i], input, cctx)
		if err != nil || f.isUncertain() {
			// Excluded arm (BAML Err, or a value native cannot prove byte-exact). The
			// winner is never an excluded arm for these families, so exclusion is safe.
			continue
		}
		if f.score == 0 {
			return av, f, i, nil // first score-0 wins immediately
		}
		vals = append(vals, av)
		arms = append(arms, i)
		cands = append(cands, f.toCandidate(nil, i))
	}
	// iter_include_null: the implicit NULL option, appended LAST for a nullable union.
	// Its origin index is the count of preceding non-null variants, and it is reported
	// as [aliasArmNone] so it can never be carried as an array sibling's hint.
	if prof.nullable {
		// coercePrimitiveNull always succeeds (BAML's null target resolves every value);
		// it is called for its SCORE — 0 for a null input, DefaultButHadValue 110 for a
		// non-null one — so the same first-score-0 rule applies to this option too.
		nf := &coerceFlags{targetIsUnion: true}
		_, _ = coercePrimitiveNull(input, nf)
		if nf.score == 0 {
			return aliasValue{kind: akNull}, nf, aliasArmNone, nil
		}
		vals = append(vals, aliasValue{kind: akNull})
		arms = append(arms, aliasArmNone)
		cands = append(cands, nullCandidate(len(variants)))
	}
	if len(cands) == 0 {
		return aliasValue{}, nil, aliasArmNone, unsupported("alias union: no arm succeeded")
	}
	idx, err := pickBest(true, cands)
	if err != nil {
		return aliasValue{}, nil, aliasArmNone, err
	}
	return vals[idx], aliasCandFlags(cands[idx]), arms[idx], nil
}

// aliasCandFlags rebuilds a coerceFlags carrying the winning candidate's score and
// discriminators plus the union's UnionMatch (score 0), so an enclosing scored context
// (a list element / map value) folds the exact winner score.
func aliasCandFlags(c candidate) *coerceFlags {
	f := &coerceFlags{}
	f.absorb(c)
	f.hasUnionMatch = true
	return f
}

// aliasTryCastArm dispatches one alias union arm's try_cast. Leaf arms
// (int/float/string/bool) strict-match by native JSON type (score 0); the list arm
// try_casts every element (score = Σ element scores); the map arm try_casts every
// value (ObjectToMap score 1 + Σ value scores).
func aliasTryCastArm(b *schema.Bundle, prof recAliasProfile, armT schema.Type, input value, cctx *coerceCtx) (aliasValue, *coerceFlags, bool, error) {
	switch armT.Kind {
	case schema.TypePrimitive:
		av, ok := aliasTryCastLeaf(armT.Primitive, input)
		if !ok {
			return aliasValue{}, nil, false, nil
		}
		return av, &coerceFlags{kind: candScalar}, true, nil
	case schema.TypeList:
		return aliasTryCastArray(b, prof, input, cctx)
	case schema.TypeMap:
		return aliasTryCastMap(b, prof, input, cctx)
	default:
		return aliasValue{}, nil, false, fmt.Errorf("debaml: unexpected alias arm kind %q", armT.Kind)
	}
}

// aliasTryCastLeaf strict-casts a leaf arm (score 0), reproducing BAML's strict
// coerce_primitive (coerce_primitive.rs:47-113):
//
//   - int wants a JSON number whose as_i64 is Some. Native's as_i64 is
//     [parseI64Rust] on the raw token MINUS the tokens a strict (serde) parse stores
//     as an f64 ([value.numSerdeFloat] — `-0`, an out-of-i64-range integer, any
//     `.`/`e` form); see alias_number.go. For the `JSON` family numSerdeFloat is never
//     set, so this is byte-identical to the Phase-3a behaviour (a float-valued or
//     overflow number falls to the lenient FloatToInt path).
//   - float (JsonValue only) wants a JSON number whose as_f64 is Some — every stored
//     jsonish Number, since a token that cannot become a FINITE f64 never becomes a
//     Number at all (it stays an unquoted string).
//   - string wants a JSON string, bool wants a JSON bool.
func aliasTryCastLeaf(p schema.PrimitiveKind, input value) (aliasValue, bool) {
	switch p {
	case schema.PrimitiveInt:
		if input.kind == valNumber && !input.numSerdeFloat {
			if v, ok := parseI64Rust(input.numV.String()); ok {
				return aliasValue{kind: akInt, i: v}, true
			}
		}
	case schema.PrimitiveFloat:
		if input.kind == valNumber {
			if f, ok := parseF64Rust(input.numV.String()); ok && isFiniteFloat(f) {
				return aliasValue{kind: akFloat, f: f}, true
			}
		}
	case schema.PrimitiveString:
		if input.kind == valString {
			return aliasValue{kind: akString, s: input.strV}, true
		}
	case schema.PrimitiveBool:
		if input.kind == valBool {
			return aliasValue{kind: akBool, b: input.boolV}, true
		}
	}
	return aliasValue{}, false
}

// isFiniteFloat reports whether f is a finite f64 — the values serde_json::Number can
// hold (Number::from_f64 returns None for NaN/±Inf), and therefore the only values the
// `float` arm can carry. It is also what guarantees [aliasValue.marshalPublic] never
// hits encoding/json's "unsupported value" error on the float arm.
func isFiniteFloat(f float64) bool {
	return !math.IsNaN(f) && !math.IsInf(f, 0)
}

// aliasTryCastArray ports try_cast_array (coerce_array.rs): a JSON array whose every
// element try_casts (fail-fast) -> a list scoring Σ element scores (0 for a clean
// scalar array; nonzero when an element is a nested map). An empty array try_casts to
// [] (score 0). The union hint is carried across siblings; an element that won on the
// IMPLICIT null arm carries NO hint (it has no stored-variant index).
func aliasTryCastArray(b *schema.Bundle, prof recAliasProfile, input value, cctx *coerceCtx) (aliasValue, *coerceFlags, bool, error) {
	if input.kind != valArray {
		return aliasValue{}, nil, false, nil
	}
	out := make([]aliasValue, 0, len(input.arrV))
	score := 0
	var lastHint *int
	for i := range input.arrV {
		child := cctx.enterScopeWithHint(lastHint)
		av, f, idx, ok, err := tryCastAlias(b, prof, input.arrV[i], child)
		if err != nil {
			return aliasValue{}, nil, false, err
		}
		if !ok {
			return aliasValue{}, nil, false, nil // fail-fast
		}
		score += f.score
		out = append(out, av)
		lastHint = aliasSiblingHint(idx)
	}
	return aliasValue{kind: akArray, arr: out}, &coerceFlags{kind: candList, score: score, itemsEmpty: len(out) == 0}, true, nil
}

// aliasSiblingHint converts a winning arm index into the next array sibling's
// union_variant_hint: the index itself for an ordered stored variant, and NIL for
// [aliasArmNone] — the implicit null arm of a nullable union carries no
// stored-variant index, so it must not seed a hint (scope: the implicit null arm must
// not accidentally become a sibling hint).
func aliasSiblingHint(idx int) *int {
	if idx == aliasArmNone {
		return nil
	}
	h := idx
	return &h
}

// aliasTryCastMap ports try_cast_map (coerce_map.rs): a JSON object whose every value
// try_casts (fail-fast) -> a map carrying ObjectToMap (score 1) + Σ value scores.
// Entries insert in input order with IndexMap overwrite. try_cast_map uses the SAME
// ctx per value (no enter_scope), matching stock BAML.
func aliasTryCastMap(b *schema.Bundle, prof recAliasProfile, input value, cctx *coerceCtx) (aliasValue, *coerceFlags, bool, error) {
	if input.kind != valObject {
		return aliasValue{}, nil, false, nil
	}
	m := newOrderedAliasMap()
	score := 1 // ObjectToMap
	for i := range input.objV {
		av, f, _, ok, err := tryCastAlias(b, prof, input.objV[i].val, cctx)
		if err != nil {
			return aliasValue{}, nil, false, err
		}
		if !ok {
			return aliasValue{}, nil, false, nil // fail-fast
		}
		score += f.score
		m.put(input.objV[i].key, av)
	}
	return aliasValue{kind: akMap, obj: m}, &coerceFlags{kind: candMap, score: score}, true, nil
}

// aliasCoerceArm dispatches one alias union arm's lenient coerce. Leaf arms REUSE the
// native primitive coercers (coercePrimitive*, which reproduce numeric-string parse,
// FloatToInt rounding, JsonToString stringification, and JSON-array array-to-singular
// with the exact score); the list/map arms recurse through the faithful alias coercers.
func aliasCoerceArm(b *schema.Bundle, prof recAliasProfile, armT schema.Type, input value, cctx *coerceCtx) (aliasValue, *coerceFlags, error) {
	switch armT.Kind {
	case schema.TypePrimitive:
		return aliasCoerceLeaf(armT.Primitive, input)
	case schema.TypeList:
		return aliasCoerceArray(b, prof, input, cctx)
	case schema.TypeMap:
		return aliasCoerceMap(b, prof, input, cctx)
	default:
		return aliasValue{}, nil, fmt.Errorf("debaml: unexpected alias arm kind %q", armT.Kind)
	}
}

// aliasCoerceLeaf coerces a leaf arm by reusing the native primitive coercer, then
// converts the emitted JSON bytes back into an aliasValue. targetIsUnion=true selects
// the union-arm array-to-singular scoring (UnionMatch not FirstMatch), matching a leaf
// coerced as a union arm.
func aliasCoerceLeaf(p schema.PrimitiveKind, input value) (aliasValue, *coerceFlags, error) {
	f := &coerceFlags{targetIsUnion: true}
	var out json.RawMessage
	var err error
	switch p {
	case schema.PrimitiveInt:
		out, err = coercePrimitiveInt(input, f)
	case schema.PrimitiveFloat:
		out, err = coercePrimitiveFloat(input, f)
	case schema.PrimitiveString:
		out, err = coercePrimitiveString(input, f)
	case schema.PrimitiveBool:
		out, err = coercePrimitiveBool(input, f)
	default:
		return aliasValue{}, nil, fmt.Errorf("debaml: unexpected alias leaf primitive %q", p)
	}
	if err != nil {
		return aliasValue{}, nil, err
	}
	av, cerr := rawToAliasScalar(p, out)
	if cerr != nil {
		return aliasValue{}, nil, cerr
	}
	return av, f, nil
}

// rawToAliasScalar converts a leaf coercer's emitted JSON bytes into an aliasValue.
// The FLOAT arm re-reads the emitted number into a float64 rather than keeping the
// lexeme: the public bytes must come from Go json.Marshal of the coerced float64 (the
// generated Union6.MarshalJSON contract), NOT from the provider's spelling.
func rawToAliasScalar(p schema.PrimitiveKind, out json.RawMessage) (aliasValue, error) {
	switch p {
	case schema.PrimitiveInt:
		n, err := strconv.ParseInt(string(out), 10, 64)
		if err != nil {
			return aliasValue{}, fmt.Errorf("debaml: alias int arm: %w", err)
		}
		return aliasValue{kind: akInt, i: n}, nil
	case schema.PrimitiveFloat:
		fv, ok := parseF64Rust(string(out))
		if !ok || !isFiniteFloat(fv) {
			return aliasValue{}, fmt.Errorf("debaml: alias float arm: %q is not a finite f64", string(out))
		}
		return aliasValue{kind: akFloat, f: fv}, nil
	case schema.PrimitiveString:
		var s string
		if err := json.Unmarshal(out, &s); err != nil {
			return aliasValue{}, fmt.Errorf("debaml: alias string arm: %w", err)
		}
		return aliasValue{kind: akString, s: s}, nil
	case schema.PrimitiveBool:
		return aliasValue{kind: akBool, b: string(out) == "true"}, nil
	default:
		return aliasValue{}, fmt.Errorf("debaml: unexpected alias scalar primitive %q", p)
	}
}

// aliasCoerceArray ports coerce_array (coerce_array.rs): an ARRAY coerces every element
// (dropping an errored element as ArrayItemParseError and succeeding), a NON-array is
// SingleToArray-wrapped as one implied element. The union hint carries across siblings;
// the implied element uses a hint-reset scope. This is where the `JSON` family's
// null -> [] emerges: the implied element re-enters coerce_alias on the same
// ("JSON", input) pair -> a circular-reference error -> dropped -> the empty list. The
// NULLABLE `JsonValue` family never reaches it for a null (the try_cast optional fast
// path already returned a typed null).
func aliasCoerceArray(b *schema.Bundle, prof recAliasProfile, input value, cctx *coerceCtx) (aliasValue, *coerceFlags, error) {
	out := make([]aliasValue, 0, len(input.arrV))
	score := 0
	errCount := 0
	if input.kind != valArray {
		// SingleToArray (score 1): wrap the non-array as one implied element through a
		// hint-reset scope. For these families the implied element is the SAME input value
		// (already on the active coerce set), so its coerce_alias hits the circular-
		// reference guard and the element is dropped as ArrayItemParseError(0) -> the
		// empty list. The success branch is retained for faithfulness.
		score++
		child := cctx.enterScope()
		if av, f, _, err := aliasCoerceValue(b, prof, input, child); err != nil {
			score++ // ArrayItemParseError(0) = 1 + 0
			errCount++
		} else {
			out = append(out, av)
			score += f.score
		}
		return aliasValue{kind: akArray, arr: out}, &coerceFlags{kind: candList, score: score, singleToArray: true, itemsEmpty: len(out) == 0, arrayItemErrors: errCount}, nil
	}
	var lastHint *int
	for i := range input.arrV {
		child := cctx.enterScopeWithHint(lastHint)
		av, f, idx, err := aliasCoerceValue(b, prof, input.arrV[i], child)
		if err != nil {
			score += 1 + i // ArrayItemParseError(i) = 1 + i (score.rs)
			errCount++
			continue
		}
		out = append(out, av)
		score += f.score
		lastHint = aliasSiblingHint(idx)
	}
	return aliasValue{kind: akArray, arr: out}, &coerceFlags{kind: candList, score: score, itemsEmpty: len(out) == 0, arrayItemErrors: errCount}, nil
}

// aliasCoerceMap ports coerce_map (coerce_map.rs): a JSON object -> ObjectToMap
// (score 1), coerce each VALUE FIRST via a hint-reset enter_scope(key) (an errored
// value adds MapValueParseError and skips the entry, no key coercion), then insert with
// IndexMap overwrite. The map key is a bare string so key coercion always succeeds
// (documented + tested); a non-object input is error_unexpected_type (excluded arm).
// The map's inherent score is ObjectToMap + Σ MapValueParseError + Σ (final) value
// scores.
func aliasCoerceMap(b *schema.Bundle, prof recAliasProfile, input value, cctx *coerceCtx) (aliasValue, *coerceFlags, error) {
	if input.kind != valObject {
		return aliasValue{}, nil, typeMismatch("map", input)
	}
	m := newOrderedAliasMap()
	scoreByKey := make(map[string]int, len(input.objV))
	score := 1 // ObjectToMap
	for i := range input.objV {
		key := input.objV[i].key
		child := cctx.enterScope() // enter_scope(key) resets the hint
		av, f, _, err := aliasCoerceValue(b, prof, input.objV[i].val, child)
		if err != nil {
			score++ // MapValueParseError = 1 (skip; no key coercion, no put)
			continue
		}
		// Key is a bare string: coercion is a no-op success (string key always valid).
		m.put(key, av)
		scoreByKey[key] = f.score
	}
	for _, s := range scoreByKey {
		score += s
	}
	return aliasValue{kind: akMap, obj: m}, &coerceFlags{kind: candMap, score: score}, nil
}
