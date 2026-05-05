package adapter

import (
	"context"
	"fmt"

	baml "github.com/boundaryml/baml/engine/language_client_go/pkg"
	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/introspected"
)

// TypeBuilderFactory creates a new TypeBuilder and applies the TypeBuilder config.
// The generated code implements this to have direct access to the generated
// TypeBuilder methods for processing DynamicTypes and BamlSnippets.
type TypeBuilderFactory func(tb *bamlutils.TypeBuilder) (*introspected.TypeBuilder, error)

// MediaFactory creates BAML media objects (image, audio, pdf, video) from URL or base64 data.
// Populated by the generated code which has access to the baml_client package-level constructors.
type MediaFactory func(kind bamlutils.MediaKind, url *string, base64 *string, mimeType *string) (any, error)

type BamlAdapter struct {
	context.Context

	TypeBuilderFactory TypeBuilderFactory
	MediaFactory       MediaFactory

	// ClientRegistry is the BuildRequest-safe view: every baml-rest-
	// resolved strategy parent (RR + fallback) is dropped. Used by the
	// generated BuildRequest dispatch sites (which dispatch via
	// WithClient(leaf) and never need the parent), and by the mixed-
	// mode bridge legacy-child callbacks (whose target is also a leaf,
	// not a parent). Shielding this view from operator parent shapes
	// keeps BuildRequest leaf calls insulated from BAML's eager
	// to_clients (client_registry/mod.rs:109) per-request parse, which
	// fails the *whole* request on any rejected entry.
	ClientRegistry *baml.ClientRegistry
	// LegacyClientRegistry is the top-level legacy view: keeps any
	// strategy parent the operator supplied an explicit Provider /
	// `options.strategy` / `options.start` for, dropping only inert
	// presence-only static parents. The generated final-legacy
	// fallthrough (the runNoRaw/runFullOrchestration call sites) uses
	// this view so BAML can either honour a runtime override or emit
	// its canonical ensure_strategy / ensure_int / unsupported-property
	// error.
	LegacyClientRegistry *baml.ClientRegistry
	TypeBuilder          *introspected.TypeBuilder

	// streamMode controls how streaming results are processed.
	streamMode bamlutils.StreamMode

	// logger is used for debug output during dynamic type processing.
	logger bamlutils.Logger

	// retryConfig holds per-request retry overrides from __baml_options__.retry.
	retryConfig *bamlutils.RetryConfig

	// includeThinkingInRaw is the per-request opt-in for surfacing
	// provider-specific reasoning/thinking content in /with-raw's raw
	// field. Mirrors __baml_options__.include_thinking_in_raw and is
	// honored by the SSE/non-streaming extractors. Never affects the
	// parseable text passed to Parse/ParseStream.
	includeThinkingInRaw bool

	// clientRegistryProvider is the provider of the primary client from
	// the runtime ClientRegistry override. Empty if no override.
	clientRegistryProvider string

	// originalClientRegistry stores the original request ClientRegistry
	// for named-client provider resolution in the BuildRequest router.
	originalClientRegistry *bamlutils.ClientRegistry

	// httpClient is an optional custom HTTP client for the BuildRequest path,
	// used by both streaming and non-streaming /call orchestration.
	// When nil, llmhttp.DefaultClient is used.
	httpClient *llmhttp.Client

	// rrAdvancer is the per-request round-robin Advancer installed by the
	// worker. Nil when the caller (standalone test, legacy harness)
	// doesn't plumb a shared-state client through — in that case
	// roundrobin.Resolve falls back to the introspected Coordinator.
	rrAdvancer bamlutils.RoundRobinAdvancer

	// IntrospectedClientProvider is the build-time map of static
	// .baml client name → provider string. The generated MakeAdapter
	// initialises this from introspected.ClientProvider so
	// SetClientRegistry can materialise providers for runtime
	// registry entries that omit the `provider` key (strategy-only /
	// presence-only RR overrides). Without this materialisation the
	// upstream baml.ClientRegistry forwards `provider: ""` into
	// CFFI, which rejects in ClientProvider::from_str
	// (clientspec.rs:119-144) and kills the request before BAML
	// even resolves WithClient(leaf).
	IntrospectedClientProvider map[string]string

	// upstreamClientNames records the order of names passed to
	// AddLlmClient on the BuildRequest-safe registry. Test-only
	// observability — used by SetClientRegistry tests to assert that
	// baml-rest-resolved strategy parent entries get dropped while
	// still being preserved in OriginalClientRegistry. Production
	// code must not depend on this slice; reach for the BAML-bound
	// registry directly.
	upstreamClientNames []string

	// legacyUpstreamClientNames is the same observability for the
	// legacy registry view. Keeps a separate slice so dual-view
	// tests can verify which entries reached each view.
	legacyUpstreamClientNames []string
}

func (b *BamlAdapter) SetRetryConfig(config *bamlutils.RetryConfig) {
	b.retryConfig = config
}

func (b *BamlAdapter) RetryConfig() *bamlutils.RetryConfig {
	return b.retryConfig
}

func (b *BamlAdapter) SetIncludeThinkingInRaw(includeThinking bool) {
	b.includeThinkingInRaw = includeThinking
}

func (b *BamlAdapter) IncludeThinkingInRaw() bool {
	return b.includeThinkingInRaw
}

func (b *BamlAdapter) SetClientRegistry(clientRegistry *bamlutils.ClientRegistry) error {
	if clientRegistry == nil {
		b.ClientRegistry = nil
		b.LegacyClientRegistry = nil
		b.clientRegistryProvider = ""
		b.originalClientRegistry = nil
		b.upstreamClientNames = b.upstreamClientNames[:0]
		b.legacyUpstreamClientNames = b.legacyUpstreamClientNames[:0]
		return nil
	}

	// Reject ambiguous input before any state mutation. BAML's
	// AddLlmClient is map-backed (last-wins on duplicate Names); every
	// baml-rest lookup is slice-forward (first-match). A registry with
	// two same-named entries lets the two halves classify against
	// different definitions, so route classification, support gating,
	// retry policy, RR inspection, and X-BAML-Path-Reason can drift
	// from what BAML actually executes. Validate() returns
	// ErrDuplicateClientName for that case; the early return leaves
	// originalClientRegistry / ClientRegistry / LegacyClientRegistry
	// at their previous values so a rejected call doesn't half-apply.
	if err := clientRegistry.Validate(); err != nil {
		return err
	}

	b.originalClientRegistry = clientRegistry
	b.clientRegistryProvider = "" // Clear before scanning to avoid stale values
	b.ClientRegistry = baml.NewClientRegistry()
	b.LegacyClientRegistry = baml.NewClientRegistry()
	b.upstreamClientNames = b.upstreamClientNames[:0]
	b.legacyUpstreamClientNames = b.legacyUpstreamClientNames[:0]

	// Materialise two BAML-bound registry views from the same input.
	//
	//   - ClientRegistry (BuildRequest-safe): drops every baml-rest-
	//     resolved strategy parent. baml-rest dispatches WithClient
	//     (leaf) so BAML never executes the parent; dropping shields
	//     BuildRequest leaf calls from any parent shape BAML would
	//     reject (BAML's to_clients eagerly parses every runtime
	//     client per request — client_registry/mod.rs:109 — and a
	//     single rejected entry fails the whole request).
	//   - LegacyClientRegistry (legacy-safe): keeps strategy parents
	//     the operator deliberately overrode (Provider supplied,
	//     options.strategy or options.start present), and drops only
	//     inert presence-only static parents that would re-trigger
	//     the original missing-strategy CFFI failure. The top-level
	//     legacy fallthrough call sites use this view so BAML can
	//     execute valid runtime overrides and emit canonical errors
	//     for invalid ones.
	//
	// Both views materialise the provider via UpstreamClientRegistry-
	// Provider: omitted-provider entries get their provider filled
	// from the introspected
	// map; canonical "baml-roundrobin" gets translated to the upstream-
	// accepted "baml-round-robin". The original ClientProperty stays
	// in OriginalClientRegistry untouched so baml-rest's resolver and
	// metadata classifier read operator input verbatim.
	for _, client := range clientRegistry.Clients {
		if client == nil {
			continue
		}
		upstreamProvider := bamlutils.UpstreamClientRegistryProvider(client, b.IntrospectedClientProvider)
		// BAML 0.219.0+ properly handles nested maps, no WrapMapValues needed
		if !bamlutils.IsResolvedStrategyParent(client, b.IntrospectedClientProvider) {
			b.ClientRegistry.AddLlmClient(client.Name, upstreamProvider, client.Options)
			b.upstreamClientNames = append(b.upstreamClientNames, client.Name)
		}
		if !bamlutils.ShouldDropStrategyParentForTopLevelLegacy(client, b.IntrospectedClientProvider) {
			b.LegacyClientRegistry.AddLlmClient(client.Name, upstreamProvider, client.Options)
			b.legacyUpstreamClientNames = append(b.legacyUpstreamClientNames, client.Name)
		}
	}

	// Empty-primary guard: treat `Primary != nil && *Primary == ""`
	// the same as `Primary == nil`.
	// BAML's Go ClientRegistry stores the empty string verbatim and
	// the Rust runtime then fails on PromptRenderer's lookup ("client
	// not found: '"). Most baml-rest helpers already treat empty
	// primary as no-op; the adapter was the outlier. Forwarding ""
	// to BAML would also break operator API contracts that allow
	// `"primary": ""` to mean "no override" — the field is a *string
	// and dynamic validation only checks pointer presence
	// (bamlutils/dynamic.go:332-334).
	//
	// Cache-clearing above (b.clientRegistryProvider = "") still runs
	// because a non-nil registry — empty primary or not — must reset
	// stale provider state from the previous call.
	if clientRegistry.Primary != nil && *clientRegistry.Primary != "" {
		// Set Primary on both views. The operator-supplied primary
		// stays meaningful in either path — BuildRequest reads through
		// adapter shortcuts; legacy passes the registry to BAML which
		// honours Primary on legacy fallthrough.
		b.ClientRegistry.SetPrimaryClient(*clientRegistry.Primary)
		b.LegacyClientRegistry.SetPrimaryClient(*clientRegistry.Primary)
		// Cache the materialised provider for the primary so
		// ResolveProvider's adapter shortcut returns the value BAML
		// actually sees. Plain client.Provider would skip
		// materialisation for omitted-provider primaries and hide the
		// introspected fallback from the shortcut.
		//
		// Primary cache: skip runtime entries that BOTH views drop.
		// Without applying the same drop predicate the cache scan
		// could observe a provider from an entry BAML's BuildRequest-
		// bound view doesn't have. With dual views the rule is: cache
		// from the first matching entry that survived in *at least
		// one* view; if both views dropped the entry (inert
		// presence-only static parent), keep scanning so a forwarded
		// sibling (or static synthesis) can supply the provider.
		foundPrimary := false
		for _, client := range clientRegistry.Clients {
			if client == nil {
				continue
			}
			if client.Name != *clientRegistry.Primary {
				continue
			}
			droppedFromBuildRequest := bamlutils.IsResolvedStrategyParent(client, b.IntrospectedClientProvider)
			droppedFromLegacy := bamlutils.ShouldDropStrategyParentForTopLevelLegacy(client, b.IntrospectedClientProvider)
			if droppedFromBuildRequest && droppedFromLegacy {
				// Inert presence-only static parent. Skip so the
				// !foundPrimary fallback can synthesize from the
				// introspected map.
				continue
			}
			b.clientRegistryProvider = bamlutils.UpstreamClientRegistryProvider(client, b.IntrospectedClientProvider)
			foundPrimary = true
			break
		}
		// Primary names a static client with no surviving runtime
		// entry — operator selected an existing introspected client by
		// name without redefining it in clients[], or the only matching
		// runtime entry was an inert presence-only parent both views
		// dropped. Synthesize an empty ClientProperty so
		// UpstreamClientRegistryProvider falls through to the
		// introspected map; without this the ResolveProvider adapter
		// shortcut returns "" and downstream helpers report the
		// function default instead of the primary's actual static
		// provider. The foundPrimary flag (rather than "cache is
		// empty") gates synthesis so a matching present-empty entry
		// that intentionally produced "" is not clobbered.
		if !foundPrimary {
			b.clientRegistryProvider = bamlutils.UpstreamClientRegistryProvider(
				&bamlutils.ClientProperty{Name: *clientRegistry.Primary},
				b.IntrospectedClientProvider,
			)
		}
	}

	return nil
}

func (b *BamlAdapter) ClientRegistryProvider() string {
	return b.clientRegistryProvider
}

func (b *BamlAdapter) OriginalClientRegistry() *bamlutils.ClientRegistry {
	return b.originalClientRegistry
}

func (b *BamlAdapter) SetTypeBuilder(tb *bamlutils.TypeBuilder) error {
	if b.TypeBuilderFactory == nil {
		return fmt.Errorf("adapter: TypeBuilderFactory not set")
	}

	typeBuilder, err := b.TypeBuilderFactory(tb)
	if err != nil {
		return err
	}
	b.TypeBuilder = typeBuilder
	return nil
}

func (b *BamlAdapter) SetStreamMode(mode bamlutils.StreamMode) {
	b.streamMode = mode
}

func (b *BamlAdapter) StreamMode() bamlutils.StreamMode {
	return b.streamMode
}

func (b *BamlAdapter) SetLogger(logger bamlutils.Logger) {
	b.logger = logger
}

func (b *BamlAdapter) Logger() bamlutils.Logger {
	return b.logger
}

func (b *BamlAdapter) NewMediaFromURL(kind bamlutils.MediaKind, url string, mimeType *string) (any, error) {
	if b.MediaFactory == nil {
		return nil, fmt.Errorf("adapter: MediaFactory not set")
	}

	return b.MediaFactory(kind, &url, nil, mimeType)
}

func (b *BamlAdapter) NewMediaFromBase64(kind bamlutils.MediaKind, base64 string, mimeType *string) (any, error) {
	if b.MediaFactory == nil {
		return nil, fmt.Errorf("adapter: MediaFactory not set")
	}

	return b.MediaFactory(kind, nil, &base64, mimeType)
}

// SetHTTPClient injects a custom HTTP client for BuildRequest request execution.
// When set, the generated router uses this client instead of llmhttp.DefaultClient
// for both streaming and non-streaming paths.
// Pass nil to revert to the default client.
func (b *BamlAdapter) SetHTTPClient(c *llmhttp.Client) {
	b.httpClient = c
}

func (b *BamlAdapter) HTTPClient() *llmhttp.Client {
	return b.httpClient
}

func (b *BamlAdapter) SetRoundRobinAdvancer(advancer bamlutils.RoundRobinAdvancer) {
	b.rrAdvancer = advancer
}

func (b *BamlAdapter) RoundRobinAdvancer() bamlutils.RoundRobinAdvancer {
	return b.rrAdvancer
}

var _ bamlutils.Adapter = (*BamlAdapter)(nil)
