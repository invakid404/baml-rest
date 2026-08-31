//go:build nanollm_integration

package spine_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/internal/nativespinejsonfixture"
	"github.com/invakid404/baml-rest/nativeserve/spine"
)

// execFor builds a spine executor over the JSON-alias descriptor at baseURL with the
// given binding (the emitted one, or a custom fault-injecting one).
func execFor(t *testing.T, baseURL string, binding bamlutils.NativeSpineUnaryBinding) *spine.UnaryExecutor {
	t.Helper()
	fn := reconstructJSONAlias(t, baseURL)
	e, err := spine.NewUnaryExecutor([]spine.SpineMethod{{Function: fn, Binding: binding}}, nil)
	if err != nil {
		t.Fatalf("NewUnaryExecutor: %v", err)
	}
	return e
}

// callThroughComposite drives one Call through a fallback composite so the same
// assertion covers BOTH "the result is FailedAfterClaim" and "the fallback/oracle was
// NOT invoked after the claim".
func callThroughComposite(ctx context.Context, e *spine.UnaryExecutor) (bamlutils.NativeSpineUnaryResult, int) {
	comp := &fallbackComposite{inner: e, fallbackFinal: "must-not-be-served"}
	res := comp.Call(ctx, jsonAliasMethodName, &nativespinejsonfixture.StaticRecursiveAliasJsonInput{Topic: "weather"})
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
		inner := e.Call(ctx, jsonAliasMethodName, &nativespinejsonfixture.StaticRecursiveAliasJsonInput{Topic: "weather"})
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
		lb := newLoopback(t, func(w http.ResponseWriter, r *http.Request) {
			<-release // block until the caller cancels
			okChatCompletion(w, `{"k":1}`)
		})
		e := execFor(t, lb.baseURL(), nativespinejsonfixture.Binding())
		ctx, cancel := context.WithCancel(context.Background())

		done := make(chan struct{ res bamlutils.NativeSpineUnaryResult }, 1)
		go func() {
			comp := &fallbackComposite{inner: e, fallbackFinal: "must-not-be-served"}
			r := comp.Call(ctx, jsonAliasMethodName, &nativespinejsonfixture.StaticRecursiveAliasJsonInput{Topic: "weather"})
			if comp.fallbackCalls != 0 {
				t.Errorf("fallback invoked %d times after the claim, want 0", comp.fallbackCalls)
			}
			done <- struct{ res bamlutils.NativeSpineUnaryResult }{r}
		}()

		// Give the request time to enter the socket, then cancel while it is in flight.
		time.Sleep(150 * time.Millisecond)
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

// TestOneSendExactTransport proves an admitted call opens EXACTLY one provider socket
// and the wire request carries the literal test model/key/base URL and no unapproved
// body field — the exact-transport contract, over the reused nanollm exact plan.
func TestOneSendExactTransport(t *testing.T) {
	type captured struct {
		method string
		path   string
		auth   string
		body   []byte
	}
	var rec captured
	lb := newLoopback(t, func(w http.ResponseWriter, r *http.Request) {
		buf, _ := io.ReadAll(r.Body)
		rec = captured{method: r.Method, path: r.URL.Path, auth: r.Header.Get("Authorization"), body: buf}
		okChatCompletion(w, `{"k":1}`)
	})
	e := execFor(t, lb.baseURL(), nativespinejsonfixture.Binding())
	res, _ := callThroughComposite(context.Background(), e)
	if res.Disposition != bamlutils.NativeSpineSucceeded {
		t.Fatalf("disposition = %v (err %v), want succeeded", res.Disposition, res.Err)
	}

	if lb.count() != 1 {
		t.Fatalf("provider request count = %d, want exactly 1", lb.count())
	}
	if rec.method != http.MethodPost {
		t.Errorf("method = %q, want POST", rec.method)
	}
	if rec.auth != "Bearer sk-execbridge-u1-not-a-real-secret" {
		t.Errorf("Authorization header mismatch (the literal test api_key is on the wire)")
	}
	// No unapproved body field: exactly model + messages, with the literal model.
	var body map[string]json.RawMessage
	if err := json.Unmarshal(rec.body, &body); err != nil {
		t.Fatalf("request body is not JSON: %v (%s)", err, rec.body)
	}
	if len(body) != 2 {
		t.Fatalf("request body has %d top-level keys, want exactly 2 (model, messages): %s", len(body), rec.body)
	}
	for _, k := range []string{"model", "messages"} {
		if _, ok := body[k]; !ok {
			t.Errorf("request body missing %q key: %s", k, rec.body)
		}
	}
	var model string
	_ = json.Unmarshal(body["model"], &model)
	if model != "gpt-4o-mini" {
		t.Errorf("wire model = %q, want the literal gpt-4o-mini", model)
	}
	if !strings.Contains(string(rec.body), "Return a JSON document describing weather") {
		t.Errorf("rendered prompt not on the wire: %s", rec.body)
	}
}
