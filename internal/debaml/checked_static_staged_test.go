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

// De-BAML Slice 7.2c-2 — the STAGED mapper proof.
//
// # What "staged" means, exactly
//
// The production mapper [staticCheckedMap] is reached only through a
// [staticCheckedProfile] that [staticCheckedProfileOf] produced, and that
// classifier's allowed-operator manifest is `>` and only `>`. So the five other
// direct operators cannot reach the mapper in production, and this slice does not
// change that: no route is opened, no generated return fixture is touched, and the
// served row count stays 4.
//
// What this file does is CONSTRUCT the profile directly — the one thing a test in
// this package can do and a production caller cannot — and drive the real mapper
// with it. That is the preparation 7.2c-3 needs: it answers "when the manifest is
// widened, does the mapper already produce stock's bytes?" while leaving the
// manifest closed. TestStagedMapperOperatorsAreStillDeclined runs beside every
// staged row and proves the same operator is refused by every production gate and
// by the one claiming route.
//
// # The authority
//
// The literals below are STOCK v0.223.0's own output, captured by Slice 7.2c-1 in
// internal/debaml/predicatewire: raw sonic.Marshal bytes of the decoded CFFI result
// for the two name-pinned classes, and the whole unmodified err.Error() for a false
// assert. They are copied here so this proof runs in the ordinary, CGO-free lane,
// and [TestStagedOperatorCapturesAgreeWithPredicatewire] parses that package's
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

// stagedOperatorCapture is stock's four outputs for one operator on the two
// name-pinned nested families, at the canonical literal `0`.
//
// The field names deliberately match internal/debaml/predicatewire's
// pwOperatorCapture, because the agreement guard pairs them by name.
type stagedOperatorCapture struct {
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

// stagedOperatorCaptures is the 7.2c-1 CFFI corpus for all six direct operators —
// 6 operators × (check pass, check fail, assert pass, assert fail) = 24 rows.
//
// One literal (`0`) across all six is deliberate and comes from 7.2c-1: it makes
// every byte difference between two operator captures attributable to the OPERATOR
// and to nothing else.
var stagedOperatorCaptures = map[string]stagedOperatorCapture{
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

// stagedLabel is the constraint label every captured project uses.
const stagedLabel = "positive"

// stagedLiteral is the canonical `I` every captured project compares against.
const stagedLiteral = 0

// stagedRaw is the assistant text stock was given for one nested drive — the same
// shape internal/debaml/predicatewire's pwNestedRaw builds.
func stagedRaw(confidence int64) string {
	return fmt.Sprintf(`{"answer": "sunny", "confidence": %d}`, confidence)
}

// stagedProfile builds the classification the mapper consumes DIRECTLY, bypassing
// [staticCheckedProfileOf].
//
// This is the whole staging mechanism, and it is deliberately the only thing that
// is bypassed: everything downstream — the evaluator, the carrier, the splice, the
// assertion renderer — is the production code path, unmodified. A profile the
// classifier would refuse is exactly what a widened manifest would hand the mapper
// in 7.2c-3, which is what makes driving one now a preparation rather than a
// simulation.
func stagedProfile(level schema.ConstraintLevel, op directCompareOp) staticCheckedProfile {
	className := staticCheckedCheckClass
	if level == schema.ConstraintAssert {
		className = staticCheckedAssertClass
	}
	return staticCheckedProfile{
		className:  className,
		level:      level,
		label:      stagedLabel,
		expression: directI64Expression(op, stagedLiteral),
	}
}

// ---------------------------------------------------------------------------
// The staged drive
// ---------------------------------------------------------------------------

// TestStagedMapperReproducesStockBytesForEveryOperator is the staged acceptance
// comparison: for each of the six direct operators and each of the four
// serving-shaped outcomes, the PRODUCTION mapper's raw bytes (or its exact error
// text) equal what stock v0.223.0 produced for the same declaration and the same
// assistant text.
//
// 24 rows. Byte equality on whole outputs — no substring match, no normalisation,
// no truthiness.
func TestStagedMapperReproducesStockBytesForEveryOperator(t *testing.T) {
	rows := 0
	for _, op := range directCompareOperators() {
		cap := stagedCaptureOf(t, op)
		expr := directI64Expression(op, stagedLiteral)
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
				b := staticCheckedBundle(schema.ConstraintCheck, stagedLabel, expr)
				res, err := staticCheckedMap(b, stagedProfile(schema.ConstraintCheck, op), stagedRaw(tc.value))
				if err != nil {
					t.Fatalf("%s: the staged mapper REFUSED %q over confidence=%d: %v",
						tc.name, expr, tc.value, err)
				}
				if string(res.JSON) != tc.want {
					t.Errorf("%s: staged mapper bytes differ from stock's\n got %s\nwant %s",
						tc.name, res.JSON, tc.want)
				}
				rows++
			}

			// A PASSING @assert leaves no trace: the canonical bytes, unchanged.
			ab := staticCheckedBundle(schema.ConstraintAssert, stagedLabel, expr)
			res, err := staticCheckedMap(ab, stagedProfile(schema.ConstraintAssert, op), stagedRaw(cap.trueVal))
			if err != nil {
				t.Fatalf("assert_pass: the staged mapper REFUSED %q over confidence=%d: %v",
					expr, cap.trueVal, err)
			}
			if string(res.JSON) != cap.assertTrue {
				t.Errorf("assert_pass: staged mapper bytes differ from stock's\n got %s\nwant %s",
					res.JSON, cap.assertTrue)
			}
			rows++

			// A FALSE @assert emits NO value and stock's exact error text.
			res, err = staticCheckedMap(ab, stagedProfile(schema.ConstraintAssert, op), stagedRaw(cap.falseVal))
			if err == nil {
				t.Fatalf("assert_fail: the staged mapper SERVED %s where stock rejects the value", res.JSON)
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
				t.Errorf("assert_fail: staged error text differs from stock's\n got %s\nwant %s",
					strconv.Quote(err.Error()), strconv.Quote(cap.assertFail))
			}
			rows++
		})
	}
	if rows != 24 {
		t.Fatalf("the staged drive covered %d rows, want 6 operators x 4 outcomes = 24", rows)
	}
	t.Logf("staged mapper: 6 operators x 4 outcomes = %d rows reproduced byte-for-byte from the "+
		"7.2c-1 stock CFFI corpus, with the production manifest still %v", rows, staticCheckedManifestTokens())
}

// stagedCaptureOf returns one operator's capture, failing loudly if an operator has
// none — so an operator added to the capability without stock evidence cannot ride
// through this file silently.
func stagedCaptureOf(t *testing.T, op directCompareOp) stagedOperatorCapture {
	t.Helper()
	c, ok := stagedOperatorCaptures[op.ID]
	if !ok {
		t.Fatalf("operator %q (%s) has no pinned stock capture; a staged row for it would rest on nothing",
			op.ID, op.Token)
	}
	if c.trueVal == c.falseVal {
		t.Fatalf("operator %q drives the same value for both outcomes, so its captures cannot differ", op.ID)
	}
	// The capture really is this operator's: stock retained the operator's own text.
	want := directI64Expression(op, stagedLiteral)
	for _, s := range []string{c.checkTrue, c.checkFalse, c.assertFail} {
		if !strings.Contains(s, want) {
			t.Fatalf("operator %q's capture does not quote %q; the corpus rows may be mispaired",
				op.ID, want)
		}
	}
	return c
}

// TestStagedMapperOutcomesAreDiscriminating is the non-vacuity control for the
// drive above: the 24 rows must be 24 DISTINCT strings where distinctness is the
// point, so a copy-paste error in the corpus cannot make several operators pass on
// one another's bytes.
func TestStagedMapperOutcomesAreDiscriminating(t *testing.T) {
	seen := map[string][]string{}
	for id, c := range stagedOperatorCaptures {
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
	for id, c := range stagedOperatorCaptures {
		if want := fmt.Sprintf(`{"answer":"sunny","confidence":%d}`, c.trueVal); c.assertTrue != want {
			t.Errorf("%s.assertTrue = %s, want %s", id, c.assertTrue, want)
		}
	}
	if len(stagedOperatorCaptures) != 6 {
		t.Fatalf("the corpus carries %d operators, want 6", len(stagedOperatorCaptures))
	}
}

// TestStagedMapperCorpusIsProvenToBite mutates each captured byte string in turn
// and requires the staged comparison to reject it.
//
// Without it, "the mapper reproduces stock's bytes" would be a claim that could be
// satisfied by a comparison that never discriminates.
func TestStagedMapperCorpusIsProvenToBite(t *testing.T) {
	gt := mustOpByToken(t, ">")
	cap := stagedOperatorCaptures[gt.ID]
	expr := directI64Expression(gt, stagedLiteral)
	b := staticCheckedBundle(schema.ConstraintCheck, stagedLabel, expr)
	res, err := staticCheckedMap(b, stagedProfile(schema.ConstraintCheck, gt), stagedRaw(cap.trueVal))
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
// The staged code cannot broaden a claim
// ---------------------------------------------------------------------------

// TestStagedMapperOperatorsAreStillDeclined runs beside the staged drive and proves
// what the staging did NOT do: every operator except `>` is refused by every
// production schema gate, by the classifier, and by the ONE route that carries the
// claim capability.
//
// It drives the exact same expressions the staged rows reproduce stock's bytes for,
// so "the mapper can produce these bytes" and "production will not ask it to" are
// asserted over one set of inputs rather than two.
func TestStagedMapperOperatorsAreStillDeclined(t *testing.T) {
	declined, served := 0, 0
	for _, op := range directCompareOperators() {
		expr := directI64Expression(op, stagedLiteral)
		for _, level := range []schema.ConstraintLevel{schema.ConstraintCheck, schema.ConstraintAssert} {
			b := staticCheckedBundle(level, stagedLabel, expr)
			cap := stagedOperatorCaptures[op.ID]

			profileOK := staticCheckedFingerprintAdmits(b)
			familyOK := IsAdmittedStaticCheckedFamily(b)
			supportErr := SupportsNativeFinalBundle(b)
			_, threshOK := staticCheckedThreshold(expr)
			_, routeErr := ParseStaticBundleUnaryCall(t.Context(), b, stagedRaw(cap.trueVal))
			_, directErr := ParseStaticBundle(t.Context(), b, stagedRaw(cap.trueVal))

			if op.Token == ">" {
				if !profileOK || !familyOK || supportErr != nil || !threshOK || routeErr != nil {
					t.Errorf("the ADMITTED predicate %q at %v is no longer served (profile=%v family=%v "+
						"support=%v threshold=%v route=%v)",
						expr, level, profileOK, familyOK, supportErr, threshOK, routeErr)
				}
				// Even the admitted one stays declined on a DIRECT route.
				if directErr == nil {
					t.Errorf("%q at %v was claimed by the DIRECT parse route", expr, level)
				}
				served++
				continue
			}
			if profileOK || familyOK || threshOK {
				t.Errorf("%q at %v is ADMITTED by a schema gate (profile=%v family=%v threshold=%v); "+
					"the staged mapper must not have widened the claim",
					expr, level, profileOK, familyOK, threshOK)
			}
			if supportErr == nil {
				t.Errorf("SupportsNativeFinalBundle ADMITTED %q at %v", expr, level)
			}
			if routeErr == nil {
				t.Errorf("the claiming route SERVED %q at %v", expr, level)
			}
			if directErr == nil {
				t.Errorf("the direct route SERVED %q at %v", expr, level)
			}
			declined++
		}
	}
	if served != 2 || declined != 10 {
		t.Fatalf("%d served rows and %d declined rows, want 2 and 10 (the one admitted operator on "+
			"both levels, and the five declined ones)", served, declined)
	}
}

// ---------------------------------------------------------------------------
// Mapper-level totality
// ---------------------------------------------------------------------------

// TestStagedMapperIsTotalOverTheI64Range is the mapper's half of the totality
// claim: for every direct operator and every i64 boundary value the wire can carry,
// the mapper produces a carrier or a rendered assertion error, and NEVER a decline.
//
// The evaluator's totality (constraint_direct_i64_test.go) is necessary but not
// sufficient — [staticCheckedInt] has to hand it the exact value first, and the
// splice has to survive the byte-parity rebuild afterwards. This drives the whole
// chain at MinInt64, MaxInt64 and both sides of ±2^53, which is where an i64 that
// went through a float64 anywhere would show up as a changed value rather than an
// error.
func TestStagedMapperIsTotalOverTheI64Range(t *testing.T) {
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
			prof := stagedProfile(schema.ConstraintCheck, op)
			prof.expression = expr
			b := staticCheckedBundle(schema.ConstraintCheck, stagedLabel, expr)
			for _, v := range values {
				rows++
				res, err := staticCheckedMap(b, prof, stagedRaw(v))
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

// stagedAuthorityFields names, for each field of [stagedOperatorCapture], the field
// of internal/debaml/predicatewire's pwOperatorCapture it must equal. Declared as
// data so the guard cannot be satisfied by a pairing that quietly went missing.
var stagedAuthorityFields = map[string]string{
	"checkTrue":  "checkTrue",
	"checkFalse": "checkFalse",
	"assertTrue": "assertTrue",
	"assertFail": "assertFail",
}

// TestStagedOperatorCapturesAgreeWithPredicatewire parses
// internal/debaml/predicatewire's source and proves every literal copied into this
// file is byte-identical to the stock capture it came from.
//
// It is the same mechanism [TestStaticCheckedStockAuthorityAgrees] applies to
// checkedwire, and it exists for the same reason: this file's proof runs in the
// ordinary CGO-free lane while the capture lives behind `//go:build integration`,
// and an untagged copy that could drift from the tagged original would be a proof
// about nothing. Build tags do not affect the parser, so the tagged file is
// readable from an untagged run.
func TestStagedOperatorCapturesAgreeWithPredicatewire(t *testing.T) {
	authority := stagedParsePredicatewireCaptures(t)
	if len(authority) == 0 {
		t.Fatal("no captures were read from internal/debaml/predicatewire; this guard would be vacuous")
	}
	if len(authority) != len(stagedOperatorCaptures) {
		t.Fatalf("predicatewire pins %d operator captures and this file copies %d; a capture would be "+
			"unguarded or a copy would have no authority", len(authority), len(stagedOperatorCaptures))
	}
	for id, mine := range stagedOperatorCaptures {
		theirs, ok := authority[id]
		if !ok {
			t.Errorf("predicatewire no longer pins operator %q, so its copy here has no stock authority", id)
			continue
		}
		got := map[string]string{
			"checkTrue": mine.checkTrue, "checkFalse": mine.checkFalse,
			"assertTrue": mine.assertTrue, "assertFail": mine.assertFail,
		}
		if len(got) != len(stagedAuthorityFields) {
			t.Fatalf("%d fields are compared but %d pairings are declared", len(got), len(stagedAuthorityFields))
		}
		for local, remote := range stagedAuthorityFields {
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
	if stagedOperatorCaptures["gt"].checkTrue == stagedOperatorCaptures["ge"].checkTrue {
		t.Fatal("two operators' captures are identical, so the comparison discriminates nothing")
	}
}

// stagedParsePredicatewireCaptures reads pwOperatorCaptures out of
// internal/debaml/predicatewire's source as a map of operator id → field → literal.
//
// It walks the composite literal directly rather than matching text, so a capture
// added, renamed or reshaped there surfaces here as a missing pairing instead of a
// silent pass.
func stagedParsePredicatewireCaptures(t *testing.T) map[string]map[string]string {
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
