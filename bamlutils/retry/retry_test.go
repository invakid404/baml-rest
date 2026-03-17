package retry

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestConstantDelay(t *testing.T) {
	cd := &ConstantDelay{DelayMs: 100}

	for i := 0; i < 5; i++ {
		d := cd.Delay(i)
		expected := 100 * time.Millisecond
		if d != expected {
			t.Errorf("attempt %d: expected %v, got %v", i, expected, d)
		}
	}
}

func TestExponentialBackoff(t *testing.T) {
	eb := &ExponentialBackoff{
		DelayMs:    200,
		Multiplier: 2.0,
		MaxDelayMs: 5000,
	}

	tests := []struct {
		attempt  int
		expected time.Duration
	}{
		{0, 200 * time.Millisecond},   // 200 * 2^0 = 200
		{1, 400 * time.Millisecond},   // 200 * 2^1 = 400
		{2, 800 * time.Millisecond},   // 200 * 2^2 = 800
		{3, 1600 * time.Millisecond},  // 200 * 2^3 = 1600
		{4, 3200 * time.Millisecond},  // 200 * 2^4 = 3200
		{5, 5000 * time.Millisecond},  // 200 * 2^5 = 6400, capped at 5000
		{10, 5000 * time.Millisecond}, // way over cap
	}

	for _, tt := range tests {
		d := eb.Delay(tt.attempt)
		if d != tt.expected {
			t.Errorf("attempt %d: expected %v, got %v", tt.attempt, tt.expected, d)
		}
	}
}

func TestExponentialBackoffDefaults(t *testing.T) {
	eb := &ExponentialBackoff{
		DelayMs:    DefaultDelayMs,
		Multiplier: DefaultMultiplier,
		MaxDelayMs: DefaultMaxDelayMs,
	}

	// Attempt 0: 200 * 1.5^0 = 200ms
	d := eb.Delay(0)
	if d != 200*time.Millisecond {
		t.Errorf("attempt 0: expected 200ms, got %v", d)
	}

	// Attempt 1: 200 * 1.5^1 = 300ms
	d = eb.Delay(1)
	if d != 300*time.Millisecond {
		t.Errorf("attempt 1: expected 300ms, got %v", d)
	}
}

func TestExecuteSuccess(t *testing.T) {
	policy := &Policy{MaxRetries: 3, Strategy: &ConstantDelay{DelayMs: 1}}

	attempts := 0
	result, err := Execute(context.Background(), policy, func(attempt int) (string, error) {
		attempts++
		return "ok", nil
	}, nil)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "ok" {
		t.Errorf("expected 'ok', got %q", result)
	}
	if attempts != 1 {
		t.Errorf("expected 1 attempt, got %d", attempts)
	}
}

func TestExecuteSuccessAfterRetries(t *testing.T) {
	policy := &Policy{MaxRetries: 3, Strategy: &ConstantDelay{DelayMs: 1}}

	testErr := errors.New("temporary error")
	attempts := 0
	retries := 0

	result, err := Execute(context.Background(), policy, func(attempt int) (string, error) {
		attempts++
		if attempt < 2 {
			return "", testErr
		}
		return "ok", nil
	}, func(attempt int) {
		retries++
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "ok" {
		t.Errorf("expected 'ok', got %q", result)
	}
	if attempts != 3 {
		t.Errorf("expected 3 attempts (initial + 2 retries), got %d", attempts)
	}
	if retries != 2 {
		t.Errorf("expected 2 onRetry calls, got %d", retries)
	}
}

func TestExecuteAllRetriesExhausted(t *testing.T) {
	policy := &Policy{MaxRetries: 2, Strategy: &ConstantDelay{DelayMs: 1}}

	testErr := errors.New("persistent error")
	attempts := 0

	_, err := Execute(context.Background(), policy, func(attempt int) (string, error) {
		attempts++
		return "", testErr
	}, nil)

	if !errors.Is(err, testErr) {
		t.Fatalf("expected testErr, got %v", err)
	}
	if attempts != 3 {
		t.Errorf("expected 3 attempts (1 initial + 2 retries), got %d", attempts)
	}
}

func TestExecuteNilPolicy(t *testing.T) {
	attempts := 0
	result, err := Execute[string](context.Background(), nil, func(attempt int) (string, error) {
		attempts++
		return "single", nil
	}, nil)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "single" {
		t.Errorf("expected 'single', got %q", result)
	}
	if attempts != 1 {
		t.Errorf("expected 1 attempt with nil policy, got %d", attempts)
	}
}

func TestExecuteNilPolicyError(t *testing.T) {
	testErr := errors.New("fail")
	_, err := Execute[string](context.Background(), nil, func(attempt int) (string, error) {
		return "", testErr
	}, nil)

	if !errors.Is(err, testErr) {
		t.Fatalf("expected testErr, got %v", err)
	}
}

func TestExecuteContextCancellation(t *testing.T) {
	policy := &Policy{MaxRetries: 100, Strategy: &ConstantDelay{DelayMs: 100}}

	ctx, cancel := context.WithCancel(context.Background())

	started := make(chan struct{})
	attempts := 0
	go func() {
		<-started
		cancel()
	}()

	_, err := Execute(ctx, policy, func(attempt int) (string, error) {
		attempts++
		if attempts == 1 {
			close(started)
		}
		return "", errors.New("fail")
	}, nil)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	// Should have made at most a few attempts before cancellation
	if attempts > 10 {
		t.Errorf("expected few attempts before cancel, got %d", attempts)
	}
}

func TestExecuteContextCancelledBeforeStart(t *testing.T) {
	policy := &Policy{MaxRetries: 3, Strategy: &ConstantDelay{DelayMs: 1}}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	attempts := 0
	_, err := Execute(ctx, policy, func(attempt int) (string, error) {
		attempts++
		return "should not reach", nil
	}, nil)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if attempts != 0 {
		t.Errorf("expected 0 attempts with pre-cancelled context, got %d", attempts)
	}
}

func TestExecuteOnRetryCalledCorrectly(t *testing.T) {
	policy := &Policy{MaxRetries: 3, Strategy: &ConstantDelay{DelayMs: 1}}

	testErr := errors.New("fail")
	var retryAttempts []int

	_, err := Execute(context.Background(), policy, func(attempt int) (string, error) {
		return "", testErr
	}, func(attempt int) {
		retryAttempts = append(retryAttempts, attempt)
	})

	if err == nil {
		t.Error("expected error when all retries exhausted")
	}

	// onRetry should be called with attempt numbers 1, 2, 3
	expected := []int{1, 2, 3}
	if len(retryAttempts) != len(expected) {
		t.Fatalf("expected %d onRetry calls, got %d", len(expected), len(retryAttempts))
	}
	for i, want := range expected {
		if retryAttempts[i] != want {
			t.Errorf("onRetry[%d]: expected attempt=%d, got %d", i, want, retryAttempts[i])
		}
	}
}

func TestExecuteNilOnRetry(t *testing.T) {
	policy := &Policy{MaxRetries: 2, Strategy: &ConstantDelay{DelayMs: 1}}

	// Should not panic with nil onRetry
	_, err := Execute(context.Background(), policy, func(attempt int) (string, error) {
		if attempt < 2 {
			return "", errors.New("fail")
		}
		return "ok", nil
	}, nil)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestResolveStrategyConstantDelay(t *testing.T) {
	p := &Policy{
		MaxRetries: 3,
		StrategyConfig: &StrategyConfig{
			Type:    "constant_delay",
			DelayMs: 500,
		},
	}
	p.ResolveStrategy()

	cd, ok := p.Strategy.(*ConstantDelay)
	if !ok {
		t.Fatalf("expected *ConstantDelay, got %T", p.Strategy)
	}
	if cd.DelayMs != 500 {
		t.Errorf("expected DelayMs=500, got %d", cd.DelayMs)
	}
}

func TestResolveStrategyExponentialBackoff(t *testing.T) {
	p := &Policy{
		MaxRetries: 3,
		StrategyConfig: &StrategyConfig{
			Type:       "exponential_backoff",
			DelayMs:    100,
			Multiplier: 2.0,
			MaxDelayMs: 5000,
		},
	}
	p.ResolveStrategy()

	eb, ok := p.Strategy.(*ExponentialBackoff)
	if !ok {
		t.Fatalf("expected *ExponentialBackoff, got %T", p.Strategy)
	}
	if eb.DelayMs != 100 {
		t.Errorf("expected DelayMs=100, got %d", eb.DelayMs)
	}
	if eb.Multiplier != 2.0 {
		t.Errorf("expected Multiplier=2.0, got %f", eb.Multiplier)
	}
	if eb.MaxDelayMs != 5000 {
		t.Errorf("expected MaxDelayMs=5000, got %d", eb.MaxDelayMs)
	}
}

func TestResolveStrategyDefaults(t *testing.T) {
	// No StrategyConfig at all -> default ConstantDelay(200ms)
	p := &Policy{MaxRetries: 1}
	p.ResolveStrategy()

	cd, ok := p.Strategy.(*ConstantDelay)
	if !ok {
		t.Fatalf("expected *ConstantDelay, got %T", p.Strategy)
	}
	if cd.DelayMs != DefaultDelayMs {
		t.Errorf("expected default DelayMs=%d, got %d", DefaultDelayMs, cd.DelayMs)
	}
}

func TestResolveStrategyExponentialDefaults(t *testing.T) {
	// ExponentialBackoff with zero values -> use defaults
	p := &Policy{
		MaxRetries:     1,
		StrategyConfig: &StrategyConfig{Type: "exponential_backoff"},
	}
	p.ResolveStrategy()

	eb, ok := p.Strategy.(*ExponentialBackoff)
	if !ok {
		t.Fatalf("expected *ExponentialBackoff, got %T", p.Strategy)
	}
	if eb.DelayMs != DefaultDelayMs {
		t.Errorf("expected default DelayMs=%d, got %d", DefaultDelayMs, eb.DelayMs)
	}
	if eb.Multiplier != DefaultMultiplier {
		t.Errorf("expected default Multiplier=%f, got %f", DefaultMultiplier, eb.Multiplier)
	}
	if eb.MaxDelayMs != DefaultMaxDelayMs {
		t.Errorf("expected default MaxDelayMs=%d, got %d", DefaultMaxDelayMs, eb.MaxDelayMs)
	}
}

func TestResolveStrategyIdempotent(t *testing.T) {
	customStrategy := &ConstantDelay{DelayMs: 999}
	p := &Policy{MaxRetries: 1, Strategy: customStrategy}

	p.ResolveStrategy()

	// Should not overwrite existing strategy
	if p.Strategy != customStrategy {
		t.Error("ResolveStrategy should not overwrite existing strategy")
	}
}

func TestResolveStrategyUnknownType(t *testing.T) {
	// Unknown type defaults to constant_delay
	p := &Policy{
		MaxRetries: 1,
		StrategyConfig: &StrategyConfig{
			Type:    "unknown_strategy",
			DelayMs: 300,
		},
	}
	p.ResolveStrategy()

	cd, ok := p.Strategy.(*ConstantDelay)
	if !ok {
		t.Fatalf("expected *ConstantDelay for unknown type, got %T", p.Strategy)
	}
	if cd.DelayMs != 300 {
		t.Errorf("expected DelayMs=300, got %d", cd.DelayMs)
	}
}

func TestExecuteZeroRetries(t *testing.T) {
	policy := &Policy{MaxRetries: 0, Strategy: &ConstantDelay{DelayMs: 1}}

	testErr := errors.New("fail")
	attempts := 0

	_, err := Execute(context.Background(), policy, func(attempt int) (string, error) {
		attempts++
		return "", testErr
	}, nil)

	if !errors.Is(err, testErr) {
		t.Fatalf("expected testErr, got %v", err)
	}
	if attempts != 1 {
		t.Errorf("expected 1 attempt with MaxRetries=0, got %d", attempts)
	}
}

func TestExecuteIntResult(t *testing.T) {
	policy := &Policy{MaxRetries: 1, Strategy: &ConstantDelay{DelayMs: 1}}

	result, err := Execute(context.Background(), policy, func(attempt int) (int, error) {
		return 42, nil
	}, nil)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != 42 {
		t.Errorf("expected 42, got %d", result)
	}
}

// TestExecuteConcurrentSharedPolicy verifies that sharing a single *Policy
// across concurrent Execute calls does not race. This was a reported bug:
// Execute read policy.Strategy without synchronization while ResolveStrategy
// wrote it on first use. The fix uses sync.Once inside ResolveStrategy.
func TestExecuteConcurrentSharedPolicy(t *testing.T) {
	// A shared policy with only StrategyConfig set (Strategy is nil).
	// ResolveStrategy must be called to populate Strategy — the race is
	// between the first concurrent callers that trigger that resolution.
	shared := &Policy{
		MaxRetries: 1,
		StrategyConfig: &StrategyConfig{
			Type:    "constant_delay",
			DelayMs: 1,
		},
	}

	const goroutines = 50
	errs := make(chan error, goroutines)

	for i := 0; i < goroutines; i++ {
		go func() {
			_, err := Execute(context.Background(), shared, func(attempt int) (string, error) {
				return "ok", nil
			}, nil)
			errs <- err
		}()
	}

	for i := 0; i < goroutines; i++ {
		if err := <-errs; err != nil {
			t.Errorf("concurrent Execute failed: %v", err)
		}
	}
}

func TestExecuteNegativeMaxRetries(t *testing.T) {
	// Negative MaxRetries should be clamped to 0 (single attempt), not skip the request.
	policy := &Policy{MaxRetries: -5, Strategy: &ConstantDelay{DelayMs: 1}}

	attempts := 0
	result, err := Execute(context.Background(), policy, func(attempt int) (string, error) {
		attempts++
		return "ok", nil
	}, nil)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "ok" {
		t.Errorf("expected 'ok', got %q", result)
	}
	if attempts != 1 {
		t.Errorf("expected exactly 1 attempt with negative MaxRetries, got %d", attempts)
	}
}

func TestExecuteNegativeMaxRetriesWithError(t *testing.T) {
	// With negative MaxRetries and a failing function, should still attempt once
	// and return the error.
	policy := &Policy{MaxRetries: -1, Strategy: &ConstantDelay{DelayMs: 1}}

	testErr := errors.New("fail")
	attempts := 0

	_, err := Execute(context.Background(), policy, func(attempt int) (string, error) {
		attempts++
		return "", testErr
	}, nil)

	if !errors.Is(err, testErr) {
		t.Fatalf("expected testErr, got %v", err)
	}
	if attempts != 1 {
		t.Errorf("expected exactly 1 attempt with negative MaxRetries, got %d", attempts)
	}
}
