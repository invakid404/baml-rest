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
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/buildrequest/roundrobin"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/bamlutils/retry"
	"github.com/invakid404/baml-rest/bamlutils/sse"
	"github.com/invakid404/baml-rest/bamlutils/strategyparse"
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

// parseDisableCallBuildRequestEnv reads BAML_REST_DISABLE_CALL_BUILD_REQUEST
// and returns its boolean interpretation. Extracted so the test can verify
// parsing logic independently of the cache.
func parseDisableCallBuildRequestEnv() bool {
	v := os.Getenv("BAML_REST_DISABLE_CALL_BUILD_REQUEST")
	switch strings.ToLower(v) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// disableCallBuildRequestOnce caches the result of the env lookup so
// IsCallProviderSupported stays hot. Same rationale as useBuildRequestOnce.
var disableCallBuildRequestOnce sync.Once
var disableCallBuildRequestCached bool

// disableCallBuildRequest returns true when BAML_REST_DISABLE_CALL_BUILD_REQUEST
// is set, forcing the non-streaming Request API off for all providers. The
// call-mode router block then declines and /call{,-with-raw} fall through to
// the stream-accumulation bridge (when StreamRequest is available) or legacy.
//
// Primary uses:
//
//   - Exercising the bridge path in integration tests without relying on an
//     organic divergence between callSupportedProviders and supportedProviders.
//   - An operational escape hatch if the non-streaming Request API regresses
//     for a provider; flipping this forces bridging at a latency cost while a
//     fix ships.
func disableCallBuildRequest() bool {
	disableCallBuildRequestOnce.Do(func() {
		disableCallBuildRequestCached = parseDisableCallBuildRequestEnv()
	})
	return disableCallBuildRequestCached
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
//
// Returns false for every provider when BAML_REST_DISABLE_CALL_BUILD_REQUEST
// is set, which routes /call{,-with-raw} through the stream-accumulation
// bridge (or legacy, if StreamRequest is unavailable).
//
// Debug builds (-tags debug) additionally honour
// BAML_REST_CALL_UNSUPPORTED_PROVIDERS, a comma-separated list that marks
// specific providers as call-unsupported while keeping them stream-
// supported. This exists so integration tests can force the mixed-chain
// fall-through gate to fire without waiting for callSupportedProviders and
// supportedProviders to diverge organically. See debugFilterCallSupported
// in call_support_debug.go / call_support_stub.go.
func IsCallProviderSupported(provider string) bool {
	if disableCallBuildRequest() {
		return false
	}
	return debugFilterCallSupported(provider, callSupportedProviders[provider])
}

// ResolveProvider determines the provider for a function by checking the
// runtime ClientRegistry override first, then falling back to the static
// introspected default. The resolution order is:
//
//  1. adapter.ClientRegistryProvider() — primary client's provider as
//     pre-resolved by the adapter. Used only when non-empty; an empty
//     return falls through to direct registry inspection so a present-
//     empty primary can be distinguished from an absent primary
//     (the adapter cache collapses both into "").
//  2. Direct registry lookup of the primary client's clients[] entry,
//     presence-aware. Surfaces present-empty as "" so the caller can
//     route to legacy with PathReasonInvalidProviderOverride.
//  3. Named client override: if the function's default client name
//     appears in client_registry.clients with a present provider, use
//     it (also presence-aware).
//  4. Static introspected default (passed as fallback). Reached when
//     no override is present anywhere — preserves strategy-only and
//     presence-only registry overrides that intentionally don't carry
//     a provider.
//
// Presence semantics matter because BAML upstream's
// ClientProvider::from_str rejects empty provider strings
// (clientspec.rs:119-144); we mirror that by NOT falling through to
// the introspected provider when the registry sent an explicit
// "provider":"". See PR #192 cold-review-2 verdict-8 follow-up.
func ResolveProvider(adapter bamlutils.Adapter, defaultClientName string, introspectedProvider string) string {
	// Primary's provider via the adapter abstraction. Non-empty
	// short-circuit retained so adapter implementations that pre-
	// resolve the primary string keep working. An empty return
	// falls through to direct registry inspection — the adapter cache
	// collapses absent/present-empty into "" and we need the
	// distinction.
	if p := adapter.ClientRegistryProvider(); p != "" {
		return p
	}

	reg := adapter.OriginalClientRegistry()
	if reg != nil {
		// Primary client override (presence-aware): catches the
		// present-empty case that the adapter cache hides.
		if reg.Primary != nil && *reg.Primary != "" {
			for _, client := range reg.Clients {
				if client != nil && client.Name == *reg.Primary && client.IsProviderPresent() {
					return client.Provider
				}
			}
		}

		// Named client override on the function's default client.
		if defaultClientName != "" {
			for _, client := range reg.Clients {
				if client != nil && client.Name == defaultClientName && client.IsProviderPresent() {
					return client.Provider
				}
			}
		}
	}

	return introspectedProvider
}

// BuildRequestAPI identifies which BAML API drove a Path=="buildrequest"
// request. These values populate Metadata.BuildRequestAPI and the
// X-BAML-Build-Request-API response header.
const (
	// BuildRequestAPIRequest means the request used the non-streaming
	// Request API via RunCallOrchestration (one HTTP call, JSON response).
	BuildRequestAPIRequest = "request"
	// BuildRequestAPIStreamRequest means the request used the streaming
	// StreamRequest API via RunStreamOrchestration. For stream modes this
	// is the normal path. For call modes this signals the bridge: SSE is
	// accumulated and returned as a unary response because the non-streaming
	// Request API was not available for the resolved provider.
	BuildRequestAPIStreamRequest = "streamrequest"
)

// PathReason enumerates the classification reasons an orchestrator reports
// alongside the legacy-path decision. Each value is stable and appears in
// the X-BAML-Path-Reason response header and in metadata payloads.
const (
	// PathReasonEmptyProvider: the resolved client has no provider string.
	// Usually indicates a runtime override with a missing "provider" field
	// or a client name that isn't in either the runtime registry or the
	// introspected providers map. Routed to legacy because BuildRequest
	// cannot make routing decisions without a provider.
	PathReasonEmptyProvider = "empty-provider"
	// PathReasonUnsupportedProvider: the resolved provider is not in the
	// BuildRequest supported set (e.g. aws-bedrock).
	PathReasonUnsupportedProvider = "unsupported-provider"
	// PathReasonFallbackEmptyChain: the resolved strategy client resolves
	// to baml-fallback but has no children, so BuildRequest has nothing
	// to walk.
	PathReasonFallbackEmptyChain = "fallback-empty-chain"
	// PathReasonFallbackEmptyChildProvider: the chain includes a child
	// whose provider is empty (unknown). The whole chain degrades to legacy
	// because an unknown provider can't be routed in mixed mode.
	PathReasonFallbackEmptyChildProvider = "fallback-empty-child-provider"
	// PathReasonFallbackAllLegacy: every child in the chain resolves to an
	// unsupported provider, so the whole chain runs on legacy. Deliberate
	// configuration — no operator alert.
	PathReasonFallbackAllLegacy = "fallback-all-legacy"
	// PathReasonRoundRobin: the resolved strategy is baml-roundrobin and
	// the request reached the legacy metadata-plan builder unresolved.
	// In modern adapters (BAML >= 0.219.0 with SupportsWithClient) top-
	// level RR is unwrapped by ResolveEffectiveClient before dispatch
	// and does NOT land here; this reason only fires on older adapters
	// that have no WithClient option, or as a defensive path when RR
	// resolution failed (cycle / empty chain). The string value is
	// preserved for backwards-compatibility with any metric parsers.
	PathReasonRoundRobin = "roundrobin-legacy-only"
	// PathReasonFallbackRoundRobinChildLegacy: the fallback chain
	// contains a baml-roundrobin child. Top-level RR is centralised
	// across workers via the SharedState broker socket, but nested RR
	// inside a fallback chain is intentionally NOT unwrapped by the
	// BuildRequest resolver — that would require the broad
	// "preselect one RR leaf at each fallback position" design (PR
	// #192 cold-review finding 2, deferred). Such chains still work,
	// but the RR child is handed to BAML's runtime on each worker,
	// which means its rotation is per-worker, not centralised.
	// Surfaced here so operators can spot the composition and know
	// not to expect fleet-wide rotation for the nested client.
	PathReasonFallbackRoundRobinChildLegacy = "fallback-roundrobin-child-legacy"
	// PathReasonBuildRequestDisabled: BAML_REST_USE_BUILD_REQUEST is off.
	// Deliberate configuration — no operator alert.
	PathReasonBuildRequestDisabled = "buildrequest-disabled"
	// PathReasonInvalidStrategyOverride: a runtime client_registry entry
	// supplied an `options.strategy` value that ParseStrategyOption could
	// not interpret as a non-empty bracketed list — bare tokens, mismatched
	// brackets, empty arrays, heterogeneous []any with non-string elements,
	// etc. BAML upstream rejects these inputs in ensure_strategy
	// (baml-lib/llm-client/src/clients/helpers.rs:790-829) with
	// "strategy must be an array" or "strategy must not be empty"; we
	// route the request to legacy so BAML's runtime emits the canonical
	// error rather than silently using the introspected chain. See
	// PR #192 cold-review-2 finding 1.
	PathReasonInvalidStrategyOverride = "invalid-strategy-override"
	// PathReasonInvalidProviderOverride: a runtime client_registry entry
	// explicitly supplied an empty `provider` value (`"provider": ""`).
	// BAML upstream's ClientProvider::from_str rejects empty provider
	// strings (clientspec.rs:119-144); we mirror that by routing the
	// request to legacy rather than silently using the introspected
	// fallback, so BAML emits its native invalid-provider error to the
	// caller. Distinct from PathReasonEmptyProvider (which fires when
	// no provider is configured anywhere) so operators reading the
	// header can tell "operator typo'd a registry entry" apart from
	// "client has no provider declared". See PR #192 cold-review-2
	// verdict-8 follow-up.
	PathReasonInvalidProviderOverride = "invalid-provider-override"
	// PathReasonInvalidRoundRobinStartOverride: a runtime
	// client_registry entry on a baml-roundrobin client supplied an
	// `options.start` value that inspectStartOverride could not
	// interpret as a representable Go int — strings (including
	// numeric strings like "1"), fractional floats, booleans, slices,
	// maps, etc. BAML upstream parses RR `start` via
	// PropertyHandler.ensure_int (round_robin.rs:75); we route to
	// legacy so BAML emits the canonical integer-options error rather
	// than us silently randomising. Distinct from
	// PathReasonInvalidStrategyOverride so operators can tell the
	// failing option apart in the X-BAML-Path-Reason header. See PR
	// #192 cold-review-3 finding 3.
	PathReasonInvalidRoundRobinStartOverride = "invalid-round-robin-start-override"
)

// ProviderResolution describes the outcome of resolving a request's routing
// path. Emitted by ResolveProviderWithReason for single-provider clients and
// by ResolveFallbackChainWithReason for strategy clients; the shared shape
// lets the generated router build a metadata plan from either.
type ProviderResolution struct {
	// Client is the runtime client name after runtime-override resolution.
	// For a strategy client, this is the strategy name (e.g. "baml-fallback").
	Client string
	// Provider is the resolved provider string. Empty for strategy clients.
	Provider string
	// Strategy is the strategy type for strategy clients
	// (e.g. "baml-fallback", "baml-roundrobin"), empty for non-strategies.
	Strategy string
	// Path is "buildrequest" or "legacy".
	Path string
	// PathReason is one of the PathReason* constants when Path=="legacy",
	// empty when Path=="buildrequest".
	PathReason string
}

// ResolveEffectiveClient unwraps any baml-roundrobin wrappers around the
// function's default client (after applying the runtime primary override)
// and returns the leaf client name that the orchestrator should treat as
// the effective request target. When the leaf is itself a fallback client
// the chain resolution happens downstream — this function only strips
// round-robin layers.
//
// rrInfo describes the outermost round-robin decision (name, children,
// index, selected child). It is nil when no round-robin unwrap occurred.
// Callers should copy rrInfo into the request metadata so operators can
// observe which child was selected.
//
// advancer may be nil, in which case static round-robin resolution degrades
// to per-request random selection. Production callers are expected to pass
// the package-level advancer set at generated-adapter init — either the
// in-process Coordinator (standalone) or a per-request RemoteAdvancer
// installed by the worker on the host's SharedState socket.
//
// RR rotation cadence — intentional divergence from BAML upstream.
// ResolveEffectiveClient advances the RR counter exactly once per REST
// request at router entry. BAML's runtime advances once per outbound
// LLM call under RR scope (see baml-runtime orchestrator/mod.rs:232
// and :294), which means a leaf's retry_policy bleeds into rotation
// distribution — three retries on a single REST request burn three
// persistent counter slots. We treat rotation and per-child retry as
// orthogonal: a child's transient failures should not reduce its share
// of the rotation, and operators reading X-BAML-RoundRobin-Index get
// a deterministic 1:1 cadence with request count. The divergence is
// observable (next-request child selection differs from BAML under
// sustained retry activity) but is intentional for this PR's
// load-distribution semantic, not an accidental omission. A follow-up
// could thread per-attempt advancement through the retry loop if
// operators actually need BAML-parity rotation under failure — that
// requires redesigning the advancer call site to fire per-attempt
// rather than once at router entry.
func ResolveEffectiveClient(
	adapter bamlutils.Adapter,
	defaultClientName string,
	fallbackChains map[string][]string,
	clientProviders map[string]string,
	advancer roundrobin.Advancer,
) (effective string, rrInfo *bamlutils.RoundRobinInfo, err error) {
	reg := adapter.OriginalClientRegistry()

	clientName := defaultClientName
	if reg != nil && reg.Primary != nil && *reg.Primary != "" {
		clientName = *reg.Primary
	}

	// A per-request Advancer installed on the adapter (typically a
	// RemoteAdvancer talking to the host SharedState socket) takes
	// precedence over the package-level coordinator. This is how pool-
	// managed workers observe a single rotation across the fleet —
	// without it, each worker would rotate off its own coordinator and
	// round-robin would collapse into independent per-worker draws.
	if reqAdvancer := adapter.RoundRobinAdvancer(); reqAdvancer != nil {
		advancer = reqAdvancer
	}

	res, err := roundrobin.Resolve(roundrobin.ResolveInput{
		ClientName:      clientName,
		Registry:        reg,
		FallbackChains:  fallbackChains,
		ClientProviders: clientProviders,
		Advancer:        advancer,
	})
	if err != nil {
		// Invalid runtime overrides on `options.strategy` or
		// `options.start` are not hard failures — the dispatcher should
		// fall through to legacy so BAML's runtime emits the canonical
		// ensure_strategy / ensure_int error. Returning the un-unwrapped
		// client name lets the codegen-side support gate fail (the
		// outer client is a strategy provider, which isn't in
		// supportedProviders) and route to legacy, where
		// ResolveProviderWithReason / BuildLegacyMetadataPlanForClient
		// surface PathReasonInvalidStrategyOverride or
		// PathReasonInvalidRoundRobinStartOverride for observability.
		if errors.Is(err, roundrobin.ErrInvalidStrategyOverride) ||
			errors.Is(err, roundrobin.ErrInvalidStartOverride) {
			return clientName, nil, nil
		}
		return "", nil, err
	}
	return res.Selected, res.Info, nil
}

// ResolvePrimaryClient returns the request's effective client name after
// applying the runtime client_registry primary override, without doing
// any baml-roundrobin unwrap. Used by adapter versions that cannot honor
// a resolved leaf child at dispatch time (BAML < 0.219.0 has no
// WithClient CallOption, so the generated router passes the declared
// strategy client name to BAML and lets BAML's own runtime rotate).
//
// Advancing our coordinator on those adapters would report a selection
// in the metadata headers that doesn't match BAML's actual rotation, so
// the generator skips the Resolve call entirely and uses this helper.
// Returns empty string if defaultClientName is empty and no primary
// override is set — callers should treat that the same as they treat
// an empty FunctionClient entry.
func ResolvePrimaryClient(adapter bamlutils.Adapter, defaultClientName string) string {
	clientName := defaultClientName
	if reg := adapter.OriginalClientRegistry(); reg != nil && reg.Primary != nil && *reg.Primary != "" {
		clientName = *reg.Primary
	}
	return clientName
}

// ResolveProviderWithReason is ResolveProvider with the routing decision
// classified for observability. Does not consider fallback chains — use
// ResolveFallbackChainWithReason first when the resolved provider might be
// a strategy name (baml-fallback, baml-roundrobin).
//
// If the resolved provider is a strategy, PathReason is set accordingly
// (fallback details come from ResolveFallbackChainWithReason; roundrobin
// is legacy-only). Otherwise Path is "buildrequest" when the provider is
// supported and "legacy" otherwise.
func ResolveProviderWithReason(
	adapter bamlutils.Adapter,
	defaultClientName string,
	introspectedProvider string,
	isProviderSupported func(string) bool,
) ProviderResolution {
	provider := ResolveProvider(adapter, defaultClientName, introspectedProvider)
	// Fold strategy aliases (RR + fallback) onto canonical spellings so
	// the classification switch below sees one form per strategy. A
	// runtime registry entry using the bare "fallback" or "round-robin"
	// alias would otherwise slip past the "baml-fallback" /
	// "baml-roundrobin" arms and get treated as an unsupported single-
	// provider string. See PR #192 cold-review-2 finding 2.
	provider = normalizeStrategyProvider(provider)

	// Determine the effective client name after primary override. This is
	// what the metadata Client field should report, even when the provider
	// resolution ultimately falls back to the introspected default.
	clientName := defaultClientName
	if reg := adapter.OriginalClientRegistry(); reg != nil && reg.Primary != nil && *reg.Primary != "" {
		clientName = *reg.Primary
	}

	res := ProviderResolution{Client: clientName, Provider: provider}

	// Both strategy arms below check for an invalid runtime
	// `options.strategy` override and surface PathReasonInvalidStrategyOverride
	// so the legacy plan distinguishes "operator typo" from a valid
	// strategy that legitimately routes legacy. Without this, a malformed
	// override would surface as PathReasonRoundRobin or empty (fallback)
	// on the legacy plan, hiding the actual classification from
	// operators reading metadata. The codegen-side gate is unchanged —
	// the request still falls through to legacy because BuildRequest
	// can't drive a strategy client whose chain we refuse to resolve.
	reg := adapter.OriginalClientRegistry()

	switch provider {
	case "":
		res.Path = "legacy"
		// Distinguish "no provider configured anywhere" from "registry
		// supplied an explicit empty provider string". Both surface as
		// "" out of ResolveProvider, but the latter is a malformed
		// runtime override that BAML's ClientProvider::from_str
		// rejects (clientspec.rs:119-144); operators reading
		// X-BAML-Path-Reason should see the actual cause.
		//
		// The check covers all three lookup keys ResolveProvider
		// consults so a present-empty entry on any of them surfaces
		// the right reason:
		//
		//   - clientName (= primary when set, else defaultClientName):
		//     the client metadata reports.
		//   - reg.Primary: the primary override itself, in case the
		//     primary cache hid a present-empty provider.
		//   - defaultClientName: the function's declared default,
		//     which ResolveProvider falls through to when the primary
		//     entry has no provider key. CodeRabbit verdict-9 caught
		//     this gap: with primary set and no primary `clients[]`
		//     entry, ResolveProvider returns "" from the default
		//     lookup but the classifier had no checkpoint for it,
		//     reporting PathReasonEmptyProvider instead of the
		//     malformed-override reason.
		switch {
		case hasInvalidProviderOverride(reg, clientName):
			res.PathReason = PathReasonInvalidProviderOverride
		case reg != nil && reg.Primary != nil && *reg.Primary != "" && hasInvalidProviderOverride(reg, *reg.Primary):
			res.PathReason = PathReasonInvalidProviderOverride
		case hasInvalidProviderOverride(reg, defaultClientName):
			res.PathReason = PathReasonInvalidProviderOverride
		default:
			res.PathReason = PathReasonEmptyProvider
		}
	case "baml-fallback":
		res.Strategy = "baml-fallback"
		res.Path = "legacy"
		if _, present, valid := inspectStrategyOverride(reg, clientName); present && !valid {
			res.PathReason = PathReasonInvalidStrategyOverride
		}
		// Otherwise PathReason is intentionally left empty here.
		// Callers that care about the fallback classification
		// (BuildLegacyMetadataPlan, router) invoke
		// ResolveFallbackChainWithReason for the definitive reason —
		// empty-chain, empty-child-provider, all-legacy, or ""
		// (drivable). Pre-seeding a reason here would shadow the real
		// classification when the request goes legacy for unrelated
		// reasons (feature gate off, etc.) on a perfectly valid chain.
	case "baml-roundrobin":
		res.Strategy = "baml-roundrobin"
		res.Path = "legacy"
		// Strategy / start invalidity both route to legacy via the same
		// ErrInvalid* sentinel translation in ResolveEffectiveClient;
		// the metadata reason just records WHICH option failed so
		// operators reading X-BAML-Path-Reason can distinguish them.
		// Strategy is checked first since `strategy` is the primary RR
		// option and BAML upstream parses it before `start`.
		if _, present, valid := inspectStrategyOverride(reg, clientName); present && !valid {
			res.PathReason = PathReasonInvalidStrategyOverride
		} else if hasInvalidStartOverride(reg, clientName) {
			res.PathReason = PathReasonInvalidRoundRobinStartOverride
		} else {
			res.PathReason = PathReasonRoundRobin
		}
	default:
		if isProviderSupported != nil && isProviderSupported(provider) {
			res.Path = "buildrequest"
		} else {
			res.Path = "legacy"
			res.PathReason = PathReasonUnsupportedProvider
		}
	}

	return res
}

// BuildSingleProviderPlan constructs planned metadata for a request routed
// through the BuildRequest path with a single (non-strategy) client. Called
// by the generated router once the provider has been resolved and the
// support gate has been cleared, so Path is always "buildrequest".
//
// buildRequestAPI is one of BuildRequestAPIRequest /
// BuildRequestAPIStreamRequest (see constants above).
func BuildSingleProviderPlan(
	adapter bamlutils.Adapter,
	defaultClientName string,
	provider string,
	retryPolicy *retry.Policy,
	buildRequestAPI string,
) *bamlutils.Metadata {
	clientName := defaultClientName
	if reg := adapter.OriginalClientRegistry(); reg != nil && reg.Primary != nil && *reg.Primary != "" {
		clientName = *reg.Primary
	}
	plan := &bamlutils.Metadata{
		Phase:           bamlutils.MetadataPhasePlanned,
		Path:            "buildrequest",
		BuildRequestAPI: buildRequestAPI,
		Client:          clientName,
		Provider:        provider,
		RetryPolicy:     EncodeRetryPolicy(retryPolicy),
	}
	if retryPolicy != nil {
		m := retryPolicy.MaxRetries
		plan.RetryMax = &m
	}
	return plan
}

// BuildSingleProviderPlanForClient is the primary-override-free sibling of
// BuildSingleProviderPlan. Use after ResolveEffectiveClient has already
// produced the effective client name (post-primary and post-RR unwrap) —
// clientName is recorded verbatim as plan.Client.
func BuildSingleProviderPlanForClient(
	clientName string,
	provider string,
	retryPolicy *retry.Policy,
	buildRequestAPI string,
) *bamlutils.Metadata {
	plan := &bamlutils.Metadata{
		Phase:           bamlutils.MetadataPhasePlanned,
		Path:            "buildrequest",
		BuildRequestAPI: buildRequestAPI,
		Client:          clientName,
		Provider:        provider,
		RetryPolicy:     EncodeRetryPolicy(retryPolicy),
	}
	if retryPolicy != nil {
		m := retryPolicy.MaxRetries
		plan.RetryMax = &m
	}
	return plan
}

// BuildFallbackChainPlan constructs planned metadata for a request routed
// through the BuildRequest path as a fallback strategy. chain/providers/
// legacyChildren are the resolved values from ResolveFallbackChain.
//
// buildRequestAPI is one of BuildRequestAPIRequest /
// BuildRequestAPIStreamRequest (see constants above).
func BuildFallbackChainPlan(
	adapter bamlutils.Adapter,
	defaultClientName string,
	chain []string,
	providers map[string]string,
	legacyChildren map[string]bool,
	retryPolicy *retry.Policy,
	buildRequestAPI string,
	pathReason string,
) *bamlutils.Metadata {
	clientName := defaultClientName
	if reg := adapter.OriginalClientRegistry(); reg != nil && reg.Primary != nil && *reg.Primary != "" {
		clientName = *reg.Primary
	}
	plan := &bamlutils.Metadata{
		Phase:           bamlutils.MetadataPhasePlanned,
		Path:            "buildrequest",
		BuildRequestAPI: buildRequestAPI,
		Client:          clientName,
		Strategy:        "baml-fallback",
		PathReason:      pathReason,
		Chain:           append([]string(nil), chain...),
		RetryPolicy:     EncodeRetryPolicy(retryPolicy),
	}
	if retryPolicy != nil {
		m := retryPolicy.MaxRetries
		plan.RetryMax = &m
	}
	if len(legacyChildren) > 0 {
		names := make([]string, 0, len(legacyChildren))
		for _, child := range chain {
			if legacyChildren[child] {
				names = append(names, child)
			}
		}
		if len(names) > 0 {
			plan.LegacyChildren = names
		}
	}
	_ = providers // reserved: future outcome metadata may expose per-child details
	return plan
}

// BuildFallbackChainPlanForClient is the primary-override-free sibling of
// BuildFallbackChainPlan. Use after ResolveEffectiveClient has already
// produced the effective client name.
//
// pathReason carries the informational classification from the
// `WithReason` resolver — most commonly either empty (fully supported
// chain, no notes) or PathReasonFallbackRoundRobinChildLegacy (chain
// contains a baml-roundrobin child whose rotation is left to BAML's
// runtime on each worker; see cold-review finding A). Callers should
// pass the reason produced by ResolveFallbackChainForClientWithReason
// so metadata consumers see the composition rather than the generic
// "buildrequest fallback" shape.
func BuildFallbackChainPlanForClient(
	clientName string,
	chain []string,
	providers map[string]string,
	legacyChildren map[string]bool,
	retryPolicy *retry.Policy,
	buildRequestAPI string,
	pathReason string,
) *bamlutils.Metadata {
	plan := &bamlutils.Metadata{
		Phase:           bamlutils.MetadataPhasePlanned,
		Path:            "buildrequest",
		BuildRequestAPI: buildRequestAPI,
		Client:          clientName,
		Strategy:        "baml-fallback",
		PathReason:      pathReason,
		Chain:           append([]string(nil), chain...),
		RetryPolicy:     EncodeRetryPolicy(retryPolicy),
	}
	if retryPolicy != nil {
		m := retryPolicy.MaxRetries
		plan.RetryMax = &m
	}
	if len(legacyChildren) > 0 {
		names := make([]string, 0, len(legacyChildren))
		for _, child := range chain {
			if legacyChildren[child] {
				names = append(names, child)
			}
		}
		if len(names) > 0 {
			plan.LegacyChildren = names
		}
	}
	_ = providers
	return plan
}

// BuildLegacyMetadataPlan constructs the planned metadata for a request that
// will run on the legacy CallStream+OnTick path. Called by the generated
// per-method legacy helpers (runNoRawOrchestration / runFullOrchestration
// callers) to populate the `newPlannedMetadata` closure.
//
// The plan's Path is always "legacy" (this helper is only invoked on the
// legacy side). PathReason reflects whichever of the following applied:
//   - feature-flag off (PathReasonBuildRequestDisabled)
//   - unsupported or empty provider
//   - fallback strategy not routable (empty chain, empty child provider,
//     or all-legacy chain)
//   - fallback chain contains a baml-roundrobin child whose rotation is
//     left to BAML's per-worker runtime (PathReasonFallbackRoundRobinChildLegacy)
//   - an unresolved baml-roundrobin top-level client — fires only on
//     older adapters without SupportsWithClient, or defensively if RR
//     resolution reached this builder without being unwrapped upstream
//     (PathReasonRoundRobin)
//
// The plan includes retry policy information when a policy resolves, and
// chain/legacyChildren information for strategy clients.
func BuildLegacyMetadataPlan(
	adapter bamlutils.Adapter,
	defaultClientName string,
	introspectedProvider string,
	fallbackChains map[string][]string,
	clientProviders map[string]string,
	isProviderSupported func(string) bool,
	retryPolicy *retry.Policy,
) *bamlutils.Metadata {
	resolution := ResolveProviderWithReason(adapter, defaultClientName, introspectedProvider, isProviderSupported)

	plan := &bamlutils.Metadata{
		Phase:       bamlutils.MetadataPhasePlanned,
		Path:        "legacy",
		Client:      resolution.Client,
		Strategy:    resolution.Strategy,
		PathReason:  resolution.PathReason,
		RetryPolicy: EncodeRetryPolicy(retryPolicy),
	}
	// Only populate Provider for non-strategy routes. When Strategy is set
	// (e.g. "baml-fallback"), resolution.Provider echoes the strategy name,
	// which would misrepresent it as a real provider in the header /
	// metadata payload. The strategy name alone tells the story; per-child
	// provider info lives in Chain / LegacyChildren.
	if resolution.Strategy == "" {
		plan.Provider = resolution.Provider
	}
	if retryPolicy != nil {
		m := retryPolicy.MaxRetries
		plan.RetryMax = &m
	}

	// If the resolved client is a fallback strategy, enumerate the chain so
	// operators see the planned children. The legacy path may or may not
	// actually run through BAML's fallback logic (roundrobin is different
	// from fallback), but describing the chain in planned metadata is
	// always informative.
	if resolution.Strategy == "baml-fallback" {
		chain, providers, legacy, reason := ResolveFallbackChainWithReason(
			adapter, defaultClientName, fallbackChains, clientProviders, isProviderSupported,
		)
		plan.PathReason = reason
		// chain is nil when BuildRequest rejected the chain; rebuild
		// from the runtime-resolved client (resolution.Client already
		// accounts for primary overrides) so the plan still names the
		// children. Use resolveFallbackStrategyChain rather than a
		// direct map lookup so a runtime `strategy` override on the
		// fallback client is honoured — without this, metadata would
		// describe the wrong chain whenever a request used a runtime
		// strategy option.
		//
		// Critically, key only on resolution.Client. A previous revision
		// fell through to `defaultClientName`'s chain when the runtime
		// client had no resolvable chain (cold-review-2 finding C):
		// that emitted metadata with `Client=<runtime>`,
		// `Chain=<defaultClient's chain>` — a mismatch that misled
		// operators when a primary override pointed at a fallback
		// client with an invalid strategy override or empty chain. We
		// now leave plan.Chain empty in that case so the metadata
		// reflects what actually happens at runtime.
		if chain == nil {
			reg := adapter.OriginalClientRegistry()
			chain = resolveFallbackStrategyChain(reg, resolution.Client, fallbackChains)
			// Resolve each child's provider through the runtime registry
			// before consulting the static introspected map.
			providers = make(map[string]string, len(chain))
			legacy = make(map[string]bool)
			for _, child := range chain {
				p := ResolveClientProvider(reg, child, clientProviders)
				providers[child] = p
				if p == "" || (isProviderSupported != nil && !isProviderSupported(p)) {
					legacy[child] = true
				}
			}
		}
		if len(chain) > 0 {
			plan.Chain = append([]string(nil), chain...)
			if len(legacy) > 0 {
				names := make([]string, 0, len(legacy))
				for _, child := range chain {
					if legacy[child] {
						names = append(names, child)
					}
				}
				plan.LegacyChildren = names
			}
			_ = providers // providers reserved for future outcome population
		}
	}

	// BAML_REST_USE_BUILD_REQUEST being off is never surfaced by the per-
	// request resolution helpers; encode it here since a legacy-path
	// execution with a supported single provider otherwise looks empty.
	if plan.PathReason == "" && !UseBuildRequest() {
		plan.PathReason = PathReasonBuildRequestDisabled
	}

	return plan
}

// BuildLegacyMetadataPlanForClient is the primary-override-free sibling of
// BuildLegacyMetadataPlan. The caller is expected to have already resolved
// the effective client name (e.g. after applying the primary override and
// any round-robin unwrap). No further primary lookup happens here — the
// supplied clientName is treated as authoritative.
//
// Provider resolution for the effective client falls back from the runtime
// registry to introspectedProvider when neither the registry override nor
// the static introspected map names one.
func BuildLegacyMetadataPlanForClient(
	reg *bamlutils.ClientRegistry,
	clientName string,
	introspectedProvider string,
	fallbackChains map[string][]string,
	clientProviders map[string]string,
	isProviderSupported func(string) bool,
	retryPolicy *retry.Policy,
) *bamlutils.Metadata {
	provider := ResolveClientProvider(reg, clientName, clientProviders)
	// ResolveClientProvider returns "" both for "no provider configured"
	// and "present-empty override". In the absent case we want the
	// introspected provider as a metadata fallback so the plan still
	// names something. In the present-empty case we keep "" so the
	// classification below records PathReasonInvalidProviderOverride
	// — substituting the introspected provider here would mask the
	// invalid override on metadata. See PR #192 cold-review-2
	// verdict-8 follow-up.
	if provider == "" && !hasInvalidProviderOverride(reg, clientName) {
		provider = introspectedProvider
	}
	// Fold strategy aliases (RR + fallback) onto canonical spellings so
	// the classification switch below sees one form per strategy. A
	// runtime client_registry entry that used BAML's hyphenated RR form
	// or the bare "fallback" alias would otherwise fall through to the
	// default branch and get classified as an unsupported single-
	// provider. The resolver's own IsRoundRobinProvider and
	// canonicaliseProvider in cmd/introspect already fold these aliases;
	// this mirrors that for the metadata path. See PR #192 cold-review-2
	// finding 2.
	provider = normalizeStrategyProvider(provider)

	plan := &bamlutils.Metadata{
		Phase:       bamlutils.MetadataPhasePlanned,
		Path:        "legacy",
		Client:      clientName,
		RetryPolicy: EncodeRetryPolicy(retryPolicy),
	}
	if retryPolicy != nil {
		m := retryPolicy.MaxRetries
		plan.RetryMax = &m
	}

	switch provider {
	case "":
		if hasInvalidProviderOverride(reg, clientName) {
			plan.PathReason = PathReasonInvalidProviderOverride
		} else {
			plan.PathReason = PathReasonEmptyProvider
		}
	case "baml-fallback":
		plan.Strategy = "baml-fallback"
		chain, providers, legacy, reason := ResolveFallbackChainForClientWithReason(
			reg, clientName, fallbackChains, clientProviders, isProviderSupported,
		)
		plan.PathReason = reason
		if chain == nil {
			chain = resolveFallbackStrategyChain(reg, clientName, fallbackChains)
			providers = make(map[string]string, len(chain))
			legacy = make(map[string]bool)
			for _, child := range chain {
				p := ResolveClientProvider(reg, child, clientProviders)
				providers[child] = p
				if p == "" || (isProviderSupported != nil && !isProviderSupported(p)) {
					legacy[child] = true
				}
			}
		}
		if len(chain) > 0 {
			plan.Chain = append([]string(nil), chain...)
			if len(legacy) > 0 {
				names := make([]string, 0, len(legacy))
				for _, child := range chain {
					if legacy[child] {
						names = append(names, child)
					}
				}
				plan.LegacyChildren = names
			}
			_ = providers
		}
	case "baml-roundrobin":
		// RoundRobin is normally resolved upstream before the legacy
		// plan is built. We reach here in three cases:
		//   - The RR resolver returned an error (cycle / empty chain)
		//     and the caller fell through with the unresolved client
		//     name.
		//   - ResolveEffectiveClient short-circuited an invalid runtime
		//     strategy override by returning the un-unwrapped client
		//     (cold-review-2 finding 1). In that case the routing is
		//     correct (legacy) but the *emitted metadata* needs to
		//     reflect WHY — operators reading the X-BAML-Path-Reason
		//     header should see the invalid-override classification,
		//     not the generic RR-legacy reason.
		//   - ResolveEffectiveClient short-circuited an invalid runtime
		//     `options.start` override (cold-review-3 finding 3). Same
		//     treatment as the strategy case but a distinct reason so
		//     operators can tell which option was malformed.
		plan.Strategy = "baml-roundrobin"
		if _, present, valid := inspectStrategyOverride(reg, clientName); present && !valid {
			plan.PathReason = PathReasonInvalidStrategyOverride
		} else if hasInvalidStartOverride(reg, clientName) {
			plan.PathReason = PathReasonInvalidRoundRobinStartOverride
		} else {
			plan.PathReason = PathReasonRoundRobin
		}
	default:
		plan.Provider = provider
		if isProviderSupported != nil && !isProviderSupported(provider) {
			plan.PathReason = PathReasonUnsupportedProvider
		}
	}

	if plan.PathReason == "" && !UseBuildRequest() {
		plan.PathReason = PathReasonBuildRequestDisabled
	}

	return plan
}

// ResolveFallbackChainWithReason wraps ResolveFallbackChain and returns
// a classification alongside the usual (chain, providers, legacyChildren)
// triple.
//
// `chain == nil` is the hard-failure signal: BuildRequest cannot drive
// the request and the caller must fall through to legacy. `chain != nil`
// means BuildRequest can drive the chain (potentially with mixed-mode
// children).
//
// `reason` is *both* a hard-failure code and an informational tag,
// distinguished by the chain:
//
//   - chain == nil, reason == "": the client is not a fallback strategy
//     (caller's top-level classification handles the path).
//   - chain == nil, reason != "": hard failure context — one of
//     PathReasonInvalidStrategyOverride, PathReasonFallbackEmptyChain,
//     PathReasonFallbackEmptyChildProvider, PathReasonFallbackAllLegacy.
//   - chain != nil, reason == "": fully drivable chain.
//   - chain != nil, reason == PathReasonFallbackRoundRobinChildLegacy:
//     drivable chain that contains a baml-roundrobin child whose
//     rotation is left to BAML's per-worker runtime. Informational
//     metadata, not a failure — callers use the chain normally.
//
// Callers gate on `len(chain) > 0` for the hard routing decision and
// pass `reason` straight through to metadata. See PR #192 cold-review-2
// finding B for the contract clarification — pre-clarification the doc
// claimed any non-empty reason came with nil chain, which contradicts
// the RR-child case below.
func ResolveFallbackChainWithReason(
	adapter bamlutils.Adapter,
	defaultClientName string,
	fallbackChains map[string][]string,
	clientProviders map[string]string,
	isProviderSupported func(string) bool,
) (chain []string, providers map[string]string, legacyChildren map[string]bool, reason string) {
	reg := adapter.OriginalClientRegistry()

	// An empty primary string is not an override — treat it the same as
	// a nil pointer so this matches ResolveProviderWithReason's behaviour.
	// Without this guard the empty string looks up an empty client name
	// in fallbackChains and resolves to nothing.
	clientName := defaultClientName
	if reg != nil && reg.Primary != nil && *reg.Primary != "" {
		clientName = *reg.Primary
	}

	return ResolveFallbackChainForClientWithReason(reg, clientName, fallbackChains, clientProviders, isProviderSupported)
}

// ResolveFallbackChainForClientWithReason is the primary-override-free
// sibling of ResolveFallbackChainWithReason. Callers that have already
// resolved the effective client name (e.g. after external round-robin
// unwrap) pass that name directly; no further primary lookup or RR
// unwrap happens inside.
//
// Return contract matches the sibling. `chain == nil` is the hard-
// failure signal; `chain != nil` is drivable. `reason` is hard-failure
// context when `chain == nil` and informational metadata
// (PathReasonFallbackRoundRobinChildLegacy) when `chain != nil`.
// Callers gate on `len(chain) > 0` for routing.
func ResolveFallbackChainForClientWithReason(
	reg *bamlutils.ClientRegistry,
	clientName string,
	fallbackChains map[string][]string,
	clientProviders map[string]string,
	isProviderSupported func(string) bool,
) (chain []string, providers map[string]string, legacyChildren map[string]bool, reason string) {
	parentProvider := ResolveClientProvider(reg, clientName, clientProviders)
	if parentProvider != "baml-fallback" {
		// Not a fallback client — caller decides the path from the
		// top-level ResolveProviderWithReason classification instead.
		return nil, nil, nil, ""
	}

	// Detect a present-but-unparseable runtime strategy override before
	// chain resolution. Returning PathReasonInvalidStrategyOverride here
	// causes the codegen-side `len(chain) > 0` gate to fall through to
	// legacy, where BAML's runtime emits the canonical
	// ensure_strategy error rather than us silently using the
	// introspected chain.
	if _, present, valid := inspectStrategyOverride(reg, clientName); present && !valid {
		return nil, nil, nil, PathReasonInvalidStrategyOverride
	}

	resolvedChain := resolveFallbackStrategyChain(reg, clientName, fallbackChains)
	if len(resolvedChain) == 0 {
		return nil, nil, nil, PathReasonFallbackEmptyChain
	}

	chainProviders := make(map[string]string, len(resolvedChain))
	chainLegacy := make(map[string]bool)
	legacyPositions := 0
	hasRoundRobinChild := false
	for _, child := range resolvedChain {
		p := ResolveClientProvider(reg, child, clientProviders)
		if p == "" {
			return nil, nil, nil, PathReasonFallbackEmptyChildProvider
		}
		chainProviders[child] = p
		// A nil support check means the caller couldn't determine support,
		// not "nothing is supported" — treat every child as drivable in
		// that case rather than panicking on the nil-func call or
		// misclassifying the whole chain as legacy.
		if isProviderSupported != nil && !isProviderSupported(p) {
			chainLegacy[child] = true
			legacyPositions++
		}
		// Detect any RR-provider child. Note: roundrobin.IsRoundRobinProvider
		// folds all three spellings (baml-roundrobin / baml-round-robin /
		// round-robin) onto the same classification so runtime registry
		// overrides using any form are caught.
		if roundrobin.IsRoundRobinProvider(p) {
			hasRoundRobinChild = true
		}
	}

	if legacyPositions == len(resolvedChain) {
		return nil, nil, nil, PathReasonFallbackAllLegacy
	}

	// Informational reason for mixed-mode chains that contain an RR
	// child. The chain still runs: non-RR children take the BuildRequest
	// path, and the RR child is handled by BAML's runtime on each worker
	// (per-worker rotation, not centralised). Surface the composition in
	// metadata so operators can distinguish it from a single-level RR,
	// which IS centralised via the SharedState broker. Centralised
	// unwrapping of RR children inside fallback chains is deferred — see
	// PR #192 cold-review finding 2.
	if hasRoundRobinChild {
		return resolvedChain, chainProviders, chainLegacy, PathReasonFallbackRoundRobinChildLegacy
	}

	return resolvedChain, chainProviders, chainLegacy, ""
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

// EncodeRetryPolicy formats a retry.Policy into the compact string used by
// the Metadata.RetryPolicy field. Examples:
//
//	"const:200ms" — constant delay
//	"exp:200ms:1.5:10s" — exponential backoff (base:multiplier:max)
//	"" — nil or unresolved policy
func EncodeRetryPolicy(p *retry.Policy) string {
	if p == nil {
		return ""
	}
	cfg := p.StrategyConfig
	if cfg == nil {
		return ""
	}
	switch cfg.Type {
	case "constant_delay":
		return fmt.Sprintf("const:%dms", cfg.DelayMs)
	case "exponential_backoff":
		return fmt.Sprintf("exp:%dms:%.2f:%dms", cfg.DelayMs, cfg.Multiplier, cfg.MaxDelayMs)
	}
	return cfg.Type
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

// normalizeStrategyProvider folds the BAML strategy aliases that
// clientspec.rs accepts onto the canonical baml-rest spellings used in
// the classification switches downstream. Both round-robin and
// fallback have a "with-prefix" and "without-prefix" form upstream:
//
//   - "round-robin" / "baml-round-robin" / "baml-roundrobin"
//     → "baml-roundrobin"
//   - "fallback" / "baml-fallback" → "baml-fallback"
//
// Without this normalisation, a runtime client_registry entry that
// used the bare "fallback" alias (the form the BAML CLI init template
// uses) would slip past the "baml-fallback" arm in
// ResolveProviderWithReason and fall to legacy via
// PathReasonUnsupportedProvider — losing chain plan, mixed-mode child
// handling, and RR-child plumbing. See PR #192 cold-review-2 finding 2.
//
// roundrobin.NormalizeProvider already covers the RR aliases; we keep
// this one-call helper to apply both classes at every classification
// seam without each call site duplicating the alias check.
func normalizeStrategyProvider(provider string) string {
	provider = roundrobin.NormalizeProvider(provider)
	if provider == "fallback" {
		return "baml-fallback"
	}
	return provider
}

// ResolveClientProvider returns the provider for a named client, checking
// runtime client_registry overrides before the introspected defaults. This
// mirrors ResolveProvider's per-client resolution so that runtime
// overrides that change a child's provider are respected for both
// extraction format selection and the supported-provider gate.
//
// The returned provider is normalised through normalizeStrategyProvider
// so callers see one canonical spelling per strategy regardless of
// which alias the .baml source or runtime registry used.
//
// Presence semantics (PR #192 cold-review-2 verdict-8 follow-up):
//
//   - registry entry absent or has no provider key: fall back to the
//     introspected provider, preserving strategy-only and presence-
//     only overrides (a common shape in tests and in production
//     dynamic-RR flows).
//   - registry entry present with a non-empty provider: normalize and
//     use the override.
//   - registry entry present with an empty provider: return "" so the
//     caller's IsProviderSupported gate fails and the request falls
//     through to legacy. BAML upstream rejects empty provider strings
//     (clientspec.rs:119-144); routing to legacy lets BAML emit its
//     native invalid-provider error rather than us silently using the
//     introspected fallback.
func ResolveClientProvider(reg *bamlutils.ClientRegistry, clientName string, introspectedProviders map[string]string) string {
	if reg != nil {
		for _, client := range reg.Clients {
			if client != nil && client.Name == clientName && client.IsProviderPresent() {
				if client.Provider == "" {
					return ""
				}
				return normalizeStrategyProvider(client.Provider)
			}
		}
	}
	return normalizeStrategyProvider(introspectedProviders[clientName])
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

// parseRuntimeStrategyOption delegates to the shared parser at
// bamlutils/strategyparse. Fallback and round-robin historically kept
// local copies of this logic that diverged on quote handling, which
// broke RR runtime overrides under bracketed-string shapes; the shared
// helper keeps both strategies consistent.
func parseRuntimeStrategyOption(v any) []string {
	return strategyparse.ParseStrategyOption(v)
}

// hasInvalidProviderOverride reports whether the runtime registry has
// an entry for clientName whose `provider` key was supplied with the
// empty string. BAML upstream's ClientProvider::from_str rejects empty
// provider strings (clientspec.rs:119-144); ResolveProvider already
// surfaces the empty value verbatim (rather than falling through to
// the introspected provider) so the codegen gate routes the request
// to legacy. This helper lets the metadata classifier distinguish
// "operator sent provider:''" from "no provider configured anywhere"
// — emitting PathReasonInvalidProviderOverride versus
// PathReasonEmptyProvider respectively. See PR #192 cold-review-2
// verdict-8 follow-up.
//
// Returns false when the registry is absent, the entry is missing, or
// the entry simply omits the `provider` key (a strategy-only or
// presence-only override that should keep using the introspected
// provider).
func hasInvalidProviderOverride(reg *bamlutils.ClientRegistry, clientName string) bool {
	if reg == nil || clientName == "" {
		return false
	}
	for _, c := range reg.Clients {
		if c == nil || c.Name != clientName {
			continue
		}
		return c.IsProviderPresent() && c.Provider == ""
	}
	return false
}

// hasInvalidStartOverride reports whether the runtime registry has an
// entry for clientName whose `options.start` value is present but
// unparseable as an integer. Mirrors hasInvalidProviderOverride for
// the `start` option so the metadata classifier can surface
// PathReasonInvalidRoundRobinStartOverride distinctly from the
// generic invalid-strategy / RR-legacy reasons. See PR #192
// cold-review-3 finding 3.
//
// Returns false when the registry is absent, the entry is missing,
// the entry has no options, the `start` key is absent, or the value
// parses successfully — in all of which the metadata classifier
// should keep its existing reason.
func hasInvalidStartOverride(reg *bamlutils.ClientRegistry, clientName string) bool {
	rc := findRuntimeClient(reg, clientName)
	if rc == nil {
		return false
	}
	_, present, valid := roundrobin.InspectStartOverride(rc.Options)
	return present && !valid
}

// inspectStrategyOverride examines the runtime client_registry entry for
// a strategy client. Three outcomes:
//
//   - present=false, valid=true: no `strategy` key in the runtime options
//     (or no runtime entry at all). Caller falls back to the introspected
//     chain.
//   - present=true, valid=true: the override parsed to a non-empty chain.
//     Caller honours the returned slice.
//   - present=true, valid=false: the override is present but unparseable
//     (bare token, half-bracketed, empty array, heterogeneous []any with
//     non-string elements, etc.). BAML upstream's ensure_strategy
//     rejects these, so callers route the request to legacy where BAML's
//     runtime emits "strategy must be an array" / "strategy must not
//     be empty". A previous revision silently used the introspected
//     chain for these inputs, masking operator typos and diverging from
//     upstream — see PR #192 cold-review-2 finding 1.
func inspectStrategyOverride(reg *bamlutils.ClientRegistry, clientName string) (chain []string, present bool, valid bool) {
	rc := findRuntimeClient(reg, clientName)
	if rc == nil || rc.Options == nil {
		return nil, false, true
	}
	raw, ok := rc.Options["strategy"]
	if !ok {
		return nil, false, true
	}
	parsed := parseRuntimeStrategyOption(raw)
	if len(parsed) == 0 {
		return nil, true, false
	}
	return parsed, true, true
}

func resolveFallbackStrategyChain(reg *bamlutils.ClientRegistry, clientName string, introspectedChains map[string][]string) []string {
	chain, present, valid := inspectStrategyOverride(reg, clientName)
	if present && !valid {
		return nil
	}
	if chain != nil {
		return chain
	}
	return introspectedChains[clientName]
}

// ResolveFallbackChain determines whether a function's client is a fallback
// strategy client and, if so, returns the ordered child chain, a map of
// child client names to their resolved providers, and the set of children
// whose providers are unsupported by BuildRequest (mixed-mode legacy
// children). Returns nil, nil, nil if the function does not use a fallback
// chain, if the chain cannot be resolved, or if every child is legacy (in
// which case the whole chain should route to the existing CallStream+OnTick
// legacy path).
//
// When the chain mixes supported and unsupported children, the returned
// chain and providers cover every child; legacyChildren lists the names
// whose providers require the legacy BAML Stream API. providers still
// contains entries for legacy children (useful for debugging/logging), but
// callers must consult legacyChildren before assuming BuildRequest can
// drive that child.
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
) (chain []string, providers map[string]string, legacyChildren map[string]bool) {
	reg := adapter.OriginalClientRegistry()

	// Determine which client name to look up in fallbackChains.
	// Runtime primary override takes precedence over the static default.
	// An empty primary string is not an override — treat it the same as
	// a nil pointer (matching ResolveProviderWithReason's behaviour).
	clientName := defaultClientName
	if reg != nil && reg.Primary != nil && *reg.Primary != "" {
		clientName = *reg.Primary
	}

	return ResolveFallbackChainForClient(reg, clientName, fallbackChains, clientProviders, isProviderSupported)
}

// ResolveFallbackChainForClient is the primary-override-free sibling of
// ResolveFallbackChain. Use this after an external resolver has already
// determined the effective client name (e.g. round-robin unwrap) — it
// skips primary-override resolution and treats clientName as authoritative.
func ResolveFallbackChainForClient(
	reg *bamlutils.ClientRegistry,
	clientName string,
	fallbackChains map[string][]string,
	clientProviders map[string]string,
	isProviderSupported func(string) bool,
) (chain []string, providers map[string]string, legacyChildren map[string]bool) {
	parentProvider := ResolveClientProvider(reg, clientName, clientProviders)
	if parentProvider != "baml-fallback" {
		return nil, nil, nil
	}

	chain = resolveFallbackStrategyChain(reg, clientName, fallbackChains)
	if len(chain) == 0 {
		return nil, nil, nil
	}

	// Resolve each child's provider, checking runtime client_registry
	// overrides first (same precedence as ResolveProvider). Children with
	// unsupported providers are recorded in legacyChildren so the
	// orchestrator can route them through the shared legacy BAML Stream
	// helper. Children with an empty (unknown) provider are treated as
	// fatal — we can't route an unknown provider at all, so fall back to
	// the existing legacy path for the entire chain.
	//
	// legacyPositions counts chain entries that land in legacyChildren. It
	// differs from len(legacyChildren) when the chain lists the same
	// unsupported client more than once — the set would undercount and
	// the all-legacy check below would misclassify such a chain as mixed.
	providers = make(map[string]string, len(chain))
	legacyChildren = make(map[string]bool)
	legacyPositions := 0
	for _, child := range chain {
		p := ResolveClientProvider(reg, child, clientProviders)
		if p == "" {
			return nil, nil, nil
		}
		providers[child] = p
		// Nil support check means "caller didn't supply a filter"; defer
		// the decision to the caller rather than treating every provider
		// as unsupported (which would collapse the chain to all-legacy).
		if isProviderSupported != nil && !isProviderSupported(p) {
			legacyChildren[child] = true
			legacyPositions++
		}
	}

	// If every chain position is legacy, there is nothing for BuildRequest
	// to do — the entire chain degenerates to legacy.
	if legacyPositions == len(chain) {
		return nil, nil, nil
	}

	return chain, providers, legacyChildren
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

	// IncludeThinkingInRaw is the per-request opt-in for surfacing
	// provider-specific reasoning/thinking content in raw. When true,
	// the streaming extractor accumulates Anthropic thinking_delta
	// events into raw alongside text deltas. When false (default),
	// raw matches BAML's RawLLMResponse() text-only semantics. The
	// flag never affects the parseable text passed to ParseStream.
	IncludeThinkingInRaw bool

	// ParseThrottleInterval is the minimum time between ParseStream calls.
	// Zero means parse on every SSE event (no throttling).
	ParseThrottleInterval time.Duration

	// FallbackChain is the ordered list of child client names for fallback
	// strategies. When non-empty, each retry attempt walks the entire chain
	// in order; if any child succeeds, the attempt returns immediately.
	// When empty, the single Provider is used for all attempts.
	FallbackChain []string

	// ClientOverride, when non-empty, is passed as the clientOverride
	// argument to buildRequest for single-provider attempts (FallbackChain
	// empty). Used by the round-robin path to direct BAML to build the
	// request for the RR-selected leaf client rather than the function's
	// default. Ignored for fallback-chain attempts — those iterate child
	// names and pass each as the per-attempt override.
	ClientOverride string

	// ClientProviders maps child client names to their provider strings.
	// Used with FallbackChain to resolve the provider for each attempt.
	// In mixed-mode chains this map includes both BuildRequest-supported
	// children and legacy (unsupported-provider) children — it is a pure
	// lookup table, not a "supported set." Callers must consult
	// LegacyChildren to decide which path runs a given child.
	ClientProviders map[string]string

	// LegacyChildren marks the entries in FallbackChain that must be driven
	// via the legacy BAML Stream API (WithClient + WithOnTick) rather than
	// the BuildRequest path. Populated by ResolveFallbackChain when the
	// chain mixes supported and unsupported providers. Children absent
	// from this map use the BuildRequest path (buildRequest closure).
	// When non-empty, LegacyStreamChild must be non-nil — the orchestrator
	// fails up-front validation otherwise.
	LegacyChildren map[string]bool

	// MetadataPlan is the pre-computed planned metadata for this request.
	// When non-nil, the orchestrator emits a single planned metadata event
	// right after the first heartbeat fires (so clients observe liveness
	// before routing decisions) and, on success, an outcome metadata event
	// right before the final result. Nil disables metadata emission.
	//
	// The orchestrator populates the outcome event's winner fields (winner
	// client, provider, path, retry count, upstream duration) from runtime
	// observations — callers supply only the planned portion.
	MetadataPlan *bamlutils.Metadata

	// NewMetadataResult constructs a pooled StreamResult wrapping a metadata
	// payload. Required when MetadataPlan is non-nil.
	NewMetadataResult NewMetadataResultFunc

	// LegacyStreamChild runs a single child via BAML's Stream API. The
	// orchestrator invokes it for children marked in LegacyChildren.
	//
	// Contract:
	//   - Invoke BAML's Stream.Method with WithClient(clientOverride) and
	//     wire a WithOnTick callback that invokes sendHeartbeat on the
	//     first FunctionLog tick (this matches the BuildRequest path's
	//     post-HTTP heartbeat liveness semantics — don't pre-emit the
	//     heartbeat before upstream activity, or the pool's hung detector
	//     is disabled prematurely).
	//   - Drain the stream to completion; do NOT emit partials on the
	//     orchestrator's output channel. The orchestrator is responsible
	//     for the single final emission.
	//   - Return (final, raw, err). When needsRaw is false the helper may
	//     return an empty raw string.
	//
	// Retry note: this callback goes through BAML's runtime, so any
	// client-level retry_policy declared statically in the BAML file will
	// apply on top of the orchestrator's outer retry.Execute loop. Per-
	// request __baml_options__.retry is NOT forwarded into BAML because
	// makeOptionsFromAdapter does not translate it — outer retries handle
	// per-request retries, so only the inner BAML static retry compounds.
	//
	// Raw note: legacy raw comes from BAML's FunctionLog.RawLLMResponse(),
	// which is text-only across providers (BAML's runtime drops Anthropic-
	// thinking_delta and equivalents). Under the default
	// IncludeThinkingInRaw=false this exactly matches the BuildRequest
	// path's behavior. Under IncludeThinkingInRaw=true, this niche path's
	// raw stays text-only — the flag has no effect here because BAML's
	// runtime never surfaces thinking content for this codepath to
	// accumulate. Documented as a known limitation in the
	// include-thinking-in-raw tracking issue; revisit only if reported in
	// a real workflow.
	LegacyStreamChild LegacyStreamChildFunc
}

// LegacyStreamChildFunc runs one child of a mixed-mode fallback chain via
// BAML's Stream API. See StreamConfig.LegacyStreamChild for the full
// contract.
type LegacyStreamChildFunc func(
	ctx context.Context,
	clientOverride string,
	provider string,
	needsRaw bool,
	sendHeartbeat func(),
) (finalResult any, raw string, err error)

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

// NewMetadataResultFunc creates a new pooled StreamResult wrapping a metadata
// payload. Provided by the generated adapter code via the per-method metadata
// constructor. The returned StreamResult's Kind() must be
// StreamResultKindMetadata and its Metadata() must return the supplied value.
type NewMetadataResultFunc func(md *bamlutils.Metadata) bamlutils.StreamResult

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

	// Validate the configured provider(s) up front so invalid fallback chains
	// fail before any stream/reset events are emitted.
	if len(config.FallbackChain) == 0 {
		if config.Provider == "" || !IsProviderSupported(config.Provider) {
			return fmt.Errorf("buildrequest: unsupported or empty provider %q", config.Provider)
		}
	} else {
		if len(config.LegacyChildren) > 0 && config.LegacyStreamChild == nil {
			return fmt.Errorf("buildrequest: LegacyChildren set but LegacyStreamChild is nil")
		}
		for _, child := range config.FallbackChain {
			if config.LegacyChildren[child] {
				// Legacy children are driven via BAML's Stream API, which
				// tolerates providers BuildRequest doesn't support; we
				// only require the legacy callback itself. Skipping the
				// IsProviderSupported check is safe because
				// ResolveFallbackChain rejects chains with any empty
				// provider (returns nil,nil,nil), so ClientProviders[child]
				// is guaranteed non-empty by the time we get here.
				continue
			}
			provider := config.ClientProviders[child]
			if provider == "" || !IsProviderSupported(provider) {
				return fmt.Errorf("buildrequest: unsupported or empty provider %q for child %q", provider, child)
			}
		}
	}

	var heartbeatSent atomic.Bool

	// plannedMetadataOnce gates the single planned-metadata emission. It is
	// deliberately separate from heartbeatSent — heartbeatSent resets on
	// each inner retry (so retried attempts re-arm first-byte hung
	// detection), but the planned metadata describes the whole orchestrator
	// run and must fire exactly once. Pool-level retries produce a fresh
	// orchestrator invocation with a fresh Once, so each pool attempt gets
	// its own planned emission (pool rewrites the Attempt field).
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

	// Send initial heartbeat for hung detection, then emit planned metadata.
	// Metadata is always *after* the heartbeat so that (a) the pool's
	// first-byte tracking sees the heartbeat as liveness and (b) the pool's
	// mid-retry reset injection lands on metadata only when a retry is
	// actually in progress (consumeStream honors Reset on Metadata kind).
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

	// tryOneStreamChild runs a single child's streaming attempt against the
	// BuildRequest path. Returns the parsed final plus the accumulated raw
	// text; the caller (attemptFull) is responsible for emitting the final
	// result on the output channel so supported and legacy children share a
	// single emission path.
	tryOneStreamChild := func(provider, clientOverride string) (any, string, error) {
		req, err := buildRequest(ctx, clientOverride)
		if err != nil {
			return nil, "", fmt.Errorf("buildrequest: failed to build request: %w", err)
		}

		resp, err := httpClient.ExecuteStream(ctx, req)
		if err != nil {
			return nil, "", err
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
			// attempt's provider. When IncludeThinkingInRaw is true, Anthropic
			// thinking_delta events contribute to Raw only; Parseable always
			// excludes thinking so the BAML parser cannot be influenced by
			// reasoning text regardless of the flag.
			delta, extractErr := sse.ExtractDeltaPartsFromText(provider, ev.Data, config.IncludeThinkingInRaw)
			if extractErr != nil {
				// Extraction error — fail the attempt so retry logic can handle it
				// rather than silently accumulating incomplete text.
				return nil, "", fmt.Errorf("buildrequest: delta extraction failed: %w", extractErr)
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
						return nil, "", err
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
							return nil, "", err
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
			return nil, "", fmt.Errorf("buildrequest: stream error: %w", streamErr)
		}

		// Parse the final result — let parseFinal decide whether an empty
		// completion is valid. The legacy path does not reject empty strings.
		fullRaw := rawAccumulated.String()

		finalResult, parseErr := parseFinal(ctx, parseableAccumulated.String())
		if parseErr != nil {
			return nil, "", fmt.Errorf("buildrequest: failed to parse final result: %w", parseErr)
		}

		return finalResult, fullRaw, nil
	}

	// Track the winning attempt for outcome metadata. Populated by
	// attemptFull right before emitFinal runs, so the outcome event
	// always reflects the child that actually produced the final result.
	var (
		winnerClient    string
		winnerProvider  string
		winnerPath      string
		finalRetryCount int
		startTime       = time.Now()
	)

	// emitOutcomeMetadata sends the outcome metadata event just before the
	// final result. Scheduled from attemptFull once a winning child has
	// been identified. Safe to call with a nil MetadataPlan (no-op).
	emitOutcomeMetadata := func() {
		if config.MetadataPlan == nil || config.NewMetadataResult == nil {
			return
		}
		outcome := *config.MetadataPlan
		outcome.Phase = bamlutils.MetadataPhaseOutcome
		outcome.Attempt = 0
		// Clear planned-only noise from the outcome payload. Clients key
		// behaviour off Phase, but keeping the event compact avoids a large
		// duplicated payload on every successful request.
		outcome.RetryMax = nil
		outcome.RetryPolicy = ""
		outcome.Chain = nil
		outcome.LegacyChildren = nil
		outcome.Strategy = ""
		outcome.Provider = ""

		retryCount := finalRetryCount
		outcome.RetryCount = &retryCount
		outcome.WinnerClient = winnerClient
		outcome.WinnerProvider = winnerProvider
		outcome.WinnerPath = winnerPath
		dur := time.Since(startTime).Milliseconds()
		outcome.UpstreamDurMs = &dur

		r := config.NewMetadataResult(&outcome)
		select {
		case out <- r:
		case <-ctx.Done():
			r.Release()
		}
	}

	// emitFinal sends the single StreamResultKindFinal for the winning
	// attempt, respecting context cancellation. Centralising the emission
	// here guarantees exactly one final event regardless of whether the
	// winning child was BuildRequest-driven or routed through the legacy
	// helper.
	emitFinal := func(finalResult any, raw string) error {
		emitOutcomeMetadata()
		rawForFinal := ""
		if config.NeedsRaw {
			rawForFinal = raw
		}
		r := newResult(bamlutils.StreamResultKindFinal, nil, finalResult, rawForFinal, nil, false)
		select {
		case out <- r:
			return nil
		case <-ctx.Done():
			r.Release()
			return ctx.Err()
		}
	}

	// attemptFull tries the single provider or the entire fallback chain.
	// For fallback chains, each retry walks all children in order —
	// matching the BAML runtime where retries retry the entire strategy.
	attemptFull := func(attempt int) (any, error) {
		if len(config.FallbackChain) == 0 {
			finalResult, raw, err := tryOneStreamChild(config.Provider, config.ClientOverride)
			if err != nil {
				return nil, err
			}
			winnerClient = ""
			if config.MetadataPlan != nil {
				winnerClient = config.MetadataPlan.Client
			}
			winnerProvider = config.Provider
			winnerPath = "buildrequest"
			finalRetryCount = attempt
			if emitErr := emitFinal(finalResult, raw); emitErr != nil {
				return nil, emitErr
			}
			return finalResult, nil
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
			// child in the chain. Only emitted when partials could have
			// reached the client — for NeedsPartials=false (call modes
			// bridged through this orchestrator) there is no partial state
			// downstream, and the reset would falsely flip the pool's
			// gotFirstByte flag, disabling hung detection for the next
			// child. The heartbeat reset still runs so the next child
			// re-arms first-byte detection via its own heartbeat.
			if i > 0 && lastErr != nil {
				if config.NeedsPartials {
					r := newResult(bamlutils.StreamResultKindStream, nil, nil, "", nil, true)
					select {
					case out <- r:
					case <-ctx.Done():
						r.Release()
						return nil, ctx.Err()
					}
				}
				heartbeatSent.Store(false)
			}
			var (
				finalResult any
				raw         string
				err         error
				path        string
			)
			if config.LegacyChildren[child] {
				// Legacy children run via BAML's Stream API. The callback
				// owns heartbeat timing (fires sendHeartbeat on the first
				// FunctionLog tick) so pool hung-detection stays correct.
				// Partials are never emitted by the callback — it reports
				// only (final, raw, err).
				finalResult, raw, err = config.LegacyStreamChild(
					ctx,
					child,
					config.ClientProviders[child],
					config.NeedsRaw,
					sendHeartbeat,
				)
				path = "legacy"
			} else {
				provider := config.ClientProviders[child]
				finalResult, raw, err = tryOneStreamChild(provider, child)
				path = "buildrequest"
			}
			if err == nil {
				winnerClient = child
				winnerProvider = config.ClientProviders[child]
				winnerPath = path
				finalRetryCount = attempt
				if emitErr := emitFinal(finalResult, raw); emitErr != nil {
					return nil, emitErr
				}
				return finalResult, nil
			}
			lastErr = err
		}
		return nil, lastErr
	}

	// Execute with retries
	_, err := retry.Execute(ctx, config.RetryPolicy, attemptFull, func(attempt int) {
		// Emit reset signal so downstream discards accumulated partial state.
		// Only emitted when partials could have reached the client. For
		// NeedsPartials=false (call modes bridged through this orchestrator)
		// there is no partial state downstream, and the reset event would
		// falsely flip the pool's gotFirstByte flag before the retry's
		// upstream has produced a byte — disabling hung detection.
		if config.NeedsPartials {
			r := newResult(bamlutils.StreamResultKindStream, nil, nil, "", nil, true)
			select {
			case out <- r:
			case <-ctx.Done():
				r.Release()
			}
		}

		// Reset heartbeat for new attempt so first-byte detection re-arms
		// via the next attempt's heartbeat.
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
