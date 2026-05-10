package buildrequest

import (
	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/buildrequest/roundrobin"
	"github.com/invakid404/baml-rest/bamlutils/retry"
)

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
	// level RR is normally unwrapped by ResolveEffectiveClient before
	// dispatch. This reason fires on three paths:
	//
	//   - Older adapters that lack the WithClient CallOption.
	//   - BAML_REST_USE_BUILD_REQUEST=false: the codegen gate (in
	//     adapters/common/codegen/codegen.go) only invokes
	//     ResolveEffectiveClient when supportsWithClient &&
	//     __useBuildRequest, so flipping the flag off leaves a
	//     top-level RR client unresolved even on a SupportsWithClient
	//     adapter, and the legacy plan surfaces this reason.
	//   - Defensive path when RR resolution failed (cycle / empty
	//     chain) and the caller fell through with the unresolved
	//     client name.
	//
	// The string value is preserved for backwards-compatibility with
	// any metric parsers.
	PathReasonRoundRobin = "roundrobin-legacy-only"
	// PathReasonFallbackRoundRobinChildLegacy: the fallback chain
	// contains a baml-roundrobin child. Top-level RR is centralised
	// across workers via the SharedState broker socket, but nested RR
	// inside a fallback chain is intentionally NOT unwrapped by the
	// BuildRequest resolver — that would require a "preselect one RR
	// leaf at each fallback position" design that's deferred. Such
	// chains still work, but the RR child is handed to BAML's
	// runtime on each worker, which means its rotation is per-worker,
	// not centralised. Surfaced here so operators can spot the
	// composition and know not to expect fleet-wide rotation for the
	// nested client.
	PathReasonFallbackRoundRobinChildLegacy = "fallback-roundrobin-child-legacy"
	// PathReasonFallbackRoundRobinChildBuildRequest reports that an
	// immediate baml-roundrobin fallback child was centrally unwrapped
	// to a leaf and dispatched via the BuildRequest path, rather than
	// handed off to BAML's per-worker runtime through the legacy
	// callback. The selected leaf appears in Metadata.FallbackTargets
	// keyed by the RR wrapper's chain-position name, and the RR
	// decision itself appears in Metadata.FallbackRoundRobin under the
	// same key.
	//
	// Distinct from PathReasonFallbackRoundRobinChildLegacy, which
	// keeps describing nested RR children that remain on the legacy
	// callback (deferred shapes, unsupported leaves, invalid
	// `options.strategy` / `options.start` overrides). New in issue
	// #237 PR 1 (vocabulary only); consumed starting in PR 2.
	PathReasonFallbackRoundRobinChildBuildRequest = "fallback-roundrobin-child-buildrequest"
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
	// error rather than silently using the introspected chain.
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
	// "client has no provider declared".
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
	// failing option apart in the X-BAML-Path-Reason header.
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
	// provider string.
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
		//     entry has no provider key. With primary set and no
		//     primary `clients[]` entry, ResolveProvider returns ""
		//     from the default lookup; the classifier needs a
		//     checkpoint for it so it reports the
		//     malformed-override reason rather than the generic
		//     empty-provider reason.
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
		if _, present, valid := roundrobin.InspectStrategyOverride(reg, clientName); present && !valid {
			res.PathReason = PathReasonInvalidStrategyOverride
		} else if hasExplicitStrategyProviderWithoutStrategy(reg, clientName) {
			// Explicit runtime fallback provider override with no
			// `options.strategy` is invalid even when the introspected
			// chain has children. Mirrors the resolver-level contract;
			// surfacing the dedicated reason keeps the metadata
			// classifier aligned with what the request actually
			// failed on.
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
		if _, present, valid := roundrobin.InspectStrategyOverride(reg, clientName); present && !valid {
			res.PathReason = PathReasonInvalidStrategyOverride
		} else if hasExplicitStrategyProviderWithoutStrategy(reg, clientName) {
			// Explicit runtime RR provider override with no
			// `options.strategy` is invalid even when the
			// introspected map has a static chain. Mirrors the
			// resolver-level contract: BAML's eager parse rejects
			// the missing-strategy shape, so the metadata
			// classifier surfaces invalid-strategy-override (not
			// the generic roundrobin-legacy-only reason) so
			// X-BAML-Path-Reason matches what BAML would have
			// emitted in pure legacy mode.
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

// newPlanBase returns a *bamlutils.Metadata pre-populated with the planned-
// phase preamble shared by every plan builder in this file: Phase, Path,
// BuildRequestAPI, Client, RetryPolicy, and the RetryMax pointer-to-copied-
// local. Callers layer per-flavor fields (Provider, Strategy, PathReason,
// Chain, LegacyChildren) onto the returned plan.
//
// Legacy plans pass buildRequestAPI == "" — bamlutils.Metadata declares the
// field with `omitempty`, so the empty value is dropped from the JSON
// payload and matches the previous hand-written literals byte-for-byte.
//
// The RetryMax assignment uses a copied local rather than &retryPolicy.MaxRetries
// directly so the plan owns its own backing int — preserving the existing
// ownership pattern that downstream consumers (and tests) observe.
func newPlanBase(path, clientName string, retryPolicy *retry.Policy, buildRequestAPI string) *bamlutils.Metadata {
	plan := &bamlutils.Metadata{
		Phase:           bamlutils.MetadataPhasePlanned,
		Path:            path,
		BuildRequestAPI: buildRequestAPI,
		Client:          clientName,
		RetryPolicy:     EncodeRetryPolicy(retryPolicy),
	}
	if retryPolicy != nil {
		m := retryPolicy.MaxRetries
		plan.RetryMax = &m
	}
	return plan
}

// orderedLegacyChildren returns the chain-ordered subset of children whose
// legacy[child] is true. Returns nil — not an empty slice — when there are
// no matches, so plan.LegacyChildren keeps a uniform in-memory shape across
// every builder: callers (and tests) can treat nil as "no legacy children"
// without distinguishing it from an empty slice. The wire payload is
// unaffected either way because Metadata.LegacyChildren is `omitempty` and
// drops both, but the in-memory consistency is the load-bearing invariant.
func orderedLegacyChildren(chain []string, legacy map[string]bool) []string {
	if len(legacy) == 0 {
		return nil
	}
	names := make([]string, 0, len(legacy))
	for _, child := range chain {
		if legacy[child] {
			names = append(names, child)
		}
	}
	if len(names) == 0 {
		return nil
	}
	return names
}

// rebuildLegacyChainMetadata walks chain and produces the per-child provider
// map and the legacy classification map used to populate plan.Chain /
// plan.LegacyChildren when the resolver returned no chain (typically the
// runtime-override path: ResolveFallbackChain* returned chain == nil and the
// caller wants the introspected chain enumerated for observability).
//
// Classification mirrors the resolver's own predicate after #234: a child is
// legacy if its resolved provider is empty, is itself a strategy wrapper
// (BuildRequest can't drive RR / fallback as a leaf), or is rejected by the
// supplied support predicate. Passing isProviderSupported == nil keeps
// strategy children legacy while leaving ordinary leaves drivable, matching
// the resolver's nil-predicate behaviour.
func rebuildLegacyChainMetadata(
	reg *bamlutils.ClientRegistry,
	chain []string,
	clientProviders map[string]string,
	isProviderSupported func(string) bool,
) (providers map[string]string, legacy map[string]bool) {
	providers = make(map[string]string, len(chain))
	legacy = make(map[string]bool)
	for _, child := range chain {
		p := ResolveClientProvider(reg, child, clientProviders)
		providers[child] = p
		if p == "" || isStrategyProvider(p) || (isProviderSupported != nil && !isProviderSupported(p)) {
			legacy[child] = true
		}
	}
	return providers, legacy
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
	plan := newPlanBase("buildrequest", ResolvePrimaryClient(adapter, defaultClientName), retryPolicy, buildRequestAPI)
	plan.Provider = provider
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
	plan := newPlanBase("buildrequest", clientName, retryPolicy, buildRequestAPI)
	plan.Provider = provider
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
	plan := newPlanBase("buildrequest", ResolvePrimaryClient(adapter, defaultClientName), retryPolicy, buildRequestAPI)
	plan.Strategy = "baml-fallback"
	plan.PathReason = pathReason
	plan.Chain = append([]string(nil), chain...)
	plan.LegacyChildren = orderedLegacyChildren(chain, legacyChildren)
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
// runtime on each worker). Callers should pass the reason produced
// by ResolveFallbackChainForClientWithReason so metadata consumers
// see the composition rather than the generic "buildrequest
// fallback" shape.
func BuildFallbackChainPlanForClient(
	clientName string,
	chain []string,
	providers map[string]string,
	legacyChildren map[string]bool,
	retryPolicy *retry.Policy,
	buildRequestAPI string,
	pathReason string,
) *bamlutils.Metadata {
	plan := newPlanBase("buildrequest", clientName, retryPolicy, buildRequestAPI)
	plan.Strategy = "baml-fallback"
	plan.PathReason = pathReason
	plan.Chain = append([]string(nil), chain...)
	plan.LegacyChildren = orderedLegacyChildren(chain, legacyChildren)
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
//   - an unresolved baml-roundrobin top-level client — fires on
//     older adapters without SupportsWithClient, when
//     BAML_REST_USE_BUILD_REQUEST=false (the codegen ResolveEffective-
//     Client gate is `supportsWithClient && __useBuildRequest`, so the
//     flag-off path leaves top-level RR unresolved even on modern
//     adapters), or defensively if RR resolution reached this builder
//     without being unwrapped upstream (PathReasonRoundRobin)
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

	plan := newPlanBase("legacy", resolution.Client, retryPolicy, "")
	plan.Strategy = resolution.Strategy
	plan.PathReason = resolution.PathReason
	// Only populate Provider for non-strategy routes. When Strategy is set
	// (e.g. "baml-fallback"), resolution.Provider echoes the strategy name,
	// which would misrepresent it as a real provider in the header /
	// metadata payload. The strategy name alone tells the story; per-child
	// provider info lives in Chain / LegacyChildren.
	if resolution.Strategy == "" {
		plan.Provider = resolution.Provider
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
		// Critically, key only on resolution.Client. Falling through
		// to `defaultClientName`'s chain when the runtime client has
		// no resolvable chain emits metadata with `Client=<runtime>`,
		// `Chain=<defaultClient's chain>` — a mismatch that misleads
		// operators when a primary override points at a fallback
		// client with an invalid strategy override or empty chain.
		// Leaving plan.Chain empty in that case keeps the metadata
		// honest about what happens at runtime.
		//
		// Skip the rebuild when the resolver returned an invalid-
		// override classification (provider / strategy / start
		// invalid) — surfacing a populated Chain in metadata for a
		// request the resolver rejected as malformed would mislead
		// operators into thinking the static chain ran. Mirrors the
		// guard in BuildLegacyMetadataPlanForClient's `baml-fallback`
		// arm so both classification seams are consistent.
		if chain == nil && !isInvalidOverrideReason(reason) {
			reg := adapter.OriginalClientRegistry()
			chain = resolveFallbackStrategyChain(reg, resolution.Client, fallbackChains)
			providers, legacy = rebuildLegacyChainMetadata(reg, chain, clientProviders, isProviderSupported)
		}
		if len(chain) > 0 {
			plan.Chain = append([]string(nil), chain...)
			plan.LegacyChildren = orderedLegacyChildren(chain, legacy)
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
	// invalid override on metadata.
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
	// this mirrors that for the metadata path.
	provider = normalizeStrategyProvider(provider)

	plan := newPlanBase("legacy", clientName, retryPolicy, "")

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
		// Skip the introspected-chain rebuild when the resolver
		// returned an invalid-override classification: surfacing
		// Chain in the metadata for a request the resolver
		// rejected would mislead operators into thinking the
		// chain ran. The metadata classifier should reflect the
		// resolver's actual decision, not paper over it with the
		// static fallback. Empty-chain / empty-child-provider /
		// all-legacy reasons keep the existing rebuild — those
		// are observability cases where the static chain shape
		// is still useful context.
		if chain == nil && !isInvalidOverrideReason(reason) {
			chain = resolveFallbackStrategyChain(reg, clientName, fallbackChains)
			providers, legacy = rebuildLegacyChainMetadata(reg, chain, clientProviders, isProviderSupported)
		}
		if len(chain) > 0 {
			plan.Chain = append([]string(nil), chain...)
			plan.LegacyChildren = orderedLegacyChildren(chain, legacy)
			_ = providers
		}
	case "baml-roundrobin":
		// RoundRobin is normally resolved upstream before the legacy
		// plan is built. We reach here in five cases:
		//   - The RR resolver returned an error (cycle / empty chain)
		//     and the caller fell through with the unresolved client
		//     name.
		//   - ResolveEffectiveClient short-circuited an invalid runtime
		//     strategy override by returning the un-unwrapped client.
		//     The routing is correct (legacy) but the *emitted
		//     metadata* needs to reflect WHY — operators reading the
		//     X-BAML-Path-Reason header should see the invalid-
		//     override classification, not the generic RR-legacy
		//     reason.
		//   - ResolveEffectiveClient short-circuited an invalid runtime
		//     `options.start` override. Same treatment as the strategy
		//     case but a distinct reason so operators can tell which
		//     option was malformed.
		//   - Pre-0.219 adapters (no SupportsWithClient): the codegen
		//     gate skips ResolveEffectiveClient entirely; the
		//     unresolved RR client lands here and surfaces
		//     PathReasonRoundRobin.
		//   - BAML_REST_USE_BUILD_REQUEST=false: the codegen gate is
		//     `supportsWithClient && __useBuildRequest`, so flipping
		//     the flag off also skips the unwrap on a modern adapter.
		//     PathReasonRoundRobin surfaces here too.
		plan.Strategy = "baml-roundrobin"
		if _, present, valid := roundrobin.InspectStrategyOverride(reg, clientName); present && !valid {
			plan.PathReason = PathReasonInvalidStrategyOverride
		} else if hasExplicitStrategyProviderWithoutStrategy(reg, clientName) {
			// Explicit runtime RR provider with no
			// `options.strategy` — same invalidity class as
			// present-but-malformed strategy, surfaces the same
			// path-reason. Mirrors the resolver-level (and
			// ResolveProviderWithReason) contract so every
			// classification seam emits the same reason for the
			// same operator input.
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
