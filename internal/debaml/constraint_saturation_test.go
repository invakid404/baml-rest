package debaml

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"math/rand"
	"strconv"
	"strings"
	"testing"
)

// The saturation invariant, asserted as a PROPERTY rather than as a case list.
//
// The numeric whitelist admits an expression only when every integer it can
// produce stays under 2^53, because that is exactly where minijinja-Go's
// float64 integer arithmetic stops agreeing with stock's i128. The parser tracks
// that with one sticky bit, [numeric.saturated].
//
// Round 8 fixed the additive BOUND; round 9 fixed the bit's LIFETIME. A mixed
// int/float operation used to return a fresh numeric{isFloat: true} and forget
// its operands, so an integer prefix that had correctly crossed 2^53 was
// laundered by anything as innocuous as `+ 0.0`, and the whole expression was
// admitted. A case list would have closed `+ 0.0`; the property below closes the
// class, because it never mentions an operator.
//
// THE PROPERTY: over randomly generated arithmetic, if the exact value of any
// sub-expression THE ENGINE HOLDS AS AN INTEGER reaches 2^53,
// EvaluateConstraint must refuse.
//
// Round 9 phrased that as "any INTEGER sub-expression" and modelled a float
// operand as permanently exempt, on the reasoning that both engines are
// IEEE-754 f64 once a float is involved. Round 10's review showed that is FALSE
// for this engine: minijinja-Go's Rem and Pow read their operands through
// AsInt, which ACCEPTS an integral float64, so `2.0 ** 63` is evaluated as
// INTEGER arithmetic and hands int64() a value at 2^63. The exemption is the
// reason this test did not catch it, so the exemption is gone: the model now
// mirrors the engine's own type rules (isActualInt for Add/Sub/Mul/Div/FloorDiv,
// AsInt for Rem/Pow) and applies the bound wherever the engine produces an
// integer, however the operand was spelled.
//
// The model is exact — values are math/big rationals, `//` floors and `%`
// truncates as Go does — but the TYPE decisions come from the float64 shadow,
// because that is what the engine actually branches on. Only one direction is
// asserted, crossing implies refusal, because refusing MORE than necessary is
// the documented, measured cost of the whitelist, not a defect.

// exact is the reference value of a generated sub-expression. It carries BOTH
// an exact rational and the float64 the engine would hold, because the two
// answer different questions: the rational is the reference semantics (what
// stock's i128 preserves), while the engine decides whether a node is an
// INTEGER from its float64 alone.
type exact struct {
	// actualInt mirrors minijinja-Go's own notion of an integer VALUE — the
	// payload is stored as int64. It is not the same as "the value is integral":
	// `2.0` is integral but not an actual int, and that distinction is exactly
	// what Add/Sub/Mul/Div/FloorDiv gate on (isActualInt) and what Rem/Pow do NOT
	// (AsInt, which accepts an integral float).
	actualInt bool
	r         *big.Rat // exact value
	f         float64  // the float64 the engine computes with
	// crossed is true when this sub-expression, or any beneath it, was a value
	// the ENGINE holds as an integer whose exact magnitude reached 2^53.
	crossed bool
	// promoted is true when this sub-expression, or any beneath it, became an
	// engine INTEGER out of an operand that was not an actual int — the round-10
	// class. It is only used to prove the generator reaches that path.
	promoted bool
}

// twoP53 is the first integer minijinja-Go's float64 arithmetic can no longer
// distinguish from its neighbour.
var twoP53 = new(big.Rat).SetInt64(int64(maxExactInt))

// isIntegral is minijinja-Go's AsInt predicate: `d == math.Trunc(d)` over the
// float64 payload. This is the promotion test, and it says YES to 2.0.
func (e exact) isIntegral() bool { return e.f == math.Trunc(e.f) && !math.IsInf(e.f, 0) }

func (e exact) isZero() bool { return e.r.Sign() == 0 }

func absRat(r *big.Rat) *big.Rat { return new(big.Rat).Abs(r) }

// node builds a result. engineInt says whether minijinja-Go hands this value
// back as an int64 — and THAT is what makes the 2^53 bound apply, whether the
// operands were spelled `2` or `2.0`.
func node(r *big.Rat, engineInt, promotedHere bool, a, b exact) exact {
	crossed := a.crossed || b.crossed
	if engineInt && absRat(r).Cmp(twoP53) >= 0 {
		crossed = true
	}
	f, _ := r.Float64()
	return exact{actualInt: engineInt, r: r, f: f, crossed: crossed,
		promoted: a.promoted || b.promoted || promotedHere}
}

// intLiterals are all <= 15 digits, so they pass the token gate and the
// interesting crossings come from ARITHMETIC rather than from a literal the
// profile would reject on sight. 999999999999999 is just under 2^53, so two of
// them stay in range and three do not.
var intLiterals = []string{"0", "1", "2", "3", "7", "10", "99999999", "123456789", "999999999999999", "999999999999", "4503599627370"}

// floatLiterals deliberately mixes INTEGRAL floats (`2.0`, `4.0`,
// `999999999999999.0`) with non-integral ones (`1.5`, `0.5`). Only the integral
// ones can be promoted to integers by Rem and Pow, so a pool without them would
// leave the round-10 hole unexercised — which is precisely how the round-9
// version of this test missed it.
var floatLiterals = []string{"0.0", "1.0", "1.5", "2.0", "4.0", "0.5", "10.0", "999999999999.5", "999999999999999.0"}

type exprGen struct {
	rng *rand.Rand
}

func ratFromString(lit string) *big.Rat {
	r, ok := new(big.Rat).SetString(lit)
	if !ok {
		panic("bad literal in the generator pool: " + lit)
	}
	return r
}

// leaf emits a literal.
func (g *exprGen) leaf() (string, exact) {
	if g.rng.Intn(4) == 0 {
		lit := floatLiterals[g.rng.Intn(len(floatLiterals))]
		f, err := strconv.ParseFloat(lit, 64)
		if err != nil {
			panic("bad float literal in the generator pool: " + lit)
		}
		// A float LITERAL is stored as float64, so actualInt is false even for
		// `2.0` — the engine only promotes it inside Rem and Pow.
		return lit, exact{r: ratFromString(lit), f: f}
	}
	lit := intLiterals[g.rng.Intn(len(intLiterals))]
	r := ratFromString(lit)
	return lit, node(r, true, false, exact{}, exact{})
}

// term emits an arithmetic expression of at most the given depth, together with
// its exact value.
func (g *exprGen) term(depth int) (string, exact) {
	if depth <= 0 {
		return g.leaf()
	}
	switch g.rng.Intn(12) {
	case 0:
		// Unary minus, always parenthesised so that the model and minijinja
		// cannot disagree about how it binds against `**`. Neg preserves the
		// payload type in both engines.
		s, v := g.term(depth - 1)
		return "(-" + s + ")", node(new(big.Rat).Neg(v.r), v.actualInt, false, v, exact{})
	case 1:
		// A power with a small non-negative integer LITERAL exponent — the only
		// exponent shape the whitelist admits, so this exercises the accepted path
		// rather than bouncing off the literal check.
		base, bv := g.term(depth - 1)
		e := g.rng.Intn(5)
		expr := "(" + base + ") ** " + fmt.Sprint(e)
		r := ratPow(bv.r, e)
		// Pow reads BOTH operands through AsInt, so an INTEGRAL FLOAT base is
		// promoted: `2.0 ** 63` comes back as an int64. The engine decides from
		// the float64 result, so the model does too.
		res, _ := r.Float64()
		engineInt := bv.isIntegral() && res == math.Trunc(res) &&
			!math.IsInf(res, 0) && math.Abs(res) <= math.MaxInt64
		return expr, node(r, engineInt, engineInt && !bv.actualInt, bv, exact{})
	}
	left, lv := g.term(depth - 1)
	right, rv := g.term(depth - 1)

	op := []string{"+", "-", "*", "/", "//", "%"}[g.rng.Intn(6)]
	// Neither engine survives a zero divisor, and the model cannot represent it
	// either; fall back to an operator that is always defined.
	if (op == "/" || op == "//" || op == "%") && rv.isZero() {
		op = "+"
	}
	expr := "(" + left + " " + op + " " + right + ")"

	// Add, Sub, Mul, Div and FloorDiv gate their integer result on isActualInt,
	// which type-switches on the STORED payload — so an integral float keeps them
	// in float64 and no promotion happens. Rem is the exception: like Pow it
	// reads AsInt.
	bothActualInt := lv.actualInt && rv.actualInt
	switch op {
	case "+":
		return expr, node(new(big.Rat).Add(lv.r, rv.r), bothActualInt, false, lv, rv)
	case "-":
		return expr, node(new(big.Rat).Sub(lv.r, rv.r), bothActualInt, false, lv, rv)
	case "*":
		return expr, node(new(big.Rat).Mul(lv.r, rv.r), bothActualInt, false, lv, rv)
	case "/":
		// Div always returns FromFloat, even for two integers.
		return expr, node(new(big.Rat).Quo(lv.r, rv.r), false, false, lv, rv)
	case "//":
		return expr, node(ratFloorDiv(lv.r, rv.r), bothActualInt, false, lv, rv)
	default: // "%"
		// PROMOTES: Rem returns FromInt(i1 % i2) whenever BOTH operands are
		// AsInt-able, which includes integral floats. Go's % is truncated.
		if lv.isIntegral() && rv.isIntegral() {
			return expr, node(ratTruncRem(lv.r, rv.r), true, !bothActualInt, lv, rv)
		}
		return expr, node(ratTruncRem(lv.r, rv.r), false, false, lv, rv)
	}
}

func ratPow(base *big.Rat, e int) *big.Rat {
	out := new(big.Rat).SetInt64(1)
	for i := 0; i < e; i++ {
		out.Mul(out, base)
	}
	return out
}

// ratFloorDiv is math.Floor(a/b) as an exact rational.
func ratFloorDiv(a, b *big.Rat) *big.Rat {
	q := new(big.Rat).Quo(a, b)
	n := new(big.Int).Div(q.Num(), q.Denom()) // big.Int Div floors
	return new(big.Rat).SetInt(n)
}

// ratTruncRem is a - b*trunc(a/b): Go's truncated remainder, which is what
// minijinja-Go performs on the integer branch and math.Mod on the float one.
func ratTruncRem(a, b *big.Rat) *big.Rat {
	q := new(big.Rat).Quo(a, b)
	t := new(big.Int).Quo(q.Num(), q.Denom()) // big.Int Quo truncates
	return new(big.Rat).Sub(a, new(big.Rat).Mul(b, new(big.Rat).SetInt(t)))
}

// predicate wraps two arithmetic terms in a comparison, because
// EvaluateConstraint requires the render to be exactly "true" or "false".
func (g *exprGen) predicate(depth int) (expr string, crossed, promoted bool) {
	left, lv := g.term(depth)
	right, rv := g.term(depth)
	op := []string{"==", "!=", "<", ">", "<=", ">="}[g.rng.Intn(6)]
	return left + " " + op + " " + right, lv.crossed || rv.crossed, lv.promoted || rv.promoted
}

func TestSaturationIsStickyUnderRandomArithmetic(t *testing.T) {
	// A fixed seed: this is a property test, not a fuzz target. It must fail the
	// same way on every machine and in CI, which a time-seeded generator cannot
	// promise. The corpus below is large enough that the laundering shapes appear
	// many times over (the counters printed at the end prove it did not go
	// vacuous).
	g := &exprGen{rng: rand.New(rand.NewSource(20260727))}

	const iterations = 20000
	var crossedCases, decidedCases, refusedInRange, promotedCrossings int
	for i := 0; i < iterations; i++ {
		expr, crossed, promoted := g.predicate(3)
		if crossed && promoted {
			promotedCrossings++
		}
		got, err := EvaluateConstraint(NullValue(), expr)

		switch {
		case crossed:
			crossedCases++
			if !errors.Is(err, ErrConstraintUnsupported) {
				t.Fatalf("saturation was LAUNDERED.\n"+
					"expression: %s\n"+
					"an integer sub-expression's exact value reached 2^53, so the profile had to refuse,\n"+
					"but EvaluateConstraint answered (%v, %v)", expr, got, err)
			}
		case err == nil:
			decidedCases++
		case errors.Is(err, ErrConstraintUnsupported):
			refusedInRange++
		default:
			t.Fatalf("%q failed with an error that is not the sentinel: %v", expr, err)
		}
	}

	// Guard against a vacuous pass in BOTH directions: the generator must have
	// produced crossings for the property to mean anything, and it must also have
	// produced answers, or a profile that refused everything would pass.
	if promotedCrossings < 50 {
		t.Errorf("only %d generated crossings came through an INTEGRAL-FLOAT operand; "+
			"the generator is not exercising the Rem/Pow promotion that round 10 closed", promotedCrossings)
	}
	if crossedCases < iterations/20 {
		t.Errorf("only %d/%d generated expressions crossed 2^53; the generator is not exercising the property",
			crossedCases, iterations)
	}
	// The floor is deliberately low, and lower than round 9's: closing the
	// integral-float promotion means a COMPUTED float base for `**` is now
	// refused, which this generator produces often. Several hundred answered
	// expressions is still far more than enough to catch a profile that had
	// started refusing everything, which is all this guard is for.
	if decidedCases < 500 {
		t.Errorf("only %d/%d in-range expressions were ANSWERED; the profile may have become a blanket refusal",
			decidedCases, iterations)
	}
	t.Logf("%d crossed (all refused; %d of them only because an integral float was promoted to an integer), "+
		"%d answered, %d in-range but refused for another whitelist reason",
		crossedCases, promotedCrossings, decidedCases, refusedInRange)
}

// TestSaturationSurvivesEveryFloatProducingPath is the case-level companion to
// the property above: it names the specific handoffs that used to drop the bit,
// so a regression reports WHICH path leaked rather than only that some random
// tree leaked.
func TestSaturationSurvivesEveryFloatProducingPath(t *testing.T) {
	const k = "999999999999999"
	// prefix is ten `+ k` terms on top of a seed, whose exact value is well past
	// 2^53 while every literal in it is a 15-digit token the profile accepts.
	prefix := func(seed string) string {
		return "(" + seed + strings.Repeat(" + "+k, 10) + ")"
	}
	saturated := prefix("1")

	launderings := map[string]string{
		// The reviewer's exact case.
		"reviewer's + 0.0 handoff": "(" + saturated + " - 1 + 0.0) == (" + saturated + " + 0.0)",
		// The same laundering through each of the other mixed float operators.
		"mixed float *":  "(" + saturated + " - 1) * 1.0 == " + saturated + " * 1.0",
		"mixed float /":  "(" + saturated + " - 1) / 1.0 == " + saturated + " / 1.0",
		"mixed float -":  "(" + saturated + " - 1) - 0.0 == " + saturated + " - 0.0",
		"mixed float //": "(" + saturated + " - 1) // 1.0 == " + saturated + " // 1.0",
		// A float on the LEFT, so the bit has to survive from the right operand.
		"float on the left": "0.0 + (" + saturated + " - 1) == 0.0 + " + saturated,
		// parsePow's float-base branch, reached by a parenthesised saturated
		// expression that has already been turned into a float. Since round 10
		// this is refused twice over: the base is computed rather than a literal,
		// AND the saturation bit is still set.
		"float-base **": "(" + saturated + " * 1.0) ** 1 == (" + saturated + " * 1.0) ** 1",
		// The saturated prefix as an integer base, which must stay refused.
		"int-base **": "(" + saturated + ") ** 1 == (" + saturated + ") ** 1",
		// Nested: laundered once, then used again.
		"nested re-entry": "((" + saturated + " + 0.0) - 1.0) * 2.0 > 0.0",
		// A comparison result can never re-enter arithmetic, but pin it anyway.
		"through a comparison": "(" + saturated + " > 0) == true",
	}
	for name, expr := range launderings {
		if got, err := EvaluateConstraint(NullValue(), expr); !errors.Is(err, ErrConstraintUnsupported) {
			t.Errorf("%s: saturation laundered — answered (%v, %v) for %.60s…", name, got, err, expr)
		}
	}

	// The in-range controls. Each is the same SHAPE as a laundering above, with a
	// prefix that never crosses 2^53, and each must still be answered — otherwise
	// the sticky bit has become a blanket refusal of mixed arithmetic.
	controls := map[string]bool{
		"(1 + 2 + 0.0) == (3 + 0.0)":        true,
		"(1 + 2) * 1.0 == 3 * 1.0":          true,
		"(1 + 2) / 1.0 == 3.0":              true,
		"0.0 + (1 + 2) == 0.0 + 3":          true,
		"2.0 ** 3 == 8.0":                   true,
		"2.0 ** 3 == 8":                     true,
		"0.5 ** 3 == 0.125":                 true,
		"(1 + 2) ** 2 == 9":                 true,
		"((1 + 2 + 0.0) - 1.0) * 2.0 > 0.0": true,
		"999999999999 + 999999999999 > 0":   true,
	}
	for expr, want := range controls {
		got, err := EvaluateConstraint(NullValue(), expr)
		if err != nil {
			t.Errorf("in-range control %q was refused (%v); the sticky bit is over-broad", expr, err)
			continue
		}
		if got != want {
			t.Errorf("in-range control %q = %v, want %v", expr, got, want)
		}
	}
}

// TestIntegralFloatsAreNotExemptFromTheIntegerBound pins the round-10 class.
//
// The profile used to treat any float operand as proof that both engines stayed
// in IEEE-754 f64. minijinja-Go v2.16.0 disagrees in exactly two of its numeric
// ops, because they read their operands through AsInt — which accepts an
// integral float64 — rather than isActualInt, which type-switches on the stored
// payload:
//
//	Add Sub Mul Div FloorDiv Neg   isActualInt   an integral float stays a float
//	Rem Pow                        AsInt         an integral float becomes an int
//
// So `2.0 ** 63` is INTEGER arithmetic in the port: math.Pow gives 2^63, the
// guard `result <= math.MaxInt64` passes because that untyped constant rounds UP
// to 2^63 in f64, and int64(float64(2^63)) is the invalid conversion from round
// 3 — MinInt64 on linux/amd64, saturated on arm64. Stock coerces the float base
// to F64 and calls powf, so `2.0 ** 63 > 0.0` is true there and was false here.
func TestIntegralFloatsAreNotExemptFromTheIntegerBound(t *testing.T) {
	for name, expr := range map[string]string{
		// The reviewer's exact case, and the boundary either side of it.
		"the reported case":     "2.0 ** 63 > 0.0",
		"at the 2^53 bound":     "2.0 ** 53 > 0.0",
		"through parentheses":   "(2.0) ** 63 > 0.0",
		"negated float base":    "(-2.0) ** 63 < 0.0",
		"a large integral base": "999999999999999.0 ** 2 > 0.0",
		"a mid-range base":      "9999999999.0 ** 3 > 0.0",
		// A COMPUTED float base cannot be bounded here at all: `/` by a fraction
		// grows the magnitude without limit, and this profile does not evaluate.
		"computed base, division": "(2.0 / 0.0000000000001) ** 2 > 0.0",
		"computed base, addition": "(1.0 + 1.0) ** 63 > 0.0",
		// A float EXPONENT is promoted by the same AsInt read.
		"float exponent":      "2.0 ** 63.0 > 0.0",
		"int base, float exp": "2 ** 3.0 == 8",
		// Rem is the other promoting op. It is unreachable because `%` demands
		// non-negative INTEGER literals, but pin that rather than rely on it.
		"rem with a float operand": "4.0 % 3 == 1",
		"rem past int64 via float": "1000000000000000.0 * 1000.0 % 3 == 0",
		"floordiv with floats":     "7.0 // 2.0 == 3.0",
	} {
		if got, err := EvaluateConstraint(NullValue(), expr); !errors.Is(err, ErrConstraintUnsupported) {
			t.Errorf("%s: %q answered (%v, %v); an integral float can be promoted to an integer here",
				name, expr, got, err)
		}
	}

	// Below the bound, a float-base power is proven and must still decide — the
	// engines agree there, and refusing it would be a needless loss.
	for expr, want := range map[string]bool{
		"2.0 ** 3 == 8.0":     true,
		"2.0 ** 3 == 8":       true,
		"2.0 ** 52 > 0.0":     true,
		"0.5 ** 3 == 0.125":   true,
		"2.5 ** 2 == 6.25":    true,
		"1.0 ** 100 == 1.0":   true,
		"10.0 ** 3 == 1000.0": true,
	} {
		got, err := EvaluateConstraint(NullValue(), expr)
		if err != nil {
			t.Errorf("in-range float-base power %q was refused (%v); the bound is over-broad", expr, err)
			continue
		}
		if got != want {
			t.Errorf("%q = %v, want %v", expr, got, want)
		}
	}

	// The non-promoting ops must be untouched: an integral float stays a float
	// through Add, Sub, Mul and Div in BOTH engines, so a large one is not a
	// crossing and must not start refusing.
	for expr, want := range map[string]bool{
		"999999999999999.0 * 999999999999999.0 > 0.0": true,
		"999999999999999.0 + 999999999999999.0 > 0.0": true,
		"1.0 + 1.0 == 2.0":                            true,
		"1.5 * 2.0 == 3.0":                            true,
		"7.0 / 2.0 == 3.5":                            true,
	} {
		got, err := EvaluateConstraint(NullValue(), expr)
		if err != nil {
			t.Errorf("non-promoting float arithmetic %q was refused (%v)", expr, err)
			continue
		}
		if got != want {
			t.Errorf("%q = %v, want %v", expr, got, want)
		}
	}
}
