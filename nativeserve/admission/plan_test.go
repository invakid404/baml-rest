//go:build nanollm_integration

package admission

// Focused, socket-free coverage of the post-Prepare plan validators (the layer-5
// stages) and the StageBody canonical-body gate, plus the request-scoped mapper.
// These craft nanollm.PreparedRequest / llmhttp.ExactAttemptRequest values
// directly so each stage's decline path fires on its own — the full-pipeline
// positive path (TestAdmitPositive) exercises the same validators against a REAL
// nanollm plan.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	nanollm "github.com/viktordanov/nanollm-ffi/go"
)

// validPlan is the shape a real openai Prepare produces for the fence values.
func validPlan() *nanollm.PreparedRequest {
	return &nanollm.PreparedRequest{
		Method: "POST",
		URL:    wantURL,
		Headers: [][2]string{
			{"Content-Type", "application/json"},
			{"Authorization", "Bearer " + fenceAPIKey},
		},
		Body:           []byte(`{"model":"` + fenceModel + `","messages":[]}`),
		ResponseFormat: nanollm.FormatJSON,
		Meta: nanollm.PreparedMeta{
			ModelAlias:  fenceAlias,
			TargetModel: fenceModel,
			Provider:    "openai",
			RequestType: nanollm.ChatCompletion,
			Stream:      false,
		},
	}
}

func wantDecline(t *testing.T, d *Decline, stage Stage, reason Reason) {
	t.Helper()
	if d == nil {
		t.Fatalf("expected a decline (%s, %s), got nil", stage, reason)
	}
	if d.Stage != stage || d.Reason != reason {
		t.Fatalf("decline = (%s, %s), want (%s, %s)", d.Stage, d.Reason, stage, reason)
	}
}

func TestValidateCanonicalBody(t *testing.T) {
	if d := validateCanonicalBody(validPlan().Body, fenceModel); d != nil {
		t.Fatalf("valid canonical body declined: %v", d)
	}
	wantDecline(t, validateCanonicalBody(nil, fenceModel), StageBody, ReasonBodyEmpty)
	wantDecline(t, validateCanonicalBody([]byte("not json"), fenceModel), StageBody, ReasonBodyNotJSON)
	wantDecline(t, validateCanonicalBody([]byte(`{"model":"other"}`), fenceModel), StageBody, ReasonBodyMissingTarget)
}

func TestValidatePreparedBody(t *testing.T) {
	canonical := validPlan().Body
	if d := validatePreparedBody(validPlan(), canonical); d != nil {
		t.Fatalf("valid prepared body declined: %v", d)
	}
	wantDecline(t, validatePreparedBody(&nanollm.PreparedRequest{Body: nil}, canonical), StagePrepare, ReasonBodyEmpty)
	wantDecline(t, validatePreparedBody(&nanollm.PreparedRequest{Body: []byte("nope")}, canonical), StagePrepare, ReasonBodyNotJSON)
	wantDecline(t, validatePreparedBody(&nanollm.PreparedRequest{Body: []byte(`{"model":"x"}`)}, canonical), StagePrepare, ReasonBodyNotByteEqual)
}

func TestValidatePlanMeta(t *testing.T) {
	if d := validatePlanMeta(validPlan(), fenceAlias, fenceModel); d != nil {
		t.Fatalf("valid plan meta declined: %v", d)
	}
	mut := func(f func(p *nanollm.PreparedRequest)) *nanollm.PreparedRequest {
		p := validPlan()
		f(p)
		return p
	}
	wantDecline(t, validatePlanMeta(mut(func(p *nanollm.PreparedRequest) { p.Meta.ModelAlias = "other" }), fenceAlias, fenceModel), StagePlanMeta, ReasonAliasMismatch)
	wantDecline(t, validatePlanMeta(mut(func(p *nanollm.PreparedRequest) { p.Meta.TargetModel = "other" }), fenceAlias, fenceModel), StagePlanMeta, ReasonTargetMismatch)
	wantDecline(t, validatePlanMeta(mut(func(p *nanollm.PreparedRequest) { p.Meta.Provider = "anthropic" }), fenceAlias, fenceModel), StagePlanMeta, ReasonPlanProviderMismatch)
	wantDecline(t, validatePlanMeta(mut(func(p *nanollm.PreparedRequest) { p.Meta.RequestType = nanollm.Completion }), fenceAlias, fenceModel), StagePlanMeta, ReasonRequestTypeMismatch)
	wantDecline(t, validatePlanMeta(mut(func(p *nanollm.PreparedRequest) { p.Meta.Stream = true }), fenceAlias, fenceModel), StagePlanMeta, ReasonPlanStreamTrue)
	wantDecline(t, validatePlanMeta(mut(func(p *nanollm.PreparedRequest) { p.Meta.TransformKey = "jq" }), fenceAlias, fenceModel), StagePlanMeta, ReasonTransformPresent)
	wantDecline(t, validatePlanMeta(mut(func(p *nanollm.PreparedRequest) { p.Meta.MaxRetries = 3 }), fenceAlias, fenceModel), StagePlanMeta, ReasonMaxRetriesNonzero)
	wantDecline(t, validatePlanMeta(mut(func(p *nanollm.PreparedRequest) { p.ResponseFormat = nanollm.ResponseFormat("sse") }), fenceAlias, fenceModel), StagePlanMeta, ReasonResponseFormatNotJSON)
}

func TestValidatePlanExpiry(t *testing.T) {
	if d := validatePlanExpiry(validPlan()); d != nil {
		t.Fatalf("valid unsigned plan declined: %v", d)
	}
	now := time.Now()
	past := now.Add(-time.Hour)
	future := now.Add(time.Hour)

	signed := validPlan()
	signed.Meta.SignedAt = &now
	wantDecline(t, validatePlanExpiry(signed), StagePlanExpiry, ReasonSignedPlan)

	expiring := validPlan()
	expiring.Meta.ExpiresAt = &future
	wantDecline(t, validatePlanExpiry(expiring), StagePlanExpiry, ReasonExpiringPlan)

	// A past ExpiresAt is caught as an expiry before Expired() is consulted; the
	// dedicated expired branch is reached via a plan whose Expired() is true
	// without an ExpiresAt is not constructible, so the ExpiresAt branch is the
	// observable expiry decline (see nanollm.PreparedRequest.Expired()).
	expired := validPlan()
	expired.Meta.ExpiresAt = &past
	if !expired.Expired() {
		t.Fatal("precondition: a past ExpiresAt must report Expired()")
	}
	wantDecline(t, validatePlanExpiry(expired), StagePlanExpiry, ReasonExpiringPlan)
}

func TestValidatePlanHeaders(t *testing.T) {
	if d := validatePlanHeaders(validPlan(), fenceBaseURL); d != nil {
		t.Fatalf("valid plan headers declined: %v", d)
	}
	mut := func(f func(p *nanollm.PreparedRequest)) *nanollm.PreparedRequest {
		p := validPlan()
		f(p)
		return p
	}
	wantDecline(t, validatePlanHeaders(mut(func(p *nanollm.PreparedRequest) { p.Method = "GET" }), fenceBaseURL), StagePlanHeaders, ReasonMethodNotPost)
	wantDecline(t, validatePlanHeaders(mut(func(p *nanollm.PreparedRequest) { p.URL = wantURL + "/extra" }), fenceBaseURL), StagePlanHeaders, ReasonURLMismatch)
	wantDecline(t, validatePlanHeaders(mut(func(p *nanollm.PreparedRequest) {
		p.Headers = append(p.Headers, [2]string{"X-Custom", "1"})
	}), fenceBaseURL), StagePlanHeaders, ReasonUnexpectedHeader)
	wantDecline(t, validatePlanHeaders(mut(func(p *nanollm.PreparedRequest) {
		p.Headers = append(p.Headers, [2]string{"Host", "evil.example"})
	}), fenceBaseURL), StagePlanHeaders, ReasonHostHeader)
	wantDecline(t, validatePlanHeaders(mut(func(p *nanollm.PreparedRequest) {
		p.Headers = append(p.Headers, [2]string{"Content-Type", "application/json"})
	}), fenceBaseURL), StagePlanHeaders, ReasonDuplicateHeader)
	wantDecline(t, validatePlanHeaders(mut(func(p *nanollm.PreparedRequest) {
		p.Headers[0][1] = "text/plain"
	}), fenceBaseURL), StagePlanHeaders, ReasonWrongContentType)
	wantDecline(t, validatePlanHeaders(mut(func(p *nanollm.PreparedRequest) {
		p.Headers = [][2]string{{"Authorization", "Bearer " + fenceAPIKey}}
	}), fenceBaseURL), StagePlanHeaders, ReasonMissingContentType)
	wantDecline(t, validatePlanHeaders(mut(func(p *nanollm.PreparedRequest) {
		p.Headers = [][2]string{{"Content-Type", "application/json"}}
	}), fenceBaseURL), StagePlanHeaders, ReasonMissingAuthorization)
	wantDecline(t, validatePlanHeaders(mut(func(p *nanollm.PreparedRequest) {
		p.Headers = append(p.Headers, [2]string{"Authorization", "Bearer second"})
	}), fenceBaseURL), StagePlanHeaders, ReasonMultipleAuthorization)
	wantDecline(t, validatePlanHeaders(mut(func(p *nanollm.PreparedRequest) {
		p.Headers[1][1] = "Basic abc"
	}), fenceBaseURL), StagePlanHeaders, ReasonMissingAuthorization)
	// A bare "Bearer " with an empty token is not a valid bearer credential.
	wantDecline(t, validatePlanHeaders(mut(func(p *nanollm.PreparedRequest) {
		p.Headers[1][1] = "Bearer "
	}), fenceBaseURL), StagePlanHeaders, ReasonMissingAuthorization)
}

func TestValidateExactTransport(t *testing.T) {
	// The exec preflights through a counting transport; no case may dial it.
	ct := &countingTransport{}
	exec := llmhttp.NewExactExecutor(ct)

	// A clean exact request from the valid plan preflights without a socket.
	ok := exactRequestFromPlan(validPlan())
	if d := validateExactTransport(exec, ok); d != nil {
		t.Fatalf("valid exact request declined: %v", d)
	}

	controlled := &llmhttp.ExactAttemptRequest{
		Method:      "POST",
		URL:         wantURL,
		Headers:     []llmhttp.HeaderField{{Name: "Connection", Value: "close"}},
		Body:        []byte("{}"),
		BodyPresent: true,
	}
	wantDecline(t, validateExactTransport(exec, controlled), StageExactTransport, ReasonTransportControlledHeader)

	dupHost := &llmhttp.ExactAttemptRequest{
		Method:      "POST",
		URL:         wantURL,
		Headers:     []llmhttp.HeaderField{{Name: "Host", Value: "a"}, {Name: "Host", Value: "b"}},
		Body:        []byte("{}"),
		BodyPresent: true,
	}
	wantDecline(t, validateExactTransport(exec, dupHost), StageExactTransport, ReasonDuplicateHost)

	// A control byte in the Authorization value passes the plan_headers bearer
	// gate but the transport would reject it at RoundTrip; the preflight declines
	// it pre-socket, and the value never appears in the decline.
	ctrlVal := &llmhttp.ExactAttemptRequest{
		Method:      "POST",
		URL:         wantURL,
		Headers:     []llmhttp.HeaderField{{Name: "Authorization", Value: "Bearer sk-\x00abc"}},
		Body:        []byte("{}"),
		BodyPresent: true,
	}
	dv := validateExactTransport(exec, ctrlVal)
	wantDecline(t, dv, StageExactTransport, ReasonInvalidHeaderValue)
	if strings.Contains(dv.Detail, "sk-") || strings.Contains(dv.Detail, "\x00") {
		// Never print dv.Detail — it contains the leaked header value.
		t.Fatal("invalid-value decline detail leaked the header value (detail redacted)")
	}

	badName := &llmhttp.ExactAttemptRequest{
		Method:      "POST",
		URL:         wantURL,
		Headers:     []llmhttp.HeaderField{{Name: "Bad Name", Value: "x"}},
		Body:        []byte("{}"),
		BodyPresent: true,
	}
	wantDecline(t, validateExactTransport(exec, badName), StageExactTransport, ReasonInvalidHeaderName)

	if n := ct.n.Load(); n != 0 {
		t.Fatalf("exact-transport preflight opened %d socket(s), want 0", n)
	}
}

// TestMapDynamicClient proves the request-scoped mapper builds a live one-openai
// client engine from the fence trio and safely Closes it, resolving the target
// and base URL while never retaining the api key in the returned facts.
func TestMapDynamicClient(t *testing.T) {
	client, facts, policy, dec, err := mapDynamicClient(context.Background(), validRegistry(), fenceAlias, "openai", nil)
	if err != nil {
		t.Fatalf("mapDynamicClient planner error: %v", err)
	}
	if dec != nil {
		t.Fatalf("mapDynamicClient declined a valid registry: %v", dec)
	}
	if client == nil {
		t.Fatal("mapDynamicClient returned a nil client")
	}
	if policy != PolicyStrictOpenAI {
		t.Errorf("mapDynamicClient policy = %v, want strict_openai", policy)
	}
	// Safe Close, twice — Close is idempotent and must never panic/leak.
	if err := client.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if err := client.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	if facts.provider != "openai" {
		t.Errorf("facts.provider = %q, want openai", facts.provider)
	}
	if facts.target != fenceModel {
		t.Errorf("facts.target = %q, want %q", facts.target, fenceModel)
	}
	if facts.baseURL != fenceBaseURL {
		t.Errorf("facts.baseURL = %q, want %q", llmhttp.RedactedURL(facts.baseURL), llmhttp.RedactedURL(fenceBaseURL))
	}
}
