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
		env.AddFilter(name, guardForeignMapping(name, builtin))
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
