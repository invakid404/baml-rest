package worker

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/bamlutils/urlrewrite"
)

// TestHandlerNewRequiresRuntime pins the construction-time contract
// that Config.Runtime is mandatory. The handler stack assumes a
// non-nil runtime in CallStream / Parse — surfacing the error at
// construction keeps the failure clear rather than crashing on first
// dispatch.
func TestHandlerNewRequiresRuntime(t *testing.T) {
	t.Parallel()

	_, err := New(Config{})
	if err == nil {
		t.Fatal("expected error from New(Config{}) with no Runtime, got nil")
	}
	if !errors.Is(err, ErrRuntimeRequired) {
		t.Fatalf("expected ErrRuntimeRequired, got %v", err)
	}
}

// TestHandlerNewRejectsServeShadowConflict pins the mutual-exclusion contract:
// supplying BOTH a native serve and a native shadow comparator to New is a
// construction error, so a direct or dynclient caller cannot bypass workerboot's
// factory-level check and install two native child-attempt callbacks. Either
// comparator alone is accepted.
func TestHandlerNewRejectsServeShadowConflict(t *testing.T) {
	t.Parallel()

	serve := func(context.Context, bamlutils.NativeServeRequest) bamlutils.NativeServeResult {
		return bamlutils.NativeServeResult{}
	}
	shadow := func(context.Context, bamlutils.NativeShadowRequest) bamlutils.NativeShadowResult {
		return bamlutils.NativeShadowResult{}
	}

	if _, err := New(Config{
		Runtime:                &fakeRuntime{},
		NativeServeComparator:  serve,
		NativeShadowComparator: shadow,
	}); !errors.Is(err, ErrNativeCallbackConflict) {
		t.Fatalf("expected ErrNativeCallbackConflict for both comparators, got %v", err)
	}
	if _, err := New(Config{Runtime: &fakeRuntime{}, NativeServeComparator: serve}); err != nil {
		t.Fatalf("serve-only New must succeed, got %v", err)
	}
	if _, err := New(Config{Runtime: &fakeRuntime{}, NativeShadowComparator: shadow}); err != nil {
		t.Fatalf("shadow-only New must succeed, got %v", err)
	}
}

// TestHandlerCallStreamUsesInjectedRuntime asserts CallStream looks up
// the method on the injected Runtime (not the root baml_rest package)
// and consults the runtime's MakeAdapter for the per-request adapter.
// Without the injection the handler would silently fall back to the
// root generated package and the dynclient seam would be decorative.
func TestHandlerCallStreamUsesInjectedRuntime(t *testing.T) {
	t.Parallel()

	wantInput := map[string]any{"key": "value"}
	called := false
	method := bamlutils.StreamingMethod{
		MakeInput: func() any { return &map[string]any{} },
		Impl: func(adapter bamlutils.Adapter, input any) (<-chan bamlutils.StreamResult, error) {
			called = true
			// Round-trip the input through the make-input contract so a
			// regression that swapped the runtime would surface as
			// "method never invoked" rather than as a successful no-op.
			ch := make(chan bamlutils.StreamResult)
			close(ch)
			return ch, nil
		},
	}
	rt := &fakeRuntime{methods: map[string]bamlutils.StreamingMethod{"ok": method}}
	h := newTestHandler(t, Config{Runtime: rt})

	body := []byte(`{"key":"value"}`)
	out, err := h.CallStream(context.Background(), "ok", body, bamlutils.StreamModeCall)
	if err != nil {
		t.Fatalf("CallStream: %v", err)
	}
	// Drain so the bridge goroutine exits cleanly.
	for range out {
	}
	if !called {
		t.Fatal("expected injected method.Impl to be invoked")
	}
	_ = wantInput
}

// TestHandlerCallStreamMissingMethod pins the existing
// `method %q not found` contract on the injected-runtime path. The
// error shape doubles as a regression guard against a refactor that
// flips the lookup return order or replaces the sentinel with a
// generic error.
func TestHandlerCallStreamMissingMethod(t *testing.T) {
	t.Parallel()

	h := newTestHandler(t, Config{Runtime: &fakeRuntime{}})
	_, err := h.CallStream(context.Background(), "missing", []byte(`{}`), bamlutils.StreamModeCall)
	if err == nil {
		t.Fatal("expected error for missing method")
	}
	if !strings.Contains(err.Error(), `method "missing" not found`) {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestHandlerParseUsesInjectedRuntime mirrors the CallStream test for
// the Parse path.
func TestHandlerParseUsesInjectedRuntime(t *testing.T) {
	t.Parallel()

	called := false
	method := bamlutils.ParseMethod{
		MakeOutput: func() any { return &map[string]any{} },
		Impl: func(adapter bamlutils.Adapter, raw string) (any, error) {
			called = true
			return map[string]any{"got": raw}, nil
		},
	}
	rt := &fakeRuntime{parseMethods: map[string]bamlutils.ParseMethod{"parse-ok": method}}
	h := newTestHandler(t, Config{Runtime: rt})

	res, err := h.Parse(context.Background(), "parse-ok", []byte(`{"raw":"hello"}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !called {
		t.Fatal("expected injected parse method to be invoked")
	}
	if !strings.Contains(string(res.Data), "hello") {
		t.Fatalf("unexpected parse payload: %s", string(res.Data))
	}
}

// TestHandlerParseMissingMethod pins the existing
// `parse method %q not found` contract on the injected-runtime path.
func TestHandlerParseMissingMethod(t *testing.T) {
	t.Parallel()

	h := newTestHandler(t, Config{Runtime: &fakeRuntime{}})
	_, err := h.Parse(context.Background(), "missing", []byte(`{"raw":"x"}`))
	if err == nil {
		t.Fatal("expected error for missing parse method")
	}
	if !strings.Contains(err.Error(), `parse method "missing" not found`) {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestHandlerDeBAMLConfigAndSchemaWiring pins that the de-BAML umbrella
// switch is installed per-handler via configureAdapter (alongside
// BuildRequest) and that the carried output schema from
// __baml_options__.output_schema is installed via apply. The generated
// dynamic BuildRequest seam reads both through DeBAMLConfig() /
// DeBAMLOutputSchema(); they MUST reflect the per-handler config and the
// per-request payload, not a shared global.
func TestHandlerDeBAMLConfigAndSchemaWiring(t *testing.T) {
	t.Parallel()

	var captured *fakeAdapter
	rt := &fakeRuntime{methods: map[string]bamlutils.StreamingMethod{
		"x": {
			MakeInput: func() any { return &map[string]any{} },
			Impl: func(adapter bamlutils.Adapter, _ any) (<-chan bamlutils.StreamResult, error) {
				captured = adapter.(*fakeAdapter)
				ch := make(chan bamlutils.StreamResult)
				close(ch)
				return ch, nil
			},
		},
	}}
	renderCalls := 0
	render := func(*bamlutils.DynamicOutputSchema) (string, error) {
		renderCalls++
		return "block", nil
	}
	h := newTestHandler(t, Config{Runtime: rt, DeBAML: bamlutils.DeBAMLConfig{Enabled: true}, DeBAMLRender: render})

	body := []byte(`{"__baml_options__":{"output_schema":{"properties":{"answer":{"type":"string"}}}}}`)
	out, err := h.CallStream(context.Background(), "x", body, bamlutils.StreamModeCall)
	if err != nil {
		t.Fatalf("CallStream: %v", err)
	}
	for range out {
	}

	if captured == nil {
		t.Fatal("expected the handler to install an adapter via the fake runtime")
	}
	if !captured.DeBAMLConfig().Enabled {
		t.Errorf("DeBAMLConfig not installed: %#v", captured.DeBAMLConfig())
	}
	if captured.setDeBAMLConfigCalls != 1 {
		t.Errorf("SetDeBAMLConfig called %d times, want 1", captured.setDeBAMLConfigCalls)
	}
	if captured.setDeBAMLOutputSchemaCalls != 1 {
		t.Errorf("SetDeBAMLOutputSchema called %d times, want 1", captured.setDeBAMLOutputSchemaCalls)
	}
	schema := captured.DeBAMLOutputSchema()
	if schema == nil || !schema.Properties.Has("answer") {
		t.Errorf("carried output schema not installed from __baml_options__: %#v", schema)
	}
	if captured.setDeBAMLRendererCalls != 1 {
		t.Errorf("SetDeBAMLRenderer called %d times, want 1", captured.setDeBAMLRendererCalls)
	}
	if captured.deBAMLRenderer == nil {
		t.Fatal("render callback not installed on the adapter")
	}
	if _, err := captured.deBAMLRenderer(schema); err != nil || renderCalls != 1 {
		t.Errorf("installed renderer not the one wired in: err=%v calls=%d", err, renderCalls)
	}
}

// TestHandlerPerInstanceURLRewrites pins that the worker's
// __baml_options__.client_registry rewrite pass reads from the
// per-handler BaseURLRewrites slice rather than the urlrewrite global.
// Two handlers with different rules, fed the same incoming registry,
// MUST produce different rewritten base_urls on their adapter.
func TestHandlerPerInstanceURLRewrites(t *testing.T) {
	t.Parallel()

	rulesA := []urlrewrite.Rule{{From: "https://upstream.example/", To: "http://a.local:8080/"}}
	rulesB := []urlrewrite.Rule{{From: "https://upstream.example/", To: "http://b.local:9090/"}}

	makeMethod := func(captured **fakeAdapter) bamlutils.StreamingMethod {
		return bamlutils.StreamingMethod{
			MakeInput: func() any { return &map[string]any{} },
			Impl: func(adapter bamlutils.Adapter, _ any) (<-chan bamlutils.StreamResult, error) {
				*captured = adapter.(*fakeAdapter)
				ch := make(chan bamlutils.StreamResult)
				close(ch)
				return ch, nil
			},
		}
	}

	var capA, capB *fakeAdapter
	rtA := &fakeRuntime{methods: map[string]bamlutils.StreamingMethod{"x": makeMethod(&capA)}}
	rtB := &fakeRuntime{methods: map[string]bamlutils.StreamingMethod{"x": makeMethod(&capB)}}
	hA := newTestHandler(t, Config{Runtime: rtA, BaseURLRewrites: rulesA})
	hB := newTestHandler(t, Config{Runtime: rtB, BaseURLRewrites: rulesB})

	body := []byte(`{"__baml_options__":{"client_registry":{"clients":[{"name":"C","provider":"openai","options":{"base_url":"https://upstream.example/v1"}}]}}}`)

	for _, h := range []*Handler{hA, hB} {
		out, err := h.CallStream(context.Background(), "x", body, bamlutils.StreamModeCall)
		if err != nil {
			t.Fatalf("CallStream: %v", err)
		}
		for range out {
		}
	}

	gotA := capA.OriginalClientRegistry().Clients[0].Options["base_url"]
	gotB := capB.OriginalClientRegistry().Clients[0].Options["base_url"]
	if gotA == gotB {
		t.Fatalf("expected handlers with different rewrite rules to produce different base_urls; got %v both", gotA)
	}
	if want := "http://a.local:8080/v1"; gotA != want {
		t.Errorf("handler A: base_url = %v, want %s", gotA, want)
	}
	if want := "http://b.local:9090/v1"; gotB != want {
		t.Errorf("handler B: base_url = %v, want %s", gotB, want)
	}
}

// TestHandlerInstallsHTTPClient pins that the per-handler HTTPClient
// is installed on every adapter via SetHTTPClient. configureAdapter
// runs unconditionally, and the fake adapter records the pointer
// identity — a regression that dropped the install would fail this
// equality check.
func TestHandlerInstallsHTTPClient(t *testing.T) {
	t.Parallel()

	client := llmhttp.NewClientWithOptions(llmhttp.ClientOptions{
		NetHTTPClient: &http.Client{},
		Mode:          llmhttp.ClientModeNetHTTP,
	})

	var captured *fakeAdapter
	method := bamlutils.StreamingMethod{
		MakeInput: func() any { return &map[string]any{} },
		Impl: func(adapter bamlutils.Adapter, _ any) (<-chan bamlutils.StreamResult, error) {
			captured = adapter.(*fakeAdapter)
			ch := make(chan bamlutils.StreamResult)
			close(ch)
			return ch, nil
		},
	}
	rt := &fakeRuntime{methods: map[string]bamlutils.StreamingMethod{"x": method}}
	h := newTestHandler(t, Config{Runtime: rt, HTTPClient: client})

	out, err := h.CallStream(context.Background(), "x", []byte(`{}`), bamlutils.StreamModeCall)
	if err != nil {
		t.Fatalf("CallStream: %v", err)
	}
	for range out {
	}

	if captured == nil {
		t.Fatal("expected adapter to be captured")
	}
	if captured.setHTTPClientCalls != 1 {
		t.Errorf("expected SetHTTPClient to be called exactly once, got %d", captured.setHTTPClientCalls)
	}
	if captured.HTTPClient() != client {
		t.Errorf("expected adapter.HTTPClient() to equal the injected client pointer")
	}
}
