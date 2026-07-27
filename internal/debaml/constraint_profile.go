package debaml

import (
	"errors"
	"fmt"

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
	// `length` / `count`: minijinja raises "cannot calculate length of value of
	// type none"; minijinja-Go's FilterLength returns 0 for anything with no
	// length (filters.go), so `none|length == 0` is true here and an error
	// there. Refuse exactly the inputs that have no length.
	guardLength := func(state filters.State, val mjvalue.Value, args []mjvalue.Value, kwargs map[string]mjvalue.Value) (mjvalue.Value, error) {
		if _, ok := val.Len(); !ok {
			return mjvalue.Undefined(), unsupportedConstraint("length of a value with no length (kind %s); minijinja rejects it", val.Kind())
		}
		return filters.FilterLength(state, val, args, kwargs)
	}
	env.AddFilter("length", guardLength)
	env.AddFilter("count", guardLength)

	// `split`: minijinja returns a LAZY ITERATOR with no length, so
	// `|split(",")|length` raises "cannot calculate length of value of type
	// iterator"; minijinja-Go returns a materialised list, so it answers. The
	// difference leaks into length, indexing and equality on the result, so the
	// filter itself is outside the profile rather than any one consumer of it.
	env.AddFilter("split", func(filters.State, mjvalue.Value, []mjvalue.Value, map[string]mjvalue.Value) (mjvalue.Value, error) {
		return mjvalue.Undefined(), unsupportedConstraint("`split` returns a lazy iterator in minijinja and a list in minijinja-Go")
	})

	// `last`: minijinja rejects a mapping; minijinja-Go iterates the object and
	// returns its final key.
	env.AddFilter("last", func(state filters.State, val mjvalue.Value, args []mjvalue.Value, kwargs map[string]mjvalue.Value) (mjvalue.Value, error) {
		if val.Kind() == mjvalue.KindMap {
			return mjvalue.Undefined(), unsupportedConstraint("`last` over a mapping; minijinja rejects it")
		}
		return filters.FilterLast(state, val, args, kwargs)
	})

	// `items`: minijinja-Go's FilterItems reaches the mapping through
	// value.AsMap — an unordered Go map — and then SORTS, so BAML's insertion
	// order is lost even for the ordered projection. Unrecoverable for a mapping
	// literal built inside the expression, so no mapping input is admitted.
	env.AddFilter("items", func(state filters.State, val mjvalue.Value, args []mjvalue.Value, kwargs map[string]mjvalue.Value) (mjvalue.Value, error) {
		if val.Kind() == mjvalue.KindMap {
			return mjvalue.Undefined(), unsupportedConstraint("`items` over a mapping; minijinja-Go sorts the keys, minijinja preserves insertion order")
		}
		return filters.FilterItems(state, val, args, kwargs)
	})

	// `tojson`: same sorted-vs-insertion-order loss, at any depth, because the
	// rendered document embeds the order.
	env.AddFilter("tojson", func(state filters.State, val mjvalue.Value, args []mjvalue.Value, kwargs map[string]mjvalue.Value) (mjvalue.Value, error) {
		if containsMapping(val, 0) {
			return mjvalue.Undefined(), unsupportedConstraint("`tojson` over a value containing a mapping; key order differs")
		}
		return filters.FilterTojson(state, val, args, kwargs)
	})

	// `divisibleby(0)`: minijinja-Go answers false; stock BAML v0.223 PANICS THE
	// PROCESS ("attempt to calculate the remainder with a divisor of zero", a
	// Rust panic on the CFFI callback thread that a Go caller cannot recover
	// from). Answering where the oracle cannot even be observed is exactly what
	// the profile forbids — and refusing it also keeps the guard honest about a
	// stock defect. Proven by TestStockDivisibleByZeroIsProcessFatal.
	env.AddTest("divisibleby", func(state filters.State, val mjvalue.Value, args []mjvalue.Value) (bool, error) {
		if len(args) == 1 {
			if d, ok := args[0].AsInt(); ok && d == 0 {
				return false, unsupportedConstraint("`divisibleby(0)`; stock BAML v0.223 aborts the process on it")
			}
		}
		return tests.TestDivisibleBy(state, val, args)
	})

	// Every filter that can MANUFACTURE or CARRY an integer runs through the
	// integer-result guard, so no out-of-range integer can enter the expression
	// from a filter — see [guardIntegerResult] for the reachable `int` case that
	// made this necessary. `float` is absent deliberately: it produces a float,
	// and float semantics already agree between the engines.
	for name, builtin := range map[string]mj.FilterFunc{
		"int":   filters.FilterInt,
		"abs":   filters.FilterAbs,
		"round": filters.FilterRound,
		"min":   filters.FilterMin,
		"max":   filters.FilterMax,
		"attr":  filters.FilterAttr,
	} {
		env.AddFilter(name, guardIntegerResult(name, builtin))
	}
	// `sum` is BAML's own (filterSum), not a builtin, so it is wrapped in place.
	env.AddFilter("sum", guardIntegerResult("sum", filterSum))

	// Foreign mappings — a `{...}` literal or `dict(...)` built INSIDE the
	// expression — are minijinja-Go's native mapping, which enumerates sorted
	// where BAML preserves insertion order. The representation-agreement check
	// in [renderConstraint] cannot see them (they are identical in both runs),
	// so every filter whose output depends on the input's iteration order
	// refuses them explicitly.
	//
	// Filters absent from this list are order-insensitive over a mapping
	// (`sort`, `dictsort`, `min`, `max` all order their own output; `attr`,
	// `default`, `bool` ignore order; `sum` already rejects a mapping as "not
	// iterable" in both engines).
	// The builtins are referenced from the filters package rather than read back
	// out of the environment: minijinja-Go has no Environment filter getter, and
	// the getter reachable from inside a filter (filters.State.GetFilter) would
	// return the wrapper and recurse.
	for name, builtin := range map[string]mj.FilterFunc{
		"list":       filters.FilterList,
		"join":       filters.FilterJoin,
		"first":      filters.FilterFirst,
		"map":        filters.FilterMap,
		"select":     filters.FilterSelect,
		"reject":     filters.FilterReject,
		"selectattr": filters.FilterSelectAttr,
		"rejectattr": filters.FilterRejectAttr,
		"groupby":    filters.FilterGroupBy,
		"chain":      filters.FilterChain,
		"zip":        filters.FilterZip,
		"unique":     filters.FilterUnique,
		"batch":      filters.FilterBatch,
		"slice":      filters.FilterSlice,
		"reverse":    filters.FilterReverse,
		"pprint":     filters.FilterPprint,
		"string":     filters.FilterString,
		"indent":     filters.FilterIndent,
	} {
		env.AddFilter(name, guardIntegerResult(name, guardForeignMapping(name, builtin)))
	}
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

// exceedsExactIntegerRange reports whether the expression could involve an
// integer outside the exactly-representable range, and must therefore be
// refused.
//
// The engine exposes no intermediates, so the bound is STATIC. It has two
// modes, and the distinction matters because a crude bound is wrong in both
// directions — it lets runtime-produced integers through and it refuses
// arithmetic that is provably exact:
//
//   - OPERAND-AWARE. When the expression contains exactly ONE
//     magnitude-growing operator and both of its operands are integer
//     literals, the operation is evaluated exactly (saturating) and that IS the
//     bound. `2 ** 10 == 1024` is 1024, not the 10^10 a global-maximum estimate
//     would guess, so it is admitted and stays in live agreement with stock.
//     The single-operator condition is what makes this sound: with a chain, an
//     operand is another operation's result rather than the literal beside it
//     (`2 ** 10 ** 3` is 2^1000, not 1024), so the exact reading no longer
//     bounds anything.
//
//   - PESSIMISTIC otherwise. Start from the largest integer the expression can
//     see and apply each growing operator to a running bound. With k
//     multiplications over leaves of magnitude <= M the true maximum is
//     M^(k+1), which is exactly what repeated `bound * M` yields; likewise
//     (k+1)*M for additions. Sound, and deliberately loose.
//
// Only `**`, `*`, `+` and `-` grow magnitude: `/` produces a float (identical
// f64 semantics in both engines) and `//` and `%` shrink. Floats are exempt
// throughout.
//
// This covers integers that are VISIBLE before evaluation. Integers
// manufactured DURING evaluation — `"9007199254740993"|int` is the reachable
// example — are invisible here by construction, and are caught at the point of
// production instead by [guardIntegerResult].
func exceedsExactIntegerRange(this ConstraintValue, expr string) bool {
	m := maxAbsInt(this)
	src := scanIntegerSource(expr)
	if src.maxLiteral > m {
		m = src.maxLiteral
	}
	if m == 0 && src.growOps() == 0 {
		return false
	}
	if m >= maxExactInt {
		return true
	}

	// Operand-aware: one growing operator, both operands read as integer
	// literals. The operation cannot produce more than its own exact result.
	if src.growOps() == 1 && src.soleOpExact {
		bound := src.soleOpResult
		if m > bound {
			bound = m
		}
		return bound >= maxExactInt
	}

	bound := m
	for i := 0; i < src.pow && bound < maxExactInt; i++ {
		bound = satPow(bound, m)
	}
	for i := 0; i < src.mul && bound < maxExactInt; i++ {
		bound = satMul(bound, m)
	}
	for i := 0; i < src.addSub && bound < maxExactInt; i++ {
		bound = satAdd(bound, m)
	}
	return bound >= maxExactInt
}

// maxAbsInt is the largest integer magnitude anywhere in a constraint value.
// Floats are skipped: they are f64 on both sides already.
func maxAbsInt(v ConstraintValue) uint64 {
	switch v.kind {
	case ConstraintKindInt:
		if v.i < 0 {
			// -(-1<<63) overflows int64; convert through uint64 instead.
			return uint64(-(v.i + 1)) + 1
		}
		return uint64(v.i)
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

// sourceScan is what a lexical pass over the expression can establish.
type sourceScan struct {
	maxLiteral       uint64
	pow, mul, addSub int
	soleOpExact      bool   // the one growing operator had two integer-literal operands
	soleOpResult     uint64 // its exact (saturating) result
}

func (s sourceScan) growOps() int { return s.pow + s.mul + s.addSub }

// scanIntegerSource finds the largest INTEGER literal in the expression, counts
// the magnitude-growing operators, and — when there is exactly one — reads its
// operands so the bound can be exact rather than estimated.
//
// This is a lexical scan, not a parse: minijinja-Go's parser is under internal/
// and cannot be imported. That is sound in this direction, because the scan can
// only over-count operators and over-estimate literals, and both push towards
// refusing. The operand reading is the one place that could push the other way,
// which is why it is used ONLY when it is the sole operator.
func scanIntegerSource(expr string) sourceScan {
	var out sourceScan
	type opSite struct {
		kind  byte // '^' pow, '*', '+', '-'
		index int  // index of the operator
		width int
	}
	var sites []opSite

	for i := 0; i < len(expr); {
		c := expr[i]
		switch {
		case c == '"' || c == '\'':
			i = skipStringLiteral(expr, i)
		case c >= '0' && c <= '9' && !isIdentByte(prevByte(expr, i)):
			var n uint64
			n, i = scanNumericLiteral(expr, i)
			if n > out.maxLiteral {
				out.maxLiteral = n
			}
		case c == '*':
			if i+1 < len(expr) && expr[i+1] == '*' {
				out.pow++
				sites = append(sites, opSite{'^', i, 2})
				i += 2
			} else {
				out.mul++
				sites = append(sites, opSite{'*', i, 1})
				i++
			}
		case c == '+' || c == '-':
			out.addSub++
			sites = append(sites, opSite{c, i, 1})
			i++
		default:
			i++
		}
	}

	if len(sites) == 1 {
		if a, b, ok := literalOperands(expr, sites[0].index, sites[0].width); ok {
			out.soleOpExact = true
			switch sites[0].kind {
			case '^':
				out.soleOpResult = satPow(a, b)
			case '*':
				out.soleOpResult = satMul(a, b)
			case '+':
				out.soleOpResult = satAdd(a, b)
			case '-':
				// Magnitude of a difference is bounded by the larger operand.
				out.soleOpResult = a
				if b > a {
					out.soleOpResult = b
				}
			}
		}
	}
	return out
}

// literalOperands reads the integer literals immediately either side of an
// operator, skipping only whitespace. Anything else — a parenthesis, an
// identifier, a float, a string — means the operand is not a literal and the
// exact reading does not apply.
func literalOperands(expr string, opIndex, opWidth int) (left, right uint64, ok bool) {
	i := opIndex - 1
	for i >= 0 && (expr[i] == ' ' || expr[i] == '\t') {
		i--
	}
	end := i + 1
	for i >= 0 && expr[i] >= '0' && expr[i] <= '9' {
		i--
	}
	if end == i+1 || isIdentByte(prevByte(expr, i+1)) {
		return 0, 0, false
	}
	left, consumed := scanNumericLiteral(expr, i+1)
	if consumed != end {
		// A float, or a literal that did not end where the operator begins.
		return 0, 0, false
	}

	j := opIndex + opWidth
	for j < len(expr) && (expr[j] == ' ' || expr[j] == '\t') {
		j++
	}
	if j >= len(expr) || expr[j] < '0' || expr[j] > '9' {
		return 0, 0, false
	}
	right, after := scanNumericLiteral(expr, j)
	if after == j {
		return 0, 0, false
	}
	// A float operand scans as 0; treat that as unreadable rather than as zero.
	if right == 0 && expr[j] != '0' {
		return 0, 0, false
	}
	return left, right, true
}

// skipStringLiteral returns the index just past the literal starting at i,
// honouring backslash escapes. An unterminated literal consumes the rest (the
// expression will fail to compile anyway).
func skipStringLiteral(s string, i int) int {
	quote := s[i]
	for i++; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case quote:
			return i + 1
		}
	}
	return len(s)
}

// scanNumericLiteral consumes one numeric literal. A literal with a fractional
// part or an exponent is a FLOAT and contributes 0, since float arithmetic
// agrees between the engines. An integer too large for uint64 saturates, which
// refuses.
func scanNumericLiteral(s string, i int) (uint64, int) {
	start := i
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	isFloat := false
	if i < len(s) && s[i] == '.' && i+1 < len(s) && s[i+1] >= '0' && s[i+1] <= '9' {
		isFloat = true
		for i++; i < len(s) && s[i] >= '0' && s[i] <= '9'; i++ {
		}
	}
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		isFloat = true
		i++
		if i < len(s) && (s[i] == '+' || s[i] == '-') {
			i++
		}
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
	}
	if isFloat {
		return 0, i
	}
	var n uint64
	for _, d := range []byte(s[start:i]) {
		n = satMul(n, 10)
		n = satAdd(n, uint64(d-'0'))
		if n >= maxExactInt {
			return maxExactInt, i
		}
	}
	return n, i
}

func prevByte(s string, i int) byte {
	if i == 0 {
		return 0
	}
	return s[i-1]
}

// isIdentByte reports whether b can be part of an identifier or an attribute
// path, so digits inside `f_sum_1` or `this.c0` are not read as literals.
func isIdentByte(b byte) bool {
	return b == '_' || b == '.' ||
		(b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
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

func satPow(base, exp uint64) uint64 {
	result := uint64(1)
	for i := uint64(0); i < exp; i++ {
		result = satMul(result, base)
		if result >= maxExactInt {
			return maxExactInt
		}
	}
	return result
}

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
