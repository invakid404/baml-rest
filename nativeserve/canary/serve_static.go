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

	"github.com/prometheus/client_golang/prometheus"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/internal/debaml"
	"github.com/invakid404/baml-rest/internal/schema"
	"github.com/invakid404/baml-rest/nativeserve/admission"
	"github.com/invakid404/baml-rest/nativeserve/execute"
	"github.com/invakid404/baml-rest/nativeserve/staticoracle"
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
	// Serving-cutover S1 identity — the static unary twin of Serve's: the surface is
	// this LANE's constant and the cohort is what the default-deny gate resolves.
	// Registered FIRST so it runs LAST and observes the final named result (including
	// the panic guard's substitution), recording exactly one phase+winner pair.
	surface, cohort := admission.SurfaceStaticCall, admission.ResolveCohort(admission.SurfaceStaticCall, s.staticCohortInput(inv))
	defer func() { s.recordStaticServeTerminal(surface, cohort, claimed, result.Disposition, result.WinnerEngine) }()
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

	claim, err := s.staticAdmitClaim(ctx, s.toStaticAdmissionInput(inv))
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
	// Recorded at the TRUE claim boundary (after the plan-expiry / ctx pre-socket
	// gates above), so claimed == native_sockets exactly.
	s.metrics.RecordAdmissionPhase(surface, cohort, admission.PhaseClaimed)

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
			// De-BAML Slice 7.2b-3: the STATIC UNARY /call route — the ONE route the
			// scope admits the checked-static fingerprint on — so it calls the
			// capability-carrying entry point rather than the bare one. For every other
			// bundle ParseStaticBundleUnaryCall IS ParseStaticBundle, so nothing else
			// moves; for the fingerprint it is the difference between serving the
			// Checked[T] carrier / stock's assertion error and declining. The shadow
			// comparator next door deliberately keeps the DIRECT entry point.
			parsed, perr := debaml.ParseStaticBundleUnaryCall(pctx, bundle, raw)
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
// through to a BAML resend — mirroring mapAttempt. The post-response decision (the
// same-bytes BAML oracle, structured/order compare, drift/parse-decline fallback, and
// terminal error classification) is the shared, transport-free
// [staticoracle.Resolve]; this method replays the returned bounded facet observations
// into the exact metrics the static serve path recorded inline before the extraction.
func (s *Server) mapStaticAttempt(ctx context.Context, inv bamlutils.NativeStaticInvocation, bundle *schema.Bundle, res *execute.AttemptResult, aerr error) bamlutils.NativeStaticServeResult {
	r := staticoracle.Resolve(ctx, bundle, res, aerr, inv.BAMLOnlyParse)
	s.recordStaticOracleMetrics(inv, r)
	if r.Served {
		return bamlutils.NativeStaticServeResult{
			Disposition:  bamlutils.NativeStaticServeSucceeded,
			FinalJSON:    r.FinalJSON,
			Raw:          r.Raw,
			Reasoning:    r.Reasoning,
			WinnerEngine: r.Winner,
		}
	}
	return failStaticResult(r.Err, r.RawDiagnostic)
}

// recordStaticOracleMetrics replays the resolver's bounded facet observations into the
// exact static-serve metrics the inline mapStaticAttempt/serveStaticStructured/
// serveStaticParseOnly recorded before the same-bytes oracle was factored out. The
// resolver records nothing itself; the canary path owns its own phase/winner/facet
// series (the spine composite records its own separate bounded set).
func (s *Server) recordStaticOracleMetrics(inv bamlutils.NativeStaticInvocation, r staticoracle.Result) {
	if r.SameResponseOracleRan {
		// The strict same-response oracle ran: BAML's `Parse.<Method>` over the SAME bytes
		// the ONE native provider request returned. Its own phase, so a BAML parse win is
		// never conflated with a BAML transport win.
		cohort := admission.ResolveCohort(admission.SurfaceStaticCall, s.staticCohortInput(inv))
		s.metrics.RecordAdmissionPhase(admission.SurfaceStaticCall, cohort, admission.PhaseSameResponseOracle)
	}
	if r.ErrorCompareRecorded {
		cmp := admission.ResponseCompareMismatch
		if r.ErrorCompareMatch {
			cmp = admission.ResponseCompareMatch
		}
		s.metrics.RecordResponseCompare(cmp, admission.ResponseCompareFieldError)
	}
	if r.StructuredBranchServed {
		// Per-facet response parity. Native is the SOLE extractor, so the
		// assistant/raw/reasoning channels are native-owned and translate is always OK on
		// a structured claim; the load-bearing safety compare is structured/order.
		s.recordResponse(true, admission.ResponseCompareFieldTranslate)
		s.recordResponse(true, admission.ResponseCompareFieldAssistant)
		s.recordResponse(true, admission.ResponseCompareFieldRaw)
		s.recordResponse(true, admission.ResponseCompareFieldReasoning)
		s.recordResponse(r.StructuredMatch, admission.ResponseCompareFieldStructured)
		s.recordResponse(r.OrderMatch, admission.ResponseCompareFieldOrder)
	}
	if r.ParseDeclineServed {
		// native declined where BAML parsed -> a real structured/order divergence.
		s.recordResponse(true, admission.ResponseCompareFieldTranslate)
		s.recordResponse(false, admission.ResponseCompareFieldStructured)
		s.recordResponse(false, admission.ResponseCompareFieldOrder)
	}
	if r.Fallback {
		s.metrics.RecordFallback(admission.FallbackParseOnly)
	}
	s.metrics.RecordServeOutcome(admission.ModeCall, inv.Provider, mapStaticOracleOutcome(r.Outcome))
}

// mapStaticOracleOutcome maps the neutral resolver outcome onto the admission serve
// outcome the static path records.
func mapStaticOracleOutcome(o staticoracle.Outcome) admission.Outcome {
	switch o {
	case staticoracle.OutcomeSuccess:
		return admission.OutcomeSuccess
	case staticoracle.OutcomeParseDecline:
		return admission.OutcomeParseDecline
	case staticoracle.OutcomeTranslateError:
		return admission.OutcomeTranslateError
	case staticoracle.OutcomeProviderError:
		return admission.OutcomeProviderError
	case staticoracle.OutcomeTransportError:
		return admission.OutcomeTransportError
	default:
		return admission.OutcomeParseError
	}
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

// toStaticAdmissionInput maps the neutral static invocation into the S3 static
// admission StaticInput. The serve implementation only runs on a native-capable
// worker with the umbrella flag on, on the static BuildRequest route, so those
// layer-1 facts are fixed true; the TRUTHFUL selected-child facts, the
// WouldRewriteOrProxy predicate, and provider/descriptor/args/mode come from the
// invocation. It mirrors nativeserve.NewStaticObserve's mapping exactly, but targets
// the SERVING claim rather than the observe-only path.
func (s *Server) toStaticAdmissionInput(inv bamlutils.NativeStaticInvocation) admission.StaticInput {
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
		Values:                  inv.Values,
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
		Cohort:                  s.staticCohortInput(inv),
	}
}

// staticCohortInput is the SINGLE definition of a static invocation's
// serving-cutover configuration identity — the static twin of serveCohortInput, and
// it resolves the SAME way, from the SAME resolver, over the SAME trusted seal.
//
// # Why it resolves an identity on a surface nothing enrolls
//
// S3a left this returning the zero identity, on the argument that resolving an
// identity for a surface no policy will enroll widens what carries an identity
// without widening what may claim. The first half of that is true and the second
// half is what makes it safe — but it costs ATTRIBUTION, and attribution is what a
// rollout is read from. With no identity, every static decline is the generic
// "presented no configuration identity" one, so an operator (and a proof) cannot
// tell "the deployment's approved configuration reached the static surface and the
// gate refused it there" from "nothing identifiable ever arrived".
//
// So the identity is resolved here too, and it is resolved from exactly one fact
// the request cannot manufacture: the TRUSTED-CONFIGURATION SEAL the worker's config
// load applied to a client the DEPLOYMENT configured. This CANNOT widen what may
// claim, and the reason is structural rather than a promise: an inventory record
// declares the surfaces its class is approved for, and CohortGate.Resolve returns
// that record's cohort ONLY on those surfaces. The shipped fe-v1 record declares
// dynamic_call and nothing else, so a sealed fe-v1 configuration arriving here
// resolves the reserved, NON-ENROLLABLE `unrecognized` bucket — a bucket
// CohortPolicy refuses to enroll at construction — and still declines at the cohort
// stage before any native work. What changes is only which of the three refusal
// shapes the metric records, which is the point.
//
// s.cohort remains the one exception and is not a production path: the gated proof
// suites build their server through the `nanollm_integration`-tagged constructor
// with a fixed enrolled identity. Every untagged factory leaves it zero.
func (s *Server) staticCohortInput(inv bamlutils.NativeStaticInvocation) admission.CohortInput {
	if s.cohort.Assigned() {
		return s.cohort
	}
	id := admission.ResolveConfigIdentity(admission.ConfigSelection{
		Registry:         inv.Registry,
		ResolvedProvider: inv.Provider,
		SelectedLeaf:     inv.ClientOverride,
		SingleLeaf:       inv.SingleLeaf,
		HasFallbackChain: inv.HasFallbackChain,
		HasRoundRobin:    inv.HasRoundRobin,
		// The same truthful narrowing conditions the dynamic seam threads: a retry
		// override means the effective selected leaf is not one proven answer, and a
		// seam carrying no BAML no-send plan builder could never satisfy the strict
		// plan equality an enrolled class requires, so it is never given an identity.
		HasRequestRetryOverride: inv.HasRequestRetryOverride,
		HasBAMLPlanOracle:       inv.BuildBAMLRequest != nil,
	})
	return admission.CohortInput{Fingerprint: id.Fingerprint, Provider: id.Provider}
}

// recordStaticServeTerminal is the static twin of recordServeTerminal: exactly one
// phase+winner pair per finished static request, with the same fail-closed mapping
// (a post-claim decline or an unrecognized winner-engine token records failure, not
// a safe decline and not a native win).
func (s *Server) recordStaticServeTerminal(surface admission.Surface, cohort admission.CohortID, claimed bool, disposition bamlutils.NativeStaticServeDisposition, engine string) {
	if !claimed {
		s.metrics.RecordPreclaimDecline(surface, cohort)
		return
	}
	winner := admission.WinnerFailure
	if disposition == bamlutils.NativeStaticServeSucceeded {
		switch engine {
		case bamlutils.NativeStaticServeEngineNative:
			winner = admission.WinnerNative
		case bamlutils.NativeStaticServeEngineBAMLParse:
			winner = admission.WinnerBAMLParseSameResponse
		}
	}
	s.metrics.RecordPostclaimTerminal(surface, cohort, winner)
}
