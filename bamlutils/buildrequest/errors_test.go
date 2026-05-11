package buildrequest

import (
	"errors"
	"fmt"
	"testing"
)

// TestErrOutputParseIs pins the errors.Is contract on OutputParseError —
// the wrapper's Is method must match the package sentinel so callers can
// branch on `errors.Is(err, ErrOutputParse)` without also threading the
// sentinel through Unwrap.
func TestErrOutputParseIs(t *testing.T) {
	t.Parallel()

	inner := errors.New("Parsing error: bad field")
	wrapped := &OutputParseError{Err: inner}

	if !errors.Is(wrapped, ErrOutputParse) {
		t.Fatal("errors.Is should match ErrOutputParse on a bare OutputParseError")
	}

	// Wrapping via fmt.Errorf %w should still satisfy errors.Is via the
	// wrapper's Unwrap method (since fmt.Errorf returns a chain).
	chained := fmt.Errorf("buildrequest: %w", wrapped)
	if !errors.Is(chained, ErrOutputParse) {
		t.Fatal("errors.Is should walk through fmt.Errorf wrap")
	}

	// errors.Is against a plain error must return false — the wrapper
	// matches only the sentinel, not arbitrary errors.
	if errors.Is(wrapped, errors.New("other")) {
		t.Fatal("errors.Is should not match arbitrary errors")
	}
}

// TestErrOutputParseAs pins the errors.As contract — callers can recover
// the typed wrapper to access the underlying BAML error verbatim.
func TestErrOutputParseAs(t *testing.T) {
	t.Parallel()

	inner := errors.New("Parsing error: bad field")
	wrapped := fmt.Errorf("buildrequest: failed to parse final result: %w", inner)
	classified := &OutputParseError{Err: wrapped}
	chained := fmt.Errorf("outer: %w", classified)

	var got *OutputParseError
	if !errors.As(chained, &got) {
		t.Fatal("errors.As should recover *OutputParseError through a wrap chain")
	}
	if got == nil {
		t.Fatal("errors.As populated nil pointer")
	}
	if !errors.Is(got, ErrOutputParse) {
		t.Fatal("recovered OutputParseError should still satisfy ErrOutputParse")
	}
}

// TestOutputParseErrorPreservesMessage verifies the wrapper's Error()
// returns the underlying BAML message verbatim so downstream consumers
// (e.g. apierror.error in the HTTP response envelope) still see the
// original diagnostic text. The classification code rides alongside on
// the wire — it does NOT replace the message.
func TestOutputParseErrorPreservesMessage(t *testing.T) {
	t.Parallel()

	const msg = "buildrequest: failed to parse final result: Parsing error: bad field"
	inner := errors.New(msg)
	wrapped := &OutputParseError{Err: inner}

	if wrapped.Error() != msg {
		t.Fatalf("Error() = %q, want %q", wrapped.Error(), msg)
	}
	if !errors.Is(wrapped, inner) {
		t.Fatal("errors.Is should still match the underlying error via Unwrap")
	}
}

// TestWrapOutputParseNilPassthrough confirms the helper used at the
// final-parse call sites returns nil for a nil error so call sites can
// write `return wrapOutputParse(parseErr)` unconditionally.
func TestWrapOutputParseNilPassthrough(t *testing.T) {
	t.Parallel()

	if got := wrapOutputParse(nil); got != nil {
		t.Fatalf("wrapOutputParse(nil) = %v, want nil", got)
	}
}
