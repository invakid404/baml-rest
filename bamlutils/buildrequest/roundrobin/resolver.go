package roundrobin

import (
	"fmt"
	"strings"

	"github.com/invakid404/baml-rest/bamlutils"
)

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

		chain := resolveStrategyChain(in.Registry, current, in.FallbackChains)
		if len(chain) == 0 {
			return nil, fmt.Errorf("roundrobin: client %q has no children", current)
		}

		idx := selectIndex(in.Advancer, in.Registry, current, len(chain))
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
func selectIndex(a Advancer, reg *bamlutils.ClientRegistry, clientName string, childCount int) int {
	if isDynamicRRClient(reg, clientName) {
		return AdvanceDynamic(childCount)
	}
	if a == nil {
		return AdvanceDynamic(childCount)
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

// resolveStrategyChain returns the ordered child list for a strategy
// client, honouring any runtime client_registry override of the strategy
// option before falling back to the introspected chain. Mirrors
// buildrequest.resolveFallbackStrategyChain — duplicated to avoid pulling
// buildrequest into the roundrobin package's import graph.
func resolveStrategyChain(reg *bamlutils.ClientRegistry, clientName string, introspectedChains map[string][]string) []string {
	if rc := findRuntimeClient(reg, clientName); rc != nil && rc.Options != nil {
		if raw, ok := rc.Options["strategy"]; ok {
			if chain := parseRuntimeStrategyOption(raw); len(chain) > 0 {
				return chain
			}
		}
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

// parseRuntimeStrategyOption mirrors buildrequest.parseRuntimeStrategyOption.
// The shape accommodates three legal input forms: a bracket-delimited string
// ("strategy [A, B]"), a native string slice, and a heterogeneous []any
// (from JSON unmarshalling).
func parseRuntimeStrategyOption(v any) []string {
	normalizeToken := func(token string) string {
		token = strings.TrimSpace(token)
		if len(token) >= 2 {
			if (token[0] == '"' && token[len(token)-1] == '"') || (token[0] == '\'' && token[len(token)-1] == '\'') {
				token = token[1 : len(token)-1]
			}
		}
		return strings.TrimSpace(token)
	}
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
		out := make([]string, 0, len(parts))
		for _, part := range parts {
			part = normalizeToken(part)
			if part != "" {
				out = append(out, part)
			}
		}
		return out
	}
	switch vv := v.(type) {
	case string:
		return splitStrategy(vv)
	case []string:
		out := make([]string, 0, len(vv))
		for _, item := range vv {
			item = normalizeToken(item)
			if item != "" {
				out = append(out, item)
			}
		}
		return out
	case []any:
		out := make([]string, 0, len(vv))
		for _, item := range vv {
			str, ok := item.(string)
			if !ok {
				return nil
			}
			str = normalizeToken(str)
			if str != "" {
				out = append(out, str)
			}
		}
		return out
	default:
		return nil
	}
}
