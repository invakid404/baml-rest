package buildrequest

import (
	"context"
	"fmt"
	"sync/atomic"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/bamlutils/retry"
)

// CallConfig holds configuration for a non-streaming call request.
// This is simpler than StreamConfig because non-streaming calls don't need
// partial result handling or parse throttling.
type CallConfig struct {
	// Provider is the LLM provider name (e.g., "openai", "anthropic").
	// Used for response content extraction.
	Provider string

	// RetryPolicy is the retry policy for this request. Nil means no retries.
	RetryPolicy *retry.Policy

	// NeedsRaw is true if the caller wants raw LLM response text
	// (for /call-with-raw endpoint).
	NeedsRaw bool
}

// BuildCallRequestFunc builds an HTTP request for a non-streaming call by
// calling Request.Method(ctx, args, opts...) and converting the result
// to an llmhttp.Request. This is provided by the generated adapter code.
type BuildCallRequestFunc func(ctx context.Context) (*llmhttp.Request, error)

// ExtractResponseFunc extracts the LLM output text from a non-streaming
// JSON response body. Returns two strings: parseable (for Parse.Method)
// and raw (for /call-with-raw's Raw()). For most providers these are
// identical; for Anthropic with extended thinking, raw includes thinking
// blocks while parseable does not.
type ExtractResponseFunc func(provider string, responseBody string) (parseable, raw string, err error)

// RunCallOrchestration executes the non-streaming BuildRequest path.
//
// Flow:
//  1. Retry loop (build request + HTTP roundtrip):
//     a. buildRequest(ctx) → llmhttp.Request
//     b. httpClient.Execute(ctx, req, onSuccess) → llmhttp.Response
//     c. onSuccess callback emits heartbeat after 2xx status, before body read
//     d. on failure: onRetry emits heartbeat to maintain liveness during backoff
//  2. After retry succeeds with a response body:
//     a. extractResponse(provider, body) → parseable + raw text
//     b. parseFinal(ctx, parseable) → typed result
//     c. emit StreamResultKindFinal on channel
//
// The request is rebuilt on each attempt because retry policies may route
// through different models/providers — BAML's Request API may return a
// different HTTP request on each invocation depending on the client
// strategy (e.g., fallback, round-robin).
//
// Extraction and parsing happen OUTSIDE the retry loop because they are
// deterministic: a malformed 200 response or a parse failure will produce
// the same error on every attempt. Retrying the LLM call for those would
// waste quota and latency without any chance of recovery.
//
// The output channel is NOT closed by this function — the caller (generated
// code) is responsible for closing it.
//
// This function blocks until the call is complete or the context is cancelled.
func RunCallOrchestration(
	ctx context.Context,
	out chan<- bamlutils.StreamResult,
	config *CallConfig,
	httpClient *llmhttp.Client,
	buildRequest BuildCallRequestFunc,
	parseFinal ParseFinalFunc,
	extractResponse ExtractResponseFunc,
	newResult NewResultFunc,
) error {
	if httpClient == nil {
		httpClient = llmhttp.DefaultClient
	}

	if config.Provider == "" || !IsCallProviderSupported(config.Provider) {
		return fmt.Errorf("buildrequest: unsupported or empty provider %q for non-streaming call", config.Provider)
	}

	// sendHeartbeat emits a heartbeat to the pool's hung detector via
	// Execute()'s onSuccess callback. It fires after the provider returns
	// a 2xx status but before the body is read, so the pool sees liveness
	// without waiting for the full body to buffer.
	//
	// Heartbeat sends are best-effort: if the output channel is full the
	// heartbeat is dropped (via the default branch) and the StreamResult
	// is released back to the pool. This is safe because a full channel
	// means the consumer already has buffered results and won't consider
	// the worker hung.
	var heartbeatSent atomic.Bool
	sendHeartbeat := func() {
		if heartbeatSent.CompareAndSwap(false, true) {
			r := newResult(bamlutils.StreamResultKindHeartbeat, nil, nil, "", nil, false)
			select {
			case out <- r:
			default:
				r.Release()
			}
		}
	}

	// trySend attempts to send a StreamResult on the output channel,
	// prioritizing cancellation. Uses the priority-select idiom: check
	// ctx.Done() first with a non-blocking select, then fall through to
	// the blocking send only if the context is still active. This avoids
	// the race where ctx cancels between an if-guard and the select,
	// making both cases ready and allowing Go to nondeterministically
	// choose the send.
	trySend := func(r bamlutils.StreamResult) bool {
		select {
		case <-ctx.Done():
			r.Release()
			return false
		default:
		}
		select {
		case out <- r:
			return true
		case <-ctx.Done():
			r.Release()
			return false
		}
	}

	// Each attempt rebuilds the request and executes the HTTP roundtrip.
	// The request must be rebuilt per attempt because retry policies may
	// cover multiple models — BAML's Request API can return a different
	// HTTP request on each call depending on the client strategy.
	attemptHTTP := func(attempt int) (any, error) {
		req, err := buildRequest(ctx)
		if err != nil {
			return nil, fmt.Errorf("buildrequest: failed to build request: %w", err)
		}
		resp, httpErr := httpClient.Execute(ctx, req, sendHeartbeat)
		if httpErr != nil {
			return nil, httpErr
		}
		return resp, nil
	}

	// The onRetry callback emits a heartbeat before the backoff sleep
	// so the pool's hung detector sees liveness between attempts, then
	// resets the atomic so the next attempt's onSuccess heartbeat fires.
	result, err := retry.Execute(ctx, config.RetryPolicy, attemptHTTP, func(attempt int) {
		// Emit heartbeat directly (bypassing the atomic guard) to signal
		// liveness during retry backoff. Without this, repeated fast
		// 5xx/429 responses followed by a long backoff would leave the
		// request silent and trip FirstByteTimeout.
		r := newResult(bamlutils.StreamResultKindHeartbeat, nil, nil, "", nil, false)
		select {
		case out <- r:
		default:
			r.Release()
		}
		heartbeatSent.Store(false)
	})

	if err != nil {
		trySend(newResult(bamlutils.StreamResultKindError, nil, nil, "", err, false))
		return nil
	}

	// Extraction and parsing run once, outside the retry loop. These are
	// deterministic: a malformed response or parse failure will produce
	// the same error every time, so retrying would waste LLM calls.
	resp := result.(*llmhttp.Response)

	parseable, raw, err := extractResponse(config.Provider, resp.Body)
	if err != nil {
		trySend(newResult(bamlutils.StreamResultKindError, nil, nil, "", fmt.Errorf("buildrequest: failed to extract response content: %w", err), false))
		return nil
	}

	finalResult, parseErr := parseFinal(ctx, parseable)
	if parseErr != nil {
		trySend(newResult(bamlutils.StreamResultKindError, nil, nil, "", fmt.Errorf("buildrequest: failed to parse final result: %w", parseErr), false))
		return nil
	}

	rawForFinal := ""
	if config.NeedsRaw {
		rawForFinal = raw
	}
	trySend(newResult(bamlutils.StreamResultKindFinal, nil, finalResult, rawForFinal, nil, false))

	return nil
}
