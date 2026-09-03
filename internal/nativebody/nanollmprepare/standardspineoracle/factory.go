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
	return NewStaticServeFromExecutor(reg, exec)
}

// NewStaticServeFromExecutor builds the composite over an ALREADY-CONSTRUCTED oracle
// executor. NewStaticServe delegates to it after resolving the deployment-generated
// executor; it is also the injection seam a cross-boundary test uses to drive a real
// standard generated static method through a test-built spine executor (proving the REAL
// adapter maps a pre-socket decline back to BAML). It reuses the worker's bounded de-BAML
// collectors (both the dynamic serve and this composite install on the SAME registry, so
// a fresh NewMetrics would panic on duplicate registration) and registers the bounded
// population counter.
func NewStaticServeFromExecutor(reg prometheus.Registerer, exec bamlutils.NativeSpineUnaryOracleExecutor) (bamlutils.NativeStaticServeFunc, error) {
	if exec == nil {
		return nil, fmt.Errorf("standardspineoracle: nil oracle executor")
	}
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
		recordOracle(m, pop, inv, res)
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

// recordOracle replays the bounded observations CallWithOracle carried out into the SAME
// worker de-BAML metric series the canary static serve path records — plan compare, native
// socket, per-facet response compare, fallback, serve outcome, and the same-response phase —
// plus the population/phase/winner series. The cohort is CohortNone: the U1 lane is a
// code-owned totality with NO enrollment, so it never fabricates a productionEnrollments()
// row. The observations are carried even across a post-claim panic, so the plan-match /
// one-socket / same-response / drift evidence the scope requires is never absent.
func recordOracle(m *admission.Metrics, pop *prometheus.CounterVec, inv bamlutils.NativeStaticInvocation, res bamlutils.NativeSpineUnaryOracleResult) {
	surface, cohort := admission.SurfaceStaticCall, admission.CohortNone
	obs := res.Observations

	// Population + phase(claimed)/winner.
	switch res.Disposition {
	case bamlutils.NativeSpineDeclinedPreSocket:
		pop.WithLabelValues(populationExactJSONU1, dispDeclined).Inc()
		m.RecordPreclaimDecline(surface, cohort)
	case bamlutils.NativeSpineSucceeded:
		pop.WithLabelValues(populationExactJSONU1, dispSucceeded).Inc()
		m.RecordAdmissionPhase(surface, cohort, admission.PhaseClaimed)
		m.RecordPostclaimTerminal(surface, cohort, oracleWinner(res.WinnerEngine))
	default:
		// Failed-after-claim (or a fail-closed unknown disposition): a socket may have
		// opened, so it is a claimed terminal failure.
		pop.WithLabelValues(populationExactJSONU1, dispFailed).Inc()
		m.RecordAdmissionPhase(surface, cohort, admission.PhaseClaimed)
		m.RecordPostclaimTerminal(surface, cohort, admission.WinnerFailure)
	}

	// The same-response oracle PHASE — recorded from the panic-safe observation (set before
	// the parser), so a parser panic that fails the request still records the phase.
	if obs.SameResponseOracleRan {
		m.RecordAdmissionPhase(surface, cohort, admission.PhaseSameResponseOracle)
	}
	// Live BAML plan-compare evidence (whole-plan byte match, recorded under the meta field).
	if obs.PlanCompareRan {
		result := admission.PlanCompareMismatch
		if obs.PlanMatched {
			result = admission.PlanCompareMatch
		}
		m.RecordPlanCompare(result, admission.PlanCompareFieldMeta)
	}
	// Exactly-one native socket.
	if obs.SocketOpened {
		outcome := admission.NativeSocketTransportError
		if obs.SocketResponded {
			outcome = admission.NativeSocketResponded
		}
		m.RecordNativeSocket(outcome)
	}
	// Same-response per-facet compares (mirroring the canary path exactly).
	if obs.ErrorCompareRecorded {
		recordResponse(m, obs.ErrorCompareMatch, admission.ResponseCompareFieldError)
	}
	if obs.StructuredBranchServed {
		recordResponse(m, true, admission.ResponseCompareFieldTranslate)
		recordResponse(m, true, admission.ResponseCompareFieldAssistant)
		recordResponse(m, true, admission.ResponseCompareFieldRaw)
		recordResponse(m, true, admission.ResponseCompareFieldReasoning)
		recordResponse(m, obs.StructuredMatch, admission.ResponseCompareFieldStructured)
		recordResponse(m, obs.OrderMatch, admission.ResponseCompareFieldOrder)
	}
	if obs.ParseDeclineServed {
		recordResponse(m, true, admission.ResponseCompareFieldTranslate)
		recordResponse(m, false, admission.ResponseCompareFieldStructured)
		recordResponse(m, false, admission.ResponseCompareFieldOrder)
	}
	if obs.Fallback {
		m.RecordFallback(admission.FallbackParseOnly)
	}
	// Serve outcome (attempts_total). None on a pre-socket decline; EVERY other non-success
	// terminal — a claimed failure/panic with no resolver outcome, AND an out-of-contract
	// unknown disposition that adaptOracleResult fails closed — records internal_error, so a
	// fail-closed terminal is never silently unobserved on attempts_total.
	if outcome, ok := mapServeOutcome(obs.ServeOutcome); ok {
		m.RecordServeOutcome(admission.ModeCall, inv.Provider, outcome)
	} else if res.Disposition != bamlutils.NativeSpineDeclinedPreSocket && res.Disposition != bamlutils.NativeSpineSucceeded {
		m.RecordServeOutcome(admission.ModeCall, inv.Provider, admission.OutcomeInternalError)
	}
}

// recordResponse folds a per-facet match/mismatch into the response-compare metric,
// mirroring canary.Server.recordResponse.
func recordResponse(m *admission.Metrics, match bool, field admission.ResponseCompareField) {
	result := admission.ResponseCompareMismatch
	if match {
		result = admission.ResponseCompareMatch
	}
	m.RecordResponseCompare(result, field)
}

// mapServeOutcome maps the neutral bounded serve-outcome onto the admission serve outcome.
// The second return is false for NativeStaticOutcomeNone (a pre-socket decline records none).
func mapServeOutcome(o bamlutils.NativeStaticServeOutcome) (admission.Outcome, bool) {
	switch o {
	case bamlutils.NativeStaticOutcomeSuccess:
		return admission.OutcomeSuccess, true
	case bamlutils.NativeStaticOutcomeParseDecline:
		return admission.OutcomeParseDecline, true
	case bamlutils.NativeStaticOutcomeParseError:
		return admission.OutcomeParseError, true
	case bamlutils.NativeStaticOutcomeTranslateError:
		return admission.OutcomeTranslateError, true
	case bamlutils.NativeStaticOutcomeProviderError:
		return admission.OutcomeProviderError, true
	case bamlutils.NativeStaticOutcomeTransportError:
		return admission.OutcomeTransportError, true
	default:
		return admission.Outcome(""), false
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
