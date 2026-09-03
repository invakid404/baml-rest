package spine

import (
	"context"
	"errors"
	"fmt"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/bamlutils/promptdescriptor"
	"github.com/invakid404/baml-rest/internal/debaml"
	"github.com/invakid404/baml-rest/nativeserve/admission"
	"github.com/invakid404/baml-rest/nativeserve/execute"
	"github.com/invakid404/baml-rest/nativeserve/staticoracle"
)

// errorBodyCap bounds the provider error body retained on an HTTPError.
const errorBodyCap = 64 << 10

// Call performs one unary final call for an admitted method and returns the neutral
// tri-state result. Ownership discipline (Phase-7): a pre-socket DECLINE guarantees
// zero RoundTrips and is fallback-legal; from the claim onward every terminal
// condition is SUCCEEDED or FAILED-AFTER-CLAIM — never a decline, never a resend. The
// point of no return is claimed=true, set immediately before the exact executor.
func (e *UnaryExecutor) Call(ctx context.Context, method string, input any) (result bamlutils.NativeSpineUnaryResult) {
	claimed := false
	defer func() {
		if r := recover(); r != nil {
			// The recovered value is DROPPED, never interpolated: only the bounded sentinel
			// + stage/reason leave here.
			_ = r
			if claimed {
				// Post-claim panic: a socket may have opened. FAIL — a decline here would
				// invite a hidden fallback resend for the same call.
				result = bamlutils.FailedAfterClaimSpineResult(errPanicAfterClaim, stageServe, reasonPanic)
				e.metrics.failures.Add(1)
			} else {
				// Pre-claim panic: no socket occurred, so declining is safe.
				result = bamlutils.DeclinedSpineResult(errPanicBeforeClaim, stageServe, reasonPanic)
				e.metrics.declines.Add(1)
			}
		}
	}()

	rm, ok := e.registry[method]
	if !ok {
		return e.declined(&bamlutils.NativeSpineUnsupportedMethodError{Method: method, Reason: reasonUnsupportedMethod}, stageRegistry, reasonUnsupportedMethod)
	}
	// Cancellation BEFORE the claim declines with zero sockets (fallback-legal), and
	// the ordinary outer policy then fails once with the same context error.
	if err := ctx.Err(); err != nil {
		return e.declined(err, stagePreflight, reasonContextCancelled)
	}

	// Read the REQUEST-SCOPED routing/orchestration facts off the adapter the worker
	// configured (the emitted BuildMethod passes the adapter as this Call's context)
	// and DECLINE PRE-SOCKET when any cohort-forbidden fact is present — matching the
	// generated-static lane's admission boundary in lockstep (Codex review finding 1).
	// The exact cohort serves only the descriptor's default client with no request
	// override, so a caller-supplied client_registry or dynamic output schema declines
	// here; retry/round-robin/rewrite-proxy decline inside admission (see staticInput).
	ad, _ := ctx.(bamlutils.Adapter)
	if ad != nil {
		if ad.OriginalClientRegistry() != nil {
			return e.declined(errClientRegistry, stageAdmission, reasonClientRegistry)
		}
		if ad.DeBAMLOutputSchema() != nil {
			return e.declined(errDynamicSchema, stageAdmission, reasonDynamicSchema)
		}
	}

	values, perr := rm.binding.ProjectInput(input)
	if perr != nil {
		return e.declined(perr, stageProject, reasonProjectInputErr)
	}

	claim, aerr := e.admitClaim(ctx, e.staticInput(rm, values, ad))
	if aerr != nil {
		var d *admission.StaticDecline
		if errors.As(aerr, &d) {
			// A typed pre-socket admission decline: zero sockets, fallback-legal.
			return e.declined(d, d.Stage, d.Reason)
		}
		// An unexpected planner/FFI error before any socket: availability-first decline.
		return e.declined(aerr, stagePlanner, reasonPlannerError)
	}
	// The claim keeps the request-scoped engine alive for exactly one RoundTrip and one
	// TranslateResponse. Close on EVERY path.
	defer claim.Close()

	// A provably PRE-SOCKET preflight rejection (unsigned OpenAI plans never hit this)
	// opens no socket, so decline rather than claim and fail.
	if claim.PlanExpired() {
		return e.declined(errPlanExpired, stagePreflight, reasonPlanExpired)
	}
	// ctx check immediately BEFORE the claim/FFI socket: an already-cancelled caller
	// declines safely (no socket) rather than claiming and failing.
	if err := ctx.Err(); err != nil {
		return e.declined(err, stagePreflight, reasonContextCancelled)
	}

	// CLAIM the native attempt (ownership boundary). From here every terminal condition
	// is SUCCEEDED or FAILED-AFTER-CLAIM.
	claimed = true
	e.metrics.claims.Add(1)
	e.metrics.sockets.Add(1)

	res, rerr := e.runClaimedAttempt(ctx, claim, false, nil)
	return e.mapAttempt(rm, res, rerr)
}

// CallWithOracle performs one unary final call for an admitted method through the
// ExecBridge-U1c LIVE-oracle composite: a live BAML `Request.<Method>` no-send
// plan-compare admission before the claim, exactly one provider RoundTrip, and the
// same-bytes BAML `Parse.<Method>` safety oracle over that ONE response — returning
// canonical JSON for the standard generated decoder. It shares the SAME immutable
// registry, exact transport, claim boundary, panic discipline, and atomic counters as
// [UnaryExecutor.Call]; they differ ONLY in policy (live-vs-frozen oracle claim and
// carrier-vs-canonical-JSON output). The native-only Call stays frozen + terminal and
// never learns about this method.
//
// Ownership discipline is identical: a pre-socket DECLINE certifies zero RoundTrips and
// is fallback-legal (the outer composite returns it to the BAML orchestrator, which
// serves the same call); from the claim onward every terminal condition is SUCCEEDED or
// FAILED-AFTER-CLAIM. The panic guard tracks the same local `claimed` boolean: a panic
// before the claim (in BuildBAMLRequest during admission, say) is a decline; a panic
// after it (response translation, native parse, BAMLOnlyParse, compare) is terminal.
func (e *UnaryExecutor) CallWithOracle(ctx context.Context, inv bamlutils.NativeStaticInvocation) (result bamlutils.NativeSpineUnaryOracleResult) {
	claimed := false
	// obs accumulates the bounded metric observations INCREMENTALLY, so a post-claim panic
	// still carries out what happened up to the panic (the socket opened, the plan matched,
	// the same-response oracle was entered). The panic guard re-attaches it below.
	var obs bamlutils.NativeSpineUnaryOracleObservations
	defer func() {
		if r := recover(); r != nil {
			// The recovered value is DROPPED, never interpolated: the standard adapter
			// propagates this terminal error to the outer policy, so an arbitrary/sensitive
			// panic payload must not escape. Only the bounded sentinel + stage/reason leave.
			_ = r
			if claimed {
				// Post-claim panic: a socket may have opened. FAIL — a decline here would
				// invite a hidden BAML resend for the same call.
				result = bamlutils.FailedAfterClaimOracleResult(errPanicAfterClaim, stageServe, reasonPanic, "")
				e.metrics.failures.Add(1)
			} else {
				// Pre-claim panic: no socket occurred, so declining is safe.
				result = bamlutils.DeclinedOracleResult(errPanicBeforeClaim, stageServe, reasonPanic)
				e.metrics.declines.Add(1)
			}
			result.Observations = obs
		}
	}()

	rm, ok := e.registry[inv.Method]
	if !ok {
		return e.declinedOracle(&bamlutils.NativeSpineUnsupportedMethodError{Method: inv.Method, Reason: reasonUnsupportedMethod}, stageRegistry, reasonUnsupportedMethod)
	}
	// Cancellation BEFORE the claim declines with zero sockets (fallback-legal).
	if err := ctx.Err(); err != nil {
		return e.declinedOracle(err, stagePreflight, reasonContextCancelled)
	}
	// Request-scoped exact-cohort declines, read from the TRUTHFUL invocation facts the
	// generated static seam populated — never re-derived here (§12 "two classifiers").
	// The exact cohort serves only the descriptor's default client against the static
	// schema, so a request client_registry or a dynamic output schema declines pre-socket.
	if inv.HasClientRegistryOverride {
		return e.declinedOracle(errClientRegistry, stageAdmission, reasonClientRegistry)
	}
	if inv.HasDynamicOutputSchema {
		return e.declinedOracle(errDynamicSchema, stageAdmission, reasonDynamicSchema)
	}
	// Both BAML oracle callbacks MUST be present BEFORE the claim. A missing no-send
	// plan builder or same-bytes parser discovered after the provider response is safe
	// from double-send but violates the default-on safety promise, so decline pre-claim
	// (§4). Production codegen always supplies both; this bites only a direct caller.
	if inv.BuildBAMLRequest == nil {
		return e.declinedOracle(errNoBAMLPlan, stageAdmission, reasonNoBAMLPlan)
	}
	if inv.BAMLOnlyParse == nil {
		return e.declinedOracle(errNoBAMLParse, stageAdmission, reasonNoBAMLParse)
	}

	claim, aerr := e.admitClaimOracle(ctx, e.oracleStaticInput(rm, inv))
	if aerr != nil {
		var d *admission.StaticDecline
		if errors.As(aerr, &d) {
			// A typed pre-socket admission decline (plan mismatch, totality miss, rewrite):
			// zero sockets, fallback-legal. A plan-MISMATCH decline (Stage == plan_compare)
			// carries plan-compare evidence out so the composite records the mismatch.
			out := e.declinedOracle(d, d.Stage, d.Reason)
			if d.Stage == string(admission.StagePlanCompare) {
				obs.PlanCompareRan = true
			}
			out.Observations = obs
			return out
		}
		// An unexpected planner/FFI error before any socket: availability-first decline.
		out := e.declinedOracle(aerr, stagePlanner, reasonPlannerError)
		out.Observations = obs
		return out
	}
	defer claim.Close()

	// PRE-SOCKET preflight rejections (unsigned OpenAI plans never hit plan-expiry) open
	// no socket, so decline rather than claim and fail.
	if claim.PlanExpired() {
		out := e.declinedOracle(errPlanExpired, stagePreflight, reasonPlanExpired)
		out.Observations = obs
		return out
	}
	if err := ctx.Err(); err != nil {
		out := e.declinedOracle(err, stagePreflight, reasonContextCancelled)
		out.Observations = obs
		return out
	}

	// CLAIM the native attempt (ownership boundary). From here every terminal condition
	// is SUCCEEDED or FAILED-AFTER-CLAIM — never a decline, never a hidden resend. The plan
	// byte-matched (that is why we claimed), and exactly one socket is about to open.
	claimed = true
	e.metrics.claims.Add(1)
	e.metrics.sockets.Add(1)
	obs.PlanCompareRan = true
	obs.PlanMatched = true
	obs.SocketOpened = true

	res, rerr := e.runClaimedAttempt(ctx, claim, inv.IncludeReasoning, inv.SendHeartbeat)
	obs.SocketResponded = res != nil
	// The SHARED same-bytes oracle resolves the claimed response: on a structured/order
	// match native's canonical JSON wins; on drift or a native parse-decline the BAML
	// parse of the SAME bytes wins; every fault is terminal. It opens no socket. The
	// onStructuredOracle hook sets SameResponseOracleRan BEFORE the parser, so a parser
	// panic still carries the phase out.
	r := staticoracle.Resolve(ctx, claim.Bundle, res, rerr, inv.BAMLOnlyParse, func() { obs.SameResponseOracleRan = true })
	obs.ErrorCompareRecorded = r.ErrorCompareRecorded
	obs.ErrorCompareMatch = r.ErrorCompareMatch
	obs.StructuredBranchServed = r.StructuredBranchServed
	obs.StructuredMatch = r.StructuredMatch
	obs.OrderMatch = r.OrderMatch
	obs.ParseDeclineServed = r.ParseDeclineServed
	obs.Fallback = r.Fallback
	obs.ServeOutcome = mapStaticServeOutcome(r.Outcome)
	if r.Served {
		e.metrics.successes.Add(1)
		out := bamlutils.SucceededOracleResult(r.FinalJSON, r.Raw, r.Reasoning, r.Winner)
		out.Observations = obs
		return out
	}
	e.metrics.failures.Add(1)
	stage, reason := oracleFailStageReason(r.Outcome)
	out := bamlutils.FailedAfterClaimOracleResult(r.Err, stage, reason, r.RawDiagnostic)
	out.Observations = obs
	return out
}

// mapStaticServeOutcome maps the neutral resolver outcome onto the bounded serve-outcome
// the standard composite replays into RecordServeOutcome.
func mapStaticServeOutcome(o staticoracle.Outcome) bamlutils.NativeStaticServeOutcome {
	switch o {
	case staticoracle.OutcomeSuccess:
		return bamlutils.NativeStaticOutcomeSuccess
	case staticoracle.OutcomeParseDecline:
		return bamlutils.NativeStaticOutcomeParseDecline
	case staticoracle.OutcomeTranslateError:
		return bamlutils.NativeStaticOutcomeTranslateError
	case staticoracle.OutcomeProviderError:
		return bamlutils.NativeStaticOutcomeProviderError
	case staticoracle.OutcomeTransportError:
		return bamlutils.NativeStaticOutcomeTransportError
	default:
		return bamlutils.NativeStaticOutcomeParseError
	}
}

// runClaimedAttempt performs the ONE exact provider RoundTrip + native static SAP over
// the claimed plan, shared by Call and CallWithOracle. The SAP parser is a
// SCHEMA-NEUTRAL closure capturing the stored Return Bundle: execute never learns about
// Bundles, and internal/debaml owns the parse (debaml.ParseStaticBundleUnaryCall — the
// same predicate that gated registration and gates direct parse). No generated BAML, no
// CFFI.
func (e *UnaryExecutor) runClaimedAttempt(ctx context.Context, claim *admission.StaticClaim, includeReasoning bool, sendHeartbeat func()) (*execute.AttemptResult, error) {
	bundle := claim.Bundle
	return execute.RunAttempt(ctx, execute.AttemptConfig{
		Client:   claim.Client(),
		Prepared: claim.Prepared,
		Executor: e.exec,
		ParseResponse: func(pctx context.Context, rawText string) ([]byte, error) {
			parsed, perr := debaml.ParseStaticBundleUnaryCall(pctx, bundle, rawText)
			if perr != nil {
				return nil, perr
			}
			return parsed.JSON, nil
		},
		IncludeReasoning: includeReasoning,
		SendHeartbeat:    sendHeartbeat,
	})
}

// oracleFailStageReason maps a terminal same-bytes oracle outcome onto the executor's
// bounded stage/reason tokens for the failed-after-claim oracle result. The typed Err
// carries the real error class; these are secret-free observability tokens only.
func oracleFailStageReason(o staticoracle.Outcome) (stage, reason string) {
	switch o {
	case staticoracle.OutcomeProviderError:
		return stageProvider, reasonProviderError
	case staticoracle.OutcomeTranslateError:
		return stageTransport, reasonInvalidBody
	case staticoracle.OutcomeTransportError:
		return stageTransport, reasonTransportError
	case staticoracle.OutcomeParseError:
		return stageParse, reasonNativeParse
	default:
		return stageServe, reasonUnknownOutcome
	}
}

// mapAttempt maps one claimed native attempt's (result, error) onto the neutral
// tri-state result. It NEVER declines (post-claim) and NEVER falls through to a
// fallback/BAML resend.
func (e *UnaryExecutor) mapAttempt(rm *registeredMethod, res *execute.AttemptResult, aerr error) bamlutils.NativeSpineUnaryResult {
	if aerr != nil {
		// Transport / translate / extraction failure (res==nil or SAP not invoked), or a
		// claimed native SAP parse failure (SAPInvoked) — all terminal post-claim.
		stage, reason := stageTransport, reasonTransportError
		if res != nil && res.SAPInvoked {
			stage, reason = stageParse, reasonNativeParse
		}
		return e.failed(aerr, stage, reason)
	}
	switch res.Outcome {
	case execute.OutcomeStructured:
		final, derr := rm.binding.DecodeFinal(res.Structured)
		if derr != nil {
			return e.failed(derr, stageDecode, reasonDecodeError)
		}
		return e.succeeded(final)
	case execute.OutcomeParseDeclined:
		// The exact-JSON totality predicate proves the native final parser reaches a
		// carrier or a native TERMINAL parse error for every admitted response — a
		// support/value decline never survives the claim here. There is no BAML oracle on
		// this cohort, so a parse-decline is terminal (never a fallback).
		return e.failed(errParseDecline, stageParse, reasonParseDeclined)
	case execute.OutcomeProviderError:
		return e.failed(&llmhttp.HTTPError{StatusCode: res.ProviderStatus, Body: capBody(res.ProviderBody)}, stageProvider, reasonProviderError)
	case execute.OutcomeInvalidBody:
		return e.failed(errMalformed2xx, stageTransport, reasonInvalidBody)
	default:
		return e.failed(fmt.Errorf("nativespine: unexpected attempt outcome %v", res.Outcome), stageServe, reasonUnknownOutcome)
	}
}

// Parse is the local, SOCKET-FREE parse route: the same admitted binding's native
// final parser + emitted decoder over raw. A non-admitted method returns the typed
// capability-decline; malformed raw for an admitted method is an ordinary terminal
// parse error (never a capability decline). Keeping parse out of the call-only
// tri-state disposition avoids falsely implying a socket existed.
func (e *UnaryExecutor) Parse(ctx context.Context, method string, raw string) (any, error) {
	rm, ok := e.registry[method]
	if !ok {
		return nil, &bamlutils.NativeSpineUnsupportedMethodError{Method: method, Reason: reasonUnsupportedMethod}
	}
	// A caller-supplied dynamic output schema would change the parse target, but this
	// route parses against the descriptor's fixed cohort schema. Reject it here — as the
	// Call route does — so a non-cohort parse request FAILS rather than being silently
	// parsed under the wrong (cohort) schema. The emitted parse binding passes the
	// request adapter as ctx; a plain-context parse (no adapter) carries no override.
	if ad, ok := ctx.(bamlutils.Adapter); ok && ad != nil {
		if ad.DeBAMLOutputSchema() != nil {
			return nil, errDynamicSchema
		}
	}
	parsed, err := debaml.ParseStaticBundleUnaryCall(ctx, rm.bundle, raw)
	if err != nil {
		// Ordinary terminal parse error for an admitted method — NOT a capability decline.
		return nil, err
	}
	return rm.binding.DecodeFinal(parsed.JSON)
}

// baseStaticInput maps a registered method + projected values into the shared static
// admission StaticInput both spine lanes start from. The layer-1 facts are the lane's
// constants; the SPINE lane bypasses the dynamic-rollout cohort gate (its admission is
// its own registration-time totality gate). checkArgBinder (inside admission) proves the
// projected vector's names/order match the descriptor exactly.
//
// The native plan is DESCRIPTOR-driven: Descriptor is the BAKED reconstructed rm.fn (not
// the request's live descriptor), so the oracle lane can compare native(baked descriptor
// + live values) against BAML's live no-send plan and catch a deployment mutation that
// changed only BAML's plan.
//
// The rewrite/proxy gate must ALWAYS run, even on a PLAIN-context call with no
// adapter-configured client: the predicate defaults to the process-global
// llmhttp.DefaultClient (which knows the global rewrite/proxy rules) so admission can
// never SKIP the gate and let a global rewrite/proxy rule route a native send elsewhere
// (CodeRabbit #9). A caller OVERRIDES the default with its own effective client.
//
// HasRoundRobin / HasFallbackChain are a property of the SELECTED CLIENT's plan, not of
// request infrastructure. An admitted method's client is a proven single resolved leaf —
// registration declines a fallback / round-robin strategy client as an out-of-cohort
// miss and omits it — so both are false here.
func (e *UnaryExecutor) baseStaticInput(rm *registeredMethod, values []promptdescriptor.ArgumentValue) admission.StaticInput {
	args := make(map[string]any, len(values))
	order := make([]string, 0, len(values))
	for _, v := range values {
		args[v.Name] = v.Value
		order = append(order, v.Name)
	}
	return admission.StaticInput{
		WorkerCapable:       true,
		RequestAPIPresent:   true,
		OnBuildRequestRoute: true,
		FlagEnabled:         true,
		RouteKind:           admission.RouteKindStatic,
		Method:              rm.fn.Method,
		Descriptor:          rm.fn,
		Args:                args,
		ArgOrder:            order,
		Values:              values,
		Mode:                bamlutils.NativeStaticModeFinal,
		SingleLeaf:          true,
		Provider:            rm.fn.Provider,
		WouldRewriteOrProxy: llmhttp.DefaultClient.WouldRewriteOrProxy,
	}
}

// staticInput is the FROZEN-evidence native-only Call's admission input: the base plus
// the genuinely request-scoped facts read off the adapter (a per-request retry override
// and the effective send client's rewrite/proxy rule). A caller registry / dynamic
// schema already declined in Call before this point.
func (e *UnaryExecutor) staticInput(rm *registeredMethod, values []promptdescriptor.ArgumentValue, ad bamlutils.Adapter) admission.StaticInput {
	in := e.baseStaticInput(rm, values)
	if ad != nil {
		in.HasRequestRetryOverride = ad.RetryConfig() != nil
		if hc := ad.HTTPClient(); hc != nil {
			in.WouldRewriteOrProxy = hc.WouldRewriteOrProxy
		}
	}
	return in
}

// oracleStaticInput is the LIVE-oracle CallWithOracle's admission input. baseStaticInput
// contributes ONLY the intentionally-baked descriptor + projected Values (rm.fn +
// inv.Values); every OTHER admission fact is the request's TRUTHFUL selected-route fact,
// forwarded from the invocation the generated seam populated. Forwarding them (rather than
// synthesizing fixed final/single-leaf/no-override facts) is load-bearing: a request-scoped
// near-miss OUTSIDE the exact U1 population — call-with-raw, a non-default client override,
// a fallback / round-robin / retry strategy, or a non-openai resolved leaf — MUST decline
// PRE-SOCKET at the shared admission gates, never claim on a plan match. In particular
// /call-with-raw is explicitly outside U1, and the strategy gates are the barrier that
// keeps a post-claim failure out of the outer fallback loop. A client-registry /
// dynamic-schema request already declined in CallWithOracle before this point.
func (e *UnaryExecutor) oracleStaticInput(rm *registeredMethod, inv bamlutils.NativeStaticInvocation) admission.StaticInput {
	in := e.baseStaticInput(rm, inv.Values)
	in.Mode = inv.Mode
	in.Raw = inv.Raw
	in.Provider = inv.Provider
	in.ClientOverride = inv.ClientOverride
	in.SingleLeaf = inv.SingleLeaf
	in.HasFallbackChain = inv.HasFallbackChain
	in.HasRoundRobin = inv.HasRoundRobin
	in.HasRequestRetryOverride = inv.HasRequestRetryOverride
	if inv.WouldRewriteOrProxy != nil {
		in.WouldRewriteOrProxy = inv.WouldRewriteOrProxy
	}
	in.BuildBAMLRequest = inv.BuildBAMLRequest
	return in
}

func (e *UnaryExecutor) declined(err error, stage, reason string) bamlutils.NativeSpineUnaryResult {
	e.metrics.declines.Add(1)
	return bamlutils.DeclinedSpineResult(err, stage, reason)
}

func (e *UnaryExecutor) declinedOracle(err error, stage, reason string) bamlutils.NativeSpineUnaryOracleResult {
	e.metrics.declines.Add(1)
	return bamlutils.DeclinedOracleResult(err, stage, reason)
}

func (e *UnaryExecutor) succeeded(final any) bamlutils.NativeSpineUnaryResult {
	e.metrics.successes.Add(1)
	return bamlutils.SucceededSpineResult(final)
}

func (e *UnaryExecutor) failed(err error, stage, reason string) bamlutils.NativeSpineUnaryResult {
	e.metrics.failures.Add(1)
	return bamlutils.FailedAfterClaimSpineResult(err, stage, reason)
}

// capBody bounds a retained provider error body to errorBodyCap bytes and returns it
// as the string llmhttp.HTTPError.Body expects.
func capBody(b []byte) string {
	if len(b) <= errorBodyCap {
		return string(b)
	}
	return string(b[:errorBodyCap])
}
