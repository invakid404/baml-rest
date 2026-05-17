package generated

import (
	"context"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/internal/worker"
)

// Compile-time assertion: Runtime satisfies the worker.Runtime
// contract dynclient consumers wire through worker.New. Without this
// the package would still build (the dispatch surface is reflective)
// but a contract drift in internal/worker would only surface inside
// the dynclient consumer, after the public package has shipped.
var _ worker.Runtime = (*Runtime)(nil)

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
// trips the per-handler BuildRequestConfig knob. Confirms the codegen-
// emitted MakeAdapter wired the setter/getter the worker pool relies
// on, without touching any BAML runtime that requires the native
// library.
func TestRuntimeMakeAdapter(t *testing.T) {
	rt := Runtime{}
	adapter := rt.MakeAdapter(context.Background())
	if adapter == nil {
		t.Fatal("Runtime.MakeAdapter returned nil")
	}

	want := bamlutils.BuildRequestConfig{
		UseBuildRequest:         true,
		DisableCallBuildRequest: true,
	}
	adapter.SetBuildRequestConfig(want)
	got := adapter.BuildRequestConfig()
	if got != want {
		t.Fatalf("BuildRequestConfig round-trip mismatch: got %+v, want %+v", got, want)
	}
}
