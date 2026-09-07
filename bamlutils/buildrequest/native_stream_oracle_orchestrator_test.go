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

// ExecBridge-U1s orchestrator-seam tests. They exercise the ORACLE-OWNED native STREAM
// seam with a MOCK NativeStreamOracleAttemptFunc (no nanollm, CGO-free) and prove the
// properties that distinguish it from the legacy transport-only seam next to it:
//
//   - on the oracle-owned SUCCESS path the orchestrator NEVER calls NativeParseStream,
//     NativeParseFinal, or the legacy EmitDelta cadence — the events it publishes are the
//     ones the oracle already resolved, and the final is the one the oracle already
//     compared;
//   - a pre-socket DECLINE falls through to the existing BAML build/send for the same
//     child, exactly once;
//   - a FailedAfterClaim is TERMINAL: no retry, no fallback, no reset, no BAML resend;
//   - installing BOTH native stream seams FAILS CLOSED before either callback runs.

// oracleStreamSpy is a mock oracle-owned native stream attempt: it publishes a scripted
// list of ALREADY-RESOLVED events through EmitResolved, optionally fires SendHeaders, and
// returns a per-call disposition.
type oracleStreamSpy struct {
	calls        atomic.Int32
	headerCalls  atomic.Int32
	events       []bamlutils.NativeSpineStreamEvent
	fireHeaders  bool
	dispositionN func(callN int) NativeStreamOracleOutcome
}

func (s *oracleStreamSpy) attempt(_ context.Context, att NativeStreamOracleAttempt) NativeStreamOracleOutcome {
	n := int(s.calls.Add(1))
	if s.fireHeaders && att.SendHeaders != nil {
		att.SendHeaders()
		s.headerCalls.Add(1)
	}
	for _, ev := range s.events {
		if err := att.EmitResolved(ev); err != nil {
			// The orchestrator asked the claimed stream to stop (a cancelled partial
			// send): a terminal FailedAfterClaim, never a retry.
			return FailNativeStreamOracleAfterClaim(err, "")
		}
	}
	return s.dispositionN(n)
}

// oracleStreamTestConfig builds a StreamConfig wired for the ORACLE seam. It deliberately
// ALSO installs the legacy native-only parser closures as SPIES: the oracle owns the parse,
// so a success path that touches either of them is the double-parse bug this seam exists to
// prevent, and these counters are what catch it.
func oracleStreamTestConfig(t *testing.T, spy *oracleStreamSpy, enabled bool) (
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
		NativeOracleAttemptEnabled: enabled,
		NativeOracleAttempt:        spy.attempt,
		NativeMode:                 bamlutils.NativeStreamModeStreamWithRaw,
		NativeParseStream: func(context.Context, string) (any, error) {
			nativeParseStreamCalls.Add(1)
			return "legacy-partial", nil
		},
		NativeParseFinal: func(context.Context, string) (any, error) {
			nativeParseFinalCalls.Add(1)
			return "legacy-final", nil
		},
		PlannedEngine: "native",
	}
	return cfg, nativeParseStreamCalls, nativeParseFinalCalls
}

// resolvedEvents is a small scripted transcript of ALREADY-ORACLED events.
func resolvedEvents() []bamlutils.NativeSpineStreamEvent {
	return []bamlutils.NativeSpineStreamEvent{
		{HasPartial: true, Partial: "resolved-1", Raw: "a"},
		{Raw: "b"},
		{HasPartial: true, Partial: "resolved-2", Raw: "c"},
	}
}

// TestNativeStreamOracle_Completed_OwnsTheParseAndTheFinal is the headline: on the
// oracle-owned success path the orchestrator publishes the resolved events verbatim,
// returns the ALREADY-ORACLED final, and touches neither native parser nor the BAML build.
//
// The two zero-count assertions are the discriminating ones. An implementation that reused
// the legacy Completed arm (or allocated the outer cadence for this branch) would re-parse
// the accumulated text with NativeParseFinal and publish a final the oracle never compared.
func TestNativeStreamOracle_Completed_OwnsTheParseAndTheFinal(t *testing.T) {
	out := make(chan bamlutils.StreamResult, 100)
	spy := &oracleStreamSpy{
		fireHeaders: true,
		events:      resolvedEvents(),
		dispositionN: func(int) NativeStreamOracleOutcome {
			return CompleteNativeStreamOracle("oracled-final", "abc", "think", bamlutils.NativeStaticServeEngineNative)
		},
	}
	cfg, nativeParseStreamCalls, nativeParseFinalCalls := oracleStreamTestConfig(t, spy, true)

	var bamlBuildCalls atomic.Int32
	err := RunStreamOrchestration(
		context.Background(), out, cfg, nil,
		func(context.Context, string) (*llmhttp.Request, error) {
			bamlBuildCalls.Add(1)
			return nil, errors.New("BAML build must not be called on the oracle-owned Completed path")
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
		t.Errorf("exactly one oracle-owned attempt expected, got %d", got)
	}
	if got := bamlBuildCalls.Load(); got != 0 {
		t.Errorf("NO BAML send after the claim: BAML build called %d times (want 0)", got)
	}
	if got := nativeParseStreamCalls.Load(); got != 0 {
		t.Errorf("the oracle-owned path invoked NativeParseStream %d time(s); the oracle already resolved every prefix, so a second parse would publish an uncompared partial", got)
	}
	if got := nativeParseFinalCalls.Load(); got != 0 {
		t.Errorf("the oracle-owned path invoked NativeParseFinal %d time(s); the final it returns was ALREADY compared, so re-parsing would discard the oracle's decision", got)
	}
	results := drainResults(out)
	heartbeats, partials, finals, errs, resets, outcome := countKinds(results)
	if finals != 1 || errs != 0 {
		t.Errorf("oracle-owned Completed: want 1 final, 0 errors; got finals=%d errors=%d", finals, errs)
	}
	if resets != 0 {
		t.Errorf("a claimed stream emitted %d reset(s); there is no continuation to reset to", resets)
	}
	// Three resolved events were published: two structured + one raw-only.
	if partials != len(resolvedEvents()) {
		t.Errorf("published %d partial frame(s), want %d — EmitResolved delivers what the oracle decided, verbatim", partials, len(resolvedEvents()))
	}
	if heartbeats != 1 {
		t.Errorf("SendHeaders must emit exactly one heartbeat, got %d", heartbeats)
	}
	if outcome == nil || outcome.WinnerEngine != bamlutils.NativeStaticServeEngineNative {
		t.Errorf("outcome winner_engine = %v, want the oracle's bounded token", outcome)
	}
	// The FINAL the client receives is the one the oracle returned, not a re-parse.
	var finalPayload any
	for _, r := range results {
		if r.kind == bamlutils.StreamResultKindFinal {
			finalPayload = r.final
		}
	}
	if finalPayload != "oracled-final" {
		t.Errorf("published final = %#v, want the already-oracled value", finalPayload)
	}
}

// TestNativeStreamOracle_FlagOff_KillSwitch: with the enabled gate off the orchestrator
// invokes ZERO oracle work and BAML serves the stream byte-identically.
func TestNativeStreamOracle_FlagOff_KillSwitch(t *testing.T) {
	server := makeOpenAIServer([]string{"Hello", " world"})
	defer server.Close()
	client := llmhttp.NewClient(server.Client())
	out := make(chan bamlutils.StreamResult, 100)

	spy := &oracleStreamSpy{
		events: resolvedEvents(),
		dispositionN: func(int) NativeStreamOracleOutcome {
			return CompleteNativeStreamOracle("oracled-final", "", "", bamlutils.NativeStaticServeEngineNative)
		},
	}
	cfg, _, _ := oracleStreamTestConfig(t, spy, false)

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
		t.Errorf("KILL SWITCH VIOLATED: the oracle attempt ran %d time(s) with the gate off", got)
	}
	if bamlParseFinalCalls.Load() == 0 {
		t.Error("a gate-off stream must be served by BAML, but BAML parseFinal was never called")
	}
	_, _, finals, errs, _, outcome := countKinds(drainResults(out))
	if finals != 1 || errs != 0 {
		t.Errorf("gate-off BAML stream: want 1 final, 0 errors; got finals=%d errors=%d", finals, errs)
	}
	if outcome != nil && outcome.WinnerEngine != "" {
		t.Errorf("gate-off winner_engine must be empty (BAML served), got %q", outcome.WinnerEngine)
	}
}

// TestNativeStreamOracle_Declined_FallsThroughToBAMLOnce: a pre-socket decline runs the
// existing BAML build/send for the SAME child, exactly once, and publishes BAML's own
// partials and final.
func TestNativeStreamOracle_Declined_FallsThroughToBAMLOnce(t *testing.T) {
	server := makeOpenAIServer([]string{"Hello", " world"})
	defer server.Close()
	client := llmhttp.NewClient(server.Client())
	out := make(chan bamlutils.StreamResult, 100)

	spy := &oracleStreamSpy{
		dispositionN: func(int) NativeStreamOracleOutcome {
			return DeclineNativeStreamOracle("registry", "method_not_registered")
		},
	}
	cfg, nativeParseStreamCalls, nativeParseFinalCalls := oracleStreamTestConfig(t, spy, true)

	var bamlBuildCalls, bamlParseFinalCalls atomic.Int32
	err := RunStreamOrchestration(
		context.Background(), out, cfg, client,
		func(context.Context, string) (*llmhttp.Request, error) {
			bamlBuildCalls.Add(1)
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
	if got := spy.calls.Load(); got != 1 {
		t.Errorf("the oracle attempt ran %d time(s), want exactly 1 before the decline", got)
	}
	if got := bamlBuildCalls.Load(); got != 1 {
		t.Errorf("BAML build ran %d time(s) after a decline, want exactly 1 (same child, same retry iteration)", got)
	}
	if bamlParseFinalCalls.Load() != 1 {
		t.Errorf("BAML parseFinal ran %d time(s), want exactly 1 — BAML owns a declined stream end to end", bamlParseFinalCalls.Load())
	}
	if got := nativeParseStreamCalls.Load() + nativeParseFinalCalls.Load(); got != 0 {
		t.Errorf("a declined oracle stream reached the native-only parsers %d time(s); the BAML fall-through must be byte-identical to today", got)
	}
	_, _, finals, errs, _, outcome := countKinds(drainResults(out))
	if finals != 1 || errs != 0 {
		t.Errorf("declined -> BAML: want 1 final, 0 errors; got finals=%d errors=%d", finals, errs)
	}
	if outcome != nil && outcome.WinnerEngine != "" {
		t.Errorf("a declined stream is served by BAML, so winner_engine must be empty, got %q", outcome.WinnerEngine)
	}
}

// TestNativeStreamOracle_FailedAfterClaim_IsTerminal: a post-claim failure produces ONE
// error frame with no reset, consumes no retry, advances no fallback child, and never
// reaches the BAML build.
func TestNativeStreamOracle_FailedAfterClaim_IsTerminal(t *testing.T) {
	out := make(chan bamlutils.StreamResult, 100)
	boom := errors.New("provider stream truncated")
	spy := &oracleStreamSpy{
		events: []bamlutils.NativeSpineStreamEvent{{HasPartial: true, Partial: "resolved-1", Raw: "a"}},
		dispositionN: func(int) NativeStreamOracleOutcome {
			return FailNativeStreamOracleAfterClaim(boom, "a")
		},
	}
	cfg, _, _ := oracleStreamTestConfig(t, spy, true)
	// A retry policy that WOULD retry, so "no retry" is a real observation rather than
	// the absence of an opportunity.
	cfg.RetryPolicy = &retry.Policy{MaxRetries: 3, Strategy: &retry.ConstantDelay{DelayMs: 1}}

	var bamlBuildCalls atomic.Int32
	err := RunStreamOrchestration(
		context.Background(), out, cfg, nil,
		func(context.Context, string) (*llmhttp.Request, error) {
			bamlBuildCalls.Add(1)
			return nil, errors.New("BAML build must not run after a claimed native stream failed")
		},
		func(_ context.Context, accumulated string) (any, error) { return "baml:" + accumulated, nil },
		func(_ context.Context, accumulated string) (any, error) { return "baml-final:" + accumulated, nil },
		newTestResult,
	)
	close(out)
	// Terminal stream errors flow through the CHANNEL, not the return value.
	if err != nil {
		t.Fatalf("RunStreamOrchestration returned a non-nil error (errors flow via the channel): %v", err)
	}
	if got := spy.calls.Load(); got != 1 {
		t.Errorf("the oracle attempt ran %d time(s) despite MaxRetries=3; a post-claim terminal must bypass retry.Execute", got)
	}
	if got := bamlBuildCalls.Load(); got != 0 {
		t.Errorf("BAML build ran %d time(s) after a claimed failure (want 0) — there is no fallback after the claim", got)
	}
	results := drainResults(out)
	_, _, finals, errs, resets, _ := countKinds(results)
	if finals != 0 {
		t.Errorf("a terminal stream published %d final(s), want 0", finals)
	}
	if resets != 0 {
		t.Errorf("a terminal claimed stream published %d reset(s), want 0 — a reset invites a continuation that cannot happen", resets)
	}
	if errs != 1 {
		t.Fatalf("want exactly 1 terminal error frame, got %d", errs)
	}
	var errFrame *testResult
	for _, r := range results {
		if r.kind == bamlutils.StreamResultKindError {
			errFrame = r
		}
	}
	if errFrame == nil || !errors.Is(errFrame.err, boom) {
		t.Errorf("the terminal error frame should wrap the typed cause, got %v", errFrame)
	}
	if errFrame != nil && errFrame.raw != "a" {
		t.Errorf("the terminal error frame should carry details.raw=%q, got %q", "a", errFrame.raw)
	}
}

// TestNativeStreamOracle_BothSeamsInstalledFailsClosed is the fail-closed guard. The two
// native stream seams own different amounts of the request, so with both installed there is
// no safe choice — running the legacy one leaves a default-serve lane without its oracle,
// and running the oracle one beside the legacy parsers invites a second parse. It must
// terminate BEFORE either callback runs, so no socket is claimed.
func TestNativeStreamOracle_BothSeamsInstalledFailsClosed(t *testing.T) {
	out := make(chan bamlutils.StreamResult, 100)
	oracleSpy := &oracleStreamSpy{
		dispositionN: func(int) NativeStreamOracleOutcome {
			return CompleteNativeStreamOracle("oracled-final", "", "", bamlutils.NativeStaticServeEngineNative)
		},
	}
	legacySpy := &nativeStreamSpy{
		dispositionN: func(int) NativeStreamOutcome { return CompleteNativeStream("native") },
	}
	cfg, _, _ := oracleStreamTestConfig(t, oracleSpy, true)
	cfg.NativeAttemptEnabled = true
	cfg.NativeAttempt = legacySpy.attempt

	var bamlBuildCalls atomic.Int32
	err := RunStreamOrchestration(
		context.Background(), out, cfg, nil,
		func(context.Context, string) (*llmhttp.Request, error) {
			bamlBuildCalls.Add(1)
			return nil, errors.New("BAML build must not run for a misconfigured double-installed seam")
		},
		func(_ context.Context, accumulated string) (any, error) { return "baml:" + accumulated, nil },
		func(_ context.Context, accumulated string) (any, error) { return "baml-final:" + accumulated, nil },
		newTestResult,
	)
	close(out)
	if err != nil {
		t.Fatalf("RunStreamOrchestration returned a non-nil error (errors flow via the channel): %v", err)
	}
	if got := oracleSpy.calls.Load() + legacySpy.calls.Load(); got != 0 {
		t.Errorf("a native attempt ran %d time(s) for a double-installed seam; the guard must fire BEFORE either callback, so no socket is claimed", got)
	}
	if got := bamlBuildCalls.Load(); got != 0 {
		t.Errorf("BAML build ran %d time(s) for a double-installed seam; silently falling through would mask the broken wiring", got)
	}
	results := drainResults(out)
	_, _, finals, errs, _, _ := countKinds(results)
	if finals != 0 {
		t.Errorf("a misconfigured seam published %d final(s), want 0", finals)
	}
	if errs != 1 {
		t.Fatalf("want exactly 1 terminal error frame for a double-installed seam, got %d", errs)
	}
}

// TestNativeStreamOracle_EmptyResolvedEventIsNotPublished: the sink drops a fully empty
// event rather than publishing a bogus null partial frame. It is the streaming analogue of
// the reset-only guard in worker/stream.go, at the seam where a resolved "no structured
// partial, no raw" tick can legitimately arrive.
func TestNativeStreamOracle_EmptyResolvedEventIsNotPublished(t *testing.T) {
	out := make(chan bamlutils.StreamResult, 100)
	spy := &oracleStreamSpy{
		events: []bamlutils.NativeSpineStreamEvent{
			{}, // suppressed tick: nothing to publish
			{HasPartial: true, Partial: "resolved-1"}, // a real one
		},
		dispositionN: func(int) NativeStreamOracleOutcome {
			return CompleteNativeStreamOracle("oracled-final", "", "", bamlutils.NativeStaticServeEngineNative)
		},
	}
	cfg, _, _ := oracleStreamTestConfig(t, spy, true)

	err := RunStreamOrchestration(
		context.Background(), out, cfg, nil,
		func(context.Context, string) (*llmhttp.Request, error) {
			return nil, errors.New("BAML build must not be called")
		},
		func(_ context.Context, accumulated string) (any, error) { return "baml:" + accumulated, nil },
		func(_ context.Context, accumulated string) (any, error) { return "baml-final:" + accumulated, nil },
		newTestResult,
	)
	close(out)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_, partials, finals, _, _, _ := countKinds(drainResults(out))
	if partials != 1 {
		t.Errorf("published %d partial frame(s), want exactly 1 — a fully empty resolved event carries nothing to publish", partials)
	}
	if finals != 1 {
		t.Errorf("finals = %d, want 1", finals)
	}
}
