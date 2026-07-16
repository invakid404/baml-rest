//go:build nanollm_integration

package admission

// Request-scoped nanollm engine lifecycle benchmark. It measures the cost the
// admission path pays per request when it builds a one-openai-client engine from
// the request's dynamic secrets and Closes it — the input to the cache decision
// (whether a bounded, close-aware cache is warranted, never an unbounded
// secret-keyed one). All values are the fake fence trio; nanollm.New validates
// and constructs the engine WITHOUT any socket, so the benchmark performs zero
// network I/O.

import (
	"context"
	"testing"

	nanollm "github.com/viktordanov/nanollm-ffi/go"
)

// fenceConfig is the exact request-scoped config the mapper builds: one openai
// model under the internal alias, zero retries, no fallbacks, and no ambient env.
func fenceConfig() nanollm.Config {
	return nanollm.Config{
		Models: []nanollm.ModelConfig{{
			Name:       fenceAlias,
			Model:      "openai/" + fenceModel,
			APIKey:     fenceAPIKey,
			BaseURL:    fenceBaseURL,
			MaxRetries: 0,
		}},
		Env:           nil,
		UseProcessEnv: false,
	}
}

// BenchmarkNanollmNewClose isolates the New+Close pair — the lifecycle a cache
// would amortize.
func BenchmarkNanollmNewClose(b *testing.B) {
	cfg := fenceConfig()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c, err := nanollm.New(cfg)
		if err != nil {
			b.Fatalf("nanollm.New: %v", err)
		}
		if err := c.Close(); err != nil {
			b.Fatalf("Close: %v", err)
		}
	}
}

// BenchmarkAdmit measures the whole no-send admission (map+New, render, body,
// Prepare, revalidate, Close) so the New/Close share of a full request-scoped
// admission is visible, not just the isolated pair.
func BenchmarkAdmit(b *testing.B) {
	a := NewAdmitter(nil, nil)
	ctx := context.Background()
	// Build the fixture ONCE, before timing, so the benchmark measures the
	// admission path (map+New, render, body, Prepare, revalidate, Close) and not
	// the per-iteration cost of constructing validInput(). Admit is read-only on
	// its Input, so a single value is safe to reuse across iterations.
	in := validInput()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := a.Admit(ctx, in); err != nil {
			b.Fatalf("Admit: %v", err)
		}
	}
}
