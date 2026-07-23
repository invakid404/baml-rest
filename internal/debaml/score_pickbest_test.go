package debaml

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/internal/schema"
)

// M3 slice a — score model + pick_best unit tests. These pin the two crux
// pieces directly: the types.rs INHERENT score computed by the coercers, and the
// faithful array_helper::pick_best special ordering over candidate metadata.

// TestScoreModel_Leaves pins the inherent flag weights on scalar coercions.
func TestScoreModel_Leaves(t *testing.T) {
	cases := []struct {
		name  string
		run   func(*coerceFlags) error
		score int
	}{
		{"int clean", func(f *coerceFlags) error { _, e := coercePrimitiveInt(numV("5"), f); return e }, 0},
		{"int FloatToInt", func(f *coerceFlags) error { _, e := coercePrimitiveInt(numV("1.6"), f); return e }, 1},
		{"int FloatToInt from string", func(f *coerceFlags) error { _, e := coercePrimitiveInt(strVv("1.6"), f); return e }, 1},
		{"float clean", func(f *coerceFlags) error { _, e := coercePrimitiveFloat(numV("1.5"), f); return e }, 0},
		{"bool StringToBool", func(f *coerceFlags) error { _, e := coercePrimitiveBool(strVv("true"), f); return e }, 1},
		{"string clean", func(f *coerceFlags) error { _, e := coercePrimitiveString(strVv("x"), f); return e }, 0},
		{"string JsonToString", func(f *coerceFlags) error { _, e := coercePrimitiveString(numV("5"), f); return e }, 2},
		{"null clean", func(f *coerceFlags) error { _, e := coercePrimitiveNull(nullVal(), f); return e }, 0},
		{"null DefaultButHadValue", func(f *coerceFlags) error { _, e := coercePrimitiveNull(numV("5"), f); return e }, 110},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &coerceFlags{}
			if err := c.run(f); err != nil {
				t.Fatalf("coerce: %v", err)
			}
			if f.score != c.score {
				t.Errorf("score = %d, want %d", f.score, c.score)
			}
		})
	}
	// JsonToString sets the scalar-vs-composite discriminator.
	f := &coerceFlags{}
	if _, err := coercePrimitiveString(numV("5"), f); err != nil {
		t.Fatal(err)
	}
	if !f.hasJsonToString || f.kind != candScalar {
		t.Errorf("string JsonToString: hasJsonToString=%v kind=%v", f.hasJsonToString, f.kind)
	}
}

// TestScoreModel_Composite pins the inherent COMPOSITE score = own conditions
// plus every child score, on the coercers that build lists / maps / classes.
func TestScoreModel_Composite(t *testing.T) {
	// class score = ExtraKey count + field scores. Root{a int, b string}, extras.
	{
		s := &bamlutils.DynamicOutputSchema{Properties: props(kv("a", intProp()), kv("b", strProp()))}
		b, ct := classBundle(t, s, "Baml_Rest_DynamicOutput")
		f := &coerceFlags{}
		// a=1.6 -> FloatToInt(1); 3 extra keys -> ExtraKey(3); total 4.
		in := objVal(fld("a", numV("1.6")), fld("b", strVv("x")), fld("e0", numV("1")), fld("e1", numV("1")), fld("e2", numV("1")))
		if _, err := coerceClass(b, ct.Name, ct.Mode, in, f, nil); err != nil {
			t.Fatalf("coerceClass: %v", err)
		}
		if f.score != 4 || f.kind != candClass {
			t.Errorf("class score = %d (kind %v), want 4 / candClass", f.score, f.kind)
		}
		if f.classPropCount != 2 || f.classAllDefault {
			t.Errorf("classPropCount=%d classAllDefault=%v, want 2 / false", f.classPropCount, f.classAllDefault)
		}
	}
	// list score = SingleToArray(1) + item score; a non-array singleton wrapping a
	// FloatToInt element scores 1 (SingleToArray) + 1 (FloatToInt) = 2.
	{
		elem := schema.Type{Kind: schema.TypePrimitive, Primitive: schema.PrimitiveInt}
		f := &coerceFlags{}
		out, err := coerceList(nil, &elem, numV("2.6"), f, nil)
		if err != nil {
			t.Fatalf("coerceList: %v", err)
		}
		if string(out) != "[3]" {
			t.Errorf("coerceList singleton out = %s, want [3]", out)
		}
		if f.score != 2 || f.kind != candList || !f.singleToArray {
			t.Errorf("list score=%d kind=%v singleToArray=%v, want 2 / candList / true", f.score, f.kind, f.singleToArray)
		}
	}
}

// mkCand is a terse candidate builder for the pick_best tests.
func mkCand(idx int, k candKind, score int) candidate {
	return candidate{originIndex: idx, kind: k, score: score}
}

// TestToCandidate_NilSafe pins that toCandidate is nil-safe like its sibling
// coerceFlags methods: a nil receiver yields a zero-scored candidate (output +
// originIndex only), no panic. Locks the API-consistency guard.
func TestToCandidate_NilSafe(t *testing.T) {
	var f *coerceFlags
	c := f.toCandidate([]byte(`null`), 3)
	if string(c.output) != "null" || c.originIndex != 3 || c.score != 0 || c.kind != candScalar {
		t.Errorf("nil toCandidate = %+v, want output=null originIndex=3 score=0 kind=candScalar(0)", c)
	}
}

// TestPickBest_GenericOrder pins the base (default, score, index) ordering.
func TestPickBest_GenericOrder(t *testing.T) {
	// Lower score wins.
	if got, _ := pickBest(true, []candidate{mkCand(0, candScalar, 5), mkCand(1, candScalar, 2)}); got != 1 {
		t.Errorf("lower-score winner = %d, want 1", got)
	}
	// Equal score -> lower index wins.
	if got, _ := pickBest(true, []candidate{mkCand(0, candScalar, 2), mkCand(1, candScalar, 2)}); got != 0 {
		t.Errorf("tie winner = %d, want 0 (lower index)", got)
	}
	// Single candidate returns itself.
	if got, _ := pickBest(true, []candidate{mkCand(3, candScalar, 9)}); got != 0 {
		t.Errorf("single winner = %d, want 0", got)
	}
	// Empty candidate set is an error.
	if _, err := pickBest(true, nil); err == nil {
		t.Errorf("empty pick_best must error")
	}
}

// TestPickBest_NullBoundary pins the nullable score boundary: a class arm beats
// null (110) below the boundary, ties to the class (lower index) at 110, and
// loses above it.
func TestPickBest_NullBoundary(t *testing.T) {
	cls := func(score int) candidate { return candidate{originIndex: 0, kind: candClass, score: score} }
	null := nullCandidate(1)
	// 109 < 110 -> class wins.
	if got, _ := pickBest(true, []candidate{cls(109), null}); got != 0 {
		t.Errorf("class(109) vs null(110): winner %d, want 0 (class)", got)
	}
	// 110 == 110 -> tie -> lower index (class at 0) wins.
	if got, _ := pickBest(true, []candidate{cls(110), null}); got != 0 {
		t.Errorf("class(110) vs null(110): winner %d, want 0 (class, lower index)", got)
	}
	// 111 > 110 -> null wins.
	if got, _ := pickBest(true, []candidate{cls(111), null}); got != 1 {
		t.Errorf("class(111) vs null(110): winner %d, want 1 (null)", got)
	}
}

// TestPickBest_CyclicDeclines pins F1: cmpCandidates is INTRANSITIVE on mixed
// shapes (inherited from BAML's pairwise sort_by), so pickBest must NOT run an
// undefined sort — it declines when the candidate set has no consistent global
// minimum. Ordinary-class(111) < all-default-class(100) < null(110) < ordinary
// is a real cmp cycle.
func TestPickBest_CyclicDeclines(t *testing.T) {
	ordinary := candidate{originIndex: 0, kind: candClass, score: 111, classAllDefault: false}
	allDefault := candidate{originIndex: 1, kind: candClass, score: 100, classAllDefault: true}
	null := nullCandidate(2) // score 110

	// Sanity: the pairwise relation really does cycle.
	if !(cmpCandidates(true, ordinary, allDefault) < 0 &&
		cmpCandidates(true, allDefault, null) < 0 &&
		cmpCandidates(true, null, ordinary) < 0) {
		t.Fatalf("expected an A<B<C<A cmp cycle; got cmp(A,B)=%d cmp(B,C)=%d cmp(C,A)=%d",
			cmpCandidates(true, ordinary, allDefault), cmpCandidates(true, allDefault, null), cmpCandidates(true, null, ordinary))
	}

	if _, err := pickBest(true, []candidate{ordinary, allDefault, null}); err == nil {
		t.Errorf("cyclic candidate set must decline (no consistent minimum)")
	} else if !errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
		t.Errorf("cyclic decline must wrap ErrDeBAMLParseUnsupported, got %v", err)
	}

	// The SAME three candidates without the all-default cycle (all ordinary classes)
	// is a valid total order -> lowest score wins, deterministically.
	b := candidate{originIndex: 1, kind: candClass, score: 100}
	if got, _ := pickBest(true, []candidate{ordinary, b, null}); got != 1 {
		t.Errorf("non-cyclic winner = %d, want 1 (score 100)", got)
	}
}

// TestPickBest_ClaimableFamiliesTotalOrder pins that the candidate sets M3a
// actually produces never trigger the cyclic guard. A claimable union is
// HOMOGENEOUS — a homogeneous literal/scalar family (+ null) has NO composite, so
// scalar-vs-composite never fires; a flat-class-union family (+ null) has real-
// field classes (never all-default / single-implied) and a null arm with no
// JsonToString/FirstMatch, so class-vs-class and scalar-vs-composite never fire.
// Both reduce to the (score, index) total order — verify transitivity + that
// pickBest returns a winner without declining.
func TestPickBest_ClaimableFamiliesTotalOrder(t *testing.T) {
	assertTotalOrder := func(name string, set []candidate) {
		for _, a := range set {
			for _, b := range set {
				for _, c := range set {
					if cmpCandidates(true, a, b) < 0 && cmpCandidates(true, b, c) < 0 && cmpCandidates(true, a, c) >= 0 {
						t.Errorf("%s: intransitive: cmp(%d,%d)<0, cmp(%d,%d)<0, but cmp(%d,%d)>=0",
							name, a.originIndex, b.originIndex, b.originIndex, c.originIndex, a.originIndex, c.originIndex)
					}
				}
			}
		}
		if _, err := pickBest(true, set); err != nil {
			t.Errorf("%s: claimable-family set must not decline: %v", name, err)
		}
	}
	// Homogeneous literal/scalar family (+ null): all non-composite.
	assertTotalOrder("scalar/literal/null", []candidate{
		{originIndex: 0, kind: candScalar, score: 0},
		{originIndex: 1, kind: candScalar, score: 2, hasJsonToString: true},
		{originIndex: 2, kind: candScalar, score: 4}, // e.g. ObjectToString + SubstringMatch
		{originIndex: 3, kind: candNull, score: 110},
	})
	// Flat-class-union family (+ null): real-field classes, no defaults.
	assertTotalOrder("flat-class/null", []candidate{
		{originIndex: 0, kind: candClass, score: 2, classPropCount: 2},
		{originIndex: 1, kind: candClass, score: 4, classPropCount: 2},
		{originIndex: 2, kind: candClass, score: 115, classPropCount: 2},
		{originIndex: 3, kind: candNull, score: 110},
	})
}

// TestPickBest_DefaultListDevalued pins that an empty SingleToArray list (the
// "default" bit) sorts behind ANY non-default candidate regardless of score.
func TestPickBest_DefaultListDevalued(t *testing.T) {
	defaultList := candidate{originIndex: 0, kind: candList, score: 1, itemsEmpty: true, singleToArray: true}
	scalar := candidate{originIndex: 1, kind: candScalar, score: 5}
	if got, _ := pickBest(true, []candidate{defaultList, scalar}); got != 1 {
		t.Errorf("default list (score 1) vs scalar (score 5): winner %d, want 1 (non-default beats default)", got)
	}
}

// TestPickBest_ListVsList pins the list-vs-list special ordering.
func TestPickBest_ListVsList(t *testing.T) {
	// Prefer the non-SingleToArray list even at a higher score.
	single := candidate{originIndex: 0, kind: candList, score: 1, singleToArray: true, itemsEmpty: false}
	notSingle := candidate{originIndex: 1, kind: candList, score: 5, singleToArray: false}
	if got, _ := pickBest(true, []candidate{single, notSingle}); got != 1 {
		t.Errorf("single vs non-single list: winner %d, want 1 (non-single)", got)
	}
	// Prefer NON-markdown content (same single-ness).
	md := candidate{originIndex: 0, kind: candList, score: 1, firstItemMarkdown: true}
	noMd := candidate{originIndex: 1, kind: candList, score: 5, firstItemMarkdown: false}
	if got, _ := pickBest(true, []candidate{md, noMd}); got != 1 {
		t.Errorf("markdown vs non-markdown list: winner %d, want 1 (non-markdown)", got)
	}
	// Prefer the no-parse-error list over one empty ONLY due to ArrayItemParseError.
	emptyErr := candidate{originIndex: 0, kind: candList, score: 2, arrayItemErrors: 1, itemsEmpty: true}
	clean := candidate{originIndex: 1, kind: candList, score: 5, arrayItemErrors: 0, itemsEmpty: false}
	if got, _ := pickBest(true, []candidate{emptyErr, clean}); got != 1 {
		t.Errorf("empty-error vs clean list: winner %d, want 1 (clean/no-error)", got)
	}
}

// TestPickBest_ClassVsClass pins the class-vs-class devalue rules under a union
// target: an all-default class and a single-string-ImpliedKey class both sort
// behind an ordinary class regardless of score.
func TestPickBest_ClassVsClass(t *testing.T) {
	// All-default class devalued behind a non-default class.
	allDefault := candidate{originIndex: 0, kind: candClass, score: 0, classAllDefault: true}
	nonDefault := candidate{originIndex: 1, kind: candClass, score: 5, classAllDefault: false}
	if got, _ := pickBest(true, []candidate{allDefault, nonDefault}); got != 1 {
		t.Errorf("all-default vs non-default class: winner %d, want 1 (non-default)", got)
	}
	// Single-string-ImpliedKey class devalued under a UNION target.
	implied := candidate{originIndex: 0, kind: candClass, score: 0, classSingleImplied: true, classPropCount: 1}
	ordinary := candidate{originIndex: 1, kind: candClass, score: 5, classSingleImplied: false}
	if got, _ := pickBest(true, []candidate{implied, ordinary}); got != 1 {
		t.Errorf("single-implied vs ordinary class (union): winner %d, want 1 (ordinary)", got)
	}
	// The single-implied devalue does NOT fire for a NON-union target.
	if got, _ := pickBest(false, []candidate{implied, ordinary}); got != 0 {
		t.Errorf("single-implied vs ordinary class (non-union): winner %d, want 0 (lower score)", got)
	}
}

// TestPickBest_ScalarVsComposite pins that a non-composite scalar carrying
// JsonToString / FirstMatch is devalued against a composite regardless of score.
func TestPickBest_ScalarVsComposite(t *testing.T) {
	jsonStr := candidate{originIndex: 0, kind: candScalar, score: 2, hasJsonToString: true}
	composite := candidate{originIndex: 1, kind: candClass, score: 5}
	if got, _ := pickBest(true, []candidate{jsonStr, composite}); got != 1 {
		t.Errorf("JsonToString scalar vs composite: winner %d, want 1 (composite)", got)
	}
	// Symmetric: composite first, coerced scalar second.
	firstMatch := candidate{originIndex: 1, kind: candScalar, score: 2, hasFirstMatch: true}
	compositeFirst := candidate{originIndex: 0, kind: candList, score: 5}
	if got, _ := pickBest(true, []candidate{compositeFirst, firstMatch}); got != 0 {
		t.Errorf("composite vs FirstMatch scalar: winner %d, want 0 (composite)", got)
	}
	// An ORDINARY scalar (no JsonToString/FirstMatch) is NOT devalued: lower score wins.
	plainScalar := candidate{originIndex: 0, kind: candScalar, score: 2}
	if got, _ := pickBest(true, []candidate{plainScalar, composite}); got != 0 {
		t.Errorf("plain scalar (2) vs composite (5): winner %d, want 0 (lower score)", got)
	}
}

// TestNullableClassUnion_ScoreBoundary pins the end-to-end nullable score
// boundary through Parse: a Book arm carrying N extra keys (ExtraKey score N)
// against Book|Car|null claims Book below/at 110 and null above it.
func TestNullableClassUnion_ScoreBoundary(t *testing.T) {
	s := nullableClassUnionSchema()
	raw := func(n int) string {
		var b strings.Builder
		b.WriteString(`{"u":{"title":"Go","pages":300`)
		for i := 0; i < n; i++ {
			fmt.Fprintf(&b, `,"e%d":1`, i)
		}
		b.WriteString(`}}`)
		return b.String()
	}
	mustParse(t, s, raw(109), `{"u":{"title":"Go","pages":300}}`) // 109 < 110 -> Book
	mustParse(t, s, raw(110), `{"u":{"title":"Go","pages":300}}`) // 110 tie -> Book (index)
	mustParse(t, s, raw(111), `{"u":null}`)                       // 111 > 110 -> null
}
