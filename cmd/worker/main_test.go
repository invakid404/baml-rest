package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/workerplugin"
)

type fakeStreamResult struct {
	kind     bamlutils.StreamResultKind
	stream   any
	final    any
	err      error
	raw      string
	reset    bool
	metadata *bamlutils.Metadata
	release  sync.Once
	released chan struct{}
}

func newFakeStreamResult(kind bamlutils.StreamResultKind) *fakeStreamResult {
	return &fakeStreamResult{
		kind:     kind,
		released: make(chan struct{}),
	}
}

func (r *fakeStreamResult) Kind() bamlutils.StreamResultKind { return r.kind }
func (r *fakeStreamResult) Stream() any                      { return r.stream }
func (r *fakeStreamResult) Final() any                       { return r.final }
func (r *fakeStreamResult) Error() error                     { return r.err }
func (r *fakeStreamResult) Raw() string                      { return r.raw }
func (r *fakeStreamResult) Reset() bool                      { return r.reset }
func (r *fakeStreamResult) Metadata() *bamlutils.Metadata    { return r.metadata }
func (r *fakeStreamResult) Release() {
	r.release.Do(func() {
		close(r.released)
	})
}

func TestBridgeStreamResultsCancelsWhileUpstreamBlocked(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	in := make(chan bamlutils.StreamResult)
	out := bridgeStreamResults(ctx, in, nil)

	cancel()

	select {
	case _, ok := <-out:
		if ok {
			t.Fatal("expected bridged output channel to close after cancellation")
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("timed out waiting for bridge goroutine to exit after cancellation")
	}

	// Close the input channel so the drain goroutine can exit.
	// drainStreamResults waits until channel close (no timeout),
	// so leaving it open would leak the goroutine.
	close(in)
}

func TestBridgeStreamResultsReleasesBufferedResultsOnCancel(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	in := make(chan bamlutils.StreamResult, 2)
	first := newFakeStreamResult(bamlutils.StreamResultKindStream)
	second := newFakeStreamResult(bamlutils.StreamResultKindFinal)
	in <- first
	in <- second
	close(in)

	out := bridgeStreamResults(ctx, in, nil)

	select {
	case <-first.released:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("expected first buffered result to be released")
	}

	select {
	case <-second.released:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("expected second buffered result to be released")
	}

	select {
	case _, ok := <-out:
		if ok {
			t.Fatal("expected bridged output channel to close after cancellation")
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("timed out waiting for bridge goroutine to exit after cancellation")
	}
}

func TestBridgeStreamResultsReleasesPostCancelResultsUntilClosed(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	in := make(chan bamlutils.StreamResult)
	out := bridgeStreamResults(ctx, in, nil)

	cancel()

	select {
	case _, ok := <-out:
		if ok {
			t.Fatal("expected bridged output channel to close after cancellation")
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("timed out waiting for bridge goroutine to exit after cancellation")
	}

	late := newFakeStreamResult(bamlutils.StreamResultKindStream)
	sent := make(chan struct{})
	go func() {
		defer close(sent)
		in <- late
		close(in)
	}()

	select {
	case <-sent:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("expected post-cancel sender to be drained")
	}

	select {
	case <-late.released:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("expected post-cancel result to be released")
	}
}

func TestDrainStreamResultsReleasesAndReturnsOnClose(t *testing.T) {
	t.Parallel()

	in := make(chan bamlutils.StreamResult, 1)
	fake := newFakeStreamResult(bamlutils.StreamResultKindStream)
	in <- fake
	close(in)

	done := make(chan struct{})
	go func() {
		drainStreamResults(in, nil)
		close(done)
	}()

	select {
	case <-fake.released:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("expected queued result to be released")
	}

	select {
	case <-done:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("expected drainStreamResults to return after channel close")
	}
}

func TestDrainStreamResultsReleasesLateResult(t *testing.T) {
	t.Parallel()

	in := make(chan bamlutils.StreamResult)
	done := make(chan struct{})
	go func() {
		drainStreamResults(in, nil)
		close(done)
	}()

	// Simulate a producer that takes 200ms to emit after cancellation.
	// The drain has no timeout — it waits until the channel is closed,
	// so it always catches late results regardless of delay.
	late := newFakeStreamResult(bamlutils.StreamResultKindStream)
	time.AfterFunc(200*time.Millisecond, func() {
		in <- late
		close(in)
	})

	select {
	case <-late.released:
	case <-time.After(2 * time.Second):
		t.Fatal("expected late result to be released by drain")
	}

	select {
	case <-done:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("expected drainStreamResults to return after channel close")
	}
}

func TestBridgeStreamResultsForwardsFinalResult(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	in := make(chan bamlutils.StreamResult, 1)
	fake := newFakeStreamResult(bamlutils.StreamResultKindFinal)
	fake.final = map[string]string{"message": "done"}
	fake.raw = "raw-output"
	fake.reset = true
	in <- fake
	close(in)

	out := bridgeStreamResults(ctx, in, nil)

	select {
	case <-fake.released:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("expected upstream result to be released")
	}

	select {
	case got, ok := <-out:
		if !ok {
			t.Fatal("expected bridged result")
		}
		defer workerplugin.ReleaseStreamResult(got)
		if got.Kind != workerplugin.StreamResultKindFinal {
			t.Fatalf("expected final result kind, got %v", got.Kind)
		}
		if string(got.Data) != `{"message":"done"}` {
			t.Fatalf("unexpected bridged payload: %s", got.Data)
		}
		if got.Raw != "raw-output" {
			t.Fatalf("unexpected raw output: %q", got.Raw)
		}
		if !got.Reset {
			t.Fatal("expected reset flag to propagate")
		}
		if got.Error != nil {
			t.Fatalf("unexpected bridged error: %v", got.Error)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("timed out waiting for bridged result")
	}

	select {
	case _, ok := <-out:
		if ok {
			t.Fatal("expected bridged output channel to close after upstream closes")
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("timed out waiting for bridged output channel to close")
	}
}

func TestBridgeStreamResultsForwardsStreamResult(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	in := make(chan bamlutils.StreamResult, 1)
	fake := newFakeStreamResult(bamlutils.StreamResultKindStream)
	fake.stream = map[string]string{"delta": "hi"}
	fake.raw = "partial-raw"
	fake.reset = true
	in <- fake
	close(in)

	out := bridgeStreamResults(ctx, in, nil)

	select {
	case got, ok := <-out:
		if !ok {
			t.Fatal("expected bridged stream result")
		}
		defer workerplugin.ReleaseStreamResult(got)
		if got.Kind != workerplugin.StreamResultKindStream {
			t.Fatalf("expected stream result kind, got %v", got.Kind)
		}
		if string(got.Data) != `{"delta":"hi"}` {
			t.Fatalf("unexpected bridged payload: %s", got.Data)
		}
		if got.Raw != "partial-raw" {
			t.Fatalf("unexpected raw output: %q", got.Raw)
		}
		if !got.Reset {
			t.Fatal("expected reset flag to propagate")
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("timed out waiting for bridged stream result")
	}

	select {
	case _, ok := <-out:
		if ok {
			t.Fatal("expected bridged output channel to close after upstream closes (no leakage past the single forwarded frame)")
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("timed out waiting for bridged output channel to close")
	}
}

func TestBridgeStreamResultsResetOnlyStreamHasNoPayload(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	in := make(chan bamlutils.StreamResult, 1)
	fake := newFakeStreamResult(bamlutils.StreamResultKindStream)
	fake.reset = true
	in <- fake
	close(in)

	out := bridgeStreamResults(ctx, in, nil)

	select {
	case got, ok := <-out:
		if !ok {
			t.Fatal("expected bridged reset-only stream result")
		}
		defer workerplugin.ReleaseStreamResult(got)
		if got.Kind != workerplugin.StreamResultKindStream {
			t.Fatalf("expected stream result kind, got %v", got.Kind)
		}
		if !got.Reset {
			t.Fatal("expected reset flag to propagate")
		}
		if len(got.Data) != 0 {
			t.Fatalf("expected no payload for reset-only stream result, got %q", string(got.Data))
		}
		if got.Raw != "" {
			t.Fatalf("expected empty raw for reset-only stream result, got %q", got.Raw)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("timed out waiting for bridged reset-only stream result")
	}

	select {
	case _, ok := <-out:
		if ok {
			t.Fatal("expected bridged output channel to close after upstream closes (no leakage past the reset-only frame)")
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("timed out waiting for bridged output channel to close")
	}
}

func TestBridgeStreamResultsForwardsHeartbeat(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	in := make(chan bamlutils.StreamResult, 1)
	fake := newFakeStreamResult(bamlutils.StreamResultKindHeartbeat)
	in <- fake
	close(in)

	out := bridgeStreamResults(ctx, in, nil)

	select {
	case got, ok := <-out:
		if !ok {
			t.Fatal("expected bridged heartbeat result")
		}
		defer workerplugin.ReleaseStreamResult(got)
		if got.Kind != workerplugin.StreamResultKindHeartbeat {
			t.Fatalf("expected heartbeat result kind, got %v", got.Kind)
		}
		if len(got.Data) != 0 {
			t.Fatalf("expected no heartbeat payload, got %q", string(got.Data))
		}
		if got.Error != nil {
			t.Fatalf("unexpected heartbeat error: %v", got.Error)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("timed out waiting for bridged heartbeat result")
	}
}

func TestBridgeStreamResultsForwardsMetadata(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	max := 3
	dur := int64(42)
	md := &bamlutils.Metadata{
		Phase:         bamlutils.MetadataPhaseOutcome,
		Attempt:       1,
		Path:          "buildrequest",
		Client:        "MyClient",
		WinnerPath:    "buildrequest",
		RetryMax:      &max,
		UpstreamDurMs: &dur,
	}

	in := make(chan bamlutils.StreamResult, 1)
	fake := newFakeStreamResult(bamlutils.StreamResultKindMetadata)
	fake.metadata = md
	in <- fake
	close(in)

	out := bridgeStreamResults(ctx, in, nil)

	select {
	case got, ok := <-out:
		if !ok {
			t.Fatal("expected bridged metadata result")
		}
		defer workerplugin.ReleaseStreamResult(got)
		if got.Kind != workerplugin.StreamResultKindMetadata {
			t.Fatalf("expected metadata result kind, got %v", got.Kind)
		}
		if len(got.Data) == 0 {
			t.Fatal("expected metadata payload bytes")
		}
		// JSON shape check: must contain the phase + winner_path so the
		// pool's downstream consumers can parse it.
		payload := string(got.Data)
		if !strings.Contains(payload, `"phase":"outcome"`) {
			t.Errorf("payload missing phase=outcome; got %s", payload)
		}
		if !strings.Contains(payload, `"winner_path":"buildrequest"`) {
			t.Errorf("payload missing winner_path; got %s", payload)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("timed out waiting for bridged metadata result")
	}
}

func TestBridgeStreamResultsErrorsOnNilMetadataPayload(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	in := make(chan bamlutils.StreamResult, 1)
	// Metadata kind with no payload — this is a producer bug; the bridge
	// must surface it as an error rather than silently forwarding empty
	// bytes that would mis-decode at the consumer.
	fake := newFakeStreamResult(bamlutils.StreamResultKindMetadata)
	in <- fake
	close(in)

	out := bridgeStreamResults(ctx, in, nil)

	select {
	case got, ok := <-out:
		if !ok {
			t.Fatal("expected bridged result for metadata with nil payload")
		}
		defer workerplugin.ReleaseStreamResult(got)
		if got.Kind != workerplugin.StreamResultKindError {
			t.Fatalf("expected error kind for nil-metadata payload, got %v", got.Kind)
		}
		if got.Error == nil {
			t.Fatal("expected non-nil error for nil-metadata payload")
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("timed out waiting for bridged result")
	}
}

func TestBridgeStreamResultsPropagatesErrors(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	in := make(chan bamlutils.StreamResult, 1)
	fake := newFakeStreamResult(bamlutils.StreamResultKindError)
	fake.err = errors.New("boom")
	in <- fake
	close(in)

	out := bridgeStreamResults(ctx, in, nil)

	select {
	case got, ok := <-out:
		if !ok {
			t.Fatal("expected bridged error result")
		}
		defer workerplugin.ReleaseStreamResult(got)
		if got.Kind != workerplugin.StreamResultKindError {
			t.Fatalf("expected error result kind, got %v", got.Kind)
		}
		if got.Error == nil || got.Error.Error() != "boom" {
			t.Fatalf("unexpected bridged error: %v", got.Error)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("timed out waiting for bridged error result")
	}
}

func TestBridgeStreamResultsCancelsDuringDownstreamSend(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	in := make(chan bamlutils.StreamResult, 2)
	fake := newFakeStreamResult(bamlutils.StreamResultKindStream)
	fake.stream = map[string]string{"delta": "hi"}
	queued := newFakeStreamResult(bamlutils.StreamResultKindHeartbeat)
	in <- fake
	in <- queued
	close(in)

	out := bridgeStreamResults(ctx, in, nil)

	select {
	case <-fake.released:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("expected upstream result to be released before downstream send")
	}

	cancel()

	// The bridge may deliver one in-flight result before seeing
	// the cancel (send and ctx.Done() race in the select). Drain
	// until the channel closes.
	drainDone := make(chan struct{})
	go func() {
		for range out {
		}
		close(drainDone)
	}()

	select {
	case <-drainDone:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("timed out waiting for bridge goroutine to exit after cancellation")
	}

	select {
	case <-queued.released:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("expected queued buffered result to be released after cancellation")
	}
}

// ---------------------------------------------------------------------------
// Tests: drain leak detection
// ---------------------------------------------------------------------------

func TestDrainStreamResultsTracksActiveGoroutines(t *testing.T) {
	// Not parallel: asserts on the process-global activeDrainGoroutines
	// counter. Running concurrently with other tests that spawn drain
	// goroutines causes nondeterministic failures.

	before := ActiveDrainGoroutines()

	in := make(chan bamlutils.StreamResult)
	done := make(chan struct{})
	go func() {
		drainStreamResults(in, nil)
		close(done)
	}()

	// Wait for the drain goroutine to register itself.
	deadline := time.After(time.Second)
	for {
		delta := ActiveDrainGoroutines() - before
		if delta >= 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("drain goroutine did not increment activeDrainGoroutines")
		default:
			time.Sleep(time.Millisecond)
		}
	}

	// Close the channel — drain should exit and decrement the counter.
	close(in)

	select {
	case <-done:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("drain goroutine did not exit after channel close")
	}

	after := ActiveDrainGoroutines()
	if after != before {
		t.Fatalf("activeDrainGoroutines = %d after drain completed, want %d", after, before)
	}
}

type testLogger struct {
	warnings []string
	mu       sync.Mutex
}

func (l *testLogger) Debug(string, ...interface{}) {}
func (l *testLogger) Info(string, ...interface{})  {}
func (l *testLogger) Error(string, ...interface{}) {}
func (l *testLogger) Warn(msg string, args ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.warnings = append(l.warnings, msg)
}
func (l *testLogger) getWarnings() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	cp := make([]string, len(l.warnings))
	copy(cp, l.warnings)
	return cp
}

func TestDrainStreamResultsLogsLeakWarning(t *testing.T) {
	// Not parallel: modifies package-level threshold.
	origThreshold := getDrainLeakThreshold()
	setDrainLeakThreshold(50 * time.Millisecond)
	defer setDrainLeakThreshold(origThreshold)

	logger := &testLogger{}
	in := make(chan bamlutils.StreamResult)
	done := make(chan struct{})
	go func() {
		drainStreamResults(in, logger)
		close(done)
	}()

	// Wait for the warning to fire.
	deadline := time.After(2 * time.Second)
	for {
		warnings := logger.getWarnings()
		if len(warnings) > 0 {
			found := false
			for _, w := range warnings {
				if strings.Contains(w, "drain goroutine still waiting") {
					found = true
					break
				}
			}
			if found {
				break
			}
		}
		select {
		case <-deadline:
			t.Fatal("expected leak warning to be logged after threshold")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	// Clean up: close the channel so the drain goroutine exits.
	close(in)

	select {
	case <-done:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("drain goroutine did not exit after channel close")
	}
}

func TestDrainStreamResultsNoWarningOnFastClose(t *testing.T) {
	// Not parallel: modifies package-level threshold.
	origThreshold := getDrainLeakThreshold()
	setDrainLeakThreshold(500 * time.Millisecond)
	defer setDrainLeakThreshold(origThreshold)

	logger := &testLogger{}
	in := make(chan bamlutils.StreamResult, 1)
	fake := newFakeStreamResult(bamlutils.StreamResultKindStream)
	in <- fake
	close(in)

	done := make(chan struct{})
	go func() {
		drainStreamResults(in, logger)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("drain goroutine did not exit after channel close")
	}

	// Give the warning goroutine time to clean up.
	time.Sleep(50 * time.Millisecond)

	warnings := logger.getWarnings()
	if len(warnings) > 0 {
		t.Fatalf("expected no warnings for fast drain, got: %v", warnings)
	}
}
