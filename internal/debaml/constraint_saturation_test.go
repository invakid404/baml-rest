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
	// compositional is the round-11 shape specifically: the crossing happened at
	// THIS op, both operands were individually in range, and at least one of them
	// was a promoted integral float. `(2.0 ** 52) * (2.0 ** 11)` is the case the
	// review reported. Counting it is what proves the generator reaches "a
	// below-bound promoted integer subsequently reaches an ordinary integer
	// operation", which round 10's version did not.
	compositional bool
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
	crossedHere := engineInt && absRat(r).Cmp(twoP53) >= 0
	f, _ := r.Float64()
	return exact{
		actualInt: engineInt, r: r, f: f,
		crossed:  a.crossed || b.crossed || crossedHere,
		promoted: a.promoted || b.promoted || promotedHere,
		compositional: a.compositional || b.compositional ||
			(crossedHere && !a.crossed && !b.crossed && (a.promoted || b.promoted)),
	}
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
//
// The last three are the round-12 shape: a FRACTIONALLY SPELLED literal whose
// PARSED f64 is integral. At 15 digits an f64 ULP is ~0.0156-0.125, so a
// 15-digit tail rounds clean away and `562949953421312.000000000000001` is
// exactly 2^49 to the engine. `562949953421312.5` is the neighbour that does
// NOT round — 0.5 is exactly representable there — and must stay a float.
var floatLiterals = []string{
	"0.0", "1.0", "1.5", "2.0", "4.0", "0.5", "10.0", "999999999999.5", "999999999999999.0",
	"562949953421312.000000000000001", "999999999999999.000000000000001", "562949953421312.5",
}

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
		//
		// The exact value is the PARSED f64, not the decimal text: minijinja-Go's
		// lexer runs strconv.ParseFloat over the literal, so
		// `562949953421312.000000000000001` IS 2^49 in the engine and the model
		// must agree, or the property would be asserted against a number that
		// never exists at runtime.
		return lit, exact{r: new(big.Rat).SetFloat64(f), f: f}
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
		// A power with a non-negative integer LITERAL exponent — the only exponent
		// shape the whitelist admits, so this exercises the accepted path rather
		// than bouncing off the literal check.
		//
		// The exponent range reaches past 2^53 ON PURPOSE. Round 10's version
		// stopped at 4, so a promoted power was always tiny and the generator
		// could not produce the round-11 shape: a below-bound promoted integer
		// that only crosses when a LATER integer op consumes it. Ranging over
		// 0..40 means two such powers routinely multiply past the bound.
		base, bv := g.term(depth - 1)
		// Mostly small, so ordinary in-range arithmetic still gets generated and
		// the "did anything get ANSWERED" guard below stays meaningful; large a
		// fifth of the time, so the crossings are reached too.
		e := g.rng.Intn(5)
		if g.rng.Intn(5) == 0 {
			e = g.rng.Intn(41)
		}
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

	if g.rng.Intn(6) == 0 {
		// THE ROUND-11 SHAPE, generated on purpose rather than hoped for: two
		// integral-float powers, each individually in range, combined by an
		// integer op. `(2.0 ** 52) * (2.0 ** 11)` is this with i=52, j=11.
		bases := []string{"2.0", "4.0", "10.0"}
		bl, br := bases[g.rng.Intn(len(bases))], bases[g.rng.Intn(len(bases))]
		i, j := g.rng.Intn(40), g.rng.Intn(40)
		left = fmt.Sprintf("(%s ** %d)", bl, i)
		right = fmt.Sprintf("(%s ** %d)", br, j)
		lv = powNode(bl, i)
		rv = powNode(br, j)
	}

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

// powNode is the model for `<integral float literal> ** e`: minijinja-Go's Pow
// AsInt-promotes the base, so the result is an actual int64 whenever it stays
// inside int64 range.
func powNode(base string, e int) exact {
	bv := exact{r: ratFromString(base)}
	bv.f, _ = bv.r.Float64()
	r := ratPow(bv.r, e)
	res, _ := r.Float64()
	engineInt := bv.isIntegral() && res == math.Trunc(res) &&
		!math.IsInf(res, 0) && math.Abs(res) <= math.MaxInt64
	return node(r, engineInt, engineInt && !bv.actualInt, bv, exact{})
}

// predicate wraps two arithmetic terms in a comparison, because
// EvaluateConstraint requires the render to be exactly "true" or "false".
func (g *exprGen) predicate(depth int) (expr string, crossed, promoted, compositional bool) {
	left, lv := g.term(depth)
	right, rv := g.term(depth)
	op := []string{"==", "!=", "<", ">", "<=", ">="}[g.rng.Intn(6)]
	return left + " " + op + " " + right,
		lv.crossed || rv.crossed,
		lv.promoted || rv.promoted,
		lv.compositional || rv.compositional
}

func TestSaturationIsStickyUnderRandomArithmetic(t *testing.T) {
	// A fixed seed: this is a property test, not a fuzz target. It must fail the
	// same way on every machine and in CI, which a time-seeded generator cannot
	// promise. The corpus below is large enough that the laundering shapes appear
	// many times over (the counters printed at the end prove it did not go
	// vacuous).
	g := &exprGen{rng: rand.New(rand.NewSource(20260727))}

	const iterations = 20000
	var crossedCases, decidedCases, refusedInRange, promotedCrossings, compositionalCrossings int
	for i := 0; i < iterations; i++ {
		expr, crossed, promoted, compositional := g.predicate(3)
		if crossed && promoted {
			promotedCrossings++
		}
		if compositional {
			compositionalCrossings++
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
	if compositionalCrossings < 50 {
		t.Errorf("only %d generated crossings were COMPOSITIONAL — a below-bound promoted integral "+
			"float reaching a later integer op. That is the round-11 shape, and a generator that "+
			"does not reach it cannot prove the bound follows the payload type", compositionalCrossings)
	}
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
	t.Logf("%d crossed (all refused; %d involved a promoted integral float, %d of those only crossed "+
		"COMPOSITIONALLY — two in-range promoted operands meeting a later integer op), "+
		"%d answered, %d in-range but refused for another whitelist reason",
		crossedCases, promotedCrossings, compositionalCrossings, decidedCases, refusedInRange)
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

// TestPromotedIntegralFloatCompositionsAreBounded is the round-11 regression,
// and it is a DETERMINISTIC sweep rather than a sample.
//
// Round 10 bounded a promoted power at its own node and then handed the result
// on as "a float", so two individually in-range promoted integers could be
// multiplied without any bound at all:
//
//	(2.0 ** 52) * (2.0 ** 11) > 0.0
//
// minijinja-Go's Pow AsInt-promotes each side to an actual int64, Mul sees two
// actual ints and computes FromInt(int64(2^52 * 2^11)) = int64(float64(2^63)) —
// MinInt64 on linux/amd64 — so native answered FALSE; stock keeps both powers in
// f64, gets +2^63 and answers TRUE.
//
// The sweep below pins the boundary itself rather than the one reported pair:
// `2.0 ** i` is an integer of magnitude 2^i, so the product's bound is 2^(i+j)
// and the expression must be admitted exactly while i+j < 53.
func TestPromotedIntegralFloatCompositionsAreBounded(t *testing.T) {
	const limit = 40 // i+j spans 0..80, so the sweep crosses 53 from both sides
	// Both spellings of the SAME payload. `2.000000000000000` is 2.0 written with
	// a 15-digit tail, which the engine's ParseFloat collapses to exactly 2.0 —
	// so the sweep must produce identical verdicts for the two, or engineInt is
	// still being decided by the source text (the round-12 finding).
	for _, base := range []string{"2.0", "2.000000000000000"} {
		for i := 0; i <= limit; i++ {
			for j := 0; j <= limit; j++ {
				expr := fmt.Sprintf("(%s ** %d) * (%s ** %d) > 0.0", base, i, base, j)
				got, err := EvaluateConstraint(NullValue(), expr)
				crosses := i+j >= 53
				switch {
				case crosses && !errors.Is(err, ErrConstraintUnsupported):
					t.Fatalf("%s: product bound is 2^%d, at or past 2^53 — must refuse, answered (%v, %v)",
						expr, i+j, got, err)
				case !crosses && err != nil:
					t.Fatalf("%s: product bound is 2^%d, well inside 2^53 — must decide, refused with %v",
						expr, i+j, err)
				case !crosses && !got:
					t.Fatalf("%s = false, want true", expr)
				}
			}
		}
	}

	// The same sweep anchored at 2^49, where an f64 ULP is 0.125 and a 15-digit
	// fractional tail therefore rounds ENTIRELY away. `2^49 ** 1` is an integer of
	// magnitude 2^49 to the engine however it was spelled, so multiplying by 2^k
	// must refuse from k = 4 up.
	for _, base := range []string{"562949953421312.0", "562949953421312.000000000000001"} {
		for k := 0; k <= 20; k++ {
			expr := fmt.Sprintf("(%s ** 1) * (2.0 ** %d) > 0.0", base, k)
			got, err := EvaluateConstraint(NullValue(), expr)
			crosses := 49+k >= 53
			switch {
			case crosses && !errors.Is(err, ErrConstraintUnsupported):
				t.Fatalf("%s: bound is 2^%d — must refuse, answered (%v, %v)", expr, 49+k, got, err)
			case !crosses && err != nil:
				t.Fatalf("%s: bound is 2^%d — must decide, refused with %v", expr, 49+k, err)
			case !crosses && !got:
				t.Fatalf("%s = false, want true", expr)
			}
		}
	}

	// And the NON-integral neighbour at the same anchor: 0.5 is exactly
	// representable at 2^49, so AsInt rejects it, no promotion happens, both
	// engines stay in f64 — every k must be admitted.
	for k := 0; k <= 20; k++ {
		expr := fmt.Sprintf("(562949953421312.5 ** 1) * (2.0 ** %d) > 0.0", k)
		if got, err := EvaluateConstraint(NullValue(), expr); err != nil || !got {
			t.Fatalf("%s: a non-integral payload is never promoted, so it must decide; got (%v, %v)",
				expr, got, err)
		}
	}

	for i := 0; i <= limit; i++ {
		for j := 0; j <= limit; j++ {
			expr := fmt.Sprintf("(2.0 ** %d) * (2.0 ** %d) > 0.0", i, j)
			got, err := EvaluateConstraint(NullValue(), expr)
			crosses := i+j >= 53
			switch {
			case crosses && !errors.Is(err, ErrConstraintUnsupported):
				t.Fatalf("%s: product bound is 2^%d, at or past 2^53 — must refuse, answered (%v, %v)",
					expr, i+j, got, err)
			case !crosses && err != nil:
				t.Fatalf("%s: product bound is 2^%d, well inside 2^53 — must decide, refused with %v",
					expr, i+j, err)
			case !crosses && !got:
				t.Fatalf("%s = false, want true", expr)
			}
		}
	}

	// The same sweep for the additive operators, whose bound is the conservative
	// SUM from round 8: 2^i + 2^j.
	for i := 0; i <= 54; i++ {
		for j := 0; j <= 54; j++ {
			for _, op := range []string{"+", "-"} {
				expr := fmt.Sprintf("(2.0 ** %d) %s (2.0 ** %d) > -1.0", i, op, j)
				_, err := EvaluateConstraint(NullValue(), expr)
				crosses := satAdd(satPow(2, uint64(i)), satPow(2, uint64(j))) >= maxExactInt
				if crosses != errors.Is(err, ErrConstraintUnsupported) {
					t.Fatalf("%s: crosses=%v but err=%v", expr, crosses, err)
				}
			}
		}
	}
}

// TestEngineIntegerTrackingFollowsThePayloadNotTheSpelling states the round-11
// model directly: the bound follows minijinja-Go's STORED payload type, so an
// integral float that the engine promoted is an integer everywhere downstream,
// and a value the engine keeps in float64 is not bounded at all.
func TestEngineIntegerTrackingFollowsThePayloadNotTheSpelling(t *testing.T) {
	// PROMOTED, therefore bounded — every one of these reaches an integer op
	// carrying a promoted integral-float power.
	for name, expr := range map[string]string{
		"the reported composition":  "(2.0 ** 52) * (2.0 ** 11) > 0.0",
		"split evenly":              "(2.0 ** 26) * (2.0 ** 27) > 0.0",
		"promoted times an int":     "(2.0 ** 52) * 2 > 0.0",
		"int times a promoted":      "2 * (2.0 ** 52) > 0.0",
		"promoted plus a promoted":  "(2.0 ** 52) + (2.0 ** 52) > 0.0",
		"promoted minus a promoted": "(2.0 ** 52) - (2.0 ** 52) == 0",
		"nested composition":        "((2.0 ** 26) * (2.0 ** 26)) * 2 > 0.0",
		"promoted as a pow base":    "(2.0 ** 52) ** 2 > 0.0",
		"another base":              "(4.0 ** 26) * (4.0 ** 1) > 0.0",
		"base ten":                  "(10.0 ** 15) * (10.0 ** 3) > 0.0",
		"promoted into floordiv":    "(2.0 ** 52) // 1 > 0",
		"promoted into modulo":      "(2.0 ** 52) % 3 == 1",
		"promoted then compared":    "(2.0 ** 52) * (2.0 ** 11) == 9223372036854775808",
	} {
		if got, err := EvaluateConstraint(NullValue(), expr); !errors.Is(err, ErrConstraintUnsupported) {
			t.Errorf("%s: %q answered (%v, %v); a promoted integral float must stay bounded downstream",
				name, expr, got, err)
		}
	}

	// NOT promoted, therefore NOT bounded — the engine keeps these in float64
	// and so does stock, so bounding them would be a pure loss.
	for expr, want := range map[string]bool{
		// A float64 operand keeps Mul/Add out of their integer arm, even when the
		// other side is a promoted integer.
		"(2.0 ** 52) * 2.0 > 0.0": true,
		"(2.0 ** 52) + 1.0 > 0.0": true,
		// AsInt REJECTS a non-integral base, so no promotion happens at all.
		"(2.5 ** 2) * (2.5 ** 2) == 39.0625": true,
		"2.5 ** 2 == 6.25":                   true,
		"0.5 ** 3 == 0.125":                  true,
		// Ordinary float arithmetic, unbounded in both engines.
		"999999999999999.0 * 999999999999999.0 > 0.0": true,
		"999999999999999.0 + 999999999999999.0 > 0.0": true,
		"7.0 / 2.0 == 3.5":                            true,
		// In-range promoted compositions still decide.
		"(2.0 ** 3) * (2.0 ** 4) == 128.0": true,
		"(2.0 ** 3) + (2.0 ** 4) == 24.0":  true,
		"(2.0 ** 3) - 1 == 7":              true,
		"2.0 ** 52 > 0.0":                  true,
		"10.0 ** 3 == 1000.0":              true,
	} {
		got, err := EvaluateConstraint(NullValue(), expr)
		if err != nil {
			t.Errorf("%q was refused (%v); the payload-type bound is over-broad", expr, err)
			continue
		}
		if got != want {
			t.Errorf("%q = %v, want %v", expr, got, want)
		}
	}
}

// TestFloatLiteralsAreClassifiedByTheParsedPayload pins the round-12 boundary:
// engineInt is decided by the f64 the ENGINE will hold, not by how the literal
// was written.
//
// minijinja-Go's lexer parses every float literal with
// strconv.ParseFloat(.., 64), re-emits it as FormatFloat(v, 'f', -1, 64) — the
// shortest form that round-trips — and the parser re-parses that, so the
// composition is the identity on the float64. Value.AsInt then promotes on
// `d == math.Trunc(d)` alone. Asking "is the written fraction all zeros?" is a
// DIFFERENT question, and the gap between them is reachable: at 15 digits an
// f64 ULP is between ~0.0156 and 0.125, so any 15-digit fractional tail on a
// 15-digit integer part rounds clean away.
func TestFloatLiteralsAreClassifiedByTheParsedPayload(t *testing.T) {
	// Every literal the closed grammar admits: the profile's verdict on
	// integrality must equal strconv.ParseFloat + math.Trunc, because that pair
	// IS the engine.
	for _, lit := range []string{
		"0.0", "1.0", "2.0", "2.5", "0.5", "1.5", "10.0", "999999999999.5",
		"999999999999999.0", "562949953421312.5", "562949953421312.25",
		"562949953421312.000000000000001", "999999999999999.000000000000001",
		"123456789012345.000000000000009", "100000000000000.999999999999999",
		"2.000000000000001", "2.00000000000000", "0.000000000000001",
	} {
		payload, err := strconv.ParseFloat(lit, 64)
		if err != nil {
			t.Fatalf("test literal %q does not parse: %v", lit, err)
		}
		wantIntegral := payload == math.Trunc(payload)

		n, ok := parseNumeric(lit)
		if !ok {
			// Refusal is always allowed; it just cannot be silently wrong.
			continue
		}
		if n.integralFloatLiteral != wantIntegral {
			t.Errorf("%q parses to %v (integral=%v) but the profile says integral=%v — "+
				"the classifier is reading the source text, not the payload",
				lit, payload, wantIntegral, n.integralFloatLiteral)
		}
		if wantIntegral && n.mag != uint64(math.Abs(math.Trunc(payload))) {
			t.Errorf("%q has payload %v but magnitude %d", lit, payload, n.mag)
		}
	}

	// The reviewer's case, and the same shape at other anchors. Each is a
	// fractionally spelled literal whose payload is integral, promoted by `**`,
	// then multiplied past 2^53 by an ordinary integer op.
	for name, expr := range map[string]string{
		"the reported case":     "(562949953421312.000000000000001 ** 1) * 16384 > 0.0",
		"same, via a promotion": "(562949953421312.000000000000001 ** 1) * (2.0 ** 14) > 0.0",
		"a nine-anchored value": "(999999999999999.000000000000001 ** 1) * 16 > 0.0",
		// Crosses only at the ADDITIVE step: 2^49 * 8 is 2^52, still in range, and
		// the sum with another 2^52 is exactly 2^53.
		"additive":           "(562949953421312.000000000000001 ** 1) * 8 + (2.0 ** 52) > 0.0",
		"canonical spelling": "(562949953421312.0 ** 1) * 16384 > 0.0",
	} {
		if got, err := EvaluateConstraint(NullValue(), expr); !errors.Is(err, ErrConstraintUnsupported) {
			t.Errorf("%s: %q answered (%v, %v); the literal's PAYLOAD is integral, so the engine "+
				"promotes it and the product crosses 2^53", name, expr, got, err)
		}
	}

	// The non-integral NEIGHBOURS. 0.5 and 0.25 are exactly representable at
	// 2^49, so these do NOT round to an integer, AsInt rejects them, both engines
	// stay in f64 — and refusing them would be a pure loss.
	for expr, want := range map[string]bool{
		"(562949953421312.5 ** 1) * 16384 > 0.0":        true,
		"(562949953421312.25 ** 1) * 16384 > 0.0":       true,
		"(2.000000000000001 ** 52) * (2.0 ** 11) > 0.0": true,
		"(2.5 ** 2) * (2.5 ** 2) == 39.0625":            true,
		// Not a promoting op: Value.Mul gates on isActualInt, which is false for
		// ANY float64 payload, integral or not. So this stays f64 in both engines
		// even though the payload is exactly 2^49.
		"562949953421312.000000000000001 * 16384 > 0.0": true,
		// A rounded-to-integral literal must behave exactly like its canonical
		// spelling, in both directions.
		"(2.00000000000000 ** 3) == 8.0": true,
	} {
		got, err := EvaluateConstraint(NullValue(), expr)
		if err != nil {
			t.Errorf("%q was refused (%v); the payload classifier is over-broad", expr, err)
			continue
		}
		if got != want {
			t.Errorf("%q = %v, want %v", expr, got, want)
		}
	}
}

// hazardFloat is 2^63 exactly, as a float64. It is the round-13 reproducer:
// minijinja-Go's AsInt float arm is int64(d) after `d == math.Trunc(d)`, and
// int64(float64(2^63)) is MinInt64 on linux/amd64 (arm64 saturates), while
// stock's minijinja fork converts F64 to i128 when `(val as i64 as f64) == val`
// and Rust SATURATES 2^63 to i64::MAX. MinInt64 is even, i64::MAX is odd — so
// `this is even` was true natively and false in stock.
func hazardFloat() ConstraintValue { return FloatValue(math.Ldexp(1, 63)) }

// TestAsIntHazardIsRefusedWhereverAValueCanReachABuiltin pins the round-13
// class. The guard is keyed on the VALUE, not on a list of builtin names, so an
// AsInt-consuming builtin nobody enumerated is covered on the same terms.
func TestAsIntHazardIsRefusedWhereverAValueCanReachABuiltin(t *testing.T) {
	big := hazardFloat()
	list := ListValue([]ConstraintValue{IntValue(2), IntValue(3), hazardFloat()})
	nested := ClassValue("C", []ConstraintEntry{{Key: "v", Value: hazardFloat()}})

	for name, tc := range map[string]struct {
		this ConstraintValue
		expr string
	}{
		// The reported reproducer, both polarities.
		"even":        {big, "this is even"},
		"odd":         {big, "this is odd"},
		"divisibleby": {big, "this is divisibleby(2)"},
		"integer":     {big, "this is integer"},
		// Filters that read their subject through AsInt.
		"abs":    {big, "this|abs > 0"},
		"int":    {big, "this|int > 0"},
		"format": {big, `this|format("%d") == "x"`},
		"round":  {big, "this|round > 0"},
		// And ones that do not — the guard is on the value, so they refuse too.
		"string":     {big, `this|string == "x"`},
		"comparison": {big, "this > 0"},
		// Reached through select/reject dispatch, which is how a list element
		// gets into `even` without ever being the subject.
		"select":  {list, `this|select("even")|list|length == 2`},
		"reject":  {list, `this|reject("odd")|list|length == 2`},
		"sum":     {list, "this|sum > 0"},
		"first":   {list, "this|first is even"},
		"element": {list, "this[2] is even"},
		// At depth inside a class, which is where a real BAML value would carry it.
		"nested in a class": {nested, "this.v is even"},
	} {
		got, err := EvaluateConstraint(tc.this, tc.expr)
		if !errors.Is(err, ErrConstraintUnsupported) {
			t.Errorf("%s: %q over a 2^63 float answered (%v, %v); AsInt converts it differently in the two engines",
				name, tc.expr, got, err)
		}
	}

	// In-range controls. Below 2^53 an integral float converts to the SAME int64
	// in both engines and a non-integral one is rejected by both, so these are
	// proven and must still decide — the guard is a boundary, not a ban on
	// floats.
	small := FloatValue(4)
	smallList := ListValue([]ConstraintValue{IntValue(2), IntValue(3), IntValue(4)})
	for name, tc := range map[string]struct {
		this ConstraintValue
		expr string
		want bool
	}{
		"small float even":      {small, "this is even", true},
		"small float odd":       {small, "this is odd", false},
		"small float divisible": {small, "this is divisibleby(2)", true},
		"small float abs":       {small, "this|abs == 4", true},
		"small float compare":   {small, "this > 0", true},
		"non-integral float":    {FloatValue(2.5), "this is even", false},
		"integer this":          {IntValue(4), "this is even", true},
		"literal":               {NullValue(), "4 is even", true},
		"int list select":       {smallList, `this|select("even")|list|length == 2`, true},
		"int list reject":       {smallList, `this|reject("odd")|list|length == 2`, true},
	} {
		got, err := EvaluateConstraint(tc.this, tc.expr)
		if err != nil {
			t.Errorf("%s: %q was refused (%v); the AsInt guard is over-broad", name, tc.expr, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s: %q = %v, want %v", name, tc.expr, got, tc.want)
		}
	}
}

// TestEveryRegisteredTestIsGuarded proves the guard is installed on EVERY test
// minijinja-Go registers, not just the ones known to call AsInt.
//
// It proves it behaviourally rather than by comparing two hand-written lists:
// for each test name, the SAME expression must decide over an in-range value and
// refuse over the hazard value. An arity or type error would fail the first leg,
// so the second leg cannot pass vacuously.
func TestEveryRegisteredTestIsGuarded(t *testing.T) {
	// Every name from minijinja-Go v2.16.0 defaults.go that is spellable as
	// `x is NAME`, EXCEPT `containing` — a minijinja-contrib test BAML does not
	// build, which [withdrawNonBAMLBuiltins] turns into an unknown-test error, so
	// it must stay withdrawn rather than guarded. The operator aliases (`==`,
	// `!=`, `<`, `<=`, `>`, `>=`) are the same functions under names the template
	// grammar cannot address here.
	exprs := map[string]string{
		"defined": "this is defined", "undefined": "this is undefined",
		"none": "this is none", "true": "this is true", "false": "this is false",
		"odd": "this is odd", "even": "this is even",
		"divisibleby": "this is divisibleby(2)",
		"eq":          "this is eq(4)", "equalto": "this is equalto(4)",
		"ne": "this is ne(4)", "lt": "this is lt(9)", "lessthan": "this is lessthan(9)",
		"le": "this is le(9)", "gt": "this is gt(1)", "greaterthan": "this is greaterthan(1)",
		"ge": "this is ge(1)", "in": "this is in([4])",
		"string": "this is string", "number": "this is number",
		"integer": "this is integer", "int": "this is int", "float": "this is float",
		"boolean": "this is boolean", "sequence": "this is sequence",
		"mapping": "this is mapping", "iterable": "this is iterable",
		"sameas": "this is sameas(4)",
		"safe":   "this is safe", "escaped": "this is escaped",
		"lower": "this is lower", "upper": "this is upper",
		"filter": `this is filter("upper")`, "test": `this is test("even")`,
		"startingwith": `this is startingwith("4")`, "endingwith": `this is endingwith("4")`,
	}

	var exercised int
	for name, expr := range exprs {
		// Leg 1: the expression is well-formed and decides over an in-range value.
		if _, err := EvaluateConstraint(FloatValue(4), expr); err != nil {
			t.Errorf("%s: %q did not decide over an in-range float (%v); the sweep would be vacuous for it",
				name, expr, err)
			continue
		}
		// Leg 2: the same expression refuses over the hazard value.
		if got, err := EvaluateConstraint(hazardFloat(), expr); !errors.Is(err, ErrConstraintUnsupported) {
			t.Errorf("test %q is NOT guarded: %q answered (%v, %v) over a 2^63 float", name, expr, got, err)
			continue
		}
		exercised++
	}
	if exercised < len(exprs) {
		t.Errorf("only %d/%d registered tests were proven guarded", exercised, len(exprs))
	}
}

// TestSubscriptAndSliceBoundsMustBeLiterals closes the last two AsInt consumers,
// and the only two that are neither a filter nor a test: the VM's own subscript
// and slice operators.
//
//	value.Value.GetItem   value.go:1174-1221   key.AsInt() per payload arm
//	State.evalSlice       state.go:2919-2939   start, stop AND step
//
// Neither can be wrapped — the engine runs them between evaluating the index and
// using it — so the bound is structural, in the same posture as the arithmetic
// gate: inside `[...]`, only literals. The reachable case needs no hazardous
// input at all; it manufactures the float in the expression, and every source
// token in it is short enough to pass the numeric-token gate.
func TestSubscriptAndSliceBoundsMustBeLiterals(t *testing.T) {
	const bigFloat = `(("9223372036854" ~ "775808")|float)`
	list := ListValue([]ConstraintValue{IntValue(1), IntValue(2), IntValue(3)})
	str := StringValue("abcdef")

	for name, tc := range map[string]struct {
		this ConstraintValue
		expr string
	}{
		// The reviewer's exact case, then the other two slice bounds.
		"slice start":     {NullValue(), `[1,2,3][` + bigFloat + `:]|length == 3`},
		"slice stop":      {NullValue(), `[1,2,3][:` + bigFloat + `]|length == 3`},
		"slice step":      {NullValue(), `[1,2,3][::` + bigFloat + `]|length == 3`},
		"direct index":    {NullValue(), `[1,2,3][` + bigFloat + `] == 1`},
		"index over this": {list, `this[` + bigFloat + `] == 1`},
		"slice over this": {str, `this[` + bigFloat + `:] == "a"`},
		// Any DERIVED bound, hazardous or not — the profile does not evaluate it
		// to find out.
		"filter-derived index": {list, `this[this|length] == 1`},
		"filter-derived bound": {list, `this[0:this|length]|length == 3`},
		"int-cast index":       {list, `this["9007199254740993"|int] == 1`},
		// Nested brackets are not analysed, so they refuse.
		"nested brackets": {NullValue(), `[[1],[2]][0][0] == 1`},
	} {
		if got, err := EvaluateConstraint(tc.this, tc.expr); !errors.Is(err, ErrConstraintUnsupported) {
			t.Errorf("%s: %q answered (%v, %v); a subscript or slice bound reaches Value.AsInt inside the VM",
				name, tc.expr, got, err)
		}
	}

	// Literal bounds stay admitted — the guard is about PROVENANCE, not about
	// banning indexing. A source integer token is already capped at 15 digits by
	// everyNumericTokenIsProvablySmall, so it cannot reach the conversion
	// boundary.
	for name, tc := range map[string]struct {
		this ConstraintValue
		expr string
		want bool
	}{
		"literal index":     {NullValue(), `[1,2,3][0] == 1`, true},
		"slice from":        {NullValue(), `[1,2,3][1:]|length == 2`, true},
		"slice to":          {NullValue(), `[1,2,3][:2]|length == 2`, true},
		"slice step":        {NullValue(), `[1,2,3][::2]|length == 2`, true},
		"all three bounds":  {NullValue(), `[1,2,3][0:3:1]|length == 3`, true},
		"list literal":      {NullValue(), `[1,2,3]|length == 3`, true},
		"string list":       {NullValue(), `"a" in ["a","b"]`, true},
		"index over this":   {ListValue([]ConstraintValue{IntValue(1), IntValue(2), IntValue(3)}), `this[0] == 1`, true},
		"slice over this":   {ListValue([]ConstraintValue{IntValue(1), IntValue(2), IntValue(3)}), `this[1:]|length == 2`, true},
		"string index":      {StringValue("abcdef"), `this[0] == "a"`, true},
		"string slice":      {StringValue("abcdef"), `this[1:3] == "bc"`, true},
		"mapping subscript": {MapValue([]ConstraintEntry{{Key: "k", Value: IntValue(7)}}), `this["k"] == 7`, true},

		// ROUND 15 controls. A LIST LITERAL may still hold non-integer elements —
		// only a SUBSCRIPT or SLICE bound is integer-only, because only those
		// reach evalSlice/GetItem.
		"list literal with a float": {NullValue(), `[1,2.5]|sum == 3.5`, true},
		"list literal of floats":    {NullValue(), `[1.5,2.5]|sum == 4.0`, true},
		// Omitted bounds are what both engines call omitted.
		"open slice": {NullValue(), `[1,2,3][:]|length == 3`, true},
		"unit step":  {NullValue(), `[1,2,3][::1]|length == 3`, true},
		// A string KEY in a direct subscript has no conversion to disagree about:
		// GetItem's map arm reads AsString and its sequence arms simply find no
		// item. Measured live against stock on a sequence as well as a mapping.
		"string key on a sequence": {NullValue(), `[1,2,3]["x"] == 1`, false},
	} {
		got, err := EvaluateConstraint(tc.this, tc.expr)
		if err != nil {
			t.Errorf("%s: %q was refused (%v); a literal bound is provably in range", name, tc.expr, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s: %q = %v, want %v", name, tc.expr, got, tc.want)
		}
	}
}

// TestBracketScannerFailsClosed exercises the scanner itself, because rounds 5
// and 6 established that a hand scanner which allows what it does not model is
// the bug rather than the fix. Every shape it cannot classify must refuse.
func TestBracketScannerFailsClosed(t *testing.T) {
	for _, expr := range []string{
		`[1,2,3][0`,            // unbalanced open
		`[1,2,3]0]`,            // unbalanced close
		`[[1,2],[3]]|length`,   // nested
		`["a\"b"][0]`,          // an escape inside a string
		`["unterminated][0]`,   // unterminated string
		`[this][0]`,            // an identifier as a bracket element
		`[1|abs][0]`,           // a filter inside brackets
		`[1,2,3][0|int]`,       // a filter as the index
		`[1,2,3][ "a" ~ "b" ]`, // a concatenation as the index
		`[1,2,3][1.5:]`,        // a fractional slice bound
		`[1,2,3]["x":]`,        // a string slice bound
		`[1,2,3][0:1:2:3]`,     // more terms than start:stop:step
	} {
		if got, err := EvaluateConstraint(NullValue(), expr); !errors.Is(err, ErrConstraintUnsupported) {
			t.Errorf("%q answered (%v, %v); the bracket scan must fail closed on anything it cannot classify",
				expr, got, err)
		}
	}
}

// TestBuiltinArgumentParity is the ARGUMENT half of the proven-parity posture.
//
// Rounds 10-15 bounded what a value's MAGNITUDE could do. These diverge at
// perfectly ordinary magnitudes, because the two engines also disagree about
// ARITY, about COERCION, and about what happens at an EDGE value:
//
//	1.5 is divisibleby(0.5)          native false, stock TRUE  (stock has an f64 branch)
//	[1,2,3]|slice(0)|length == 1     native true,  stock ERRORS (usize count == 0)
//	[1,2,3]|slice(1.5)|length == 1   native true,  stock ERRORS (usize rejects 1.5)
//	"aaa"|replace("a","b",1)         native "baa", stock ERRORS (TooManyArguments)
//
// Each is declined rather than reimplemented, per the standing preference: the
// port-only default, arity and coercion paths are not proven identical, so they
// are outside the profile.
func TestBuiltinArgumentParity(t *testing.T) {
	str := StringValue("abcdef")
	for name, tc := range map[string]struct {
		this ConstraintValue
		expr string
	}{
		// P1.1 — divisibleby's f64 branch exists only in stock.
		"divisibleby, both fractional":    {NullValue(), `1.5 is divisibleby(0.5)`},
		"divisibleby, fractional subject": {NullValue(), `1.5 is divisibleby(1)`},
		"divisibleby, fractional divisor": {NullValue(), `4 is divisibleby(1.5)`},
		// P1.2 — a count that minijinja-Go defaults and stock rejects.
		"slice(0)":    {NullValue(), `[1,2,3]|slice(0)|length == 1`},
		"slice(1.5)":  {NullValue(), `[1,2,3]|slice(1.5)|length == 1`},
		"batch(0)":    {NullValue(), `[1,2,3]|batch(0)|length == 1`},
		"batch(1.5)":  {NullValue(), `[1,2,3]|batch(1.5)|length == 1`},
		"truncate(0)": {str, `this|truncate(0) != ""`},
		"indent(1.5)": {str, `this|indent(1.5) != ""`},
		// P1.2 — arity, and keyword arguments.
		"replace with a count": {NullValue(), `"aaa"|replace("a","b",1) == "baa"`},
		"format with an extra": {NullValue(), `1|format("%d","x") == "1"`},
		"tojson(indent=1.5)":   {NullValue(), `[1,2]|tojson(indent=1.5) != ""`},
		"sum(attribute=)":      {NullValue(), `[{"a":1}]|sum(attribute="a") == 1`},
	} {
		if got, err := EvaluateConstraint(tc.this, tc.expr); !errors.Is(err, ErrConstraintUnsupported) {
			t.Errorf("%s: %q answered (%v, %v); this arity/coercion path is not proven identical to stock",
				name, tc.expr, got, err)
		}
	}

	// The proven forms must still decide — the guard is about the SHAPE of a
	// call, not a ban on arguments.
	for name, tc := range map[string]struct {
		this ConstraintValue
		expr string
		want bool
	}{
		"divisibleby, both ints": {NullValue(), `9 is divisibleby(3)`, true},
		"divisibleby, false":     {NullValue(), `9 is divisibleby(2)`, false},
		// An INTEGRAL float is admitted however it is spelled: AsInt promotes it,
		// so Go computes `4 % 2` where stock computes `4.0 % 2.0`, and below 2^53
		// those agree exactly. Only the non-integral branch is unrepresented.
		"divisibleby, integral float subject": {NullValue(), `4.0 is divisibleby(2)`, true},
		"divisibleby, integral float divisor": {NullValue(), `4 is divisibleby(2.0)`, true},
		"slice(2)":                            {NullValue(), `[1,2,3]|slice(2)|length == 2`, true},
		"batch(2)":                            {NullValue(), `[1,2,3]|batch(2)|length == 2`, true},
		"replace, two args":                   {NullValue(), `"aaa"|replace("a","b") == "bbb"`, true},
		"join":                                {NullValue(), `["a","b"]|join(",") == "a,b"`, true},
		"upper":                               {StringValue("hi"), `this|upper == "HI"`, true},
	} {
		got, err := EvaluateConstraint(tc.this, tc.expr)
		if err != nil {
			t.Errorf("%s: %q was refused (%v); the parity guard is over-broad", name, tc.expr, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s: %q = %v, want %v", name, tc.expr, got, tc.want)
		}
	}
}

// TestNegativeBracketBoundsAreNotArithmetic pins the round-15 over-decline the
// review flagged as P2.1.
//
// `[1,2,3][-1:]|length == 1` carries a `-`, but it is BRACKET SYNTAX — a
// negative bound, which the region check has already proved is a bare `-?d+` —
// not arithmetic. The global byte check sent the whole expression to
// parseNumeric because of it, and parseNumeric cannot parse a list-and-filter
// expression, so a form stock answers TRUE on was refused. The arithmetic gate
// now runs over the expression with VALIDATED bracket regions blanked out,
// which is safe precisely because they were validated first: only literals and
// separators are removed, so no binary operator can hide in them.
func TestNegativeBracketBoundsAreNotArithmetic(t *testing.T) {
	list := ListValue([]ConstraintValue{IntValue(1), IntValue(2), IntValue(3)})
	for expr, want := range map[string]bool{
		`[1,2,3][-1:]|length == 1`:  true,
		`[1,2,3][-2:]|length == 2`:  true,
		`[1,2,3][:-1]|length == 2`:  true,
		`[1,2,3][::-1]|length == 3`: true,
		`[1,2,3][-1] == 3`:          true,
		`[1,-2,3]|length == 3`:      true,
	} {
		got, err := EvaluateConstraint(NullValue(), expr)
		if err != nil {
			t.Errorf("%q was refused (%v); a negative bound is bracket syntax, not arithmetic", expr, err)
			continue
		}
		if got != want {
			t.Errorf("%q = %v, want %v", expr, got, want)
		}
	}
	if got, err := EvaluateConstraint(list, `this[-1:]|length == 1`); err != nil || !got {
		t.Errorf(`this[-1:] over a list = (%v, %v), want (true, nil)`, got, err)
	}

	// REAL arithmetic outside a bracket stays gated exactly as before, and a
	// derived bound inside one stays refused.
	for _, expr := range []string{
		`[1,2,3][0] - 1 == 0`,           // arithmetic outside the bracket
		`[1,2,3][this|length - 1] == 3`, // a derived bound
	} {
		if _, err := EvaluateConstraint(list, expr); !errors.Is(err, ErrConstraintUnsupported) {
			t.Errorf("%q must stay gated; got err=%v", expr, err)
		}
	}
}
