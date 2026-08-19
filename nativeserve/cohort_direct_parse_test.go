package nativeserve

// De-BAML serving cutover S1 — the FIFTH surface's production telemetry proof.
//
// It lives in this package rather than next to the other four surfaces' sweep
// because the direct-parse observer is built by nativeserve.NewDirectParseObserve,
// and canary (where that sweep lives) is imported BY this package — a test there
// importing back would be an import cycle.

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/nativeserve/admission"
)

// counterValue sums a de-BAML counter family's series matching the given labels
// (subset match; unnamed labels are wildcards), or -1 when the family is absent.
func counterValue(t *testing.T, reg *prometheus.Registry, family string, want map[string]string) float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range families {
		if mf.GetName() != family {
			continue
		}
		total := 0.0
		for _, m := range mf.GetMetric() {
			if !labelsMatch(m, want) {
				continue
			}
			total += m.GetCounter().GetValue()
		}
		return total
	}
	return -1
}

func labelsMatch(m *dto.Metric, want map[string]string) bool {
	got := map[string]string{}
	for _, lp := range m.GetLabel() {
		got[lp.GetName()] = lp.GetValue()
	}
	for k, v := range want {
		if got[k] != v {
			return false
		}
	}
	return true
}

// TestDirectParseRouteEmitsRealPerRequestTelemetry drives the FIFTH surface through
// its own production path and asserts a real, nonzero, per-request series — the same
// standard the other four are held to.
//
// `/parse/{method}` never reaches native admission: the worker invokes BAML's
// method.Impl / method.StreamImpl directly, and this module offers no native parse to
// claim. Earlier revisions concluded that meant the surface could only carry
// pre-initialized zeros; two reviews disagreed, and they were right that a pre-created
// zero is not evidence that a BAML-owned request happened. The resolution is the
// observation seam: the worker's parse route reports each request to
// NewDirectParseObserve, which runs the same default-deny cohort gate the other four
// surfaces run and records the resulting pre-claim decline.
//
// So the surface now emits exactly what the others emit — preclaim_decline plus the
// baml_transport winner, once per request — from the observer the production factory
// builds, not from a test double.
func TestDirectParseRouteEmitsRealPerRequestTelemetry(t *testing.T) {
	reg := prometheus.NewRegistry()
	observe, err := NewDirectParseObserve(reg)
	if err != nil {
		t.Fatalf("NewDirectParseObserve: %v", err)
	}
	if observe == nil {
		t.Fatal("NewDirectParseObserve returned a nil observer")
	}

	// Two requests, one of each parse shape, exactly as the worker route reports them.
	observe(context.Background(), bamlutils.NativeDirectParseObservation{})
	observe(context.Background(), bamlutils.NativeDirectParseObservation{Stream: true})

	decline := map[string]string{
		"surface": admission.SurfaceDirectParse.Label(),
		"cohort":  string(admission.CohortNone),
		"phase":   string(admission.PhasePreclaimDecline),
	}
	if got := counterValue(t, reg, "baml_rest_debaml_admission_phase_total", decline); got != 2 {
		t.Errorf("direct_parse preclaim_decline = %v, want 2 (one REAL series per observed request)", got)
	}
	winner := map[string]string{
		"surface": admission.SurfaceDirectParse.Label(),
		"cohort":  string(admission.CohortNone),
		"winner":  string(admission.WinnerBAMLTransport),
	}
	if got := counterValue(t, reg, "baml_rest_debaml_winner_total", winner); got != 2 {
		t.Errorf("direct_parse baml_transport winner = %v, want 2", got)
	}
	// And it never claims: BAML owns the surface outright, and no socket exists to open.
	for _, rollout := range []struct {
		family string
		labels map[string]string
	}{
		{"baml_rest_debaml_admission_phase_total", map[string]string{"surface": "direct_parse", "phase": string(admission.PhaseClaimed)}},
		{"baml_rest_debaml_winner_total", map[string]string{"surface": "direct_parse", "winner": string(admission.WinnerNative)}},
		{"baml_rest_debaml_native_sockets_total", nil},
	} {
		if got := counterValue(t, reg, rollout.family, rollout.labels); got > 0 {
			t.Errorf("direct_parse %s%v = %v, want 0 — the surface has no native path", rollout.family, rollout.labels, got)
		}
	}
}

// TestDirectParseObserverIsAdvisoryOnly pins the seam's other half: the observer
// returns nothing the parse route can act on, so it cannot claim, decline, substitute
// a result or fail a request. The signature is the proof — a func with no return
// value — and this test states it so a future change that gives it one has to argue
// with something.
func TestDirectParseObserverIsAdvisoryOnly(t *testing.T) {
	var fn bamlutils.NativeDirectParseObserveFunc = func(context.Context, bamlutils.NativeDirectParseObservation) {}
	// If NativeDirectParseObserveFunc ever grows a return value, this assignment stops
	// compiling — which is the point.
	_ = fn
	obs := bamlutils.NativeDirectParseObservation{Stream: true}
	if !obs.Stream {
		t.Fatal("the observation lost its only field")
	}
}
