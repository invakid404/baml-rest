//go:build nanollm_integration

package admission

// S2 typed engine-boundary coverage (real FFI, ZERO transport): it drives the real
// nanollm.New + Prepare for the trusted providers and asserts OUR post-Prepare
// self-consistency contract (alias/target/provider/request-type/nonstream/retry-zero
// + method/response-format), never a BAML differential. It also pins the two
// deferred contracts this reduced slice depends on:
//
//   - the COHERE tripwire: cohere ChatCompletion must auto-decline PRE-socket (a
//     typed unsupported/invalid-provider Prepare error, or a plan the generic gate
//     rejects) — it must NEVER yield an admissible chat plan (embeddings-only in
//     v0.4.3);
//   - the UNKNOWN-provider known limitation: v0.4.3 has no typed invalid_provider,
//     so an unknown prefix classifies as a planner error (alerts), NOT an ordinary
//     unsupported decline. This is a LOGGED known limitation deferred to the P0
//     epic (#546) / ledger (#583), not a correctness gap — every unknown provider
//     still declines to BAML pre-socket with zero sockets.

import (
	"context"
	"strings"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	nanollm "github.com/viktordanov/nanollm-ffi/go"
)

// prepareTrusted maps + constructs + prepares a trusted provider request with a
// single system message and the mapped body options. It returns the open client
// (caller Closes when non-nil), the prepared plan, the New error, and the Prepare
// error — so a test can assert on the exact boundary that fired.
func prepareTrusted(t *testing.T, provider string, opts map[string]any) (client *nanollm.Client, prep *nanollm.PreparedRequest, target string, newErr, prepErr error) {
	t.Helper()
	reg := mapReg("T", provider, opts)
	m, dec, err := mapClientConfig(context.Background(), mappingInput{registry: reg, alias: trustedAlias, resolvedProvider: provider})
	if err != nil {
		t.Fatalf("%s: mapping planner error: %v", provider, err)
	}
	if dec != nil {
		t.Fatalf("%s: mapping declined (want a mapping): %v", provider, dec)
	}
	if m == nil {
		t.Fatalf("%s: mappedClient nil", provider)
	}
	target = m.target
	c, cerr := newMappedClient(m)
	if cerr != nil {
		return nil, nil, target, cerr, nil
	}
	r := nanollm.ChatRequest{
		Model: m.target,
		Messages: []nanollm.ChatMessage{{
			Role:    "system",
			Content: []canonicalTextBlock{{Type: "text", Text: "You are concise."}},
		}},
	}
	applyBodyOptions(&r, m.body)
	nreq, berr := r.Build(canonicalSonicMarshaler)
	if berr != nil {
		c.Close()
		t.Fatalf("%s: ChatRequest.Build: %v", provider, berr)
	}
	nreq.Model = m.alias
	p, perr := c.Prepare(nreq)
	return c, p, target, nil, perr
}

func TestEngineBoundary_AnthropicPlan(t *testing.T) {
	client, prep, target, newErr, prepErr := prepareTrusted(t, "anthropic", map[string]any{
		"model":   "claude-3-haiku",
		"api_key": "sk-ant-fake",
	})
	if newErr != nil {
		t.Fatalf("nanollm.New(anthropic): %v", newErr)
	}
	defer client.Close()
	if prepErr != nil {
		t.Fatalf("Prepare(anthropic): %v", prepErr)
	}
	assertTrustedPlan(t, prep, "anthropic", target)
}

func TestEngineBoundary_CerebrasPlan(t *testing.T) {
	client, prep, target, newErr, prepErr := prepareTrusted(t, "cerebras", map[string]any{
		"model":   "llama-3.1-8b",
		"api_key": "sk-cb-fake",
	})
	if newErr != nil {
		t.Fatalf("nanollm.New(cerebras): %v", newErr)
	}
	defer client.Close()
	if prepErr != nil {
		t.Fatalf("Prepare(cerebras): %v", prepErr)
	}
	assertTrustedPlan(t, prep, "cerebras", target)
}

func TestEngineBoundary_BedrockSignedPlan(t *testing.T) {
	client, prep, target, newErr, prepErr := prepareTrusted(t, "aws-bedrock", map[string]any{
		"model_id":          "anthropic.claude-v2",
		"region":            "us-east-1",
		"access_key_id":     "AKIAFAKE",
		"secret_access_key": "secretfake",
	})
	if newErr != nil {
		t.Fatalf("nanollm.New(bedrock): %v", newErr)
	}
	defer client.Close()
	if prepErr != nil {
		t.Fatalf("Prepare(bedrock): %v", prepErr)
	}
	assertTrustedPlan(t, prep, "bedrock", target)
	// Bedrock is SIGNED by nanollm: the plan carries a signing window and an expiry,
	// is not already expired, and is admitted while fresh by validateGenericPlan.
	if prep.Meta.SignedAt == nil || prep.Meta.ExpiresAt == nil {
		t.Errorf("bedrock plan is not signed (SignedAt=%v ExpiresAt=%v)", prep.Meta.SignedAt, prep.Meta.ExpiresAt)
	}
	if prep.Expired() {
		t.Error("fresh bedrock plan must not be Expired()")
	}
}

// assertTrustedPlan pins the generic post-Prepare self-consistency contract (§5.2)
// — the same one validateGenericPlan enforces — WITHOUT any BAML comparison.
func assertTrustedPlan(t *testing.T, prep *nanollm.PreparedRequest, wantProvider, wantTarget string) {
	t.Helper()
	if d := validateGenericPlan(prep, trustedAlias, wantTarget, wantProvider); d != nil {
		t.Fatalf("generic self-consistency declined a valid %s plan: %v", wantProvider, d)
	}
	m := prep.Meta
	if m.ModelAlias != trustedAlias || m.TargetModel != wantTarget || m.Provider != wantProvider {
		t.Errorf("meta = (alias=%q target=%q provider=%q), want (%q,%q,%q)", m.ModelAlias, m.TargetModel, m.Provider, trustedAlias, wantTarget, wantProvider)
	}
	if m.RequestType != nanollm.ChatCompletion {
		t.Errorf("request type = %q, want chat_completion", m.RequestType)
	}
	if m.Stream {
		t.Error("plan stream must be false")
	}
	if m.MaxRetries != 0 {
		t.Errorf("plan max retries = %d, want 0", m.MaxRetries)
	}
	if prep.Method != "POST" {
		t.Errorf("method = %q, want POST", prep.Method)
	}
	if prep.ResponseFormat != nanollm.FormatJSON {
		t.Errorf("response format = %q, want json", prep.ResponseFormat)
	}
}

// TestEngineBoundary_CohereTripwire is the load-bearing safety net for the DEFERRED
// cohere provider: cohere ChatCompletion must auto-decline PRE-socket. It flows
// through the SAME common bearer mapper (no cohere-specific mapping) and must never
// yield an admissible chat plan — whatever the mechanism (a typed
// unsupported/invalid-provider New/Prepare error, or a plan the generic gate
// rejects, e.g. an /embed plan). If a future nanollm makes cohere a real chat
// provider, this test flips to admitting it and the tripwire is retired.
func TestEngineBoundary_CohereTripwire(t *testing.T) {
	client, prep, target, newErr, prepErr := prepareTrusted(t, "cohere", map[string]any{
		"model":   "command-r",
		"api_key": "co-fake",
	})
	if client != nil {
		defer client.Close()
	}

	if newErr != nil {
		// nanollm.New rejected cohere chat -> pre-socket decline. classifyEngineError
		// tells us whether it was an ordinary typed unsupported decline (ideal) or a
		// planner error (still a safe decline). Either way cohere does NOT admit.
		t.Logf("cohere nanollm.New declined (disposition=%d): %v", classifyEngineError(newErr), newErr)
		return
	}
	if prepErr != nil {
		t.Logf("cohere Prepare declined (disposition=%d): %v", classifyEngineError(prepErr), prepErr)
		return
	}
	// Prepare SUCCEEDED — the plan MUST NOT be an admissible chat plan. In v0.4.3
	// cohere plans /v2/embed for a chat request (RequestType still reads
	// chat_completion — the §1.3 gap), so the provider-neutral generic gate rejects
	// it as an embedding plan BEFORE any socket. It must NEVER pass.
	d := validateGenericPlan(prep, trustedAlias, target, "cohere")
	if d == nil {
		t.Fatalf("COHERE ADMITTED a chat plan (url=%q request_type=%q) — it must auto-decline pre-socket in v0.4.3 (embeddings-only). A later P0 activates cohere chat.", llmhttp.RedactedURL(prep.URL), prep.Meta.RequestType)
	}
	t.Logf("cohere Prepare produced url=%q; declined pre-socket at %s/%s", llmhttp.RedactedURL(prep.URL), d.Stage, d.Reason)
	if strings.Contains(strings.ToLower(prep.URL), "/embed") && d.Reason != ReasonEmbeddingPlan {
		t.Errorf("cohere /embed plan declined for reason %s, want embedding_plan", d.Reason)
	}
}

// TestEngineBoundary_UnknownProviderKnownLimitation documents the v0.4.3 gap the
// brief flags as a LOGGED known limitation (deferred to #546/#583): there is no
// typed invalid_provider code yet, so an unknown provider prefix classifies as a
// PLANNER error (alerts) rather than an ordinary unsupported decline. It is NOT a
// correctness gap — the request still declines to BAML pre-socket with zero
// sockets; only the metric reads planner_error instead of a clean invalid_provider.
// When P0 lands the typed code, classifyEngineError flips to engineUnsupported and
// this test is updated.
func TestEngineBoundary_UnknownProviderKnownLimitation(t *testing.T) {
	client, _, _, newErr, prepErr := prepareTrusted(t, "nanollmunknownproviderxyz", map[string]any{
		"model":   "whatever",
		"api_key": "k",
	})
	if client != nil {
		defer client.Close()
	}
	// The failure surfaces at New (unknown prefix) or, failing that, at Prepare.
	boundaryErr := newErr
	if boundaryErr == nil {
		boundaryErr = prepErr
	}
	if boundaryErr == nil {
		t.Fatal("an unknown provider must fail at nanollm.New/Prepare (never admit)")
	}
	// KNOWN LIMITATION (deferred to #546/#583): v0.4.3 has no invalid_provider code,
	// so the unknown prefix classifies as a planner error, not engineUnsupported.
	if got := classifyEngineError(boundaryErr); got != enginePlannerError {
		t.Fatalf("unknown-provider disposition = %d, want enginePlannerError in v0.4.3 (known limitation).\n"+
			"If P0 landed a typed invalid_provider code, update this test to expect engineUnsupported: %v", got, boundaryErr)
	}
}
