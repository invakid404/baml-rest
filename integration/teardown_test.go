//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// maxBoundedTerminateElapsed bounds how long boundedTerminate may take on
// a 150ms budget. Tight enough to catch a regression where the deadline
// is not honored (which would take ~the full Terminate time), but loose
// enough to tolerate goroutine-scheduling jitter on a loaded CI runner.
// The separate 5s time.After guard in each test stays as the anti-hang
// escape; this is the real assertion.
const maxBoundedTerminateElapsed = 2 * time.Second

// fakeTerminator is an envTerminator whose Terminate blocks until its
// context is cancelled, modelling a wedged container that won't stop —
// the #420 teardown shape.
type fakeTerminator struct {
	started chan struct{}
}

func (f *fakeTerminator) Terminate(ctx context.Context) error {
	if f.started != nil {
		close(f.started)
	}
	<-ctx.Done()
	return ctx.Err()
}

// TestBoundedTerminateHonorsDeadline pins the #420 teardown fix: a
// teardown that would otherwise block forever must return promptly once
// the budget elapses, flagged as timed-out, rather than stalling to the
// job cap.
func TestBoundedTerminateHonorsDeadline(t *testing.T) {
	f := &fakeTerminator{started: make(chan struct{})}

	type result struct {
		err      error
		timedOut bool
		elapsed  time.Duration
	}
	done := make(chan result, 1)
	go func() {
		start := time.Now()
		err, timedOut := boundedTerminate(f, 150*time.Millisecond)
		done <- result{err, timedOut, time.Since(start)}
	}()

	// Independent guard: if a regression makes boundedTerminate stop
	// passing a deadline ctx, the call blocks on the fakeTerminator
	// forever — fail fast here instead of hanging to the suite timeout.
	var r result
	select {
	case r = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("boundedTerminate did not return within 5s — deadline not propagated")
	}

	if r.err == nil {
		t.Fatal("boundedTerminate err = nil, want a deadline error")
	}
	if !errors.Is(r.err, context.DeadlineExceeded) {
		t.Fatalf("boundedTerminate err = %v, want context.DeadlineExceeded", r.err)
	}
	if !r.timedOut {
		t.Fatal("boundedTerminate timedOut = false, want true on a blocked teardown")
	}
	if r.elapsed > maxBoundedTerminateElapsed {
		t.Fatalf("boundedTerminate blocked %s past the 150ms budget — context not honored", r.elapsed)
	}
	select {
	case <-f.started:
	default:
		t.Fatal("Terminate was never invoked")
	}
}

// TestBoundedTerminateZeroBudgetIsUnbounded verifies the explicit opt-out
// (budget 0 → context.Background()): a teardown that completes is not
// reported as timed out.
func TestBoundedTerminateZeroBudgetIsUnbounded(t *testing.T) {
	completed := completingTerminator{}
	err, timedOut := boundedTerminate(completed, 0)
	if err != nil {
		t.Fatalf("boundedTerminate err = %v, want nil", err)
	}
	if timedOut {
		t.Fatal("boundedTerminate timedOut = true, want false for a clean unbounded teardown")
	}
}

// completingTerminator is an envTerminator that returns immediately.
type completingTerminator struct{}

func (completingTerminator) Terminate(context.Context) error { return nil }

// ctxIgnoringTerminator models the worst case the #420 fix must survive:
// a Terminate that does NOT honor the context (a Docker stop/remove that
// can't be aborted by ctx cancellation) and blocks until released. It
// proves boundedTerminate enforces its budget independently of the
// terminator's ctx handling.
type ctxIgnoringTerminator struct {
	release chan struct{}
}

func (c *ctxIgnoringTerminator) Terminate(context.Context) error {
	<-c.release
	return nil
}

// TestBoundedTerminateBoundsContextIgnoringTerminate is the core
// regression guard CodeRabbit asked for: even when Terminate ignores the
// context entirely, boundedTerminate must still return timedOut=true
// around the budget rather than blocking on it.
func TestBoundedTerminateBoundsContextIgnoringTerminate(t *testing.T) {
	c := &ctxIgnoringTerminator{release: make(chan struct{})}
	// Release the blocked Terminate goroutine on the way out so it doesn't
	// linger past the test.
	defer close(c.release)

	type result struct {
		err      error
		timedOut bool
		elapsed  time.Duration
	}
	done := make(chan result, 1)
	go func() {
		start := time.Now()
		err, timedOut := boundedTerminate(c, 150*time.Millisecond)
		done <- result{err, timedOut, time.Since(start)}
	}()

	var r result
	select {
	case r = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("boundedTerminate did not return within 5s — budget not enforced independently of Terminate")
	}

	if !r.timedOut {
		t.Fatal("boundedTerminate timedOut = false, want true for a context-ignoring teardown")
	}
	if !errors.Is(r.err, context.DeadlineExceeded) {
		t.Fatalf("boundedTerminate err = %v, want context.DeadlineExceeded", r.err)
	}
	if r.elapsed > maxBoundedTerminateElapsed {
		t.Fatalf("boundedTerminate blocked %s past the 150ms budget", r.elapsed)
	}
}

// flattenedTimeoutTerminator blocks until its context is cancelled, then
// returns an aggregate error flattened with %v (not %w) — mirroring
// TestEnvironment.Terminate, whose `fmt.Errorf("terminate errors: %v",
// errs)` strips any context.DeadlineExceeded wrapping. boundedTerminate
// must classify a budget overrun as a timeout regardless of the error's
// shape, so the #420 diagnostics dump fires.
type flattenedTimeoutTerminator struct{}

func (flattenedTimeoutTerminator) Terminate(ctx context.Context) error {
	<-ctx.Done()
	// Flatten with %v so the result does NOT errors.Is-match
	// context.DeadlineExceeded — the exact shape that made the old
	// error-based classifier report timedOut=false.
	return fmt.Errorf("terminate errors: %v", ctx.Err())
}

// TestBoundedTerminateTimeoutWithFlattenedError pins the #420 diagnostics
// bug: a budget overrun whose error does not wrap context.DeadlineExceeded
// must still yield timedOut=true (classified on the bounded ctx, not the
// error value), or the caller would skip the goroutine/native stack dump.
func TestBoundedTerminateTimeoutWithFlattenedError(t *testing.T) {
	type result struct {
		err      error
		timedOut bool
		elapsed  time.Duration
	}
	done := make(chan result, 1)
	go func() {
		start := time.Now()
		err, timedOut := boundedTerminate(flattenedTimeoutTerminator{}, 150*time.Millisecond)
		done <- result{err, timedOut, time.Since(start)}
	}()

	var r result
	select {
	case r = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("boundedTerminate did not return within 5s")
	}

	if r.err == nil {
		t.Fatal("boundedTerminate err = nil, want a non-nil teardown error")
	}
	if !r.timedOut {
		t.Fatal("boundedTerminate timedOut = false on a budget overrun with a flattened (non-wrapping) error — #420 dump would be skipped")
	}
	if r.elapsed > maxBoundedTerminateElapsed {
		t.Fatalf("boundedTerminate blocked %s past the 150ms budget", r.elapsed)
	}
}

// TestClassifyTeardownResult deterministically pins the teardown
// classification contract — independent of select timing — and in
// particular the #420 diagnostics bug class: a budget overrun whose error
// does NOT wrap context.DeadlineExceeded (TestEnvironment.Terminate
// flattens with %v) must still classify timedOut=true so the caller's
// stack-dump path runs. That case returns timedOut=false under the old
// errors.Is logic, so it genuinely distinguishes fixed from buggy.
func TestClassifyTeardownResult(t *testing.T) {
	// %v, so this does NOT wrap context.DeadlineExceeded.
	flattened := fmt.Errorf("terminate errors: %v", context.DeadlineExceeded)
	if errors.Is(flattened, context.DeadlineExceeded) {
		t.Fatal("test precondition: flattened error must NOT errors.Is-match DeadlineExceeded")
	}
	otherErr := errors.New("docker daemon refused")

	cases := []struct {
		name         string
		err          error
		ctxErr       error
		wantErr      error
		wantTimedOut bool
	}{
		{"success, ctx alive", nil, nil, nil, false},
		{"success, ctx expired (boundary win)", nil, context.DeadlineExceeded, nil, false},
		{"flattened error after deadline", flattened, context.DeadlineExceeded, flattened, true},
		{"non-deadline error before deadline", otherErr, nil, otherErr, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotErr, gotTimedOut := classifyTeardownResult(tc.err, tc.ctxErr)
			if gotErr != tc.wantErr {
				t.Errorf("err = %v, want %v", gotErr, tc.wantErr)
			}
			if gotTimedOut != tc.wantTimedOut {
				t.Errorf("timedOut = %v, want %v", gotTimedOut, tc.wantTimedOut)
			}
		})
	}
}

// erroringTerminator returns a fixed non-nil, non-deadline error
// immediately, ignoring the context.
type erroringTerminator struct{ err error }

func (e erroringTerminator) Terminate(context.Context) error { return e.err }

// TestBoundedTerminateSuccessIsNotTimeout guards the (nil, true) contract
// hole: a teardown that returns nil must yield (nil, false), never a
// phantom timedOut. (The true photo-finish — errCh delivering nil at the
// exact instant ctx expires — is inherently racy and cannot be forced
// deterministically; this locks the success contract on the errCh arm,
// which is where the old `ctx.Err() != nil` could have leaked a phantom
// timeout.) A benign successful teardown must never look like #420.
func TestBoundedTerminateSuccessIsNotTimeout(t *testing.T) {
	err, timedOut := boundedTerminate(completingTerminator{}, 5*time.Second)
	if err != nil {
		t.Fatalf("boundedTerminate err = %v, want nil for a successful teardown", err)
	}
	if timedOut {
		t.Fatal("boundedTerminate timedOut = true, want false for a successful teardown")
	}
}

// TestBoundedTerminateNonDeadlineErrorNotTimeout: a non-deadline teardown
// error surfaces as (err, false) — timedOut is reserved for genuine
// deadline overruns, while the caller's err != nil gate still catches the
// failure and fails CI.
func TestBoundedTerminateNonDeadlineErrorNotTimeout(t *testing.T) {
	boom := errors.New("docker daemon refused")
	err, timedOut := boundedTerminate(erroringTerminator{err: boom}, 5*time.Second)
	if !errors.Is(err, boom) {
		t.Fatalf("boundedTerminate err = %v, want %v", err, boom)
	}
	if timedOut {
		t.Fatal("boundedTerminate timedOut = true, want false for a non-deadline error")
	}
}

// TestExitCodeAfterTeardown pins the invariant that a bounded-teardown
// error fails the suite even when the tests themselves passed (the #420
// wedged-teardown hang must not exit green), while never masking a real
// test failure.
func TestExitCodeAfterTeardown(t *testing.T) {
	teardownErr := errors.New("teardown timed out")
	cases := []struct {
		name        string
		code        int
		teardownErr error
		want        int
	}{
		{"green run, clean teardown", 0, nil, 0},
		{"green run, teardown error", 0, teardownErr, 1},
		{"failed run, clean teardown", 2, nil, 2},
		{"failed run, teardown error keeps test code", 2, teardownErr, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := exitCodeAfterTeardown(tc.code, tc.teardownErr); got != tc.want {
				t.Fatalf("exitCodeAfterTeardown(%d, %v) = %d, want %d", tc.code, tc.teardownErr, got, tc.want)
			}
		})
	}
}
