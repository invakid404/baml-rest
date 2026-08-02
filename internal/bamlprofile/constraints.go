package bamlprofile

import (
	"errors"
	"fmt"
	"strings"

	minijinja "github.com/invakid404/minijinja-go/v2"
	"github.com/invakid404/minijinja-go/v2/value"
)

// This file is BAML v0.223's CONSTRAINT evaluator and its narrow typed façade.
//
// Authority (byte-for-byte): BAML v0.223
//   - the evaluator: engine/baml-lib/baml-core/src/ir/jinja_helpers.rs:67-93
//   - the batch:     engine/baml-lib/jsonish/src/deserializer/coercer/mod.rs:322-338
//   - assert/check:  engine/baml-lib/jsonish/src/deserializer/coercer/field_type.rs:180-294
//   - the data type: engine/baml-lib/baml-types/src/constraint.rs:3-43
//
// BAML's evaluator is four lines and is copied, not improved:
//
//	pub fn evaluate_predicate(this: &BamlValue, e: &JinjaExpression) -> Result<bool> {
//	    let ctx = HashMap::from([("this", Value::from_serialize(this))]);
//	    match render_expression(e, &ctx)?.as_ref() {
//	        "true"  => Ok(true),
//	        "false" => Ok(false),
//	        _       => Err(anyhow!("Predicate did not evaluate to a boolean")),
//	    }
//	}
//	// render_expression: get_env(); format!("{{{{ {} }}}}", expression.0); render_str
//
// Four consequences drive every decision below:
//
//  1. the stored expression is BARE — BAML wraps it in `{{ ... }}` itself, so a
//     caller passing `{{ this > 0 }}` is passing a DIFFERENT string than the
//     reference evaluator receives and is rejected, not stripped;
//  2. the environment is a bare get_env(), so `_`, `ctx` and the enum namespaces
//     are undefined in a predicate (newConstraintEnvironment, env.go);
//  3. `this` is the SERDE projection of the BamlValue, not the prompt host object
//     (projectConstraintThis, project.go);
//  4. classification is on the RENDERED TEXT, not a boolean downcast: exactly
//     "true" and exactly "false"; anything else — including " true" and "True" —
//     is an ERROR, never a failed predicate.
//
// The batch mirrors run_user_checks' `collect::<Result<Vec<_>>>()`: one predicate
// error aborts the whole evaluation and yields NO partial report. A false ASSERT
// is not an error — it is a successful evaluation whose result is terminal at the
// serving layer (validate_asserts); a false CHECK is retained as metadata.
//
// What this façade does NOT own, by design: BAML source parsing, interpolation
// stripping, Jinja typechecking, BAML IR/descriptors, prompt globals, the
// serving `Checked<T>` envelope, its ordering policy, and assert-message wording.
// See doc.go for the full decline list.

// ConstraintLevel is a resolved BAML constraint kind
// (baml-types/src/constraint.rs:39-43).
//
// The zero value is deliberately INVALID so a `Constraint{}` built without a
// level fails the preflight instead of silently defaulting to a check or an
// assert — the two differ in whether a false result rejects the value.
type ConstraintLevel uint8

const (
	// ConstraintCheck is `@check(label, {{ expr }})`: a false result is retained
	// as metadata and does NOT reject the value.
	ConstraintCheck ConstraintLevel = iota + 1
	// ConstraintAssert is `@assert({{ expr }})` / `@assert(label, {{ expr }})`: a
	// false result is terminal for the parsed value at the serving layer.
	ConstraintAssert
)

// String renders a level for diagnostics.
func (l ConstraintLevel) String() string {
	switch l {
	case ConstraintCheck:
		return "check"
	case ConstraintAssert:
		return "assert"
	default:
		return fmt.Sprintf("ConstraintLevel(%d)", uint8(l))
	}
}

// Constraint is one already-RESOLVED BAML constraint: the exact
// `{level, expression, label}` triple baml_types::Constraint stores.
//
// Expression is the BARE JinjaExpression BAML records — `this > 0`, NOT
// `{{ this > 0 }}`. BAML's parser strips the interpolation brackets when it
// builds the JinjaExpression, and its evaluator adds them back; a caller that
// supplies the wrapped form is supplying a string the reference evaluator never
// sees, and EvaluateConstraints rejects it rather than guessing which layer the
// caller meant. Producing the bare form from a descriptor is the resolved
// adapter's job (Slice 7.2), not this leaf's.
//
// Label must be non-nil for ConstraintCheck — BAML's grammar and its type
// validator both require a check to carry an identifier
// (parser-database/src/attributes/constraint.rs:33-38). It is optional for
// ConstraintAssert. A non-nil label is a BAML identifier, so it is never empty.
type Constraint struct {
	Level      ConstraintLevel
	Expression string
	Label      *string
}

// clone deep-copies the label so a caller writing through its *string after the
// call cannot retroactively change a returned result's label — the same
// ownership rule newEnumMember applies to a resolved alias. The pointer-ness is
// preserved (nil is "no label", which is distinct from any string).
func (c Constraint) clone() Constraint {
	out := c
	if c.Label != nil {
		l := *c.Label
		out.Label = &l
	}
	return out
}

// ConstraintRequest is one evaluation: the resolved constraints in DECLARED
// order, and the single value they are all evaluated against.
//
// This must be a scalar, none, or a value built by the bamlprofile host
// constructors (EnumMember / ClassValue / ListValue). It is not an arbitrary Go
// or fork value: it is projected into BAML's serde shape before binding, and a
// shape with no proven projection is declined (project.go).
type ConstraintRequest struct {
	Constraints []Constraint
	This        value.Value
}

// ConstraintResult is one evaluated constraint. Results appear in exactly the
// input constraint order, matching run_user_checks' ordered iteration over the
// type's stored constraints.
type ConstraintResult struct {
	Constraint Constraint
	Passed     bool
}

// ConstraintReport is the outcome of a fully successful batch: every predicate
// compiled, rendered, and classified as a literal "true"/"false".
//
// AssertFailed is DERIVED from Results (a false ConstraintAssert), reproducing
// validate_asserts' rejection condition without reproducing its message or its
// first-five presentation — those are the serving layer's (Slice 7.2). A false
// CHECK is not an error and not a rejection: it stays in Results with
// Passed == false, which is what the caller turns into a ResponseCheck.
//
// This is deliberately NOT a `Checked<T>` envelope. The client-facing
// `{value, checks}` shape and its stable ordering are a later representation
// layer; PR-3 only guarantees that Results is in declared input order so that
// layer has a stable source order to work from.
type ConstraintReport struct {
	Results      []ConstraintResult
	AssertFailed bool
}

// ConstraintStage names WHERE an evaluation failed. It is part of the typed
// error so a caller can tell a caller-contract violation (validate/project)
// apart from a predicate the engine could not evaluate (compile/render/classify)
// without matching on message text.
type ConstraintStage string

const (
	// ConstraintStageValidate: the request violates a structural invariant BAML's
	// own parser/validator guarantees — an unknown level, a check with no label,
	// or a `{{ ... }}`-wrapped expression. BAML could never have produced it.
	ConstraintStageValidate ConstraintStage = "validate"
	// ConstraintStageProject: This is not a shape with a proven serde projection
	// (undefined, media, a native container, an unrecognized host object).
	ConstraintStageProject ConstraintStage = "project"
	// ConstraintStageCompile: the synthesized `{{ <expr> }}` template did not
	// parse. In BAML this is render_expression's `env.render_str(..)?`.
	ConstraintStageCompile ConstraintStage = "compile"
	// ConstraintStageRender: the template parsed but raised while rendering (an
	// unknown attribute chain, a bad filter argument, ...). Also
	// render_expression's `?`.
	ConstraintStageRender ConstraintStage = "render"
	// ConstraintStageClassify: the predicate rendered successfully but its text is
	// neither "true" nor "false" — BAML's `Predicate did not evaluate to a
	// boolean`. This is an evaluator error, NOT a failed predicate.
	ConstraintStageClassify ConstraintStage = "classify"
)

// ConstraintError is the single error type EvaluateConstraints returns. It
// carries the stage, the failing constraint, and its index, so a caller can
// report which of a batch of predicates went wrong without parsing a string.
//
// Index is the position in ConstraintRequest.Constraints, or -1 when the failure
// is not attributable to one constraint — which today means only
// ConstraintStageProject, since `This` is projected once for the whole batch.
// Constraint is the zero Constraint in that case.
type ConstraintError struct {
	Index      int
	Constraint Constraint
	Stage      ConstraintStage
	Err        error
}

func (e *ConstraintError) Error() string {
	if e.Index < 0 {
		return fmt.Sprintf("bamlprofile: constraint %s: %v", e.Stage, e.Err)
	}
	return fmt.Sprintf("bamlprofile: constraint %d (%s %q) %s: %v",
		e.Index, e.Constraint.Level, e.Constraint.Expression, e.Stage, e.Err)
}

func (e *ConstraintError) Unwrap() error { return e.Err }

// constraintTemplateName is the name given to the one-off synthetic template a
// predicate is compiled as. The fork does not store a template built with
// TemplateFromNamedString in the environment (environment.go:498-501), so the
// same name is reused for every predicate without any of them colliding. It is
// observable only in a compile/render error message; the environment forces
// AutoEscapeNone (env.go), so unlike stock MiniJinja's name-driven autoescape it
// cannot change what a predicate renders.
const constraintTemplateName = "bamlprofile_constraint"

// EvaluateConstraints evaluates every constraint in req against req.This and
// reports the results in input order.
//
// It reproduces BAML v0.223's run_user_checks + evaluate_predicate exactly:
//
//   - a bare get_env() environment with NO prompt globals;
//   - `this` bound to the SERDE projection of the host value, and nothing else
//     bound;
//   - each stored expression wrapped verbatim as `{{ <expr> }}` and rendered;
//   - the rendered text classified as literally "true" or "false";
//   - the batch ABORTING on the first evaluator error, with no partial report,
//     mirroring `collect::<Result<Vec<_>>>()`.
//
// The two structural preflights run BEFORE any evaluation, so a malformed
// request can never produce a half-evaluated report:
//
//   - every constraint is validated (validateConstraint);
//   - req.This is projected once (projectConstraintThis).
//
// The projection runs even when there are no constraints. BAML would skip the
// evaluator entirely in that case (field_type.rs:180), so this errors where BAML
// is silent — the conservative direction. It never does the reverse: an
// unsupported value is refused, never bound and rendered against.
//
// A false assert is NOT an error: it is a successful evaluation whose report has
// AssertFailed set. A false check is likewise a normal result. Only an
// unsupported projection, a compile failure, a render failure, or a
// non-"true"/"false" rendering produce an error, and each returns a
// *ConstraintError naming its stage.
func EvaluateConstraints(req ConstraintRequest) (ConstraintReport, error) {
	// PREFLIGHT 1: structural invariants, over the WHOLE batch, before anything is
	// evaluated. These are conditions BAML's parser and type validator already
	// guarantee, so a request that trips one describes something stock BAML could
	// not have produced; failing loud beats evaluating a predicate whose meaning we
	// would have had to guess at.
	//
	// The constraints are cloned here, once, and everything downstream (including
	// the returned Results) uses the COPIES — so a caller mutating its slice or a
	// label pointee after the call cannot change the report it was handed.
	constraints := make([]Constraint, len(req.Constraints))
	for i, c := range req.Constraints {
		if err := validateConstraint(c); err != nil {
			return ConstraintReport{}, &ConstraintError{Index: i, Constraint: c.clone(), Stage: ConstraintStageValidate, Err: err}
		}
		constraints[i] = c.clone()
	}

	// PREFLIGHT 2: the serde projection of `this`, done ONCE for the batch exactly
	// as BAML builds one `Value::from_serialize(this)` per evaluate_predicate call
	// over the same value.
	this, err := projectConstraintThis(req.This)
	if err != nil {
		return ConstraintReport{}, &ConstraintError{Index: -1, Stage: ConstraintStageProject, Err: err}
	}

	// The context has exactly ONE key, matching evaluate_predicate's single-entry
	// HashMap. Its map ordering is therefore unobservable; the ordering that IS
	// observable lives inside a projected class, which uses the fork's ordered map.
	// The fork passes an already-built value.Value through unchanged
	// (value.FromAny, value/value.go:699-721), so the projected host value reaches
	// the predicate as itself.
	ctx := value.FromMap(map[string]value.Value{"this": this})

	// One environment for the batch. BAML calls get_env() per predicate, but the
	// environment is pure configuration — formatter, flags, filters, unknown-method
	// callback — with no per-render mutable state and no stored templates, so a
	// shared instance renders identically to a fresh one. It carries NO globals:
	// `_`, `ctx` and the enum namespaces must be undefined in a predicate.
	env := newConstraintEnvironment()

	results := make([]ConstraintResult, 0, len(constraints))
	assertFailed := false
	for i, c := range constraints {
		// render_expression's `format!(r#"{{{{ {} }}}}"#, expression.0)`: the stored
		// expression wrapped VERBATIM. No trimming, no normalization — whatever the
		// resolved constraint holds is what BAML would have wrapped.
		tmpl, err := env.TemplateFromNamedString(constraintTemplateName, "{{ "+c.Expression+" }}")
		if err != nil {
			return ConstraintReport{}, &ConstraintError{Index: i, Constraint: c, Stage: ConstraintStageCompile, Err: err}
		}
		text, err := tmpl.Render(ctx)
		if err != nil {
			return ConstraintReport{}, &ConstraintError{Index: i, Constraint: c, Stage: ConstraintStageRender, Err: err}
		}
		passed, ok := classifyRenderedBool(text)
		if !ok {
			return ConstraintReport{}, &ConstraintError{Index: i, Constraint: c, Stage: ConstraintStageClassify,
				Err: fmt.Errorf("predicate did not evaluate to a boolean (rendered %q)", text)}
		}
		if !passed && c.Level == ConstraintAssert {
			assertFailed = true
		}
		results = append(results, ConstraintResult{Constraint: c, Passed: passed})
	}
	return ConstraintReport{Results: results, AssertFailed: assertFailed}, nil
}

// classifyRenderedBool is BAML's rendered-TEXT boolean classifier
// (jinja_helpers.rs:89-93). ok is false for every other successful rendering.
//
// The comparison is byte-exact on purpose. BAML matches the rendered string
// against the literals "true" and "false"; it does not trim, lowercase, or
// downcast the underlying value. So `{{ this }}` on the boolean true renders
// "true" and passes, while a predicate rendering " true", "True", "1" or "" is
// an EVALUATOR ERROR — not a failed predicate. Treating any of those as false
// would silently pass an unevaluatable predicate off as a rejection.
func classifyRenderedBool(text string) (passed, ok bool) {
	switch text {
	case "true":
		return true, true
	case "false":
		return false, true
	default:
		return false, false
	}
}

// validateConstraint rejects the resolved-constraint shapes stock BAML cannot
// produce. Each one is a structural impossibility upstream, so rejecting it can
// never lose parity — it can only refuse a request BAML never issues.
func validateConstraint(c Constraint) error {
	switch c.Level {
	case ConstraintCheck:
		// parser-database/src/attributes/constraint.rs:33-38 and the type validator
		// (validations/types.rs:231-238) BOTH reject an unlabelled check.
		if c.Label == nil {
			return errors.New("a check constraint must have a label")
		}
	case ConstraintAssert:
		// An assert's label is optional.
	default:
		return fmt.Errorf("unknown constraint level %d", uint8(c.Level))
	}
	// A PRESENT label is a BAML identifier, which cannot be empty. An empty one
	// would become an empty ResponseCheck name at the serving layer — a silent
	// collision magnet — so it is refused here instead.
	if c.Label != nil && *c.Label == "" {
		return errors.New("constraint label is present but empty")
	}
	if isBracketWrapped(c.Expression) {
		return fmt.Errorf("expression %q is wrapped in {{ ... }}; a resolved constraint holds the BARE expression "+
			"(the evaluator adds the brackets), so this leaf declines rather than stripping them", c.Expression)
	}
	return nil
}

// isBracketWrapped reports whether an expression was handed over still wrapped in
// Jinja interpolation brackets.
//
// This is a DECLINE detector, not a parser: it recognizes the one mistake a
// descriptor adapter actually makes — passing the source spelling
// `{{ this > 0 }}` where BAML stores `this > 0`. A bare expression that happens
// to both start with `{{` and end with `}}` (only reachable via string literals,
// e.g. `"{{" ~ this ~ "}}"`) is refused too. That is deliberate: refusing is a
// loud, recoverable decline, whereas silently evaluating `{{ {{ this > 0 }} }}`
// or silently stripping the brackets would be a wrong answer. If such an
// expression ever turns up in real resolved metadata, the fix is an explicit
// differentially-proven carve-out, not a looser heuristic here.
func isBracketWrapped(expr string) bool {
	t := strings.TrimSpace(expr)
	return len(t) >= 4 && strings.HasPrefix(t, "{{") && strings.HasSuffix(t, "}}")
}

// newConstraintEnvironment builds the environment BAML v0.223 evaluates a
// PREDICATE in: get_env() and NOTHING else (jinja_helpers.rs:67-77, which calls
// get_env() and then binds only `this`).
//
// It stops at newGetEnvBase on purpose. Adding `_`, `ctx`, or the per-enum
// namespace globals here — the three things [New] adds for a prompt — would let
// a predicate reference names stock BAML leaves undefined, which is an out-do:
// `{{ Color.RED }}` inside a constraint must be an error on both legs, not a
// value on ours. The profileoracle context-isolation rows prove that against
// live CFFI.
//
// No fork change is implied by this split; it is entirely a baml-rest-owned
// factory boundary (see newGetEnvBase).
func newConstraintEnvironment() *minijinja.Environment {
	return newGetEnvBase()
}
