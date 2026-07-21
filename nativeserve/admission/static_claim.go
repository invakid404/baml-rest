package admission

import (
	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/internal/schema"
	nanollm "github.com/viktordanov/nanollm-ffi/go"
)

// De-BAML Slice 8C — static serve claim.
//
// StaticClaim is the static twin of the dynamic [Claim]: it is the request-scoped
// serve token AdmitStaticClaim returns on a full would-admit. It keeps the nanollm
// engine ALIVE so the serve core can RoundTrip and TranslateResponse on the exact
// client Prepare produced the plan on, and carries the prepared plan, the lowered
// Return Bundle (the native static SAP surface), the exact-attempt request, and the
// internal alias. The serve core owns Close-ing it on EVERY path (success, failure,
// panic, cancel).

// StaticClaim carries a claimed static serve attempt: the kept-alive nanollm engine
// plus the prepared plan, Return Bundle, exact-attempt request, and alias.
type StaticClaim struct {
	client *nanollm.Client

	// Prepared is the nanollm prepared plan the exact executor sends (exactly once).
	Prepared *nanollm.PreparedRequest
	// Bundle is the lowered, validated Return Bundle the native static SAP parses
	// the response against. The serve core captures it in a schema-neutral parse
	// closure (debaml.ParseStaticBundle) — nativeserve never re-implements Bundle
	// semantics, keeping the recursion slices tar-free.
	Bundle *schema.Bundle
	// ExactRequest is the neutral exact-attempt carrier derived from Prepared
	// (ordered headers, raw body) — retained for the same-bytes plan/one-send proofs.
	ExactRequest *llmhttp.ExactAttemptRequest
	// Alias is the fixed internal nanollm alias the plan was prepared under and
	// TranslateResponse is called with (never a target model / selected client).
	Alias string
}

// Client returns the request-scoped nanollm engine (nil after Close).
func (c *StaticClaim) Client() *nanollm.Client {
	if c == nil {
		return nil
	}
	return c.client
}

// Close releases the request-scoped nanollm engine. It is idempotent and safe on a
// nil claim, so the serve core can defer it unconditionally.
func (c *StaticClaim) Close() {
	if c == nil || c.client == nil {
		return
	}
	c.client.Close()
	c.client = nil
}

// PlanExpired reports whether the prepared plan's signature window passed before the
// socket. OpenAI plans are unsigned and never expire; this guards the seam for the
// signed-plan providers a later phase adds, mirroring the dynamic Claim.
func (c *StaticClaim) PlanExpired() bool {
	if c == nil || c.Prepared == nil {
		return false
	}
	return c.Prepared.Expired()
}

// StaticDecline is the typed PRE-CLAIM decline AdmitStaticClaim returns: a bounded,
// secret-free stage/reason/family/observation the serve core maps into a
// NativeStaticServeDeclined result so BAML serves the same call. It is the static
// twin of [Decline]; it guarantees NO provider socket occurred.
type StaticDecline struct {
	Observation bamlutils.NativeStaticObservation
	Family      bamlutils.NativeStaticObserveFamily
	Stage       string
	Reason      string
}

// Error returns a bounded, secret-free description (never a value/payload).
func (d *StaticDecline) Error() string {
	return "nativeserve: static admission declined (" + d.Stage + "): " + d.Reason
}

// staticDeclineFromObs converts a recorded observation into the typed decline error.
func staticDeclineFromObs(obs StaticObservation) *StaticDecline {
	return &StaticDecline{
		Observation: obs.Observation,
		Family:      obs.Family,
		Stage:       obs.Stage,
		Reason:      obs.Reason,
	}
}
