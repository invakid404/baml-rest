package debaml

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/invakid404/baml-rest/internal/schema"
)

// De-BAML Phase 3b (recursive-alias STREAMING) — the completion-aware alias stream
// coercer + BAML semantic-streaming, the streaming twin of the Phase-3a final coercer
// (alias_coerce.go).
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

// aliasStreamValue is the private carrier for a partially-coerced JSON value. Unlike the
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

// ParseAliasStreamPartial is the gate-free native partial entry the Phase-3b per-prefix
// DIFFERENTIAL drives (the streaming twin of ParseStaticBundle): it strips comments,
// extracts a completion-bearing jsonish value from the accumulated prefix, coerces it
// against the JSON alias with semantic streaming, and returns (sorted-public bytes, emit).
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
		if v, ok := streamExtractCandidate(stripped); ok {
			return coerceStreamAliasRoot(b, v)
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
	// ROOT unquoted-scalar recovery (oracle-driven): a clean integer → int; complete
	// true/false → bool; null → [] (non-nullable list fallback); a float / incomplete
	// number → [] (the incomplete required-done int drops → SingleToArray → circular → []);
	// every other unquoted token (an incomplete keyword, a lone `-`, a bareword) → the token
	// as an (incomplete) STRING. The scalar token ends at the first whitespace.
	tok := lead
	if i := strings.IndexAny(lead, " \t\r\n"); i >= 0 {
		tok = lead[:i]
	}
	out, err := aliasRootScalar(tok).marshalPublic()
	if err != nil {
		return nil, false, err
	}
	return out, true, nil
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

// coerceStreamAliasRoot is the ParseStaticStreamPartial entry for the admitted JSON alias.
// It coerces the completion-bearing input against the alias, applies the ROOT semantic-
// streaming disposition (the root streaming type is Union[JSON, null], optionalized by
// BAML's converter), and returns the sorted-public partial bytes + whether to EMIT. A
// dropped root value (an incomplete required-done scalar) is a no-emit today; the exact
// root null/[] disposition is pinned by the differential.
func coerceStreamAliasRoot(b *schema.Bundle, input value) (json.RawMessage, bool, error) {
	asv, _, dropped, err := coerceStreamAliasValue(b, input, &coerceCtx{})
	if err != nil {
		// A claimed coercion failure (e.g. a circular reference at the true root, which
		// is unreachable for finite JSON) — surface as no-emit; the differential proves
		// native emits wherever BAML does.
		return nil, false, err
	}
	if dropped {
		// The root JSON arm was dropped by semantic streaming (incomplete required-done
		// scalar). No partial this tick.
		return nil, false, nil
	}
	out, merr := asv.marshalPublic()
	if merr != nil {
		return nil, false, merr
	}
	return out, true, nil
}

// coerceStreamAliasValue coerces one value against the JSON alias and applies the per-node
// semantic-streaming rule STRUCTURALLY — from the selected arm + the parsed value's shape, with
// no completion state threaded or read. It returns the carrier, the WINNING union-arm index
// (armIdx — the same value the array-sibling hint carries; -1 on error / no arm), whether the
// node was DROPPED by the required-done rule (e.g. a float-shaped number on the int arm), and a
// claimed error. It REUSES the Phase-3a arm selection (aliasCoerceValue) to pick the winning arm
// index ONCE, then materialises that arm with streaming semantics + fresh per-child re-selection;
// the returned armIdx lets a list caller carry the next-sibling hint without a second selection pass.
func coerceStreamAliasValue(b *schema.Bundle, input value, cctx *coerceCtx) (aliasStreamValue, int, bool, error) {
	av, _, armIdx, err := aliasCoerceValue(b, input, cctx)
	if err != nil {
		return aliasStreamValue{}, -1, true, err
	}
	variants, verr := aliasVariants(b)
	if verr != nil {
		return aliasStreamValue{}, -1, false, verr
	}
	if armIdx < 0 || armIdx >= len(variants) {
		return aliasStreamValue{}, -1, false, unsupported("alias stream: arm index out of range")
	}
	arm := variants[armIdx]
	switch arm.Kind {
	case schema.TypePrimitive:
		asv, dropped, aerr := streamScalarArm(av, arm.Primitive, input)
		return asv, armIdx, dropped, aerr
	case schema.TypeList:
		asv, dropped, aerr := streamListArm(b, input, cctx)
		return asv, armIdx, dropped, aerr
	case schema.TypeMap:
		asv, dropped, aerr := streamMapArm(b, input, cctx)
		return asv, armIdx, dropped, aerr
	default:
		return aliasStreamValue{}, -1, false, unsupported("alias stream: unexpected arm kind")
	}
}

// streamScalarArm materialises a leaf arm (int/string/bool) and applies the required-done
// drop. int/bool are required-done: an incomplete one is DROPPED (returns dropped=true).
// string is NOT required-done: an incomplete string is kept as a partial.
func streamScalarArm(av aliasValue, prim schema.PrimitiveKind, input value) (aliasStreamValue, bool, error) {
	switch prim {
	case schema.PrimitiveInt:
		// A FLOAT number is DROPPED (int is required-done; a streamed float's precision is
		// "incomplete", so the int drops → its container drops it / the root falls to the
		// list fallback []). A clean INTEGER token is KEPT even mid-stream — BAML treats an
		// integer as a complete i64 value the instant it has digits (LIVE-PROVEN: `[1` → [1],
		// `[1,2` → [1,2]).
		if input.kind == valNumber && strings.ContainsAny(input.numV.String(), ".eE") {
			return aliasStreamValue{}, true, nil // dropped (float precision incomplete)
		}
		return aliasStreamValue{kind: akInt, i: av.i}, false, nil
	case schema.PrimitiveBool:
		// Bool is intrinsically complete once recognized; a recognized bool is kept.
		return aliasStreamValue{kind: akBool, b: av.b}, false, nil
	case schema.PrimitiveString:
		// String is NOT required-done: kept even when incomplete.
		return aliasStreamValue{kind: akString, s: av.s}, false, nil
	default:
		return aliasStreamValue{}, false, unsupported("alias stream: unexpected scalar arm")
	}
}

// streamListArm materialises the list arm with per-element streaming: each element is
// coerced fresh (re-selecting its arm), and an element DROPPED by semantic streaming is
// filtered out (like BAML's List filter_map). A non-array input is SingleToArray-wrapped;
// the implied element re-enters the alias on the same (JSON, input) pair → circular →
// dropped → the empty list (matching the Phase-3a null→[] behaviour). The list itself is
// NOT required-done, so it is kept as a partial regardless of its own completion.
func streamListArm(b *schema.Bundle, input value, cctx *coerceCtx) (aliasStreamValue, bool, error) {
	out := aliasStreamValue{kind: akArray}
	if input.kind != valArray {
		// SingleToArray of a non-array: the implied element re-enters the alias on the SAME
		// (JSON, input) pair → circular-reference → dropped as ArrayItemParseError → the
		// empty list. This is the Phase-3a null→[] mechanism; the element ALWAYS
		// circular-drops for this family, so materialise [] directly (no recursion).
		out.arr = []aliasStreamValue{}
		return out, false, nil
	}
	var lastHint *int
	for i := range input.arrV {
		childCtx := cctx.enterScopeWithHint(lastHint)
		asv, childArm, dropped, err := coerceStreamAliasValue(b, input.arrV[i], childCtx)
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
		hh := childArm
		lastHint = &hh
	}
	if out.arr == nil {
		out.arr = []aliasStreamValue{}
	}
	return out, false, nil
}

// streamMapArm materialises the map arm with per-ENTRY streaming: each value is coerced
// fresh, and an entry whose value is DROPPED by semantic streaming has its whole key/value
// entry filtered out (BAML's Map filter_map). Entries insert in input order with IndexMap
// overwrite-at-first-position. The map itself is NOT required-done. A non-object input is a
// type mismatch (the arm would not have been selected).
func streamMapArm(b *schema.Bundle, input value, cctx *coerceCtx) (aliasStreamValue, bool, error) {
	out := aliasStreamValue{kind: akMap}
	index := map[string]int{}
	for i := range input.objV {
		key := input.objV[i].key
		child := cctx.enterScope() // enter_scope(key) resets the hint
		asv, _, dropped, err := coerceStreamAliasValue(b, input.objV[i].val, child)
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
