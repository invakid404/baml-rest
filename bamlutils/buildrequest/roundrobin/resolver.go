package roundrobin

import (
	"errors"
	"fmt"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/strategyparse"
)

// ErrInvalidStrategyOverride signals that the runtime client_registry
// supplied a `strategy` value that ParseStrategyOption could not
// interpret as a non-empty bracketed list (bare token, half-bracketed,
// empty array, heterogeneous []any with non-string elements, etc.).
//
// BAML upstream's ensure_strategy rejects these inputs in
// baml-lib/llm-client/src/clients/helpers.rs:790-829 with
// "strategy must be an array" / "strategy must not be empty". Resolve
// returns this sentinel so callers (ResolveEffectiveClient) can skip
// the RR unwrap and let the request fall through to the legacy path,
// where BAML's runtime emits the canonical error rather than us
// silently using the introspected chain. See PR #192 cold-review-2
// finding 1.
var ErrInvalidStrategyOverride = errors.New("roundrobin: runtime strategy override is invalid or empty")

// CanonicalProvider is the spelling emitted in metadata and headers
// regardless of which input form the .baml source or runtime override
// used. Kept backward-compatible with the existing legacy-path value.
const CanonicalProvider = "baml-roundrobin"

// IsRoundRobinProvider reports whether the provider string identifies the
// round-robin strategy. Accepts three spellings to cover:
//   - "baml-roundrobin": the form already emitted by the legacy path and
//     used throughout baml-rest's introspected output.
//   - "baml-round-robin": the hyphenated form BAML upstream parses.
//   - "round-robin": the shorthand BAML internally routes through.
func IsRoundRobinProvider(provider string) bool {
	switch provider {
	case "baml-roundrobin", "baml-round-robin", "round-robin":
		return true
	}
	return false
}

// NormalizeProvider returns the canonical baml-roundrobin spelling when the
// input matches any accepted variant, otherwise returns the input verbatim.
// Intended for introspection-time normalisation of .baml source values so
// downstream maps use a single spelling.
func NormalizeProvider(provider string) string {
	if IsRoundRobinProvider(provider) {
		return CanonicalProvider
	}
	return provider
}

// ResolveInput carries the inputs needed to resolve a potentially-RR client
// down to a non-RR leaf (a single-provider client or a fallback strategy).
type ResolveInput struct {
	// ClientName is the client to resolve. Callers must have already applied
	// any runtime primary override; this function treats ClientName as the
	// effective top-level client for the request.
	ClientName string
	// Registry is the runtime client_registry override (may be nil). Read
	// for per-client provider overrides and for strategy-option overrides.
	Registry *bamlutils.ClientRegistry
	// FallbackChains is the introspected map of strategy client names to
	// their ordered child lists (applies to both baml-fallback and
	// baml-roundrobin).
	FallbackChains map[string][]string
	// ClientProviders is the introspected map of client name to provider
	// string. The resolver uses it to detect RR strategies.
	ClientProviders map[string]string
	// Advancer drives static-client counter selection. Pass a *Coordinator
	// for in-process rotation (standalone / tests) or a *RemoteAdvancer
	// for pool-hosted workers talking to the host SharedState service.
	// May be nil, in which case every static RR resolution degrades to
	// AdvanceDynamic.
	Advancer Advancer
}

// Result is the outcome of unwrapping round-robin strategies.
type Result struct {
	// Selected is the client name to dispatch to after stripping any RR
	// wrapper(s). For a non-RR input this equals ResolveInput.ClientName.
	Selected string
	// Info describes the outermost RR decision. Nil if no RR resolution
	// occurred during unwrap. For nested RR, the inner counters still
	// advance — operators see only the top-level choice.
	Info *bamlutils.RoundRobinInfo
}

// Resolve recursively unwraps round-robin strategies. Returns the leaf
// client name to dispatch to plus metadata describing the outermost RR
// decision (nil when the resolved client was never a RR strategy).
//
// Errors:
//   - cycle detected (A points at B, B points at A via RR)
//   - RR client with an empty child list
func Resolve(in ResolveInput) (*Result, error) {
	if in.ClientName == "" {
		return &Result{Selected: "", Info: nil}, nil
	}
	seen := make(map[string]bool)
	current := in.ClientName
	var outerInfo *bamlutils.RoundRobinInfo
	for {
		if seen[current] {
			return nil, fmt.Errorf("roundrobin: cycle detected at client %q", current)
		}
		seen[current] = true

		provider := resolveClientProvider(in.Registry, current, in.ClientProviders)
		if !IsRoundRobinProvider(provider) {
			return &Result{Selected: current, Info: outerInfo}, nil
		}

		// Detect a present-but-unparseable runtime strategy override
		// before chain resolution. Surfacing ErrInvalidStrategyOverride
		// lets ResolveEffectiveClient bail out of the RR unwrap and
		// hand the un-unwrapped client name to the dispatcher, which
		// falls through to legacy where BAML's runtime emits the
		// canonical ensure_strategy error.
		if _, present, valid := inspectStrategyOverride(in.Registry, current); present && !valid {
			return nil, ErrInvalidStrategyOverride
		}

		chain := resolveStrategyChain(in.Registry, current, in.FallbackChains)
		if len(chain) == 0 {
			return nil, fmt.Errorf("roundrobin: client %q has no children", current)
		}

		idx, err := selectIndex(in.Advancer, in.Registry, current, len(chain))
		if err != nil {
			return nil, fmt.Errorf("roundrobin: select child for %q: %w", current, err)
		}
		selected := chain[idx]

		if outerInfo == nil {
			outerInfo = &bamlutils.RoundRobinInfo{
				Name:     current,
				Children: append([]string(nil), chain...),
				Index:    idx,
				Selected: selected,
			}
		}
		current = selected
	}
}

// selectIndex picks the child index for a RR resolution. Dynamic clients
// (whose RR identity is declared purely via runtime client_registry) get
// a fresh random selection matching BAML's fresh-Arc-per-context semantics.
// Static clients delegate to the supplied Advancer — either an in-process
// Coordinator or a RemoteAdvancer talking to the host SharedState service.
//
// The Advancer error is surfaced verbatim. For the in-process path it
// never fires; for the remote path it's a transport failure that must
// fail the request, because silently falling back to AdvanceDynamic
// would defeat the whole point of centralised rotation.
func selectIndex(a Advancer, reg *bamlutils.ClientRegistry, clientName string, childCount int) (int, error) {
	if isDynamicRRClient(reg, clientName) {
		return AdvanceDynamic(childCount), nil
	}
	if a == nil {
		return AdvanceDynamic(childCount), nil
	}
	return a.Advance(clientName, childCount)
}

// isDynamicRRClient reports whether a named client's RR identity is
// controlled by runtime client_registry state, in which case the
// coordinator's long-lived counter must not drive selection.
//
// Any registry entry keyed by clientName counts as dynamic — not just a
// provider override. A strategy-only override (registry supplies
// options.strategy but keeps the static provider) swaps the child list
// out from under the counter; advancing the static counter in that
// state would rotate through a child set the operator never configured.
// BAML upstream rebuilds a fresh Arc<LLMProvider> per request-scoped
// context whenever the registry touches a client, so fresh-random
// selection is the behaviour to mirror.
func isDynamicRRClient(reg *bamlutils.ClientRegistry, clientName string) bool {
	return findRuntimeClient(reg, clientName) != nil
}

// resolveClientProvider returns the provider for a named client, checking
// runtime client_registry overrides before the introspected defaults. This
// mirrors buildrequest.resolveChildProvider but lives here to keep the
// roundrobin package a leaf (no dependency on buildrequest).
func resolveClientProvider(reg *bamlutils.ClientRegistry, clientName string, introspected map[string]string) string {
	if reg != nil {
		for _, c := range reg.Clients {
			if c != nil && c.Name == clientName && c.Provider != "" {
				return c.Provider
			}
		}
	}
	return introspected[clientName]
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
//     non-string elements, etc.). Resolve returns ErrInvalidStrategyOverride
//     for this case so ResolveEffectiveClient routes the request to
//     legacy and BAML emits the canonical ensure_strategy error.
//
// Mirrors bamlutils/buildrequest.inspectStrategyOverride — duplicated to
// avoid pulling buildrequest into the roundrobin package's import graph.
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

// resolveStrategyChain returns the ordered child list for a strategy
// client, honouring any runtime client_registry override of the strategy
// option before falling back to the introspected chain. Mirrors
// buildrequest.resolveFallbackStrategyChain — duplicated to avoid pulling
// buildrequest into the roundrobin package's import graph.
//
// Callers must invoke inspectStrategyOverride first when they need to
// distinguish "absent override" from "present-but-invalid override";
// this helper collapses both into "use introspected chain", matching
// the existing pre-PR-192-cold-review-2 behaviour for callers that
// don't want the strict validation.
func resolveStrategyChain(reg *bamlutils.ClientRegistry, clientName string, introspectedChains map[string][]string) []string {
	chain, present, valid := inspectStrategyOverride(reg, clientName)
	if present && valid {
		return chain
	}
	return introspectedChains[clientName]
}

func findRuntimeClient(reg *bamlutils.ClientRegistry, clientName string) *bamlutils.ClientProperty {
	if reg == nil {
		return nil
	}
	for _, c := range reg.Clients {
		if c != nil && c.Name == clientName {
			return c
		}
	}
	return nil
}

// parseRuntimeStrategyOption delegates to the shared parser at
// bamlutils/strategyparse. The fallback and round-robin resolvers used
// to keep private copies of this logic that diverged on quote handling;
// sharing the helper keeps both strategies consistent. Quoted tokens
// (`"ClientA"`) are stripped to bare names so runtime client_registry
// overrides with a bracketed-string shape match the introspected client
// names.
func parseRuntimeStrategyOption(v any) []string {
	return strategyparse.ParseStrategyOption(v)
}
