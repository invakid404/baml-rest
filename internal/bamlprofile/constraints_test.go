package bamlprofile

import (
	"errors"
	"strings"
	"testing"

	"github.com/invakid404/minijinja-go/v2/value"
)

// These are CFFI-free REGRESSION GUARDS for the constraint lowerer, not parity
// claims. The authority for what stock BAML v0.223 does is the live-CFFI
// constraint differential in ./profileoracle (build tag `integration`); these
// pin the mechanics that differential cannot see from the outside — the typed
// error stages, the projection's exact fork representation, input-order
// preservation, and the ownership of caller-supplied pointers.

func lbl(s string) *string { return &s }

// evalOK runs a batch that is expected to succeed.
func evalOK(t *testing.T, this value.Value, cs ...Constraint) ConstraintReport {
	t.Helper()
	rep, err := EvaluateConstraints(ConstraintRequest{Constraints: cs, This: this})
	if err != nil {
		t.Fatalf("EvaluateConstraints: unexpected error: %v", err)
	}
	return rep
}

// evalErr runs a batch that is expected to fail, and returns the typed error.
func evalErr(t *testing.T, this value.Value, cs ...Constraint) *ConstraintError {
	t.Helper()
	rep, err := EvaluateConstraints(ConstraintRequest{Constraints: cs, This: this})
	if err == nil {
		t.Fatalf("EvaluateConstraints: expected an error, got report %+v", rep)
	}
	// A failed batch must yield NO partial report — BAML's collect::<Result<Vec>>
	// discards everything when any predicate fails.
	if rep.Results != nil || rep.AssertFailed {
		t.Errorf("failed batch returned a partial report: %+v", rep)
	}
	var ce *ConstraintError
	if !errors.As(err, &ce) {
		t.Fatalf("error %v is not a *ConstraintError", err)
	}
	return ce
}

func assertConstraint(expr string) Constraint {
	return Constraint{Level: ConstraintAssert, Expression: expr}
}

func checkConstraint(label, expr string) Constraint {
	return Constraint{Level: ConstraintCheck, Expression: expr, Label: lbl(label)}
}

// --- the core predicate + rendered-text classifier -------------------------

// TestConstraintBareExpressionIsWrappedVerbatim pins the synthetic-template seam:
// the stored expression is BARE and the evaluator supplies `{{ ... }}`, exactly
// as render_expression's format!(r#"{{{{ {} }}}}"#, expression.0) does.
func TestConstraintBareExpressionIsWrappedVerbatim(t *testing.T) {
	rep := evalOK(t, value.FromInt(5), assertConstraint("this > 0"))
	if len(rep.Results) != 1 || !rep.Results[0].Passed {
		t.Fatalf("bare `this > 0` on 5 did not pass: %+v", rep)
	}
	// The wrapping is textual, so an expression carrying its own interior braces
	// (a map literal) still composes.
	rep = evalOK(t, value.FromString("k"), assertConstraint(`{"k": 1}[this] == 1`))
	if !rep.Results[0].Passed {
		t.Fatalf("map-literal predicate did not pass: %+v", rep)
	}
}

// TestConstraintExactBooleanText pins BAML's rendered-TEXT classifier: exactly
// "true" and exactly "false", and every other successful rendering is an
// evaluator error rather than a false predicate.
func TestConstraintExactBooleanText(t *testing.T) {
	passing := []string{
		"true",            // the literal
		"1 == 1",          // a comparison
		"this",            // a boolean `this` renders "true"
		"'tru' ~ 'e'",     // built by concatenation — text, not a downcast
		"[1]|length == 1", // through a filter
		"true if this else false",
	}
	for _, expr := range passing {
		t.Run("pass/"+expr, func(t *testing.T) {
			rep := evalOK(t, value.True(), assertConstraint(expr))
			if !rep.Results[0].Passed {
				t.Errorf("%q classified as false, want true", expr)
			}
		})
	}

	rep := evalOK(t, value.False(), assertConstraint("this"))
	if rep.Results[0].Passed {
		t.Error("`this` on the boolean false classified as true")
	}

	// Anything else is an ERROR. " true" is the load-bearing case: a trimming
	// classifier would accept it, and a boolean-downcast classifier would too.
	nonBoolean := []struct{ name, expr string }{
		{"leading_space", `" true"`},
		{"trailing_space", `"true "`},
		{"capitalized", `"True"`},
		{"one", `1`},
		{"empty", `""`},
		{"none", `none`},
		{"list_of_true", `[true]`},
		{"true_true", `"true" ~ "true"`},
	}
	for _, tc := range nonBoolean {
		t.Run("error/"+tc.name, func(t *testing.T) {
			ce := evalErr(t, value.True(), assertConstraint(tc.expr))
			if ce.Stage != ConstraintStageClassify {
				t.Errorf("stage = %q, want %q (err: %v)", ce.Stage, ConstraintStageClassify, ce.Err)
			}
		})
	}
}

// TestConstraintNonBooleanIsNeverFalse is the same rule stated as the property
// that actually matters: a predicate the evaluator cannot classify must not
// become a failed assert. Turning it into `false` would reject a value BAML
// would have errored on — a different, quieter wrong answer.
func TestConstraintNonBooleanIsNeverFalse(t *testing.T) {
	rep, err := EvaluateConstraints(ConstraintRequest{
		Constraints: []Constraint{assertConstraint(`"maybe"`)},
		This:        value.FromInt(1),
	})
	if err == nil {
		t.Fatalf("non-boolean predicate produced a report instead of an error: %+v", rep)
	}
	if rep.AssertFailed {
		t.Error("non-boolean predicate was reported as a failed assert")
	}
}

// --- error stages ----------------------------------------------------------

func TestConstraintErrorStages(t *testing.T) {
	cases := []struct {
		name  string
		this  value.Value
		c     Constraint
		stage ConstraintStage
	}{
		{"validate_unknown_level", value.FromInt(1),
			Constraint{Level: ConstraintLevel(0), Expression: "true"}, ConstraintStageValidate},
		{"validate_unknown_level_high", value.FromInt(1),
			Constraint{Level: ConstraintLevel(9), Expression: "true"}, ConstraintStageValidate},
		{"validate_check_without_label", value.FromInt(1),
			Constraint{Level: ConstraintCheck, Expression: "true"}, ConstraintStageValidate},
		{"validate_empty_label", value.FromInt(1),
			Constraint{Level: ConstraintAssert, Expression: "true", Label: lbl("")}, ConstraintStageValidate},
		{"validate_bracket_wrapped", value.FromInt(1),
			assertConstraint("{{ this > 0 }}"), ConstraintStageValidate},
		{"validate_bracket_wrapped_padded", value.FromInt(1),
			assertConstraint("  {{ this > 0 }}  "), ConstraintStageValidate},
		{"project_undefined", value.Undefined(),
			assertConstraint("true"), ConstraintStageProject},
		{"compile_syntax", value.FromInt(1),
			assertConstraint("this >"), ConstraintStageCompile},
		{"compile_unbalanced_brace", value.FromInt(1),
			assertConstraint("}} bad {{"), ConstraintStageCompile},
		{"render_unknown_filter", value.FromInt(1),
			assertConstraint("this|no_such_filter"), ConstraintStageRender},
		{"classify_non_boolean", value.FromInt(1),
			assertConstraint("this"), ConstraintStageClassify},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ce := evalErr(t, tc.this, tc.c)
			if ce.Stage != tc.stage {
				t.Errorf("stage = %q, want %q (err: %v)", ce.Stage, tc.stage, ce.Err)
			}
		})
	}
}

// TestConstraintErrorIndexIdentifiesTheFailingConstraint pins that the typed
// error names WHICH predicate failed, and that a whole-request failure (the one
// projection, shared by the batch) reports -1 rather than pointing at an
// arbitrary constraint.
func TestConstraintErrorIndexIdentifiesTheFailingConstraint(t *testing.T) {
	ce := evalErr(t, value.FromInt(1),
		assertConstraint("this > 0"),
		assertConstraint("this > 1"),
		assertConstraint("this|no_such_filter"),
	)
	if ce.Index != 2 {
		t.Errorf("Index = %d, want 2", ce.Index)
	}
	if ce.Constraint.Expression != "this|no_such_filter" {
		t.Errorf("Constraint.Expression = %q, want the failing one", ce.Constraint.Expression)
	}

	ce = evalErr(t, value.Undefined(), assertConstraint("true"))
	if ce.Index != -1 {
		t.Errorf("projection failure Index = %d, want -1", ce.Index)
	}
	if ce.Constraint != (Constraint{}) {
		t.Errorf("projection failure carries a constraint %+v, want the zero Constraint", ce.Constraint)
	}
}

// TestConstraintBatchAbortsWithoutPartialReport pins run_user_checks'
// collect::<Result<Vec<_>>>(): an error anywhere discards the results that
// already succeeded, rather than returning them alongside the failure.
func TestConstraintBatchAbortsWithoutPartialReport(t *testing.T) {
	rep, err := EvaluateConstraints(ConstraintRequest{
		Constraints: []Constraint{
			checkConstraint("ok", "this > 0"),       // would pass
			assertConstraint("this > 100"),          // would fail (a normal result)
			assertConstraint("this|no_such_filter"), // errors
			checkConstraint("never", "true"),        // never reached
		},
		This: value.FromInt(5),
	})
	if err == nil {
		t.Fatalf("expected an error, got %+v", rep)
	}
	if len(rep.Results) != 0 {
		t.Errorf("aborted batch returned %d results, want none: %+v", len(rep.Results), rep.Results)
	}
	if rep.AssertFailed {
		t.Error("aborted batch reported AssertFailed; a failed batch makes no assert claim at all")
	}
}

// TestConstraintPreflightRunsBeforeAnyEvaluation pins that a structurally
// impossible constraint LATER in the batch is caught before an earlier one is
// evaluated — so a malformed request can never half-run.
func TestConstraintPreflightRunsBeforeAnyEvaluation(t *testing.T) {
	ce := evalErr(t, value.FromInt(1),
		assertConstraint("this|no_such_filter"),                // would fail at render
		Constraint{Level: ConstraintCheck, Expression: "true"}, // unlabelled check
	)
	if ce.Stage != ConstraintStageValidate || ce.Index != 1 {
		t.Errorf("got stage %q index %d, want the validate failure at index 1", ce.Stage, ce.Index)
	}
}

// --- levels, labels, order, ownership --------------------------------------

// TestConstraintFalseAssertVersusFalseCheck is the assert/check split: a false
// assert sets AssertFailed (terminal at the serving layer), a false check is
// retained as an ordinary result and makes no rejection claim.
func TestConstraintFalseAssertVersusFalseCheck(t *testing.T) {
	rep := evalOK(t, value.FromInt(5), checkConstraint("big", "this > 100"))
	if rep.AssertFailed {
		t.Error("a false CHECK set AssertFailed")
	}
	if len(rep.Results) != 1 || rep.Results[0].Passed {
		t.Fatalf("false check not retained as a result: %+v", rep.Results)
	}

	rep = evalOK(t, value.FromInt(5), assertConstraint("this > 100"))
	if !rep.AssertFailed {
		t.Error("a false ASSERT did not set AssertFailed")
	}
	if len(rep.Results) != 1 || rep.Results[0].Passed {
		t.Fatalf("false assert not retained as a result: %+v", rep.Results)
	}

	// A PASSING assert alongside a failing check must not set AssertFailed.
	rep = evalOK(t, value.FromInt(5),
		assertConstraint("this > 0"),
		checkConstraint("big", "this > 100"),
	)
	if rep.AssertFailed {
		t.Error("AssertFailed set although every assert passed")
	}
}

// TestConstraintResultsAreInInputOrder pins the ordering contract PR-3 owns:
// results come back in declared input order, with each constraint's level, label
// and expression intact, so Slice 7.2 has a stable source order to build a
// Checked<T> from.
func TestConstraintResultsAreInInputOrder(t *testing.T) {
	cs := []Constraint{
		checkConstraint("c1", "this > 0"),
		assertConstraint("this > 1"),
		checkConstraint("c2", "this > 100"),
		{Level: ConstraintAssert, Expression: "this > 200", Label: lbl("a2")},
	}
	rep := evalOK(t, value.FromInt(5), cs...)
	if len(rep.Results) != len(cs) {
		t.Fatalf("got %d results, want %d", len(rep.Results), len(cs))
	}
	wantPassed := []bool{true, true, false, false}
	for i, r := range rep.Results {
		if r.Constraint.Expression != cs[i].Expression || r.Constraint.Level != cs[i].Level {
			t.Errorf("result %d = %+v, want the constraint at input index %d", i, r.Constraint, i)
		}
		if r.Passed != wantPassed[i] {
			t.Errorf("result %d Passed = %v, want %v", i, r.Passed, wantPassed[i])
		}
	}
	if !rep.AssertFailed {
		t.Error("AssertFailed not set although `this > 200` failed on 5")
	}

	// A caller filtering to checks sees label/expression/pass state intact.
	var checks []ConstraintResult
	for _, r := range rep.Results {
		if r.Constraint.Level == ConstraintCheck {
			checks = append(checks, r)
		}
	}
	if len(checks) != 2 || *checks[0].Constraint.Label != "c1" || *checks[1].Constraint.Label != "c2" {
		t.Errorf("check filter = %+v, want c1 then c2", checks)
	}
}

// TestConstraintReportOwnsItsLabels pins that the report does not alias the
// caller's *string labels. Without the copy, writing through a label pointer
// after the call would retroactively rename a check in a report already handed
// out — the same ownership rule enum members apply to a resolved alias.
func TestConstraintReportOwnsItsLabels(t *testing.T) {
	label := "original"
	cs := []Constraint{{Level: ConstraintCheck, Expression: "true", Label: &label}}
	rep := evalOK(t, value.FromInt(1), cs...)

	label = "mutated"
	cs[0].Expression = "false"

	if got := *rep.Results[0].Constraint.Label; got != "original" {
		t.Errorf("label = %q after the caller mutated its pointee, want %q", got, "original")
	}
	if got := rep.Results[0].Constraint.Expression; got != "true" {
		t.Errorf("expression = %q after the caller mutated its slice, want %q", got, "true")
	}
}

// TestConstraintEmptyBatch pins that no constraints is a successful, empty
// report — matching run_user_checks over an empty constraint list — while the
// projection preflight still runs (an unsupported `this` is refused rather than
// silently accepted because nothing would have looked at it).
func TestConstraintEmptyBatch(t *testing.T) {
	rep := evalOK(t, value.FromInt(1))
	if len(rep.Results) != 0 || rep.AssertFailed {
		t.Errorf("empty batch = %+v, want an empty report", rep)
	}
	if ce := evalErr(t, value.Undefined()); ce.Stage != ConstraintStageProject {
		t.Errorf("empty batch with an unsupported This: stage %q, want %q", ce.Stage, ConstraintStageProject)
	}
}

// TestConstraintBatchIsIndependentOfBatching guards the one optimization
// EvaluateConstraints takes over the reference: BAML calls get_env() per
// predicate, while this builds one environment per BATCH.
//
// The claim is that the environment carries no per-render state, so batching
// cannot be observable. That is asserted directly — every constraint must give
// the same answer alone as it does alongside the others, in either order, and a
// repeated batch must be identical — rather than argued from the fork's
// internals, which could change under us.
func TestConstraintBatchIsIndependentOfBatching(t *testing.T) {
	cs := []Constraint{
		checkConstraint("ns", "namespace(x=1).x == 1"),
		checkConstraint("leak", "x is undefined"),
		checkConstraint("macro_free", "[1, 2]|map('string')|join(',') == '1,2'"),
		checkConstraint("plain", "this > 0"),
	}
	this := value.FromInt(5)

	batched := evalOK(t, this, cs...)
	for i, c := range cs {
		single := evalOK(t, this, c)
		if single.Results[0].Passed != batched.Results[i].Passed {
			t.Errorf("constraint %d (%s) = %v alone but %v in a batch; the shared environment is leaking state",
				i, c.Expression, single.Results[0].Passed, batched.Results[i].Passed)
		}
	}

	// Reversing the batch must not change any individual verdict either.
	reversed := make([]Constraint, len(cs))
	for i, c := range cs {
		reversed[len(cs)-1-i] = c
	}
	revRep := evalOK(t, this, reversed...)
	for i := range cs {
		if revRep.Results[len(cs)-1-i].Passed != batched.Results[i].Passed {
			t.Errorf("constraint %d (%s) changed verdict when the batch was reversed", i, cs[i].Expression)
		}
	}

	// And a repeat of the same batch is identical.
	again := evalOK(t, this, cs...)
	if len(again.Results) != len(batched.Results) || again.AssertFailed != batched.AssertFailed {
		t.Fatalf("repeated batch differs: %+v vs %+v", again, batched)
	}
	for i := range again.Results {
		if again.Results[i].Passed != batched.Results[i].Passed {
			t.Errorf("constraint %d changed verdict on a repeated batch", i)
		}
	}
}

// --- the constraint environment --------------------------------------------

// TestConstraintEnvironmentHasNoPromptGlobals is the context-isolation guard:
// BAML's evaluator binds `this` and nothing else, so the three globals New adds
// for a PROMPT must be undefined in a predicate. The same names are asserted
// present through New, so the test fails if the refactor ever drains the prompt
// factory instead of the constraint one.
func TestConstraintEnvironmentHasNoPromptGlobals(t *testing.T) {
	names := []string{"_", "ctx", "Color"}
	cfg := Config{Enums: []EnumDef{{Name: "Color", Values: []EnumValue{{Canonical: "RED"}}}}}

	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			rep := evalOK(t, value.FromInt(1), assertConstraint(name+" is undefined"))
			if !rep.Results[0].Passed {
				t.Errorf("%s is DEFINED inside a constraint; the constraint environment must add no prompt globals", name)
			}
			if got := render(t, cfg, "{{ "+name+" is undefined }}"); got != "false" {
				t.Errorf("%s is undefined in a PROMPT render (%q); the refactor removed a prompt global", name, got)
			}
		})
	}

	// The enum NAMESPACE being undefined also makes member access an error rather
	// than a value: `Color.RED` cannot quietly become something in a predicate.
	if ce := evalErr(t, value.FromInt(1), assertConstraint("Color.RED == 'RED'")); ce.Stage != ConstraintStageRender {
		t.Errorf("Color.RED in a constraint: stage %q, want %q", ce.Stage, ConstraintStageRender)
	}
}

// TestConstraintEnvironmentKeepsGetEnvConfiguration pins the other half of the
// split: dropping the globals must not drop get_env's ENGINE configuration.
// Each row is a get_env delta or a registry entry a predicate can reach.
func TestConstraintEnvironmentKeepsGetEnvConfiguration(t *testing.T) {
	cases := []struct{ name, expr string }{
		{"builtin_length", "[1, 2, 3]|length == 3"},
		{"baml_sum_override", "[1, 2.5]|sum == 3.5"},
		{"baml_sum_empty", "[]|sum == 0"},
		{"regex_match", `"abc123"|regex_match("[0-9]+")`},
		{"regex_match_bad_pattern_is_false", `"x"|regex_match("(") == false`},
		{"pycompat_unknown_method", `"abc".upper() == "ABC"`},
		{"builtin_function_range", "range(3)|list|length == 3"},
		{"whitespace_flags_are_inert_here", "true"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rep := evalOK(t, value.None(), assertConstraint(tc.expr))
			if !rep.Results[0].Passed {
				t.Errorf("%q did not hold in a constraint environment", tc.expr)
			}
		})
	}

	// get_env's none -> "null" FORMATTER is installed here too, even though a
	// constraint can never end up depending on it: the formatter runs on the final
	// output, and the classifier only accepts "true"/"false", so a predicate that
	// renders a none is an error either way. What the formatter changes is WHICH
	// text the error reports — "null" rather than the engine default "". Pinning
	// that keeps the constraint environment on the same get_env base as the prompt
	// one instead of quietly drifting to a bare NewEnvironment.
	ce := evalErr(t, value.None(), assertConstraint("this"))
	if ce.Stage != ConstraintStageClassify || !strings.Contains(ce.Err.Error(), `"null"`) {
		t.Errorf("a none-rendering predicate reported %v; want a classify error over the get_env %q text", ce.Err, "null")
	}
}

// --- the serde projection ---------------------------------------------------

func mustEnum(t *testing.T, enumName, canonical string, alias *string) value.Value {
	t.Helper()
	v, err := EnumMember(enumName, canonical, alias)
	if err != nil {
		t.Fatalf("EnumMember: %v", err)
	}
	return v
}

func mustClass(t *testing.T, fields ...ClassField) value.Value {
	t.Helper()
	v, err := ClassValue(fields)
	if err != nil {
		t.Fatalf("ClassValue: %v", err)
	}
	return v
}

func mustList(t *testing.T, items ...value.Value) value.Value {
	t.Helper()
	v, err := ListValue(items)
	if err != nil {
		t.Fatalf("ListValue: %v", err)
	}
	return v
}

// TestProjectionEnumIsCanonicalString pins BamlValue::Enum's serde arm: the
// canonical variant, never the prompt display alias. This is THE divergence
// between the two host lowerings, so both directions are asserted.
func TestProjectionEnumIsCanonicalString(t *testing.T) {
	red := mustEnum(t, "Color", "RED", lbl("rouge"))
	cases := []struct {
		expr string
		want bool
	}{
		{`this == "RED"`, true},
		{`this == "rouge"`, false},
		{`this is string`, true},
		{`this|upper == "RED"`, true},
		{`this|string == "RED"`, true},
		// `.value` is the PROMPT object's only attribute; the projected string has
		// no attributes at all.
		{`this.value is undefined`, true},
	}
	for _, tc := range cases {
		t.Run(tc.expr, func(t *testing.T) {
			rep := evalOK(t, red, assertConstraint(tc.expr))
			if rep.Results[0].Passed != tc.want {
				t.Errorf("%q = %v, want %v", tc.expr, rep.Results[0].Passed, tc.want)
			}
		})
	}
}

// TestProjectionClassIsCanonicalKeyOrderedMap pins BamlValue::Class's serde arm:
// a plain mapping keyed by CANONICAL names, in declared order, with no alias key
// and no host debug rendering.
func TestProjectionClassIsCanonicalKeyOrderedMap(t *testing.T) {
	c := mustClass(t,
		ClassField{Canonical: "zeta", Alias: lbl("zwire"), Value: value.FromInt(1)},
		ClassField{Canonical: "alpha", Alias: lbl("awire"), Value: value.FromString("v")},
	)
	cases := []struct {
		expr string
		want bool
	}{
		{`this.zeta == 1`, true},
		{`this.alpha == "v"`, true},
		{`this.zwire is undefined`, true},
		{`this.awire is undefined`, true},
		{`this is mapping`, true},
		{`this|length == 2`, true},
		// DECLARED order, not sorted: a Go map would have yielded alpha first.
		{`this|list|join(",") == "zeta,alpha"`, true},
		{`this|list|join(",") == "alpha,zeta"`, false},
		// The prompt class's pretty `{map:#?}` render must not leak in.
		{`this|string == "{\"zeta\": 1, \"alpha\": \"v\"}"`, true},
	}
	for _, tc := range cases {
		t.Run(tc.expr, func(t *testing.T) {
			rep := evalOK(t, c, assertConstraint(tc.expr))
			if rep.Results[0].Passed != tc.want {
				t.Errorf("%q = %v, want %v", tc.expr, rep.Results[0].Passed, tc.want)
			}
		})
	}
}

// TestProjectionListIsPlainSequence pins BamlValue::List's serde arm.
func TestProjectionListIsPlainSequence(t *testing.T) {
	l := mustList(t, value.FromString("a"), value.FromString("b"))
	cases := []struct {
		expr string
		want bool
	}{
		{`this|length == 2`, true},
		{`this[0] == "a"`, true},
		{`"b" in this`, true},
		{`this is sequence`, true},
		{`this|join(",") == "a,b"`, true},
	}
	for _, tc := range cases {
		t.Run(tc.expr, func(t *testing.T) {
			rep := evalOK(t, l, assertConstraint(tc.expr))
			if rep.Results[0].Passed != tc.want {
				t.Errorf("%q = %v, want %v", tc.expr, rep.Results[0].Passed, tc.want)
			}
		})
	}
}

// TestProjectionRecurses pins that the projection walks nested host values —
// an aliased enum buried in a list inside a class must still be its canonical
// string, and a nested none must still be none.
func TestProjectionRecurses(t *testing.T) {
	nested := mustClass(t,
		ClassField{Canonical: "inner", Alias: lbl("nest"), Value: mustList(t,
			mustEnum(t, "Color", "RED", lbl("rouge")),
			mustClass(t, ClassField{Canonical: "deep", Value: value.None()}),
		)},
	)
	cases := []struct {
		expr string
		want bool
	}{
		{`this.inner[0] == "RED"`, true},
		{`this.inner[0] == "rouge"`, false},
		{`this.inner[1].deep is none`, true},
		{`this.inner[1].deep is undefined`, false},
		{`this.nest is undefined`, true},
		{`this.inner|length == 2`, true},
	}
	for _, tc := range cases {
		t.Run(tc.expr, func(t *testing.T) {
			rep := evalOK(t, nested, assertConstraint(tc.expr))
			if rep.Results[0].Passed != tc.want {
				t.Errorf("%q = %v, want %v", tc.expr, rep.Results[0].Passed, tc.want)
			}
		})
	}
}

// TestProjectionScalarsAndNone pins the scalar/none arms are passed through
// unchanged.
func TestProjectionScalarsAndNone(t *testing.T) {
	cases := []struct {
		name string
		this value.Value
		expr string
	}{
		{"string", value.FromString("hi"), `this == "hi"`},
		{"int", value.FromInt(7), `this == 7`},
		{"float", value.FromFloat(1.5), `this == 1.5`},
		{"bool", value.True(), `this == true`},
		{"none", value.None(), `this is none`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rep := evalOK(t, tc.this, assertConstraint(tc.expr))
			if !rep.Results[0].Passed {
				t.Errorf("%s: %q did not hold", tc.name, tc.expr)
			}
		})
	}
}

// TestProjectionAlternateDebugIsNotThePromptRenderer pins the |pprint / debug()
// path, which is a DIFFERENT fork code path from |string (v2.16.0-baml.6 PATCHES
// #106-#108 scope the object-render dispatch to the map arm, and the list arm
// respects the ambient alternate flag). A green |string assertion therefore does
// not cover it.
//
// It is the path where PR-2's hand-written Rust-debug renderer would leak into a
// predicate if the prompt host object were ever bound to `this` — alias keys for
// a class, bare enum aliases for a list. Both the projected bytes and the prompt
// bytes are spelled out, so the test records exactly what the wrong answer looks
// like rather than only asserting the right one.
//
// The live-CFFI half is class_pprint_is_serde_map / list_pprint_is_serde_seq in
// ./profileoracle; this is the CGO-free regression guard for the same bytes.
func TestProjectionAlternateDebugIsNotThePromptRenderer(t *testing.T) {
	promptClass := mustClass(t,
		ClassField{Canonical: "zeta", Alias: lbl("z"), Value: value.FromInt(1)},
		ClassField{Canonical: "alpha", Alias: lbl("a"), Value: value.FromString("v")},
	)
	promptList := mustList(t,
		mustEnum(t, "Color", "RED", lbl("rouge")),
		mustEnum(t, "Color", "GREEN", nil),
	)

	cases := []struct {
		name string
		this value.Value
		// wantProjected is what a predicate must see: the serde projection's
		// alternate-debug bytes (canonical keys, quoted canonical enum strings).
		wantProjected string
		// wantPrompt is PR-2's prompt-lowering rendering of the SAME value — the
		// bytes that must never reach a predicate.
		wantPrompt string
	}{
		{
			name:          "class",
			this:          promptClass,
			wantProjected: "{\n    \"zeta\": 1,\n    \"alpha\": \"v\",\n}",
			wantPrompt:    "{\n    \"z\": 1,\n    \"a\": \"v\",\n}",
		},
		{
			name:          "list",
			this:          promptList,
			wantProjected: "[\n    \"RED\",\n    \"GREEN\",\n]",
			wantPrompt:    "[\n    rouge,\n    GREEN,\n]",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.wantProjected == tc.wantPrompt {
				t.Fatal("the two spellings are identical, so this row cannot discriminate")
			}
			// The prompt object really does render the leaking spelling — without
			// this the test could pass because the renderer changed rather than
			// because the projection is right.
			if got := renderPromptPprint(t, tc.this); got != tc.wantPrompt {
				t.Fatalf("PR-2 prompt |pprint = %q, want %q; the counter-value this row records is stale", got, tc.wantPrompt)
			}
			// Both alternate-debug entry points on the PROJECTED value.
			for _, expr := range []string{"this|pprint", "debug(this)"} {
				got := renderConstraintText(t, tc.this, expr)
				if got != tc.wantProjected {
					t.Errorf("%s = %q, want %q", expr, got, tc.wantProjected)
				}
				if got == tc.wantPrompt {
					t.Errorf("%s leaked the PR-2 prompt rendering into a predicate", expr)
				}
			}
		})
	}
}

// renderConstraintText renders a bare expression through the constraint seam —
// the projection, the constraint environment and the `{{ ... }}` wrapping — and
// returns the raw text, bypassing the "true"/"false" classifier so a rendering
// can be inspected directly.
func renderConstraintText(t *testing.T, this value.Value, expr string) string {
	t.Helper()
	projected, err := projectConstraintThis(this)
	if err != nil {
		t.Fatalf("projectConstraintThis: %v", err)
	}
	tmpl, err := newConstraintEnvironment().TemplateFromNamedString(constraintTemplateName, "{{ "+expr+" }}")
	if err != nil {
		t.Fatalf("compile %q: %v", expr, err)
	}
	out, err := tmpl.Render(value.FromMap(map[string]value.Value{"this": projected}))
	if err != nil {
		t.Fatalf("render %q: %v", expr, err)
	}
	return out
}

// renderPromptPprint renders |pprint of a value bound WITHOUT the projection, as
// the prompt factory would — the leak this package must prevent.
func renderPromptPprint(t *testing.T, this value.Value) string {
	t.Helper()
	env, err := New(Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	tmpl, err := env.TemplateFromNamedString("prompt", "{{ this|pprint }}")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	out, err := tmpl.Render(map[string]any{"this": this})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	return out
}

// TestProjectionFailsClosed pins the decline-by-default rule. Every one of these
// is a value BAML's `this` can never be, and each must be REFUSED rather than
// bound — binding a prompt host object, or a native container the caller built
// itself, is exactly the out-do the parity-decline rule forbids.
func TestProjectionFailsClosed(t *testing.T) {
	cases := []struct {
		name string
		this value.Value
	}{
		{"undefined", value.Undefined()},
		{"native_slice", value.FromSlice([]value.Value{value.FromInt(1)})},
		{"native_map", value.FromMap(map[string]value.Value{"a": value.FromInt(1)})},
		{"ordered_map", value.FromOrderedMap(value.NewOrderedMap(0))},
		{"bytes", value.FromBytes([]byte("x"))},
		{"foreign_object", value.FromObject(foreignHostObject{})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ce := evalErr(t, tc.this, assertConstraint("true"))
			if ce.Stage != ConstraintStageProject {
				t.Errorf("stage = %q, want %q (err: %v)", ce.Stage, ConstraintStageProject, ce.Err)
			}
		})
	}
}

// TestProjectionFailsClosedInsideAContainer pins that the decline survives
// nesting: an unsupported value buried in a host class or list is refused too,
// naming the path so a caller can find it.
func TestProjectionFailsClosedInsideAContainer(t *testing.T) {
	// ClassValue/ListValue reject a foreign object at construction, so the nested
	// case is reached through the projector directly — which is exactly the guard
	// that keeps a future host type from silently flowing through.
	bad := &classObject{
		fields: []classField{{canonical: "f", aliasKey: "f", value: value.FromObject(foreignHostObject{})}},
		byName: map[string]value.Value{"f": value.FromObject(foreignHostObject{})},
	}
	_, err := projectConstraintThis(value.FromObject(bad))
	if err == nil {
		t.Fatal("a foreign object nested in a class projected successfully")
	}
	if !strings.Contains(err.Error(), `field "f"`) {
		t.Errorf("error %q does not name the offending field", err)
	}

	badList := &listObject{items: []value.Value{value.FromObject(foreignHostObject{})}}
	_, err = projectConstraintThis(value.FromObject(badList))
	if err == nil {
		t.Fatal("a foreign object nested in a list projected successfully")
	}
	if !strings.Contains(err.Error(), "item 0") {
		t.Errorf("error %q does not name the offending item", err)
	}
}

// foreignHostObject stands in for a fork object this package did not build — a
// media value, or any host type whose constraint ingress is not yet proven.
type foreignHostObject struct{}

func (foreignHostObject) GetAttr(string) value.Value { return value.Undefined() }
