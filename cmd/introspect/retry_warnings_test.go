package main

import (
	"io"
	"os"
	"strings"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils/bamlparser"
)

// TestProcessBAMLRetryPolicyBlock_InvalidNumericFieldsEmitWarnings pins
// the four stderr warnings emitted by processBAMLRetryPolicyBlock when a
// retry_policy declares non-numeric values for max_retries, delay_ms,
// multiplier, or max_delay_ms. The warnings were preserved
// byte-for-byte from the old parseRetryPolicyBlock helper in #265 PR 2
// but never had a dedicated test — the parity/golden comparison only
// observes *bamlConfig, not stderr — so a future refactor could silently
// drop them without any test surface breaking. This test makes the
// warnings part of the regression net.
func TestProcessBAMLRetryPolicyBlock_InvalidNumericFieldsEmitWarnings(t *testing.T) {
	const src = `
retry_policy BadNums {
  max_retries not_a_number
  strategy {
    type exponential_backoff
    delay_ms also_not_a_number
    multiplier still_not_a_number
    max_delay_ms nope_neither
  }
}
`
	file, err := bamlparser.ParseString("retry_warnings.baml", src)
	if err != nil {
		t.Fatalf("parser failed: %v", err)
	}

	captured := captureStderr(t, func() {
		cfg := newTestBamlConfig()
		processBAMLFile(cfg, file)
	})

	wantSubstrings := []string{
		"invalid max_retries value in retry_policy BadNums",
		"invalid delay_ms value in retry_policy BadNums",
		"invalid multiplier value in retry_policy BadNums",
		"invalid max_delay_ms value in retry_policy BadNums",
	}

	// Total-count check: catches both duplicates (same warning fires twice
	// for one bad field) and extras (a new spurious warning gets emitted by
	// future code). The walker uses a common "warning: invalid " prefix
	// for every numeric-field warning, so counting that prefix gives an
	// upper bound on the number of warning lines.
	const warningPrefix = "warning: invalid "
	if got := strings.Count(captured, warningPrefix); got != len(wantSubstrings) {
		t.Fatalf("warning count = %d, want %d\n--- captured stderr ---\n%s",
			got, len(wantSubstrings), captured)
	}
	for _, want := range wantSubstrings {
		if got := strings.Count(captured, want); got != 1 {
			t.Errorf("warning %q count = %d, want 1\n--- captured stderr ---\n%s",
				want, got, captured)
		}
	}
}

// captureStderr redirects os.Stderr to a pipe for the duration of fn and
// returns whatever fn wrote. Both pipe ends are closed before returning,
// and os.Stderr is restored to its original value immediately after fn
// runs (the t.Cleanup is kept as a safety net for the rare panic-mid-fn
// case so the rest of the test suite isn't left writing into a closed
// pipe).
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	orig := os.Stderr
	os.Stderr = w
	t.Cleanup(func() { os.Stderr = orig })

	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()

	fn()
	os.Stderr = orig
	if err := w.Close(); err != nil {
		t.Fatalf("close stderr writer: %v", err)
	}
	captured := <-done
	if err := r.Close(); err != nil {
		t.Fatalf("close stderr reader: %v", err)
	}
	return captured
}
