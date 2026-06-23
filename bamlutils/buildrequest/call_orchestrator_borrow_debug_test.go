//go:build llmhttp_debug

package buildrequest

import (
	"context"
	"errors"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/bamlutils/retry"
)

// Stage 2 (#487): per-attempt release ordering, verified deterministically via
// the debug-build live-borrow gauge (llmhttp.LiveBorrowsForTest). The gauge is
// incremented when ExecuteBorrowed arms a borrowed response and decremented
// when Release disarms it, both synchronously on the orchestration goroutine.
//
// The orchestrator must release each attempt's borrowed response BEFORE the
// next attempt runs, so at the moment any attempt extracts its body exactly one
// borrow is outstanding. A regression that deferred release to per-call (or
// otherwise accumulated borrows across attempts) would show the gauge climbing
// above the single-attempt baseline — caught here without relying on sync.Pool
// reuse or scheduler timing.
//
// These tests are debug-tagged because the gauge only exists under
// -tags llmhttp_debug (zero-cost in production builds); run them with
// `go test -tags llmhttp_debug ./bamlutils/buildrequest`.

// assertOneLiveBorrow fails if the outstanding-borrow count is not exactly
// base+1 — i.e. the current attempt's borrow plus nothing left over from a
// prior, supposedly-released attempt.
func assertOneLiveBorrow(t *testing.T, base int64, attempt int) {
	t.Helper()
	if got := llmhttp.LiveBorrowsForTest(); got != base+1 {
		t.Errorf("attempt %d: %d borrows outstanding during extraction, want exactly 1 "+
			"(base=%d) — a prior attempt's borrowed response was not released before this one",
			attempt, got-base, base)
	}
}

// TestRunCallOrchestration_RetryReleasesBeforeNextAttempt proves the retry path
// releases each attempt's borrowed response before the next attempt starts:
// attempt 0 fails at parseFinal (after extraction), attempt 1 succeeds, and the
// live-borrow gauge reads exactly one throughout — attempt 0's borrow was
// released before attempt 1 armed its own.
func TestRunCallOrchestration_RetryReleasesBeforeNextAttempt(t *testing.T) {
	const body = `{"choices":[{"message":{"content":"hello"}}]}`
	server := makeJSONServer(200, body)
	defer server.Close()
	client := forceFastClient(server)

	base := llmhttp.LiveBorrowsForTest()

	var calls int
	extractor := func(provider, responseBody string, includeReasoning bool) (string, string, string, error) {
		assertOneLiveBorrow(t, base, calls)
		calls++
		return "parsed", "", "", nil
	}
	var parseCalls int
	parseFinal := func(_ context.Context, text string) (any, error) {
		parseCalls++
		if parseCalls == 1 {
			return nil, errors.New("force retry")
		}
		return text, nil
	}

	out := make(chan bamlutils.StreamResult, 16)
	err := RunCallOrchestration(
		context.Background(), out,
		&CallConfig{
			Provider:    "openai",
			RetryPolicy: &retry.Policy{MaxRetries: 1, Strategy: &retry.ConstantDelay{DelayMs: 1}},
		},
		client, makeBuildCallRequest(server.URL),
		parseFinal, extractor, nil, nil, newTestResult,
	)
	close(out)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = lastFinal(t, out) // attempt 1 succeeded

	if calls != 2 {
		t.Fatalf("expected 2 extractor calls (initial + 1 retry), got %d", calls)
	}
	// After the orchestration completes, every borrow must be released.
	if got := llmhttp.LiveBorrowsForTest(); got != base {
		t.Errorf("after orchestration %d borrows still outstanding, want 0 (base=%d)", got-base, base)
	}
}

// TestRunCallOrchestration_FallbackReleasesBeforeSwitching proves the same
// per-attempt release ordering across a provider-fallback switch: child0's
// extraction fails (advancing the chain), child1 succeeds, and exactly one
// borrow is outstanding while each child extracts — child0's borrow was
// released before child1 armed its own.
func TestRunCallOrchestration_FallbackReleasesBeforeSwitching(t *testing.T) {
	const body = `{"choices":[{"message":{"content":"hello"}}]}`
	server := makeJSONServer(200, body)
	defer server.Close()
	client := forceFastClient(server)

	base := llmhttp.LiveBorrowsForTest()

	var calls int
	extractor := func(provider, responseBody string, includeReasoning bool) (string, string, string, error) {
		assertOneLiveBorrow(t, base, calls)
		calls++
		if calls == 1 {
			return "", "", "", errors.New("child0 extraction failure")
		}
		return "parsed", "", "", nil
	}

	out := make(chan bamlutils.StreamResult, 16)
	err := RunCallOrchestration(
		context.Background(), out,
		&CallConfig{
			FallbackChain:   []string{"child0", "child1"},
			ClientProviders: map[string]string{"child0": "openai", "child1": "openai"},
		},
		client, makeBuildCallRequest(server.URL),
		identityParseFinal, extractor, nil, nil, newTestResult,
	)
	close(out)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = lastFinal(t, out) // child1 succeeded

	if calls != 2 {
		t.Fatalf("expected 2 extractor calls (child0 + child1), got %d", calls)
	}
	if got := llmhttp.LiveBorrowsForTest(); got != base {
		t.Errorf("after orchestration %d borrows still outstanding, want 0 (base=%d)", got-base, base)
	}
}
