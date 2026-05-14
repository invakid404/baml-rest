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
	for _, want := range wantSubstrings {
		if !strings.Contains(captured, want) {
			t.Errorf("stderr missing %q\n--- captured stderr ---\n%s", want, captured)
		}
	}
}

// captureStderr redirects os.Stderr to a pipe for the duration of fn and
// returns whatever fn wrote.
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
	w.Close()
	return <-done
}
