// Package canary is the de-BAML cutover Slice 6 native SERVE implementation: for
// an admitted unary OpenAI `_dynamic` call it actually SERVES natively — it opens
// exactly ONE provider socket, translates/extracts/parses that response, runs the
// S5 same-response BAML-parse safety compare, and returns the ordinary generated
// final through the already-merged Slice-1 tryOneChild seam. It is the serving
// twin of the shadow comparator (which always declines with BAML still serving).
//
// It lives in the out-of-go.work nativeserve module and links nanollm (through
// admission + execute), so nothing here can enter the host/root link graph — the
// host stays zero-nanollm and CGO-free. A SERVE deploy profile's worker injects it
// as a neutral bamlutils.NativeServeFunc; every default build and every flag-off
// build leaves it nil, so the orchestrator's native child-attempt callback stays
// hard-off and every request is byte-identical BAML.
//
// Ownership boundary (the load-bearing invariant): admission + the S4 plan compare
// run BEFORE the claim and may DECLINE (guaranteeing no socket) so the orchestrator
// serves BAML for the same child in the same retry iteration. From the CLAIM
// onward every terminal condition is a SUCCESS or a typed FAILURE handed to the
// outer policy — there is NEVER a hidden same-child BAML resend after the claim.
package canary

import (
	"context"
	"errors"
	"fmt"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/buildrequest"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/internal/debaml"
	"github.com/invakid404/baml-rest/nativeserve/admission"
	"github.com/invakid404/baml-rest/nativeserve/execute"
	"github.com/invakid404/baml-rest/nativeserve/parity"
	"github.com/invakid404/baml-rest/nativeserve/staticoracle"
)

// canaryInternalAlias is the fixed, secret-free internal nanollm alias the serve
// engine is configured under. Like the shadow alias it is an implausible token so
// it never collides with a resolved target model or a selected client name (a
// collision declines safely at the admission client-selection stage).
const canaryInternalAlias = "__debaml_serve_internal_alias__"

// Stable, secret-free stage/reason tokens the serve implementation returns on a
// PRE-CLAIM decline (an admission decline returns the admission stage/reason
// instead; these cover the terminal serve-declined dispositions only). No token
// is ever free-form text or a secret.
const (
	stageServe       = "serve"
	stagePlanCompare = "plan_compare"
	stagePlanner     = "planner"

	reasonServedBAML      = "served_baml"
	reasonServedBAMLPanic = "served_baml_panic"
	reasonServedBAMLCtx   = "served_baml_ctx"
	reasonNoBAMLBuilder   = "no_baml_builder"
	reasonBAMLBuildError  = "baml_build_error"
	reasonPlanMismatch    = "plan_mismatch"
	reasonPlanExpired     = "plan_expired"
	reasonPlannerError    = "planner_error"
	reasonNilOutputSchema = "nil_output_schema"
	reasonNilEmitDelta    = "nil_emit_delta"
)

// errNativeServePanic backstops a panic AFTER the claim: a socket may have opened,
// so the attempt FAILS (never declines) rather than risk a hidden BAML resend.
var errNativeServePanic = errors.New("nativeserve/canary: native serve attempt panicked after claim")

// errNoBAMLOnlyParse marks a serve attempt that reached the response phase without
// a BAML-only parse closure. The generated serve seam always threads one, so this
// is a wiring bug; post-claim it FAILS (never serves an unverified native final).
// It is the shared same-response-oracle sentinel (nativeserve/staticoracle) so the
// dynamic serve path, the static shadow, and the factored static oracle all carry one
// identity.
var errNoBAMLOnlyParse = staticoracle.ErrNoBAMLOnlyParse

// Server runs the native serve implementation. Construct it with NewServer (or the
// NewServeFunc factory the serve worker uses). It holds the S3 admitter (which
// records declines/attempts) plus the de-BAML metrics and the exact-transport
// executor (the SINGLE RoundTrip owner).
type Server struct {
	admitter *admission.Admitter
	metrics  *admission.Metrics
	exec     *llmhttp.ExactExecutor
	// admitClaim is the admission step Serve runs. It defaults to
	// admitter.AdmitClaim (the real production predicate) and is OVERRIDDEN ONLY by
	// same-package tests to inject a SYNTHETIC trusted claim — S1 ships no
	// production non-openai mapping, so the trusted verification path is otherwise
	// unreachable through Serve. Production never rebinds it.
	admitClaim func(ctx context.Context, in admission.Input) (*admission.Claim, error)
	// bamlExtract is the BAML leg of the S5 same-response oracle: the
	// assistant/raw/reasoning extraction the BAML serving path performs over the
	// RAW provider bytes. It defaults to buildrequest.ExtractResponseContentBytes
	// (the production extractor) and is OVERRIDDEN ONLY by same-package tests, for
	// one reason: on the enrolled OpenAI surface both legs run that SAME extractor
	// over the SAME bytes (nanollm's openai TranslateResponse is byte-verbatim —
	// TestOpenAITranslateResponseIsByteVerbatim in nativeserve/execute pins it), so
	// no upstream response can make the assistant, raw or reasoning facet disagree
	// on its own, and the terminal predicate's per-facet gating would otherwise be
	// untestable. Production never rebinds it.
	bamlExtract func(provider string, body []byte, includeReasoning bool) (parseable, raw, reasoning string, err error)

	// cohort is a FIXED serving-cutover configuration identity this server presents to
	// admission instead of resolving one per request. Production leaves it ZERO — both
	// unary lanes resolve their identity from the request through
	// [Server.serveCohortInput] and [Server.staticCohortInput] — so a request resolves
	// to the enrolled fe-v1 cohort only when the DEPLOYMENT sealed its effective
	// selected configuration with the enrolled slot, and anything else declines at the
	// default-deny gate before any native work. It is set to a non-zero identity ONLY
	// by [NewServerWithCohortIdentity], which the gated end-to-end proofs use to
	// exercise the serving pipeline on the (surface, cohort) pairs the shipped policy
	// does NOT enable. The shipped policy enables exactly ONE pair — fe_v1 on the
	// dynamic unary call surface (serving cutover S3b) — and every other surface,
	// cohort and slot still declines pre-socket, including a sealed fe-v1
	// configuration arriving on a surface its inventory record does not declare. It is
	// NOT a rollout control: the factories a worker installs (NewServeFunc /
	// NewStaticServeFunc) present the zero identity, and enrolling a cohort is a
	// change to the shipped policy, not to this field.
	cohort admission.CohortInput

	// staticAdmitClaim is the static admission step ServeStatic runs (de-BAML Slice
	// 8C). It defaults to admission.AdmitStaticClaim (the real production predicate)
	// and is OVERRIDDEN ONLY by same-package tests to inject a SYNTHETIC static claim
	// so the post-claim serving pipeline (exact one-send, static SAP, S5 same-response
	// compare, tri-state mapping) is exercised without a live BAML plan oracle.
	// Production never rebinds it.
	staticAdmitClaim func(ctx context.Context, in admission.StaticInput) (*admission.StaticClaim, error)
}

// NewServer builds a Server recording on m and sending admitted plans through exec
// (the ONE exact RoundTrip per claimed attempt). A nil exec defaults to a hardened
// single-attempt exact executor; tests pass an executor over a loopback-guarded
// transport to fence external dials.
func NewServer(m *admission.Metrics, exec *llmhttp.ExactExecutor) *Server {
	if exec == nil {
		exec = llmhttp.NewExactExecutor(nil)
	}
	s := &Server{admitter: admission.NewAdmitter(m, exec), metrics: m, exec: exec}
	s.admitClaim = s.admitter.AdmitClaim
	s.bamlExtract = buildrequest.ExtractResponseContentBytes
	s.staticAdmitClaim = admission.AdmitStaticClaim
	return s
}

// NewServeFunc is the factory a serve-profile worker injects via
// workerboot.Options.NativeServeFactory. It registers the bounded de-BAML
// collectors on the worker's private registry and returns the neutral
// bamlutils.NativeServeFunc that drives serving.
func NewServeFunc(reg prometheus.Registerer) (bamlutils.NativeServeFunc, error) {
	m, err := admission.NewMetrics(reg)
	if err != nil {
		return nil, err
	}
	return NewServer(m, llmhttp.NewExactExecutor(nil)).Serve, nil
}

// Serve is the bamlutils.NativeServeFunc. It runs S3 admission (keeping the
// request-scoped nanollm client alive as a Claim), the S4 plan compare as a
// PRE-SOCKET precondition, then — on a full plan match — CLAIMS the attempt and
// performs exactly one native RoundTrip via execute.RunAttempt, maps the outcome
// through the disposition table, and runs the S5 same-response BAML-parse safety
// compare. Before the claim it may DECLINE (no socket); from the claim onward it
// only SUCCEEDS or FAILS.
func (s *Server) Serve(ctx context.Context, req bamlutils.NativeServeRequest) (result bamlutils.NativeServeResult) {
	claimed := false
	// Serving-cutover S1 identity: the surface is this LANE's constant (the callback
	// is installed only on the dynamic unary Request path) and the cohort is what the
	// default-deny gate resolves for the request's configuration identity — the SAME
	// bounded bucket admission gates on, so a decline recorded here and a decline
	// recorded by admission carry identical labels. Production resolves CohortNone.
	in := s.toAdmissionInput(req)
	surface, cohort := admission.SurfaceDynamicCall, admission.ResolveCohort(admission.SurfaceDynamicCall, s.serveCohortInput(req))
	// Registered FIRST so it runs LAST: it observes the final named result, including
	// the one the panic guard below substitutes. It records EXACTLY ONE phase+winner
	// pair per request (pre-claim decline, or the post-claim terminal), which is what
	// makes "sockets == claimed" and "every request has exactly one winner" provable
	// rather than asserted.
	defer func() { s.recordServeTerminal(surface, cohort, claimed, result.Disposition, result.WinnerEngine) }()
	defer func() {
		if r := recover(); r != nil {
			if claimed {
				// Post-claim panic: a socket may have opened. FAIL — a decline here
				// would trigger a hidden BAML resend for the same child. Record a
				// bounded internal_error (NOT parse_error): the panic may originate
				// anywhere in the claimed pipeline (executor/transport, translation,
				// or parser), so a parse-specific label would misclassify a pre-parse
				// panic. The once-only socket defer still counts native_sockets exactly
				// once (conservative transport-error) — see the claim boundary below.
				s.metrics.RecordServeOutcome(toAdmissionMode(req.Mode), req.Provider, admission.OutcomeInternalError)
				result = failResult(errNativeServePanic, "")
			} else {
				// Pre-claim panic: no socket occurred, so declining to BAML is safe.
				result = declineResult(stageServe, reasonServedBAMLPanic)
			}
		}
	}()

	// Cancellation gate at ENTRY, BEFORE any native FFI: an already-cancelled
	// request declines to BAML with zero native work — no New/render/Prepare, no
	// socket — and the ordinary BAML attempt then fails once with the same context
	// error. (Cancellation racing the FFI mid-admission is caught by the gates
	// inside AdmitClaim; racing the post-2xx pipeline is caught in mapAttempt.)
	if ctx.Err() != nil {
		return declineResult(stageServe, reasonServedBAMLCtx)
	}

	// Pre-socket nil-schema guard (de-BAML Slice 8C): NativeServeRequest.OutputSchema
	// is nullable, and the dynamic serve path pre-wraps the schema-neutral parser as
	// execute.DynamicParse(debaml.Parse, req.OutputSchema) below — a pre-supplied
	// ParseResponse that SHORT-CIRCUITS RunAttempt's resolveParse nil-schema guard, so
	// a nil schema would otherwise surface only inside the CLAIMED parse, after the
	// socket opened. A nil schema can never parse natively, so decline to BAML here
	// with ZERO native work (no planner, no claim, no socket) — availability-first,
	// and BAML serves with its own runtime schema.
	if req.OutputSchema == nil {
		return declineResult(stageServe, reasonNilOutputSchema)
	}

	claim, err := s.admitClaim(ctx, in)
	if err != nil {
		var d *admission.Decline
		if errors.As(err, &d) {
			return declineResult(string(d.Stage), string(d.Reason))
		}
		// Unexpected native planner/FFI error before any socket: availability-first
		// decline to BAML (admission already counted it as planner_error).
		return declineResult(stagePlanner, reasonPlannerError)
	}
	// The claim keeps the request-scoped engine alive so TranslateResponse runs on
	// the SAME client Prepare produced the plan on. Close on EVERY path (incl.
	// panic/cancel below).
	defer claim.Close()

	// The mapper's verification policy decides the pre-socket verification regime
	// (§6): the STRICT OpenAI anchor runs the S4 BAML plan-compare precondition
	// below; a TRUSTED provider SKIPS it entirely — nanollm owns the provider's
	// transport contract, so there is no BAML plan to build or compare, and
	// BuildBAMLRequest is expected nil (the direct-legacy probe passes it nil).
	policy := claim.Verification

	// S4 plan compare — a PRE-SOCKET precondition, STRICT OpenAI ONLY. A missing/
	// failed BAML builder or any per-field mismatch declines to BAML BEFORE the
	// claim; native never sends. Trusted providers record NO plan_compare at all.
	if policy == admission.PolicyStrictOpenAI {
		if req.BuildBAMLRequest == nil {
			s.metrics.RecordPlanCompare(admission.PlanCompareMismatch, admission.PlanCompareFieldMeta)
			return declineResult(stageServe, reasonNoBAMLBuilder)
		}
		bamlReq, berr := req.BuildBAMLRequest(ctx)
		if berr != nil || bamlReq == nil {
			s.metrics.RecordPlanCompare(admission.PlanCompareMismatch, admission.PlanCompareFieldMeta)
			return declineResult(stageServe, reasonBAMLBuildError)
		}
		cmp := parity.ComparePlans(bamlReq, claim.ExactRequest)
		s.recordPlanComparison(cmp)
		if !cmp.AllMatch() {
			return declineResult(stagePlanCompare, reasonPlanMismatch)
		}
	}

	// A provably PRE-SOCKET preflight rejection — the prepared plan's signature
	// window passed during the (BAML-plan-build + compare) step — opens NO socket,
	// so decline to BAML rather than claim and fail. Checked BEFORE the claim so
	// the ownership boundary is truly "immediately before the socket" and
	// execute.RunAttempt's own ErrPlanExpired preflight is never reached with a
	// stale plan. Unreachable for the admitted never-expiring OpenAI surface.
	if claim.PlanExpired() {
		return declineResult(stageServe, reasonPlanExpired)
	}

	// ctx check immediately BEFORE the claim/FFI socket: an already-cancelled
	// caller declines safely (no socket opened) rather than claiming and failing.
	if ctx.Err() != nil {
		return declineResult(stageServe, reasonServedBAMLCtx)
	}

	// CLAIM the native attempt (ownership boundary). From here every terminal
	// condition is SUCCESS or FAILURE — never a decline, never a hidden resend.
	claimed = true
	// The claimed phase is recorded HERE, at the true claim boundary — not when
	// admission returned a Claim, because the plan-compare / plan-expiry / ctx gates
	// above can still decline PRE-SOCKET after admission succeeded. Recording it here
	// keeps claimed == native_sockets exactly (operational invariant 2).
	s.metrics.RecordAdmissionPhase(surface, cohort, admission.PhaseClaimed)

	// native_sockets_total is recorded EXACTLY ONCE per claimed attempt — including
	// a panic in the executor/translation/parser (which the panic guard turns into
	// a terminal failure). A post-claim once-only defer records the conservative
	// transport-error outcome (a socket may have opened); it is superseded by the
	// definitive outcome below the instant a response — or its absence — is known,
	// so on the normal path the defer is a no-op. flag is always "on" (this path is
	// unreachable while the flag is off), so an "off" increment is impossible.
	socketRecorded := false
	recordSocket := func(outcome admission.NativeSocketOutcome) {
		if socketRecorded {
			return
		}
		socketRecorded = true
		s.metrics.RecordNativeSocket(outcome)
	}
	defer recordSocket(admission.NativeSocketTransportError)

	res, aerr := execute.RunAttempt(ctx, execute.AttemptConfig{
		Client:   claim.Client(),
		Prepared: claim.Prepared,
		Executor: s.exec,
		// Schema-neutral parser (de-BAML Slice 8C): the dynamic serve path captures
		// its DynamicOutputSchema in the closure, so execute stays schema-agnostic —
		// the static serve path captures a Return Bundle instead (ServeStatic).
		ParseResponse:    execute.DynamicParse(debaml.Parse, req.OutputSchema),
		IncludeReasoning: req.IncludeReasoning,
		// The merged S5 first-2xx heartbeat: fired before the body is buffered so
		// the pool hung-detector sees liveness on a slow body.
		SendHeartbeat: req.SendHeartbeat,
	})

	// Definitive socket outcome: a nil result means the attempt produced NO HTTP
	// response (transport/dial/timeout/read failure); a non-nil result means the
	// socket returned a response (any status).
	if res == nil {
		recordSocket(admission.NativeSocketTransportError)
	} else {
		recordSocket(admission.NativeSocketResponded)
	}

	return s.mapAttempt(ctx, req, policy, res, aerr)
}

// mapAttempt maps one claimed native attempt's (result, error) onto the neutral
// serve result, per the cutover Slice 6 disposition/error table. It NEVER declines
// (post-claim) and NEVER falls through to a BAML resend.
func (s *Server) mapAttempt(ctx context.Context, req bamlutils.NativeServeRequest, policy admission.VerificationPolicy, res *execute.AttemptResult, aerr error) bamlutils.NativeServeResult {
	if aerr != nil {
		// Caller cancellation / deadline is a transport-class typed error to the
		// outer policy — regardless of whether a partial response was buffered
		// (cancellation can race the post-2xx translate/SAP pipeline, which returns
		// the context error alongside the response context). Preserved UNCHANGED so
		// errors.Is(context.Canceled/DeadlineExceeded) holds; no details.raw.
		if isContextErr(aerr) {
			s.metrics.RecordServeOutcome(toAdmissionMode(req.Mode), req.Provider, admission.OutcomeTransportError)
			return failResult(aerr, "")
		}
		// A nil result means the failure occurred BEFORE any HTTP response was
		// produced (transport/dial/timeout/body-read, or a pre-socket RunAttempt
		// guard). Hand the typed error to the outer policy UNCHANGED so its
		// classification behaves exactly as for a BAML attempt (preserving
		// errors.As(*llmhttp.TransportError)/errors.Is(ErrTransportFlake)). No
		// response body exists, so no details.raw.
		if res == nil {
			s.metrics.RecordServeOutcome(toAdmissionMode(req.Mode), req.Provider, admission.OutcomeTransportError)
			return failResult(aerr, "")
		}
		// Post-response failure — res is non-nil carrying the response context.
		if res.SAPInvoked {
			// CLAIMED native SAP parse failure -> ErrOutputParse + the extracted
			// assistant text as details.raw (mirrors the BAML parse_error envelope).
			s.metrics.RecordServeOutcome(toAdmissionMode(req.Mode), req.Provider, admission.OutcomeParseError)
			return failResult(&buildrequest.OutputParseError{Err: aerr}, res.Raw)
		}
		// Translate / non-JSON-2xx / assistant-extraction failure -> today's
		// BuildRequest extraction error class (NOT nanollm's synthesized 502) with
		// the raw upstream body retained as details.raw.
		s.metrics.RecordServeOutcome(toAdmissionMode(req.Mode), req.Provider, admission.OutcomeTranslateError)
		return failResult(fmt.Errorf("buildrequest: failed to extract response content: %w", aerr), string(res.ProviderBody))
	}

	// aerr == nil: dispatch on the provider/parse outcome (data, not error).
	switch res.Outcome {
	case execute.OutcomeProviderError:
		// Provider non-2xx: real upstream status + body capped to the 4 KiB PUBLIC
		// cap (llmhttp.MaxErrorBodyBytes), NOT the 64 KiB internal cap. Maps to the
		// existing provider_error classifier. SAP was never invoked.
		s.metrics.RecordServeOutcome(toAdmissionMode(req.Mode), req.Provider, admission.OutcomeProviderError)
		return failResult(&llmhttp.HTTPError{
			StatusCode: res.ProviderStatus,
			Body:       capErrorBody(res.ProviderBody),
		}, "")
	case execute.OutcomeInvalidBody:
		// Malformed provider 2xx: today's extraction error class with the raw
		// upstream body as details.raw — NOT nanollm's synthesized 502.
		s.metrics.RecordServeOutcome(toAdmissionMode(req.Mode), req.Provider, admission.OutcomeTranslateError)
		return failResult(errMalformed2xx, string(res.ProviderBody))
	case execute.OutcomeParseDeclined:
		// Trusted providers take the non-comparison parse-only fallback (§6); the
		// strict OpenAI path keeps the same-response-facet-recording serveParseOnly.
		if policy == admission.PolicyTrustedProvider {
			return s.serveTrustedParseOnly(ctx, req, res)
		}
		return s.serveParseOnly(ctx, req, res)
	case execute.OutcomeStructured:
		// Trusted providers serve the native SAP result DIRECTLY (no BAML compare);
		// the strict OpenAI path runs the S5 same-response safety compare unchanged.
		if policy == admission.PolicyTrustedProvider {
			return s.serveTrustedStructured(req, res)
		}
		return s.serveStructured(ctx, req, res)
	default:
		// Unreachable for the closed Outcome set; fail conservatively (never serve).
		s.metrics.RecordServeOutcome(toAdmissionMode(req.Mode), req.Provider, admission.OutcomeParseError)
		return failResult(fmt.Errorf("nativeserve/canary: unexpected attempt outcome %v", res.Outcome), "")
	}
}

// serveStructured handles a clean native structured claim (OutcomeStructured): it
// runs the S5 same-response BAML-parse safety compare over the SAME bytes and
// records response_compare per facet. If BAML's extraction/parse ERRORS on those
// bytes, compatibility wins — the corresponding parse/extraction error is
// returned (never a native final BAML would have rejected). It serves native ONLY
// when ALL FIVE compared facets agree — assistant text, raw, reasoning, structured
// value and ordering; on ANY drift it serves BAML's reading of the same bytes.
func (s *Server) serveStructured(ctx context.Context, req bamlutils.NativeServeRequest, res *execute.AttemptResult) bamlutils.NativeServeResult {
	// BAML leg — extract assistant/raw/reasoning the BAML serving path would, from
	// the SAME raw provider body, then run the BAML-ONLY parse closure. Reasoning
	// is gated by the REQUEST's IncludeReasoning (matching the native leg's own
	// gating) so the reasoning facet compares apples-to-apples for the served
	// envelope — a plain `call` (reasoning off) compares two empty channels, not a
	// forced-on native-vs-off drift.
	//
	// The strict same-response oracle runs from here, so record its own phase: BAML
	// extracts + parses the SAME bytes the ONE native provider request returned. A
	// separate phase is what keeps "BAML parsed these bytes" from ever reading as
	// "BAML transported this request".
	s.metrics.RecordAdmissionPhase(admission.SurfaceDynamicCall, admission.ResolveCohort(admission.SurfaceDynamicCall, s.serveCohortInput(req)), admission.PhaseSameResponseOracle)

	bamlParseable, bamlRaw, bamlReasoning, xerr := s.bamlExtract("openai", res.ProviderBody, req.IncludeReasoning)
	if xerr != nil {
		// BAML extraction errors on bytes native claims -> return the extraction
		// error (BAML would have rejected), never serve a divergent native final.
		s.metrics.RecordResponseCompare(admission.ResponseCompareMismatch, admission.ResponseCompareFieldError)
		s.metrics.RecordServeOutcome(toAdmissionMode(req.Mode), req.Provider, admission.OutcomeTranslateError)
		return failResult(fmt.Errorf("buildrequest: failed to extract response content: %w", xerr), string(res.ProviderBody))
	}
	if req.BAMLOnlyParse == nil {
		s.metrics.RecordResponseCompare(admission.ResponseCompareMismatch, admission.ResponseCompareFieldError)
		s.metrics.RecordServeOutcome(toAdmissionMode(req.Mode), req.Provider, admission.OutcomeParseError)
		return failResult(&buildrequest.OutputParseError{Err: errNoBAMLOnlyParse}, res.Raw)
	}
	// ctx gate BEFORE the BAML parse: a request already canceled/deadline-exceeded
	// must never be served, regardless of what BAMLOnlyParse would return.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return s.ctxTransportFail(req, ctxErr)
	}
	bamlStructured, berr := req.BAMLOnlyParse(ctx, bamlParseable)
	// ctx gate AFTER the BAML parse: BAMLOnlyParse may IGNORE cancellation and
	// return a valid value (err == nil) on a canceled context — never serve it.
	// Checked before berr so a canceled request maps to transport_error, not a
	// (spurious) served result or parse error.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return s.ctxTransportFail(req, ctxErr)
	}
	if berr != nil {
		if isContextErr(berr) {
			// BAMLOnlyParse returned its OWN context error (e.g. an internal
			// deadline) while the request ctx is still live: transport-class, NOT a
			// parse failure. No OutputParseError wrap, no details.raw.
			return s.ctxTransportFail(req, berr)
		}
		// BAML parse errors on bytes native claims -> return the parse error
		// (BAML-as-current compatibility), never serve a result BAML would reject.
		s.metrics.RecordResponseCompare(admission.ResponseCompareMismatch, admission.ResponseCompareFieldError)
		s.metrics.RecordServeOutcome(toAdmissionMode(req.Mode), req.Provider, admission.OutcomeParseError)
		return failResult(&buildrequest.OutputParseError{Err: berr}, res.Raw)
	}
	s.metrics.RecordResponseCompare(admission.ResponseCompareMatch, admission.ResponseCompareFieldError)

	// Per-facet response parity — translate always OK on OutcomeStructured.
	//
	// EVERY facet below is BOTH recorded AND gated on (S3 admission gate 9): the
	// assistant channel, the two /call-with-raw channels, the structured value and
	// its ordering must ALL agree before native may own the served envelope. A
	// facet that were only RECORDED would be an out-claim: the drift would be
	// visible as a metric while the caller still received native's answer for a
	// response the two engines read differently.
	s.recordResponse(true, admission.ResponseCompareFieldTranslate)
	assistantMatch := res.AssistantText == bamlParseable
	s.recordResponse(assistantMatch, admission.ResponseCompareFieldAssistant)
	rawMatch := res.Raw == bamlRaw
	s.recordResponse(rawMatch, admission.ResponseCompareFieldRaw)
	reasoningMatch := res.Reasoning == bamlReasoning
	s.recordResponse(reasoningMatch, admission.ResponseCompareFieldReasoning)
	structuredMatch, orderMatch := parity.CompareStructured(res.Structured, bamlStructured, req.OutputSchema)
	s.recordResponse(structuredMatch, admission.ResponseCompareFieldStructured)
	s.recordResponse(orderMatch, admission.ResponseCompareFieldOrder)

	if assistantMatch && rawMatch && reasoningMatch && structuredMatch && orderMatch {
		// FULL MATCH on every compared facet -> serve the native flattened JSON
		// with native-owned raw/reasoning.
		s.metrics.RecordServeOutcome(toAdmissionMode(req.Mode), req.Provider, admission.OutcomeSuccess)
		return successResult(res.Structured, res, bamlutils.NativeServeEngineNative)
	}
	// ANY same-response facet drift -> serve BAML's reading of the SAME bytes for
	// safety (still one native provider request, never a resend). The whole
	// envelope comes from BAML's leg — its parse AND its raw/reasoning channels —
	// because a raw/reasoning drift is exactly the case where returning native's
	// channels alongside BAML's structured value would ship a THIRD answer neither
	// engine would have produced. The response_compare mismatch is the
	// zero-tolerance signal; winner_engine records that BAML's parse produced it.
	s.metrics.RecordFallback(admission.FallbackParseOnly)
	s.metrics.RecordServeOutcome(toAdmissionMode(req.Mode), req.Provider, admission.OutcomeSuccess)
	return bamlSameResponseResult(bamlStructured, bamlRaw, bamlReasoning)
}

// serveParseOnly handles a native SAP decline (OutcomeParseDeclined): native
// transported and translated cleanly but declined the parse shape, so BAML
// parse-only runs on the SAME extracted assistant text and serves that final.
// One native provider request, zero BAML provider requests.
func (s *Server) serveParseOnly(ctx context.Context, req bamlutils.NativeServeRequest, res *execute.AttemptResult) bamlutils.NativeServeResult {
	if req.BAMLOnlyParse == nil {
		s.metrics.RecordResponseCompare(admission.ResponseCompareMismatch, admission.ResponseCompareFieldError)
		s.metrics.RecordServeOutcome(toAdmissionMode(req.Mode), req.Provider, admission.OutcomeParseError)
		return failResult(&buildrequest.OutputParseError{Err: errNoBAMLOnlyParse}, res.Raw)
	}
	// ctx gate BEFORE the BAML parse-only fallback: a request already canceled/
	// deadline-exceeded must never be served, regardless of what BAMLOnlyParse
	// would return.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return s.ctxTransportFail(req, ctxErr)
	}
	bamlStructured, berr := req.BAMLOnlyParse(ctx, res.AssistantText)
	// ctx gate AFTER: BAMLOnlyParse may IGNORE cancellation and return a valid
	// value (err == nil) on a canceled context — never serve it. Checked before
	// berr so a canceled request maps to transport_error.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return s.ctxTransportFail(req, ctxErr)
	}
	if berr != nil {
		if isContextErr(berr) {
			// BAMLOnlyParse returned its OWN context error while the request ctx is
			// still live: transport-class, NOT a parse error.
			return s.ctxTransportFail(req, berr)
		}
		// BAML parse-only itself errored -> parse_error with raw; no resend.
		s.metrics.RecordServeOutcome(toAdmissionMode(req.Mode), req.Provider, admission.OutcomeParseError)
		return failResult(&buildrequest.OutputParseError{Err: berr}, res.Raw)
	}
	// native declined where BAML parsed -> a real structured/order drift.
	s.recordResponse(true, admission.ResponseCompareFieldTranslate)
	s.recordResponse(false, admission.ResponseCompareFieldStructured)
	s.recordResponse(false, admission.ResponseCompareFieldOrder)
	s.metrics.RecordFallback(admission.FallbackParseOnly)
	s.metrics.RecordServeOutcome(toAdmissionMode(req.Mode), req.Provider, admission.OutcomeParseDecline)
	return successResult(bamlStructured, res, bamlutils.NativeServeEngineBAMLParse)
}

// serveTrustedStructured serves a clean TRUSTED-provider structured claim
// (OutcomeStructured) DIRECTLY: RunAttempt already translated the provider
// response and extracted the structured output from nanollm's normalized OpenAI
// body, so there is nothing to compare against BAML. It records the served
// success and returns the native SAP result with the native winner engine — it
// NEVER calls ExtractResponseContentBytes on raw provider bytes, BAMLOnlyParse,
// parity.CompareStructured, or any comparison recorder (§6: trusted providers
// record NO plan/response comparison at all; there is no BAML differential).
func (s *Server) serveTrustedStructured(req bamlutils.NativeServeRequest, res *execute.AttemptResult) bamlutils.NativeServeResult {
	s.metrics.RecordServeOutcome(toAdmissionMode(req.Mode), req.Provider, admission.OutcomeSuccess)
	return successResult(res.Structured, res, bamlutils.NativeServeEngineNative)
}

// serveTrustedParseOnly handles a TRUSTED-provider native SAP decline
// (OutcomeParseDeclined): native transported and translated cleanly but declined
// the parse shape, so BAML parse-only runs on the SAME extracted assistant text
// as a PARSER fallback (not a BAML transport or a differential admission bar) and
// serves that final. Unlike the strict serveParseOnly it records ONLY
// fallback=parse_only and the terminal outcome — NEVER a native-vs-BAML
// response_compare facet (§6). One native provider request, zero BAML provider
// requests.
func (s *Server) serveTrustedParseOnly(ctx context.Context, req bamlutils.NativeServeRequest, res *execute.AttemptResult) bamlutils.NativeServeResult {
	if req.BAMLOnlyParse == nil {
		s.metrics.RecordServeOutcome(toAdmissionMode(req.Mode), req.Provider, admission.OutcomeParseError)
		return failResult(&buildrequest.OutputParseError{Err: errNoBAMLOnlyParse}, res.Raw)
	}
	// ctx gate BEFORE the BAML parse-only fallback: a request already canceled/
	// deadline-exceeded must never be served, regardless of what BAMLOnlyParse
	// would return.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return s.ctxTransportFail(req, ctxErr)
	}
	bamlStructured, berr := req.BAMLOnlyParse(ctx, res.AssistantText)
	// ctx gate AFTER: BAMLOnlyParse may IGNORE cancellation and return a valid
	// value (err == nil) on a canceled context — never serve it.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return s.ctxTransportFail(req, ctxErr)
	}
	if berr != nil {
		if isContextErr(berr) {
			// BAMLOnlyParse returned its OWN context error while the request ctx is
			// still live: transport-class, NOT a parse error.
			return s.ctxTransportFail(req, berr)
		}
		s.metrics.RecordServeOutcome(toAdmissionMode(req.Mode), req.Provider, admission.OutcomeParseError)
		return failResult(&buildrequest.OutputParseError{Err: berr}, res.Raw)
	}
	s.metrics.RecordFallback(admission.FallbackParseOnly)
	s.metrics.RecordServeOutcome(toAdmissionMode(req.Mode), req.Provider, admission.OutcomeParseDecline)
	return successResult(bamlStructured, res, bamlutils.NativeServeEngineBAMLParse)
}

// recordPlanComparison records one plan_compare series per compared field (NO
// values), so a per-field mismatch is independently observable and alertable
// (zero tolerance). It mirrors the shadow comparator's recording exactly.
func (s *Server) recordPlanComparison(cmp parity.PlanComparison) {
	s.metrics.RecordPlanCompare(planResult(cmp.Method), admission.PlanCompareFieldMethod)
	s.metrics.RecordPlanCompare(planResult(cmp.Target), admission.PlanCompareFieldTarget)
	s.metrics.RecordPlanCompare(planResult(cmp.Host), admission.PlanCompareFieldHost)
	s.metrics.RecordPlanCompare(planResult(cmp.Headers), admission.PlanCompareFieldHeaders)
	s.metrics.RecordPlanCompare(planResult(cmp.Body), admission.PlanCompareFieldBody)
	if cmp.MetaMismatch {
		s.metrics.RecordPlanCompare(admission.PlanCompareMismatch, admission.PlanCompareFieldMeta)
	}
}

func (s *Server) recordResponse(match bool, field admission.ResponseCompareField) {
	s.metrics.RecordResponseCompare(responseResult(match), field)
}

// errMalformed2xx is today's BuildRequest extraction error class for a malformed
// provider 2xx body — a plain error (NOT an *HTTPError, NOT ErrOutputParse) so the
// worker classifier maps it to worker_error with details.raw, exactly like the
// BAML build/send path, rather than surfacing nanollm's synthesized 502.
var errMalformed2xx = staticoracle.ErrMalformed2xx

// isContextErr reports whether err is a caller cancellation or deadline. These
// are handled as a transport-class typed error everywhere post-claim (mapAttempt
// and the BAML-only parse branches): recorded as transport_error and returned
// UNCHANGED (no OutputParseError wrap, no details.raw) so errors.Is holds for the
// outer policy.
func isContextErr(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// ctxTransportFail records a transport_error serve outcome and returns the given
// context error UNCHANGED (no OutputParseError wrap, no details.raw) so
// errors.Is(context.Canceled/DeadlineExceeded) holds for the outer policy. It is
// the terminal for the BAML-only parse ctx gates: a canceled/deadline-exceeded
// request must NEVER be served a native/parse result, even if the BAMLOnlyParse
// callback ignored cancellation and returned a valid value.
func (s *Server) ctxTransportFail(req bamlutils.NativeServeRequest, ctxErr error) bamlutils.NativeServeResult {
	s.metrics.RecordServeOutcome(toAdmissionMode(req.Mode), req.Provider, admission.OutcomeTransportError)
	return failResult(ctxErr, "")
}

// capErrorBody caps the raw provider body to the 4 KiB PUBLIC diagnostic cap
// (llmhttp.MaxErrorBodyBytes) used by the existing provider_error envelope — NOT
// the 64 KiB internal exact cap.
func capErrorBody(b []byte) string {
	if len(b) > llmhttp.MaxErrorBodyBytes {
		b = b[:llmhttp.MaxErrorBodyBytes]
	}
	return string(b)
}

func declineResult(stage, reason string) bamlutils.NativeServeResult {
	return bamlutils.NativeServeResult{
		Disposition: bamlutils.NativeServeDeclined,
		Stage:       stage,
		Reason:      reason,
	}
}

func failResult(err error, raw string) bamlutils.NativeServeResult {
	return bamlutils.NativeServeResult{
		Disposition:   bamlutils.NativeServeFailed,
		Err:           err,
		RawDiagnostic: raw,
	}
}

// successResult wraps a served final: finalJSON is the flattened dynamic-output
// JSON (the generated helper wraps it via wrapDeBAMLDynamicOutput), raw/reasoning
// are the native-owned /call-with-raw channels, and engine is the bounded
// winner-engine token.
func successResult(finalJSON []byte, res *execute.AttemptResult, engine string) bamlutils.NativeServeResult {
	return bamlutils.NativeServeResult{
		Disposition:  bamlutils.NativeServeSucceeded,
		FinalJSON:    finalJSON,
		Raw:          res.Raw,
		Reasoning:    res.Reasoning,
		WinnerEngine: engine,
	}
}

// bamlSameResponseResult wraps the BAML same-response terminal: BAML's parse of
// the SAME bytes the ONE native provider request returned, together with BAML's
// OWN raw/reasoning channels extracted from those bytes. It is deliberately NOT
// successResult with the native AttemptResult — on this path a facet the two
// engines read differently is exactly what brought us here, so every channel the
// caller sees must come from the single engine whose reading is being served.
// Still one native provider request; never a resend.
func bamlSameResponseResult(finalJSON []byte, bamlRaw, bamlReasoning string) bamlutils.NativeServeResult {
	return bamlutils.NativeServeResult{
		Disposition:  bamlutils.NativeServeSucceeded,
		FinalJSON:    finalJSON,
		Raw:          bamlRaw,
		Reasoning:    bamlReasoning,
		WinnerEngine: bamlutils.NativeServeEngineBAMLParse,
	}
}

func planResult(match bool) admission.PlanCompareResult {
	if match {
		return admission.PlanCompareMatch
	}
	return admission.PlanCompareMismatch
}

func responseResult(match bool) admission.ResponseCompareResult {
	if match {
		return admission.ResponseCompareMatch
	}
	return admission.ResponseCompareMismatch
}

// toAdmissionInput maps the neutral serve request into the S3 admission Input.
// The serve implementation only runs on a native-capable worker, with the umbrella
// flag on, on the dynamic BuildRequest route (the callback is invoked from the
// non-streaming Request path only), so those layer-1 facts are fixed true; the
// TRUTHFUL request-retry-override fact, the WouldRewriteOrProxy predicate, and
// provider/registry/messages/schema come from the request.
func (s *Server) toAdmissionInput(req bamlutils.NativeServeRequest) admission.Input {
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
		Alias:                   canaryInternalAlias,
		Messages:                req.Messages,
		OutputSchema:            req.OutputSchema,
		Cohort:                  s.serveCohortInput(req),
	}
}

// serveCohortInput is the SINGLE definition of a served request's serving-cutover
// configuration identity, so admission's gate and the serve boundary's telemetry
// can never disagree about which cohort a request belongs to.
//
// Serving cutover S3a makes it PER REQUEST, and sources it from a fact the request
// cannot manufacture. The registry reaching this seam is the CALLER'S document — the
// dynamic surface takes `client_registry` from the public request body — so nothing
// derived from it can establish that a request is running the deployment's approved
// configuration, not even a byte-exact match against the approved shape. What can is
// the TRUSTED-CONFIGURATION SEAL the worker's config load applies to a client the
// deployment itself configured; admission.ResolveConfigIdentity reads that seal and
// nothing else for identity, with the request facts below as narrowing conditions on
// top of it.
//
// So neither the caller nor the process decides: a worker that happens to host an
// approved configuration gives no identity to a request that did not select the
// sealed client, and a caller that describes the approved configuration perfectly
// gets none either. If the effective selected configuration cannot be PROVEN to be
// one the deployment sealed, the resolver returns no identity, the gate resolves
// admission.CohortNone, and the request declines at the cohort stage before any
// native work. With nothing sealed — the shipped default — that is EVERY request.
//
// s.cohort is the one exception and it is not a production path: the gated proof
// suites build their server through the `nanollm_integration`-tagged constructor with
// a fixed enrolled identity, so they can exercise the pipeline BEHIND the default-deny
// gate. A released consumer cannot link that constructor, and every untagged factory
// leaves s.cohort zero.
func (s *Server) serveCohortInput(req bamlutils.NativeServeRequest) admission.CohortInput {
	if s.cohort.Assigned() {
		return s.cohort
	}
	id := admission.ResolveConfigIdentity(admission.ConfigSelection{
		Registry:         req.Registry,
		ResolvedProvider: req.Provider,
		SelectedLeaf:     req.ClientOverride,
		SingleLeaf:       req.SingleLeaf,
		HasFallbackChain: req.HasFallbackChain,
		HasRoundRobin:    req.HasRoundRobin,
		// The truthful request-retry-override fact the seam threads: a retry override
		// means the single-attempt exact lane is not what would run, so the effective
		// selected leaf is not a single proven answer.
		HasRequestRetryOverride: req.HasRequestRetryOverride,
		// The direct-legacy native-first probe route carries no BAML plan builder, so
		// the strict plan equality fe-v1 requires could not run for it at all. A
		// request that could never be a valid claim never gets an identity.
		HasBAMLPlanOracle: req.BuildBAMLRequest != nil,
	})
	return admission.CohortInput{Fingerprint: id.Fingerprint, Provider: id.Provider}
}

// recordServeTerminal records EXACTLY ONE serving-cutover phase+winner pair for a
// finished request, from the disposition the serve path actually returned.
//
// Pre-claim (claimed == false) the only possible disposition is Declined, and it is
// always the safe BAML-transport winner: no socket opened, BAML serves the same
// request. Post-claim there is no decline — the ownership boundary forbids it — so
// the terminal is a success (native, or BAML's parse of the SAME response bytes) or
// a typed failure. A post-claim Declined would be an ownership-boundary violation;
// it is recorded as a post-claim FAILURE rather than as a safe pre-claim decline, so
// the anomaly shows up in the winner series instead of hiding inside the normal
// baml_transport bucket.
//
// An unrecognized winner-engine token also records failure rather than native: the
// reporting side fails closed, so a future engine token cannot silently inflate the
// native-win count that gates cohort promotion.
func (s *Server) recordServeTerminal(surface admission.Surface, cohort admission.CohortID, claimed bool, disposition bamlutils.NativeServeDisposition, engine string) {
	if !claimed {
		s.metrics.RecordPreclaimDecline(surface, cohort)
		return
	}
	winner := admission.WinnerFailure
	if disposition == bamlutils.NativeServeSucceeded {
		switch engine {
		case bamlutils.NativeServeEngineNative:
			winner = admission.WinnerNative
		case bamlutils.NativeServeEngineBAMLParse:
			winner = admission.WinnerBAMLParseSameResponse
		}
	}
	s.metrics.RecordPostclaimTerminal(surface, cohort, winner)
}

func toAdmissionMode(m bamlutils.NativeServeMode) admission.Mode {
	switch m {
	case bamlutils.NativeServeModeCall:
		return admission.ModeCall
	case bamlutils.NativeServeModeCallWithRaw:
		return admission.ModeCallWithRaw
	default:
		return admission.ModeUnknown
	}
}
