package shadow

import (
	"context"
	"errors"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/internal/nativebody/nanollmprepare/admission"
)

// shadowInternalAlias is the fixed, secret-free internal nanollm alias the shadow
// comparator configures its request-scoped engine under. It is deliberately an
// implausible token so it never collides with a resolved target model or a
// selected client name — a collision would decline at the admission
// client-selection stage (safe, non-serving), never admit a tautological plan
// where the alias equals the target.
const shadowInternalAlias = "__debaml_shadow_internal_alias__"

// Stable, secret-free stage/reason tokens the comparator returns to the Slice-1
// decline outcome. An admission decline returns the admission stage/reason
// instead; these cover the terminal shadow dispositions only.
const (
	stageShadow          = "shadow"
	stagePlanner         = "planner"
	reasonServedBAML     = "served_baml"
	reasonBAMLBuildError = "baml_build_error"
	reasonNoBAMLBuilder  = "no_baml_builder"
	reasonPlannerError   = "planner_error"
)

// Comparator runs the one-send shadow comparison. Construct it with NewComparator
// (or the NewShadowFunc factory the shadow worker uses). It holds the S3 admitter
// (which records declines/attempts) plus the de-BAML metrics (which record
// plan_compare). It opens ZERO sockets and performs ZERO RoundTrips on every path.
type Comparator struct {
	admitter *admission.Admitter
	metrics  *admission.Metrics
}

// NewComparator builds a Comparator recording on m and preflighting through exec.
// exec is used ONLY for the admission no-send header preflight (Admit never
// dials), so the comparator opens no socket; a nil exec defaults to a hardened
// single-attempt exact executor. Tests pass an executor over a counting transport
// to observe that the whole comparison stays at zero sockets.
func NewComparator(m *admission.Metrics, exec *llmhttp.ExactExecutor) *Comparator {
	return &Comparator{admitter: admission.NewAdmitter(m, exec), metrics: m}
}

// NewShadowFunc is the factory a shadow-profile worker injects via
// workerboot.Options.NativeShadowFactory. It registers the bounded de-BAML
// collectors (declines / attempts / plan_compare) on the worker's private
// registry and returns the neutral bamlutils.NativeShadowFunc that drives the
// comparison. The hardened exact executor it builds is used only for the no-send
// preflight, so the comparator opens no socket.
func NewShadowFunc(reg prometheus.Registerer) (bamlutils.NativeShadowFunc, error) {
	m, err := admission.NewMetrics(reg)
	if err != nil {
		return nil, err
	}
	c := NewComparator(m, llmhttp.NewExactExecutor(nil))
	return c.Compare, nil
}

// Compare is the bamlutils.NativeShadowFunc. It runs the S3 admission for the
// request; on ADMIT it builds BAML's plan for the SAME child WITHOUT sending and
// compares it against the native plan, recording a bounded plan_compare result
// per field (NO values). It ALWAYS returns a DECLINE so the orchestrator serves
// BAML for the same child in the same retry iteration — native never sends. On an
// admission DECLINE it records nothing extra (admission already recorded the
// decline) and returns the stable admission stage/reason so the routing decision
// is observable.
func (c *Comparator) Compare(ctx context.Context, req bamlutils.NativeShadowRequest) (result bamlutils.NativeShadowResult) {
	// Shadow comparison is strictly non-authoritative. A panic from the native
	// admission/FFI work or the no-send plan-builder must not escape the callback
	// and turn an otherwise BAML-served request into a shadow-induced failure.
	// Do not record a fabricated comparison for the interrupted work; simply
	// decline and let the orchestrator run its ordinary BAML path.
	defer func() {
		if recover() != nil {
			result = bamlutils.NativeShadowResult{Stage: stageShadow, Reason: reasonServedBAML}
		}
	}()

	admitted, err := c.admitter.Admit(ctx, toAdmissionInput(req))
	if err != nil {
		var d *admission.Decline
		if errors.As(err, &d) {
			return bamlutils.NativeShadowResult{Stage: string(d.Stage), Reason: string(d.Reason)}
		}
		// An unexpected native planner/FFI error before any socket: availability-
		// first decline to BAML. admission already counted it as planner_error.
		return bamlutils.NativeShadowResult{Stage: stagePlanner, Reason: reasonPlannerError}
	}

	// Admitted up to — but not including — the RoundTrip. Obtain BAML's built plan
	// for the SAME child WITHOUT sending, then compare. A failure to build BAML's
	// plan is a structural (meta) mismatch — recorded, then declined to BAML.
	if req.BuildBAMLRequest == nil {
		c.metrics.RecordPlanCompare(admission.PlanCompareMismatch, admission.PlanCompareFieldMeta)
		return bamlutils.NativeShadowResult{Stage: stageShadow, Reason: reasonNoBAMLBuilder}
	}
	bamlReq, berr := req.BuildBAMLRequest(ctx)
	if berr != nil || bamlReq == nil {
		c.metrics.RecordPlanCompare(admission.PlanCompareMismatch, admission.PlanCompareFieldMeta)
		return bamlutils.NativeShadowResult{Stage: stageShadow, Reason: reasonBAMLBuildError}
	}

	cmp := comparePlans(bamlReq, admitted.ExactRequest)
	c.recordComparison(cmp)

	// Assign the named return (not a new var — `result` is the named result of
	// Compare, guarded by the deferred recover above).
	result = bamlutils.NativeShadowResult{Stage: stageShadow, Reason: reasonServedBAML}
	// Same-response parity (de-BAML cutover Slice 5) runs ONLY when the
	// request-plan comparison MATCHED every facet: an unproven request shape must
	// never have its response parity recorded. On a match, install the OnResponse
	// continuation the generated seam forwards onto the declined outcome — the
	// orchestrator invokes it with BAML's already-fetched status+body AFTER BAML
	// serves. It opens no socket and never changes what BAML serves.
	if cmp.allMatch() {
		result.OnResponse = func(rctx context.Context, status int, body []byte) {
			c.compareResponse(rctx, req, status, body)
		}
	}
	return result
}

// recordComparison records one plan_compare series per compared field, so a
// per-field mismatch is independently observable and alertable (zero tolerance).
// A structural fail-closed on the header facet (a plan that could not be
// normalized) additionally records a `meta` mismatch, keeping it distinct from a
// semantic header-value drift while leaving the unaffected method/target/host/
// body facets to report their real result.
func (c *Comparator) recordComparison(cmp planComparison) {
	c.metrics.RecordPlanCompare(planResult(cmp.Method), admission.PlanCompareFieldMethod)
	c.metrics.RecordPlanCompare(planResult(cmp.Target), admission.PlanCompareFieldTarget)
	c.metrics.RecordPlanCompare(planResult(cmp.Host), admission.PlanCompareFieldHost)
	c.metrics.RecordPlanCompare(planResult(cmp.Headers), admission.PlanCompareFieldHeaders)
	c.metrics.RecordPlanCompare(planResult(cmp.Body), admission.PlanCompareFieldBody)
	if cmp.MetaMismatch {
		c.metrics.RecordPlanCompare(admission.PlanCompareMismatch, admission.PlanCompareFieldMeta)
	}
}

func planResult(match bool) admission.PlanCompareResult {
	if match {
		return admission.PlanCompareMatch
	}
	return admission.PlanCompareMismatch
}

// toAdmissionInput maps the neutral shadow request into the S3 admission Input.
// The shadow comparator only runs on a native-capable worker, with the umbrella
// flag on, on the dynamic BuildRequest route (the callback is invoked from the
// non-streaming Request path only), so those layer-1 facts are fixed true; the
// TRUTHFUL request-retry-override fact, the WouldRewriteOrProxy predicate (the send
// client's own rewrite/proxy resolver, which admission evaluates against the
// effective target it resolves), and provider/registry/messages/schema come from
// the request. A true retry-override declines at the strategy gate; a rewrite or a
// proxied effective target declines immediately after the effective client is
// mapped — both BEFORE BAML's plan is obtained and BEFORE any plan_compare is
// recorded, so an unproven shape never records a (possibly false) match.
func toAdmissionInput(req bamlutils.NativeShadowRequest) admission.Input {
	return admission.Input{
		WorkerCapable:           true,
		RequestAPIPresent:       true,
		OnBuildRequestRoute:     true,
		FlagEnabled:             true,
		Method:                  bamlutils.DynamicMethodName,
		Mode:                    toAdmissionMode(req.Mode),
		SingleLeaf:              req.SingleLeaf,
		HasFallbackChain:        req.HasFallbackChain,
		HasRoundRobin:           req.HasRoundRobin,
		IsLegacyChild:           false,
		HasRequestRetryOverride: req.HasRequestRetryOverride,
		WouldRewriteOrProxy:     req.WouldRewriteOrProxy,
		ResolvedProvider:        req.Provider,
		Registry:                req.Registry,
		Alias:                   shadowInternalAlias,
		Messages:                req.Messages,
		OutputSchema:            req.OutputSchema,
	}
}

func toAdmissionMode(m bamlutils.NativeShadowMode) admission.Mode {
	switch m {
	case bamlutils.NativeShadowModeCall:
		return admission.ModeCall
	case bamlutils.NativeShadowModeCallWithRaw:
		return admission.ModeCallWithRaw
	default:
		return admission.ModeUnknown
	}
}
