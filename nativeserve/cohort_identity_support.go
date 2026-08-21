//go:build nanollm_integration

package nativeserve

// De-BAML serving cutover S1 — the gated cohort-identity factories.
//
// The shipped policy enrolls exactly ONE tuple — the fe-v1 class on the dynamic
// unary call surface (serving cutover S3b) — and a request reaches it only by
// presenting an identity the DEPLOYMENT sealed. So a serve func built by [New] /
// [NewStaticServe] / [NewStaticStream] declines every request pre-socket unless the
// deployment approved that exact configuration, and the static lanes decline
// unconditionally. The gated end-to-end proofs still have to exercise the serve
// pipeline BEHIND that gate on surfaces nothing is enrolled on, so they build their
// serve func here with an enrolled proof identity.
//
// These are behind the `nanollm_integration` opt-in tag, and the only thing that can
// produce a gate-bearing identity (admission.ProofCohortInputForTest) is behind the
// same tag, because a cold review of the first draft found both shipping untagged in
// the public module — a second admission path an external consumer could take
// without touching the production policy. A released consumer can reach only the
// no-argument factories, which is also all workerboot's factory signatures allow.

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/nativeserve/admission"
	"github.com/invakid404/baml-rest/nativeserve/canary"
)

// NewWithCohortIdentity is [New] with an explicit serving-cutover cohort identity.
//
// Since serving-cutover S1, admission requires an ENROLLED surface/cohort pair before
// any native work, and the shipped policy enrolls NOTHING — so a serve func built by
// [New] declines every request pre-socket and BAML serves it. That is the intended
// production behaviour, and it would also delete the end-to-end evidence that the
// serve pipeline itself still works, so the gated end-to-end proofs build their serve
// func here with an enrolled proof identity instead of weakening the gate.
//
// It is NOT a rollout control and no worker installs it: workerboot's factories take
// only a registry, so a deploy profile can only reach [New]. Enrolling real traffic is
// a reviewed change to admission's shipped policy plus a config-load fingerprint
// assignment — never a different constructor.
func NewWithCohortIdentity(reg prometheus.Registerer, identity admission.CohortInput) (bamlutils.NativeServeFunc, error) {
	return canary.NewServeFuncWithCohortIdentity(reg, identity)
}

// NewStaticServeWithCohortIdentity is [NewStaticServe] with an explicit cohort
// identity, for the gated end-to-end static serve proof. See [NewWithCohortIdentity]
// for why it is not a rollout control.
func NewStaticServeWithCohortIdentity(reg prometheus.Registerer, identity admission.CohortInput) (bamlutils.NativeStaticServeFunc, error) {
	return canary.NewStaticServeFuncWithCohortIdentity(reg, identity)
}

// NewStaticStreamWithCohortIdentity is [NewStaticStream] with an explicit cohort
// identity, for the gated end-to-end static-stream proof. See [NewWithCohortIdentity]
// for why it is not a rollout control.
func NewStaticStreamWithCohortIdentity(reg prometheus.Registerer, identity admission.CohortInput) (bamlutils.NativeStaticStreamServeFunc, error) {
	return canary.NewStaticStreamServeFuncWithCohortIdentity(reg, identity)
}

// NewStaticObserveWithCohortIdentity is [NewStaticObserve] with an explicit cohort
// identity, for the gated end-to-end observer proof.
//
// The shipped observer presents the production (zero) identity and declines at the
// default-deny gate — the no-send profiles get no exception in S1 — so the proof
// that the observer still ATTACHES to the generated seam and still measures the
// predicate has to present an enrolled identity, exactly like the serve proofs do.
func NewStaticObserveWithCohortIdentity(reg prometheus.Registerer, identity admission.CohortInput) (bamlutils.NativeStaticObserveFunc, error) {
	return newStaticObserve(reg, identity)
}

// NewStaticShadowWithCohortIdentity is [NewStaticShadow] with an explicit cohort
// identity, for the gated end-to-end static-shadow proof. See
// [NewStaticObserveWithCohortIdentity].
func NewStaticShadowWithCohortIdentity(reg prometheus.Registerer, identity admission.CohortInput) (bamlutils.NativeStaticShadowFunc, error) {
	return canary.NewStaticShadowFuncWithCohortIdentity(reg, identity)
}
