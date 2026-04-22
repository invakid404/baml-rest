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
	// Used for response content extraction. For single-provider clients,
	// this is the only provider used. For fallback chains, this is
	// ignored in favor of per-attempt provider resolution.
	Provider string

	// RetryPolicy is the retry policy for this request. Nil means no retries.
	RetryPolicy *retry.Policy

	// NeedsRaw is true if the caller wants raw LLM response text
	// (for /call-with-raw endpoint).
	NeedsRaw bool

	// FallbackChain is the ordered list of child client names for fallback
	// strategies. When non-empty, each retry attempt walks the entire chain
	// in order; if any child succeeds, the attempt returns immediately.
	// When empty, the single Provider is used for all attempts.
	FallbackChain []string

	// ClientProviders maps child client names to their provider strings.
	// Used with FallbackChain to resolve the provider for each attempt.
	// In mixed-mode chains this map includes both BuildRequest-supported
	// children and legacy (unsupported-provider) children — it is a pure
	// lookup table, not a "supported set." Callers must consult
	// LegacyChildren to decide which path runs a given child.
	ClientProviders map[string]string

	// LegacyChildren marks the entries in FallbackChain that must be driven
	// via the legacy BAML Stream API rather than the BuildRequest path.
	// Mirrors StreamConfig.LegacyChildren — see that field for details.
	LegacyChildren map[string]bool

	// LegacyCallChild runs a single child via BAML's Stream API for the
	// non-streaming call path. The orchestrator invokes it for children
	// marked in LegacyChildren. Contract mirrors
	// StreamConfig.LegacyStreamChild: callback fires sendHeartbeat on the
	// first FunctionLog tick (matching the BuildRequest call path's
	// post-HTTP liveness semantics) and returns (final, raw, err).
	//
	// Even though this is the non-streaming path, the BAML runtime exposes
	// streaming as the primitive, so the legacy helper goes through
	// Stream.Method + FunctionLog capture identically to the streaming
	// callback — raw comes from FunctionLog.RawLLMResponse().
	LegacyCallChild LegacyCallChildFunc
}

// LegacyCallChildFunc runs one child of a mixed-mode fallback chain via
// BAML's Stream API for the non-streaming call path. See
// CallConfig.LegacyCallChild for the full contract.
type LegacyCallChildFunc func(
	ctx context.Context,
	clientOverride string,
	provider string,
	needsRaw bool,
	sendHeartbeat func(),
) (finalResult any, raw string, err error)

// BuildCallRequestFunc builds an HTTP request for a non-streaming call by
// calling Request.Method(ctx, args, opts...) and converting the result
// to an llmhttp.Request. The clientOverride parameter, when non-empty,
// forces the request to use a specific child client (for fallback chain
// iteration). This is provided by the generated adapter code.
type BuildCallRequestFunc func(ctx context.Context, clientOverride string) (*llmhttp.Request, error)

// ExtractResponseFunc extracts the LLM output text from a non-streaming
// JSON response body. Returns two strings: parseable (for Parse.Method)
// and raw (for /call-with-raw's Raw()). For most providers these are
// identical; for Anthropic with extended thinking, raw includes thinking
// blocks while parseable does not.
type ExtractResponseFunc func(provider string, responseBody string) (parseable, raw string, err error)

// RunCallOrchestration executes the non-streaming BuildRequest path.
//
// Flow — single retry loop covering HTTP, extraction, and parsing:
//  1. buildRequest(ctx, clientOverride) → llmhttp.Request
//  2. httpClient.Execute(ctx, req, onSuccess) → llmhttp.Response
//  3. extractResponse(provider, body) → parseable + raw text
//  4. parseFinal(ctx, parseable) → typed result
//  5. emit StreamResultKindFinal on channel
//
// All four steps run inside the retry loop so that extraction/parse failures
// in a fallback chain advance to the next child. This matches the streaming
// orchestrator, where parseFinal failures are retried from inside the attempt
// function.
//
// The request is rebuilt on each attempt because BuildRequest fallback routing
// may select a different child client/provider per retry. The generated
// adapter passes the chosen child via clientOverride, and Request may return a
// different HTTP request on each invocation. baml-roundrobin stays on the
// legacy path because this per-request orchestrator does not keep cross-request
// rotation state.
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

	// Validate the configured provider(s) up front so invalid fallback
	// chains fail before any retry attempts are launched. The validation
	// walks every child — previously only the first child was checked,
	// which allowed a mixed-shaped chain with a valid first but broken
	// later child to start retrying before surfacing the error.
	if len(config.FallbackChain) == 0 {
		if config.Provider == "" || !IsCallProviderSupported(config.Provider) {
			return fmt.Errorf("buildrequest: unsupported or empty provider %q for non-streaming call", config.Provider)
		}
	} else {
		if len(config.LegacyChildren) > 0 && config.LegacyCallChild == nil {
			return fmt.Errorf("buildrequest: LegacyChildren set but LegacyCallChild is nil")
		}
		for _, child := range config.FallbackChain {
			if config.LegacyChildren[child] {
				// Legacy children are driven via BAML's Stream API, which
				// tolerates providers BuildRequest doesn't support; we
				// only require the legacy callback itself.
				continue
			}
			provider := config.ClientProviders[child]
			if provider == "" || !IsCallProviderSupported(provider) {
				return fmt.Errorf("buildrequest: unsupported or empty provider %q for child %q", provider, child)
			}
		}
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

	// callAttemptResult carries the final parsed value and raw text from
	// the winning attempt.
	type callAttemptResult struct {
		finalResult any
		raw         string
	}

	// tryOneChild builds, executes, extracts, and parses a single child.
	tryOneChild := func(provider, clientOverride string) (*callAttemptResult, error) {
		req, err := buildRequest(ctx, clientOverride)
		if err != nil {
			return nil, fmt.Errorf("buildrequest: failed to build request: %w", err)
		}
		resp, httpErr := httpClient.Execute(ctx, req, sendHeartbeat)
		if httpErr != nil {
			return nil, httpErr
		}

		parseable, raw, extractErr := extractResponse(provider, resp.Body)
		if extractErr != nil {
			return nil, fmt.Errorf("buildrequest: failed to extract response content: %w", extractErr)
		}

		finalResult, parseErr := parseFinal(ctx, parseable)
		if parseErr != nil {
			return nil, fmt.Errorf("buildrequest: failed to parse final result: %w", parseErr)
		}

		return &callAttemptResult{finalResult: finalResult, raw: raw}, nil
	}

	// attemptFull tries either the single provider or the entire fallback
	// chain. For fallback chains, each retry attempt walks all children in
	// order — matching the BAML runtime's semantics where retries retry the
	// entire strategy after all children fail.
	attemptFull := func(_ int) (any, error) {
		if len(config.FallbackChain) == 0 {
			return tryOneChild(config.Provider, "")
		}
		var lastErr error
		for _, child := range config.FallbackChain {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			default:
			}
			if config.LegacyChildren[child] {
				// Legacy children run via BAML's Stream API. The callback
				// owns heartbeat timing (fires sendHeartbeat on the first
				// FunctionLog tick) so pool hung-detection stays correct
				// for slow upstreams that eventually produce bytes.
				finalResult, raw, err := config.LegacyCallChild(
					ctx,
					child,
					config.ClientProviders[child],
					config.NeedsRaw,
					sendHeartbeat,
				)
				if err == nil {
					return &callAttemptResult{finalResult: finalResult, raw: raw}, nil
				}
				lastErr = err
				continue
			}
			provider := config.ClientProviders[child]
			result, err := tryOneChild(provider, child)
			if err == nil {
				return result, nil
			}
			lastErr = err
		}
		return nil, lastErr
	}

	// Non-streaming retry callbacks do not emit heartbeats. In the pool,
	// any received result marks gotFirstByte=true before heartbeat results
	// are filtered, so an onRetry heartbeat would disable first-byte hung
	// detection even though no provider has produced a real 2xx response yet.
	result, err := retry.Execute(ctx, config.RetryPolicy, attemptFull, nil)

	if err != nil {
		trySend(newResult(bamlutils.StreamResultKindError, nil, nil, "", err, false))
		return nil
	}

	winningResult := result.(*callAttemptResult)

	rawForFinal := ""
	if config.NeedsRaw {
		rawForFinal = winningResult.raw
	}
	trySend(newResult(bamlutils.StreamResultKindFinal, nil, winningResult.finalResult, rawForFinal, nil, false))

	return nil
}
