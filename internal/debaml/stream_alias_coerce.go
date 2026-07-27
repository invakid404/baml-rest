package debaml

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"

	"github.com/invakid404/baml-rest/internal/schema"
)

// De-BAML Phase 3b/3c (recursive-alias STREAMING) — the completion-aware alias stream
// coercer + BAML semantic-streaming, the streaming twin of the Phase-3a/3c final
// coercer (alias_coerce.go). It handles BOTH alias families, parameterized by
// [recAliasProfile].
//
// ADMISSION vs CAPABILITY. Only `JSON` is admitted to the production streaming lane. The
// `JsonValue` code below is COMPLETE and proven at the parser level (the strict gate-free
// per-prefix differential agrees with stock v0.223 on 100% of the surface it owns), but the
// family's stream gate is CLOSED: a claimed stream has no route back to BAML, and this
// family's parse can still decline on a VALUE (the negative zero in alias_coerce.go, plus
// the shared #583 jsonish-recovery residual). [staticStreamAliasProfile] carries the full
// reasoning, and TestJsonValueStreamResidualLedger enumerates the exact blocker set that
// must be empty before the gate is opened. Treat the JsonValue paths here as built-dark, in
// the same sense Phase 3b's Phase A was.
//
// BAML's streaming pipeline for a partial is: jsonish parser (→ Value) → field_type.coerce
// (arm selection, UNCHANGED from final) → validate_streaming_state (process_node /
// required_done: drop required-done leaves that are not yet final, filter list/map children,
// root optional/null) → public projection. This file reproduces steps 2-4 for the direct
// five-arm JSON alias: it REUSES the Phase-3a arm selection (aliasCoerceValue — same
// try-cast/score/pick_best/SingleToArray) so a complete prefix coerces identically to the
// final, then applies BAML's semantic-streaming keep/drop rule per SELECTED ARM.
//
// required_done for the JSON union is determined by the SELECTED ARM (semantic_streaming.rs
// required_done for a union uses the matched variant). The drop is reproduced STRUCTURALLY —
// from the coerced arm + the PARSED VALUE'S SHAPE — with NO completion flag stored on or read
// from the carrier:
//   - int arm: a number whose text is FLOAT-shaped (contains '.', 'e', or 'E') is DROPPED (a
//     streamed float's precision is not yet final — BAML's required-done int drop); a clean
//     integer token is KEPT even mid-stream (BAML treats it as a complete i64 the instant it
//     has digits: `[1` → [1], `[1,2` → [1,2]). See [streamScalarArm].
//   - bool arm: KEPT (intrinsically complete once recognized).
//   - string / list / map arms: NOT required-done → KEPT as partials.
// A dropped leaf inside a list drops the element (list filter); inside a map drops the whole
// key/value entry (map filter); at the root a dropped/absent JSON arm is the root-optional
// no-emit or the SingleToArray→[] fallback (see [coerceStreamAliasRoot] / [aliasRootScalar]).
//
// PHASE 3c — the `JsonValue` family NEVER DROPS. This is the single biggest streaming
// finding, settled against the LIVE stock-v0.223 per-prefix oracle rather than inferred
// from the primitive required-done rule, and it is proven row-by-row by the strict
// per-prefix differential:
//
//	stock v0.223 ParseStream.StaticRecursiveAliasJsonValue(prefix)
//	  ==  stock v0.223 Parse.StaticRecursiveAliasJsonValue(prefix)   for EVERY prefix.
//
// The mechanism is the float arm. For `JSON` an incomplete float-shaped number reaches
// the required-done INT arm and is dropped (`1.` → [], `[1.` → [[]]). For `JsonValue`
// the `float` arm (arm 1) absorbs every number whose as_i64 is None, so the int arm only
// ever wins on a CLEAN, COMPLETE i64 token — which BAML keeps mid-stream — and the float
// arm is not required-done in v0.223 (LIVE: `1.`→1, `1.2`→1.2, `[1.`→[1], `{"a":1.`
// →{"a":1}). bool/null are intrinsically complete once recognized, and string/list/map
// are not required-done. So no `JsonValue` node is ever deleted by semantic streaming,
// and its partial cadence is exactly its final coercion of the same prefix.
//
// The other Phase-3c streaming facts, all LIVE-CAPTURED:
//
//   - `null` is a PRESENT typed-null partial (`null`), never `[]` and never a no-emit.
//     A present null inside a list/map is KEPT, not filtered (`[null`→[null],
//     `{"a":null`→{"a":null}).
//   - the null-keyword PREFIXES `n`/`nu`/`nul` are incomplete unquoted STRINGS
//     (`"n"`/`"nu"`/`"nul"`), reselecting to the null arm only at the complete `null`.
//   - an incomplete float token `1.` is the FLOAT arm (f64 1.0 → public `1`), while a
//     non-f64-parseable numeric prefix (`1e`, `1.2e`) is the STRING arm, reselecting to
//     float once a valid suffix arrives (`1.2e`→"1.2e", `1.2e5`→120000).

// aliasStreamValue is the private carrier for a partially-coerced alias value. Unlike the
// Phase-3a [aliasValue] it retains the selected arm (kind) + ordered children so the
// semantic-streaming filter can drop elements/entries before the public projection. The map
// arm keeps ordered entries (BAML IndexMap order) so the public sorted marshal matches
// Phase-3a.
//
// The carrier is STATELESS with respect to completion: it stores NO completion flag, and the
// coercer reads none. The required-done keep/drop decision is made during coercion by the
// arm-specific rules in [streamScalarArm] / [streamListArm] / [streamMapArm] — driven by the
// SELECTED arm and the PARSED VALUE'S STRUCTURE (notably the int arm drops a FLOAT-shaped
// number, see the file header), not by any completion flag. (An earlier `complete` field was
// written on every node but never read; it was removed as dead state.)
type aliasStreamValue struct {
	kind aliasKind
	i    int64
	f    float64
	s    string
	b    bool
	arr  []aliasStreamValue
	obj  []aliasStreamEntry
}

type aliasStreamEntry struct {
	key string
	val aliasStreamValue
}

// toAny materialises the ordered stream tree into a Go `any` (map arm → map[string]any,
// whose keys json.Marshal sorts lexically — the same sorted-public representation Phase-3a
// proves). Empty array/map stay non-nil so they marshal as []/{}.
func (av aliasStreamValue) toAny() any {
	switch av.kind {
	case akInt:
		return av.i
	case akFloat:
		// Same public projection as the final carrier: Go json.Marshal of the coerced
		// float64, byte-identical to the generated Union6.MarshalJSON of BAML's f64.
		return av.f
	case akNull:
		// A PRESENT typed null — the stream twin of the final [akNull]. It marshals to
		// `null` and is a genuine EMIT, never collapsed into "no event".
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
		m := make(map[string]any, len(av.obj))
		for i := range av.obj {
			m[av.obj[i].key] = av.obj[i].val.toAny()
		}
		return m
	default:
		return nil
	}
}

func (av aliasStreamValue) marshalPublic() (json.RawMessage, error) {
	return json.Marshal(av.toAny())
}

// aliasStreamHasNegativeZero is the stream twin of [aliasHasNegativeZero]: the streaming
// carrier is the SAME generated *Union6, so a coerced negative zero is unprovable in this
// lane too. See [errNegativeZeroFloat] for why.
func aliasStreamHasNegativeZero(av aliasStreamValue) bool {
	switch av.kind {
	case akFloat:
		return av.f == 0 && math.Signbit(av.f)
	case akArray:
		for i := range av.arr {
			if aliasStreamHasNegativeZero(av.arr[i]) {
				return true
			}
		}
	case akMap:
		for i := range av.obj {
			if aliasStreamHasNegativeZero(av.obj[i].val) {
				return true
			}
		}
	}
	return false
}

// ParseAliasStreamPartial is the gate-free native partial entry the Phase-3b/3c
// per-prefix DIFFERENTIAL drives (the streaming twin of ParseStaticBundle): it strips
// comments, extracts a completion-bearing jsonish value from the accumulated prefix,
// coerces it against the admitted alias family with semantic streaming, and returns
// (sorted-public bytes, emit).
// It runs the parser DIRECTLY (bypassing SupportsNativeStaticStreamBundle) so the oracle /
// per-prefix differential can prove byte/event-exactness against stock BAML without the
// admission gate in the way. This entry is GATE-FREE by design; the PRODUCTION path is
// [ParseStaticStreamPartial], which applies the support gate first and then routes through
// this.
//
//   - (bytes, true, nil): native emits this partial.
//   - (nil, false, nil):  native has no partial for this prefix (a benign no-emit).
//   - (nil, false, err):  a claimed coercion failure.
func ParseAliasStreamPartial(b *schema.Bundle, raw string) (json.RawMessage, bool, error) {
	if b == nil {
		return nil, false, unsupported("nil static stream bundle")
	}
	// Classify the family ONCE and thread the profile through the whole partial: the
	// root scalar disposition, the number-token mode, and the per-arm streaming rules
	// are all profile-driven, so the two families can never be mixed mid-prefix.
	prof, ok := admittedServedRecursiveAliasProfile(b)
	if !ok {
		return nil, false, unsupported("static stream: not a served recursive-alias family")
	}
	stripped := stripJSONComments(raw)
	// Trim LEADING whitespace only — the value starts after it; trailing whitespace may be
	// INSIDE an open quoted string (BAML keeps it), so it must not be trimmed here.
	lead := strings.TrimLeft(stripped, " \t\r\n")
	if lead == "" {
		return nil, false, nil
	}
	// A container anywhere ({/[) — including one embedded in prose or a markdown fence —
	// routes to the shared streaming extractor, which reproduces BAML's object/array/
	// greedy-comma cadence AND its prose/fence recovery (path-3 span / fenced block).
	if strings.ContainsAny(stripped, "{[") {
		if v, ok := streamExtractCandidateMode(stripped, profileNumMode(prof)); ok {
			return coerceStreamAliasRoot(b, prof, v)
		}
		// else fall through to the scalar/string recovery below.
	}
	// MARKDOWN fence OPENER before any content ({/[): BAML strips the leading backticks and
	// treats the partial language-tag line (up to the first newline) as an unquoted STRING
	// (` → "", ```j → "j", ```json → "json"). Once fenced CONTENT ({/[) arrives it is caught
	// by the container branch above (extractFenceContentStream).
	if lead[0] == '`' {
		rest := strings.TrimLeft(lead, "`")
		if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
			// Header line complete (newline consumed): the fenced CONTENT follows. A
			// container in it was already handled above; empty/whitespace content is an
			// empty string; any other bare content falls through to the string/scalar logic.
			content := strings.TrimLeft(rest[nl+1:], " \t\r\n")
			if content == "" {
				out, err := (aliasStreamValue{kind: akString, s: ""}).marshalPublic()
				if err != nil {
					return nil, false, err
				}
				return out, true, nil
			}
			lead = content
		} else {
			// Partial header line (no newline yet) → the language-tag partial is a string.
			out, err := (aliasStreamValue{kind: akString, s: rest}).marshalPublic()
			if err != nil {
				return nil, false, err
			}
			return out, true, nil
		}
	}
	// ROOT double-quoted string (possibly unclosed): BAML keeps the evolving partial as a
	// STRING (not required-done). Decode BAML's escape set (incl. \" → ") and keep the
	// partial — including any trailing whitespace INSIDE the open string — for an
	// unterminated string.
	if lead[0] == '"' {
		s, ok := parseAliasRootQuotedString(lead)
		if !ok {
			return nil, false, nil
		}
		out, err := (aliasStreamValue{kind: akString, s: s}).marshalPublic()
		if err != nil {
			return nil, false, err
		}
		return out, true, nil
	}
	// ROOT unquoted-scalar recovery (oracle-driven). The scalar token ends at the first
	// whitespace. `JSON` keeps its frozen Phase-3b classification ([aliasRootScalar]);
	// `JsonValue` uses its own profile-driven one ([jsonValueRootScalar]).
	tok := lead
	if i := strings.IndexAny(lead, " \t\r\n"); i >= 0 {
		tok = lead[:i]
	}
	if prof.isJsonValue() {
		return jsonValueRootScalar(b, prof, tok)
	}
	out, err := aliasRootScalar(tok).marshalPublic()
	if err != nil {
		return nil, false, err
	}
	return out, true, nil
}

// jsonValueRootScalar is the Phase-3c ROOT unquoted-scalar recovery: it rebuilds the
// jsonish value BAML would have parsed for the bare token through the SAME two number
// paths the rest of the family uses (alias_number.go) — a STRICT (serde) decode first,
// then the fixing parser's bare-token conversion — and then runs the ordinary alias
// stream coercion over it. Routing the root through the shared coercer (instead of a
// bespoke token table) is what makes the root agree with the identical token inside a
// list or map, and it is what gives the root its three distinct dispositions:
//
//	`1`   -> strict Number, as_i64 Some   -> int arm    -> 1
//	`-0`  -> strict Number, as_i64 None   -> float arm  -> -0        (negative zero)
//	`1.`  -> fixing Number (f64 1.0)      -> float arm  -> 1
//	`1e`  -> fixing: not a number         -> string arm -> "1e"
//	`null`-> strict Null                  -> NULL arm   -> null      (a PRESENT emit)
//	`nul` -> fixing: not a number/keyword -> string arm -> "nul"
//
// All six are LIVE-CAPTURED against stock v0.223 and re-proven per prefix by the strict
// differential.
func jsonValueRootScalar(b *schema.Bundle, prof recAliasProfile, tok string) (json.RawMessage, bool, error) {
	v, err := strictDecodeMode(tok, numModeSerde)
	if err != nil {
		// Not valid strict JSON (or a serde-rejected number): BAML's fixing parser owns
		// the bare token. classifyScalarSerde never fails — the family's `string` arm
		// receives whatever is not a keyword or a number.
		v, err = classifyScalarSerde(tok)
		if err != nil {
			return nil, false, err
		}
		// The token ran to the end of the streamed prefix, so it is still building. The
		// STRICT branch above deliberately leaves `incomplete` false, mirroring the
		// fixing parser's own rule that a value closed by its proper form is Complete;
		// either way the bit is inert for this family (it feeds only the pair guard,
		// which a finite bare scalar never reaches, and the required-done drop, which
		// `JsonValue` has none of).
		v.incomplete = true
	}
	return coerceStreamAliasRoot(b, prof, v)
}

// aliasRootScalar classifies a ROOT unquoted scalar token (trimmed, non-empty, not starting
// with a container/quote) into its streamed public value, reproducing stock BAML v0.223:
//   - "true"/"false" → bool; "null" → [] (the non-nullable list fallback).
//   - a valid i64 integer → int.
//   - any other number-ish token (a float, or an incomplete number like `1.` / `-2.`) → []
//     (BAML drops the incomplete required-done int and the list SingleToArray fallback wins).
//   - anything else (an incomplete keyword, a lone `-`, a bareword) → the raw token as a
//     partial STRING.
func aliasRootScalar(tok string) aliasStreamValue {
	switch tok {
	case "true":
		return aliasStreamValue{kind: akBool, b: true}
	case "false":
		return aliasStreamValue{kind: akBool, b: false}
	case "null":
		return aliasStreamValue{kind: akArray, arr: []aliasStreamValue{}}
	}
	if n, err := strconv.ParseInt(tok, 10, 64); err == nil && strconv.FormatInt(n, 10) == tok {
		// CANONICAL integer only (FormatInt round-trips the token): rejects `-0`, `007`, and
		// other non-canonical forms BAML treats as a float/number → the [] fallback.
		return aliasStreamValue{kind: akInt, i: n}
	}
	if isNumberishToken(tok) {
		// A float / incomplete number → the empty-list fallback.
		return aliasStreamValue{kind: akArray, arr: []aliasStreamValue{}}
	}
	// Bareword / incomplete keyword / lone '-' → unquoted string (raw token, incomplete).
	return aliasStreamValue{kind: akString, s: tok}
}

// parseAliasRootQuotedString parses a ROOT double-quoted string with streaming recovery,
// reproducing BAML's jsonish escape set (\" \\ \n \t \r \b \f decode; every other escape is
// literal) and keeping the partial for an unterminated string. Unlike the shared
// parseDoubleQuotedStream it does NOT defer the escaped-quote (\") case — the served alias
// requires \" → " byte-exact. Returns (decoded, ok); ok is false only for a triple-quoted
// opener (deferred).
func parseAliasRootQuotedString(s string) (string, bool) {
	if strings.HasPrefix(s, `"""`) {
		return "", false // triple-quoted deferred
	}
	p := &fixer{s: s}
	p.pos++ // consume opening '"'
	var sb strings.Builder
	for !p.eof() {
		c := p.s[p.pos]
		if c == '\\' {
			p.decodeDoubleQuoteEscape(&sb)
			continue
		}
		if c == '"' {
			p.pos++
			return sb.String(), true // complete
		}
		sb.WriteByte(c)
		p.pos++
	}
	return sb.String(), true // unterminated → partial
}

// isNumberishToken reports whether tok begins a JSON number (a digit, or a sign followed by a
// digit) — the tokens BAML's jsonish treats as a NUMBER value rather than an unquoted string.
// A lone '-'/'+' (no following digit) is NOT number-ish (it is a bareword string).
func isNumberishToken(tok string) bool {
	if tok == "" {
		return false
	}
	c := tok[0]
	if c >= '0' && c <= '9' {
		return true
	}
	return (c == '-' || c == '+') && len(tok) > 1 && tok[1] >= '0' && tok[1] <= '9'
}

// coerceStreamAliasRoot is the ParseStaticStreamPartial entry for an admitted alias
// family. It coerces the completion-bearing input against the alias, applies the ROOT
// semantic-streaming disposition (the root streaming type is Union[<alias>, null],
// optionalized by BAML's converter), and returns the sorted-public partial bytes +
// whether to EMIT. A dropped root value (an incomplete required-done scalar) is a
// no-emit; the exact root null/[] disposition is pinned by the differential.
//
// For `JsonValue` nothing is ever dropped (see the file header), so the emit decision is
// total and a typed NULL root emits the bytes `null` — a PRESENT partial, never
// collapsed into "no event".
func coerceStreamAliasRoot(b *schema.Bundle, prof recAliasProfile, input value) (json.RawMessage, bool, error) {
	asv, _, dropped, err := coerceStreamAliasValue(b, prof, input, &coerceCtx{})
	if err != nil {
		// A claimed coercion failure (e.g. a circular reference at the true root, which
		// is unreachable for finite JSON) — surface as no-emit; the differential proves
		// native emits wherever BAML does.
		return nil, false, err
	}
	if dropped {
		// The root alias arm was dropped by semantic streaming (incomplete required-done
		// scalar). No partial this tick.
		return nil, false, nil
	}
	if aliasStreamHasNegativeZero(asv) {
		// The one value the served seam cannot carry byte-exactly — see
		// [errNegativeZeroFloat]. The stream carrier is the SAME generated union, so the
		// partial is declined for the identical reason; the route falls back to BAML.
		return nil, false, errNegativeZeroFloat
	}
	out, merr := asv.marshalPublic()
	if merr != nil {
		return nil, false, merr
	}
	return out, true, nil
}

// coerceStreamAliasValue coerces one value against the admitted alias family and applies the
// per-node semantic-streaming rule STRUCTURALLY — from the selected arm + the parsed value's shape, with
// no completion state threaded or read. It returns the carrier, the WINNING union-arm index
// (armIdx — the same value the array-sibling hint carries; -1 on error / no arm), whether the
// node was DROPPED by the required-done rule (`JSON` only — e.g. a float-shaped number on its
// int arm; `JsonValue` never drops), and a claimed error. It REUSES the Phase-3a arm selection (aliasCoerceValue) to pick the winning arm
// index ONCE, then materialises that arm with streaming semantics + fresh per-child re-selection;
// the returned armIdx lets a list caller carry the next-sibling hint without a second selection pass.
func coerceStreamAliasValue(b *schema.Bundle, prof recAliasProfile, input value, cctx *coerceCtx) (aliasStreamValue, int, bool, error) {
	av, _, armIdx, err := aliasCoerceValue(b, prof, input, cctx)
	if err != nil {
		return aliasStreamValue{}, aliasArmNone, true, err
	}
	// Dispatch on the SELECTED ARM, which [aliasValue.kind] already records. The IMPLICIT
	// null arm of a nullable union has no stored-variant index (aliasArmNone), so the
	// carrier kind — not the index — is the authoritative arm identity here; armIdx is
	// returned only as the next array sibling's hint.
	switch av.kind {
	case akInt, akFloat, akString, akBool:
		asv, dropped, aerr := streamScalarArm(prof, av, input)
		return asv, armIdx, dropped, aerr
	case akNull:
		// A PRESENT typed null (nullable fast path). Null is intrinsically complete in
		// BAML's jsonish (value.rs), so it is never required-done-dropped: it EMITS.
		return aliasStreamValue{kind: akNull}, armIdx, false, nil
	case akArray:
		asv, dropped, aerr := streamListArm(b, prof, input, cctx)
		return asv, armIdx, dropped, aerr
	case akMap:
		asv, dropped, aerr := streamMapArm(b, prof, input, cctx)
		return asv, armIdx, dropped, aerr
	default:
		return aliasStreamValue{}, aliasArmNone, false, unsupported("alias stream: unexpected arm kind")
	}
}

// streamScalarArm materialises a leaf arm (int/float/string/bool) and applies the
// required-done drop for the selected FAMILY.
//
//   - `JSON` (frozen Phase-3b behaviour): int/bool are required-done; a number whose text
//     is FLOAT-shaped reaches the int arm and is DROPPED, and every other case is kept.
//   - `JsonValue`: NOTHING is dropped. The float arm absorbs every number whose as_i64 is
//     None, so the int arm only ever wins on a clean COMPLETE i64 token, and the float arm
//     is not required-done in v0.223 — both LIVE-PROVEN and re-proven per prefix by the
//     strict differential (see the file header).
func streamScalarArm(prof recAliasProfile, av aliasValue, input value) (aliasStreamValue, bool, error) {
	switch av.kind {
	case akInt:
		// JSON ONLY: a FLOAT number is DROPPED (int is required-done; a streamed float's
		// precision is "incomplete", so the int drops → its container drops it / the root
		// falls to the list fallback []). A clean INTEGER token is KEPT even mid-stream —
		// BAML treats an integer as a complete i64 value the instant it has digits
		// (LIVE-PROVEN: `[1` → [1], `[1,2` → [1,2]). For `JsonValue` this branch is
		// unreachable: a float-shaped number never reaches its int arm.
		if !prof.isJsonValue() && input.kind == valNumber && strings.ContainsAny(input.numV.String(), ".eE") {
			return aliasStreamValue{}, true, nil // dropped (float precision incomplete)
		}
		return aliasStreamValue{kind: akInt, i: av.i}, false, nil
	case akFloat:
		// JsonValue ONLY. NOT required-done in v0.223: an incomplete float token keeps its
		// partial value (LIVE: `1.`→1, `[1.2`→[1.2]).
		return aliasStreamValue{kind: akFloat, f: av.f}, false, nil
	case akBool:
		// Bool is intrinsically complete once recognized; a recognized bool is kept.
		return aliasStreamValue{kind: akBool, b: av.b}, false, nil
	case akString:
		// String is NOT required-done: kept even when incomplete.
		return aliasStreamValue{kind: akString, s: av.s}, false, nil
	default:
		return aliasStreamValue{}, false, unsupported("alias stream: unexpected scalar arm")
	}
}

// streamListArm materialises the list arm with per-element streaming: each element is
// coerced fresh (re-selecting its arm), and an element DROPPED by semantic streaming is
// filtered out (like BAML's List filter_map). A non-array input is SingleToArray-wrapped;
// the implied element re-enters the alias on the same (alias, input) pair → circular →
// dropped → the empty list (the Phase-3a `JSON` null→[] behaviour). The list itself is
// NOT required-done, so it is kept as a partial regardless of its own completion.
//
// For `JsonValue` a PRESENT typed-null element is never filtered (LIVE: `[null`→[null],
// `[null,null]`→[null,null]) — the filter drops only semantic-streaming DROPS, and that
// family has none.
func streamListArm(b *schema.Bundle, prof recAliasProfile, input value, cctx *coerceCtx) (aliasStreamValue, bool, error) {
	out := aliasStreamValue{kind: akArray}
	if input.kind != valArray {
		// SingleToArray of a non-array: the implied element re-enters the alias on the SAME
		// (alias, input) pair → circular-reference → dropped as ArrayItemParseError → the
		// empty list. This is the Phase-3a `JSON` null→[] mechanism; the element ALWAYS
		// circular-drops there, so materialise [] directly (no recursion). For `JsonValue`
		// the list arm is only ever SELECTED for an actual array (its strict arms are total
		// over the jsonish kinds and a null takes the nullable fast path), so this branch
		// is unreachable for that family.
		out.arr = []aliasStreamValue{}
		return out, false, nil
	}
	var lastHint *int
	for i := range input.arrV {
		childCtx := cctx.enterScopeWithHint(lastHint)
		asv, childArm, dropped, err := coerceStreamAliasValue(b, prof, input.arrV[i], childCtx)
		if err != nil {
			// A claimed error (circular ref) on an element → drop it (ArrayItemParseError).
			continue
		}
		if dropped {
			continue
		}
		out.arr = append(out.arr, asv)
		// Carry this element's winning arm as the next sibling's hint. childArm is the SAME
		// selection coerceStreamAliasValue already computed for this element (via
		// aliasCoerceValue), so reusing it is byte-identical to a fresh re-selection while
		// avoiding a second pass.
		lastHint = aliasSiblingHint(childArm)
	}
	if out.arr == nil {
		out.arr = []aliasStreamValue{}
	}
	return out, false, nil
}

// streamMapArm materialises the map arm with per-ENTRY streaming: each value is coerced
// fresh, and an entry whose value is DROPPED by semantic streaming has its whole key/value
// entry filtered out (BAML's Map filter_map). A `JsonValue` entry whose value is a PRESENT
// typed null is KEPT (LIVE: `{"a":null`→{"a":null}) — that family drops nothing. Entries insert in input order with IndexMap
// overwrite-at-first-position. The map itself is NOT required-done. A non-object input is a
// type mismatch (the arm would not have been selected).
func streamMapArm(b *schema.Bundle, prof recAliasProfile, input value, cctx *coerceCtx) (aliasStreamValue, bool, error) {
	out := aliasStreamValue{kind: akMap}
	index := map[string]int{}
	for i := range input.objV {
		key := input.objV[i].key
		child := cctx.enterScope() // enter_scope(key) resets the hint
		asv, _, dropped, err := coerceStreamAliasValue(b, prof, input.objV[i].val, child)
		if err != nil || dropped {
			// MapValueParseError / dropped value → skip the whole entry.
			continue
		}
		if pos, ok := index[key]; ok {
			out.obj[pos].val = asv // IndexMap overwrite at first position
			continue
		}
		index[key] = len(out.obj)
		out.obj = append(out.obj, aliasStreamEntry{key: key, val: asv})
	}
	return out, false, nil
}
