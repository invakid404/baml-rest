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

// TestRecordOracle proves the composite reuses the bounded phase/winner series and
// increments the bounded population counter, with NO enrollment cohort (CohortNone).
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

	recordOracle(m, pop, bamlutils.SucceededOracleResult([]byte(`1`), "", "", bamlutils.NativeStaticServeEngineNative))
	recordOracle(m, pop, bamlutils.DeclinedOracleResult(errors.New("d"), "admission", "r"))
	recordOracle(m, pop, bamlutils.FailedAfterClaimOracleResult(errors.New("f"), "transport", "r", ""))

	const name = "debaml_native_static_population_total"
	for _, disp := range []string{dispSucceeded, dispDeclined, dispFailed} {
		if got := counterValue(t, reg, name, map[string]string{"population": populationExactJSONU1, "disposition": disp}); got != 1 {
			t.Errorf("population %s = %v, want 1", disp, got)
		}
	}
	// No enrollment cohort was fabricated: the population dimension is the only new label,
	// and it never carries a config identity.
	if got := counterValue(t, reg, name, map[string]string{"population": "fe_v1", "disposition": dispSucceeded}); got != 0 {
		t.Errorf("population lane fabricated an enrollment cohort label: got %v", got)
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
