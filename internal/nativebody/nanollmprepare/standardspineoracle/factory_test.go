package standardspineoracle

import (
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/internal/nativebody/nanollmprepare/nativegenerated"
	"github.com/invakid404/baml-rest/nativeserve/admission"
)

// counterValue reads one CounterVec cell out of a registry by name + exact labels,
// returning 0 when absent. Dependency-light (reg.Gather + client_model) so the isolated
// module needs no test-only go.sum entry.
func counterValue(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string) float64 {
	t.Helper()
	fams, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range fams {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			got := map[string]string{}
			for _, lp := range m.GetLabel() {
				got[lp.GetName()] = lp.GetValue()
			}
			if labelsEqual(got, labels) {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

func labelsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range b {
		if a[k] != v {
			return false
		}
	}
	return true
}

// TestAdaptOracleResult is the exhaustive spine-oracle -> static-serve disposition map,
// including the fail-closed unknown arm.
func TestAdaptOracleResult(t *testing.T) {
	failErr := errors.New("boom")

	t.Run("declined preserves stage/reason", func(t *testing.T) {
		got := adaptOracleResult(bamlutils.DeclinedOracleResult(errors.New("d"), "admission", "client_registry_present"))
		if got.Disposition != bamlutils.NativeStaticServeDeclined {
			t.Fatalf("disposition = %v, want declined", got.Disposition)
		}
		if got.Stage != "admission" || got.Reason != "client_registry_present" {
			t.Errorf("stage/reason = %q/%q, want admission/client_registry_present", got.Stage, got.Reason)
		}
	})

	t.Run("succeeded carries owned payload", func(t *testing.T) {
		got := adaptOracleResult(bamlutils.SucceededOracleResult([]byte(`{"k":1}`), "raw", "reason", bamlutils.NativeStaticServeEngineNative))
		if got.Disposition != bamlutils.NativeStaticServeSucceeded {
			t.Fatalf("disposition = %v, want succeeded", got.Disposition)
		}
		if string(got.FinalJSON) != `{"k":1}` || got.Raw != "raw" || got.Reasoning != "reason" {
			t.Errorf("payload = %q/%q/%q", got.FinalJSON, got.Raw, got.Reasoning)
		}
		if got.WinnerEngine != bamlutils.NativeStaticServeEngineNative {
			t.Errorf("winner = %q", got.WinnerEngine)
		}
	})

	t.Run("failed-after-claim carries typed error + raw", func(t *testing.T) {
		got := adaptOracleResult(bamlutils.FailedAfterClaimOracleResult(failErr, "transport", "transport_error", "diag"))
		if got.Disposition != bamlutils.NativeStaticServeFailed {
			t.Fatalf("disposition = %v, want failed", got.Disposition)
		}
		if !errors.Is(got.Err, failErr) || got.RawDiagnostic != "diag" {
			t.Errorf("err/raw = %v/%q", got.Err, got.RawDiagnostic)
		}
	})

	t.Run("unknown disposition fails closed", func(t *testing.T) {
		got := adaptOracleResult(bamlutils.NativeSpineUnaryOracleResult{Disposition: bamlutils.NativeSpineUnaryDisposition(99)})
		if got.Disposition != bamlutils.NativeStaticServeFailed {
			t.Fatalf("unknown disposition mapped to %v, want failed (fail-closed: no zero-socket proof)", got.Disposition)
		}
		if got.Err == nil {
			t.Error("unknown disposition produced no error")
		}
	})
}

func TestOracleWinner(t *testing.T) {
	cases := map[string]admission.Winner{
		bamlutils.NativeStaticServeEngineNative:    admission.WinnerNative,
		bamlutils.NativeStaticServeEngineBAMLParse: admission.WinnerBAMLParseSameResponse,
		"something-else": admission.WinnerFailure,
	}
	for engine, want := range cases {
		if got := oracleWinner(engine); got != want {
			t.Errorf("oracleWinner(%q) = %q, want %q", engine, got, want)
		}
	}
}

// sumCounter sums every CounterVec cell whose labels CONTAIN the wanted subset (partial
// match), by name — for metrics with more labels than the assertion pins. A nil/empty want
// sums the whole series.
func sumCounter(t *testing.T, reg *prometheus.Registry, name string, want map[string]string) float64 {
	t.Helper()
	fams, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var sum float64
	for _, mf := range fams {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			got := map[string]string{}
			for _, lp := range m.GetLabel() {
				got[lp.GetName()] = lp.GetValue()
			}
			match := true
			for k, v := range want {
				if got[k] != v {
					match = false
					break
				}
			}
			if match {
				sum += m.GetCounter().GetValue()
			}
		}
	}
	return sum
}

// metric family names recordOracle writes into.
const (
	mPop      = "debaml_native_static_population_total"
	mPhase    = "baml_rest_debaml_admission_phase_total"
	mWinner   = "baml_rest_debaml_winner_total"
	mPlan     = "baml_rest_debaml_plan_compare_total"
	mSocket   = "baml_rest_debaml_native_sockets_total"
	mResp     = "baml_rest_debaml_response_compare_total"
	mFallback = "baml_rest_debaml_fallback_total"
	mAttempts = "baml_rest_debaml_attempts_total"
)

// TestRecordOracle proves — on a FRESH registry per scenario, so before/after deltas are
// exact — that the composite replays EXACTLY the bounded de-BAML series
// CallWithOracle's observations describe, for every terminal shape: a native-win match, a
// same-bytes drift, a provider fault, a transport fault, a BAML-parse error, a post-claim
// parser panic (observations carried THROUGH the panic), a plan-mismatch pre-socket decline,
// and a near-miss pre-socket decline. Each scenario asserts (a) every EXPECTED cell is
// exactly one — under the enrollment-free surface=static_call/cohort=none attribution — and
// (b) each metric family's TOTAL equals the expected count, so a MISSING or DOUBLE-COUNTED
// failure-path metric (the pre-fix defect: population/phase/winner only, no plan/socket/
// response/fallback/attempts) cannot pass.
func TestRecordOracle(t *testing.T) {
	// sc is the enrollment-free attribution subset every phase/winner cell must carry.
	sc := func(extra map[string]string) map[string]string {
		out := map[string]string{"surface": "static_call", "cohort": "none"}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	// fullMatchObs is a native-win observation set; scenarios derive from it.
	succ := func(engine string, obs bamlutils.NativeSpineUnaryOracleObservations) bamlutils.NativeSpineUnaryOracleResult {
		r := bamlutils.SucceededOracleResult([]byte(`1`), "", "", engine)
		r.Observations = obs
		return r
	}
	fail := func(obs bamlutils.NativeSpineUnaryOracleObservations) bamlutils.NativeSpineUnaryOracleResult {
		r := bamlutils.FailedAfterClaimOracleResult(errors.New("x"), "provider", "provider_error", "")
		r.Observations = obs
		return r
	}
	decline := func(planRan, matched bool) bamlutils.NativeSpineUnaryOracleResult {
		r := bamlutils.DeclinedOracleResult(errors.New("d"), "strategy", "not_single_leaf")
		r.Observations = bamlutils.NativeSpineUnaryOracleObservations{PlanCompareRan: planRan, PlanMatched: matched}
		return r
	}

	type cell struct {
		name   string
		labels map[string]string
	}
	scenarios := []struct {
		name   string
		res    bamlutils.NativeSpineUnaryOracleResult
		ones   []cell         // cells that must each equal exactly 1
		totals map[string]int // family -> exact total across the whole series
	}{
		{
			name: "native win match",
			res: succ(bamlutils.NativeStaticServeEngineNative, bamlutils.NativeSpineUnaryOracleObservations{
				PlanCompareRan: true, PlanMatched: true, SocketOpened: true, SocketResponded: true,
				SameResponseOracleRan: true, ErrorCompareRecorded: true, ErrorCompareMatch: true,
				StructuredBranchServed: true, StructuredMatch: true, OrderMatch: true,
				ServeOutcome: bamlutils.NativeStaticOutcomeSuccess,
			}),
			ones: []cell{
				{mPop, map[string]string{"population": populationExactJSONU1, "disposition": dispSucceeded}},
				{mPhase, sc(map[string]string{"phase": "claimed"})},
				{mPhase, sc(map[string]string{"phase": "postclaim_terminal"})},
				{mPhase, sc(map[string]string{"phase": "same_response_oracle"})},
				{mWinner, sc(map[string]string{"winner": "native"})},
				{mPlan, map[string]string{"result": "match", "field": "meta"}},
				{mSocket, map[string]string{"flag": "on", "outcome": "responded"}},
				{mResp, map[string]string{"field": "structured", "result": "match"}},
				{mResp, map[string]string{"field": "order", "result": "match"}},
				{mResp, map[string]string{"field": "error", "result": "match"}},
				{mAttempts, map[string]string{"outcome": "success"}},
			},
			totals: map[string]int{mPop: 1, mPhase: 3, mWinner: 1, mPlan: 1, mSocket: 1, mResp: 7, mFallback: 0, mAttempts: 1},
		},
		{
			name: "same-bytes drift",
			res: succ(bamlutils.NativeStaticServeEngineBAMLParse, bamlutils.NativeSpineUnaryOracleObservations{
				PlanCompareRan: true, PlanMatched: true, SocketOpened: true, SocketResponded: true,
				SameResponseOracleRan: true, ErrorCompareRecorded: true, ErrorCompareMatch: true,
				StructuredBranchServed: true, StructuredMatch: false, OrderMatch: true, Fallback: true,
				ServeOutcome: bamlutils.NativeStaticOutcomeSuccess,
			}),
			ones: []cell{
				{mPop, map[string]string{"population": populationExactJSONU1, "disposition": dispSucceeded}},
				{mWinner, sc(map[string]string{"winner": "baml_parse_same_response"})},
				{mPhase, sc(map[string]string{"phase": "same_response_oracle"})},
				{mPlan, map[string]string{"result": "match", "field": "meta"}},
				{mSocket, map[string]string{"flag": "on", "outcome": "responded"}},
				{mResp, map[string]string{"field": "structured", "result": "mismatch"}},
				{mResp, map[string]string{"field": "order", "result": "match"}},
				{mFallback, map[string]string{"kind": "parse_only"}},
				{mAttempts, map[string]string{"outcome": "success"}},
			},
			totals: map[string]int{mPop: 1, mPhase: 3, mWinner: 1, mPlan: 1, mSocket: 1, mResp: 7, mFallback: 1, mAttempts: 1},
		},
		{
			name: "provider fault",
			res: fail(bamlutils.NativeSpineUnaryOracleObservations{
				PlanCompareRan: true, PlanMatched: true, SocketOpened: true, SocketResponded: true,
				ServeOutcome: bamlutils.NativeStaticOutcomeProviderError,
			}),
			ones: []cell{
				{mPop, map[string]string{"population": populationExactJSONU1, "disposition": dispFailed}},
				{mWinner, sc(map[string]string{"winner": "failure"})},
				{mPlan, map[string]string{"result": "match", "field": "meta"}},
				{mSocket, map[string]string{"flag": "on", "outcome": "responded"}},
				{mAttempts, map[string]string{"outcome": "provider_error"}},
			},
			totals: map[string]int{mPop: 1, mPhase: 2, mWinner: 1, mPlan: 1, mSocket: 1, mResp: 0, mFallback: 0, mAttempts: 1},
		},
		{
			name: "transport fault",
			res: fail(bamlutils.NativeSpineUnaryOracleObservations{
				PlanCompareRan: true, PlanMatched: true, SocketOpened: true, SocketResponded: false,
				ServeOutcome: bamlutils.NativeStaticOutcomeTransportError,
			}),
			ones: []cell{
				{mPop, map[string]string{"population": populationExactJSONU1, "disposition": dispFailed}},
				{mSocket, map[string]string{"flag": "on", "outcome": "transport_error"}},
				{mAttempts, map[string]string{"outcome": "transport_error"}},
			},
			totals: map[string]int{mPop: 1, mPhase: 2, mWinner: 1, mPlan: 1, mSocket: 1, mResp: 0, mFallback: 0, mAttempts: 1},
		},
		{
			name: "baml parse error",
			res: fail(bamlutils.NativeSpineUnaryOracleObservations{
				PlanCompareRan: true, PlanMatched: true, SocketOpened: true, SocketResponded: true,
				SameResponseOracleRan: true, ErrorCompareRecorded: true, ErrorCompareMatch: false,
				ServeOutcome: bamlutils.NativeStaticOutcomeParseError,
			}),
			ones: []cell{
				{mPop, map[string]string{"population": populationExactJSONU1, "disposition": dispFailed}},
				{mPhase, sc(map[string]string{"phase": "same_response_oracle"})},
				{mResp, map[string]string{"field": "error", "result": "mismatch"}},
				{mAttempts, map[string]string{"outcome": "parse_error"}},
			},
			totals: map[string]int{mPop: 1, mPhase: 3, mWinner: 1, mPlan: 1, mSocket: 1, mResp: 1, mFallback: 0, mAttempts: 1},
		},
		{
			// Post-claim parser panic: obs carry PlanCompareRan/SameResponseOracleRan set BEFORE
			// the panic and NO ServeOutcome (Resolve never returned), so the composite records
			// attempts{internal_error} via the failed-after-claim fallback — the phase survives.
			name: "parser panic",
			res: fail(bamlutils.NativeSpineUnaryOracleObservations{
				PlanCompareRan: true, PlanMatched: true, SocketOpened: true, SocketResponded: true,
				SameResponseOracleRan: true, ServeOutcome: bamlutils.NativeStaticOutcomeNone,
			}),
			ones: []cell{
				{mPop, map[string]string{"population": populationExactJSONU1, "disposition": dispFailed}},
				{mPhase, sc(map[string]string{"phase": "same_response_oracle"})},
				{mWinner, sc(map[string]string{"winner": "failure"})},
				{mSocket, map[string]string{"flag": "on", "outcome": "responded"}},
				{mAttempts, map[string]string{"outcome": "internal_error"}},
			},
			totals: map[string]int{mPop: 1, mPhase: 3, mWinner: 1, mPlan: 1, mSocket: 1, mResp: 0, mFallback: 0, mAttempts: 1},
		},
		{
			name: "plan-mismatch decline",
			res:  decline(true, false),
			ones: []cell{
				{mPop, map[string]string{"population": populationExactJSONU1, "disposition": dispDeclined}},
				{mPhase, sc(map[string]string{"phase": "preclaim_decline"})},
				{mWinner, sc(map[string]string{"winner": "baml_transport"})},
				{mPlan, map[string]string{"result": "mismatch", "field": "meta"}},
			},
			totals: map[string]int{mPop: 1, mPhase: 1, mWinner: 1, mPlan: 1, mSocket: 0, mResp: 0, mFallback: 0, mAttempts: 0},
		},
		{
			name: "near-miss decline",
			res:  decline(false, false),
			ones: []cell{
				{mPop, map[string]string{"population": populationExactJSONU1, "disposition": dispDeclined}},
				{mPhase, sc(map[string]string{"phase": "preclaim_decline"})},
				{mWinner, sc(map[string]string{"winner": "baml_transport"})},
			},
			totals: map[string]int{mPop: 1, mPhase: 1, mWinner: 1, mPlan: 0, mSocket: 0, mResp: 0, mFallback: 0, mAttempts: 0},
		},
	}

	inv := bamlutils.NativeStaticInvocation{Provider: "openai"}
	for _, s := range scenarios {
		t.Run(s.name, func(t *testing.T) {
			reg := prometheus.NewRegistry()
			m, err := admission.NewMetrics(reg)
			if err != nil {
				t.Fatalf("NewMetrics: %v", err)
			}
			pop, err := registerPopulationCounter(reg)
			if err != nil {
				t.Fatalf("registerPopulationCounter: %v", err)
			}
			recordOracle(m, pop, inv, s.res)

			for _, c := range s.ones {
				if got := sumCounter(t, reg, c.name, c.labels); got != 1 {
					t.Errorf("%s cell %v = %v, want exactly 1", c.name, c.labels, got)
				}
			}
			for name, want := range s.totals {
				if got := sumCounter(t, reg, name, nil); got != float64(want) {
					t.Errorf("%s TOTAL = %v, want %d (no missing/double-counted cell)", name, got, want)
				}
			}
			// Enrollment-free: nothing under a fe_v1 cohort, ever.
			if got := sumCounter(t, reg, mWinner, map[string]string{"cohort": "fe_v1"}); got != 0 {
				t.Errorf("composite fabricated an enrollment cohort: winner{fe_v1} = %v", got)
			}
			if got := sumCounter(t, reg, mPhase, map[string]string{"cohort": "fe_v1"}); got != 0 {
				t.Errorf("composite fabricated an enrollment cohort: phase{fe_v1} = %v", got)
			}
		})
	}
}

// TestRegisterPopulationCounterIsReusable proves a second registration on the SAME
// registry reuses the existing collector rather than failing — the serve profile builds
// this factory once, but a defensive reuse keeps a duplicate build from panicking.
func TestRegisterPopulationCounterIsReusable(t *testing.T) {
	reg := prometheus.NewRegistry()
	a, err := registerPopulationCounter(reg)
	if err != nil {
		t.Fatalf("first register: %v", err)
	}
	b, err := registerPopulationCounter(reg)
	if err != nil {
		t.Fatalf("second register: %v", err)
	}
	a.WithLabelValues(populationExactJSONU1, dispSucceeded).Inc()
	if got := counterValue(t, reg, "debaml_native_static_population_total", map[string]string{"population": populationExactJSONU1, "disposition": dispSucceeded}); got != 1 {
		t.Errorf("reused counter did not share state: got %v, want 1", got)
	}
	_ = b
}

// TestNewStaticServeFailsLoudWithoutGeneratedRegistry proves the fail-loud guard: in a
// source checkout (no debamlnativespinegenerated tag) nativegenerated is the stub, so
// NewStaticServe surfaces the generation error rather than silently degrading to all-BAML.
func TestNewStaticServeFailsLoudWithoutGeneratedRegistry(t *testing.T) {
	_, err := NewStaticServe(prometheus.NewRegistry())
	if err == nil {
		t.Fatal("NewStaticServe succeeded without a generated registry; it must fail loud so the standard build never silently degrades to all-BAML")
	}
	if !errors.Is(err, nativegenerated.ErrRuntimeNotGenerated) {
		t.Errorf("err = %v, want it to wrap nativegenerated.ErrRuntimeNotGenerated", err)
	}
}
