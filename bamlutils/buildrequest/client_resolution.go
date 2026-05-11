package buildrequest

import (
	"errors"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/buildrequest/roundrobin"
)

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
// "provider":"".
func ResolveProvider(adapter bamlutils.Adapter, defaultClientName string, introspectedProvider string) string {
	// Every non-empty return path runs through normalizeStrategyProvider
	// so callers see one canonical spelling per strategy regardless of
	// which alias (round-robin / baml-round-robin / baml-roundrobin /
	// fallback / baml-fallback) the adapter cache, runtime registry,
	// or introspected map happens to carry. The sibling resolvers
	// ResolveProviderWithReason and ResolveClientProvider already
	// normalise; this one used to leak raw aliases on three of its
	// four return paths, which made downstream classification
	// inconsistent depending on which seam answered first.
	//
	// Primary's provider via the adapter abstraction. Non-empty
	// short-circuit retained so adapter implementations that pre-
	// resolve the primary string keep working. An empty return
	// falls through to direct registry inspection — the adapter cache
	// collapses absent/present-empty into "" and we need the
	// distinction.
	if p := adapter.ClientRegistryProvider(); p != "" {
		return normalizeStrategyProvider(p)
	}

	reg := adapter.OriginalClientRegistry()
	if reg != nil {
		// Primary client override (presence-aware): catches the
		// present-empty case that the adapter cache hides.
		if reg.Primary != nil && *reg.Primary != "" {
			for _, client := range reg.Clients {
				if client != nil && client.Name == *reg.Primary && client.IsProviderPresent() {
					return normalizeStrategyProvider(client.Provider)
				}
			}
		}

		// Named client override on the function's default client.
		if defaultClientName != "" {
			for _, client := range reg.Clients {
				if client != nil && client.Name == defaultClientName && client.IsProviderPresent() {
					return normalizeStrategyProvider(client.Provider)
				}
			}
		}
	}

	return normalizeStrategyProvider(introspectedProvider)
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

// PreferAdvancer returns the per-request RoundRobinAdvancer installed on
// the adapter when set, otherwise the supplied fallback. Mirrors the
// preference ResolveEffectiveClient applies for top-level RR so that
// nested fallback-RR resolution observes the same advancer — and the
// same SharedState idempotency key under pool retries — that top-level
// RR uses. Callers pass the package-level Coordinator as the fallback
// so standalone workers without a RemoteAdvancer still rotate
// deterministically against the introspected starts.
func PreferAdvancer(adapter bamlutils.Adapter, fallback roundrobin.Advancer) roundrobin.Advancer {
	if a := adapter.RoundRobinAdvancer(); a != nil {
		return a
	}
	return fallback
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
// handling, and RR-child plumbing.
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
// Presence semantics:
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

// hasInvalidProviderOverride reports whether the runtime registry has
// an entry for clientName whose `provider` key was supplied with the
// empty string. BAML upstream's ClientProvider::from_str rejects empty
// provider strings (clientspec.rs:119-144); ResolveProvider already
// surfaces the empty value verbatim (rather than falling through to
// the introspected provider) so the codegen gate routes the request
// to legacy. This helper lets the metadata classifier distinguish
// "operator sent provider:”" from "no provider configured anywhere"
// — emitting PathReasonInvalidProviderOverride versus
// PathReasonEmptyProvider respectively.
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
// generic invalid-strategy / RR-legacy reasons.
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

// isInvalidOverrideReason reports whether reason classifies a
// runtime-override invalidity (provider, strategy, or start). Used
// by metadata-only paths to skip an introspected-chain rebuild
// when the resolver has already decided the request is malformed
// — surfacing a Chain in the metadata for a rejected request would
// mislead operators into thinking the static chain ran.
func isInvalidOverrideReason(reason string) bool {
	switch reason {
	case PathReasonInvalidProviderOverride,
		PathReasonInvalidStrategyOverride,
		PathReasonInvalidRoundRobinStartOverride:
		return true
	}
	return false
}

// hasExplicitStrategyProviderWithoutStrategy reports whether the
// runtime registry has an entry for clientName whose `provider` is
// explicitly set to a strategy spelling (round-robin / fallback /
// aliases) AND whose `options.strategy` is absent. This is the
// "operator typed `provider:'round-robin'` but forgot the strategy
// list" shape — BAML's eager parse rejects it with its canonical
// ensure_strategy error, but baml-rest's older classifier silently
// fell through to either the introspected static chain or the
// generic roundrobin-legacy-only reason.
//
// The resolver-level contract (in roundrobin.Resolve and
// FindInvalidReachableStrategyOverride) returns
// ErrInvalidStrategyOverride / PathReasonInvalidStrategyOverride
// for this shape. The metadata classifier in
// ResolveProviderWithReason needs the same predicate so
// X-BAML-Path-Reason matches the resolver's routing decision when
// the request reaches the top-level legacy fallthrough.
func hasExplicitStrategyProviderWithoutStrategy(reg *bamlutils.ClientRegistry, clientName string) bool {
	rc := findRuntimeClient(reg, clientName)
	if rc == nil || !rc.IsProviderPresent() {
		return false
	}
	if !isStrategyProvider(rc.Provider) {
		return false
	}
	_, present, _ := roundrobin.InspectStrategyOverride(reg, clientName)
	return !present
}

// isStrategyProvider reports whether p is one of the BAML strategy
// provider spellings (round-robin or fallback). Used by
// ResolveFallbackChainForClientWithReason to identify chain children
// that need the invalid-nested preflight, since nested strategy
// parents are the ones whose runtime overrides are dropped from the
// BuildRequest-safe view and silently masked when invalid.
func isStrategyProvider(p string) bool {
	return roundrobin.IsRoundRobinProvider(p) || normalizeStrategyProvider(p) == "baml-fallback"
}

func resolveFallbackStrategyChain(reg *bamlutils.ClientRegistry, clientName string, introspectedChains map[string][]string) []string {
	chain, present, valid := roundrobin.InspectStrategyOverride(reg, clientName)
	if present && !valid {
		return nil
	}
	if chain != nil {
		return chain
	}
	return introspectedChains[clientName]
}
