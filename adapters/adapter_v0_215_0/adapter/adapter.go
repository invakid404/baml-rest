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

	ClientRegistry *baml.ClientRegistry
	TypeBuilder    *introspected.TypeBuilder

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
	// (strategy-only / presence-only overrides). See PR #192
	// cold-review-3 finding 1.
	IntrospectedClientProvider map[string]string
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
	b.originalClientRegistry = clientRegistry
	b.ClientRegistry = baml.NewClientRegistry()

	for _, client := range clientRegistry.Clients {
		// Materialise provider for the BAML-bound copy; the original
		// ClientProperty stays untouched so the resolver and metadata
		// classifier still see operator input verbatim. See PR #192
		// cold-review-3 findings 1 and 2.
		upstreamProvider := bamlutils.UpstreamClientRegistryProvider(client, b.IntrospectedClientProvider)
		// BAML 0.215.0+ properly handles nested maps, no WrapMapValues needed
		b.ClientRegistry.AddLlmClient(client.Name, upstreamProvider, client.Options)
	}

	if clientRegistry.Primary != nil {
		b.ClientRegistry.SetPrimaryClient(*clientRegistry.Primary)
		for _, client := range clientRegistry.Clients {
			if client.Name == *clientRegistry.Primary {
				b.clientRegistryProvider = bamlutils.UpstreamClientRegistryProvider(client, b.IntrospectedClientProvider)
				break
			}
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
