package spine

import (
	"context"
	"errors"
	"time"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/buildrequest"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/bamlutils/projectdescriptor"
	"github.com/invakid404/baml-rest/bamlutils/promptdescriptor"
	"github.com/invakid404/baml-rest/internal/debaml"
	"github.com/invakid404/baml-rest/nativeserve/admission"
	"github.com/invakid404/baml-rest/nativeserve/execute"
)

// M3e-A — the production BAML-FREE codegen-spine STREAM executor.
//
// It is the streaming twin of [UnaryExecutor.Call] and reuses the SAME machinery the
// unary lane does, with nothing new invented:
//
//	admission.AdmitStaticSpineStreamClaim  the no-send pre-claim predicate + kept-alive engine
//	execute.RunStream                      the one-shot exact stream client + nanollm DoStream
//	buildrequest.StreamCadence             the shared accumulation/throttle/delivery cadence
//	internal/debaml Parse*Stream*          the root-owned native partial + final parsers
//	the emitted stream binding             the pointer-carrier partial decoder
//
// Its whole contract is the claim discipline. Before the claim, every rejection is a
// DECLINE certifying zero provider sockets and zero emitted events. The claim is taken
// immediately before execute.RunStream, and from that instant EVERY fault — transport,
// provider status, decoder, timeout, cancellation, emit failure, partial or final
// parse/decode failure, or a panic — is FailedAfterClaim: terminal, with no BAML, no
// retry, no fallback child, no reset, no pool replay, and no second RoundTrip.
//
// There is nothing to fall back TO. This executor is the native-only artifact's whole
// serving path, so a terminal failure is a caller-visible error frame — which is exactly
// why the strict cadence policy is correct here and the legacy swallow-everything policy
// is not. Under it the ONLY benign non-emitting outcome is the parse closure's explicit
// no-partial RESULT, which it produces solely for the PARSER's own "no parseable partial
// for this prefix yet" sentinel and only BEFORE the decoder runs. Every error that
// reaches the cadence is terminal regardless of its chain, so a decoder failure that
// returns or wraps that same sentinel fails the claimed stream rather than being read as
// a no-event.

// Bounded, secret-free stage/reason tokens for the stream lane. They extend — never
// replace — the unary lane's tokens, so a metric label set stays closed.
const (
	stageStream = "stream"

	reasonNoStreamBinding   = "no_stream_binding"
	reasonNilEmitCallback   = "nil_emit_callback"
	reasonModeNotStream     = "mode_not_stream"
	reasonStreamEmit        = "emit_error"
	reasonStreamDecode      = "stream_decoder_error"
	reasonStreamProtocol    = "stream_protocol_error"
	reasonStreamFirstBody   = "stream_first_body_timeout"
	reasonStreamIdle        = "stream_idle_timeout"
	reasonStreamCancelled   = "stream_cancelled"
	reasonFinalParse        = "native_final_parse_error"
	reasonFinalParseDecline = "native_final_parse_declined"
)

var (
	errNoStreamBinding = errors.New("nativespine: method is registered without a stream binding; the exact cohort serves streams only for a stream-capable generated method")
	errNilEmit         = errors.New("nativespine: stream requires a non-nil emit callback")
	errStreamMode      = errors.New("nativespine: the requested public mode is not a stream mode; the exact cohort streams only /stream and /stream-with-raw")
	errFinalDecline    = errors.New("nativespine: native stream final parser declined the accumulated response (no BAML fallback on this cohort)")
)

// StreamExecutor is the production [bamlutils.NativeSpineStreamExecutor] over the exact
// five-arm `JSON` cohort. It EMBEDS the immutable [UnaryExecutor], so Call, Parse, and
// CallWithOracle are inherited BYTE-FOR-BYTE — this type adds Stream and ParseStream and
// changes nothing about the unary lane. It is immutable after construction.
type StreamExecutor struct {
	*UnaryExecutor

	firstBodyTimeout time.Duration
	idleTimeout      time.Duration

	// admitStreamClaim is the pre-socket admission step, defaulting to the BAML-free
	// admission.AdmitStaticSpineStreamClaim. It is a field only so gated tests can
	// inject a synthetic claim and drive the post-claim fault matrix deterministically;
	// every production constructor leaves the default.
	admitStreamClaim func(ctx context.Context, in admission.StaticStreamInput) (*admission.StaticStreamClaim, error)
}

// compile-time assertions: the stream executor satisfies the STREAM contract, and (via
// the embedded unary executor) the frozen unary and oracle contracts unchanged.
var (
	_ bamlutils.NativeSpineStreamExecutor      = (*StreamExecutor)(nil)
	_ bamlutils.NativeSpineUnaryExecutor       = (*StreamExecutor)(nil)
	_ bamlutils.NativeSpineUnaryOracleExecutor = (*StreamExecutor)(nil)
)

// NewStreamExecutor builds an immutable stream executor over the VALIDATED project plus
// the emitted per-method STREAM registrations. Like [NewUnaryExecutor] it is STRICT:
// every passed registration must be admitted (a caller passes only what it means to
// serve), and it drives the SAME single classifier — so a registration accepted here is
// accepted identically by the population classifier and vice versa.
//
// A nil exact executor uses the hardened default. The two byte-progress deadlines come
// from the environment, the same source the legacy static-stream server uses.
func NewStreamExecutor(proj projectdescriptor.Project, registrations []StreamRegistration, exact *llmhttp.ExactExecutor) (*StreamExecutor, error) {
	normalized := make([]candidateRegistration, len(registrations))
	for i := range registrations {
		// Per-iteration COPY: taking the address of the caller's slice element would let
		// a post-construction mutation of that slice reach the registry this builds.
		b := registrations[i].Binding
		normalized[i] = candidateRegistration{
			binding: b.Unary,
			stream:  &b,
			build:   registrations[i].BuildMethod,
		}
	}
	return newStreamExecutorFrom(proj, normalized, exact)
}

// newStreamExecutorFrom is the shared private constructor: it builds the strict registry
// through the SAME newExecutor core the unary lane uses (with the stream surface
// required) and wraps it. NewWorkerRuntime calls it with the already-classified accepted
// subset so the runtime's method maps and the executor's registry are in lockstep by
// construction.
func newStreamExecutorFrom(proj projectdescriptor.Project, candidates []candidateRegistration, exact *llmhttp.ExactExecutor) (*StreamExecutor, error) {
	base, err := newExecutor(proj, candidates, exact, true)
	if err != nil {
		return nil, err
	}
	return &StreamExecutor{
		UnaryExecutor:    base,
		firstBodyTimeout: llmhttp.StreamFirstBodyTimeoutFromEnv(),
		idleTimeout:      llmhttp.StreamIdleTimeoutFromEnv(),
		admitStreamClaim: admission.AdmitStaticSpineStreamClaim,
	}, nil
}

// Stream performs ONE claimed native stream for an admitted method and returns the
// neutral tri-state result, after delivering every public event through emit.
//
// Ownership discipline: a pre-socket DECLINE guarantees zero RoundTrips and zero emitted
// events; from claimed=true onward every terminal condition is SUCCEEDED or
// FAILED-AFTER-CLAIM — never a decline, never a resend. The logical claim is set
// immediately BEFORE execute.RunStream, deliberately earlier than any internal RunStream
// wiring error: once this executor enters the one-send operation, its failure is terminal
// rather than a possible fallback.
func (e *StreamExecutor) Stream(ctx context.Context, method string, input any, emit bamlutils.NativeSpineStreamEmit) (result bamlutils.NativeSpineStreamResult) {
	claimed := false
	// cadence is read by the panic guard for the accumulated raw diagnostic, so it is
	// declared before the guard is installed.
	var cadence *buildrequest.StreamCadence
	defer func() {
		if r := recover(); r != nil {
			// The recovered value is DROPPED, never interpolated: only the bounded
			// sentinel + stage/reason leave here.
			_ = r
			if claimed {
				// Post-claim panic: a socket may have opened and events may already have
				// been delivered. FAIL — a decline here would invite a hidden resend.
				result = bamlutils.FailedAfterClaimSpineStreamResult(errPanicAfterClaim, stageStream, reasonPanic, accumulatedRaw(cadence))
				e.metrics.failures.Add(1)
			} else {
				// Pre-claim panic: no socket and no event occurred, so declining is safe.
				result = bamlutils.DeclinedSpineStreamResult(errPanicBeforeClaim, stageStream, reasonPanic)
				e.metrics.declines.Add(1)
			}
		}
	}()

	// --- Pre-socket registry + preflight gates. Every one of these is a DECLINE. ---
	rm, ok := e.registry[method]
	if !ok {
		return e.declinedStream(&bamlutils.NativeSpineUnsupportedMethodError{Method: method, Reason: reasonUnsupportedMethod}, stageRegistry, reasonUnsupportedMethod)
	}
	if rm.stream == nil || rm.stream.DecodePartial == nil {
		return e.declinedStream(errNoStreamBinding, stageRegistry, reasonNoStreamBinding)
	}
	// The emit callback is dereferenced only AFTER the socket opens, so a mis-wired
	// caller that omitted it would otherwise burn one provider request and terminate.
	// Gate it here, among the pre-socket gates.
	if emit == nil {
		return e.declinedStream(errNilEmit, stagePreflight, reasonNilEmitCallback)
	}
	if err := ctx.Err(); err != nil {
		return e.declinedStream(err, stagePreflight, reasonContextCancelled)
	}

	// Read the REQUEST-SCOPED facts off the adapter the worker configured (the emitted
	// BuildMethod passes the adapter as this Stream's context) and DECLINE PRE-SOCKET on
	// any cohort-forbidden fact, exactly as the unary lane does.
	ad, _ := ctx.(bamlutils.Adapter)
	publicMode := bamlutils.StreamModeCall
	if ad != nil {
		publicMode = ad.StreamMode()
	}
	nativeMode, ok := nativeStreamMode(publicMode)
	if !ok {
		return e.declinedStream(errStreamMode, stagePreflight, reasonModeNotStream)
	}
	if ad != nil {
		if ad.OriginalClientRegistry() != nil {
			return e.declinedStream(errClientRegistry, stageAdmission, reasonClientRegistry)
		}
		if ad.DeBAMLOutputSchema() != nil {
			return e.declinedStream(errDynamicSchema, stageAdmission, reasonDynamicSchema)
		}
	}

	// Project the input through the EMBEDDED unary binding — one projector for both
	// lanes, so a stream request binds exactly the arguments a /call would.
	values, perr := rm.binding.ProjectInput(input)
	if perr != nil {
		return e.declinedStream(perr, stageProject, reasonProjectInputErr)
	}

	claim, aerr := e.admitStreamClaim(ctx, e.staticStreamInput(rm, values, ad, nativeMode))
	if aerr != nil {
		var d *admission.StaticDecline
		if errors.As(aerr, &d) {
			// A typed pre-socket admission decline: zero sockets, zero events.
			return e.declinedStream(d, d.Stage, d.Reason)
		}
		// An unexpected planner/FFI error before any socket.
		return e.declinedStream(aerr, stagePlanner, reasonPlannerError)
	}
	// The claim keeps the request-scoped engine alive for exactly one DoStream. Close it
	// on EVERY path.
	defer claim.Close()

	// Provably PRE-SOCKET preflight rejections (unsigned OpenAI plans never expire).
	if claim.PlanExpired() {
		return e.declinedStream(errPlanExpired, stagePreflight, reasonPlanExpired)
	}
	if err := ctx.Err(); err != nil {
		return e.declinedStream(err, stagePreflight, reasonContextCancelled)
	}

	// --- Build EVERY cadence/parser/emit closure BEFORE the claim. No callback may
	// first be discovered after a socket might be open. ---
	bundle := claim.Bundle
	decodePartial := rm.stream.DecodePartial
	needsRaw := nativeMode == bamlutils.NativeStreamModeStreamWithRaw
	includeReasoning := ad != nil && ad.IncludeReasoning()
	cadence = buildrequest.NewStreamCadence(buildrequest.StreamCadenceConfig{
		NeedsPartials: true,
		NeedsRaw:      needsRaw,
		// The native lane parses on EVERY tick, matching the generated adapters (none of
		// which configures a throttle) and therefore stock BAML's partial cadence.
		ParseThrottleInterval: 0,
		ParsePartial: func(pctx context.Context, accumulated string) (any, bool, error) {
			// The root-owned native static-stream PARTIAL parse, then the emitted
			// POINTER-carrier decode. No BAML on any prefix.
			parsed, err := debaml.ParseStaticStreamPartial(pctx, bundle, accumulated)
			if err != nil {
				// ONLY the PARSER's documented "no parseable partial for this prefix
				// yet" sentinel is benign, and it is resolved HERE — before the decoder
				// runs — into the cadence's explicit no-partial result. For an immutable
				// bundle that already passed admission it means exactly that, and it is
				// never a fallback: there is nothing to fall back to on a claimed stream.
				if errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
					return nil, false, nil
				}
				return nil, false, err
			}
			carrier, derr := decodePartial(parsed.JSON)
			if derr != nil {
				// EVERY decoder error is TERMINAL, whatever its error chain. A decoder
				// that returned or wrapped the parser's sentinel must NEVER be read as a
				// benign no-partial: the parser already produced bytes for this prefix,
				// so failing to decode them is a real post-claim failure.
				return nil, false, derr
			}
			// Presence is the DECODER's success, not a nil check: a typed-nil carrier
			// (a present-but-null partial) is a real event and must not be collapsed.
			return carrier, true, nil
		},
		// STRICT: every error that reaches the cadence is terminal. The benign
		// no-partial case never arrives as an error — it is the explicit result above.
		ParsePolicy: buildrequest.CadenceParseErrorsAreTerminal,
		Emit: func(ev buildrequest.StreamCadenceEvent) error {
			return emit(bamlutils.NativeSpineStreamEvent{
				HasPartial: ev.HasPartial,
				Partial:    ev.Partial,
				Raw:        ev.Raw,
				Reasoning:  ev.Reasoning,
			})
		},
	})

	// --- CLAIM the native attempt (ownership boundary). From here every terminal
	// condition is SUCCEEDED or FAILED-AFTER-CLAIM. ---
	//
	// ORDER IS LOAD-BEARING and is pinned structurally by
	// TestStreamClaimMarkerIsTheLastStatementBeforeRunStream: the bookkeeping happens
	// FIRST, so `claimed = true` is the FINAL statement before entering the one-send
	// operation. Nothing may be inserted between them — a statement in that gap could
	// panic while the guard still reads `claimed == false`, turning a post-claim fault
	// into a DECLINE and inviting a resend.
	e.metrics.claims.Add(1)
	claimed = true
	res, rerr := execute.RunStream(ctx, execute.StreamConfig{
		Client:           claim.Client(),
		Request:          claim.Request(),
		Expected:         claim.ExactRequest,
		IncludeReasoning: includeReasoning,
		EmitDelta: func(d execute.StreamDelta) error {
			return cadence.Delta(ctx, d.ParseableDelta, d.RawDelta, d.ReasoningDelta)
		},
		FirstBodyTimeout: e.firstBodyTimeout,
		// OnClaim fires immediately before the underlying RoundTrip: it is the
		// PHYSICAL socket marker, an observability proof that the transport boundary
		// was reached — never permission to decline if RunStream then fails.
		IdleTimeout: e.idleTimeout,
		OnClaim:     func() { e.metrics.sockets.Add(1) },
	})
	if rerr != nil {
		// EVERY RunStream error is terminal. Preserve the provider status when one is
		// available (D11: the provider-native body is never normalized away), retain the
		// bounded phase as stage/reason, and carry the accumulated raw as the diagnostic.
		// Admission is NOT re-run, DoStream is NOT retried, no BAML is invoked, no
		// fallback advances, no reset is emitted, and no pool replay is requested.
		stage, reason := streamFailStageReason(rerr)
		if httpErr, ok := execute.ProviderStatusHTTPError(rerr); ok {
			return e.failedStream(httpErr, stage, reason, cadence.Raw())
		}
		return e.failedStream(rerr, stage, reason, cadence.Raw())
	}
	_ = res

	// A caller cancellation observed AFTER a clean completion is still POST-CLAIM: the
	// socket was owned, events were delivered, and the final parse below has not run. It
	// must be terminal, not a success — returning Succeeded here would hand the caller a
	// final for a request it already abandoned, and would be the one path on which a
	// cancelled stream did not report as cancelled.
	if err := ctx.Err(); err != nil {
		return e.failedStream(err, stageStream, reasonStreamCancelled, cadence.Raw())
	}

	// --- Clean completion: the FINAL parse over the accumulated parseable text, then
	// the EMBEDDED unary final decoder. A stream final has the ordinary final
	// value-union type, so it uses DecodeFinal — never the partial decoder. Either
	// failure is post-claim TERMINAL. ---
	parsed, ferr := debaml.ParseStaticStreamFinal(ctx, bundle, cadence.Parseable())
	if ferr != nil {
		// The totality predicate proves the native final parser reaches a carrier or a
		// terminal parse error for every admitted response, so a support decline cannot
		// survive the claim; it is reported distinctly for observability only.
		if errors.Is(ferr, bamlutils.ErrDeBAMLParseUnsupported) {
			return e.failedStream(errFinalDecline, stageParse, reasonFinalParseDecline, cadence.Raw())
		}
		return e.failedStream(ferr, stageParse, reasonFinalParse, cadence.Raw())
	}
	final, derr := rm.binding.DecodeFinal(parsed.JSON)
	if derr != nil {
		return e.failedStream(derr, stageDecode, reasonDecodeError, cadence.Raw())
	}
	e.metrics.successes.Add(1)
	// Raw/reasoning ride the FINAL only for a raw-wanted mode, matching the shared
	// orchestrator's emitFinal gate.
	if !needsRaw {
		return bamlutils.SucceededSpineStreamResult(final, "", "")
	}
	return bamlutils.SucceededSpineStreamResult(final, cadence.Raw(), cadence.Reasoning())
}

// ParseStream is the local, SOCKET-FREE stream parse route: the admitted method's native
// static-stream PARTIAL parser plus the emitted pointer-carrier decoder over raw. It
// opens no client and no socket.
//
// A non-admitted method returns the typed capability-decline. For an ADMITTED method the
// no-partial sentinel is an ordinary parse RESULT error for this direct request — it is
// not a transport decline and there is no socket to decline.
func (e *StreamExecutor) ParseStream(ctx context.Context, method string, raw string) (any, error) {
	rm, ok := e.registry[method]
	if !ok {
		return nil, &bamlutils.NativeSpineUnsupportedMethodError{Method: method, Reason: reasonUnsupportedMethod}
	}
	if rm.stream == nil || rm.stream.DecodePartial == nil {
		return nil, errNoStreamBinding
	}
	// The same dynamic-schema guard the final parse route applies: this route parses
	// against the descriptor's fixed cohort schema, so a caller-supplied output schema
	// must FAIL rather than be silently parsed under the wrong schema.
	if ad, ok := ctx.(bamlutils.Adapter); ok && ad != nil {
		if ad.DeBAMLOutputSchema() != nil {
			return nil, errDynamicSchema
		}
	}
	parsed, err := debaml.ParseStaticStreamPartial(ctx, rm.bundle, raw)
	if err != nil {
		return nil, err
	}
	return rm.stream.DecodePartial(parsed.JSON)
}

// nativeStreamMode maps the PUBLIC stream mode onto the neutral native stream mode,
// admitting exactly the two real streaming modes. /call, /call-with-raw, and any unknown
// mode are not stream modes and decline before admission.
func nativeStreamMode(m bamlutils.StreamMode) (bamlutils.NativeStreamMode, bool) {
	switch m {
	case bamlutils.StreamModeStream:
		return bamlutils.NativeStreamModeStream, true
	case bamlutils.StreamModeStreamWithRaw:
		return bamlutils.NativeStreamModeStreamWithRaw, true
	default:
		return "", false
	}
}

// staticStreamInput maps a registered method + projected values + the request-scoped
// adapter facts into the static-STREAM admission input. It mirrors the unary lane's
// baseStaticInput exactly, differing only in the two genuinely streaming facts (the real
// public mode and NeedsRaw) and in carrying no BAML plan closure — this lane has none.
//
// The native plan is DESCRIPTOR-driven: Descriptor is the BAKED reconstructed rm.fn.
// HasRoundRobin / HasFallbackChain are properties of the SELECTED CLIENT's plan, and an
// admitted method's client is a proven single resolved leaf (registration omits a
// fallback / round-robin strategy client), so both are false. The rewrite/proxy
// predicate defaults to the process-global client so admission can never SKIP the gate;
// a request-scoped client overrides it.
func (e *StreamExecutor) staticStreamInput(rm *registeredMethod, values []promptdescriptor.ArgumentValue, ad bamlutils.Adapter, mode bamlutils.NativeStreamMode) admission.StaticStreamInput {
	args := make(map[string]any, len(values))
	order := make([]string, 0, len(values))
	for _, v := range values {
		args[v.Name] = v.Value
		order = append(order, v.Name)
	}
	in := admission.StaticStreamInput{
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
		Mode:                mode,
		NeedsRaw:            mode == bamlutils.NativeStreamModeStreamWithRaw,
		SingleLeaf:          true,
		Provider:            rm.fn.Provider,
		WouldRewriteOrProxy: llmhttp.DefaultClient.WouldRewriteOrProxy,
	}
	if ad != nil {
		in.HasRequestRetryOverride = ad.RetryConfig() != nil
		if hc := ad.HTTPClient(); hc != nil {
			in.WouldRewriteOrProxy = hc.WouldRewriteOrProxy
		}
	}
	return in
}

// streamFailStageReason maps a terminal RunStream error onto the executor's bounded
// stage/reason tokens. The typed error carries the real failure class; these are
// secret-free observability tokens only.
func streamFailStageReason(err error) (stage, reason string) {
	var te *execute.TerminalError
	if !errors.As(err, &te) {
		// A RunStream wiring/guard failure (nil client / nil plan / empty body). It
		// happens at or after the logical claim, so it is terminal all the same.
		return stageStream, reasonUnknownOutcome
	}
	switch te.Phase {
	case execute.StreamPhaseConnect:
		return stageTransport, reasonTransportError
	case execute.StreamPhaseStatus:
		return stageProvider, reasonProviderError
	case execute.StreamPhaseProtocol:
		return stageTransport, reasonStreamProtocol
	case execute.StreamPhaseFirstBody:
		return stageTransport, reasonStreamFirstBody
	case execute.StreamPhaseIdle:
		return stageTransport, reasonStreamIdle
	case execute.StreamPhaseDecode:
		return stageTransport, reasonStreamDecode
	case execute.StreamPhaseEmit:
		// The cadence stopped the stream: a strict partial parse/decode failure, or the
		// emit callback itself failing or observing cancellation.
		return stageStream, reasonStreamEmit
	case execute.StreamPhaseCancel:
		return stageStream, reasonStreamCancelled
	default:
		return stageStream, reasonUnknownOutcome
	}
}

// accumulatedRaw returns the cadence's accumulated raw, tolerating a nil cadence (a
// panic before the cadence was built).
func accumulatedRaw(c *buildrequest.StreamCadence) string {
	if c == nil {
		return ""
	}
	return c.Raw()
}

func (e *StreamExecutor) declinedStream(err error, stage, reason string) bamlutils.NativeSpineStreamResult {
	e.metrics.declines.Add(1)
	return bamlutils.DeclinedSpineStreamResult(err, stage, reason)
}

func (e *StreamExecutor) failedStream(err error, stage, reason, rawDiagnostic string) bamlutils.NativeSpineStreamResult {
	e.metrics.failures.Add(1)
	return bamlutils.FailedAfterClaimSpineStreamResult(err, stage, reason, rawDiagnostic)
}
