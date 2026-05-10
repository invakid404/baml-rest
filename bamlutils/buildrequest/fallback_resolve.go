package buildrequest

import (
	"errors"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/buildrequest/roundrobin"
)

// FallbackChainResolution carries the planning output of a fallback-chain
// resolution: the chain to run, per-child provider classification, the
// subset of children that must dispatch through the legacy callback, and
// — when an immediate baml-roundrobin fallback child is centrally
// unwrapped to a leaf via BuildRequest — the per-child dispatch target
// and RR decision (see issue #237 PR 2).
//
// Shape contract:
//
//   - Chain is the operator-authored child list (configured order). Never
//     mutated by centralization; runtime override of the parent's
//     `options.strategy` is honoured.
//   - Providers is per-child resolved provider. For a centralized RR
//     wrapper child, this is the SELECTED LEAF's provider — the
//     orchestrator's IsProviderSupported gate sees the leaf provider,
//     not "baml-roundrobin".
//   - LegacyChildren marks children that still dispatch through the
//     legacy callback (unsupported leaves, deferred strategy shapes,
//     ineligible nested compositions). Centralized RR wrapper children
//     are NOT in this map.
//   - Targets is sparse: keyed by RR wrapper child name, valued by the
//     selected leaf. Only set when target != child. Empty/nil when no
//     centralization happened.
//   - NestedRoundRobin is sparse: keyed by RR wrapper child name, valued
//     by the *bamlutils.RoundRobinInfo describing which leaf the RR
//     decision picked. Mirrors Metadata.FallbackRoundRobin's wire shape.
//   - Reason is the informational PathReason* token surfaced in
//     metadata. PathReasonFallbackRoundRobinChildBuildRequest wins over
//     PathReasonFallbackRoundRobinChildLegacy when at least one child
//     is centralized AND the rest stays legacy.
//
// When the chain is rejected wholesale, the helper returns a struct with
// Chain == nil and a Reason explaining why (one of the existing
// PathReason* hard-fail values). Callers gate on `len(.Chain) > 0`
// for the routing decision exactly as they do for the 4-tuple wrapper.
type FallbackChainResolution struct {
	Chain            []string
	Providers        map[string]string
	LegacyChildren   map[string]bool
	Targets          map[string]string
	NestedRoundRobin map[string]*bamlutils.RoundRobinInfo
	Reason           string
}

// ResolveFallbackChainPlan applies the runtime primary override before
// delegating to ResolveFallbackChainPlanForClient. Mirrors
// ResolveFallbackChain's primary-resolution shape.
//
// The advancer is drawn from adapter.RoundRobinAdvancer() to match
// ResolveEffectiveClient's preference (request-scoped RemoteAdvancer
// carries the SharedState idempotency key for cross-worker rotation).
// When the adapter has no per-request advancer, RR resolution degrades
// to AdvanceDynamic — static clients pick a random leaf, just like
// top-level RR on a standalone worker.
//
// The error return matches top-level RR's hard-error semantics: cycle
// detection, empty children, advancer transport errors, and out-of-range
// indices propagate from roundrobin.Resolve. Sentinel-class errors
// (ErrInvalidStrategyOverride / ErrInvalidStartOverride) are converted
// to the existing PathReasonInvalid* values on the returned resolution
// with Chain == nil, so the request falls through to top-level legacy
// and BAML emits the canonical validation error. See section 4 of
// issue #237 PR 2's brief for the full matrix.
func ResolveFallbackChainPlan(
	adapter bamlutils.Adapter,
	defaultClientName string,
	fallbackChains map[string][]string,
	clientProviders map[string]string,
	isProviderSupported func(string) bool,
) (*FallbackChainResolution, error) {
	reg := adapter.OriginalClientRegistry()

	clientName := defaultClientName
	if reg != nil && reg.Primary != nil && *reg.Primary != "" {
		clientName = *reg.Primary
	}
	advancer := adapter.RoundRobinAdvancer()
	return ResolveFallbackChainPlanForClient(reg, clientName, fallbackChains, clientProviders, isProviderSupported, advancer)
}

// ResolveFallbackChainPlanForClient is the primary-override-free sibling
// of ResolveFallbackChainPlan. Use this after the effective client has
// already been resolved upstream (e.g. after RR unwrap). The advancer
// is passed in directly — pass adapter.RoundRobinAdvancer() (preferred)
// or a package-level Coordinator, matching the policy ResolveEffective-
// Client uses for top-level RR.
//
// Returns (nil, nil) when the client is not a baml-fallback strategy —
// the caller's top-level classification picks the path.
func ResolveFallbackChainPlanForClient(
	reg *bamlutils.ClientRegistry,
	clientName string,
	fallbackChains map[string][]string,
	clientProviders map[string]string,
	isProviderSupported func(string) bool,
	advancer roundrobin.Advancer,
) (*FallbackChainResolution, error) {
	parentProvider := ResolveClientProvider(reg, clientName, clientProviders)
	if parentProvider != "baml-fallback" {
		return nil, nil
	}

	// Detect a present-but-unparseable runtime strategy override before
	// chain resolution. Mirrors the 4-tuple wrapper's preflight so the
	// metadata classifier emits PathReasonInvalidStrategyOverride and
	// the request falls through to legacy where BAML emits the canonical
	// ensure_strategy error.
	if _, present, valid := roundrobin.InspectStrategyOverride(reg, clientName); present && !valid {
		return &FallbackChainResolution{Reason: PathReasonInvalidStrategyOverride}, nil
	}
	if hasExplicitStrategyProviderWithoutStrategy(reg, clientName) {
		return &FallbackChainResolution{Reason: PathReasonInvalidStrategyOverride}, nil
	}

	resolvedChain := resolveFallbackStrategyChain(reg, clientName, fallbackChains)
	if len(resolvedChain) == 0 {
		return &FallbackChainResolution{Reason: PathReasonFallbackEmptyChain}, nil
	}

	chainProviders := make(map[string]string, len(resolvedChain))
	chainLegacy := make(map[string]bool)
	var targets map[string]string
	var nestedRR map[string]*bamlutils.RoundRobinInfo
	legacyPositions := 0
	hasRoundRobinChildLegacy := false
	hasRoundRobinChildCentralized := false

	for _, child := range resolvedChain {
		if hasInvalidProviderOverride(reg, child) {
			return &FallbackChainResolution{Reason: PathReasonInvalidProviderOverride}, nil
		}

		p := ResolveClientProvider(reg, child, clientProviders)
		if p == "" {
			return &FallbackChainResolution{Reason: PathReasonFallbackEmptyChildProvider}, nil
		}
		chainProviders[child] = p

		isStrategyChild := isStrategyProvider(p)

		if isStrategyChild {
			if reason := FindInvalidReachableStrategyOverride(
				reg, child, clientProviders, fallbackChains,
			); reason != "" {
				return &FallbackChainResolution{Reason: reason}, nil
			}
		}

		// Per #237 PR 2: attempt centralization for an immediate
		// baml-roundrobin fallback child. Eligibility (see brief
		// section 2):
		//   1. Resolved provider is RR (this branch).
		//   2. Invalid-override preflight passed (handled above for
		//      every strategy child).
		//   3. RR resolution yields a selected leaf without a hard error.
		//   4. Selected leaf is non-strategy (not RR, not fallback).
		//   5. Selected leaf's provider is non-empty AND supported by
		//      isProviderSupported (or isProviderSupported is non-nil).
		//
		// Hard errors from roundrobin.Resolve — cycle detection, empty
		// children, advancer transport errors, out-of-range indices —
		// propagate up so the request fails fast. Sentinel-class
		// errors (ErrInvalidStrategyOverride, ErrInvalidStartOverride)
		// shouldn't fire here: the preflight above already caught the
		// per-child shapes that produce them. Translate defensively
		// anyway so a future divergence between preflight and resolver
		// surfaces as a top-level legacy fallthrough rather than a
		// silent request-fatal.
		if roundrobin.IsRoundRobinProvider(p) {
			leaf, leafProvider, info, err := resolveImmediateRRChild(reg, child, fallbackChains, clientProviders, advancer)
			if err != nil {
				if errors.Is(err, roundrobin.ErrInvalidStrategyOverride) {
					return &FallbackChainResolution{Reason: PathReasonInvalidStrategyOverride}, nil
				}
				if errors.Is(err, roundrobin.ErrInvalidStartOverride) {
					return &FallbackChainResolution{Reason: PathReasonInvalidRoundRobinStartOverride}, nil
				}
				// Hard error — propagate to the caller; the request
				// fails fast, matching top-level RR semantics.
				return nil, err
			}
			if isCentralizationEligible(leaf, leafProvider, isProviderSupported) {
				// Centralize: drop legacy classification, surface
				// leaf provider for support gating, record the
				// dispatch target and RR decision.
				chainProviders[child] = leafProvider
				if leaf != child {
					if targets == nil {
						targets = make(map[string]string)
					}
					targets[child] = leaf
				}
				if info != nil {
					if nestedRR == nil {
						nestedRR = make(map[string]*bamlutils.RoundRobinInfo)
					}
					nestedRR[child] = info
				}
				hasRoundRobinChildCentralized = true
				continue
			}
			// Ineligible — fall through to the legacy classification
			// path below. The wrapper provider stays on chainProviders
			// (already set above before the centralization attempt
			// rewrote it for the success path; rewrite the value back
			// since the centralization attempt didn't take).
			chainProviders[child] = p
			hasRoundRobinChildLegacy = true
		}

		// Strategy-provider children (RR — ineligible, fallback)
		// always land on the legacy child list. Ordinary leaves
		// follow the support predicate (nil predicate keeps them
		// drivable, matching #234).
		if isStrategyChild || (isProviderSupported != nil && !isProviderSupported(p)) {
			chainLegacy[child] = true
			legacyPositions++
		}
	}

	if legacyPositions == len(resolvedChain) {
		return &FallbackChainResolution{Reason: PathReasonFallbackAllLegacy}, nil
	}

	reason := ""
	switch {
	case hasRoundRobinChildCentralized:
		// Per brief: ChildBuildRequest wins when at least one RR child
		// was centralized, even if a sibling RR child stayed legacy.
		reason = PathReasonFallbackRoundRobinChildBuildRequest
	case hasRoundRobinChildLegacy:
		reason = PathReasonFallbackRoundRobinChildLegacy
	}

	return &FallbackChainResolution{
		Chain:            resolvedChain,
		Providers:        chainProviders,
		LegacyChildren:   chainLegacy,
		Targets:          targets,
		NestedRoundRobin: nestedRR,
		Reason:           reason,
	}, nil
}

// resolveImmediateRRChild attempts to unwrap a single immediate
// baml-roundrobin fallback child to a leaf using the same advancer
// preference as ResolveEffectiveClient — so the request-scoped
// RemoteAdvancer (when present) carries the same (key, op_id)
// idempotency surface across pool retries.
//
// Returns (selectedLeaf, leafProvider, info, nil) on a clean resolution.
// Returns ("", "", nil, err) for hard errors (cycle, empty children,
// advancer transport, out-of-range) and sentinel-class errors
// (ErrInvalidStrategyOverride / ErrInvalidStartOverride) — the caller
// translates the sentinel class into a top-level legacy fallthrough and
// the hard-error class into request-fatal propagation.
//
// Eligibility checks #4 / #5 (non-strategy leaf, BR-supported leaf
// provider) live in the caller (isCentralizationEligible) so the helper
// stays a thin wrapper around roundrobin.Resolve.
func resolveImmediateRRChild(
	reg *bamlutils.ClientRegistry,
	rrChildName string,
	fallbackChains map[string][]string,
	clientProviders map[string]string,
	advancer roundrobin.Advancer,
) (selectedLeaf string, leafProvider string, info *bamlutils.RoundRobinInfo, err error) {
	res, err := roundrobin.Resolve(roundrobin.ResolveInput{
		ClientName:      rrChildName,
		Registry:        reg,
		FallbackChains:  fallbackChains,
		ClientProviders: clientProviders,
		Advancer:        advancer,
	})
	if err != nil {
		return "", "", nil, err
	}
	leaf := res.Selected
	prov := ResolveClientProvider(reg, leaf, clientProviders)
	return leaf, prov, res.Info, nil
}

// isCentralizationEligible enforces the leaf-side eligibility checks
// (#4 non-strategy leaf, #5 BR-supported leaf provider). A nil
// isProviderSupported is treated as "unable to determine support" —
// the centralization path is intentionally conservative under nil
// (the orchestrator's IsProviderSupported gate has no signal to gate
// on, so dispatching as drivable could route an unsupported leaf
// through BuildRequest). Ordinary leaves under nil-support stay
// drivable via the existing legacy-classification predicate, which
// matches #234's contract for the same nil case.
func isCentralizationEligible(leaf, leafProvider string, isProviderSupported func(string) bool) bool {
	if leaf == "" || leafProvider == "" {
		return false
	}
	if isStrategyProvider(leafProvider) {
		return false
	}
	if isProviderSupported == nil {
		return false
	}
	return isProviderSupported(leafProvider)
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
//     PathReasonInvalidStrategyOverride,
//     PathReasonInvalidProviderOverride,
//     PathReasonInvalidRoundRobinStartOverride,
//     PathReasonFallbackEmptyChain,
//     PathReasonFallbackEmptyChildProvider, PathReasonFallbackAllLegacy.
//   - chain != nil, reason == "": fully drivable chain.
//   - chain != nil, reason == PathReasonFallbackRoundRobinChildLegacy:
//     drivable chain containing a baml-roundrobin child that is
//     ineligible for centralization (e.g. selected leaf unsupported,
//     selected leaf is itself strategy, or the support predicate is nil).
//     The wrapper child still runs via the legacy callback.
//   - chain != nil, reason == PathReasonFallbackRoundRobinChildBuildRequest:
//     drivable chain containing at least one baml-roundrobin child
//     centrally unwrapped to a leaf via BuildRequest.
//
// Callers gate on `len(chain) > 0` for the hard routing decision and
// pass `reason` straight through to metadata.
//
// This 4-tuple shape is the compatibility surface that the generated
// router emits against. The typed sibling
// ResolveFallbackChainPlan{,ForClient} returns FallbackChainResolution
// directly — call sites that need Targets / NestedRoundRobin to populate
// orchestrator dispatch must use the typed helper (PR 3 wires codegen
// through it; PR 2 keeps the 4-tuple wrapper unchanged at its existing
// call sites).
//
// Hard errors from nested RR resolution (cycle, empty children, advancer
// transport, out-of-range) are swallowed by this wrapper: the per-child
// centralization downgrades to legacy classification for the affected
// wrapper child, preserving the 4-tuple wrapper's "drivable chain or
// chain == nil" contract. Callers that need the request-fatal posture
// for hard errors must use ResolveFallbackChainPlanForClient directly.
func ResolveFallbackChainWithReason(
	adapter bamlutils.Adapter,
	defaultClientName string,
	fallbackChains map[string][]string,
	clientProviders map[string]string,
	isProviderSupported func(string) bool,
) (chain []string, providers map[string]string, legacyChildren map[string]bool, reason string) {
	reg := adapter.OriginalClientRegistry()

	clientName := defaultClientName
	if reg != nil && reg.Primary != nil && *reg.Primary != "" {
		clientName = *reg.Primary
	}

	return resolveFallbackChainForClientWithAdvancer(
		reg, clientName, fallbackChains, clientProviders, isProviderSupported,
		adapter.RoundRobinAdvancer(),
	)
}

// ResolveFallbackChainForClientWithReason is the primary-override-free
// sibling of ResolveFallbackChainWithReason. Callers that have already
// resolved the effective client name (e.g. after external round-robin
// unwrap) pass that name directly; no further primary lookup or RR
// unwrap happens inside.
//
// Return contract matches the sibling — see
// ResolveFallbackChainWithReason's doc for the full reason matrix.
// Centralization of immediate RR children with a BR-supported leaf
// surfaces as `chain != nil` + reason == PathReasonFallbackRoundRobinChildBuildRequest,
// with the wrapper child REMOVED from legacyChildren and providers[child]
// set to the leaf's provider. Targets / NestedRoundRobin information is
// only available via ResolveFallbackChainPlanForClient.
func ResolveFallbackChainForClientWithReason(
	reg *bamlutils.ClientRegistry,
	clientName string,
	fallbackChains map[string][]string,
	clientProviders map[string]string,
	isProviderSupported func(string) bool,
) (chain []string, providers map[string]string, legacyChildren map[string]bool, reason string) {
	// No advancer at this seam — the 4-tuple wrapper is kept callable
	// without an adapter. Nested RR resolution falls back to
	// AdvanceDynamic for static clients, matching top-level RR's
	// standalone-worker behaviour. Codegen call sites get this shape
	// today; PR 3 will switch them to ResolveFallbackChainPlanForClient
	// so the request-scoped RemoteAdvancer threads through.
	return resolveFallbackChainForClientWithAdvancer(
		reg, clientName, fallbackChains, clientProviders, isProviderSupported, nil,
	)
}

// resolveFallbackChainForClientWithAdvancer drives both 4-tuple wrappers
// off the typed helper. It tolerates hard errors from nested RR
// resolution by demoting the affected wrapper child to legacy
// classification — preserving the 4-tuple contract where a non-nil
// chain is always drivable.
func resolveFallbackChainForClientWithAdvancer(
	reg *bamlutils.ClientRegistry,
	clientName string,
	fallbackChains map[string][]string,
	clientProviders map[string]string,
	isProviderSupported func(string) bool,
	advancer roundrobin.Advancer,
) (chain []string, providers map[string]string, legacyChildren map[string]bool, reason string) {
	res, err := ResolveFallbackChainPlanForClient(
		reg, clientName, fallbackChains, clientProviders, isProviderSupported, advancer,
	)
	if err != nil {
		// Hard error from nested RR (cycle / empty children / advancer
		// transport). The 4-tuple wrapper's compat contract does not
		// surface errors — demote to the existing "RR child stays
		// legacy" classification by re-running the resolution with
		// centralization disabled. This keeps PR 2 a strict superset
		// of pre-PR 2 behaviour for the 4-tuple seam; the typed helper
		// is the entry point that propagates errors per top-level RR
		// semantics (see issue #237 PR 2 section 4).
		return resolveFallbackChainLegacyClassification(
			reg, clientName, fallbackChains, clientProviders, isProviderSupported,
		)
	}
	if res == nil {
		return nil, nil, nil, ""
	}
	return res.Chain, res.Providers, res.LegacyChildren, res.Reason
}

// resolveFallbackChainLegacyClassification runs the chain resolution
// without attempting RR-child centralization — used by the 4-tuple
// wrapper as a safety net when the typed helper would otherwise return a
// hard error. The output matches the pre-PR-2 classification: any
// baml-roundrobin child lands on legacyChildren with reason
// PathReasonFallbackRoundRobinChildLegacy.
func resolveFallbackChainLegacyClassification(
	reg *bamlutils.ClientRegistry,
	clientName string,
	fallbackChains map[string][]string,
	clientProviders map[string]string,
	isProviderSupported func(string) bool,
) (chain []string, providers map[string]string, legacyChildren map[string]bool, reason string) {
	parentProvider := ResolveClientProvider(reg, clientName, clientProviders)
	if parentProvider != "baml-fallback" {
		return nil, nil, nil, ""
	}
	if _, present, valid := roundrobin.InspectStrategyOverride(reg, clientName); present && !valid {
		return nil, nil, nil, PathReasonInvalidStrategyOverride
	}
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
		if hasInvalidProviderOverride(reg, child) {
			return nil, nil, nil, PathReasonInvalidProviderOverride
		}
		p := ResolveClientProvider(reg, child, clientProviders)
		if p == "" {
			return nil, nil, nil, PathReasonFallbackEmptyChildProvider
		}
		chainProviders[child] = p

		isStrategyChild := isStrategyProvider(p)
		if isStrategyChild {
			if r := FindInvalidReachableStrategyOverride(
				reg, child, clientProviders, fallbackChains,
			); r != "" {
				return nil, nil, nil, r
			}
		}
		if isStrategyChild || (isProviderSupported != nil && !isProviderSupported(p)) {
			chainLegacy[child] = true
			legacyPositions++
		}
		if roundrobin.IsRoundRobinProvider(p) {
			hasRoundRobinChild = true
		}
	}

	if legacyPositions == len(resolvedChain) {
		return nil, nil, nil, PathReasonFallbackAllLegacy
	}
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
	chain, providers, legacyChildren, _ = ResolveFallbackChainWithReason(
		adapter, defaultClientName, fallbackChains, clientProviders, isProviderSupported,
	)
	return chain, providers, legacyChildren
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
