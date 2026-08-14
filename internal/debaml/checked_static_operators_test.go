package debaml

import (
	"errors"
	"fmt"
	"go/ast"
	"go/token"
	"math"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/internal/schema"
)

// De-BAML Slice 7.2c-3 — the SIX-OPERATOR mapper/route proof, through PRODUCTION.
//
// # It is no longer staged
//
// Slice 7.2c-2 wrote this file as a STAGED proof: it built a [staticCheckedProfile]
// by hand, because the classifier's allowed-operator manifest was `>` alone and the
// other five operators could not reach the mapper in production at all. The staging
// was the honest way to say "the mapper already produces stock's bytes, and nothing
// admits it yet".
//
// 7.2c-3 is the cutover, so the staging is GONE and every row below now goes through
// [staticCheckedProfileOf] — the real, root-owned classifier — before it reaches
// [staticCheckedMap]. That is the point of the change rather than a tidy-up: a
// hand-built profile proves the MAPPER can render bytes, while a classified one
// proves the ADMITTED SCHEMA renders them, which is the claim the cutover actually
// makes. [TestStaticCheckedEveryOperatorIsAdmittedAtEveryGate] runs beside the
// corpus and requires every one of the six to be admitted by all five schema gates
// and served on the one claiming route — and still declined on every direct route.
//
// # The authority
//
// The literals below are STOCK v0.223.0's own output, captured by Slice 7.2c-1 in
// internal/debaml/predicatewire: raw sonic.Marshal bytes of the decoded CFFI result
// for the two name-pinned classes, and the whole unmodified err.Error() for a false
// assert. They are copied here so this proof runs in the ordinary, CGO-free lane,
// and [TestStaticCheckedOperatorCapturesAgreeWithPredicatewire] parses that package's
// source and proves every copy is byte-identical to the capture it came from — the
// same guard [TestStaticCheckedStockAuthorityAgrees] already applies to
// checkedwire's literals, for the same reason: an untagged proof must not be able
// to drift away from the tagged capture.
//
// Native output is NEVER re-fed to the CFFI. Stock produced these bytes; the mapper
// has to reproduce them.

// ---------------------------------------------------------------------------
// The corpus
// ---------------------------------------------------------------------------

// stockOperatorCapture is stock's four outputs for one operator on the two
// name-pinned nested families, at the canonical literal `0`.
//
// The field names deliberately match internal/debaml/predicatewire's
// pwOperatorCapture, because the agreement guard pairs them by name.
type stockOperatorCapture struct {
	// checkTrue / checkFalse: sonic.Marshal of the decoded CHECK family with the
	// predicate holding and failing. Both carry the value — a false @check is DATA.
	checkTrue  string
	checkFalse string
	// assertTrue: sonic.Marshal of the decoded ASSERT family with the predicate
	// holding — an ordinary int, no wrapper, no check entry.
	assertTrue string
	// assertFail: the UNMODIFIED err.Error() of the ASSERT family with the predicate
	// FALSE. The embedded `\n` is stock's Rust DEBUG escape (two bytes), never a
	// real newline.
	assertFail string
	// trueVal / falseVal are the `confidence` values that make `this OP 0` hold and
	// fail. They are part of the capture: the raw assistant text stock was given is
	// what produced the bytes above.
	trueVal  int64
	falseVal int64
}

// stockOperatorCaptures is the 7.2c-1 CFFI corpus for all six direct operators —
// 6 operators × (check pass, check fail, assert pass, assert fail) = 24 rows.
//
// One literal (`0`) across all six is deliberate and comes from 7.2c-1: it makes
// every byte difference between two operator captures attributable to the OPERATOR
// and to nothing else.
var stockOperatorCaptures = map[string]stockOperatorCapture{
	"gt": {
		checkTrue:  `{"answer":"sunny","confidence":{"value":9,"checks":{"positive":{"name":"positive","expression":"this > 0","status":"succeeded"}}}}`,
		checkFalse: `{"answer":"sunny","confidence":{"value":-1,"checks":{"positive":{"name":"positive","expression":"this > 0","status":"failed"}}}}`,
		assertTrue: `{"answer":"sunny","confidence":9}`,
		assertFail: `Failed to coerce value: ParsingError { scope: [], reason: "Failed while parsing required fields: missing=0, unparsed=1", causes: [ParsingError { scope: [], reason: "Failed to parse field confidence: <root>: Assertions failed.\n  - <root>: Failed: positive this > 0", causes: [ParsingError { scope: [], reason: "Assertions failed.", causes: [ParsingError { scope: [], reason: "Failed: positive this > 0", causes: [] }] }] }] }`,
		trueVal:    9, falseVal: -1,
	},
	"ge": {
		checkTrue:  `{"answer":"sunny","confidence":{"value":0,"checks":{"positive":{"name":"positive","expression":"this >= 0","status":"succeeded"}}}}`,
		checkFalse: `{"answer":"sunny","confidence":{"value":-1,"checks":{"positive":{"name":"positive","expression":"this >= 0","status":"failed"}}}}`,
		assertTrue: `{"answer":"sunny","confidence":0}`,
		assertFail: `Failed to coerce value: ParsingError { scope: [], reason: "Failed while parsing required fields: missing=0, unparsed=1", causes: [ParsingError { scope: [], reason: "Failed to parse field confidence: <root>: Assertions failed.\n  - <root>: Failed: positive this >= 0", causes: [ParsingError { scope: [], reason: "Assertions failed.", causes: [ParsingError { scope: [], reason: "Failed: positive this >= 0", causes: [] }] }] }] }`,
		trueVal:    0, falseVal: -1,
	},
	"lt": {
		checkTrue:  `{"answer":"sunny","confidence":{"value":-1,"checks":{"positive":{"name":"positive","expression":"this < 0","status":"succeeded"}}}}`,
		checkFalse: `{"answer":"sunny","confidence":{"value":9,"checks":{"positive":{"name":"positive","expression":"this < 0","status":"failed"}}}}`,
		assertTrue: `{"answer":"sunny","confidence":-1}`,
		assertFail: `Failed to coerce value: ParsingError { scope: [], reason: "Failed while parsing required fields: missing=0, unparsed=1", causes: [ParsingError { scope: [], reason: "Failed to parse field confidence: <root>: Assertions failed.\n  - <root>: Failed: positive this < 0", causes: [ParsingError { scope: [], reason: "Assertions failed.", causes: [ParsingError { scope: [], reason: "Failed: positive this < 0", causes: [] }] }] }] }`,
		trueVal:    -1, falseVal: 9,
	},
	"le": {
		checkTrue:  `{"answer":"sunny","confidence":{"value":0,"checks":{"positive":{"name":"positive","expression":"this <= 0","status":"succeeded"}}}}`,
		checkFalse: `{"answer":"sunny","confidence":{"value":9,"checks":{"positive":{"name":"positive","expression":"this <= 0","status":"failed"}}}}`,
		assertTrue: `{"answer":"sunny","confidence":0}`,
		assertFail: `Failed to coerce value: ParsingError { scope: [], reason: "Failed while parsing required fields: missing=0, unparsed=1", causes: [ParsingError { scope: [], reason: "Failed to parse field confidence: <root>: Assertions failed.\n  - <root>: Failed: positive this <= 0", causes: [ParsingError { scope: [], reason: "Assertions failed.", causes: [ParsingError { scope: [], reason: "Failed: positive this <= 0", causes: [] }] }] }] }`,
		trueVal:    0, falseVal: 9,
	},
	"eq": {
		checkTrue:  `{"answer":"sunny","confidence":{"value":0,"checks":{"positive":{"name":"positive","expression":"this == 0","status":"succeeded"}}}}`,
		checkFalse: `{"answer":"sunny","confidence":{"value":9,"checks":{"positive":{"name":"positive","expression":"this == 0","status":"failed"}}}}`,
		assertTrue: `{"answer":"sunny","confidence":0}`,
		assertFail: `Failed to coerce value: ParsingError { scope: [], reason: "Failed while parsing required fields: missing=0, unparsed=1", causes: [ParsingError { scope: [], reason: "Failed to parse field confidence: <root>: Assertions failed.\n  - <root>: Failed: positive this == 0", causes: [ParsingError { scope: [], reason: "Assertions failed.", causes: [ParsingError { scope: [], reason: "Failed: positive this == 0", causes: [] }] }] }] }`,
		trueVal:    0, falseVal: 9,
	},
	"ne": {
		checkTrue:  `{"answer":"sunny","confidence":{"value":9,"checks":{"positive":{"name":"positive","expression":"this != 0","status":"succeeded"}}}}`,
		checkFalse: `{"answer":"sunny","confidence":{"value":0,"checks":{"positive":{"name":"positive","expression":"this != 0","status":"failed"}}}}`,
		assertTrue: `{"answer":"sunny","confidence":9}`,
		assertFail: `Failed to coerce value: ParsingError { scope: [], reason: "Failed while parsing required fields: missing=0, unparsed=1", causes: [ParsingError { scope: [], reason: "Failed to parse field confidence: <root>: Assertions failed.\n  - <root>: Failed: positive this != 0", causes: [ParsingError { scope: [], reason: "Assertions failed.", causes: [ParsingError { scope: [], reason: "Failed: positive this != 0", causes: [] }] }] }] }`,
		trueVal:    9, falseVal: 0,
	},
}

// stockOperatorLabel is the constraint label every captured project uses.
const stockOperatorLabel = "positive"

// stockOperatorLiteral is the canonical `I` every captured project compares against.
const stockOperatorLiteral = 0

// stockOperatorRaw is the assistant text stock was given for one nested drive — the same
// shape internal/debaml/predicatewire's pwNestedRaw builds.
func stockOperatorRaw(confidence int64) string {
	return fmt.Sprintf(`{"answer": "sunny", "confidence": %d}`, confidence)
}

// staticCheckedOperatorProfile classifies one operator's bundle THROUGH PRODUCTION
// and returns the profile the mapper will consume.
//
// Slice 7.2c-2's version of this helper CONSTRUCTED the profile by hand, because the
// manifest was `>` alone and [staticCheckedProfileOf] would have refused the other
// five. 7.2c-3 removes that bypass: the profile comes from the real classifier, so a
// row that reproduces stock's bytes below is a row PRODUCTION admits, not one a test
// smuggled past the gate. If the classifier refuses, the test fails here rather than
// falling back to a hand-built profile — a silent fallback would turn the cutover's
// central assertion into a proof about the mapper alone.
//
// It also returns the bundle, so callers assert against the same object the
// classifier saw.
func staticCheckedOperatorProfile(
	t *testing.T, level schema.ConstraintLevel, expr string,
) (*schema.Bundle, staticCheckedProfile) {
	t.Helper()
	b := staticCheckedBundle(level, stockOperatorLabel, expr)
	prof, ok := staticCheckedProfileOf(b)
	if !ok {
		t.Fatalf("the PRODUCTION classifier refused %q at level %v; Slice 7.2c-3 admits all six direct "+
			"comparisons, so this row cannot be driven through a hand-built profile instead", expr, level)
	}
	// The classification really describes this row, so a mispaired capture below
	// cannot pass on a profile that says something else.
	if prof.expression != expr || prof.label != stockOperatorLabel || prof.level != level {
		t.Fatalf("the classifier returned profile{expr:%q label:%q level:%v} for %q at %v",
			prof.expression, prof.label, prof.level, expr, level)
	}
	return b, prof
}

// ---------------------------------------------------------------------------
// The 24-row drive
// ---------------------------------------------------------------------------

// TestStaticCheckedMapperReproducesStockBytesForEveryOperator is the cutover's
// acceptance comparison: for each of the six direct operators and each of the four
// serving-shaped outcomes, the PRODUCTION mapper's raw bytes (or its exact error
// text) equal what stock v0.223.0 produced for the same declaration and the same
// assistant text.
//
// 24 rows. Byte equality on whole outputs — no substring match, no normalisation,
// no truthiness.
//
// Every row is driven TWICE: once through [staticCheckedMap] with the profile the
// real classifier produced, and once through [ParseStaticBundleUnaryCall] — the one
// route that carries the claim capability, i.e. what a caller actually reaches. The
// two must produce the SAME bytes or the SAME error, because a cutover that fixed the
// mapper and left the route on the ordinary constraint-blind path would serve
// `{"answer":…,"confidence":9}` with no carrier at all and still pass a mapper-only
// proof.
func TestStaticCheckedMapperReproducesStockBytesForEveryOperator(t *testing.T) {
	rows := 0
	for _, op := range directCompareOperators() {
		cap := stockOperatorCaptureOf(t, op)
		expr := directI64Expression(op, stockOperatorLiteral)
		t.Run(op.ID, func(t *testing.T) {
			// The two @check outcomes: both emit the value.
			for _, tc := range []struct {
				name  string
				value int64
				want  string
			}{
				{"check_pass", cap.trueVal, cap.checkTrue},
				{"check_fail", cap.falseVal, cap.checkFalse},
			} {
				b, prof := staticCheckedOperatorProfile(t, schema.ConstraintCheck, expr)
				res, err := staticCheckedMap(b, prof, stockOperatorRaw(tc.value))
				if err != nil {
					t.Fatalf("%s: the mapper REFUSED the ADMITTED %q over confidence=%d: %v",
						tc.name, expr, tc.value, err)
				}
				if string(res.JSON) != tc.want {
					t.Errorf("%s: mapper bytes differ from stock's\n got %s\nwant %s",
						tc.name, res.JSON, tc.want)
				}
				staticCheckedRequireRouteAgrees(t, tc.name, b, stockOperatorRaw(tc.value), tc.want, "")
				rows++
			}

			// A PASSING @assert leaves no trace: the canonical bytes, unchanged.
			ab, aprof := staticCheckedOperatorProfile(t, schema.ConstraintAssert, expr)
			res, err := staticCheckedMap(ab, aprof, stockOperatorRaw(cap.trueVal))
			if err != nil {
				t.Fatalf("assert_pass: the mapper REFUSED the ADMITTED %q over confidence=%d: %v",
					expr, cap.trueVal, err)
			}
			if string(res.JSON) != cap.assertTrue {
				t.Errorf("assert_pass: mapper bytes differ from stock's\n got %s\nwant %s",
					res.JSON, cap.assertTrue)
			}
			staticCheckedRequireRouteAgrees(t, "assert_pass", ab, stockOperatorRaw(cap.trueVal), cap.assertTrue, "")
			rows++

			// A FALSE @assert emits NO value and stock's exact error text.
			res, err = staticCheckedMap(ab, aprof, stockOperatorRaw(cap.falseVal))
			if err == nil {
				t.Fatalf("assert_fail: the mapper SERVED %s where stock rejects the value", res.JSON)
			}
			if len(res.JSON) != 0 {
				t.Errorf("assert_fail: the mapper emitted %s alongside the error; a false assert emits "+
					"NO value", res.JSON)
			}
			if !staticCheckedIsAssertFailure(err) {
				t.Fatalf("assert_fail: %v is not the rendered assertion failure; a false assert is a "+
					"CLAIMED parse failure, never a decline", err)
			}
			if errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
				t.Error("assert_fail: the assertion error carries the DECLINE sentinel")
			}
			if err.Error() != cap.assertFail {
				t.Errorf("assert_fail: error text differs from stock's\n got %s\nwant %s",
					strconv.Quote(err.Error()), strconv.Quote(cap.assertFail))
			}
			staticCheckedRequireRouteAgrees(t, "assert_fail", ab, stockOperatorRaw(cap.falseVal), "", cap.assertFail)
			rows++
		})
	}
	if rows != 24 {
		t.Fatalf("the drive covered %d rows, want 6 operators x 4 outcomes = 24", rows)
	}
	t.Logf("SERVED MANIFEST: 6 operators x 4 outcomes = %d rows reproduced byte-for-byte from the "+
		"7.2c-1 stock CFFI corpus, through the production classifier AND the claiming route; "+
		"production manifest %v", rows, staticCheckedManifestTokens())
}

// staticCheckedRequireRouteAgrees drives the ONE claiming route over the same bundle
// and raw text and requires it to reach the same public outcome the mapper did.
//
// wantJSON and wantErr are mutually exclusive: exactly one is non-empty, which is the
// two-outcome contract an admitted fingerprint has. A DECLINE on this route is a
// separate, louder failure: the schema was admitted before any socket, so discovering
// it unservable at the route is the post-claim hazard the 7.2c scope forbids outright.
func staticCheckedRequireRouteAgrees(t *testing.T, name string, b *schema.Bundle, raw, wantJSON, wantErr string) {
	t.Helper()
	if (wantJSON == "") == (wantErr == "") {
		t.Fatalf("%s: the route expectation names %d outcomes, want exactly 1", name, 2)
	}
	res, err := ParseStaticBundleUnaryCall(t.Context(), b, raw)
	if errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
		t.Fatalf("%s: the claiming route DECLINED an ADMITTED row (%v); admission happens before the "+
			"socket, so a post-claim decline is a broken claim", name, err)
	}
	if wantErr != "" {
		if err == nil {
			t.Fatalf("%s: the claiming route SERVED %s where stock rejects the value", name, res.JSON)
		}
		if len(res.JSON) != 0 {
			t.Errorf("%s: the claiming route emitted %s alongside the error", name, res.JSON)
		}
		if !staticCheckedIsAssertFailure(err) {
			t.Fatalf("%s: the claiming route failed with %T (%v), not the rendered stock assertion",
				name, err, err)
		}
		if err.Error() != wantErr {
			t.Errorf("%s: the claiming route's error text differs from stock's\n got %s\nwant %s",
				name, strconv.Quote(err.Error()), strconv.Quote(wantErr))
		}
		return
	}
	if err != nil {
		t.Fatalf("%s: the claiming route failed on an admitted row: %v", name, err)
	}
	if string(res.JSON) != wantJSON {
		t.Errorf("%s: the claiming route's bytes differ from stock's\n got %s\nwant %s",
			name, res.JSON, wantJSON)
	}
}

// stockOperatorCaptureOf returns one operator's capture, failing loudly if an operator has
// none — so an operator added to the capability without stock evidence cannot ride
// through this file silently.
func stockOperatorCaptureOf(t *testing.T, op directCompareOp) stockOperatorCapture {
	t.Helper()
	c, ok := stockOperatorCaptures[op.ID]
	if !ok {
		t.Fatalf("operator %q (%s) has no pinned stock capture; a staged row for it would rest on nothing",
			op.ID, op.Token)
	}
	if c.trueVal == c.falseVal {
		t.Fatalf("operator %q drives the same value for both outcomes, so its captures cannot differ", op.ID)
	}
	// The capture really is this operator's: stock retained the operator's own text.
	want := directI64Expression(op, stockOperatorLiteral)
	for _, s := range []string{c.checkTrue, c.checkFalse, c.assertFail} {
		if !strings.Contains(s, want) {
			t.Fatalf("operator %q's capture does not quote %q; the corpus rows may be mispaired",
				op.ID, want)
		}
	}
	return c
}

// TestStaticCheckedOperatorCorpusIsDiscriminating is the non-vacuity control for the
// drive above: the 24 rows must be 24 DISTINCT strings where distinctness is the
// point, so a copy-paste error in the corpus cannot make several operators pass on
// one another's bytes.
func TestStaticCheckedOperatorCorpusIsDiscriminating(t *testing.T) {
	seen := map[string][]string{}
	for id, c := range stockOperatorCaptures {
		seen[c.checkTrue] = append(seen[c.checkTrue], id+".checkTrue")
		seen[c.checkFalse] = append(seen[c.checkFalse], id+".checkFalse")
		seen[c.assertFail] = append(seen[c.assertFail], id+".assertFail")
	}
	for s, owners := range seen {
		if len(owners) > 1 {
			sort.Strings(owners)
			t.Errorf("%v share the byte string %s; the operator is then not what the row proves",
				owners, strconv.Quote(s))
		}
	}
	// assertTrue is deliberately NOT required to be unique — it is a bare int and
	// several operators hold at the same value — so it is checked for the right
	// VALUE instead, which is the thing that could actually be wrong.
	for id, c := range stockOperatorCaptures {
		if want := fmt.Sprintf(`{"answer":"sunny","confidence":%d}`, c.trueVal); c.assertTrue != want {
			t.Errorf("%s.assertTrue = %s, want %s", id, c.assertTrue, want)
		}
	}
	if len(stockOperatorCaptures) != 6 {
		t.Fatalf("the corpus carries %d operators, want 6", len(stockOperatorCaptures))
	}
}

// TestStaticCheckedOperatorCorpusIsProvenToBite mutates each captured byte string in turn
// and requires the staged comparison to reject it.
//
// Without it, "the mapper reproduces stock's bytes" would be a claim that could be
// satisfied by a comparison that never discriminates.
func TestStaticCheckedOperatorCorpusIsProvenToBite(t *testing.T) {
	gt := mustOpByToken(t, ">")
	cap := stockOperatorCaptures[gt.ID]
	expr := directI64Expression(gt, stockOperatorLiteral)
	b, prof := staticCheckedOperatorProfile(t, schema.ConstraintCheck, expr)
	res, err := staticCheckedMap(b, prof, stockOperatorRaw(cap.trueVal))
	if err != nil {
		t.Fatalf("the control row was refused: %v", err)
	}
	got := string(res.JSON)
	if got != cap.checkTrue {
		t.Fatalf("the control row does not match its capture, so the mutants below prove nothing")
	}
	for _, mutant := range []struct {
		name string
		want string
	}{
		{"status flipped", strings.Replace(cap.checkTrue, "succeeded", "failed", 1)},
		{"expression respaced", strings.Replace(cap.checkTrue, "this > 0", "this>0", 1)},
		{"operator swapped", strings.Replace(cap.checkTrue, "this > 0", "this >= 0", 1)},
		{"label renamed", strings.ReplaceAll(cap.checkTrue, "positive", "negative")},
		{"value changed", strings.Replace(cap.checkTrue, `"value":9`, `"value":8`, 1)},
		{"key order swapped", `{"confidence":{"value":9,"checks":{"positive":{"name":"positive","expression":"this > 0","status":"succeeded"}}},"answer":"sunny"}`},
		{"trailing byte", cap.checkTrue + " "},
	} {
		if mutant.want == cap.checkTrue {
			t.Fatalf("the %q mutant is identical to the capture; it changes nothing", mutant.name)
		}
		if got == mutant.want {
			t.Errorf("the staged comparison would ACCEPT the %q mutant; it is not byte-discriminating",
				mutant.name)
		}
	}
}

// ---------------------------------------------------------------------------
// The cutover: admitted at every gate, on ONE route
// ---------------------------------------------------------------------------

// TestStaticCheckedEveryOperatorIsAdmittedAtEveryGate is the ADMIT side of the
// cutover, over the exact expressions the 24-row corpus reproduces stock's bytes for.
//
// Slice 7.2c-2's version of this test asserted the opposite for five of the six
// operators, and it was the thing that had to go red before the manifest could widen.
// It is inverted rather than deleted: every one of the six is now required to be
// admitted by EVERY named schema gate — the classifier, the exported nativeserve
// delegate, the support predicate, the expression profile — and SERVED on the one
// claiming route, on BOTH levels. All twelve still DECLINE on the direct route, which
// is the `/call`-only boundary the scope keeps fixed.
//
// The `>` row is not special-cased any more, and that is deliberate: after the cutover
// there is no privileged operator, so a helper that treated one differently would be
// describing the old world.
func TestStaticCheckedEveryOperatorIsAdmittedAtEveryGate(t *testing.T) {
	served, directDeclines := 0, 0
	for _, op := range directCompareOperators() {
		expr := directI64Expression(op, stockOperatorLiteral)
		for _, level := range []schema.ConstraintLevel{schema.ConstraintCheck, schema.ConstraintAssert} {
			b := staticCheckedBundle(level, stockOperatorLabel, expr)
			cap := stockOperatorCaptureOf(t, op)

			profileOK := staticCheckedFingerprintAdmits(b)
			familyOK := IsAdmittedStaticCheckedFamily(b)
			supportErr := SupportsNativeFinalBundle(b)
			_, threshOK := staticCheckedThreshold(expr)
			_, routeErr := ParseStaticBundleUnaryCall(t.Context(), b, stockOperatorRaw(cap.trueVal))
			_, directErr := ParseStaticBundle(t.Context(), b, stockOperatorRaw(cap.trueVal))

			if !profileOK || !familyOK || supportErr != nil || !threshOK || routeErr != nil {
				t.Errorf("the ADMITTED predicate %q at %v is not served (profile=%v family=%v "+
					"support=%v threshold=%v route=%v)",
					expr, level, profileOK, familyOK, supportErr, threshOK, routeErr)
				continue
			}
			served++
			// The DIRECT route stays closed for every one of them — the cutover widened
			// the SHAPE, never the route set.
			if !errors.Is(directErr, bamlutils.ErrDeBAMLParseUnsupported) {
				t.Errorf("%q at %v was claimed by the DIRECT parse route (%v)", expr, level, directErr)
				continue
			}
			directDeclines++
		}
	}
	if served != 12 || directDeclines != 12 {
		t.Fatalf("%d served rows and %d direct declines, want 12 and 12 (six operators on both levels)",
			served, directDeclines)
	}
	t.Logf("cutover admit side: %d rows (6 operators x 2 levels) admitted by every schema gate and "+
		"served on the claiming route; all %d still decline on the direct route", served, directDeclines)
}

// ---------------------------------------------------------------------------
// Mapper-level totality
// ---------------------------------------------------------------------------

// TestStaticCheckedMapperIsTotalOverTheI64Range is the mapper's half of the totality
// claim: for every direct operator and every i64 boundary value the wire can carry,
// the mapper produces a carrier or a rendered assertion error, and NEVER a decline.
//
// The evaluator's totality (constraint_direct_i64_test.go) is necessary but not
// sufficient — [staticCheckedInt] has to hand it the exact value first, and the
// splice has to survive the byte-parity rebuild afterwards. This drives the whole
// chain at MinInt64, MaxInt64 and both sides of ±2^53, which is where an i64 that
// went through a float64 anywhere would show up as a changed value rather than an
// error.
func TestStaticCheckedMapperIsTotalOverTheI64Range(t *testing.T) {
	values := []int64{
		0, 1, -1,
		(1 << 53) - 1, 1 << 53, (1 << 53) + 1,
		-((1 << 53) + 1), -(1 << 53),
		math.MaxInt64 - 1, math.MaxInt64,
		math.MinInt64 + 1, math.MinInt64,
	}
	literals := []int64{0, -1, 1 << 53, math.MinInt64, math.MaxInt64}

	rows, declines := 0, 0
	for _, op := range directCompareOperators() {
		for _, literal := range literals {
			expr := directI64Expression(op, literal)
			// Through the PRODUCTION classifier, so every literal below is one the
			// cutover really admits rather than one a hand-built profile smuggled in.
			b, prof := staticCheckedOperatorProfile(t, schema.ConstraintCheck, expr)
			for _, v := range values {
				rows++
				res, err := staticCheckedMap(b, prof, stockOperatorRaw(v))
				if err != nil {
					declines++
					t.Errorf("POST-CLAIM DECLINE: the mapper refused %q over confidence=%d: %v",
						expr, v, err)
					continue
				}
				// The value survived EXACTLY — not rounded through a float64 — and the
				// status matches the exact comparison.
				wantStatus := bamlutils.CheckFailed
				if op.Holds(v, literal) {
					wantStatus = bamlutils.CheckSucceeded
				}
				wantValue := fmt.Sprintf(`"value":%d`, v)
				if !strings.Contains(string(res.JSON), wantValue) {
					t.Errorf("%q over confidence=%d emitted %s, which does not carry the exact value %s",
						expr, v, res.JSON, wantValue)
				}
				if !strings.Contains(string(res.JSON), `"status":"`+wantStatus+`"`) {
					t.Errorf("%q over confidence=%d emitted %s, want status %q", expr, v, res.JSON, wantStatus)
				}
			}
		}
	}
	if declines != 0 {
		t.Fatalf("%d of %d staged mapper rows declined after the profile was claimed", declines, rows)
	}
	if rows != 6*len(literals)*len(values) {
		t.Fatalf("the staged totality drive covered %d rows, want %d", rows, 6*len(literals)*len(values))
	}
	t.Logf("staged mapper totality: %d operators x %d literals x %d i64 values = %d rows, 0 declines",
		6, len(literals), len(values), rows)
}

// ---------------------------------------------------------------------------
// The authority guard
// ---------------------------------------------------------------------------

// stockOperatorAuthorityFields names, for each field of [stockOperatorCapture], the field
// of internal/debaml/predicatewire's pwOperatorCapture it must equal. Declared as
// data so the guard cannot be satisfied by a pairing that quietly went missing.
var stockOperatorAuthorityFields = map[string]string{
	"checkTrue":  "checkTrue",
	"checkFalse": "checkFalse",
	"assertTrue": "assertTrue",
	"assertFail": "assertFail",
}

// TestStaticCheckedOperatorCapturesAgreeWithPredicatewire parses
// internal/debaml/predicatewire's source and proves every literal copied into this
// file is byte-identical to the stock capture it came from.
//
// It is the same mechanism [TestStaticCheckedStockAuthorityAgrees] applies to
// checkedwire, and it exists for the same reason: this file's proof runs in the
// ordinary CGO-free lane while the capture lives behind `//go:build integration`,
// and an untagged copy that could drift from the tagged original would be a proof
// about nothing. Build tags do not affect the parser, so the tagged file is
// readable from an untagged run.
func TestStaticCheckedOperatorCapturesAgreeWithPredicatewire(t *testing.T) {
	authority := stockOperatorParsePredicatewireCaptures(t)
	if len(authority) == 0 {
		t.Fatal("no captures were read from internal/debaml/predicatewire; this guard would be vacuous")
	}
	if len(authority) != len(stockOperatorCaptures) {
		t.Fatalf("predicatewire pins %d operator captures and this file copies %d; a capture would be "+
			"unguarded or a copy would have no authority", len(authority), len(stockOperatorCaptures))
	}
	for id, mine := range stockOperatorCaptures {
		theirs, ok := authority[id]
		if !ok {
			t.Errorf("predicatewire no longer pins operator %q, so its copy here has no stock authority", id)
			continue
		}
		got := map[string]string{
			"checkTrue": mine.checkTrue, "checkFalse": mine.checkFalse,
			"assertTrue": mine.assertTrue, "assertFail": mine.assertFail,
		}
		if len(got) != len(stockOperatorAuthorityFields) {
			t.Fatalf("%d fields are compared but %d pairings are declared", len(got), len(stockOperatorAuthorityFields))
		}
		for local, remote := range stockOperatorAuthorityFields {
			want, ok := theirs[remote]
			if !ok {
				t.Errorf("%s.%s has no counterpart in predicatewire's capture", id, remote)
				continue
			}
			if got[local] != want {
				t.Errorf("%s.%s has drifted from the stock capture:\n got %s\nwant %s",
					id, local, strconv.Quote(got[local]), strconv.Quote(want))
			}
		}
	}
	// NON-VACUITY: the guard must be able to tell two captures apart.
	if stockOperatorCaptures["gt"].checkTrue == stockOperatorCaptures["ge"].checkTrue {
		t.Fatal("two operators' captures are identical, so the comparison discriminates nothing")
	}
}

// stockOperatorParsePredicatewireCaptures reads pwOperatorCaptures out of
// internal/debaml/predicatewire's source as a map of operator id → field → literal.
//
// It walks the composite literal directly rather than matching text, so a capture
// added, renamed or reshaped there surfaces here as a missing pairing instead of a
// silent pass.
func stockOperatorParsePredicatewireCaptures(t *testing.T) map[string]map[string]string {
	t.Helper()
	file := staticCheckedParseSource(t, staticCheckedSourcePath(t, filepath.Join("predicatewire", "operators_test.go")))
	out := map[string]map[string]string{}
	ast.Inspect(file, func(n ast.Node) bool {
		gen, ok := n.(*ast.GenDecl)
		if !ok || gen.Tok != token.VAR {
			return true
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok || len(vs.Names) != 1 || vs.Names[0].Name != "pwOperatorCaptures" || len(vs.Values) != 1 {
				continue
			}
			lit, ok := vs.Values[0].(*ast.CompositeLit)
			if !ok {
				t.Fatal("pwOperatorCaptures is no longer a composite literal; the guard cannot read it")
			}
			for _, elt := range lit.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					t.Fatal("a pwOperatorCaptures entry is not a key/value pair")
				}
				keyLit, ok := kv.Key.(*ast.BasicLit)
				if !ok || keyLit.Kind != token.STRING {
					t.Fatal("a pwOperatorCaptures key is not a string literal")
				}
				id, err := strconv.Unquote(keyLit.Value)
				if err != nil {
					t.Fatalf("unquote pwOperatorCaptures key %s: %v", keyLit.Value, err)
				}
				body, ok := kv.Value.(*ast.CompositeLit)
				if !ok {
					t.Fatalf("pwOperatorCaptures[%q] is not a composite literal", id)
				}
				fields := map[string]string{}
				for _, f := range body.Elts {
					fkv, ok := f.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					name, ok := fkv.Key.(*ast.Ident)
					if !ok {
						continue
					}
					val, ok := fkv.Value.(*ast.BasicLit)
					if !ok || val.Kind != token.STRING {
						continue
					}
					unquoted, err := strconv.Unquote(val.Value)
					if err != nil {
						t.Fatalf("unquote pwOperatorCaptures[%q].%s: %v", id, name.Name, err)
					}
					fields[name.Name] = unquoted
				}
				if len(fields) == 0 {
					t.Fatalf("pwOperatorCaptures[%q] yielded no string fields", id)
				}
				out[id] = fields
			}
		}
		return true
	})
	return out
}

// ---------------------------------------------------------------------------
// No post-claim unsupported (the 7.2c scope's proof point 8)
// ---------------------------------------------------------------------------

// TestStaticCheckedAdmittedRowNeverReachesPostClaimUnsupported is the invariant the
// 7.2c scope states as its non-negotiable preclaim rule:
//
//	"An admitted schema must have a total, byte-proven native outcome for every value
//	 that native coercion can produce. If the evaluator may return unsupported for such
//	 a value, the schema declines before a native socket opens."
//
// It is asserted at the ROUTE, over the whole i64 range, for every admitted operator
// and both levels. That placement is the point. [TestStaticCheckedMapperIsTotalOverThe
// I64Range] proves the mapper is total, and the direct-i64 suite proves the evaluator
// is — but the claim the cutover actually makes is about what a CALLER reaches, and the
// caller reaches [ParseStaticBundleUnaryCall]. Between the two sit the strict i64
// extractor, the byte-parity splice and the assertion renderer, each of which can
// decline on its own.
//
// A decline here is a FAILING INVARIANT, never an accepted fall-through: the schema was
// admitted before any socket was opened, so discovering it unservable afterwards means
// native claimed a call it could not complete and BAML no longer has it.
func TestStaticCheckedAdmittedRowNeverReachesPostClaimUnsupported(t *testing.T) {
	values := []int64{
		0, 1, -1, 7, -7,
		(1 << 53) - 1, 1 << 53, (1 << 53) + 1,
		-((1 << 53) + 1), -(1 << 53), -((1 << 53) - 1),
		math.MaxInt64 - 1, math.MaxInt64,
		math.MinInt64 + 1, math.MinInt64,
	}
	literals := []int64{0, 1, -1, 1 << 53, -(1 << 53), math.MinInt64, math.MaxInt64}

	rows, declines, served, claimedFailures := 0, 0, 0, 0
	for _, op := range directCompareOperators() {
		for _, literal := range literals {
			expr := directI64Expression(op, literal)
			for _, level := range []schema.ConstraintLevel{schema.ConstraintCheck, schema.ConstraintAssert} {
				b := staticCheckedBundle(level, stockOperatorLabel, expr)
				// ADMISSION FIRST. If the row is not admitted, a "no decline" result
				// below would be vacuous — the route would be refusing it for a shape
				// reason and the invariant would be about nothing.
				if !staticCheckedFingerprintAdmits(b) {
					t.Fatalf("%q at %v is NOT admitted, so the post-claim invariant below is vacuous "+
						"for it", expr, level)
				}
				for _, v := range values {
					rows++
					res, err := ParseStaticBundleUnaryCall(t.Context(), b, stockOperatorRaw(v))
					switch {
					case errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported):
						declines++
						t.Errorf("POST-CLAIM DECLINE: %q at %v over confidence=%d returned the decline "+
							"sentinel AFTER the schema was admitted: %v", expr, level, v, err)
					case err == nil:
						if len(res.JSON) == 0 {
							t.Errorf("%q at %v over confidence=%d succeeded with no bytes", expr, level, v)
							continue
						}
						served++
					case staticCheckedIsAssertFailure(err):
						// The one CLAIMED failure an admitted row may reach: a false
						// @assert. It carries no value, which is stock's shape.
						if len(res.JSON) != 0 {
							t.Errorf("%q at %v over confidence=%d rejected the node but emitted %s",
								expr, level, v, res.JSON)
						}
						claimedFailures++
					default:
						t.Errorf("%q at %v over confidence=%d reached neither public outcome: %T %v",
							expr, level, v, err, err)
					}
				}
			}
		}
	}
	if declines != 0 {
		t.Fatalf("%d of %d admitted rows declined at the route after admission", declines, rows)
	}
	// BOTH public outcomes must appear, or the sweep would only prove one of them is
	// reachable — and a run where every row happened to serve would say nothing about
	// the false-assert path, which is the one that returns an error.
	if served == 0 || claimedFailures == 0 {
		t.Fatalf("the sweep produced %d served and %d claimed failures; both public outcomes must be "+
			"exercised", served, claimedFailures)
	}
	if want := 6 * len(literals) * 2 * len(values); rows != want {
		t.Fatalf("the sweep covered %d rows, want %d", rows, want)
	}
	t.Logf("no post-claim unsupported: %d rows (6 operators x %d literals x 2 levels x %d i64 values) "+
		"through the CLAIMING ROUTE — %d served, %d claimed assertion failures, 0 declines",
		rows, len(literals), len(values), served, claimedFailures)
}
