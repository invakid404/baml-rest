//go:build integration

package integration

import (
	"context"
	stdjson "encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/dynclient"
	"github.com/invakid404/baml-rest/internal/debaml"
)

// De-BAML M4d — native-first parse-stream RUNTIME seam (GitHub #536 / P4 M4d).
//
// M4d wires the native de-BAML stream parser into the generated dynamic
// parseStreamFn: for a de-BAML-enabled request the generated closure calls
// maybeParseDeBAMLStream (native, Stream=true) FIRST and falls back SILENTLY to
// BAML ParseStream on the unsupported sentinel, propagating a claimed native
// error the same way the orchestrator handles a BAML parse-stream error
// (per-prefix parse errors are NON-TERMINAL for partial emission). These
// integration tests exercise that generated seam in the SAME live streaming path
// a real request uses (dynclient DynamicStream over the BuildRequest route),
// proving BOTH native-first (claim) and fallback run there.
//
// The native parser is wrapped in an instrumented callback so the test can
// observe the per-prefix disposition (claim vs unsupported-fallback) the
// generated seam drives, without reaching into generated internals.

// streamSeamCounters tallies the native parser's per-prefix stream dispositions
// observed through the wired callback.
type streamSeamCounters struct {
	streamClaims    atomic.Int64 // native CLAIMED a stream prefix (native-first)
	streamFallbacks atomic.Int64 // native declined a stream prefix (unsupported → BAML)
	streamErrors    atomic.Int64 // native returned a CLAIMED (non-sentinel) stream error — a regression; the test fails on these
	finalCalls      atomic.Int64 // non-stream (final) parse calls, for reference
}

// instrumentedNativeParser wraps the real native de-BAML parser
// (internal/debaml.Parse) and records each STREAM prefix's disposition into c,
// so a test can assert the generated seam actually drove native-first and/or
// fell back. Behaviour is otherwise identical to the bare parser — the counters
// are pure observation, EXCEPT that a claimed (non-sentinel) native stream error
// also FAILS the test (see the default branch).
func instrumentedNativeParser(t *testing.T, c *streamSeamCounters) bamlutils.DeBAMLParseFunc {
	t.Helper()
	return func(ctx context.Context, req bamlutils.DeBAMLParseRequest) (bamlutils.DeBAMLParseResult, error) {
		res, err := debaml.Parse(ctx, req)
		if !req.Stream {
			c.finalCalls.Add(1)
			return res, err
		}
		switch {
		case err == nil:
			c.streamClaims.Add(1)
		case errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported):
			c.streamFallbacks.Add(1)
		default:
			// A CLAIMED native stream error (any non-nil, non-sentinel error). Per-prefix
			// stream parse errors are NON-TERMINAL — the orchestrator drops them — so
			// BAML's final-parse recovery could SILENTLY MASK a native-stream-parser
			// regression on the now-live seam, letting the seam tests still pass. Count
			// it AND fail so a claimed-prefix native error can never be masked.
			//
			// t.Errorf (NOT Fatalf): this callback runs in the orchestrator's stream
			// goroutine, not the test goroutine, so it must not call runtime.Goexit; all
			// invocations complete before drainLibStream returns, so t is still live.
			c.streamErrors.Add(1)
			t.Errorf("native CLAIMED a stream parse error (non-sentinel) on the live parseStreamFn seam — a regression the non-terminal per-prefix path would otherwise mask: err=%v raw=%q outputSchema=%+v", err, req.Raw, req.OutputSchema)
		}
		return res, err
	}
}

// drivenStreamFinal opens a de-BAML-enabled DynamicStream for req, drains it,
// and returns the final flattened payload plus the observed native
// dispositions.
func drivenStreamFinal(t *testing.T, ctx context.Context, req dynclient.Request) (stdjson.RawMessage, *streamSeamCounters) {
	t.Helper()
	c := &streamSeamCounters{}
	on := newDynclient(t,
		dynclient.WithDeBAML(true),
		dynclient.WithDeBAMLRenderer(debaml.Render),
		dynclient.WithDeBAMLParser(instrumentedNativeParser(t, c)),
	)
	stream, err := on.DynamicStream(ctx, req)
	if err != nil {
		t.Fatalf("DynamicStream (de-BAML on): %v", err)
	}
	defer stream.Close()
	final, _ := drainLibStream(t, stream)
	return final, c
}

// TestDeBAMLStream_NativeFirstSeam drives a de-BAML-enabled DynamicStream for a
// schema native CLAIMS on the partial surface (a plain string field), proving
// the generated parseStreamFn calls the native parser first in the live path.
// The final payload must match the BAML-as-today (de-BAML off) stream final —
// stream support must not change what the request produces.
func TestDeBAMLStream_NativeFirstSeam(t *testing.T) {
	debamlGate(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// A long string value so the mock chunks it across several parse prefixes.
	const content = `{"answer":"hello there, this is a reasonably long streamed answer"}`
	opts := setupScenario(t, "test-debaml-stream-native-first", content)
	_, libSchema := simpleAnswerSchema()

	hello := "stream please"
	req := dynclient.Request{
		Messages:       []dynclient.Message{{Role: "user", TextContent: &hello}},
		ClientRegistry: dynRegistry(opts.ClientRegistry),
		OutputSchema:   libSchema,
	}

	// Baseline: de-BAML OFF (BAML-as-today) stream final.
	off := newDynclient(t)
	offStream, err := off.DynamicStream(ctx, req)
	if err != nil {
		t.Fatalf("DynamicStream (de-BAML off): %v", err)
	}
	offFinal, _ := drainLibStream(t, offStream)
	offStream.Close()

	onFinal, c := drivenStreamFinal(t, ctx, req)

	if !jsonEqual(t, offFinal, onFinal) {
		t.Errorf("de-BAML stream final diverged from BAML baseline:\n off=%s\n on =%s", string(offFinal), string(onFinal))
	}
	// The generated seam must have driven native-first at least once (a claimed
	// stream prefix) — otherwise the seam isn't wired into the live path.
	if got := c.streamClaims.Load(); got == 0 {
		t.Errorf("native-first stream seam never CLAIMED a prefix (claims=%d, fallbacks=%d); parseStreamFn is not native-first",
			got, c.streamFallbacks.Load())
	}
	// No CLAIMED native stream error may occur (the instrumented parser also fails
	// on each; this is the explicit end-of-run guard against the non-terminal blind spot).
	if got := c.streamErrors.Load(); got != 0 {
		t.Errorf("native returned %d CLAIMED stream error(s) on the seam (want 0)", got)
	}
	t.Logf("native-first seam: stream claims=%d fallbacks=%d errors=%d final-calls=%d",
		c.streamClaims.Load(), c.streamFallbacks.Load(), c.streamErrors.Load(), c.finalCalls.Load())
}

// TestDeBAMLStream_FallbackSeam drives a de-BAML-enabled DynamicStream for a
// schema native DECLINES entirely in stream mode — a direct
// list<multi-arm-union> element, whose cross-element union_variant_hint BAML
// carries but native does not model (intentionally deferred after M3). Native
// declines with the unsupported sentinel on EVERY prefix, so the generated seam
// must fall back SILENTLY to BAML parse-stream, and the final must still match
// the BAML-as-today baseline. This proves fallback runs in the same live
// streaming path used by real requests.
func TestDeBAMLStream_FallbackSeam(t *testing.T) {
	debamlGate(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	const content = `{"items":[1,true,2,false,3]}`
	opts := setupScenario(t, "test-debaml-stream-fallback", content)

	// items: list<int | bool> — a multi-arm union list element, which native
	// declines at the structural cut-line (checkSupported) in BOTH stream and
	// final parse, so BAML stays authoritative throughout.
	libSchema := &dynclient.OutputSchema{
		Properties: dynclientFromMap(map[string]*dynclient.Property{
			"items": {
				Type: "list",
				Items: &dynclient.TypeSpec{
					Type: "union",
					OneOf: []*dynclient.TypeSpec{
						{Type: "int"},
						{Type: "bool"},
					},
				},
			},
		}),
	}

	hello := "stream please"
	req := dynclient.Request{
		Messages:       []dynclient.Message{{Role: "user", TextContent: &hello}},
		ClientRegistry: dynRegistry(opts.ClientRegistry),
		OutputSchema:   libSchema,
	}

	// Baseline: de-BAML OFF stream final.
	off := newDynclient(t)
	offStream, err := off.DynamicStream(ctx, req)
	if err != nil {
		t.Fatalf("DynamicStream (de-BAML off): %v", err)
	}
	offFinal, _ := drainLibStream(t, offStream)
	offStream.Close()

	onFinal, c := drivenStreamFinal(t, ctx, req)

	if !jsonEqual(t, offFinal, onFinal) {
		t.Errorf("de-BAML fallback stream final diverged from BAML baseline:\n off=%s\n on =%s", string(offFinal), string(onFinal))
	}
	// Native must have been consulted and DECLINED every stream prefix (silent
	// BAML fallback), never claiming an unsupported shape.
	if got := c.streamFallbacks.Load(); got == 0 {
		t.Errorf("fallback stream seam never fell back (fallbacks=%d, claims=%d); native was not consulted on the stream path",
			got, c.streamClaims.Load())
	}
	if got := c.streamClaims.Load(); got != 0 {
		t.Errorf("native must NOT claim a list<multi-arm-union> stream prefix (over-claim), got claims=%d", got)
	}
	// A declining schema must still never produce a CLAIMED native stream error
	// (it declines with the unsupported sentinel, counted as a fallback above).
	if got := c.streamErrors.Load(); got != 0 {
		t.Errorf("native returned %d CLAIMED stream error(s) on the seam (want 0)", got)
	}
	t.Logf("fallback seam: stream claims=%d fallbacks=%d errors=%d final-calls=%d",
		c.streamClaims.Load(), c.streamFallbacks.Load(), c.streamErrors.Load(), c.finalCalls.Load())
}
