//go:build integration

package testutil

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// fastBackoff is a backoff slice the length of the production setupRetryBackoff
// (3 retries -> 4 total attempts) but with negligible delays, so the
// inter-attempt sleep can never dominate or flake these timing tests. The
// retry-vs-bail behavior under test is event-driven (it keys off context
// cancellation, not wall-clock sleeps), so 1ms is plenty.
var fastBackoff = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}

// TestSetupWithRetry_PerAttemptTimeoutDoesNotPoisonParent is the #424
// regression: an attempt that exhausts its per-attempt budget must NOT poison
// the parent into the "context done before retrying" bail — a subsequent
// attempt must still run while the parent has budget left.
//
// Non-flaky by construction: attempt 1 consumes EXACTLY its per-attempt budget
// by blocking on the attempt ctx (no fixed sleep), and the parent (1s) is far
// larger than perAttempt (20ms) + backoff so the parent is never the limiter.
func TestSetupWithRetry_PerAttemptTimeoutDoesNotPoisonParent(t *testing.T) {
	parent, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	const perAttempt = 20 * time.Millisecond

	var calls int
	want := &TestEnvironment{}
	attempt := func(ctx context.Context) (*TestEnvironment, error) {
		calls++
		if calls == 1 {
			// Consume exactly the per-attempt budget, event-driven: block
			// until this attempt's ctx fires, then surface its deadline error.
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return want, nil
	}

	got, err := setupWithRetry(parent, perAttempt, fastBackoff, attempt)
	if err != nil {
		t.Fatalf("setupWithRetry returned error, want success after retry: %v", err)
	}
	if got != want {
		t.Fatalf("setupWithRetry returned %p, want the attempt-2 env %p", got, want)
	}
	if calls != 2 {
		t.Fatalf("attempt ran %d time(s), want exactly 2 (attempt 2 must run after attempt 1's per-attempt timeout)", calls)
	}
	if parent.Err() != nil {
		t.Fatalf("parent ctx was consumed (%v); a per-attempt timeout must not poison the parent", parent.Err())
	}
}

// TestSetupWithRetry_ParentExhaustionBails is the negative counterpart: when
// every attempt exhausts its per-attempt budget and the parent is too small to
// fund another, the loop must bail via the parent ctx.Err() path (authoritative
// parent cap) rather than looping forever.
//
// Non-flaky by construction: parent (25ms) is just over one perAttempt (20ms),
// so attempt 1 gets its own 20ms timer (parent alive afterwards -> retry) and
// attempt 2's requested deadline lands AFTER the parent deadline, so Go ties it
// directly to the parent (no independent timer) — its cancellation therefore
// guarantees parent.Err() is set when we check. Under scheduler jitter the only
// drift is bailing one attempt earlier, which the assertions still accept.
func TestSetupWithRetry_ParentExhaustionBails(t *testing.T) {
	parent, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()

	const perAttempt = 20 * time.Millisecond

	var calls int
	attempt := func(ctx context.Context) (*TestEnvironment, error) {
		calls++
		<-ctx.Done() // every attempt exhausts its (parent-capped) budget
		return nil, ctx.Err()
	}

	env, err := setupWithRetry(parent, perAttempt, fastBackoff, attempt)
	if env != nil {
		t.Fatalf("setupWithRetry returned env %p, want nil on parent exhaustion", env)
	}
	if err == nil {
		t.Fatal("setupWithRetry returned nil error, want a parent-exhaustion failure")
	}
	// setupWithRetry has TWO parent-deadline bail branches — post-attempt
	// ("context done before retrying") and during the backoff wait ("while
	// waiting to retry") — and under the tiny budgets here scheduler timing
	// decides which wins. Both wrap the parent ctx.Err(), so assert the
	// parent-deadline SHAPE (the robust invariant) rather than a single
	// message; that IS the test's contract: parent exhaustion bails with a
	// parent-deadline error.
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		t.Fatalf("error %v does not wrap a parent-deadline error (DeadlineExceeded/Canceled)", err)
	}
	if !strings.Contains(err.Error(), "context done before retrying") &&
		!strings.Contains(err.Error(), "while waiting to retry") {
		t.Fatalf("error %q is not a parent-exhaustion bail", err)
	}
	if maxAttempts := len(fastBackoff) + 1; calls > maxAttempts {
		t.Fatalf("attempt ran %d time(s), want <= %d (must not loop past the cap)", calls, maxAttempts)
	}
}

// TestSetupWithRetry_NoPerAttemptBound proves perAttempt <= 0 runs the attempt
// on the parent ctx directly (no sub-context), so an attempt that ignores any
// per-attempt deadline still sees the parent's.
func TestSetupWithRetry_NoPerAttemptBound(t *testing.T) {
	parent, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	var gotDeadline time.Time
	attempt := func(ctx context.Context) (*TestEnvironment, error) {
		d, _ := ctx.Deadline()
		gotDeadline = d
		return &TestEnvironment{}, nil
	}

	if _, err := setupWithRetry(parent, 0, fastBackoff, attempt); err != nil {
		t.Fatalf("setupWithRetry returned error: %v", err)
	}
	parentDeadline, _ := parent.Deadline()
	if !gotDeadline.Equal(parentDeadline) {
		t.Fatalf("attempt ctx deadline = %v, want the parent deadline %v (no per-attempt sub-context)", gotDeadline, parentDeadline)
	}
}

// TestRunSetupCleanup_UsesLiveContext is the partial-cleanup-leak regression:
// when an attempt times out, setupOnce / the start helpers must tear down
// whatever they partially created on a LIVE context (testcontainers Terminate
// needs one to issue Docker stop/remove). runSetupCleanup is the single path
// all those teardowns now route through; this proves the context it hands the
// teardown func is not already-done — even though the per-attempt work ctx (and
// potentially the parent) has expired — and is bounded so it can't itself hang.
func TestRunSetupCleanup_UsesLiveContext(t *testing.T) {
	// Stand in for the failure scenario: the per-attempt work ctx has already
	// expired by the time cleanup runs.
	attemptCtx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	<-attemptCtx.Done() // event-driven: wait for the deadline, no fixed sleep
	if attemptCtx.Err() == nil {
		t.Fatal("precondition: attempt ctx should be expired before cleanup")
	}

	// Capture the cleanup ctx's state INSIDE the callback, while it is still
	// live — runSetupCleanup defers cancel(), so inspecting it after the call
	// returns would always show "canceled".
	var (
		called    bool
		isSame    bool
		errAtCall error
		dlAtCall  time.Time
		dlOK      bool
	)
	runSetupCleanup(func(ctx context.Context) error {
		called = true
		isSame = ctx == attemptCtx
		errAtCall = ctx.Err()
		dlAtCall, dlOK = ctx.Deadline()
		return nil
	})

	if !called {
		t.Fatal("runSetupCleanup did not invoke the teardown func")
	}
	if errAtCall != nil {
		t.Fatalf("cleanup ctx was already done at teardown time (%v); teardown needs a LIVE context", errAtCall)
	}
	if isSame {
		t.Fatal("cleanup ctx is the expired attempt ctx; it must be independent")
	}
	if !dlOK {
		t.Fatal("cleanup ctx has no deadline; it must be bounded")
	}
	if remaining := time.Until(dlAtCall); remaining <= 0 {
		t.Fatalf("cleanup ctx deadline already passed (remaining %s)", remaining)
	}
}

// TestRunSetupCleanup_EnforcesBudget proves the cleanup budget is actually
// enforced: a teardown that IGNORES its context and blocks must NOT stall the
// retry/bail loop — runSetupCleanup must return when the budget expires. This
// is the #422 failure mode (Docker stop/remove that doesn't honor ctx). Under
// the previous synchronous implementation this test would hang and time out.
// Event-driven and non-flaky: a tiny budget bounds the return, and the blocked
// teardown goroutine is released only after the assertion.
func TestRunSetupCleanup_EnforcesBudget(t *testing.T) {
	saved := setupCleanupTimeout
	setupCleanupTimeout = 20 * time.Millisecond
	t.Cleanup(func() { setupCleanupTimeout = saved })

	// release unblocks the (intentionally leaked) teardown goroutine after the
	// assertion, so it can drain to the buffered channel and exit cleanly.
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	started := make(chan struct{})
	returned := make(chan struct{})
	go func() {
		runSetupCleanup(func(ctx context.Context) error {
			close(started)
			<-release // ignore ctx entirely: a sync impl would hang here forever
			return nil
		})
		close(returned)
	}()

	<-started // teardown is running and blocked
	select {
	case <-returned:
		// returned despite teardown ignoring its ctx — budget enforced
	case <-time.After(2 * time.Second):
		t.Fatal("runSetupCleanup did not return within its budget when teardown ignored its context")
	}
}

// TestRunSetupCleanup_ContainsPanic proves a panicking teardown does NOT crash
// the test binary. teardown runs on its own goroutine (outside
// runSetupAttempt's recover), so without containment a panic from
// Terminate/network removal would be unhandled and abort the process. The
// helper must recover it and return normally. Deterministic: the panic fires
// synchronously inside teardown, and runSetupCleanup blocks until it observes
// the result, so reaching the line after the call proves containment.
func TestRunSetupCleanup_ContainsPanic(t *testing.T) {
	var called bool
	runSetupCleanup(func(ctx context.Context) error {
		called = true
		panic("boom from Terminate")
	})

	if !called {
		t.Fatal("runSetupCleanup did not invoke the teardown func")
	}
	// Reaching here at all is the assertion: the panic was contained on the
	// cleanup goroutine instead of crashing the process.
}

func TestSetupBudget(t *testing.T) {
	if got := SetupBudget(SetupOptions{}); got != OverallSetupBudgetLight {
		t.Fatalf("SetupBudget(light) = %s, want %s", got, OverallSetupBudgetLight)
	}
	if got := SetupBudget(SetupOptions{BAMLSource: "/some/baml"}); got != OverallSetupBudgetBAMLSource {
		t.Fatalf("SetupBudget(baml-source) = %s, want %s", got, OverallSetupBudgetBAMLSource)
	}
}

func TestPerAttemptBudget(t *testing.T) {
	// No deadline -> 0 ("no per-attempt bound").
	if got := perAttemptBudget(context.Background()); got != 0 {
		t.Fatalf("perAttemptBudget(no deadline) = %s, want 0", got)
	}

	// Healthy parent -> remaining minus the retry reserve.
	parent, cancel := context.WithTimeout(context.Background(), OverallSetupBudgetLight)
	defer cancel()
	got := perAttemptBudget(parent)
	want := OverallSetupBudgetLight - setupRetryReserve
	// Allow a small delta for the time elapsed between construction and read.
	if delta := want - got; delta < 0 || delta > time.Second {
		t.Fatalf("perAttemptBudget(%s parent) = %s, want ~%s (reserve %s)", OverallSetupBudgetLight, got, want, setupRetryReserve)
	}

	// Parent smaller than the reserve -> 0 (don't shrink the attempt with a
	// non-positive sub-deadline; the parent still caps it).
	tight, cancelTight := context.WithTimeout(context.Background(), setupRetryReserve/2)
	defer cancelTight()
	if got := perAttemptBudget(tight); got != 0 {
		t.Fatalf("perAttemptBudget(sub-reserve parent) = %s, want 0", got)
	}
}
