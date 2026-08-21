package canary

// De-BAML serving cutover S3a — the SERVE-SEAM proof that identity is PER REQUEST
// and comes from the DEPLOYMENT.
//
// admission owns the resolver's own biting matrix. What this file proves is what the
// resolver alone cannot: that the SERVE SEAM consults it with the facts of the
// request in front of it, rather than presenting a value the server holds — and that
// what it consults is the deployment's seal rather than the caller's bytes.
//
// The two load-bearing assertions are pairs. The SAME server, two requests: one that
// let the deployment configure its client and one that configured it itself with
// identical values — only the first gets an identity. And the same server, two
// requests both sealed but selecting different clients — only the approved one does.
// A worker-wide identity fails the first pair; a byte-matching resolver fails it too.

import (
	"context"
	"net/http"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/bamlutils/trustedclients"
	"github.com/invakid404/baml-rest/nativeserve/admission"
)

const (
	seamApprovedFingerprint = "cfg001"
	seamApprovedClient      = "SeamApproved"
	seamOtherClient         = "SeamOther"
)

func seamStr(s string) *string { return &s }

// seamDeclaration is the deployment's approved-configuration declaration: two
// classes, so the seam proof can show identity following the SELECTED one.
func seamDeclaration(t *testing.T) *trustedclients.Set {
	t.Helper()
	set, err := trustedclients.Parse(`{"trusted_clients":[
		{"name":"` + seamApprovedClient + `","fingerprint":"` + seamApprovedFingerprint + `","provider":"openai",
		 "options":{"model":"gpt-4o-mini","base_url":"https://seam-approved.example/v1","api_key":"sk-seam"}},
		{"name":"` + seamOtherClient + `","fingerprint":"cfg002","provider":"openai",
		 "options":{"model":"gpt-4o-mini","base_url":"https://seam-other.example/v1","api_key":"sk-other"}}
	]}`)
	if err != nil {
		t.Fatalf("trustedclients.Parse: %v", err)
	}
	return set
}

// seamNamedOnly is what a request that merely NAMES a class looks like: a primary and
// a client name, nothing else. This is the only shape a deployment will seal.
func seamNamedOnly(name string) *bamlutils.ClientRegistry {
	return &bamlutils.ClientRegistry{
		Primary: seamStr(name),
		Clients: []*bamlutils.ClientProperty{{Name: name}},
	}
}

// seamSealed is that request after the worker's config-load pass ran.
func seamSealed(t *testing.T, name string) *bamlutils.ClientRegistry {
	t.Helper()
	reg := seamNamedOnly(name)
	seamDeclaration(t).Seal(reg)
	if _, _, sealed := reg.Clients[0].TrustedConfigSeal(); !sealed {
		t.Fatalf("the declaration did not seal a request naming %q", name)
	}
	return reg
}

// seamCallerSupplied is the ATTACK SHAPE: the caller describes the approved
// configuration itself, byte for byte, instead of naming it. The worker's sealing
// pass leaves it exactly as it arrived and seals nothing.
func seamCallerSupplied(t *testing.T) *bamlutils.ClientRegistry {
	t.Helper()
	reg := &bamlutils.ClientRegistry{
		Primary: seamStr(seamApprovedClient),
		Clients: []*bamlutils.ClientProperty{{
			Name:     seamApprovedClient,
			Provider: "openai",
			Options: map[string]any{
				"model":    "gpt-4o-mini",
				"base_url": "https://seam-approved.example/v1",
				"api_key":  "sk-seam",
			},
		}},
	}
	seamDeclaration(t).Seal(reg)
	return reg
}

// seamRequest is a fully-formed dynamic unary serve request over reg: the shape a
// generated `/call` produces, including BAML's no-send plan builder and a non-nil
// output schema (the lane declines a nil schema before admission).
func seamRequest(reg *bamlutils.ClientRegistry) bamlutils.NativeServeRequest {
	name := ""
	if reg != nil && reg.Primary != nil {
		name = *reg.Primary
	}
	return bamlutils.NativeServeRequest{
		Registry:       reg,
		Provider:       "openai",
		ClientOverride: name,
		Mode:           bamlutils.NativeServeModeCall,
		SingleLeaf:     true,
		OutputSchema:   proofSchema(),
		BuildBAMLRequest: func(context.Context) (*llmhttp.Request, error) {
			return nil, http.ErrServerClosed
		},
	}
}

// TestServeSeamIdentityComesFromTheDeploymentNotTheCaller is the P1 proof at the
// seam: one server, two requests whose EFFECTIVE configurations are identical, and
// only the one the deployment configured carries an identity.
func TestServeSeamIdentityComesFromTheDeploymentNotTheCaller(t *testing.T) {
	s := NewServer(nil, nil)

	sealed := s.serveCohortInput(seamRequest(seamSealed(t, seamApprovedClient)))
	if sealed.Fingerprint != seamApprovedFingerprint {
		t.Fatalf("the deployment-sealed configuration presented %q, want %q", sealed.Fingerprint, seamApprovedFingerprint)
	}
	if sealed.Provider != admission.ConfigProviderOpenAI {
		t.Errorf("the sealed identity presented provider class %q, want openai", sealed.Provider)
	}
	supplied := s.serveCohortInput(seamRequest(seamCallerSupplied(t)))
	if supplied.Fingerprint != "" {
		t.Errorf("a caller-supplied configuration presented %q; the registry is the CALLER's document and can never be an identity", supplied.Fingerprint)
	}
	if sealed.Fingerprint == supplied.Fingerprint {
		t.Error("both requests presented the same identity; the seam is not distinguishing provenance")
	}
}

// TestServeSeamIdentityFollowsTheSelectedConfiguration is the anti-worker-wide proof:
// one server, two SEALED requests selecting different approved classes, two different
// identities. A worker-wide implementation answers both the same.
func TestServeSeamIdentityFollowsTheSelectedConfiguration(t *testing.T) {
	s := NewServer(nil, nil)
	approved := s.serveCohortInput(seamRequest(seamSealed(t, seamApprovedClient)))
	other := s.serveCohortInput(seamRequest(seamSealed(t, seamOtherClient)))
	if approved.Fingerprint != seamApprovedFingerprint {
		t.Fatalf("the approved class presented %q", approved.Fingerprint)
	}
	if other.Fingerprint != "cfg002" {
		t.Fatalf("the second class presented %q, want cfg002", other.Fingerprint)
	}
	if approved.Fingerprint == other.Fingerprint {
		t.Error("two different sealed configurations presented one identity; the seam is answering per WORKER, not per request")
	}
}

// TestServeSeamIdentityFollowsEveryTrustedFact walks the facts the seam threads and
// requires each one, alone, to remove the identity — proving the seam PASSES each
// fact through rather than resolving on the seal alone.
func TestServeSeamIdentityFollowsEveryTrustedFact(t *testing.T) {
	s := NewServer(nil, nil)
	if got := s.serveCohortInput(seamRequest(seamSealed(t, seamApprovedClient))).Fingerprint; got != seamApprovedFingerprint {
		t.Fatalf("the approved control resolved %q; the mutations below would be vacuous", got)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*bamlutils.NativeServeRequest)
	}{
		{"a fallback chain", func(r *bamlutils.NativeServeRequest) { r.HasFallbackChain = true }},
		{"round robin", func(r *bamlutils.NativeServeRequest) { r.HasRoundRobin = true }},
		{"a request retry override", func(r *bamlutils.NativeServeRequest) { r.HasRequestRetryOverride = true }},
		{"more than one resolved leaf", func(r *bamlutils.NativeServeRequest) { r.SingleLeaf = false }},
		{"a different selected leaf", func(r *bamlutils.NativeServeRequest) { r.ClientOverride = "SomethingElse" }},
		{"no selected leaf", func(r *bamlutils.NativeServeRequest) { r.ClientOverride = "" }},
		{"a different resolved provider", func(r *bamlutils.NativeServeRequest) { r.Provider = "anthropic" }},
		{"the legacy probe route (no BAML plan builder)", func(r *bamlutils.NativeServeRequest) { r.BuildBAMLRequest = nil }},
		{"no registry", func(r *bamlutils.NativeServeRequest) { r.Registry = nil }},
		{"a post-seal mutation of the sealed client", func(r *bamlutils.NativeServeRequest) {
			r.Registry.Clients[0].Options["base_url"] = "https://elsewhere.example/v1"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := seamRequest(seamSealed(t, seamApprovedClient))
			tc.mutate(&req)
			if got := s.serveCohortInput(req).Fingerprint; got != "" {
				t.Errorf("resolved identity %q; %s must remove the identity", got, tc.name)
			}
		})
	}
}

// TestUndeclaredDeploymentResolvesNoIdentity pins the SHIPPED default: with nothing
// declared, the very same request — which the worker's sealing pass leaves untouched
// — resolves nothing.
func TestUndeclaredDeploymentResolvesNoIdentity(t *testing.T) {
	empty, err := trustedclients.Parse("")
	if err != nil {
		t.Fatalf("trustedclients.Parse: %v", err)
	}
	reg := seamNamedOnly(seamApprovedClient)
	empty.Seal(reg)
	s := NewServer(nil, nil)
	if got := s.serveCohortInput(seamRequest(reg)).Fingerprint; got != "" {
		t.Errorf("an undeclared deployment presented fingerprint %q", got)
	}
}

// TestSealedIdentityStillDeclinesPreSocket is the S3a serving guarantee at the serve
// boundary: a request that DOES resolve a sealed identity is still refused by the
// empty policy, before any socket, and the decline is attributed to the RESOLVED
// cohort bucket rather than to `none` — which is what makes "the resolver is live"
// observable without enrolling anything.
func TestSealedIdentityStillDeclinesPreSocket(t *testing.T) {
	ct := &dialCountingTransport{}
	reg := prometheus.NewRegistry()
	m, err := admission.NewMetrics(reg)
	if err != nil {
		t.Fatalf("admission.NewMetrics: %v", err)
	}
	s := NewServer(m, llmhttp.NewExactExecutor(ct))

	res := s.Serve(context.Background(), seamRequest(seamSealed(t, seamApprovedClient)))
	if res.Disposition != bamlutils.NativeServeDeclined {
		t.Fatalf("a sealed identity produced disposition %v under the EMPTY policy; S3a must serve nothing natively", res.Disposition)
	}
	if res.Stage != string(admission.StageCohort) || res.Reason != string(admission.ReasonCohortNotEnrolled) {
		t.Errorf("declined with (%s,%s), want the bounded cohort refusal", res.Stage, res.Reason)
	}
	if got := ct.n.Load(); got != 0 {
		t.Errorf("a pre-claim decline opened %d socket(s)", got)
	}
	// The identity DID resolve: the fingerprint is sealed but not inventoried, so the
	// gate folds it onto `unrecognized`. `none` here would mean the seam presented no
	// identity at all — i.e. the wiring is not live.
	if got := counterValue(t, reg, "baml_rest_debaml_admission_phase_total", map[string]string{
		"surface": "dynamic_call", "cohort": "unrecognized", "phase": "preclaim_decline",
	}); got != 1 {
		t.Errorf("preclaim_decline{cohort=unrecognized} = %v, want 1 — the resolved identity did not reach the recorded cohort", got)
	}
	if got := counterValue(t, reg, "baml_rest_debaml_admission_phase_total", map[string]string{
		"surface": "dynamic_call", "phase": "claimed",
	}); got > 0 {
		t.Errorf("the empty policy produced %v claim(s); S3a must produce zero", got)
	}
	if got := counterValue(t, reg, "baml_rest_debaml_winner_total", map[string]string{
		"surface": "dynamic_call", "winner": "native",
	}); got > 0 {
		t.Errorf("the empty policy produced %v native winner(s); S3a must produce zero", got)
	}
}
