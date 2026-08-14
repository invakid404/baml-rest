package debaml

import (
	"math"
	"strconv"
)

// De-BAML Slice 7.2c-2 — the EXACT signed-i64 direct-comparison capability.
//
// # What this file is for
//
// The generic constraint evaluator (constraint_eval.go / constraint_profile.go)
// renders a minijinja template and then bounds what it will trust. Its numeric
// bound is a WHITELIST — [exceedsExactIntegerRange] — and the whitelist refuses
// three independent things that a direct integer predicate routinely carries:
//
//	VALUE  |this| >= 2^53                 constraint_profile.go maxAbsInt
//	TOKEN  a literal longer than 15 digits  everyNumericTokenIsProvablySmall
//	SYNTAX any `-`, `+`, `*`, `/`, `%` byte containsArithmeticByte — which a NEGATIVE
//	                                       literal always carries
//
// Every one of those refusals is CORRECT for a general expression: the engine's
// comparison arithmetic is not proven exact there, and over-declining is the safe
// direction. But the static-unary /call route ALREADY SERVES `this > I`
// (checked_static.go), and its mapper coerces any i64 out of the wire
// ([staticCheckedInt]) before calling [EvaluateConstraint]. So a value the schema
// gate admitted could reach the evaluator and be REFUSED after the claim — a
// post-claim `ErrConstraintUnsupported` on an admitted route, which the 7.2c scope
// forbids outright:
//
//	"An admitted schema must have a total, byte-proven native outcome for every
//	 value that native coercion can produce."
//
// Slice 7.2c-1 MEASURED that hazard rather than predicting it: of the 37
// (literal, value) pairs its boundary matrix drives over literals the SHIPPED
// `this > I` fingerprint admits, the production evaluator refused 31 after the
// admission decision (`this > -1` over `this = 0` is one of them). This file
// closes it.
//
// # What it does
//
// It is a CLOSED, TOTAL, EXACT decision procedure for one grammar:
//
//	expr    := "this" SP OP SP I
//	OP      := ">" | ">=" | "<" | "<=" | "==" | "!="
//	I       := strconv.FormatInt(<any int64>, 10)
//	SP      := one ASCII space (U+0020), exactly one, never a tab or newline
//
// evaluated against a `this` that is BamlValue::Int, by comparing two native Go
// `int64`s. There is no float64 anywhere on the path, no arithmetic that could
// overflow (a comparison is not a subtraction), and no engine involvement at all —
// so the answer is exact at every point of the i64 range, including the two
// endpoints and both sides of ±2^53 where float64 conflates neighbours.
//
// TOTALITY is therefore structural rather than tested-into-existence: the parse
// either recognises the whole expression or it does not, and when it does, `>`,
// `>=`, `<`, `<=`, `==`, `!=` on two `int64`s are total functions. There is no
// input for which this capability can return "unsupported".
// TestDirectI64CapabilityIsTotalAcrossTheWholeRange drives the claim anyway,
// across the endpoints, ±2^53±1, zero, negatives and both sides of every literal.
//
// # Scope: this is EXACTNESS, not a wider claim
//
// The capability is consulted for EXACTLY this grammar and nothing else. Every
// other expression — filters, fields, arithmetic, compounds, non-i64 operands,
// floats, a non-Int `this`, even a `this OP I` with one byte out of place — falls
// straight through to the generic evaluator with its existing fail-closed profile,
// `|int| >= 2^53` refusal included. TestDirectI64ExactnessIsScoped proves that in
// both directions: the generic guard still refuses everything it refused before,
// and nothing outside the grammar reaches this file.
//
// # Authority
//
// Stock v0.223 CFFI, captured by Slice 7.2c-1 in internal/debaml/predicatewire:
// the six-operator wire/error oracle (24 nested + 24 top-level rows), and the
// 13-threshold × 6-operator direct-i64 boundary matrix, of which the report
// records: "Stock answers all 222 rows, exactly. It distinguishes 2^53 from
// 2^53+1 (where float64 conflates them) and evaluates correctly at MinInt64."
// That is the behaviour reproduced here. Native output is never re-fed to the
// CFFI; the captures are one-way fixtures.
//
// # This is a CAPABILITY, not an admission
//
// Recognising all six operators here does NOT admit them. Admission is decided by
// [staticCheckedProfileOf], whose allowed-operator MANIFEST
// ([staticCheckedManifestTokens]) is `>` and only `>`; the other five decline at
// every schema gate exactly as they did before, and the served row count stays 4.
// The evaluator has always parsed all six comparisons (constraint_operator.go's
// grammar) — what changes here is that for the direct i64 form it now DECIDES them
// exactly instead of refusing on a whitelist written for general arithmetic. 7.2c-3
// is the slice that may widen the manifest, and it may do so only per-operator,
// against 7.2c-1's captures.

// directCompareOp is one direct comparison operator, as DATA.
//
// The operator set is a table rather than a switch because two independent things
// have to be keyed by it and must not drift apart: the EXACT decision procedure
// below, and the static classifier's allowed-operator manifest in
// checked_static.go. A switch would let the two disagree silently — the classifier
// admitting an operator the evaluator cannot decide, or the reverse — which is
// precisely the shape of the totality gap this slice closes.
type directCompareOp struct {
	// ID is the stable ASCII identifier for this operator. It is the same key
	// internal/debaml/predicatewire uses for its per-operator stock captures
	// (`gt`, `ge`, `lt`, `le`, `eq`, `ne`), so a capture and the operator it is
	// authority for can be paired by name.
	ID string
	// Token is the BAML source operator exactly as it appears between `this` and
	// the literal. It is the byte sequence stock retains in Check.Expression and
	// quotes in an assertion cause, so it is part of the wire contract, not a
	// spelling choice.
	Token string
	// Holds is the EXACT decision procedure: a comparison of two native Go int64
	// values. It is a comparison and never an arithmetic combination, so it cannot
	// overflow and is total over the whole i64 × i64 domain.
	//
	// `this` is the left operand and the literal the right, in the source order —
	// `this < 0` is Holds(this, 0), not the reverse. TestDirectI64OperatorsAreNotSymmetric
	// pins the orientation, because `<` and `>` swapped would still pass a test
	// that only drove symmetric operands.
	Holds func(this, literal int64) bool
}

// directCompareOperators is the WHOLE capability: the six direct comparisons the
// 7.2c scope names, in the scope's own order.
//
// It is returned by value from a function rather than exposed as a package
// variable so no caller — production or test — can mutate the capability set of a
// running binary. Every consumer that needs a narrower set FILTERS this one
// ([directCompareManifest]); nothing constructs a second table.
func directCompareOperators() []directCompareOp {
	return []directCompareOp{
		{ID: "gt", Token: ">", Holds: func(this, literal int64) bool { return this > literal }},
		{ID: "ge", Token: ">=", Holds: func(this, literal int64) bool { return this >= literal }},
		{ID: "lt", Token: "<", Holds: func(this, literal int64) bool { return this < literal }},
		{ID: "le", Token: "<=", Holds: func(this, literal int64) bool { return this <= literal }},
		{ID: "eq", Token: "==", Holds: func(this, literal int64) bool { return this == literal }},
		{ID: "ne", Token: "!=", Holds: func(this, literal int64) bool { return this != literal }},
	}
}

// directCompareManifest selects the operators named by tokens, in the CAPABILITY's
// order, and reports whether every requested token was found.
//
// It fails closed on an unknown token: a manifest naming an operator the capability
// cannot decide would be a classifier that admits what the evaluator must refuse,
// so it yields no manifest at all rather than a silently shorter one.
func directCompareManifest(tokens []string) ([]directCompareOp, bool) {
	want := make(map[string]bool, len(tokens))
	for _, tok := range tokens {
		if want[tok] {
			return nil, false // a duplicate token is a manifest that was not written carefully
		}
		want[tok] = true
	}
	var out []directCompareOp
	for _, op := range directCompareOperators() {
		if want[op.Token] {
			out = append(out, op)
			delete(want, op.Token)
		}
	}
	if len(want) != 0 {
		return nil, false // a token no operator carries
	}
	return out, true
}

// directI64Subject is the ONLY left operand the direct grammar accepts: the bare
// `this`. A field, an index, a filter or a literal on the left is a different
// expression and belongs to the generic evaluator.
const directI64Subject = "this"

// directI64Sep is the separator the grammar requires on each side of the operator:
// exactly one ASCII space.
//
// One, not "some whitespace". BAML's own `{{ this > 0 }}` attribute produces
// exactly this spacing and stock retains exactly this string in Check.Expression
// (measured by internal/debaml/predicatewire's padding probes across both operator
// widths), so any other spelling — no space, two spaces, a tab, a newline — is text
// no capture describes. It declines to the generic evaluator rather than being
// normalised, because normalising it here would change the bytes the carrier and
// the assertion cause quote.
const directI64Sep = " "

// directI64Comparison is one parsed `this OP I`.
type directI64Comparison struct {
	op      directCompareOp
	literal int64
}

// holds decides the comparison EXACTLY, in native int64.
func (c directI64Comparison) holds(this int64) bool { return c.op.Holds(this, c.literal) }

// parseDirectI64Comparison recognises `this OP I` over the given operator manifest.
//
// It is a whole-string match, never a prefix one: an expression that merely STARTS
// with the grammar (`this > 0 and this < 9`) or merely ends with it (` this > 0`)
// is not this grammar and is refused, so the caller can never decide a predicate
// from a fragment of one.
//
// AMBIGUITY. The one-character and two-character tokens share a prefix (`>` and
// `>=`), so a naive first-match loop could commit to the wrong one. It cannot
// happen here, because each candidate is matched as the WHOLE fixed string
// `"this" SP TOKEN SP` — for the input `this >= 0`, the `>` candidate compares its
// required separator against the byte `=` and fails outright rather than consuming
// a shorter operator. TestDirectI64ManifestIsUnambiguous proves no two manifest
// entries can both match one expression, over the full capability set.
func parseDirectI64Comparison(expr string, manifest []directCompareOp) (directI64Comparison, bool) {
	for _, op := range manifest {
		head := directI64Subject + directI64Sep + op.Token + directI64Sep
		if len(expr) <= len(head) || expr[:len(head)] != head {
			continue
		}
		literal, ok := directI64Literal(expr[len(head):])
		if !ok {
			// The head matched but the tail is not a canonical i64. There is no
			// second operator that could match this head (see AMBIGUITY above), so
			// this is a definitive refusal rather than a reason to keep looking.
			return directI64Comparison{}, false
		}
		return directI64Comparison{op: op, literal: literal}, true
	}
	return directI64Comparison{}, false
}

// directI64Literal parses I: a CANONICAL base-10 signed decimal int64.
//
// Canonical means the text is exactly what [strconv.FormatInt] would produce for
// the value it parses to, which is what rejects `+5`, `007`, `-0`, `1_000`, `5.0`,
// `0x10`, `1e3` and `9223372036854775808`. That matters more than it looks:
// Slice 7.2c-1 drove those spellings through the real CFFI and found FOUR of the
// five it tried COMPILE and evaluate under stock (`007` reads as 7, `1_000` as
// 1000, `5.0` as a float, and `9223372036854775808` does NOT wrap to i64). So the
// grammar cannot lean on BAML rejecting them upstream — it has to reject them
// itself, and it does so here, at the one place the literal is read.
func directI64Literal(s string) (int64, bool) {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || strconv.FormatInt(n, 10) != s {
		return 0, false
	}
	return n, true
}

// directI64Expression renders the canonical source text for one operator and
// literal — the exact bytes stock v0.223 retains in Check.Expression and quotes in
// an assertion cause.
//
// It is the inverse of [parseDirectI64Comparison] and exists so length bounds and
// fixtures are DERIVED from the grammar rather than restated beside it;
// TestDirectI64ExpressionRoundTrips drives the two against each other.
func directI64Expression(op directCompareOp, literal int64) string {
	return directI64Subject + directI64Sep + op.Token + directI64Sep + strconv.FormatInt(literal, 10)
}

// directI64LongestExpression is the longest canonical expression a manifest can
// produce, in BYTES.
//
// The longest literal over the whole i64 domain is [math.MinInt64]: its
// FormatInt text is 20 bytes, one more than [math.MaxInt64]'s 19, and no other
// int64 formats longer than either (every magnitude below 2^63 has at most 19
// digits, and only the minimum carries a sign byte on top of 19).
// TestDirectI64LongestLiteralIsTheMinimum drives both endpoints and a spread of
// interior values rather than asserting the arithmetic in a comment.
//
// It FAILS CLOSED on an empty manifest by falling back to the whole capability:
// an empty manifest would otherwise yield the empty string and hand every caller
// deriving a length bound from it a bound that is too LARGE — the exact defect the
// 7.2c scope names ("It must not preserve a now-too-large limit").
func directI64LongestExpression(manifest []directCompareOp) string {
	if len(manifest) == 0 {
		manifest = directCompareOperators()
	}
	longest := ""
	for _, op := range manifest {
		for _, literal := range []int64{math.MinInt64, math.MaxInt64} {
			if e := directI64Expression(op, literal); len(e) > len(longest) {
				longest = e
			}
		}
	}
	return longest
}

// evaluateDirectI64 is THE exact capability, and the function [EvaluateConstraint]
// consults before it renders anything.
//
// It reports `decided = false` for every input outside the closed grammar — a
// `this` that is not BamlValue::Int, or an expression that is not exactly
// `this OP I` over the six operators — and the caller then proceeds to the generic
// evaluator completely unchanged. It NEVER returns an error, because there is no
// input inside the grammar it cannot decide.
//
// `decided` is a separate return rather than an error precisely so that "outside
// the grammar" cannot be confused with "unsupported": the first is a routing fact
// and the second is a claim about BAML, and collapsing them is how a decline turns
// into a served wrong answer.
func evaluateDirectI64(this ConstraintValue, expression string) (held bool, decided bool) {
	if this.kind != ConstraintKindInt {
		return false, false
	}
	cmp, ok := parseDirectI64Comparison(expression, directCompareOperators())
	if !ok {
		return false, false
	}
	return cmp.holds(this.i), true
}
