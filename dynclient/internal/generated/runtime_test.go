package generated

import (
	"context"
	"net/http"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
)

// TestRuntimeWiring verifies the Method / ParseMethod / MakeAdapter
// seams the worker handler consumes. No live LLM call — just the
// dispatch wiring around the generated baml_client.
func TestRuntimeWiring(t *testing.T) {
	rt := Runtime{}

	const dynamicMethod = "Baml_Rest_Dynamic"

	if _, ok := rt.Method(dynamicMethod); !ok {
		t.Fatalf("Runtime.Method(%q) reported missing; generated dispatch map is empty", dynamicMethod)
	}
	if _, ok := rt.ParseMethod(dynamicMethod); !ok {
		t.Fatalf("Runtime.ParseMethod(%q) reported missing; generated parse map is empty", dynamicMethod)
	}

	if _, ok := rt.Method("nonexistent_method"); ok {
		t.Fatal("Runtime.Method returned ok for an unknown method; (value, ok) contract violated")
	}
	if _, ok := rt.ParseMethod("nonexistent_method"); ok {
		t.Fatal("Runtime.ParseMethod returned ok for an unknown method; (value, ok) contract violated")
	}
}

// TestRuntimeMakeAdapter constructs the framework adapter and round-
// trips the per-handler BuildRequestConfig and HTTPClient knobs.
// Confirms the codegen-emitted MakeAdapter wired the setters/getters
// the worker pool relies on, without touching any BAML runtime that
// requires the native library.
func TestRuntimeMakeAdapter(t *testing.T) {
	rt := Runtime{}
	adapter := rt.MakeAdapter(context.Background())
	if adapter == nil {
		t.Fatal("Runtime.MakeAdapter returned nil")
	}

	wantBR := bamlutils.BuildRequestConfig{
		DisableCallBuildRequest: true,
	}
	adapter.SetBuildRequestConfig(wantBR)
	if got := adapter.BuildRequestConfig(); got != wantBR {
		t.Fatalf("BuildRequestConfig round-trip mismatch: got %+v, want %+v", got, wantBR)
	}

	httpClient := llmhttp.NewClient(&http.Client{})
	adapter.SetHTTPClient(httpClient)
	if got := adapter.HTTPClient(); got != httpClient {
		t.Fatalf("HTTPClient round-trip mismatch: got %p, want %p", got, httpClient)
	}
}
