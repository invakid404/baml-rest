package workerplugin

import (
	"errors"
	"testing"
)

// TestNewErrorWithMetadata_CopiesDetails pins the defensive-copy
// contract: ErrorWithStack.Details must own its bytes so that
// callers reusing pooled decode buffers (StreamResult.ErrorDetails
// from a streamResultPool entry that's about to be released) can't
// mutate the wrapped error's payload after-the-fact. Without the
// copy, releasing the StreamResult and re-using the buffer would
// race with any later read of the wrapped error's Details.
func TestNewErrorWithMetadata_CopiesDetails(t *testing.T) {
	original := []byte(`{"code":"parse_error","field":"x"}`)
	wrapped := NewErrorWithMetadata(errors.New("boom"), "", "parse_error", original)

	stackErr, ok := wrapped.(*ErrorWithStack)
	if !ok {
		t.Fatalf("expected *ErrorWithStack, got %T", wrapped)
	}

	// Mutate the input slice — simulating buffer reuse after the
	// wrap. The wrapped error must NOT observe the mutation.
	for i := range original {
		original[i] = 'X'
	}

	if got := string(stackErr.GetDetails()); got != `{"code":"parse_error","field":"x"}` {
		t.Errorf("wrapped Details aliased input slice; got %q", got)
	}
}

// TestNewErrorWithMetadata_NoMetadataReturnsRaw verifies the
// fast-path: when stacktrace, code, and details are all empty, the
// original err is returned unwrapped (no allocation). Pinned so a
// later refactor can't accidentally pay the wrapper cost on every
// happy-path return through helpers that call NewErrorWithMetadata
// unconditionally.
func TestNewErrorWithMetadata_NoMetadataReturnsRaw(t *testing.T) {
	orig := errors.New("plain")
	got := NewErrorWithMetadata(orig, "", "", nil)
	if got != orig {
		t.Errorf("expected raw err returned when no metadata; got %v", got)
	}

	got = NewErrorWithMetadata(orig, "", "", []byte{})
	if got != orig {
		t.Errorf("expected raw err returned for empty details slice; got %v", got)
	}
}
