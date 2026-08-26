package buildrequest

// Canary forward-port of the Codex-approved 0.0.48 raw-stream hotfix
// (hotfix/raw-stream-partials). These locks are written FAILING-FIRST against
// clean master (the de-BAML codebase) and prove two independent defects + their
// scope guards, driving the REAL RunStreamOrchestration against REAL httptest
// SSE / AWS-event-stream servers streaming the exact customer prose deltas. Only
// parseStream / parseFinal are injected — they stand in for the pinned BAML
// runtime's root-coercion verdict on a `{value: string}` class schema fed plain
// prose (runtime-originated, not the orchestrator under repair).
//
// Defect (1) raw-decouple: live raw partials must flow for a NeedsRaw stream
// regardless of whether ParseStream succeeds, errors, returns nil, OR the tick
// is throttle-skipped. On clean master the ordinary-text arm emits raw ONLY
// inside `if parseErr == nil && parsed != nil`, so a prose-before-a-class-object
// stream hides every live partial.
//
// Defect (2) soft-final (opt-in, default OFF): with SoftFinalParse enabled on a
// streaming raw-wanted call, a final structured-parse miss completes
// successfully carrying the accumulated raw text instead of hard-failing —
// scoped to STREAMING (NeedsPartials && NeedsRaw), cancellation-safe, and never
// applied to the non-streaming CallWithRaw bridge.

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
)

// customerProseDeltas are the three SSE content deltas from the customer bundle
// (ISSUE.md): plain prose streamed against a `{value: string}` class schema, so
// BAML's ParseStream returns a root-coercion error for every prefix.
var customerProseDeltas = []string{"Here is ", "plain prose, ", "not structured JSON."}

// customerCoerceError mirrors the stable BAML root-coercion verdict the pinned
// runtime returns for prose against a class schema. parseStream / parseFinal
// return it so these hermetic locks exercise the same failure the real E2E
// exercises, without linking the CFFI.
func customerCoerceError() error {
	return errors.New("Failed to coerce value: [InferedObject(String(\"Here is plain prose, not structured JSON.\", Complete))]")
}

// rawStreamCounts categorises a drained orchestration output channel.
type rawStreamCounts struct {
	heartbeats int
	partials   int // StreamResultKindStream, non-reset
	rawDeltas  []string
	finals     int
	finalRaw   string
	errors     int
	lastErr    error
}

func drainRawStream(out <-chan bamlutils.StreamResult) rawStreamCounts {
	var c rawStreamCounts
	for r := range out {
		switch r.Kind() {
		case bamlutils.StreamResultKindHeartbeat:
			c.heartbeats++
		case bamlutils.StreamResultKindStream:
			if r.Reset() {
				continue
			}
			c.partials++
			c.rawDeltas = append(c.rawDeltas, r.Raw())
		case bamlutils.StreamResultKindFinal:
			c.finals++
			c.finalRaw = r.Raw()
		case bamlutils.StreamResultKindError:
			c.errors++
			c.lastErr = r.Error()
		}
	}
	return c
}

func proseBuildRequestFn(url string) BuildRequestFunc {
	return func(_ context.Context, _ string) (*llmhttp.Request, error) {
		return &llmhttp.Request{URL: url, Method: "POST", Body: `{}`}, nil
	}
}

// --- Defect (1): raw partials survive ParseStream failure (SSE) --------------

// TestRunStreamOrchestration_RawPartialsSurviveParseFailure_SSE FAILS on clean
// master: a NeedsRaw stream whose ParseStream errors for every prefix emits 0
// live partials (expected 3, one per prose delta).
func TestRunStreamOrchestration_RawPartialsSurviveParseFailure_SSE(t *testing.T) {
	server := makeOpenAIServer(customerProseDeltas)
	defer server.Close()

	client := llmhttp.NewClient(server.Client())
	out := make(chan bamlutils.StreamResult, 100)

	config := &StreamConfig{Provider: "openai", NeedsPartials: true, NeedsRaw: true}

	err := RunStreamOrchestration(
		context.Background(), out, config, client,
		proseBuildRequestFn(server.URL),
		func(context.Context, string) (any, error) { return nil, customerCoerceError() },
		func(context.Context, string) (any, error) { return nil, customerCoerceError() },
		newTestResult,
	)
	if err != nil {
		t.Fatalf("RunStreamOrchestration returned err: %v", err)
	}
	close(out)

	c := drainRawStream(out)
	if c.partials != len(customerProseDeltas) {
		t.Fatalf("expected %d live raw partials (one per prose delta), got %d: %v",
			len(customerProseDeltas), c.partials, c.rawDeltas)
	}
	for i, want := range customerProseDeltas {
		if c.rawDeltas[i] != want {
			t.Errorf("raw partial[%d] = %q, want %q", i, c.rawDeltas[i], want)
		}
	}
}

// TestRunStreamOrchestration_RawPartialsSurviveParseFailure_Bedrock is the
// AWS-event-stream twin: the bedrock child shares the same raw-gating defect.
func TestRunStreamOrchestration_RawPartialsSurviveParseFailure_Bedrock(t *testing.T) {
	pinnedTime := time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)

	var frames bytes.Buffer
	frames.Write(bedrockStreamFrame(t, "messageStart", []byte(`{"role":"assistant"}`)))
	for _, d := range customerProseDeltas {
		frames.Write(bedrockStreamFrame(t, "contentBlockDelta", []byte(`{"delta":{"text":`+jsonQuote(d)+`}}`)))
	}
	frames.Write(bedrockStreamFrame(t, "messageStop", []byte(`{"stopReason":"end_turn"}`)))

	server := newMockBedrockStreamServer(t, frames.Bytes(), nil, nil, nil)
	defer server.Close()

	client := llmhttp.NewClient(server.Client())
	out := make(chan bamlutils.StreamResult, 100)

	config := &StreamConfig{
		Provider:                  "aws-bedrock",
		NeedsPartials:             true,
		NeedsRaw:                  true,
		BuildBedrockStreamRequest: makeBedrockStreamRequestFn(server.URL, pinnedTime),
	}

	err := RunStreamOrchestration(
		context.Background(), out, config, client,
		func(context.Context, string) (*llmhttp.Request, error) {
			return nil, errors.New("buildRequest must not be called for aws-bedrock")
		},
		func(context.Context, string) (any, error) { return nil, customerCoerceError() },
		func(context.Context, string) (any, error) { return nil, customerCoerceError() },
		newTestResult,
	)
	if err != nil {
		t.Fatalf("RunStreamOrchestration returned err: %v", err)
	}
	close(out)

	c := drainRawStream(out)
	if c.partials != len(customerProseDeltas) {
		t.Fatalf("expected %d live raw partials (one per prose delta), got %d: %v",
			len(customerProseDeltas), c.partials, c.rawDeltas)
	}
}

// TestRunStreamOrchestration_RawPartialsSurviveThrottle_SSE FAILS on clean
// master: with ParseThrottleInterval large enough that only the first tick
// parses, the throttle-skipped ticks drop their raw entirely (raw-only emit
// lives inside the throttle gate). Raw must survive a throttle-skip.
func TestRunStreamOrchestration_RawPartialsSurviveThrottle_SSE(t *testing.T) {
	server := makeOpenAIServer(customerProseDeltas)
	defer server.Close()

	client := llmhttp.NewClient(server.Client())
	out := make(chan bamlutils.StreamResult, 100)

	config := &StreamConfig{
		Provider:              "openai",
		NeedsPartials:         true,
		NeedsRaw:              true,
		ParseThrottleInterval: time.Hour,
	}

	err := RunStreamOrchestration(
		context.Background(), out, config, client,
		proseBuildRequestFn(server.URL),
		func(context.Context, string) (any, error) { return nil, customerCoerceError() },
		func(context.Context, string) (any, error) { return nil, customerCoerceError() },
		newTestResult,
	)
	if err != nil {
		t.Fatalf("RunStreamOrchestration returned err: %v", err)
	}
	close(out)

	c := drainRawStream(out)
	if c.partials != len(customerProseDeltas) {
		t.Fatalf("throttle: expected %d raw partials (raw independent of parse cadence), got %d: %v",
			len(customerProseDeltas), c.partials, c.rawDeltas)
	}
}

// TestRunStreamOrchestration_NonRawStrictOnParseFailure_SSE is a scope guard:
// a non-raw stream (StreamModeStream) still emits 0 partials while ParseStream
// fails, and still hard-fails at the final parse. PASSES before AND after —
// the raw-decouple must not leak into non-raw streams.
func TestRunStreamOrchestration_NonRawStrictOnParseFailure_SSE(t *testing.T) {
	server := makeOpenAIServer(customerProseDeltas)
	defer server.Close()

	client := llmhttp.NewClient(server.Client())
	out := make(chan bamlutils.StreamResult, 100)

	config := &StreamConfig{Provider: "openai", NeedsPartials: true, NeedsRaw: false}

	err := RunStreamOrchestration(
		context.Background(), out, config, client,
		proseBuildRequestFn(server.URL),
		func(context.Context, string) (any, error) { return nil, customerCoerceError() },
		func(context.Context, string) (any, error) { return nil, customerCoerceError() },
		newTestResult,
	)
	if err != nil {
		t.Fatalf("RunStreamOrchestration returned err: %v", err)
	}
	close(out)

	c := drainRawStream(out)
	if c.partials != 0 {
		t.Errorf("non-raw stream should emit 0 partials on parse failure, got %d", c.partials)
	}
	if c.errors != 1 {
		t.Errorf("non-raw stream should hard-fail at final parse (1 error), got %d", c.errors)
	}
	if c.finals != 0 {
		t.Errorf("non-raw stream should emit 0 finals on parse failure, got %d", c.finals)
	}
}

// --- Defect (2): opt-in soft-final ------------------------------------------

// TestRunStreamOrchestration_SoftFinalParse_OptIn_ReturnsRawSuccess_SSE FAILS
// on clean master: with SoftFinalParse enabled, a final-parse miss on a
// streaming raw-wanted call must complete SUCCESSFULLY carrying the accumulated
// raw (0 errors, 1 final). Clean master still hard-fails (1 error, 0 finals).
func TestRunStreamOrchestration_SoftFinalParse_OptIn_ReturnsRawSuccess_SSE(t *testing.T) {
	server := makeOpenAIServer(customerProseDeltas)
	defer server.Close()

	client := llmhttp.NewClient(server.Client())
	out := make(chan bamlutils.StreamResult, 100)

	config := &StreamConfig{
		Provider:       "openai",
		NeedsPartials:  true,
		NeedsRaw:       true,
		SoftFinalParse: true,
	}

	err := RunStreamOrchestration(
		context.Background(), out, config, client,
		proseBuildRequestFn(server.URL),
		func(context.Context, string) (any, error) { return nil, customerCoerceError() },
		func(context.Context, string) (any, error) { return nil, customerCoerceError() },
		newTestResult,
	)
	if err != nil {
		t.Fatalf("RunStreamOrchestration returned err: %v", err)
	}
	close(out)

	c := drainRawStream(out)
	if c.errors != 0 {
		t.Errorf("SoftFinalParse ON: expected 0 error results, got %d (%v)", c.errors, c.lastErr)
	}
	if c.finals != 1 {
		t.Fatalf("SoftFinalParse ON: expected 1 successful final, got %d", c.finals)
	}
	if want := strings.Join(customerProseDeltas, ""); c.finalRaw != want {
		t.Errorf("SoftFinalParse ON: final raw = %q, want %q", c.finalRaw, want)
	}
}

// TestRunStreamOrchestration_SoftFinalParse_DefaultStrict_Errors_SSE is the
// default guard: with SoftFinalParse OFF (default), a final-parse miss still
// hard-fails. PASSES before AND after.
func TestRunStreamOrchestration_SoftFinalParse_DefaultStrict_Errors_SSE(t *testing.T) {
	server := makeOpenAIServer(customerProseDeltas)
	defer server.Close()

	client := llmhttp.NewClient(server.Client())
	out := make(chan bamlutils.StreamResult, 100)

	config := &StreamConfig{
		Provider:       "openai",
		NeedsPartials:  true,
		NeedsRaw:       true,
		SoftFinalParse: false,
	}

	err := RunStreamOrchestration(
		context.Background(), out, config, client,
		proseBuildRequestFn(server.URL),
		func(context.Context, string) (any, error) { return nil, customerCoerceError() },
		func(context.Context, string) (any, error) { return nil, customerCoerceError() },
		newTestResult,
	)
	if err != nil {
		t.Fatalf("RunStreamOrchestration returned err: %v", err)
	}
	close(out)

	c := drainRawStream(out)
	if c.errors != 1 {
		t.Errorf("SoftFinalParse OFF: expected 1 error result, got %d", c.errors)
	}
	if c.finals != 0 {
		t.Errorf("SoftFinalParse OFF: expected 0 finals, got %d", c.finals)
	}
}

// TestRunStreamOrchestration_SoftFinalParse_ScopedToStreaming_SSE guards that
// the opt-in never leaks into the non-streaming CallWithRaw bridge
// (NeedsPartials=false, NeedsRaw=true). Even with SoftFinalParse ON, a
// final-parse miss stays strict. PASSES before AND after.
func TestRunStreamOrchestration_SoftFinalParse_ScopedToStreaming_SSE(t *testing.T) {
	server := makeOpenAIServer(customerProseDeltas)
	defer server.Close()

	client := llmhttp.NewClient(server.Client())
	out := make(chan bamlutils.StreamResult, 100)

	// StreamModeCallWithRaw shape: NeedsRaw=true, NeedsPartials=false.
	config := &StreamConfig{
		Provider:       "openai",
		NeedsPartials:  false,
		NeedsRaw:       true,
		SoftFinalParse: true,
	}

	err := RunStreamOrchestration(
		context.Background(), out, config, client,
		proseBuildRequestFn(server.URL),
		nil, // call-with-raw does not parse partials
		func(context.Context, string) (any, error) { return nil, customerCoerceError() },
		newTestResult,
	)
	if err != nil {
		t.Fatalf("RunStreamOrchestration returned err: %v", err)
	}
	close(out)

	c := drainRawStream(out)
	if c.errors != 1 {
		t.Errorf("SoftFinalParse scoped-to-streaming: CallWithRaw must stay strict (1 error), got %d", c.errors)
	}
	if c.finals != 0 {
		t.Errorf("SoftFinalParse scoped-to-streaming: expected 0 finals, got %d", c.finals)
	}
}

// TestRunStreamOrchestration_SoftFinalParse_HonorsCancellation_SSE guards that
// the opt-in never turns a cancellation/deadline into a successful raw final:
// parseFinal returning context.Canceled must fall through to the strict path.
// PASSES before AND after; catches a soft-final that swallows cancellation.
func TestRunStreamOrchestration_SoftFinalParse_HonorsCancellation_SSE(t *testing.T) {
	server := makeOpenAIServer(customerProseDeltas)
	defer server.Close()

	client := llmhttp.NewClient(server.Client())
	out := make(chan bamlutils.StreamResult, 100)

	config := &StreamConfig{
		Provider:       "openai",
		NeedsPartials:  true,
		NeedsRaw:       true,
		SoftFinalParse: true,
	}

	err := RunStreamOrchestration(
		context.Background(), out, config, client,
		proseBuildRequestFn(server.URL),
		func(context.Context, string) (any, error) { return nil, customerCoerceError() },
		// parseFinal reports a cancellation, but the request context is still
		// live (ctx.Err()==nil) — the soft branch must reject it on the
		// errors.Is(parseErr, context.Canceled) clause, not on ctx.Err().
		func(context.Context, string) (any, error) { return nil, context.Canceled },
		newTestResult,
	)
	if err != nil {
		t.Fatalf("RunStreamOrchestration returned err: %v", err)
	}
	close(out)

	c := drainRawStream(out)
	if c.finals != 0 {
		t.Errorf("cancellation must NOT become a raw-success final; got %d finals (raw=%q)", c.finals, c.finalRaw)
	}
	if c.errors != 1 {
		t.Errorf("cancellation should surface a strict error result, got %d", c.errors)
	}
}

// jsonQuote returns the JSON string literal for s (including surrounding
// quotes), used to embed a prose delta into a Bedrock contentBlockDelta
// fixture payload.
func jsonQuote(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
