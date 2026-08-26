//go:build integration

package integration

// Real-BAML forward-port E2E for the raw-stream fix + WithSoftFinalParse opt-in
// (canary of the Codex-approved 0.0.48 hotfix). Unlike the hermetic
// orchestrator locks in bamlutils/buildrequest (which inject the parse verdict),
// these exercise the REAL BAML runtime, the REAL generated dynclient adapter,
// and the mock upstream streaming plain prose against a `{answer: string}` class
// schema — so BAML's ParseStream/Parse return the authentic root-coercion
// verdict for every prefix, exactly the customer's reported failure.
//
// Gated //go:build integration (needs the mock-LLM TestEnv), so it never runs in
// the hermetic de-BAML unit-test suite.

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/invakid404/baml-rest/dynclient"
)

// proseContent is plain prose streamed against a `{answer: string}` class
// schema. BAML's Parse/ParseStream cannot coerce a bare string into the object,
// so the final parse misses with the stable customer error.
const proseContent = "Here is plain prose, not structured JSON."

// TestDynclientRawStreamProsePartials_RealRuntime is the faithful E2E: live raw
// partials must flow while the structured parse fails for every prefix, and the
// WithSoftFinalParse opt-in must convert the terminal final-parse miss into a
// successful raw-only final.
func TestDynclientRawStreamProsePartials_RealRuntime(t *testing.T) {
	dynclientCallGate(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	opts := setupScenario(t, "test-dynclient-rawstream-prose", proseContent)
	_, libSchema := simpleAnswerSchema()
	hello := "Give me the answer."

	newReq := func() dynclient.Request {
		return dynclient.Request{
			Messages:       []dynclient.Message{{Role: "user", TextContent: &hello}},
			ClientRegistry: dynRegistry(opts.ClientRegistry),
			OutputSchema:   libSchema,
		}
	}

	t.Run("live_raw_partials_even_when_final_parse_fails", func(t *testing.T) {
		client := newDynclient(t) // default: strict final parse
		stream, err := client.DynamicStreamRaw(ctx, newReq())
		if err != nil {
			t.Fatalf("DynamicStreamRaw open: %v", err)
		}
		defer stream.Close()

		var (
			rawSeen  []string
			finals   int
			finalRaw string
			termErr  error
		)
		for {
			ev, err := stream.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				termErr = err
				break
			}
			switch ev.Kind {
			case dynclient.EventPartial:
				if ev.Raw != "" {
					rawSeen = append(rawSeen, ev.Raw)
				}
			case dynclient.EventFinal:
				finals++
				finalRaw = ev.Raw
			}
		}

		// Fix (1): live raw partials flow even though every prefix fails to parse.
		if len(rawSeen) == 0 {
			t.Errorf("expected live raw partials as the prose streams, got none " +
				"(regression: raw suppressed by structured-parse gating)")
		}
		// Default strict: no successful final, and the terminal error is the
		// stable customer coerce error (assert the stable wrapper + payload, not
		// an over-specific string).
		if finals != 0 {
			t.Errorf("default strict: expected no final frame, got %d (raw=%q)", finals, finalRaw)
		}
		if termErr == nil {
			t.Fatalf("default strict: expected a terminal final-parse error, got clean EOF")
		}
		msg := termErr.Error()
		if !strings.Contains(msg, "failed to parse final result") {
			t.Errorf("terminal error missing stable wrapper %q: %v", "failed to parse final result", msg)
		}
		if !strings.Contains(msg, "Failed to coerce value") {
			t.Errorf("terminal error missing root-coercion payload %q: %v", "Failed to coerce value", msg)
		}
	})

	t.Run("WithSoftFinalParse_yields_successful_raw_final", func(t *testing.T) {
		client := newDynclient(t, dynclient.WithSoftFinalParse())
		stream, err := client.DynamicStreamRaw(ctx, newReq())
		if err != nil {
			t.Fatalf("DynamicStreamRaw open: %v", err)
		}
		defer stream.Close()

		var (
			rawSeen  []string
			finals   int
			finalRaw string
			termErr  error
		)
		for {
			ev, err := stream.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				termErr = err
				break
			}
			switch ev.Kind {
			case dynclient.EventPartial:
				if ev.Raw != "" {
					rawSeen = append(rawSeen, ev.Raw)
				}
			case dynclient.EventFinal:
				finals++
				finalRaw = ev.Raw
			}
		}

		if len(rawSeen) == 0 {
			t.Errorf("expected live raw partials with the opt-in on, got none")
		}
		if termErr != nil {
			t.Fatalf("WithSoftFinalParse: expected clean completion, got terminal error: %v", termErr)
		}
		if finals != 1 {
			t.Fatalf("WithSoftFinalParse: expected exactly 1 successful final, got %d", finals)
		}
		if finalRaw != proseContent {
			t.Errorf("WithSoftFinalParse: final raw = %q, want the full accumulated prose %q", finalRaw, proseContent)
		}
	})
}

// TestDynclientCallRaw_SoftFinalParse_ScopeGuard_RealRuntime pins that the
// opt-in never leaks into the non-streaming DynamicCallRaw bridge
// (NeedsPartials=false): even with WithSoftFinalParse enabled, a prose response
// against a class schema returns the strict terminal parse error.
func TestDynclientCallRaw_SoftFinalParse_ScopeGuard_RealRuntime(t *testing.T) {
	dynclientCallGate(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	opts := setupScenario(t, "test-dynclient-callraw-prose-scope", proseContent)
	_, libSchema := simpleAnswerSchema()
	hello := "Give me the answer."

	client := newDynclient(t, dynclient.WithSoftFinalParse())
	_, err := client.DynamicCallRaw(ctx, dynclient.Request{
		Messages:       []dynclient.Message{{Role: "user", TextContent: &hello}},
		ClientRegistry: dynRegistry(opts.ClientRegistry),
		OutputSchema:   libSchema,
	})
	if err == nil {
		t.Fatalf("DynamicCallRaw with WithSoftFinalParse must stay strict on a prose/class mismatch, got success")
	}
	if msg := err.Error(); !strings.Contains(msg, "Failed to coerce value") {
		t.Errorf("DynamicCallRaw scope guard: expected the strict root-coercion error, got: %v", msg)
	}
}
