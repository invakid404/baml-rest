package retry

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// terminalErr is a test error implementing the TerminalError contract.
type terminalErr struct{ msg string }

func (e *terminalErr) Error() string       { return e.msg }
func (e *terminalErr) RetryTerminal() bool { return true }

// wrappedTerminal wraps a terminalErr to prove Execute finds the marker through an
// Unwrap chain (mirroring the orchestrator's raw-carrying wrapper around the native
// stream terminal error).
type wrappedTerminal struct{ err error }

func (w *wrappedTerminal) Error() string { return "wrapped: " + w.err.Error() }
func (w *wrappedTerminal) Unwrap() error { return w.err }

// TestExecute_TerminalErrorAbortsRetry proves a TerminalError stops Execute
// immediately: fn runs exactly ONCE under a policy of MaxRetries=3, no onRetry
// callback fires, and the terminal error is returned unchanged. This is the retry
// half of the native-stream no-replay invariant (scope §4 I4).
func TestExecute_TerminalErrorAbortsRetry(t *testing.T) {
	term := &terminalErr{msg: "claimed then failed"}
	var attempts, onRetryCalls int
	_, err := Execute(context.Background(), &Policy{MaxRetries: 3},
		func(int) (int, error) {
			attempts++
			return 0, term
		},
		func(int) { onRetryCalls++ },
	)
	if attempts != 1 {
		t.Errorf("terminal error must run fn exactly once, got %d attempts", attempts)
	}
	if onRetryCalls != 0 {
		t.Errorf("terminal error must fire no onRetry callback, got %d", onRetryCalls)
	}
	if !errors.Is(err, term) {
		t.Errorf("Execute must return the terminal error, got %v", err)
	}
}

// TestExecute_WrappedTerminalAbortsRetry proves the marker is found through an
// Unwrap chain (errors.As), so a terminal wrapped by the orchestrator's raw carrier
// still aborts retries.
func TestExecute_WrappedTerminalAbortsRetry(t *testing.T) {
	term := &terminalErr{msg: "inner terminal"}
	wrapped := fmt.Errorf("outer: %w", &wrappedTerminal{err: term})
	var attempts int
	_, err := Execute(context.Background(), &Policy{MaxRetries: 5},
		func(int) (int, error) {
			attempts++
			return 0, wrapped
		},
		nil,
	)
	if attempts != 1 {
		t.Errorf("wrapped terminal error must run fn exactly once, got %d attempts", attempts)
	}
	if !errors.Is(err, term) {
		t.Errorf("Execute must return the wrapped terminal error, got %v", err)
	}
}

// TestExecute_NonTerminalStillRetries guards against over-reach: an ordinary error
// (no TerminalError marker) still retries up to the policy limit, so the terminal
// abort does not change existing BAML retry behaviour.
func TestExecute_NonTerminalStillRetries(t *testing.T) {
	var attempts int
	_, err := Execute(context.Background(), &Policy{MaxRetries: 2, StrategyConfig: &StrategyConfig{Type: "constant_delay", DelayMs: 1}},
		func(int) (int, error) {
			attempts++
			return 0, errors.New("ordinary transient error")
		},
		nil,
	)
	if attempts != 3 {
		t.Errorf("ordinary error must retry to the policy limit (3 attempts), got %d", attempts)
	}
	if err == nil {
		t.Errorf("expected the last error to surface after exhaustion")
	}
}
