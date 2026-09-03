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

// TestRecordOracle proves the composite replays the FULL bounded metric series — plan
// compare, native socket, per-facet response compare, fallback, serve outcome, and the
// same-response phase — plus population/phase/winner, all under CohortNone (no enrollment),
// from the observations CallWithOracle carries out.
func TestRecordOracle(t *testing.T) {
	reg := prometheus.NewRegistry()
	m, err := admission.NewMetrics(reg)
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	pop, err := registerPopulationCounter(reg)
	if err != nil {
		t.Fatalf("registerPopulationCounter: %v", err)
	}
	inv := bamlutils.NativeStaticInvocation{Provider: "openai"}

	// A NATIVE-WIN success carries full observations: plan matched, one responded socket,
	// same-response oracle entered, structured/order match, serve outcome success.
	nativeWin := bamlutils.SucceededOracleResult([]byte(`1`), "", "", bamlutils.NativeStaticServeEngineNative)
	nativeWin.Observations = bamlutils.NativeSpineUnaryOracleObservations{
		PlanCompareRan: true, PlanMatched: true,
		SocketOpened: true, SocketResponded: true,
		SameResponseOracleRan: true,
		ErrorCompareRecorded:  true, ErrorCompareMatch: true,
		StructuredBranchServed: true, StructuredMatch: true, OrderMatch: true,
		ServeOutcome: bamlutils.NativeStaticOutcomeSuccess,
	}
	recordOracle(m, pop, inv, nativeWin)

	checks := []struct {
		what   string
		name   string
		want   map[string]string
		expect float64
	}{
		{"population succeeded", "debaml_native_static_population_total", map[string]string{"population": populationExactJSONU1, "disposition": dispSucceeded}, 1},
		{"plan_compare match", "baml_rest_debaml_plan_compare_total", map[string]string{"result": "match"}, 1},
		{"native_sockets responded", "baml_rest_debaml_native_sockets_total", map[string]string{"outcome": "responded"}, 1},
		{"phase same_response_oracle", "baml_rest_debaml_admission_phase_total", map[string]string{"surface": "static_call", "phase": "same_response_oracle"}, 1},
		{"phase claimed", "baml_rest_debaml_admission_phase_total", map[string]string{"surface": "static_call", "phase": "claimed"}, 1},
		{"winner native", "baml_rest_debaml_winner_total", map[string]string{"surface": "static_call", "winner": "native"}, 1},
		{"attempts success", "baml_rest_debaml_attempts_total", map[string]string{"outcome": "success"}, 1},
		{"response_compare structured match", "baml_rest_debaml_response_compare_total", map[string]string{"field": "structured", "result": "match"}, 1},
	}
	for _, c := range checks {
		if got := sumCounter(t, reg, c.name, c.want); got != c.expect {
			t.Errorf("native-win %s (%s%v) = %v, want %v", c.what, c.name, c.want, got, c.expect)
		}
	}
	// No enrollment cohort was fabricated: the winner/phase carry cohort=none, never fe_v1.
	if got := sumCounter(t, reg, "baml_rest_debaml_winner_total", map[string]string{"surface": "static_call", "cohort": "fe_v1"}); got != 0 {
		t.Errorf("composite fabricated an enrollment cohort: winner{fe_v1} = %v", got)
	}

	// A DRIFT success records fallback (the same-bytes BAML parse won).
	drift := bamlutils.SucceededOracleResult([]byte(`2`), "", "", bamlutils.NativeStaticServeEngineBAMLParse)
	drift.Observations = bamlutils.NativeSpineUnaryOracleObservations{
		PlanCompareRan: true, PlanMatched: true, SocketOpened: true, SocketResponded: true,
		SameResponseOracleRan: true, StructuredBranchServed: true, Fallback: true,
		ServeOutcome: bamlutils.NativeStaticOutcomeSuccess,
	}
	recordOracle(m, pop, inv, drift)
	if got := sumCounter(t, reg, "baml_rest_debaml_fallback_total", nil); got != 1 {
		t.Errorf("fallback_total = %v after one drift, want 1", got)
	}

	// A pre-socket DECLINE records population(declined) + a preclaim phase, and opens NO
	// socket / runs NO plan compare.
	recordOracle(m, pop, inv, bamlutils.DeclinedOracleResult(errors.New("d"), "registry", "method_not_registered"))
	if got := counterValue(t, reg, "debaml_native_static_population_total", map[string]string{"population": populationExactJSONU1, "disposition": dispDeclined}); got != 1 {
		t.Errorf("population declined = %v, want 1", got)
	}
	// Two claimed attempts opened sockets; the decline opened none.
	if got := sumCounter(t, reg, "baml_rest_debaml_native_sockets_total", nil); got != 2 {
		t.Errorf("native_sockets_total = %v, want 2 (the decline opened no socket)", got)
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
