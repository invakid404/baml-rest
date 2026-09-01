package spine

import (
	"context"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
)

// spineAdapter is the production pure-Go bamlutils.Adapter for the native-only
// worker runtime. It is the promotion of the adapter pattern proven in
// internal/nativespinejsonfixture (and internal/nativespinefixture) into
// nativeserve/spine ownership: it stores the request context, the stream mode
// (which the emitted closure reads to admit/decline a mode), the per-request
// HTTP client, and the request-scoped retry/registry/round-robin/dynamic-schema
// facts the executor's Call reads off it to decline pre-socket. Every other
// bamlutils.Adapter method is an inert no-op.
//
// It contains NO generated-BAML setters and NO CFFI object — it exists precisely
// so the native runtime can satisfy worker.Runtime.MakeAdapter without the
// CFFI-bound generated BamlAdapter. spine imports it here (in production, not
// only test code) because the native-only worker's MakeAdapter returns it.
type spineAdapter struct {
	context.Context

	streamMode         bamlutils.StreamMode
	logger             bamlutils.Logger
	retryConfig        *bamlutils.RetryConfig
	includeReasoning   bool
	originalRegistry   *bamlutils.ClientRegistry
	roundRobinAdvancer bamlutils.RoundRobinAdvancer
	httpClient         *llmhttp.Client
	deBAMLConfig       bamlutils.DeBAMLConfig
	deBAMLOutputSchema *bamlutils.DynamicOutputSchema
}

// newSpineAdapter returns a fresh adapter carrying the request context. It links
// no CFFI — that is the whole point of the native-only runtime.
func newSpineAdapter(ctx context.Context) *spineAdapter {
	return &spineAdapter{Context: ctx}
}

var _ bamlutils.Adapter = (*spineAdapter)(nil)

func (a *spineAdapter) SetClientRegistry(r *bamlutils.ClientRegistry) error {
	a.originalRegistry = r
	return nil
}
func (a *spineAdapter) SetTypeBuilder(_ *bamlutils.TypeBuilder) error { return nil }
func (a *spineAdapter) SetStreamMode(mode bamlutils.StreamMode)       { a.streamMode = mode }
func (a *spineAdapter) StreamMode() bamlutils.StreamMode              { return a.streamMode }
func (a *spineAdapter) SetLogger(l bamlutils.Logger)                  { a.logger = l }
func (a *spineAdapter) Logger() bamlutils.Logger                      { return a.logger }
func (a *spineAdapter) NewMediaFromURL(_ bamlutils.MediaKind, _ string, _ *string) (any, error) {
	return nil, nil
}
func (a *spineAdapter) NewMediaFromBase64(_ bamlutils.MediaKind, _ string, _ *string) (any, error) {
	return nil, nil
}
func (a *spineAdapter) SetRetryConfig(c *bamlutils.RetryConfig) { a.retryConfig = c }
func (a *spineAdapter) RetryConfig() *bamlutils.RetryConfig     { return a.retryConfig }
func (a *spineAdapter) SetIncludeReasoning(v bool)              { a.includeReasoning = v }
func (a *spineAdapter) IncludeReasoning() bool                  { return a.includeReasoning }
func (a *spineAdapter) SetSoftFinalParse(bool)                  {}
func (a *spineAdapter) SoftFinalParse() bool                    { return false }
func (a *spineAdapter) ClientRegistryProvider() string          { return "" }
func (a *spineAdapter) OriginalClientRegistry() *bamlutils.ClientRegistry {
	return a.originalRegistry
}
func (a *spineAdapter) HTTPClient() *llmhttp.Client     { return a.httpClient }
func (a *spineAdapter) SetHTTPClient(c *llmhttp.Client) { a.httpClient = c }
func (a *spineAdapter) SetRoundRobinAdvancer(adv bamlutils.RoundRobinAdvancer) {
	a.roundRobinAdvancer = adv
}
func (a *spineAdapter) RoundRobinAdvancer() bamlutils.RoundRobinAdvancer {
	return a.roundRobinAdvancer
}
func (a *spineAdapter) SetDeBAMLConfig(c bamlutils.DeBAMLConfig) { a.deBAMLConfig = c }
func (a *spineAdapter) DeBAMLConfig() bamlutils.DeBAMLConfig     { return a.deBAMLConfig }
func (a *spineAdapter) SetDeBAMLOutputSchema(s *bamlutils.DynamicOutputSchema) {
	a.deBAMLOutputSchema = s
}
func (a *spineAdapter) DeBAMLOutputSchema() *bamlutils.DynamicOutputSchema {
	return a.deBAMLOutputSchema
}

// SetDeBAMLRenderer satisfies the worker's optional deBAMLRendererSetter interface
// so configureAdapter installs its (nil, in the native-only worker) callback here
// harmlessly.
func (a *spineAdapter) SetDeBAMLRenderer(_ bamlutils.DeBAMLRenderFunc) {}
