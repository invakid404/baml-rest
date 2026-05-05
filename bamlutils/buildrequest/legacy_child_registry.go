package buildrequest

import (
	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/buildrequest/roundrobin"
)

// LegacyChildRegistryEntry is one client to forward into the
// per-callback upstream BAML registry built by a generated adapter
// helper (makeLegacyChildOptionsFromAdapter). The helper iterates this
// slice, calling the adapter's baml.ClientRegistry.AddLlmClient for
// each entry.
//
// Provider has already been materialised via UpstreamClientRegistryProvider
// (omitted-provider entries get the introspected fallback;
// "baml-roundrobin" is translated to "baml-round-robin"), so the
// generated helper does no further provider rewriting.
type LegacyChildRegistryEntry struct {
	Name     string
	Provider string
	Options  map[string]any
}

// BuildLegacyChildRegistryEntries returns the entries for a fresh
// upstream BAML registry scoped to a single mixed-mode legacy child
// callback rooted at clientOverride.
//
// When ResolveFallbackChainForClientWithReason classifies a fallback
// child as legacy because its provider is itself a strategy parent
// (round-robin or nested fallback), the orchestrator drives that child
// via LegacyStreamChild / LegacyCallChild. Reusing the outer
// BuildRequest-safe options for those callbacks would hand BAML a
// registry that intentionally drops every resolved strategy parent —
// so a runtime override on the nested parent (changed strategy,
// runtime start, dynamic-only definition) silently fails: BAML either
// falls through to the compiled static parent or returns
// client-not-found.
//
// The fix is per-callback scope. The returned registry must contain:
//
//   - Every non-strategy runtime client REACHABLE from clientOverride
//     through the strategy graph. Required so dynamic leaves declared
//     at request time (e.g. a TenantLeaf under a runtime-redirected
//     RR) survive into the BAML CFFI seam. Unrelated runtime leaves —
//     non-strategy clients that aren't reachable from the callback
//     root — are intentionally dropped: forwarding them would leak
//     names BAML's CFFI shouldn't see for this child, defeating the
//     dual-view split.
//   - The target strategy parent (clientOverride) when it carries an
//     explicit override (a Provider, options.strategy, or
//     options.start). Inert presence-only entries are dropped — BAML
//     would reject the missing-strategy shape.
//   - Every strategy parent transitively reachable from clientOverride
//     through the runtime strategy chain (with introspected fallback
//     when no runtime override is present). This covers nested
//     compositions like InnerFallback → [LeafA, DeeperRR] where
//     DeeperRR also has a runtime override; BAML executes the whole
//     nested strategy inside the legacy callback, so the registry has
//     to include strategy parents reachable from the callback root.
//
// Cycles in the runtime strategy graph are guarded by a visited set so
// reciprocal definitions (A → B → A, mistakes or hostile inputs) don't
// infinite-loop registry construction.
//
// The returned slice preserves the original Clients order so output is
// deterministic and matches existing AddLlmClient call ordering for
// the BuildRequest-safe view.
//
// reg may be nil — returns nil. introspectedProviders / introspectedChains
// may be nil; the walker treats absent maps as empty.
func BuildLegacyChildRegistryEntries(
	reg *bamlutils.ClientRegistry,
	clientOverride string,
	introspectedProviders map[string]string,
	introspectedChains map[string][]string,
) []LegacyChildRegistryEntry {
	if reg == nil {
		return nil
	}

	reachable := computeReachableSubgraph(
		reg, clientOverride, introspectedProviders, introspectedChains,
	)

	out := make([]LegacyChildRegistryEntry, 0, len(reg.Clients))
	for _, client := range reg.Clients {
		if client == nil {
			continue
		}
		// Reachability gate covers BOTH strategy parents and non-
		// strategy leaves. Without the leaf gate, an unrelated runtime
		// leaf (e.g. a dynamic LLM client unrelated to the legacy-
		// callback subgraph) would leak into the per-callback registry
		// and resolve names BAML's CFFI shouldn't see for this child.
		// Reachable leaves like a dynamic TenantLeaf under a
		// runtime-redirected RR are still included because the walker
		// records every visited node, not just strategy parents.
		if _, ok := reachable[client.Name]; !ok {
			continue
		}
		if !bamlutils.IsResolvedStrategyParent(client, introspectedProviders) {
			out = append(out, LegacyChildRegistryEntry{
				Name:     client.Name,
				Provider: bamlutils.UpstreamClientRegistryProvider(client, introspectedProviders),
				Options:  client.Options,
			})
			continue
		}
		if bamlutils.ShouldDropStrategyParentForTopLevelLegacy(client, introspectedProviders) {
			continue
		}
		out = append(out, LegacyChildRegistryEntry{
			Name:     client.Name,
			Provider: bamlutils.UpstreamClientRegistryProvider(client, introspectedProviders),
			Options:  client.Options,
		})
	}
	return out
}

// computeReachableSubgraph walks the strategy graph rooted at rootName
// and returns the set of every node visited — strategy parents and
// non-strategy leaves alike. A node classifies as a strategy parent if
// its runtime entry (or, in the absence of one, its introspected
// provider) classifies via IsResolvedStrategyParent.
//
// Children are selected via InspectStrategyOverride's three-state
// shape:
//
//   - present && valid:   walk the runtime override's chain.
//   - present && !valid:  short-circuit — STOP the walk at this node
//     without descending. Falling through to introspectedChains
//     would silently leak static descendants the operator's
//     malformed override clearly didn't intend. The node itself is
//     still recorded in the reachable set so the inclusion gate
//     keeps it (callers preempt this case via the invalid-nested
//     preflight in production, but the helper is exported and the
//     contract holds for direct callers).
//   - not present:        fall back to introspectedChains[name].
//
// The returned set is the inclusion gate for BuildLegacyChildRegistry-
// Entries: both strategy parents and non-strategy leaves must appear
// here to be forwarded into the per-callback registry. Leaves get
// recorded because a dynamic leaf under a runtime-redirected strategy
// parent (e.g. TenantLeaf under InnerRR.strategy=[TenantLeaf]) has to
// reach the BAML CFFI seam, but unrelated runtime leaves outside this
// subgraph must NOT — they belong to other callbacks.
//
// Walking continues through strategy children regardless of whether
// an inner override is "inert presence-only" — we still need to know
// which strategy parents are reachable so the iterator can decide
// whether to include each. The drop-inert decision happens at the
// inclusion site, not here.
func computeReachableSubgraph(
	reg *bamlutils.ClientRegistry,
	rootName string,
	introspectedProviders map[string]string,
	introspectedChains map[string][]string,
) map[string]struct{} {
	visited := make(map[string]struct{})
	out := make(map[string]struct{})
	var walk func(name string)
	walk = func(name string) {
		if _, seen := visited[name]; seen {
			return
		}
		visited[name] = struct{}{}
		// Record every visited node, not just strategy parents.
		// Non-strategy leaves are reachable terminals of the
		// subgraph; the inclusion gate at the call site needs to
		// distinguish reachable leaves (forward) from unrelated
		// runtime leaves (drop).
		out[name] = struct{}{}

		probe := lookupRuntimeClient(reg, name)
		if probe == nil {
			probe = &bamlutils.ClientProperty{Name: name}
		}
		if !bamlutils.IsResolvedStrategyParent(probe, introspectedProviders) {
			return
		}

		// Three-state branching on the runtime strategy override:
		//   - present && valid → walk the runtime chain
		//   - present && !valid → stop walking; there's no clean
		//     source-of-truth for descendants once the operator's
		//     explicit override is malformed, and silently falling
		//     back to the introspected chain would leak static
		//     descendants the operator's override clearly didn't
		//     intend. Production callers preempt this case via the
		//     invalid-nested preflight, but the helper is exported
		//     and the contract should hold for direct callers.
		//   - absent → walk the introspected chain
		chain, present, valid := roundrobin.InspectStrategyOverride(reg, name)
		switch {
		case present && valid:
			// chain is the runtime override; use it.
		case present && !valid:
			return
		default:
			chain = introspectedChains[name]
		}
		for _, child := range chain {
			walk(child)
		}
	}
	walk(rootName)
	return out
}

func lookupRuntimeClient(reg *bamlutils.ClientRegistry, name string) *bamlutils.ClientProperty {
	if reg == nil {
		return nil
	}
	for _, c := range reg.Clients {
		if c != nil && c.Name == name {
			return c
		}
	}
	return nil
}

// FindInvalidReachableStrategyOverride walks the strategy graph
// rooted at rootName and returns a non-empty PathReason* if any
// reachable strategy parent carries an invalid explicit runtime
// override. Empty string when every reachable strategy parent is
// valid (or rootName is not itself a strategy parent).
//
// Walking covers strategy parents transitively via the same
// reachability rules as computeReachableSubgraph and follows the
// same three-state shape on InspectStrategyOverride:
//
//   - present && valid:   descend into the runtime override's chain.
//   - present && !valid:  short-circuit — return
//     PathReasonInvalidStrategyOverride for this node directly
//     (the malformed override IS the invalidity; falling through
//     to introspectedChains would route to a wrong static child
//     instead of surfacing the operator's misconfiguration).
//   - not present:        descend via introspectedChains[name].
//
// Visited bookkeeping guards reciprocal definitions from infinite-
// looping the walk; a cycle with no invalid override returns empty
// string and the orchestrator keeps the chain.
//
// Per-node precedence mirrors ResolveProviderWithReason's switch
// on a top-level strategy parent — present-empty provider, then
// invalid strategy, then invalid start — so X-BAML-Path-Reason is
// consistent regardless of whether the malformed override sits at
// the top-level client, an immediate fallback child, or a deeply
// nested strategy parent. Walking is depth-first; the first
// invalid node short-circuits the whole walk.
//
// The orchestrator's chain-walk preflight calls this helper with
// rootName == each immediate strategy-parent chain child, so a
// transitively reachable malformed override (deeper RR / fallback
// whose runtime entry is invalid) routes the whole request to
// top-level legacy with the canonical PathReason*. Without
// transitive scope, BAML's per-callback rejection of the deeper
// entry would be re-classified as an ordinary fallback child
// failure inside RunStreamOrchestration / RunCallOrchestration and
// a later successful sibling could silently mask the
// misconfiguration — the validation-suppression class the top-
// level preflight already rules out.
func FindInvalidReachableStrategyOverride(
	reg *bamlutils.ClientRegistry,
	rootName string,
	introspectedProviders map[string]string,
	introspectedChains map[string][]string,
) string {
	visited := make(map[string]struct{})

	// resolveEffectiveProvider returns the provider spelling that
	// drives per-node classification: runtime override when present
	// and non-empty, else the introspected provider. The same
	// resolution rule that drives inStrategyGraph below.
	resolveEffectiveProvider := func(name string) string {
		if rc := lookupRuntimeClient(reg, name); rc != nil &&
			rc.IsProviderPresent() && rc.Provider != "" {
			return rc.Provider
		}
		if introspectedProviders == nil {
			return ""
		}
		return introspectedProviders[name]
	}

	// A node is "in the strategy graph" for walking purposes if its
	// resolved provider classifies as a strategy provider name.
	// IsResolvedStrategyParent alone short-circuits on a present-
	// empty `provider:""` runtime override (Provider == "" is not
	// a strategy spelling), which would skip exactly the node the
	// preflight is supposed to flag — the present-empty override is
	// the invalidity. Routing classification through the introspected
	// provider when the runtime entry is present-empty preserves the
	// graph topology so the per-node invalid-provider check fires.
	inStrategyGraph := func(name string) bool {
		return isStrategyProvider(resolveEffectiveProvider(name))
	}

	var walk func(name string) string
	walk = func(name string) string {
		if _, seen := visited[name]; seen {
			return ""
		}
		visited[name] = struct{}{}

		// hasInvalidProviderOverride must run BEFORE the strategy-graph
		// gate. The previous order (gate first, provider check second)
		// missed the case of a reachable non-strategy leaf with
		// `Provider:"" + ProviderSet:true` deep in a runtime strategy
		// chain — e.g. an operator-typo'd leaf override under a
		// fallback's RR child. inStrategyGraph returns false for such
		// a leaf (the empty provider isn't an RR/fallback spelling and
		// no introspected entry exists either), so the walker would
		// short-circuit without flagging the invalidity and the leaf
		// would still be forwarded into the per-callback scoped
		// registry by BuildLegacyChildRegistryEntries — BAML's eager
		// registry parse would then reject inside the callback, and
		// fallback orchestration could continue to a later sibling,
		// silently masking the operator's misconfiguration. The
		// strategy-scoped checks below (hasInvalidStartOverride,
		// invalid-strategy-override, the explicit-strategy-provider-
		// without-options.strategy guard) stay strategy-graph-gated
		// because `start` and `strategy` options apply only to RR /
		// fallback strategy parents.
		if hasInvalidProviderOverride(reg, name) {
			return PathReasonInvalidProviderOverride
		}

		if !inStrategyGraph(name) {
			return ""
		}

		chain, present, valid := roundrobin.InspectStrategyOverride(reg, name)
		if present && !valid {
			return PathReasonInvalidStrategyOverride
		}
		// An explicit runtime strategy-provider override on this node
		// with no `options.strategy` is invalid even when introspected
		// has a chain for the same name. BAML's eager parse rejects
		// the shape; falling back to the introspected chain would
		// silently route to a wrong static child instead of routing
		// to top-level legacy where BAML emits its canonical
		// ensure_strategy error.
		if rc := lookupRuntimeClient(reg, name); rc != nil &&
			rc.IsProviderPresent() && isStrategyProvider(rc.Provider) && !present {
			return PathReasonInvalidStrategyOverride
		}
		// `start` is RR-specific (round_robin.rs config). A
		// fallback node with `options.start` is not malformed —
		// BAML's fallback parser ignores unknown options. Gate the
		// check on the resolved provider so a malformed start on a
		// fallback node doesn't get misclassified as
		// PathReasonInvalidRoundRobinStartOverride. Mirrors the
		// top-level metadata classifier (orchestrator.go), which
		// only checks hasInvalidStartOverride under the
		// baml-roundrobin arm.
		if roundrobin.IsRoundRobinProvider(resolveEffectiveProvider(name)) &&
			hasInvalidStartOverride(reg, name) {
			return PathReasonInvalidRoundRobinStartOverride
		}

		if !(present && valid) {
			chain = introspectedChains[name]
		}
		for _, child := range chain {
			if reason := walk(child); reason != "" {
				return reason
			}
		}
		return ""
	}
	return walk(rootName)
}
