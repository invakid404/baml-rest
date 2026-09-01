package nativespinejsonfixture

import (
	"context"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
)

// fixtureAdapter is a pure-Go bamlutils.Adapter for the ExecBridge-U1 JSON-alias
// native runtime. It stores only the stream mode (which the generated closure reads
// to admit/decline a mode) and the handful of fields worker.Handler.CallStream /
// Parse touch; every other method is an inert no-op. It links no BAML/CFFI — it
// exists precisely so the native runtime can satisfy worker.Runtime.MakeAdapter
// without the CFFI-bound generated BamlAdapter. Modeled on the worker package's own
// fakeAdapter (identical to internal/nativespinefixture's adapter).
type fixtureAdapter struct {
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

func newFixtureAdapter(ctx context.Context) *fixtureAdapter {
	return &fixtureAdapter{Context: ctx}
}

func (a *fixtureAdapter) SetClientRegistry(r *bamlutils.ClientRegistry) error {
	a.originalRegistry = r
	return nil
}
func (a *fixtureAdapter) SetTypeBuilder(_ *bamlutils.TypeBuilder) error { return nil }
func (a *fixtureAdapter) SetStreamMode(mode bamlutils.StreamMode)       { a.streamMode = mode }
func (a *fixtureAdapter) StreamMode() bamlutils.StreamMode              { return a.streamMode }
func (a *fixtureAdapter) SetLogger(l bamlutils.Logger)                  { a.logger = l }
func (a *fixtureAdapter) Logger() bamlutils.Logger                      { return a.logger }
func (a *fixtureAdapter) NewMediaFromURL(_ bamlutils.MediaKind, _ string, _ *string) (any, error) {
	return nil, nil
}
func (a *fixtureAdapter) NewMediaFromBase64(_ bamlutils.MediaKind, _ string, _ *string) (any, error) {
	return nil, nil
}
func (a *fixtureAdapter) SetRetryConfig(c *bamlutils.RetryConfig) { a.retryConfig = c }
func (a *fixtureAdapter) RetryConfig() *bamlutils.RetryConfig     { return a.retryConfig }
func (a *fixtureAdapter) SetIncludeReasoning(v bool)              { a.includeReasoning = v }
func (a *fixtureAdapter) IncludeReasoning() bool                  { return a.includeReasoning }
func (a *fixtureAdapter) SetSoftFinalParse(bool)                  {}
func (a *fixtureAdapter) SoftFinalParse() bool                    { return false }
func (a *fixtureAdapter) ClientRegistryProvider() string          { return "" }
func (a *fixtureAdapter) OriginalClientRegistry() *bamlutils.ClientRegistry {
	return a.originalRegistry
}
func (a *fixtureAdapter) HTTPClient() *llmhttp.Client     { return a.httpClient }
func (a *fixtureAdapter) SetHTTPClient(c *llmhttp.Client) { a.httpClient = c }
func (a *fixtureAdapter) SetRoundRobinAdvancer(adv bamlutils.RoundRobinAdvancer) {
	a.roundRobinAdvancer = adv
}
func (a *fixtureAdapter) RoundRobinAdvancer() bamlutils.RoundRobinAdvancer {
	return a.roundRobinAdvancer
}
func (a *fixtureAdapter) SetDeBAMLConfig(c bamlutils.DeBAMLConfig) { a.deBAMLConfig = c }
func (a *fixtureAdapter) DeBAMLConfig() bamlutils.DeBAMLConfig     { return a.deBAMLConfig }
func (a *fixtureAdapter) SetDeBAMLOutputSchema(s *bamlutils.DynamicOutputSchema) {
	a.deBAMLOutputSchema = s
}
func (a *fixtureAdapter) DeBAMLOutputSchema() *bamlutils.DynamicOutputSchema {
	return a.deBAMLOutputSchema
}

// SetDeBAMLRenderer satisfies the worker's optional deBAMLRendererSetter interface so
// configureAdapter installs its callback here harmlessly.
func (a *fixtureAdapter) SetDeBAMLRenderer(_ bamlutils.DeBAMLRenderFunc) {}
