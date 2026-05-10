package buildrequest

import (
	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/buildrequest/roundrobin"
)

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
//     PathReasonInvalidStrategyOverride,
//     PathReasonInvalidProviderOverride,
//     PathReasonInvalidRoundRobinStartOverride,
//     PathReasonFallbackEmptyChain,
//     PathReasonFallbackEmptyChildProvider, PathReasonFallbackAllLegacy.
//     The three Invalid* reasons surface when a chain child or any
//     strategy parent transitively reachable from one carries an
//     explicit-but-malformed runtime override; the request routes to
//     top-level legacy and BAML emits its canonical validation error.
//   - chain != nil, reason == "": fully drivable chain.
//   - chain != nil, reason == PathReasonFallbackRoundRobinChildLegacy:
//     drivable chain that contains a baml-roundrobin child whose
//     rotation is left to BAML's per-worker runtime. Informational
//     metadata, not a failure — callers use the chain normally.
//
// Callers gate on `len(chain) > 0` for the hard routing decision and
// pass `reason` straight through to metadata.
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
// Return contract matches the sibling — see
// ResolveFallbackChainWithReason's doc for the full reason matrix.
// In summary: `chain == nil` is the hard-failure signal; `chain != nil`
// is drivable. When `chain == nil` and `reason != ""`, reason is one
// of PathReasonInvalidStrategyOverride,
// PathReasonInvalidProviderOverride,
// PathReasonInvalidRoundRobinStartOverride,
// PathReasonFallbackEmptyChain, PathReasonFallbackEmptyChildProvider,
// or PathReasonFallbackAllLegacy. When `chain != nil`, reason is
// either empty or the informational
// PathReasonFallbackRoundRobinChildLegacy. Callers gate on
// `len(chain) > 0` for routing.
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
	if _, present, valid := roundrobin.InspectStrategyOverride(reg, clientName); present && !valid {
		return nil, nil, nil, PathReasonInvalidStrategyOverride
	}

	// Same shape for the missing-strategy case: an explicit runtime
	// fallback provider override with no `options.strategy` is
	// invalid even when the introspected map has a static chain.
	// BAML's eager parse rejects the missing-strategy shape; the
	// helper mirrors the resolver-level (roundrobin.Resolve) and
	// metadata-classifier (ResolveProviderWithReason) contracts so
	// every classification seam emits the same path-reason for
	// the same operator input.
	if hasExplicitStrategyProviderWithoutStrategy(reg, clientName) {
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
		// Preflight (provider): a present-empty `provider` on any
		// chain child — strategy parent or leaf — is an explicit
		// operator override that BAML's CFFI rejects on eager parse.
		// Surfacing PathReasonInvalidProviderOverride here preempts
		// the generic FallbackEmptyChildProvider so operators get the
		// same canonical reason whether the typo targets the top-
		// level client or a nested chain child.
		if hasInvalidProviderOverride(reg, child) {
			return nil, nil, nil, PathReasonInvalidProviderOverride
		}

		p := ResolveClientProvider(reg, child, clientProviders)
		if p == "" {
			return nil, nil, nil, PathReasonFallbackEmptyChildProvider
		}
		chainProviders[child] = p

		isStrategyChild := isStrategyProvider(p)

		// Preflight (strategy/start): a nested strategy-parent child,
		// or any strategy parent transitively reachable from it,
		// carrying an invalid runtime override (malformed strategy,
		// non-int start, present-empty provider) must short-circuit
		// the whole chain to legacy. Without this, the mixed-mode
		// legacy callback would forward the invalid entry into BAML
		// inside the per-child callback, where BAML's eager-parse
		// rejection is re-classified as an ordinary fallback child
		// failure — and a later successful sibling in the outer
		// chain silently masks the operator's misconfiguration.
		//
		// Routing the whole request to top-level legacy lets BAML
		// emit its canonical validation error against the full
		// runtime registry exactly as it would in pure legacy mode,
		// matching the contract the top-level preflight already
		// enforces for invalid overrides on the request's effective
		// client.
		//
		// Transitive scope mirrors the per-callback scoped registry
		// (BuildLegacyChildRegistryEntries / computeReachableSubgraph):
		// both follow runtime strategy overrides when present-and-
		// valid, fall back to the introspected chain otherwise, and
		// use a visited set to guard cycles. So a valid runtime
		// override at any depth keeps the mixed-mode path, and an
		// invalid override at any depth re-routes uniformly.
		if isStrategyChild {
			if reason := FindInvalidReachableStrategyOverride(
				reg, child, clientProviders, fallbackChains,
			); reason != "" {
				return nil, nil, nil, reason
			}
		}

		// A nil support check means the caller couldn't determine support,
		// not "nothing is supported" — treat ordinary leaf providers as
		// drivable in that case (unknown support → not legacy) rather
		// than panicking on the nil-func call or misclassifying the
		// whole chain as legacy.
		//
		// Strategy-provider children (round-robin, fallback) are always
		// classified legacy here regardless of the support predicate:
		// BuildRequest cannot drive a strategy wrapper as a leaf, so
		// the wrapper child must take the legacy callback path even
		// when the caller passed nil for isProviderSupported.
		if isStrategyChild || (isProviderSupported != nil && !isProviderSupported(p)) {
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
	// unwrapping of RR children inside fallback chains is deferred.
	if hasRoundRobinChild {
		return resolvedChain, chainProviders, chainLegacy, PathReasonFallbackRoundRobinChildLegacy
	}

	return resolvedChain, chainProviders, chainLegacy, ""
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
// skips primary-override resolution and treats clientName as
// authoritative.
//
// Delegates to ResolveFallbackChainForClientWithReason and discards the
// reason. Callers that don't need the path-reason classification (the
// metadata-emitting sites consume the reason directly via the WithReason
// variant) get the same chain / providers / legacyChildren contract:
// `chain == nil` is the hard-failure signal, `chain != nil` is drivable.
//
// `reason` carries informational metadata
// (PathReasonFallbackRoundRobinChildLegacy) when chain is non-nil; it is
// never a hard failure on a drivable chain. Discarding it here is safe:
// the no-Reason variant has no observable reason channel and the
// behaviour matches what callers already expect.
func ResolveFallbackChainForClient(
	reg *bamlutils.ClientRegistry,
	clientName string,
	fallbackChains map[string][]string,
	clientProviders map[string]string,
	isProviderSupported func(string) bool,
) (chain []string, providers map[string]string, legacyChildren map[string]bool) {
	chain, providers, legacyChildren, _ = ResolveFallbackChainForClientWithReason(
		reg, clientName, fallbackChains, clientProviders, isProviderSupported,
	)
	return chain, providers, legacyChildren
}
