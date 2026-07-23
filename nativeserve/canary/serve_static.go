package canary

// De-BAML Slice 8C — static unary SHADOW→SERVE implementation.
//
// ServeStatic is the STATIC twin of Serve: for an admitted static unary `/call` it
// actually SERVES natively — AdmitStaticClaim runs the full pre-socket predicate
// (descriptor envelope, arg binder, Return-Bundle lower/support, RenderStatic,
// canonical body, nanollm Prepare, and the strict BAML `Request.<Method>` no-send
// plan compare) and, on a full would-admit, returns a request-scoped claim; then
// ServeStatic CLAIMS the attempt and performs exactly ONE native RoundTrip via
// execute.RunAttempt, runs native static SAP over the selected Return Bundle
// (debaml.ParseStaticBundle, captured in a schema-neutral parse closure), runs the
// same-response BAML `Parse.<Method>` safety compare, and returns the winning
// flattened canonical JSON. Before the claim it may DECLINE (no socket) so the
// generated seam serves BAML; from the claim onward it only SUCCEEDS or FAILS —
// never a hidden BAML resend, and the BAML parse of the identical completed
// response is a comparator only (it never builds/sends a second request).
//
// It reuses the EXACT Slice-6 serve core — the shared admission Metrics + exact
// executor (the SINGLE RoundTrip owner) + execute.RunAttempt + parity — through the
// closed static route kind 8B added, so the static and dynamic serving paths are at
// transport parity by construction. The static Bundle PARSING stays owned by
// internal/debaml (ParseStaticBundle) and crosses this boundary only as a neutral
// closure, keeping the later recursion slices tar-free (scope §7).

import (
	"context"
	"errors"
	"fmt"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/buildrequest"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/internal/debaml"
	"github.com/invakid404/baml-rest/internal/schema"
	"github.com/invakid404/baml-rest/nativeserve/admission"
	"github.com/invakid404/baml-rest/nativeserve/execute"
	"github.com/invakid404/baml-rest/nativeserve/parity"
)

// NewStaticServeFunc is the factory a SERVE-profile worker injects via
// workerboot.Options.NativeStaticServeFactory. It registers the bounded de-BAML
// collectors on the worker's private registry and returns the neutral
// bamlutils.NativeStaticServeFunc that drives static serving. It shares the exact
// serve Server the dynamic path uses, so both drive byte-identical transport.
func NewStaticServeFunc(reg prometheus.Registerer) (bamlutils.NativeStaticServeFunc, error) {
	// REUSE the de-BAML collectors: the SERVE profile installs both the dynamic unary
	// serve (nativeserve.New -> NewMetrics) and this static serve on the SAME worker
	// registry, so a plain NewMetrics here would fail with "duplicate metrics collector
	// registration attempted" and crash the worker before its plugin handshake. Reuse
	// shares one collector set so both write the SAME series; on a fresh registry it
	// behaves exactly like NewMetrics.
	m, err := admission.NewMetricsReusing(reg)
	if err != nil {
		return nil, err
	}
	s := NewServer(m, llmhttp.NewExactExecutor(nil))
	return s.ServeStatic, nil
}

// ServeStatic is the bamlutils.NativeStaticServeFunc. It runs static admission
// (keeping the request-scoped nanollm client alive as a StaticClaim, with the S4
// plan compare as a PRE-SOCKET precondition inside AdmitStaticClaim), then — on a
// full plan match — CLAIMS the attempt and performs exactly one native RoundTrip via
// execute.RunAttempt, maps the outcome, and runs the S5 same-response BAML-parse
// safety compare. Before the claim it may DECLINE (no socket); from the claim onward
// it only SUCCEEDS or FAILS.
func (s *Server) ServeStatic(ctx context.Context, inv bamlutils.NativeStaticInvocation) (result bamlutils.NativeStaticServeResult) {
	claimed := false
	defer func() {
		if r := recover(); r != nil {
			if claimed {
				// Post-claim panic: a socket may have opened. FAIL — a decline here
				// would trigger a hidden BAML resend for the same call. Bounded
				// internal_error; the once-only socket defer still counts the socket.
				s.metrics.RecordServeOutcome(admission.ModeCall, inv.Provider, admission.OutcomeInternalError)
				result = failStaticResult(errNativeServePanic, "")
			} else {
				// Pre-claim panic: no socket occurred, so declining to BAML is safe.
				result = declineStaticResult(stageServe, reasonServedBAMLPanic)
			}
		}
	}()

	// Cancellation gate at ENTRY, BEFORE any native FFI: an already-cancelled request
	// declines to BAML with zero native work, and the ordinary BAML call then fails
	// once with the same context error.
	if ctx.Err() != nil {
		return declineStaticResult(stageServe, reasonServedBAMLCtx)
	}

	claim, err := s.staticAdmitClaim(ctx, toStaticAdmissionInput(inv))
	if err != nil {
		var d *admission.StaticDecline
		if errors.As(err, &d) {
			return declineStaticResult(d.Stage, d.Reason)
		}
		// Unexpected native planner/FFI error before any socket: availability-first
		// decline to BAML.
		return declineStaticResult(stagePlanner, reasonPlannerError)
	}
	// The claim keeps the request-scoped engine alive so TranslateResponse runs on
	// the SAME client Prepare produced the plan on. Close on EVERY path.
	defer claim.Close()

	// A provably PRE-SOCKET preflight rejection (unsigned/never-expiring OpenAI plans
	// never hit this) opens NO socket, so decline rather than claim and fail.
	if claim.PlanExpired() {
		return declineStaticResult(stageServe, reasonPlanExpired)
	}
	// ctx check immediately BEFORE the claim/FFI socket: an already-cancelled caller
	// declines safely (no socket) rather than claiming and failing.
	if ctx.Err() != nil {
		return declineStaticResult(stageServe, reasonServedBAMLCtx)
	}

	// CLAIM the native attempt (ownership boundary). From here every terminal
	// condition is SUCCESS or FAILURE — never a decline, never a hidden resend.
	claimed = true

	// native_sockets_total is recorded EXACTLY ONCE per claimed attempt (mirroring
	// the dynamic path): a post-claim once-only defer records the conservative
	// transport-error outcome, superseded by the definitive outcome below.
	socketRecorded := false
	recordSocket := func(outcome admission.NativeSocketOutcome) {
		if socketRecorded {
			return
		}
		socketRecorded = true
		s.metrics.RecordNativeSocket(outcome)
	}
	defer recordSocket(admission.NativeSocketTransportError)

	// The static SAP parser is a SCHEMA-NEUTRAL closure capturing the selected Return
	// Bundle: execute never learns about Bundles, and internal/debaml owns the parse.
	bundle := claim.Bundle
	res, aerr := execute.RunAttempt(ctx, execute.AttemptConfig{
		Client:   claim.Client(),
		Prepared: claim.Prepared,
		Executor: s.exec,
		ParseResponse: func(pctx context.Context, raw string) ([]byte, error) {
			parsed, perr := debaml.ParseStaticBundle(pctx, bundle, raw)
			if perr != nil {
				return nil, perr
			}
			return parsed.JSON, nil
		},
		IncludeReasoning: inv.IncludeReasoning,
		// The merged first-2xx heartbeat: fired before the body is buffered so the
		// pool hung-detector sees liveness on a slow body.
		SendHeartbeat: inv.SendHeartbeat,
	})

	// Definitive socket outcome: nil result → no HTTP response (transport failure);
	// non-nil → the socket returned a response (any status).
	if res == nil {
		recordSocket(admission.NativeSocketTransportError)
	} else {
		recordSocket(admission.NativeSocketResponded)
	}

	return s.mapStaticAttempt(ctx, inv, bundle, res, aerr)
}

// mapStaticAttempt maps one claimed native static attempt's (result, error) onto the
// neutral static serve result. It NEVER declines (post-claim) and NEVER falls
// through to a BAML resend — mirroring mapAttempt.
func (s *Server) mapStaticAttempt(ctx context.Context, inv bamlutils.NativeStaticInvocation, bundle *schema.Bundle, res *execute.AttemptResult, aerr error) bamlutils.NativeStaticServeResult {
	if aerr != nil {
		if isContextErr(aerr) {
			s.metrics.RecordServeOutcome(admission.ModeCall, inv.Provider, admission.OutcomeTransportError)
			return failStaticResult(aerr, "")
		}
		if res == nil {
			s.metrics.RecordServeOutcome(admission.ModeCall, inv.Provider, admission.OutcomeTransportError)
			return failStaticResult(aerr, "")
		}
		if res.SAPInvoked {
			// CLAIMED native SAP parse failure -> ErrOutputParse + the extracted
			// assistant text as details.raw (mirrors BAML's parse_error envelope).
			s.metrics.RecordServeOutcome(admission.ModeCall, inv.Provider, admission.OutcomeParseError)
			return failStaticResult(&buildrequest.OutputParseError{Err: aerr}, res.Raw)
		}
		// Translate / non-JSON-2xx / assistant-extraction failure -> today's extraction
		// error class with the raw upstream body retained as details.raw.
		s.metrics.RecordServeOutcome(admission.ModeCall, inv.Provider, admission.OutcomeTranslateError)
		return failStaticResult(fmt.Errorf("buildrequest: failed to extract response content: %w", aerr), string(res.ProviderBody))
	}

	switch res.Outcome {
	case execute.OutcomeProviderError:
		s.metrics.RecordServeOutcome(admission.ModeCall, inv.Provider, admission.OutcomeProviderError)
		return failStaticResult(&llmhttp.HTTPError{
			StatusCode: res.ProviderStatus,
			Body:       capErrorBody(res.ProviderBody),
		}, "")
	case execute.OutcomeInvalidBody:
		s.metrics.RecordServeOutcome(admission.ModeCall, inv.Provider, admission.OutcomeTranslateError)
		return failStaticResult(errMalformed2xx, string(res.ProviderBody))
	case execute.OutcomeParseDeclined:
		return s.serveStaticParseOnly(ctx, inv, res)
	case execute.OutcomeStructured:
		return s.serveStaticStructured(ctx, inv, bundle, res)
	default:
		s.metrics.RecordServeOutcome(admission.ModeCall, inv.Provider, admission.OutcomeParseError)
		return failStaticResult(fmt.Errorf("nativeserve/canary: unexpected static attempt outcome %v", res.Outcome), "")
	}
}

// serveStaticStructured handles a clean native static structured claim: it runs the
// S5 same-response BAML `Parse.<Method>` safety compare over the SAME bytes and
// records response_compare per facet. On a structured/order MATCH it serves the
// native flattened JSON; on drift it serves the BAML parse of the same bytes (still
// one native provider request). If BAML's extraction/parse ERRORS on those bytes,
// compatibility wins — the corresponding error is returned (never a native final
// BAML would have rejected).
func (s *Server) serveStaticStructured(ctx context.Context, inv bamlutils.NativeStaticInvocation, bundle *schema.Bundle, res *execute.AttemptResult) bamlutils.NativeStaticServeResult {
	if inv.BAMLOnlyParse == nil {
		s.metrics.RecordResponseCompare(admission.ResponseCompareMismatch, admission.ResponseCompareFieldError)
		s.metrics.RecordServeOutcome(admission.ModeCall, inv.Provider, admission.OutcomeParseError)
		return failStaticResult(&buildrequest.OutputParseError{Err: errNoBAMLOnlyParse}, res.Raw)
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return s.ctxStaticTransportFail(inv, ctxErr)
	}
	// S5 same-response BAML Parse over the SAME assistant text native parsed:
	// res.AssistantText — the text-only channel ConsumeResponse extracted from the
	// OpenAI-TRANSLATED body — NOT a re-extraction from the pre-translation
	// res.ProviderBody. ProviderBody is the provider-native wire shape; for a TRUSTED
	// (non-OpenAI) provider an "openai" re-extraction of it would yield text native
	// never saw, feeding BAML the wrong input and corrupting the structured/order
	// compare + the drift-fallback served result. Mirrors serveStaticParseOnly.
	bamlStructured, berr := inv.BAMLOnlyParse(ctx, res.AssistantText)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return s.ctxStaticTransportFail(inv, ctxErr)
	}
	if berr != nil {
		if isContextErr(berr) {
			return s.ctxStaticTransportFail(inv, berr)
		}
		s.metrics.RecordResponseCompare(admission.ResponseCompareMismatch, admission.ResponseCompareFieldError)
		s.metrics.RecordServeOutcome(admission.ModeCall, inv.Provider, admission.OutcomeParseError)
		return failStaticResult(&buildrequest.OutputParseError{Err: berr}, res.Raw)
	}
	s.metrics.RecordResponseCompare(admission.ResponseCompareMatch, admission.ResponseCompareFieldError)

	// Per-facet response parity. In the SERVE path native is the SOLE extractor, so
	// the assistant/raw/reasoning channels are native-owned (res.AssistantText/Raw/
	// Reasoning, all from the same translated body) and served as-is — there is no
	// independent BAML extraction to diverge from — and translate is always OK on
	// OutcomeStructured. The load-bearing safety compare is structured/order below
	// (native's flattened structured vs BAML's parse of that same assistant text).
	s.recordResponse(true, admission.ResponseCompareFieldTranslate)
	s.recordResponse(true, admission.ResponseCompareFieldAssistant)
	s.recordResponse(true, admission.ResponseCompareFieldRaw)
	s.recordResponse(true, admission.ResponseCompareFieldReasoning)
	structuredMatch, orderMatch := parity.CompareStaticStructured(res.Structured, bamlStructured, bundle)
	s.recordResponse(structuredMatch, admission.ResponseCompareFieldStructured)
	s.recordResponse(orderMatch, admission.ResponseCompareFieldOrder)

	if structuredMatch && orderMatch {
		// MATCH -> serve the native flattened JSON with native-owned raw/reasoning.
		s.metrics.RecordServeOutcome(admission.ModeCall, inv.Provider, admission.OutcomeSuccess)
		return successStaticResult(res.Structured, res, bamlutils.NativeStaticServeEngineNative)
	}
	// Structured/order drift -> serve the BAML parse of the SAME bytes for safety
	// (still one native provider request). winner_engine records BAML produced it.
	s.metrics.RecordFallback(admission.FallbackParseOnly)
	s.metrics.RecordServeOutcome(admission.ModeCall, inv.Provider, admission.OutcomeSuccess)
	return successStaticResult(bamlStructured, res, bamlutils.NativeStaticServeEngineBAMLParse)
}

// serveStaticParseOnly handles a native SAP decline (OutcomeParseDeclined): native
// transported and translated cleanly but declined the parse shape (e.g. a bare
// primitive string return native never claims as JSON), so BAML `Parse.<Method>`
// runs on the SAME extracted assistant text and serves that final. One native
// provider request, zero BAML provider requests.
func (s *Server) serveStaticParseOnly(ctx context.Context, inv bamlutils.NativeStaticInvocation, res *execute.AttemptResult) bamlutils.NativeStaticServeResult {
	if inv.BAMLOnlyParse == nil {
		s.metrics.RecordResponseCompare(admission.ResponseCompareMismatch, admission.ResponseCompareFieldError)
		s.metrics.RecordServeOutcome(admission.ModeCall, inv.Provider, admission.OutcomeParseError)
		return failStaticResult(&buildrequest.OutputParseError{Err: errNoBAMLOnlyParse}, res.Raw)
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return s.ctxStaticTransportFail(inv, ctxErr)
	}
	bamlStructured, berr := inv.BAMLOnlyParse(ctx, res.AssistantText)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return s.ctxStaticTransportFail(inv, ctxErr)
	}
	if berr != nil {
		if isContextErr(berr) {
			return s.ctxStaticTransportFail(inv, berr)
		}
		s.metrics.RecordServeOutcome(admission.ModeCall, inv.Provider, admission.OutcomeParseError)
		return failStaticResult(&buildrequest.OutputParseError{Err: berr}, res.Raw)
	}
	// native declined where BAML parsed -> a real structured/order divergence.
	s.recordResponse(true, admission.ResponseCompareFieldTranslate)
	s.recordResponse(false, admission.ResponseCompareFieldStructured)
	s.recordResponse(false, admission.ResponseCompareFieldOrder)
	s.metrics.RecordFallback(admission.FallbackParseOnly)
	s.metrics.RecordServeOutcome(admission.ModeCall, inv.Provider, admission.OutcomeParseDecline)
	return successStaticResult(bamlStructured, res, bamlutils.NativeStaticServeEngineBAMLParse)
}

// ctxStaticTransportFail records a transport_error serve outcome and returns the
// context error UNCHANGED (no OutputParseError wrap, no details.raw) so
// errors.Is(context.Canceled/DeadlineExceeded) holds for the outer policy.
func (s *Server) ctxStaticTransportFail(inv bamlutils.NativeStaticInvocation, ctxErr error) bamlutils.NativeStaticServeResult {
	s.metrics.RecordServeOutcome(admission.ModeCall, inv.Provider, admission.OutcomeTransportError)
	return failStaticResult(ctxErr, "")
}

func declineStaticResult(stage, reason string) bamlutils.NativeStaticServeResult {
	return bamlutils.NativeStaticServeResult{
		Disposition: bamlutils.NativeStaticServeDeclined,
		Stage:       stage,
		Reason:      reason,
	}
}

func failStaticResult(err error, raw string) bamlutils.NativeStaticServeResult {
	return bamlutils.NativeStaticServeResult{
		Disposition:   bamlutils.NativeStaticServeFailed,
		Err:           err,
		RawDiagnostic: raw,
	}
}

// successStaticResult wraps a served static final: finalJSON is the winning
// flattened canonical JSON (the generated /call seam decodes it into the concrete
// return type via DecodeNativeStaticFinal), raw/reasoning are the native-owned
// /call-with-raw channels, and engine is the bounded winner-engine token.
func successStaticResult(finalJSON []byte, res *execute.AttemptResult, engine string) bamlutils.NativeStaticServeResult {
	return bamlutils.NativeStaticServeResult{
		Disposition:  bamlutils.NativeStaticServeSucceeded,
		FinalJSON:    finalJSON,
		Raw:          res.Raw,
		Reasoning:    res.Reasoning,
		WinnerEngine: engine,
	}
}

// toStaticAdmissionInput maps the neutral static invocation into the S3 static
// admission StaticInput. The serve implementation only runs on a native-capable
// worker with the umbrella flag on, on the static BuildRequest route, so those
// layer-1 facts are fixed true; the TRUTHFUL selected-child facts, the
// WouldRewriteOrProxy predicate, and provider/descriptor/args/mode come from the
// invocation. It mirrors nativeserve.NewStaticObserve's mapping exactly, but targets
// the SERVING claim rather than the observe-only path.
func toStaticAdmissionInput(inv bamlutils.NativeStaticInvocation) admission.StaticInput {
	return admission.StaticInput{
		WorkerCapable:           true,
		RequestAPIPresent:       true,
		OnBuildRequestRoute:     true,
		FlagEnabled:             true,
		RouteKind:               admission.RouteKindStatic,
		Method:                  inv.Method,
		Descriptor:              inv.Descriptor,
		Args:                    inv.Args,
		ArgOrder:                inv.ArgOrder,
		Mode:                    inv.Mode,
		SingleLeaf:              inv.SingleLeaf,
		HasFallbackChain:        inv.HasFallbackChain,
		HasRoundRobin:           inv.HasRoundRobin,
		HasRequestRetryOverride: inv.HasRequestRetryOverride,
		Raw:                     inv.Raw,
		ClientOverride:          inv.ClientOverride,
		Provider:                inv.Provider,
		WouldRewriteOrProxy:     inv.WouldRewriteOrProxy,
		BuildBAMLRequest:        inv.BuildBAMLRequest,
	}
}
