package debaml

import (
	"fmt"
	"math"
	"regexp"
	"sync"

	mj "github.com/mitsuhiko/minijinja/minijinja-go/v2"
	"github.com/mitsuhiko/minijinja/minijinja-go/v2/filters"
	"github.com/mitsuhiko/minijinja/minijinja-go/v2/syntax"
	mjvalue "github.com/mitsuhiko/minijinja/minijinja-go/v2/value"
)

// Constraint expression evaluation — the native half of BAML v0.223's
// @assert / @check predicates.
//
// SCOPE. This file is the EVALUATOR ONLY. Nothing here is wired into
// admission, coercion or serving: every reachable @assert/@check still declines
// to BAML at checkSupported (see boundary_decline_test.go
// TestNativeDeclines_Constraints). It exists so the later serving slice has a
// proven expression engine to sit behind, and so the divergences it cannot
// close are measured before anything depends on them.
//
// AUTHORITY. BAML v0.223 evaluates a constraint by rendering a minijinja
// template, not by walking a bespoke AST
// (engine/baml-lib/baml-core/src/ir/jinja_helpers.rs):
//
//	get_env()            :7-36   the environment reproduced by [newConstraintEnv]
//	render_expression    :69-79  wraps the bare source as `{{ <expr> }}` and renders it
//	evaluate_predicate   :83-94  context {"this": from_serialize(value)}; the render
//	                             must be exactly "true" or "false", else an error
//
// The engine is the SAME minijinja 2.16.0 the prompt renderer already uses
// (internal/nativeprompt), via the pinned pure-Go port
// github.com/mitsuhiko/minijinja/minijinja-go/v2 v2.16.0. No new dependency.

// errConstraintNotBoolean is BAML's `Predicate did not evaluate to a boolean`
// (jinja_helpers.rs:92): the render succeeded but produced something other than
// the exact strings "true"/"false". BAML surfaces it as a coercion failure of
// the constrained node ("Failed to evaluate constraints: ..."), i.e. it is NOT
// a false check — it rejects the value outright.
var errConstraintNotBoolean = fmt.Errorf("debaml: predicate did not evaluate to a boolean")

// newConstraintEnv builds the minijinja environment BAML installs for constraint
// evaluation, matching jinja_helpers.rs get_env() (:7-36) line for line:
//
//   - a formatter that renders a TOP-LEVEL none as "null" instead of
//     minijinja's "none", then defers to the auto-escape formatter (:10-27);
//   - debug mode (:29);
//   - trim_blocks + lstrip_blocks (:30-31);
//   - the `regex_match` filter (:32);
//   - the `sum` filter, which REPLACES minijinja's built-in one (:33);
//   - the minijinja-contrib pycompat unknown-method callback (:34).
//
// Two deliberate deviations, both forced by the port and both fail-closed:
//
//   - AUTO-ESCAPE. Rust reaches the formatter through `escape_formatter`, whose
//     escaping comes from the template name; `render_str` names the template
//     `<string>`, which has no extension, so minijinja's default callback
//     selects AutoEscape::None. minijinja-Go's default callback is the same
//     shape, but pinning it explicitly makes the constraint environment
//     independent of how the port names an anonymous template.
//   - UNKNOWN-METHOD CALLBACK. minijinja-Go v2.16.0 exposes no
//     environment-level unknown-method hook (there is no SetUnknownMethodCallback;
//     state.go dispatches obj.CallMethod then GetAttr and otherwise errors). The
//     pycompat MAP methods are therefore implemented on the mapping object the
//     value model owns ([orderedMapping.CallMethod], a port of
//     minijinja-contrib 2.16.0 pycompat.rs:276-318). The STRING and SEQUENCE
//     halves (`"s".upper()`, `"{}".format(x)`, `[1,2].count(1)`, …) cannot be:
//     a Go string Value has no method table and wrapping it in an object would
//     stop it being a string everywhere else. Those calls raise an evaluation
//     ERROR here where BAML answers — which is the safe direction (an error
//     declines to BAML) but IS a real gap, measured by the differential harness.
//
// constraintEnv is built once and shared. The environment is immutable after
// construction — the only entry point is TemplateFromString, which compiles
// without storing anything in it (environment.go:503-517), and each render gets
// its own State — so a single instance is safe for concurrent use and saves
// re-registering the whole builtin set per predicate.
var constraintEnv = sync.OnceValue(newConstraintEnv)

func newConstraintEnv() *mj.Environment {
	env := mj.NewEnvironment()

	// jinja_helpers.rs:10-27. Rust substitutes the VALUE (`&Value::from("null")`)
	// before handing it to escape_formatter, so the replacement is escaped like
	// any other string; with AutoEscape::None that is the identity.
	env.SetFormatter(func(_ *mj.State, val mjvalue.Value, escape func(string) string) string {
		if val.IsNone() {
			return escape("null")
		}
		return escape(displayString(val))
	})

	env.SetDebug(true)
	env.SetWhitespace(syntax.WhitespaceConfig{TrimBlocks: true, LstripBlocks: true})
	env.SetAutoEscapeFunc(func(string) mj.AutoEscape { return mj.AutoEscapeNone })

	env.AddFilter("regex_match", filterRegexMatch)
	env.AddFilter("sum", filterSum)

	withdrawNonBAMLBuiltins(env)

	return env
}

// withdrawNonBAMLBuiltins removes the builtins minijinja-Go registers that
// BAML's minijinja build does NOT have.
//
// BAML links minijinja with `default-features = false` and an explicit feature
// list (engine/Cargo.toml:99-115: macros, builtins, debug, preserve_order,
// adjacent_loop_items, unicode, json, unstable_machinery, custom_syntax,
// deserialization, serde). minijinja-Go registers one flat default set, so its
// surface is a strict SUPERSET of BAML's. Comparing minijinja 2.16.0's
// defaults.rs (get_builtin_filters / get_builtin_tests / get_globals under
// builtins+json, without urlencode) against minijinja-go/v2 defaults.go, the
// difference is exactly five names:
//
//	filter   urlencode  — Rust-gated behind the `urlencode` feature, which BAML does not enable
//	test     containing — not present in minijinja 2.16.0 at all
//	function cycler     — not present in minijinja 2.16.0 at all
//	function joiner     — not present in minijinja 2.16.0 at all
//	function lipsum     — not present in minijinja 2.16.0 at all
//
// Leaving them registered would be the DANGEROUS asymmetry: native would answer
// `"a b"|urlencode == "a%20b"` -> true where BAML raises
// `unknown filter: filter urlencode is unknown` and rejects the value. All five
// are verified against stock v0.223 by the differential harness. minijinja-Go
// has no RemoveFilter/RemoveTest/RemoveFunction, so each is replaced by a stub
// that raises the same class of error minijinja raises for an unknown name.
func withdrawNonBAMLBuiltins(env *mj.Environment) {
	env.AddFilter("urlencode", func(filters.State, mjvalue.Value, []mjvalue.Value, map[string]mjvalue.Value) (mjvalue.Value, error) {
		return mjvalue.Undefined(), mj.NewError(mj.ErrUnknownFilter, "filter urlencode is unknown")
	})
	env.AddTest("containing", func(filters.State, mjvalue.Value, []mjvalue.Value) (bool, error) {
		return false, mj.NewError(mj.ErrUnknownTest, "test containing is unknown")
	})
	for _, name := range []string{"cycler", "joiner", "lipsum"} {
		env.AddFunction(name, func(*mj.State, []mjvalue.Value, map[string]mjvalue.Value) (mjvalue.Value, error) {
			return mjvalue.Undefined(), mj.NewError(mj.ErrUnknownFunction, name+" is unknown")
		})
	}
}

// RenderConstraintExpression renders a bare BAML constraint expression against
// the given value bound to `this`, returning the rendered TEXT.
//
// It is the native [render_expression] + the one-entry context
// [evaluate_predicate] builds, kept separate so callers (and the differential
// harness) can observe non-boolean renders, which BAML also distinguishes:
//
//	render_expression   jinja_helpers.rs:69-79  template = "{{ " + expr + " }}"
//	evaluate_predicate  jinja_helpers.rs:87-88  ctx = {"this": <value>}
//
// A compile or evaluation failure is returned as an error; BAML turns the same
// failure into a coercion error on the constrained node, never into a passing
// or failing check.
func RenderConstraintExpression(this ConstraintValue, expression string) (string, error) {
	env := constraintEnv()
	// jinja_helpers.rs:76 — `format!(r#"{{{{ {} }}}}"#, expression.0)`, i.e. the
	// literal "{{ ", the bare source, then " }}". The single space on each side
	// is part of the template and must not be trimmed. TemplateFromString names
	// the template `<string>`, the same name Rust's render_str uses.
	tmpl, err := env.TemplateFromString("{{ " + expression + " }}")
	if err != nil {
		return "", fmt.Errorf("debaml: compile constraint expression: %w", err)
	}
	out, err := tmpl.Render(map[string]mjvalue.Value{"this": this.toMinijinja()})
	if err != nil {
		return "", fmt.Errorf("debaml: evaluate constraint expression: %w", err)
	}
	return out, nil
}

// EvaluateConstraint evaluates one @assert/@check predicate over a value,
// reproducing BAML v0.223's evaluate_predicate (jinja_helpers.rs:83-94).
//
// The render must be EXACTLY "true" or "false". Anything else — a number, an
// empty string, a rendered list — is [errConstraintNotBoolean], matching BAML's
// `Predicate did not evaluate to a boolean`; BAML treats that as a hard failure
// of the constrained node, not as a failed check, so a caller must never map it
// to `status: failed`.
//
// FAIL-CLOSED CONTRACT for the future serving slice. Any error returned here —
// unknown filter, unknown method, bad operand, non-boolean render — means
// native could not reproduce BAML's answer and the request must decline to
// BAML. Errors are strictly MORE common here than in BAML (see the pycompat
// note on [newConstraintEnv]), which is the safe asymmetry. The differential
// harness enumerates where the two disagree WITHOUT erroring; those shapes must
// be declined by expression shape, not trusted.
func EvaluateConstraint(this ConstraintValue, expression string) (bool, error) {
	rendered, err := RenderConstraintExpression(this, expression)
	if err != nil {
		return false, err
	}
	switch rendered {
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, errConstraintNotBoolean
	}
}

// displayString reproduces Rust's `impl Display for minijinja::Value` for the
// value shapes the constraint model can produce.
//
// minijinja-Go's Value.String() does not consult value.ObjectWithString (the
// same gap internal/nativeprompt/env.go documents for media markers), so a
// mapping produced by the value model would otherwise render as Go's default
// object formatting instead of the minijinja map rendering.
func displayString(val mjvalue.Value) string {
	if obj, ok := val.AsObject(); ok {
		if s, ok := obj.(mjvalue.ObjectWithString); ok {
			return s.ObjectString()
		}
	}
	return val.String()
}

// filterRegexMatch is BAML's `regex_match` filter (jinja_helpers.rs:38-43):
//
//	fn regex_match(value: String, regex: String) -> bool {
//	    match Regex::new(&regex) { Err(_) => false, Ok(re) => re.is_match(&value) }
//	}
//
// Two behaviours that are easy to get wrong and are both load-bearing:
//
//   - An INVALID pattern is not an error — it is `false`. A native
//     regexp.Compile failure must therefore also yield false, not an error, or
//     native would decline where BAML answers.
//   - Both parameters are declared `String`, and minijinja's ArgType for String
//     is `value.to_string()` (minijinja 2.16.0 value/argtypes.rs:964-985) — i.e.
//     it accepts ANY value and Displays it. `1|regex_match("1")` is true in
//     BAML, not a type error, so this uses [displayString] rather than AsString.
//
// Engine note: Rust's `regex` crate and Go's `regexp` are both RE2-style
// (linear-time, no backreferences, no lookaround), which is why this is
// portable at all; the differential harness exercises the syntax surface that
// the two do not share exactly.
func filterRegexMatch(_ filters.State, val mjvalue.Value, args []mjvalue.Value, kwargs map[string]mjvalue.Value) (mjvalue.Value, error) {
	if len(kwargs) > 0 {
		return mjvalue.Undefined(), mj.NewError(mj.ErrTooManyArguments, "regex_match takes no keyword arguments")
	}
	if len(args) < 1 {
		return mjvalue.Undefined(), mj.NewError(mj.ErrMissingArgument, "regex_match requires a pattern argument")
	}
	if len(args) > 1 {
		return mjvalue.Undefined(), mj.NewError(mj.ErrTooManyArguments, "regex_match takes exactly one argument")
	}
	re, err := regexp.Compile(displayString(args[0]))
	if err != nil {
		return mjvalue.FromBool(false), nil
	}
	return mjvalue.FromBool(re.MatchString(displayString(val))), nil
}

// filterSum is BAML's `sum` filter (jinja_helpers.rs:45-65), which REPLACES
// minijinja's built-in `sum`. The built-in takes an `attribute` kwarg and sums
// whatever it finds; BAML's takes none and has a specific int-vs-float rule:
//
//	if every element converts to i64      -> the i64 sum
//	else if every element converts to f64 -> the f64 sum
//	else                                  -> 0
//
// The conversions are minijinja's, not Go's, and they are asymmetric
// (value/argtypes.rs:410-465):
//
//	i64: bool -> 0/1; i64/u64/i128/u128 -> itself; f64 ONLY if integral
//	     (`val as i64 as f64 == val`), so 2.0 is an int but 2.5 is not
//	f64: i64/u64/i128/u128/f64 -> itself; bool is NOT convertible
//
// so `[1, 2.0]|sum` is the INT 3 (not 3.0), `[1, 2.5]|sum` is 3.5, `[true, 1]`
// is the int 2 (bool converts to i64 but not f64), and `["a"]|sum` is 0.
//
// The input is declared `Vec<Value>`, whose ArgType requires an object with
// ObjectRepr Seq or Iterable (value/argtypes.rs:987-1006): a string, a mapping
// or a scalar is `not iterable`, an ERROR rather than 0.
func filterSum(_ filters.State, val mjvalue.Value, args []mjvalue.Value, kwargs map[string]mjvalue.Value) (mjvalue.Value, error) {
	if len(args) > 0 || len(kwargs) > 0 {
		// BAML's sum_filter declares no arguments, so minijinja rejects any.
		// (This is also why `|sum(attribute="x")` — legal against minijinja's
		// built-in — is an error under BAML.)
		return mjvalue.Undefined(), mj.NewError(mj.ErrTooManyArguments, "sum takes no arguments")
	}
	items, ok := sumIterable(val)
	if !ok {
		return mjvalue.Undefined(), mj.NewError(mj.ErrInvalidOperation, "not iterable")
	}

	intSum, intOK := int64(0), true
	for _, item := range items {
		n, ok := minijinjaAsI64(item)
		if !ok {
			intOK = false
			break
		}
		intSum += n
	}
	if intOK {
		return mjvalue.FromInt(intSum), nil
	}

	floatSum, floatOK := float64(0), true
	for _, item := range items {
		f, ok := minijinjaAsF64(item)
		if !ok {
			floatOK = false
			break
		}
		floatSum += f
	}
	if floatOK {
		return mjvalue.FromFloat(floatSum), nil
	}
	return mjvalue.FromInt(0), nil
}

// sumIterable reproduces the `Vec<Value>` ArgType admission rule: only a
// sequence or a generic iterable is accepted. Strings (which minijinja-Go will
// happily iterate as runes) and mappings are NOT, so they must be rejected here
// rather than silently summed.
func sumIterable(val mjvalue.Value) ([]mjvalue.Value, bool) {
	switch val.Kind() {
	case mjvalue.KindSeq, mjvalue.KindIterable:
		return val.Iter(), true
	default:
		return nil, false
	}
}

// minijinjaAsI64 is minijinja's `i64::try_from(Value)`
// (value/argtypes.rs:410-422 primitive_int_try_from): bools convert, integral
// floats convert, non-integral floats and everything else do not.
func minijinjaAsI64(v mjvalue.Value) (int64, bool) {
	if b, ok := v.AsBool(); ok {
		if b {
			return 1, true
		}
		return 0, true
	}
	if v.Kind() != mjvalue.KindNumber {
		return 0, false
	}
	if v.IsActualInt() {
		i, ok := v.AsInt()
		return i, ok
	}
	f, ok := v.AsFloat()
	if !ok {
		return 0, false
	}
	// `ValueRepr::F64(val) if (val as i64 as f64 == val)`. The Rust cast
	// saturates rather than wrapping, so an out-of-range float fails the
	// round-trip and is rejected; the explicit range test reproduces that
	// without relying on Go's undefined out-of-range conversion.
	if f < math.MinInt64 || f >= math.MaxInt64 || float64(int64(f)) != f {
		return 0, false
	}
	return int64(f), true
}

// minijinjaAsF64 is minijinja's `f64::try_from(Value)`
// (value/argtypes.rs:465-471): every numeric repr converts; bool does NOT.
func minijinjaAsF64(v mjvalue.Value) (float64, bool) {
	if v.Kind() != mjvalue.KindNumber {
		return 0, false
	}
	return v.AsFloat()
}

// errArgCount / errUnknownKwargs render the argument errors the pycompat
// methods raise, matching minijinja's `from_args` arity checking.
func errArgCount(method string, want, got int) error {
	return mj.NewError(mj.ErrTooManyArguments,
		fmt.Sprintf("%s() takes %d argument(s), got %d", method, want, got))
}

func errUnknownKwargs(method string) error {
	return mj.NewError(mj.ErrTooManyArguments,
		fmt.Sprintf("%s() takes no keyword arguments", method))
}
