//go:build nanollm_integration

package canary

// De-BAML serving cutover S1 — operational invariant I2, over a real claim and a
// real socket:
//
//	a native CLAIM has exactly ONE native provider attempt and ZERO BAML provider
//	attempts after the claim.
//
// The non-gated cohort_serve_test.go proves the other side of the boundary (every
// surface declines pre-socket with no enrollment, zero sockets). This proves the
// accounting on the side S1 does not enable yet but S3 will: when a claim DOES
// happen, the phase/winner signals count it exactly once and agree with the socket
// counter, so the queries the rollout gates on ("claimed == native_sockets",
// "winner is native, parse-only is zero") are true by construction rather than by
// hope.
//
// The claim is SYNTHETIC (admission.AdmitTrustedClaimForTest, injected through the
// same test-only Server.admitClaim seam the trusted-bypass proof uses): S1 enrolls
// nothing, so there is no production path that produces a claim, and manufacturing
// one in the test is the only way to exercise the post-claim accounting without
// enrolling a cohort.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/nativeserve/admission"
)

// TestClaimedRequestAccountsExactlyOnce is I2's proof.
func TestClaimedRequestAccountsExactlyOnce(t *testing.T) {
	var hits atomic.Int64
	cs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"{\"answer\":\"ok\"}"}}]}`))
	}))
	defer cs.Close()

	reg := prometheus.NewRegistry()
	m, err := admission.NewMetrics(reg)
	if err != nil {
		t.Fatalf("admission.NewMetrics: %v", err)
	}
	s := NewServer(m, llmhttp.NewExactExecutor(&http.Transport{DisableKeepAlives: true}))

	claim, err := admission.AdmitTrustedClaimForTest(trustedRegistry(cs.URL), "__cohort_claim_alias__")
	if err != nil {
		t.Fatalf("AdmitTrustedClaimForTest: %v", err)
	}
	s.admitClaim = func(context.Context, admission.Input) (*admission.Claim, error) { return claim, nil }

	out := s.Serve(context.Background(), bamlutils.NativeServeRequest{
		Provider:     "cerebras",
		Mode:         bamlutils.NativeServeModeCall,
		OutputSchema: trustedSchema(),
		// A post-claim BAML PROVIDER attempt would have to come through one of these
		// two callbacks; both panic, so "zero BAML provider attempts after the claim"
		// is enforced rather than counted.
		BuildBAMLRequest: func(context.Context) (*llmhttp.Request, error) {
			panic("no BAML request may be built for this claim")
		},
		BAMLOnlyParse: func(context.Context, string) ([]byte, error) {
			panic("no BAML parse may run on a clean native structured success")
		},
	})

	if out.Disposition != bamlutils.NativeServeSucceeded {
		t.Fatalf("disposition = %v, want succeeded", out.Disposition)
	}
	// A post-claim DECLINE is what would make the orchestrator resend through BAML.
	// Succeeded (above) is therefore also the no-resend proof.
	if out.WinnerEngine != bamlutils.NativeServeEngineNative {
		t.Fatalf("winner engine = %q, want native", out.WinnerEngine)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("the provider saw %d requests, want exactly 1", got)
	}

	dyn := map[string]string{"surface": admission.SurfaceDynamicCall.Label(), "cohort": string(admission.CohortNone)}
	claimed := map[string]string{"surface": dyn["surface"], "cohort": dyn["cohort"], "phase": string(admission.PhaseClaimed)}
	if got := counterValue(t, reg, "baml_rest_debaml_admission_phase_total", claimed); got != 1 {
		t.Errorf("claimed phase = %v, want exactly 1", got)
	}
	terminal := map[string]string{"surface": dyn["surface"], "cohort": dyn["cohort"], "phase": string(admission.PhasePostclaimTerminal)}
	if got := counterValue(t, reg, "baml_rest_debaml_admission_phase_total", terminal); got != 1 {
		t.Errorf("postclaim_terminal phase = %v, want exactly 1", got)
	}
	declined := map[string]string{"phase": string(admission.PhasePreclaimDecline)}
	if got := counterValue(t, reg, "baml_rest_debaml_admission_phase_total", declined); got != 0 {
		t.Errorf("preclaim_decline phase = %v, want 0 — a claimed request must not also count as declined", got)
	}
	native := map[string]string{"surface": dyn["surface"], "winner": string(admission.WinnerNative)}
	if got := counterValue(t, reg, "baml_rest_debaml_winner_total", native); got != 1 {
		t.Errorf("native winner = %v, want exactly 1", got)
	}
	for _, other := range []admission.Winner{admission.WinnerBAMLTransport, admission.WinnerBAMLParseSameResponse, admission.WinnerFailure} {
		if got := counterValue(t, reg, "baml_rest_debaml_winner_total", map[string]string{"winner": string(other)}); got != 0 {
			t.Errorf("winner %s = %v, want 0 — exactly one winner per request", other, got)
		}
	}
	// claimed == native_sockets is the invariant an operator alerts on; prove the two
	// counters agree for this request rather than merely each being 1 in isolation.
	sockets := counterValue(t, reg, "baml_rest_debaml_native_sockets_total", map[string]string{"flag": string(admission.SocketFlagOn)})
	if sockets != counterValue(t, reg, "baml_rest_debaml_admission_phase_total", claimed) {
		t.Errorf("native_sockets (%v) != claimed phase (%v)", sockets, counterValue(t, reg, "baml_rest_debaml_admission_phase_total", claimed))
	}
	// counterValue returns -1 for a family with no series at all, which is what an
	// untouched fallback family looks like; either that or an explicit 0 is correct
	// here, and anything above 0 is a parse-only win masquerading as a native one.
	if got := counterValue(t, reg, "baml_rest_debaml_fallback_total", nil); got > 0 {
		t.Errorf("parse-only fallback = %v, want none for a native win", got)
	}
}

// TestCohortIdentityConstructorsActuallyEnroll is the control for the seam the gated
// end-to-end proofs depend on: a server built with the proof identity presents it,
// and a request through that server gets PAST the default-deny gate (it declines
// later, at the first predicate the minimal request cannot satisfy). Without this,
// the …WithCohortIdentity constructors could be inert and the e2e proofs would be
// passing for the wrong reason.
//
// It lives behind the tag with the constructors themselves: an untagged build has
// neither, which is the point.
func TestCohortIdentityConstructorsActuallyEnroll(t *testing.T) {
	identity := admission.ProofCohortInputForTest()
	s := NewServerWithCohortIdentity(nil, llmhttp.NewExactExecutor(&dialCountingTransport{}), identity)
	if got := admission.ResolveCohort(admission.SurfaceDynamicCall, s.serveCohortInput(bamlutils.NativeServeRequest{})); got != admission.ProofCohort {
		t.Fatalf("the constructed server presents cohort %q, want %q", got, admission.ProofCohort)
	}
	out := s.Serve(context.Background(), bamlutils.NativeServeRequest{
		Provider: "openai", Mode: bamlutils.NativeServeModeCall, SingleLeaf: true, OutputSchema: proofSchema(),
	})
	if out.Disposition != bamlutils.NativeServeDeclined {
		t.Fatalf("disposition = %v, want Declined (the minimal request has no registry)", out.Disposition)
	}
	if out.Stage == string(admission.StageCohort) {
		t.Fatal("an ENROLLED identity still declined at the cohort gate: the constructor seam is inert")
	}
}
