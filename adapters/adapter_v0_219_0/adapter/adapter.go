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
		b.clientRegistryProvider = ""
		b.originalClientRegistry = nil
		return nil
	}

	b.originalClientRegistry = clientRegistry
	b.clientRegistryProvider = "" // Clear before scanning to avoid stale values
	b.ClientRegistry = baml.NewClientRegistry()

	for _, client := range clientRegistry.Clients {
		if client == nil {
			continue
		}
		// BAML 0.219.0+ properly handles nested maps, no WrapMapValues needed
		b.ClientRegistry.AddLlmClient(client.Name, client.Provider, client.Options)
	}

	if clientRegistry.Primary != nil {
		b.ClientRegistry.SetPrimaryClient(*clientRegistry.Primary)
		for _, client := range clientRegistry.Clients {
			if client == nil {
				continue
			}
			if client.Name == *clientRegistry.Primary {
				b.clientRegistryProvider = client.Provider
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
