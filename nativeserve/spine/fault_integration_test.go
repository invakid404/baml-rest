//go:build nanollm_integration

package spine_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/bamlutils/urlrewrite"
	"github.com/invakid404/baml-rest/internal/nativespinejsonfixture"
	"github.com/invakid404/baml-rest/nativeserve/spine"
)

// execFor builds a spine executor over the JSON-alias descriptor at baseURL with the
// given binding (the emitted one, or a custom fault-injecting one).
func execFor(t *testing.T, baseURL string, binding bamlutils.NativeSpineUnaryBinding) *spine.UnaryExecutor {
	t.Helper()
	return newJSONExec(t, baseURL, nil, binding)
}

// callThroughComposite drives one Call through a fallback composite so the same
// assertion covers BOTH "the result is FailedAfterClaim" and "the fallback/oracle was
// NOT invoked after the claim".
func callThroughComposite(ctx context.Context, e *spine.UnaryExecutor) (bamlutils.NativeSpineUnaryResult, int) {
	comp := &fallbackComposite{inner: e, fallbackFinal: "must-not-be-served"}
	res := comp.Call(ctx, jsonAliasMethod, &nativespinejsonfixture.StaticRecursiveAliasJsonInput{Topic: "weather"})
	return res, comp.fallbackCalls
}

// TestNoFallbackAfterClaim_FaultMatrix injects each post-claim fault and asserts:
// disposition is FailedAfterClaim, the outer fallback spy is invoked ZERO times, and
// (for a socket that was entered) the provider saw exactly one request.
func TestNoFallbackAfterClaim_FaultMatrix(t *testing.T) {
	emitted := nativespinejsonfixture.Binding()

	// A binding whose decoder always errors (emitted carrier decode failure).
	decodeErr := emitted
	decodeErr.DecodeFinal = func([]byte) (any, error) { return nil, errors.New("forced decode error") }
	// A binding whose decoder panics (post-claim panic).
	decodePanic := emitted
	decodePanic.DecodeFinal = func([]byte) (any, error) { panic("forced decode panic") }

	type row struct {
		name    string
		binding bamlutils.NativeSpineUnaryBinding
		handler func(w http.ResponseWriter, r *http.Request)
	}
	rows := []row{
		{"provider_non_2xx", emitted, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"boom"}`))
		}},
		{"malformed_2xx_body", emitted, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`this is not json`))
		}},
		{"translate_extract_error", emitted, func(w http.ResponseWriter, r *http.Request) {
			// Valid JSON, but not an OpenAI chat completion (no choices/message).
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"unexpected":"shape"}`))
		}},
		{"invalid_structured_output", emitted, func(w http.ResponseWriter, r *http.Request) {
			// Unquoted multi-token prose has no cleanly-claimable JSON candidate, so the
			// native final parser reaches a terminal parse outcome (never a fallback).
			okChatCompletion(w, `hello world foo`)
		}},
		{"carrier_decode_error", decodeErr, func(w http.ResponseWriter, r *http.Request) {
			okChatCompletion(w, `{"k":1}`)
		}},
		{"post_claim_panic", decodePanic, func(w http.ResponseWriter, r *http.Request) {
			okChatCompletion(w, `{"k":1}`)
		}},
	}

	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			lb := newLoopback(t, tc.handler)
			e := execFor(t, lb.baseURL(), tc.binding)
			res, fallbacks := callThroughComposite(context.Background(), e)

			if res.Disposition != bamlutils.NativeSpineFailedAfterClaim {
				t.Fatalf("disposition = %v (reason %q, err %v), want failed_after_claim", res.Disposition, res.Reason, res.Err)
			}
			if fallbacks != 0 {
				t.Fatalf("fallback invoked %d times after the claim, want 0 (no fallback after claim)", fallbacks)
			}
			if lb.count() != 1 {
				t.Fatalf("provider request count = %d, want exactly 1 (the socket was entered)", lb.count())
			}
			if snap := e.Metrics().Snapshot(); snap.Claims != 1 || snap.Sockets != 1 || snap.Failures != 1 || snap.Successes != 0 || snap.Declines != 0 {
				t.Fatalf("metrics = %+v, want exactly one claim/socket/failure", snap)
			}
		})
	}
}

// TestCancelBeforeClaimDeclines proves cancellation BEFORE the claim declines with
// zero sockets (fallback-legal), while cancellation AFTER the claim fails terminally
// with no fallback and exactly one entered socket.
func TestCancelBeforeAfterClaim(t *testing.T) {
	// --- cancel before claim: pre-socket decline, zero sockets ------------------
	t.Run("cancel_before_claim", func(t *testing.T) {
		lb := newLoopback(t, func(w http.ResponseWriter, r *http.Request) { okChatCompletion(w, `{"k":1}`) })
		e := execFor(t, lb.baseURL(), nativespinejsonfixture.Binding())
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // already cancelled

		// The INNER executor declines pre-socket (the composite masks that to a fallback
		// success, which is exactly the fallback-legal behaviour we assert next).
		inner := e.Call(ctx, jsonAliasMethod, &nativespinejsonfixture.StaticRecursiveAliasJsonInput{Topic: "weather"})
		if inner.Disposition != bamlutils.NativeSpineDeclinedPreSocket {
			t.Fatalf("inner disposition = %v, want declined_pre_socket", inner.Disposition)
		}
		if lb.count() != 0 {
			t.Fatalf("provider request count = %d, want 0 (no socket before claim)", lb.count())
		}
		if snap := e.Metrics().Snapshot(); snap.Sockets != 0 || snap.Claims != 0 || snap.Declines != 1 {
			t.Fatalf("metrics = %+v, want zero sockets/claims and one decline", snap)
		}

		// The pre-socket decline is fallback-legal: the outer composite serves the
		// fallback exactly once (a fresh executor to keep the metric counts clean).
		e2 := execFor(t, lb.baseURL(), nativespinejsonfixture.Binding())
		res, fallbacks := callThroughComposite(ctx, e2)
		if res.Disposition != bamlutils.NativeSpineSucceeded || fallbacks != 1 {
			t.Fatalf("composite over a pre-socket decline: disposition=%v fallbacks=%d, want succeeded/1", res.Disposition, fallbacks)
		}
	})

	// --- cancel after claim: terminal failure, no fallback, one entered socket ---
	t.Run("cancel_after_claim", func(t *testing.T) {
		release := make(chan struct{})
		entered := make(chan struct{}, 1)
		lb := newLoopback(t, func(w http.ResponseWriter, r *http.Request) {
			// Signal that the socket was actually ENTERED (the handler was reached). A
			// buffered non-blocking send is safe even if the handler somehow ran twice
			// (the spine sends exactly once, which the count()==1 assertion also checks).
			select {
			case entered <- struct{}{}:
			default:
			}
			<-release // block until the caller cancels
			okChatCompletion(w, `{"k":1}`)
		})
		e := execFor(t, lb.baseURL(), nativespinejsonfixture.Binding())
		ctx, cancel := context.WithCancel(context.Background())

		done := make(chan struct{ res bamlutils.NativeSpineUnaryResult }, 1)
		go func() {
			comp := &fallbackComposite{inner: e, fallbackFinal: "must-not-be-served"}
			r := comp.Call(ctx, jsonAliasMethod, &nativespinejsonfixture.StaticRecursiveAliasJsonInput{Topic: "weather"})
			if comp.fallbackCalls != 0 {
				t.Errorf("fallback invoked %d times after the claim, want 0", comp.fallbackCalls)
			}
			done <- struct{ res bamlutils.NativeSpineUnaryResult }{r}
		}()

		// Wait until the request has actually ENTERED the socket (handler reached), then
		// cancel while it is in flight — deterministic, not a fixed sleep that can race a
		// loaded runner into a pre-claim cancel (CodeRabbit #7).
		<-entered
		cancel()
		close(release)

		got := <-done
		if got.res.Disposition != bamlutils.NativeSpineFailedAfterClaim {
			t.Fatalf("disposition = %v (err %v), want failed_after_claim", got.res.Disposition, got.res.Err)
		}
		if lb.count() != 1 {
			t.Fatalf("provider request count = %d, want exactly 1 (socket entered)", lb.count())
		}
		if snap := e.Metrics().Snapshot(); snap.Claims != 1 || snap.Sockets != 1 || snap.Failures != 1 {
			t.Fatalf("metrics = %+v, want one claim/socket/failure", snap)
		}
	})
}

// TestConnectionFailureAfterClaim points the descriptor at a dead endpoint: the claim
// succeeds, the socket is attempted, the transport fails, and the result is a terminal
// failed-after-claim with no fallback.
func TestConnectionFailureAfterClaim(t *testing.T) {
	// Stand a loopback up and immediately close it to get a refused endpoint URL.
	lb := newLoopback(t, func(w http.ResponseWriter, r *http.Request) {})
	dead := lb.baseURL()
	lb.srv.Close()

	e := execFor(t, dead, nativespinejsonfixture.Binding())
	res, fallbacks := callThroughComposite(context.Background(), e)
	if res.Disposition != bamlutils.NativeSpineFailedAfterClaim {
		t.Fatalf("disposition = %v (err %v), want failed_after_claim", res.Disposition, res.Err)
	}
	if fallbacks != 0 {
		t.Fatalf("fallback invoked %d times after the claim, want 0", fallbacks)
	}
	if snap := e.Metrics().Snapshot(); snap.Claims != 1 || snap.Sockets != 1 || snap.Failures != 1 {
		t.Fatalf("metrics = %+v, want one claim/socket/failure (socket attempted)", snap)
	}
}

// TestRewriteProxyDeclinesPreClaim proves the finding-1 rewrite/proxy gate: an
// adapter whose HTTP client would rewrite the outbound URL declines PRE-CLAIM (the
// check the omitted BAML plan-compare used to own), opening zero sockets. The check
// runs against the prepared URL, so nanollm Prepare has run but no socket opened.
func TestRewriteProxyDeclinesPreClaim(t *testing.T) {
	lb := newLoopback(t, func(w http.ResponseWriter, r *http.Request) { okChatCompletion(w, `{"k":1}`) })
	e := execFor(t, lb.baseURL(), nativespinejsonfixture.Binding())

	ad := newTestAdapter()
	// Any non-empty rewrite rule makes WouldRewriteOrProxy report true.
	ad.httpClient = llmhttp.NewClientWithOptions(llmhttp.ClientOptions{
		RewriteRules: []urlrewrite.Rule{{From: "https://upstream.example/", To: "http://elsewhere.local/"}},
	})
	res := e.Call(ad, jsonAliasMethod, &nativespinejsonfixture.StaticRecursiveAliasJsonInput{Topic: "weather"})
	if res.Disposition != bamlutils.NativeSpineDeclinedPreSocket {
		t.Fatalf("disposition = %v (reason %q), want declined_pre_socket", res.Disposition, res.Reason)
	}
	if lb.count() != 0 {
		t.Fatalf("provider request count = %d, want 0 (rewrite/proxy declines pre-socket)", lb.count())
	}
	if snap := e.Metrics().Snapshot(); snap.Sockets != 0 || snap.Claims != 0 || snap.Failures != 0 {
		t.Fatalf("metrics = %+v, want zero sockets/claims/failures", snap)
	}
}

// TestGlobalRewriteProxyDeclinesPlainContext proves the rewrite/proxy gate runs even on a
// PLAIN-context call with NO adapter-configured client: a GLOBAL rewrite/proxy rule on
// llmhttp.DefaultClient still declines pre-socket, so a global rule can never silently
// route a native send elsewhere. Before CodeRabbit #9, staticInput left
// WouldRewriteOrProxy nil when ad==nil, so AdmitStaticSpineClaim SKIPPED the gate.
func TestGlobalRewriteProxyDeclinesPlainContext(t *testing.T) {
	lb := newLoopback(t, func(w http.ResponseWriter, r *http.Request) { okChatCompletion(w, `{"k":1}`) })
	e := execFor(t, lb.baseURL(), nativespinejsonfixture.Binding())

	// Install a global rewrite rule on the process-default client (restored after).
	orig := llmhttp.DefaultClient
	t.Cleanup(func() { llmhttp.DefaultClient = orig })
	llmhttp.DefaultClient = llmhttp.NewClientWithOptions(llmhttp.ClientOptions{
		RewriteRules: []urlrewrite.Rule{{From: "https://upstream.example/", To: "http://elsewhere.local/"}},
	})

	// PLAIN context.Background() — no adapter, so the gate falls back to DefaultClient.
	res := e.Call(context.Background(), jsonAliasMethod, &nativespinejsonfixture.StaticRecursiveAliasJsonInput{Topic: "weather"})
	if res.Disposition != bamlutils.NativeSpineDeclinedPreSocket {
		t.Fatalf("disposition = %v (reason %q), want declined_pre_socket", res.Disposition, res.Reason)
	}
	if lb.count() != 0 {
		t.Fatalf("provider request count = %d, want 0 (a global rewrite/proxy rule must decline pre-socket)", lb.count())
	}
	if snap := e.Metrics().Snapshot(); snap.Sockets != 0 || snap.Claims != 0 {
		t.Fatalf("metrics = %+v, want zero sockets/claims", snap)
	}
}
