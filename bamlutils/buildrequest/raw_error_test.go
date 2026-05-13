package buildrequest

import (
	"errors"
	"fmt"
	"testing"
)

// TestNewRawErrorNilInput pins the nil-error short-circuit: callers
// can write `return newRawError(maybeNilErr, raw)` unconditionally and
// trust that nil-in stays nil-out without an allocation.
func TestNewRawErrorNilInput(t *testing.T) {
	t.Parallel()

	if got := newRawError(nil, "ignored"); got != nil {
		t.Fatalf("newRawError(nil, raw) = %v, want nil", got)
	}
}

// TestNewRawErrorEmptyRaw pins the empty-raw short-circuit: when the
// attempt failed before accumulating any text, the wrapper is skipped
// entirely so common pre-stream failures keep their original error
// identity (and zero extra allocations).
func TestNewRawErrorEmptyRaw(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("transport failed")
	got := newRawError(sentinel, "")
	if got != sentinel {
		t.Fatalf("newRawError(err, \"\") should pass through the original error, got %v", got)
	}
}

// TestNewRawErrorWrapsBoth verifies the happy path — wrapper preserves
// the original Error() message and exposes raw via Raw()/Unwrap().
func TestNewRawErrorWrapsBoth(t *testing.T) {
	t.Parallel()

	inner := errors.New("parse failed")
	wrapped := newRawError(inner, "partial output")

	if wrapped.Error() != "parse failed" {
		t.Fatalf("Error() = %q, want %q", wrapped.Error(), "parse failed")
	}
	if !errors.Is(wrapped, inner) {
		t.Fatal("errors.Is should match the wrapped inner via Unwrap")
	}
	rce, ok := wrapped.(*rawCarryingError)
	if !ok {
		t.Fatalf("expected *rawCarryingError, got %T", wrapped)
	}
	if rce.Raw() != "partial output" {
		t.Fatalf("Raw() = %q, want %q", rce.Raw(), "partial output")
	}
}

// TestRawFromErrorNilInput guards against panics — the outer emission
// path calls rawFromError unconditionally on the retry.Execute return,
// which is nil on success.
func TestRawFromErrorNilInput(t *testing.T) {
	t.Parallel()

	if got := rawFromError(nil); got != "" {
		t.Fatalf("rawFromError(nil) = %q, want \"\"", got)
	}
}

// TestRawFromErrorPlainError covers the orchestrator's "attempt failed
// before any accumulation" path: a plain transport error reaches the
// outer emission, the wrapper was never applied, and rawFromError
// returns empty so the worker bridge's mergeRawDetail no-ops.
func TestRawFromErrorPlainError(t *testing.T) {
	t.Parallel()

	if got := rawFromError(errors.New("plain")); got != "" {
		t.Fatalf("rawFromError(plain) = %q, want \"\"", got)
	}
}

// TestRawFromErrorWalksSingleWrap is the simplest extraction case.
func TestRawFromErrorWalksSingleWrap(t *testing.T) {
	t.Parallel()

	wrapped := newRawError(errors.New("boom"), "abc")
	if got := rawFromError(wrapped); got != "abc" {
		t.Fatalf("rawFromError = %q, want %q", got, "abc")
	}
}

// TestRawFromErrorWalksFmtErrorf covers the realistic streaming
// orchestrator shape: attempt returns `fmt.Errorf("buildrequest: stream
// error: %w", err)`, the retry layer wraps further, and the outer
// emission still recovers raw.
func TestRawFromErrorWalksFmtErrorf(t *testing.T) {
	t.Parallel()

	inner := newRawError(errors.New("upstream eof"), "midstream text")
	wrapped := fmt.Errorf("buildrequest: stream error: %w", inner)
	wrapped = fmt.Errorf("retry exhausted: %w", wrapped)

	if got := rawFromError(wrapped); got != "midstream text" {
		t.Fatalf("rawFromError = %q, want %q", got, "midstream text")
	}
}

// TestRawFromErrorWalksDoubleWrap protects against accidental
// double-wrapping (e.g. if a retry boundary re-wrapped) — errors.As
// finds the innermost rawCarryingError. Whichever wrap is closest to
// the chain head wins, so callers should wrap at the failing-attempt
// site and not re-wrap downstream.
func TestRawFromErrorWalksDoubleWrap(t *testing.T) {
	t.Parallel()

	inner := newRawError(errors.New("boom"), "outer-raw")
	doubleWrapped := newRawError(inner, "should-not-be-used")

	// errors.As returns the FIRST match in the chain; we wrap the
	// inner so the outer is the head — its raw wins.
	if got := rawFromError(doubleWrapped); got != "should-not-be-used" {
		t.Fatalf("rawFromError on double-wrap = %q, want outermost %q", got, "should-not-be-used")
	}
}

// TestRawCarryingErrorPreservesClassification verifies that wrapping a
// classified parse error with rawCarryingError keeps the classification
// reachable via errors.Is — important because the worker bridge calls
// classifyBAMLError(result.Error()) on the emitted error, and that
// classifier branches on `errors.Is(err, ErrOutputParse)`.
func TestRawCarryingErrorPreservesClassification(t *testing.T) {
	t.Parallel()

	parseErr := wrapOutputParse(errors.New("Parsing error: bad field"))
	wrapped := newRawError(parseErr, "some accumulated raw")

	if !errors.Is(wrapped, ErrOutputParse) {
		t.Fatal("rawCarryingError must not hide ErrOutputParse from errors.Is")
	}
	if got := rawFromError(wrapped); got != "some accumulated raw" {
		t.Fatalf("rawFromError = %q, want %q", got, "some accumulated raw")
	}
}
