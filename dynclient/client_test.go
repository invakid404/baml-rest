package dynclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bytedance/sonic"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/clientdefaults"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/workerplugin"
)

// -- Test plumbing --------------------------------------------------

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
	setLoggerCalls             int
	setClientRegistryCalls     int
}

func (a *fakeAdapter) SetClientRegistry(r *bamlutils.ClientRegistry) error {
	a.setClientRegistryCalls++
	a.originalRegistry = r
	return nil
}
func (a *fakeAdapter) SetTypeBuilder(_ *bamlutils.TypeBuilder) error { return nil }
func (a *fakeAdapter) SetStreamMode(mode bamlutils.StreamMode)       { a.streamMode = mode }
func (a *fakeAdapter) StreamMode() bamlutils.StreamMode              { return a.streamMode }
func (a *fakeAdapter) SetLogger(l bamlutils.Logger)                  { a.setLoggerCalls++; a.logger = l }
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
func (a *fakeAdapter) SetDeBAMLConfig(c bamlutils.DeBAMLConfig)         { a.deBAMLConfig = c }
func (a *fakeAdapter) DeBAMLConfig() bamlutils.DeBAMLConfig             { return a.deBAMLConfig }
func (a *fakeAdapter) SetDeBAMLOutputSchema(s *bamlutils.DynamicOutputSchema) {
	a.deBAMLOutputSchema = s
}
func (a *fakeAdapter) DeBAMLOutputSchema() *bamlutils.DynamicOutputSchema {
	return a.deBAMLOutputSchema
}
func (a *fakeAdapter) SetDeBAMLRenderer(fn bamlutils.DeBAMLRenderFunc) { a.deBAMLRenderer = fn }
func (a *fakeAdapter) DeBAMLRenderer() bamlutils.DeBAMLRenderFunc      { return a.deBAMLRenderer }

// fakeRuntime is the per-test stand-in for the dynamic BAML runtime.
// Tests populate streamingImpl / parseImpl to control the produced
// events; capturedAdapter records the per-request adapter so option
// wiring assertions can inspect what the handler installed.
type fakeRuntime struct {
	streamingImpl   func(adapter bamlutils.Adapter, input any) (<-chan bamlutils.StreamResult, error)
	parseImpl       func(adapter bamlutils.Adapter, raw string) (any, error)
	capturedAdapter atomic.Pointer[fakeAdapter]
	initCalls       atomic.Int64
}

func (r *fakeRuntime) InitRuntime() { r.initCalls.Add(1) }

func (r *fakeRuntime) Method(name string) (bamlutils.StreamingMethod, bool) {
	if name != bamlutils.DynamicMethodName {
		return bamlutils.StreamingMethod{}, false
	}
	return bamlutils.StreamingMethod{
		MakeInput: func() any { return &map[string]any{} },
		Impl: func(adapter bamlutils.Adapter, input any) (<-chan bamlutils.StreamResult, error) {
			if a, ok := adapter.(*fakeAdapter); ok {
				r.capturedAdapter.Store(a)
			}
			if r.streamingImpl != nil {
				return r.streamingImpl(adapter, input)
			}
			ch := make(chan bamlutils.StreamResult)
			close(ch)
			return ch, nil
		},
	}, true
}

func (r *fakeRuntime) ParseMethod(name string) (bamlutils.ParseMethod, bool) {
	if name != bamlutils.DynamicMethodName {
		return bamlutils.ParseMethod{}, false
	}
	return bamlutils.ParseMethod{
		MakeOutput: func() any { return &map[string]any{} },
		Impl: func(adapter bamlutils.Adapter, raw string) (any, error) {
			if a, ok := adapter.(*fakeAdapter); ok {
				r.capturedAdapter.Store(a)
			}
			if r.parseImpl != nil {
				return r.parseImpl(adapter, raw)
			}
			return map[string]any{}, nil
		},
	}, true
}

func (r *fakeRuntime) MakeAdapter(ctx context.Context) bamlutils.Adapter {
	return &fakeAdapter{Context: ctx}
}

// fakeStreamResult is the public StreamResult shape that BAML produces.
// Tests construct values directly rather than going through a real
// adapter so events can be ordered and shaped per case.
type fakeStreamResult struct {
	kind      bamlutils.StreamResultKind
	stream    any
	final     any
	err       error
	raw       string
	reasoning string
	reset     bool
	metadata  *bamlutils.Metadata
	released  *atomic.Int64
}

func (r *fakeStreamResult) Kind() bamlutils.StreamResultKind { return r.kind }
func (r *fakeStreamResult) Stream() any                      { return r.stream }
func (r *fakeStreamResult) Final() any                       { return r.final }
func (r *fakeStreamResult) Error() error                     { return r.err }
func (r *fakeStreamResult) Raw() string                      { return r.raw }
func (r *fakeStreamResult) Reasoning() string                { return r.reasoning }
func (r *fakeStreamResult) Reset() bool                      { return r.reset }
func (r *fakeStreamResult) Metadata() *bamlutils.Metadata    { return r.metadata }
func (r *fakeStreamResult) Release() {
	if r.released != nil {
		r.released.Add(1)
	}
}

func newClient(t *testing.T, rt *fakeRuntime, opts ...Option) *Client {
	t.Helper()
	c, err := newWithRuntime(rt, func() {}, opts...)
	if err != nil {
		t.Fatalf("newWithRuntime: %v", err)
	}
	return c
}

// validRequest is the smallest valid Request the dynamic endpoints
// accept: one user message, a primary-only ClientRegistry, and a single
// string property on the output schema.
func validRequest() Request {
	hello := "hello"
	primary := "TestClient"
	return Request{
		Messages: []Message{
			{Role: "user", TextContent: &hello},
		},
		ClientRegistry: &ClientRegistry{
			Primary: &primary,
			Clients: []*ClientProperty{
				{
					Name:     "TestClient",
					Provider: "openai",
					Options:  map[string]any{"base_url": "https://upstream.example/v1"},
				},
			},
		},
		OutputSchema: &OutputSchema{
			Properties: MustOrderedMap(OrderedKV("answer", &Property{Type: "string"})),
		},
	}
}

// -- Tests ----------------------------------------------------------

func TestNewAppliesOptions(t *testing.T) {
	rt := &fakeRuntime{}

	logger := &captureLogger{}
	metrics := prometheus.NewRegistry()
	rewrites := []BaseURLRewriteRule{{From: "https://upstream.example/", To: "http://proxy.local:8080/"}}
	httpClient := &http.Client{}
	defaults := &clientdefaults.Config{}
	store := workerplugin.NewSharedStateStore(func(string) uint64 { return 0 })
	t.Cleanup(store.Close)

	c := newClient(t, rt,
		WithUseBuildRequest(true),
		WithDisableCallBuildRequest(true),
		WithDeBAML(true),
		WithDeBAMLRenderer(func(*bamlutils.DynamicOutputSchema) (string, error) { return "block", nil }),
		WithLogger(logger),
		WithMetricsRegistry(metrics),
		WithBaseURLRewrites(rewrites),
		WithNetHTTPClient(httpClient),
		WithClientDefaults(defaults),
		WithSharedStateStore(store),
	)

	rt.streamingImpl = func(adapter bamlutils.Adapter, _ any) (<-chan bamlutils.StreamResult, error) {
		ch := make(chan bamlutils.StreamResult, 1)
		ch <- &fakeStreamResult{kind: bamlutils.StreamResultKindFinal, final: map[string]any{"DynamicProperties": map[string]any{"answer": "ok"}}}
		close(ch)
		return ch, nil
	}

	if _, err := c.DynamicCall(context.Background(), validRequest()); err != nil {
		t.Fatalf("DynamicCall: %v", err)
	}

	captured := rt.capturedAdapter.Load()
	if captured == nil {
		t.Fatal("expected adapter to be captured")
	}
	if got := captured.BuildRequestConfig(); got.UseBuildRequest != true || got.DisableCallBuildRequest != true {
		t.Errorf("BuildRequestConfig = %#v, want both flags true", got)
	}
	if !captured.DeBAMLConfig().Enabled {
		t.Errorf("DeBAMLConfig = %#v, want Enabled=true (WithDeBAML wiring)", captured.DeBAMLConfig())
	}
	if s := captured.DeBAMLOutputSchema(); s == nil || !s.Properties.Has("answer") {
		t.Errorf("carried output schema not installed on the adapter: %#v", s)
	}
	if captured.deBAMLRenderer == nil {
		t.Error("WithDeBAMLRenderer callback not installed on the adapter")
	}
	if captured.HTTPClient() == nil {
		t.Error("expected HTTPClient to be installed on the adapter")
	}
	if captured.logger == nil {
		t.Error("expected logger to be installed on the adapter")
	}
	if captured.StreamMode() != bamlutils.StreamModeCall {
		t.Errorf("StreamMode = %v, want StreamModeCall", captured.StreamMode())
	}
	if captured.setClientRegistryCalls != 1 {
		t.Errorf("SetClientRegistry calls = %d, want 1", captured.setClientRegistryCalls)
	}
	if captured.RoundRobinAdvancer() == nil {
		t.Error("expected round-robin advancer to be installed when SharedStateStore is configured")
	}

	reg := captured.OriginalClientRegistry()
	if reg == nil || len(reg.Clients) == 0 {
		t.Fatal("expected client registry to be captured")
	}
	gotURL, _ := reg.Clients[0].Options["base_url"].(string)
	if gotURL != "http://proxy.local:8080/v1" {
		t.Errorf("base_url after rewrite = %q, want http://proxy.local:8080/v1", gotURL)
	}
}

func TestNewDoesNotReadEnv(t *testing.T) {
	t.Setenv("BAML_REST_BASE_URL_REWRITES", "https://upstream.example/=http://env.local:9090/")
	t.Setenv("BAML_REST_CLIENT_DEFAULTS", `{"api_key":"env-key"}`)
	// dynclient defaults preserve_schema_order to true at New() and
	// must not consult the cmd/serve-side server-default env var.
	// TestNewDoesNotReadPreserveOrderEnv covers the preserve-order
	// resolution path explicitly; the env setting here ensures other
	// option wiring is also unaffected by the env presence.
	t.Setenv("BAML_REST_PRESERVE_SCHEMA_ORDER_DEFAULT", "false")

	rt := &fakeRuntime{}
	c := newClient(t, rt)

	rt.streamingImpl = func(adapter bamlutils.Adapter, _ any) (<-chan bamlutils.StreamResult, error) {
		ch := make(chan bamlutils.StreamResult, 1)
		ch <- &fakeStreamResult{kind: bamlutils.StreamResultKindFinal, final: map[string]any{"DynamicProperties": map[string]any{"answer": "ok"}}}
		close(ch)
		return ch, nil
	}

	if _, err := c.DynamicCall(context.Background(), validRequest()); err != nil {
		t.Fatalf("DynamicCall: %v", err)
	}

	captured := rt.capturedAdapter.Load()
	if captured == nil {
		t.Fatal("expected adapter to be captured")
	}
	reg := captured.OriginalClientRegistry()
	gotURL, _ := reg.Clients[0].Options["base_url"].(string)
	if gotURL != "https://upstream.example/v1" {
		t.Errorf("base_url = %q; env-driven rewrite leaked into New()", gotURL)
	}
	if _, ok := reg.Clients[0].Options["api_key"]; ok {
		t.Errorf("api_key = %v; env-driven client defaults leaked into New()", reg.Clients[0].Options["api_key"])
	}
}

func TestDynamicCallFlattensAndCopiesData(t *testing.T) {
	rt := &fakeRuntime{}
	c := newClient(t, rt)

	released := &atomic.Int64{}
	// The worker bridge marshals StreamResult.Final() into Data and then
	// calls Release on the source result. We track Release through the
	// fake to be sure pooled bytes really do go back.
	rt.streamingImpl = func(adapter bamlutils.Adapter, _ any) (<-chan bamlutils.StreamResult, error) {
		ch := make(chan bamlutils.StreamResult, 1)
		ch <- &fakeStreamResult{
			kind:     bamlutils.StreamResultKindFinal,
			final:    map[string]any{"DynamicProperties": map[string]any{"answer": "4"}},
			released: released,
		}
		close(ch)
		return ch, nil
	}

	result, err := c.DynamicCall(context.Background(), validRequest())
	if err != nil {
		t.Fatalf("DynamicCall: %v", err)
	}
	if released.Load() == 0 {
		t.Error("expected the source StreamResult to be released back to its pool")
	}

	var decoded map[string]any
	if err := sonic.Unmarshal(result.Data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := decoded["DynamicProperties"]; ok {
		t.Errorf("DynamicProperties wrapper leaked into public result: %s", result.Data)
	}
	if decoded["answer"] != "4" {
		t.Errorf("answer = %v, want %q", decoded["answer"], "4")
	}

	// Mutate the returned bytes; since CallResult.Data is a fresh copy,
	// the underlying buffer is no longer aliased to anything inside the
	// bridge or the worker pool.
	if len(result.Data) > 0 {
		result.Data[0] = 'X'
	}
}

func TestDynamicCallRawSeparatesRawAndReasoning(t *testing.T) {
	rt := &fakeRuntime{}
	c := newClient(t, rt)

	rt.streamingImpl = func(adapter bamlutils.Adapter, _ any) (<-chan bamlutils.StreamResult, error) {
		ch := make(chan bamlutils.StreamResult, 1)
		ch <- &fakeStreamResult{
			kind:      bamlutils.StreamResultKindFinal,
			final:     map[string]any{"DynamicProperties": map[string]any{"answer": "4"}},
			raw:       "raw text",
			reasoning: "because reasons",
		}
		close(ch)
		return ch, nil
	}

	result, err := c.DynamicCallRaw(context.Background(), validRequest())
	if err != nil {
		t.Fatalf("DynamicCallRaw: %v", err)
	}
	if result.Raw != "raw text" {
		t.Errorf("Raw = %q, want %q", result.Raw, "raw text")
	}
	if result.Reasoning != "because reasons" {
		t.Errorf("Reasoning = %q, want %q", result.Reasoning, "because reasons")
	}
	var decoded map[string]any
	if err := sonic.Unmarshal(result.Data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded["answer"] != "4" {
		t.Errorf("answer = %v, want %q", decoded["answer"], "4")
	}
}

func TestDynamicParseFlattensData(t *testing.T) {
	rt := &fakeRuntime{}
	c := newClient(t, rt)

	rt.parseImpl = func(adapter bamlutils.Adapter, raw string) (any, error) {
		return map[string]any{"DynamicProperties": map[string]any{"answer": raw}}, nil
	}

	parseReq := ParseRequest{
		Raw: "4",
		OutputSchema: &OutputSchema{
			Properties: MustOrderedMap(OrderedKV("answer", &Property{Type: "string"})),
		},
	}
	result, err := c.DynamicParse(context.Background(), parseReq)
	if err != nil {
		t.Fatalf("DynamicParse: %v", err)
	}
	var decoded map[string]any
	if err := sonic.Unmarshal(result.Data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := decoded["DynamicProperties"]; ok {
		t.Errorf("DynamicProperties wrapper leaked: %s", result.Data)
	}
	if decoded["answer"] != "4" {
		t.Errorf("answer = %v, want %q", decoded["answer"], "4")
	}
}

func TestStreamNextOrderingAndRawAccumulation(t *testing.T) {
	rt := &fakeRuntime{}
	c := newClient(t, rt)

	planned := bamlutils.MetadataPhasePlanned
	plannedMeta := &bamlutils.Metadata{Phase: planned, Path: "buildrequest"}

	rt.streamingImpl = func(adapter bamlutils.Adapter, _ any) (<-chan bamlutils.StreamResult, error) {
		ch := make(chan bamlutils.StreamResult, 6)
		// metadata, then reset bundled with a partial raw delta, then
		// another partial raw delta, then a final with the full raw.
		ch <- &fakeStreamResult{kind: bamlutils.StreamResultKindMetadata, metadata: plannedMeta}
		ch <- &fakeStreamResult{
			kind:   bamlutils.StreamResultKindStream,
			stream: map[string]any{"DynamicProperties": map[string]any{"answer": "h"}},
			raw:    "h",
			reset:  true,
		}
		ch <- &fakeStreamResult{
			kind:   bamlutils.StreamResultKindStream,
			stream: map[string]any{"DynamicProperties": map[string]any{"answer": "he"}},
			raw:    "e",
		}
		ch <- &fakeStreamResult{
			kind:  bamlutils.StreamResultKindFinal,
			final: map[string]any{"DynamicProperties": map[string]any{"answer": "hello"}},
			raw:   "hello",
		}
		close(ch)
		return ch, nil
	}

	stream, err := c.DynamicStreamRaw(context.Background(), validRequest())
	if err != nil {
		t.Fatalf("DynamicStreamRaw: %v", err)
	}
	t.Cleanup(func() { _ = stream.Close() })

	events := drainStreamFull(t, stream)
	if len(events) < 5 {
		t.Fatalf("expected at least 5 events, got %d: %+v", len(events), events)
	}

	if events[0].Kind != EventMetadata {
		t.Errorf("event 0 kind = %v, want EventMetadata", events[0].Kind)
	}
	if events[0].Metadata == nil || events[0].Metadata.Path != "buildrequest" {
		t.Errorf("event 0 metadata = %+v, want path=buildrequest", events[0].Metadata)
	}
	if events[1].Kind != EventReset {
		t.Errorf("event 1 kind = %v, want EventReset (reset must come before payload)", events[1].Kind)
	}
	if events[2].Kind != EventPartial {
		t.Errorf("event 2 kind = %v, want EventPartial", events[2].Kind)
	}
	if events[2].Raw != "h" {
		t.Errorf("event 2 raw = %q, want %q (first partial after reset)", events[2].Raw, "h")
	}
	if events[3].Kind != EventPartial {
		t.Errorf("event 3 kind = %v, want EventPartial", events[3].Kind)
	}
	if events[3].Raw != "he" {
		t.Errorf("event 3 raw = %q, want %q (accumulated partial)", events[3].Raw, "he")
	}
	if events[4].Kind != EventFinal {
		t.Errorf("event 4 kind = %v, want EventFinal", events[4].Kind)
	}
	if events[4].Raw != "hello" {
		t.Errorf("event 4 raw = %q, want %q (final uses full upstream raw, not accumulator)", events[4].Raw, "hello")
	}
}

func TestStreamCloseCleansUp(t *testing.T) {
	rt := &fakeRuntime{}
	store := workerplugin.NewSharedStateStore(func(string) uint64 { return 0 })
	t.Cleanup(store.Close)
	c := newClient(t, rt, WithSharedStateStore(store))

	released := &atomic.Int64{}
	producerDone := make(chan struct{})

	rt.streamingImpl = func(adapter bamlutils.Adapter, _ any) (<-chan bamlutils.StreamResult, error) {
		ch := make(chan bamlutils.StreamResult)
		go func() {
			defer close(ch)
			defer close(producerDone)
			for i := 0; i < 3; i++ {
				ev := &fakeStreamResult{
					kind:     bamlutils.StreamResultKindStream,
					stream:   map[string]any{"DynamicProperties": map[string]any{"answer": "x"}},
					released: released,
				}
				select {
				case ch <- ev:
				case <-adapter.Done():
					// Release the un-sent event we constructed so the count
					// reflects every produced result, not just dispatched ones.
					ev.Release()
					return
				}
			}
		}()
		return ch, nil
	}

	stream, err := c.DynamicStream(context.Background(), validRequest())
	if err != nil {
		t.Fatalf("DynamicStream: %v", err)
	}

	// Pull one event so a buffered result exists to be released.
	if _, err := stream.Next(); err != nil {
		t.Fatalf("first Next: %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Close again to verify multi-call safety.
	if err := stream.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	select {
	case <-producerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("producer did not observe cancel within 2s — Close did not drain")
	}

	// Give the background drain goroutine a moment to release any
	// still-buffered results.
	deadline := time.Now().Add(2 * time.Second)
	for released.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if released.Load() < 1 {
		t.Errorf("expected at least one StreamResult.Release call after Close, got %d", released.Load())
	}
}

func TestStreamNextReturnsEOF(t *testing.T) {
	rt := &fakeRuntime{}
	c := newClient(t, rt)

	rt.streamingImpl = func(adapter bamlutils.Adapter, _ any) (<-chan bamlutils.StreamResult, error) {
		ch := make(chan bamlutils.StreamResult, 1)
		ch <- &fakeStreamResult{
			kind:  bamlutils.StreamResultKindFinal,
			final: map[string]any{"DynamicProperties": map[string]any{"answer": "4"}},
		}
		close(ch)
		return ch, nil
	}

	stream, err := c.DynamicStream(context.Background(), validRequest())
	if err != nil {
		t.Fatalf("DynamicStream: %v", err)
	}

	// Final event.
	if _, err := stream.Next(); err != nil {
		t.Fatalf("first Next: %v", err)
	}
	// Channel close -> io.EOF.
	if _, err := stream.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("second Next err = %v, want io.EOF", err)
	}
	// EOF latches; further Next calls remain EOF.
	if _, err := stream.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("third Next err = %v, want latched io.EOF", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("Close after EOF: %v", err)
	}
}

// TestStreamNextHonorsContextDeadline pins the #420 fix: Next must abort
// when the stream context's deadline expires, even if the worker never
// produces a result and never closes the result channel. Before the fix
// Next did a bare `<-s.results`, so a wedged worker stalled the read
// forever and a per-call deadline could not preempt it — the failure
// mode that ran the streaming CI cell to its job-level timeout.
//
// The Stream is built directly over a controlled result channel that is
// never closed until after Next returns, so the ONLY select arm Next can
// take is ctx.Done(). Driving it through the full client (where the same
// cancellation that fires the deadline also closes the bridge channel)
// would race the two arms and let Next observe io.EOF instead — a flaky
// assertion. We require a deterministic context.DeadlineExceeded.
func TestStreamNextHonorsContextDeadline(t *testing.T) {
	results := make(chan *workerplugin.StreamResult)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	s := newStream(ctx, cancel, func() {}, results, false, "test stream", nil, false)

	start := time.Now()
	_, err := s.Next()
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Next err = %v, want context.DeadlineExceeded", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("Next blocked %s past the 100ms deadline — context not honored", elapsed)
	}
	// The terminal context error latches across subsequent Next calls.
	if _, err := s.Next(); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second Next err = %v, want latched context.DeadlineExceeded", err)
	}

	// Only now is it safe to close: no Next is in flight to race the
	// close against the deadline arm.
	close(results)
}

func TestNewWithRuntimeRunsInit(t *testing.T) {
	// init must run before the worker handler is constructed so the
	// public New() path triggers BAML's native sync.Once before any
	// dispatch lands.
	var ran atomic.Int64
	rt := &fakeRuntime{}
	c, err := newWithRuntime(rt, func() { ran.Add(1) })
	if err != nil {
		t.Fatalf("newWithRuntime: %v", err)
	}
	if c == nil {
		t.Fatal("expected non-nil client")
	}
	if got := ran.Load(); got != 1 {
		t.Errorf("init ran %d times, want 1", got)
	}
}

func TestNilReceiverReturnsError(t *testing.T) {
	var c *Client
	if _, err := c.DynamicCall(context.Background(), validRequest()); err == nil {
		t.Error("expected DynamicCall on nil receiver to return error")
	}
	if _, err := c.DynamicCallRaw(context.Background(), validRequest()); err == nil {
		t.Error("expected DynamicCallRaw on nil receiver to return error")
	}
	if _, err := c.DynamicParse(context.Background(), ParseRequest{Raw: "x", OutputSchema: &OutputSchema{Properties: MustOrderedMap(OrderedKV("a", &Property{Type: "string"}))}}); err == nil {
		t.Error("expected DynamicParse on nil receiver to return error")
	}
	if _, err := c.DynamicStream(context.Background(), validRequest()); err == nil {
		t.Error("expected DynamicStream on nil receiver to return error")
	}
	if _, err := c.DynamicStreamRaw(context.Background(), validRequest()); err == nil {
		t.Error("expected DynamicStreamRaw on nil receiver to return error")
	}
}

func TestInvalidRequestWrapsValidationError(t *testing.T) {
	rt := &fakeRuntime{}
	c := newClient(t, rt)

	_, err := c.DynamicCall(context.Background(), Request{})
	if err == nil {
		t.Fatal("expected validation error on empty Request")
	}
	if !strings.Contains(err.Error(), "dynclient: dynamic call") {
		t.Errorf("error = %q, want it to be wrapped with the dynclient label", err.Error())
	}
}

// directStream wires a Stream straight on top of a hand-fed
// *workerplugin.StreamResult channel, bypassing the worker bridge so
// tests can pin Data/Raw/Reasoning values exactly. The real bridge in
// the worker package rewrites these fields (e.g. marshals a nil
// Stream() to JSON "null"), which would hide the precise code paths
// the CR fix targets.
func directStream(t *testing.T, needRaw bool, frames ...*workerplugin.StreamResult) *Stream {
	t.Helper()
	ch := make(chan *workerplugin.StreamResult, len(frames))
	for _, f := range frames {
		ch <- f
	}
	close(ch)
	ctx, cancel := context.WithCancel(context.Background())
	s := newStream(ctx, cancel, func() {}, ch, needRaw, "dynamic stream", nil, false)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestStreamSuppressesEmptyPartial_NonRaw(t *testing.T) {
	stream := directStream(t, false,
		// Empty-data stream frame — should be suppressed in non-raw mode.
		&workerplugin.StreamResult{Kind: workerplugin.StreamResultKindStream},
		// Real partial with structured data.
		&workerplugin.StreamResult{
			Kind: workerplugin.StreamResultKindStream,
			Data: []byte(`{"answer":"x"}`),
		},
		&workerplugin.StreamResult{
			Kind: workerplugin.StreamResultKindFinal,
			Data: []byte(`{"answer":"x"}`),
		},
	)

	events := drainStreamFull(t, stream)
	partials := 0
	for _, ev := range events {
		if ev.Kind == EventPartial {
			partials++
		}
	}
	if partials != 1 {
		t.Errorf("EventPartial count = %d, want 1 (empty-data partial should be suppressed)", partials)
	}
}

func TestStreamSuppressesEmptyPartial_Raw_NoText(t *testing.T) {
	stream := directStream(t, true,
		// Reset-only stream frame with no data, no raw, no reasoning.
		// Should produce only EventReset, not an EventPartial.
		&workerplugin.StreamResult{Kind: workerplugin.StreamResultKindStream, Reset: true},
		&workerplugin.StreamResult{
			Kind: workerplugin.StreamResultKindFinal,
			Data: []byte(`{"answer":"x"}`),
			Raw:  "x",
		},
	)

	events := drainStreamFull(t, stream)
	for _, ev := range events {
		if ev.Kind == EventPartial {
			t.Errorf("unexpected EventPartial for reset-only frame: %+v", ev)
		}
	}
	sawReset := false
	for _, ev := range events {
		if ev.Kind == EventReset {
			sawReset = true
		}
	}
	if !sawReset {
		t.Error("expected EventReset to be emitted for reset-only frame")
	}
}

func TestStreamRawEmitsOnTextOnly(t *testing.T) {
	// Text-only raw delta: no structured Data but a non-empty Raw. The
	// library should surface an EventPartial so raw consumers see the
	// accumulated upstream text mid-stream.
	stream := directStream(t, true,
		&workerplugin.StreamResult{Kind: workerplugin.StreamResultKindStream, Raw: "hi"},
		&workerplugin.StreamResult{
			Kind: workerplugin.StreamResultKindFinal,
			Data: []byte(`{"answer":"hi"}`),
			Raw:  "hi",
		},
	)

	events := drainStreamFull(t, stream)
	var partial *Event
	for _, ev := range events {
		if ev.Kind == EventPartial {
			partial = ev
			break
		}
	}
	if partial == nil {
		t.Fatal("expected an EventPartial for a text-only raw delta")
	}
	if partial.Raw != "hi" {
		t.Errorf("partial.Raw = %q, want %q", partial.Raw, "hi")
	}
	if partial.Data != nil {
		t.Errorf("partial.Data = %s, want nil (no structured payload)", string(partial.Data))
	}
}

func TestStream_ZeroValue_NextReturnsError(t *testing.T) {
	var s Stream
	_, err := s.Next()
	if err == nil {
		t.Fatal("expected zero-value Stream.Next to return an error, got nil")
	}
	if !strings.Contains(err.Error(), "uninitialized Stream") {
		t.Errorf("error = %q, want it to mention %q", err.Error(), "uninitialized Stream")
	}
}

func TestStream_ZeroValue_CloseNoOp(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("zero-value Stream.Close panicked: %v", r)
		}
	}()
	var s Stream
	if err := s.Close(); err != nil {
		t.Errorf("zero-value Stream.Close err = %v, want nil", err)
	}
}

// -- Helpers --------------------------------------------------------

func drainStreamFull(t *testing.T, s *Stream) []*Event {
	t.Helper()
	var events []*Event
	for {
		ev, err := s.Next()
		if errors.Is(err, io.EOF) {
			return events
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		events = append(events, ev)
	}
}

type captureLogger struct {
	mu    sync.Mutex
	lines []string
}

func (l *captureLogger) Debug(msg string, args ...interface{}) { l.record("DEBUG", msg) }
func (l *captureLogger) Info(msg string, args ...interface{})  { l.record("INFO", msg) }
func (l *captureLogger) Warn(msg string, args ...interface{})  { l.record("WARN", msg) }
func (l *captureLogger) Error(msg string, args ...interface{}) { l.record("ERROR", msg) }
func (l *captureLogger) record(level, msg string) {
	l.mu.Lock()
	l.lines = append(l.lines, level+":"+msg)
	l.mu.Unlock()
}

// reflect imported only to silence go vet when other consumers are
// behind build tags; the test below uses it.
var _ = reflect.DeepEqual

// compile-time check that *Client satisfies the public method set we
// document — a refactor that drops a method should fail to build here
// rather than at first downstream use.
type _ifaceCheck interface {
	DynamicCall(context.Context, Request) (*CallResult, error)
	DynamicCallRaw(context.Context, Request) (*CallRawResult, error)
	DynamicStream(context.Context, Request) (*Stream, error)
	DynamicStreamRaw(context.Context, Request) (*Stream, error)
	DynamicParse(context.Context, ParseRequest) (*ParseResult, error)
}

var _ _ifaceCheck = (*Client)(nil)

// -- Backend option interaction tests --------------------------------

// runOptionsCase exercises newWithRuntime with a counted init callback
// and returns the resulting error. Error cases assert the init counter
// stays at zero, proving validation rejected the configuration BEFORE
// runtime initialization could load the native library.
func runOptionsCase(t *testing.T, wantErr bool, opts ...Option) error {
	t.Helper()
	var initCalls atomic.Int64
	rt := &fakeRuntime{}
	_, err := newWithRuntime(rt, func() { initCalls.Add(1) }, opts...)
	if wantErr {
		if err == nil {
			t.Fatalf("expected validation error, got nil")
		}
		if got := initCalls.Load(); got != 0 {
			t.Errorf("init ran %d times on a rejected config; validation must run before init", got)
		}
		return err
	}
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := initCalls.Load(); got != 1 {
		t.Errorf("init ran %d times on a valid config, want 1", got)
	}
	return nil
}

func TestBackendOptions_ValidCombinations(t *testing.T) {
	netClient := &http.Client{}
	fastOpts := llmhttp.FastHTTPClientOptions{MaxConns: 64}

	cases := []struct {
		name string
		opts []Option
	}{
		{"no options", nil},
		{"fasthttp tuning only", []Option{WithFastHTTPClient(fastOpts)}},
		{"net/http client only", []Option{WithNetHTTPClient(netClient)}},
		{"both tunings, implicit auto", []Option{WithFastHTTPClient(fastOpts), WithNetHTTPClient(netClient)}},
		{"explicit auto + both tunings", []Option{WithClientMode(llmhttp.ClientModeAuto), WithFastHTTPClient(fastOpts), WithNetHTTPClient(netClient)}},
		{"forced net/http + net client", []Option{WithClientMode(llmhttp.ClientModeNetHTTP), WithNetHTTPClient(netClient)}},
		{"forced fasthttp + fast opts", []Option{WithClientMode(llmhttp.ClientModeFastHTTP), WithFastHTTPClient(fastOpts)}},
		// WithNetHTTPClient(nil) signals "net/http tuning requested" so
		// validation runs, but the default tuned transport still applies.
		{"forced net/http + nil net client", []Option{WithClientMode(llmhttp.ClientModeNetHTTP), WithNetHTTPClient(nil)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runOptionsCase(t, false, tc.opts...)
		})
	}
}

func TestBackendOptions_ConflictingCombinations(t *testing.T) {
	netClient := &http.Client{}
	fastOpts := llmhttp.FastHTTPClientOptions{MaxConns: 64}

	cases := []struct {
		name        string
		opts        []Option
		mustNameA   string
		mustNameB   string
		mustNameC   string // optional
		invalidEnum bool
	}{
		{
			name:      "forced net/http with fasthttp tuning",
			opts:      []Option{WithClientMode(llmhttp.ClientModeNetHTTP), WithFastHTTPClient(fastOpts)},
			mustNameA: "WithClientMode",
			mustNameB: "WithFastHTTPClient",
		},
		{
			name:      "forced fasthttp with net/http client",
			opts:      []Option{WithClientMode(llmhttp.ClientModeFastHTTP), WithNetHTTPClient(netClient)},
			mustNameA: "WithClientMode",
			mustNameB: "WithNetHTTPClient",
		},
		{
			name:      "forced net/http with both tunings",
			opts:      []Option{WithClientMode(llmhttp.ClientModeNetHTTP), WithFastHTTPClient(fastOpts), WithNetHTTPClient(netClient)},
			mustNameA: "WithClientMode",
			mustNameB: "WithFastHTTPClient",
		},
		{
			name:      "forced fasthttp with both tunings",
			opts:      []Option{WithClientMode(llmhttp.ClientModeFastHTTP), WithFastHTTPClient(fastOpts), WithNetHTTPClient(netClient)},
			mustNameA: "WithClientMode",
			mustNameB: "WithNetHTTPClient",
		},
		{
			name:        "invalid client mode value",
			opts:        []Option{WithClientMode(llmhttp.ClientMode(999))},
			invalidEnum: true,
		},
		{
			// nil counts as "net/http tuning supplied" so even nil cannot
			// pass under ClientModeFastHTTP.
			name:      "forced fasthttp with nil net client",
			opts:      []Option{WithClientMode(llmhttp.ClientModeFastHTTP), WithNetHTTPClient(nil)},
			mustNameA: "WithClientMode",
			mustNameB: "WithNetHTTPClient",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := runOptionsCase(t, true, tc.opts...)
			if !strings.Contains(err.Error(), "dynclient: new client") {
				t.Errorf("error not wrapped with construction label: %v", err)
			}
			if tc.invalidEnum {
				if !strings.Contains(err.Error(), "invalid llmhttp.ClientMode") {
					t.Errorf("expected invalid-mode error text, got: %v", err)
				}
				return
			}
			for _, name := range []string{tc.mustNameA, tc.mustNameB, tc.mustNameC} {
				if name == "" {
					continue
				}
				if !strings.Contains(err.Error(), name) {
					t.Errorf("expected error to name %q; got: %v", name, err)
				}
			}
		})
	}
}

func TestBackendOptions_LastWins(t *testing.T) {
	netClient := &http.Client{}
	fastOpts := llmhttp.FastHTTPClientOptions{MaxConns: 64}

	// Last-wins on WithClientMode: an early NetHTTP that would conflict
	// with WithFastHTTPClient is harmless if a later WithClientMode(Auto)
	// overrides it.
	runOptionsCase(t, false,
		WithClientMode(llmhttp.ClientModeNetHTTP),
		WithClientMode(llmhttp.ClientModeAuto),
		WithFastHTTPClient(fastOpts),
	)

	// Last-wins on WithClientMode: FastHTTP first then NetHTTP, with
	// WithNetHTTPClient present, is valid because the final mode is
	// NetHTTP.
	runOptionsCase(t, false,
		WithClientMode(llmhttp.ClientModeFastHTTP),
		WithClientMode(llmhttp.ClientModeNetHTTP),
		WithNetHTTPClient(netClient),
	)

	// Last-wins on WithNetHTTPClient: two calls — the final non-nil
	// client is what reaches the underlying llmhttp.Client.
	clientA := &http.Client{Timeout: 1 * time.Second}
	clientB := &http.Client{Timeout: 2 * time.Second}
	cfg := &config{}
	for _, opt := range []Option{WithNetHTTPClient(clientA), WithNetHTTPClient(clientB)} {
		if err := opt(cfg); err != nil {
			t.Fatalf("option: %v", err)
		}
	}
	if cfg.netHTTPClient != clientB {
		t.Errorf("WithNetHTTPClient is not last-wins: got %p, want %p", cfg.netHTTPClient, clientB)
	}

	// Last-wins on WithFastHTTPClient: two calls — the final options
	// value is what reaches the cache.
	cfg = &config{}
	first := llmhttp.FastHTTPClientOptions{MaxConns: 10}
	second := llmhttp.FastHTTPClientOptions{MaxConns: 20}
	for _, opt := range []Option{WithFastHTTPClient(first), WithFastHTTPClient(second)} {
		if err := opt(cfg); err != nil {
			t.Fatalf("option: %v", err)
		}
	}
	if cfg.fastHTTPOptions.MaxConns != 20 {
		t.Errorf("WithFastHTTPClient is not last-wins: MaxConns = %d, want 20", cfg.fastHTTPOptions.MaxConns)
	}
}

// capturePreserveOrder runs req through the supplied client and returns
// the value of dynamic_types.preserve_order observed on the worker
// payload. Streaming and call paths route through the same
// ToWorkerInput, so a single capture is sufficient to assert resolution
// behaviour for every public method.
func capturePreserveOrder(t *testing.T, c *Client, rt *fakeRuntime, req Request) bool {
	t.Helper()
	var got bool
	rt.streamingImpl = func(_ bamlutils.Adapter, input any) (<-chan bamlutils.StreamResult, error) {
		// MakeInput hands the handler a *map[string]any; the worker
		// unmarshals our bytes into it before invoking Impl.
		ptr, ok := input.(*map[string]any)
		if !ok || ptr == nil {
			t.Fatalf("unexpected input shape %T", input)
		}
		opts, _ := (*ptr)["__baml_options__"].(map[string]any)
		typeBuilder, _ := opts["type_builder"].(map[string]any)
		dt, _ := typeBuilder["dynamic_types"].(map[string]any)
		if v, ok := dt["preserve_order"].(bool); ok {
			got = v
		}
		ch := make(chan bamlutils.StreamResult, 1)
		ch <- &fakeStreamResult{kind: bamlutils.StreamResultKindFinal, final: map[string]any{"DynamicProperties": map[string]any{"answer": "ok"}}}
		close(ch)
		return ch, nil
	}
	if _, err := c.DynamicCall(context.Background(), req); err != nil {
		t.Fatalf("DynamicCall: %v", err)
	}
	return got
}

// TestPreserveSchemaOrderDefaultTruthTable pins the four-way resolution
// table for the dynclient default plus per-request override:
//
//   - dynclient.New()           + nil request   => preserve (default true)
//   - WithPreserveSchemaOrderDefault(false) + nil request => sorted
//   - request *true   always preserves (regardless of client default)
//   - request *false  always sorts (regardless of client default)
func TestPreserveSchemaOrderDefaultTruthTable(t *testing.T) {
	cases := []struct {
		name          string
		opts          []Option
		preserve      *bool
		wantPreserved bool
	}{
		{name: "default client + nil request => preserved", opts: nil, preserve: nil, wantPreserved: true},
		{name: "WithPreserveSchemaOrderDefault(false) + nil request => sorted", opts: []Option{WithPreserveSchemaOrderDefault(false)}, preserve: nil, wantPreserved: false},
		{name: "default client + request *true => preserved", opts: nil, preserve: boolPtr(true), wantPreserved: true},
		{name: "default client + request *false => sorted", opts: nil, preserve: boolPtr(false), wantPreserved: false},
		{name: "WithPreserveSchemaOrderDefault(false) + request *true => preserved", opts: []Option{WithPreserveSchemaOrderDefault(false)}, preserve: boolPtr(true), wantPreserved: true},
		{name: "WithPreserveSchemaOrderDefault(true) + request *false => sorted", opts: []Option{WithPreserveSchemaOrderDefault(true)}, preserve: boolPtr(false), wantPreserved: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := &fakeRuntime{}
			c := newClient(t, rt, tc.opts...)
			req := validRequest()
			req.PreserveSchemaOrder = tc.preserve
			got := capturePreserveOrder(t, c, rt, req)
			if got != tc.wantPreserved {
				t.Errorf("preserve_order: got %v want %v", got, tc.wantPreserved)
			}
		})
	}
}

// TestNewDoesNotReadPreserveOrderEnv pins that the preserve-order
// default does not silently read process env vars at New() — the only
// way to flip the dynclient default is through
// WithPreserveSchemaOrderDefault.
func TestNewDoesNotReadPreserveOrderEnv(t *testing.T) {
	t.Setenv("BAML_REST_PRESERVE_SCHEMA_ORDER_DEFAULT", "false")

	rt := &fakeRuntime{}
	c := newClient(t, rt)
	if got := capturePreserveOrder(t, c, rt, validRequest()); got != true {
		t.Errorf("env-driven default leaked: preserve_order = %v, want true (dynclient default)", got)
	}

	rt2 := &fakeRuntime{}
	c2 := newClient(t, rt2, WithPreserveSchemaOrderDefault(false))
	if got := capturePreserveOrder(t, c2, rt2, validRequest()); got != false {
		t.Errorf("explicit WithPreserveSchemaOrderDefault(false) ignored: preserve_order = %v, want false", got)
	}
}

// TestPreserveSchemaOrderDefault_DoesNotMutateCallerRequest pins that
// the in-method default resolution operates on the by-value copy of
// Request / ParseRequest, so the caller's PreserveSchemaOrder pointer
// is not silently filled in.
func TestPreserveSchemaOrderDefault_DoesNotMutateCallerRequest(t *testing.T) {
	rt := &fakeRuntime{}
	c := newClient(t, rt)
	rt.streamingImpl = func(_ bamlutils.Adapter, _ any) (<-chan bamlutils.StreamResult, error) {
		ch := make(chan bamlutils.StreamResult, 1)
		ch <- &fakeStreamResult{kind: bamlutils.StreamResultKindFinal, final: map[string]any{"DynamicProperties": map[string]any{"answer": "ok"}}}
		close(ch)
		return ch, nil
	}
	req := validRequest()
	if req.PreserveSchemaOrder != nil {
		t.Fatalf("validRequest must start with nil PreserveSchemaOrder; got %v", req.PreserveSchemaOrder)
	}
	if _, err := c.DynamicCall(context.Background(), req); err != nil {
		t.Fatalf("DynamicCall: %v", err)
	}
	if req.PreserveSchemaOrder != nil {
		t.Errorf("PreserveSchemaOrder mutated through the by-value call: got %v want nil", req.PreserveSchemaOrder)
	}
}

func boolPtr(b bool) *bool { return &b }

// TestIssue324_DynamicCall_NoPanic pins the public dynclient.Client.
// DynamicCall regression for #324: the bamlutils worker-boundary marshal
// no longer crashes the host on Viktor's nested-embedded-struct shape.
// Exercises the full public call path through a fakeRuntime so no real
// network I/O is involved, and asserts a normal (nil-error, decodable
// result) return — without the migration this call panicked inside
// goccy/go-json's encoder VM before reaching transport.
func TestIssue324_DynamicCall_NoPanic(t *testing.T) {
	rt := &fakeRuntime{}
	c := newClient(t, rt)
	rt.streamingImpl = func(_ bamlutils.Adapter, _ any) (<-chan bamlutils.StreamResult, error) {
		ch := make(chan bamlutils.StreamResult, 1)
		ch <- &fakeStreamResult{
			kind:  bamlutils.StreamResultKindFinal,
			final: map[string]any{"DynamicProperties": map[string]any{"answer": "ok"}},
		}
		close(ch)
		return ch, nil
	}

	type Inner struct {
		B string   `json:"b"`
		C string   `json:"c"`
		D []string `json:"d,omitempty"`
	}
	type Outer struct {
		Inner
	}

	req := validRequest()
	// Append Viktor's panic-trigger shape onto the validRequest's
	// schema. The OrderedMap append goes through Set so the existing
	// "answer" entry is preserved and a literal_string property whose
	// Value carries a nested embedded struct (the original #324
	// trigger) is added.
	if err := req.OutputSchema.Properties.Set("panic_trigger", &Property{
		Type:  "literal_string",
		Value: Outer{},
	}); err != nil {
		t.Fatalf("Properties.Set: %v", err)
	}

	result, err := c.DynamicCall(context.Background(), req)
	if err != nil {
		t.Fatalf("DynamicCall must not panic and must return a normal result; got error: %v", err)
	}
	if result == nil || len(result.Data) == 0 {
		t.Fatalf("DynamicCall returned empty result")
	}
}
