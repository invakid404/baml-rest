package canary

// De-BAML Phase 7D native STREAM serve implementation. This is the streaming twin
// of serve.go's unary Server: for an admitted dynamic OpenAI `_dynamic`
// StreamRequest it actually SERVES natively — it runs the full no-send stream
// admission (AdmitStreamClaim), the pre-transport plan-compare precondition, then
// CLAIMS the transport and drives nanollm DoStream through the 7A one-shot exact
// stream client (execute.RunStream), emitting every normalized delta SYNCHRONOUSLY
// through the orchestrator's neutral EmitDelta so the shared accumulation/cadence/
// partial-parse/emission pipeline runs on the native lane. The orchestrator keeps
// ownership of the native-only partial and final parsers (scope §5.3/§5.4).
//
// It links nanollm (through admission + execute), so nothing here can enter the
// host/root link graph — the host stays zero-nanollm and CGO-free. A serve deploy
// profile's worker injects it as a neutral bamlutils.NativeStreamServeFunc; every
// default build and every flag-off build leaves it nil, so the orchestrator's
// native stream callback stays hard-off and every streamed request is byte-
// identical BAML.
//
// Ownership boundary (the load-bearing invariant, scope §4 I2/I4): admission + the
// plan compare run BEFORE the transport is claimed and may DECLINE (guaranteeing
// no socket, no EmitDelta) so the orchestrator serves BAML for the same child in
// the same retry iteration. From the moment execute.RunStream is entered every
// terminal condition is a Completed or a FailedAfterClaim — NEVER a decline, and
// NEVER a hidden same-child BAML resend/retry/fallback/pool-replay after the claim.

import (
	"context"
	"errors"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/nativeserve/admission"
	"github.com/invakid404/baml-rest/nativeserve/execute"
	"github.com/invakid404/baml-rest/nativeserve/parity"
)

// errNativeStreamServePanic backstops a panic AFTER the executor is entered: a
// socket may have opened, so the attempt FAILS (never declines) rather than risk a
// hidden BAML resend / a second physical native request.
var errNativeStreamServePanic = errors.New("nativeserve/canary: native stream serve attempt panicked after transport entry")

// StreamServer runs the native stream serve implementation. Construct it with
// NewStreamServer (or the NewStreamServeFunc factory the serve worker uses). It
// holds the stream admitter (AdmitStreamClaim), the exact executor (used ONLY for
// the no-send exact-transport preflight inside admission), and the two byte-
// progress deadlines threaded into the exact stream client.
type StreamServer struct {
	admitter         *admission.Admitter
	exec             *llmhttp.ExactExecutor
	firstBodyTimeout time.Duration
	idleTimeout      time.Duration
	// admitStreamClaim is the admission step Serve runs. It defaults to
	// admitter.AdmitStreamClaim (the real production predicate); same-package tests
	// may override it. Production never rebinds it.
	admitStreamClaim func(ctx context.Context, in admission.Input) (*admission.StreamClaim, error)
}

// NewStreamServer builds a StreamServer. A nil exec defaults to a hardened
// single-attempt exact executor; tests pass an executor over a loopback-guarded
// transport to fence external dials. firstBodyTimeout / idleTimeout are the two
// independent byte-progress deadlines (scope §5.10); a <= 0 value disables that
// bound in the exact stream client.
func NewStreamServer(m *admission.Metrics, exec *llmhttp.ExactExecutor, firstBodyTimeout, idleTimeout time.Duration) *StreamServer {
	if exec == nil {
		exec = llmhttp.NewExactExecutor(nil)
	}
	s := &StreamServer{
		admitter:         admission.NewAdmitter(m, exec),
		exec:             exec,
		firstBodyTimeout: firstBodyTimeout,
		idleTimeout:      idleTimeout,
	}
	s.admitStreamClaim = s.admitter.AdmitStreamClaim
	return s
}

// NewStreamServeFunc is the factory a serve-profile worker injects via
// workerboot.Options.NativeStreamServeFactory. It resolves the two byte-progress
// deadlines from the environment (reusing the shared stream-idle value, with a
// SEPARATELY-named first-body bound — scope §5.10/§11) and returns the neutral
// bamlutils.NativeStreamServeFunc that drives native streaming.
//
// Per-lane de-BAML STREAM metrics are deferred with the owner-trimmed rollout
// observability (scope §8 is release-time ops): the admitter is built with nil
// metrics (nil-safe), so it records no counters. The reg parameter is retained for
// signature symmetry with NewServeFunc and a future stream metrics family; sharing
// the unary serve's collectors on the same registry would double-register the
// de-BAML families, so the stream lane deliberately records none here.
func NewStreamServeFunc(reg prometheus.Registerer) (bamlutils.NativeStreamServeFunc, error) {
	_ = reg
	s := NewStreamServer(
		nil,
		llmhttp.NewExactExecutor(nil),
		llmhttp.StreamFirstBodyTimeoutFromEnv(),
		llmhttp.StreamIdleTimeoutFromEnv(),
	)
	return s.Serve, nil
}

// Serve is the bamlutils.NativeStreamServeFunc. It runs stream admission (keeping
// the request-scoped nanollm client alive as a StreamClaim), the pre-transport
// plan compare precondition, then — on a full plan match — ENTERS the exact
// executor (the point of no return for a decline) and drives DoStream through the
// 7A one-shot exact stream client, emitting owned deltas through the orchestrator's
// EmitDelta. Before entry it may DECLINE (no socket); from entry onward it only
// COMPLETES or FAILS-AFTER-CLAIM.
func (s *StreamServer) Serve(ctx context.Context, req bamlutils.NativeStreamServeRequest) (result bamlutils.NativeStreamServeResult) {
	entered := false
	defer func() {
		if r := recover(); r != nil {
			if entered {
				// Post-entry panic: a socket may have opened. FAIL — a decline here
				// would trigger a hidden BAML resend / a second native request.
				result = failAfterClaimStreamResult(errNativeStreamServePanic, "")
			} else {
				// Pre-entry panic: no socket occurred, so declining to BAML is safe.
				result = declineStreamResult(stageServe, reasonServedBAMLPanic)
			}
		}
	}()

	// Cancellation gate at ENTRY, BEFORE any native FFI: an already-cancelled
	// request declines to BAML with zero native work; the ordinary BAML stream then
	// fails once with the same context error.
	if ctx.Err() != nil {
		return declineStreamResult(stageServe, reasonServedBAMLCtx)
	}

	claim, err := s.admitStreamClaim(ctx, toStreamAdmissionInput(req))
	if err != nil {
		var d *admission.Decline
		if errors.As(err, &d) {
			return declineStreamResult(string(d.Stage), string(d.Reason))
		}
		// Unexpected native planner/FFI error before any socket: availability-first
		// decline to BAML.
		return declineStreamResult(stagePlanner, reasonPlannerError)
	}
	// The claim keeps the request-scoped engine alive so DoStream runs on the SAME
	// client Prepare produced the plan on. Close on EVERY path.
	defer claim.Close()

	// Streaming is STRICT OpenAI only in this slice (admission declines trusted-
	// provider streaming earlier via ReasonStreamingUnproven), so a returned claim
	// is always the strict anchor. Run the pre-transport plan-compare precondition:
	// a missing/failed BAML builder or any per-field drift declines to BAML BEFORE
	// the transport is claimed; native never sends.
	if claim.Verification == admission.PolicyStrictOpenAI {
		if req.BuildBAMLRequest == nil {
			return declineStreamResult(stageServe, reasonNoBAMLBuilder)
		}
		bamlReq, berr := req.BuildBAMLRequest(ctx)
		if berr != nil || bamlReq == nil {
			return declineStreamResult(stageServe, reasonBAMLBuildError)
		}
		if cmp := parity.ComparePlans(bamlReq, claim.ExactRequest); !cmp.AllMatch() {
			return declineStreamResult(stagePlanCompare, reasonPlanMismatch)
		}
	}

	// A provably PRE-TRANSPORT preflight rejection (the prepared plan's signature
	// window passed during the plan-build+compare step) opens NO socket, so decline
	// to BAML. Unreachable for the admitted never-expiring OpenAI surface.
	if claim.PlanExpired() {
		return declineStreamResult(stageServe, reasonPlanExpired)
	}

	// ctx gate IMMEDIATELY BEFORE entering the executor: an already-cancelled caller
	// declines safely (no socket) rather than entering and failing terminally.
	if ctx.Err() != nil {
		return declineStreamResult(stageServe, reasonServedBAMLCtx)
	}

	// --- ENTER THE EXACT EXECUTOR: POINT OF NO RETURN for a decline (I2/I4) ---
	// The exact stream client fires OnClaim immediately before the ONE RoundTrip.
	// From here every terminal condition is COMPLETED or FAILED-AFTER-CLAIM, never a
	// decline: even a zero-socket failure (a nanollm re-prepare plan drift caught by
	// the one-shot RoundTripper, or a nil-config guard) is a conservative claimed
	// failure, so there is no hidden BAML fallback after entry. This is a safe
	// false-negative before a socket that can never cause a duplicate request.
	entered = true

	res, aerr := execute.RunStream(ctx, execute.StreamConfig{
		Client:           claim.Client(),
		Request:          claim.Request(),
		Expected:         claim.ExactRequest,
		IncludeReasoning: req.IncludeReasoning,
		// Forward each normalized owned delta through the orchestrator's neutral
		// EmitDelta sink; an EmitDelta error asks RunStream to stop reading (a
		// terminal StreamPhaseEmit), never a retry.
		EmitDelta: func(d execute.StreamDelta) error {
			return req.EmitDelta(bamlutils.NativeStreamDelta{
				ParseableDelta: d.ParseableDelta,
				RawDelta:       d.RawDelta,
				ReasoningDelta: d.ReasoningDelta,
			})
		},
		FirstBodyTimeout:  s.firstBodyTimeout,
		IdleTimeout:       s.idleTimeout,
		OnResponseHeaders: req.SendHeaders,
		OnFirstBody:       req.SendFirstBody,
	})
	if aerr != nil {
		// Map to a public typed error (§5.11): a provider non-2xx (D11) becomes the
		// REAL *llmhttp.HTTPError (status + 4 KiB public body cap); every other
		// terminal preserves its typed cause (context error, exact-stream error,
		// decoder/timeout) via Unwrap so the orchestrator classifies it exactly like a
		// BAML terminal. TERMINAL: the orchestrator never resends/retries/replays.
		if httpErr, ok := execute.ProviderStatusHTTPError(aerr); ok {
			return failAfterClaimStreamResult(httpErr, "")
		}
		return failAfterClaimStreamResult(aerr, "")
	}

	// Completed: the upstream stream reached a valid terminal condition and the
	// decoder closed cleanly. Every partial was already emitted via EmitDelta; the
	// orchestrator owns the FINAL parse over the accumulated text and marks
	// winner_engine=native.
	return completedStreamResult(res)
}

// toStreamAdmissionInput maps the neutral stream serve request into the shared
// admission Input. The serve implementation only runs on a native-capable worker,
// with the umbrella flag on, on the dynamic BuildRequest StreamRequest route, so
// those layer-1 facts are fixed true; the TRUTHFUL strategy/retry facts, the
// rewrite/proxy predicate, and provider/registry/messages/schema come from the
// request.
func toStreamAdmissionInput(req bamlutils.NativeStreamServeRequest) admission.Input {
	return admission.Input{
		WorkerCapable:           true,
		RequestAPIPresent:       true,
		OnBuildRequestRoute:     true,
		FlagEnabled:             true,
		Method:                  bamlutils.DynamicMethodName,
		Mode:                    toStreamAdmissionMode(req.Mode),
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
	}
}

func toStreamAdmissionMode(m bamlutils.NativeStreamMode) admission.Mode {
	switch m {
	case bamlutils.NativeStreamModeStream:
		return admission.ModeStream
	case bamlutils.NativeStreamModeStreamWithRaw:
		return admission.ModeStreamWithRaw
	default:
		return admission.ModeUnknown
	}
}

func declineStreamResult(stage, reason string) bamlutils.NativeStreamServeResult {
	return bamlutils.NativeStreamServeResult{
		Disposition: bamlutils.NativeStreamDeclined,
		Stage:       stage,
		Reason:      reason,
	}
}

func failAfterClaimStreamResult(err error, raw string) bamlutils.NativeStreamServeResult {
	return bamlutils.NativeStreamServeResult{
		Disposition:   bamlutils.NativeStreamFailedAfterClaim,
		Err:           err,
		RawDiagnostic: raw,
	}
}

// completedStreamResult builds a completed outcome carrying the native winner-
// engine token and the sampled token usage (internal only; never surfaced on the
// public wire this phase).
func completedStreamResult(res *execute.StreamResult) bamlutils.NativeStreamServeResult {
	out := bamlutils.NativeStreamServeResult{
		Disposition:  bamlutils.NativeStreamCompleted,
		WinnerEngine: bamlutils.NativeServeEngineNative,
	}
	if res != nil && res.Usage != nil {
		out.Usage = bamlutils.NativeStreamUsage{
			PromptTokens:     int64(res.Usage.PromptTokens),
			CompletionTokens: int64(res.Usage.CompletionTokens),
		}
		if res.Usage.TotalTokens != nil {
			out.Usage.TotalTokens = int64(*res.Usage.TotalTokens)
		}
	}
	return out
}
