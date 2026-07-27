package debaml

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	mj "github.com/mitsuhiko/minijinja/minijinja-go/v2"
	"github.com/mitsuhiko/minijinja/minijinja-go/v2/filters"
	"github.com/mitsuhiko/minijinja/minijinja-go/v2/tests"
	mjvalue "github.com/mitsuhiko/minijinja/minijinja-go/v2/value"
)

// The PROVEN PROFILE — the evaluator's fail-closed boundary.
//
// THE CONTRACT. [EvaluateConstraint] returns either
//
//	(b, nil)                        where b is byte-identical to what BAML v0.223 decides, or
//	(_, ErrConstraintUnsupported)   meaning native cannot reproduce BAML here
//
// and NOTHING else. It must never hand back a usable boolean that BAML would
// have decided differently, or that BAML would have refused to decide at all.
// That is the whole safety argument for the serving slice that will consume
// this: an unsupported result declines to BAML, a wrong boolean would be
// served.
//
// Enforcing it needs two mechanisms, because the residual divergences between
// minijinja-Go v2.16.0 and BAML's minijinja 2.16.0 build come in two shapes.
//
// 1. SHAPES WRONG IN EVERY REPRESENTATION — a handful of filters and one test
// whose minijinja-Go behaviour differs from Rust's for a recognisable input.
// Each is wrapped by a guard that recognises the input and returns
// ErrConstraintUnsupported before delegating to the builtin
// ([installProfileGuards]). These are enumerated, and the stock differential
// (internal/debaml/constraintoracle) proves the enumeration is complete over
// the whole minijinja-Go surface.
//
// 2. SHAPES SENSITIVE TO HOW A MAPPING IS REPRESENTED. BAML's mappings are
// insertion-ordered IndexMaps; minijinja-Go offers an ordered mapping OBJECT
// (order-faithful, but invisible to the `in` operator, which type-switches on
// the concrete payload) and a native Go map (membership-faithful, but
// enumerated SORTED). Neither is faithful on both axes, and the gaps are not
// reachable by wrapping filters — `in` is an operator and `~` is an operator,
// with no extension point.
//
// So instead of guessing which paths observe which property, the evaluator
// ASKS: a value carrying any mapping is rendered TWICE, once under each
// representation, and the result is trusted only if the two agree
// ([renderConstraint]). Any expression whose answer depends on the
// representation — `in` over a mapping, iteration order, `~` or `|string` over
// a mapping, and anything not yet enumerated — disagrees, and is refused. This
// is why the profile is closed against constructs nobody has thought of yet,
// which a fixed guard list can never be.
//
// WHAT THIS COSTS. Mapping predicates narrow to membership-free, order-free
// use: `this.field`, `this["k"]`, `this|length`, `this is mapping`, equality,
// `|dictsort`. Iterating or rendering a mapping, and `in` over one, are
// refused. The differential records the exact set (every case whose native
// outcome is `outUnsupported`), so the cost is measured rather than asserted.

// ErrConstraintUnsupported marks an expression, or an expression/value pair,
// that is outside the proven native profile. Callers MUST treat it as "decline
// to BAML"; it is never a failed check.
//
// Every error [EvaluateConstraint] and [RenderConstraintExpression] return
// wraps this sentinel — including ordinary minijinja compile and evaluation
// errors, which are equally a statement that native did not reproduce BAML's
// answer. errors.Is is therefore a total test for "native could not decide".
var ErrConstraintUnsupported = errors.New("debaml: constraint expression outside the proven native profile")

// unsupportedConstraint wraps a reason as the sentinel. (The parse path has its
// own `unsupported` helper over bamlutils.ErrDeBAMLParseUnsupported; the two
// sentinels are deliberately distinct — this one is not wired into the decline
// plumbing, because the evaluator is not wired into parsing at all yet.)
func unsupportedConstraint(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrConstraintUnsupported, fmt.Sprintf(format, args...))
}

// mappingMode selects how the value model projects a BAML map/class.
type mappingMode uint8

const (
	// mappingOrdered is the primary projection: an ordered mapping object whose
	// iteration is BAML's insertion order ([orderedMapping]).
	mappingOrdered mappingMode = iota
	// mappingNative is the second opinion: minijinja-Go's own mapping, whose
	// membership and equality are faithful but whose iteration is sorted.
	mappingNative
)

// installProfileGuards wraps the builtins whose minijinja-Go behaviour differs
// from BAML's minijinja for a recognisable input, so the difference surfaces as
// ErrConstraintUnsupported instead of as a wrong boolean.
//
// Every entry below is a MEASURED divergence, each pinned by a case in the
// stock differential; none is speculative.
func installProfileGuards(env *mj.Environment) {
	// EVERY filter minijinja-Go registers, so the integer-result guard below is
	// TOTAL rather than a hand-picked subset. Round 5 shipped a subset and `last`
	// was missing from it, which let `range(...)|last` carry an out-of-range
	// integer into an exact comparison. A list that must be complete is safer
	// written out than inferred.
	builtins := map[string]mj.FilterFunc{
		"upper": filters.FilterUpper, "lower": filters.FilterLower,
		"capitalize": filters.FilterCapitalize, "title": filters.FilterTitle,
		"trim": filters.FilterTrim, "replace": filters.FilterReplace,
		"format": filters.FilterFormat, "default": filters.FilterDefault,
		"d": filters.FilterDefault, "safe": filters.FilterSafe,
		"escape": filters.FilterEscape, "e": filters.FilterEscape,
		"string": filters.FilterString, "bool": filters.FilterBool,
		"split": filters.FilterSplit, "lines": filters.FilterLines,
		"length": filters.FilterLength, "count": filters.FilterLength,
		"first": filters.FilterFirst, "last": filters.FilterLast,
		"reverse": filters.FilterReverse, "sort": filters.FilterSort,
		"join": filters.FilterJoin, "list": filters.FilterList,
		"unique": filters.FilterUnique, "min": filters.FilterMin,
		"max": filters.FilterMax, "batch": filters.FilterBatch,
		"slice": filters.FilterSlice, "map": filters.FilterMap,
		"select": filters.FilterSelect, "reject": filters.FilterReject,
		"selectattr": filters.FilterSelectAttr, "rejectattr": filters.FilterRejectAttr,
		"groupby": filters.FilterGroupBy, "chain": filters.FilterChain,
		"zip": filters.FilterZip, "abs": filters.FilterAbs,
		"int": filters.FilterInt, "float": filters.FilterFloat,
		"round": filters.FilterRound, "items": filters.FilterItems,
		"dictsort": filters.FilterDictSort, "attr": filters.FilterAttr,
		"indent": filters.FilterIndent, "pprint": filters.FilterPprint,
		"tojson": filters.FilterTojson,
		// BAML's own, replacing minijinja-Go's built-in `sum`.
		"sum":         filterSum,
		"regex_match": filterRegexMatch,
	}

	// Filters whose minijinja-Go behaviour differs from Rust's for a
	// recognisable input get a specific guard first; the integer-result guard is
	// then layered over everything uniformly.
	lengthGuard := func(name string, builtin mj.FilterFunc) mj.FilterFunc {
		return func(state filters.State, val mjvalue.Value, args []mjvalue.Value, kwargs map[string]mjvalue.Value) (mjvalue.Value, error) {
			if _, ok := val.Len(); !ok {
				return mjvalue.Undefined(), unsupportedConstraint("length of a value with no length (kind %s); minijinja rejects it", val.Kind())
			}
			return builtin(state, val, args, kwargs)
		}
	}
	builtins["length"] = lengthGuard("length", builtins["length"])
	builtins["count"] = lengthGuard("count", builtins["count"])

	// `split`: minijinja returns a LAZY ITERATOR with no length, minijinja-Go a
	// materialised list, and the difference leaks into length, indexing and
	// equality on the result.
	builtins["split"] = func(filters.State, mjvalue.Value, []mjvalue.Value, map[string]mjvalue.Value) (mjvalue.Value, error) {
		return mjvalue.Undefined(), unsupportedConstraint("`split` returns a lazy iterator in minijinja and a list in minijinja-Go")
	}

	// `last`: minijinja rejects a mapping; minijinja-Go returns its final key.
	lastBuiltin := builtins["last"]
	builtins["last"] = func(state filters.State, val mjvalue.Value, args []mjvalue.Value, kwargs map[string]mjvalue.Value) (mjvalue.Value, error) {
		if val.Kind() == mjvalue.KindMap {
			return mjvalue.Undefined(), unsupportedConstraint("`last` over a mapping; minijinja rejects it")
		}
		return lastBuiltin(state, val, args, kwargs)
	}

	// `items`/`tojson`: BAML's insertion order is lost through minijinja-Go's
	// unordered AsMap seam, and is unrecoverable for a mapping literal.
	itemsBuiltin := builtins["items"]
	builtins["items"] = func(state filters.State, val mjvalue.Value, args []mjvalue.Value, kwargs map[string]mjvalue.Value) (mjvalue.Value, error) {
		if val.Kind() == mjvalue.KindMap {
			return mjvalue.Undefined(), unsupportedConstraint("`items` over a mapping; minijinja-Go sorts the keys, minijinja preserves insertion order")
		}
		return itemsBuiltin(state, val, args, kwargs)
	}
	tojsonBuiltin := builtins["tojson"]
	builtins["tojson"] = func(state filters.State, val mjvalue.Value, args []mjvalue.Value, kwargs map[string]mjvalue.Value) (mjvalue.Value, error) {
		if containsMapping(val, 0) {
			return mjvalue.Undefined(), unsupportedConstraint("`tojson` over a value containing a mapping; key order differs")
		}
		return tojsonBuiltin(state, val, args, kwargs)
	}

	// Foreign mappings — a `{...}` literal or `dict(...)` built INSIDE the
	// expression — are minijinja-Go's native mapping, enumerated sorted where
	// BAML preserves insertion order, and the representation-agreement check
	// cannot see them (they are identical in both runs).
	for _, name := range []string{
		"list", "join", "first", "map", "select", "reject", "selectattr",
		"rejectattr", "groupby", "chain", "zip", "unique", "batch", "slice",
		"reverse", "pprint", "string", "indent",
	} {
		builtins[name] = guardForeignMapping(name, builtins[name])
	}

	// The number guard, applied to EVERY filter without exception: an
	// out-of-range number may neither go IN nor come OUT.
	for name, builtin := range builtins {
		env.AddFilter(name, guardIntegerResult(name, builtin))
	}

	// The same INPUT guard over EVERY test minijinja-Go registers. Round 12 wrapped
	// only `divisibleby`, so `even`/`odd` — which read their input through AsInt —
	// were reachable with any value at all. Written out rather than iterated
	// because minijinja-Go exports no accessor for its registered tests, and a
	// list that must be complete is safer explicit than inferred; the coverage is
	// asserted behaviourally by TestEveryRegisteredTestIsGuarded.
	//
	// `containing` is deliberately ABSENT: it is a minijinja-contrib test BAML
	// does not build, and [withdrawNonBAMLBuiltins] replaces it with an
	// unknown-test error. Re-registering it here would silently reinstate it —
	// which is exactly what happened on the first attempt, and the live corpus
	// caught it as an UNSAFE row (stock errors, native answered).
	for name, builtin := range map[string]mj.TestFunc{
		"defined": tests.TestDefined, "undefined": tests.TestUndefined,
		"none": tests.TestNone, "true": tests.TestTrue, "false": tests.TestFalse,
		"odd": tests.TestOdd, "even": tests.TestEven,
		"eq": tests.TestEq, "equalto": tests.TestEq, "==": tests.TestEq,
		"ne": tests.TestNe, "!=": tests.TestNe,
		"lt": tests.TestLt, "lessthan": tests.TestLt, "<": tests.TestLt,
		"le": tests.TestLe, "<=": tests.TestLe,
		"gt": tests.TestGt, "greaterthan": tests.TestGt, ">": tests.TestGt,
		"ge": tests.TestGe, ">=": tests.TestGe,
		"in": tests.TestIn, "string": tests.TestString, "number": tests.TestNumber,
		"integer": tests.TestInteger, "int": tests.TestInteger,
		"float": tests.TestFloat, "boolean": tests.TestBoolean,
		"sequence": tests.TestSequence, "mapping": tests.TestMapping,
		"iterable":     tests.TestIterable,
		"startingwith": tests.TestStartingWith, "endingwith": tests.TestEndingWith,
		"safe": tests.TestSafe, "escaped": tests.TestSafe,
		"sameas": tests.TestSameAs, "lower": tests.TestLower, "upper": tests.TestUpper,
		"filter": tests.TestFilter, "test": tests.TestTest,
	} {
		env.AddTest(name, guardTestInput(name, builtin))
	}

	// `divisibleby(0)`: minijinja-Go answers false; stock BAML v0.223 takes the
	// process down (a Rust panic on the CFFI callback thread that a Go caller
	// cannot recover from). Proven by TestStockDivisibleByZeroIsUnobservable.
	env.AddTest("divisibleby", guardTestInput("divisibleby",
		func(state filters.State, val mjvalue.Value, args []mjvalue.Value) (bool, error) {
			if len(args) == 1 {
				if d, ok := args[0].AsInt(); ok && d == 0 {
					return false, unsupportedConstraint("`divisibleby(0)`; stock BAML v0.223 aborts the process on it")
				}
			}
			return tests.TestDivisibleBy(state, val, args)
		}))

	// Global FUNCTIONS can carry integers too, and `range` is the reachable one:
	// `range(...)|last` was the round-5 P1.3 escape, where an out-of-range
	// integer reached an exact comparison. minijinja-Go exports no accessor for
	// its registered globals, so `range` cannot be WRAPPED the way the filters
	// are — and re-implementing it would be the look-alike this slice refuses to
	// build. It is therefore withdrawn from the profile outright, which also
	// removes the unbounded-allocation vector a large range argument would open.
	// `dict` and `namespace` need no guard: every integer they carry comes from a
	// source literal, and those are bounded by everyNumericTokenIsProvablySmall.
	env.AddFunction("range", func(*mj.State, []mjvalue.Value, map[string]mjvalue.Value) (mjvalue.Value, error) {
		return mjvalue.Undefined(), unsupportedConstraint(
			"`range` is outside the profile: minijinja-Go exports no handle on it to guard, so an " +
				"out-of-range integer it produces could reach an exact comparison unchecked")
	})
}

// guardForeignMapping refuses a mapping that the value model did not build.
func guardForeignMapping(name string, builtin mj.FilterFunc) mj.FilterFunc {
	return func(state filters.State, val mjvalue.Value, args []mjvalue.Value, kwargs map[string]mjvalue.Value) (mjvalue.Value, error) {
		if val.Kind() == mjvalue.KindMap && !isOrderedMapping(val) {
			return mjvalue.Undefined(), unsupportedConstraint("`%s` over a mapping literal; minijinja-Go enumerates it sorted, minijinja in insertion order", name)
		}
		return builtin(state, val, args, kwargs)
	}
}

// isOrderedMapping reports whether a mapping came from the value model (and so
// carries BAML's insertion order) rather than from a literal in the expression.
func isOrderedMapping(val mjvalue.Value) bool {
	obj, ok := val.AsObject()
	if !ok {
		return false
	}
	_, ok = obj.(*orderedMapping)
	return ok
}

// containsMapping reports whether val is, or transitively contains, a mapping.
// The depth cap is a cycle backstop; a constraint value is a finite tree, but
// the engine can hand this filter values it did not build.
func containsMapping(val mjvalue.Value, depth int) bool {
	if depth > 32 {
		return true // unknown shape: refuse rather than guess
	}
	switch val.Kind() {
	case mjvalue.KindMap:
		return true
	case mjvalue.KindSeq, mjvalue.KindIterable:
		for _, item := range val.Iter() {
			if containsMapping(item, depth+1) {
				return true
			}
		}
	}
	return false
}

// hasMapping reports whether the native value carries a map or class anywhere,
// i.e. whether the representation-agreement check has anything to check.
func hasMapping(v ConstraintValue) bool {
	switch v.kind {
	case ConstraintKindMap, ConstraintKindClass:
		return true
	case ConstraintKindList:
		for _, item := range v.list {
			if hasMapping(item) {
				return true
			}
		}
	}
	return false
}

// hasMedia reports whether the native value carries a media value anywhere.
//
// Media is REFUSED rather than converted. BAML's two conversions disagree on
// this arm — `Value::from_serialize` (the path evaluate_predicate takes) emits
// the BamlMedia serde document, while `From<BamlValue> for minijinja::Value`
// (the PROMPT renderer's path) wraps it in a magic-marker object — and no
// media value can reach a constraint on the native path to decide between them:
// schema.Bundle.ValidateOutput rejects every media output before parsing
// ("media is not usable as an output type", internal/schema/validate.go:65).
// Shipping an unprovable conversion would be a claim the differential cannot
// back, so the profile excludes media outright and
// TestConstraintMediaIsRefused pins it.
func hasMedia(v ConstraintValue) bool {
	switch v.kind {
	case ConstraintKindMedia:
		return true
	case ConstraintKindList:
		for _, item := range v.list {
			if hasMedia(item) {
				return true
			}
		}
	case ConstraintKindMap, ConstraintKindClass:
		for _, e := range v.entries {
			if hasMedia(e.Value) {
				return true
			}
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// The numeric boundary.
// ---------------------------------------------------------------------------

// maxExactInt is 2^53: the largest magnitude at which an integer survives a
// float64 round trip. It is the boundary of everything below.
const maxExactInt uint64 = 1 << 53

// minijinja-Go performs ALL integer arithmetic in float64 and converts back
// (value/ops.go Add/Sub/Mul/Div/Pow: `if isActualInt(v) && isActualInt(other) {
// return FromInt(int64(f1 - f2)) }`), and Value.Equal / Value.Compare likewise
// compare numbers as float64. minijinja in Rust keeps i64 arithmetic exact.
// Two distinct wrong answers follow, and both were live before this guard:
//
//	9007199254740993 == 9007199254740992     -> native TRUE, BAML false
//	    2^53+1 and 2^53 are the same float64, so the comparison conflates them.
//	    Wrong on EVERY architecture.
//
//	9223372036854775807 - 1 == 9223372036854775806   -> ARCHITECTURE-DEPENDENT
//	    i64::MAX rounds UP to 2^63 as a float64, so the subtraction leaves a
//	    value outside int64's range and `int64(f)` is implementation-defined in
//	    Go: arm64 saturates to i64::MAX, amd64 yields i64::MIN. Measured with one
//	    cross-compiled binary: darwin/arm64 renders "true", linux/amd64 renders
//	    "false", and BAML says true. This is why the differential passed locally
//	    and failed in CI — the same source, the same engine, a different answer.
//
// Neither is reachable by guarding a filter: `-` and `==` are operators. So the
// boundary is enforced BEFORE evaluation, over the whole expression, by bounding
// the magnitude any integer in it could reach.
// exceedsExactIntegerRange reports whether this expression's numeric behaviour
// is outside the proven profile and must be refused.
//
// WHY THIS IS A WHITELIST AND NOT A SCANNER. Rounds 2-5 each tried to bound the
// numerics by SCANNING the expression, and each scanner failed OPEN on a form it
// did not model — hexadecimal and underscored literals, newlines, parenthesised
// operands, negative exponents. A scanner that allows what it does not
// understand cannot be repaired by adding cases to it; the defect is the
// direction of its default.
//
// The faithful alternative — deriving the bound from minijinja-Go's own
// tokenizer and AST — is not available. Its lexer and parser live under
// `github.com/mitsuhiko/minijinja/minijinja-go/v2/internal/...`, and Go's
// visibility rule forbids this package from importing them ("use of internal
// package ... not allowed"); the module exports no AST accessor either. Writing
// a Jinja lexer by hand is precisely what produced the previous holes.
//
// So the profile inverts the default and narrows the claim. Numerics are
// admitted in exactly two shapes, and everything else is refused:
//
//  1. NO ARITHMETIC. With no arithmetic byte anywhere, no operator can
//     manufacture an out-of-range integer. Every integer in play is then a
//     literal (checked below), a value-model integer (checked below), or a
//     producer result — and [guardIntegerResult] wraps EVERY filter and global
//     function, so a producer result is always in range. Comparisons between
//     values all below 2^53 are exact in float64.
//
//  2. ARITHMETIC OVER A CLOSED NUMERIC SUBLANGUAGE. If arithmetic is present,
//     the WHOLE expression must parse as pure numeric arithmetic — literals,
//     operators, parentheses, comparisons, nothing else — via [parseNumeric], a
//     TOTAL parser for that sublanguage. It is not a Jinja lexer: it accepts a
//     closed grammar and rejects every byte outside it, so an identifier,
//     filter, call, string or comment ends the parse and refuses. What it does
//     accept it evaluates on the real operands, so `2 ** (10) == 1024` is
//     admitted while `2 ** -1` is refused for the reason stock refuses it.
//
// Every unrecognised token form maps to a refusal, never to an allowance.
func exceedsExactIntegerRange(this ConstraintValue, expr string) bool {
	// A value-model integer outside the range is unusable whatever the syntax.
	if maxAbsInt(this) >= maxExactInt {
		return true
	}
	// Every numeric token must have a certain magnitude. A run beginning with a
	// digit that is not a plain small decimal or decimal fraction — 0x…, 0b…,
	// 0o…, 1_000, 1e5, or simply too long — is refused rather than guessed at.
	if !everyNumericTokenIsProvablySmall(expr) {
		return true
	}
	// Subscript and slice bounds reach Value.AsInt inside the VM, where nothing
	// can be wrapped. See [bracketBoundsAreProvablySafe].
	if !bracketBoundsAreProvablySafe(expr) {
		return true
	}
	if !containsArithmeticByte(expr) {
		return false
	}
	n, ok := parseNumeric(expr)
	return !ok || n.saturated
}

// containsArithmeticByte reports whether any byte could be an arithmetic
// operator. It deliberately does not skip strings or comments: over-detecting
// costs coverage, under-detecting would cost the guarantee.
func containsArithmeticByte(expr string) bool {
	return strings.ContainsAny(expr, "+-*/%")
}

// bracketBoundsAreProvablySafe covers the last two AsInt consumers, and they are
// the only ones that are neither a filter nor a test: the VM's own subscript and
// slice operators.
//
//	value.Value.GetItem   value.go:1174-1221   `key.AsInt()` per payload arm
//	State.evalSlice       state.go:2919-2939   start, stop AND step
//
// Neither is reachable by wrapping anything. They are executed by the engine
// between evaluating the index expression and using it, and this package cannot
// hook the pinned dependency there. The reachable case needs no hazardous input
// at all — it manufactures the float inside the expression:
//
//	[1,2,3][(("9223372036854" ~ "775808")|float):]|length == 3
//
// Both string tokens are short, so everyNumericTokenIsProvablySmall admits them;
// `~`, `|` and the slice carry none of the `+-*/%` bytes that would invoke the
// numeric sublanguage; and `|float` legitimately returns a non-IsActualInt float,
// which containsInexactInteger passes by design. evalSlice then does
// int64(2^63): MinInt64 on linux/amd64, normalised to a start of 0, so native
// answered TRUE with the full slice. Stock's i64::try_from SATURATES to i64::MAX,
// giving an empty slice and FALSE.
//
// So the bound is enforced STRUCTURALLY, before evaluation, in the same posture
// as the arithmetic gate: inside any `[...]` region, only LITERALS are admitted —
// small decimal integers (optionally negated), string literals, and the `,` and
// `:` separators. An index or bound that is computed, filtered, concatenated,
// fractional or in any way derived is refused, whatever it would have evaluated
// to. That covers GetItem and all three of evalSlice's bounds at once, and it
// covers a list literal in the same sweep because the rule is about the REGION,
// not about deciding which kind of `[` this is — a distinction a hand scanner
// would have to get right, and the one thing rounds 5 and 6 proved it will not.
//
// Fail-closed on anything the scan cannot classify: an unbalanced bracket, a
// nested bracket, a backslash inside a string, or an unterminated string.
func bracketBoundsAreProvablySafe(expr string) bool {
	depth := 0
	for i := 0; i < len(expr); i++ {
		c := expr[i]
		switch {
		case c == '"' || c == '\'':
			// A string literal, inside brackets or out. Consume it here so a `[`
			// in its body is never mistaken for a subscript.
			quote := c
			i++
			for ; i < len(expr) && expr[i] != quote; i++ {
				if expr[i] == '\\' {
					return false // an escape: not something to guess at
				}
			}
			if i >= len(expr) {
				return false // unterminated
			}
		case c == '[':
			depth++
			if depth > 1 {
				return false // a nested bracket region is not analysed
			}
		case c == ']':
			depth--
			if depth < 0 {
				return false
			}
		case depth == 0:
			// Outside brackets everything is somebody else's problem.
		case c >= '0' && c <= '9', c == '.', c == ',', c == ':', c == '-',
			c == ' ', c == '\t', c == '\n', c == '\r', c == '\f', c == '\v':
			// Numeric literals, negation, and the separators. A LITERAL is safe at
			// any of these positions — including a fractional one — because
			// everyNumericTokenIsProvablySmall has already capped every source
			// numeric token at 15 integer digits, well inside the range where both
			// engines convert alike. `.` also lets an ordinary list literal such as
			// `[1,2.5]` through, which is not a subscript at all. The hazard this
			// guard exists for is a DERIVED bound, and every way of deriving one
			// needs a letter, quote, pipe, paren or bracket — all refused below.
		default:
			return false // an identifier, filter, call, operator or attribute
		}
	}
	return depth == 0
}

// maxSmallDigits is 15 because 10^15 - 1 < 2^53, so any accepted integer — and
// any modest combination of them — is exactly representable as a float64.
const maxSmallDigits = 15

// everyNumericTokenIsProvablySmall checks each maximal run of literal-ish
// characters. A run starting with a letter or `_` is an identifier and ignored;
// a run starting with a DIGIT must be `d{1,15}` or `d{1,15}.d{1,15}`.
func everyNumericTokenIsProvablySmall(expr string) bool {
	for i := 0; i < len(expr); {
		if !isNumericTokenByte(expr[i]) {
			i++
			continue
		}
		start := i
		for i < len(expr) && isNumericTokenByte(expr[i]) {
			i++
		}
		run := expr[start:i]
		if run[0] < '0' || run[0] > '9' {
			continue
		}
		if !isProvablySmallNumber(run) {
			return false
		}
	}
	return true
}

func isNumericTokenByte(b byte) bool {
	return b == '_' || b == '.' ||
		(b >= '0' && b <= '9') || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

func isProvablySmallNumber(run string) bool {
	digits, fracDigits := 0, 0
	dot := false
	for i := 0; i < len(run); i++ {
		c := run[i]
		switch {
		case c >= '0' && c <= '9':
			if dot {
				fracDigits++
				if fracDigits > maxSmallDigits {
					return false
				}
			} else {
				digits++
				if digits > maxSmallDigits {
					return false
				}
			}
		case c == '.':
			if dot || digits == 0 || i == len(run)-1 {
				return false
			}
			dot = true
		default:
			return false // a base prefix, digit separator, or exponent
		}
	}
	return digits > 0 && (!dot || fracDigits > 0)
}

// ---------------------------------------------------------------------------
// The closed numeric sublanguage.
// ---------------------------------------------------------------------------

// numeric is a value in the sublanguage: a comparison result, or a number
// classified by the PAYLOAD TYPE minijinja-Go would store it as.
//
// The classification is engineInt, NOT the source spelling, and the difference
// is load-bearing. Rounds 9 and 10 both modelled "written with a `.`" as
// "exempt, because both engines are IEEE-754 f64 from here on", and that premise
// failed twice — first because a float op must not clear an already-crossed
// integer, then because `Pow` and `Rem` PROMOTE an integral float into an actual
// int64. Round 11 removes the premise instead of guarding it again: a node is
// bounded as an integer exactly when the engine holds it as one.
//
// WHY THAT IS SOUND. Every engineInt node carries a magnitude bound, and the
// profile refuses the whole expression if any of them reaches 2^53. Under that
// condition every node has the SAME value in both engines:
//
//   - an engineInt node below 2^53 is exactly representable in f64, so
//     minijinja-Go's `FromInt(int64(f1 op f2))` and stock's i128 agree, and it
//     also equals the f64 stock may be holding instead (stock keeps a promoted
//     power as f64 — same number, different box);
//   - a non-engineInt node is f64 in BOTH engines. minijinja-Go reaches that
//     state only when an operand is a float64 payload, and any float operand
//     puts stock in f64 too. IEEE-754 is deterministic, so no bound is needed —
//     which is why `999999999999999.0 * 999999999999999.0` still decides.
//
// The only bridges between the two worlds are the promoting ops, and each is
// handled where it happens: see [parsePow] and [parseMul].
type numeric struct {
	// engineInt is true when minijinja-Go stores this value as an int64. A float
	// LITERAL is not engineInt — `2.0` is a float64 payload until a promoting op
	// reads it through AsInt.
	engineInt bool
	isBool    bool
	neg       bool
	// mag bounds |value| for an engineInt node. For a float literal it bounds the
	// literal itself, which is what [parsePow] needs to bound a promoted power.
	mag uint64
	// nonNegLiteral marks a node that IS a non-negative integer literal — a bare
	// digit run, possibly wrapped in parentheses. Any operator, and unary minus,
	// clear it. It is the only provenance the operators below will accept,
	// because a computed operand's SIGN cannot be established here and the
	// engines disagree on exactly the signed cases (see [parseMul]/[parsePow]).
	nonNegLiteral bool
	// nonNegFloatLiteral is the same provenance rule for a non-negative FLOAT
	// literal (`2.0`, `0.5`), and mag then carries a CEILING on its magnitude.
	// integralFloatLiteral additionally records whether that literal is what
	// AsInt calls an integer (`2.0` yes, `2.5` no), which decides whether a
	// promoting op turns it into an int64.
	//
	// It exists because "a float operand means both engines stay in f64" is
	// FALSE for this engine: minijinja-Go PROMOTES AN INTEGRAL FLOAT TO AN
	// INTEGER inside two of its numeric ops, and an integer it manufactures that
	// way is subject to the same 2^53 (and int64-conversion) divergence as any
	// other. The census of value/ops.go v2.16.0, one row per numeric op, by the
	// predicate it uses to decide whether to return an integer:
	//
	//	Add       isActualInt(v) && isActualInt(other)   NO promotion
	//	Sub       isActualInt(v) && isActualInt(other)   NO promotion
	//	Mul       isActualInt(v) && isActualInt(other)   NO promotion
	//	Div       always FromFloat                       NO promotion
	//	FloorDiv  isActualInt(v) && isActualInt(other)   NO promotion
	//	Rem       v.AsInt() && other.AsInt()             PROMOTES
	//	Pow       v.AsInt() && other.AsInt() >= 0        PROMOTES
	//	Neg       type switch on float64                 NO promotion
	//
	// isActualInt type-switches on the STORED payload (`_, ok := v.data.(int64)`)
	// so `2.0` is not an actual int; AsInt accepts any integral float64
	// (`d == math.Trunc(d)`) so `2.0` IS an int to it. That difference is the
	// whole hole. Mul has three further AsInt reads, but they are the
	// string/sequence repetition arms and need a string or seq operand, which
	// this sublanguage cannot produce — a string literal or identifier fails the
	// parse.
	//
	// Both promoting ops are therefore handled explicitly: `%` admits only
	// non-negative INTEGER literals (see [parseMul]), so a float can never reach
	// Rem; and `**` bounds a promoted power as an integer and hands it on as one,
	// so the ORDINARY integer machinery covers everything downstream of it (see
	// [parsePow]). That last part is round 11: bounding the power at its own node
	// was not enough, because `(2.0 ** 52) * (2.0 ** 11)` is two in-range
	// promoted integers whose PRODUCT is 2^63.
	nonNegFloatLiteral   bool
	integralFloatLiteral bool
	// saturated is STICKY, and that is the whole point of it.
	//
	// It records that SOME sub-expression already reached or passed 2^53 — where
	// minijinja-Go's float64 integer arithmetic stops agreeing with stock's i128.
	// Once set it must survive every subsequent production, including ones whose
	// own result is a float, because a float operation does not undo the
	// divergence that already happened in the integer prefix:
	//
	//	(1 + k*10 - 1 + 0.0) == (1 + k*10 + 0.0)     k = 999999999999999
	//
	// Stock keeps the prefix in i128 (…990 and …991), and those are still two
	// DISTINCT f64 values at the float handoff, so it answers false. minijinja-Go
	// did each Add through float64, rounding both prefixes to …992, so it answers
	// true. Before round 9 the `+ 0.0` returned a fresh float-typed numeric and
	// dropped the bit, and the whole expression was admitted.
	//
	// THE INVARIANT: every production that returns a numeric ORs IN the saturated
	// bits of every operand it consumed. The census, one entry per construction
	// site (there are no others — `numeric{}, false` sites are parse failures,
	// which refuse):
	//
	//	parseCmp    comparison result   left.saturated || right.saturated
	//	parseAdd    + and -             via combineNumeric
	//	parseMul    *, /, //, %         via combineNumeric
	//	parsePow    integer base        base.saturated || exp.saturated || overflow
	//	parsePow    float base          base.saturated || exp.saturated || overflow
	//	parseUnary  - and +             returns the operand struct, bit intact
	//	parsePrimary  ( expr )          returns the inner struct, bit intact
	//	parsePrimary  literal           leaf: set iff the literal itself is >= 2^53
	//	combineNumeric  bool operand    unconditionally true (refuses)
	//	combineNumeric  float result    a.saturated || b.saturated
	//	combineNumeric  int result      a.saturated || b.saturated || overflow
	//
	// TestSaturationIsStickyUnderRandomArithmetic asserts the consequence rather
	// than the census: over randomised expression trees, whenever the EXACT value
	// of any integer sub-expression crosses 2^53, EvaluateConstraint refuses.
	saturated bool
}

// parseNumeric parses and evaluates:
//
//	expr    := cmp
//	cmp     := add (('=='|'!='|'<='|'>='|'<'|'>') add)*
//	add     := mul (('+'|'-') mul)*
//	mul     := pow (('*'|'//'|'/'|'%') pow)*
//	pow     := unary ('**' pow)?
//	unary   := ('-'|'+') unary | primary
//	primary := NUMBER | '(' expr ')'
//
// It is TOTAL: every byte must be consumed, so any identifier, filter, call,
// string, bracket or comment ends the parse with ok=false. All ASCII whitespace
// separates tokens, so a newline or carriage return cannot change how an
// operator is read — the round-5 P1.2 failure mode.
func parseNumeric(expr string) (numeric, bool) {
	p := &numericParser{src: expr}
	n, ok := p.parseCmp()
	if !ok {
		return numeric{}, false
	}
	p.skipSpace()
	if p.pos != len(p.src) {
		return numeric{}, false
	}
	return n, true
}

type numericParser struct {
	src string
	pos int
}

func (p *numericParser) skipSpace() {
	for p.pos < len(p.src) {
		switch p.src[p.pos] {
		case ' ', '\t', '\n', '\r', '\f', '\v':
			p.pos++
		default:
			return
		}
	}
}

func (p *numericParser) accept(tok string) bool {
	p.skipSpace()
	if strings.HasPrefix(p.src[p.pos:], tok) {
		p.pos += len(tok)
		return true
	}
	return false
}

func (p *numericParser) parseCmp() (numeric, bool) {
	left, ok := p.parseAdd()
	if !ok {
		return numeric{}, false
	}
	for {
		matched := false
		for _, op := range []string{"==", "!=", "<=", ">=", "<", ">"} {
			if p.accept(op) {
				right, ok := p.parseAdd()
				if !ok {
					return numeric{}, false
				}
				left = numeric{isBool: true, saturated: left.saturated || right.saturated}
				matched = true
				break
			}
		}
		if !matched {
			return left, true
		}
	}
}

func (p *numericParser) parseAdd() (numeric, bool) {
	left, ok := p.parseMul()
	if !ok {
		return numeric{}, false
	}
	for {
		switch {
		case p.acceptSingle('+'):
			right, ok := p.parseMul()
			if !ok {
				return numeric{}, false
			}
			left = combineNumeric(left, right, satAdd)
		case p.acceptSingle('-'):
			right, ok := p.parseMul()
			if !ok {
				return numeric{}, false
			}
			// SUBTRACTION USES THE SUM BOUND, NOT max(|a|, |b|).
			//
			// max is only an upper bound when the operands share a sign.
			// Subtracting a NEGATIVE grows the magnitude by the sum:
			// a - (-k) == a + k. A chain of ten `- (-999999999999999)` keeps a
			// max-bound at 999999999999999 while the true value passes 2^53,
			// where minijinja-Go's float64 Sub collapses two distinct integers
			// onto one and stock's i128 keeps them apart — a usable wrong
			// boolean. The sign IS tracked syntactically, but a chain's signs
			// cannot be established without evaluating it, so the bound stays
			// conservative rather than clever: |a| + |b| holds whatever the
			// signs are.
			left = combineNumeric(left, right, satAdd)
		default:
			return left, true
		}
	}
}

func (p *numericParser) acceptSingle(op byte) bool {
	p.skipSpace()
	if p.pos < len(p.src) && p.src[p.pos] == op {
		p.pos++
		return true
	}
	return false
}

func (p *numericParser) parseMul() (numeric, bool) {
	left, ok := p.parsePow()
	if !ok {
		return numeric{}, false
	}
	for {
		switch {
		case p.acceptMul():
			right, ok := p.parsePow()
			if !ok {
				return numeric{}, false
			}
			left = combineNumeric(left, right, satMul)
		case p.accept("//"), p.accept("%"):
			// SIGNED `//` and `%` DIVERGE. minijinja-Go's Value.Rem is Go's
			// truncated `%` (sign follows the dividend) and its Value.FloorDiv is
			// math.Floor(a/b); stock v2.16 uses checked_rem_euclid and
			// checked_div_euclid, which are EUCLIDEAN. So `-1 % 2` is -1 here and
			// 1 there, and `1 // -2` is -1 here and 0 there. Only non-negative
			// literal operands are proven identical, and a computed operand's sign
			// is not something this parser will try to establish.
			//
			// The same rule is ALSO what keeps `Rem`'s integral-float promotion
			// out of reach. Rem is the other op that reads its operands through
			// AsInt (`if i1, ok := v.AsInt(); ok { if i2, ok := other.AsInt() …
			// return FromInt(i1 % i2) }`), so `1000000000000000.0 * 1000.0 % 3`
			// would hand int64() a float past 2^63. nonNegLiteral is an INTEGER
			// literal — a float literal never sets it — so no float operand, and
			// no computed operand, can reach either operator. See [parsePow] for
			// the promotion this profile has to model rather than exclude.
			right, ok := p.parsePow()
			if !ok {
				return numeric{}, false
			}
			if !left.nonNegLiteral || !right.nonNegLiteral {
				return numeric{}, false
			}
			left = combineNumeric(left, right, func(a, _ uint64) uint64 { return a })
		case p.accept("/"):
			// True division is f64 on both sides, so sign is immaterial — and
			// minijinja-Go's Value.Div returns FromFloat UNCONDITIONALLY, even for
			// two actual ints, so the result is never an engine integer no matter
			// what went in. Saturation still carries.
			right, ok := p.parsePow()
			if !ok {
				return numeric{}, false
			}
			if left.isBool || right.isBool {
				return numeric{}, false
			}
			left = numeric{saturated: left.saturated || right.saturated}
		default:
			return left, true
		}
	}
}

// acceptMul accepts a single `*` only when it does not begin `**`.
func (p *numericParser) acceptMul() bool {
	p.skipSpace()
	if strings.HasPrefix(p.src[p.pos:], "**") {
		return false
	}
	if p.pos < len(p.src) && p.src[p.pos] == '*' {
		p.pos++
		return true
	}
	return false
}

func (p *numericParser) parsePow() (numeric, bool) {
	base, ok := p.parseUnary()
	if !ok {
		return numeric{}, false
	}
	if !p.accept("**") {
		return base, true
	}
	exp, ok := p.parsePow() // right-associative
	if !ok {
		return numeric{}, false
	}
	if base.isBool || exp.isBool {
		return numeric{}, false
	}
	// Stock converts an integer exponent to u32 and ERRORS if it does not fit,
	// so a negative, oversized or non-integer exponent must be refused rather
	// than evaluated: minijinja-Go calls math.Pow and ANSWERS where stock
	// rejects — `2 ** -1` yields 0.5 here and an error there.
	//
	// The exponent must therefore be a non-negative integer LITERAL. Tracking a
	// `neg` flag through unary syntax is not enough: `2 ** (0 - 1)` computes -1
	// with no unary minus anywhere, and no magnitude bound can reveal its sign.
	// Rather than try to prove the sign of a computed operand, the profile only
	// accepts an exponent whose non-negativity is manifest.
	if !exp.nonNegLiteral || exp.mag >= powExponentLimit {
		return numeric{}, false
	}
	if !base.engineInt {
		// A FLOAT BASE IS NOT EXEMPT: `Pow` PROMOTES AN INTEGRAL FLOAT TO AN
		// INTEGER.
		//
		// minijinja-Go's Value.Pow computes math.Pow in f64 and then tries to
		// hand back an INTEGER: `if _, ok1 := v.AsInt(); ok1 { if i2, ok2 :=
		// other.AsInt(); ok2 && i2 >= 0 { if result == math.Trunc(result) &&
		// result <= math.MaxInt64 … return FromInt(int64(result)) } }`. AsInt
		// ACCEPTS an integral float64 — `2.0` is `(2, true)` — so `2.0 ** 63`
		// takes that branch. math.MaxInt64 as an untyped constant rounds UP to
		// 2^63 in the f64 comparison, so the guard passes at exactly 2^63 and
		// int64(float64(2^63)) is the same invalid conversion that produced the
		// round-3 op_i64max failure: MinInt64 on linux/amd64, saturated on arm64.
		// The predicate `2.0 ** 63 > 0.0` is then FALSE natively, while stock
		// coerces the float base to F64, calls powf and answers TRUE.
		//
		// So the base is bounded exactly like an integer one, and — like the
		// exponent — only a MANIFEST non-negative literal is accepted, because
		// the magnitude of a COMPUTED float cannot be bounded here: `/` by a
		// fraction grows it without limit (`2.0 / 0.0000000000001`), and this
		// profile does not evaluate.
		if !base.nonNegFloatLiteral {
			return numeric{}, false
		}
		if !base.integralFloatLiteral {
			// AsInt REJECTS a non-integral float (`2.5` is not math.Trunc(2.5)),
			// so neither engine leaves f64 and no integer bound applies. STICKY:
			// the saturation bit still carries. See [numeric.saturated].
			return numeric{saturated: base.saturated || exp.saturated}, true
		}
		// PROMOTED. The engine hands back an int64, so this node IS an integer
		// from here on — bounded now, and, crucially, handed downstream as
		// engineInt so that a LATER integer op is bounded too. Round 10 bounded
		// only this node, which left `(2.0 ** 52) * (2.0 ** 11)` — two in-range
		// promoted integers whose product is 2^63 — admitted.
		m := satPow(base.mag, exp.mag)
		return numeric{engineInt: true, mag: m,
			saturated: base.saturated || exp.saturated || m >= maxExactInt}, true
	}
	m := satPow(base.mag, exp.mag)
	return numeric{engineInt: true, mag: m,
		saturated: base.saturated || exp.saturated || m >= maxExactInt}, true
}

func (p *numericParser) parseUnary() (numeric, bool) {
	if p.accept("-") {
		n, ok := p.parseUnary()
		if !ok {
			return numeric{}, false
		}
		n.neg = !n.neg
		// Negated: no longer a NON-NEGATIVE literal, in either flavour.
		n.nonNegLiteral = false
		n.nonNegFloatLiteral = false
		return n, true
	}
	if p.accept("+") {
		return p.parseUnary()
	}
	return p.parsePrimary()
}

func (p *numericParser) parsePrimary() (numeric, bool) {
	if p.accept("(") {
		n, ok := p.parseCmp()
		if !ok || !p.accept(")") {
			return numeric{}, false
		}
		// Parentheses are transparent: `(10)` is still a literal for the
		// operators that insist on one.
		return n, true
	}
	p.skipSpace()
	start := p.pos
	for p.pos < len(p.src) && isNumericTokenByte(p.src[p.pos]) {
		p.pos++
	}
	if p.pos == start {
		return numeric{}, false
	}
	run := p.src[start:p.pos]
	if !isProvablySmallNumber(run) {
		return numeric{}, false
	}
	if strings.IndexByte(run, '.') >= 0 {
		// A float LITERAL is a float64 payload in the engine, so it is not
		// engineInt — but it carries a bound on its magnitude, because it stops
		// being exempt the moment a promoting op reads it (see [parsePow]).
		//
		// THE CLASSIFICATION IS THE PARSED f64, NOT THE SOURCE TEXT. Round 11
		// asked whether the written fraction was all zeros, which is not the
		// engine's question. minijinja-Go's lexer parses a float literal with
		// strconv.ParseFloat(.., 64) and re-emits it as
		// FormatFloat(v, 'f', -1, 64) — the shortest form that round-trips — which
		// the parser re-parses, so the composition is the IDENTITY on the float64
		// and the payload is exactly ParseFloat(run, 64). Value.AsInt then promotes
		// on `d == math.Trunc(d)` alone, with no range test of its own.
		//
		// The gap that opens between those two questions is real and reachable:
		//
		//	(562949953421312.000000000000001 ** 1) * 16384 > 0.0
		//
		// 562949953421312 is 2^49, where an f64 ULP is 0.125, so the 15-digit tail
		// rounds away and the payload is EXACTLY 2^49. Pow promotes it, `* 16384`
		// takes Value.Mul's actual-int branch at 2^63 — MinInt64 on linux/amd64 —
		// and native answered false where stock, holding a syntactic float in F64,
		// answers true. Asking ParseFloat closes that by construction: whatever the
		// engine will hold is what gets classified.
		payload, err := strconv.ParseFloat(run, 64)
		if err != nil || math.IsInf(payload, 0) || math.IsNaN(payload) {
			return numeric{}, false
		}
		integral := payload == math.Trunc(payload) // Value.AsInt's predicate, verbatim
		abs := math.Abs(payload)
		var mag uint64
		switch {
		case abs >= float64(maxExactInt):
			// Past the exactness boundary before any operator runs. Saturate rather
			// than compute a bound, so a promotion of it can only ever refuse.
			// (isProvablySmallNumber caps the integer part at 15 digits, so this is
			// unreachable today; it is here so the bound does not depend on that.)
			mag = math.MaxUint64
		case integral:
			mag = uint64(abs)
		default:
			mag = uint64(math.Ceil(abs))
		}
		return numeric{nonNegFloatLiteral: true, integralFloatLiteral: integral, mag: mag}, true
	}
	var mag uint64
	for i := 0; i < len(run); i++ {
		mag = satMul(mag, 10)
		mag = satAdd(mag, uint64(run[i]-'0'))
	}
	return numeric{engineInt: true, mag: mag, nonNegLiteral: true, saturated: mag >= maxExactInt}, true
}

// combineNumeric applies an integer magnitude rule, propagating float-ness
// (mixed int/float arithmetic yields a float in both engines) and saturation. A
// comparison result reaching arithmetic is refused.
func combineNumeric(a, b numeric, mag func(x, y uint64) uint64) numeric {
	if a.isBool || b.isBool {
		return numeric{saturated: true}
	}
	if !a.engineInt || !b.engineInt {
		// The engine keeps this in float64 — Add/Sub/Mul/FloorDiv all gate their
		// integer result on isActualInt, which is false for a float64 payload —
		// and so does stock, because any float operand puts it in f64. IEEE-754
		// is deterministic across the two, so no magnitude bound applies here.
		//
		// STICKY: a mixed operation must still NOT launder an operand that
		// already crossed 2^53. See [numeric.saturated].
		return numeric{saturated: a.saturated || b.saturated}
	}
	// BOTH operands are int64 in the engine, whatever they were SPELLED as — a
	// promoted `2.0 ** 52` arrives here as an integer of magnitude 2^52, and its
	// product with `2.0 ** 11` saturates exactly as `4503599627370496 * 2048`
	// would. That is the round-11 fix: one bound, reached through the payload
	// type rather than through the source text.
	m := mag(a.mag, b.mag)
	// A computed value is never a literal, so it can never satisfy the
	// operators that require one. This is what closes `2 ** (0 - 1)`.
	return numeric{engineInt: true, mag: m, saturated: a.saturated || b.saturated || m >= maxExactInt}
}

// powExponentLimit is where stock stops: minijinja converts a `**` exponent to
// u32 and errors if it does not fit.
const powExponentLimit uint64 = 1 << 32

// ---------------------------------------------------------------------------
// Runtime integer producers.
// ---------------------------------------------------------------------------

// guardIntegerResult wraps a filter so it can never hand back an integer
// outside the exactly-representable range.
//
// [exceedsExactIntegerRange] bounds the integers VISIBLE before evaluation —
// value-model integers and source literals. It cannot see one that is
// MANUFACTURED during evaluation, and there is a reachable way to do that:
//
//	"9007199254740993"|int == "9007199254740992"|int
//
// minijinja-Go's FilterInt parses each string with strconv.ParseInt(s, 10, 64),
// producing two distinct int64s, and Value.Equal then compares them through
// AsFloat, where both collapse to the same float64 — native true. minijinja's
// `int` parses as i128 and compares exactly — stock false. No literal and no
// value-model integer is large here, so the static bound sees nothing.
//
// The fix is to check at the point of production rather than to predict it:
// every filter that can manufacture or carry an integer runs through this
// wrapper, and a result containing an out-of-range integer becomes
// ErrConstraintUnsupported instead of a value. That closes the same hole for a
// string-valued `this` (`this|int`), for elements reached through `map("int")`,
// and for any other producer, because the check is on the VALUE that comes back
// rather than on the syntax that produced it.
func guardIntegerResult(name string, builtin mj.FilterFunc) mj.FilterFunc {
	return func(state filters.State, val mjvalue.Value, args []mjvalue.Value, kwargs map[string]mjvalue.Value) (mjvalue.Value, error) {
		// INPUT half (round 13). A filter may read its subject or an argument
		// through AsInt — `|abs`, `|int`, `|format("%d")`, `|batch(n)` and
		// `|round(n)` all do — and AsInt's float arm is int64(d), which diverges
		// from stock's saturating i128 conversion outside the proven range. The
		// check is on the VALUE rather than on the filter's identity, so a builtin
		// nobody enumerated is covered on the same terms.
		if containsAsIntHazard(val, 0) {
			return mjvalue.Undefined(), asIntHazardError(name)
		}
		for _, a := range args {
			if containsAsIntHazard(a, 0) {
				return mjvalue.Undefined(), asIntHazardError(name)
			}
		}
		for _, a := range kwargs {
			if containsAsIntHazard(a, 0) {
				return mjvalue.Undefined(), asIntHazardError(name)
			}
		}
		out, err := builtin(state, val, args, kwargs)
		if err != nil {
			return out, err
		}
		if containsInexactInteger(out, 0) {
			return mjvalue.Undefined(), unsupportedConstraint(
				"`%s` produced an integer at or past 2^53; minijinja-Go compares integers as float64, "+
					"so the result would be indistinguishable from its neighbour", name)
		}
		return out, nil
	}
}

// guardTestInput is the same INPUT check for a TEST. Tests return a bool rather
// than a value, so [guardIntegerResult]'s output half has nothing to inspect —
// the divergence is entirely in what they read. `even` and `odd` are the
// reachable ones (both `val.AsInt()`), `divisibleby` AsInts its argument too,
// and `integer`/`int` AsInt before consulting IsActualInt; the guard is applied
// to EVERY registered test regardless, so the safety does not depend on that
// enumeration staying right.
func guardTestInput(name string, builtin mj.TestFunc) mj.TestFunc {
	return func(state filters.State, val mjvalue.Value, args []mjvalue.Value) (bool, error) {
		if containsAsIntHazard(val, 0) {
			return false, asIntHazardError("is " + name)
		}
		for _, a := range args {
			if containsAsIntHazard(a, 0) {
				return false, asIntHazardError("is " + name)
			}
		}
		return builtin(state, val, args)
	}
}

func asIntHazardError(name string) error {
	return unsupportedConstraint(
		"`%s` was handed a number outside the range where the two engines convert alike: "+
			"minijinja-Go reads a value through AsInt, whose float arm is int64(d), while stock's "+
			"minijinja fork converts F64 to i128 only when `(val as i64 as f64) == val` and Rust "+
			"SATURATES instead of wrapping. Below 2^53 both produce the same integer; at or past it "+
			"this profile refuses rather than guess at the boundary", name)
}

// containsInexactInteger reports whether a value is, or contains, an integer
// whose magnitude has left the exactly-representable range. The recursion
// covers sequences, so a filter that returns a LIST of manufactured integers is
// caught as readily as one that returns a scalar.
func containsInexactInteger(v mjvalue.Value, depth int) bool {
	if depth > 32 {
		return true // unknown shape: refuse rather than guess
	}
	switch v.Kind() {
	case mjvalue.KindNumber:
		if !v.IsActualInt() {
			return false // floats are f64 on both sides
		}
		n, ok := v.AsInt()
		if !ok {
			return true
		}
		return absInt64(n) >= maxExactInt
	case mjvalue.KindSeq, mjvalue.KindIterable:
		for _, item := range v.Iter() {
			if containsInexactInteger(item, depth+1) {
				return true
			}
		}
	}
	return false
}

func absInt64(n int64) uint64 {
	if n < 0 {
		// -(-1<<63) overflows int64; convert through uint64 instead.
		return uint64(-(n + 1)) + 1
	}
	return uint64(n)
}

// floatIntConversionMagnitude bounds what a float would become if a builtin read
// it through AsInt, saturating at the point the profile stops proving. See
// [maxAbsInt] for why floats are not exempt.
func floatIntConversionMagnitude(f float64) uint64 {
	abs := math.Abs(f)
	if math.IsNaN(f) || abs >= float64(maxExactInt) {
		return maxExactInt
	}
	return uint64(abs)
}

// containsAsIntHazard reports whether a value is, or contains, a number that a
// builtin reading it through AsInt could convert differently in the two
// engines. It is the INPUT counterpart of [containsInexactInteger], which
// guards what a filter RETURNS.
//
// Both halves matter, and they are not the same check: an integer at or past
// 2^53 is indistinguishable from its neighbour once minijinja-Go compares it as
// float64, while a FLOAT at that magnitude is the AsInt hazard from round 13.
func containsAsIntHazard(v mjvalue.Value, depth int) bool {
	if depth > 32 {
		return true // unknown shape: refuse rather than guess
	}
	switch v.Kind() {
	case mjvalue.KindNumber:
		if v.IsActualInt() {
			n, ok := v.AsInt()
			return !ok || absInt64(n) >= maxExactInt
		}
		f, ok := v.AsFloat()
		return !ok || math.IsNaN(f) || math.Abs(f) >= float64(maxExactInt)
	case mjvalue.KindSeq, mjvalue.KindIterable:
		for _, item := range v.Iter() {
			if containsAsIntHazard(item, depth+1) {
				return true
			}
		}
	}
	return false
}

// maxAbsInt is the largest magnitude anywhere in a constraint value that could
// reach an int64 conversion in either engine.
//
// FLOATS COUNT. They were skipped until round 13, on the reasoning that an f64
// is an f64 on both sides — true of arithmetic, false of the builtins that read
// a value through AsInt. `this is even` over FloatValue(2^63) has no source
// token to bound and no arithmetic to inspect, so nothing else in the profile
// looked at it: minijinja-Go's TestEven calls AsInt, whose float arm is
// int64(d) after d == math.Trunc(d), and int64(float64(2^63)) is MinInt64 on
// linux/amd64 — even, so native said TRUE. Stock's boundaryml/minijinja fork
// converts F64 to i128 when `(val as i64 as f64) == val`, and Rust SATURATES
// 2^63 to i64::MAX, which is odd — so stock said FALSE. Opposite booleans, no
// sentinel.
//
// A float therefore contributes its own magnitude, saturated: below 2^53 an
// integral float converts to the same int64 in both engines and a non-integral
// one is rejected by both, so those are proven; at or past 2^53 this profile
// does not try to prove the int64/i128/saturation boundary and refuses.
// Non-finite is refused too, because math.Trunc(Inf) == Inf makes AsInt accept
// it.
func maxAbsInt(v ConstraintValue) uint64 {
	switch v.kind {
	case ConstraintKindInt:
		return absInt64(v.i)
	case ConstraintKindFloat:
		return floatIntConversionMagnitude(v.f)
	case ConstraintKindList:
		var m uint64
		for _, item := range v.list {
			if n := maxAbsInt(item); n > m {
				m = n
			}
		}
		return m
	case ConstraintKindMap, ConstraintKindClass:
		var m uint64
		for _, e := range v.entries {
			if n := maxAbsInt(e.Value); n > m {
				m = n
			}
		}
		return m
	}
	return 0
}

// Saturating helpers. Everything saturates at maxExactInt, which is the point
// at which the answer is refused anyway.
func satAdd(a, b uint64) uint64 {
	if a > maxExactInt-b {
		return maxExactInt
	}
	return a + b
}

func satMul(a, b uint64) uint64 {
	if a == 0 || b == 0 {
		return 0
	}
	if a > maxExactInt/b {
		return maxExactInt
	}
	return a * b
}

// satPow is exponentiation by squaring, saturating at maxExactInt.
//
// The linear version it replaces could not terminate in reasonable time: for
// `1 ** 9007199254740991` it looped roughly nine quadrillion times BEFORE the
// template was compiled, because multiplying by 1 never saturates. Bases 0 and 1
// are answered directly, and for any base >= 2 an exponent of 64 already exceeds
// 2^53, so the loop is bounded by ~6 iterations.
func satPow(base, exp uint64) uint64 {
	switch base {
	case 0:
		if exp == 0 {
			return 1
		}
		return 0
	case 1:
		return 1
	}
	if exp >= 64 {
		return maxExactInt // base >= 2, so the result is at least 2^64
	}
	result := uint64(1)
	for exp > 0 {
		if exp&1 == 1 {
			result = satMul(result, base)
			if result >= maxExactInt {
				return maxExactInt
			}
		}
		exp >>= 1
		if exp == 0 {
			break
		}
		base = satMul(base, base)
		if base >= maxExactInt {
			return maxExactInt
		}
	}
	return result
}
