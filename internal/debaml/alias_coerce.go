package debaml

import (
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/invakid404/baml-rest/internal/schema"
)

// De-BAML Phase 3a (recursive ALIASES) — the alias-specific EXACT scored coercion path
// for the served five-arm JSON family (recursive_alias_profile.go).
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
//   - NULL -> []: JSON is a NON-nullable union, so null does not short-circuit; each
//     arm coerces null (int/string/bool/map error), and the list arm SingleToArray-
//     wraps the null whose element re-enters coerce_alias on the SAME ("JSON", null)
//     pair already on the active coerce set -> a circular-reference error that
//     coerce_array records as ArrayItemParseError and drops -> the EMPTY list wins.
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

// aliasKind tags which arm of the JSON union an [aliasValue] holds.
type aliasKind uint8

const (
	akInt aliasKind = iota
	akString
	akBool
	akArray
	akMap
)

// aliasValue is the native ordered internal carrier for a coerced JSON value. The
// object arm ([orderedAliasMap]) retains BAML's IndexMap insertion order (with
// overwrite-at-first-position) for the ordered-tree test helper and for coercion
// semantics; the public bytes sort it (see [aliasValue.marshalPublic]).
type aliasValue struct {
	kind aliasKind
	i    int64
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
// json.Marshal on the generated types.JSON union).
func (av aliasValue) marshalPublic() (json.RawMessage, error) {
	return json.Marshal(av.toAny())
}

// coerceAliasFinal is the ParseStaticBundle entry for the admitted JSON alias family:
// it runs the exact alias coercion over the extracted candidate and returns the
// SORTED-public FinalJSON bytes. A FRESH coerceCtx (empty pair sets, nil hint) starts
// the recursion. A circular-reference error at the ROOT (an input structurally equal
// to a proper ancestor — unreachable for finite JSON) propagates as a CLAIMED parse
// failure, matching BAML; every other path succeeds (the list arm never errors).
func coerceAliasFinal(b *schema.Bundle, input value) (json.RawMessage, error) {
	av, err := coerceAliasTree(b, input, &coerceCtx{})
	if err != nil {
		return nil, err
	}
	return av.marshalPublic()
}

// coerceAliasTree is the ordered-internal entry (also used by the ordered-tree test
// helper): field_type.coerce for the JSON alias — try_cast first (a way to exit
// early), else the lenient coerce.
func coerceAliasTree(b *schema.Bundle, input value, cctx *coerceCtx) (aliasValue, error) {
	av, _, _, err := aliasCoerceValue(b, input, cctx)
	return av, err
}

// aliasCoerceValue ports field_type.coerce for TypeIR::RecursiveTypeAlias: it runs the
// alias try_cast (an early exit) and, only if that finds no match, the lenient alias
// coerce. It returns the coerced value, its coerceFlags (score + pick_best
// discriminators, for an enclosing scored context), and the winning JSON-union arm
// index (the outermost UnionMatch, carried as the next array sibling's hint).
func aliasCoerceValue(b *schema.Bundle, input value, cctx *coerceCtx) (aliasValue, *coerceFlags, int, error) {
	if av, f, idx, ok, err := tryCastAlias(b, input, cctx); err != nil {
		return aliasValue{}, nil, 0, err
	} else if ok {
		return av, f, idx, nil
	}
	return coerceAlias(b, input, cctx)
}

// aliasVariants resolves the JSON alias to its five ordered union arms via
// Bundle.FindRecursiveAlias (the exact fingerprint guarantees the shape). The parser
// profile has already proven the alias exists and is well-formed, so a lookup miss or
// a non-union target is an invariant failure.
func aliasVariants(b *schema.Bundle) ([]schema.Type, error) {
	def, ok := b.FindRecursiveAlias(recAliasJSONName)
	if !ok {
		return nil, fmt.Errorf("debaml: recursive alias %q not found", recAliasJSONName)
	}
	if def.Target.Kind != schema.TypeUnion || def.Target.Union == nil {
		return nil, fmt.Errorf("debaml: recursive alias %q target is not a union", recAliasJSONName)
	}
	return def.Target.Union.Variants, nil
}

// tryCastAlias ports try_cast_alias (ir_ref/coerce_alias.rs): the try_cast pair-guard
// (a repeat on the active TRY_CAST set is an ordinary no-match, None), then resolves
// the alias and try_casts the resolved union. Returns (value, flags, armIdx, matched,
// err); err is only an invariant failure.
func tryCastAlias(b *schema.Bundle, input value, cctx *coerceCtx) (aliasValue, *coerceFlags, int, bool, error) {
	if cctx.tryCastHasAlias(recAliasJSONName, input) {
		return aliasValue{}, nil, 0, false, nil
	}
	child := cctx.enterTryCastAlias(recAliasJSONName, input)
	variants, err := aliasVariants(b)
	if err != nil {
		return aliasValue{}, nil, 0, false, err
	}
	return tryCastAliasUnion(b, variants, input, child)
}

// coerceAlias ports coerce_alias (ir_ref/coerce_alias.rs): the coerce pair-guard (a
// repeat on the active COERCE set is a CLAIMED circular-reference error), then resolves
// the alias and coerces the resolved union.
func coerceAlias(b *schema.Bundle, input value, cctx *coerceCtx) (aliasValue, *coerceFlags, int, error) {
	if cctx.coerceHasAlias(recAliasJSONName, input) {
		return aliasValue{}, nil, 0, errCircularReference(recAliasJSONName)
	}
	child := cctx.enterCoerceAlias(recAliasJSONName, input)
	variants, err := aliasVariants(b)
	if err != nil {
		return aliasValue{}, nil, 0, err
	}
	return coerceAliasUnion(b, variants, input, child)
}

// tryCastAliasUnion ports try_cast_union (coerce_union.rs) for the JSON union (never
// optional, so no null fast path). It tries the hint arm first (score-0 win), then the
// non-null arms in order; the FIRST score-0 arm wins immediately; otherwise the
// (>=1-scored) matches go to pick_best. For the JSON union the arms are input-type
// disjoint, so at most one arm try_casts a given value.
func tryCastAliasUnion(b *schema.Bundle, variants []schema.Type, input value, cctx *coerceCtx) (aliasValue, *coerceFlags, int, bool, error) {
	// Hint fast path: the previous array sibling's winning arm, tried first.
	if cctx.hint != nil && *cctx.hint >= 0 && *cctx.hint < len(variants) {
		if av, f, ok, err := aliasTryCastArm(b, variants[*cctx.hint], input, cctx); err != nil {
			return aliasValue{}, nil, 0, false, err
		} else if ok && f.score == 0 {
			return av, f, *cctx.hint, true, nil
		}
	}
	var vals []aliasValue
	var cands []candidate
	for i := range variants {
		av, f, ok, err := aliasTryCastArm(b, variants[i], input, cctx)
		if err != nil {
			return aliasValue{}, nil, 0, false, err
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
		return aliasValue{}, nil, 0, false, nil // no arm try_casts -> lenient pass
	}
	if len(cands) == 1 {
		return vals[0], aliasCandFlags(cands[0]), cands[0].originIndex, true, nil
	}
	idx, err := pickBest(true, cands)
	if err != nil {
		return aliasValue{}, nil, 0, false, err
	}
	return vals[idx], aliasCandFlags(cands[idx]), cands[idx].originIndex, true, nil
}

// coerceAliasUnion ports coerce_union (coerce_union.rs) for the JSON union: the hint
// arm's coerce (score-0 early win), then each arm in order (first score-0 wins),
// otherwise pick_best over every successful arm. A per-arm error (including a
// circular-reference from the pair-guard, and an arm native declines because it cannot
// prove BAML's bytes) is EXCLUDED like BAML's Err arm — safe for this family because
// the excluded arm can never be the winner (the list arm always succeeds, and a
// stringified-number arm always loses to the int arm). The list arm always succeeds,
// so there is always at least one candidate.
func coerceAliasUnion(b *schema.Bundle, variants []schema.Type, input value, cctx *coerceCtx) (aliasValue, *coerceFlags, int, error) {
	// Hint fast path.
	if cctx.hint != nil && *cctx.hint >= 0 && *cctx.hint < len(variants) {
		if av, f, err := aliasCoerceArm(b, variants[*cctx.hint], input, cctx); err == nil && f.score == 0 && !f.isUncertain() {
			return av, f, *cctx.hint, nil
		}
	}
	var vals []aliasValue
	var cands []candidate
	for i := range variants {
		av, f, err := aliasCoerceArm(b, variants[i], input, cctx)
		if err != nil || f.isUncertain() {
			// Excluded arm (BAML Err, or a value native cannot prove byte-exact). The
			// winner is never an excluded arm for this family, so exclusion is safe.
			continue
		}
		if f.score == 0 {
			return av, f, i, nil // first score-0 wins immediately
		}
		vals = append(vals, av)
		cands = append(cands, f.toCandidate(nil, i))
	}
	if len(cands) == 0 {
		return aliasValue{}, nil, 0, unsupported("alias union: no arm succeeded")
	}
	idx, err := pickBest(true, cands)
	if err != nil {
		return aliasValue{}, nil, 0, err
	}
	return vals[idx], aliasCandFlags(cands[idx]), cands[idx].originIndex, nil
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

// aliasTryCastArm dispatches one JSON union arm's try_cast. Leaf arms (int/string/bool)
// strict-match by native JSON type (score 0); the list arm try_casts every element
// (score = Σ element scores); the map arm try_casts every value (ObjectToMap score 1 +
// Σ value scores).
func aliasTryCastArm(b *schema.Bundle, armT schema.Type, input value, cctx *coerceCtx) (aliasValue, *coerceFlags, bool, error) {
	switch armT.Kind {
	case schema.TypePrimitive:
		av, ok := aliasTryCastLeaf(armT.Primitive, input)
		if !ok {
			return aliasValue{}, nil, false, nil
		}
		return av, &coerceFlags{kind: candScalar}, true, nil
	case schema.TypeList:
		return aliasTryCastArray(b, input, cctx)
	case schema.TypeMap:
		return aliasTryCastMap(b, input, cctx)
	default:
		return aliasValue{}, nil, false, fmt.Errorf("debaml: unexpected alias arm kind %q", armT.Kind)
	}
}

// aliasTryCastLeaf strict-casts a leaf arm (score 0): int wants a JSON number that is
// an i64 (a float-valued or overflow number falls to the lenient FloatToInt path),
// string wants a JSON string, bool wants a JSON bool.
func aliasTryCastLeaf(p schema.PrimitiveKind, input value) (aliasValue, bool) {
	switch p {
	case schema.PrimitiveInt:
		if input.kind == valNumber {
			if v, ok := parseI64Rust(input.numV.String()); ok {
				return aliasValue{kind: akInt, i: v}, true
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

// aliasTryCastArray ports try_cast_array (coerce_array.rs): a JSON array whose every
// element try_casts (fail-fast) -> a list scoring Σ element scores (0 for a clean
// scalar array; nonzero when an element is a nested map). An empty array try_casts to
// [] (score 0). The union hint is carried across siblings.
func aliasTryCastArray(b *schema.Bundle, input value, cctx *coerceCtx) (aliasValue, *coerceFlags, bool, error) {
	if input.kind != valArray {
		return aliasValue{}, nil, false, nil
	}
	out := make([]aliasValue, 0, len(input.arrV))
	score := 0
	var lastHint *int
	for i := range input.arrV {
		child := cctx.enterScopeWithHint(lastHint)
		av, f, idx, ok, err := tryCastAlias(b, input.arrV[i], child)
		if err != nil {
			return aliasValue{}, nil, false, err
		}
		if !ok {
			return aliasValue{}, nil, false, nil // fail-fast
		}
		score += f.score
		out = append(out, av)
		h := idx
		lastHint = &h
	}
	return aliasValue{kind: akArray, arr: out}, &coerceFlags{kind: candList, score: score, itemsEmpty: len(out) == 0}, true, nil
}

// aliasTryCastMap ports try_cast_map (coerce_map.rs): a JSON object whose every value
// try_casts (fail-fast) -> a map carrying ObjectToMap (score 1) + Σ value scores.
// Entries insert in input order with IndexMap overwrite. try_cast_map uses the SAME
// ctx per value (no enter_scope), matching stock BAML.
func aliasTryCastMap(b *schema.Bundle, input value, cctx *coerceCtx) (aliasValue, *coerceFlags, bool, error) {
	if input.kind != valObject {
		return aliasValue{}, nil, false, nil
	}
	m := newOrderedAliasMap()
	score := 1 // ObjectToMap
	for i := range input.objV {
		av, f, _, ok, err := tryCastAlias(b, input.objV[i].val, cctx)
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

// aliasCoerceArm dispatches one JSON union arm's lenient coerce. Leaf arms REUSE the
// native primitive coercers (coercePrimitive*, which reproduce numeric-string parse,
// FloatToInt rounding, JsonToString stringification, and JSON-array array-to-singular
// with the exact score); the list/map arms recurse through the faithful alias coercers.
func aliasCoerceArm(b *schema.Bundle, armT schema.Type, input value, cctx *coerceCtx) (aliasValue, *coerceFlags, error) {
	switch armT.Kind {
	case schema.TypePrimitive:
		return aliasCoerceLeaf(armT.Primitive, input)
	case schema.TypeList:
		return aliasCoerceArray(b, input, cctx)
	case schema.TypeMap:
		return aliasCoerceMap(b, input, cctx)
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
func rawToAliasScalar(p schema.PrimitiveKind, out json.RawMessage) (aliasValue, error) {
	switch p {
	case schema.PrimitiveInt:
		n, err := strconv.ParseInt(string(out), 10, 64)
		if err != nil {
			return aliasValue{}, fmt.Errorf("debaml: alias int arm: %w", err)
		}
		return aliasValue{kind: akInt, i: n}, nil
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
// the implied element uses a hint-reset scope. This is where null -> [] emerges: the
// implied element re-enters coerce_alias on the same ("JSON", input) pair -> a
// circular-reference error -> dropped -> the empty list.
func aliasCoerceArray(b *schema.Bundle, input value, cctx *coerceCtx) (aliasValue, *coerceFlags, error) {
	out := make([]aliasValue, 0, len(input.arrV))
	score := 0
	errCount := 0
	if input.kind != valArray {
		// SingleToArray (score 1): wrap the non-array as one implied element through a
		// hint-reset scope. For this family the implied element is the SAME input value
		// (already on the active coerce set), so its coerce_alias hits the circular-
		// reference guard and the element is dropped as ArrayItemParseError(0) -> the
		// empty list. The success branch is retained for faithfulness.
		score++
		child := cctx.enterScope()
		if av, f, _, err := aliasCoerceValue(b, input, child); err != nil {
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
		av, f, idx, err := aliasCoerceValue(b, input.arrV[i], child)
		if err != nil {
			score += 1 + i // ArrayItemParseError(i) = 1 + i (score.rs)
			errCount++
			continue
		}
		out = append(out, av)
		score += f.score
		h := idx
		lastHint = &h
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
func aliasCoerceMap(b *schema.Bundle, input value, cctx *coerceCtx) (aliasValue, *coerceFlags, error) {
	if input.kind != valObject {
		return aliasValue{}, nil, typeMismatch("map", input)
	}
	m := newOrderedAliasMap()
	scoreByKey := make(map[string]int, len(input.objV))
	score := 1 // ObjectToMap
	for i := range input.objV {
		key := input.objV[i].key
		child := cctx.enterScope() // enter_scope(key) resets the hint
		av, f, _, err := aliasCoerceValue(b, input.objV[i].val, child)
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
