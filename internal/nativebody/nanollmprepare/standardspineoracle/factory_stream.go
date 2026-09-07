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

// ExecBridge-U1s / M3e-B — the STANDARD-ONLY STREAM composite: the thin adapter that
// attaches the generated spine STREAM lane to the BAML+nanollm serve worker as its DEFAULT
// static `/stream{,-with-raw}` factory, default-selecting the exact structural
// `ClassStaticStream` population through a LIVE BAML plan-compare admission plus a
// PER-PREFIX and FINAL BAML parse oracle over that ONE response.
//
// It is the streaming twin of the unary composite in factory.go and owns exactly the same
// three things: constructing the deployment-generated population-filtered executor (empty
// allowed — an all-decline / all-BAML-fallback executor is legitimate for a standard
// worker), adapting the neutral spine oracle tri-state with a TOTAL switch (an unknown
// disposition fails CLOSED, because no zero-socket proof exists for it), and replaying the
// bounded observations into the worker's de-BAML metric series.
//
// It contains NO stream loop, opens NO socket, and never calls BAML itself: it forwards the
// neutral invocation — whose BAML closures the already-BAML-linked generated method
// captured — to exec.StreamWithOracle and maps what comes back. That is what keeps
// one-send ownership auditable and prevents recursive dispatch, and it is why this package
// (like its unary half) imports neither BoundaryML/BAML nor any generated baml_client.

// populationExactJSONU1s is the ONE bounded structural population label for the U1s stream
// lane. Like the unary lane's it is NOT an enrollment cohort — no productionEnrollments()
// row backs it — so it never carries a configuration identity.
const populationExactJSONU1s = "exact_json_u1s"

// bounded oracle-compare stages for the per-prefix/final comparison counter.
const (
	compareStagePrefix = "prefix"
	compareStageFinal  = "final"
)

// NewStaticStreamServe is the workerboot.Options.NativeStaticStreamServeFactory the STANDARD
// serve worker installs, in place of the legacy nativeserve.NewStaticStream. It builds the
// deployment-generated stream oracle executor once at boot, reuses the worker's bounded
// de-BAML metrics, registers the two bounded stream counters, and returns the neutral
// callback that drives the exact structural population through the per-prefix + final
// oracle.
//
// A generation/registry failure is FATAL (returned as an error, so workerboot exits
// non-zero): a standard build that expected the generated registry but linked the fail-loud
// stub must not silently degrade to all-BAML streaming. Only a successfully generated
// (possibly empty) population yields an all-decline executor.
func NewStaticStreamServe(reg prometheus.Registerer) (bamlutils.NativeStaticStreamOracleServeFunc, error) {
	exec, err := nativegenerated.NewStreamOracleExecutor()
	if err != nil {
		return nil, fmt.Errorf("standardspineoracle: build generated native spine stream executor: %w", err)
	}
	return NewStaticStreamServeFromExecutor(reg, exec)
}

// NewStaticStreamServeFromExecutor builds the composite over an ALREADY-CONSTRUCTED stream
// oracle executor. NewStaticStreamServe delegates to it after resolving the
// deployment-generated executor; it is also the injection seam a cross-boundary test uses to
// drive a real standard generated static stream method through a test-built executor,
// proving the REAL adapter maps a pre-socket decline back to BAML.
//
// It reuses the worker's bounded de-BAML collectors (the dynamic serve, the unary composite
// and this one all install on the SAME registry, so a fresh NewMetrics would panic on
// duplicate registration).
func NewStaticStreamServeFromExecutor(reg prometheus.Registerer, exec bamlutils.NativeSpineStreamOracleExecutor) (bamlutils.NativeStaticStreamOracleServeFunc, error) {
	if exec == nil {
		return nil, fmt.Errorf("standardspineoracle: nil stream oracle executor")
	}
	m, err := admission.NewMetricsReusing(reg)
	if err != nil {
		return nil, fmt.Errorf("standardspineoracle: reuse de-BAML metrics: %w", err)
	}
	pop, err := registerStreamPopulationCounter(reg)
	if err != nil {
		return nil, fmt.Errorf("standardspineoracle: register stream population counter: %w", err)
	}
	cmp, err := registerStreamCompareCounter(reg)
	if err != nil {
		return nil, fmt.Errorf("standardspineoracle: register stream oracle compare counter: %w", err)
	}
	return func(ctx context.Context, inv bamlutils.NativeStaticStreamOracleInvocation, emit bamlutils.NativeSpineStreamEmit) bamlutils.NativeStaticStreamOracleServeResult {
		res := exec.StreamWithOracle(ctx, inv, emit)
		recordStreamOracle(m, pop, cmp, inv, res)
		return adaptStreamOracleResult(res)
	}, nil
}

// adaptStreamOracleResult is the TOTAL tri-state map from the neutral spine stream oracle
// result to the neutral static-stream oracle serve result. "Fallback" here means returning
// the known PRE-SOCKET decline to the already-running generated BAML orchestrator, which
// then serves the same child once — this adapter never calls the BAML method itself.
func adaptStreamOracleResult(res bamlutils.NativeSpineStreamOracleResult) bamlutils.NativeStaticStreamOracleServeResult {
	switch res.Disposition {
	case bamlutils.NativeSpineStreamDeclinedPreSocket:
		// Zero sockets, zero public events -> the generated seam runs BAML once.
		return bamlutils.NativeStaticStreamOracleServeResult{
			Disposition: bamlutils.NativeStaticStreamOracleDeclined,
			Stage:       res.Stage,
			Reason:      res.Reason,
		}
	case bamlutils.NativeSpineStreamSucceeded:
		// Native owned the one provider stream; every partial was already resolved against
		// the oracle and delivered through emit. No BAML provider send.
		return bamlutils.NativeStaticStreamOracleServeResult{
			Disposition:  bamlutils.NativeStaticStreamOracleSucceeded,
			Final:        res.Final,
			Raw:          res.Raw,
			Reasoning:    res.Reasoning,
			WinnerEngine: res.WinnerEngine,
		}
	case bamlutils.NativeSpineStreamFailedAfterClaim:
		// Post-claim terminal; never a BAML resend, retry, fallback, reset or pool replay.
		return bamlutils.NativeStaticStreamOracleServeResult{
			Disposition:   bamlutils.NativeStaticStreamOracleFailed,
			Err:           res.Err,
			RawDiagnostic: res.RawDiagnostic,
			Stage:         res.Stage,
			Reason:        res.Reason,
		}
	default:
		// An unknown integer disposition cannot assert "no socket and no emitted event",
		// so fail closed rather than risk a hidden second same-request BAML stream.
		// Unreachable for the closed set.
		return bamlutils.NativeStaticStreamOracleServeResult{
			Disposition:   bamlutils.NativeStaticStreamOracleFailed,
			Err:           fmt.Errorf("standardspineoracle: unknown spine stream oracle disposition %d", res.Disposition),
			RawDiagnostic: res.RawDiagnostic,
		}
	}
}

// recordStreamOracle replays the bounded observations StreamWithOracle carried out into the
// worker's de-BAML metric series, under SurfaceStaticStream and the enrollment-free
// CohortNone: the U1s lane is a code-owned structural totality with NO enrollment, so it
// never fabricates a productionEnrollments() row. The observations survive a post-claim
// panic, so the plan-match / one-socket / per-prefix-comparison / drift evidence is never
// absent from a failed request.
func recordStreamOracle(
	m *admission.Metrics,
	pop, cmp *prometheus.CounterVec,
	inv bamlutils.NativeStaticStreamOracleInvocation,
	res bamlutils.NativeSpineStreamOracleResult,
) {
	surface, cohort := admission.SurfaceStaticStream, admission.CohortNone
	obs := res.Observations

	// Population + phase(claimed)/winner.
	switch res.Disposition {
	case bamlutils.NativeSpineStreamDeclinedPreSocket:
		pop.WithLabelValues(populationExactJSONU1s, dispDeclined).Inc()
		m.RecordPreclaimDecline(surface, cohort)
	case bamlutils.NativeSpineStreamSucceeded:
		pop.WithLabelValues(populationExactJSONU1s, dispSucceeded).Inc()
		m.RecordAdmissionPhase(surface, cohort, admission.PhaseClaimed)
		m.RecordPostclaimTerminal(surface, cohort, oracleWinner(res.WinnerEngine))
	default:
		// Failed-after-claim (or a fail-closed unknown disposition): a socket may have
		// opened and events may already have been delivered, so it is a claimed terminal.
		pop.WithLabelValues(populationExactJSONU1s, dispFailed).Inc()
		m.RecordAdmissionPhase(surface, cohort, admission.PhaseClaimed)
		m.RecordPostclaimTerminal(surface, cohort, admission.WinnerFailure)
	}

	// The same-response oracle PHASE. A claimed U1s stream runs the oracle on every
	// structured tick and on the final, so the phase is recorded whenever either happened —
	// including when the oracle itself is what terminated the stream.
	if obs.PrefixComparisons > 0 || obs.FinalOracleRan {
		m.RecordAdmissionPhase(surface, cohort, admission.PhaseSameResponseOracle)
	}
	// Live BAML StreamRequest plan-compare evidence (whole-plan byte match).
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
	// The bounded per-prefix and final comparison ledger. Each counter is a closed
	// classification; none carries prefix text, a parsed value, or an error string.
	recordCompare(cmp, compareStagePrefix, bamlutils.NativeStreamCompareMatch, obs.PrefixMatch)
	recordCompare(cmp, compareStagePrefix, bamlutils.NativeStreamCompareNativeNoValue, obs.PrefixNativeNoValue)
	recordCompare(cmp, compareStagePrefix, bamlutils.NativeStreamCompareBAMLNoValue, obs.PrefixBAMLNoValue)
	recordCompare(cmp, compareStagePrefix, bamlutils.NativeStreamCompareBytesMismatch, obs.PrefixBytesMismatch)
	recordCompare(cmp, compareStagePrefix, bamlutils.NativeStreamCompareNativeError, obs.PrefixNativeError)
	if obs.FinalCompare != bamlutils.NativeStreamCompareNone {
		recordCompare(cmp, compareStageFinal, obs.FinalCompare, 1)
	}
	// The served answer came from BAML's parse of the SAME response (a substituted prefix
	// or final, or a suppressed native partial), recorded exactly as the unary composite
	// records its same-bytes fallback.
	if res.WinnerEngine == bamlutils.NativeStaticServeEngineBAMLParse {
		m.RecordFallback(admission.FallbackParseOnly)
	}
	// Serve outcome (attempts_total), under the request's REAL streaming mode. None on a
	// pre-socket decline; EVERY other non-success terminal — including a claimed
	// failure/panic with no resolver outcome and an out-of-contract unknown disposition —
	// records internal_error, so a fail-closed terminal is never silently unobserved.
	mode := admission.ModeStream
	if inv.NeedsRaw {
		mode = admission.ModeStreamWithRaw
	}
	switch {
	case res.Disposition == bamlutils.NativeSpineStreamDeclinedPreSocket:
		// A pre-socket decline is not an attempt; attempts_total counts claimed ones.
	case res.Disposition != bamlutils.NativeSpineStreamSucceeded &&
		res.Disposition != bamlutils.NativeSpineStreamFailedAfterClaim:
		// An out-of-contract disposition. adaptStreamOracleResult fails it CLOSED, so the
		// outcome it happens to carry must not be published — least of all a `success` on
		// a request the adapter is about to report as failed.
		m.RecordServeOutcome(mode, inv.Provider, admission.OutcomeInternalError)
	default:
		if outcome, ok := mapServeOutcome(obs.ServeOutcome); ok {
			m.RecordServeOutcome(mode, inv.Provider, outcome)
		} else if res.Disposition != bamlutils.NativeSpineStreamSucceeded {
			// A claimed terminal that carried no resolver outcome (a post-claim panic,
			// say) is still an attempt and must be counted. A SUCCESS without one is
			// excluded, matching the unary twin: mislabelling a served stream
			// internal_error would be worse than not counting it.
			m.RecordServeOutcome(mode, inv.Provider, admission.OutcomeInternalError)
		}
	}
}

// knownStreamCompare is the CLOSED set of comparison classifications that may become a
// metric label. Validating against it is what keeps this series' cardinality bounded by the
// code rather than by whatever the resolver happens to return: a token added upstream
// without a counter would otherwise be published verbatim as a new label value.
var knownStreamCompare = map[bamlutils.NativeStreamOracleCompare]bool{
	bamlutils.NativeStreamCompareMatch:         true,
	bamlutils.NativeStreamCompareNativeNoValue: true,
	bamlutils.NativeStreamCompareBAMLNoValue:   true,
	bamlutils.NativeStreamCompareBytesMismatch: true,
	bamlutils.NativeStreamCompareNativeError:   true,
}

// compareOther is where an unrecognized classification folds. It is a fixed label, so an
// out-of-contract token costs one extra series rather than one per distinct value — and it
// is still COUNTED, because silently dropping it would under-report drift.
const compareOther = "other"

// recordCompare adds n to one bounded (stage, result) comparison cell, skipping zero so an
// untouched classification stays absent rather than being published as an explicit zero.
func recordCompare(cmp *prometheus.CounterVec, stage string, result bamlutils.NativeStreamOracleCompare, n int) {
	if n <= 0 {
		return
	}
	label := string(result)
	if !knownStreamCompare[result] {
		label = compareOther
	}
	cmp.WithLabelValues(stage, label).Add(float64(n))
}

// registerStreamPopulationCounter registers (or reuses, on a shared registry) the ONE
// bounded structural population counter for the U1s stream lane. It is deliberately a
// SEPARATE series from the unary lane's debaml_native_static_population_total rather than a
// third label on it: that counter's help text states it counts static /call requests, and
// folding stream attempts into it would make both readings wrong.
func registerStreamPopulationCounter(reg prometheus.Registerer) (*prometheus.CounterVec, error) {
	return registerCounterVec(reg, prometheus.CounterOpts{
		Name: "debaml_native_static_stream_population_total",
		Help: "Count of static /stream{,-with-raw} requests routed through the ExecBridge-U1s structural population lane, by bounded population and disposition. Structural, not an enrollment cohort.",
	}, []string{"population", "disposition"})
}

// registerStreamCompareCounter registers the bounded per-prefix/final oracle comparison
// ledger: for every structured cadence tick and for the final, which way the native-vs-BAML
// comparison came out. Its labels are two closed enums — never a prefix, a parsed value, or
// an error string.
func registerStreamCompareCounter(reg prometheus.Registerer) (*prometheus.CounterVec, error) {
	return registerCounterVec(reg, prometheus.CounterOpts{
		Name: "debaml_native_static_stream_oracle_compare_total",
		Help: "Count of ExecBridge-U1s native-vs-BAML same-prefix comparisons on a claimed static stream, by bounded stage (prefix/final) and result (match, native_no_value, baml_no_value, bytes_mismatch, native_error).",
	}, []string{"stage", "result"})
}

// registerCounterVec registers a counter vector, tolerating the shared-registry case where
// an identical collector is already installed (the worker builds its factories against ONE
// registry, and a boot that constructs the composite twice must reuse rather than panic).
func registerCounterVec(reg prometheus.Registerer, opts prometheus.CounterOpts, labels []string) (*prometheus.CounterVec, error) {
	vec := prometheus.NewCounterVec(opts, labels)
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
