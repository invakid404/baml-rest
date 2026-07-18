//go:build nanollm_integration

package canary

// Gated (real FFI + one loopback socket) proof that the PUBLIC Serve entrypoint —
// not just mapAttempt — honours the trusted verification bypass: for a
// PolicyTrustedProvider claim, Serve SKIPS the strict-OpenAI S4 precondition
// (BuildBAMLRequest + plan_compare) entirely and serves the native structured
// result. It complements the non-gated mapAttempt trusted tests (which never
// exercise Serve's earlier BuildBAMLRequest guard).
//
// S1 keeps ZERO non-openai admitted: the trusted claim is SYNTHETIC (an openai-wire
// plan marked trusted via admission.AdmitTrustedClaimForTest, injected through the
// test-only Server.admitClaim seam) — no production non-openai mapping is
// activated, and the single socket is a loopback capture server.

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

const trustedFenceAPIKey = "sk-trusted-fence-not-a-real-secret"

// TestServe_TrustedClaimSkipsBuildBAMLRequest proves the Serve-level trusted
// bypass: with a trusted claim, Serve NEVER calls BuildBAMLRequest (wired to
// PANIC) and records ZERO plan_compare, then serves the native structured result
// over exactly one loopback socket with the native winner engine.
func TestServe_TrustedClaimSkipsBuildBAMLRequest(t *testing.T) {
	// Loopback capture server returning an OpenAI-shaped 2xx whose assistant
	// content is the flattened schema JSON.
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
	// The exec sends the claimed plan to the loopback capture server.
	s := NewServer(m, llmhttp.NewExactExecutor(&http.Transport{DisableKeepAlives: true}))

	// Build a SYNTHETIC trusted claim (openai-wire plan, trusted policy) targeting
	// the loopback base, and inject it through the test-only admission seam.
	claim, err := admission.AdmitTrustedClaimForTest(trustedRegistry(cs.URL), "__trusted_serve_alias__")
	if err != nil {
		t.Fatalf("AdmitTrustedClaimForTest: %v", err)
	}
	s.admitClaim = func(context.Context, admission.Input) (*admission.Claim, error) { return claim, nil }

	// BuildBAMLRequest + BAMLOnlyParse PANIC: neither may run for a trusted claim
	// on a clean structured success.
	req := bamlutils.NativeServeRequest{
		Provider:     "cerebras",
		Mode:         bamlutils.NativeServeModeCall,
		OutputSchema: trustedSchema(),
		BuildBAMLRequest: func(context.Context) (*llmhttp.Request, error) {
			panic("BuildBAMLRequest must NOT run for a trusted claim")
		},
		BAMLOnlyParse: func(context.Context, string) ([]byte, error) {
			panic("BAMLOnlyParse must NOT run on a trusted structured success")
		},
	}

	out := s.Serve(context.Background(), req)

	if out.Disposition != bamlutils.NativeServeSucceeded {
		t.Fatalf("disposition = %v (err=%v), want succeeded", out.Disposition, errSummary(out.Err))
	}
	if out.WinnerEngine != bamlutils.NativeServeEngineNative {
		t.Errorf("winner engine = %q, want native", out.WinnerEngine)
	}
	if string(out.FinalJSON) != `{"answer":"ok"}` {
		t.Errorf("final = %q, want the native SAP structured output", bodyDigest(out.FinalJSON))
	}
	// Exactly one native socket (the loopback capture server).
	if got := hits.Load(); got != 1 {
		t.Errorf("capture server saw %d requests, want exactly 1 native socket", got)
	}
	// The trusted bypass records NO plan_compare at all.
	if got := familyTotal(t, reg, "baml_rest_debaml_plan_compare_total"); got != 0 {
		t.Errorf("plan_compare total = %v, want 0 (trusted claim never builds/compares a BAML plan)", got)
	}
	if got := familyTotal(t, reg, "baml_rest_debaml_response_compare_total"); got != 0 {
		t.Errorf("response_compare total = %v, want 0 (trusted claim runs no BAML comparison)", got)
	}
}

func trustedRegistry(base string) *bamlutils.ClientRegistry {
	primary := "TrustedClient"
	return &bamlutils.ClientRegistry{
		Primary: &primary,
		Clients: []*bamlutils.ClientProperty{{
			Name:     "TrustedClient",
			Provider: "openai",
			Options: map[string]any{
				"model":    "gpt-4o-mini",
				"base_url": base + "/v1",
				"api_key":  trustedFenceAPIKey,
			},
		}},
	}
}

func trustedSchema() *bamlutils.DynamicOutputSchema {
	return &bamlutils.DynamicOutputSchema{
		Properties: bamlutils.MustOrderedMap(
			bamlutils.OrderedKV("answer", &bamlutils.DynamicProperty{Type: "string"}),
		),
	}
}
