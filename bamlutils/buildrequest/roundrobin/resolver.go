package roundrobin

import (
	"errors"
	"fmt"
	"math"

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

// ErrInvalidStartOverride signals that the runtime client_registry
// supplied an `options.start` value outside the i32 contract that
// BAML's upstream ensure_int enforces. The signoff-10 tightening
// pinned this contract to:
//
//   - Accepted: signed integer kinds (int, int8, int16, int32, int64)
//     within [math.MinInt32, math.MaxInt32]; finite whole float64
//     values within that same range.
//   - Rejected: all unsigned integer kinds (uint, uint8, uint16,
//     uint32, uint64 — BAML's Go encoder has no unsigned branch),
//     json.Number (encoded as string upstream, which the decoder
//     rejects for an integer option), strings (including numeric
//     strings like "1"), fractional or non-finite floats (NaN, ±Inf,
//     1.5, etc.), booleans, slices, maps, nil, and out-of-int32
//     signed/float values.
//
// BAML upstream's ensure_int parses via parse::<i32>()
// (baml-lib/llm-client/src/clients/helpers.rs:168-180, :917-930);
// round-robin invokes it via `properties.ensure_int("start", false)`
// in round_robin.rs:75. Resolve returns this sentinel so callers
// (ResolveEffectiveClient) can skip the RR unwrap and treat the
// request as having an invalid runtime `options.start`, falling
// through to legacy where BAML's runtime emits the canonical error
// rather than us silently randomising. See PR #192 cold-review-3
// finding 3 / signoff-10 tightening / verdict-13 finding 4.
var ErrInvalidStartOverride = errors.New("roundrobin: runtime start override is invalid")

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
		if _, present, valid := InspectStrategyOverride(in.Registry, current); present && !valid {
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
		// Defensive interface-edge guard (CodeRabbit verdict-33 finding
		// F3): every shipped Advancer (Coordinator, RemoteAdvancer,
		// AdvanceDynamic, normalizeStartIndex) is documented and
		// verified to return [0, childCount), but Advancer is an
		// interface and a buggy custom/future implementation could
		// return -1 or len(chain). Without this guard the next line's
		// chain[idx] would panic; returning a normal resolver error
		// instead keeps the caller's contract (RR routes to legacy on
		// any resolver error) consistent with every other failure
		// mode in this package.
		if idx < 0 || idx >= len(chain) {
			return nil, fmt.Errorf("roundrobin: selected child index %d out of range [0,%d) for %q", idx, len(chain), current)
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
// a fresh selection matching BAML's fresh-Arc-per-context semantics:
// when `options.start` is supplied, the selection is deterministic at
// `start % childCount` (with negatives clamped to zero, matching the
// static-side decision in NewCoordinatorWithStarts); otherwise it is
// uniformly random. Static clients delegate to the supplied Advancer —
// either an in-process Coordinator or a RemoteAdvancer talking to the
// host SharedState service.
//
// Returning ErrInvalidStartOverride is reserved for present-but-
// unparseable runtime `options.start` values (strings, fractional
// floats, etc.). Callers (ResolveEffectiveClient) translate the
// sentinel into a legacy fallthrough so BAML emits its canonical
// integer-options error.
//
// The Advancer error is surfaced verbatim. For the in-process path it
// never fires; for the remote path it's a transport failure that must
// fail the request, because silently falling back to AdvanceDynamic
// would defeat the whole point of centralised rotation.
func selectIndex(a Advancer, reg *bamlutils.ClientRegistry, clientName string, childCount int) (int, error) {
	if isDynamicRRClient(reg, clientName) {
		rc := findRuntimeClient(reg, clientName)
		start, present, valid := inspectStartOverride(rc.Options)
		if present && !valid {
			return 0, ErrInvalidStartOverride
		}
		if present {
			return normalizeStartIndex(start, childCount), nil
		}
		return AdvanceDynamic(childCount), nil
	}
	if a == nil {
		return AdvanceDynamic(childCount), nil
	}
	return a.Advance(clientName, childCount)
}

// InspectStartOverride is the exported variant of inspectStartOverride,
// shared with the orchestrator-level metadata classifier that needs the
// same three-state classification when emitting
// PathReasonInvalidRoundRobinStartOverride. Both call sites must agree
// on what counts as a valid integer shape so the routing decision and
// the metadata it produces stay in sync. See PR #192 cold-review-3
// finding 3.
func InspectStartOverride(opts map[string]any) (start int, present bool, valid bool) {
	return inspectStartOverride(opts)
}

// inspectStartOverride examines a runtime client_registry entry's
// options map for a `start` key. Three outcomes:
//
//   - present=false, valid=true: no `start` key (or no options at
//     all). Caller falls back to random / coordinator selection.
//   - present=true, valid=true: the override parsed to a representable
//     Go int. Caller uses normalizeStartIndex to project into
//     [0, childCount).
//   - present=true, valid=false: the override is present but
//     unparseable. Caller surfaces ErrInvalidStartOverride so
//     ResolveEffectiveClient routes the request to legacy.
//
// Accepted shapes match BAML upstream's ensure_int contract more
// tightly than the previous draft. Upstream returns an i32 and parses
// numeric values via parse::<i32>() (helpers.rs:168-180, :917-930), so
// values outside [math.MinInt32, math.MaxInt32] cannot survive
// upstream parsing even when they fit in a Go int.
//
// Accept signed integers (int, int8, int16, int32, int64) only when
// the value fits in int32 range. Accept finite whole float64 values
// in the same range.
//
// Reject all unsigned integer types: BAML's Go encoder
// (engine/language_client_go/baml_go/serde/encode.go:141-153) has no
// uint branch, so a uint* value would never make it through the CFFI
// path even if our parser accepted it. Reject json.Number for the
// same reason — encode.go:134-139 emits it as a string, which BAML's
// decoder rejects for an i32 option.
//
// Reject strings (including numeric strings like "1"), fractional
// floats, booleans, slices, and maps — BAML's decoder rejects the
// same shapes. See PR #192 cold-review-3 signoff-10 finding F3.
func inspectStartOverride(opts map[string]any) (start int, present bool, valid bool) {
	if opts == nil {
		return 0, false, true
	}
	raw, ok := opts["start"]
	if !ok {
		return 0, false, true
	}

	// Project signed-integer kinds onto int64, then clamp to int32 range.
	var asInt64 int64
	switch v := raw.(type) {
	case int:
		asInt64 = int64(v)
	case int8:
		asInt64 = int64(v)
	case int16:
		asInt64 = int64(v)
	case int32:
		asInt64 = int64(v)
	case int64:
		asInt64 = v
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return 0, true, false
		}
		if math.Trunc(v) != v {
			return 0, true, false
		}
		if v < float64(math.MinInt32) || v > float64(math.MaxInt32) {
			return 0, true, false
		}
		return int(int32(v)), true, true
	default:
		// uint*, json.Number, string, bool, slice, map, nil, etc.
		return 0, true, false
	}
	if asInt64 < math.MinInt32 || asInt64 > math.MaxInt32 {
		return 0, true, false
	}
	return int(asInt64), true, true
}

// normalizeStartIndex projects a parsed start value into [0, childCount).
// Negatives clamp to zero, matching NewCoordinatorWithStarts's existing
// static-side decision (coordinator.go:54-65). Non-negative values are
// taken modulo childCount so a start beyond the chain wraps cleanly.
//
// childCount must be > 0; selectIndex guarantees this because Resolve
// rejects empty chains before invoking selectIndex.
func normalizeStartIndex(start, childCount int) int {
	if start < 0 {
		return 0
	}
	return start % childCount
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
// mirrors buildrequest.ResolveClientProvider but lives here to keep the
// roundrobin package a leaf (no dependency on buildrequest).
//
// Presence semantics: an absent provider key falls through to the
// introspected provider (preserves strategy-only and presence-only
// overrides — see TestResolve_StrategyOnlyOverride_IsDynamic and
// TestResolve_RegistryPresenceWithoutOverride_IsDynamic). A
// present-empty provider key surfaces as "" so Resolve sees a non-RR
// classification, returns the un-unwrapped client as the leaf, and the
// dispatcher's BuildRequest gate fails — routing the request to legacy
// where BAML emits its native invalid-provider error rather than the
// resolver silently advancing the RR counter on the introspected
// provider. See PR #192 cold-review-2 verdict-8 follow-up.
func resolveClientProvider(reg *bamlutils.ClientRegistry, clientName string, introspected map[string]string) string {
	if reg != nil {
		for _, c := range reg.Clients {
			if c != nil && c.Name == clientName && c.IsProviderPresent() {
				return c.Provider
			}
		}
	}
	return introspected[clientName]
}

// InspectStrategyOverride examines the runtime client_registry entry
// for a strategy client. Three outcomes:
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
// Shared across the resolver (RR sentinel detection) and the
// orchestrator-level metadata classifier (fallback / RR path-reason
// emission). Previously each side kept a private copy that risked
// drifting on quote handling, empty-list semantics, or bracketed-
// string parsing — see PR #192 verdict-11 finding 1.
func InspectStrategyOverride(reg *bamlutils.ClientRegistry, clientName string) (chain []string, present bool, valid bool) {
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
// Callers must invoke InspectStrategyOverride first when they need to
// distinguish "absent override" from "present-but-invalid override";
// this helper collapses both into "use introspected chain", matching
// the existing pre-PR-192-cold-review-2 behaviour for callers that
// don't want the strict validation.
func resolveStrategyChain(reg *bamlutils.ClientRegistry, clientName string, introspectedChains map[string][]string) []string {
	chain, present, valid := InspectStrategyOverride(reg, clientName)
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
