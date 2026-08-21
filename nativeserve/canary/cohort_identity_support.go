//go:build nanollm_integration

package canary

// De-BAML serving cutover S1 — the gated cohort-identity constructors.
//
// # Why these are behind a build tag
//
// The shipped policy enrolls exactly ONE tuple — the fe-v1 class on the dynamic
// unary call surface (serving cutover S3b) — reachable only by a request whose
// effective configuration the DEPLOYMENT sealed. So a server built by the ordinary
// constructors declines every other request pre-socket, and the static/stream lanes
// decline unconditionally. The gated end-to-end proofs still have to exercise the
// SERVE pipeline behind that gate on those surfaces, so they build their server with
// an enrolled proof identity instead of weakening the gate.
//
// A cold review of the first draft found these constructors shipping UNTAGGED in the
// public module, alongside an exported gate-override field. Together they were a
// second admission path: an external consumer could enroll itself and reach native
// Prepare/claim without touching the production policy. Both halves are closed now —
// admission.CohortInput's gate field is unexported, and the only thing that can
// produce a gate-bearing identity is admission.ProofCohortInputForTest, which lives
// behind this same tag.
//
// So a released consumer cannot link any of this, and the untagged surface offers no
// way to select a non-production gate (admission.TestNoUntaggedExportedGateInjection
// is the standing guard).

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/nativeserve/admission"
)

// NewServerWithCohortIdentity is [NewServer] with an explicit serving-cutover cohort
// identity: the dynamic-unary and static-unary lanes of the returned server present
// it to admission instead of the production (zero) identity.
func NewServerWithCohortIdentity(m *admission.Metrics, exec *llmhttp.ExactExecutor, identity admission.CohortInput) *Server {
	s := NewServer(m, exec)
	s.cohort = identity
	return s
}

// NewServeFuncWithCohortIdentity is [NewServeFunc] with an explicit cohort identity.
func NewServeFuncWithCohortIdentity(reg prometheus.Registerer, identity admission.CohortInput) (bamlutils.NativeServeFunc, error) {
	m, err := admission.NewMetrics(reg)
	if err != nil {
		return nil, err
	}
	return NewServerWithCohortIdentity(m, llmhttp.NewExactExecutor(nil), identity).Serve, nil
}

// NewStaticServeFuncWithCohortIdentity is [NewStaticServeFunc] with an explicit
// cohort identity.
func NewStaticServeFuncWithCohortIdentity(reg prometheus.Registerer, identity admission.CohortInput) (bamlutils.NativeStaticServeFunc, error) {
	m, err := admission.NewMetricsReusing(reg)
	if err != nil {
		return nil, err
	}
	return NewServerWithCohortIdentity(m, llmhttp.NewExactExecutor(nil), identity).ServeStatic, nil
}

// NewStreamServerWithCohortIdentity is [NewStreamServer] with an explicit cohort
// identity.
func NewStreamServerWithCohortIdentity(m *admission.Metrics, exec *llmhttp.ExactExecutor, firstBodyTimeout, idleTimeout time.Duration, identity admission.CohortInput) *StreamServer {
	s := NewStreamServer(m, exec, firstBodyTimeout, idleTimeout)
	s.cohort = identity
	return s
}

// NewStaticStreamServerWithCohortIdentity is [NewStaticStreamServer] with an explicit
// cohort identity.
func NewStaticStreamServerWithCohortIdentity(firstBodyTimeout, idleTimeout time.Duration, identity admission.CohortInput) *StaticStreamServer {
	s := NewStaticStreamServer(firstBodyTimeout, idleTimeout)
	s.cohort = identity
	return s
}

// NewStaticStreamServeFuncWithCohortIdentity is [NewStaticStreamServeFunc] with an
// explicit cohort identity.
func NewStaticStreamServeFuncWithCohortIdentity(reg prometheus.Registerer, identity admission.CohortInput) (bamlutils.NativeStaticStreamServeFunc, error) {
	fn, err := NewStaticStreamServeFunc(reg)
	if err != nil {
		return nil, err
	}
	_ = fn
	m, err := admission.NewMetricsReusing(reg)
	if err != nil {
		return nil, err
	}
	s := NewStaticStreamServerWithCohortIdentity(
		llmhttp.StreamFirstBodyTimeoutFromEnv(),
		llmhttp.StreamIdleTimeoutFromEnv(),
		identity,
	)
	s.metrics = m
	return s.ServeStaticStream, nil
}

// NewStaticShadowFuncWithCohortIdentity is [NewStaticShadowFunc] with an explicit
// cohort identity, for the gated end-to-end static-shadow proof.
func NewStaticShadowFuncWithCohortIdentity(reg prometheus.Registerer, identity admission.CohortInput) (bamlutils.NativeStaticShadowFunc, error) {
	m, err := admission.NewMetricsReusing(reg)
	if err != nil {
		return nil, err
	}
	return NewServerWithCohortIdentity(m, llmhttp.NewExactExecutor(nil), identity).ShadowStatic, nil
}
