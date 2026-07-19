package buildrequest

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/bamlutils/retry"
)

// De-BAML Phase 7D orchestrator-seam tests. They exercise the native STREAM
// child-attempt seam in RunStreamOrchestration with a MOCK NativeStreamAttemptFunc
// (no nanollm, CGO-free) to prove the four load-bearing states independently of the
// concrete transport: the flag-off kill switch (zero native), native routing on
// Completed, pre-transport decline to BAML, and the FailedAfterClaim terminal that
// bypasses retry / fallback / reset / BAML-resend (scope §4 I1/I2/I4).

// nativeStreamSpy is a mock native stream attempt: it drives EmitDelta with a fixed
// delta script, optionally fires SendHeaders, and returns a per-call disposition.
type nativeStreamSpy struct {
	calls        atomic.Int32
	headerCalls  atomic.Int32
	deltas       []bamlutils.NativeStreamDelta
	fireHeaders  bool
	emitStopAt   int // if >0, EmitDelta is asked to stop (returns its error) at this delta index (1-based)
	dispositionN func(callN int) NativeStreamOutcome
}

func (s *nativeStreamSpy) attempt(_ context.Context, att NativeStreamAttempt) NativeStreamOutcome {
	n := int(s.calls.Add(1))
	if s.fireHeaders && att.SendHeaders != nil {
		att.SendHeaders()
		s.headerCalls.Add(1)
	}
	for i, d := range s.deltas {
		if err := att.EmitDelta(d); err != nil {
			// The orchestrator asked execution to stop (e.g. ctx cancel during a
			// partial send): a terminal FailedAfterClaim, never a retry.
			return FailNativeStreamAfterClaim(err, "")
		}
		if s.emitStopAt == i+1 {
			break
		}
	}
	return s.dispositionN(n)
}

// nativeStreamTestConfig builds a StreamConfig wired for the native seam plus the
// metadata plan (so winner_engine is observable) and returns spies for the BAML
// build closure and the native-only parsers.
func nativeStreamTestConfig(t *testing.T, spy *nativeStreamSpy, enabled bool) (
	cfg *StreamConfig,
	nativeParseStreamCalls *atomic.Int32,
	nativeParseFinalCalls *atomic.Int32,
) {
	t.Helper()
	nativeParseStreamCalls = &atomic.Int32{}
	nativeParseFinalCalls = &atomic.Int32{}
	cfg = &StreamConfig{
		Provider:      "openai",
		NeedsPartials: true,
		NeedsRaw:      true,
		MetadataPlan:  &bamlutils.Metadata{Client: "openai-leaf"},
		NewMetadataResult: func(md *bamlutils.Metadata) bamlutils.StreamResult {
			return &testResult{kind: bamlutils.StreamResultKindMetadata, metadata: md}
		},
		NativeAttemptEnabled: enabled,
		NativeAttempt:        spy.attempt,
		NativeMode:           bamlutils.NativeStreamModeStreamWithRaw,
		NativeParseStream: func(_ context.Context, accumulated string) (any, error) {
			nativeParseStreamCalls.Add(1)
			return "native-partial:" + accumulated, nil
		},
		NativeParseFinal: func(_ context.Context, accumulated string) (any, error) {
			nativeParseFinalCalls.Add(1)
			return "native-final:" + accumulated, nil
		},
		PlannedEngine: "native",
	}
	return cfg, nativeParseStreamCalls, nativeParseFinalCalls
}

func drainResults(out chan bamlutils.StreamResult) []*testResult {
	var results []*testResult
	for r := range out {
		results = append(results, r.(*testResult))
	}
	return results
}

func countKinds(results []*testResult) (heartbeats, partials, finals, errs, resets int, outcome *bamlutils.Metadata) {
	for _, r := range results {
		switch r.kind {
		case bamlutils.StreamResultKindHeartbeat:
			heartbeats++
		case bamlutils.StreamResultKindStream:
			partials++
			if r.reset {
				resets++
			}
		case bamlutils.StreamResultKindFinal:
			finals++
		case bamlutils.StreamResultKindError:
			errs++
		case bamlutils.StreamResultKindMetadata:
			if r.metadata != nil && r.metadata.Phase == bamlutils.MetadataPhaseOutcome {
				outcome = r.metadata
			}
		}
	}
	return
}

// TestNativeStream_FlagOff_KillSwitch is the kill-switch proof (I1): with the native
// seam DISABLED (NativeAttemptEnabled=false) the orchestrator invokes ZERO native
// work — the native attempt spy, the native-only parsers are all untouched — and the
// stream is served byte-identically by the BAML path.
func TestNativeStream_FlagOff_KillSwitch(t *testing.T) {
	server := makeOpenAIServer([]string{"Hello", " world"})
	defer server.Close()
	client := llmhttp.NewClient(server.Client())
	out := make(chan bamlutils.StreamResult, 100)

	spy := &nativeStreamSpy{
		deltas:       []bamlutils.NativeStreamDelta{{ParseableDelta: "X", RawDelta: "X"}},
		dispositionN: func(int) NativeStreamOutcome { return CompleteNativeStream("native") },
	}
	// enabled=false — the kill switch. The spy is installed but the enabled gate is
	// off, so the orchestrator must never call it.
	cfg, nativeParseStreamCalls, nativeParseFinalCalls := nativeStreamTestConfig(t, spy, false)

	var bamlParseFinalCalls atomic.Int32
	err := RunStreamOrchestration(
		context.Background(), out, cfg, client,
		func(context.Context, string) (*llmhttp.Request, error) {
			return &llmhttp.Request{URL: server.URL, Method: "POST", Body: `{}`}, nil
		},
		func(_ context.Context, accumulated string) (any, error) { return "baml:" + accumulated, nil },
		func(_ context.Context, accumulated string) (any, error) {
			bamlParseFinalCalls.Add(1)
			return "baml-final:" + accumulated, nil
		},
		newTestResult,
	)
	close(out)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got := spy.calls.Load(); got != 0 {
		t.Errorf("KILL SWITCH VIOLATED: native attempt called %d times with the flag off (want 0)", got)
	}
	if got := nativeParseStreamCalls.Load(); got != 0 {
		t.Errorf("KILL SWITCH VIOLATED: native-only ParseStream reached %d times with the flag off (want 0)", got)
	}
	if got := nativeParseFinalCalls.Load(); got != 0 {
		t.Errorf("KILL SWITCH VIOLATED: native-only ParseFinal reached %d times with the flag off (want 0)", got)
	}
	if got := bamlParseFinalCalls.Load(); got == 0 {
		t.Errorf("flag-off stream must be served by BAML, but BAML parseFinal was never called")
	}
	results := drainResults(out)
	_, _, finals, errs, _, outcome := countKinds(results)
	if finals != 1 || errs != 0 {
		t.Errorf("flag-off BAML stream: want 1 final, 0 errors; got finals=%d errors=%d", finals, errs)
	}
	if outcome != nil && outcome.WinnerEngine != "" {
		t.Errorf("flag-off winner_engine must be empty (BAML served), got %q", outcome.WinnerEngine)
	}
}

// TestNativeStream_Completed_RoutesNative proves the flag-on Completed path: the
// native attempt streams partials through EmitDelta (shared cadence + native-only
// parser), the orchestrator runs the native-only FINAL parse, emits winner_engine=
// native, and NEVER calls the BAML build/send.
func TestNativeStream_Completed_RoutesNative(t *testing.T) {
	out := make(chan bamlutils.StreamResult, 100)
	spy := &nativeStreamSpy{
		fireHeaders: true,
		deltas: []bamlutils.NativeStreamDelta{
			{ParseableDelta: "a", RawDelta: "a"},
			{ParseableDelta: "b", RawDelta: "b"},
			{ParseableDelta: "c", RawDelta: "c"},
		},
		dispositionN: func(int) NativeStreamOutcome { return CompleteNativeStream("native") },
	}
	cfg, nativeParseStreamCalls, nativeParseFinalCalls := nativeStreamTestConfig(t, spy, true)

	var bamlBuildCalls atomic.Int32
	err := RunStreamOrchestration(
		context.Background(), out, cfg, nil,
		func(context.Context, string) (*llmhttp.Request, error) {
			bamlBuildCalls.Add(1)
			return nil, errors.New("BAML build must not be called on the native Completed path")
		},
		func(_ context.Context, accumulated string) (any, error) { return "baml:" + accumulated, nil },
		func(_ context.Context, accumulated string) (any, error) { return "baml-final:" + accumulated, nil },
		newTestResult,
	)
	close(out)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := spy.calls.Load(); got != 1 {
		t.Errorf("exactly one native attempt expected, got %d", got)
	}
	if got := bamlBuildCalls.Load(); got != 0 {
		t.Errorf("NO BAML send after native claim (I4): BAML build called %d times (want 0)", got)
	}
	if got := nativeParseFinalCalls.Load(); got != 1 {
		t.Errorf("native-only ParseFinal should run exactly once on Completed, got %d", got)
	}
	if got := nativeParseStreamCalls.Load(); got == 0 {
		t.Errorf("native-only ParseStream should drive the partial cadence, got 0 calls")
	}
	results := drainResults(out)
	heartbeats, partials, finals, errs, _, outcome := countKinds(results)
	if finals != 1 || errs != 0 {
		t.Errorf("native Completed: want 1 final, 0 errors; got finals=%d errors=%d", finals, errs)
	}
	if partials == 0 {
		t.Errorf("native Completed must emit partials via EmitDelta cadence, got 0")
	}
	if heartbeats != 1 {
		t.Errorf("SendHeaders must emit exactly one heartbeat, got %d", heartbeats)
	}
	if outcome == nil || outcome.WinnerEngine != "native" {
		t.Errorf("winner_engine=native expected on outcome metadata, got %+v", outcome)
	}
	if outcome != nil && outcome.PlannedEngine != "native" {
		t.Errorf("planned_engine=native expected on outcome metadata, got %q", outcome.PlannedEngine)
	}
	// Final should carry the native-only final parse result.
	last := results[len(results)-1]
	if last.kind != bamlutils.StreamResultKindFinal || last.final != "native-final:abc" {
		t.Errorf("final should be the native-only parse of the accumulated text, got kind=%d final=%v", last.kind, last.final)
	}
}

// TestNativeStream_Declined_FallsToBAML proves the pre-transport decline (I2): a
// declined native attempt (no EmitDelta, no socket) falls through to the ordinary
// BAML build/send for the same child, with winner_engine unset (BAML served).
func TestNativeStream_Declined_FallsToBAML(t *testing.T) {
	server := makeOpenAIServer([]string{"Hi"})
	defer server.Close()
	client := llmhttp.NewClient(server.Client())
	out := make(chan bamlutils.StreamResult, 100)

	spy := &nativeStreamSpy{
		// No deltas: a decline asserts no EmitDelta occurred.
		dispositionN: func(int) NativeStreamOutcome {
			return DeclineNativeStream("strategy", "fallback_chain")
		},
	}
	cfg, nativeParseStreamCalls, nativeParseFinalCalls := nativeStreamTestConfig(t, spy, true)

	var bamlBuildCalls atomic.Int32
	err := RunStreamOrchestration(
		context.Background(), out, cfg, client,
		func(context.Context, string) (*llmhttp.Request, error) {
			bamlBuildCalls.Add(1)
			return &llmhttp.Request{URL: server.URL, Method: "POST", Body: `{}`}, nil
		},
		func(_ context.Context, accumulated string) (any, error) { return "baml:" + accumulated, nil },
		func(_ context.Context, accumulated string) (any, error) { return "baml-final:" + accumulated, nil },
		newTestResult,
	)
	close(out)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := spy.calls.Load(); got != 1 {
		t.Errorf("native attempt called %d times (want 1 decline)", got)
	}
	if got := bamlBuildCalls.Load(); got == 0 {
		t.Errorf("a pre-transport decline must fall through to the BAML build/send, but it was never called")
	}
	if got := nativeParseStreamCalls.Load() + nativeParseFinalCalls.Load(); got != 0 {
		t.Errorf("declined native attempt must not run the native parsers, got %d calls", got)
	}
	results := drainResults(out)
	_, _, finals, errs, _, outcome := countKinds(results)
	if finals != 1 || errs != 0 {
		t.Errorf("declined→BAML: want 1 final, 0 errors; got finals=%d errors=%d", finals, errs)
	}
	if outcome != nil && outcome.WinnerEngine != "" {
		t.Errorf("declined→BAML winner_engine must be empty, got %q", outcome.WinnerEngine)
	}
}

// TestNativeStream_FailedAfterClaim_Terminal is the crux no-replay proof (I4): a
// FailedAfterClaim outcome under a retry policy of MaxRetries=2 must produce EXACTLY
// ONE native attempt (no retry), NO reset marker, NO BAML build/send, and a single
// terminal error frame carrying the typed cause.
func TestNativeStream_FailedAfterClaim_Terminal(t *testing.T) {
	out := make(chan bamlutils.StreamResult, 100)
	sentinel := errors.New("provider stream truncated after claim")
	spy := &nativeStreamSpy{
		fireHeaders: true,
		// One partial reaches the client before the failure, so a naive retry owner
		// would emit a reset before replaying.
		deltas:     []bamlutils.NativeStreamDelta{{ParseableDelta: "x", RawDelta: "x"}},
		emitStopAt: 1,
		dispositionN: func(int) NativeStreamOutcome {
			return FailNativeStreamAfterClaim(sentinel, "x")
		},
	}
	cfg, _, _ := nativeStreamTestConfig(t, spy, true)
	cfg.RetryPolicy = &retry.Policy{MaxRetries: 2}

	var bamlBuildCalls atomic.Int32
	err := RunStreamOrchestration(
		context.Background(), out, cfg, nil,
		func(context.Context, string) (*llmhttp.Request, error) {
			bamlBuildCalls.Add(1)
			return nil, errors.New("BAML build must not be called after native claim")
		},
		func(_ context.Context, accumulated string) (any, error) { return accumulated, nil },
		func(_ context.Context, accumulated string) (any, error) { return accumulated, nil },
		newTestResult,
	)
	close(out)
	if err != nil {
		t.Fatalf("RunStreamOrchestration returned a non-nil error (errors flow via the channel): %v", err)
	}

	if got := spy.calls.Load(); got != 1 {
		t.Errorf("TERMINAL VIOLATED: native attempt ran %d times under MaxRetries=2 (want exactly 1 — no retry after claim)", got)
	}
	if got := bamlBuildCalls.Load(); got != 0 {
		t.Errorf("TERMINAL VIOLATED: BAML build/send ran %d times after native claim (want 0)", got)
	}
	results := drainResults(out)
	_, _, finals, errs, resets, _ := countKinds(results)
	if resets != 0 {
		t.Errorf("TERMINAL VIOLATED: %d reset markers emitted after native claim (want 0)", resets)
	}
	if finals != 0 {
		t.Errorf("a terminal native failure must not emit a final, got %d", finals)
	}
	if errs != 1 {
		t.Fatalf("want exactly 1 terminal error frame, got %d", errs)
	}
	// The terminal error frame must carry the typed cause (errors.Is reaches it
	// through the terminal + raw-carrying wrappers).
	var errFrame *testResult
	for _, r := range results {
		if r.kind == bamlutils.StreamResultKindError {
			errFrame = r
		}
	}
	if errFrame == nil || !errors.Is(errFrame.err, sentinel) {
		t.Errorf("terminal error frame should wrap the sentinel cause, got %v", errFrame.err)
	}
	// details.raw survives the terminal wrapper.
	if errFrame != nil && errFrame.raw != "x" {
		t.Errorf("terminal error frame should carry details.raw=%q, got %q", "x", errFrame.raw)
	}
}

// TestNativeStream_NilParserMisconfig_Terminal proves the fail-fast companion-callback
// guard (CodeRabbit r3610897349): a malformed/future installer that enables the native
// stream seam (NativeAttemptEnabled + NativeAttempt) but leaves a native-only parser
// closure nil must NOT panic (the Completed path deref'd NativeParseFinal
// unconditionally) and must NOT silently fall through to BAML. It fails TERMINALLY,
// fail-fast BEFORE the native attempt is even invoked (so no socket is claimed).
func TestNativeStream_NilParserMisconfig_Terminal(t *testing.T) {
	for _, tc := range []struct {
		name      string
		nilStream bool
		nilFinal  bool
	}{
		{"nil NativeParseFinal", false, true},
		{"nil NativeParseStream", true, false},
		{"both nil", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := make(chan bamlutils.StreamResult, 100)
			spy := &nativeStreamSpy{
				// Would return Completed → the Completed path used to deref NativeParseFinal.
				dispositionN: func(int) NativeStreamOutcome { return CompleteNativeStream("native") },
			}
			cfg, _, _ := nativeStreamTestConfig(t, spy, true)
			if tc.nilStream {
				cfg.NativeParseStream = nil
			}
			if tc.nilFinal {
				cfg.NativeParseFinal = nil
			}

			var bamlBuildCalls atomic.Int32
			// Must not panic on the nil closure.
			err := RunStreamOrchestration(
				context.Background(), out, cfg, nil,
				func(context.Context, string) (*llmhttp.Request, error) {
					bamlBuildCalls.Add(1)
					return nil, errors.New("BAML build must not be called on a nil-parser misconfig")
				},
				func(_ context.Context, accumulated string) (any, error) { return accumulated, nil },
				func(_ context.Context, accumulated string) (any, error) { return accumulated, nil },
				newTestResult,
			)
			close(out)
			if err != nil {
				t.Fatalf("RunStreamOrchestration returned a non-nil error (errors flow via the channel): %v", err)
			}
			if got := spy.calls.Load(); got != 0 {
				t.Errorf("FAIL-FAST: native attempt must NOT be invoked on a nil-parser misconfig, got %d calls", got)
			}
			if got := bamlBuildCalls.Load(); got != 0 {
				t.Errorf("nil-parser misconfig must NOT fall through to BAML (fail-loud terminal), got %d builds", got)
			}
			results := drainResults(out)
			_, _, finals, errs, resets, _ := countKinds(results)
			if errs != 1 {
				t.Errorf("want exactly 1 terminal error frame, got %d", errs)
			}
			if finals != 0 {
				t.Errorf("no final on a misconfig, got %d", finals)
			}
			if resets != 0 {
				t.Errorf("no reset on a terminal misconfig, got %d", resets)
			}
		})
	}
}

// TestNativeStream_FailedAfterClaim_NoFallbackAdvance proves the fallback owner does
// not advance to the next child on a native terminal (defense-in-depth for I4): a
// two-child chain whose FIRST child's native attempt fails-after-claim must not
// invoke the SECOND child's native attempt or any BAML build.
func TestNativeStream_FailedAfterClaim_NoFallbackAdvance(t *testing.T) {
	out := make(chan bamlutils.StreamResult, 100)
	spy := &nativeStreamSpy{
		dispositionN: func(int) NativeStreamOutcome {
			return FailNativeStreamAfterClaim(errors.New("claimed then died"), "")
		},
	}
	cfg, _, _ := nativeStreamTestConfig(t, spy, true)
	cfg.FallbackChain = []string{"child-a", "child-b"}
	cfg.ClientProviders = map[string]string{"child-a": "openai", "child-b": "openai"}

	var bamlBuildCalls atomic.Int32
	err := RunStreamOrchestration(
		context.Background(), out, cfg, nil,
		func(context.Context, string) (*llmhttp.Request, error) {
			bamlBuildCalls.Add(1)
			return nil, errors.New("BAML build must not be called")
		},
		func(_ context.Context, accumulated string) (any, error) { return accumulated, nil },
		func(_ context.Context, accumulated string) (any, error) { return accumulated, nil },
		newTestResult,
	)
	close(out)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := spy.calls.Load(); got != 1 {
		t.Errorf("FALLBACK REPLAY VIOLATED: native attempt ran %d times across the chain (want 1 — terminal breaks the fallback loop)", got)
	}
	if got := bamlBuildCalls.Load(); got != 0 {
		t.Errorf("no BAML build after a native terminal, got %d", got)
	}
	results := drainResults(out)
	_, _, _, errs, resets, _ := countKinds(results)
	if errs != 1 {
		t.Errorf("want 1 terminal error frame, got %d", errs)
	}
	if resets != 0 {
		t.Errorf("no reset after native terminal, got %d", resets)
	}
}
