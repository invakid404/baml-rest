package buildrequest

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

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

	// IncludeThinkingInRaw is the per-request opt-in for surfacing
	// provider-specific reasoning/thinking content in raw. When true,
	// the response extractor accumulates Anthropic thinking blocks
	// into raw alongside text blocks. When false (default), raw
	// matches BAML's RawLLMResponse() text-only semantics. The flag
	// never affects the parseable text passed to Parse.
	IncludeThinkingInRaw bool

	// FallbackChain is the ordered list of child client names for fallback
	// strategies. When non-empty, each retry attempt walks the entire chain
	// in order; if any child succeeds, the attempt returns immediately.
	// When empty, the single Provider is used for all attempts.
	FallbackChain []string

	// ClientOverride mirrors StreamConfig.ClientOverride — the
	// clientOverride value forwarded to buildRequest on single-provider
	// attempts, used by the round-robin path to target the RR-selected
	// leaf instead of the function's default client.
	ClientOverride string

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

	// MetadataPlan is the pre-computed planned metadata for this request.
	// See StreamConfig.MetadataPlan for the contract; the call orchestrator
	// behaves identically (heartbeat → planned metadata → final → outcome).
	MetadataPlan *bamlutils.Metadata

	// NewMetadataResult constructs a pooled StreamResult wrapping a metadata
	// payload. Required when MetadataPlan is non-nil.
	NewMetadataResult NewMetadataResultFunc

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
// and raw (for /call-with-raw's Raw()).
//
// Parseable is always text-only — reasoning/thinking content is never
// accumulated into it, so the BAML parser cannot be influenced by
// reasoning text. Raw matches parseable by default; when includeThinking
// is true the function may additionally surface provider-specific
// reasoning (e.g. Anthropic thinking blocks) in raw for telemetry.
type ExtractResponseFunc func(provider string, responseBody string, includeThinking bool) (parseable, raw string, err error)

// RunCallOrchestration executes the non-streaming BuildRequest path.
//
// Flow — single retry loop covering HTTP, extraction, and parsing:
//  1. buildRequest(ctx, clientOverride) → llmhttp.Request
//  2. httpClient.Execute(ctx, req, onSuccess) → llmhttp.Response
//  3. extractResponse(provider, body, includeThinking) → parseable + raw text
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
// different HTTP request on each invocation.
//
// Top-level baml-roundrobin is resolved upstream by ResolveEffectiveClient
// (backed by SharedStateStore for cross-worker centralisation on modern
// adapters with SupportsWithClient); this orchestrator then dispatches to
// the resolved leaf like any other single-provider client. Older adapters
// without SupportsWithClient, and baml-roundrobin nested inside a fallback
// chain, still fall through to BAML's per-worker runtime rotation.
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
				// only require the legacy callback itself. Skipping the
				// IsCallProviderSupported check is safe because
				// ResolveFallbackChain rejects chains with any empty
				// provider (returns nil,nil,nil), so ClientProviders[child]
				// is guaranteed non-empty by the time we get here.
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

	// plannedMetadataOnce gates the single planned-metadata emission
	// (see RunStreamOrchestration for the rationale — heartbeatSent resets
	// per inner retry, planned metadata must not).
	var plannedMetadataOnce sync.Once
	emitPlannedMetadata := func() {
		if config.MetadataPlan == nil || config.NewMetadataResult == nil {
			return
		}
		plannedMetadataOnce.Do(func() {
			plan := *config.MetadataPlan
			plan.Phase = bamlutils.MetadataPhasePlanned
			plan.Attempt = 0
			r := config.NewMetadataResult(&plan)
			select {
			case out <- r:
			default:
				r.Release()
			}
		})
	}

	sendHeartbeat := func() {
		if heartbeatSent.CompareAndSwap(false, true) {
			r := newResult(bamlutils.StreamResultKindHeartbeat, nil, nil, "", nil, false)
			select {
			case out <- r:
			default:
				r.Release()
			}
		}
		emitPlannedMetadata()
	}

	// Emit planned metadata upfront so the routing decision is observable
	// even when the BuildRequest path returns an error before any HTTP
	// response (e.g., immediate validation failure, unsupported provider
	// caught later in the chain). Mirrors the legacy-path orchestrators
	// (runNoRawOrchestration / runFullOrchestration) which emit upfront in
	// their stream goroutine for the same reason. The plannedMetadataOnce
	// gate guarantees idempotency; subsequent sendHeartbeat calls are
	// no-ops on the planned side. See PR #192 verdict-15 follow-up.
	emitPlannedMetadata()

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
	// the winning attempt, plus the winner's routing identity for outcome
	// metadata.
	type callAttemptResult struct {
		finalResult    any
		raw            string
		winnerClient   string
		winnerProvider string
		winnerPath     string
	}

	startTime := time.Now()

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

		parseable, raw, extractErr := extractResponse(provider, resp.Body, config.IncludeThinkingInRaw)
		if extractErr != nil {
			return nil, fmt.Errorf("buildrequest: failed to extract response content: %w", extractErr)
		}

		finalResult, parseErr := parseFinal(ctx, parseable)
		if parseErr != nil {
			return nil, fmt.Errorf("buildrequest: failed to parse final result: %w", parseErr)
		}

		return &callAttemptResult{
			finalResult:    finalResult,
			raw:            raw,
			winnerProvider: provider,
			winnerPath:     "buildrequest",
		}, nil
	}

	// finalAttempt carries the 0-indexed attempt number of the winning call,
	// captured inside attemptFull because retry.Execute does not expose it
	// to the caller on success.
	var finalAttempt int

	// attemptFull tries either the single provider or the entire fallback
	// chain. For fallback chains, each retry attempt walks all children in
	// order — matching the BAML runtime's semantics where retries retry the
	// entire strategy after all children fail.
	attemptFull := func(attempt int) (any, error) {
		if len(config.FallbackChain) == 0 {
			result, err := tryOneChild(config.Provider, config.ClientOverride)
			if err == nil {
				finalAttempt = attempt
				if config.MetadataPlan != nil {
					result.winnerClient = config.MetadataPlan.Client
				}
			}
			return result, err
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
					finalAttempt = attempt
					return &callAttemptResult{
						finalResult:    finalResult,
						raw:            raw,
						winnerClient:   child,
						winnerProvider: config.ClientProviders[child],
						winnerPath:     "legacy",
					}, nil
				}
				lastErr = err
				continue
			}
			provider := config.ClientProviders[child]
			result, err := tryOneChild(provider, child)
			if err == nil {
				finalAttempt = attempt
				result.winnerClient = child
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

	// Emit outcome metadata before the final result so clients observe the
	// routing outcome attached to (and ordered before) the terminal payload.
	if config.MetadataPlan != nil && config.NewMetadataResult != nil {
		outcome := *config.MetadataPlan
		outcome.Phase = bamlutils.MetadataPhaseOutcome
		outcome.Attempt = 0
		outcome.RetryMax = nil
		outcome.RetryPolicy = ""
		outcome.Chain = nil
		outcome.LegacyChildren = nil
		outcome.Strategy = ""
		outcome.Provider = ""

		retryCount := finalAttempt
		outcome.RetryCount = &retryCount
		outcome.WinnerClient = winningResult.winnerClient
		outcome.WinnerProvider = winningResult.winnerProvider
		outcome.WinnerPath = winningResult.winnerPath
		dur := time.Since(startTime).Milliseconds()
		outcome.UpstreamDurMs = &dur

		if !trySend(config.NewMetadataResult(&outcome)) {
			return nil
		}
	}

	rawForFinal := ""
	if config.NeedsRaw {
		rawForFinal = winningResult.raw
	}
	trySend(newResult(bamlutils.StreamResultKindFinal, nil, winningResult.finalResult, rawForFinal, nil, false))

	return nil
}
