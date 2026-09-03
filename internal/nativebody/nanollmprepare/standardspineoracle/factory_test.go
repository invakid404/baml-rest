package standardspineoracle

import (
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/invakid404/baml-rest/bamlutils"
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

// cellKey canonicalizes a label set as sorted "k=v" joined by "," so a metric cell is
// identified by its COMPLETE label set, regardless of map order.
func cellKey(labels map[string]string) string {
	ks := make([]string, 0, len(labels))
	for k := range labels {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	parts := make([]string, len(ks))
	for i, k := range ks {
		parts[i] = k + "=" + labels[k]
	}
	return strings.Join(parts, ",")
}

// gatherFamily returns every NON-ZERO cell of one metric family as cellKey->value (skipping
// the zero cells NewMetrics pre-initializes). Comparing this whole map against a complete
// expected map rejects a missing, dropped, duplicated, RELABELED, or unexpected label cell —
// not merely a wrong family total, which a one-line relabel leaves untouched.
func gatherFamily(t *testing.T, reg *prometheus.Registry, name string) map[string]float64 {
	t.Helper()
	fams, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	out := map[string]float64{}
	for _, mf := range fams {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			v := m.GetCounter().GetValue()
			if v == 0 {
				continue
			}
			labels := map[string]string{}
			for _, lp := range m.GetLabel() {
				labels[lp.GetName()] = lp.GetValue()
			}
			out[cellKey(labels)] = v
		}
	}
	return out
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
// and a near-miss pre-socket decline. Each scenario compares the COMPLETE per-label cell map
// of every metric family against the exact expected map — so a missing, dropped, duplicated,
// RELABELED (e.g. response_compare field assistant->raw), or unexpected cell fails, not merely
// a wrong family total (which a one-line relabel leaves unchanged). Enrollment-freeness
// (surface=static_call/cohort=none, never fe_v1) is enforced by the same exact compare: any
// fe_v1 cell is an unexpected cell.
func TestRecordOracle(t *testing.T) {
	// Full-label-set builders for each family's cells (every label the family carries).
	popl := func(disp string) map[string]string {
		return map[string]string{"population": populationExactJSONU1, "disposition": disp}
	}
	sc := func(phase string) map[string]string {
		return map[string]string{"surface": "static_call", "cohort": "none", "phase": phase}
	}
	scw := func(winner string) map[string]string {
		return map[string]string{"surface": "static_call", "cohort": "none", "winner": winner}
	}
	plan := func(result string) map[string]string { return map[string]string{"result": result, "field": "meta"} }
	sock := func(outcome string) map[string]string { return map[string]string{"flag": "on", "outcome": outcome} }
	resp := func(field, result string) map[string]string {
		return map[string]string{"field": field, "result": result}
	}
	att := func(outcome string) map[string]string {
		return map[string]string{"mode": "call", "engine": "native", "provider": "openai", "outcome": outcome}
	}
	fb := map[string]string{"kind": "parse_only"}
	// respBranch is the 7-cell response-compare map for a served structured branch: the four
	// native-owned facets + error always match; structured/order vary per scenario.
	respBranch := func(structured, order string) map[string]float64 {
		return map[string]float64{
			cellKey(resp("translate", "match")): 1, cellKey(resp("assistant", "match")): 1,
			cellKey(resp("raw", "match")): 1, cellKey(resp("reasoning", "match")): 1,
			cellKey(resp("error", "match")):         1,
			cellKey(resp("structured", structured)): 1, cellKey(resp("order", order)): 1,
		}
	}
	one := func(labels map[string]string) map[string]float64 { return map[string]float64{cellKey(labels): 1} }

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

	// want is the COMPLETE non-zero cell map per family; an absent family means "no cell".
	type want map[string]map[string]float64
	scenarios := []struct {
		name string
		res  bamlutils.NativeSpineUnaryOracleResult
		want want
	}{
		{
			name: "native win match",
			res: succ(bamlutils.NativeStaticServeEngineNative, bamlutils.NativeSpineUnaryOracleObservations{
				PlanCompareRan: true, PlanMatched: true, SocketOpened: true, SocketResponded: true,
				SameResponseOracleRan: true, ErrorCompareRecorded: true, ErrorCompareMatch: true,
				StructuredBranchServed: true, StructuredMatch: true, OrderMatch: true,
				ServeOutcome: bamlutils.NativeStaticOutcomeSuccess,
			}),
			want: want{
				mPop:      one(popl(dispSucceeded)),
				mPhase:    {cellKey(sc("claimed")): 1, cellKey(sc("postclaim_terminal")): 1, cellKey(sc("same_response_oracle")): 1},
				mWinner:   one(scw("native")),
				mPlan:     one(plan("match")),
				mSocket:   one(sock("responded")),
				mResp:     respBranch("match", "match"),
				mAttempts: one(att("success")),
			},
		},
		{
			name: "same-bytes drift",
			res: succ(bamlutils.NativeStaticServeEngineBAMLParse, bamlutils.NativeSpineUnaryOracleObservations{
				PlanCompareRan: true, PlanMatched: true, SocketOpened: true, SocketResponded: true,
				SameResponseOracleRan: true, ErrorCompareRecorded: true, ErrorCompareMatch: true,
				StructuredBranchServed: true, StructuredMatch: false, OrderMatch: true, Fallback: true,
				ServeOutcome: bamlutils.NativeStaticOutcomeSuccess,
			}),
			want: want{
				mPop:      one(popl(dispSucceeded)),
				mPhase:    {cellKey(sc("claimed")): 1, cellKey(sc("postclaim_terminal")): 1, cellKey(sc("same_response_oracle")): 1},
				mWinner:   one(scw("baml_parse_same_response")),
				mPlan:     one(plan("match")),
				mSocket:   one(sock("responded")),
				mResp:     respBranch("mismatch", "match"),
				mFallback: one(fb),
				mAttempts: one(att("success")),
			},
		},
		{
			name: "provider fault",
			res: fail(bamlutils.NativeSpineUnaryOracleObservations{
				PlanCompareRan: true, PlanMatched: true, SocketOpened: true, SocketResponded: true,
				ServeOutcome: bamlutils.NativeStaticOutcomeProviderError,
			}),
			want: want{
				mPop:      one(popl(dispFailed)),
				mPhase:    {cellKey(sc("claimed")): 1, cellKey(sc("postclaim_terminal")): 1},
				mWinner:   one(scw("failure")),
				mPlan:     one(plan("match")),
				mSocket:   one(sock("responded")),
				mAttempts: one(att("provider_error")),
			},
		},
		{
			name: "transport fault",
			res: fail(bamlutils.NativeSpineUnaryOracleObservations{
				PlanCompareRan: true, PlanMatched: true, SocketOpened: true, SocketResponded: false,
				ServeOutcome: bamlutils.NativeStaticOutcomeTransportError,
			}),
			want: want{
				mPop:      one(popl(dispFailed)),
				mPhase:    {cellKey(sc("claimed")): 1, cellKey(sc("postclaim_terminal")): 1},
				mWinner:   one(scw("failure")),
				mPlan:     one(plan("match")),
				mSocket:   one(sock("transport_error")),
				mAttempts: one(att("transport_error")),
			},
		},
		{
			name: "baml parse error",
			res: fail(bamlutils.NativeSpineUnaryOracleObservations{
				PlanCompareRan: true, PlanMatched: true, SocketOpened: true, SocketResponded: true,
				SameResponseOracleRan: true, ErrorCompareRecorded: true, ErrorCompareMatch: false,
				ServeOutcome: bamlutils.NativeStaticOutcomeParseError,
			}),
			want: want{
				mPop:      one(popl(dispFailed)),
				mPhase:    {cellKey(sc("claimed")): 1, cellKey(sc("postclaim_terminal")): 1, cellKey(sc("same_response_oracle")): 1},
				mWinner:   one(scw("failure")),
				mPlan:     one(plan("match")),
				mSocket:   one(sock("responded")),
				mResp:     one(resp("error", "mismatch")),
				mAttempts: one(att("parse_error")),
			},
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
			want: want{
				mPop:      one(popl(dispFailed)),
				mPhase:    {cellKey(sc("claimed")): 1, cellKey(sc("postclaim_terminal")): 1, cellKey(sc("same_response_oracle")): 1},
				mWinner:   one(scw("failure")),
				mPlan:     one(plan("match")),
				mSocket:   one(sock("responded")),
				mAttempts: one(att("internal_error")),
			},
		},
		{
			name: "plan-mismatch decline",
			res:  decline(true, false),
			want: want{
				mPop:    one(popl(dispDeclined)),
				mPhase:  one(sc("preclaim_decline")),
				mWinner: one(scw("baml_transport")),
				mPlan:   one(plan("mismatch")),
			},
		},
		{
			name: "near-miss decline",
			res:  decline(false, false),
			want: want{
				mPop:    one(popl(dispDeclined)),
				mPhase:  one(sc("preclaim_decline")),
				mWinner: one(scw("baml_transport")),
			},
		},
		{
			// An out-of-contract unknown disposition: adaptOracleResult fails it closed, and the
			// recorder must still emit a terminal attempts{internal_error} — otherwise a
			// fail-closed terminal is unobserved on attempts_total (cubic factory.go:212).
			name: "unknown disposition fails closed",
			res:  bamlutils.NativeSpineUnaryOracleResult{Disposition: bamlutils.NativeSpineUnaryDisposition(99)},
			want: want{
				mPop:      one(popl(dispFailed)),
				mPhase:    {cellKey(sc("claimed")): 1, cellKey(sc("postclaim_terminal")): 1},
				mWinner:   one(scw("failure")),
				mAttempts: one(att("internal_error")),
			},
		},
	}

	// Every family is checked in every scenario, so an unexpected cell in a family a scenario
	// does not populate (e.g. a stray fe_v1 winner, or a fallback on a non-drift path) fails.
	families := []string{mPop, mPhase, mWinner, mPlan, mSocket, mResp, mFallback, mAttempts}
	inv := bamlutils.NativeStaticInvocation{Provider: "openai"}
	for _, s := range scenarios {
		t.Run(s.name, func(t *testing.T) {
			reg := prometheus.NewRegistry()
			m, err := admission.NewMetrics(reg)
			if err != nil {
				t.Fatalf("NewMetrics: %v", err)
			}
			popCounter, err := registerPopulationCounter(reg)
			if err != nil {
				t.Fatalf("registerPopulationCounter: %v", err)
			}
			recordOracle(m, popCounter, inv, s.res)

			for _, fam := range families {
				got := gatherFamily(t, reg, fam)
				exp := s.want[fam]
				if exp == nil {
					exp = map[string]float64{}
				}
				if !reflect.DeepEqual(got, exp) {
					t.Errorf("%s cells =\n    %v\nwant EXACTLY\n    %v\n(a missing, dropped, duplicated, relabeled, or unexpected label cell)", fam, got, exp)
				}
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
	if a != b {
		t.Errorf("second register returned a different collector: %p vs %p (reuse did not share the collector)", a, b)
	}
	// Increment through the SECOND handle: it must feed the registered collector, so a fresh
	// unregistered CounterVec returned as b would make this assertion fail.
	b.WithLabelValues(populationExactJSONU1, dispSucceeded).Inc()
	if got := counterValue(t, reg, "debaml_native_static_population_total", map[string]string{"population": populationExactJSONU1, "disposition": dispSucceeded}); got != 1 {
		t.Errorf("reused counter did not share state: got %v, want 1", got)
	}
}
