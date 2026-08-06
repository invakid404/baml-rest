package debaml

import (
	"errors"
	"fmt"
	"iter"
	"strings"
	"testing"

	mj "github.com/invakid404/minijinja-go/v2"
	"github.com/invakid404/minijinja-go/v2/filters"
	mjvalue "github.com/invakid404/minijinja-go/v2/value"
)

// The NEGATIVES behind Slice 7.2a-1's guard removal, and the reason the second
// candidate was NOT removed.
//
// A removal rests on one claim: a guard that STAYS already refuses every input
// the removed guard could have seen, so deleting it changed no outcome. The
// stock-CFFI rows in internal/debaml/guardledger show that no ENVELOPE moved;
// these tests are the other half — they exercise the SURVIVING mechanism
// directly, at the seam the removed guard sat on, so a later change that
// narrowed it fails HERE rather than silently reopening the hole.
//
// "At the seam" is load-bearing and is why these tests do not go through
// EvaluateConstraint. [operatorShapeIsProven] runs before evaluation and refuses
// many expressions outright, so an expression-level assertion can pass for a
// reason that has nothing to do with the guard under discussion. Everything
// below therefore drives either [checkCallParity] itself or a bare environment
// carrying only [installProfileGuards], where the operator gate is not involved.

// removedLengthGuardMessage is the exact text the deleted guard produced. A
// refusal that still carried it would mean the guard was reintroduced, and the
// assertions below would then be testing the wrong mechanism.
const removedLengthGuardMessage = "length of a value with no length"

// stockLengthMessage is the message stock BAML v0.223 raises for a subject with
// no length, shared by every kind it rejects — guardledger pins the full text
// per row (`... of type number`, `... of type bool`, `... of type iterator`).
// The engine must refuse for the SAME reason, which is what makes the removed
// guard's residual case a decline rather than an unrelated failure.
const stockLengthMessage = "cannot calculate length of value of type"

// checkCallParitySubjectMarker is the distinctive part of [checkCallParity]'s
// SUBJECT-KIND refusal ("`%s` was applied to a %s subject, which is not a
// proven-identical conversion for it").
//
// It is named once so the two guards that lean on it — the removed length/count
// wrapper and the retained `last` wrapper — assert the SAME mechanism, and so a
// reworded message fails here rather than silently turning either proof into
// "some error happened".
const checkCallParitySubjectMarker = "is not a proven-identical conversion for it"

// lengthlessSequence is a value that IS in [kSized] — its kind is a sequence, so
// [checkCallParity] admits it to `length` — and yet has no length, which is
// exactly the input the removed guard existed to catch. The value model cannot
// build one; it is constructed here so the residual case can be tested rather
// than argued about.
type lengthlessSequence struct{}

func (lengthlessSequence) ObjectRepr() mjvalue.ObjectRepr { return mjvalue.ObjectReprSeq }
func (lengthlessSequence) GetAttr(string) mjvalue.Value   { return mjvalue.Undefined() }
func (lengthlessSequence) Iterate() iter.Seq[mjvalue.Value] {
	return func(yield func(mjvalue.Value) bool) { yield(mjvalue.FromInt(1)) }
}

// modelValues is one value per ConstraintValue kind the model can produce
// (media excepted, which renderConstraint refuses before conversion).
func modelValues() map[string]ConstraintValue {
	return map[string]ConstraintValue{
		"string": StringValue("abc"),
		"int":    IntValue(3),
		"float":  FloatValue(1.5),
		"bool":   BoolValue(true),
		"null":   NullValue(),
		"enum":   EnumValue("Hue", "RED"),
		"list":   ListValue([]ConstraintValue{IntValue(1), IntValue(2), IntValue(3)}),
		"map": MapValue([]ConstraintEntry{
			{Key: "b", Value: IntValue(1)}, {Key: "a", Value: IntValue(2)},
		}),
		"class": ClassValue("Probe", []ConstraintEntry{
			{Key: "b", Value: IntValue(1)}, {Key: "a", Value: StringValue("x")},
		}),
	}
}

// profileEnv builds an environment carrying ONLY the profile's filter/test
// guards, with no operator gate in front of it. Rendering through it is how a
// test reaches the filter seam directly.
func profileEnv(t *testing.T) *mj.Environment {
	t.Helper()
	env := mj.NewEnvironment()
	env.SetAutoEscapeFunc(func(string) mj.AutoEscape { return mj.AutoEscapeNone })
	withdrawNonBAMLBuiltins(env)
	installProfileGuards(env)
	return env
}

// renderAtFilterSeam applies expr to `this` through [profileEnv], returning the
// render and its error. The operator gate is deliberately absent.
func renderAtFilterSeam(t *testing.T, env *mj.Environment, this mjvalue.Value, expr string) (string, error) {
	t.Helper()
	tmpl, err := env.TemplateFromString("{{ " + expr + " }}")
	if err != nil {
		t.Fatalf("compile %q: %v", expr, err)
	}
	return tmpl.Render(map[string]mjvalue.Value{"this": this})
}

// TestRemovedLengthGuardIsSubsumedAtTheFilterSeam is the proven-to-bite negative
// for the one guard this slice removed.
//
// It drives the SURVIVOR — [checkCallParity]'s subject rule — directly and
// through a bare profile environment, so it cannot pass because some earlier
// gate happened to refuse the expression.
func TestRemovedLengthGuardIsSubsumedAtTheFilterSeam(t *testing.T) {
	empty := mjvalue.NewOrderedMap(0)

	// (1) THE SURVIVOR, called directly. checkCallParity is the function the
	// ledger names as carrying the removed guard's refusals; it must refuse every
	// subject kind that has no length, for BOTH `length` and `count`. If it ever
	// returns nil here, the removal is invalid — no other mechanism is behind it.
	for _, name := range []string{"length", "count"} {
		for kind, val := range map[string]mjvalue.Value{
			"number":    mjvalue.FromInt(3),
			"float":     mjvalue.FromFloat(1.5),
			"bool":      mjvalue.FromBool(true),
			"none":      mjvalue.None(),
			"undefined": mjvalue.Undefined(),
		} {
			err := checkCallParity(name, val, nil, empty)
			if err == nil {
				t.Errorf("checkCallParity(%q, %s) returned nil; it is the guard the `%s` removal rests on, "+
					"so a value with no length would now reach the builtin unguarded", name, kind, name)
				continue
			}
			if !strings.Contains(err.Error(), checkCallParitySubjectMarker) {
				t.Errorf("checkCallParity(%q, %s) refused for a different reason than the subject rule: %v", name, kind, err)
			}
		}
		// And it must still ADMIT every sized kind, so the survivor is specific
		// rather than a blanket refusal that would make the test vacuous.
		for kind, val := range map[string]mjvalue.Value{
			"string": mjvalue.FromString("abc"),
			"seq":    mjvalue.FromSlice([]mjvalue.Value{mjvalue.FromInt(1)}),
			"map":    mjvalue.FromMap(map[string]mjvalue.Value{"a": mjvalue.FromInt(1)}),
		} {
			if err := checkCallParity(name, val, nil, empty); err != nil {
				t.Errorf("checkCallParity(%q, %s) refused a sized subject: %v", name, kind, err)
			}
		}
	}

	// (2) THE SEAM, rendered. The operator gate is not in this environment, so a
	// refusal here can only have come from the filter wrapper chain — which is
	// exactly where the removed guard used to sit.
	env := profileEnv(t)
	for _, expr := range []string{"this|length", "this|count"} {
		out, err := renderAtFilterSeam(t, env, mjvalue.FromInt(3), expr)
		if err == nil {
			t.Errorf("%q over a number rendered %q at the filter seam; without the removed guard the "+
				"signature table is the only thing refusing it, and it did not", expr, out)
			continue
		}
		if !strings.Contains(err.Error(), checkCallParitySubjectMarker) {
			t.Errorf("%q over a number was refused at the filter seam for a different reason: %v", expr, err)
		}
		if strings.Contains(err.Error(), removedLengthGuardMessage) {
			t.Errorf("%q was refused by the REMOVED length guard, so it is still installed", expr)
		}
		// A sized subject must still render, so the seam is not simply broken.
		if out, err := renderAtFilterSeam(t, env, mjvalue.FromString("abc"), expr); err != nil || out != "3" {
			t.Errorf("%q over a string rendered (%q, %v) at the filter seam, want (\"3\", nil)", expr, out, err)
		}
	}

	// (3) kSized IS the has-a-length set, over every value the model produces and
	// under BOTH mapping projections. This is why the guard was unreachable in the
	// first place; widening kSized fails here.
	for name, cv := range modelValues() {
		for _, mode := range []mappingMode{mappingOrdered, mappingNative} {
			mv := cv.toMinijinjaMode(mode)
			_, hasLen := mv.Len()
			admitted := kindOf(mv)&provenSignatures["length"].subject != 0
			if hasLen != admitted {
				t.Errorf("%s under projection %d: Len() ok=%v but the `length` signature admits it=%v; "+
					"the removed guard covered exactly this gap, so it may not exist",
					name, mode, hasLen, admitted)
			}
			if admitted != (kindOf(mv)&provenSignatures["count"].subject != 0) {
				t.Errorf("%s: `length` and `count` no longer admit the same subject kinds", name)
			}
		}
	}

	// (4) The residual case, constructed rather than assumed: a value that IS
	// admitted by the signature table and has no length. The engine's own filter
	// must RAISE on it, and raise the SAME error class stock raises for a
	// length-less subject — guardledger row SPLIT_LENGTH records
	// `invalid operation: cannot calculate length of value of type iterator` —
	// so even there the removal only ever produces a decline.
	//
	// The MESSAGE is pinned, not just the presence of an error. "some error
	// happened" would be satisfied by an unrelated failure from a future library
	// change, and the claim this residual case makes is specifically that the
	// engine refuses for the reason stock refuses.
	lengthless := mjvalue.FromObject(lengthlessSequence{})
	if _, ok := lengthless.Len(); ok {
		t.Fatal("the lengthless fixture grew a length; it no longer tests the residual case")
	}
	if kindOf(lengthless)&provenSignatures["length"].subject == 0 {
		t.Fatal("the lengthless fixture is no longer admitted by the `length` signature; it no longer tests the residual case")
	}
	_, err := filters.FilterLength(nil, lengthless, nil, empty)
	if err == nil {
		t.Fatal("the engine's `length` filter ANSWERED for a sequence with no length; without the removed " +
			"guard that answer would reach a comparison, so the removal is no longer justified")
	}
	if !strings.Contains(err.Error(), stockLengthMessage) {
		t.Errorf("the engine's `length` filter refused a length-less sequence with %q; stock refuses a "+
			"length-less subject with a message containing %q, and the residual case rests on the two agreeing",
			err, stockLengthMessage)
	}
}

// The `last`-over-a-mapping guard: two separate facts, asserted separately.
//
// The round-2 review found the previous single test defective, and it was: it
// required only that a bare profile environment ERROR on `<mapping>|last`, which
// checkCallParity does on its own. Deleting the wrapper left it green, so it
// proved the wrong mechanism.
//
// The two facts are now split, and each is executable:
//
//	1. checkCallParity PRE-EMPTS the wrapper, so it is unreachable in production.
//	   That is the honest description of the guard, and it is asserted rather
//	   than argued.
//	2. The wrapper is nevertheless INSTALLED and DOES fire, which is what makes
//	   deleting it a real change. Proving that needs the shadow lifted, because
//	   there is no other way to reach the seam.
//
// Neither test can pass if the guard is deleted while checkCallParity is
// unchanged: (1) would still pass, but (2) would see the ENGINE's own refusal
// (`cannot get last item from value`) instead of the guard's marker, which is a
// different message from a different mechanism.

// TestLastOverMappingGuardPreEmptedByCheckCallParity is fact (1): the guard is
// DEAD in production, and the thing that kills it is a guard that stays.
//
// This is the executable version of "unreachable behind checkCallParity". It
// asserts the pre-emption at the signature table itself and at the filter seam,
// so a change that let a mapping through to the wrapper would be caught here.
func TestLastOverMappingGuardPreEmptedByCheckCallParity(t *testing.T) {
	empty := mjvalue.NewOrderedMap(0)
	mapping := modelValues()["map"].toMinijinjaMode(mappingOrdered)

	// The signature table refuses a mapping subject for `last`, which is what
	// runs first inside guardIntegerResult.
	if provenSignatures["last"].subject&kMap != 0 {
		t.Fatal("`last` now admits a mapping subject, so checkCallParity no longer pre-empts the wrapper; " +
			"the guard has become reachable and the ledger record must be re-derived")
	}
	err := checkCallParity("last", mapping, nil, empty)
	if err == nil {
		t.Fatal("checkCallParity admitted `<mapping>|last`; the guard behind it is reachable again")
	}
	if !strings.Contains(err.Error(), checkCallParitySubjectMarker) {
		t.Errorf("checkCallParity refused `<mapping>|last` for a different reason than the subject rule: %v", err)
	}

	// And at the INSTALLED seam: the message a mapping subject actually gets is
	// checkCallParity's subject-kind error, NOT the wrapper's — which is
	// precisely why no witness row can observe the wrapper.
	//
	// Both halves are asserted. "not the wrapper's marker" alone would be
	// satisfied by ANY other refusal, including one from a mechanism that has
	// nothing to do with the claim; naming checkCallParity's own message is what
	// makes this the pre-emption proof rather than "something refused".
	env := profileEnv(t)
	out, err := renderAtFilterSeam(t, env, mapping, "this|last")
	switch {
	case err == nil:
		t.Fatalf("`<mapping>|last` rendered %q at the filter seam; stock RAISES on it", out)
	case strings.Contains(err.Error(), lastOverMappingMarker):
		t.Errorf("`<mapping>|last` was refused by the WRAPPER, not by checkCallParity; the ledger records the "+
			"opposite and the kept-unprovable classification depends on it: %v", err)
	case !strings.Contains(err.Error(), checkCallParitySubjectMarker):
		t.Errorf("`<mapping>|last` was refused at the seam by neither the wrapper nor checkCallParity's subject "+
			"rule: %v\nwant a refusal carrying %q — the ledger's pre-emption claim names that mechanism "+
			"specifically, and any other refusal would make the claim unproven", err, checkCallParitySubjectMarker)
	}

	// The EXPRESSION path refuses even earlier, which is the reason the removal
	// is unprovable rather than merely inert: no corpus row can reach the seam.
	if operatorShapeIsProven(modelValues()["map"], `this|last == "a"`) {
		t.Fatal("the operator gate now admits `<mapping>|last`. That is the FIRST of the two gates in front of the " +
			"wrapper — checkCallParity's subject rule, asserted above, is the other — so a witness row still cannot " +
			"reach the seam on this alone. The ledger's reachability condition names both, and it should be " +
			"re-derived now that one has moved")
	}
}

// TestLastOverMappingGuardIsLiveInTheInstalledChain is fact (2): the wrapper is
// really installed on the environment and really refuses a mapping.
//
// It cannot be observed while checkCallParity shadows it, so the test LIFTS the
// shadow — it widens `last`'s proven subject set to admit a mapping for the
// duration, drives the installed filter, and restores the table. That is the
// only way to reach this seam, and it is what makes the assertion
// mutation-sensitive: with the wrapper deleted, the refusal that comes back is
// the ENGINE's own (`cannot get last item from value`) rather than the guard's
// marker, and this test fails on the difference. It was verified that way, not
// assumed.
//
// The mutation is confined to the test: the table is restored (and the restore
// is verified) before returning, and Go runs a package's tests sequentially, so
// no other test observes the widened entry.
func TestLastOverMappingGuardIsLiveInTheInstalledChain(t *testing.T) {
	const name = "last"
	original, ok := provenSignatures[name]
	if !ok {
		t.Fatalf("`%s` has no proven signature; this test can no longer lift the shadow it needs to", name)
	}
	defer func() {
		provenSignatures[name] = original
		if provenSignatures[name].subject&kMap != 0 {
			t.Fatalf("the widened `%s` signature was not restored; every later test would run against it", name)
		}
	}()

	// Lift the shadow: admit a mapping subject so checkCallParity delegates to
	// the wrapper chain instead of refusing first.
	widened := original
	widened.subject = original.subject | kMap
	provenSignatures[name] = widened
	if err := checkCallParity(name, modelValues()["map"].toMinijinjaMode(mappingOrdered), nil, mjvalue.NewOrderedMap(0)); err != nil {
		t.Fatalf("the shadow was not lifted (checkCallParity still refuses): %v", err)
	}

	// The environment must be built AFTER the widening, because
	// installProfileGuards captures nothing about the signature table — but
	// building it here also proves the wrapper is applied by that function
	// rather than by this test.
	env := profileEnv(t)

	out, err := renderAtFilterSeam(t, env, modelValues()["map"].toMinijinjaMode(mappingOrdered), "this|last")
	if err == nil {
		t.Fatalf("with checkCallParity's subject rule lifted, `<mapping>|last` rendered %q — the "+
			"mapping guard is NOT installed in the filter chain. Stock raises on this expression "+
			"(guardledger rows LAST_MAP_KEY / LAST_CLS_KEY), so answering would be a wrong boolean.", out)
	}
	if !strings.Contains(err.Error(), lastOverMappingMarker) {
		t.Fatalf("with the shadow lifted, `<mapping>|last` was refused by something OTHER than the mapping "+
			"guard: %v\nwant a refusal carrying %q", err, lastOverMappingMarker)
	}
	if !errors.Is(err, ErrConstraintUnsupported) {
		t.Errorf("the mapping guard's refusal does not wrap ErrConstraintUnsupported: %v", err)
	}

	// The guard is SPECIFIC: a sequence subject still passes through it to the
	// engine, so the test above is not satisfied by a wrapper that refuses
	// everything.
	if out, err := renderAtFilterSeam(t, env,
		modelValues()["list"].toMinijinjaMode(mappingOrdered), "this|last"); err != nil || out != "3" {
		t.Errorf("`<list>|last` rendered (%q, %v) through the same chain, want (\"3\", nil)", out, err)
	}
}

// TestLastOverMappingEngineAlreadyRaises records the THIRD layer of redundancy,
// measured rather than assumed: even with the guard bypassed entirely, the
// fork's own `last` raises on a mapping — and raises the SAME message stock
// raises, which internal/debaml/guardledger pins as row LAST_MAP_KEY's inner
// error.
//
// It is recorded because it is what a future removal would rest on, and because
// the guard's own source comment claimed the opposite until this measurement —
// "minijinja-Go returns its final key" was true of the UPSTREAM port the guard
// was written against and is NOT true of the fork the package now compiles
// against. It does NOT license the removal on its own: no witness row can reach
// this seam, so the removal stays unprovable.
func TestLastOverMappingEngineAlreadyRaises(t *testing.T) {
	// The message stock BAML v0.223 produces, pinned by the guardledger row.
	const stockMessage = "cannot get last item from value"
	_, err := filters.FilterLast(nil, modelValues()["map"].toMinijinjaMode(mappingOrdered), nil, mjvalue.NewOrderedMap(0))
	if err == nil {
		t.Fatal("the engine's own `last` ANSWERED for a mapping; the guard is load-bearing after all and its " +
			"ledger record must be re-derived")
	}
	if !strings.Contains(err.Error(), stockMessage) {
		t.Errorf("the engine's own `last` raised %q for a mapping; stock raises a message containing %q, and the "+
			"ledger records that they agree", err, stockMessage)
	}
}

// TestLastOverMappingGuardRefusesDirectly is the third, cheapest bite: the
// extracted wrapper itself, called with no chain around it at all.
func TestLastOverMappingGuardRefusesDirectly(t *testing.T) {
	sentinel := errors.New("delegated")
	wrapped := guardLastOverMapping(func(filters.State, mjvalue.Value, []mjvalue.Value, *mjvalue.OrderedMap) (mjvalue.Value, error) {
		return mjvalue.Undefined(), sentinel
	})
	_, err := wrapped(nil, modelValues()["map"].toMinijinjaMode(mappingOrdered), nil, mjvalue.NewOrderedMap(0))
	if !strings.Contains(fmt.Sprint(err), lastOverMappingMarker) {
		t.Errorf("the guard did not refuse a mapping subject: %v", err)
	}
	// Anything else is DELEGATED unchanged — the sentinel proves the wrapper is
	// a filter rather than a replacement.
	if _, err := wrapped(nil, mjvalue.FromSlice([]mjvalue.Value{mjvalue.FromInt(1)}), nil, mjvalue.NewOrderedMap(0)); !errors.Is(err, sentinel) {
		t.Errorf("the guard did not delegate a sequence subject to the builtin: %v", err)
	}
}

// TestRemovedGuardsDidNotWidenTheAnswerSurface is the blunt counterpart to the
// structural proofs: the expressions the removal touches must produce the SAME
// outcome they produced before, so "inert" is asserted rather than claimed.
//
// The expectations are the stock envelopes recorded by internal/debaml/guardledger
// (rows LEN_*, CNT_*, LAST_*, FIRST_LIST), reproduced here in the CGO-free lane so
// a regression is caught without the integration build.
func TestRemovedGuardsDidNotWidenTheAnswerSurface(t *testing.T) {
	vals := modelValues()
	for _, tc := range []struct {
		row     string
		this    ConstraintValue
		expr    string
		decides bool
		want    bool
	}{
		{"LEN_STR", vals["string"], "this|length == 3", true, true},
		{"LEN_LIST", vals["list"], "this|length == 3", true, true},
		{"LEN_MAP", vals["map"], "this|length == 2", true, true},
		{"LEN_CLS", vals["class"], "this|length == 2", true, true},
		{"CNT_STR", vals["string"], "this|count == 3", true, true},
		{"CNT_LIST", vals["list"], "this|count == 3", true, true},
		{"CNT_MAP", vals["map"], "this|count == 2", true, true},
		{"CNT_CLS", vals["class"], "this|count == 2", true, true},
		{"LAST_LIST", vals["list"], "this|last == 3", true, true},
		{"FIRST_LIST", vals["list"], "this|first == 1", true, true},
		{"LEN_INT", vals["int"], "this|length == 0", false, false},
		{"LEN_BOOL", vals["bool"], "this|length == 0", false, false},
		{"LEN_NULL", vals["null"], "this|length == 0", false, false},
		{"CNT_INT", vals["int"], "this|count == 0", false, false},
		{"CNT_BOOL", vals["bool"], "this|count == 0", false, false},
		{"CNT_NULL", vals["null"], "this|count == 0", false, false},
		{"LAST_MAP_KEY", vals["map"], `this|last == "a"`, false, false},
		{"LAST_CLS_KEY", vals["class"], `this|last == "a"`, false, false},
		{"LAST_CLS_VALUE", vals["class"], `this|last == "x"`, false, false},
	} {
		got, err := EvaluateConstraint(tc.this, tc.expr)
		switch {
		case tc.decides && err != nil:
			t.Errorf("row %s (%q): must still decide, got err=%v", tc.row, tc.expr, err)
		case tc.decides && got != tc.want:
			t.Errorf("row %s (%q): decided %v, want %v", tc.row, tc.expr, got, tc.want)
		case !tc.decides && !errors.Is(err, ErrConstraintUnsupported):
			t.Errorf("row %s (%q): must still decline, got (%v, %v)", tc.row, tc.expr, got, err)
		}
	}
}
