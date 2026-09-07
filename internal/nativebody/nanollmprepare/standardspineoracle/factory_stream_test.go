package standardspineoracle

import (
	"context"
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/nativeserve/admission"
)

// factory_stream_test.go is the ExecBridge-U1s STREAM composite's adapter + metric proof.
// It drives the real NewStaticStreamServeFromExecutor over a stub executor, so it exercises
// the production mapping and the production recorder without a nanollm engine or a socket.

// stubStreamOracleExec returns a fixed result and records what it was handed.
type stubStreamOracleExec struct {
	res      bamlutils.NativeSpineStreamOracleResult
	calls    int
	gotInv   bamlutils.NativeStaticStreamOracleInvocation
	gotEmit  bamlutils.NativeSpineStreamEmit
	emitBack *bamlutils.NativeSpineStreamEvent
}

func (s *stubStreamOracleExec) StreamWithOracle(_ context.Context, inv bamlutils.NativeStaticStreamOracleInvocation, emit bamlutils.NativeSpineStreamEmit) bamlutils.NativeSpineStreamOracleResult {
	s.calls++
	s.gotInv = inv
	s.gotEmit = emit
	if s.emitBack != nil && emit != nil {
		_ = emit(*s.emitBack)
	}
	return s.res
}

// TestAdaptStreamOracleResult is the exhaustive spine-stream-oracle -> static-stream-serve
// disposition map, including the fail-closed unknown arm.
func TestAdaptStreamOracleResult(t *testing.T) {
	t.Run("declined preserves stage/reason and carries no payload", func(t *testing.T) {
		got := adaptStreamOracleResult(bamlutils.DeclinedSpineStreamOracleResult(errors.New("d"), "admission", "client_registry_present"))
		if got.Disposition != bamlutils.NativeStaticStreamOracleDeclined {
			t.Fatalf("disposition = %v, want declined", got.Disposition)
		}
		if got.Stage != "admission" || got.Reason != "client_registry_present" {
			t.Errorf("stage/reason = %q/%q, want the bounded tokens forwarded", got.Stage, got.Reason)
		}
		if got.Final != nil || got.Raw != "" || got.WinnerEngine != "" {
			t.Errorf("a decline carries a payload: %+v", got)
		}
	})

	t.Run("succeeded forwards the oracled final and winner", func(t *testing.T) {
		got := adaptStreamOracleResult(bamlutils.SucceededSpineStreamOracleResult("final", "raw", "think", bamlutils.NativeStaticServeEngineBAMLParse))
		if got.Disposition != bamlutils.NativeStaticStreamOracleSucceeded {
			t.Fatalf("disposition = %v, want succeeded", got.Disposition)
		}
		if got.Final != "final" || got.Raw != "raw" || got.Reasoning != "think" {
			t.Errorf("payload = %+v, want the oracled final and channels forwarded verbatim", got)
		}
		if got.WinnerEngine != bamlutils.NativeStaticServeEngineBAMLParse {
			t.Errorf("winner = %q, want the sticky attribution forwarded", got.WinnerEngine)
		}
	})

	t.Run("failed after claim never becomes a decline", func(t *testing.T) {
		boom := errors.New("boom")
		got := adaptStreamOracleResult(bamlutils.FailedAfterClaimSpineStreamOracleResult(boom, "stream", "emit_error", "partial-raw"))
		if got.Disposition != bamlutils.NativeStaticStreamOracleFailed {
			t.Fatalf("disposition = %v, want failed", got.Disposition)
		}
		if !errors.Is(got.Err, boom) || got.RawDiagnostic != "partial-raw" {
			t.Errorf("payload = %+v, want the typed cause and the owned raw diagnostic", got)
		}
	})

	t.Run("unknown disposition fails closed", func(t *testing.T) {
		got := adaptStreamOracleResult(bamlutils.NativeSpineStreamOracleResult{Disposition: 99})
		if got.Disposition != bamlutils.NativeStaticStreamOracleFailed {
			t.Fatalf("disposition = %v, want failed — an unknown disposition cannot assert zero sockets", got.Disposition)
		}
		if got.Err == nil {
			t.Error("the fail-closed arm carries no error")
		}
	})
}

// TestNewStaticStreamServeFromExecutor_ForwardsAndRecords drives the REAL composite: the
// invocation and emit sink reach the executor untouched, the result is adapted, and the
// bounded population + comparison counters move.
func TestNewStaticStreamServeFromExecutor_ForwardsAndRecords(t *testing.T) {
	reg := prometheus.NewRegistry()
	stub := &stubStreamOracleExec{
		res:      bamlutils.SucceededSpineStreamOracleResult("final", "raw", "", bamlutils.NativeStaticServeEngineNative),
		emitBack: &bamlutils.NativeSpineStreamEvent{HasPartial: true, Partial: "p"},
	}
	stub.res.Observations = bamlutils.NativeSpineStreamOracleObservations{
		PlanCompareRan: true, PlanMatched: true,
		SocketOpened: true, SocketResponded: true,
		PrefixComparisons: 3, PrefixMatch: 2, PrefixNativeNoValue: 1,
		FinalOracleRan: true, FinalCompare: bamlutils.NativeStreamCompareMatch,
		ServeOutcome: bamlutils.NativeStaticOutcomeSuccess,
	}
	serve, err := NewStaticStreamServeFromExecutor(reg, stub)
	if err != nil {
		t.Fatalf("NewStaticStreamServeFromExecutor: %v", err)
	}

	emitted := 0
	res := serve(context.Background(), bamlutils.NativeStaticStreamOracleInvocation{
		Method:   "M",
		Provider: "openai",
		NeedsRaw: true,
	}, func(bamlutils.NativeSpineStreamEvent) error {
		emitted++
		return nil
	})
	if stub.calls != 1 {
		t.Fatalf("the executor was driven %d time(s), want exactly 1", stub.calls)
	}
	if stub.gotInv.Method != "M" || stub.gotInv.Provider != "openai" {
		t.Errorf("the invocation was mutated on the way through: %+v", stub.gotInv)
	}
	if emitted != 1 {
		t.Errorf("the emit sink was invoked %d time(s); the composite must forward the caller's sink untouched", emitted)
	}
	if res.Disposition != bamlutils.NativeStaticStreamOracleSucceeded || res.Final != "final" {
		t.Errorf("result = %+v, want the adapted success", res)
	}

	if got := counterValue(t, reg, "debaml_native_static_stream_population_total", map[string]string{
		"population": populationExactJSONU1s, "disposition": dispSucceeded,
	}); got != 1 {
		t.Errorf("stream population{succeeded} = %v, want 1", got)
	}
	// The bounded comparison ledger is per-result, not per-request.
	if got := counterValue(t, reg, "debaml_native_static_stream_oracle_compare_total", map[string]string{
		"stage": compareStagePrefix, "result": string(bamlutils.NativeStreamCompareMatch),
	}); got != 2 {
		t.Errorf("prefix compare{match} = %v, want 2", got)
	}
	if got := counterValue(t, reg, "debaml_native_static_stream_oracle_compare_total", map[string]string{
		"stage": compareStagePrefix, "result": string(bamlutils.NativeStreamCompareNativeNoValue),
	}); got != 1 {
		t.Errorf("prefix compare{native_no_value} = %v, want 1", got)
	}
	if got := counterValue(t, reg, "debaml_native_static_stream_oracle_compare_total", map[string]string{
		"stage": compareStageFinal, "result": string(bamlutils.NativeStreamCompareMatch),
	}); got != 1 {
		t.Errorf("final compare{match} = %v, want 1", got)
	}
	// A classification with no occurrences stays ABSENT rather than being published as
	// an explicit zero, so a dashboard cannot read "we saw zero drifts" from a series
	// that was never touched.
	if got := counterValue(t, reg, "debaml_native_static_stream_oracle_compare_total", map[string]string{
		"stage": compareStagePrefix, "result": string(bamlutils.NativeStreamCompareBytesMismatch),
	}); got != 0 {
		t.Errorf("prefix compare{bytes_mismatch} = %v, want the cell absent", got)
	}
	// Surface/phase/winner land on the STREAM surface under the enrollment-free cohort.
	assertStreamSeries(t, reg, mPhase, map[string]string{
		"surface": "static_stream", "cohort": "none", "phase": "claimed",
	}, 1)
	assertStreamSeries(t, reg, mWinner, map[string]string{
		"surface": "static_stream", "cohort": "none", "winner": "native",
	}, 1)
	assertStreamSeries(t, reg, mPhase, map[string]string{
		"surface": "static_stream", "cohort": "none", "phase": "same_response_oracle",
	}, 1)
}

// TestStreamComposite_DeclineRecordsNoClaimedEvidence: a pre-socket decline records the
// decline and nothing that would imply a claim.
func TestStreamComposite_DeclineRecordsNoClaimedEvidence(t *testing.T) {
	reg := prometheus.NewRegistry()
	stub := &stubStreamOracleExec{res: bamlutils.DeclinedSpineStreamOracleResult(errors.New("d"), "registry", "method_not_registered")}
	serve, err := NewStaticStreamServeFromExecutor(reg, stub)
	if err != nil {
		t.Fatalf("NewStaticStreamServeFromExecutor: %v", err)
	}
	if got := serve(context.Background(), bamlutils.NativeStaticStreamOracleInvocation{Method: "M", Provider: "openai"}, func(bamlutils.NativeSpineStreamEvent) error { return nil }); got.Disposition != bamlutils.NativeStaticStreamOracleDeclined {
		t.Fatalf("disposition = %v, want declined", got.Disposition)
	}
	assertStreamSeries(t, reg, mPhase, map[string]string{
		"surface": "static_stream", "cohort": "none", "phase": "preclaim_decline",
	}, 1)
	assertStreamSeries(t, reg, mPhase, map[string]string{
		"surface": "static_stream", "cohort": "none", "phase": "claimed",
	}, 0)
	if got := counterValue(t, reg, "debaml_native_static_stream_population_total", map[string]string{
		"population": populationExactJSONU1s, "disposition": dispDeclined,
	}); got != 1 {
		t.Errorf("stream population{declined} = %v, want 1", got)
	}
	// A decline records NO serve outcome AT ALL: attempts_total counts claimed attempts.
	// Checking only `success` would let the fail-closed internal_error branch start firing
	// on declines unnoticed, which is the regression most likely to happen here.
	for _, mode := range []admission.Mode{admission.ModeStream, admission.ModeStreamWithRaw} {
		for _, outcome := range []string{"success", "internal_error", "parse_error", "provider_error", "transport_error"} {
			if got := counterValue(t, reg, mAttempts, map[string]string{
				"mode": string(mode), "engine": "native", "provider": "openai", "outcome": outcome,
			}); got != 0 {
				t.Errorf("attempts_total{%s,%s} = %v on a decline, want 0", mode, outcome, got)
			}
		}
	}
}

// TestStreamComposite_UnknownDispositionNeverPublishesAMappedOutcome pins the fail-closed
// metric arm: an out-of-contract disposition is reported as a FAILURE by the adapter, so the
// serve outcome it happens to carry must not be published — least of all a `success` on a
// request the adapter is about to fail.
func TestStreamComposite_UnknownDispositionNeverPublishesAMappedOutcome(t *testing.T) {
	reg := prometheus.NewRegistry()
	stub := &stubStreamOracleExec{res: bamlutils.NativeSpineStreamOracleResult{Disposition: 99}}
	stub.res.Observations.ServeOutcome = bamlutils.NativeStaticOutcomeSuccess
	serve, err := NewStaticStreamServeFromExecutor(reg, stub)
	if err != nil {
		t.Fatalf("NewStaticStreamServeFromExecutor: %v", err)
	}
	got := serve(context.Background(), bamlutils.NativeStaticStreamOracleInvocation{Method: "M", Provider: "openai"}, func(bamlutils.NativeSpineStreamEvent) error { return nil })
	if got.Disposition != bamlutils.NativeStaticStreamOracleFailed {
		t.Fatalf("disposition = %v, want failed (fail-closed)", got.Disposition)
	}
	if v := counterValue(t, reg, mAttempts, map[string]string{
		"mode": "stream", "engine": "native", "provider": "openai", "outcome": "success",
	}); v != 0 {
		t.Errorf("attempts_total{success} = %v for an unknown disposition the adapter FAILED; the carried outcome must not be published", v)
	}
	if v := counterValue(t, reg, mAttempts, map[string]string{
		"mode": "stream", "engine": "native", "provider": "openai", "outcome": "internal_error",
	}); v != 1 {
		t.Errorf("attempts_total{internal_error} = %v, want 1 — a fail-closed terminal is still an attempt and must be counted", v)
	}
}

// TestStreamComposite_SucceededWithoutAnOutcomeIsNotInternalError matches the unary twin: a
// success that carried no resolver outcome must not be mislabelled a failure. Unreachable
// with the production executor (which always sets one), which is exactly why the arm needs
// a test rather than a reader's assumption.
func TestStreamComposite_SucceededWithoutAnOutcomeIsNotInternalError(t *testing.T) {
	reg := prometheus.NewRegistry()
	stub := &stubStreamOracleExec{res: bamlutils.SucceededSpineStreamOracleResult("f", "", "", bamlutils.NativeStaticServeEngineNative)}
	// Observations deliberately left zero: ServeOutcome == NativeStaticOutcomeNone.
	serve, err := NewStaticStreamServeFromExecutor(reg, stub)
	if err != nil {
		t.Fatalf("NewStaticStreamServeFromExecutor: %v", err)
	}
	serve(context.Background(), bamlutils.NativeStaticStreamOracleInvocation{Method: "M", Provider: "openai"}, func(bamlutils.NativeSpineStreamEvent) error { return nil })
	if v := counterValue(t, reg, mAttempts, map[string]string{
		"mode": "stream", "engine": "native", "provider": "openai", "outcome": "internal_error",
	}); v != 0 {
		t.Errorf("attempts_total{internal_error} = %v for a SUCCEEDED stream; a served request must never read as an internal error", v)
	}
}

// TestStreamComposite_UnknownCompareTokenFoldsOntoOneLabel pins the metric's bounded
// cardinality: a classification the recorder does not know must fold onto a fixed label
// rather than be published verbatim as a new label value — and must still be COUNTED, since
// dropping it would under-report drift.
func TestStreamComposite_UnknownCompareTokenFoldsOntoOneLabel(t *testing.T) {
	reg := prometheus.NewRegistry()
	stub := &stubStreamOracleExec{res: bamlutils.SucceededSpineStreamOracleResult("f", "", "", bamlutils.NativeStaticServeEngineNative)}
	stub.res.Observations = bamlutils.NativeSpineStreamOracleObservations{
		PrefixComparisons: 1, FinalOracleRan: true,
		FinalCompare: bamlutils.NativeStreamOracleCompare("a_token_from_the_future"),
		ServeOutcome: bamlutils.NativeStaticOutcomeSuccess,
	}
	serve, err := NewStaticStreamServeFromExecutor(reg, stub)
	if err != nil {
		t.Fatalf("NewStaticStreamServeFromExecutor: %v", err)
	}
	serve(context.Background(), bamlutils.NativeStaticStreamOracleInvocation{Method: "M", Provider: "openai"}, func(bamlutils.NativeSpineStreamEvent) error { return nil })
	if v := counterValue(t, reg, "debaml_native_static_stream_oracle_compare_total", map[string]string{
		"stage": compareStageFinal, "result": "a_token_from_the_future",
	}); v != 0 {
		t.Errorf("an unrecognized classification was published verbatim as a label value (%v); the series' cardinality must be bounded by the code", v)
	}
	if v := counterValue(t, reg, "debaml_native_static_stream_oracle_compare_total", map[string]string{
		"stage": compareStageFinal, "result": compareOther,
	}); v != 1 {
		t.Errorf("compare{final,other} = %v, want 1 — an unknown classification folds onto one label but is still counted", v)
	}
}

// TestStreamComposite_ClaimedFailureWithoutOutcomeRecordsInternalError: a claimed terminal
// that carried no resolver outcome (a post-claim panic, say) must still move attempts_total
// — otherwise a fail-closed terminal would be silently unobserved.
func TestStreamComposite_ClaimedFailureWithoutOutcomeRecordsInternalError(t *testing.T) {
	reg := prometheus.NewRegistry()
	stub := &stubStreamOracleExec{res: bamlutils.FailedAfterClaimSpineStreamOracleResult(errors.New("panic"), "stream", "panic", "")}
	serve, err := NewStaticStreamServeFromExecutor(reg, stub)
	if err != nil {
		t.Fatalf("NewStaticStreamServeFromExecutor: %v", err)
	}
	serve(context.Background(), bamlutils.NativeStaticStreamOracleInvocation{Method: "M", Provider: "openai", NeedsRaw: true}, func(bamlutils.NativeSpineStreamEvent) error { return nil })
	if got := counterValue(t, reg, mAttempts, map[string]string{
		"mode": "stream_with_raw", "engine": "native", "provider": "openai", "outcome": "internal_error",
	}); got != 1 {
		t.Errorf("attempts_total{stream_with_raw,internal_error} = %v, want 1", got)
	}
	assertStreamSeries(t, reg, mWinner, map[string]string{
		"surface": "static_stream", "cohort": "none", "winner": "failure",
	}, 1)
}

// TestStreamComposite_ModeLabelFollowsTheRequest pins that the serve outcome is recorded
// under the request's REAL streaming mode, so /stream and /stream-with-raw stay separable.
func TestStreamComposite_ModeLabelFollowsTheRequest(t *testing.T) {
	for _, tc := range []struct {
		needsRaw bool
		want     admission.Mode
	}{
		{false, admission.ModeStream},
		{true, admission.ModeStreamWithRaw},
	} {
		reg := prometheus.NewRegistry()
		stub := &stubStreamOracleExec{res: bamlutils.SucceededSpineStreamOracleResult("f", "", "", bamlutils.NativeStaticServeEngineNative)}
		stub.res.Observations.ServeOutcome = bamlutils.NativeStaticOutcomeSuccess
		serve, err := NewStaticStreamServeFromExecutor(reg, stub)
		if err != nil {
			t.Fatalf("NewStaticStreamServeFromExecutor: %v", err)
		}
		serve(context.Background(), bamlutils.NativeStaticStreamOracleInvocation{Method: "M", Provider: "openai", NeedsRaw: tc.needsRaw}, func(bamlutils.NativeSpineStreamEvent) error { return nil })
		if got := counterValue(t, reg, mAttempts, map[string]string{
			"mode": string(tc.want), "engine": "native", "provider": "openai", "outcome": "success",
		}); got != 1 {
			t.Errorf("attempts_total{%s} = %v, want 1", tc.want, got)
		}
	}
}

// TestStreamComposite_SubstitutedWinnerRecordsTheSameResponseFallback pins that a stream
// served from BAML's parse of the SAME response records the fallback exactly as the unary
// composite does.
func TestStreamComposite_SubstitutedWinnerRecordsTheSameResponseFallback(t *testing.T) {
	reg := prometheus.NewRegistry()
	stub := &stubStreamOracleExec{res: bamlutils.SucceededSpineStreamOracleResult("f", "", "", bamlutils.NativeStaticServeEngineBAMLParse)}
	stub.res.Observations = bamlutils.NativeSpineStreamOracleObservations{
		PrefixComparisons: 1, PrefixBytesMismatch: 1, Substituted: true,
		FinalOracleRan: true, FinalCompare: bamlutils.NativeStreamCompareMatch,
		ServeOutcome: bamlutils.NativeStaticOutcomeSuccess,
	}
	serve, err := NewStaticStreamServeFromExecutor(reg, stub)
	if err != nil {
		t.Fatalf("NewStaticStreamServeFromExecutor: %v", err)
	}
	serve(context.Background(), bamlutils.NativeStaticStreamOracleInvocation{Method: "M", Provider: "openai"}, func(bamlutils.NativeSpineStreamEvent) error { return nil })
	if got := counterValue(t, reg, mFallback, map[string]string{"kind": "parse_only"}); got != 1 {
		t.Errorf("fallback{parse_only} = %v, want 1", got)
	}
	assertStreamSeries(t, reg, mWinner, map[string]string{
		"surface": "static_stream", "cohort": "none", "winner": "baml_parse_same_response",
	}, 1)
}

// TestNewStaticStreamServeFromExecutor_RefusesANilExecutor: a nil executor is a wiring bug,
// not an all-decline lane.
func TestNewStaticStreamServeFromExecutor_RefusesANilExecutor(t *testing.T) {
	if _, err := NewStaticStreamServeFromExecutor(prometheus.NewRegistry(), nil); err == nil {
		t.Fatal("a nil stream oracle executor was accepted")
	}
}

// TestStreamCountersAreReusableOnASharedRegistry: the worker builds its factories against
// ONE registry, so constructing the composite twice must reuse the collectors rather than
// panic on duplicate registration.
func TestStreamCountersAreReusableOnASharedRegistry(t *testing.T) {
	reg := prometheus.NewRegistry()
	stub := &stubStreamOracleExec{res: bamlutils.DeclinedSpineStreamOracleResult(errors.New("d"), "registry", "method_not_registered")}
	if _, err := NewStaticStreamServeFromExecutor(reg, stub); err != nil {
		t.Fatalf("first construction: %v", err)
	}
	if _, err := NewStaticStreamServeFromExecutor(reg, stub); err != nil {
		t.Fatalf("second construction on the same registry: %v", err)
	}
	// The unary composite installs on the same registry too; both must coexist.
	if _, err := NewStaticServeFromExecutor(reg, &stubUnaryOracleExec{}); err != nil {
		t.Fatalf("unary composite on the same registry: %v", err)
	}
}

// stubUnaryOracleExec is the minimal unary oracle executor for the shared-registry check.
type stubUnaryOracleExec struct {
	bamlutils.NativeSpineUnaryOracleExecutor
}

// assertStreamSeries reads one labeled counter cell and compares it.
func assertStreamSeries(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string, want float64) {
	t.Helper()
	if got := counterValue(t, reg, name, labels); got != want {
		t.Errorf("%s%v = %v, want %v", name, labels, got, want)
	}
}
