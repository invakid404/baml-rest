package canary

// De-BAML Phase 3b native STATIC STREAM serve implementation. This is the STATIC twin
// of stream_serve.go's dynamic StreamServer and the STREAMING twin of serve_static.go's
// unary ServeStatic: for an admitted generated static `/stream{,-with-raw}` request it
// actually SERVES natively — AdmitStaticStreamClaim runs the full no-send static-stream
// predicate (descriptor envelope, arg binder, Return-Bundle lower/support, the SERVE-only
// static-stream return-shape gate, RenderStatic, the streaming canonical body, nanollm
// Prepare with Stream:true, and the strict BAML **StreamRequest** no-send plan compare),
// and on a full would-admit returns a request-scoped claim; then ServeStaticStream CLAIMS
// the transport and drives nanollm DoStream through the 7A one-shot exact stream client
// (execute.RunStream), emitting every normalized delta SYNCHRONOUSLY through the
// orchestrator's neutral EmitDelta. The orchestrator keeps ownership of the native-only
// partial + final parsers (the generated static-stream installer wires them to
// debaml.ParseStaticStreamPartial/Final).
//
// Ownership boundary (I2/I4, mirroring StreamServer.Serve): admission + the plan compare
// run BEFORE the transport is claimed and may DECLINE (guaranteeing no socket, no
// EmitDelta) so the orchestrator serves BAML for the same child in the same retry
// iteration. From the moment execute.RunStream is entered every terminal condition is a
// Completed or a FailedAfterClaim — NEVER a decline, and NEVER a hidden same-child BAML
// resend/retry/fallback/pool-replay after the claim.
//
// Tri-state behavior (the claim happens ONLY on a full would-admit, per the paragraph above —
// the return-shape match alone is NOT sufficient): AdmitStaticStreamClaim FIRST runs the
// shared no-send bundle predicate (admitStaticStreamThroughBundle — context / mode / strategy /
// descriptor / arg-binder / Return-Bundle lower+support), ANY of which can DECLINE before the
// return-shape gate is ever reached. A request that DOES reach that gate and whose Return Bundle
// matches the PROVEN recursive-alias family (the exact JSON alias — admittedStaticStreamReturnShape)
// is then only ELIGIBLE to proceed: it still passes through RenderStatic, the client/provider
// normalize + canonical body, nanollm New/Prepare, and the strict StreamRequest plan compare, ANY
// of which can still decline pre-claim. ONLY on a full would-admit does AdmitStaticStreamClaim
// return a claim and ServeStaticStream CLAIM the transport and serve natively. Every non-matching
// shape — and every earlier-or-later pre-claim decline — returns a DECLINE before transport (no
// socket), and BAML serves.

import (
	"context"
	"errors"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/nativeserve/admission"
	"github.com/invakid404/baml-rest/nativeserve/execute"
)

// errNativeStaticStreamServePanic backstops a panic AFTER the executor is entered: a
// socket may have opened, so the attempt FAILS (never declines) rather than risk a hidden
// BAML resend / a second physical native request.
var errNativeStaticStreamServePanic = errors.New("nativeserve/canary: native static stream serve attempt panicked after transport entry")

// StaticStreamServer runs the native static-stream serve implementation. Construct it
// with NewStaticStreamServer (or the NewStaticStreamServeFunc factory the serve worker
// uses). It holds only the two byte-progress deadlines threaded into the one-shot exact
// stream client execute.RunStream builds. Unlike the dynamic StreamServer it carries NO
// exact executor: static-stream admission is the package-level AdmitStaticStreamClaim
// (which does its own no-send nanollm New/Prepare + StreamRequest plan-compare, no
// executor), and RunStream owns the single DoStream RoundTrip through its own
// NewExactStreamClient — so there is no seam an injected executor would participate in.
type StaticStreamServer struct {
	firstBodyTimeout time.Duration
	idleTimeout      time.Duration
	// admitStaticStreamClaim is the admission step ServeStaticStream runs. It defaults to
	// admission.AdmitStaticStreamClaim (the real production predicate); same-package tests
	// may override it to force a claim over a loopback SSE server. Production never rebinds it.
	admitStaticStreamClaim func(ctx context.Context, in admission.StaticStreamInput) (*admission.StaticStreamClaim, error)

	// cohort is the serving-cutover configuration identity this static-stream server
	// presents. Production leaves it ZERO, so the default-deny gate declines every
	// static stream before any native work; the gated end-to-end proof passes an
	// enrolled proof identity through NewStaticStreamServerWithCohortIdentity. Not a
	// rollout control — see canary.NewServerWithCohortIdentity.
	cohort admission.CohortInput
	// metrics carries the bounded de-BAML collectors so this lane records the SAME
	// surface/cohort/phase/winner series the other three do.
	//
	// It was deliberately absent until a cold review pointed out the consequence:
	// with no recorder, `static_stream` had no series at all, so operational
	// invariant 4 ("a non-enrolled surface reporting a native claim is a
	// rollout-stop") could not alert for that surface — the one place where a silent
	// claim would be least visible. The lane's per-lane STREAM counters stay
	// owner-trimmed; what it now records is the cutover's five-surface accounting.
	metrics *admission.Metrics
}

// NewStaticStreamServer builds a StaticStreamServer. firstBodyTimeout / idleTimeout are the
// two independent byte-progress deadlines threaded into RunStream's exact stream client; a
// <= 0 value disables that bound.
func NewStaticStreamServer(firstBodyTimeout, idleTimeout time.Duration) *StaticStreamServer {
	s := &StaticStreamServer{
		firstBodyTimeout: firstBodyTimeout,
		idleTimeout:      idleTimeout,
	}
	s.admitStaticStreamClaim = admission.AdmitStaticStreamClaim
	return s
}

// NewStaticStreamServeFunc is the factory a serve-profile worker injects via
// workerboot.Options.NativeStaticStreamServeFactory. It resolves the two byte-progress
// deadlines from the environment (reusing the shared stream-idle value + the
// separately-named first-body bound) and returns the neutral
// bamlutils.NativeStaticStreamServeFunc that drives native static streaming. It REUSES
// the worker's collectors so the lane records the serving-cutover surface/cohort/phase/
// winner series; its own per-lane STREAM decline/attempt counters remain owner-trimmed
// because static-stream admission runs through package-level functions with no metrics
// receiver.
func NewStaticStreamServeFunc(reg prometheus.Registerer) (bamlutils.NativeStaticStreamServeFunc, error) {
	m, err := admission.NewMetricsReusing(reg)
	if err != nil {
		return nil, err
	}
	s := NewStaticStreamServer(
		llmhttp.StreamFirstBodyTimeoutFromEnv(),
		llmhttp.StreamIdleTimeoutFromEnv(),
	)
	s.metrics = m
	return s.ServeStaticStream, nil
}

// ServeStaticStream is the bamlutils.NativeStaticStreamServeFunc. It runs static-stream
// admission (keeping the request-scoped nanollm client alive as a StaticStreamClaim, with
// the strict BAML StreamRequest plan compare as a PRE-SOCKET precondition INSIDE
// AdmitStaticStreamClaim), then — on a full would-admit — ENTERS the exact executor (the
// point of no return for a decline) and drives DoStream through the one-shot exact stream
// client, emitting owned deltas through the orchestrator's EmitDelta. Before entry it may
// DECLINE (no socket); from entry onward it only COMPLETES or FAILS-AFTER-CLAIM.
func (s *StaticStreamServer) ServeStaticStream(ctx context.Context, inv bamlutils.NativeStaticStreamInvocation) (result bamlutils.NativeStreamServeResult) {
	entered := false
	// Serving-cutover S1 identity + exactly-once phase/winner accounting, registered
	// FIRST so it runs LAST and observes the final named result.
	surface, cohort := admission.SurfaceStaticStream, admission.ResolveCohort(admission.SurfaceStaticStream, s.toStaticStreamAdmissionInput(inv).Cohort)
	defer func() {
		s.recordStaticStreamTerminal(surface, cohort, entered, result.Disposition, result.WinnerEngine)
	}()
	defer func() {
		if r := recover(); r != nil {
			if entered {
				// Post-entry panic: a socket may have opened. FAIL — a decline here would
				// trigger a hidden BAML resend / a second native request.
				result = failAfterClaimStreamResult(errNativeStaticStreamServePanic, "")
			} else {
				// Pre-entry panic: no socket occurred, so declining to BAML is safe.
				result = declineStreamResult(stageServe, reasonServedBAMLPanic)
			}
		}
	}()

	// Cancellation gate at ENTRY, BEFORE any native FFI.
	if ctx.Err() != nil {
		return declineStreamResult(stageServe, reasonServedBAMLCtx)
	}

	// EmitDelta is the ONE mandatory callback: RunStream dereferences it inside its delta
	// callback AFTER entry (after the socket opens), so a seam that hands over an invocation
	// without it would burn one provider request and terminate via the post-entry panic
	// backstop. Gate it here among the pre-entry gates so a mis-wired seam declines PRE-CLAIM
	// (no socket, no admission work) instead. SendHeaders/SendFirstBody stay optional
	// (RunStream tolerates nil for those).
	if inv.EmitDelta == nil {
		return declineStreamResult(stageServe, reasonNilEmitDelta)
	}

	claim, err := s.admitStaticStreamClaim(ctx, s.toStaticStreamAdmissionInput(inv))
	if err != nil {
		var d *admission.StaticDecline
		if errors.As(err, &d) {
			return declineStreamResult(d.Stage, d.Reason)
		}
		// Unexpected native planner/FFI error before any socket: availability-first decline.
		return declineStreamResult(stagePlanner, reasonPlannerError)
	}
	// The claim keeps the request-scoped engine alive so DoStream runs on the SAME client
	// Prepare produced the plan on. Close on EVERY path. The strict StreamRequest plan
	// compare already ran INSIDE AdmitStaticStreamClaim (mirroring ServeStatic), so there
	// is no second compare here.
	defer claim.Close()

	if claim.PlanExpired() {
		return declineStreamResult(stageServe, reasonPlanExpired)
	}
	if ctx.Err() != nil {
		return declineStreamResult(stageServe, reasonServedBAMLCtx)
	}

	// --- ENTER THE EXACT EXECUTOR: POINT OF NO RETURN for a decline (I2/I4) ---
	entered = true
	// The claim boundary for this lane is executor ENTRY (the exact stream client
	// fires its one RoundTrip immediately after), so the claimed phase is recorded
	// here — after every pre-transport decline gate above.
	s.metrics.RecordAdmissionPhase(surface, cohort, admission.PhaseClaimed)

	res, aerr := execute.RunStream(ctx, execute.StreamConfig{
		Client:           claim.Client(),
		Request:          claim.Request(),
		Expected:         claim.ExactRequest,
		IncludeReasoning: inv.IncludeReasoning,
		EmitDelta: func(d execute.StreamDelta) error {
			return inv.EmitDelta(bamlutils.NativeStreamDelta{
				ParseableDelta: d.ParseableDelta,
				RawDelta:       d.RawDelta,
				ReasoningDelta: d.ReasoningDelta,
			})
		},
		FirstBodyTimeout:  s.firstBodyTimeout,
		IdleTimeout:       s.idleTimeout,
		OnResponseHeaders: inv.SendHeaders,
		OnFirstBody:       inv.SendFirstBody,
	})
	if aerr != nil {
		if httpErr, ok := execute.ProviderStatusHTTPError(aerr); ok {
			return failAfterClaimStreamResult(httpErr, "")
		}
		return failAfterClaimStreamResult(aerr, "")
	}

	return completedStreamResult(res)
}

// toStaticStreamAdmissionInput maps the neutral static-stream invocation into the
// static-stream admission input. The serve implementation only runs on a native-capable
// worker with the umbrella flag on, on the static BuildRequest StreamRequest route, so
// those layer-1 facts are fixed true; the TRUTHFUL selected-child facts, the
// WouldRewriteOrProxy predicate, and provider/descriptor/args/mode come from the
// invocation. It mirrors toStaticAdmissionInput exactly, targeting the streaming claim.
func (s *StaticStreamServer) toStaticStreamAdmissionInput(inv bamlutils.NativeStaticStreamInvocation) admission.StaticStreamInput {
	return admission.StaticStreamInput{
		// Serving-cutover S1: the server's configuration identity — ZERO in production
		// (no config-load fingerprint is plumbed yet), so the default-deny gate inside
		// admission resolves CohortNone and declines this lane before any native work.
		// This lane's own per-lane STREAM counters stay owner-trimmed, but it DOES record
		// the cutover's surface/cohort/phase/winner series (the factory supplies the
		// shared collectors), so static_stream is accounted for like every other surface.
		Cohort:                  s.cohort,
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
		NeedsRaw:                inv.NeedsRaw,
		SingleLeaf:              inv.SingleLeaf,
		HasFallbackChain:        inv.HasFallbackChain,
		HasRoundRobin:           inv.HasRoundRobin,
		HasRequestRetryOverride: inv.HasRequestRetryOverride,
		ClientOverride:          inv.ClientOverride,
		Provider:                inv.Provider,
		WouldRewriteOrProxy:     inv.WouldRewriteOrProxy,
		BuildBAMLRequest:        inv.BuildBAMLRequest,
	}
}

// recordStaticStreamTerminal records EXACTLY ONE phase+winner pair for a finished
// static stream, with the same fail-closed mapping the other three lanes use: before
// executor entry the only disposition is a safe pre-claim decline; after it, a
// completed native stream or a typed failure — and a post-entry decline (an
// ownership-boundary violation) or an unrecognized winner-engine token records
// failure rather than a safe decline or a native win.
func (s *StaticStreamServer) recordStaticStreamTerminal(surface admission.Surface, cohort admission.CohortID, entered bool, disposition bamlutils.NativeStreamDisposition, engine string) {
	if !entered {
		s.metrics.RecordPreclaimDecline(surface, cohort)
		return
	}
	winner := admission.WinnerFailure
	if disposition == bamlutils.NativeStreamCompleted && engine == bamlutils.NativeServeEngineNative {
		winner = admission.WinnerNative
	}
	s.metrics.RecordPostclaimTerminal(surface, cohort, winner)
}
