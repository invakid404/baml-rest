//go:build integration

package integration

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestCheckSingleCleanFinalEmptyPayload is the ad-hoc sanity check for the
// non-empty-final-payload guard in checkSingleCleanFinal. It isolates the
// helper from the live streaming legs and drives two synthetic leg-results
// directly:
//
//   - a single terminal final whose Data is empty must FAIL (the
//     vacuous-pass hole: diffAndRecord skips len==0 sides, so an empty
//     final would otherwise clear every equivalence assertion);
//   - a canonical single terminal final whose Data is non-empty must PASS.
//
// Mirrors the terminality reasoning of the broader assertion 5 without
// needing the mock-LLM subprocess or network.
func TestCheckSingleCleanFinalEmptyPayload(t *testing.T) {
	t.Run("empty final fails", func(t *testing.T) {
		var failures []string
		kinds := []string{"partial", "final"}
		sawReset := checkSingleCleanFinal("synthetic", kinds, json.RawMessage(nil), &failures)
		if sawReset {
			t.Fatalf("did not expect a reset, got sawReset=true")
		}
		if len(failures) == 0 {
			t.Fatalf("expected a failure for an empty final payload, got none (kinds=%v)", kinds)
		}
		var sawEmpty bool
		for _, f := range failures {
			if strings.Contains(f, "empty Data") {
				sawEmpty = true
			}
		}
		if !sawEmpty {
			t.Fatalf("expected an empty-Data failure, got: %v", failures)
		}
	})

	t.Run("canonical non-empty final passes", func(t *testing.T) {
		var failures []string
		kinds := []string{"partial", "final"}
		sawReset := checkSingleCleanFinal("synthetic", kinds, json.RawMessage(`{"k":"v"}`), &failures)
		if sawReset {
			t.Fatalf("did not expect a reset, got sawReset=true")
		}
		if len(failures) != 0 {
			t.Fatalf("expected a clean pass for a non-empty terminal final, got: %v", failures)
		}
	})
}
