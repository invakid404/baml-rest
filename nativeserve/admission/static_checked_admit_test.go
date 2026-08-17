package admission

// De-BAML Slice 7.2b-3 — the ADMISSION-side half of the one-fingerprint cutover.
//
// internal/debaml's own tests drive the three checkSupported* cut-line functions,
// SupportsNativeFinalBundle, ParseStaticBundle, ParseStaticBundleUnaryCall and root
// Parse over every companion row (four under 7.2b-3, 24 since Slice 7.2c-3 widened the
// predicate to the six direct comparisons). They cannot drive THESE gates: nativeserve
// imports internal/debaml, so the dependency only runs one way and both admission gates
// have to be asserted here.
//
// The two CLASSES are the exact concrete generated fixture return types the 7.2b scope
// admits as the first production-admission fingerprint — the same two the staticserve
// fixture project declares and the same two internal/debaml/checkedwire captured stock
// bytes for. Slice 7.2c-3 widened the PREDICATE on them, not the class set, so this file
// now drives both levels x all six operators ([checkedAdmittedOperators]):
//
//	class StaticCheckedAnswer { answer string; confidence int @check(positive, {{ this OP 0 }}) }
//	class StaticAssertAnswer  { answer string; confidence int @assert(positive, {{ this OP 0 }}) }
//
// BOTH admission gates must now ADMIT them — the native-final SUPPORT predicate
// (checkStaticReturnBundle) and the RETURN-SHAPE gate (admittedStaticReturnShape) — and
// every return-schema sibling must still be refused BEFORE any socket, by BOTH.
//
// Those two are the WHOLE return-shape decision. There is no codegen-side twin: codegen
// cannot see a method's return SCHEMA (adapters/common does not depend on the root module
// and its Introspection carries no static descriptors), so it emits the seam
// unconditionally and admission decides. adapters/common/codegen's
// TestCodegenMakesNoStaticReturnShapeClaim pins that absence from the other side.

import (
	"testing"

	"github.com/invakid404/baml-rest/bamlutils/promptdescriptor"
	"github.com/invakid404/baml-rest/bamlutils/schemadescriptor"
	"github.com/invakid404/baml-rest/internal/debaml"
	"github.com/invakid404/baml-rest/internal/schema"
)

// strPtrAdmit returns a pointer to s, so a PRESENT label (including an empty one) is
// written distinctly from an absent one.
func strPtrAdmit(s string) *string { return &s }

// lowerCheckedFixture lowers a fixture descriptor to the internal Bundle, FAILING on a
// lowering error: a descriptor this test cannot lower is not evidence of anything.
func lowerCheckedFixture(t *testing.T, desc schemadescriptor.Bundle) *schema.Bundle {
	t.Helper()
	b, err := schema.FromStaticDescriptor(desc)
	if err != nil {
		t.Fatalf("lower the fixture descriptor: %v", err)
	}
	return b
}

// checkedFixtureDescriptor builds one of the two narrow return descriptors. label is
// empty for an unlabelled assert; level "" strips the constraint entirely.
func checkedFixtureDescriptor(class string, level schemadescriptor.ConstraintLevel, label, expr string) schemadescriptor.Bundle {
	// An EMPTY label argument means ABSENT — a nil pointer. A PRESENT-but-empty label is
	// a different descriptor and is built by checkedFixtureDescriptorLabelPtr.
	if label == "" {
		return checkedFixtureDescriptorLabelPtr(class, level, nil, expr)
	}
	l := label
	return checkedFixtureDescriptorLabelPtr(class, level, &l, expr)
}

// checkedFixtureDescriptorLabelPtr builds the return descriptor with the constraint label
// given as a POINTER, so an absent label (nil) and a present-but-empty one (&"") are
// distinct inputs.
//
// schema.lowerConstraints deliberately preserves that distinction and validates only the
// level, so a present-empty label really does reach the fingerprint through the ordinary
// descriptor ingress — which is why it has to be constructible here.
func checkedFixtureDescriptorLabelPtr(class string, level schemadescriptor.ConstraintLevel, label *string, expr string) schemadescriptor.Bundle {
	confidence := schemadescriptor.Type{
		Kind: schemadescriptor.TypePrimitive, Primitive: schemadescriptor.PrimitiveInt,
	}
	if level != "" {
		confidence.Meta.Constraints = []schemadescriptor.Constraint{
			{Level: level, Expression: expr, Label: label},
		}
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

// checkedAdmittedOperators are the direct comparisons the root-owned fingerprint
// admits, written out INDEPENDENTLY of the root package.
//
// De-BAML Slice 7.2c-3 widened the fingerprint from `this > I` to all six, and this
// module is where the widening has to be re-measured rather than inherited: the
// return-shape gate delegates to debaml.IsAdmittedStaticCheckedFamily, so a delegate
// that had drifted — or a root predicate that moved without this module noticing —
// would show up as a gate disagreement on these rows.
//
// They are spelled out here, not imported, precisely so this is a second statement of
// the manifest rather than a restatement of the same variable.
func checkedAdmittedOperators() []string {
	return []string{">", ">=", "<", "<=", "==", "!="}
}

// checkedFixtureRows are the admitted fixtures: both levels x every admitted operator.
//
// It was TWO rows until Slice 7.2c-3 (the `>` check and assert fixtures) and is now
// twelve, which is the same widening the root package's served manifest records as
// 6 operators x 4 outcomes = 24 (admission is outcome-blind, so it sees 6 x 2).
func checkedFixtureRows() []struct {
	name string
	desc schemadescriptor.Bundle
} {
	var out []struct {
		name string
		desc schemadescriptor.Bundle
	}
	for _, op := range checkedAdmittedOperators() {
		expr := "this " + op + " 0"
		out = append(out, struct {
			name string
			desc schemadescriptor.Bundle
		}{"checked fixture " + expr, checkedFixtureDescriptor(
			"StaticCheckedAnswer", schemadescriptor.ConstraintCheck, "positive", expr)},
			struct {
				name string
				desc schemadescriptor.Bundle
			}{"assert fixture " + expr, checkedFixtureDescriptor(
				"StaticAssertAnswer", schemadescriptor.ConstraintAssert, "positive", expr)})
	}
	return out
}

// TestStaticCheckedFingerprintIsAdmittedAtAdmission drives BOTH admission gates over the
// two narrow fixtures and requires each to ADMIT.
//
// Driving both in one place is the point: a cutover that lifted only the support
// predicate would leave the return-shape gate refusing the very shape the parser is now
// prepared to serve, and the route would decline for a reason nobody could attribute.
func TestStaticCheckedFingerprintIsAdmittedAtAdmission(t *testing.T) {
	for _, tc := range checkedFixtureRows() {
		t.Run(tc.name, func(t *testing.T) {
			fn := promptdescriptor.Function{Method: "M", Return: tc.desc}
			bundle, obs := checkStaticReturnBundle(fn)
			if obs != nil {
				t.Fatalf("admission DECLINED the constraint-bearing return bundle at stage %q reason %q; "+
					"the 7.2b-3 cutover admits this exact fingerprint", obs.Stage, obs.Reason)
			}
			if bundle == nil {
				t.Fatal("admission admitted but produced no lowered bundle")
			}
			// The SECOND gate — the return-shape set, which is the SOLE pre-claim
			// return-shape decision (codegen makes none) — must agree, or
			// AdmitStaticClaim would decline with reasonReturnShapeUnproven after the
			// support predicate said yes.
			if !admittedStaticReturnShape(bundle) {
				t.Fatal("the return-shape gate REFUSED a bundle the support predicate admitted; the two " +
					"admission gates must share one fingerprint")
			}
			// And it is the ROOT-owned predicate that decided, not a restatement here.
			if !debaml.IsAdmittedStaticCheckedFamily(bundle) {
				t.Fatal("the root-owned fingerprint does not recognise the bundle both admission gates " +
					"admitted; this module has drifted from the parser that must produce the bytes")
			}
		})
	}
}

// TestStaticCheckedAdmissionIsAttributedToTheFingerprint is the control that makes the
// admits above mean "this fingerprint" rather than "constraints are fine now".
//
// Every row is a RETURN-SCHEMA SIBLING of an admitted fixture — the same generated
// return descriptor with the one category the row's name gives changed — and must be
// refused by BOTH gates, BEFORE any socket.
//
// Most rows vary a SINGLE property. One does not, and it says so at the row itself
// ("the constraint on the OTHER field"): what it witnesses is that the family is refused
// pre-socket, not which axis refused it. Single-axis attribution for constraint
// LOCATION, predicate and label is internal/debaml's sibling corpus's job, which varies
// each independently.
//
// The refusal reason is required for the support predicate: checkStaticReturnBundle has
// three distinct decline stages, and only reasonReturnBundleFinalUnsupported means "the
// fingerprint did not recognise it".
func TestStaticCheckedAdmissionIsAttributedToTheFingerprint(t *testing.T) {
	label := func(s string) *string { return &s }
	base := func() schemadescriptor.Bundle {
		return checkedFixtureDescriptor("StaticCheckedAnswer", schemadescriptor.ConstraintCheck, "positive", "this > 0")
	}
	mutate := func(fn func(*schemadescriptor.Bundle)) schemadescriptor.Bundle {
		d := base()
		fn(&d)
		return d
	}
	siblings := []struct {
		name string
		desc schemadescriptor.Bundle
	}{
		{"a differently-named class", checkedFixtureDescriptor(
			"SomeOtherAnswer", schemadescriptor.ConstraintCheck, "positive", "this > 0")},
		{"the assert level on the check class", checkedFixtureDescriptor(
			"StaticCheckedAnswer", schemadescriptor.ConstraintAssert, "positive", "this > 0")},
		{"the check level on the assert class", checkedFixtureDescriptor(
			"StaticAssertAnswer", schemadescriptor.ConstraintCheck, "positive", "this > 0")},
		{"a second constraint", mutate(func(d *schemadescriptor.Bundle) {
			d.Classes[0].Fields[1].Type.Meta.Constraints = append(d.Classes[0].Fields[1].Type.Meta.Constraints,
				schemadescriptor.Constraint{Level: schemadescriptor.ConstraintCheck,
					Expression: "this > 1", Label: label("other")})
		})},
		{"a duplicate check label", mutate(func(d *schemadescriptor.Bundle) {
			d.Classes[0].Fields[1].Type.Meta.Constraints = append(d.Classes[0].Fields[1].Type.Meta.Constraints,
				schemadescriptor.Constraint{Level: schemadescriptor.ConstraintCheck,
					Expression: "this > 1", Label: label("positive")})
		})},
		{"a check plus an assert", mutate(func(d *schemadescriptor.Bundle) {
			d.Classes[0].Fields[1].Type.Meta.Constraints = append(d.Classes[0].Fields[1].Type.Meta.Constraints,
				schemadescriptor.Constraint{Level: schemadescriptor.ConstraintAssert,
					Expression: "this > 1", Label: label("a")})
		})},
		{"the two fields in the other order", mutate(func(d *schemadescriptor.Bundle) {
			f := d.Classes[0].Fields
			f[0], f[1] = f[1], f[0]
		})},
		{"an aliased field", mutate(func(d *schemadescriptor.Bundle) {
			d.Classes[0].Fields[1].Name.Alias = label("score")
		})},
		{"an aliased class", mutate(func(d *schemadescriptor.Bundle) {
			d.Classes[0].Name.Alias = label("Answer")
		})},
		{"a third field", mutate(func(d *schemadescriptor.Bundle) {
			d.Classes[0].Fields = append(d.Classes[0].Fields, schemadescriptor.ClassField{
				Name: schemadescriptor.Name{Name: "extra"},
				Type: schemadescriptor.Type{Kind: schemadescriptor.TypePrimitive, Primitive: schemadescriptor.PrimitiveString},
			})
		})},
		// The one row in this table that is NOT single-axis, stated rather than glossed:
		// it moves the constraint OFF `confidence` and onto `answer`, carrying a
		// string-appropriate predicate and label with it. Nothing forces that — these are
		// hand-built descriptors that never have to compile, and schema.lowerConstraints
		// carries the expression opaquely — the row is written to look like a schema
		// someone would actually author. So it witnesses that the family is refused
		// pre-socket, and internal/debaml's corpus is what attributes constraint LOCATION
		// on its own: its "a constraint on the OTHER field" row KEEPS the admitted
		// confidence constraint and adds an otherwise-unchanged one to `answer`.
		{"the constraint on the OTHER field", mutate(func(d *schemadescriptor.Bundle) {
			d.Classes[0].Fields[1].Type.Meta.Constraints = nil
			d.Classes[0].Fields[0].Type.Meta.Constraints = []schemadescriptor.Constraint{{
				Level: schemadescriptor.ConstraintCheck, Expression: `this != ""`, Label: label("a")}}
		})},
		{"a class-level constraint", mutate(func(d *schemadescriptor.Bundle) {
			d.Classes[0].Constraints = []schemadescriptor.Constraint{{
				Level: schemadescriptor.ConstraintCheck, Expression: "this.confidence > 0", Label: label("c")}}
		})},
		{"a target-level constraint", mutate(func(d *schemadescriptor.Bundle) {
			d.Target.Meta.Constraints = []schemadescriptor.Constraint{{
				Level: schemadescriptor.ConstraintCheck, Expression: "this.confidence > 0", Label: label("t")}}
		})},
		// De-BAML Slice 7.2c-3 RE-POINTED this row. It used to carry `this >= 0`, which
		// the cutover ADMITS — it is a served fixture row above now. The unproven
		// predicate axis is kept, one step further out: `<>` is a real alternate
		// spelling of the admitted `!=` in several template languages, so it is what a
		// gate written from the operator LIST rather than from the stock captures would
		// grow into, and it has no capture at all.
		{"an unproven predicate (a seventh comparison form)", checkedFixtureDescriptor(
			"StaticCheckedAnswer", schemadescriptor.ConstraintCheck, "positive", "this <> 0")},
		// The REVERSED operand order: a well-formed comparison denoting an ADMITTED
		// relation, with `this` on the right. It is the likeliest thing to be waved
		// through as obviously equivalent, and stock would retain `0 < this` verbatim —
		// a string no capture covers.
		{"an unproven predicate (operands reversed)", checkedFixtureDescriptor(
			"StaticCheckedAnswer", schemadescriptor.ConstraintCheck, "positive", "0 < this")},
		// A COMPOUND of two ADMITTED comparisons — #583, and the form the widening makes
		// most tempting.
		{"an unproven predicate (a compound of two admitted comparisons)", checkedFixtureDescriptor(
			"StaticCheckedAnswer", schemadescriptor.ConstraintCheck, "positive", "this >= 0 and this <= 100")},
		{"a non-canonical literal", checkedFixtureDescriptor(
			"StaticCheckedAnswer", schemadescriptor.ConstraintCheck, "positive", "this > 007")},
		{"a non-ASCII label", checkedFixtureDescriptor(
			"StaticCheckedAnswer", schemadescriptor.ConstraintCheck, "positifé", "this > 0")},
		{"an unlabelled check", mutate(func(d *schemadescriptor.Bundle) {
			d.Classes[0].Fields[1].Type.Meta.Constraints[0].Label = nil
		})},
		// PRESENT-BUT-EMPTY labels. `&""` is not `nil`: schema.lowerConstraints preserves
		// the distinction and validates only the level, so this descriptor lowers and
		// ValidateOutput succeeds. The ASSERT row is the one that matters — an assert may
		// omit its label, so a fingerprint that read the NORMALISED string admitted this
		// as "absent" through every gate.
		{"an assert with a present-but-EMPTY label", checkedFixtureDescriptorLabelPtr(
			"StaticAssertAnswer", schemadescriptor.ConstraintAssert, strPtrAdmit(""), "this > 0")},
		{"a check with a present-but-EMPTY label", checkedFixtureDescriptorLabelPtr(
			"StaticCheckedAnswer", schemadescriptor.ConstraintCheck, strPtrAdmit(""), "this > 0")},
		{"a float confidence", mutate(func(d *schemadescriptor.Bundle) {
			d.Classes[0].Fields[1].Type.Primitive = schemadescriptor.PrimitiveFloat
		})},
		{"a renamed field", mutate(func(d *schemadescriptor.Bundle) {
			d.Classes[0].Fields[1].Name.Name = "score"
		})},
		// The COLLECTION / OPTIONAL families the scope leaves declined. Each keeps the
		// constraint where the fingerprint admits it for an `int` and changes only the
		// field's KIND, so the refusal is that kind's.
		{"a list confidence", mutate(func(d *schemadescriptor.Bundle) {
			elem := schemadescriptor.Type{Kind: schemadescriptor.TypePrimitive, Primitive: schemadescriptor.PrimitiveInt}
			t := &d.Classes[0].Fields[1].Type
			t.Kind, t.Primitive, t.Elem = schemadescriptor.TypeList, "", &elem
		})},
		{"a map confidence", mutate(func(d *schemadescriptor.Bundle) {
			key := schemadescriptor.Type{Kind: schemadescriptor.TypePrimitive, Primitive: schemadescriptor.PrimitiveString}
			val := schemadescriptor.Type{Kind: schemadescriptor.TypePrimitive, Primitive: schemadescriptor.PrimitiveInt}
			t := &d.Classes[0].Fields[1].Type
			t.Kind, t.Primitive, t.Key, t.Value = schemadescriptor.TypeMap, "", &key, &val
		})},
		{"an optional confidence", mutate(func(d *schemadescriptor.Bundle) {
			inner := schemadescriptor.Type{Kind: schemadescriptor.TypePrimitive, Primitive: schemadescriptor.PrimitiveInt}
			t := &d.Classes[0].Fields[1].Type
			t.Kind, t.Primitive = schemadescriptor.TypeUnion, ""
			t.Union = &schemadescriptor.UnionType{Nullable: true, Variants: []schemadescriptor.Type{inner}}
		})},
		{"a multi-arm union confidence", mutate(func(d *schemadescriptor.Bundle) {
			i := schemadescriptor.Type{Kind: schemadescriptor.TypePrimitive, Primitive: schemadescriptor.PrimitiveInt}
			str := schemadescriptor.Type{Kind: schemadescriptor.TypePrimitive, Primitive: schemadescriptor.PrimitiveString}
			t := &d.Classes[0].Fields[1].Type
			t.Kind, t.Primitive = schemadescriptor.TypeUnion, ""
			t.Union = &schemadescriptor.UnionType{Variants: []schemadescriptor.Type{i, str}}
		})},
		{"a bool confidence", mutate(func(d *schemadescriptor.Bundle) {
			d.Classes[0].Fields[1].Type.Primitive = schemadescriptor.PrimitiveBool
		})},
		{"a string confidence", mutate(func(d *schemadescriptor.Bundle) {
			d.Classes[0].Fields[1].Type.Primitive = schemadescriptor.PrimitiveString
		})},
	}
	if len(siblings) < 20 {
		t.Fatalf("only %d siblings; the fingerprint's narrowness at admission would be barely witnessed",
			len(siblings))
	}
	for _, tc := range siblings {
		t.Run(tc.name, func(t *testing.T) {
			fn := promptdescriptor.Function{Method: "M", Return: tc.desc}
			bundle, obs := checkStaticReturnBundle(fn)
			if obs == nil {
				t.Fatalf("admission ADMITTED a return-schema sibling (lowered %+v); native would serve a "+
					"schema with no stock byte capture behind it", bundle)
			}
			if bundle != nil {
				t.Error("admission declined but still returned a lowered bundle")
			}
			// The EXACT reason: this claim is that the shape falls through the native-final
			// SUPPORT predicate because the fingerprint did not recognise it. A malformed
			// descriptor rejected one stage earlier would satisfy a bare non-nil check while
			// proving nothing.
			if Reason(obs.Reason) != reasonReturnBundleFinalUnsupported {
				t.Errorf("decline reason = %q, want %q (the native FINAL support predicate)",
					obs.Reason, reasonReturnBundleFinalUnsupported)
			}
			if Stage(obs.Stage) != StagePrompt {
				t.Errorf("decline stage = %q, want %q (PRE-SOCKET)", obs.Stage, StagePrompt)
			}
			// The SECOND gate refuses it independently, so a regression in either one is
			// caught rather than masked by the other.
			if admittedStaticReturnShape(lowerCheckedFixture(t, tc.desc)) {
				t.Error("the return-shape gate ADMITTED a return-schema sibling; it delegates to the " +
					"root-owned fingerprint and must give the same answer")
			}
		})
	}
}

// TestStaticCheckedAdmissionStripsToTheUnconstrainedShape is the other attribution
// direction: the constraint-stripped twin is the ordinary StaticAnswer shape both gates
// already served before this slice, so the admits above are not the two-field class
// merely becoming admissible for the first time.
func TestStaticCheckedAdmissionStripsToTheUnconstrainedShape(t *testing.T) {
	for _, class := range []string{"StaticCheckedAnswer", "StaticAssertAnswer"} {
		t.Run(class, func(t *testing.T) {
			stripped := checkedFixtureDescriptor(class, "", "", "")
			fn := promptdescriptor.Function{Method: "M", Return: stripped}
			bundle, obs := checkStaticReturnBundle(fn)
			if obs != nil {
				t.Fatalf("the constraint-STRIPPED twin was declined (%q)", obs.Reason)
			}
			if bundle == nil {
				t.Fatal("the stripped twin admitted but produced no lowered bundle")
			}
			if !admittedStaticReturnShape(bundle) {
				t.Fatal("the constraint-stripped twin is outside the admitted return-shape set")
			}
			// …and it is admitted by the ORDINARY StaticAnswer arm, not by the new
			// checked-static one, so the two arms are not being conflated.
			if debaml.IsAdmittedStaticCheckedFamily(bundle) {
				t.Fatal("the checked-static fingerprint claimed a bundle carrying NO constraint; the " +
					"fingerprint is not about this shape")
			}
		})
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
// being counted as evidence for the field guard.
func TestStaticCheckedDynamicPrimitiveIsDescriptorInvalid(t *testing.T) {
	dynamicField := func(class string, field int) schemadescriptor.Bundle {
		d := checkedFixtureDescriptor(class, schemadescriptor.ConstraintCheck, "positive", "this > 0")
		d.Classes[0].Fields[field].Type.Dynamic = true
		return d
	}

	for _, tc := range []struct {
		name string
		desc schemadescriptor.Bundle
	}{
		{"dynamic answer", dynamicField("StaticCheckedAnswer", 0)},
		{"dynamic confidence", dynamicField("StaticCheckedAnswer", 1)},
	} {
		bundle, obs := checkStaticReturnBundle(promptdescriptor.Function{Method: "M", Return: tc.desc})
		if obs == nil {
			t.Errorf("%s: admission ADMITTED a dynamic primitive return (lowered %+v)", tc.name, bundle)
			continue
		}
		if bundle != nil {
			t.Errorf("%s: admission declined but still returned a lowered bundle", tc.name)
		}
		// The REASON matters: this ingress is protected by descriptor validation, not by
		// the native-final support predicate, and conflating the two would let a
		// regression in either look like the other still holding.
		if Reason(obs.Reason) != reasonReturnBundleInvalid {
			t.Errorf("%s: decline reason = %q, want %q (descriptor lowering, not the support predicate)",
				tc.name, obs.Reason, reasonReturnBundleInvalid)
		}
	}
	// CONTROL: the same descriptor WITHOUT the dynamic bit is ADMITTED, so the reason
	// above is attributable to the dynamic bit rather than to the constraint.
	clean := checkedFixtureDescriptor("StaticCheckedAnswer", schemadescriptor.ConstraintCheck, "positive", "this > 0")
	if _, obs := checkStaticReturnBundle(promptdescriptor.Function{Method: "M", Return: clean}); obs != nil {
		t.Fatalf("the non-dynamic control declined (%q); the assertions above would not be attributable "+
			"to the dynamic bit", obs.Reason)
	}
}

// staticCheckedEmptyCollection is one collection the fingerprint requires ABSENT, in its
// NON-NIL EMPTY form, applied to an already-lowered Bundle.
type staticCheckedEmptyCollection struct {
	name string
	set  func(*schema.Bundle)
}

// TestStaticCheckedReturnShapeRefusesNonNilEmptyCollections drives the return-shape gate
// over the PRE-LOWERED Bundle ingress, which is the one this class of over-claim lives at.
//
// It is a separate test from the descriptor sibling table above, and necessarily so: the
// descriptor lowerer NORMALISES an empty constraint slice to nil (measured — a descriptor
// carrying `Constraints: []schemadescriptor.Constraint{}` lowers to a nil slice), so the
// case cannot be expressed through a descriptor at all. But `admittedStaticReturnShape`
// is reached with an already-lowered *schema.Bundle, and
// `debaml.SupportsNativeFinalBundle` / `debaml.ParseStaticBundleUnaryCall` accept one
// directly — so a hand-constructed, ValidateOutput-valid Bundle is a real ingress, and
// that is where a length-based absence test admitted a populated metadata payload.
//
// Each row is ONE property away from the admitted fixture, differing only by a slice that
// is non-nil and zero-length.
func TestStaticCheckedReturnShapeRefusesNonNilEmptyCollections(t *testing.T) {
	base := func(t *testing.T) *schema.Bundle {
		t.Helper()
		return lowerCheckedFixture(t, checkedFixtureDescriptor(
			"StaticCheckedAnswer", schemadescriptor.ConstraintCheck, "positive", "this > 0"))
	}
	// CONTROL: the unmutated lowered fixture IS admitted by both gates, so every
	// rejection below is about the one empty slice.
	if b := base(t); !admittedStaticReturnShape(b) || !debaml.IsAdmittedStaticCheckedFamily(b) {
		t.Fatal("the lowered control fixture is not admitted; every rejection below would be vacuous")
	}

	for _, tc := range []staticCheckedEmptyCollection{
		{"Bundle.Enums", func(b *schema.Bundle) { b.Enums = []schema.EnumDef{} }},
		{"Bundle.RecursiveClasses", func(b *schema.Bundle) { b.RecursiveClasses = []string{} }},
		{"Bundle.StructuralRecursiveAliases", func(b *schema.Bundle) {
			b.StructuralRecursiveAliases = []schema.RecursiveAliasDef{}
		}},
		{"ClassDef.Constraints", func(b *schema.Bundle) { b.Classes[0].Constraints = []schema.Constraint{} }},
		{"TARGET Type.Meta.Constraints", func(b *schema.Bundle) {
			b.Target.Meta.Constraints = []schema.Constraint{}
		}},
		{"ANSWER Type.Meta.Constraints", func(b *schema.Bundle) {
			b.Classes[0].Fields[0].Type.Meta.Constraints = []schema.Constraint{}
		}},
		{"TARGET Type.Items", func(b *schema.Bundle) { b.Target.Items = []schema.Type{} }},
		{"ANSWER Type.Items", func(b *schema.Bundle) { b.Classes[0].Fields[0].Type.Items = []schema.Type{} }},
		{"CONFIDENCE Type.Items", func(b *schema.Bundle) { b.Classes[0].Fields[1].Type.Items = []schema.Type{} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := base(t)
			tc.set(b)
			// It really is a valid pre-lowered Bundle — otherwise the gate would be
			// refusing a malformed input rather than a non-canonical one.
			if err := b.ValidateOutput(); err != nil {
				t.Fatalf("the mutated bundle is not ValidateOutput-valid (%v); the rejection below "+
					"would witness invalidity rather than non-canonicality", err)
			}
			if admittedStaticReturnShape(b) {
				t.Errorf("the return-shape gate ADMITTED a non-nil EMPTY %s; absence must be a nil "+
					"test, never a length test", tc.name)
			}
			if debaml.IsAdmittedStaticCheckedFamily(b) {
				t.Errorf("the root fingerprint ADMITTED a non-nil EMPTY %s", tc.name)
			}
		})
	}
}
