//go:build nanollm_integration

package main

import (
	"context"
	"net/http"
	"slices"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/projectdescriptor"
	"github.com/invakid404/baml-rest/workerplugin"
)

// TestNativeOnlyWorker_BootCallParse is the booted-command acceptance proof: the
// REAL ./cmd/worker-nativeonly binary, dispensed over go-plugin, serves the exact
// five-arm JSON alias method with one provider hit and exact canonical bytes, and
// its socket-free /parse route returns the same bytes with the hit count unchanged.
// Health and metrics RPCs prove compatibility with the real pool/plugin bootstrap.
func TestNativeOnlyWorker_BootCallParse(t *testing.T) {
	const modelText = `{"k":1}`
	hits := setProviderHandler(func(w http.ResponseWriter, r *http.Request) {
		okChatCompletion(w, modelText)
	})

	bw := bootWorker(t)
	ctx := context.Background()

	// --- Default-deny at boot: GENERATED then DECLINED --------------------------
	// The fixture declares THREE static methods the codegen classifier admits into the
	// generated candidate set — the exact JSON alias (stamped static_stream), a
	// non-alias (string) return, and a retry-policy client method (both static_unary).
	// Proving both halves distinguishes a generate-then-decline from a candidate that
	// was never generated: the emitted descriptor must carry all three names...
	gen := generatedCandidateNames(t)
	for _, want := range []string{"StaticRecursiveAliasJSON", "NonCohortStringReturn", "RetryPolicyMethod"} {
		if !slices.Contains(gen, want) {
			t.Fatalf("generated candidate %q missing from the emitted descriptor %v; the generate-then-decline proof is vacuous if a candidate was never generated", want, gen)
		}
	}
	// ...with the classes the classifier is supposed to have stamped. Pinning them here
	// keeps the helper above honest: a class filter that silently dropped a served
	// method would otherwise only surface as a confusing count mismatch below.
	//
	// The class is derived from the RETURN SHAPE ALONE — internal/nativespine lowers the
	// Return and asks the one totality predicate — so it says nothing about the client.
	// RetryPolicyMethod therefore carries the STREAM class (its return IS the exact
	// five-arm JSON alias) even though its retry-policy client puts it outside the
	// runtime cohort. Those are two independent axes, and the boot count below is what
	// proves the second one: a stream-CLASS method is still omitted at boot for a
	// client-level reason.
	for method, wantClass := range map[string]projectdescriptor.MethodClass{
		"StaticRecursiveAliasJSON": projectdescriptor.ClassStaticStream,
		"NonCohortStringReturn":    projectdescriptor.ClassStaticUnary,
		"RetryPolicyMethod":        projectdescriptor.ClassStaticStream,
	} {
		if got := generatedCandidateClass(t, method); got != wantClass {
			t.Fatalf("generated candidate %q has class %q, want %q", method, got, wantClass)
		}
	}
	// ...while the booted runtime admits exactly ONE. The other two were GENERATED but
	// declined at admission — a non-alias return (out of the population by SHAPE) and a
	// retry-policy client (out of it by CLIENT, despite carrying the stream class) —
	// then omitted at boot, not merely absent.
	if n := admittedMethodCount(t, bw); n != 1 {
		t.Fatalf("admitted_method_count = %d, want exactly 1 (the two non-cohort candidates must be omitted at boot)", n)
	}

	// --- Call route (one real provider request) --------------------------------
	ch, err := bw.worker.CallStream(ctx, nativeOnlyMethod, callInput("weather"), bamlutils.StreamModeCall)
	if err != nil {
		t.Fatalf("CallStream: %v", err)
	}
	results := drainFinal(t, ch)
	if len(results) != 1 {
		t.Fatalf("want 1 stream result, got %d: %+v", len(results), results)
	}
	if results[0].Error != nil {
		t.Fatalf("unexpected error frame: %v", results[0].Error)
	}
	if got := string(results[0].Data); got != modelText {
		t.Fatalf("call envelope = %s, want canonical %s", got, modelText)
	}
	if hits() != 1 {
		t.Fatalf("provider hit count = %d, want exactly 1 (one send, no retry/fallback)", hits())
	}

	// --- Parse route (zero sockets) --------------------------------------------
	before := hits()
	pres, err := bw.worker.Parse(ctx, nativeOnlyMethod, parseInput(modelText))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := string(pres.Data); got != modelText {
		t.Fatalf("parse result = %s, want %s", got, modelText)
	}
	if hits() != before {
		t.Fatalf("parse route opened a socket: hits %d -> %d", before, hits())
	}

	// --- Health + metrics (pool/plugin compatibility) --------------------------
	if ok, err := bw.worker.Health(ctx); err != nil || !ok {
		t.Fatalf("Health = (%v, %v), want (true, nil)", ok, err)
	}
	metrics, err := bw.worker.GetMetrics(ctx)
	if err != nil {
		t.Fatalf("GetMetrics: %v", err)
	}
	if len(metrics) == 0 {
		t.Fatalf("GetMetrics returned no metric families; the artifact-profile collectors must register")
	}
}

// TestNativeOnlyWorker_ServesUnderPooledSharedStateAndRequestID is the availability
// regression for the round-robin advancer. A pool attaches shared state to the worker
// and supplies a request id on EVERY request, so the worker installs a LIVE round-robin
// advancer on every call. The admitted direct-client method must STILL serve — its
// client is a proven single resolved leaf, not a round-robin strategy — with exactly
// one provider hit. Deriving the round-robin decline from advancer presence rather than
// from the selected client's plan would reject 100% of pooled production traffic.
func TestNativeOnlyWorker_ServesUnderPooledSharedStateAndRequestID(t *testing.T) {
	const modelText = `{"k":1}`
	hits := setProviderHandler(func(w http.ResponseWriter, r *http.Request) {
		okChatCompletion(w, modelText)
	})
	bw := bootWorker(t) // hosts a real shared-state store: the AttachSharedState handshake runs
	// The production-style context: a request id makes the shared-state round-robin
	// advancer live for this call, exactly as pool.CallStream does on every request.
	ctx := workerplugin.WithRequestID(context.Background(), "u1b-pooled-request-id")

	ch, err := bw.worker.CallStream(ctx, nativeOnlyMethod, callInput("weather"), bamlutils.StreamModeCall)
	if err != nil {
		t.Fatalf("CallStream: %v", err)
	}
	results := drainFinal(t, ch)
	if len(results) != 1 {
		t.Fatalf("want 1 stream result, got %d: %+v", len(results), results)
	}
	if results[0].Error != nil {
		t.Fatalf("admitted direct-client method declined under a live advancer + request id (the availability bug): %v", results[0].Error)
	}
	if got := string(results[0].Data); got != modelText {
		t.Fatalf("serve envelope = %s, want canonical %s", got, modelText)
	}
	if hits() != 1 {
		t.Fatalf("provider hit count = %d, want exactly 1 (served under a live advancer, no strategy-gate decline)", hits())
	}
}

// TestNativeOnlyWorker_EverythingElseDeclines boots the same artifact and proves
// every non-cohort request declines with ZERO provider sockets: there is no BAML
// fallback, so a pre-socket decline is a terminal caller-visible error with zero
// provider hits.
func TestNativeOnlyWorker_EverythingElseDeclines(t *testing.T) {
	hits := setProviderHandler(func(w http.ResponseWriter, r *http.Request) {
		// The provider must NEVER be reached on any decline path; if it is, surface
		// it as a 500 so the assertion below (hits==0) fails loudly.
		w.WriteHeader(http.StatusInternalServerError)
	})
	bw := bootWorker(t)
	ctx := context.Background()

	// Call-route declines: (input, streamMode).
	// NOTE (M3e-A): the two REAL stream modes are no longer decline rows — the exact
	// cohort is now stream-capable through this artifact, and their served transcript is
	// the subject of e2e_stream_test.go. /call-with-raw stays declined: it is outside
	// ClassStaticStream's promise, which is a CLOSED mode set, not "everything but call".
	callCases := []struct {
		name  string
		input []byte
		mode  bamlutils.StreamMode
	}{
		{"call_with_raw_mode", callInput("x"), bamlutils.StreamModeCallWithRaw},
		{"caller_client_registry", []byte(`{"topic":"x","__baml_options__":{"client_registry":{"clients":[]}}}`), bamlutils.StreamModeCall},
		{"dynamic_output_schema", []byte(`{"topic":"x","__baml_options__":{"output_schema":{}}}`), bamlutils.StreamModeCall},
		{"retry_override", []byte(`{"topic":"x","__baml_options__":{"retry":{}}}`), bamlutils.StreamModeCall},
	}
	for _, tc := range callCases {
		t.Run(tc.name, func(t *testing.T) {
			before := hits()
			assertCallDeclines(t, bw, ctx, nativeOnlyMethod, tc.input, tc.mode)
			if hits() != before {
				t.Fatalf("decline %q opened a provider socket: hits %d -> %d", tc.name, before, hits())
			}
		})
	}

	// Unknown / dynamic method: neither call nor parse reaches a socket.
	t.Run("unknown_method_call", func(t *testing.T) {
		before := hits()
		assertCallDeclines(t, bw, ctx, "NoSuchMethod", callInput("x"), bamlutils.StreamModeCall)
		if hits() != before {
			t.Fatalf("unknown-method call opened a socket")
		}
	})
	t.Run("dynamic_method_name_call", func(t *testing.T) {
		before := hits()
		assertCallDeclines(t, bw, ctx, "Baml_Rest_Dynamic", callInput("x"), bamlutils.StreamModeCall)
		if hits() != before {
			t.Fatalf("dynamic-method call opened a socket")
		}
	})
	t.Run("unregistered_direct_parse", func(t *testing.T) {
		before := hits()
		if _, err := bw.worker.Parse(ctx, "NoSuchMethod", parseInput(`{"k":1}`)); err == nil {
			t.Fatalf("Parse of an unregistered method must return an error")
		}
		if hits() != before {
			t.Fatalf("unregistered parse opened a socket")
		}
	})

	// A GENERATED candidate OUTSIDE the U1 cohort (a non-JSON-alias return) was
	// omitted at boot by the classifier, so it does not exist to the handler —
	// default-deny at the deletion frontier, not a call-time decline.
	t.Run("generated_candidate_outside_u1_non_alias_return", func(t *testing.T) {
		before := hits()
		assertCallDeclines(t, bw, ctx, "NonCohortStringReturn", callInput("x"), bamlutils.StreamModeCall)
		if hits() != before {
			t.Fatalf("non-cohort candidate call opened a socket")
		}
	})
	// A retry-policy client candidate (M1-generated, U1-declined at admission) was
	// likewise omitted at boot, so it does not exist to the handler.
	t.Run("retry_policy_client_candidate", func(t *testing.T) {
		before := hits()
		assertCallDeclines(t, bw, ctx, "RetryPolicyMethod", callInput("x"), bamlutils.StreamModeCall)
		if hits() != before {
			t.Fatalf("retry-policy candidate call opened a socket")
		}
	})
	// A request cancelled before the native claim reaches no provider.
	t.Run("cancelled_before_claim", func(t *testing.T) {
		before := hits()
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		assertCallDeclines(t, bw, cctx, nativeOnlyMethod, callInput("weather"), bamlutils.StreamModeCall)
		if hits() != before {
			t.Fatalf("cancelled-before-claim opened a provider socket: hits %d -> %d", before, hits())
		}
	})
}

// TestNativeOnlyWorker_RewriteProxyTargetDeclines boots a worker whose base-URL
// rewrite rules would divert the admitted method's send target, and proves the
// admitted call declines PRE-SOCKET (the exact cohort refuses a request whose
// effective target would be rewritten/proxied) — a booted-artifact acceptance case.
func TestNativeOnlyWorker_RewriteProxyTargetDeclines(t *testing.T) {
	hits := setProviderHandler(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError) // must never be reached
	})
	// Rewrite the loopback host so the prepared send URL would be diverted; the
	// admission rewrite/proxy gate declines before any socket.
	bw := bootWorker(t, "BAML_REST_BASE_URL_REWRITES=127.0.0.1=10.255.255.1")
	ctx := context.Background()

	before := hits()
	assertCallDeclines(t, bw, ctx, nativeOnlyMethod, callInput("weather"), bamlutils.StreamModeCall)
	if hits() != before {
		t.Fatalf("rewrite/proxy-target decline opened a provider socket: hits %d -> %d", before, hits())
	}
}

// TestNativeOnlyWorker_PostClaimFailureIsTerminal proves a provider failure AFTER
// the native claim is terminal and produces exactly ONE provider hit — the no-resend
// boundary (there is no BAML fallback to try again).
func TestNativeOnlyWorker_PostClaimFailureIsTerminal(t *testing.T) {
	hits := setProviderHandler(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"boom"}`, http.StatusInternalServerError)
	})
	bw := bootWorker(t)
	ctx := context.Background()

	ch, err := bw.worker.CallStream(ctx, nativeOnlyMethod, callInput("weather"), bamlutils.StreamModeCall)
	if err != nil {
		t.Fatalf("CallStream: %v", err)
	}
	results := drainFinal(t, ch)
	if len(results) != 1 || results[0].Error == nil {
		t.Fatalf("want exactly one terminal error frame, got %+v", results)
	}
	if hits() != 1 {
		t.Fatalf("post-claim provider failure produced %d hits, want exactly 1 (no resend)", hits())
	}
}

// assertCallDeclines drives CallStream and asserts the request did NOT succeed:
// either CallStream returns an error, or the single stream frame is an error frame.
// It never asserts a socket count itself — the caller brackets it with hit checks.
func assertCallDeclines(t *testing.T, bw *bootedWorker, ctx context.Context, method string, input []byte, mode bamlutils.StreamMode) {
	t.Helper()
	ch, err := bw.worker.CallStream(ctx, method, input, mode)
	if err != nil {
		return // declined before a stream even opened
	}
	results := drainFinal(t, ch)
	for _, r := range results {
		if r.Kind == workerplugin.StreamResultKindFinal && r.Error == nil {
			t.Fatalf("expected a decline, got a successful final frame: %s", r.Data)
		}
	}
	if len(results) == 0 {
		t.Fatalf("expected a decline error frame, got no frames")
	}
}
