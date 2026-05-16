package worker

import (
	"context"
	"strings"
	"testing"
)

// TestGetGoroutinesFilterBranches exercises the goroutine-filter
// branches (empty / include-only / exclude-only / no-op exclude) against
// the live test-runner goroutines. The fictitious sentinel pattern is
// chosen so it cannot appear in any real stack, which makes the
// exclude-only assertion symmetric to the include-only one.
func TestGetGoroutinesFilterBranches(t *testing.T) {
	// Not parallel: reads the live goroutine profile, and other parallel
	// tests in the package spawn drain goroutines that would race with
	// the include-pattern count.

	const sentinel = "baml-rest-pr280-filter-sentinel-xyz"

	h, err := New(Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()

	t.Run("empty filter returns no matched stacks", func(t *testing.T) {
		got, err := h.GetGoroutines(ctx, "")
		if err != nil {
			t.Fatalf("GetGoroutines: %v", err)
		}
		if got.TotalCount <= 0 {
			t.Fatalf("TotalCount: got %d, want > 0", got.TotalCount)
		}
		if got.MatchCount != 0 || len(got.MatchedStacks) != 0 {
			t.Fatalf("empty filter must return no matched stacks; got MatchCount=%d, len(MatchedStacks)=%d",
				got.MatchCount, len(got.MatchedStacks))
		}
	})

	t.Run("include-only fictitious pattern matches nothing", func(t *testing.T) {
		got, err := h.GetGoroutines(ctx, sentinel)
		if err != nil {
			t.Fatalf("GetGoroutines: %v", err)
		}
		if got.MatchCount != 0 || len(got.MatchedStacks) != 0 {
			t.Fatalf("sentinel include must match no stacks; got MatchCount=%d, MatchedStacks=%v",
				got.MatchCount, got.MatchedStacks)
		}
	})

	t.Run("exclude-only fictitious pattern matches every stack", func(t *testing.T) {
		got, err := h.GetGoroutines(ctx, "-"+sentinel)
		if err != nil {
			t.Fatalf("GetGoroutines: %v", err)
		}
		// The sentinel cannot appear in any real stack, so an exclude-only
		// filter must return at least one matched stack. Before the fix
		// the outer gate short-circuited on empty includePatterns and
		// MatchCount was zero.
		if got.MatchCount == 0 || len(got.MatchedStacks) == 0 {
			t.Fatalf("exclude-only filter returned zero matched stacks; want > 0")
		}
		// And none of the returned stacks should contain the sentinel
		// (vacuously true, but pins the exclude pass actually runs).
		for _, stack := range got.MatchedStacks {
			if strings.Contains(strings.ToLower(stack), sentinel) {
				t.Errorf("matched stack contains the excluded sentinel: %q", stack)
			}
		}
	})

	t.Run("include-only testing pattern hits the test runner", func(t *testing.T) {
		// "testing." appears in every Go test process (the test runner
		// goroutine itself), so an include-only filter on it must
		// surface at least one stack. Guards against an over-eager fix
		// that broke the include path.
		got, err := h.GetGoroutines(ctx, "testing.")
		if err != nil {
			t.Fatalf("GetGoroutines: %v", err)
		}
		if got.MatchCount == 0 || len(got.MatchedStacks) == 0 {
			t.Fatalf("include filter for 'testing.' returned zero matches; expected the test runner stack")
		}
	})
}
