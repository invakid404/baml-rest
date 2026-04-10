// Package buildrequest provides the runtime orchestrators for the
// BuildRequest/StreamRequest call and streaming paths. This replaces the
// CallStream+OnTick pipeline for providers that support the modular API.
//
// The generated adapter code calls RunStreamOrchestration with
// provider-specific closures. This package handles SSE event consumption,
// delta extraction, ParseStream throttling, retry logic, and heartbeat
// emission — producing the same StreamResult channel shape as the legacy paths.
package buildrequest

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/bamlutils/retry"
	"github.com/invakid404/baml-rest/bamlutils/sse"
)

// parseBuildRequestEnv reads BAML_REST_USE_BUILD_REQUEST and returns its
// boolean interpretation. Extracted so the test can verify parsing logic
// independently of the cache.
func parseBuildRequestEnv() bool {
	v := os.Getenv("BAML_REST_USE_BUILD_REQUEST")
	switch strings.ToLower(v) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// useBuildRequestOnce caches the result of parseBuildRequestEnv. The env var
// is read once on first call and the result is reused for all subsequent
// calls. This avoids repeated os.Getenv (which acquires a lock) on every
// request dispatch — the generated router calls UseBuildRequest() twice per
// request (once for the call path, once for the stream path).
var useBuildRequestOnce sync.Once
var useBuildRequestCached bool

// UseBuildRequest returns true if the BuildRequest/StreamRequest paths are enabled.
// Controlled by the BAML_REST_USE_BUILD_REQUEST environment variable.
// When false, the legacy CallStream+OnTick path is used for all providers.
// The environment variable is read once and cached for the process lifetime.
func UseBuildRequest() bool {
	useBuildRequestOnce.Do(func() {
		useBuildRequestCached = parseBuildRequestEnv()
	})
	return useBuildRequestCached
}

// supportedProviders is the set of providers whose SSE format is handled by
// ExtractDeltaFromText. This must match the switch cases in that function
// exactly — any provider not in this set falls back to the legacy path.
var supportedProviders = map[string]bool{
	"openai":           true,
	"openai-generic":   true,
	"azure-openai":     true,
	"ollama":           true,
	"openrouter":       true,
	"openai-responses": true,
	"anthropic":        true,
	"google-ai":        true,
	"vertex-ai":        true,
	// "aws-bedrock" is intentionally absent: BAML's StreamRequest errors for it,
	// and its SSE format requires special handling not suited to BuildRequest.
}

// IsProviderSupported returns true if the provider's SSE format is handled
// by ExtractDeltaFromText and the provider supports BAML StreamRequest.
// Unknown providers fall back to the legacy CallStream+OnTick path.
func IsProviderSupported(provider string) bool {
	return supportedProviders[provider]
}

// callSupportedProviders is the set of providers whose non-streaming JSON
// response format is handled by ExtractResponseContent. This is separate
// from supportedProviders because the streaming path requires SSE format
// compatibility while the non-streaming path requires JSON response format
// compatibility — different constraints that may evolve independently.
//
// "aws-bedrock" is excluded pending verification that BAML's Request API
// supports Bedrock (see design doc Section 5.3).
var callSupportedProviders = map[string]bool{
	"openai":           true,
	"openai-generic":   true,
	"azure-openai":     true,
	"ollama":           true,
	"openrouter":       true,
	"openai-responses": true,
	"anthropic":        true,
	"google-ai":        true,
	"vertex-ai":        true,
}

// IsCallProviderSupported returns true if the provider's non-streaming JSON
// response format is handled by ExtractResponseContent and the provider
// supports BAML's Request API. Unknown providers fall back to the legacy
// CallStream+OnTick path.
func IsCallProviderSupported(provider string) bool {
	return callSupportedProviders[provider]
}

// ResolveProvider determines the provider for a function by checking the
// runtime ClientRegistry override first, then falling back to the static
// introspected default. The resolution order is:
//  1. adapter.ClientRegistryProvider() — primary client's provider
//  2. Named client override: if the function's default client name appears
//     in client_registry.clients, use that client's provider
//  3. Static introspected default (passed as fallback)
func ResolveProvider(adapter bamlutils.Adapter, defaultClientName string, introspectedProvider string) string {
	// Check primary client override first
	if p := adapter.ClientRegistryProvider(); p != "" {
		return p
	}

	// Check if the function's default client was overridden by name
	if reg := adapter.OriginalClientRegistry(); reg != nil && defaultClientName != "" {
		for _, client := range reg.Clients {
			if client == nil {
				continue
			}
			if client.Name == defaultClientName {
				return client.Provider
			}
		}
	}

	return introspectedProvider
}

// ResolveRetryPolicy determines the retry policy for a function. Resolution
// order mirrors ResolveProvider so the same runtime client drives both
// provider selection and retry behaviour:
//  1. Per-request override from adapter.RetryConfig() (__baml_options__.retry)
//  2. Primary client's retry_policy from runtime client_registry
//  3. Named-client retry_policy for the function's default client in
//     client_registry.clients
//  4. Static introspected default from FunctionRetryPolicy → RetryPolicies
//
// introspectedPolicyName is the policy name from FunctionRetryPolicy[method].
// introspectedPolicies is the full RetryPolicies map from introspection.
func ResolveRetryPolicy(
	adapter bamlutils.Adapter,
	defaultClientName string,
	introspectedPolicyName string,
	introspectedPolicies map[string]*retry.Policy,
) *retry.Policy {
	// 1. Per-request override takes highest priority
	if rc := adapter.RetryConfig(); rc != nil {
		return RetryConfigToPolicy(rc)
	}

	// 2. Client-level retry_policy from runtime client_registry.
	if reg := adapter.OriginalClientRegistry(); reg != nil {
		// When primary is set, only check the primary client's retry_policy.
		// Do NOT fall through to the default client — the primary client
		// is the one actually being used for streaming, so inheriting a
		// different client's retry policy would be incorrect. If the primary
		// has no retry_policy, skip straight to the introspected default.
		if reg.Primary != nil {
			for _, client := range reg.Clients {
				if client == nil {
					continue
				}
				if client.Name == *reg.Primary && client.RetryPolicy != nil {
					if p, ok := introspectedPolicies[*client.RetryPolicy]; ok {
						return p
					}
				}
			}
			// Primary is set but has no retry_policy — fall through to
			// introspected default (step 3), NOT to the default client.
		} else if defaultClientName != "" {
			// No primary set — check the function's default client name
			for _, client := range reg.Clients {
				if client == nil {
					continue
				}
				if client.Name == defaultClientName && client.RetryPolicy != nil {
					if p, ok := introspectedPolicies[*client.RetryPolicy]; ok {
						return p
					}
				}
			}
		}
	}

	// 4. Static introspected default
	if introspectedPolicyName != "" {
		return introspectedPolicies[introspectedPolicyName]
	}

	return nil
}

// RetryConfigToPolicy converts a bamlutils.RetryConfig (from __baml_options__.retry)
// into a retry.Policy that the orchestrator can use.
func RetryConfigToPolicy(rc *bamlutils.RetryConfig) *retry.Policy {
	if rc == nil {
		return nil
	}
	p := &retry.Policy{
		MaxRetries: rc.MaxRetries,
		StrategyConfig: &retry.StrategyConfig{
			Type:       rc.Strategy,
			DelayMs:    rc.DelayMs,
			Multiplier: rc.Multiplier,
			MaxDelayMs: rc.MaxDelayMs,
		},
	}
	p.ResolveStrategy()
	return p
}

// resolveChildProvider returns the provider for a named client, checking
// runtime client_registry overrides before the introspected defaults. This
// mirrors ResolveProvider's per-client resolution (lines 104-112) so that
// runtime overrides that change a child's provider are respected for both
// extraction format selection and the supported-provider gate.
func resolveChildProvider(reg *bamlutils.ClientRegistry, clientName string, introspectedProviders map[string]string) string {
	if reg != nil {
		for _, client := range reg.Clients {
			if client != nil && client.Name == clientName && client.Provider != "" {
				return client.Provider
			}
		}
	}
	return introspectedProviders[clientName]
}

func findRuntimeClient(reg *bamlutils.ClientRegistry, clientName string) *bamlutils.ClientProperty {
	if reg == nil {
		return nil
	}
	for _, client := range reg.Clients {
		if client != nil && client.Name == clientName {
			return client
		}
	}
	return nil
}

func parseRuntimeStrategyOption(v any) []string {
	splitStrategy := func(s string) []string {
		s = strings.TrimSpace(s)
		if strings.HasPrefix(s, "strategy ") {
			s = strings.TrimSpace(strings.TrimPrefix(s, "strategy "))
		}
		if strings.HasPrefix(s, "[") {
			s = s[1:]
		}
		if closeIdx := strings.LastIndex(s, "]"); closeIdx >= 0 {
			s = s[:closeIdx]
		}
		parts := strings.FieldsFunc(s, func(r rune) bool {
			return r == ',' || r == ' ' || r == '\t' || r == '\n'
		})
		chain := make([]string, 0, len(parts))
		for _, part := range parts {
			part = strings.TrimSpace(part)
			if part != "" {
				chain = append(chain, part)
			}
		}
		return chain
	}

	switch vv := v.(type) {
	case string:
		return splitStrategy(vv)
	case []string:
		chain := make([]string, 0, len(vv))
		for _, item := range vv {
			item = strings.TrimSpace(item)
			if item != "" {
				chain = append(chain, item)
			}
		}
		return chain
	case []any:
		chain := make([]string, 0, len(vv))
		for _, item := range vv {
			str, ok := item.(string)
			if !ok {
				return nil
			}
			str = strings.TrimSpace(str)
			if str != "" {
				chain = append(chain, str)
			}
		}
		return chain
	default:
		return nil
	}
}

func resolveFallbackStrategyChain(reg *bamlutils.ClientRegistry, clientName string, introspectedChains map[string][]string) []string {
	if runtimeClient := findRuntimeClient(reg, clientName); runtimeClient != nil && runtimeClient.Options != nil {
		if rawStrategy, ok := runtimeClient.Options["strategy"]; ok {
			if chain := parseRuntimeStrategyOption(rawStrategy); len(chain) > 0 {
				return chain
			}
		}
	}
	return introspectedChains[clientName]
}

// ResolveFallbackChain determines whether a function's client is a fallback
// strategy client and, if so, returns the ordered child chain and a map of
// child client names to their providers. Returns nil, nil if the function
// does not use a fallback chain or if any child's provider is unsupported.
//
// Parameters:
//   - adapter: the request adapter (for runtime client_registry overrides)
//   - defaultClientName: the function's default client from introspection
//   - fallbackChains: introspected map of strategy client → child list
//   - clientProviders: introspected map of client name → provider string
//   - isProviderSupported: provider support check function (streaming vs call)
func ResolveFallbackChain(
	adapter bamlutils.Adapter,
	defaultClientName string,
	fallbackChains map[string][]string,
	clientProviders map[string]string,
	isProviderSupported func(string) bool,
) (chain []string, providers map[string]string) {
	reg := adapter.OriginalClientRegistry()

	// Determine which client name to look up in fallbackChains.
	// Runtime primary override takes precedence over the static default.
	clientName := defaultClientName
	if reg != nil && reg.Primary != nil {
		clientName = *reg.Primary
	}

	// Only support baml-fallback in the BuildRequest path. baml-roundrobin
	// requires cross-request state to distribute load (each new request
	// should start at a different child), which the per-request orchestrator
	// does not have. Without it, round-robin degrades to fallback (always
	// starting at child 0), which is silently wrong. Leave round-robin on
	// the legacy path where the BAML runtime handles the rotation.
	parentProvider := resolveChildProvider(reg, clientName, clientProviders)
	if parentProvider != "baml-fallback" {
		return nil, nil
	}

	chain = resolveFallbackStrategyChain(reg, clientName, fallbackChains)
	if len(chain) == 0 {
		return nil, nil
	}

	// Resolve each child's provider, checking runtime client_registry
	// overrides first (same precedence as ResolveProvider). If any child
	// is unsupported or has no provider, fall back to the legacy path.
	providers = make(map[string]string, len(chain))
	for _, child := range chain {
		p := resolveChildProvider(reg, child, clientProviders)
		if p == "" || !isProviderSupported(p) {
			return nil, nil
		}
		providers[child] = p
	}

	return chain, providers
}

// StreamConfig holds the configuration for a single streaming request.
type StreamConfig struct {
	// Provider is the LLM provider name (e.g., "openai", "anthropic").
	// Used for SSE delta extraction. For single-provider clients, this
	// is the only provider used. For fallback chains, this is ignored
	// in favor of per-attempt provider resolution via ClientProviders.
	Provider string

	// RetryPolicy is the retry policy for this request. Nil means no retries.
	RetryPolicy *retry.Policy

	// NeedsPartials is true if the caller wants intermediate parsed partials.
	NeedsPartials bool

	// NeedsRaw is true if the caller wants raw LLM response text.
	NeedsRaw bool

	// ParseThrottleInterval is the minimum time between ParseStream calls.
	// Zero means parse on every SSE event (no throttling).
	ParseThrottleInterval time.Duration

	// FallbackChain is the ordered list of child client names for fallback
	// strategies. When non-empty, each retry attempt walks the entire chain
	// in order; if any child succeeds, the attempt returns immediately.
	// When empty, the single Provider is used for all attempts.
	FallbackChain []string

	// ClientProviders maps child client names to their provider strings.
	// Used with FallbackChain to resolve the provider for each attempt.
	ClientProviders map[string]string
}

// BuildRequestFunc builds an HTTP request for streaming by calling
// StreamRequest.Method(ctx, args, opts...) and converting the result
// to an llmhttp.Request. The clientOverride parameter, when non-empty,
// forces the request to use a specific child client (for fallback chain
// iteration). This is provided by the generated adapter code.
type BuildRequestFunc func(ctx context.Context, clientOverride string) (*llmhttp.Request, error)

// ParseStreamFunc parses accumulated raw text into a typed partial.
// This wraps ParseStream.Method(accumulated, opts...) from the generated code.
type ParseStreamFunc func(ctx context.Context, accumulated string) (any, error)

// ParseFinalFunc parses complete raw text into the typed final result.
// This wraps Parse.Method(accumulated, opts...) from the generated code.
type ParseFinalFunc func(ctx context.Context, accumulated string) (any, error)

// NewResultFunc creates a new pooled StreamResult with the given fields.
// This is provided by the generated adapter code (the per-method pool getter).
type NewResultFunc func(kind bamlutils.StreamResultKind, stream, final any, raw string, err error, reset bool) bamlutils.StreamResult

// RunStreamOrchestration executes the BuildRequest streaming path.
//
// It builds an HTTP request, executes it, parses SSE events, extracts deltas,
// optionally calls ParseStream for typed partials, and emits StreamResult
// values on the output channel. Retries are handled per the config's policy.
//
// The output channel is NOT closed by this function — the caller (generated
// code) is responsible for closing it.
//
// This function blocks until streaming is complete or the context is cancelled.
func RunStreamOrchestration(
	ctx context.Context,
	out chan<- bamlutils.StreamResult,
	config *StreamConfig,
	httpClient *llmhttp.Client,
	buildRequest BuildRequestFunc,
	parseStream ParseStreamFunc,
	parseFinal ParseFinalFunc,
	newResult NewResultFunc,
) error {
	if httpClient == nil {
		httpClient = llmhttp.DefaultClient
	}

	// Validate that the first provider is supported.
	if len(config.FallbackChain) == 0 {
		if config.Provider == "" || !IsProviderSupported(config.Provider) {
			return fmt.Errorf("buildrequest: unsupported or empty provider %q", config.Provider)
		}
	} else {
		first := config.ClientProviders[config.FallbackChain[0]]
		if first == "" || !IsProviderSupported(first) {
			return fmt.Errorf("buildrequest: unsupported or empty provider %q", first)
		}
	}

	var heartbeatSent atomic.Bool

	// Send initial heartbeat for hung detection
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

	// tryOneStreamChild runs a single child's streaming attempt.
	tryOneStreamChild := func(provider, clientOverride string) (any, error) {
		req, err := buildRequest(ctx, clientOverride)
		if err != nil {
			return nil, fmt.Errorf("buildrequest: failed to build request: %w", err)
		}

		resp, err := httpClient.ExecuteStream(ctx, req)
		if err != nil {
			return nil, err
		}
		defer resp.Close()

		// Send heartbeat on connection success
		sendHeartbeat()

		var parseableAccumulated strings.Builder
		var rawAccumulated strings.Builder
		var lastParseTime time.Time

		trySendPartial := func(parsed any, rawDelta string) error {
			r := newResult(bamlutils.StreamResultKindStream, parsed, nil, rawDelta, nil, false)
			select {
			case out <- r:
			case <-ctx.Done():
				r.Release()
				return ctx.Err()
			default:
				r.Release() // Drop partial/raw delta — buffer full
			}
			return nil
		}

		for ev := range resp.Events {
			// Extract parseable/raw delta content from the SSE event using this
			// attempt's provider. Anthropic thinking deltas contribute only to raw.
			delta, extractErr := sse.ExtractDeltaPartsFromText(provider, ev.Data)
			if extractErr != nil {
				// Extraction error — fail the attempt so retry logic can handle it
				// rather than silently accumulating incomplete text.
				return nil, fmt.Errorf("buildrequest: delta extraction failed: %w", extractErr)
			}
			if delta.Raw == "" {
				continue
			}

			rawAccumulated.WriteString(delta.Raw)
			if delta.Parseable != "" {
				parseableAccumulated.WriteString(delta.Parseable)
			}

			if config.NeedsPartials && delta.Parseable == "" {
				if config.NeedsRaw {
					if err := trySendPartial(nil, delta.Raw); err != nil {
						return nil, err
					}
				}
				continue
			}

			// Emit partial if needed.
			// Non-blocking sends for partials/deltas: drop when the output
			// buffer is full so the LLM stream keeps draining. This matches
			// the legacy path behavior which intentionally drops non-reset
			// partials rather than coupling upstream HTTP reads to downstream
			// consumer backpressure.
			if config.NeedsPartials && parseStream != nil {
				shouldParse := config.ParseThrottleInterval == 0 ||
					time.Since(lastParseTime) >= config.ParseThrottleInterval

				if shouldParse {
					// Update throttle timestamp regardless of parse success/failure
					// so that repeated failures don't bypass the throttle interval.
					lastParseTime = time.Now()

					parsed, parseErr := parseStream(ctx, parseableAccumulated.String())
					if parseErr == nil && parsed != nil {
						rawForResult := ""
						if config.NeedsRaw {
							rawForResult = delta.Raw
						}
						if err := trySendPartial(parsed, rawForResult); err != nil {
							return nil, err
						}
					}
				}
			} else if config.NeedsRaw {
				// Raw-only mode: accumulate silently; full raw text is
				// included in the final result via rawForFinal.
			}
		}

		// Check for stream errors
		if streamErr := <-resp.Errc; streamErr != nil {
			return nil, fmt.Errorf("buildrequest: stream error: %w", streamErr)
		}

		// Parse the final result — let parseFinal decide whether an empty
		// completion is valid. The legacy path does not reject empty strings.
		fullRaw := rawAccumulated.String()

		finalResult, parseErr := parseFinal(ctx, parseableAccumulated.String())
		if parseErr != nil {
			return nil, fmt.Errorf("buildrequest: failed to parse final result: %w", parseErr)
		}

		// Emit final result
		rawForFinal := ""
		if config.NeedsRaw {
			rawForFinal = fullRaw
		}
		r := newResult(bamlutils.StreamResultKindFinal, nil, finalResult, rawForFinal, nil, false)
		select {
		case out <- r:
		case <-ctx.Done():
			r.Release()
			return nil, ctx.Err()
		}

		return finalResult, nil
	}

	// attemptFull tries the single provider or the entire fallback chain.
	// For fallback chains, each retry walks all children in order —
	// matching the BAML runtime where retries retry the entire strategy.
	attemptFull := func(_ int) (any, error) {
		if len(config.FallbackChain) == 0 {
			return tryOneStreamChild(config.Provider, "")
		}
		var lastErr error
		for i, child := range config.FallbackChain {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			default:
			}
			// Emit a reset signal before trying the next child so the
			// downstream discards any partial/raw state leaked by the
			// previous child's failed stream. Not needed before the first
			// child in the chain.
			if i > 0 && lastErr != nil {
				r := newResult(bamlutils.StreamResultKindStream, nil, nil, "", nil, true)
				select {
				case out <- r:
				case <-ctx.Done():
					r.Release()
					return nil, ctx.Err()
				}
				heartbeatSent.Store(false)
			}
			provider := config.ClientProviders[child]
			result, err := tryOneStreamChild(provider, child)
			if err == nil {
				return result, nil
			}
			lastErr = err
		}
		return nil, lastErr
	}

	// Execute with retries
	_, err := retry.Execute(ctx, config.RetryPolicy, attemptFull, func(attempt int) {
		// Emit reset signal so downstream discards accumulated partial state
		r := newResult(bamlutils.StreamResultKindStream, nil, nil, "", nil, true)
		select {
		case out <- r:
		case <-ctx.Done():
			r.Release()
		}

		// Reset heartbeat for new attempt
		heartbeatSent.Store(false)
	})

	if err != nil && ctx.Err() == nil {
		// Emit error result (skip if context was cancelled — cancellation is not an error)
		r := newResult(bamlutils.StreamResultKindError, nil, nil, "", err, false)
		select {
		case out <- r:
		case <-ctx.Done():
			r.Release()
		}
	}

	return nil // errors are communicated via the channel, not the return value
}
