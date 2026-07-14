package buildrequest

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/bamlutils/retry"
)

// Stable, secret-free decline tokens used by the fake callbacks. Real
// implementations supply their own bounded enums; these just prove the
// orchestrator carries whatever the callback returns.
const (
	testDeclineStage  NativeDeclineStage  = "strategy"
	testDeclineReason NativeDeclineReason = "test_decline"
)

// nativeCallRecorder records every invocation of a fake native callback plus
// the decision it should return, so tests can assert invocation count/order,
// the neutral attempt context handed in, and context threading — all without a
// nanollm dependency. RunCallOrchestration runs synchronously in the calling
// goroutine, so a plain slice is race-free here.
type nativeCallRecorder struct {
	calls   []NativeCallAttempt
	ctxErrs []error
	decide  func(NativeCallAttempt) NativeCallOutcome
}

func (r *nativeCallRecorder) callback() NativeCallAttemptFunc {
	return func(ctx context.Context, attempt NativeCallAttempt) NativeCallOutcome {
		r.calls = append(r.calls, attempt)
		r.ctxErrs = append(r.ctxErrs, ctx.Err())
		return r.decide(attempt)
	}
}

func nativeDrain(ch <-chan bamlutils.StreamResult) []bamlutils.StreamResult {
	var out []bamlutils.StreamResult
	for r := range ch {
		out = append(out, r)
	}
	return out
}

// countingBuildFn wraps makeBuildCallRequest with an invocation counter and an
// override recorder so tests can prove "the BAML build/send ran" or "no second
// same-child send happened".
func countingBuildFn(serverURL string, count *atomic.Int32, overrides *[]string) BuildCallRequestFunc {
	return func(ctx context.Context, clientOverride string) (*llmhttp.Request, error) {
		count.Add(1)
		*overrides = append(*overrides, clientOverride)
		return &llmhttp.Request{
			URL:    serverURL,
			Method: "POST",
			Body:   `{"model":"gpt-4","stream":false}`,
		}, nil
	}
}

// TestNativeSeam_OffPaths pins the double gate: the seam fires only when the
// callback is installed AND the neutral enabled predicate is true. With either
// off, behavior is byte-identical to today (the BAML build/send runs).
func TestNativeSeam_OffPaths(t *testing.T) {
	t.Run("nil callback, enabled true", func(t *testing.T) {
		var buildCount atomic.Int32
		var serverHits atomic.Int32
		var overrides []string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			serverHits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(200)
			fmt.Fprint(w, `{"choices":[{"message":{"content":"baml world"}}]}`)
		}))
		defer server.Close()

		out := make(chan bamlutils.StreamResult, 100)
		config := &CallConfig{
			Provider: "openai",
			// Enabled but no callback installed -> hard off.
			NativeAttemptEnabled: true,
		}
		err := RunCallOrchestration(
			context.Background(), out, config, llmhttp.NewClient(server.Client()),
			countingBuildFn(server.URL, &buildCount, &overrides),
			identityParseFinal,
			ExtractResponseContent, ExtractResponseContentBytes, nil, newTestResult,
		)
		close(out)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		results := nativeDrain(out)
		final := results[len(results)-1]
		if final.Kind() != bamlutils.StreamResultKindFinal || final.Final() != "baml world" {
			t.Fatalf("expected BAML final 'baml world', got kind=%v final=%v", final.Kind(), final.Final())
		}
		if buildCount.Load() != 1 || serverHits.Load() != 1 {
			t.Fatalf("expected exactly one BAML build+send, got build=%d hits=%d", buildCount.Load(), serverHits.Load())
		}
	})

	t.Run("callback installed, enabled false", func(t *testing.T) {
		var buildCount atomic.Int32
		var serverHits atomic.Int32
		var overrides []string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			serverHits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(200)
			fmt.Fprint(w, `{"choices":[{"message":{"content":"baml world"}}]}`)
		}))
		defer server.Close()

		// This callback would "succeed" — if it were ever invoked the final
		// would be the native value and no BAML send would occur. Enabled=false
		// must keep it dormant.
		rec := &nativeCallRecorder{decide: func(NativeCallAttempt) NativeCallOutcome {
			return SucceedNativeCall("native value", "", "")
		}}
		out := make(chan bamlutils.StreamResult, 100)
		config := &CallConfig{
			Provider:             "openai",
			NativeAttempt:        rec.callback(),
			NativeAttemptEnabled: false,
		}
		err := RunCallOrchestration(
			context.Background(), out, config, llmhttp.NewClient(server.Client()),
			countingBuildFn(server.URL, &buildCount, &overrides),
			identityParseFinal,
			ExtractResponseContent, ExtractResponseContentBytes, nil, newTestResult,
		)
		close(out)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(rec.calls) != 0 {
			t.Fatalf("callback must not be invoked when NativeAttemptEnabled=false, got %d calls", len(rec.calls))
		}
		results := nativeDrain(out)
		final := results[len(results)-1]
		if final.Final() != "baml world" {
			t.Fatalf("expected BAML final, got %v", final.Final())
		}
		if buildCount.Load() != 1 || serverHits.Load() != 1 {
			t.Fatalf("expected exactly one BAML build+send, got build=%d hits=%d", buildCount.Load(), serverHits.Load())
		}
	})
}

// TestNativeSeam_DeclineRunsSameChildBAML proves a pre-socket decline continues
// with the existing BAML build/send for the SAME child in the SAME iteration:
// the callback fires once, the BAML path runs exactly once (no double send),
// and the decline tokens are the stable secret-free values the callback chose.
func TestNativeSeam_DeclineRunsSameChildBAML(t *testing.T) {
	var buildCount atomic.Int32
	var serverHits atomic.Int32
	var overrides []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serverHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		fmt.Fprint(w, `{"choices":[{"message":{"content":"baml after decline"}}]}`)
	}))
	defer server.Close()

	var declined NativeCallOutcome
	rec := &nativeCallRecorder{decide: func(NativeCallAttempt) NativeCallOutcome {
		declined = DeclineNativeCall(testDeclineStage, testDeclineReason)
		return declined
	}}
	out := make(chan bamlutils.StreamResult, 100)
	config := &CallConfig{
		Provider:             "openai",
		NativeAttempt:        rec.callback(),
		NativeAttemptEnabled: true,
	}
	err := RunCallOrchestration(
		context.Background(), out, config, llmhttp.NewClient(server.Client()),
		countingBuildFn(server.URL, &buildCount, &overrides),
		identityParseFinal,
		ExtractResponseContent, ExtractResponseContentBytes, nil, newTestResult,
	)
	close(out)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(rec.calls) != 1 {
		t.Fatalf("expected callback invoked exactly once, got %d", len(rec.calls))
	}
	// Neutral attempt context: the selected leaf identity is threaded through.
	if got := rec.calls[0]; got.Provider != "openai" || got.ClientOverride != "" {
		t.Errorf("attempt context mismatch: got provider=%q clientOverride=%q", got.Provider, got.ClientOverride)
	}
	// No double send: exactly one BAML build and one socket.
	if buildCount.Load() != 1 || serverHits.Load() != 1 {
		t.Fatalf("decline must run the BAML path exactly once (no double send), got build=%d hits=%d", buildCount.Load(), serverHits.Load())
	}
	results := nativeDrain(out)
	final := results[len(results)-1]
	if final.Kind() != bamlutils.StreamResultKindFinal || final.Final() != "baml after decline" {
		t.Fatalf("expected BAML final after decline, got kind=%v final=%v", final.Kind(), final.Final())
	}
	// Decline tokens are stable, secret-free, and carried verbatim.
	if declined.DeclineStage != testDeclineStage || declined.DeclineReason != testDeclineReason {
		t.Errorf("decline tokens not carried: stage=%q reason=%q", declined.DeclineStage, declined.DeclineReason)
	}
}

// TestNativeSeam_DeclineNoFallbackAdvanceNoRetry proves that a decline followed
// by a successful same-child BAML send does NOT advance the fallback chain and
// does NOT consume an extra retry: the callback and BAML both run for the first
// child only, and the outcome RetryCount is 0.
func TestNativeSeam_DeclineNoFallbackAdvanceNoRetry(t *testing.T) {
	var buildCount atomic.Int32
	var serverHits atomic.Int32
	var overrides []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serverHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		fmt.Fprint(w, `{"choices":[{"message":{"content":"child A ok"}}]}`)
	}))
	defer server.Close()

	rec := &nativeCallRecorder{decide: func(NativeCallAttempt) NativeCallOutcome {
		return DeclineNativeCall(testDeclineStage, testDeclineReason)
	}}
	plan := &bamlutils.Metadata{Path: "buildrequest", Client: "ChildA"}
	out := make(chan bamlutils.StreamResult, 100)
	config := &CallConfig{
		RetryPolicy:          &retry.Policy{MaxRetries: 3, Strategy: &retry.ConstantDelay{DelayMs: 1}},
		FallbackChain:        []string{"ChildA", "ChildB"},
		ClientProviders:      map[string]string{"ChildA": "openai", "ChildB": "openai"},
		MetadataPlan:         plan,
		NewMetadataResult:    newTestMetadataResult,
		NativeAttempt:        rec.callback(),
		NativeAttemptEnabled: true,
	}
	err := RunCallOrchestration(
		context.Background(), out, config, llmhttp.NewClient(server.Client()),
		countingBuildFn(server.URL, &buildCount, &overrides),
		identityParseFinal,
		ExtractResponseContent, ExtractResponseContentBytes, nil, newTestResult,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	close(out)

	// The callback and BAML run for ChildA only; ChildB is never reached.
	if len(rec.calls) != 1 || rec.calls[0].ClientOverride != "ChildA" {
		t.Fatalf("expected callback for ChildA only, got %+v", rec.calls)
	}
	if !slices.Equal(overrides, []string{"ChildA"}) {
		t.Fatalf("fallback chain must not advance past ChildA on decline+success, got overrides=%v", overrides)
	}
	if buildCount.Load() != 1 || serverHits.Load() != 1 {
		t.Fatalf("expected one BAML build+send for ChildA, got build=%d hits=%d", buildCount.Load(), serverHits.Load())
	}

	_, outcome, _, _ := collectMetadata(t, out)
	if outcome == nil {
		t.Fatal("expected an outcome metadata frame")
	}
	if outcome.RetryCount == nil || *outcome.RetryCount != 0 {
		t.Errorf("expected RetryCount=0 (no extra retry consumed), got %v", outcome.RetryCount)
	}
	if outcome.WinnerClient != "ChildA" {
		t.Errorf("expected winner ChildA, got %q", outcome.WinnerClient)
	}
	// The BAML path won, so the engine marker stays empty (byte-identical to
	// today's outcome frames).
	if outcome.WinnerEngine != "" {
		t.Errorf("expected empty WinnerEngine on BAML win, got %q", outcome.WinnerEngine)
	}
}

// TestNativeSeam_SucceedReturnsResult proves a native success is returned as an
// ordinary attempt result — no BAML build/send happens, raw/reasoning are
// carried (gated on NeedsRaw), and the native winner-engine marker reaches the
// outcome metadata.
func TestNativeSeam_SucceedReturnsResult(t *testing.T) {
	buildFn := func(ctx context.Context, clientOverride string) (*llmhttp.Request, error) {
		t.Fatal("buildRequest must not be called on native success")
		return nil, nil
	}

	rec := &nativeCallRecorder{decide: func(NativeCallAttempt) NativeCallOutcome {
		return SucceedNativeCall("native final", "native raw", "native reasoning")
	}}
	plan := &bamlutils.Metadata{Path: "buildrequest", Client: "OnlyClient", Provider: "openai"}
	out := make(chan bamlutils.StreamResult, 100)
	config := &CallConfig{
		Provider:             "openai",
		NeedsRaw:             true,
		MetadataPlan:         plan,
		NewMetadataResult:    newTestMetadataResult,
		NativeAttempt:        rec.callback(),
		NativeAttemptEnabled: true,
	}
	// httpClient nil is fine: no HTTP is attempted on the success path.
	err := RunCallOrchestration(
		context.Background(), out, config, nil,
		buildFn,
		identityParseFinal,
		ExtractResponseContent, ExtractResponseContentBytes, nil, newTestResult,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	close(out)

	results := nativeDrain(out)
	var final bamlutils.StreamResult
	for _, r := range results {
		if r.Kind() == bamlutils.StreamResultKindFinal {
			final = r
		}
	}
	if final == nil {
		t.Fatalf("expected a final result, got kinds=%v", kindsOf(results))
	}
	if final.Final() != "native final" {
		t.Errorf("expected native final value, got %v", final.Final())
	}
	if final.Raw() != "native raw" {
		t.Errorf("expected native raw carried, got %q", final.Raw())
	}
	if final.Reasoning() != "native reasoning" {
		t.Errorf("expected native reasoning carried, got %q", final.Reasoning())
	}

	_, outcome, _, _ := collectMetadataFrom(results, t)
	if outcome == nil {
		t.Fatal("expected an outcome metadata frame")
	}
	if outcome.WinnerEngine != "native" {
		t.Errorf("expected WinnerEngine=native, got %q", outcome.WinnerEngine)
	}
	if outcome.WinnerPath != "buildrequest" {
		t.Errorf("expected WinnerPath=buildrequest, got %q", outcome.WinnerPath)
	}
	if outcome.WinnerProvider != "openai" {
		t.Errorf("expected WinnerProvider=openai, got %q", outcome.WinnerProvider)
	}
	if outcome.WinnerClient != "OnlyClient" {
		t.Errorf("expected WinnerClient from plan, got %q", outcome.WinnerClient)
	}
}

// TestNativeSeam_SucceedGatesRawOnNeedsRaw proves raw/reasoning are suppressed
// when NeedsRaw is false, exactly like the BAML path.
func TestNativeSeam_SucceedGatesRawOnNeedsRaw(t *testing.T) {
	rec := &nativeCallRecorder{decide: func(NativeCallAttempt) NativeCallOutcome {
		return SucceedNativeCall("native final", "native raw", "native reasoning")
	}}
	out := make(chan bamlutils.StreamResult, 100)
	config := &CallConfig{
		Provider:             "openai",
		NeedsRaw:             false,
		NativeAttempt:        rec.callback(),
		NativeAttemptEnabled: true,
	}
	err := RunCallOrchestration(
		context.Background(), out, config, nil,
		func(context.Context, string) (*llmhttp.Request, error) {
			t.Fatal("buildRequest must not be called on native success")
			return nil, nil
		},
		identityParseFinal,
		ExtractResponseContent, ExtractResponseContentBytes, nil, newTestResult,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	close(out)
	results := nativeDrain(out)
	final := results[len(results)-1]
	if final.Final() != "native final" {
		t.Errorf("expected native final, got %v", final.Final())
	}
	if final.Raw() != "" || final.Reasoning() != "" {
		t.Errorf("expected raw/reasoning suppressed when NeedsRaw=false, got raw=%q reasoning=%q", final.Raw(), final.Reasoning())
	}
}

// TestNativeSeam_FailedPropagatesNoSecondSend proves a post-claim native
// failure is terminal for the child attempt: the typed error propagates to the
// caller and there is NO hidden second same-child BAML send.
func TestNativeSeam_FailedPropagatesNoSecondSend(t *testing.T) {
	sentinel := errors.New("native transport boom")
	var buildCount atomic.Int32
	rec := &nativeCallRecorder{decide: func(NativeCallAttempt) NativeCallOutcome {
		return FailNativeCall(sentinel)
	}}
	out := make(chan bamlutils.StreamResult, 100)
	config := &CallConfig{
		Provider:             "openai",
		RetryPolicy:          &retry.Policy{MaxRetries: 0},
		NativeAttempt:        rec.callback(),
		NativeAttemptEnabled: true,
	}
	var overrides []string
	err := RunCallOrchestration(
		context.Background(), out, config, nil,
		countingBuildFn("http://127.0.0.1:0", &buildCount, &overrides),
		identityParseFinal,
		ExtractResponseContent, ExtractResponseContentBytes, nil, newTestResult,
	)
	if err != nil {
		t.Fatalf("RunCallOrchestration returned a Go error (errors surface as frames): %v", err)
	}
	close(out)

	if buildCount.Load() != 0 {
		t.Fatalf("failed native attempt must NOT fall through to a second BAML send, got build=%d", buildCount.Load())
	}
	results := nativeDrain(out)
	final := results[len(results)-1]
	if final.Kind() != bamlutils.StreamResultKindError {
		t.Fatalf("expected error frame, got %v", final.Kind())
	}
	if !errors.Is(final.Error(), sentinel) {
		t.Errorf("expected the callback's typed error to propagate, got %v", final.Error())
	}
}

// TestNativeSeam_FailedOwnedByOuterRetry proves the outer retry loop owns a
// native failure exactly as it owns a BAML failure: with MaxRetries=2 the
// attempt (native included) is retried three times total and never falls
// through to BAML.
func TestNativeSeam_FailedOwnedByOuterRetry(t *testing.T) {
	sentinel := errors.New("native planner boom")
	var buildCount atomic.Int32
	rec := &nativeCallRecorder{decide: func(NativeCallAttempt) NativeCallOutcome {
		return FailNativeCall(sentinel)
	}}
	out := make(chan bamlutils.StreamResult, 100)
	config := &CallConfig{
		Provider:             "openai",
		RetryPolicy:          &retry.Policy{MaxRetries: 2, Strategy: &retry.ConstantDelay{DelayMs: 1}},
		NativeAttempt:        rec.callback(),
		NativeAttemptEnabled: true,
	}
	var overrides []string
	err := RunCallOrchestration(
		context.Background(), out, config, nil,
		countingBuildFn("http://127.0.0.1:0", &buildCount, &overrides),
		identityParseFinal,
		ExtractResponseContent, ExtractResponseContentBytes, nil, newTestResult,
	)
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	close(out)

	if len(rec.calls) != 3 {
		t.Fatalf("expected the native attempt retried by the outer loop (1 + 2 retries = 3 calls), got %d", len(rec.calls))
	}
	if buildCount.Load() != 0 {
		t.Fatalf("outer retry of a native failure must not invoke BAML, got build=%d", buildCount.Load())
	}
	results := nativeDrain(out)
	final := results[len(results)-1]
	if final.Kind() != bamlutils.StreamResultKindError || !errors.Is(final.Error(), sentinel) {
		t.Fatalf("expected terminal error carrying the sentinel, got kind=%v err=%v", final.Kind(), final.Error())
	}
}

// TestNativeSeam_FailedChildAdvancesFallbackNoSecondSend proves the tri-state
// boundary in a chain: a native failure on child A is terminal for A (no second
// BAML send for A) while the OUTER fallback loop still advances to child B
// exactly as today. Child B then declines and its BAML send serves the request.
func TestNativeSeam_FailedChildAdvancesFallbackNoSecondSend(t *testing.T) {
	sentinel := errors.New("child A native boom")
	var buildCount atomic.Int32
	var serverHits atomic.Int32
	var overrides []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serverHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		fmt.Fprint(w, `{"choices":[{"message":{"content":"child B baml ok"}}]}`)
	}))
	defer server.Close()

	rec := &nativeCallRecorder{decide: func(a NativeCallAttempt) NativeCallOutcome {
		if a.ClientOverride == "ChildA" {
			return FailNativeCall(sentinel)
		}
		return DeclineNativeCall(testDeclineStage, testDeclineReason)
	}}
	out := make(chan bamlutils.StreamResult, 100)
	config := &CallConfig{
		RetryPolicy:          &retry.Policy{MaxRetries: 0},
		FallbackChain:        []string{"ChildA", "ChildB"},
		ClientProviders:      map[string]string{"ChildA": "openai", "ChildB": "openai"},
		NativeAttempt:        rec.callback(),
		NativeAttemptEnabled: true,
	}
	err := RunCallOrchestration(
		context.Background(), out, config, llmhttp.NewClient(server.Client()),
		countingBuildFn(server.URL, &buildCount, &overrides),
		identityParseFinal,
		ExtractResponseContent, ExtractResponseContentBytes, nil, newTestResult,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	close(out)

	// Callback fired for both children in order.
	if len(rec.calls) != 2 || rec.calls[0].ClientOverride != "ChildA" || rec.calls[1].ClientOverride != "ChildB" {
		t.Fatalf("expected callback for ChildA then ChildB, got %+v", rec.calls)
	}
	// The BAML send ran for ChildB ONLY — ChildA's native failure never fell
	// through to a same-child BAML send.
	if !slices.Equal(overrides, []string{"ChildB"}) {
		t.Fatalf("expected BAML send for ChildB only (no second send for ChildA), got %v", overrides)
	}
	if buildCount.Load() != 1 || serverHits.Load() != 1 {
		t.Fatalf("expected exactly one BAML build+send (ChildB), got build=%d hits=%d", buildCount.Load(), serverHits.Load())
	}
	results := nativeDrain(out)
	final := results[len(results)-1]
	if final.Kind() != bamlutils.StreamResultKindFinal || final.Final() != "child B baml ok" {
		t.Fatalf("expected BAML final from ChildB, got kind=%v final=%v", final.Kind(), final.Final())
	}
}

// TestNativeSeam_ContextThreaded proves the orchestrator's own context is
// handed to the callback (so cancellation/deadline propagate to a native
// attempt): a value set on the context is visible inside the callback, and the
// callback observes a live (non-cancelled) context on a normal attempt.
func TestNativeSeam_ContextThreaded(t *testing.T) {
	type ctxKey struct{}
	var sawValue any
	var sawErr error = errors.New("unset")
	rec := &nativeCallRecorder{decide: func(NativeCallAttempt) NativeCallOutcome {
		return SucceedNativeCall("ok", "", "")
	}}
	cb := func(ctx context.Context, attempt NativeCallAttempt) NativeCallOutcome {
		sawValue = ctx.Value(ctxKey{})
		sawErr = ctx.Err()
		return rec.decide(attempt)
	}
	out := make(chan bamlutils.StreamResult, 100)
	config := &CallConfig{
		Provider:             "openai",
		NativeAttempt:        cb,
		NativeAttemptEnabled: true,
	}
	ctx := context.WithValue(context.Background(), ctxKey{}, "sentinel-value")
	err := RunCallOrchestration(
		ctx, out, config, nil,
		func(context.Context, string) (*llmhttp.Request, error) {
			t.Fatal("buildRequest must not be called on native success")
			return nil, nil
		},
		identityParseFinal,
		ExtractResponseContent, ExtractResponseContentBytes, nil, newTestResult,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	close(out)
	if sawValue != "sentinel-value" {
		t.Errorf("callback did not receive the orchestrator context value, got %v", sawValue)
	}
	if sawErr != nil {
		t.Errorf("expected live context inside callback, got err=%v", sawErr)
	}
}

// TestNativeSeam_UnknownDispositionIsTerminal proves an out-of-contract
// disposition is treated as terminal (never a hidden second send), guarding the
// no-double-send invariant against a misbehaving implementation.
func TestNativeSeam_UnknownDispositionIsTerminal(t *testing.T) {
	var buildCount atomic.Int32
	var overrides []string
	rec := &nativeCallRecorder{decide: func(NativeCallAttempt) NativeCallOutcome {
		return NativeCallOutcome{Disposition: NativeCallDisposition(200)}
	}}
	out := make(chan bamlutils.StreamResult, 100)
	config := &CallConfig{
		Provider:             "openai",
		RetryPolicy:          &retry.Policy{MaxRetries: 0},
		NativeAttempt:        rec.callback(),
		NativeAttemptEnabled: true,
	}
	err := RunCallOrchestration(
		context.Background(), out, config, nil,
		countingBuildFn("http://127.0.0.1:0", &buildCount, &overrides),
		identityParseFinal,
		ExtractResponseContent, ExtractResponseContentBytes, nil, newTestResult,
	)
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	close(out)
	if buildCount.Load() != 0 {
		t.Fatalf("unknown disposition must not fall through to a BAML send, got build=%d", buildCount.Load())
	}
	results := nativeDrain(out)
	final := results[len(results)-1]
	if final.Kind() != bamlutils.StreamResultKindError {
		t.Fatalf("expected an error frame for an unknown disposition, got %v", final.Kind())
	}
}

// countKind counts frames of a given kind in a drained result slice.
func countKind(results []bamlutils.StreamResult, kind bamlutils.StreamResultKind) int {
	n := 0
	for _, r := range results {
		if r.Kind() == kind {
			n++
		}
	}
	return n
}

// TestNativeSeam_SuccessHeartbeat proves the native attempt context carries the
// same first-2xx liveness signal the BAML path gets: when the (simulated)
// native transport calls attempt.SendHeartbeat() on 2xx headers, exactly one
// heartbeat frame is emitted — even if it is called more than once — so a slow
// native body is never judged hung by the pool.
func TestNativeSeam_SuccessHeartbeat(t *testing.T) {
	t.Run("emits exactly one heartbeat", func(t *testing.T) {
		cb := func(ctx context.Context, attempt NativeCallAttempt) NativeCallOutcome {
			if attempt.SendHeartbeat == nil {
				t.Fatal("native attempt context must carry a SendHeartbeat callback")
			}
			// The native transport signals liveness the instant it reads 2xx
			// headers, before buffering the body.
			attempt.SendHeartbeat()
			return SucceedNativeCall("native final", "", "")
		}
		out := make(chan bamlutils.StreamResult, 100)
		config := &CallConfig{
			Provider:             "openai",
			NativeAttempt:        cb,
			NativeAttemptEnabled: true,
		}
		err := RunCallOrchestration(
			context.Background(), out, config, nil,
			func(context.Context, string) (*llmhttp.Request, error) {
				t.Fatal("buildRequest must not be called on native success")
				return nil, nil
			},
			identityParseFinal,
			ExtractResponseContent, ExtractResponseContentBytes, nil, newTestResult,
		)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		close(out)
		results := nativeDrain(out)
		if hb := countKind(results, bamlutils.StreamResultKindHeartbeat); hb != 1 {
			t.Fatalf("expected exactly one heartbeat on native success, got %d (kinds=%v)", hb, kindsOf(results))
		}
		final := results[len(results)-1]
		if final.Kind() != bamlutils.StreamResultKindFinal || final.Final() != "native final" {
			t.Fatalf("expected native final after heartbeat, got kind=%v final=%v", final.Kind(), final.Final())
		}
	})

	t.Run("idempotent across repeated calls", func(t *testing.T) {
		cb := func(ctx context.Context, attempt NativeCallAttempt) NativeCallOutcome {
			// A misbehaving transport calling it twice must still yield one
			// heartbeat frame.
			attempt.SendHeartbeat()
			attempt.SendHeartbeat()
			return SucceedNativeCall("ok", "", "")
		}
		out := make(chan bamlutils.StreamResult, 100)
		config := &CallConfig{
			Provider:             "openai",
			NativeAttempt:        cb,
			NativeAttemptEnabled: true,
		}
		err := RunCallOrchestration(
			context.Background(), out, config, nil,
			func(context.Context, string) (*llmhttp.Request, error) {
				t.Fatal("buildRequest must not be called on native success")
				return nil, nil
			},
			identityParseFinal,
			ExtractResponseContent, ExtractResponseContentBytes, nil, newTestResult,
		)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		close(out)
		results := nativeDrain(out)
		if hb := countKind(results, bamlutils.StreamResultKindHeartbeat); hb != 1 {
			t.Fatalf("expected exactly one heartbeat despite repeated SendHeartbeat calls, got %d", hb)
		}
	})
}

// TestNativeSeam_FailedLiteralNilErrIsTerminal proves the no-double-send / typed
// handoff invariant survives a hand-built (not FailNativeCall) failed outcome
// with a nil error: NativeCallOutcome is public, so a literal
// NativeCallOutcome{Disposition: NativeCallFailed} must NOT be read as success
// (which would panic on the nil *callAttemptResult) and must NOT fall through to
// a BAML send. The orchestrator normalizes the nil error to the sentinel.
func TestNativeSeam_FailedLiteralNilErrIsTerminal(t *testing.T) {
	var buildCount atomic.Int32
	var overrides []string
	rec := &nativeCallRecorder{decide: func(NativeCallAttempt) NativeCallOutcome {
		// Direct literal — bypasses FailNativeCall's own nil-error backstop.
		return NativeCallOutcome{Disposition: NativeCallFailed}
	}}
	out := make(chan bamlutils.StreamResult, 100)
	config := &CallConfig{
		Provider:             "openai",
		RetryPolicy:          &retry.Policy{MaxRetries: 0},
		NativeAttempt:        rec.callback(),
		NativeAttemptEnabled: true,
	}
	// A panic (nil *callAttemptResult deref) would crash the test process, so
	// simply reaching the assertions proves no panic occurred.
	err := RunCallOrchestration(
		context.Background(), out, config, nil,
		countingBuildFn("http://127.0.0.1:0", &buildCount, &overrides),
		identityParseFinal,
		ExtractResponseContent, ExtractResponseContentBytes, nil, newTestResult,
	)
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	close(out)

	if buildCount.Load() != 0 {
		t.Fatalf("a literal failed outcome must not fall through to a BAML send, got build=%d", buildCount.Load())
	}
	results := nativeDrain(out)
	final := results[len(results)-1]
	if final.Kind() != bamlutils.StreamResultKindError {
		t.Fatalf("expected a terminal error frame, got %v", final.Kind())
	}
	if !errors.Is(final.Error(), errNativeCallFailedNil) {
		t.Errorf("expected the normalized nil-error sentinel, got %v", final.Error())
	}
}

// kindsOf is a tiny diagnostic helper.
func kindsOf(results []bamlutils.StreamResult) []bamlutils.StreamResultKind {
	ks := make([]bamlutils.StreamResultKind, len(results))
	for i, r := range results {
		ks[i] = r.Kind()
	}
	return ks
}

// collectMetadataFrom extracts planned/outcome metadata from an already-drained
// result slice (collectMetadata consumes a channel; some tests drain first to
// also inspect the final frame).
func collectMetadataFrom(results []bamlutils.StreamResult, t *testing.T) (planned, outcome *bamlutils.Metadata, _ []bamlutils.StreamResultKind, _ []bamlutils.MetadataPhase) {
	t.Helper()
	for _, r := range results {
		if r.Kind() != bamlutils.StreamResultKindMetadata {
			continue
		}
		md := r.Metadata()
		if md == nil {
			t.Fatalf("metadata kind without payload")
		}
		switch md.Phase {
		case bamlutils.MetadataPhasePlanned:
			cp := *md
			planned = &cp
		case bamlutils.MetadataPhaseOutcome:
			cp := *md
			outcome = &cp
		}
	}
	return planned, outcome, nil, nil
}
