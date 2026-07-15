//go:build integration && nanollm_integration

package dynamic

// De-BAML cutover Slice 5 END-TO-END same-response shadow proof, through the REAL
// generated dynamic call seam (dynclient + patched BAML + the nanollm-backed
// shadow comparator). It EXTENDS the Slice-4 shadow_serve harness (shadowSpy /
// runShadowDynamicCall / liveCaptureServer) to the RESPONSE side:
//
//   - FLAG ON: for an admitted `_dynamic` /call whose request-plan comparison
//     MATCHES, BAML serves the request with EXACTLY ONE provider request, and the
//     orchestrator then hands BAML's already-fetched status+body to the
//     comparator's same-response oracle. The oracle runs native TranslateResponse
//     + SAP and the INDEPENDENT BAML-only parse over that SAME body and records a
//     response_compare series per facet — all `match`, zero `mismatch`. Native
//     opens ZERO sockets and issues ZERO second provider requests; BAML's envelope
//     is served byte-identically.
//   - FLAG OFF: no callback is installed, so the oracle never runs — ZERO
//     response_compare series — and BAML serves normally.
//
// Native NEVER RoundTrips on either path (asserted via the comparator's own exact
// executor counting transport AND the single loopback provider request). Gated
// `integration && nanollm_integration` (BAML CFFI + nanollm).

import (
	"testing"
)

// sumResponseCompare sums the response_compare counter across all fields for one
// result label ("match" / "mismatch"), read off the comparator's private registry.
func (s *shadowSpy) sumResponseCompare(t *testing.T, result string) float64 {
	t.Helper()
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
			for _, lp := range mm.GetLabel() {
				if lp.GetName() == "result" && lp.GetValue() == result {
					sum += mm.GetCounter().GetValue()
				}
			}
		}
	}
	return sum
}

// TestSameResponseShadow_FlagOnComparesResponseAndServesBAML proves the flag-on
// same-response path: after a plan-match BAML send, the oracle records an
// all-match response_compare across every facet, opens zero sockets, and BAML
// serves the request with exactly one provider request.
func TestSameResponseShadow_FlagOnComparesResponseAndServesBAML(t *testing.T) {
	spy := newShadowSpy(t)
	server, res := runShadowDynamicCall(t, spy, true)

	// Exactly one provider request — BAML's send. Native never sent (not for the
	// plan comparison, and not for the same-response oracle, which consumes BAML's
	// already-fetched body with no transport).
	if got := server.count(); got != 1 {
		t.Fatalf("provider saw %d requests, want exactly 1 (BAML serves; native never sends)", got)
	}
	if got := spy.calls.Load(); got != 1 {
		t.Fatalf("shadow comparator invoked %d times, want exactly 1", got)
	}
	if got := spy.exec.n.Load(); got != 0 {
		t.Fatalf("shadow comparator opened %d socket(s); the no-send comparator must open zero", got)
	}

	// The plan matched (5 facets), so the response oracle ran and recorded one
	// series per response facet, all match, zero mismatch (zero-tolerance).
	if got := spy.sumResponseCompare(t, "mismatch"); got != 0 {
		t.Errorf("response_compare mismatches = %v, want 0 (zero-tolerance)", got)
	}
	// translate + assistant + structured + order + raw + reasoning + error = 7.
	if got := spy.sumResponseCompare(t, "match"); got != 7 {
		t.Errorf("response_compare matches = %v, want 7 (translate/assistant/structured/order/raw/reasoning/error)", got)
	}

	// The user-facing structured output is BAML's, byte-identical to the served
	// answer — the oracle never changed what was served.
	if !liveJSONSemEqual(t, res.Data, []byte(`{"answer":"ok"}`)) {
		t.Errorf("served structured output = %s, want {\"answer\":\"ok\"}", liveBodyDigest(res.Data))
	}
}

// TestSameResponseShadow_FlagOffNoResponseCompare proves the kill switch reaches
// the response side too: with the umbrella flag off no callback is installed, so
// the same-response oracle never runs — zero response_compare series — and BAML
// serves normally with one provider request.
func TestSameResponseShadow_FlagOffNoResponseCompare(t *testing.T) {
	spy := newShadowSpy(t)
	server, res := runShadowDynamicCall(t, spy, false)

	if got := server.count(); got != 1 {
		t.Fatalf("provider saw %d requests, want exactly 1 (BAML serves)", got)
	}
	if got := spy.calls.Load(); got != 0 {
		t.Fatalf("flag-off must NOT invoke the shadow comparator, got %d calls", got)
	}
	if got := spy.exec.n.Load(); got != 0 {
		t.Fatalf("flag-off opened %d native socket(s), want 0", got)
	}
	if got := spy.sumResponseCompare(t, "match") + spy.sumResponseCompare(t, "mismatch"); got != 0 {
		t.Errorf("flag-off recorded %v response_compare series, want 0 (zero native)", got)
	}
	if !liveJSONSemEqual(t, res.Data, []byte(`{"answer":"ok"}`)) {
		t.Errorf("served structured output = %s, want {\"answer\":\"ok\"}", liveBodyDigest(res.Data))
	}
}
