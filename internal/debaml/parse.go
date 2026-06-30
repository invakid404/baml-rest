package debaml

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"unicode"

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
// multi-variant union (len(Variants) >= 2) is one of the two M2c safe
// families. It is the structural half of the M2c claim: it proves BAML
// cannot leniently succeed on a SECOND arm (which would force BAML's scored
// pick_best, where native's single pick may diverge). The value-level guard
// in coerceUnionSafe then proves BAML sees exactly one clean winner for the
// concrete input.
//
// The two safe families are:
//
//   - HOMOGENEOUS LITERAL-ONLY union: every variant is a literal of the SAME
//     kind. For string literals the value set must be pairwise disjoint under
//     a conservative match_string SUPERSET (matchStringMightMatch) — exact
//     disjointness is not enough because BAML fuzzy-matches string literals.
//     int/bool literals need no extra check: BAML matches them by value
//     equality, which (after dedup) no two distinct literals can both satisfy.
//   - HOMOGENEOUS FLAT CLASS union: every variant is a constraint-free class
//     ref whose class has >= 2 fields, every field REQUIRED and a FLAT LEAF
//     (primitive scalar / literal / enum — no union/map/list/optional/
//     recursive-alias/nested-class), and whose rendered field-name sets are
//     pairwise disjoint under the same conservative key-match superset.
//
// Every other shape — a bare primitive variant, a list/map variant, a nested
// union, mixed literal kinds, literal-vs-class, enum-vs-anything, overlapping
// or single-field classes — declines, because BAML could leniently succeed on
// a second arm there.
func checkSupportedUnionShape(b *schema.Bundle, u *schema.UnionType) error {
	vs := u.Variants
	if len(vs) < 2 {
		return unsupported("union shape: needs >= 2 non-null variants")
	}
	switch {
	case allLiteralVariants(vs):
		return checkHomogeneousLiteralUnion(vs)
	case allClassVariants(vs):
		return checkFlatClassUnion(b, vs)
	default:
		// Bare primitive, list, map, nested union, or a mixed variant family:
		// BAML's lenient coercers (stringify, string<->number, array-to-
		// singular, fuzzy enum/literal, implied-key class, defaults) can make
		// a second arm succeed, so native cannot prove a single winner.
		return unsupported("union shape: not a homogeneous literal or flat class union")
	}
}

// allLiteralVariants reports whether every variant is a constraint-free
// literal — the precondition for the homogeneous-literal family.
func allLiteralVariants(vs []schema.Type) bool {
	for i := range vs {
		v := &vs[i]
		if v.Kind != schema.TypeLiteral || v.Literal == nil || len(v.Meta.Constraints) > 0 {
			return false
		}
	}
	return true
}

// allClassVariants reports whether every variant is a constraint-free class
// ref — the precondition for the flat-class family.
func allClassVariants(vs []schema.Type) bool {
	for i := range vs {
		v := &vs[i]
		if v.Kind != schema.TypeClass || len(v.Meta.Constraints) > 0 {
			return false
		}
	}
	return true
}

// checkHomogeneousLiteralUnion validates a literal-only variant set: all the
// same literal kind, and — for string literals — pairwise match-disjoint.
func checkHomogeneousLiteralUnion(vs []schema.Type) error {
	kind := vs[0].Literal.Kind
	for i := range vs {
		if vs[i].Literal.Kind != kind {
			return unsupported("literal union: mixed literal kinds (BAML may coerce a 2nd arm)")
		}
	}
	if kind == schema.LiteralString {
		lits := make([]string, len(vs))
		for i := range vs {
			lits[i] = vs[i].Literal.String
		}
		if !stringsPairwiseDisjoint(lits) {
			return unsupported("string-literal union: values not pairwise match-disjoint (BAML may fuzzy-match a 2nd arm)")
		}
	}
	return nil
}

// checkFlatClassUnion validates a class-only variant set as a flat,
// disjoint-key discriminated union: every variant class is constraint-free,
// has >= 2 required FLAT-LEAF fields, has no duplicate rendered field names,
// and the variants' rendered field-name sets are pairwise disjoint under the
// conservative key-match superset.
func checkFlatClassUnion(b *schema.Bundle, vs []schema.Type) error {
	sets := make([][]string, len(vs))
	for i := range vs {
		v := &vs[i]
		cls, ok := b.FindClass(v.Name, v.Mode)
		if !ok {
			return fmt.Errorf("debaml: unknown class %q", v.Name)
		}
		if len(cls.Constraints) > 0 {
			return unsupported("class-union variant class has constraints")
		}
		if len(cls.Fields) < 2 {
			// A single-field class is implied-key/inferred-object risk: BAML
			// can absorb a scalar or a differently-keyed object into the lone
			// field, succeeding where native's exact match fails.
			return unsupported("class-union variant class has < 2 fields (BAML implied-key risk)")
		}
		names := make([]string, 0, len(cls.Fields))
		seen := make(map[string]struct{}, len(cls.Fields))
		for j := range cls.Fields {
			f := &cls.Fields[j]
			if !isFlatLeafField(f.Type) {
				// Any optional/union/map/list/nested-class/recursive field opens
				// BAML leniency (defaults, implied keys, partial maps, single-to-
				// array), so the class is not a clean discriminated arm.
				return unsupported("class-union variant has a non-flat-leaf or optional field")
			}
			rn := f.Name.RenderedName()
			if _, dup := seen[rn]; dup {
				return unsupported("class-union variant has duplicate rendered field names")
			}
			seen[rn] = struct{}{}
			names = append(names, rn)
		}
		sets[i] = names
	}
	if !fieldNameSetsPairwiseDisjoint(sets) {
		return unsupported("class-union field-name sets not pairwise match-disjoint (BAML may fuzzy-match keys to a 2nd arm)")
	}
	return nil
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

// stringsPairwiseDisjoint reports whether no two distinct strings in vals
// could match under the conservative match_string superset.
func stringsPairwiseDisjoint(vals []string) bool {
	for i := 0; i < len(vals); i++ {
		for j := i + 1; j < len(vals); j++ {
			if matchStringMightMatch(vals[i], vals[j]) {
				return false
			}
		}
	}
	return true
}

// fieldNameSetsPairwiseDisjoint reports whether no field name in any set
// could match a field name in any OTHER set under the conservative
// match_string superset — the property that lets native prove BAML cannot
// fuzzy-match the input keys of one class-union arm onto another arm.
func fieldNameSetsPairwiseDisjoint(sets [][]string) bool {
	for i := 0; i < len(sets); i++ {
		for j := i + 1; j < len(sets); j++ {
			for _, a := range sets[i] {
				for _, b := range sets[j] {
					if matchStringMightMatch(a, b) {
						return false
					}
				}
			}
		}
	}
	return true
}

// matchStringMightMatch is a CONSERVATIVE SUPERSET of BAML's match_string: it
// returns true whenever BAML could possibly match one of a/b against the
// other, and only "wrongly" returns true in extra cases (which merely cause
// native to DECLINE — always parity-safe). BAML's match_string normalizes
// (trim, strip punctuation, fold case, fold accents) and then substring-
// matches in both directions; this mirrors that and additionally treats any
// non-ASCII residue conservatively (BAML may transliterate it in ways this
// folder does not reproduce, e.g. ø->o, ß->ss), so a pair that still carries
// non-ASCII letters after folding is treated as a possible match.
func matchStringMightMatch(a, b string) bool {
	na, asciiA := normalizeForMatchString(a)
	nb, asciiB := normalizeForMatchString(b)
	if !asciiA || !asciiB {
		// Non-ASCII residue after accent folding could still transliterate-
		// match in BAML; be conservative and treat as a possible match.
		return true
	}
	if na == "" || nb == "" {
		// An all-punctuation token normalizes empty and is a substring of
		// everything under BAML's substring matching; treat as a match.
		return true
	}
	if na == nb {
		return true
	}
	return strings.Contains(na, nb) || strings.Contains(nb, na)
}

// normalizeForMatchString folds a string the way the conservative
// match_string superset compares: NFD-decompose, drop combining marks (accent
// fold), lowercase, and keep only letters and digits (trim + punctuation
// strip). It also reports whether every kept rune is ASCII, so the caller can
// treat unfoldable non-ASCII residue conservatively.
func normalizeForMatchString(s string) (string, bool) {
	d := norm.NFD.String(s)
	var sb strings.Builder
	allASCII := true
	for _, r := range d {
		if unicode.Is(unicode.Mn, r) {
			// Combining mark left by NFD decomposition (the accent itself).
			continue
		}
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			continue
		}
		lr := unicode.ToLower(r)
		if lr > unicode.MaxASCII {
			allASCII = false
		}
		sb.WriteRune(lr)
	}
	return sb.String(), allASCII
}

// The production matcher below ports BAML's deserializer/coercer/match_string.rs
// — the fuzzy matcher enum, string-literal, class-field-key, and map-key
// coercion route through (Mcoerce-a). It is deliberately distinct from the
// conservative matchStringMightMatch SUPERSET above: that one only gates
// union-shape disjointness (over-declining there is parity-safe), while this
// one is the PRODUCTION matcher whose accept/reject/ambiguous verdict and
// matched candidate native must reproduce BAML byte-exact (a fuzzy match
// changes the emitted value). It lives here, beside the gate, rather than in a
// separate file so the de-BAML embed source list stays unchanged.
//
// Null handling and non-string stringification (ObjectToString) are the
// CALLER's job: matchString operates on an already-string input. Mcoerce-a only
// coerces string inputs; non-string enum/literal inputs decline upstream (their
// jsonish::Value Display reproduction is Mcoerce-b/d).

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

// matchString ports match_string.rs::match_string. It trims the input, then
// runs the case-sensitive / accent-folded / punctuation-stripped /
// case-insensitive / substring strategies in BAML's exact order, returning the
// matched candidate name, an outcome, and whether the match came from the
// SUBSTRING strategy (BAML's SubstringMatch flag, cost 2 — the exact/fold
// strategies are score 0). allowSubstring mirrors match_string's
// allow_substring_match: class field keys pass false (via
// matchesStringToString); enum / string-literal / map-key coercion pass true.
func matchString(input string, candidates []matchCandidate, allowSubstring bool) (string, matchOutcome, bool) {
	// Trim whitespace (no flag, score 0).
	matchContext := strings.TrimSpace(input)

	// Attempt 1: original (trimmed) candidates.
	if name, outcome, sub, found := stringMatchStrategy(matchContext, candidates, allowSubstring); found {
		return name, outcome, sub
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
		return name, outcome, sub
	}

	// Attempt 4: case-insensitive over the stripped forms (no flag, score 0).
	matchContext = strings.ToLower(matchContext)
	lowered := make([]matchCandidate, len(stripped))
	for i := range stripped {
		vals := make([]string, len(stripped[i].validValues))
		for j, v := range stripped[i].validValues {
			vals[j] = strings.ToLower(v)
		}
		lowered[i] = matchCandidate{name: stripped[i].name, validValues: vals}
	}
	if name, outcome, sub, found := stringMatchStrategy(matchContext, lowered, allowSubstring); found {
		return name, outcome, sub
	}

	return "", matchNone, false
}

// matchesStringToString ports match_string.rs::matches_string_to_string: a
// single-candidate, NO-substring match used for class object field keys
// (coerce_class.rs:209). Returns whether input matches target. (The key match
// adds no class flag in BAML, so its substring bit is irrelevant — substring
// is disabled here anyway.)
func matchesStringToString(input, target string) bool {
	_, outcome, _ := matchString(input, []matchCandidate{{name: target, validValues: []string{target}}}, false)
	return outcome == matchOne
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
