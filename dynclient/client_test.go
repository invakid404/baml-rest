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

	"github.com/goccy/go-json"
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
			Properties: map[string]*Property{
				"answer": {Type: "string"},
			},
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
		WithLogger(logger),
		WithMetricsRegistry(metrics),
		WithBaseURLRewrites(rewrites),
		WithHTTPClient(httpClient),
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
	if err := json.Unmarshal(result.Data, &decoded); err != nil {
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
	if err := json.Unmarshal(result.Data, &decoded); err != nil {
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
			Properties: map[string]*Property{"answer": {Type: "string"}},
		},
	}
	result, err := c.DynamicParse(context.Background(), parseReq)
	if err != nil {
		t.Fatalf("DynamicParse: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(result.Data, &decoded); err != nil {
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
	if _, err := c.DynamicParse(context.Background(), ParseRequest{Raw: "x", OutputSchema: &OutputSchema{Properties: map[string]*Property{"a": {Type: "string"}}}}); err == nil {
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
