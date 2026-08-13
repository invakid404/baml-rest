//go:build integration

package predicatewire

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// The PROJECT TABLES. Every declaration stock compiles, and every byte stock is given,
// is here or in the row tables that name these projects.
//
// Nothing in these tables is derived from native output. Each drive's raw text is a
// fixed assistant string; what comes back is compared against a pinned literal.

// ---------------------------------------------------------------------------
// (A) The six direct operators, on BOTH name-pinned nested families.
// ---------------------------------------------------------------------------

// pwOperator is one direct comparison, with the values that make its predicate true and
// false against the SAME canonical literal.
//
// One literal across all six is deliberate: it makes every byte difference between two
// operator captures attributable to the OPERATOR and to nothing else. The literal is `0`
// — the same one the 7.2b fixtures use — so the six captures are also directly
// comparable with checkedwire's `this > 0` rows.
type pwOperator struct {
	// ID is the project/label suffix; Op is the BAML source operator.
	ID string
	Op string
	// TrueVal and FalseVal are `confidence` values that make `this OP 0` hold and fail.
	TrueVal  int64
	FalseVal int64
}

// pwCanonicalLiteral is the `I` every operator project compares against.
const pwCanonicalLiteral = 0

// expr is the canonical inner text of this operator's predicate: exactly the string the
// 7.2c grammar would admit, and exactly what stock reports in Check.Expression.
func (o pwOperator) expr() string {
	return fmt.Sprintf("this %s %d", o.Op, pwCanonicalLiteral)
}

func (o pwOperator) projectKey() string { return "op_" + o.ID }

// pwOperators is the whole six-operator manifest. It is COMPLETE by construction:
// [TestOperatorManifestIsTheWholeGrammar] checks it against the scope's operator set, so
// a quietly dropped operator is a failure rather than a smaller matrix.
func pwOperators() []pwOperator {
	return []pwOperator{
		{ID: "gt", Op: ">", TrueVal: 9, FalseVal: -1},
		{ID: "ge", Op: ">=", TrueVal: 0, FalseVal: -1},
		{ID: "lt", Op: "<", TrueVal: -1, FalseVal: 9},
		{ID: "le", Op: "<=", TrueVal: 0, FalseVal: 9},
		{ID: "eq", Op: "==", TrueVal: 0, FalseVal: 9},
		{ID: "ne", Op: "!=", TrueVal: 9, FalseVal: 0},
	}
}

// pwNestedRaw is the assistant text for one nested-family drive.
func pwNestedRaw(confidence int64) string {
	return fmt.Sprintf(`{"answer": "sunny", "confidence": %d}`, confidence)
}

// pwCheckedClassDecl renders the CHECK family with a given attribute list.
func pwCheckedClassDecl(attrs string) string {
	return fmt.Sprintf("class %s {\n  answer string\n  confidence int %s\n}\n", pwCheckedClass, attrs)
}

// pwAssertClassDecl renders the ASSERT family with a given attribute list.
func pwAssertClassDecl(attrs string) string {
	return fmt.Sprintf("class %s {\n  answer string\n  confidence int %s\n}\n", pwAssertClass, attrs)
}

// The two function names every operator project carries. They are the same in all six
// projects on purpose: the projects are isolated, so the role is what the name means.
const (
	pwFnChecked = "Checked"
	pwFnAssert  = "Assert"
)

// pwOperatorProjects is one isolated project per operator, each declaring BOTH pinned
// classes once with that operator's predicate.
//
// This is the shape the whole package exists for. A single project could not hold them:
// six `class StaticCheckedAnswer` declarations are six definitions of one name, and
// renaming any of them would capture a different family.
func pwOperatorProjects() []pwProject {
	var out []pwProject
	for _, o := range pwOperators() {
		out = append(out, pwProject{
			Key: o.projectKey(),
			Doc: "Slice 7.2c-1 OPERATOR CAPTURE: the two name-pinned static families carrying\n" +
				"the direct comparison `" + o.expr() + "`. Isolated in its own project because the\n" +
				"class names are PINNED — six predicate variants of one name cannot coexist.",
			Decls: []string{
				pwCheckedClassDecl(fmt.Sprintf("@check(positive, {{ %s }})", o.expr())),
				pwAssertClassDecl(fmt.Sprintf("@assert(positive, {{ %s }})", o.expr())),
			},
			Funcs: []pwFunc{
				{Name: pwFnChecked, Doc: "WIRE: the @check family, driven true and false.", Target: pwCheckedClass},
				{Name: pwFnAssert, Doc: "WIRE + ERROR BYTES: the @assert family, driven true and false.", Target: pwAssertClass},
			},
			WantCompile: true,
		})
	}
	return out
}

// ---------------------------------------------------------------------------
// (A2) The same six operators, TOP-LEVEL.
// ---------------------------------------------------------------------------

// pwTopLevelKey is the single project carrying every top-level operator probe.
const pwTopLevelKey = "toplevel"

// The two function-name prefixes inside that project.
const (
	pwTopCheckPrefix  = "Top_Check_"
	pwTopAssertPrefix = "Top_Assert_"
)

// pwTopCheckFn / pwTopAssertFn name one operator's top-level probes.
func pwTopCheckFn(o pwOperator) string  { return pwTopCheckPrefix + o.ID }
func pwTopAssertFn(o pwOperator) string { return pwTopAssertPrefix + o.ID }

// pwTopLevelProject carries all twelve top-level probes: a bare `int` target carrying the
// constraint DIRECTLY, per operator, at both levels.
//
// The scope's differential requirements ask for nested AND top-level check pass/fail and
// assert pass/fail for every direct operator, and the two are genuinely different
// outcomes rather than one inferable from the other:
//
//   - a top-level check emits the carrier as the WHOLE response, with no enclosing
//     object, so its bytes are not a substring of the nested form; and
//   - a top-level failing assert has NO required-field wrapper at all — the nested error's
//     entire `Failed while parsing required fields` / `Failed to parse field confidence`
//     chain comes from the field position, not from the assert.
//
// They all fit ONE project because a bare target declares no class, so there is no
// name-pinned declaration to collide — the isolation that the nested captures need does
// not apply here, and twelve extra runtimes would buy nothing.
func pwTopLevelProject() pwProject {
	p := pwProject{
		Key: pwTopLevelKey,
		Doc: "Slice 7.2c-1 TOP-LEVEL OPERATOR CAPTURE: the six direct comparisons on a BARE `int`\n" +
			"target, at both levels. The nested twins live in the op_* projects; these are the\n" +
			"outcomes that cannot be inferred from them — an unenclosed carrier, and an assert\n" +
			"failure with no required-field wrapper.",
		WantCompile: true,
	}
	for _, o := range pwOperators() {
		p.Funcs = append(p.Funcs,
			pwFunc{
				Name:   pwTopCheckFn(o),
				Doc:    "WIRE: top-level @check " + strconv.Quote(o.expr()) + ", driven true and false.",
				Target: fmt.Sprintf("int @check(%s, {{ %s }})", pwCheckedLabel, o.expr()),
			},
			pwFunc{
				Name:   pwTopAssertFn(o),
				Doc:    "WIRE + ERROR BYTES: top-level @assert " + strconv.Quote(o.expr()) + ", driven true and false.",
				Target: fmt.Sprintf("int @assert(%s, {{ %s }})", pwCheckedLabel, o.expr()),
			},
		)
	}
	return p
}

// ---------------------------------------------------------------------------
// (B) Expression text: source padding.
// ---------------------------------------------------------------------------

// pwExprTextKey is the single project that carries every padding probe.
const pwExprTextKey = "exprtext"

// pwPadProbe is one `{{ PAD expr PAD }}` source-padding probe.
type pwPadProbe struct {
	// Label is the @check label AND the probe id.
	Label string
	// Pad is the number of ASCII spaces on EACH side inside the `{{ }}`.
	Pad int
	// Op is the operator whose canonical expression is padded.
	Op pwOperator
}

// canonical is the expression text with no padding — what the 7.2c grammar calls the
// canonical inner text, and what stock is expected to report.
func (p pwPadProbe) canonical() string { return p.Op.expr() }

// source is the attribute's inner text as written in the .baml, padding included.
func (p pwPadProbe) source() string {
	pad := strings.Repeat(" ", p.Pad)
	return pad + p.canonical() + pad
}

// pwPadProbes is zero and one ASCII space each side for ALL SIX operators — the two
// paddings the 7.2c grammar would admit — plus TWO spaces on the two-byte operator
// `>=`.
//
// The pad-2 probe is not an admission candidate. checkedwire measured 0/1/2 for the
// ONE-byte `>` and found stock reports the same unpadded string for all three; this row
// asks the same question of a TWO-byte operator, so the padding rule is measured on both
// widths rather than generalised from one. The 7.2c fingerprint still admits at most one
// space, and TestStaticCheckedSiblingsDecline pins `  this >= 0  ` as a decline.
func pwPadProbes() []pwPadProbe {
	var out []pwPadProbe
	for _, o := range pwOperators() {
		out = append(out,
			pwPadProbe{Label: "pad0_" + o.ID, Pad: 0, Op: o},
			pwPadProbe{Label: "pad1_" + o.ID, Pad: 1, Op: o},
		)
		if o.ID == pwPadTwoOperator {
			out = append(out, pwPadProbe{Label: "pad2_" + o.ID, Pad: 2, Op: o})
		}
	}
	return out
}

// pwPadTwoOperator is the ONE operator the two-space probe runs on: the two-byte `>=`.
// checkedwire already measured 0/1/2 on the one-byte `>`, so what is open is whether the
// wider operator behaves the same, not whether a third padding should be admitted.
const pwPadTwoOperator = "ge"

// pwExprTextProject carries every padding probe on a BARE `int` target.
//
// A bare target is correct here and is not a weakening: what a padding probe measures is
// the string stock retains in Check.Expression, which is a property of the ATTRIBUTE, not
// of where the constrained node sits. Driving them on a bare target is also what lets all
// THIRTEEN — six operators at pad 0, the same six at pad 1, and the single two-byte `>=`
// probe at pad 2 — share ONE project, because a nested probe would need the pinned class
// and the pinned class may be declared only once per project.
//
// [TestStockPaddingIsStrippedForEveryOperator] logs the per-pad counts and fails if
// pad 0 or pad 1 stops covering all six, so this sentence cannot drift away from the
// table it describes without a red test.
func pwExprTextProject() pwProject {
	p := pwProject{
		Key: pwExprTextKey,
		Doc: "Slice 7.2c-1 EXPRESSION TEXT: what stock retains in Check.Expression for each of the\n" +
			"six canonical predicates under zero, one and two ASCII spaces of source padding.\n" +
			"Bare `int` targets, so all of them fit one project without touching a pinned class name.",
		WantCompile: true,
	}
	for _, probe := range pwPadProbes() {
		p.Funcs = append(p.Funcs, pwFunc{
			Name:   "Pad_" + probe.Label,
			Doc:    fmt.Sprintf("EXPRESSION TEXT: %d space(s) each side of %q.", probe.Pad, probe.canonical()),
			Target: fmt.Sprintf("int @check(%s, {{%s}})", probe.Label, probe.source()),
		})
	}
	return p
}

// ---------------------------------------------------------------------------
// (C) Canonical-literal discriminators.
// ---------------------------------------------------------------------------

// pwLiteralProbe is one NON-canonical integer literal spelling, on the PINNED check
// family.
//
// The 7.2c grammar admits exactly `strconv.FormatInt` output, so `+5`, `007`, `1_000`, a
// float and an i64 overflow are all outside it. What is NOT known without measuring is
// what STOCK does with them: accept and evaluate, accept and report a different retained
// expression, or reject the project at compile time. Each gets its own isolated project
// precisely because a compile rejection would otherwise take its neighbours down.
type pwLiteralProbe struct {
	// ID names the project (lit_<ID>).
	ID string
	// Literal is the right-hand side as written in the .baml.
	Literal string
	// Doc says which rule of the canonical grammar it violates.
	Doc string
	// Confidence is the value driven when the project compiles.
	Confidence int64
	// Rejected records that BAML's OWN parser refuses this attribute text, so the
	// project never compiles and there is no wire capture to take. It is a measured
	// disposition, not a convenience: [TestPredicateWireProjectsAreTheOnesStockDrives]
	// fails just as loudly if a project pinned as rejected starts compiling.
	Rejected bool
}

func (p pwLiteralProbe) projectKey() string { return "lit_" + p.ID }
func (p pwLiteralProbe) expr() string       { return "this > " + p.Literal }

// pwLiteralProbes are the five non-canonical spellings the 7.2b fingerprint already
// rejects (internal/debaml.staticCheckedThreshold), captured against stock so the
// rejection is a measured over-decline rather than an unexamined one.
func pwLiteralProbes() []pwLiteralProbe {
	return []pwLiteralProbe{
		{ID: "plus5", Literal: "+5", Doc: "an explicit `+` sign, which FormatInt never emits", Confidence: 9, Rejected: true},
		{ID: "leading_zeros", Literal: "007", Doc: "leading zeros, which FormatInt never emits", Confidence: 9},
		{ID: "underscore", Literal: "1_000", Doc: "a digit separator, which FormatInt never emits", Confidence: 9},
		{ID: "float", Literal: "5.0", Doc: "a float spelling of an integer threshold", Confidence: 9},
		{ID: "overflow", Literal: "9223372036854775808", Doc: "one past math.MaxInt64", Confidence: 9},
	}
}

// pwLiteralProjects is one isolated project per non-canonical literal, each declaring
// the PINNED check family so the capture is about the admitted family's own attribute
// text.
//
// The measured split is four/one: BAML's attribute grammar ACCEPTS `007`, `1_000`, `5.0`
// and the i64 overflow and evaluates all four, which is precisely why the native
// fingerprint has to reject them ITSELF rather than relying on a parse error upstream. It
// REFUSES `+5` at parse time. WantCompile carries that measurement per project, so either
// disposition changing in a future BAML is a visible decision rather than a silent one.
func pwLiteralProjects() []pwProject {
	var out []pwProject
	for _, p := range pwLiteralProbes() {
		disposition := "Isolated so a compile rejection is attributable to this spelling alone."
		if p.Rejected {
			disposition = "RECORDED: stock's own Jinja parser REFUSES this spelling, so the project does not compile."
		}
		out = append(out, pwProject{
			Key: p.projectKey(),
			Doc: "Slice 7.2c-1 CANONICAL-LITERAL DISCRIMINATOR: `" + p.expr() + "` — " + p.Doc + ".\n" +
				disposition,
			Decls:       []string{pwCheckedClassDecl(fmt.Sprintf("@check(positive, {{ %s }})", p.expr()))},
			Funcs:       []pwFunc{{Name: pwFnChecked, Doc: "WIRE: the non-canonical literal, driven once.", Target: pwCheckedClass}},
			WantCompile: !p.Rejected,
		})
	}
	return out
}

// ---------------------------------------------------------------------------
// (D) The direct-i64 boundary matrix.
// ---------------------------------------------------------------------------

// pwBoundaryThreshold is one integer literal the six operators are all measured against.
type pwBoundaryThreshold struct {
	// ID names the project (bound_<ID>).
	ID string
	// N is the literal, written into the .baml as strconv.FormatInt(N, 10).
	N int64
	// Doc says why this threshold is on the boundary.
	Doc string
}

func (b pwBoundaryThreshold) projectKey() string { return "bound_" + b.ID }
func (b pwBoundaryThreshold) literal() string    { return strconv.FormatInt(b.N, 10) }

// maxExactInt is 2^53 — the magnitude at which internal/debaml's generic evaluator
// profile starts REFUSING (constraint_profile.go's exceedsExactIntegerRange tests
// `>= 2^53`).
//
// It is NOT where float64 stops being exact, and the two must not be conflated. Every
// integer of magnitude up to and INCLUDING 2^53 round-trips through float64 exactly;
// 2^53+1 is the first one that does not, because it needs 54 significand bits and
// collapses onto 2^53. The guard therefore sits one step BEFORE the first inexact
// integer — a deliberate conservatism, not the representability boundary itself.
//
// The boundary axis samples both sides of both facts: `exact_at` is the guard's own
// threshold and `exact_above` is the first genuinely unrepresentable integer, so a change
// to either would move a different row.
const maxExactInt int64 = 1 << 53

// pwBoundaryThresholds is the whole literal axis: zero, both signs, both sides of
// ±2^53, and both i64 endpoints with their inward neighbours.
//
// Every one of these is a value the strict i64 extractor in the production mapper can
// produce, so every one of them is a value an admitted direct-int schema would have to be
// TOTAL over. That is what makes this matrix a precondition for 7.2c-2 rather than a
// curiosity.
func pwBoundaryThresholds() []pwBoundaryThreshold {
	return []pwBoundaryThreshold{
		{ID: "zero", N: 0, Doc: "zero"},
		{ID: "pos_one", N: 1, Doc: "the smallest positive threshold"},
		{ID: "neg_one", N: -1, Doc: "the smallest negative threshold"},
		{ID: "exact_below", N: maxExactInt - 1, Doc: "2^53-1, the largest magnitude the native guard still admits"},
		{ID: "exact_at", N: maxExactInt, Doc: "2^53, where the native generic guard starts refusing — still EXACT in float64"},
		{ID: "exact_above", N: maxExactInt + 1, Doc: "2^53+1, the FIRST integer float64 cannot represent (it collapses onto 2^53)"},
		{ID: "neg_exact_below", N: -(maxExactInt - 1), Doc: "-(2^53-1)"},
		{ID: "neg_exact_at", N: -maxExactInt, Doc: "-2^53"},
		{ID: "neg_exact_above", N: -(maxExactInt + 1), Doc: "-(2^53+1)"},
		{ID: "i64max_below", N: math.MaxInt64 - 1, Doc: "math.MaxInt64-1"},
		{ID: "i64max", N: math.MaxInt64, Doc: "math.MaxInt64"},
		{ID: "i64min_above", N: math.MinInt64 + 1, Doc: "math.MinInt64+1"},
		{ID: "i64min", N: math.MinInt64, Doc: "math.MinInt64 — the one literal whose magnitude has no positive i64"},
	}
}

// pwBoundaryFn is the single function each boundary project carries.
const pwBoundaryFn = "Bound"

// pwBoundaryValues are the `this` values driven against a threshold: the threshold
// itself and its two neighbours, clamped so an i64 endpoint does not wrap.
//
// Three values is exactly what distinguishes all six operators at once — below, at and
// above — so one parse of a six-check function yields the full truth table for that
// literal. At an endpoint one neighbour does not exist and is dropped; the matrix test
// LOGS that so a shorter row cannot read as full coverage.
func pwBoundaryValues(n int64) []int64 {
	out := []int64{n}
	if n > math.MinInt64 {
		out = append([]int64{n - 1}, out...)
	}
	if n < math.MaxInt64 {
		out = append(out, n+1)
	}
	return out
}

// pwBoundaryProjects is one isolated project per threshold, each carrying ONE bare-int
// function with SIX checks — one per operator — so a single parse yields all six
// statuses for that literal.
//
// One project per threshold rather than one for all thirteen: `-9223372036854775808` is
// unary minus applied to a magnitude with no positive i64, and whether BAML's expression
// parser accepts it at all is one of the things being measured. A shared project would
// let that one answer erase the other twelve.
func pwBoundaryProjects() []pwProject {
	var out []pwProject
	for _, b := range pwBoundaryThresholds() {
		var attrs []string
		for _, o := range pwOperators() {
			attrs = append(attrs, fmt.Sprintf("@check(%s, {{ this %s %s }})", o.ID, o.Op, b.literal()))
		}
		out = append(out, pwProject{
			Key: b.projectKey(),
			Doc: "Slice 7.2c-1 DIRECT-i64 BOUNDARY: all six operators against " + b.literal() + " (" + b.Doc + ").\n" +
				"One function, six checks, so one parse yields the whole truth table for this literal.",
			Funcs: []pwFunc{{
				Name:   pwBoundaryFn,
				Doc:    "BOUNDARY: six direct comparisons against " + b.literal() + ".",
				Target: "int " + strings.Join(attrs, " "),
			}},
			WantCompile: true,
		})
	}
	return out
}

// ---------------------------------------------------------------------------
// (E) Residual characterization — captured, NEVER admitted.
// ---------------------------------------------------------------------------

// pwResidual is one deferred constraint FORM: a shape 7.2c explicitly does not admit,
// captured so its deferral rests on measurement instead of on an argument.
type pwResidual struct {
	// ID names the project (res_<ID>).
	ID string
	// Attrs is the attribute list on `confidence`.
	Attrs string
	// Class is the pinned class the form is declared on. A form carrying ANY @check
	// produces a Checked wrapper, so it must be declared on the CHECK family for the
	// generated Go shape to match; an assert-only form goes on the ASSERT family.
	Class string
	// Drives are the `confidence` values driven, in order.
	Drives []int64
	// Doc says what the form is and why it stays declined.
	Doc string
}

func (r pwResidual) projectKey() string { return "res_" + r.ID }

// fn is the function name inside this residual's project. It follows the CLASS, because
// the two families decode to different Go shapes and the name is what says which one a
// drive expects.
func (r pwResidual) fn() string {
	if r.Class == pwAssertClass {
		return pwFnAssert
	}
	return pwFnChecked
}

// pwResiduals are the deferred CONSTRAINT forms — the ones whose deferral turns on stock
// behaviour this package can measure.
//
// The deferred TYPE/SHAPE candidates (float, string, bool, enum, nullable, list, map,
// nested, multi-field, reordered/third field, target and list-element constraints) are
// NOT here, and that bound is deliberate and logged by
// [TestResidualLedgerCoversEveryDeferral]: each of them changes the GENERATED GO SHAPE of
// the pinned class, and the baml_go type map is process-global — one entry per class
// name across every runtime — so a shape variant cannot share the pinned name with the
// operator captures above. Renaming it to make it fit is the exact move the scope forbids.
// Their stock behaviour is already captured, under their own names, by the 49
// constraint-bearing rows of internal/debaml's serving oracle; residuals.md cites that
// authority per row rather than duplicating it here under a name that would confound
// "declined for its shape" with "declined for its name".
func pwResiduals() []pwResidual {
	return []pwResidual{{
		ID:    "two_checks",
		Class: pwCheckedClass,
		Attrs: "@check(alpha, {{ this > 0 }}) @check(beta, {{ this < 100 }})",
		Doc: "TWO UNIQUE @check attributes. The carrier can serialize N checks in declaration\n" +
			"order; stock's public map[string]Check cannot, and sonic.Marshal of a Go map has no\n" +
			"stable byte order. TestTwoCheckWireOrderIsUnstable records what stock actually does.",
		Drives: []int64{9},
	}, {
		ID:    "three_checks",
		Class: pwCheckedClass,
		Attrs: "@check(alpha, {{ this > 0 }}) @check(beta, {{ this < 100 }}) @check(gamma, {{ this != 7 }})",
		Doc: "THREE UNIQUE @check attributes — the same instability with more keys, so the\n" +
			"observation is not a two-element coincidence.",
		Drives: []int64{9},
	}, {
		ID:    "duplicate_labels",
		Class: pwCheckedClass,
		Attrs: "@check(dup, {{ this > 0 }}) @check(dup, {{ this > 1 }})",
		Doc: "DUPLICATE LABELS on the pinned family. The raw CFFI list keeps both; baml_go's map\n" +
			"fold keeps one. checkedwire pins the same fold on a bare target; this row pins it\n" +
			"inside the admitted family's own shape.",
		Drives: []int64{9},
	}, {
		ID:    "check_then_assert",
		Class: pwCheckedClass,
		Attrs: "@check(c, {{ this < 100 }}) @assert(a, {{ this > 0 }})",
		Doc: "MIXED, @check FIRST. Driven with the assert HOLDING (check also holds) and with the\n" +
			"assert FALSE while the check would otherwise have been emitted — the state the\n" +
			"one-constraint mapper and one-assert renderer have no model for.",
		Drives: []int64{9, -1},
	}, {
		ID:    "assert_then_check",
		Class: pwCheckedClass,
		Attrs: "@assert(a, {{ this > 0 }}) @check(c, {{ this < 100 }})",
		Doc: "MIXED, @assert FIRST — the SAME two constraints in the other declaration order, so\n" +
			"any ordering effect in stock's evaluation or error is attributable to the order.",
		Drives: []int64{9, -1},
	}, {
		ID:    "two_asserts",
		Class: pwAssertClass,
		Attrs: "@assert(first, {{ this > 100 }}) @assert(second, {{ this > 200 }})",
		Doc: "TWO @assert attributes, both FAILING, on the ASSERT family. checkedwire measured\n" +
			"stock's MAX_CAUSES=5 and cause ORDER on a bare target; this row asks the same of the\n" +
			"name-pinned family, whose renderer models exactly ONE failing assert on one required\n" +
			"field. The cause order recorded here is what a multi-assert admission would have to\n" +
			"reproduce.",
		Drives: []int64{9},
	}}
}

// pwResidualProjects is one isolated project per residual form.
func pwResidualProjects() []pwProject {
	var out []pwProject
	for _, r := range pwResiduals() {
		decl := pwCheckedClassDecl(r.Attrs)
		if r.Class == pwAssertClass {
			decl = pwAssertClassDecl(r.Attrs)
		}
		out = append(out, pwProject{
			Key:         r.projectKey(),
			Doc:         "Slice 7.2c-1 RESIDUAL (captured, NEVER admitted):\n" + r.Doc,
			Decls:       []string{decl},
			Funcs:       []pwFunc{{Name: r.fn(), Doc: "RESIDUAL: driven for characterization only.", Target: r.Class}},
			WantCompile: true,
		})
	}
	return out
}
