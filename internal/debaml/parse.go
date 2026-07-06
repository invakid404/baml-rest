package debaml

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"unicode"

	"golang.org/x/text/cases"
	"golang.org/x/text/language"
	"golang.org/x/text/unicode/norm"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/internal/schema"
)

// Parse is the bamlutils.DeBAMLParseFunc implementation: the bounded
// native response parser. It lowers the dynamic output schema, validates
// it, then extracts and decodes a JSON candidate from the raw model text —
// strict first, then a conservative fixing pass (M2a) — and coerces it
// against the schema, returning the flattened dynamic output JSON (no
// DynamicProperties envelope).
//
// The cut-line (see package and method docs below) is deliberately narrow.
// Anything outside it returns bamlutils.ErrDeBAMLParseUnsupported so the
// caller falls back to BAML for that final parse:
//
//   - Stream parses (req.Stream==true) — native stream semantics are M4.
//   - Schemas that cannot be lowered/validated, or that use general
//     (multi-variant) unions, constraints, recursive aliases, or a map
//     whose key/value falls outside the clean M2b map subset (see
//     checkSupportedMapKey and coerceMap). Clean maps — object input,
//     exact key match, in-scope value — are claimed in input key order.
//   - Raw text whose JSON-looking candidate needs a repair outside the
//     conservative M2a fixing subset — comments, escapes, missing commas,
//     unterminated structures, multiple top-level values, … — which stays
//     BAML's job (see fix.go for the exact claimed subset).
//   - Raw text with no cleanly-claimable JSON candidate at all — no
//     JSON-looking content, or only an unterminated/incomplete structure —
//     which BAML may still recover, so native declines rather than claiming
//     a parse error (extraction never claims; only coercion does).
//
// A non-sentinel error is a CLAIMED native parse failure and propagates: a
// decoded value fails to coerce against the schema in a way BAML also
// rejects (e.g. a non-object where a multi-field class is required). The
// native-vs-BAML differential compares these claims against BAML, so drift
// surfaces rather than being masked behind a silent fallback.
func Parse(ctx context.Context, req bamlutils.DeBAMLParseRequest) (bamlutils.DeBAMLParseResult, error) {
	_ = ctx // M1 parsing is a local CPU operation; no cancellation points.

	if req.Stream {
		return bamlutils.DeBAMLParseResult{}, unsupported("stream parse")
	}
	if req.OutputSchema == nil {
		return bamlutils.DeBAMLParseResult{}, unsupported("nil output schema")
	}

	bundle, err := schema.FromDynamicOutputSchema(req.OutputSchema, schema.BuildOptions{})
	if err != nil {
		// A schema we cannot lower self-containedly (e.g. references to
		// static baml_src types the dynamic bundle does not carry) is out
		// of M1 scope — fall back to BAML rather than claim a parse error.
		return bamlutils.DeBAMLParseResult{}, unsupportedErr("lower schema", err)
	}
	if err := bundle.ValidateOutput(); err != nil {
		return bamlutils.DeBAMLParseResult{}, unsupportedErr("validate schema", err)
	}
	if err := checkSupported(bundle); err != nil {
		return bamlutils.DeBAMLParseResult{}, err
	}

	parsed, ok := extractCandidate(req.Raw)
	if !ok {
		// No cleanly-claimable JSON candidate: no JSON-looking content, a
		// repair outside the conservative M2a fixing subset, an unterminated
		// structure, or multiple top-level values. BAML may still recover any
		// of these, so DECLINE (fall back) rather than claim a parse error —
		// "could not find / complete a candidate" is never a claim.
		return bamlutils.DeBAMLParseResult{}, unsupported("no cleanly-claimable JSON candidate")
	}

	// Top-level coercion needs no cleanliness tracking (nil accumulator): a
	// nullable target's own null/clean decision is made inside coerceUnionSafe.
	out, err := coerce(bundle, bundle.Target, parsed, nil)
	if err != nil {
		// A candidate was decoded but does not coerce against the schema.
		// coerce returns ErrDeBAMLParseUnsupported where the failure is only
		// native being stricter than BAML's lenient coercers (so the caller
		// falls back); any other error is a CLAIMED parse failure BAML would
		// also hit (e.g. missing required field), propagated for parity.
		return bamlutils.DeBAMLParseResult{}, err
	}
	return bamlutils.DeBAMLParseResult{JSON: out}, nil
}

// Compile-time assertion that Parse satisfies the public callback type.
var _ bamlutils.DeBAMLParseFunc = Parse

// checkSupported reports whether every type reachable from the lowered
// bundle is inside the M1 coercion cut-line. It walks the synthetic
// target's class and every other reachable class (TypeClass/TypeEnum
// references are leaves here because the bundle lists each reachable
// definition as its own entry), returning a wrapped
// bamlutils.ErrDeBAMLParseUnsupported for the first out-of-scope feature.
//
// Cycles in the schema (a class that references itself) need no special
// handling: coercion is data-driven, so it descends only as deep as the
// finite JSON input and always terminates. Structural recursive aliases
// and explicitly-marked recursive classes are rejected anyway — the
// dynamic lowering never produces them, so their presence signals an
// unexpected shape.
func checkSupported(b *schema.Bundle) error {
	if len(b.StructuralRecursiveAliases) > 0 {
		return unsupported("structural recursive alias")
	}
	if len(b.RecursiveClasses) > 0 {
		return unsupported("recursive class")
	}
	for i := range b.Enums {
		if len(b.Enums[i].Constraints) > 0 {
			return unsupported("enum constraints")
		}
	}
	for i := range b.Classes {
		c := &b.Classes[i]
		if len(c.Constraints) > 0 {
			return unsupported("class constraints")
		}
		for j := range c.Fields {
			if err := checkSupportedType(b, c.Fields[j].Type); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkSupportedType walks one type tree, rejecting the kinds native does
// not coerce. Named class/enum references are leaves (their definitions are
// validated via the bundle's class/enum slices). It needs the bundle so a
// general union can be checked against the M2c safe-union families (which
// inspect each class variant's fields).
func checkSupportedType(b *schema.Bundle, t schema.Type) error {
	if len(t.Meta.Constraints) > 0 {
		return unsupported("type constraints")
	}
	switch t.Kind {
	case schema.TypePrimitive, schema.TypeLiteral, schema.TypeEnum, schema.TypeClass:
		return nil
	case schema.TypeList:
		if t.Elem == nil {
			return unsupported("list without element")
		}
		// A MULTI-ARM union as a LIST ELEMENT stays out of scope: BAML threads the
		// previous element's winning arm into the next element as
		// ctx.union_variant_hint (coerce_array.rs enter_scope_with_hint, the ONLY
		// setter of the hint) and tries that arm FIRST in both the try_cast and
		// lenient phases (coerce_union.rs), so an array like list<Color | string> can
		// pick a DIFFERENT arm per element than a hint-less native coercer (e.g. a
		// hinted string arm keeps an exact enum token as a string). The hint only
		// flows when the elements are THEMSELVES unions (the hint is the previous
		// element's OUTERMOST UnionMatch, which a map/class/list element never
		// carries), and map values and class fields RESET the hint (enter_scope /
		// visit_class_value_pair), so map<_, union> and class union fields stay in
		// scope. A single-non-null-arm optional element (T?) is safe — its only hint
		// is that one arm. M3d EVALUATED modeling the array union_variant_hint and
		// DELIBERATELY DEFERRED it (the conservative answer to the scope's open
		// question): faithfully reproducing the per-element hint carry-over and its
		// two-phase first-try semantics — and PROVING per-element parity for every
		// arm shape — is not something native can guarantee without over-claim risk,
		// so a multi-arm-union list element keeps DECLINING (over-decline is safe).
		if isMultiArmUnion(*t.Elem) {
			return unsupported("list element is a multi-arm union (array union_variant_hint deferred — over-claim risk)")
		}
		return checkSupportedType(b, *t.Elem)
	case schema.TypeUnion:
		if t.Union == nil {
			return unsupported("union without payload")
		}
		u := t.Union
		// A NULLABLE union always passes the gate for the null fast path: a
		// JSON-null input coerces to null (coerceUnionSafe) regardless of the
		// non-null arms — this must hold for ANY nullable union, including a
		// single-arm optional whose lone non-null arm is itself unsupported
		// (e.g. a nested/general-union arm or an out-of-scope map). Non-null
		// input is still decided at coerce time and never over-claims: a
		// single-non-null optional delegates to coerce on the lone arm (which
		// declines if that arm is unsupported), and a nullable multi-union
		// re-proves its non-null arm set is an M2c safe family. Checking
		// Nullable BEFORE the len==1 recursion is what makes the null claim
		// consistent across single-arm and multi-arm nullable unions.
		if u.Nullable {
			return nil
		}
		// A NON-nullable single-variant union collapses to its lone arm in
		// simplifyUnion, so this is effectively unreachable; recurse into the
		// arm defensively to mirror that collapse.
		if len(u.Variants) == 1 {
			return checkSupportedType(b, u.Variants[0])
		}
		// A non-nullable multi-union is in scope only when its variants form
		// one of the M2c safe families (homogeneous exact-literal union or
		// flat disjoint-key class union); checkSupportedUnionShape proves it.
		return checkSupportedUnionShape(b, u)
	case schema.TypeMap:
		// M2b CLAIMS clean maps: a JSON-object input coerced under a
		// map-key-safe key type and a value type that itself passes the
		// cut-line. The key must be checked SPECIALLY — not via the general
		// checkSupportedType, which rejects every union — because a
		// non-nullable union of string literals is a legal map key while
		// being an out-of-scope union everywhere else.
		if t.Key == nil || t.Value == nil {
			return unsupported("map without key or value")
		}
		if err := checkSupportedMapKey(*t.Key); err != nil {
			return err
		}
		return checkSupportedType(b, *t.Value)
	case schema.TypeRecursiveAlias:
		return unsupported("recursive alias")
	default:
		return unsupported(fmt.Sprintf("type kind %q", t.Kind))
	}
}

// checkSupportedMapKey reports whether t is a map key shape M2b coerces by
// EXACT match: a string primitive, an enum, a string literal, or a
// non-nullable union whose recursively-flattened members are all string
// literals. This mirrors BAML's allowed map-key set (coerce_map.rs) and the
// repo's own isValidMapKey schema gate, kept separate from
// checkSupportedType so the legal union-of-string-literals key is not
// caught by the general-union rejection. A constrained key is out of scope.
func checkSupportedMapKey(t schema.Type) error {
	if len(t.Meta.Constraints) > 0 {
		return unsupported("map key constraints")
	}
	switch t.Kind {
	case schema.TypePrimitive:
		if t.Primitive == schema.PrimitiveString {
			return nil
		}
		return unsupported(fmt.Sprintf("map key primitive %q (only string)", t.Primitive))
	case schema.TypeEnum:
		return nil
	case schema.TypeLiteral:
		if t.Literal != nil && t.Literal.Kind == schema.LiteralString {
			return nil
		}
		return unsupported("map key literal must be a string literal")
	case schema.TypeUnion:
		if isStringLiteralUnionType(t) {
			return nil
		}
		return unsupported("map key union must be a non-nullable union of string literals")
	default:
		return unsupported(fmt.Sprintf("map key kind %q", t.Kind))
	}
}

// isStringLiteralUnionType reports whether t is a non-nullable union every
// member of which is a string literal or a nested non-nullable union of
// string literals — the only union shape BAML (and M2b) accept as a map
// key. It mirrors schema.isStringLiteralUnion: the non-nullable requirement
// reproduces jsonish rejecting the null iter_include_null() appends for an
// optional union.
func isStringLiteralUnionType(t schema.Type) bool {
	if t.Union == nil || t.Union.Nullable || len(t.Union.Variants) == 0 {
		return false
	}
	for i := range t.Union.Variants {
		v := &t.Union.Variants[i]
		switch v.Kind {
		case schema.TypeLiteral:
			if v.Literal == nil || v.Literal.Kind != schema.LiteralString {
				return false
			}
		case schema.TypeUnion:
			if !isStringLiteralUnionType(*v) {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// checkSupportedUnionShape reports whether the NON-NULL variant set of a
// multi-variant union (len(Variants) >= 2) is one native reproduces exactly. It
// is the structural half of the union claim: it admits only variant sets whose
// two-phase scored selection native matches, so BAML's try_cast_union / lenient
// coerce_union + array_helper::pick_best never diverges from native's.
//
// It admits a variant set in which EVERY variant is either:
//
//   - a fully-modeled non-composite LEAF — a primitive scalar (int/float/bool/
//     string), a literal (any kind), or an enum (the M3b scalar-leaf family); or
//   - a constraint-free, resolvable class ref whose class is constraint-free and
//     has only REQUIRED FLAT-LEAF fields (primitive scalar / literal / enum — no
//     optional/union/map/list/nested-class/recursive field) with no duplicate
//     rendered field names (the M3c class family).
//
// A MIX of the two (literal/enum/class, scalar/class) is admitted too — both
// families flow through the SAME two-phase coercer (coerceUnionSafeMulti): a
// phase-1 try_cast pass (the first arm whose STRICT native-type cast matches wins
// at score 0 — coerce_union.rs try_cast_union, with class try_cast ported by
// tryCastClass) and, only when no arm try_casts, a phase-2 lenient coerce +
// array_helper::pick_best (whose list/class/scalar-vs-composite special ordering
// native reproduces in cmpCandidates). Nested scalar unions arrive here already
// flattened into this variant set by simplifyUnion.
//
// Every over-claim path declines the WHOLE union at coerce time — an arm native
// cannot prove (a non-proven ErrDeBAMLParseUnsupported), a case-fold-uncertain
// verdict, or ARRAY input to an int/float/bool/class arm (array-to-singular is
// M3d) — so over-claim is impossible; the only residual is safe under-claim.
//
// Every OTHER shape declines: a list/map variant (its SingleToArray / markdown /
// FirstMatch / array-of-union-hint scoring is M3d), a class with any non-flat-leaf
// or optional field (its default/partial/implied scoring inside a union is not yet
// proven), a constrained or recursive class, or a surviving nested union.
func checkSupportedUnionShape(b *schema.Bundle, u *schema.UnionType) error {
	vs := u.Variants
	if len(vs) < 2 {
		return unsupported("union shape: needs >= 2 non-null variants")
	}
	for i := range vs {
		if err := checkUnionVariant(b, vs[i]); err != nil {
			return err
		}
	}
	return nil
}

// checkUnionVariant reports whether one union variant is in the supported set: a
// fully-modeled scalar/literal/enum LEAF (isFlatLeafField — the identical predicate
// flat class-union fields use, because a union arm and a class field route through
// the same coercers), a modelable CLASS ref (checkUnionClassVariant), or (M3d) a
// LIST or STRING-keyed MAP arm whose scored selection native now reproduces:
//
//   - LIST arm: scored in phase 2 by coerceList (SingleToArray / partial skips /
//     child scores) and pick_best's list-vs-list / scalar-vs-composite ordering
//     (cmpCandidates). No list try_cast is modeled — a list try_cast always scores
//     0 and native's lenient early-first-score-0 rule reproduces that winner (no
//     earlier arm lenient-scores-0 on an array without also try_cast-scoring-0). The
//     element must be in scope (checkSupportedType), and a MULTI-ARM-UNION element
//     declines (the array union_variant_hint is not modeled — see the TypeList
//     branch of checkSupportedType).
//   - MAP arm: scored by tryCastMap (phase 1, ObjectToMap) and coerceMap (phase 2,
//     partial value skips). Restricted to a STRING key: enum/literal map-key
//     dynamic-keep semantics are unproven, and try_cast_map skips key validation, so
//     only a string key (every key valid) is safe inside a union.
//
// Every other kind — nested union, media, recursive alias — declines. Null is never
// a variant here (it is hoisted to UnionType.Nullable).
func checkUnionVariant(b *schema.Bundle, v schema.Type) error {
	if isFlatLeafField(v) {
		return nil
	}
	switch v.Kind {
	case schema.TypeClass:
		return checkUnionClassVariant(b, v)
	case schema.TypeList:
		// checkSupportedType handles the list arm's constraints, its element being
		// in scope, and the multi-arm-union-element decline (array hint is M3d+).
		return checkSupportedType(b, v)
	case schema.TypeMap:
		return checkUnionMapVariant(b, v)
	default:
		return unsupported(fmt.Sprintf("union variant kind %q: not a scalar/literal/enum leaf, a required-flat-leaf class, a list, or a string-keyed map", v.Kind))
	}
}

// checkUnionMapVariant validates a MAP variant of a union: constraint-free, a
// STRING primitive key, and an in-scope value type. A non-string key (enum /
// literal / string-literal union) is rejected because its dynamic-keep semantics
// are unproven and try_cast_map (phase 1) skips key validation — only a string key
// (where every key is valid) resolves identically to BAML inside a union.
func checkUnionMapVariant(b *schema.Bundle, v schema.Type) error {
	if len(v.Meta.Constraints) > 0 {
		return unsupported("union map variant has type constraints")
	}
	if v.Key == nil || v.Value == nil {
		return unsupported("union map variant without key or value")
	}
	// A CONSTRAINED key stays fallback (native does not model key constraints),
	// mirroring checkSupportedMapKey's up-front constraint reject. The dynamic
	// bridge carries no constraint channel today, so this is defensive, but it must
	// hold before the primitive-string-key check so a constrained string key never
	// slips through the union map-arm gate.
	if len(v.Key.Meta.Constraints) > 0 {
		return unsupported("union map variant key has constraints")
	}
	if v.Key.Kind != schema.TypePrimitive || v.Key.Primitive != schema.PrimitiveString {
		return unsupported("union map variant must have a string key (enum/literal map-key dynamic-keep is unproven in a union)")
	}
	return checkSupportedType(b, *v.Value)
}

// checkUnionClassVariant validates one CLASS variant of a union as a fully-modeled
// arm: constraint-free, resolvable, whose class is constraint-free and every field
// is a REQUIRED FLAT LEAF (primitive scalar / literal / enum — isFlatLeafField),
// with no duplicate rendered field names. This is the shape whose STRICT try_cast
// (tryCastClass) and LENIENT coerce (coerceClass) native reproduces byte-exact, so
// the two-phase union selection matches BAML.
//
// SINGLE-field classes ARE admitted (unlike the pre-M3c flat-disjoint family that
// rejected them): the implied-key / inferred-object paths and the pick_best
// classSingleImplied devalue are now modeled, so a single-field class arm is a
// faithful pick_best participant. NO disjoint-key requirement either — overlapping
// field-name sets are resolved by pick_best now, not declined.
//
// A class with ANY optional / list / map / union / nested-class field declines: its
// try_cast can score non-zero (a missing optional → OptionalDefaultFromNoValue) and
// its lenient default/partial/implied scoring inside a union is not yet proven (that
// broadening, plus the try_cast_union non-zero-collection sub-path, is M3d+). A
// zero-field class declines too (its NoFields / empty-object try_cast is unmodeled).
func checkUnionClassVariant(b *schema.Bundle, v schema.Type) error {
	if len(v.Meta.Constraints) > 0 {
		return unsupported("class-union variant has type constraints")
	}
	cls, ok := b.FindClass(v.Name, v.Mode)
	if !ok {
		return fmt.Errorf("debaml: unknown class %q", v.Name)
	}
	if len(cls.Constraints) > 0 {
		return unsupported("class-union variant class has constraints")
	}
	if len(cls.Fields) == 0 {
		return unsupported("class-union variant class has no fields (NoFields/empty-object try_cast unmodeled)")
	}
	seen := make(map[string]struct{}, len(cls.Fields))
	for j := range cls.Fields {
		f := &cls.Fields[j]
		if !isFlatLeafField(f.Type) {
			// Any optional/union/map/list/nested-class/recursive field opens BAML
			// leniency (defaults, implied keys, partial maps, single-to-array) whose
			// union scoring native does not yet prove — decline (M3d).
			return unsupported("class-union variant class has a non-flat-leaf or optional field (M3c models required flat-leaf class fields only)")
		}
		rn := f.Name.RenderedName()
		if _, dup := seen[rn]; dup {
			return unsupported("class-union variant class has duplicate rendered field names")
		}
		seen[rn] = struct{}{}
	}
	return nil
}

// isMultiArmUnion reports whether t is a union with two or more NON-NULL
// variants — the hint-sensitive shape a list element must not be (see the
// TypeList branch of checkSupportedType). A nullable multi-union keeps its two+
// non-null variants in Variants (null is hoisted to Nullable), so it is
// multi-arm; a single-non-null optional (T?) has one Variant and is NOT (its
// only union_variant_hint is that lone arm, so per-element coercion cannot
// diverge). A non-nullable single-variant union is collapsed by simplifyUnion
// and never appears here.
func isMultiArmUnion(t schema.Type) bool {
	return t.Kind == schema.TypeUnion && t.Union != nil && len(t.Union.Variants) >= 2
}

// isFlatLeafField reports whether t is a flat exact leaf field type usable in
// an M2c class-union variant: a constraint-free primitive scalar (string /
// int / float / bool), literal, or enum. Everything else — null/media
// primitives, unions (incl. optionals), maps, lists, nested classes,
// recursive aliases — is rejected, because each one introduces BAML leniency
// native cannot prove away inside a union.
func isFlatLeafField(t schema.Type) bool {
	if len(t.Meta.Constraints) > 0 {
		return false
	}
	switch t.Kind {
	case schema.TypePrimitive:
		switch t.Primitive {
		case schema.PrimitiveString, schema.PrimitiveInt, schema.PrimitiveFloat, schema.PrimitiveBool:
			return true
		default:
			return false
		}
	case schema.TypeLiteral, schema.TypeEnum:
		return true
	default:
		return false
	}
}

// The production matcher below ports BAML's deserializer/coercer/match_string.rs
// — the fuzzy matcher enum, string-literal, class-field-key, and map-key
// coercion route through (Mcoerce-a). It is the PRODUCTION matcher whose
// accept/reject/ambiguous verdict and matched candidate native must reproduce
// BAML byte-exact (a fuzzy match changes the emitted value). It lives here,
// beside the gate, rather than in a separate file so the de-BAML embed source
// list stays unchanged. (M3c removed the older conservative match_string
// SUPERSET that gated class-union field-name disjointness: pick_best now
// resolves overlapping-key class arms, so the disjointness gate is gone.)
//
// Null handling and non-string stringification (ObjectToString) are the
// CALLER's job: matchString operates on an already-string input. Mcoerce-b adds
// lenient numeric/bool/null PRIMITIVE + int/bool LITERAL coercion (see
// coerce.go), but enum/string-literal matching still coerces string inputs
// only; non-string enum/literal inputs decline upstream (their jsonish::Value
// Display reproduction — ObjectToString / JsonToString — is Mcoerce-d).

// matchOutcome is the verdict of a matchString evaluation.
type matchOutcome int

const (
	// matchNone: no candidate matched (BAML error_unexpected_type).
	matchNone matchOutcome = iota
	// matchOne: exactly one best candidate — a clean match.
	matchOne
	// matchAmbiguous: a substring tie across variants — BAML errors via
	// StrMatchOneFromMany (try_match_only_once) BEFORE emitting, so native
	// must never pick one of the tied variants.
	matchAmbiguous
)

// matchCandidate is one (name, valid_values) tuple. name is the value
// emitted on a match (enum real name, literal string, field rendered name);
// validValues are the strings the input is matched against (the rendered
// name, plus enum description forms).
type matchCandidate struct {
	name        string
	validValues []string
}

// undLowerCaser builds a fresh full-Unicode lowercaser. BAML's match_string
// case-fold uses Rust str::to_lowercase (full SpecialCasing — e.g. İ ->
// i+U+0307), which Go's strings.ToLower (simple per-rune mapping) does NOT
// reproduce; golang.org/x/text/cases.Lower(language.Und) does. A cases.Caser is
// NOT safe for concurrent use, so callers build a local one per matchString
// call (only when the case-fold attempt is actually reached).
func undLowerCaser() cases.Caser { return cases.Lower(language.Und) }

// caseFoldUncertain reports whether lowercasing s could DIVERGE from Rust's
// str::to_lowercase. cases.Lower is not byte-identical to Rust for every rune
// (e.g. x/text v0.38 leaves U+A7DC 'Ƛ' unchanged while Rust lowercases it to
// 'ƛ'; Go's own case tables don't even classify U+A7DC as uppercase). The
// robust, conservative test that native CAN prove: a rune is lowercase-stable
// iff it is ASCII or Go reports it IsLower (a genuinely lowercase letter, whose
// lowercasing is the identity on both Go and Rust — this keeps 'é'/'ß'/'ü'
// certain so accented inputs like "Résumé" still match). Any OTHER non-ASCII
// rune (uppercase, titlecase, or a cased letter Go's tables don't recognize)
// is treated as uncertain. The corpus is ASCII, so this never fires there.
func caseFoldUncertain(s string) bool {
	for _, r := range s {
		if r > unicode.MaxASCII && !unicode.IsLower(r) {
			return true
		}
	}
	return false
}

// matchString ports match_string.rs::match_string. It trims the input, then
// runs the case-sensitive / accent-folded / punctuation-stripped /
// case-insensitive / substring strategies in BAML's exact order, returning the
// matched candidate name, an outcome, whether the match came from the SUBSTRING
// strategy (BAML's SubstringMatch flag, cost 2 — the exact/fold strategies are
// score 0), and whether the case-fold attempt involved a non-ASCII rune whose
// lowercasing native cannot prove equals Rust's (uncertain — see
// caseFoldUncertain). uncertain is false whenever a match is found before the
// case-fold attempt. allowSubstring mirrors match_string's allow_substring_match:
// class field keys pass false (via matchesStringToString); enum / string-literal
// / map-key coercion pass true.
func matchString(input string, candidates []matchCandidate, allowSubstring bool) (string, matchOutcome, bool, bool) {
	// Trim whitespace (no flag, score 0).
	matchContext := strings.TrimSpace(input)

	// Attempt 1: original (trimmed) candidates.
	if name, outcome, sub, found := stringMatchStrategy(matchContext, candidates, allowSubstring); found {
		return name, outcome, sub, false
	}

	// Strip punctuation from input and from every candidate value, then retry
	// (no flag — BAML never adds StrippedNonAlphaNumeric despite the unused flag).
	matchContext = stripPunctuation(matchContext)
	stripped := make([]matchCandidate, len(candidates))
	for i := range candidates {
		vals := make([]string, len(candidates[i].validValues))
		for j, v := range candidates[i].validValues {
			vals[j] = stripPunctuation(v)
		}
		stripped[i] = matchCandidate{name: candidates[i].name, validValues: vals}
	}

	// Attempt 2: punctuation-stripped. (BAML's third attempt is a verbatim
	// repeat of this one over the SAME match_context/candidates — it can only
	// return the same result — so it is intentionally omitted here.)
	if name, outcome, sub, found := stringMatchStrategy(matchContext, stripped, allowSubstring); found {
		return name, outcome, sub, false
	}

	// The case-fold attempt is now reached. Determine whether lowercasing any of
	// the forms about to be lowered could diverge from Rust (uncertain).
	uncertain := caseFoldUncertain(matchContext)
	if !uncertain {
		for i := range stripped {
			for _, v := range stripped[i].validValues {
				if caseFoldUncertain(v) {
					uncertain = true
				}
			}
		}
	}

	// Attempt 4: case-insensitive over the stripped forms (no flag, score 0),
	// using full-Unicode lowercasing to match Rust's str::to_lowercase.
	caser := undLowerCaser()
	matchContext = caser.String(matchContext)
	lowered := make([]matchCandidate, len(stripped))
	for i := range stripped {
		vals := make([]string, len(stripped[i].validValues))
		for j, v := range stripped[i].validValues {
			vals[j] = caser.String(v)
		}
		lowered[i] = matchCandidate{name: stripped[i].name, validValues: vals}
	}
	if name, outcome, sub, found := stringMatchStrategy(matchContext, lowered, allowSubstring); found {
		return name, outcome, sub, uncertain
	}

	return "", matchNone, false, uncertain
}

// matchesStringToString ports match_string.rs::matches_string_to_string: a
// single-candidate, NO-substring match used for class object field keys
// (coerce_class.rs:209). Returns whether input matches target, plus whether the
// verdict depended on a non-ASCII case fold native cannot prove equals BAML
// (uncertain — caller declines on it). (The key match adds no class flag in
// BAML, so the substring bit is irrelevant — substring is disabled here anyway.)
func matchesStringToString(input, target string) (matched, uncertain bool) {
	_, outcome, _, unc := matchString(input, []matchCandidate{{name: target, validValues: []string{target}}}, false)
	return outcome == matchOne, unc
}

// stringMatchStrategy ports match_string.rs::string_match_strategy for one
// already-transformed pass: exact case-sensitive, then accent-folded
// case-sensitive, then (if allowSubstring) non-overlapping substring counting.
// found is true when this pass produced a verdict (matchOne or matchAmbiguous);
// viaSubstring is true only when the verdict came from the substring section.
func stringMatchStrategy(valueStr string, candidates []matchCandidate, allowSubstring bool) (string, matchOutcome, bool, bool) {
	// Strategy 1: exact case-sensitive match. First candidate (in order) with
	// any exactly-equal valid value wins.
	for i := range candidates {
		for _, v := range candidates[i].validValues {
			if v == valueStr {
				return candidates[i].name, matchOne, false, true
			}
		}
	}

	// Strategy 2: accent/ligature-folded case-sensitive match.
	unaccentedValue := removeAccents(valueStr)
	for i := range candidates {
		for _, v := range candidates[i].validValues {
			if removeAccents(v) == unaccentedValue {
				return candidates[i].name, matchOne, false, true
			}
		}
	}

	if !allowSubstring {
		return "", matchNone, false, false
	}

	// Substring matching: gather every occurrence of each candidate value
	// within valueStr (variant = candidate name).
	type span struct {
		start, end int
		variant    string
	}
	var all []span
	for i := range candidates {
		for _, valid := range candidates[i].validValues {
			for _, start := range matchIndices(valueStr, valid) {
				all = append(all, span{start: start, end: start + len(valid), variant: candidates[i].name})
			}
		}
	}

	// If nothing matched directly, retry against the accent-folded forms.
	// BAML deliberately keeps end_idx = start + len(ORIGINAL valid_name).
	if len(all) == 0 {
		for i := range candidates {
			for _, valid := range candidates[i].validValues {
				unaccentedValid := removeAccents(valid)
				for _, start := range matchIndices(unaccentedValue, unaccentedValid) {
					all = append(all, span{start: start, end: start + len(valid), variant: candidates[i].name})
				}
			}
		}
	}

	if len(all) == 0 {
		return "", matchNone, false, false
	}

	// Sort by start ascending, then by end descending (longer first).
	sort.SliceStable(all, func(a, b int) bool {
		if all[a].start != all[b].start {
			return all[a].start < all[b].start
		}
		return all[a].end > all[b].end
	})

	// Drop overlapping matches, keeping the earliest/longest at each position.
	var filtered []span
	lastEnd := 0
	for _, s := range all {
		if s.start >= lastEnd {
			lastEnd = s.end
			filtered = append(filtered, s)
		}
	}

	// Count non-overlapping occurrences per variant, preserving first-seen
	// order so the winner is deterministic on a unique max.
	counts := make(map[string]int, len(filtered))
	var order []string
	for _, s := range filtered {
		if _, seen := counts[s.variant]; !seen {
			order = append(order, s.variant)
		}
		counts[s.variant]++
	}

	best := ""
	max := 0
	atMax := 0
	for _, v := range order {
		c := counts[v]
		switch {
		case c > max:
			max = c
			best = v
			atMax = 1
		case c == max:
			atMax++
		}
	}
	if atMax > 1 {
		// Tie across variants -> StrMatchOneFromMany -> BAML errors.
		return "", matchAmbiguous, true, true
	}
	return best, matchOne, true, true
}

// matchIndices returns the byte offsets of every non-overlapping occurrence
// of needle in haystack, matching Rust's str::match_indices (left-to-right,
// the next search resumes after a match). An empty needle matches at every
// char boundary plus the end, reproducing Rust's empty-pattern behavior.
func matchIndices(haystack, needle string) []int {
	if needle == "" {
		idx := make([]int, 0, len(haystack)+1)
		for i := range haystack {
			idx = append(idx, i)
		}
		return append(idx, len(haystack))
	}
	var idx []int
	for start := 0; start <= len(haystack); {
		rel := strings.Index(haystack[start:], needle)
		if rel < 0 {
			break
		}
		pos := start + rel
		idx = append(idx, pos)
		start = pos + len(needle)
	}
	return idx
}

// stripPunctuation ports match_string.rs::strip_punctuation: keep
// alphanumeric runes plus '-' and '_', drop everything else.
func stripPunctuation(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r == '-' || r == '_' || unicode.IsLetter(r) || unicode.IsNumber(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// ligatureFolder reproduces match_string.rs::remove_accents's pre-NFKD
// ligature substitutions (ß/æ/Æ/ø/Ø/œ/Œ); the targets share no source rune,
// so a single non-overlapping pass equals BAML's sequential .replace() calls.
var ligatureFolder = strings.NewReplacer(
	"ß", "ss",
	"æ", "ae", "Æ", "AE",
	"ø", "o", "Ø", "O",
	"œ", "oe", "Œ", "OE",
)

// removeAccents ports match_string.rs::remove_accents: fold the ligatures
// above, NFKD-decompose, then drop combining marks (General_Category=Mark).
func removeAccents(s string) string {
	s = ligatureFolder.Replace(s)
	decomposed := norm.NFKD.String(s)
	var b strings.Builder
	b.Grow(len(decomposed))
	for _, r := range decomposed {
		if unicode.In(r, unicode.Mn, unicode.Mc, unicode.Me) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// unsupported wraps bamlutils.ErrDeBAMLParseUnsupported with a reason so
// the caller falls back to BAML while logs/metrics still record why.
func unsupported(reason string) error {
	return fmt.Errorf("%w: %s", bamlutils.ErrDeBAMLParseUnsupported, reason)
}

// unsupportedErr is unsupported with an underlying cause attached.
func unsupportedErr(stage string, cause error) error {
	return fmt.Errorf("%w: %s: %v", bamlutils.ErrDeBAMLParseUnsupported, stage, cause)
}
