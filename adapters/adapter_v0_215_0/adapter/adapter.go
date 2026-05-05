package adapter

import (
	"context"

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

	// ClientRegistry is the BuildRequest-safe view (drops every
	// baml-rest-resolved strategy parent). LegacyClientRegistry is
	// the top-level legacy view (keeps explicit parent overrides,
	// drops only inert presence-only static parents). See the v0.219
	// adapter doc for the dual-view rationale.
	ClientRegistry       *baml.ClientRegistry
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

	// originalClientRegistry stores the original request ClientRegistry.
	originalClientRegistry *bamlutils.ClientRegistry

	// rrAdvancer is the per-request round-robin Advancer installed by the
	// worker; nil falls back to the introspected Coordinator.
	rrAdvancer bamlutils.RoundRobinAdvancer

	// IntrospectedClientProvider is the build-time map of static
	// .baml client name → provider string. Set by the generated
	// MakeAdapter so SetClientRegistry can materialise providers
	// for runtime registry entries that omit the `provider` key
	// (strategy-only / presence-only overrides).
	IntrospectedClientProvider map[string]string

	// upstreamClientNames / legacyUpstreamClientNames record the order
	// of names passed to AddLlmClient on the BuildRequest-safe and
	// legacy registry views respectively. Test-only observability for
	// the dual-view forwarding rules and the strategy-parent drop.
	upstreamClientNames       []string
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
	// Nil-registry / nil-client / stale-cache guards mirroring the
	// v0.219 adapter pattern.
	if clientRegistry == nil {
		b.ClientRegistry = nil
		b.LegacyClientRegistry = nil
		b.clientRegistryProvider = ""
		b.originalClientRegistry = nil
		b.upstreamClientNames = b.upstreamClientNames[:0]
		b.legacyUpstreamClientNames = b.legacyUpstreamClientNames[:0]
		return nil
	}

	// Reject duplicate runtime client names before mutating adapter
	// state — see v0.219 adapter for the BAML-last-wins / baml-rest-
	// first-match divergence rationale.
	if err := clientRegistry.Validate(); err != nil {
		return err
	}

	b.originalClientRegistry = clientRegistry
	b.clientRegistryProvider = "" // Clear before scanning to avoid stale values
	b.ClientRegistry = baml.NewClientRegistry()
	b.LegacyClientRegistry = baml.NewClientRegistry()
	b.upstreamClientNames = b.upstreamClientNames[:0]
	b.legacyUpstreamClientNames = b.legacyUpstreamClientNames[:0]

	// Materialise two BAML-bound registry views — see v0.219 adapter
	// for the dual-view rationale.
	for _, client := range clientRegistry.Clients {
		if client == nil {
			continue
		}
		upstreamProvider := bamlutils.UpstreamClientRegistryProvider(client, b.IntrospectedClientProvider)
		// BAML 0.215.0+ properly handles nested maps, no WrapMapValues needed
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
	// See v0.219 adapter for the full rationale — BAML's runtime
	// stores "" verbatim and PromptRenderer fails on the empty-name
	// lookup. Cache-clearing earlier in SetClientRegistry still runs
	// because a non-nil registry must reset stale provider state.
	if clientRegistry.Primary != nil && *clientRegistry.Primary != "" {
		b.ClientRegistry.SetPrimaryClient(*clientRegistry.Primary)
		b.LegacyClientRegistry.SetPrimaryClient(*clientRegistry.Primary)
		// Primary cache: skip runtime entries that BOTH views drop
		// (predicate `droppedFromBuildRequest && droppedFromLegacy`).
		// Cache from the first matching entry that survived in at
		// least ONE view — a runtime entry kept by the legacy view
		// but dropped from the BuildRequest-safe view is still a
		// valid provider source for the cache. If both views dropped
		// the entry (inert presence-only static parent), keep
		// scanning so the !foundPrimary fallback can synthesize from
		// the introspected map. Mirrors v0.219 adapter:201-210.
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
				continue
			}
			b.clientRegistryProvider = bamlutils.UpstreamClientRegistryProvider(client, b.IntrospectedClientProvider)
			foundPrimary = true
			break
		}
		// Synthesize from the introspected map when primary names a
		// static client without a surviving runtime entry: the adapter
		// materializes the provider from IntrospectedClientProvider so
		// ClientRegistryProvider() reflects the same provider BAML
		// would resolve for that named client.
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
	return b.MediaFactory(kind, &url, nil, mimeType)
}

func (b *BamlAdapter) NewMediaFromBase64(kind bamlutils.MediaKind, base64 string, mimeType *string) (any, error) {
	return b.MediaFactory(kind, nil, &base64, mimeType)
}

func (b *BamlAdapter) HTTPClient() *llmhttp.Client {
	return nil
}

func (b *BamlAdapter) SetRoundRobinAdvancer(advancer bamlutils.RoundRobinAdvancer) {
	b.rrAdvancer = advancer
}

func (b *BamlAdapter) RoundRobinAdvancer() bamlutils.RoundRobinAdvancer {
	return b.rrAdvancer
}

var _ bamlutils.Adapter = (*BamlAdapter)(nil)
