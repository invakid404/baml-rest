package codegen

import (
	"reflect"
	"strings"
	"testing"

	"github.com/dave/jennifer/jen"

	"github.com/invakid404/baml-rest/bamlutils"
)

// De-BAML Slice 7.2b-2 — the generated static types for a `@check`-bearing return.
//
// A generated BAML client reaches the constraint carrier through an alias
// (`type Checked[T any] = baml.Checked[T]`), and a de-BAML client re-points that alias
// at [bamlutils.Checked] so the emitted bytes are deterministic (stock's plain struct
// under sonic emits `checks` in Go map iteration order, which differs run to run).
// What this file pins is the two consequences for CODEGEN:
//
//  1. the per-method `DecodeNativeStaticFinal` closure is instantiated at BOTH
//     concrete forms the two narrow fixtures need — `Checked[int64]` for the check
//     fixture, and a bare `int64` for the assert fixture — and
//  2. BOTH go through the STRICT [bamlutils.DecodeStaticFinal], never the lenient
//     recursive-alias decoder.
//
// (2) is the part that needed a code change: the carrier declares UnmarshalJSON on its
// pointer receiver, so a top-level checked return satisfies the alias router's
// json.Unmarshaler probe and would have been handed to the decoder that deliberately
// does not set DisallowUnknownFields.

// ckStaticCheckedAnswer is the generated static return type for the check fixture.
type ckStaticCheckedAnswer struct {
	Answer     string                   `json:"answer"`
	Confidence bamlutils.Checked[int64] `json:"confidence"`
}

// ckStaticAssertAnswer is the generated static return type for the assert fixture: an
// assert-only field keeps its ordinary Go type, because `as_check()` excludes an assert
// from the CFFI check list and a passing assert therefore produces no wrapper.
type ckStaticAssertAnswer struct {
	Answer     string `json:"answer"`
	Confidence int64  `json:"confidence"`
}

// ckEmitter builds a method emitter around a synthetic sync func with the given
// return type, which is the only input finalResultDecoderName consults.
func ckEmitter(fn any) *methodEmitter {
	return &methodEmitter{syncFuncType: reflect.TypeOf(fn)}
}

// TestCheckedStaticDecoderRoutingIsStrict drives the decoder selection over every form
// the two narrow fixtures produce, plus the neighbours that must NOT move.
func TestCheckedStaticDecoderRoutingIsStrict(t *testing.T) {
	for _, tc := range []struct {
		name string
		fn   any
		want string
	}{
		// The two fixtures, as whole return types.
		{"nested Checked[int64] carrier", func() (ckStaticCheckedAnswer, error) { return ckStaticCheckedAnswer{}, nil }, "DecodeStaticFinal"},
		{"assert-only int64 field", func() (ckStaticAssertAnswer, error) { return ckStaticAssertAnswer{}, nil }, "DecodeStaticFinal"},
		// The TOP-LEVEL carrier — the case the alias router would otherwise capture,
		// because *Checked[T] implements json.Unmarshaler.
		{"top-level Checked[int64]", func() (bamlutils.Checked[int64], error) { return bamlutils.Checked[int64]{}, nil }, "DecodeStaticFinal"},
		{"top-level *Checked[int64]", func() (*bamlutils.Checked[int64], error) { return nil, nil }, "DecodeStaticFinal"},
		{"top-level Checked[string]", func() (bamlutils.Checked[string], error) { return bamlutils.Checked[string]{}, nil }, "DecodeStaticFinal"},
		// The unconstrained neighbours, unchanged.
		{"bare int64", func() (int64, error) { return 0, nil }, "DecodeStaticFinal"},
		{"bare string", func() (string, error) { return "", nil }, "DecodeStaticFinal"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ckEmitter(tc.fn).finalResultDecoderName(); got != tc.want {
				t.Fatalf("decoder = %s, want %s", got, tc.want)
			}
		})
	}
}

// TestCheckedStaticRoutingIsProvenToBite is the anti-false-green control: without the
// carrier arm, a top-level checked return WOULD be routed to the lenient alias decoder.
//
// It asserts the mechanism directly (the pointer receiver satisfies the probe) rather
// than trusting that the arm is load-bearing, so deleting the arm turns
// TestCheckedStaticDecoderRoutingIsStrict red rather than leaving it vacuously green.
func TestCheckedStaticRoutingIsProvenToBite(t *testing.T) {
	carrier := reflect.TypeOf(bamlutils.Checked[int64]{})
	if !reflect.PointerTo(carrier).Implements(jsonUnmarshalerType) {
		t.Fatal("*bamlutils.Checked[int64] no longer implements json.Unmarshaler, so the alias router " +
			"could not have mis-routed it and this guard describes a hazard that no longer exists")
	}
	if !isCheckedConstraintCarrier(carrier) {
		t.Fatal("the carrier fingerprint does not recognise bamlutils.Checked[int64]; the strict arm is dead")
	}
	// The fingerprint is STRUCTURAL, so stock's own carrier — what an untransformed
	// generated client still carries — is recognised too.
	type stockCheck struct {
		Name       string `json:"name"`
		Expression string `json:"expression"`
		Status     string `json:"status"`
	}
	type stockChecked struct {
		Value  int64                 `json:"value"`
		Checks map[string]stockCheck `json:"checks"`
	}
	if !isCheckedConstraintCarrier(reflect.TypeOf(stockChecked{})) {
		t.Fatal("the carrier fingerprint does not recognise stock's identically-shaped Checked[int64]")
	}
}

// TestCheckedStaticCarrierFingerprintIsNarrow drives the fingerprint over one-property
// siblings, so a recognition is attributable to the shape rather than to a loose match.
func TestCheckedStaticCarrierFingerprintIsNarrow(t *testing.T) {
	type check struct {
		Name       string `json:"name"`
		Expression string `json:"expression"`
		Status     string `json:"status"`
	}
	type permutedCheck struct {
		Expression string `json:"expression"`
		Name       string `json:"name"`
		Status     string `json:"status"`
	}
	type extraField struct {
		Value  int64            `json:"value"`
		Checks map[string]check `json:"checks"`
		Extra  int              `json:"extra"`
	}
	type renamedValue struct {
		Val    int64            `json:"value"`
		Checks map[string]check `json:"checks"`
	}
	type retaggedValue struct {
		Value  int64            `json:"v"`
		Checks map[string]check `json:"checks"`
	}
	type listChecks struct {
		Value  int64   `json:"value"`
		Checks []check `json:"checks"`
	}
	type intKeyedChecks struct {
		Value  int64         `json:"value"`
		Checks map[int]check `json:"checks"`
	}
	type permutedCheckFields struct {
		Value  int64                    `json:"value"`
		Checks map[string]permutedCheck `json:"checks"`
	}
	for _, tc := range []struct {
		name string
		t    reflect.Type
	}{
		{"an extra field", reflect.TypeOf(extraField{})},
		{"a renamed value field", reflect.TypeOf(renamedValue{})},
		{"a re-tagged value field", reflect.TypeOf(retaggedValue{})},
		{"checks as a list", reflect.TypeOf(listChecks{})},
		{"checks keyed by int", reflect.TypeOf(intKeyedChecks{})},
		{"the check fields permuted", reflect.TypeOf(permutedCheckFields{})},
		{"the nested fixture (the carrier is a FIELD, not the type)", reflect.TypeOf(ckStaticCheckedAnswer{})},
		{"a plain struct", reflect.TypeOf(ckStaticAssertAnswer{})},
		{"a scalar", reflect.TypeOf(int64(0))},
		{"a map", reflect.TypeOf(map[string]int64{})},
	} {
		if isCheckedConstraintCarrier(tc.t) {
			t.Errorf("the carrier fingerprint ADMITTED %s (%s)", tc.name, tc.t)
		}
	}
	// CONTROL: the real carrier is still recognised, so the rejections above are about
	// the mutations rather than about a fingerprint that matches nothing.
	if !isCheckedConstraintCarrier(reflect.TypeOf(bamlutils.Checked[int64]{})) {
		t.Fatal("the control carrier is not recognised; every rejection above is vacuous")
	}
}

// TestCheckedStaticDecodeClosureInstantiatesBothForms pins the EMITTED code: the
// per-method closure names bamlutils.DecodeStaticFinal at the method's concrete return
// type, for both fixtures.
func TestCheckedStaticDecodeClosureInstantiatesBothForms(t *testing.T) {
	g := &generator{pkgs: PackageConfig{InterfacesPkg: "github.com/invakid404/baml-rest/bamlutils"}}
	for _, tc := range []struct {
		name string
		fn   any
		want string
	}{
		{"check fixture", func() (ckStaticCheckedAnswer, error) { return ckStaticCheckedAnswer{}, nil },
			"bamlutils.DecodeStaticFinal[codegen.ckStaticCheckedAnswer](__cj)"},
		{"assert fixture", func() (ckStaticAssertAnswer, error) { return ckStaticAssertAnswer{}, nil },
			"bamlutils.DecodeStaticFinal[codegen.ckStaticAssertAnswer](__cj)"},
		{"top-level carrier", func() (bamlutils.Checked[int64], error) { return bamlutils.Checked[int64]{}, nil },
			"bamlutils.DecodeStaticFinal[bamlutils.Checked[int64]](__cj)"},
		{"top-level int64", func() (int64, error) { return 0, nil },
			"bamlutils.DecodeStaticFinal[int64](__cj)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			me := ckEmitter(tc.fn)
			me.g = g
			me.finalType = parseReflectType(me.syncFuncType.Out(0))
			me.finalResultType = me.finalType.statement
			// A bare func literal is not a formattable statement on its own, so the
			// closure is rendered inside a trivial assignment; only the call inside it
			// is what this test reads.
			rendered := renderCode(jen.Id("_").Op("=").Add(
				me.decodeClosure(me.finalResultDecoderName(), me.finalResultTypeCode())))
			if !strings.Contains(rendered, tc.want) {
				t.Fatalf("emitted closure does not instantiate %s:\n%s", tc.want, rendered)
			}
			if strings.Contains(rendered, "DecodeStaticAlias") {
				t.Fatalf("emitted closure routes through an ALIAS decoder, losing strictness:\n%s", rendered)
			}
		})
	}
}
