//go:build integration

package predicatewire

import (
	"context"
	stdjson "encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/boundaryml/baml/engine/language_client_go/baml_go/shared"
	"github.com/bytedance/sonic"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/internal/debaml"
	"github.com/invakid404/baml-rest/internal/schema"
)

// RESIDUAL CHARACTERIZATION — captured, never admitted.
//
// Every form in this file is a #583 deferral. Capturing it is what turns "deferred"
// from an argument into a measurement: the 2+-check ordering question in particular is
// the single riskiest broadening dimension the scope names, and its answer is not a
// design preference but an observable property of stock's own output.
//
// Nothing here proposes an admission. [TestResidualFormsAreDeclined] proves each form is
// still refused by the production gates, and [TestResidualDeclinesAreProvenToBite] proves
// that assertion would fire if one of them started being claimed.

// pwMarshalObservations is how many times a stock value is re-serialized when asking
// whether its byte order is stable.
//
// Go randomises map iteration per range statement, so for a 2-key map the chance that N
// independent marshals all agree by luck is 2^-(N-1). At 200 that is not a number any
// test run will meet, which is what lets "only one ordering was observed" be a REPORTABLE
// event rather than an expected flake.
const pwMarshalObservations = 200

// pwDistinctMarshals serializes v repeatedly and returns the distinct byte strings
// observed, sorted so the record is deterministic.
func pwDistinctMarshals(t *testing.T, v any) []string {
	t.Helper()
	seen := map[string]bool{}
	for i := 0; i < pwMarshalObservations; i++ {
		b, err := sonic.Marshal(v)
		if err != nil {
			t.Fatalf("sonic.Marshal (observation %d): %v", i, err)
		}
		seen[string(b)] = true
	}
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// TestTwoCheckWireOrderIsUnstable is the RECORDED FACT the 7.2c scope calls its highest
// risk, measured rather than assumed.
//
// Stock's CFFI result is an ordered LIST of checks, but the public Go value folds it into
// a map[string]Check — and sonic.Marshal of a Go map follows Go's map iteration, which is
// randomised. So for two or more distinct labels there is no single byte string that IS
// "stock's output", and the slice's acceptance rule ("native's bytes equal stock's bytes")
// has nothing to compare against.
//
// That is WHY 2+ @check stays declined in 7.2c. It is not that the carrier cannot
// serialize N checks — bamlutils.Checked does so deterministically, in declaration order —
// but that a deterministic answer cannot be byte-identical to a nondeterministic one.
func TestTwoCheckWireOrderIsUnstable(t *testing.T) {
	for _, tc := range []struct {
		project string
		labels  []string
	}{
		{"res_two_checks", []string{"alpha", "beta"}},
		{"res_three_checks", []string{"alpha", "beta", "gamma"}},
	} {
		t.Run(tc.project, func(t *testing.T) {
			key := pwDriveKey{Project: tc.project, Func: pwFnChecked, Raw: pwNestedRaw(9)}
			stock := pwCheckedValue(t, key)

			// The CONTENT is stable and exact — this is not a claim that stock loses
			// data. Each declared label is present exactly once, with its own
			// expression and status.
			if len(stock.Confidence.Checks) != len(tc.labels) {
				t.Fatalf("stock reported %d checks, want %d: %v",
					len(stock.Confidence.Checks), len(tc.labels), stock.Confidence.Checks)
			}
			for _, label := range tc.labels {
				got, ok := stock.Confidence.Checks[label]
				if !ok {
					t.Fatalf("stock reported no check under the declared label %q: %v",
						label, stock.Confidence.Checks)
				}
				if got.Name != label || got.Status != "succeeded" {
					t.Fatalf("stock check %q = %+v, want name=%s status=succeeded", label, got, label)
				}
			}

			// The ORDER is not. This is the recorded observation.
			orderings := pwDistinctMarshals(t, stock)
			t.Logf("RECORDED: %d marshals of stock's %d-check result produced %d DISTINCT byte "+
				"orderings", pwMarshalObservations, len(tc.labels), len(orderings))
			for _, o := range orderings {
				t.Logf("  %s", o)
			}
			if len(orderings) < 2 {
				t.Fatalf("RECORDED: %d marshals of a %d-key stock result produced only ONE byte "+
					"ordering (%s). That is not evidence of a stable stock ordering — Go's map "+
					"iteration randomisation makes a single ordering across %d observations a ~2^-%d "+
					"event — but it does mean this run failed to witness the instability the "+
					"deferral rests on, and the observation must be re-taken rather than read as "+
					"stability.",
					pwMarshalObservations, len(tc.labels), orderings[0],
					pwMarshalObservations, pwMarshalObservations-1)
			}

			// And the DECISIVE consequence: the native carrier is deterministic, so its
			// single answer can match at most one of the orderings stock produces. There
			// is no byte-exact parity to have.
			ordered := make([]bamlutils.Check, 0, len(tc.labels))
			for _, label := range tc.labels {
				got := stock.Confidence.Checks[label]
				ordered = append(ordered, bamlutils.Check{Name: got.Name, Expression: got.Expression, Status: got.Status})
			}
			carrier, err := bamlutils.NewChecked(stock.Confidence.Value, ordered)
			if err != nil {
				t.Fatalf("NewChecked over stock's own check results: %v", err)
			}
			// The native value is marshalled inside the SAME two-field struct stock's
			// was, or the two byte sets would not be comparable at all and a "matches
			// none" record would be an artifact of the wrapper rather than a fact about
			// the ordering.
			type nativeAnswer struct {
				Answer     string                   `json:"answer"`
				Confidence bamlutils.Checked[int64] `json:"confidence"`
			}
			nativeOrderings := pwDistinctMarshals(t, nativeAnswer{Answer: stock.Answer, Confidence: carrier})
			if len(nativeOrderings) != 1 {
				t.Fatalf("the NATIVE carrier produced %d distinct orderings; it is supposed to be "+
					"deterministic, and a nondeterministic native output would be a defect in its own "+
					"right: %v", len(nativeOrderings), nativeOrderings)
			}
			t.Logf("RECORDED: the native carrier is DETERMINISTIC (1 ordering, declaration order): %s",
				nativeOrderings[0])

			// COMPARABILITY, established SEMANTICALLY rather than by hoping a particular
			// permutation was sampled.
			//
			// The two sides carry the same value and the same checks; only the key ORDER
			// differs. That is checked by decoding both through [pwDecodeAnswerStrict] —
			// which rejects an unknown field and anything after the document — and
			// comparing the decoded forms, which are order-insensitive by construction.
			// (An OMITTED field is caught by the comparison rather than the decoder,
			// since it decodes to a different zero value; [TestDecodedAnswerComparatorIsStrict]
			// drives both kinds.) Requiring the native bytes to equal one of the SAMPLED
			// stock orderings would instead be an assumption about Go's map iteration:
			// with three keys there are six permutations and this run observes three, so
			// "native is among them" is luck, not a stock wire contract.
			for i, o := range orderings {
				pwRequireSameDecodedAnswer(t, fmt.Sprintf("stock ordering %d", i), o, nativeOrderings[0])
			}

			// THE DECISIVE CONSEQUENCE, stated without needing to know which permutation
			// native lands on: stock emits at least two distinct byte strings for one
			// value, and native emits exactly one. So for any deterministic native
			// output there is at least one stock output it does not equal — byte-exact
			// parity is unavailable at this key count, whichever ordering native picks.
			matched := 0
			for _, o := range orderings {
				if o == nativeOrderings[0] {
					matched++
				}
			}
			if matched >= len(orderings) {
				t.Fatalf("the native ordering matched %d of the %d distinct stock orderings; a single "+
					"deterministic string cannot equal two different ones, so the observation above is "+
					"inconsistent", matched, len(orderings))
			}
			t.Logf("RECORDED: stock emitted %d distinct byte orderings for one value and native emits "+
				"exactly 1, so at least %d of stock's own outputs are unreachable for ANY deterministic "+
				"native ordering. (This run's native bytes matched %d of the %d SAMPLED orderings; that "+
				"count is an observation about which permutations Go's map iteration happened to "+
				"produce, NOT a stock contract.) This is the measured reason 2+ @check stays DECLINED "+
				"in 7.2c.", len(orderings), len(orderings)-1, matched, len(orderings))
		})
	}
}

// pwDecodedAnswer mirrors the two-field wire shape for the ORDER-INSENSITIVE comparison
// in [TestTwoCheckWireOrderIsUnstable].
//
// It is a decode target, not a carrier: `checks` is a map, so two byte strings that
// differ only in key order decode to equal values, which is exactly the property being
// used to separate "different order" from "different content".
type pwDecodedAnswer struct {
	Answer     string              `json:"answer"`
	Confidence pwDecodedCheckedInt `json:"confidence"`
}

type pwDecodedCheckedInt struct {
	Value  int64                     `json:"value"`
	Checks map[string]pwDecodedCheck `json:"checks"`
}

type pwDecodedCheck struct {
	Name       string `json:"name"`
	Expression string `json:"expression"`
	Status     string `json:"status"`
}

// pwDecodeAnswerStrict decodes ONE wire string into [pwDecodedAnswer], strictly, and
// requires the input to hold that value and nothing else.
//
// Two separate strictnesses, both load-bearing for the claim that a pair of wire strings
// differs ONLY in key order:
//
//   - DisallowUnknownFields. A field either side emitted that this shape does not model
//     would otherwise be silently dropped, and the comparison would then say "equal"
//     about two documents that are not. (A field either side OMITTED is caught by the
//     comparison instead, since it decodes to a different zero value — the decoder has
//     no say there, and this comment does not claim it does.)
//   - END OF INPUT. Anything after the value means the string was not the single
//     document the comparison treats it as.
//
// The EOF half is a SECOND Decode that must return io.EOF, not `Decoder.More()`. `More`
// is documented as reporting "whether there is another element in the current array or
// object being parsed"; after a complete top-level value there is no such array or
// object, so using it as an end-of-input test relies on behaviour the API does not
// promise — and it misses trailing bytes beginning with `}` or `]` outright. Decoding
// again and demanding io.EOF is what the contract does define.
func pwDecodeAnswerStrict(in string) (pwDecodedAnswer, error) {
	var out pwDecodedAnswer
	dec := stdjson.NewDecoder(strings.NewReader(in))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		return pwDecodedAnswer{}, fmt.Errorf("strict decode: %w", err)
	}
	var tail stdjson.RawMessage
	switch err := dec.Decode(&tail); {
	case errors.Is(err, io.EOF):
		return out, nil
	case err == nil:
		return pwDecodedAnswer{}, fmt.Errorf("trailing JSON value after the document: %s", tail)
	default:
		return pwDecodedAnswer{}, fmt.Errorf("trailing content after the document: %w", err)
	}
}

// pwSameDecodedAnswer reports whether two wire strings decode strictly to the same value.
//
// It returns an error rather than taking a *testing.T so the comparator itself can be
// driven by a negative control ([TestDecodedAnswerComparatorIsStrict]) instead of only
// ever being exercised on inputs that pass.
func pwSameDecodedAnswer(left, right string) error {
	got, err := pwDecodeAnswerStrict(left)
	if err != nil {
		return fmt.Errorf("stock: %w\n%s", err, left)
	}
	want, err := pwDecodeAnswerStrict(right)
	if err != nil {
		return fmt.Errorf("native: %w\n%s", err, right)
	}
	if !reflect.DeepEqual(got, want) {
		return fmt.Errorf("the two wire strings differ in CONTENT, not only in key order:\n"+
			" stock  %s\n native %s\n decoded stock  %+v\n decoded native %+v",
			left, right, got, want)
	}
	return nil
}

// pwRequireSameDecodedAnswer is the assertion form.
func pwRequireSameDecodedAnswer(t *testing.T, what, left, right string) {
	t.Helper()
	if err := pwSameDecodedAnswer(left, right); err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

// TestDecodedAnswerComparatorIsStrict is the negative control for the comparator the
// 2+-check ordering fact rests on.
//
// Without it, "strict and content-preserving" is a claim about code that has only ever
// been run on inputs that pass. Every row below is a way the comparison could have said
// "these differ only in key order" about two strings that do not.
func TestDecodedAnswerComparatorIsStrict(t *testing.T) {
	const (
		base     = `{"answer":"sunny","confidence":{"value":9,"checks":{"alpha":{"name":"alpha","expression":"this > 0","status":"succeeded"}}}}`
		permuted = `{"confidence":{"checks":{"alpha":{"status":"succeeded","expression":"this > 0","name":"alpha"}},"value":9},"answer":"sunny"}`
	)
	// The POSITIVE control first: a genuine key-order permutation must compare EQUAL, or
	// every negative below would pass for the wrong reason.
	if err := pwSameDecodedAnswer(base, permuted); err != nil {
		t.Fatalf("two orderings of the SAME document did not compare equal, so this comparator "+
			"cannot witness the ordering fact at all: %v", err)
	}

	for _, tc := range []struct {
		name  string
		left  string
		right string
	}{
		{"a trailing second document", base + base, base},
		// The case Decoder.More() misses: trailing bytes that begin with a closing
		// delimiter. More() peeks and returns false for `}`, so this slipped through
		// before the io.EOF check replaced it.
		{"a trailing close brace", base + "}", base},
		{"a trailing close bracket", base + "]", base},
		{"trailing garbage", base + "nonsense", base},
		{"an unknown field", strings.Replace(base, `"answer":"sunny"`, `"answer":"sunny","extra":1`, 1), base},
		{"a different value", strings.Replace(base, `"value":9`, `"value":10`, 1), base},
		{"a different status", strings.Replace(base, "succeeded", "failed", 1), base},
		{"a different expression", strings.Replace(base, "this > 0", "this >= 0", 1), base},
		{"a dropped check", `{"answer":"sunny","confidence":{"value":9,"checks":{}}}`, base},
		{"a renamed label", strings.ReplaceAll(base, "alpha", "beta"), base},
		{"a missing field", `{"confidence":{"value":9,"checks":{"alpha":{"name":"alpha","expression":"this > 0","status":"succeeded"}}}}`, base},
		{"an empty input", "", base},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := pwSameDecodedAnswer(tc.left, tc.right); err == nil {
				t.Fatalf("the comparator accepted %s as differing only in key order:\n%s", tc.name, tc.left)
			}
			// And the same defect on the OTHER side must be caught too, so the
			// comparator is not strict in one direction only.
			if err := pwSameDecodedAnswer(tc.right, tc.left); err == nil {
				t.Fatalf("the comparator accepted %s on the RIGHT-hand side:\n%s", tc.name, tc.left)
			}
		})
	}
}

// TestSingleCheckWireOrderIsStable is the discriminating control for the test above.
//
// Without it, "the bytes vary" could be a property of the harness rather than of the key
// count. A ONE-key result — the admitted family's own shape — must be byte-stable across
// the same number of observations, which is exactly why 7.2b could pin its literals at all.
func TestSingleCheckWireOrderIsStable(t *testing.T) {
	o := pwOperators()[0]
	key := pwDriveKey{Project: o.projectKey(), Func: pwFnChecked, Raw: pwNestedRaw(o.TrueVal)}
	stock := pwCheckedValue(t, key)
	if len(stock.Confidence.Checks) != 1 {
		t.Fatalf("the control row carries %d checks, want exactly 1", len(stock.Confidence.Checks))
	}
	orderings := pwDistinctMarshals(t, stock)
	if len(orderings) != 1 {
		t.Fatalf("a ONE-key stock result produced %d distinct byte orderings across %d marshals; the "+
			"whole 7.2b byte authority rests on this being 1: %v",
			len(orderings), pwMarshalObservations, orderings)
	}
	if orderings[0] != pwOperatorCaptures[o.ID].checkTrue {
		t.Fatalf("the stable single-key ordering is not the pinned literal:\n got %s\nwant %s",
			orderings[0], pwOperatorCaptures[o.ID].checkTrue)
	}
	t.Logf("RECORDED: a ONE-key result is byte-stable across %d marshals; the instability above is a "+
		"property of the KEY COUNT, not of this harness", pwMarshalObservations)
}

// ---------------------------------------------------------------------------
// Duplicate labels.
// ---------------------------------------------------------------------------

// pwDuplicateLabelWire is stock's output for two @check attributes sharing ONE label,
// declared on the pinned family.
//
// The map fold is LAST-WRITE-WINS: the surviving entry carries the SECOND expression.
// checkedwire pins the same fold on a bare target; this row pins it inside the admitted
// family's own shape, where a mapper would have to reproduce it.
const pwDuplicateLabelWire = `{"answer":"sunny","confidence":{"value":9,"checks":{"dup":{"name":"dup","expression":"this > 1","status":"succeeded"}}}}`

// TestDuplicateLabelsFoldLastWriteWins pins the fold and the data it destroys.
func TestDuplicateLabelsFoldLastWriteWins(t *testing.T) {
	key := pwDriveKey{Project: "res_duplicate_labels", Func: pwFnChecked, Raw: pwNestedRaw(9)}
	stock := pwCheckedValue(t, key)
	if len(stock.Confidence.Checks) != 1 {
		t.Fatalf("stock reported %d checks for two attributes under one label, want 1 after the fold: %v",
			len(stock.Confidence.Checks), stock.Confidence.Checks)
	}
	want := shared.Check{Name: "dup", Expression: "this > 1", Status: "succeeded"}
	if got := stock.Confidence.Checks["dup"]; got != want {
		t.Fatalf("stock check = %+v, want %+v (the SECOND declaration wins the fold)", got, want)
	}
	pwRequireSonicBytes(t, "stock", stock, pwDuplicateLabelWire)

	// DISCRIMINATING: the FIRST declaration is absent from the bytes. A fold that kept
	// the first would fail here rather than pass on a superset.
	if strings.Contains(pwDuplicateLabelWire, "this > 0") {
		t.Fatalf("the pinned duplicate-label literal carries the FIRST declaration: %s", pwDuplicateLabelWire)
	}
	// And the native carrier REFUSES the pair outright rather than reproducing a fold —
	// a deliberate divergence, which is why the form is declined before it can be served.
	_, err := bamlutils.NewChecked[int64](9, []bamlutils.Check{
		{Name: "dup", Expression: "this > 0", Status: "succeeded"},
		{Name: "dup", Expression: "this > 1", Status: "succeeded"},
	})
	if err == nil {
		t.Fatal("bamlutils.NewChecked accepted a duplicate label; it is supposed to refuse the pair " +
			"rather than silently pick a winner")
	}
	t.Logf("RECORDED: stock folds duplicate labels LAST-WRITE-WINS (%q survives); the native carrier "+
		"REFUSES the pair (%v). The two cannot agree, so duplicate labels stay DECLINED.",
		"this > 1", err)
}

// ---------------------------------------------------------------------------
// Mixed @check + @assert, in BOTH declaration orders.
// ---------------------------------------------------------------------------

// The mixed-form captures. Both declaration orders produce the SAME bytes and the SAME
// error, which is itself the finding: stock's output does not record which attribute was
// written first.
const (
	// A node with one check and a PASSING assert keeps only the check.
	pwMixedPassWire = `{"answer":"sunny","confidence":{"value":9,"checks":{"c":{"name":"c","expression":"this < 100","status":"succeeded"}}}}`
	// A FALSE assert emits no value at all — and the PASSING check beside it, which
	// would otherwise have been emitted, is absent from the error entirely.
	pwMixedAssertFailErr = `Failed to coerce value: ParsingError { scope: [], reason: "Failed while parsing required fields: missing=0, unparsed=1", causes: [ParsingError { scope: [], reason: "Failed to parse field confidence: <root>: Assertions failed.\n  - <root>: Failed: a this > 0", causes: [ParsingError { scope: [], reason: "Assertions failed.", causes: [ParsingError { scope: [], reason: "Failed: a this > 0", causes: [] }] }] }] }`
)

// TestMixedCheckAndAssertInBothDeclarationOrders captures the mixed form in both orders
// and in both outcomes, including the false-assert path the scope names as unproved.
func TestMixedCheckAndAssertInBothDeclarationOrders(t *testing.T) {
	for _, project := range []string{"res_check_then_assert", "res_assert_then_check"} {
		t.Run(project, func(t *testing.T) {
			// (1) BOTH constraints holding: only the CHECK reaches the wire.
			passKey := pwDriveKey{Project: project, Func: pwFnChecked, Raw: pwNestedRaw(9)}
			stock := pwCheckedValue(t, passKey)
			if len(stock.Confidence.Checks) != 1 {
				t.Fatalf("stock reported %d checks for a check+assert node, want exactly 1: %v",
					len(stock.Confidence.Checks), stock.Confidence.Checks)
			}
			if _, present := stock.Confidence.Checks["a"]; present {
				t.Fatalf("the passing @assert produced a check entry: %v", stock.Confidence.Checks)
			}
			pwRequireSonicBytes(t, "stock", stock, pwMixedPassWire)

			// (2) The assert FALSE while the check still HOLDS. This is the state the
			// scope calls unproved: a check that would otherwise have been emitted is
			// suppressed, and the error names only the assert.
			failKey := pwDriveKey{Project: project, Func: pwFnChecked, Raw: pwNestedRaw(-1)}
			err := pwError(t, failKey)
			if got := err.Error(); got != pwMixedAssertFailErr {
				t.Fatalf("stock mixed-node assertion error bytes:\n got %s\nwant %s",
					strconv.Quote(got), strconv.Quote(pwMixedAssertFailErr))
			}
			// DISCRIMINATING: the surviving check is nowhere in the error. An
			// implementation that emitted the check beside the failure — or folded the
			// assert into a failed check — would not produce these bytes.
			for _, absent := range []string{"this < 100", `"status"`, `"checks"`, "succeeded"} {
				if strings.Contains(pwMixedAssertFailErr, absent) {
					t.Errorf("the pinned mixed-node error carries %q; the passing check is supposed to "+
						"be absent from it entirely", absent)
				}
			}
		})
	}

	// (3) The two declaration orders are INDISTINGUISHABLE in stock's output. Recorded
	// as a fact, because it is the reason a mixed admission would need a state machine
	// rather than an ordering rule: nothing in the output says which came first.
	a := pwCheckedValue(t, pwDriveKey{Project: "res_check_then_assert", Func: pwFnChecked, Raw: pwNestedRaw(9)})
	b := pwCheckedValue(t, pwDriveKey{Project: "res_assert_then_check", Func: pwFnChecked, Raw: pwNestedRaw(9)})
	aBytes, err := sonic.Marshal(a)
	if err != nil {
		t.Fatalf("sonic.Marshal: %v", err)
	}
	bBytes, err := sonic.Marshal(b)
	if err != nil {
		t.Fatalf("sonic.Marshal: %v", err)
	}
	if string(aBytes) != string(bBytes) {
		t.Fatalf("the two declaration orders produced DIFFERENT bytes:\n  check-first: %s\n  assert-first: %s",
			aBytes, bBytes)
	}
	errA := pwError(t, pwDriveKey{Project: "res_check_then_assert", Func: pwFnChecked, Raw: pwNestedRaw(-1)})
	errB := pwError(t, pwDriveKey{Project: "res_assert_then_check", Func: pwFnChecked, Raw: pwNestedRaw(-1)})
	if errA.Error() != errB.Error() {
		t.Fatalf("the two declaration orders produced DIFFERENT errors:\n  check-first: %s\n  assert-first: %s",
			strconv.Quote(errA.Error()), strconv.Quote(errB.Error()))
	}
	t.Log("RECORDED: stock's output is IDENTICAL for both declaration orders, in both outcomes. A " +
		"mixed admission therefore cannot be derived from declaration order — it needs its own " +
		"output/error state machine, which is why it stays DECLINED.")
}

// ---------------------------------------------------------------------------
// Two failing @assert attributes on the pinned family.
// ---------------------------------------------------------------------------

// pwTwoAssertsErr is stock's UNMODIFIED err.Error() for TWO failing asserts on the
// name-pinned assert family.
//
// Both causes are present, in DECLARATION order, flattened into the field-level reason
// and repeated as a sibling pair in the inner tree. The current renderer models exactly
// ONE failing assert on one required field, so this is a shape it cannot produce — which
// is the measured reason multi-assert stays declined on this family.
const pwTwoAssertsErr = `Failed to coerce value: ParsingError { scope: [], reason: "Failed while parsing required fields: missing=0, unparsed=1", causes: [ParsingError { scope: [], reason: "Failed to parse field confidence: <root>: Assertions failed.\n  - <root>: Failed: first this > 100\n  - <root>: Failed: second this > 200", causes: [ParsingError { scope: [], reason: "Assertions failed.", causes: [ParsingError { scope: [], reason: "Failed: first this > 100", causes: [] }, ParsingError { scope: [], reason: "Failed: second this > 200", causes: [] }] }] }] }`

// TestTwoAssertsOnThePinnedFamilyRecordCauseOrder pins the multi-assert error on the
// admitted family's own shape and records the cause ORDER.
func TestTwoAssertsOnThePinnedFamilyRecordCauseOrder(t *testing.T) {
	key := pwDriveKey{Project: "res_two_asserts", Func: pwFnAssert, Raw: pwNestedRaw(9)}
	err := pwError(t, key)
	if got := err.Error(); got != pwTwoAssertsErr {
		t.Fatalf("stock two-assert error bytes:\n got %s\nwant %s",
			strconv.Quote(got), strconv.Quote(pwTwoAssertsErr))
	}
	// The ORDER is the recorded fact: `first` before `second`, which is DECLARATION
	// order, in both the flattened reason and the inner cause list. Asserted by index so
	// a reordering fails rather than passing on a set-membership check.
	firstAt := strings.Index(pwTwoAssertsErr, "Failed: first this > 100")
	secondAt := strings.Index(pwTwoAssertsErr, "Failed: second this > 200")
	if firstAt < 0 || secondAt < 0 {
		t.Fatalf("the pinned two-assert error does not carry both causes: %s", pwTwoAssertsErr)
	}
	if firstAt >= secondAt {
		t.Fatalf("the pinned two-assert error carries `second` before `first`; the recorded order is "+
			"wrong: %s", pwTwoAssertsErr)
	}
	// DISCRIMINATING: it is a genuine multi-cause error, not the single-assert shape with
	// one cause swapped in — the one-assert literal must not be a prefix or equal.
	if pwTwoAssertsErr == pwOperatorCaptures["gt"].assertFail {
		t.Fatal("the two-assert error equals the one-assert error")
	}
	if strings.Count(pwTwoAssertsErr, "Failed: ") != 4 {
		t.Fatalf("the pinned two-assert error carries %d `Failed: ` causes, want 4 (each of the two "+
			"appears in the flattened reason and again in the inner tree): %s",
			strings.Count(pwTwoAssertsErr, "Failed: "), pwTwoAssertsErr)
	}
	t.Logf("RECORDED: two failing asserts on the pinned family produce BOTH causes in DECLARATION " +
		"order, flattened into the field reason and repeated as a sibling pair in the inner tree. " +
		"The current renderer models ONE failing assert, so multi-assert stays DECLINED.")
}

// ---------------------------------------------------------------------------
// Every residual form still DECLINES.
// ---------------------------------------------------------------------------

// pwConstraint builds one labelled constraint.
func pwConstraint(level schema.ConstraintLevel, label, expr string) schema.Constraint {
	l := label
	return schema.Constraint{Level: level, Expression: expr, Label: &l}
}

// pwMultiConstraintBundle builds the pinned CHECK family carrying N constraints on
// `confidence`, so the residual forms can be asked of the production gates as the same
// shape stock was driven with.
func pwMultiConstraintBundle(cs ...schema.Constraint) *schema.Bundle {
	confidence := schema.Type{Kind: schema.TypePrimitive, Primitive: schema.PrimitiveInt}
	confidence.Meta.Constraints = cs
	b := &schema.Bundle{
		Target: schema.Type{Kind: schema.TypeClass, Name: pwCheckedClass, Mode: schema.NonStreaming},
		Classes: []schema.ClassDef{{
			Name: schema.Name{Name: pwCheckedClass},
			Mode: schema.NonStreaming,
			Fields: []schema.ClassField{
				{Name: schema.Name{Name: "answer"}, Type: schema.Type{Kind: schema.TypePrimitive, Primitive: schema.PrimitiveString}},
				{Name: schema.Name{Name: "confidence"}, Type: confidence},
			},
		}},
	}
	if err := b.RebuildIndexes(); err != nil {
		panic("predicatewire residual fixture: " + err.Error())
	}
	return b
}

// pwResidualBundles pairs each captured residual FORM with the bundle that describes it.
func pwResidualBundles() map[string]*schema.Bundle {
	check, assert := schema.ConstraintCheck, schema.ConstraintAssert
	return map[string]*schema.Bundle{
		"two_checks": pwMultiConstraintBundle(
			pwConstraint(check, "alpha", "this > 0"), pwConstraint(check, "beta", "this < 100")),
		"three_checks": pwMultiConstraintBundle(
			pwConstraint(check, "alpha", "this > 0"), pwConstraint(check, "beta", "this < 100"),
			pwConstraint(check, "gamma", "this != 7")),
		"duplicate_labels": pwMultiConstraintBundle(
			pwConstraint(check, "dup", "this > 0"), pwConstraint(check, "dup", "this > 1")),
		"check_then_assert": pwMultiConstraintBundle(
			pwConstraint(check, "c", "this < 100"), pwConstraint(assert, "a", "this > 0")),
		"assert_then_check": pwMultiConstraintBundle(
			pwConstraint(assert, "a", "this > 0"), pwConstraint(check, "c", "this < 100")),
		"two_asserts": pwAssertFamilyBundle(
			pwConstraint(assert, "first", "this > 100"), pwConstraint(assert, "second", "this > 200")),
	}
}

// pwAssertFamilyBundle is the pinned ASSERT family carrying N constraints on
// `confidence`. It is separate from [pwMultiConstraintBundle] because the two families
// are DIFFERENT pinned class names, and asking the gates about the wrong one would
// measure the name rather than the form.
func pwAssertFamilyBundle(cs ...schema.Constraint) *schema.Bundle {
	b := pwMultiConstraintBundle(cs...)
	b.Target.Name = pwAssertClass
	b.Classes[0].Name.Name = pwAssertClass
	if err := b.RebuildIndexes(); err != nil {
		panic("predicatewire residual fixture: " + err.Error())
	}
	return b
}

// pwResidualDeclineReport returns one line per residual form the production gates did NOT
// decline, so the comparison can be driven with a wrong expectation and shown to bite.
func pwResidualDeclineReport(t *testing.T, declines func(form string) bool) []string {
	t.Helper()
	var out []string
	bundles := pwResidualBundles()
	for _, form := range pwSortedBundleKeys(bundles) {
		b := bundles[form]
		admitted := pwAdmits(t, "residual "+form, b)
		if admitted == declines(form) {
			out = append(out, form+": gates admitted="+strconv.FormatBool(admitted)+
				", expected declined="+strconv.FormatBool(declines(form)))
		}
	}
	return out
}

func pwSortedBundleKeys(m map[string]*schema.Bundle) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestResidualFormsAreDeclined proves every captured residual form is still refused by
// the production gates, and on the direct parse route as well.
func TestResidualFormsAreDeclined(t *testing.T) {
	always := func(string) bool { return true }
	if got := pwResidualDeclineReport(t, always); len(got) != 0 {
		t.Fatalf("a residual form is no longer declined:\n  %s", strings.Join(got, "\n  "))
	}
	bundles := pwResidualBundles()
	for _, form := range pwSortedBundleKeys(bundles) {
		if _, err := debaml.ParseStaticBundle(context.Background(), bundles[form], pwNestedRaw(9)); !errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
			t.Errorf("%s: ParseStaticBundle did not decline on the direct route: %v", form, err)
		}
	}
	// Every CAPTURED residual project must have a bundle, or a form could be measured
	// against stock and never checked against the gates.
	for _, r := range pwResiduals() {
		if _, ok := bundles[r.ID]; !ok {
			t.Errorf("residual %q is captured from stock but has no bundle, so its DECLINE is unproven", r.ID)
		}
	}
	t.Logf("%d residual forms captured from stock, all %d still DECLINED at the production gates",
		len(pwResiduals()), len(bundles))
}

// TestResidualDeclinesAreProvenToBite feeds the same comparison the opposite expectation
// and requires every form to be reported.
//
// A suite whose every assertion is "this still declines" is exactly the shape that can be
// green while measuring nothing.
func TestResidualDeclinesAreProvenToBite(t *testing.T) {
	never := func(string) bool { return false }
	got := pwResidualDeclineReport(t, never)
	if len(got) != len(pwResidualBundles()) {
		t.Fatalf("an expectation that NO residual form declines was reported %d times, want %d "+
			"(one per form); the comparison is not covering every form", len(got), len(pwResidualBundles()))
	}
}
