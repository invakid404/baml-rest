//go:build nanollm_integration

package canary

// Gated (real FFI + one loopback SSE socket) proof of the de-BAML Phase 3b static STREAM
// SERVE ownership contract through the PUBLIC ServeStaticStream entrypoint:
//
//   - EXACT ONE-SEND + NO-FALLBACK: a CLAIMED static stream drives EXACTLY one provider
//     DoStream RoundTrip (one accepted connection), emits its normalized deltas through
//     the orchestrator's EmitDelta, completes, and NEVER resends/falls back.
//   - TRI-STATE PRE-CLAIM decline: a pre-claim decline sends ZERO native sockets and
//     returns NativeStreamDeclined so BAML serves.
//
// The claim is SYNTHETIC (admission.AdmitStaticStreamClaimForTest, injected through the
// test-only StaticStreamServer.admitStaticStreamClaim seam) so the post-claim transport is
// exercised without a live BAML StreamRequest plan oracle.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/internal/schema"
	"github.com/invakid404/baml-rest/nativeserve/admission"
)

const staticStreamFenceAPIKey = "sk-static-stream-fence-not-a-real-secret"

func writeStaticStreamSSE(w http.ResponseWriter, data string) {
	fmt.Fprintf(w, "data: %s\n\n", data)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// staticStreamLoopback builds a StaticStreamServer whose synthetic claim targets a
// loopback SSE server that returns a couple of OpenAI streaming content deltas + [DONE],
// counting the accepted requests so the one-send contract is byte-observable.
func staticStreamLoopback(t *testing.T, hits *atomic.Int64) (*StaticStreamServer, *schema.Bundle, *httptest.Server) {
	t.Helper()
	cs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		writeStaticStreamSSE(w, `{"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`)
		writeStaticStreamSSE(w, `{"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"[1"},"finish_reason":null}]}`)
		writeStaticStreamSSE(w, `{"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":",2]"},"finish_reason":"stop"}]}`)
		writeStaticStreamSSE(w, "[DONE]")
	}))
	t.Cleanup(cs.Close)

	s := NewStaticStreamServer(0, 0)
	bundle := staticAnswerBundle(t)
	body := []byte(`{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}`)
	claim, err := admission.AdmitStaticStreamClaimForTest(cs.URL+"/v1", staticStreamFenceAPIKey, "__static_stream_alias__", "gpt-4o-mini", bundle, body)
	if err != nil {
		t.Fatalf("AdmitStaticStreamClaimForTest: %v", err)
	}
	// Close is idempotent + nil-safe: this covers the tests that rebind admission (e.g.
	// TestServeStaticStream_DeclineNoSend) and therefore never hand this synthetic claim to
	// ServeStaticStream — which would otherwise release its request-scoped FFI engine via the
	// serve path's `defer claim.Close()`.
	t.Cleanup(claim.Close)
	s.admitStaticStreamClaim = func(context.Context, admission.StaticStreamInput) (*admission.StaticStreamClaim, error) {
		return claim, nil
	}
	return s, bundle, cs
}

// TestServeStaticStream_OneSend_NoFallback proves a claimed static stream drives exactly
// ONE provider RoundTrip, emits its deltas, completes, and never resends.
func TestServeStaticStream_OneSend_NoFallback(t *testing.T) {
	var hits atomic.Int64
	s, _, _ := staticStreamLoopback(t, &hits)

	var parseable strings.Builder
	var deltas atomic.Int64
	inv := bamlutils.NativeStaticStreamInvocation{
		Method:   "StaticOutputFormat",
		Provider: "openai",
		Mode:     bamlutils.NativeStreamModeStream,
		EmitDelta: func(d bamlutils.NativeStreamDelta) error {
			parseable.WriteString(d.ParseableDelta)
			deltas.Add(1)
			return nil
		},
	}
	out := s.ServeStaticStream(context.Background(), inv)

	if out.Disposition != bamlutils.NativeStreamCompleted {
		t.Fatalf("disposition = %v (err=%v), want NativeStreamCompleted", out.Disposition, out.Err)
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("provider RoundTrips = %d, want EXACTLY 1 (one-send / no-fallback)", got)
	}
	if got := parseable.String(); got != "[1,2]" {
		t.Errorf("accumulated parseable deltas = %q, want %q", got, "[1,2]")
	}
	if deltas.Load() == 0 {
		t.Error("no deltas emitted through EmitDelta")
	}
	if out.WinnerEngine != bamlutils.NativeServeEngineNative {
		t.Errorf("winner engine = %q, want %q", out.WinnerEngine, bamlutils.NativeServeEngineNative)
	}
}

// TestServeStaticStream_DeclineNoSend proves a pre-claim decline opens ZERO sockets and
// returns NativeStreamDeclined so BAML serves.
func TestServeStaticStream_DeclineNoSend(t *testing.T) {
	var hits atomic.Int64
	s, _, _ := staticStreamLoopback(t, &hits)
	// Rebind admission to force a pre-claim return-shape decline (what a stream OUTSIDE the
	// admitted alias family gets), so the no-socket decline path is exercised.
	s.admitStaticStreamClaim = func(context.Context, admission.StaticStreamInput) (*admission.StaticStreamClaim, error) {
		return nil, &admission.StaticDecline{Stage: "prompt", Reason: "return_shape_decoder_unproven"}
	}

	sawEmit := false
	inv := bamlutils.NativeStaticStreamInvocation{
		Method:    "StaticOutputFormat",
		Provider:  "openai",
		Mode:      bamlutils.NativeStreamModeStream,
		EmitDelta: func(bamlutils.NativeStreamDelta) error { sawEmit = true; return nil },
	}
	out := s.ServeStaticStream(context.Background(), inv)

	if out.Disposition != bamlutils.NativeStreamDeclined {
		t.Fatalf("disposition = %v, want NativeStreamDeclined", out.Disposition)
	}
	if got := hits.Load(); got != 0 {
		t.Errorf("provider RoundTrips = %d, want ZERO on a pre-claim decline", got)
	}
	if sawEmit {
		t.Error("EmitDelta fired on a declined stream (must be zero deltas pre-claim)")
	}
	if out.Reason != "return_shape_decoder_unproven" {
		t.Errorf("decline reason = %q, want the return-shape token", out.Reason)
	}
}

// TestServeStaticStream_NilEmitDeltaDeclinesPreClaim proves a mis-wired invocation with a nil
// EmitDelta DECLINES pre-claim (zero provider sockets) instead of entering the executor and
// dying as a post-claim FailedAfterClaim that would burn one provider request.
func TestServeStaticStream_NilEmitDeltaDeclinesPreClaim(t *testing.T) {
	var hits atomic.Int64
	s, _, _ := staticStreamLoopback(t, &hits)

	inv := bamlutils.NativeStaticStreamInvocation{
		Method:    "StaticOutputFormat",
		Provider:  "openai",
		Mode:      bamlutils.NativeStreamModeStream,
		EmitDelta: nil, // the mandatory owned-delta sink is missing
	}
	out := s.ServeStaticStream(context.Background(), inv)

	if out.Disposition != bamlutils.NativeStreamDeclined {
		t.Fatalf("disposition = %v (err=%v), want NativeStreamDeclined (pre-claim, no socket)", out.Disposition, out.Err)
	}
	if got := hits.Load(); got != 0 {
		t.Errorf("provider RoundTrips = %d, want ZERO — a nil EmitDelta must NOT burn a provider request", got)
	}
	if out.Reason != "nil_emit_delta" {
		t.Errorf("decline reason = %q, want %q", out.Reason, "nil_emit_delta")
	}
}
