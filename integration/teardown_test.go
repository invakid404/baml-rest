//go:build integration

package integration

import (
	"context"
	"errors"
	"testing"
	"time"
)

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

	start := time.Now()
	err, timedOut := boundedTerminate(f, 150*time.Millisecond)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("boundedTerminate err = nil, want a deadline error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("boundedTerminate err = %v, want context.DeadlineExceeded", err)
	}
	if !timedOut {
		t.Fatal("boundedTerminate timedOut = false, want true on a blocked teardown")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("boundedTerminate blocked %s past the 150ms budget — context not honored", elapsed)
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
