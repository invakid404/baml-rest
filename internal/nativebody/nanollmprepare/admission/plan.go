package admission

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"

	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/internal/nativebody"
	nanollm "github.com/viktordanov/nanollm-ffi/go"
)

// validatePreparedBody is the StagePrepare body gate: the plan body bytes must
// be non-empty, valid JSON, and BYTE-EQUAL to the admitted canonical body, so a
// mutated/re-serialized plan can never masquerade as the proven request.
func validatePreparedBody(prep *nanollm.PreparedRequest, canonical []byte) *Decline {
	if len(prep.Body) == 0 {
		return declinef(StagePrepare, ReasonBodyEmpty, "prepared plan body is empty")
	}
	if !json.Valid(prep.Body) {
		return declinef(StagePrepare, ReasonBodyNotJSON, "prepared plan body is not valid JSON")
	}
	if !bytes.Equal(prep.Body, canonical) {
		return declinef(StagePrepare, ReasonBodyNotByteEqual, "prepared plan body is not byte-equal to the canonical body")
	}
	return nil
}

// validatePlanMeta is the StagePlanMeta gate: the prepared plan's meta must
// resolve the attempted internal alias, the canonical target, the openai
// provider, a non-streaming ChatCompletion with no jq transform and a zero
// nanollm retry budget, and a JSON response format — exactly the preflight
// intent. Any divergence declines instead of out-claiming BAML.
func validatePlanMeta(prep *nanollm.PreparedRequest, alias, target string) *Decline {
	m := prep.Meta
	switch {
	case m.ModelAlias != alias:
		return declinef(StagePlanMeta, ReasonAliasMismatch, "plan alias differs from the attempted internal alias")
	case m.TargetModel != target:
		return declinef(StagePlanMeta, ReasonTargetMismatch, "plan target model differs from the canonical target")
	case m.Provider != nativebody.ProviderOpenAI:
		return declinef(StagePlanMeta, ReasonPlanProviderMismatch, "plan provider %q is not openai", m.Provider)
	case m.RequestType != nanollm.ChatCompletion:
		return declinef(StagePlanMeta, ReasonRequestTypeMismatch, "plan request type %q is not chat_completion", m.RequestType)
	case m.Stream:
		return declinef(StagePlanMeta, ReasonPlanStreamTrue, "plan stream is true")
	case m.TransformKey != "":
		return declinef(StagePlanMeta, ReasonTransformPresent, "plan carries a jq transform key")
	case m.MaxRetries != 0:
		return declinef(StagePlanMeta, ReasonMaxRetriesNonzero, "plan max retries is not zero")
	case prep.ResponseFormat != nanollm.FormatJSON:
		return declinef(StagePlanMeta, ReasonResponseFormatNotJSON, "plan response format %q is not json", prep.ResponseFormat)
	}
	return nil
}

// validatePlanExpiry is the StagePlanExpiry gate: the admitted OpenAI plan is
// unsigned and never expires. A signed window, an expiry, or an already-expired
// plan declines — re-preparing belongs to the outer planner, never here.
func validatePlanExpiry(prep *nanollm.PreparedRequest) *Decline {
	if prep.Meta.SignedAt != nil {
		return declinef(StagePlanExpiry, ReasonSignedPlan, "plan carries a signing window")
	}
	if prep.Meta.ExpiresAt != nil {
		return declinef(StagePlanExpiry, ReasonExpiringPlan, "plan carries an expiry")
	}
	if prep.Expired() {
		return declinef(StagePlanExpiry, ReasonExpiredPlan, "plan is already expired")
	}
	return nil
}

// validatePlanHeaders is the StagePlanHeaders gate: method POST; URL exactly the
// admitted base + /chat/completions; and headers with only the proved unique
// OpenAI semantics — one Content-Type: application/json and one bearer
// Authorization, nothing else. A custom, duplicate, Host, framing, proxy, or
// connection-controlled field declines. Header VALUES are never surfaced in a
// diagnostic; the Authorization value is checked for the non-empty bearer scheme
// prefix without ever being logged.
func validatePlanHeaders(prep *nanollm.PreparedRequest, baseURL string) *Decline {
	if prep.Method != "POST" {
		return declinef(StagePlanHeaders, ReasonMethodNotPost, "plan method %q is not POST", prep.Method)
	}
	wantURL := baseURL + "/chat/completions"
	if prep.URL != wantURL {
		return declinef(StagePlanHeaders, ReasonURLMismatch, "plan URL is not the admitted base + /chat/completions")
	}

	contentTypes, authorizations := 0, 0
	var authValue string
	for _, h := range prep.Headers {
		switch strings.ToLower(h[0]) {
		case "content-type":
			contentTypes++
			if h[1] != "application/json" {
				return declinef(StagePlanHeaders, ReasonWrongContentType, "plan Content-Type is not application/json")
			}
		case "authorization":
			authorizations++
			authValue = h[1]
		case "host":
			return declinef(StagePlanHeaders, ReasonHostHeader, "plan carries a Host header")
		default:
			// Any header beyond the two proved unique-semantic fields is a custom/
			// duplicate/framing field the admitted plan must not carry. The NAME is
			// safe to surface; the value never is.
			return declinef(StagePlanHeaders, ReasonUnexpectedHeader, "plan carries an unexpected header %q", h[0])
		}
	}
	// The bearer token is the substring after the "Bearer " scheme prefix; it
	// must be non-empty. A bare "Bearer " (empty token) is declined. The token is
	// never surfaced in a diagnostic.
	const bearerPrefix = "Bearer "
	bearerToken := strings.TrimPrefix(authValue, bearerPrefix)
	switch {
	case contentTypes == 0:
		return declinef(StagePlanHeaders, ReasonMissingContentType, "plan has no Content-Type header")
	case contentTypes > 1:
		return declinef(StagePlanHeaders, ReasonDuplicateHeader, "plan has a duplicate Content-Type header")
	case authorizations == 0:
		return declinef(StagePlanHeaders, ReasonMissingAuthorization, "plan has no Authorization header")
	case authorizations > 1:
		return declinef(StagePlanHeaders, ReasonMultipleAuthorization, "plan has more than one Authorization header")
	case !strings.HasPrefix(authValue, bearerPrefix) || bearerToken == "":
		return declinef(StagePlanHeaders, ReasonMissingAuthorization, "plan Authorization is not a non-empty bearer credential")
	}
	return nil
}

// exactRequestFromPlan converts a prepared plan into the neutral exact-attempt
// carrier (ordered headers, raw body). The body is present and non-empty by the
// time this runs. It builds — but never sends — the request.
func exactRequestFromPlan(prep *nanollm.PreparedRequest) *llmhttp.ExactAttemptRequest {
	headers := make([]llmhttp.HeaderField, len(prep.Headers))
	for i, h := range prep.Headers {
		headers[i] = llmhttp.HeaderField{Name: h[0], Value: h[1]}
	}
	return &llmhttp.ExactAttemptRequest{
		Method:      prep.Method,
		URL:         prep.URL,
		Headers:     headers,
		Body:        prep.Body,
		BodyPresent: true,
	}
}

// validateExactTransport is the StageExactTransport gate: the exact lane's full
// header-admissibility scan must succeed BEFORE any RoundTrip. It first rejects
// any header whose NAME is not a valid HTTP token or whose VALUE carries a
// control byte the transport would reject at RoundTrip (e.g. an Authorization
// with an embedded control character) — precise, secret-free declines — then
// runs the executor's Preflight for the transport-controlled/duplicate-Host
// scan. Preflight builds and discards an *http.Request WITHOUT dialing, so this
// opens ZERO sockets; because it runs on the SAME executor a later send would
// use, a stray RoundTrip would be observable on that executor's transport.
func validateExactTransport(exec *llmhttp.ExactExecutor, req *llmhttp.ExactAttemptRequest) *Decline {
	for _, h := range req.Headers {
		if !llmhttp.ValidHeaderName(h.Name) {
			return declinef(StageExactTransport, ReasonInvalidHeaderName, "plan carries a header with an invalid field name")
		}
		if !llmhttp.ValidHeaderValue(h.Value) {
			// The value is never surfaced; only the fact that it is control-bearing.
			return declinef(StageExactTransport, ReasonInvalidHeaderValue, "plan carries a header value with a control byte the transport rejects")
		}
	}
	err := exec.Preflight(req)
	if err == nil {
		return nil
	}
	var de *llmhttp.ExactDeclineError
	if errors.As(err, &de) {
		if strings.EqualFold(de.Field, "Host") {
			return declinef(StageExactTransport, ReasonDuplicateHost, "exact lane declines an ambiguous duplicate Host header")
		}
		return declinef(StageExactTransport, ReasonTransportControlledHeader,
			"exact lane declines a transport-controlled header %q", de.Field)
	}
	// A non-decline preflight error is unexpected; fail closed to BAML.
	return declinef(StageExactTransport, ReasonTransportControlledHeader, "exact lane preflight rejected the plan")
}
