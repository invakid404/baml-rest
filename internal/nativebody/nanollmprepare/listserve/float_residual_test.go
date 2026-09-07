//go:build integration && nanollm_integration

package listserve

// The MEASURED residual that keeps `float[]` out of this slice's admitted element
// set, recorded as evidence rather than prose.
//
// Stock BAML v0.223 renders a directly interpolated value two different ways
// depending on where it sits:
//
//	{{ r }}   a SCALAR float goes through Rust's `Display for f64` — always
//	          positional, never exponent notation;
//	{{ rs }}  a LIST goes through `debug_list`, which formats each element with
//	          `Debug for f64` — which switches to exponent notation once the
//	          magnitude is large or small enough.
//
// The native renderer (internal/bamlprofile's listObject, over minijinja's
// formatFloat) uses the POSITIONAL form for both, so it reproduces the scalar
// exactly and diverges on a list element at those magnitudes. The divergence is
// safe — the pre-claim plan compare declines it before any socket, and BAML serves —
// but it means `float[]` is a type whose values are KNOWN to decline, so it is
// excluded from the cohort rather than admitted-and-mostly-declining.
//
// This test is the reopening condition. It FAILS the moment stock and native agree,
// which is exactly when `float[]` can join cohortListElementKind in
// nativeserve/spine/executor.go.

import (
	"context"
	"math"
	"strings"
	"testing"

	baml "github.com/boundaryml/baml/engine/language_client_go/pkg"

	"github.com/invakid404/baml-rest/internal/nativespine"
)

// floatProbeSources is a one-method corpus that renders the SAME float twice — once
// as a scalar and once as the sole element of a list — so the two rendering paths
// are observed in ONE request and cannot be confused with a difference in client,
// prompt, or argument encoding.
func floatProbeSources(baseURL string) map[string]string {
	sources := nativespine.ListArgsFixtureSourcesAt(baseURL)
	sources["functions.baml"] = `function ` + nativespine.ListArgsFixtureMethod + `(r: float, rs: float[]) -> JSON {
  client ListOracle
  prompt #"scalar {{ r }} list {{ rs }}"#
}
`
	return sources
}

// TestFloatListRenderResidualIsReal measures the divergence and pins the threshold
// it appears at.
func TestFloatListRenderResidualIsReal(t *testing.T) {
	requireStockBAML(t)
	server := newJSONServer(t, `[1]`)
	env := envSnapshot()
	rt, err := baml.CreateRuntime("./baml_src", floatProbeSources(server.baseURL()), env)
	if err != nil {
		t.Fatalf("stock BAML could not compile the float probe corpus: %v", err)
	}

	render := func(v float64) (scalar, list string) {
		t.Helper()
		args := baml.BamlFunctionArguments{
			Kwargs: map[string]any{"r": v, "rs": []float64{v}, "stream": false},
			Env:    env,
		}
		encoded, err := args.Encode()
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		req, err := rt.BuildRequest(context.Background(), nativespine.ListArgsFixtureMethod, encoded)
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

	// AGREEING magnitudes: the two paths render identically, which is why an
	// ordinary float list would otherwise look fine and the exclusion would look
	// arbitrary. Negative zero is here deliberately — it is NOT the residual.
	for _, v := range []float64{0, math.Copysign(0, -1), 2, 0.1, -1.25, 1e15, 1e-4} {
		scalar, list := render(v)
		if scalar != list {
			t.Errorf("float %v: stock renders the SCALAR as %q and the LIST ELEMENT as %q; "+
				"this magnitude was expected to agree", v, scalar, list)
		}
		if strings.ContainsAny(list, "eE") {
			t.Errorf("float %v: the list element %q is already in exponent form at a magnitude the threshold below claims is positional", v, list)
		}
	}

	// DIVERGING magnitudes: the list element switches to exponent form while the
	// scalar stays positional. These are the values the native renderer cannot
	// reproduce today.
	for _, v := range []float64{1e16, 1e21, 1e-5, 5e-324, math.MaxFloat64, -math.MaxFloat64} {
		scalar, list := render(v)
		if scalar == list {
			t.Errorf("float %v: stock now renders the scalar and the list element identically (%q). "+
				"The float[] residual is CLOSED — admit ValueFloat in cohortListElementKind "+
				"(nativeserve/spine/executor.go) and add float[] back to the cohort.", v, scalar)
			continue
		}
		if !strings.ContainsAny(list, "eE") {
			t.Errorf("float %v: the list element %q is not in exponent form; the residual's shape has changed and the exclusion needs re-deriving", v, list)
		}
		if strings.ContainsAny(scalar, "eE") {
			t.Errorf("float %v: the SCALAR %q is in exponent form; the scalar float cohort's own parity claim needs re-deriving", v, scalar)
		}
	}
}

// between returns the substring of s between the first occurrence of open and the
// next occurrence of close after it.
func between(t *testing.T, s, open, close string) string {
	t.Helper()
	i := strings.Index(s, open)
	if i < 0 {
		t.Fatalf("marker %q not found in the rendered body", open)
	}
	rest := s[i+len(open):]
	j := strings.Index(rest, close)
	if j < 0 {
		t.Fatalf("closing marker %q not found after %q", close, open)
	}
	return rest[:j]
}
