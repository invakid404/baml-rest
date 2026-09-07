//go:build integration && nanollm_integration

package listserve

// The MEASURED residual that keeps `float[]` out of this slice's admitted element
// set, recorded as evidence rather than prose — and as the REOPENING GUARD that
// trips when the residual is closed.
//
// Stock BAML v0.223 renders a directly interpolated value two different ways
// depending on where it sits:
//
//	{{ r }}   a SCALAR float is rendered by MiniJinja's `Display for Value` float
//	          arm — the shortest round-tripping POSITIONAL decimal, never exponent
//	          notation, with `.0` appended when the result has no fractional part;
//	{{ rs }}  a LIST goes through BAML's `debug_list`, which formats each element
//	          with Rust's `Debug for f64` — which switches to exponent notation
//	          once the magnitude is large or small enough.
//
// The scalar arm is MiniJinja's Value display, NOT Rust's bare `Display for f64`:
// bare Display prints an integral float as `2`, and both legs here print `2.0`. The
// `.0` therefore does not distinguish the two positions and the two paths agree on
// every ordinary magnitude — the ONLY observed divergence is exponent notation, which
// is what the table below encodes and what the assertions turn on.
//
// The native renderer (internal/bamlprofile's debugValue, which delegates a list
// element to the fork's positional Value.Repr/formatFloat) uses the POSITIONAL form
// for both, so it reproduces the scalar exactly and diverges on a list element at
// those magnitudes. The divergence is safe — the pre-claim plan compare declines it
// before any socket, and BAML serves — but it means `float[]` is a type whose values
// are KNOWN to decline, so it is excluded from the cohort rather than
// admitted-and-mostly-declining.
//
// BOTH ENGINES ARE RENDERED HERE. An earlier version of this file compared two
// STOCK substrings (scalar vs list) to each other and never rendered native output
// at all, which made it useless as a reopening guard: fixing native's list-float
// formatting would have left it green. The native leg below runs the SAME
// nativeprompt.RenderStatic call nativeserve/admission makes before a claim, so the
// assertions are native-versus-stock and the guard TRIPS the moment they agree on a
// residual row — which is the signal to re-admit `float[]`.

import (
	"context"
	"math"
	"strings"
	"testing"

	baml "github.com/boundaryml/baml/engine/language_client_go/pkg"

	"github.com/invakid404/baml-rest/bamlutils/promptdescriptor"
	"github.com/invakid404/baml-rest/internal/nativeprompt"
	"github.com/invakid404/baml-rest/internal/nativespine"
)

// floatProbeMethod is the probe's function name (the fixture method, re-signatured).
const floatProbeMethod = nativespine.ListArgsFixtureMethod

// floatProbeSources is a one-method corpus that renders the SAME float twice — once
// as a scalar and once as the sole element of a list — so the two rendering paths
// are observed in ONE request and cannot be confused with a difference in client,
// prompt, or argument encoding.
func floatProbeSources(baseURL string) map[string]string {
	sources := nativespine.ListArgsFixtureSourcesAt(baseURL)
	sources["functions.baml"] = `function ` + floatProbeMethod + `(r: float, rs: float[]) -> JSON {
  client ListOracle
  prompt #"scalar {{ r }} list {{ rs }}"#
}
`
	return sources
}

// floatProbe is one probed magnitude and the EXPECTED native-versus-stock verdict
// for its list element. `agree` rows are the controls that keep the exclusion from
// looking arbitrary; the rest are the residual.
type floatProbe struct {
	name  string
	v     float64
	agree bool
}

func floatProbes() []floatProbe {
	return []floatProbe{
		// Controls: native and stock render the list element identically here, which
		// is why an ordinary float list would otherwise look fine. Negative zero is
		// deliberately a control — it is NOT part of the residual.
		{"zero", 0, true},
		{"negative_zero", math.Copysign(0, -1), true},
		{"whole", 2, true},
		{"tenth", 0.1, true},
		{"negative_fraction", -1.25, true},
		{"1e15", 1e15, true},
		{"1e-4", 1e-4, true},

		// The residual: stock switches the LIST ELEMENT to exponent form, native does
		// not. Rust's debug formatter selects exponent notation for nonzero magnitudes
		// at or above 1e16 and below 1e-4; these are tested witnesses either side of
		// that, not a claim about the exact boundary.
		{"1e16", 1e16, false},
		{"1e21", 1e21, false},
		{"1e-5", 1e-5, false},
		{"smallest_nonzero", 5e-324, false},
		{"max_float", math.MaxFloat64, false},
		{"negative_max_float", -math.MaxFloat64, false},
	}
}

// TestFloatListRenderResidualIsReal measures the divergence between the NATIVE and
// the STOCK renderer and pins the magnitudes it appears at.
func TestFloatListRenderResidualIsReal(t *testing.T) {
	requireStockBAML(t)
	server := newJSONServer(t, `[1]`)
	sources := floatProbeSources(server.baseURL())

	// --- the stock leg ------------------------------------------------------
	env := envSnapshot()
	rt, err := baml.CreateRuntime("./baml_src", sources, env)
	if err != nil {
		t.Fatalf("stock BAML could not compile the float probe corpus: %v", err)
	}

	// --- the native leg -----------------------------------------------------
	// The descriptor comes from the SAME corpus text, and RenderStatic is the exact
	// call nativeserve/admission makes on the pre-claim path — so this is the
	// production renderer, not a re-implementation of it.
	_, descriptors, err := nativespine.ProjectAndDescriptorsFromSource(sources)
	if err != nil {
		t.Fatalf("build descriptors for the float probe corpus: %v", err)
	}
	fn, ok := descriptors[floatProbeMethod]
	if !ok {
		t.Fatalf("no prompt descriptor for %s; the native leg cannot render", floatProbeMethod)
	}

	agreeing, diverging := 0, 0
	for _, p := range floatProbes() {
		t.Run(p.name, func(t *testing.T) {
			stockScalar, stockList := stockRenderFloat(t, rt, env, p.v)
			nativeScalar, nativeList := nativeRenderFloat(t, fn, p.v)

			// (a) SCALAR byte parity, on EVERY row including the residual ones. This
			// is what makes the exclusion about the list ELEMENT POSITION rather than
			// about the float type, and it is the scalar cohort's own parity claim.
			if nativeScalar != stockScalar {
				t.Errorf("SCALAR float %v: native=%q stock=%q — the scalar `float` cohort's parity claim "+
					"is broken, which is a wider problem than this file's residual", p.v, nativeScalar, stockScalar)
			}

			if p.agree {
				// (b) LIST parity on the controls.
				if nativeList != stockList {
					t.Errorf("LIST float %v: native=%q stock=%q — this magnitude was expected to AGREE, so the "+
						"residual's shape has changed and the exclusion boundary needs re-deriving", p.v, nativeList, stockList)
				}
				if strings.ContainsAny(stockList, "eE") {
					t.Errorf("LIST float %v: stock renders %q in exponent form at a magnitude this table calls positional", p.v, stockList)
				}
				return
			}

			// (c) The RESIDUAL. Agreement here is the REOPENING SIGNAL.
			if nativeList == stockList {
				t.Errorf("LIST float %v: native and stock now render the element identically (%q).\n"+
					"THE float[] RESIDUAL IS CLOSED — admit promptdescriptor.ValueFloat in cohortListElementKind "+
					"(nativeserve/spine/executor.go), put float[] back in the fixture corpus and the cross-product "+
					"rows, and delete this row from the residual table.", p.v, nativeList)
				return
			}
			if !strings.ContainsAny(stockList, "eE") {
				t.Errorf("LIST float %v: stock renders %q, not exponent form; the residual's shape has changed "+
					"and the exclusion needs re-deriving", p.v, stockList)
			}
			if strings.ContainsAny(nativeList, "eE") {
				t.Errorf("LIST float %v: native renders %q in exponent form while still differing from stock %q — "+
					"the native renderer has changed but has not reached parity", p.v, nativeList, stockList)
			}
		})
		if p.agree {
			agreeing++
		} else {
			diverging++
		}
	}

	// NON-VACUITY: an emptied or all-one-sided table would make every assertion above
	// unreachable while the test still reported PASS.
	if agreeing == 0 || diverging == 0 {
		t.Fatalf("the probe table must carry BOTH agreeing controls and residual rows; got %d agreeing, %d diverging",
			agreeing, diverging)
	}
}

// stockRenderFloat renders the probe through stock BAML's no-send request builder
// and returns the scalar and list-element substrings.
//
// The substrings are lifted out of the JSON request body. Floats carry no
// JSON-escapable character, so what is extracted is byte-identical to what BAML
// rendered — which is what makes it directly comparable with the native leg's raw
// prompt text.
func stockRenderFloat(t *testing.T, rt baml.BamlRuntime, env map[string]string, v float64) (scalar, list string) {
	t.Helper()
	args := baml.BamlFunctionArguments{
		Kwargs: map[string]any{"r": v, "rs": []float64{v}, "stream": false},
		Env:    env,
	}
	encoded, err := args.Encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	req, err := rt.BuildRequest(context.Background(), floatProbeMethod, encoded)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	body, err := req.Body()
	if err != nil {
		t.Fatalf("Body(): %v", err)
	}
	text, err := body.Text()
	if err != nil {
		t.Fatalf("Body().Text(): %v", err)
	}
	return between(t, text, "scalar ", " list "), between(t, text, " list [", "]")
}

// nativeRenderFloat renders the SAME probe through the PRODUCTION native renderer —
// nativeprompt.RenderStatic over the projected argument vector, the exact call
// nativeserve/admission makes before a claim — and returns the same two substrings.
//
// The vector is built directly rather than through an emitted projector because this
// ad-hoc probe corpus has no emitted package; the shapes are the ones both projectors
// produce for a scalar and a one-element list, and that equivalence is proven
// separately by the composed cross-product differential.
func nativeRenderFloat(t *testing.T, fn promptdescriptor.Function, v float64) (scalar, list string) {
	t.Helper()
	values := []promptdescriptor.ArgumentValue{
		{Name: "r", Value: promptdescriptor.StaticValue{Kind: promptdescriptor.StaticFloat, Float: v}},
		{Name: "rs", Value: promptdescriptor.StaticValue{
			Kind:  promptdescriptor.StaticList,
			Items: []promptdescriptor.StaticValue{{Kind: promptdescriptor.StaticFloat, Float: v}},
		}},
	}
	rendered, err := nativeprompt.RenderStatic(fn, values)
	if err != nil {
		t.Fatalf("the NATIVE renderer declined the float probe: %v.\n"+
			"That is a different failure from the residual this file measures: it means the native leg "+
			"produced nothing to compare, so the comparison below would be vacuous.", err)
	}
	if rendered.Kind != nativeprompt.KindCompletion {
		t.Fatalf("native render kind = %v, want a completion; the probe prompt declares no roles", rendered.Kind)
	}
	text := rendered.Completion
	return between(t, text, "scalar ", " list "), between(t, text, " list [", "]")
}

// between returns the substring of s between the first occurrence of open and the
// next occurrence of close after it.
func between(t *testing.T, s, open, close string) string {
	t.Helper()
	i := strings.Index(s, open)
	if i < 0 {
		t.Fatalf("marker %q not found in the rendered text", open)
	}
	rest := s[i+len(open):]
	j := strings.Index(rest, close)
	if j < 0 {
		t.Fatalf("closing marker %q not found after %q", close, open)
	}
	return rest[:j]
}
