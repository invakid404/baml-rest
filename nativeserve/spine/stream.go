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
	"github.com/invakid404/baml-rest/internal/schema"
	"github.com/invakid404/baml-rest/nativeserve/admission"
	"github.com/invakid404/baml-rest/nativeserve/execute"
	"github.com/invakid404/baml-rest/nativeserve/streamoracle"
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

	// ExecBridge-U1s stream-oracle tokens. They EXTEND the M3e-A set; none of them can
	// be produced by the native-only lane, which carries no oracle.
	reasonNoBAMLStreamPlan  = "no_baml_stream_plan_closure"
	reasonNoBAMLStreamParse = "no_baml_stream_parse_closure"
	reasonNoStreamDecoder   = "no_standard_stream_decoder"
	reasonOracleTerminal    = "stream_oracle_terminal"
	reasonFinalOracle       = "stream_final_oracle_error"
)

var (
	errNoStreamBinding = errors.New("nativespine: method is registered without a stream binding; the exact cohort serves streams only for a stream-capable generated method")
	errNilEmit         = errors.New("nativespine: stream requires a non-nil emit callback")
	errStreamMode      = errors.New("nativespine: the requested public mode is not a stream mode; the exact cohort streams only /stream and /stream-with-raw")
	errFinalDecline    = errors.New("nativespine: native stream final parser declined the accumulated response (no BAML fallback on this cohort)")

	// ExecBridge-U1s pre-socket wiring sentinels. Each one means the per-prefix/final
	// oracle could not be assembled, which is a DECLINE (BAML serves) rather than a
	// claimed stream served without its safety rail.
	errNoBAMLStreamPlan   = errors.New("nativespine: oracle stream requires a BAML no-send StreamRequest plan closure; the standard composite must supply BuildBAMLStreamRequest before the claim")
	errNoBAMLStreamOracle = errors.New("nativespine: oracle stream requires BAML per-prefix and final parse closures; the standard composite must supply both before the claim")
	errNoStreamDecoders   = errors.New("nativespine: oracle stream requires the standard method's partial and final carrier decoders; the standard composite must supply both before the claim")
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

	// admitStreamClaimOracle is the LIVE-oracle pre-socket admission step (the
	// ExecBridge-U1s standard StreamWithOracle), defaulting to the BAML plan-comparing
	// admission.AdmitStaticSpineStreamOracleClaim. Like admitStreamClaim it is a field
	// ONLY so gated tests can inject a synthetic claim and drive the post-claim fault and
	// drift matrices deterministically; every production constructor leaves the default.
	admitStreamClaimOracle func(ctx context.Context, in admission.StaticStreamInput) (*admission.StaticStreamClaim, error)
}

// compile-time assertions: the stream executor satisfies the STREAM contract, and (via
// the embedded unary executor) the frozen unary and oracle contracts unchanged.
var (
	_ bamlutils.NativeSpineStreamExecutor      = (*StreamExecutor)(nil)
	_ bamlutils.NativeSpineUnaryExecutor       = (*StreamExecutor)(nil)
	_ bamlutils.NativeSpineUnaryOracleExecutor = (*StreamExecutor)(nil)
	// ... and the ExecBridge-U1s STREAM oracle contract the standard composite drives.
	_ bamlutils.NativeSpineStreamOracleExecutor = (*StreamExecutor)(nil)
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
	// The normalized records are TRANSIENT: they exist only for the duration of this
	// constructor and are never retained, so pointing them at the caller's slice is safe.
	// Detachment from caller memory happens at the ONE ownership boundary where something
	// IS retained — copyStreamBinding, in classifyRegistration, where the registry entry
	// is built. Do not re-add a copy here: a redundant one shields that boundary and stops
	// the mutate-after-boot regression from biting when it is reverted.
	normalized := make([]candidateRegistration, len(registrations))
	for i := range registrations {
		normalized[i] = candidateRegistration{
			binding: registrations[i].Binding.Unary,
			stream:  &registrations[i].Binding,
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
		UnaryExecutor:          base,
		firstBodyTimeout:       llmhttp.StreamFirstBodyTimeoutFromEnv(),
		idleTimeout:            llmhttp.StreamIdleTimeoutFromEnv(),
		admitStreamClaim:       admission.AdmitStaticSpineStreamClaim,
		admitStreamClaimOracle: admission.AdmitStaticSpineStreamOracleClaim,
	}, nil
}

// Stream performs ONE claimed native stream for an admitted method and returns the
// neutral tri-state result, after delivering every public event through emit.
//
// Ownership discipline: a pre-socket DECLINE guarantees zero RoundTrips and zero emitted
// events; from the claim onward every terminal condition is SUCCEEDED or
// FAILED-AFTER-CLAIM — never a decline, never a resend. The logical claim is set inside the
// shared core ([StreamExecutor.runClaimedStream]) immediately BEFORE execute.RunStream,
// deliberately earlier than any internal RunStream wiring error: once this executor enters
// the one-send operation, its failure is terminal rather than a possible fallback.
//
// This function contributes only its FROZEN policies — the BAML-free admission entry, the
// native-only partial closure, and the native-only final parse plus the embedded decoder.
// Its observable behaviour is unchanged from M3e-A and is pinned by the existing tests.
func (e *StreamExecutor) Stream(ctx context.Context, method string, input any, emit bamlutils.NativeSpineStreamEmit) (result bamlutils.NativeSpineStreamResult) {
	// This guard covers only THIS function's PRE-CLAIM front end: the shared core installs
	// its own claim-aware guard and never lets a panic escape, so a panic reaching here is
	// provably pre-socket and declining is safe.
	defer func() {
		if r := recover(); r != nil {
			// The recovered value is DROPPED, never interpolated: only the bounded
			// sentinel + stage/reason leave here.
			_ = r
			result = bamlutils.DeclinedSpineStreamResult(errPanicBeforeClaim, stageStream, reasonPanic)
			e.metrics.declines.Add(1)
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

	// The two genuinely request-scoped streaming facts, read once here so the lane below
	// carries them rather than re-deriving them per tick.
	needsRaw := nativeMode == bamlutils.NativeStreamModeStreamWithRaw
	includeReasoning := ad != nil && ad.IncludeReasoning()

	// Lower this request into the shared claimed-stream core. Everything from admission
	// onward — the claim, its close, the cadence, the ONE RunStream, the final, the single
	// post-claim cancellation check, and the claim-aware panic classification — is the
	// core's; the ONLY things this lane contributes are its FROZEN policies: the BAML-free
	// admission entry, the native-only partial closure (parser sentinel resolved BEFORE the
	// decoder), and the native-only final parse + embedded decoder.
	out := e.runClaimedStream(ctx, claimedStreamLane{
		admit: func(actx context.Context) (*admission.StaticStreamClaim, error) {
			return e.admitStreamClaim(actx, e.staticStreamInput(rm, values, ad, nativeMode))
		},
		parsePartial: func(bundle *schema.Bundle) buildrequest.StreamCadenceParseFunc {
			decodePartial := rm.stream.DecodePartial
			return func(pctx context.Context, accumulated string) (any, bool, error) {
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
			}
		},
		resolveFinal: func(fctx context.Context, bundle *schema.Bundle, full string) finalResolution {
			parsed, ferr := debaml.ParseStaticStreamFinal(fctx, bundle, full)
			if ferr != nil {
				// The totality predicate proves the native final parser reaches a carrier or
				// a terminal parse error for every admitted response, so a support decline
				// cannot survive the claim; it is reported distinctly for observability only.
				if errors.Is(ferr, bamlutils.ErrDeBAMLParseUnsupported) {
					return finalResolution{err: errFinalDecline, stage: stageParse, reason: reasonFinalParseDecline}
				}
				return finalResolution{err: ferr, stage: stageParse, reason: reasonFinalParse}
			}
			final, derr := rm.binding.DecodeFinal(parsed.JSON)
			if derr != nil {
				return finalResolution{err: derr, stage: stageDecode, reason: reasonDecodeError}
			}
			return finalResolution{value: final}
		},
		emit:             emit,
		needsRaw:         needsRaw,
		includeReasoning: includeReasoning,
	})
	if out.declined {
		return bamlutils.DeclinedSpineStreamResult(out.err, out.stage, out.reason)
	}
	if out.err != nil {
		return bamlutils.FailedAfterClaimSpineStreamResult(out.err, out.stage, out.reason, out.rawDiagnostic)
	}
	return bamlutils.SucceededSpineStreamResult(out.final, out.raw, out.reasoning)
}

// StreamWithOracle performs ONE claimed native stream for an admitted method through the
// ExecBridge-U1s / M3e-B LIVE-oracle composite, and it is the STANDARD worker's whole
// default-serve stream path.
//
// It is the streaming twin of [UnaryExecutor.CallWithOracle] and stands in exactly the same
// relation to [StreamExecutor.Stream]: the SAME immutable registry, the SAME exact
// transport, the SAME claim boundary, the SAME panic discipline, and the SAME atomic
// counters — differing ONLY in policy. Where Stream is the frozen, BAML-free native-only
// lane, this one adds back the two safety rails a BAML-linked standard worker CAN build:
//
//   - BEFORE the claim, a live BAML `StreamRequest.<Method>` no-send plan compare
//     (admission.AdmitStaticSpineStreamOracleClaim); a mismatch declines pre-socket;
//   - AFTER the claim, on EVERY structured cadence tick and on the final, BOTH the native
//     parser and BAML's `ParseStream.<Method>` / `Parse.<Method>` run over the SAME
//     accumulated prefix and their PUBLIC marshaled bytes are compared before the
//     irreversible emit (nativeserve/streamoracle owns that matrix).
//
// WHY THE ORACLE LIVES INSIDE THE CADENCE, not around this call. A wrapper around Stream
// would see the typed partial only AFTER the cadence parsed and emitted it, and a plain
// /stream deliberately carries no raw text — so it could not reconstruct the exact
// accumulated prefix and therefore could not prove the two engines saw the same bytes. The
// cadence's ParsePartial callback is the ONE seam that has the exact prefix and is still
// before the public event.
//
// Ownership discipline is identical to Stream's and is NOT relaxed by the oracle: a
// pre-socket DECLINE certifies zero provider sockets and zero emitted events and is the
// only fallback-legal outcome (the composite hands it back to the already-running BAML
// orchestrator, which serves the same child once); from the claim onward EVERY fault —
// transport, provider status, cadence, oracle, final, cancellation, or a panic — is
// FailedAfterClaim, with no BAML transport, no retry, no fallback child, no reset, and no
// pool replay. Substituting BAML's typed value for a drifted prefix is NOT a fallback: it
// comes from the response already in hand and issues no second request.
func (e *StreamExecutor) StreamWithOracle(ctx context.Context, inv bamlutils.NativeStaticStreamOracleInvocation, emit bamlutils.NativeSpineStreamEmit) (result bamlutils.NativeSpineStreamOracleResult) {
	// obs accumulates the bounded metric observations INCREMENTALLY, so a post-claim panic
	// still carries out what happened up to it (the socket opened, the plan matched, N
	// prefixes were compared, drift was seen). The guard re-attaches it below.
	var obs bamlutils.NativeSpineStreamOracleObservations
	// This guard covers only THIS function's PRE-CLAIM front end: the core installs its own
	// claim-aware guard, so nothing that could be post-claim ever unwinds to here. A panic
	// reaching this point is therefore provably pre-socket, and declining is safe.
	defer func() {
		if r := recover(); r != nil {
			// The recovered value is DROPPED, never interpolated: the standard adapter
			// propagates terminal errors to the outer policy, so an arbitrary/sensitive
			// panic payload must not escape.
			_ = r
			result = bamlutils.DeclinedSpineStreamOracleResult(errPanicBeforeClaim, stageStream, reasonPanic)
			result.Observations = obs
			e.metrics.declines.Add(1)
		}
	}()

	// --- Pre-socket registry + preflight gates. Every one of these is a DECLINE. ---
	rm, ok := e.registry[inv.Method]
	if !ok {
		return e.declinedStreamOracle(&bamlutils.NativeSpineUnsupportedMethodError{Method: inv.Method, Reason: reasonUnsupportedMethod}, stageRegistry, reasonUnsupportedMethod, obs)
	}
	// The registered STREAM binding stays load-bearing even though the standard lane
	// decodes with the generated method's own carrier decoders: it is what the population
	// classifier admitted this method on, so a method without it is not in the stream
	// population at all and must not claim a socket.
	if rm.stream == nil || rm.stream.DecodePartial == nil {
		return e.declinedStreamOracle(errNoStreamBinding, stageRegistry, reasonNoStreamBinding, obs)
	}
	if emit == nil {
		return e.declinedStreamOracle(errNilEmit, stagePreflight, reasonNilEmitCallback, obs)
	}
	if err := ctx.Err(); err != nil {
		return e.declinedStreamOracle(err, stagePreflight, reasonContextCancelled, obs)
	}
	// The public mode is a TRUTHFUL invocation fact the generated seam populated (it
	// installs only for a real /stream{,-with-raw} request); anything else declines.
	if inv.Mode != bamlutils.NativeStreamModeStream && inv.Mode != bamlutils.NativeStreamModeStreamWithRaw {
		return e.declinedStreamOracle(errStreamMode, stagePreflight, reasonModeNotStream, obs)
	}
	// Request-scoped exact-cohort declines, read from the TRUTHFUL invocation facts rather
	// than re-derived here: the exact cohort serves only the descriptor's default client
	// against the static schema.
	if inv.HasClientRegistryOverride {
		return e.declinedStreamOracle(errClientRegistry, stageAdmission, reasonClientRegistry, obs)
	}
	if inv.HasDynamicOutputSchema {
		return e.declinedStreamOracle(errDynamicSchema, stageAdmission, reasonDynamicSchema, obs)
	}
	// EVERY oracle/decoder callback MUST be present BEFORE the claim. Discovering a missing
	// one after a socket is open is safe from a double send but violates the default-on
	// safety promise, so it declines PRE-CLAIM. Production codegen always supplies all five;
	// these bite only a direct caller.
	if inv.BuildBAMLStreamRequest == nil {
		return e.declinedStreamOracle(errNoBAMLStreamPlan, stageAdmission, reasonNoBAMLStreamPlan, obs)
	}
	if inv.BAMLStreamParse == nil || inv.BAMLFinalParse == nil {
		return e.declinedStreamOracle(errNoBAMLStreamOracle, stageAdmission, reasonNoBAMLStreamParse, obs)
	}
	if inv.DecodeNativeStreamPartial == nil || inv.DecodeNativeStreamFinal == nil {
		return e.declinedStreamOracle(errNoStreamDecoders, stageAdmission, reasonNoStreamDecoder, obs)
	}

	needsRaw := inv.Mode == bamlutils.NativeStreamModeStreamWithRaw
	// oracleUsed is the STICKY attribution latch. It starts false (the stream begins as
	// native) and is set by the FIRST substitution, suppression, byte mismatch, or
	// native-only failure. It is NEVER cleared by a later match: a stream that needed the
	// oracle even once was not served purely natively.
	oracleUsed := false

	out := e.runClaimedStream(ctx, claimedStreamLane{
		admit: func(actx context.Context) (*admission.StaticStreamClaim, error) {
			return e.admitStreamClaimOracle(actx, e.oracleStaticStreamInput(rm, inv))
		},
		parsePartial: func(bundle *schema.Bundle) buildrequest.StreamCadenceParseFunc {
			legs := e.oracleLegs(bundle, inv)
			return func(pctx context.Context, accumulated string) (any, bool, error) {
				// The ENTERED count is incremented BEFORE resolution, and the terminal flag
				// is set by a panic-safe guard, so an engine that PANICS still leaves the
				// ledger consistent with what happened: the tick was entered and the oracle
				// was lost. Setting them after would make a panicking stream report zero
				// comparisons and no oracle terminal, and streamOracleServeOutcome would
				// then classify an oracle parse failure as an internal error.
				obs.PrefixComparisons++
				res, err := resolvePrefixWithOracleGuard(pctx, legs, accumulated, &obs)
				if err != nil {
					// The oracle could not be established for this prefix. Under the STRICT
					// cadence policy this stops RunStream and becomes a post-claim terminal:
					// a claimed stream that lost its oracle must not keep emitting.
					obs.OracleTerminal = true
					return nil, false, err
				}
				recordPrefixCompare(&obs, res)
				if res.Drift {
					oracleUsed = true
				}
				if res.Action == streamoracle.ActionNoEvent {
					return nil, false, nil
				}
				return res.Value, true, nil
			}
		},
		resolveFinal: func(fctx context.Context, bundle *schema.Bundle, full string) finalResolution {
			legs := e.oracleLegs(bundle, inv)
			// Set BEFORE either leg runs, so a panic in one still carries the phase out.
			obs.FinalOracleRan = true
			res, err := resolveFinalWithOracleGuard(fctx, legs, full, &obs)
			if err != nil {
				obs.OracleTerminal = true
				// Wrapped as an output-parse error so the worker classifies it exactly like
				// every other terminal final-parse failure on the stream surface.
				return finalResolution{err: &buildrequest.OutputParseError{Err: err}, stage: stageParse, reason: reasonFinalOracle}
			}
			obs.FinalCompare = res.Compare
			if res.Drift {
				oracleUsed = true
			}
			if res.Substituted {
				obs.Substituted = true
			}
			return finalResolution{value: res.Value}
		},
		emit:             emit,
		needsRaw:         needsRaw,
		includeReasoning: inv.IncludeReasoning,
		// This lane REPLACES the stock BAML stream path for the exact cohort, so it owes
		// the same liveness signals that path emits — the 2xx heartbeat the pool's hung
		// detector watches, and the first-body marker.
		sendHeaders:   inv.SendHeaders,
		sendFirstBody: inv.SendFirstBody,
		onClaimed: func() {
			// The plan byte-matched (that is why the claim was granted) and exactly one
			// socket is about to open.
			obs.PlanCompareRan = true
			obs.PlanMatched = true
			obs.SocketOpened = true
		},
	})

	if out.declined {
		if out.planCompareRan {
			obs.PlanCompareRan = true
		}
		res := bamlutils.DeclinedSpineStreamOracleResult(out.err, out.stage, out.reason)
		res.Observations = obs
		return res
	}
	obs.SocketResponded = out.transportCompleted
	if out.err != nil {
		obs.ServeOutcome = streamOracleServeOutcome(out.stage, obs.OracleTerminal)
		res := bamlutils.FailedAfterClaimSpineStreamOracleResult(out.err, out.stage, out.reason, out.rawDiagnostic)
		res.Observations = obs
		return res
	}
	obs.ServeOutcome = bamlutils.NativeStaticOutcomeSuccess
	winner := bamlutils.NativeStaticServeEngineNative
	if oracleUsed {
		winner = bamlutils.NativeStaticServeEngineBAMLParse
	}
	res := bamlutils.SucceededSpineStreamOracleResult(out.final, out.raw, out.reasoning, winner)
	res.Observations = obs
	return res
}

// oracleLegs assembles the U1s per-prefix/final legs over ONE claimed bundle: the
// root-owned native static-stream parsers composed with the STANDARD generated method's own
// carrier decoders, and the BAML-only closures that method supplied. It is built per claim
// because the bundle is a claim property; every closure it needs was proved non-nil
// pre-admission.
//
// The standard lane decodes with inv's decoders rather than the registered spine binding's:
// the emitted hermetic carrier is a different Go type, which the standard method's
// NewResultFunc could not type-assert. The registered decoder stays load-bearing for
// population validation and for the native-only runtime.
func (e *StreamExecutor) oracleLegs(bundle *schema.Bundle, inv bamlutils.NativeStaticStreamOracleInvocation) streamoracle.Legs {
	return streamoracle.Legs{
		NativePrefix: func(pctx context.Context, prefix string) (any, bool, error) {
			parsed, err := debaml.ParseStaticStreamPartial(pctx, bundle, prefix)
			if err != nil {
				// The parser's documented no-partial sentinel is resolved HERE — before the
				// decoder — into the explicit no-value result, so a DECODER failure can
				// never be mistaken for a benign no-value.
				if errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
					return nil, false, nil
				}
				return nil, false, err
			}
			carrier, derr := inv.DecodeNativeStreamPartial(parsed.JSON)
			if derr != nil {
				return nil, false, derr
			}
			return carrier, true, nil
		},
		NativeFinal: func(fctx context.Context, full string) (any, error) {
			parsed, err := debaml.ParseStaticStreamFinal(fctx, bundle, full)
			if err != nil {
				return nil, err
			}
			return inv.DecodeNativeStreamFinal(parsed.JSON)
		},
		BAMLPrefix: inv.BAMLStreamParse,
		BAMLFinal:  inv.BAMLFinalParse,
	}
}

// resolvePrefixWithOracleGuard runs one prefix resolution and, if an engine PANICS, marks
// the oracle terminal in the bounded ledger BEFORE letting the panic continue to the claimed
// executor's payload-dropping guard.
//
// It deliberately RE-PANICS rather than converting the panic into an error. Swallowing it
// here would turn a broken oracle into an ordinary terminal and lose the claimed guard's
// bounded classification; this function's only job is to make the observations tell the
// truth about a stream the panic is about to end. The recovered value is never inspected,
// logged, or interpolated — it is re-panicked as-is so the claimed guard drops it.
func resolvePrefixWithOracleGuard(ctx context.Context, legs streamoracle.Legs, prefix string, obs *bamlutils.NativeSpineStreamOracleObservations) (res streamoracle.PrefixOutcome, err error) {
	completed := false
	defer func() {
		if !completed {
			obs.OracleTerminal = true
		}
	}()
	res, err = streamoracle.ResolvePrefix(ctx, legs, prefix)
	completed = true
	return res, err
}

// resolveFinalWithOracleGuard is the same guard for the final resolution.
func resolveFinalWithOracleGuard(ctx context.Context, legs streamoracle.Legs, full string, obs *bamlutils.NativeSpineStreamOracleObservations) (res streamoracle.FinalOutcome, err error) {
	completed := false
	defer func() {
		if !completed {
			obs.OracleTerminal = true
		}
	}()
	res, err = streamoracle.ResolveFinal(ctx, legs, full)
	completed = true
	return res, err
}

// recordPrefixCompare folds ONE resolved prefix comparison into the bounded observations.
// Every counter it touches is a bounded classification; none carries request content.
func recordPrefixCompare(obs *bamlutils.NativeSpineStreamOracleObservations, res streamoracle.PrefixOutcome) {
	switch res.Compare {
	case bamlutils.NativeStreamCompareMatch:
		obs.PrefixMatch++
	case bamlutils.NativeStreamCompareNativeNoValue:
		obs.PrefixNativeNoValue++
	case bamlutils.NativeStreamCompareBAMLNoValue:
		obs.PrefixBAMLNoValue++
	case bamlutils.NativeStreamCompareBytesMismatch:
		obs.PrefixBytesMismatch++
	case bamlutils.NativeStreamCompareNativeError:
		obs.PrefixNativeError++
	}
	if res.Substituted {
		obs.Substituted = true
	}
	if res.Suppressed {
		obs.Suppressed = true
	}
}

// streamOracleServeOutcome maps a claimed terminal's bounded stage onto the serve-outcome
// the standard composite replays into RecordServeOutcome. An oracle terminal is a PARSE
// error regardless of the stage it surfaced at, because that is what it is: the safety
// comparison could not be established over the response.
func streamOracleServeOutcome(stage string, oracleTerminal bool) bamlutils.NativeStaticServeOutcome {
	if oracleTerminal {
		// The safety comparison could not be established over the response. Whatever
		// stage it surfaced at (a prefix oracle reaches the caller through the cadence's
		// emit phase), that is a PARSE failure, and saying so is what keeps the drift
		// signal readable.
		return bamlutils.NativeStaticOutcomeParseError
	}
	switch stage {
	case stageProvider:
		return bamlutils.NativeStaticOutcomeProviderError
	case stageTransport:
		return bamlutils.NativeStaticOutcomeTransportError
	case stageParse, stageDecode:
		return bamlutils.NativeStaticOutcomeParseError
	default:
		// An emit failure, a cancellation, or a post-claim panic is none of the above.
		// Return NONE so the composite records it as internal_error rather than
		// inventing a parse failure that did not happen — a claimed terminal must be
		// COUNTED, but it must not be counted as the wrong thing.
		return bamlutils.NativeStaticOutcomeNone
	}
}

// finalResolution is one lane's resolution of the completed accumulated text: the value on
// success, or a terminal error with the lane's own bounded stage/reason. Returning the
// tokens WITH the error is what lets the shared core classify a lane-specific final failure
// without learning either lane's vocabulary.
type finalResolution struct {
	value  any
	err    error
	stage  string
	reason string
}

// claimedStreamLane is the private, policy-driven description of ONE claimed stream. It is
// the ONLY thing that differs between the frozen native-only [StreamExecutor.Stream] and the
// live-oracle [StreamExecutor.StreamWithOracle]; everything else — the claim, its close, the
// cadence, the one RunStream, the final, the single post-claim cancellation check, and the
// claim-aware panic classification — is shared, so the two lanes cannot drift into two
// streaming safety implementations.
type claimedStreamLane struct {
	// admit is the lane's PRE-SOCKET admission entry, already bound to this request's
	// admission input. It returns a live claim whose engine the core Closes on every path.
	admit func(ctx context.Context) (*admission.StaticStreamClaim, error)

	// parsePartial builds the cadence's per-tick parse callback over the CLAIMED bundle. It
	// is called once, immediately before the claim marker; every callback it closes over
	// was validated non-nil before admission ran, so nothing is first discovered late.
	parsePartial func(bundle *schema.Bundle) buildrequest.StreamCadenceParseFunc

	// resolveFinal resolves the completed accumulated text over the same claimed bundle.
	resolveFinal func(ctx context.Context, bundle *schema.Bundle, full string) finalResolution

	emit             bamlutils.NativeSpineStreamEmit
	needsRaw         bool
	includeReasoning bool

	// onClaimed records the lane's bounded claim-time observations. It runs with the metric
	// bookkeeping, BEFORE the `claimed = true` marker — never in the gap between that marker
	// and execute.RunStream, where a panic would be misclassified as a pre-socket decline.
	onClaimed func()

	// sendHeaders / sendFirstBody are the caller's LIVENESS signals, fired by the exact
	// client on the first 2xx response headers and on the first raw body byte. They are the
	// same two the stock BAML stream path and the legacy native seam emit, and a lane that
	// REPLACES the stock path must emit them too: without the 2xx heartbeat the pool's hung
	// detector sees no liveness on a slow body and can treat a healthy stream as hung.
	//
	// Nil on the frozen native-only lane, whose caller supplies none and whose behaviour is
	// unchanged; execute.StreamConfig accepts nil for both.
	sendHeaders   func()
	sendFirstBody func()
}

// claimedStreamOutcome is the shared core's neutral result. Each public entry maps it into
// its own result type, so the M3e-A native-only contract never acquires the oracle's fields.
type claimedStreamOutcome struct {
	// declined certifies ZERO provider sockets and ZERO emitted events. It is the only
	// fallback-legal outcome; every other terminal is post-claim.
	declined bool
	// planCompareRan reports that a plan-compare decline reached the caller, so an oracle
	// lane can record the compare it actually performed.
	planCompareRan bool
	// transportCompleted reports that RunStream returned cleanly (the socket responded).
	transportCompleted bool

	final     any
	raw       string
	reasoning string

	err           error
	stage         string
	reason        string
	rawDiagnostic string
}

// runClaimedStream is the shared claimed-stream core: ONE admission, ONE claim close, ONE
// cadence, ONE execute.RunStream, ONE final resolution, ONE post-claim cancellation check,
// and ONE claim-aware panic guard.
//
// ORDER IS LOAD-BEARING and is pinned structurally by
// TestStreamClaimMarkerIsTheLastStatementBeforeRunStream: the bookkeeping (and the lane's
// claim-time observations) happen FIRST, so `claimed = true` is the FINAL statement before
// entering the one-send operation. Nothing may be inserted between them — a statement in
// that gap could panic while the guard still reads `claimed == false`, turning a post-claim
// fault into a DECLINE and inviting a resend for a request that may already own a socket.
func (e *StreamExecutor) runClaimedStream(ctx context.Context, lane claimedStreamLane) (out claimedStreamOutcome) {
	claimed := false
	// transportCompleted records that RunStream returned cleanly. Like cadence it is read
	// by the panic guard, so a panic during the FINAL resolution still reports truthfully
	// that the socket HAD responded — the alternative is an observation claiming the
	// provider never answered for a request whose final was being compared.
	transportCompleted := false
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
				out = claimedStreamOutcome{
					transportCompleted: transportCompleted,
					err:                errPanicAfterClaim,
					stage:              stageStream,
					reason:             reasonPanic,
					rawDiagnostic:      accumulatedRaw(cadence),
				}
				e.metrics.failures.Add(1)
			} else {
				// Pre-claim panic: no socket and no event occurred, so declining is safe.
				out = claimedStreamOutcome{declined: true, err: errPanicBeforeClaim, stage: stageStream, reason: reasonPanic}
				e.metrics.declines.Add(1)
			}
		}
	}()

	claim, aerr := lane.admit(ctx)
	if aerr != nil {
		var d *admission.StaticDecline
		if errors.As(aerr, &d) {
			// A typed pre-socket admission decline: zero sockets, zero events. A
			// plan-MISMATCH decline carries its evidence out so an oracle lane can record
			// the compare that actually ran.
			return e.declinedCore(d, d.Stage, d.Reason, d.Stage == string(admission.StagePlanCompare))
		}
		// An unexpected planner/FFI error before any socket.
		return e.declinedCore(aerr, stagePlanner, reasonPlannerError, false)
	}
	// The claim keeps the request-scoped engine alive for exactly one DoStream. Close it
	// on EVERY path.
	defer claim.Close()

	// Provably PRE-SOCKET preflight rejections (unsigned OpenAI plans never expire).
	if claim.PlanExpired() {
		return e.declinedCore(errPlanExpired, stagePreflight, reasonPlanExpired, false)
	}
	if err := ctx.Err(); err != nil {
		return e.declinedCore(err, stagePreflight, reasonContextCancelled, false)
	}

	// --- Build EVERY cadence/parser/emit closure BEFORE the claim. No callback may
	// first be discovered after a socket might be open. ---
	bundle := claim.Bundle
	cadence = buildrequest.NewStreamCadence(buildrequest.StreamCadenceConfig{
		NeedsPartials: true,
		NeedsRaw:      lane.needsRaw,
		// Both lanes parse on EVERY tick, matching the generated adapters (none of which
		// configures a throttle) and therefore stock BAML's partial cadence. On the oracle
		// lane it additionally means every structured tick is compared — a throttle would
		// silently release unverified prefixes.
		ParseThrottleInterval: 0,
		ParsePartial:          lane.parsePartial(bundle),
		// STRICT: every error that reaches the cadence is terminal. The benign no-partial
		// case never arrives as an error — it is the callback's explicit result.
		ParsePolicy: buildrequest.CadenceParseErrorsAreTerminal,
		Emit: func(ev buildrequest.StreamCadenceEvent) error {
			return lane.emit(bamlutils.NativeSpineStreamEvent{
				HasPartial: ev.HasPartial,
				Partial:    ev.Partial,
				Raw:        ev.Raw,
				Reasoning:  ev.Reasoning,
			})
		},
	})

	// --- CLAIM the native attempt (ownership boundary). From here every terminal
	// condition is SUCCEEDED or FAILED-AFTER-CLAIM. ---
	e.metrics.claims.Add(1)
	if lane.onClaimed != nil {
		lane.onClaimed()
	}
	claimed = true
	res, rerr := execute.RunStream(ctx, execute.StreamConfig{
		Client:           claim.Client(),
		Request:          claim.Request(),
		Expected:         claim.ExactRequest,
		IncludeReasoning: lane.includeReasoning,
		EmitDelta: func(d execute.StreamDelta) error {
			return cadence.Delta(ctx, d.ParseableDelta, d.RawDelta, d.ReasoningDelta)
		},
		FirstBodyTimeout: e.firstBodyTimeout,
		// OnClaim fires immediately before the underlying RoundTrip: it is the
		// PHYSICAL socket marker, an observability proof that the transport boundary
		// was reached — never permission to decline if RunStream then fails.
		// OnResponseHeaders / OnFirstBody are the lane's liveness signals, fired after it
		// in that fixed order; both are nil on the frozen native-only lane.
		IdleTimeout:       e.idleTimeout,
		OnClaim:           func() { e.metrics.sockets.Add(1) },
		OnResponseHeaders: lane.sendHeaders,
		OnFirstBody:       lane.sendFirstBody,
	})
	if rerr != nil {
		// EVERY RunStream error is terminal. Preserve the provider status when one is
		// available (D11: the provider-native body is never normalized away), retain the
		// bounded phase as stage/reason, and carry the accumulated raw as the diagnostic.
		// Admission is NOT re-run, DoStream is NOT retried, no BAML is invoked, no
		// fallback advances, no reset is emitted, and no pool replay is requested.
		stage, reason := streamFailStageReason(rerr)
		if httpErr, ok := execute.ProviderStatusHTTPError(rerr); ok {
			return e.failedCore(httpErr, stage, reason, cadence.Raw(), false)
		}
		return e.failedCore(rerr, stage, reason, cadence.Raw(), false)
	}
	_ = res
	transportCompleted = true

	// --- Clean completion: the lane's FINAL resolution over the accumulated parseable
	// text. Every failure is post-claim TERMINAL. ---
	fin := lane.resolveFinal(ctx, bundle, cadence.Parseable())
	if fin.err != nil {
		return e.failedCore(fin.err, fin.stage, fin.reason, cadence.Raw(), transportCompleted)
	}

	// A caller cancellation observed anywhere in the post-claim completion path is still
	// POST-CLAIM: the socket was owned and events were delivered, so it is terminal, not
	// a success — returning Succeeded would hand a final to a request that has already
	// gone away, and would be the one path on which a cancelled stream did not report as
	// cancelled.
	//
	// This is the SINGLE check, and its position is load-bearing: it sits after ALL
	// post-claim work (RunStream, and the lane's whole final resolution — which on the
	// oracle lane includes BOTH final legs and their comparison) and immediately before
	// the only success return, so there is no window left between a cancellation and a
	// Succeeded result.
	if err := ctx.Err(); err != nil {
		return e.failedCore(err, stageStream, reasonStreamCancelled, cadence.Raw(), transportCompleted)
	}

	e.metrics.successes.Add(1)
	// Raw/reasoning ride the FINAL only for a raw-wanted mode, matching the shared
	// orchestrator's emitFinal gate.
	if !lane.needsRaw {
		return claimedStreamOutcome{final: fin.value, transportCompleted: transportCompleted}
	}
	return claimedStreamOutcome{
		final:              fin.value,
		raw:                cadence.Raw(),
		reasoning:          cadence.Reasoning(),
		transportCompleted: transportCompleted,
	}
}

func (e *StreamExecutor) declinedCore(err error, stage, reason string, planCompareRan bool) claimedStreamOutcome {
	e.metrics.declines.Add(1)
	return claimedStreamOutcome{declined: true, planCompareRan: planCompareRan, err: err, stage: stage, reason: reason}
}

func (e *StreamExecutor) failedCore(err error, stage, reason, rawDiagnostic string, transportCompleted bool) claimedStreamOutcome {
	e.metrics.failures.Add(1)
	return claimedStreamOutcome{
		transportCompleted: transportCompleted,
		err:                err,
		stage:              stage,
		reason:             reason,
		rawDiagnostic:      rawDiagnostic,
	}
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

// oracleStaticStreamInput is the LIVE-oracle StreamWithOracle's admission input.
// staticStreamInput's descriptor-driven half is intentionally BAKED (rm.fn + the projected
// inv.Values); every OTHER admission fact is the request's TRUTHFUL selected-route fact,
// forwarded from the invocation the standard generated seam populated.
//
// Forwarding them — rather than synthesizing fixed single-leaf/no-override facts as the
// native-only lane's staticStreamInput does — is load-bearing: a request-scoped near-miss
// OUTSIDE the exact population (a non-default client override, a fallback / round-robin /
// retry strategy, a non-openai resolved leaf, a rewrite/proxy send target) MUST decline
// PRE-SOCKET at the shared admission gates rather than claim on a plan match. On a stream
// that matters more than on a call: there is no route back after the claim.
//
// The BAKED descriptor is what makes the live plan compare meaningful — native's plan comes
// from the registry's reconstructed descriptor while BAML's comes from the live generated
// one, so a deployment mutation that changed only BAML's plan is caught rather than
// absorbed.
func (e *StreamExecutor) oracleStaticStreamInput(rm *registeredMethod, inv bamlutils.NativeStaticStreamOracleInvocation) admission.StaticStreamInput {
	args := make(map[string]any, len(inv.Values))
	order := make([]string, 0, len(inv.Values))
	for _, v := range inv.Values {
		args[v.Name] = v.Value
		order = append(order, v.Name)
	}
	in := admission.StaticStreamInput{
		WorkerCapable:           true,
		RequestAPIPresent:       true,
		OnBuildRequestRoute:     true,
		FlagEnabled:             true,
		RouteKind:               admission.RouteKindStatic,
		Method:                  rm.fn.Method,
		Descriptor:              rm.fn,
		Args:                    args,
		ArgOrder:                order,
		Values:                  inv.Values,
		Mode:                    inv.Mode,
		NeedsRaw:                inv.Mode == bamlutils.NativeStreamModeStreamWithRaw,
		Provider:                inv.Provider,
		ClientOverride:          inv.ClientOverride,
		SingleLeaf:              inv.SingleLeaf,
		HasFallbackChain:        inv.HasFallbackChain,
		HasRoundRobin:           inv.HasRoundRobin,
		HasRequestRetryOverride: inv.HasRequestRetryOverride,
		// The rewrite/proxy predicate defaults to the process-global client so admission
		// can never SKIP the mandatory gate; a request-scoped effective client overrides it.
		WouldRewriteOrProxy: llmhttp.DefaultClient.WouldRewriteOrProxy,
		BuildBAMLRequest:    inv.BuildBAMLStreamRequest,
	}
	if inv.WouldRewriteOrProxy != nil {
		in.WouldRewriteOrProxy = inv.WouldRewriteOrProxy
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

// declinedStreamOracle is the U1s pre-socket decline, carrying the bounded observations
// accumulated so far so a decline's evidence (a plan compare that ran and mismatched, say)
// is never lost.
func (e *StreamExecutor) declinedStreamOracle(err error, stage, reason string, obs bamlutils.NativeSpineStreamOracleObservations) bamlutils.NativeSpineStreamOracleResult {
	e.metrics.declines.Add(1)
	out := bamlutils.DeclinedSpineStreamOracleResult(err, stage, reason)
	out.Observations = obs
	return out
}

func (e *StreamExecutor) failedStream(err error, stage, reason, rawDiagnostic string) bamlutils.NativeSpineStreamResult {
	e.metrics.failures.Add(1)
	return bamlutils.FailedAfterClaimSpineStreamResult(err, stage, reason, rawDiagnostic)
}
