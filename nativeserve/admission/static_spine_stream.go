package admission

import (
	"context"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/internal/debaml"
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
	// --- Layers 1-3b: build/flag/route, the STREAM mode gate, strategy, descriptor
	// envelope + arg binder, Return-Bundle lower. The spine lane skips ONLY the
	// default-deny cohort gate (see staticStreamLane). ---
	bundle, fn, cohort, dec := admitStaticStreamThroughBundle(ctx, in, laneSpineStaticStream)
	if dec != nil {
		return nil, staticDeclineFromObs(*dec)
	}

	// --- Totality gate (EARLY): the ONE root-owned exact-cohort cut, placed BEFORE any
	// render / client normalize / nanollm New / Prepare so a stream outside the exact
	// five-arm `JSON` alias declines here having done ZERO nanollm work and opened NO
	// socket. ---
	if err := debaml.SupportsNativeStaticStreamBundle(bundle); err != nil {
		return nil, staticDeclineFromObs(declineStatic(bamlutils.NativeStaticFamilyDescriptorEnvelope, StagePrompt, reasonSpineNotExactAlias))
	}

	// --- Layers 4-6: render + streaming client normalize + BuildOpenAIChatStream parity
	// anchor + nanollm New/Prepare with Stream:true and MaxRetries 0 (NO SEND). ---
	prep, req, pdec := admitStaticStreamPrepare(ctx, in, fn, bundle)
	if pdec != nil {
		return nil, staticDeclineFromObs(*pdec)
	}
	// prep is a live request-scoped nanollm engine now owned by THIS function. Close it
	// on ANY non-transfer exit — a decline OR a PANIC from the caller-supplied
	// rewrite/proxy predicate — so a decline never leaks the engine. prep.close() is
	// idempotent.
	transferred := false
	defer func() {
		if !transferred {
			prep.close()
		}
	}()

	// --- MANDATORY rewrite/proxy gate over the PREPARED effective URL. A send-path
	// rewrite/proxy would make the exact-transport evidence meaningless (the request
	// would go elsewhere), and on a claimed stream there is no route back. ---
	if in.WouldRewriteOrProxy == nil {
		return nil, staticDeclineFromObs(declineStatic(bamlutils.NativeStaticFamilyClient, StageStrategy, reasonSpineRewriteProxyUnverified))
	}
	if in.WouldRewriteOrProxy(prep.prepared.URL) {
		return nil, staticDeclineFromObs(declineStatic(bamlutils.NativeStaticFamilyClient, StageStrategy, ReasonURLRewriteOrProxy))
	}

	// No BAML plan compare — this lane has no generated BAML to compare against; frozen
	// v0.223 oracle evidence (the per-prefix parser differential and the SSE-replay
	// differential) stands in for this exact cohort. Transfer ownership of the
	// kept-alive engine to the claim.
	claim := staticStreamClaimFrom(prep, req, cohort)
	transferred = true
	return claim, nil
}
