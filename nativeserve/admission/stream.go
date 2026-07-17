package admission

import (
	"context"

	nanollm "github.com/viktordanov/nanollm-ffi/go"
)

// De-BAML Phase 7B stream transport/plan claim. This file adds the STREAMING
// twin of admit.go's unary AdmitClaim: it builds and admits the EXACT OpenAI
// streaming plan (the same shape 7A's exact stream transport compares against)
// up to — but NOT including — the one physical RoundTrip, then hands the caller
// a live StreamClaim to drive nanollm DoStream through the 7A exact stream
// client.
//
// SCOPE (Phase 7B): this claim proves TRANSPORT eligibility only — the request
// plan is OpenAI, single-leaf, retry/rewrite/proxy-free, and byte-exact against
// the canonical stream body oracle (which the gated oracle proves equals BAML's
// StreamRequest). It does NOT prove native-only partial/final SEMANTIC serving
// support; that schema preflight is Phase 7C. So an AdmitStreamClaim success is
// NOT a full serving admission, and this whole surface is a test-harness API —
// nothing in RunStreamOrchestration, the generated adapter, workers, or any
// public route references it (Phase 7D wires the proven lane).

// StreamClaim is an admitted native STREAM plan whose request-scoped nanollm
// engine is kept ALIVE so the stream executor can drive DoStream on the SAME
// client Prepare produced the plan on. It is the streaming twin of Claim: it
// embeds the Admitted plan (Prepared streaming plan + the neutral exact-attempt
// stream request built but never sent) and additionally retains the OpenAI-format
// streaming Request the executor hands nanollm DoStream. The caller OWNS the
// client's lifecycle and MUST Close the StreamClaim on EVERY return path
// (success, error, panic, cancel).
//
// SENSITIVE — a StreamClaim embeds an Admitted (see its secret contract: the
// Prepared/ExactRequest retain the real bearer Authorization and the exact
// request body) plus a live engine and the request Body; it MUST NEVER be
// logged, serialized, or emitted. Alias/Target/Provider are secret-free.
type StreamClaim struct {
	Admitted
	client  *nanollm.Client
	request nanollm.Request
}

// Client returns the request-scoped nanollm engine the admitted streaming plan
// was Prepared on, still open so the executor can DoStream on it.
func (c *StreamClaim) Client() *nanollm.Client {
	if c == nil {
		return nil
	}
	return c.client
}

// Request returns the OpenAI-format streaming nanollm.Request the executor hands
// DoStream. Its Body is the UNARY canonical body (model + messages); DoStream
// re-prepares it with Stream=true, and the engine injects the streaming suffix —
// which the 7A exact stream client then compares against Admitted.ExactRequest.
// Treat it like the API key context it carries: never log or serialize it.
func (c *StreamClaim) Request() nanollm.Request {
	if c == nil {
		return nanollm.Request{}
	}
	return c.request
}

// PlanExpired reports whether the admitted prepared streaming plan's signature
// window has passed. Always false for the admitted never-expiring OpenAI surface;
// it guards the seam for the signed-plan providers a later phase adds, exactly as
// Claim.PlanExpired does for the unary lane.
func (c *StreamClaim) PlanExpired() bool {
	if c == nil || c.Prepared == nil {
		return false
	}
	return c.Prepared.Expired()
}

// Close releases the request-scoped nanollm engine. Idempotent-safe against a nil
// receiver / nil client; the caller must call it on every path.
func (c *StreamClaim) Close() {
	if c == nil || c.client == nil {
		return
	}
	c.client.Close()
	c.client = nil
}

// AdmitStreamClaim runs the FULL no-send native STREAM admission predicate for
// one OpenAI `_dynamic` StreamRequest and returns a StreamClaim whose
// request-scoped nanollm engine is kept ALIVE, so the stream executor can drive
// DoStream on the identical client Prepare ran on. The caller MUST Close the
// returned StreamClaim on every path. It records declines / planner errors
// exactly as the unary AdmitClaim does and — like AdmitClaim — does NOT record a
// terminal OutcomeAdmitted (the executor records the one terminal serving-side
// outcome instead).
//
// It opens ZERO sockets and performs ZERO RoundTrips on every path. It returns:
//
//   - (*StreamClaim, nil) when every layer is proven up to the exact RoundTrip —
//     the streaming plan is READY to send but deliberately NOT sent here;
//   - (nil, *Decline) — unwrapping to ErrDeclined — for a stable, secret-free
//     parity-decline to BAML;
//   - (nil, non-decline error) for an unexpected native planner/FFI error before
//     any socket (recorded as OutcomePlannerError).
func (a *Admitter) AdmitStreamClaim(ctx context.Context, in Input) (*StreamClaim, error) {
	adm, client, req, err := a.admitCore(ctx, in, false, true)
	if err != nil {
		return nil, err
	}
	return &StreamClaim{Admitted: adm, client: client, request: req}, nil
}

// admitStreamMode is the STREAM-claim mirror of admitMode: it admits exactly the
// two streaming modes (stream / stream-with-raw) and declines every unary mode
// (call / call-with-raw) and any unknown mode. Both streaming modes are admitted
// at the transport layer here; whether stream-with-raw's raw/reasoning channels
// serve correctly is a downstream (7C) semantic concern, not a transport gate.
func admitStreamMode(mode Mode) *Decline {
	switch mode {
	case ModeStream, ModeStreamWithRaw:
		return nil
	case ModeCall, ModeCallWithRaw:
		return declinef(StageMode, ReasonNotStreamMode, "unary mode reached the native stream claim")
	default:
		return declinef(StageMode, ReasonModeUnknown, "unrecognized request mode")
	}
}
