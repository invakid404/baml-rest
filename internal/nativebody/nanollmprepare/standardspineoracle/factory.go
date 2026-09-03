// Package standardspineoracle is the ExecBridge-U1c STANDARD-ONLY composite: the thin
// adapter that attaches the generated U1 spine to the BAML+nanollm serve worker as its
// static unary /call factory, default-selecting the exact structural population through a
// LIVE BAML plan-compare oracle + same-bytes safety parse.
//
// It owns only three things:
//
//   - constructing the deployment-generated population-filtered oracle executor
//     (nativegenerated.NewExecutor, which allows an empty population — an all-decline /
//     all-BAML-fallback executor);
//   - adapting the neutral spine oracle tri-state (bamlutils.NativeSpineUnaryOracleResult)
//     to the neutral static serve tri-state (bamlutils.NativeStaticServeResult) with a
//     TOTAL switch (an unknown disposition fails closed — no zero-socket proof exists);
//   - reusing the existing bounded de-BAML metrics plus one bounded structural
//     population counter (population=exact_json_u1). It records NO enrollment cohort
//     label: the U1 lane is a code-owned totality, not a rollout.
//
// It is BAML-AWARE but does NOT import BoundaryML/BAML: the live plan builder and
// same-bytes parser are the NEUTRAL closures the already-BAML-linked standard generated
// method captured (NativeStaticInvocation.BuildBAMLRequest / BAMLOnlyParse). This is
// both lower-coupling and a stronger isolation proof than a direct BAML import here.
//
// It is imported by cmd/worker ONLY. cmd/worker-nativeonly, nativeonlyboot,
// nativegenerated, nativeserve/spine, worker, and workerplugin must NEVER import it —
// proven by the whole-command dependency gate + the source import-direction test.
package standardspineoracle

import (
	"context"
	"errors"
	"fmt"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/internal/nativebody/nanollmprepare/nativegenerated"
	"github.com/invakid404/baml-rest/nativeserve/admission"
)

// populationExactJSONU1 is the ONE bounded structural population label for the U1 lane.
// It is NOT an enrollment cohort — no productionEnrollments() row backs it — so it never
// carries a config identity; it is a code-owned totality dimension for the spine counter.
const populationExactJSONU1 = "exact_json_u1"

// bounded, secret-free disposition tokens for the population counter.
const (
	dispDeclined  = "declined"
	dispSucceeded = "succeeded"
	dispFailed    = "failed"
)

// NewStaticServe is the workerboot.Options.NativeStaticServeFactory the STANDARD serve
// worker installs (in place of the legacy nativeserve.NewStaticServe). It builds the
// deployment-generated oracle executor once at boot, reuses the worker's bounded de-BAML
// metrics, registers the bounded population counter, and returns the neutral
// NativeStaticServeFunc that drives the exact-U1 population through the spine oracle.
//
// A generation/registry failure is FATAL (returned as an error, so workerboot exits
// non-zero): a standard build that expected the generated registry but linked the
// fail-loud stub must not silently degrade to all-BAML. Only a successfully generated
// (possibly empty) population yields an all-decline executor.
func NewStaticServe(reg prometheus.Registerer) (bamlutils.NativeStaticServeFunc, error) {
	exec, err := nativegenerated.NewExecutor()
	if err != nil {
		return nil, fmt.Errorf("standardspineoracle: build generated native spine executor: %w", err)
	}
	// REUSE the de-BAML collectors: the serve profile installs both the dynamic unary
	// serve (nativeserve.New) and this static composite on the SAME worker registry, so a
	// fresh NewMetrics would fail with a duplicate-registration panic. Reuse shares one
	// collector set so both write the SAME phase/winner series.
	m, err := admission.NewMetricsReusing(reg)
	if err != nil {
		return nil, fmt.Errorf("standardspineoracle: reuse de-BAML metrics: %w", err)
	}
	pop, err := registerPopulationCounter(reg)
	if err != nil {
		return nil, fmt.Errorf("standardspineoracle: register population counter: %w", err)
	}
	return func(ctx context.Context, inv bamlutils.NativeStaticInvocation) bamlutils.NativeStaticServeResult {
		res := exec.CallWithOracle(ctx, inv)
		recordOracle(m, pop, res)
		return adaptOracleResult(res)
	}, nil
}

// adaptOracleResult is the TOTAL tri-state map from the neutral spine oracle result to
// the neutral static serve result (design §3). "Fallback" is returning the known
// pre-socket decline to the already-running generated BAML orchestrator — this adapter
// never calls the BAML method itself, which is what prevents recursive dispatch and keeps
// one-send ownership auditable.
func adaptOracleResult(res bamlutils.NativeSpineUnaryOracleResult) bamlutils.NativeStaticServeResult {
	switch res.Disposition {
	case bamlutils.NativeSpineDeclinedPreSocket:
		// Pre-socket decline -> DeclineNativeCall; the generated seam runs BAML once.
		return bamlutils.NativeStaticServeResult{
			Disposition: bamlutils.NativeStaticServeDeclined,
			Stage:       res.Stage,
			Reason:      res.Reason,
		}
	case bamlutils.NativeSpineSucceeded:
		// Native owned the one provider request; the standard generated decoder serves
		// the owned canonical JSON. No BAML provider send.
		return bamlutils.NativeStaticServeResult{
			Disposition:  bamlutils.NativeStaticServeSucceeded,
			FinalJSON:    res.FinalJSON,
			Raw:          res.Raw,
			Reasoning:    res.Reasoning,
			WinnerEngine: res.WinnerEngine,
		}
	case bamlutils.NativeSpineFailedAfterClaim:
		// Post-claim terminal failure; never a BAML resend for the same call.
		return bamlutils.NativeStaticServeResult{
			Disposition:   bamlutils.NativeStaticServeFailed,
			Err:           res.Err,
			RawDiagnostic: res.RawDiagnostic,
		}
	default:
		// An unknown integer disposition cannot assert "no socket", so fail closed rather
		// than risk a hidden second same-call BAML send. Unreachable for the closed set.
		return bamlutils.NativeStaticServeResult{
			Disposition:   bamlutils.NativeStaticServeFailed,
			Err:           fmt.Errorf("standardspineoracle: unknown spine oracle disposition %d", res.Disposition),
			RawDiagnostic: res.RawDiagnostic,
		}
	}
}

// recordOracle records the reused bounded phase/winner series + the bounded population
// counter for one composite result. The cohort is CohortNone: the U1 lane is a code-owned
// totality with NO enrollment, so it never fabricates a productionEnrollments() row.
func recordOracle(m *admission.Metrics, pop *prometheus.CounterVec, res bamlutils.NativeSpineUnaryOracleResult) {
	surface, cohort := admission.SurfaceStaticCall, admission.CohortNone
	switch res.Disposition {
	case bamlutils.NativeSpineDeclinedPreSocket:
		pop.WithLabelValues(populationExactJSONU1, dispDeclined).Inc()
		m.RecordPreclaimDecline(surface, cohort)
	case bamlutils.NativeSpineSucceeded:
		pop.WithLabelValues(populationExactJSONU1, dispSucceeded).Inc()
		// A success always ran the same-response oracle over the one provider response.
		m.RecordAdmissionPhase(surface, cohort, admission.PhaseClaimed)
		m.RecordAdmissionPhase(surface, cohort, admission.PhaseSameResponseOracle)
		m.RecordPostclaimTerminal(surface, cohort, oracleWinner(res.WinnerEngine))
	default:
		// Failed-after-claim (or a fail-closed unknown disposition): a socket may have
		// opened, so it is a claimed terminal failure.
		pop.WithLabelValues(populationExactJSONU1, dispFailed).Inc()
		m.RecordAdmissionPhase(surface, cohort, admission.PhaseClaimed)
		m.RecordPostclaimTerminal(surface, cohort, admission.WinnerFailure)
	}
}

// oracleWinner maps the bounded winner-engine token to the admission winner label.
func oracleWinner(engine string) admission.Winner {
	switch engine {
	case bamlutils.NativeStaticServeEngineNative:
		return admission.WinnerNative
	case bamlutils.NativeStaticServeEngineBAMLParse:
		return admission.WinnerBAMLParseSameResponse
	default:
		return admission.WinnerFailure
	}
}

// registerPopulationCounter registers (or reuses, on a shared registry) the ONE bounded
// structural population counter for the U1 lane. Its only labels are the fixed population
// dimension and a bounded disposition — never a content-derived value or enrollment id.
func registerPopulationCounter(reg prometheus.Registerer) (*prometheus.CounterVec, error) {
	vec := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "debaml_native_static_population_total",
		Help: "Count of static /call requests routed through the ExecBridge-U1c structural population lane, by bounded population and disposition. Structural, not an enrollment cohort.",
	}, []string{"population", "disposition"})
	if err := reg.Register(vec); err != nil {
		var are prometheus.AlreadyRegisteredError
		if errors.As(err, &are) {
			if existing, ok := are.ExistingCollector.(*prometheus.CounterVec); ok {
				return existing, nil
			}
		}
		return nil, err
	}
	return vec, nil
}
