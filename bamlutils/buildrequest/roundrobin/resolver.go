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
// silently using the introspected chain.
var ErrInvalidStrategyOverride = errors.New("roundrobin: runtime strategy override is invalid or empty")

// ErrInvalidStartOverride signals that the runtime client_registry
// supplied an `options.start` value outside the i32 contract that
// BAML's upstream ensure_int enforces. The contract:
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
// rather than us silently randomising.
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
// Sentinel-class errors (use errors.Is). These signal that the
// runtime override is structurally invalid; ResolveEffectiveClient
// converts both into a no-op so codegen routes the request to
// legacy and BAML emits its canonical validation error.
//
//   - ErrInvalidStrategyOverride — present-but-malformed runtime
//     `options.strategy` (bare token, non-list, empty list,
//     heterogeneous list with non-string elements), OR an explicit
//     runtime RR provider override (`round-robin` /
//     `baml-roundrobin` / etc.) with no `options.strategy` even when
//     the introspected map has a static chain for the same name.
//     BAML's eager parse rejects the missing-strategy shape; falling
//     back to introspected would silently dispatch a static child
//     and suppress the validation.
//   - ErrInvalidStartOverride — present-but-invalid runtime
//     `options.start` (string, fractional float, NaN/Inf, unsigned
//     types, json.Number, out-of-i32-range). Mirrors BAML's
//     ensure_int contract.
//
// Hard failures (request-fatal; not legacy fallthrough). These
// propagate to the caller as request errors:
//
//   - cycle detected (A points at B, B points at A via RR).
//   - static RR client with an empty child list (no runtime override
//     and no introspected chain).
//   - Advancer.Advance failures, including remote SharedState /
//     transport failures, wrapped by `roundrobin: select child for
//     %q: %w`.
//   - out-of-range child index from a buggy / future Advancer
//     (defensive interface-edge guard; index falls outside
//     [0, childCount)).
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
		strategyOverride, strategyPresent, strategyValid := InspectStrategyOverride(in.Registry, current)
		if strategyPresent && !strategyValid {
			return nil, ErrInvalidStrategyOverride
		}

		// An explicit runtime RR provider override with no
		// `options.strategy` is invalid regardless of whether the
		// introspected map has a static chain for `current`. BAML
		// upstream rejects the shape with its canonical ensure_strategy
		// error; falling back to the introspected chain would silently
		// dispatch a static child and suppress the validation. The
		// runtime override clearly signals operator intent to redefine
		// the RR shape, so any missing piece is the operator's bug —
		// route to legacy and let BAML produce the proper error.
		//
		// Detection: runtime entry exists for `current`, IsProviderPresent
		// (operator typed `provider:"…"`), and the provider is one of the
		// RR spellings. The `len(chain) == 0` branch below still handles
		// the omitted-provider absence case (no static chain either) with
		// its own error class.
		if rc := findRuntimeClient(in.Registry, current); rc != nil &&
			rc.IsProviderPresent() && IsRoundRobinProvider(rc.Provider) &&
			!strategyPresent {
			return nil, ErrInvalidStrategyOverride
		}

		chain := strategyOverride
		if !(strategyPresent && strategyValid) {
			chain = in.FallbackChains[current]
		}
		if len(chain) == 0 {
			return nil, fmt.Errorf("roundrobin: client %q has no children", current)
		}

		idx, err := selectIndex(in.Advancer, in.Registry, current, len(chain))
		if err != nil {
			return nil, fmt.Errorf("roundrobin: select child for %q: %w", current, err)
		}
		// Defensive interface-edge guard: every shipped Advancer
		// (Coordinator, RemoteAdvancer, AdvanceDynamic,
		// normalizeStartIndex) is documented and verified to return
		// [0, childCount), but Advancer is an interface and a buggy
		// custom/future implementation could return -1 or len(chain).
		// Without this guard the next line's chain[idx] would panic;
		// returning an error instead lets the caller surface the
		// failure cleanly.
		//
		// Routing: only the two ErrInvalid* sentinels
		// (ErrInvalidStrategyOverride, ErrInvalidStartOverride) drive
		// the legacy fallthrough — ResolveEffectiveClient maps those
		// to a no-op return so the codegen-side support gate routes
		// the request to legacy. All OTHER non-sentinel resolver
		// errors, including this defensive out-of-range, propagate as
		// request errors via ResolveEffectiveClient's `return "", nil,
		// err`. That's the right outcome for a buggy Advancer: the
		// request fails fast rather than hiding a custom-implementation
		// regression behind silent legacy routing.
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

// selectIndex picks the child index for a RR resolution. The
// dynamic-vs-Coordinator switch is governed by
// isDynamicRRClient(reg, clientName), which treats *any* runtime
// client_registry entry for clientName as dynamic — strategy-only,
// presence-only (registry touch with no overrides), or full provider
// override all qualify. The rationale lives on isDynamicRRClient: BAML
// upstream rebuilds a fresh Arc<LLMProvider> per request-scoped context
// whenever the registry touches a client, so any registry presence
// must bypass the long-lived Coordinator counter and use fresh-per-
// context selection. Dynamic clients get:
// when `options.start` is supplied, the selection is deterministic at
// `int(uint64(start) % uint64(childCount))` (cast-then-modulo, matching
// upstream BAML's `(start as usize) % strategy.len()` and the
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
// the metadata it produces stay in sync.
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
// same shapes.
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
// The cast-then-modulo expression mirrors upstream BAML's runtime
// (engine/baml-runtime/src/internal/llm_client/strategy/roundrobin.rs:64-65):
// `(start as usize) % strategy.len()`. On 64-bit Go,
// `uint64(int(-1))` is 0xFFFFFFFFFFFFFFFF — the same bit pattern
// Rust's `(-1i32 as usize)` produces — so `% childCount` matches the
// per-worker runtime exactly. Result fits in int because childCount
// is bounded by the chain length, and the modulo is in
// [0, childCount).
//
// Aligning with BAML's cast keeps centralised host rotation
// consistent with BAML's legacy per-worker rotation: a request with
// start=-1 against a 2-child chain dispatches index 1 on both
// paths, against a 3-child chain dispatches index 0 on both, and
// so on.
//
// childCount must be > 0; selectIndex guarantees this because Resolve
// rejects empty chains before invoking selectIndex.
func normalizeStartIndex(start, childCount int) int {
	return int(uint64(start) % uint64(childCount))
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
// provider.
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
// string parsing.
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
