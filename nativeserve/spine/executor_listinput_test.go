package spine_test

import (
	"strings"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/projectdescriptor"
	"github.com/invakid404/baml-rest/internal/nativespinejsonfixture"
	"github.com/invakid404/baml-rest/nativeserve/spine"
)

// The scalar-LIST input widening's registry proof.
//
// The slice widens the DEFAULT-SERVING population by exactly one input predicate:
// an argument may now be a required single-level list of required primitives
// (string[]/int[]/bool[]) in addition to a required primitive. The return family is
// UNCHANGED — the exact five-arm `JSON` alias — so every row below returns it, and a
// row whose return changes belongs in TestRegistrationDeclineMatrix.
//
// `float[]` is deliberately NOT in the element set even though `float` remains an
// admitted SCALAR: stock v0.223 renders a float list element through Rust's
// `Debug for f64` (exponent form from ~1e16 and ~1e-5) while the native list
// renderer is positional. That residual is MEASURED against stock BAML in
// internal/nativebody/nanollmprepare/listserve (TestFloatListRenderResidualIsReal),
// and the `float_list_input` row in TestRegistrationDeclineMatrix is its fence here.
//
// It is asserted through all FOUR construction paths that share the one classifier,
// because the shared classifier widens every consumer at once and a test that covered
// only the strict constructor would not show that:
//
//	NewUnaryExecutor          strict explicit-binding registration
//	NewPopulationExecutor     the STANDARD worker's /call composite
//	NewPopulationStreamExecutor  the STANDARD worker's /stream + /stream-with-raw
//	NewWorkerRuntime          the BAML-free NATIVE-ONLY worker
//
// Non-vacuity: every LIST-BEARING row here DECLINED before the widening (the
// pre-change requiredScalarInputs refused ValueList outright). The three
// `*_unchanged` rows are the retained pre-existing cohort and were admitted before
// this change too — they are controls proving the widening disturbs nothing, not
// evidence of it. The fence on the other side is TestRegistrationDeclineMatrix's
// register:input-cohort rows (kind="fence") — nullable list, nullable element, float
// list, the synthetic resolved shapes, list-of-class, list-of-enum — each of which
// ADMITS under a permissive replacement of the predicate. That pair is what makes
// this file discriminating rather than a restatement.

const listJSONType = "type JSON = int | string | bool | JSON[] | map<string, JSON>"

// listCorpus builds a one-method JSON-returning project with the given argument
// list and prompt body.
func listCorpus(args, promptBody string) map[string]string {
	return corpus(listJSONType,
		"function F("+args+") -> JSON {\n  client C\n  prompt #\""+promptBody+"\"#\n}\n")
}

// listInputRows is the admitted cross-product: each of the four list primitives on
// its own, scalar/list mixtures, and the unchanged zero-argument and scalar-only
// shapes that must keep behaving exactly as before.
func listInputRows() []struct {
	name   string
	args   string
	prompt string
} {
	return []struct {
		name   string
		args   string
		prompt string
	}{
		{"string_list", "tags: string[]", "Tags: {{ tags }}"},
		{"int_list", "counts: int[]", "Counts: {{ counts }}"},
		{"bool_list", "flags: bool[]", "Flags: {{ flags }}"},
		{"scalar_and_string_list", "topic: string, tags: string[]", "{{ topic }} / {{ tags }}"},
		{"all_three_lists", "tags: string[], counts: int[], flags: bool[]",
			"{{ tags }} {{ counts }} {{ flags }}"},
		{"full_mixture",
			"topic: string, n: int, r: float, b: bool, tags: string[], counts: int[], flags: bool[]",
			"{{ topic }} {{ n }} {{ r }} {{ b }} {{ tags }} {{ counts }} {{ flags }}"},
		// The pre-existing cohort, retained verbatim: the widening must not disturb it.
		// scalar_float_unchanged is the control that makes the float_list_input decline
		// in TestRegistrationDeclineMatrix about the ELEMENT POSITION rather than about
		// the float type.
		{"scalar_only_unchanged", "topic: string", "{{ topic }}"},
		{"scalar_float_unchanged", "r: float", "{{ r }}"},
		{"zero_args_unchanged", "", "static text only"},
	}
}

// TestScalarListInputsAdmitAcrossEveryConstructor is the registry half of the
// widening: the same corpus admits through the strict executor, both standard
// population constructors, and the native-only worker runtime, and the method is
// stamped ClassStaticStream so /call, /stream and /stream-with-raw widen together.
func TestScalarListInputsAdmitAcrossEveryConstructor(t *testing.T) {
	for _, tc := range listInputRows() {
		t.Run(tc.name, func(t *testing.T) {
			proj := projectFromCorpus(t, listCorpus(tc.args, tc.prompt))

			// The source classifier must ADMIT the method and stamp it stream-capable.
			// A JSON-returning list-input method that only reached ClassStaticUnary
			// would widen /call alone, which is not this slice.
			m := methodByName(t, proj, "F")
			if m.Class != projectdescriptor.ClassStaticStream {
				t.Fatalf("method class = %q, want %q (the JSON return must stamp it stream-capable)", m.Class, projectdescriptor.ClassStaticStream)
			}

			if _, err := newExec(t, proj, jsonAliasBinding("F")); err != nil {
				t.Fatalf("NewUnaryExecutor rejected the scalar-list cohort: %v", err)
			}
			ue, err := spine.NewPopulationExecutor(proj,
				[]spine.UnaryRegistration{{Binding: jsonAliasBinding("F"), BuildMethod: jsonAliasBuildMethod()}}, nil)
			if err != nil {
				t.Fatalf("NewPopulationExecutor (standard /call): %v", err)
			}
			assertServes(t, ue.Methods(), "NewPopulationExecutor")

			se, err := spine.NewPopulationStreamExecutor(proj,
				[]spine.StreamRegistration{{Binding: jsonAliasStreamBinding("F"), BuildMethod: jsonAliasBuildMethod()}}, nil)
			if err != nil {
				t.Fatalf("NewPopulationStreamExecutor (standard /stream + /stream-with-raw): %v", err)
			}
			assertServes(t, se.Methods(), "NewPopulationStreamExecutor")

			rt, err := spine.NewWorkerRuntime(proj,
				[]spine.StreamRegistration{{Binding: jsonAliasStreamBinding("F"), BuildMethod: jsonAliasBuildMethod()}}, nil)
			if err != nil {
				t.Fatalf("NewWorkerRuntime (native-only worker): %v", err)
			}
			if rt == nil {
				t.Fatal("NewWorkerRuntime returned a nil runtime for an admitted list-input population")
			}
		})
	}
}

// TestNativeOnlyBootStillRefusesAnEmptyPopulationAfterWidening pins the asymmetry the
// scope calls out: the native-only worker refuses to boot on an EMPTY accepted
// population while the standard constructor permits one. The widening must not have
// turned an out-of-cohort input into an accepted candidate on either side, so the
// nullable-list near miss is used as the population: the standard constructor yields
// an empty (all-decline) executor and native-only boot FAILS.
func TestNativeOnlyBootStillRefusesAnEmptyPopulationAfterWidening(t *testing.T) {
	proj := projectFromCorpus(t, listCorpus("tags: string[]?", "{{ tags }}"))

	ue, err := spine.NewPopulationExecutor(proj,
		[]spine.UnaryRegistration{{Binding: jsonAliasBinding("F"), BuildMethod: jsonAliasBuildMethod()}}, nil)
	if err != nil {
		t.Fatalf("the standard constructor must PERMIT an empty population, got: %v", err)
	}
	if got := ue.Methods(); len(got) != 0 {
		t.Fatalf("a nullable-list method was accepted into the standard population: %v", got)
	}

	_, err = spine.NewWorkerRuntime(proj,
		[]spine.StreamRegistration{{Binding: jsonAliasStreamBinding("F"), BuildMethod: jsonAliasBuildMethod()}}, nil)
	if err == nil {
		t.Fatal("native-only boot accepted an EMPTY population; it must refuse one")
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Fatalf("native-only boot error = %v, want the empty-population refusal", err)
	}
}

func assertServes(t *testing.T, methods []string, ctor string) {
	t.Helper()
	if len(methods) != 1 || methods[0] != "F" {
		t.Fatalf("%s serves %v, want exactly [F]", ctor, methods)
	}
}

func methodByName(t *testing.T, proj projectdescriptor.Project, name string) projectdescriptor.Method {
	t.Helper()
	for i := range proj.Methods {
		if proj.Methods[i].Name == name {
			return proj.Methods[i]
		}
	}
	var names []string
	for i := range proj.Methods {
		names = append(names, proj.Methods[i].Name)
	}
	t.Fatalf("method %q was not admitted by the source classifier (admitted: %v, diagnostics: %v)", name, names, proj.Diagnostics)
	return projectdescriptor.Method{}
}

// jsonAliasStreamBinding is the emitted JSON-alias STREAM binding, renamed to the
// corpus method. Registration validates the binding's SHAPE (non-nil callbacks) and
// the project's facts; it never invokes the projector, so reusing the exact-cohort
// fixture binding under another name is the established pattern here
// (jsonAliasBinding does the same for the unary half).
func jsonAliasStreamBinding(name string) bamlutils.NativeSpineStreamBinding {
	b := nativespinejsonfixture.StreamBinding()
	b.Unary.Method = name
	return b
}

// jsonAliasBuildMethod is the emitted method builder the population constructors
// require alongside a binding.
func jsonAliasBuildMethod() func(bamlutils.NativeSpineUnaryExecutor) (bamlutils.StreamingMethod, bamlutils.ParseMethod) {
	return nativespinejsonfixture.BuildMethod
}
