// Package retry provides retry policy types and an executor for LLM API calls.
//
// It reimplements the retry semantics of BAML's internal CallablePolicy, which
// supports constant-delay and exponential-backoff strategies. This is needed
// because the BuildRequest/StreamRequest modular API bypasses BAML's internal
// retry loop.
//
// The retry policies can be populated from two sources:
//  1. .baml file introspection at build time (embedded in generated code)
//  2. Explicit REST API parameters at runtime (__baml_options__.retry)
package retry

import (
	"context"
	"math"
	"sync"
	"time"
)

// Policy defines a retry policy with a maximum number of retries and a delay
// strategy. A nil Policy means no retries (single attempt).
type Policy struct {
	// MaxRetries is the maximum number of retry attempts (not counting the
	// initial attempt). For example, MaxRetries=3 means up to 4 total attempts.
	MaxRetries int `json:"max_retries"`

	// Strategy determines how long to wait between attempts.
	// If nil, defaults to ConstantDelay with DefaultDelayMs.
	Strategy Strategy `json:"-"`

	// StrategyConfig is used for JSON deserialization of the strategy.
	// It is converted to a Strategy via ResolveStrategy().
	StrategyConfig *StrategyConfig `json:"strategy,omitempty"`

	// resolveOnce ensures ResolveStrategy is only executed once, making it
	// safe to share a single *Policy across concurrent requests.
	resolveOnce sync.Once
}

// StrategyConfig is the JSON-serializable representation of a retry strategy.
type StrategyConfig struct {
	Type       string  `json:"type"`                   // "constant_delay" or "exponential_backoff"
	DelayMs    int     `json:"delay_ms,omitempty"`     // Delay in milliseconds (default: 200)
	Multiplier float64 `json:"multiplier,omitempty"`   // For exponential_backoff (default: 1.5)
	MaxDelayMs int     `json:"max_delay_ms,omitempty"` // For exponential_backoff (default: 10000)
}

// Default values matching BAML's internal defaults.
const (
	DefaultDelayMs    = 200
	DefaultMultiplier = 1.5
	DefaultMaxDelayMs = 10000
)

// ResolveStrategy converts the JSON StrategyConfig to a Strategy interface.
// If StrategyConfig is nil, returns a ConstantDelay with default parameters.
// This is safe to call concurrently — it uses sync.Once internally so the
// strategy is resolved exactly once even when shared across requests.
func (p *Policy) ResolveStrategy() {
	p.resolveOnce.Do(func() {
		if p.Strategy != nil {
			return
		}

		if p.StrategyConfig == nil {
			p.Strategy = &ConstantDelay{DelayMs: DefaultDelayMs}
			return
		}

		switch p.StrategyConfig.Type {
		case "exponential_backoff":
			eb := &ExponentialBackoff{
				DelayMs:    p.StrategyConfig.DelayMs,
				Multiplier: p.StrategyConfig.Multiplier,
				MaxDelayMs: p.StrategyConfig.MaxDelayMs,
			}
			if eb.DelayMs <= 0 {
				eb.DelayMs = DefaultDelayMs
			}
			if eb.Multiplier <= 0 {
				eb.Multiplier = DefaultMultiplier
			}
			if eb.MaxDelayMs <= 0 {
				eb.MaxDelayMs = DefaultMaxDelayMs
			}
			p.Strategy = eb

		default: // "constant_delay" or unrecognized
			cd := &ConstantDelay{
				DelayMs: p.StrategyConfig.DelayMs,
			}
			if cd.DelayMs <= 0 {
				cd.DelayMs = DefaultDelayMs
			}
			p.Strategy = cd
		}
	})
}

// Strategy determines the delay between retry attempts.
type Strategy interface {
	// Delay returns the duration to wait before the given attempt (0-indexed).
	// Attempt 0 is the delay before the first retry (i.e., after the initial
	// attempt fails). The implementation may return different values for each
	// attempt (e.g., exponential backoff).
	Delay(attempt int) time.Duration
}

// ConstantDelay waits a fixed duration between attempts.
type ConstantDelay struct {
	DelayMs int `json:"delay_ms"`
}

// Delay returns the constant delay duration for any attempt.
func (c *ConstantDelay) Delay(_ int) time.Duration {
	return time.Duration(c.DelayMs) * time.Millisecond
}

// ExponentialBackoff increases the delay exponentially between attempts,
// capped at MaxDelayMs.
type ExponentialBackoff struct {
	DelayMs    int     `json:"delay_ms"`     // Initial delay in milliseconds
	Multiplier float64 `json:"multiplier"`   // Multiply delay by this each attempt
	MaxDelayMs int     `json:"max_delay_ms"` // Maximum delay cap in milliseconds
}

// Delay returns the exponential backoff delay for the given attempt.
// delay = min(DelayMs * Multiplier^attempt, MaxDelayMs)
func (e *ExponentialBackoff) Delay(attempt int) time.Duration {
	delay := float64(e.DelayMs) * math.Pow(e.Multiplier, float64(attempt))
	maxDelay := float64(e.MaxDelayMs)
	if delay > maxDelay {
		delay = maxDelay
	}
	return time.Duration(delay) * time.Millisecond
}

// Result wraps a value or error from an attempt.
type Result[T any] struct {
	Value T
	Err   error
}

// Execute runs fn with retry logic according to the given policy.
//
// Parameters:
//   - ctx: context for cancellation. If cancelled during a sleep, Execute
//     returns immediately with ctx.Err().
//   - policy: the retry policy. If nil, fn is called once with no retries.
//   - fn: the function to execute. Called with the attempt number (0-indexed,
//     where 0 is the initial attempt). Should return the result and an error.
//   - onRetry: optional callback invoked before each retry attempt (not called
//     for the initial attempt). Receives the attempt number (1-indexed for
//     retries, so first retry is attempt=1). Can be nil.
//
// Execute follows BAML's semantics: all errors are retried (no error-type
// filtering). The only conditions that stop retrying are:
//   - fn returns nil error (success)
//   - context is cancelled
//   - all retry attempts are exhausted
//
// On the last attempt (attempt == MaxRetries), no sleep occurs after failure.
func Execute[T any](ctx context.Context, policy *Policy, fn func(attempt int) (T, error), onRetry func(attempt int)) (T, error) {
	var zero T

	// No policy = single attempt
	if policy == nil {
		select {
		case <-ctx.Done():
			return zero, ctx.Err()
		default:
		}
		return fn(0)
	}

	// Resolve strategy via sync.Once — safe for concurrent callers sharing
	// the same *Policy. The Once ensures the write to policy.Strategy
	// happens-before any read, eliminating the data race.
	policy.ResolveStrategy()

	// Clamp MaxRetries to at least 0 so we always make at least one attempt.
	// Negative values from user input would otherwise skip the loop entirely.
	maxRetries := policy.MaxRetries
	if maxRetries < 0 {
		maxRetries = 0
	}
	totalAttempts := maxRetries + 1
	var lastErr error

	for attempt := 0; attempt < totalAttempts; attempt++ {
		// Check context before each attempt
		select {
		case <-ctx.Done():
			return zero, ctx.Err()
		default:
		}

		result, err := fn(attempt)
		if err == nil {
			return result, nil
		}
		lastErr = err

		// Don't sleep after the last attempt
		if attempt >= maxRetries {
			break
		}

		// Check context before emitting retry signal
		select {
		case <-ctx.Done():
			return zero, ctx.Err()
		default:
		}

		// Notify caller about the retry
		if onRetry != nil {
			onRetry(attempt + 1)
		}

		// Sleep with context awareness
		delay := policy.Strategy.Delay(attempt)
		if delay > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return zero, ctx.Err()
			case <-timer.C:
			}
		}
	}

	return zero, lastErr
}
