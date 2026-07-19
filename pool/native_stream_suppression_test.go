package pool

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/workerplugin"
)

// De-BAML Phase 7D pool stream-retry suppression tests. The pool is the SECOND
// replay owner (scope §3.5): a native worker can CLAIM its one provider socket and
// then crash before the host observes a terminal frame, and the ordinary retry loop
// would replay the whole logical stream on another worker — a SECOND physical native
// request (I3/I4). With Config.DisableStreamInfrastructureRetries the pool performs
// exactly ONE worker attempt and never replays / injects a reset, while still
// marking worker health and dispatching a replacement.

// makeMidStreamFailWorker returns a worker whose CallStream forwards one partial and
// then a retryable worker error, incrementing callCount on every invocation so a
// replay (a second CallStream, on the restart worker) is observable.
func makeMidStreamFailWorker(callCount *atomic.Int32, fail bool) *mockWorker {
	w := newMockWorker()
	w.callStreamFn = func(context.Context, string, []byte, bamlutils.StreamMode) (<-chan *workerplugin.StreamResult, error) {
		callCount.Add(1)
		ch := make(chan *workerplugin.StreamResult, 2)
		if fail {
			partial := workerplugin.GetStreamResult()
			partial.Kind = workerplugin.StreamResultKindStream
			partial.Data = []byte(`"partial"`)
			ch <- partial
			errResult := workerplugin.GetStreamResult()
			errResult.Kind = workerplugin.StreamResultKindError
			errResult.Error = unavailableErr()
			ch <- errResult
		} else {
			// The restart worker would REPLAY the stream to a successful final if the
			// pool re-issued it. Under suppression it must never be invoked.
			fin := workerplugin.GetStreamResult()
			fin.Kind = workerplugin.StreamResultKindFinal
			fin.Data = []byte(`"replayed"`)
			ch <- fin
		}
		close(ch)
		return ch, nil
	}
	return w
}

// TestCallStreamNativeCohortSuppressesReplay proves the native-cohort suppression:
// a mid-stream retryable worker error produces EXACTLY ONE CallStream invocation (no
// replay on the restart worker), NO reset marker, and a terminal error to the client
// — while worker health is still marked and a replacement is still dispatched.
func TestCallStreamNativeCohortSuppressesReplay(t *testing.T) {
	var callStreamInvocations, factoryCalls atomic.Int32
	var firstHandle *workerHandle

	factory := func(id int) (*workerHandle, error) {
		n := factoryCalls.Add(1)
		// First worker (initial slot) fails mid-stream; the restart worker would
		// succeed if replayed, so any second CallStream is a forbidden replay.
		w := makeMidStreamFailWorker(&callStreamInvocations, n == 1)
		h := newMockHandle(id, w)
		if n == 1 {
			firstHandle = h
		}
		return h, nil
	}

	p := newTestPool(t, 1, factory)
	// Arm the native-stream cohort suppression (MaxRetries stays 2 from newTestPool;
	// suppression caps stream attempts to exactly one).
	p.config.DisableStreamInfrastructureRetries = true
	defer p.Close()

	results, err := p.CallStream(context.Background(), "Test", []byte(`{}`), bamlutils.StreamModeStream)
	if err != nil {
		t.Fatalf("CallStream setup failed: %v", err)
	}

	var resets, finals, errs atomic.Int32
	requireCompleteWithin(t, 2*time.Second, func() {
		for r := range results {
			if r.Reset {
				resets.Add(1)
			}
			switch r.Kind {
			case workerplugin.StreamResultKindFinal:
				finals.Add(1)
			case workerplugin.StreamResultKindError:
				errs.Add(1)
			}
			workerplugin.ReleaseStreamResult(r)
		}
	})

	// CRUX: exactly one physical worker stream attempt — no pool replay after claim.
	if got := callStreamInvocations.Load(); got != 1 {
		t.Errorf("POOL REPLAY VIOLATED: %d CallStream invocations (want exactly 1 — no native-stream replay after claim)", got)
	}
	if got := resets.Load(); got != 0 {
		t.Errorf("POOL REPLAY VIOLATED: %d reset markers injected in the native cohort (want 0)", got)
	}
	if got := finals.Load(); got != 0 {
		t.Errorf("suppressed stream must not produce a replayed final, got %d", got)
	}
	if got := errs.Load(); got != 1 {
		t.Errorf("suppressed mid-stream failure must surface a single terminal error, got %d", got)
	}

	// Worker health is STILL marked and a replacement is STILL dispatched (only the
	// replay is suppressed).
	if firstHandle.healthy.Load() {
		t.Errorf("the failed worker must be marked unhealthy even with retries suppressed")
	}
	deadline := time.After(2 * time.Second)
	for factoryCalls.Load() < 2 {
		select {
		case <-deadline:
			t.Fatalf("replacement worker was never dispatched (factory calls=%d, want >=2)", factoryCalls.Load())
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// TestCallStreamNativeCohortUnaryStillReplays proves the suppression is SCOPED to
// real public stream modes (P1-b): with DisableStreamInfrastructureRetries=true (as
// a native-worker/flag-on process sets it cohort-wide) a UNARY /call bridge
// (StreamModeCall, via Pool.Call → CallStream) is NEVER native-eligible and MUST
// keep its ordinary worker-error retry — so the SAME mid-stream failure still
// replays on the restart worker to a successful final.
func TestCallStreamNativeCohortUnaryStillReplays(t *testing.T) {
	var callStreamInvocations, factoryCalls atomic.Int32
	factory := func(id int) (*workerHandle, error) {
		n := factoryCalls.Add(1)
		return newMockHandle(id, makeMidStreamFailWorker(&callStreamInvocations, n == 1)), nil
	}

	p := newTestPool(t, 1, factory)
	// The cohort-wide switch is ON, exactly as a native-worker/flag-on process sets
	// it — but this is a UNARY call, which must be unaffected.
	p.config.DisableStreamInfrastructureRetries = true
	defer p.Close()

	results, err := p.Call(context.Background(), "Test", []byte(`{}`), bamlutils.StreamModeCall)
	if err != nil {
		t.Fatalf("Pool.Call(StreamModeCall) failed: %v", err)
	}
	// Pool.Call already drains CallStream to the final; the returned result is the
	// replayed success. What we assert is that the replay HAPPENED.
	if results == nil {
		t.Fatalf("Pool.Call returned nil result")
	}

	if got := callStreamInvocations.Load(); got != 2 {
		t.Errorf("UNARY RETRY REGRESSION: /call (StreamModeCall) made %d CallStream invocations under the native cohort switch, want 2 (retries must be preserved for unary)", got)
	}
	if string(results.Data) != `"replayed"` {
		t.Errorf("unary /call should replay to the restart worker's final, got %s", results.Data)
	}
}

// TestCallStreamDefaultCohortStillReplays is the contrast baseline: with suppression
// OFF (the default), the SAME mid-stream failure replays on the restart worker (a
// second CallStream) and injects a reset — proving the suppression flag is the sole
// gate and default behaviour is unchanged.
func TestCallStreamDefaultCohortStillReplays(t *testing.T) {
	var callStreamInvocations, factoryCalls atomic.Int32
	factory := func(id int) (*workerHandle, error) {
		n := factoryCalls.Add(1)
		return newMockHandle(id, makeMidStreamFailWorker(&callStreamInvocations, n == 1)), nil
	}

	p := newTestPool(t, 1, factory)
	// Default: DisableStreamInfrastructureRetries is false (unset).
	defer p.Close()

	results, err := p.CallStream(context.Background(), "Test", []byte(`{}`), bamlutils.StreamModeStream)
	if err != nil {
		t.Fatalf("CallStream setup failed: %v", err)
	}

	var resets, finals atomic.Int32
	requireCompleteWithin(t, 2*time.Second, func() {
		for r := range results {
			if r.Reset {
				resets.Add(1)
			}
			if r.Kind == workerplugin.StreamResultKindFinal {
				finals.Add(1)
			}
			workerplugin.ReleaseStreamResult(r)
		}
	})

	if got := callStreamInvocations.Load(); got != 2 {
		t.Errorf("default cohort should REPLAY (2 CallStream invocations), got %d", got)
	}
	if got := resets.Load(); got != 1 {
		t.Errorf("default cohort should inject exactly 1 reset on replay, got %d", got)
	}
	if got := finals.Load(); got != 1 {
		t.Errorf("default cohort should replay to a successful final, got %d", got)
	}
}
