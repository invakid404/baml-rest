package worker

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
)

// fakeRuntime is an in-memory Runtime stand-in for handler tests. It
// records InitRuntime calls so tests can assert wiring without booting
// the real BAML shared library, and lets each test configure the
// Methods / ParseMethods / MakeAdapter return shape it cares about.
type fakeRuntime struct {
	initCalls      atomic.Int64
	methods        map[string]bamlutils.StreamingMethod
	parseMethods   map[string]bamlutils.ParseMethod
	makeAdapter    func(context.Context) bamlutils.Adapter
	lastAdapterCtx atomic.Value // context.Context
}

func (r *fakeRuntime) InitRuntime() { r.initCalls.Add(1) }

func (r *fakeRuntime) Method(name string) (bamlutils.StreamingMethod, bool) {
	m, ok := r.methods[name]
	return m, ok
}

func (r *fakeRuntime) ParseMethod(name string) (bamlutils.ParseMethod, bool) {
	m, ok := r.parseMethods[name]
	return m, ok
}

func (r *fakeRuntime) MakeAdapter(ctx context.Context) bamlutils.Adapter {
	r.lastAdapterCtx.Store(ctx)
	if r.makeAdapter != nil {
		return r.makeAdapter(ctx)
	}
	return newFakeAdapter(ctx)
}

// fakeAdapter satisfies bamlutils.Adapter with just enough state for
// the handler tests. SetClientRegistry preserves the inbound registry
// verbatim so assertions can observe URL rewrites applied before the
// adapter was handed the registry.
type fakeAdapter struct {
	context.Context
	streamMode             bamlutils.StreamMode
	logger                 bamlutils.Logger
	retryConfig            *bamlutils.RetryConfig
	includeReasoning       bool
	originalRegistry       *bamlutils.ClientRegistry
	clientRegistryProvider string
	roundRobinAdvancer     bamlutils.RoundRobinAdvancer
	httpClient             *llmhttp.Client
	buildRequestConfig     bamlutils.BuildRequestConfig
	deBAMLConfig           bamlutils.DeBAMLConfig
	deBAMLOutputSchema     *bamlutils.DynamicOutputSchema
	deBAMLRenderer         bamlutils.DeBAMLRenderFunc

	setBuildRequestConfigCalls int
	setHTTPClientCalls         int
	setDeBAMLConfigCalls       int
	setDeBAMLOutputSchemaCalls int
	setDeBAMLRendererCalls     int
}

func newFakeAdapter(ctx context.Context) *fakeAdapter {
	return &fakeAdapter{Context: ctx}
}

func (a *fakeAdapter) SetClientRegistry(r *bamlutils.ClientRegistry) error {
	a.originalRegistry = r
	return nil
}
func (a *fakeAdapter) SetTypeBuilder(_ *bamlutils.TypeBuilder) error { return nil }
func (a *fakeAdapter) SetStreamMode(mode bamlutils.StreamMode)       { a.streamMode = mode }
func (a *fakeAdapter) StreamMode() bamlutils.StreamMode              { return a.streamMode }
func (a *fakeAdapter) SetLogger(l bamlutils.Logger)                  { a.logger = l }
func (a *fakeAdapter) Logger() bamlutils.Logger                      { return a.logger }
func (a *fakeAdapter) NewMediaFromURL(_ bamlutils.MediaKind, _ string, _ *string) (any, error) {
	return nil, nil
}
func (a *fakeAdapter) NewMediaFromBase64(_ bamlutils.MediaKind, _ string, _ *string) (any, error) {
	return nil, nil
}
func (a *fakeAdapter) SetRetryConfig(c *bamlutils.RetryConfig) { a.retryConfig = c }
func (a *fakeAdapter) RetryConfig() *bamlutils.RetryConfig     { return a.retryConfig }
func (a *fakeAdapter) SetIncludeReasoning(v bool)              { a.includeReasoning = v }
func (a *fakeAdapter) IncludeReasoning() bool                  { return a.includeReasoning }
func (a *fakeAdapter) ClientRegistryProvider() string          { return a.clientRegistryProvider }
func (a *fakeAdapter) OriginalClientRegistry() *bamlutils.ClientRegistry {
	return a.originalRegistry
}
func (a *fakeAdapter) HTTPClient() *llmhttp.Client { return a.httpClient }
func (a *fakeAdapter) SetHTTPClient(c *llmhttp.Client) {
	a.setHTTPClientCalls++
	a.httpClient = c
}
func (a *fakeAdapter) SetBuildRequestConfig(c bamlutils.BuildRequestConfig) {
	a.setBuildRequestConfigCalls++
	a.buildRequestConfig = c
}
func (a *fakeAdapter) BuildRequestConfig() bamlutils.BuildRequestConfig { return a.buildRequestConfig }
func (a *fakeAdapter) SetRoundRobinAdvancer(adv bamlutils.RoundRobinAdvancer) {
	a.roundRobinAdvancer = adv
}
func (a *fakeAdapter) RoundRobinAdvancer() bamlutils.RoundRobinAdvancer { return a.roundRobinAdvancer }
func (a *fakeAdapter) SetDeBAMLConfig(c bamlutils.DeBAMLConfig) {
	a.setDeBAMLConfigCalls++
	a.deBAMLConfig = c
}
func (a *fakeAdapter) DeBAMLConfig() bamlutils.DeBAMLConfig { return a.deBAMLConfig }
func (a *fakeAdapter) SetDeBAMLOutputSchema(s *bamlutils.DynamicOutputSchema) {
	a.setDeBAMLOutputSchemaCalls++
	a.deBAMLOutputSchema = s
}
func (a *fakeAdapter) DeBAMLOutputSchema() *bamlutils.DynamicOutputSchema {
	return a.deBAMLOutputSchema
}

// SetDeBAMLRenderer satisfies the worker's narrow deBAMLRendererSetter
// optional interface so configureAdapter installs the callback here.
func (a *fakeAdapter) SetDeBAMLRenderer(fn bamlutils.DeBAMLRenderFunc) {
	a.setDeBAMLRendererCalls++
	a.deBAMLRenderer = fn
}

// newTestHandler constructs a Handler with a fake runtime as the
// default. Tests pass in any non-Runtime fields they care about; the
// fake runtime is substituted when cfg.Runtime is nil so existing
// tests that don't care about dispatch don't have to construct one.
func newTestHandler(t *testing.T, cfg Config) *Handler {
	t.Helper()
	if cfg.Runtime == nil {
		cfg.Runtime = &fakeRuntime{}
	}
	h, err := New(cfg)
	if err != nil {
		t.Fatalf("worker.New: %v", err)
	}
	return h
}
