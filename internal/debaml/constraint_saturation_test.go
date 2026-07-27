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
// THE PROPERTY: over randomly generated arithmetic, if the EXACT value of any
// INTEGER sub-expression reaches 2^53, EvaluateConstraint must refuse. Float
// sub-expressions are deliberately exempt — both engines are IEEE-754 f64 there,
// which is why a float result may not clear the bit but may still be answered
// when nothing before it crossed.
//
// The model is exact: integers are math/big, `//` and `%` use big.Int Div/Mod
// (Euclidean, matching stock's checked_div_euclid / checked_rem_euclid), and the
// float handoff is a genuine float64 conversion. Only one direction is asserted
// — crossing implies refusal — because refusing MORE than necessary is the
// documented, measured cost of the whitelist, not a defect.

// exact is the reference value of a generated sub-expression.
type exact struct {
	isFloat bool
	i       *big.Int // valid when !isFloat
	f       float64  // valid when isFloat
	// crossed is true when this sub-expression, or any sub-expression under it,
	// was an INTEGER whose exact magnitude reached 2^53.
	crossed bool
}

func (e exact) float() float64 {
	if e.isFloat {
		return e.f
	}
	f, _ := new(big.Float).SetInt(e.i).Float64()
	return f
}

func (e exact) isZero() bool {
	if e.isFloat {
		return e.f == 0
	}
	return e.i.Sign() == 0
}

// twoP53 is the first integer minijinja-Go's float64 arithmetic can no longer
// distinguish from its neighbour.
var twoP53 = big.NewInt(int64(maxExactInt))

func crossedAt(v *big.Int) bool {
	return new(big.Int).Abs(v).Cmp(twoP53) >= 0
}

// intResult builds an integer node, setting crossed from its operands and from
// its own magnitude.
func intResult(v *big.Int, a, b exact) exact {
	return exact{i: v, crossed: a.crossed || b.crossed || crossedAt(v)}
}

// floatResult builds a float node. Its own magnitude is irrelevant — f64 agrees
// between the engines — but the operands' crossing is STICKY, which is the whole
// subject of this test.
func floatResult(v float64, a, b exact) exact {
	return exact{isFloat: true, f: v, crossed: a.crossed || b.crossed}
}

// intLiterals are all <= 15 digits, so they pass the token gate and the
// interesting crossings come from ARITHMETIC rather than from a literal the
// profile would reject on sight. 999999999999999 is just under 2^53, so two of
// them stay in range and three do not.
var intLiterals = []string{"0", "1", "2", "3", "7", "10", "99999999", "123456789", "999999999999999", "999999999999", "4503599627370"}

var floatLiterals = []string{"0.0", "1.0", "1.5", "2.0", "0.5", "999999999999.5"}

type exprGen struct {
	rng *rand.Rand
}

// leaf emits a literal.
func (g *exprGen) leaf() (string, exact) {
	if g.rng.Intn(4) == 0 {
		lit := floatLiterals[g.rng.Intn(len(floatLiterals))]
		f, err := strconv.ParseFloat(lit, 64)
		if err != nil {
			panic("bad float literal in the generator pool: " + lit)
		}
		return lit, exact{isFloat: true, f: f}
	}
	lit := intLiterals[g.rng.Intn(len(intLiterals))]
	v, _ := new(big.Int).SetString(lit, 10)
	return lit, exact{i: v, crossed: crossedAt(v)}
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
		// cannot disagree about how it binds against `**`.
		s, v := g.term(depth - 1)
		if v.isFloat {
			return "(-" + s + ")", exact{isFloat: true, f: -v.f, crossed: v.crossed}
		}
		neg := new(big.Int).Neg(v.i)
		return "(-" + s + ")", exact{i: neg, crossed: v.crossed || crossedAt(neg)}
	case 1:
		// A power with a small non-negative integer LITERAL exponent — the only
		// exponent shape the whitelist admits, so this exercises the accepted path
		// rather than bouncing off the literal check.
		base, bv := g.term(depth - 1)
		e := g.rng.Intn(4)
		expr := "(" + base + ") ** " + fmt.Sprint(e)
		if bv.isFloat {
			return expr, floatResult(math.Pow(bv.f, float64(e)), bv, exact{})
		}
		p := new(big.Int).Exp(bv.i, big.NewInt(int64(e)), nil)
		return expr, intResult(p, bv, exact{})
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

	mixed := lv.isFloat || rv.isFloat
	switch op {
	case "+":
		if mixed {
			return expr, floatResult(lv.float()+rv.float(), lv, rv)
		}
		return expr, intResult(new(big.Int).Add(lv.i, rv.i), lv, rv)
	case "-":
		if mixed {
			return expr, floatResult(lv.float()-rv.float(), lv, rv)
		}
		return expr, intResult(new(big.Int).Sub(lv.i, rv.i), lv, rv)
	case "*":
		if mixed {
			return expr, floatResult(lv.float()*rv.float(), lv, rv)
		}
		return expr, intResult(new(big.Int).Mul(lv.i, rv.i), lv, rv)
	case "/":
		// True division is f64 in both engines even for two integers.
		return expr, floatResult(lv.float()/rv.float(), lv, rv)
	case "//":
		if mixed {
			return expr, floatResult(math.Floor(lv.float()/rv.float()), lv, rv)
		}
		return expr, intResult(new(big.Int).Div(lv.i, rv.i), lv, rv)
	default: // "%"
		if mixed {
			return expr, floatResult(math.Mod(lv.float(), rv.float()), lv, rv)
		}
		return expr, intResult(new(big.Int).Mod(lv.i, rv.i), lv, rv)
	}
}

// predicate wraps two arithmetic terms in a comparison, because
// EvaluateConstraint requires the render to be exactly "true" or "false".
func (g *exprGen) predicate(depth int) (string, bool) {
	left, lv := g.term(depth)
	right, rv := g.term(depth)
	op := []string{"==", "!=", "<", ">", "<=", ">="}[g.rng.Intn(6)]
	return left + " " + op + " " + right, lv.crossed || rv.crossed
}

func TestSaturationIsStickyUnderRandomArithmetic(t *testing.T) {
	// A fixed seed: this is a property test, not a fuzz target. It must fail the
	// same way on every machine and in CI, which a time-seeded generator cannot
	// promise. The corpus below is large enough that the laundering shapes appear
	// many times over (the counters printed at the end prove it did not go
	// vacuous).
	g := &exprGen{rng: rand.New(rand.NewSource(20260727))}

	const iterations = 20000
	var crossedCases, decidedCases, refusedInRange int
	for i := 0; i < iterations; i++ {
		expr, crossed := g.predicate(3)
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
	if crossedCases < iterations/20 {
		t.Errorf("only %d/%d generated expressions crossed 2^53; the generator is not exercising the property",
			crossedCases, iterations)
	}
	if decidedCases < iterations/20 {
		t.Errorf("only %d/%d in-range expressions were ANSWERED; the profile may have become a blanket refusal",
			decidedCases, iterations)
	}
	t.Logf("%d crossed (all refused), %d answered, %d in-range but refused for another whitelist reason",
		crossedCases, decidedCases, refusedInRange)
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
		// expression that has already been turned into a float.
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
		"(3 * 1.0) ** 1 == 3.0":             true,
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
