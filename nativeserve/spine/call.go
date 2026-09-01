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
			if claimed {
				// Post-claim panic: a socket may have opened. FAIL — a decline here would
				// invite a hidden fallback resend for the same call.
				result = bamlutils.FailedAfterClaimSpineResult(fmt.Errorf("nativespine: panic after claim: %v", r), stageServe, reasonPanic)
				e.metrics.failures.Add(1)
			} else {
				// Pre-claim panic: no socket occurred, so declining is safe.
				result = bamlutils.DeclinedSpineResult(fmt.Errorf("nativespine: panic before claim: %v", r), stageServe, reasonPanic)
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

	bundle := claim.Bundle
	res, rerr := execute.RunAttempt(ctx, execute.AttemptConfig{
		Client:   claim.Client(),
		Prepared: claim.Prepared,
		Executor: e.exec,
		// The native static SAP parser is a SCHEMA-NEUTRAL closure capturing the stored
		// Return Bundle: execute never learns about Bundles, and internal/debaml owns the
		// parse (debaml.ParseStaticBundleUnaryCall — the same predicate that gated
		// registration and gates direct parse). No generated BAML, no CFFI.
		ParseResponse: func(pctx context.Context, rawText string) ([]byte, error) {
			parsed, e := debaml.ParseStaticBundleUnaryCall(pctx, bundle, rawText)
			if e != nil {
				return nil, e
			}
			return parsed.JSON, nil
		},
		IncludeReasoning: false,
	})
	return e.mapAttempt(rm, res, rerr)
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
	parsed, err := debaml.ParseStaticBundleUnaryCall(ctx, rm.bundle, raw)
	if err != nil {
		// Ordinary terminal parse error for an admitted method — NOT a capability decline.
		return nil, err
	}
	return rm.binding.DecodeFinal(parsed.JSON)
}

// staticInput maps a registered method + projected values into the shared static
// admission StaticInput for the spine lane. The layer-1 facts are the lane's
// constants; the SPINE lane bypasses the dynamic-rollout cohort gate (its admission
// is its own registration-time totality gate). checkArgBinder proves the projected
// vector's names/order match the descriptor exactly.
func (e *UnaryExecutor) staticInput(rm *registeredMethod, values []promptdescriptor.ArgumentValue, ad bamlutils.Adapter) admission.StaticInput {
	args := make(map[string]any, len(values))
	order := make([]string, 0, len(values))
	for _, v := range values {
		args[v.Name] = v.Value
		order = append(order, v.Name)
	}
	in := admission.StaticInput{
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
	}
	// The rewrite/proxy gate must ALWAYS run, even on a PLAIN-context call with no
	// adapter-configured client: default the predicate to the process-global
	// llmhttp.DefaultClient (which knows the global rewrite/proxy rules) so
	// AdmitStaticSpineClaim can never SKIP the gate and let a global rewrite/proxy rule
	// route a native send elsewhere (CodeRabbit #9). An adapter-configured HTTP client,
	// when present, OVERRIDES the default below.
	in.WouldRewriteOrProxy = llmhttp.DefaultClient.WouldRewriteOrProxy
	// Request-scoped orchestration facts (Codex review finding 1). A request retry
	// override or a round-robin advancer declines at admission's strategy gate; a
	// rewrite/proxy on the effective send target declines pre-claim inside
	// AdmitStaticSpineClaim against the prepared URL (the check the omitted BAML
	// plan-compare used to own). A caller registry / dynamic schema already declined
	// in Call before this point, so Registry stays nil here.
	if ad != nil {
		in.HasRequestRetryOverride = ad.RetryConfig() != nil
		in.HasRoundRobin = ad.RoundRobinAdvancer() != nil
		if hc := ad.HTTPClient(); hc != nil {
			in.WouldRewriteOrProxy = hc.WouldRewriteOrProxy
		}
	}
	return in
}

func (e *UnaryExecutor) declined(err error, stage, reason string) bamlutils.NativeSpineUnaryResult {
	e.metrics.declines.Add(1)
	return bamlutils.DeclinedSpineResult(err, stage, reason)
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
