package admission

import (
	"context"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/internal/debaml"
	nanollm "github.com/viktordanov/nanollm-ffi/go"
)

// M3e-A — the codegen-spine STATIC-STREAM admission entry, the streaming twin of the
// unary [AdmitStaticSpineClaim].
//
// WHY THIS IS A SEPARATE EXPORTED ENTRY, not a flag on the existing one:
//
//   - [AdmitStaticStreamClaim] deliberately applies the default-deny cohort gate AND
//     the strict BAML `StreamRequest` plan compare. Both are load-bearing for the
//     standard/legacy lane and neither changes here.
//   - The spine lane is BAML-FREE by construction: the native-only artifact links no
//     generated BAML, so it has no `StreamRequest` closure to compare against, and it
//     must not touch standard cohort selection.
//   - A caller-settable boolean that skipped those gates would be unsafe: any caller
//     could flip it. The lane policy is UNEXPORTED ([staticStreamLane]) and each lane
//     has its own exported entry point, so only this function can take the spine path.
//
// Everything ELSE the streaming predicate does still runs here: the stream mode gate
// (stream / stream-with-raw only), the orchestration-plan gates, the descriptor
// envelope and projected argument binder, the Return-Bundle lower, the exact
// [debaml.SupportsNativeStaticStreamBundle] totality gate, the static prompt render,
// the OpenAI normalization + BuildOpenAIChatStream parity anchor, the `Stream:true`
// Prepare with MaxRetries 0 — plus a MANDATORY rewrite/proxy gate over the prepared
// effective URL.

// AdmitStaticSpineStreamClaim runs the BAML-free codegen-spine static-stream no-send
// admission predicate and returns a live *StaticStreamClaim on a full would-admit, else
// a typed *StaticDecline guaranteeing NO provider socket occurred. The caller MUST Close
// the returned claim on every path.
//
// The totality gate is the ONE root-owned predicate
// [debaml.SupportsNativeStaticStreamBundle] — the exact five-arm `JSON` recursive alias
// family — the SAME predicate that gated registration and gates the parse entrypoints.
// It is strictly stronger than the legacy lane's
// [admittedStaticStreamReturnShape]/IsProvenRecursiveAliasStaticStreamFamily gate (it
// additionally requires final support, because every stream ends in a final parse), so
// a bundle admitted here can never make a support decline after the claim.
//
// The rewrite/proxy gate is MANDATORY and TOTAL, not optional-on-nil, exactly as in
// admitSpineThroughTotality: this lane is cohort-gate-EXEMPT, so a caller reaching it
// with a NIL WouldRewriteOrProxy predicate must NOT claim while the check is skipped.
// FAIL CLOSED — a nil predicate means the effective target's rewrite/proxy status could
// not be verified, so it declines PRE-CLAIM exactly as a positive verdict would.
func AdmitStaticSpineStreamClaim(ctx context.Context, in StaticStreamInput) (*StaticStreamClaim, error) {
	prep, req, cohort, dec := admitSpineStreamThroughTotality(ctx, in, laneSpineStaticStream)
	if dec != nil {
		return nil, dec
	}
	// No BAML plan compare — this lane has no generated BAML to compare against; frozen
	// v0.223 oracle evidence (the per-prefix parser differential and the SSE-replay
	// differential) stands in for this exact cohort. Transfer ownership of the
	// kept-alive engine to the claim.
	return staticStreamClaimFrom(prep, req, cohort), nil
}

// AdmitStaticSpineStreamOracleClaim is the ExecBridge-U1s / M3e-B LIVE-oracle static-stream
// admission entry — the standard-worker twin of the frozen-evidence
// [AdmitStaticSpineStreamClaim], and the streaming twin of [AdmitStaticSpineOracleClaim].
//
// It runs the SAME shared spine portion (admitSpineStreamThroughTotality: the stream mode
// gate, the orchestration-plan gates, the descriptor envelope + projected arg binder, the
// Return-Bundle lower, the EARLY root-owned [debaml.SupportsNativeStaticStreamBundle]
// totality cut, the static render, the OpenAI stream normalize + BuildOpenAIChatStream
// parity anchor, the `Stream:true` / MaxRetries-0 Prepare, and the MANDATORY fail-closed
// rewrite/proxy gate) and then, for THIS entry only, adds back the strict BAML
// `StreamRequest.<Method>` no-send plan compare that the native-only lane deliberately
// omits: a standard SERVE worker CAN construct BAML's no-send stream plan
// (in.BuildBAMLRequest), so the live plan-match precondition is restored as the exact
// cohort's safety rail BEFORE any socket.
//
// Only a full [bamlutils.NativeStaticObserveWouldAdmit] byte-match transfers the kept-alive
// engine into a claim. A nil or erroring plan builder, a builder PANIC, a snapshot failure,
// a send-path rewrite/proxy, or ANY byte mismatch closes the engine and returns a typed
// *StaticDecline — so a decline certifies zero provider sockets and zero public events and
// the outer composite hands the request back to BAML.
//
// It is deliberately NOT implemented by calling [AdmitStaticStreamClaim]: that entry applies
// the default-deny cohort gate (this lane is a code-owned structural totality, not a
// rollout) and the WEAKER legacy return-shape gate (which does not require final support,
// and every stream ends in a final). Nor is it implemented by adding "skip cohort" /
// "skip compare" fields to [StaticStreamInput]: a caller-settable bit could bypass either
// rail, and on a claimed stream there is no route back.
func AdmitStaticSpineStreamOracleClaim(ctx context.Context, in StaticStreamInput) (*StaticStreamClaim, error) {
	prep, req, cohort, dec := admitSpineStreamThroughTotality(ctx, in, laneSpineStaticStreamOracle)
	if dec != nil {
		return nil, dec
	}
	// prep is a live request-scoped nanollm engine now owned by THIS function. Close it on
	// ANY non-transfer exit — a plan mismatch/decline, OR a PANIC from the live
	// BuildBAMLRequest callback inside staticPlanCompareObservation (a caller-supplied
	// closure): the panic unwinds to StreamWithOracle's pre-claim decline guard, and without
	// this defer the engine would leak, contradicting "the client is NEVER left open on a
	// decline". prep.close() is idempotent, so this is safe alongside the helper's own guard.
	transferred := false
	defer func() {
		if !transferred {
			prep.close()
		}
	}()
	// LIVE BAML StreamRequest plan compare (the standard-worker oracle rail): build BAML's
	// `StreamRequest.<Method>` plan WITHOUT sending and require a byte match before the
	// claim.
	obs := staticPlanCompareObservation(ctx, StaticInput{
		BuildBAMLRequest:    in.BuildBAMLRequest,
		WouldRewriteOrProxy: in.WouldRewriteOrProxy,
	}, prep)
	if obs.Observation != bamlutils.NativeStaticObserveWouldAdmit {
		return nil, staticDeclineFromObs(obs)
	}
	claim := staticStreamClaimFrom(prep, req, cohort)
	transferred = true
	return claim, nil
}

// admitSpineStreamThroughTotality is the shared exact-spine static-stream pre-claim portion
// BOTH spine stream entries run exactly once: layers 1-3b with the spine-lane cohort-gate
// bypass, the EARLY root-owned totality cut, layers 4-6 (render / stream normalize / parity
// anchor / `Stream:true` Prepare), and the MANDATORY fail-closed rewrite/proxy gate. On
// success it returns the kept-alive *staticPrepared plus the streaming nanollm.Request (the
// caller decides frozen-claim vs live-oracle-claim); on ANY decline it closes the engine (NO
// socket) and returns a typed *StaticDecline.
//
// The totality gate is the ONE root-owned predicate [debaml.SupportsNativeStaticStreamBundle]
// — the exact five-arm `JSON` recursive alias family, the SAME predicate that gated
// registration and gates the parse entrypoints. It is strictly stronger than the legacy
// lane's [admittedStaticStreamReturnShape] gate (it additionally requires FINAL support,
// because every stream ends in a final parse), so a bundle admitted here can never make a
// support decline after the claim. It is placed BEFORE any render / client normalize /
// nanollm New / Prepare, so an out-of-cohort stream declines having done ZERO nanollm work.
//
// The rewrite/proxy gate is MANDATORY and TOTAL, not optional-on-nil, exactly as in
// admitSpineThroughTotality: these lanes are cohort-gate-EXEMPT, so a caller reaching one
// with a NIL WouldRewriteOrProxy predicate must NOT claim while the check is skipped. FAIL
// CLOSED — a nil predicate means the effective target's rewrite/proxy status could not be
// verified, so it declines PRE-CLAIM exactly as a positive verdict would.
func admitSpineStreamThroughTotality(ctx context.Context, in StaticStreamInput, lane staticStreamLane) (*staticPrepared, nanollm.Request, CohortID, *StaticDecline) {
	bundle, fn, cohort, dec := admitStaticStreamThroughBundle(ctx, in, lane)
	if dec != nil {
		return nil, nanollm.Request{}, CohortNone, staticDeclineFromObs(*dec)
	}
	if err := debaml.SupportsNativeStaticStreamBundle(bundle); err != nil {
		return nil, nanollm.Request{}, CohortNone, staticDeclineFromObs(declineStatic(bamlutils.NativeStaticFamilyDescriptorEnvelope, StagePrompt, reasonSpineNotExactAlias))
	}
	prep, req, pdec := admitStaticStreamPrepare(ctx, in, fn, bundle)
	if pdec != nil {
		return nil, nanollm.Request{}, CohortNone, staticDeclineFromObs(*pdec)
	}
	// prep is a live request-scoped nanollm engine now owned by THIS function. Close it on
	// ANY non-transfer exit — a decline OR a PANIC from the caller-supplied rewrite/proxy
	// predicate — so a decline never leaks the engine. prep.close() is idempotent, so the
	// caller may install its own guard over the transferred value too.
	transferred := false
	defer func() {
		if !transferred {
			prep.close()
		}
	}()
	if in.WouldRewriteOrProxy == nil {
		return nil, nanollm.Request{}, CohortNone, staticDeclineFromObs(declineStatic(bamlutils.NativeStaticFamilyClient, StageStrategy, reasonSpineRewriteProxyUnverified))
	}
	if in.WouldRewriteOrProxy(prep.prepared.URL) {
		return nil, nanollm.Request{}, CohortNone, staticDeclineFromObs(declineStatic(bamlutils.NativeStaticFamilyClient, StageStrategy, ReasonURLRewriteOrProxy))
	}
	transferred = true
	return prep, req, cohort, nil
}
