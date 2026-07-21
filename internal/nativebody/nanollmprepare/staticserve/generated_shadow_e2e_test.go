//go:build integration && nanollm_integration

package staticserve

// De-BAML Slice 8C Stage-1 static SHADOW generated-route proof (review P1.1). It
// drives the REAL generated static-serve fixture adapter with the PUBLIC shadow
// comparator nativeserve.NewStaticShadow installed (and NO serve callback), over a
// loopback capture server, proving the required stage-1 experiment:
//
//   - BAML is the SOLE provider sender (exactly one provider hit).
//   - native consumes BAML's EXACT captured response bytes with ZERO native sends
//     (native_sockets{flag=on} == 0) and compares its translate/extract/static-SAP/
//     typed-decode against BAML's parse of the SAME bytes.
//   - every compared facet MATCHES (error/translate/assistant/raw/reasoning/structured/
//     order/typed), which is the MEASURED shadow-parse the cutover manifest consumes.

import (
	"context"
	"net/http"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/nativeserve"

	fixture "github.com/invakid404/baml-rest/internal/nativeprompt/testdata/staticserve_fixture/generated"
	fwadapter "github.com/invakid404/baml-rest/internal/nativeprompt/testdata/staticserve_fixture/generated/adapter"
)

// staticShadowSpy wraps the PUBLIC nativeserve.NewStaticShadow comparator, counting
// invocations and exposing the metrics registry so the test reads native_sockets
// (must be 0) and response_compare facets.
type staticShadowSpy struct {
	fn    bamlutils.NativeStaticShadowFunc
	calls int64
	reg   *prometheus.Registry
}

func (s *staticShadowSpy) Shadow(ctx context.Context, inv bamlutils.NativeStaticInvocation) bamlutils.NativeStaticShadowResult {
	s.calls++
	return s.fn(ctx, inv)
}

func newStaticShadowSpy(t *testing.T) *staticShadowSpy {
	t.Helper()
	reg := prometheus.NewRegistry()
	fn, err := nativeserve.NewStaticShadow(reg)
	if err != nil {
		t.Fatalf("nativeserve.NewStaticShadow: %v", err)
	}
	if fn == nil {
		t.Fatal("nativeserve.NewStaticShadow returned a nil comparator")
	}
	return &staticShadowSpy{fn: fn, reg: reg}
}

// counter reads a summed value of the named counter for a {label=value} pair.
func (s *staticShadowSpy) counter(t *testing.T, name, label, value string) float64 {
	t.Helper()
	fams, err := s.reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var sum float64
	for _, mf := range fams {
		if mf.GetName() != name {
			continue
		}
		for _, mm := range mf.GetMetric() {
			for _, lp := range mm.GetLabel() {
				if lp.GetName() == label && lp.GetValue() == value {
					sum += mm.GetCounter().GetValue()
				}
			}
		}
	}
	return sum
}

// responseCompareMatch reads response_compare{result=match, field=<field>}.
func (s *staticShadowSpy) responseCompareMatch(t *testing.T, field string) float64 {
	t.Helper()
	// Filter to BOTH labels: gather the metric family and sum the counters whose
	// field==field AND result==match.
	fams, err := s.reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var sum float64
	for _, mf := range fams {
		if mf.GetName() != "baml_rest_debaml_response_compare_total" {
			continue
		}
		for _, mm := range mf.GetMetric() {
			var okField, okMatch bool
			for _, lp := range mm.GetLabel() {
				if lp.GetName() == "field" && lp.GetValue() == field {
					okField = true
				}
				if lp.GetName() == "result" && lp.GetValue() == "match" {
					okMatch = true
				}
			}
			if okField && okMatch {
				sum += mm.GetCounter().GetValue()
			}
		}
	}
	return sum
}

// buildFixtureShadowAdapter constructs + configures the generated fixture adapter with
// ONLY the SHADOW comparator installed (no serve callback), so the /call seam resolves
// shadow. The loopback-allowing HTTP client carries BAML's single provider send.
func buildFixtureShadowAdapter(t *testing.T, spy *staticShadowSpy, flagOn bool) bamlutils.Adapter {
	t.Helper()
	fixtureInitRuntime()
	a := fixture.MakeAdapter(context.Background())
	ba, ok := a.(*fwadapter.BamlAdapter)
	if !ok {
		t.Fatalf("MakeAdapter returned %T, want *adapter.BamlAdapter", a)
	}
	ba.SetStreamMode(bamlutils.StreamModeCall)
	ba.SetDeBAMLConfig(bamlutils.DeBAMLConfig{Enabled: flagOn})
	ba.SetNativeStaticShadowComparator(spy.Shadow)
	ba.SetHTTPClient(llmhttp.NewClient(&http.Client{Transport: &http.Transport{Proxy: nil}}))
	return a
}

// TestGeneratedStaticShadow_BAMLSoleSenderNativeParsesCapturedBytes proves the Stage-1
// shadow contract through the generated route: BAML is the SOLE sender, native parses
// the EXACT captured bytes with ZERO native sends, and every facet matches.
func TestGeneratedStaticShadow_BAMLSoleSenderNativeParsesCapturedBytes(t *testing.T) {
	spy := newStaticShadowSpy(t)
	server := newFixtureServer(t, http.StatusOK, openAIStaticAnswer("sunny", 9))
	defer server.close()
	a := buildFixtureShadowAdapter(t, spy, true)

	final, winner, planned, err := driveStaticOutputFormat(t, a, "weather")
	if err != nil {
		t.Fatalf("shadow generated StaticOutputFormat /call errored: %v", err)
	}
	// The shadow comparator ran for the generated /call.
	if spy.calls != 1 {
		t.Fatalf("shadow comparator invoked %d times, want exactly 1", spy.calls)
	}
	// BAML is the SOLE sender: exactly one provider request, and it was BAML's.
	if got := server.count.Load(); got != 1 {
		t.Fatalf("provider saw %d requests, want exactly 1 (BAML sole sender)", got)
	}
	// ZERO native sends — the shadow only parses the captured bytes.
	if got := spy.counter(t, "baml_rest_debaml_native_sockets_total", "flag", "on"); got != 0 {
		t.Errorf("native_sockets{flag=on} = %v, want 0 (shadow makes NO native send)", got)
	}
	// The served final is BAML's (winner is not a native serve token; the shadow
	// declined so BAML served). planned_engine=native (native was considered/shadowed).
	if planned != "native" {
		t.Errorf("planned_engine = %q, want native (shadow considered native)", planned)
	}
	if winner == bamlutils.NativeStaticServeEngineNative {
		t.Errorf("winner_engine = %q, but the shadow must NEVER serve a native final", winner)
	}
	if final == nil {
		t.Fatal("BAML final is nil; the shadow must not disturb BAML's served result")
	}
	// The MEASURED shadow-parse: every compared facet matched over the captured bytes.
	for _, field := range []string{"error", "translate", "assistant", "raw", "reasoning", "structured", "order", "typed"} {
		if got := spy.responseCompareMatch(t, field); got != 1 {
			t.Errorf("response_compare{field=%s,result=match} = %v, want 1 (native == BAML on the captured bytes)", field, got)
		}
	}
}
