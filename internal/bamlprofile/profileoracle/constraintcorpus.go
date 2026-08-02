package profileoracle

// The CONSTRAINT differential corpus and its profile leg.
//
// The prompt corpus (rows.go) proves the profile RENDERS like stock BAML. This
// one proves the profile EVALUATES CONSTRAINTS like stock BAML, which is a
// different code path on both legs:
//
//   - stock leg: BamlRuntime.CallFunctionParse — the CFFI response-parse entry
//     point (language_client_go/pkg/runtime.go:178-213). Given an unambiguous raw
//     response text and a function whose RETURN TYPE carries @check/@assert
//     attributes, it runs BAML's real coercer, which calls run_user_checks ->
//     evaluate_predicate and then validate_asserts
//     (jsonish/src/deserializer/coercer/{mod.rs:322-338,field_type.rs:180-294}).
//     Merely rendering a prompt would exercise none of that.
//   - profile leg: bamlprofile.EvaluateConstraints over the SAME resolved
//     constraints and the same resolved host value, built by types.go's hostValue
//     from the row's plain-Go This exactly as the prompt corpus builds a render
//     argument.
//
// The two legs are compared by a normalized OUTCOME (parsed / assert-failed /
// evaluator error) plus, for a parsed row, the evaluated CHECK set. Stock's check
// representation is a map keyed by label, so the comparison normalizes both sides
// into the same label-keyed form rather than pretending stock exposes an order —
// PR-3 owns the ordered results, Slice 7.2 owns the serialized order.
//
// This file is CFFI-free and carries no build tag, so `go build ./...` and the
// default `go test` stay CGO-free; the stock leg lives in
// constraint_integration_test.go behind `//go:build integration`.

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/invakid404/baml-rest/internal/bamlprofile"
	"github.com/invakid404/minijinja-go/v2/value"
)

// constraintPromptBody is the inert prompt every constraint function carries.
// CallFunctionParse never renders it — it parses a supplied response text — but a
// BAML function must declare one. It is deliberately literal (no interpolation)
// so it cannot fail for a reason unrelated to constraints.
const constraintPromptBody = "constraint oracle"

// ConstraintDecl is one resolved constraint as it is BOTH declared in the
// generated .baml source and handed to the profile.
//
// Expression is the BARE expression, exactly as baml_types::Constraint stores it;
// the generator wraps it in `{{ ... }}` for the SOURCE spelling and the profile
// evaluator wraps it for the RUNTIME spelling. Keeping one bare string for both
// is what makes the differential able to compare stock's ResponseCheck.expression
// (which is the bare `expression.0`) against the profile's Constraint.Expression.
//
// Label is a *string because BAML distinguishes an unlabelled assert (None) from
// a labelled one, and requires a label on every check.
type ConstraintDecl struct {
	Level      bamlprofile.ConstraintLevel
	Label      *string
	Expression string
}

// IsCheck reports whether this declaration is a CHECK. It is the one predicate
// both the CFFI-free tests and the integration leg need — a row with any check is
// a row whose stock result arrives wrapped in a Checked<T>.
func (c ConstraintDecl) IsCheck() bool { return c.Level == bamlprofile.ConstraintCheck }

// ConstraintRow is one constraint differential case.
type ConstraintRow struct {
	// ID is the stable identifier (also the basis of the BAML function name).
	ID string
	// Surface groups rows for readability (core, levels, get_env, enum, ...).
	Surface string
	// ReturnType is the BAML return type the constraints hang off, e.g. "string",
	// "int", "Color", "C", "string[]". It must be a type types.go's hostValue can
	// lower, because the profile leg builds `this` from it.
	ReturnType string
	// Constraints are the resolved constraints in DECLARED order. The generator
	// emits them in this order and the profile evaluates them in this order.
	Constraints []ConstraintDecl
	// Raw is the raw "LLM response" text handed to CallFunctionParse. It must
	// coerce to ReturnType UNAMBIGUOUSLY: the differential is about constraints,
	// so a row whose value is in question proves nothing.
	Raw string
	// This is the plain-Go value the profile leg lowers (via hostValue) into the
	// same resolved host value BAML's coercer produces from Raw. Use
	// int64/float64/string/bool/nil and []any/map[string]any, as in Row.Args.
	This any
	// Expect, when non-empty, declares that stock BAML does NOT parse this row
	// successfully, and WHICH class it produces. Like Row.Fault it is asserted
	// against the live stock leg so it cannot rot into a fiction, and it is what
	// keeps a profile that quietly succeeded where stock failed from being green.
	// Empty means both legs must parse successfully.
	Expect ConstraintOutcomeKind
}

// ConstraintOutcomeKind classifies how a constrained parse ended. It is the
// comparison unit for every constraint row; a parsed row additionally compares
// its evaluated checks.
type ConstraintOutcomeKind string

const (
	// ConstraintParsed: every predicate evaluated and no assert failed. Stock
	// returned a value (a Checked<T> when the type declares checks); the profile
	// returned a report with AssertFailed false.
	ConstraintParsed ConstraintOutcomeKind = "parsed"
	// ConstraintAssertFailed: every predicate evaluated, and at least one ASSERT
	// was false. Stock rejects the parse (validate_asserts); the profile sets
	// ConstraintReport.AssertFailed. This is NOT an evaluator error on either leg.
	ConstraintAssertFailed ConstraintOutcomeKind = "assert_failed"
	// ConstraintEvalError: a predicate could not be evaluated — it failed to
	// compile, raised while rendering, or rendered text that is neither "true" nor
	// "false". Stock reports it as a constraint-evaluation failure; the profile
	// returns a *bamlprofile.ConstraintError at the compile/render/classify stage.
	ConstraintEvalError ConstraintOutcomeKind = "eval_error"
	// ConstraintUnsupported is PROFILE-ONLY: the leaf declined the request before
	// evaluating anything (a malformed constraint, or a `this` shape with no
	// proven serde projection). Stock BAML has no counterpart, so this kind can
	// never match a stock outcome — which is the point. A corpus row that produces
	// it is a real gap, reported with its detail rather than absorbed into
	// ConstraintEvalError where it would look like ordinary parity.
	ConstraintUnsupported ConstraintOutcomeKind = "unsupported"
	// ConstraintPanic: an INTERNAL invariant failure rather than a recoverable
	// error — stock Rust's `unreachable!()` or the fork's value.UnorderableMaps.
	// Handled exactly as the prompt corpus handles OutcomePanic (see types.go),
	// including the subprocess stock leg, so a hang can never be reported as a
	// value.
	ConstraintPanic ConstraintOutcomeKind = "panic"
)

// CheckOutcome is one evaluated CHECK, normalized to the shape both legs can
// produce: stock's ResponseCheck {name, expression, status} and the profile's
// ConstraintResult over a ConstraintCheck constraint.
type CheckOutcome struct {
	Label      string
	Expression string
	Passed     bool
}

// ConstraintOutcome is a classified constrained-parse result from one leg.
//
// Kind is always compared. Checks is compared only for ConstraintParsed, and is
// sorted by Label so the two legs' different natural orders (stock's map, the
// profile's declared order) do not make an equal result look unequal. Detail is
// DIAGNOSTIC ONLY: stock's Rust error text and the profile's Go error text
// describe the same failure in different words, and comparing them would pin a
// message rather than a behavior.
type ConstraintOutcome struct {
	Kind   ConstraintOutcomeKind
	Checks []CheckOutcome
	Detail string
}

// String renders a ConstraintOutcome for a failure message.
func (o ConstraintOutcome) String() string {
	if o.Kind == ConstraintParsed {
		return fmt.Sprintf("%s(checks=%v)", o.Kind, o.Checks)
	}
	return fmt.Sprintf("%s(%s)", o.Kind, o.Detail)
}

// FuncName is the BAML function name generated for a constraint row. The CRow_
// prefix keeps it distinct from the prompt corpus's Row_ names, so the two
// generated projects can never collide if they are ever merged.
func (r ConstraintRow) FuncName() string {
	var b strings.Builder
	b.WriteString("CRow_")
	for _, c := range r.ID {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
			b.WriteRune(c)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// DeclaresCheck reports whether a row's constraints include a CHECK.
//
// It is the row-level predicate the stock leg turns on: the CFFI wraps a parsed
// value in a CffiValueChecked only when the return type carries at least one
// check (baml_value_with_meta_encode.rs:69-101), so this decides both which
// checked-type binding the row needs and which result SHAPE the stock reader must
// insist on (see stockChecksOf).
func (r ConstraintRow) DeclaresCheck() bool {
	for _, c := range r.Constraints {
		if c.IsCheck() {
			return true
		}
	}
	return false
}

// profileConstraints converts a row's declarations into the leaf's resolved
// contract, preserving order and the presence/absence of each label.
func (r ConstraintRow) profileConstraints() []bamlprofile.Constraint {
	out := make([]bamlprofile.Constraint, len(r.Constraints))
	for i, c := range r.Constraints {
		out[i] = bamlprofile.Constraint{Level: c.Level, Expression: c.Expression, Label: c.Label}
	}
	return out
}

// EvaluateConstraintRow runs a row through the PROFILE leg and returns its
// classified outcome.
//
// Lowering `this` is the HARNESS's job and a failure there is wrapped as
// *harnessError, exactly as RenderProfile does: only bamlprofile's own verdict is
// an engine outcome. [ConstraintOutcomeProfile] re-raises a harness failure
// loudly rather than classifying it, so a row declaring a failure class cannot
// pass because the harness broke.
func EvaluateConstraintRow(r ConstraintRow) (ConstraintOutcome, error) {
	this, err := hostValue(r.ReturnType, r.This)
	if err != nil {
		return ConstraintOutcome{}, &harnessError{
			fmt.Errorf("profileoracle: row %q This (%s): %w", r.ID, r.ReturnType, err)}
	}

	report, err := bamlprofile.EvaluateConstraints(bamlprofile.ConstraintRequest{
		Constraints: r.profileConstraints(),
		This:        this,
	})
	if err != nil {
		var ce *bamlprofile.ConstraintError
		if !errors.As(err, &ce) {
			// EvaluateConstraints documents *ConstraintError as its only error type.
			// An untyped error means the leaf's contract changed under the oracle;
			// that is a harness-visible defect, not a classifiable engine outcome.
			return ConstraintOutcome{}, &harnessError{
				fmt.Errorf("profileoracle: row %q: EvaluateConstraints returned a non-*ConstraintError: %w", r.ID, err)}
		}
		switch ce.Stage {
		case bamlprofile.ConstraintStageValidate, bamlprofile.ConstraintStageProject:
			// A leaf-side DECLINE, not an evaluation. Kept distinct so it can never
			// be mistaken for stock's evaluator error.
			return ConstraintOutcome{Kind: ConstraintUnsupported, Detail: ce.Error()}, nil
		default:
			return ConstraintOutcome{Kind: ConstraintEvalError, Detail: ce.Error()}, nil
		}
	}
	if report.AssertFailed {
		return ConstraintOutcome{Kind: ConstraintAssertFailed, Detail: assertFailureDetail(report)}, nil
	}
	return ConstraintOutcome{Kind: ConstraintParsed, Checks: checksOfReport(report)}, nil
}

// assertFailureDetail summarizes which asserts failed. Diagnostic only — the
// differential compares the CLASS, never this text, because stock's wording
// ("Assertions failed." plus up to five causes) is the serving layer's contract,
// not PR-3's.
func assertFailureDetail(report bamlprofile.ConstraintReport) string {
	var failed []string
	for _, res := range report.Results {
		if res.Constraint.Level == bamlprofile.ConstraintAssert && !res.Passed {
			label := "<unlabelled>"
			if res.Constraint.Label != nil {
				label = *res.Constraint.Label
			}
			failed = append(failed, fmt.Sprintf("%s: %s", label, res.Constraint.Expression))
		}
	}
	return "failed asserts: " + strings.Join(failed, "; ")
}

// checksOfReport projects the profile's ordered results into the normalized,
// label-keyed check set stock can also produce.
//
// The projection is deliberately the SAME collapse stock's Go client applies when
// it builds `map[string]Check` from the ordered CFFI check list: a repeated label
// keeps the LAST occurrence. Doing it identically on both legs is what makes the
// duplicate-label probe row a measurement rather than an accident — the row
// asserts the collapse explicitly, and Slice 7.2 owns the policy.
func checksOfReport(report bamlprofile.ConstraintReport) []CheckOutcome {
	byLabel := map[string]CheckOutcome{}
	for _, res := range report.Results {
		if res.Constraint.Level != bamlprofile.ConstraintCheck {
			continue
		}
		label := ""
		if res.Constraint.Label != nil {
			label = *res.Constraint.Label
		}
		byLabel[label] = CheckOutcome{Label: label, Expression: res.Constraint.Expression, Passed: res.Passed}
	}
	return sortedChecks(byLabel)
}

// sortedChecks flattens a label-keyed check set into the canonical comparison
// order (by label), which both legs produce independently.
func sortedChecks(byLabel map[string]CheckOutcome) []CheckOutcome {
	out := make([]CheckOutcome, 0, len(byLabel))
	for _, c := range byLabel {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Label < out[j].Label })
	return out
}

// stockChecksOf reads the evaluated checks off a STOCK parse result and
// normalizes them into the same label-keyed set checksOfReport produces for the
// profile leg.
//
// declaresCheck is the row's [ConstraintRow.DeclaresCheck], and it selects which
// result SHAPE is legal — the reader is fail-loud in BOTH directions rather than
// tolerant in one:
//
//   - declaresCheck: the CFFI must have wrapped the value, so the result MUST be
//     a Checked[T] — a non-nil struct with a map-kind `Checks` field
//     (baml_go/shared/checked.go:12-15). nil, a plain decoded value, or a struct
//     without `Checks` is an ERROR naming the undecodable result. An EMPTY map is
//     fine and is not an error: stock legitimately reports zero checks for a bare
//     `string` return type, whose constraints it never evaluates (see
//     TestStockSkipsConstraintsOnBareStringReturn).
//   - !declaresCheck: nothing was wrapped, so there is nothing to read and a
//     plain value/nil/struct-without-`Checks` yields no checks. A result that
//     DOES carry `Checks` is an ERROR: the CFFI only wraps a checked type, so it
//     contradicts the row's own declaration.
//
// Returning an empty set for a shape the harness failed to understand would make
// the check comparison vacuously green for that row, or — once the profile leg
// disagrees — report a generic check mismatch that hides an undecodable stock
// result behind what looks like a parity failure.
//
// The read is reflective rather than a type switch so that adding a checked
// return type to the corpus needs a binding and nothing else. It touches no CFFI
// type, which is what keeps it here (and unit-testable) rather than behind the
// integration tag.
func stockChecksOf(result any, declaresCheck bool) ([]CheckOutcome, error) {
	checksField, err := checksFieldOf(result, declaresCheck)
	if err != nil || !checksField.IsValid() {
		return nil, err
	}
	byLabel := map[string]CheckOutcome{}
	iter := checksField.MapRange()
	for iter.Next() {
		entry := iter.Value()
		name, err := stringField(entry, "Name")
		if err != nil {
			return nil, err
		}
		expr, err := stringField(entry, "Expression")
		if err != nil {
			return nil, err
		}
		status, err := stringField(entry, "Status")
		if err != nil {
			return nil, err
		}
		var passed bool
		switch status {
		case "succeeded":
			passed = true
		case "failed":
			passed = false
		default:
			// ResponseCheck::from_check_result emits exactly these two
			// (baml-types/src/constraint.rs:66-72). A third would mean the contract
			// moved; refusing to map it keeps a new status from silently becoming
			// "failed".
			return nil, fmt.Errorf("check %q has status %q, want %q or %q", name, status, "succeeded", "failed")
		}
		byLabel[name] = CheckOutcome{Label: name, Expression: expr, Passed: passed}
	}
	return sortedChecks(byLabel), nil
}

// checksFieldOf locates a stock result's `Checks` map and enforces the
// shape contract described on stockChecksOf. It returns the zero reflect.Value
// when there is legitimately nothing to read.
func checksFieldOf(result any, declaresCheck bool) (reflect.Value, error) {
	missing := func(what string) (reflect.Value, error) {
		if declaresCheck {
			return reflect.Value{}, fmt.Errorf(
				"the row declares a @check, so stock must return a Checked[T], but %s; "+
					"the result is undecodable rather than check-free", what)
		}
		return reflect.Value{}, nil
	}
	if result == nil {
		return missing("the result is nil")
	}
	rv := reflect.ValueOf(result)
	if rv.Kind() != reflect.Struct {
		// A plain decoded value: string, int64, a slice, ...
		return missing(fmt.Sprintf("the result is a %s, not a struct", rv.Kind()))
	}
	checksField := rv.FieldByName("Checks")
	if !checksField.IsValid() {
		// A decoded struct that is not a Checked[T] — DynamicClass, DynamicEnum, ...
		return missing(fmt.Sprintf("the %s result has no Checks field", rv.Type()))
	}
	if !declaresCheck {
		return reflect.Value{}, fmt.Errorf(
			"the row declares NO @check, yet stock returned a %s carrying a Checks field; "+
				"the CFFI wraps only a checked type, so the row and the generated source disagree", rv.Type())
	}
	if checksField.Kind() != reflect.Map {
		return reflect.Value{}, fmt.Errorf("Checks field is a %s, want a map", checksField.Kind())
	}
	return checksField, nil
}

func stringField(v reflect.Value, name string) (string, error) {
	f := v.FieldByName(name)
	if !f.IsValid() || f.Kind() != reflect.String {
		return "", fmt.Errorf("check entry has no string field %q", name)
	}
	return f.String(), nil
}

// ConstraintOutcomeProfile runs a row through the profile leg and CLASSIFIES the
// result, re-raising a harness failure instead of turning it into an outcome.
//
// It recovers exactly one panic type — value.UnorderableMaps, the fork's
// recoverable spelling of stock MiniJinja's `unreachable!()` (v2.16.0-baml.4,
// PATCHES #103) — mirroring RenderProfileOutcome. Any other panic is re-raised:
// it would be a genuine defect here, and swallowing it would let a stock panic
// row match a quietly-classified crash.
func ConstraintOutcomeProfile(r ConstraintRow) (o ConstraintOutcome) {
	defer func() {
		if rec := recover(); rec != nil {
			u, ok := rec.(value.UnorderableMaps)
			if !ok {
				panic(rec)
			}
			o = ConstraintOutcome{Kind: ConstraintPanic, Detail: u.Error()}
		}
	}()
	out, err := EvaluateConstraintRow(r)
	if err != nil {
		var he *harnessError
		if errors.As(err, &he) {
			panic(fmt.Sprintf("profileoracle: row %q: profile harness failed before the leaf evaluated (not an engine outcome): %v", r.ID, err))
		}
		panic(fmt.Sprintf("profileoracle: row %q: unexpected non-harness error from EvaluateConstraintRow: %v", r.ID, err))
	}
	return out
}

// GenerateConstraintBAMLSource builds the deterministic in-memory .baml project
// for the constraint corpus: the shared client, the SHARED types.baml (so
// hostValue's enum aliases and class field order describe the same declarations
// stock parses into), and one function per row.
//
// It is a separate project from the prompt corpus's, not extra rows in it: the
// two exercise different runtime entry points, and keeping them apart means the
// prompt corpus's checked-in source hash is untouched by constraint work.
func GenerateConstraintBAMLSource(rows []ConstraintRow) map[string]string {
	sorted := append([]ConstraintRow(nil), rows...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })

	var fns strings.Builder
	for _, r := range sorted {
		fns.WriteString(constraintFunctionSource(r))
		fns.WriteString("\n")
	}

	return map[string]string{
		"clients.baml":     clientSource(),
		"types.baml":       typesBAMLSource(),
		"constraints.baml": fns.String(),
	}
}

// constraintFunctionSource emits one constrained function.
//
// The constraint attributes hang off the RETURN TYPE, which is the position
// BAML's coercer runs run_user_checks for (field_type.rs:180-194) and the
// position its own test corpus uses:
//
//	function FooToInt(...) -> int @check(small_int, {{ this < 10 }})
//
// The expression is emitted back inside `{{ ... }}` because that is the SOURCE
// spelling; BAML's parser strips the brackets and stores the bare expression,
// which is what ConstraintDecl.Expression already holds. That round trip is
// itself part of the proof: stock's ResponseCheck.expression comes back as the
// bare string, and the differential compares it to ours.
func constraintFunctionSource(r ConstraintRow) string {
	var b strings.Builder
	b.WriteString("function ")
	b.WriteString(r.FuncName())
	b.WriteString("() -> ")
	b.WriteString(r.ReturnType)
	for _, c := range r.Constraints {
		b.WriteString(constraintAttributeSource(c))
	}
	b.WriteString(" {\n  client ")
	b.WriteString(clientName)
	b.WriteString("\n  prompt #\"\n")
	b.WriteString(constraintPromptBody)
	b.WriteString("\n\"#\n}\n")
	return b.String()
}

// constraintAttributeSource spells one constraint as its BAML attribute.
//
// A malformed declaration panics rather than emitting source stock BAML would
// reject with an opaque CreateRuntime parse error a hundred rows away from its
// cause. These are AUTHOR-CONTROLLED corpus constants, so a panic here is a
// corpus bug caught at generation time; TestConstraintCorpusIsWellFormed checks
// the same invariants without CFFI.
func constraintAttributeSource(c ConstraintDecl) string {
	var name string
	switch c.Level {
	case bamlprofile.ConstraintCheck:
		name = "check"
		if c.Label == nil {
			panic(fmt.Sprintf("profileoracle: check constraint %q has no label; BAML rejects an unlabelled check", c.Expression))
		}
	case bamlprofile.ConstraintAssert:
		name = "assert"
	default:
		panic(fmt.Sprintf("profileoracle: constraint %q has level %v, which is not a BAML level", c.Expression, c.Level))
	}
	if strings.Contains(c.Expression, "{{") || strings.Contains(c.Expression, "}}") {
		panic(fmt.Sprintf("profileoracle: constraint expression %q contains a jinja bracket pair; "+
			"a resolved expression is BARE and the generator would emit unparseable BAML", c.Expression))
	}
	if c.Label != nil {
		// The label is emitted verbatim as a BAML IDENTIFIER (the grammar's
		// `Expression::Identifier` arm, constraint.rs:44-49). Anything that is not
		// one — a space, a quote, a closing paren — would silently reshape the
		// attribute into a form BAML rejects a hundred rows away from its cause.
		if !isBAMLIdentifier(*c.Label) {
			panic(fmt.Sprintf("profileoracle: constraint label %q is not a BAML identifier; "+
				"the generator would emit an attribute stock BAML cannot parse", *c.Label))
		}
		return " @" + name + "(" + *c.Label + ", {{ " + c.Expression + " }})"
	}
	return " @" + name + "({{ " + c.Expression + " }})"
}

// isBAMLIdentifier reports whether s is a plain ASCII identifier, the shape a
// constraint label must have.
func isBAMLIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_':
		case i > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}

// ck builds a labelled check declaration.
func ck(label, expr string) ConstraintDecl {
	return ConstraintDecl{Level: bamlprofile.ConstraintCheck, Label: &label, Expression: expr}
}

// as_ builds an UNLABELLED assert declaration (`@assert({{ expr }})`).
func as_(expr string) ConstraintDecl {
	return ConstraintDecl{Level: bamlprofile.ConstraintAssert, Expression: expr}
}

// asl builds a LABELLED assert declaration (`@assert(label, {{ expr }})`).
func asl(label, expr string) ConstraintDecl {
	return ConstraintDecl{Level: bamlprofile.ConstraintAssert, Label: &label, Expression: expr}
}
