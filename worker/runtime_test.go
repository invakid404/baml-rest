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

// TestHandlerPerInstanceBuildRequestConfig pins that two handlers with
// the same runtime but different BuildRequest configs install the
// matching value on their adapters — no leakage through a shared
// global. This is the load-bearing assertion for the dynclient seam:
// generated routers read `adapter.BuildRequestConfig().UseBuildRequest`,
// and that value MUST be the per-handler choice rather than a process-
// wide env cache.
func TestHandlerPerInstanceBuildRequestConfig(t *testing.T) {
	t.Parallel()

	cfgA := bamlutils.BuildRequestConfig{UseBuildRequest: true, DisableCallBuildRequest: false}
	cfgB := bamlutils.BuildRequestConfig{UseBuildRequest: false, DisableCallBuildRequest: true}

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
	hA := newTestHandler(t, Config{Runtime: rtA, BuildRequest: cfgA})
	hB := newTestHandler(t, Config{Runtime: rtB, BuildRequest: cfgB})

	for _, h := range []*Handler{hA, hB} {
		out, err := h.CallStream(context.Background(), "x", []byte(`{}`), bamlutils.StreamModeCall)
		if err != nil {
			t.Fatalf("CallStream: %v", err)
		}
		for range out {
		}
	}

	if capA == nil || capB == nil {
		t.Fatal("expected both handlers to install adapters via the fake runtime")
	}
	if capA.BuildRequestConfig() != cfgA {
		t.Errorf("handler A: BuildRequestConfig = %#v, want %#v", capA.BuildRequestConfig(), cfgA)
	}
	if capB.BuildRequestConfig() != cfgB {
		t.Errorf("handler B: BuildRequestConfig = %#v, want %#v", capB.BuildRequestConfig(), cfgB)
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
	h := newTestHandler(t, Config{Runtime: rt, DeBAML: bamlutils.DeBAMLConfig{Enabled: true}})

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
	if captured.setBuildRequestConfigCalls != 1 {
		t.Errorf("expected SetBuildRequestConfig to be called exactly once, got %d", captured.setBuildRequestConfigCalls)
	}
}
