package admission

// De-BAML Slice 7.2b-2 — the ADMISSION-side half of the non-admitting seam.
//
// internal/debaml's own tests drive the three checkSupported* cut-line functions,
// SupportsNativeFinalBundle, ParseStaticBundle and root Parse over the four companion
// rows. They cannot drive THIS gate: nativeserve imports internal/debaml, so the
// dependency only runs one way and the admission decline has to be asserted here.
//
// The two shapes are the exact concrete generated fixture return types the 7.2b scope
// admits as the first production-admission fingerprint — the same two the staticserve
// fixture project declares and the same two internal/debaml/checkedwire captured stock
// bytes for:
//
//	class StaticCheckedAnswer { answer string; confidence int @check(positive, {{ this > 0 }}) }
//	class StaticAssertAnswer  { answer string; confidence int @assert(positive, {{ this > 0 }}) }
//
// Both must be refused by admission's return-shape gate BEFORE any socket, and the
// refusal must be attributable to the constraint rather than to the shape.

import (
	"testing"

	"github.com/invakid404/baml-rest/bamlutils/promptdescriptor"
	"github.com/invakid404/baml-rest/bamlutils/schemadescriptor"
	"github.com/invakid404/baml-rest/internal/debaml"
	"github.com/invakid404/baml-rest/internal/schema"
)

// lowerCheckedFixture lowers a fixture descriptor to the internal Bundle, FAILING on a
// lowering error: a descriptor this test cannot lower is not evidence of a decline.
func lowerCheckedFixture(t *testing.T, desc schemadescriptor.Bundle) *schema.Bundle {
	t.Helper()
	b, err := schema.FromStaticDescriptor(desc)
	if err != nil {
		t.Fatalf("lower the fixture descriptor: %v", err)
	}
	return b
}

// checkedFixtureDescriptor builds one of the two narrow return descriptors. label is
// empty for an unlabelled assert.
func checkedFixtureDescriptor(class string, level schemadescriptor.ConstraintLevel, label, expr string) schemadescriptor.Bundle {
	confidence := schemadescriptor.Type{
		Kind: schemadescriptor.TypePrimitive, Primitive: schemadescriptor.PrimitiveInt,
	}
	if level != "" {
		c := schemadescriptor.Constraint{Level: level, Expression: expr}
		if label != "" {
			l := label
			c.Label = &l
		}
		confidence.Meta.Constraints = []schemadescriptor.Constraint{c}
	}
	return schemadescriptor.Bundle{
		Version: schemadescriptor.Version,
		Method:  "M",
		Target:  schemadescriptor.Type{Kind: schemadescriptor.TypeClass, Name: class, Mode: schemadescriptor.NonStreaming},
		Classes: []schemadescriptor.ClassDef{{
			Name: schemadescriptor.Name{Name: class},
			Mode: schemadescriptor.NonStreaming,
			Fields: []schemadescriptor.ClassField{
				{Name: schemadescriptor.Name{Name: "answer"},
					Type: schemadescriptor.Type{Kind: schemadescriptor.TypePrimitive, Primitive: schemadescriptor.PrimitiveString}},
				{Name: schemadescriptor.Name{Name: "confidence"}, Type: confidence},
			},
		}},
	}
}

// TestStaticCheckedFingerprintIsDeclinedAtAdmission drives admission's return-shape
// gate over the two narrow fixtures and requires a PRE-SOCKET decline from each.
//
// The decline must come from the FINAL-parser support predicate specifically
// (reasonReturnBundleFinalUnsupported): that is the gate internal/debaml owns and the
// one the 7.2b-3 cutover moves, so naming it is what ties this assertion to the seam
// rather than to some unrelated envelope check.
func TestStaticCheckedFingerprintIsDeclinedAtAdmission(t *testing.T) {
	for _, tc := range []struct {
		name string
		desc schemadescriptor.Bundle
	}{
		{"checked fixture", checkedFixtureDescriptor(
			"StaticCheckedAnswer", schemadescriptor.ConstraintCheck, "positive", "this > 0")},
		{"assert fixture", checkedFixtureDescriptor(
			"StaticAssertAnswer", schemadescriptor.ConstraintAssert, "positive", "this > 0")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fn := promptdescriptor.Function{Method: "M", Return: tc.desc}
			bundle, obs := checkStaticReturnBundle(fn)
			if obs == nil {
				t.Fatalf("admission ACCEPTED the constraint-bearing return bundle (lowered %+v); the "+
					"checked-static seam is closed until 7.2b-3", bundle)
			}
			if bundle != nil {
				t.Errorf("admission declined but still returned a lowered bundle")
			}
			if Reason(obs.Reason) != reasonReturnBundleFinalUnsupported {
				t.Errorf("decline reason = %q, want %q (the native FINAL support predicate, which is "+
					"the gate the cutover moves)", obs.Reason, reasonReturnBundleFinalUnsupported)
			}
			if Stage(obs.Stage) != StagePrompt {
				t.Errorf("decline stage = %q, want %q (PRE-SOCKET)", obs.Stage, StagePrompt)
			}
		})
	}
}

// TestStaticCheckedAdmissionDeclineIsAttributedToTheConstraint is the control that makes
// the declines above mean "the constraint stopped it" rather than "the shape is not
// admitted at all".
//
// The constraint-stripped twin of each fixture is the SAME two-field
// `answer:string, confidence:int` class the admitted StaticAnswer family already
// serves, so it must pass BOTH admission gates. Without this, a gate that refused every
// two-field class would satisfy the assertions above.
func TestStaticCheckedAdmissionDeclineIsAttributedToTheConstraint(t *testing.T) {
	for _, class := range []string{"StaticCheckedAnswer", "StaticAssertAnswer"} {
		t.Run(class, func(t *testing.T) {
			stripped := checkedFixtureDescriptor(class, "", "", "")
			fn := promptdescriptor.Function{Method: "M", Return: stripped}
			bundle, obs := checkStaticReturnBundle(fn)
			if obs != nil {
				t.Fatalf("the constraint-STRIPPED twin was declined (%q); the constraint-bearing decline "+
					"is then not attributable to the constraint", obs.Reason)
			}
			if bundle == nil {
				t.Fatal("the stripped twin admitted but produced no lowered bundle")
			}
			if !admittedStaticReturnShape(bundle) {
				t.Fatal("the constraint-stripped twin is outside the admitted return-shape set; the " +
					"constraint-bearing declines above would witness the shape, not the constraint")
			}
		})
	}
}

// TestStaticCheckedReturnShapeGateRefusesTheConstraint pins the SECOND admission gate —
// the return-shape set that must stay in lockstep with the codegen serve-seam emission —
// over the same two fixtures, so a change that lifted only the support predicate would
// still be caught here.
func TestStaticCheckedReturnShapeGateRefusesTheConstraint(t *testing.T) {
	for _, tc := range []struct {
		name string
		desc schemadescriptor.Bundle
	}{
		{"checked fixture", checkedFixtureDescriptor(
			"StaticCheckedAnswer", schemadescriptor.ConstraintCheck, "positive", "this > 0")},
		{"assert fixture", checkedFixtureDescriptor(
			"StaticAssertAnswer", schemadescriptor.ConstraintAssert, "positive", "this > 0")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Lowering is done directly here because checkStaticReturnBundle refuses to
			// return a bundle for a declined descriptor — which is precisely the property
			// the previous test pins, and would leave this one with nothing to drive.
			bundle := lowerCheckedFixture(t, tc.desc)
			if admittedStaticReturnShape(bundle) {
				t.Fatal("the return-shape gate ADMITTED a constraint-bearing return; it must stay in " +
					"lockstep with the closed checked-static seam")
			}
		})
	}
}

// TestStaticCheckedAdmissionOpensThroughTheRealGate is the anti-false-green control for
// the declines above, and it executes the REAL admission gate in both states.
//
// `internal/debaml` owns the checked-static seam and exposes a narrow,
// descriptor-specific opener for exactly this purpose. Opening it and re-running
// `checkStaticReturnBundle` — the same production function, over the same descriptors —
// must ADMIT: that is what proves the closed declines above are caused by the seam
// rather than by something else in the envelope refusing the shape anyway.
//
// The SECOND admission gate, `admittedStaticReturnShape`, is nativeserve's own
// codegen-lockstep lock and is NOT moved by the debaml seam. It must keep declining even
// with the seam open — its exemption is 7.2b-3's own step in this module, and asserting
// that here is what stops a one-sided cutover from looking complete.
func TestStaticCheckedAdmissionOpensThroughTheRealGate(t *testing.T) {
	rows := []struct {
		name string
		desc schemadescriptor.Bundle
	}{
		{"checked fixture", checkedFixtureDescriptor(
			"StaticCheckedAnswer", schemadescriptor.ConstraintCheck, "positive", "this > 0")},
		{"assert fixture", checkedFixtureDescriptor(
			"StaticAssertAnswer", schemadescriptor.ConstraintAssert, "positive", "this > 0")},
	}

	// CLOSED first, in this test, so the two states are compared over the same
	// descriptors in one place rather than across files.
	for _, r := range rows {
		if _, obs := checkStaticReturnBundle(promptdescriptor.Function{Method: "M", Return: r.desc}); obs == nil {
			t.Fatalf("%s: admission ADMITTED with the seam closed; the open comparison below would "+
				"witness nothing", r.name)
		}
	}

	restore := debaml.OpenStaticCheckedSeamForTest()
	defer restore()

	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			bundle, obs := checkStaticReturnBundle(promptdescriptor.Function{Method: "M", Return: r.desc})
			if obs != nil {
				t.Fatalf("admission still DECLINED (%q) with the checked-static seam OPEN; the closed "+
					"decline is then not attributable to the seam", obs.Reason)
			}
			if bundle == nil {
				t.Fatal("admission admitted but produced no lowered bundle")
			}
			// nativeserve's OWN lock stays shut: the debaml seam does not move it.
			if admittedStaticReturnShape(bundle) {
				t.Fatal("the return-shape gate ADMITTED with only the debaml seam open; this module's " +
					"lockstep exemption is 7.2b-3's own step and must not be reachable from there")
			}
		})
	}

	// NEGATIVE control: a constraint-bearing shape OUTSIDE the two fixture identities is
	// still declined with the seam open, so the movement above is the fingerprint's and
	// not a general lifting of the constraint cut-line.
	//
	// The REASON is required, not just "some observation". checkStaticReturnBundle has
	// three distinct decline stages (invalid descriptor, not output-usable, native-final
	// unsupported), and this control's whole claim is that the shape falls through the
	// SUPPORT predicate because the fingerprint did not recognise it. A malformed
	// descriptor rejected one stage earlier would satisfy a bare non-nil check while
	// proving nothing about the fingerprint.
	outside := checkedFixtureDescriptor("SomeOtherAnswer", schemadescriptor.ConstraintCheck, "positive", "this > 0")
	bundle, obs := checkStaticReturnBundle(promptdescriptor.Function{Method: "M", Return: outside})
	if obs == nil {
		t.Fatal("admission ADMITTED a differently-named constraint-bearing class with the seam open; the " +
			"seam must be descriptor-specific")
	}
	if bundle != nil {
		t.Error("admission declined the outside shape but still returned a lowered bundle")
	}
	if Reason(obs.Reason) != reasonReturnBundleFinalUnsupported {
		t.Errorf("the outside shape declined with reason %q, want %q; this control claims it falls "+
			"through the native-final SUPPORT predicate, so an earlier-stage rejection would prove "+
			"nothing about the fingerprint", obs.Reason, reasonReturnBundleFinalUnsupported)
	}
	if Stage(obs.Stage) != StagePrompt {
		t.Errorf("the outside shape declined at stage %q, want %q (PRE-SOCKET)", obs.Stage, StagePrompt)
	}
}

// TestStaticCheckedDynamicPrimitiveIsDescriptorInvalid pins the descriptor-ingress
// defence at THIS boundary, and names the reason it declines.
//
// `internal/debaml`'s fingerprint refuses a `dynamic` primitive field because no stock
// byte authority covers that variant. Admission never reaches that guard for it: a
// generated method arrives as a descriptor, and `schema.FromStaticDescriptor` refuses
// `dynamic` as a stray payload on a primitive, so `checkStaticReturnBundle` converts the
// lowering failure into `reasonReturnBundleInvalid` before the support predicate runs.
//
// That is a real and independent defence, so it is asserted on its own terms rather than
// being counted as evidence for the field guard — and it is driven in BOTH seam states,
// because a descriptor that never lowers cannot be affected by the seam and showing that
// is the point.
func TestStaticCheckedDynamicPrimitiveIsDescriptorInvalid(t *testing.T) {
	dynamicField := func(class string, field int) schemadescriptor.Bundle {
		d := checkedFixtureDescriptor(class, schemadescriptor.ConstraintCheck, "positive", "this > 0")
		d.Classes[0].Fields[field].Type.Dynamic = true
		return d
	}

	assert := func(t *testing.T, state string) {
		t.Helper()
		for _, tc := range []struct {
			name string
			desc schemadescriptor.Bundle
		}{
			{"dynamic answer", dynamicField("StaticCheckedAnswer", 0)},
			{"dynamic confidence", dynamicField("StaticCheckedAnswer", 1)},
		} {
			bundle, obs := checkStaticReturnBundle(promptdescriptor.Function{Method: "M", Return: tc.desc})
			if obs == nil {
				t.Errorf("%s: %s: admission ADMITTED a dynamic primitive return (lowered %+v)",
					state, tc.name, bundle)
				continue
			}
			if bundle != nil {
				t.Errorf("%s: %s: admission declined but still returned a lowered bundle", state, tc.name)
			}
			// The REASON matters: this ingress is protected by descriptor validation, not
			// by the native-final support predicate, and conflating the two would let a
			// regression in either look like the other still holding.
			if Reason(obs.Reason) != reasonReturnBundleInvalid {
				t.Errorf("%s: %s: decline reason = %q, want %q (descriptor lowering, not the support "+
					"predicate)", state, tc.name, obs.Reason, reasonReturnBundleInvalid)
			}
		}
		// CONTROL: the same descriptor WITHOUT the dynamic bit reaches the support
		// predicate and declines there instead, so the reason above is attributable to
		// the dynamic bit rather than to the constraint.
		clean := checkedFixtureDescriptor("StaticCheckedAnswer", schemadescriptor.ConstraintCheck, "positive", "this > 0")
		_, obs := checkStaticReturnBundle(promptdescriptor.Function{Method: "M", Return: clean})
		if obs == nil {
			// With the seam OPEN the clean descriptor is admitted — which is itself the
			// non-vacuity proof that this harness can distinguish the two outcomes.
			if state == "seam closed" {
				t.Fatal("the non-dynamic control was admitted with the seam CLOSED")
			}
			return
		}
		// The EXACT reason, not merely "not descriptor-invalid": with the seam closed the
		// clean twin must fall through to the native-final SUPPORT predicate, which is
		// what makes the descriptor-invalid reason above attributable to the dynamic bit
		// rather than to something else the envelope happens to refuse.
		if Reason(obs.Reason) != reasonReturnBundleFinalUnsupported {
			t.Fatalf("%s: the non-dynamic control declined with reason %q, want %q; the reason "+
				"assertions above would not be attributable to the dynamic bit",
				state, obs.Reason, reasonReturnBundleFinalUnsupported)
		}
	}

	assert(t, "seam closed")
	restore := debaml.OpenStaticCheckedSeamForTest()
	defer restore()
	assert(t, "seam OPEN")
}
