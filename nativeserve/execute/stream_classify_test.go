//go:build nanollm_integration

package execute

// Deterministic unit coverage for classifyStreamError: every terminal condition
// maps to its bounded phase, the cancellation precedence holds, and the
// underlying cause stays reachable via errors.As. No socket, no timing.

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	nanollm "github.com/viktordanov/nanollm-ffi/go"
)

func TestClassifyStreamError(t *testing.T) {
	// A DoStream-style wrap of the cause (mirrors "all fallback models exhausted: %w").
	wrap := func(err error) error { return fmt.Errorf("nanollm: all fallback models exhausted: %w", err) }

	cases := []struct {
		name      string
		err       error
		readPhase bool
		want      StreamPhase
	}{
		{"provider status (D11)", wrap(&nanollm.ProviderStatusError{Status: 503, Body: []byte("x")}), false, StreamPhaseStatus},
		{"first-body timeout", wrap(llmhttp.ErrExactStreamFirstBodyTimeout), true, StreamPhaseFirstBody},
		{"idle timeout", wrap(llmhttp.ErrExactStreamIdleTimeout), true, StreamPhaseIdle},
		{"content type", wrap(&llmhttp.ExactStreamContentTypeError{MediaType: "application/json"}), false, StreamPhaseProtocol},
		{"plan mismatch", wrap(&llmhttp.ExactStreamPlanMismatchError{Field: "body"}), false, StreamPhaseProtocol},
		{"second round trip", wrap(llmhttp.ErrExactStreamSecondRoundTrip), false, StreamPhaseProtocol},
		{"context canceled", wrap(context.Canceled), false, StreamPhaseCancel},
		{"deadline exceeded", wrap(context.DeadlineExceeded), false, StreamPhaseCancel},
		{"generic read-phase decode", errors.New("truncated frame"), true, StreamPhaseDecode},
		{"generic start-phase connect", errors.New("dial tcp: refused"), false, StreamPhaseConnect},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			te := classifyStreamError(context.Background(), tc.err, tc.readPhase)
			if te.Phase != tc.want {
				t.Errorf("phase = %q, want %q", te.Phase, tc.want)
			}
			// The cause stays reachable for errors.As/Is through TerminalError.Unwrap.
			if !errors.Is(te, tc.err) {
				t.Errorf("TerminalError does not wrap the cause")
			}
		})
	}
}

// TestClassifyStreamErrorCancellationWins proves an ALREADY-observed caller
// cancellation wins over a timeout classification: even a first-body-timeout
// sentinel is reported as cancel when the request context is already done
// (scope §5.10).
func TestClassifyStreamErrorCancellationWins(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	te := classifyStreamError(ctx, llmhttp.ErrExactStreamFirstBodyTimeout, true)
	if te.Phase != StreamPhaseCancel {
		t.Errorf("phase = %q, want cancel (caller cancellation wins)", te.Phase)
	}
	if !errors.Is(te, context.Canceled) {
		t.Errorf("cancel terminal does not wrap context.Canceled")
	}
}
