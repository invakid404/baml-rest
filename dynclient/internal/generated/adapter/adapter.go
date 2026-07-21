package adapter

import (
	"context"
	"fmt"
	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	baml "github.com/invakid404/baml-rest/dynclient/baml-patched/engine/language_client_go/pkg"
	"github.com/invakid404/baml-rest/dynclient/internal/generated/introspected"
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
	// drops only inert presence-only static parents).
	ClientRegistry       *baml.ClientRegistry
	LegacyClientRegistry *baml.ClientRegistry
	TypeBuilder          *introspected.TypeBuilder

	// streamMode controls how streaming results are processed.
	streamMode bamlutils.StreamMode

	// logger is used for debug output during dynamic type processing.
	logger bamlutils.Logger

	// retryConfig holds per-request retry overrides from __baml_options__.retry.
	retryConfig *bamlutils.RetryConfig

	// includeReasoning is the per-request opt-in for surfacing
	// provider-specific reasoning/thinking text on /with-raw's
	// reasoning field, distinct from raw (which stays text-only).
	// Mirrors __baml_options__.include_reasoning and is honored by
	// the SSE/non-streaming extractors. Never affects the parseable
	// text passed to Parse/ParseStream.
	includeReasoning bool

	// clientRegistryProvider is the provider of the primary client from
	// the runtime ClientRegistry override. Empty if no override.
	clientRegistryProvider string

	// originalClientRegistry stores the original request ClientRegistry.
	originalClientRegistry *bamlutils.ClientRegistry

	// httpClient is an optional custom HTTP client for the BuildRequest path,
	// used by both streaming and non-streaming /call orchestration.
	// When nil, llmhttp.DefaultClient is used.
	httpClient *llmhttp.Client

	// deBAMLConfig carries the per-handler BAML_REST_USE_DEBAML
	// umbrella switch. The generated dynamic BuildRequest seam reads
	// it via DeBAMLConfig() to decide whether to render
	// ctx.output_format natively instead of letting BAML render it.
	deBAMLConfig bamlutils.DeBAMLConfig

	// deBAMLOutputSchema carries the original dynamic output schema
	// from __baml_options__ so the native renderer can lower and
	// render it at the BuildRequest seam. nil when none installed.
	deBAMLOutputSchema *bamlutils.DynamicOutputSchema

	// deBAMLRenderer is the native ctx.output_format render callback,
	// injected by the root module (the dynclient module cannot import
	// baml-rest's internal/schema + outputformat). nil means the
	// BuildRequest seam falls back to BAML-as-today.
	deBAMLRenderer bamlutils.DeBAMLRenderFunc

	// deBAMLParser is the native response-parser callback, injected by
	// the root module for the same module-boundary reason as
	// deBAMLRenderer. nil means the final-parse seam falls back to
	// BAML-as-today.
	deBAMLParser bamlutils.DeBAMLParseFunc

	// nativeShadow is the native one-send SHADOW comparator (de-BAML
	// cutover Slice 4), injected by a shadow deploy profile's worker for
	// the same module-boundary reason as deBAMLRenderer/deBAMLParser.
	// Non-nil ONLY in the shadow build; nil in every default production
	// build, leaving the orchestrator's native child-attempt callback
	// nil/hard-off. The generated dynamic call seam installs it via the
	// Slice-1 CallConfig.NativeAttempt callback only when it is non-nil
	// and DeBAMLConfig().Enabled.
	nativeShadow bamlutils.NativeShadowFunc

	// nativeServe is the native SERVE implementation (de-BAML cutover
	// Slice 6), injected by a serve deploy profile's worker for the same
	// module-boundary reason as nativeShadow. Non-nil ONLY in the serve
	// build with the flag on; nil in every default/shadow/flag-off build,
	// leaving the orchestrator's native child-attempt callback nil/hard-off.
	// The generated dynamic call seam installs it (taking precedence over
	// nativeShadow) via the Slice-1 CallConfig.NativeAttempt callback only
	// when it is non-nil and DeBAMLConfig().Enabled.
	nativeServe bamlutils.NativeServeFunc

	// nativeStreamServe is the native STREAM SERVE implementation (de-BAML
	// Phase 7D), injected by a serve deploy profile's worker for the same
	// module-boundary reason as nativeServe. Non-nil ONLY in the serve build
	// with the flag on; nil in every default/shadow/flag-off build, leaving the
	// orchestrator's native STREAM child-attempt callback nil/hard-off. The
	// generated dynamic StreamRequest seam installs it via
	// StreamConfig.NativeAttempt only when it is non-nil and DeBAMLConfig().Enabled.
	nativeStreamServe bamlutils.NativeStreamServeFunc

	// nativeStaticObserver is the native STATIC no-send admission OBSERVER
	// (de-BAML Slice 8B), injected by a native worker for the same
	// module-boundary reason as nativeServe. Non-nil ONLY in a native worker
	// with the flag on; nil in every default/flag-off build, leaving the
	// generated static observe seam nil/hard-off. It is OBSERVE-ONLY (always
	// declines to BAML), so it never changes what BAML serves.
	nativeStaticObserver bamlutils.NativeStaticObserveFunc

	// rrAdvancer is the per-request round-robin Advancer installed by the
	// worker; nil falls back to the introspected Coordinator.
	rrAdvancer bamlutils.RoundRobinAdvancer

	// IntrospectedClientProvider is the build-time map of static
	// .baml client name -> provider string. Set by the generated
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
func (b *BamlAdapter) SetIncludeReasoning(includeReasoning bool) {
	b.includeReasoning = includeReasoning
}
func (b *BamlAdapter) IncludeReasoning() bool {
	return b.includeReasoning
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
	if err := clientRegistry.Validate(); err != nil {
		return err
	}
	b.originalClientRegistry = clientRegistry
	b.clientRegistryProvider = ""
	b.ClientRegistry = baml.NewClientRegistry()
	b.LegacyClientRegistry = baml.NewClientRegistry()
	b.upstreamClientNames = b.upstreamClientNames[:0]
	b.legacyUpstreamClientNames = b.legacyUpstreamClientNames[:0]
	for _, client := range clientRegistry.Clients {
		if client == nil {
			continue
		}
		upstreamProvider := bamlutils.UpstreamClientRegistryProvider(client, b.IntrospectedClientProvider)
		if !bamlutils.IsResolvedStrategyParent(client, b.IntrospectedClientProvider) {
			b.ClientRegistry.AddLlmClient(client.Name, upstreamProvider, client.Options)
			b.upstreamClientNames = append(b.upstreamClientNames, client.Name)
		}
		if !bamlutils.ShouldDropStrategyParentForTopLevelLegacy(client, b.IntrospectedClientProvider) {
			b.LegacyClientRegistry.AddLlmClient(client.Name, upstreamProvider, client.Options)
			b.legacyUpstreamClientNames = append(b.legacyUpstreamClientNames, client.Name)
		}
	}
	if clientRegistry.Primary != nil && *clientRegistry.Primary != "" {
		b.ClientRegistry.SetPrimaryClient(*clientRegistry.Primary)
		b.LegacyClientRegistry.SetPrimaryClient(*clientRegistry.Primary)
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
		if !foundPrimary {
			b.clientRegistryProvider = bamlutils.UpstreamClientRegistryProvider(&bamlutils.ClientProperty{Name: *clientRegistry.Primary}, b.IntrospectedClientProvider)
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
func (b *BamlAdapter) SetDeBAMLConfig(cfg bamlutils.DeBAMLConfig) {
	b.deBAMLConfig = cfg
}
func (b *BamlAdapter) DeBAMLConfig() bamlutils.DeBAMLConfig {
	return b.deBAMLConfig
}
func (b *BamlAdapter) SetDeBAMLOutputSchema(schema *bamlutils.DynamicOutputSchema) {
	b.deBAMLOutputSchema = schema
}
func (b *BamlAdapter) DeBAMLOutputSchema() *bamlutils.DynamicOutputSchema {
	return b.deBAMLOutputSchema
}
func (b *BamlAdapter) SetDeBAMLRenderer(fn bamlutils.DeBAMLRenderFunc) {
	b.deBAMLRenderer = fn
}
func (b *BamlAdapter) DeBAMLRenderer() bamlutils.DeBAMLRenderFunc {
	return b.deBAMLRenderer
}
func (b *BamlAdapter) SetDeBAMLParser(fn bamlutils.DeBAMLParseFunc) {
	b.deBAMLParser = fn
}
func (b *BamlAdapter) DeBAMLParser() bamlutils.DeBAMLParseFunc {
	return b.deBAMLParser
}
func (b *BamlAdapter) SetNativeShadowComparator(fn bamlutils.NativeShadowFunc) {
	b.nativeShadow = fn
}
func (b *BamlAdapter) NativeShadowComparator() bamlutils.NativeShadowFunc {
	return b.nativeShadow
}
func (b *BamlAdapter) SetNativeServeComparator(fn bamlutils.NativeServeFunc) {
	b.nativeServe = fn
}
func (b *BamlAdapter) NativeServeComparator() bamlutils.NativeServeFunc {
	return b.nativeServe
}
func (b *BamlAdapter) SetNativeStreamServeComparator(fn bamlutils.NativeStreamServeFunc) {
	b.nativeStreamServe = fn
}
func (b *BamlAdapter) NativeStreamServeComparator() bamlutils.NativeStreamServeFunc {
	return b.nativeStreamServe
}
func (b *BamlAdapter) SetNativeStaticObserver(fn bamlutils.NativeStaticObserveFunc) {
	b.nativeStaticObserver = fn
}
func (b *BamlAdapter) NativeStaticObserver() bamlutils.NativeStaticObserveFunc {
	return b.nativeStaticObserver
}
func (b *BamlAdapter) SetRoundRobinAdvancer(advancer bamlutils.RoundRobinAdvancer) {
	b.rrAdvancer = advancer
}
func (b *BamlAdapter) RoundRobinAdvancer() bamlutils.RoundRobinAdvancer {
	return b.rrAdvancer
}

var _ bamlutils.Adapter = (*BamlAdapter)(nil)
