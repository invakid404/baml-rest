package nativespinejsonfixture

import (
	"context"

	"github.com/invakid404/baml-rest/bamlutils"
)

// NativeRuntime is the pure-Go native runtime for the ExecBridge-U1 JSON-alias
// carrier: it registers the emitted StreamingMethod/ParseMethod for MethodName
// (built by BuildMethod from an injected neutral executor) and satisfies the
// worker.Runtime contract (InitRuntime / Method / ParseMethod / MakeAdapter) without
// importing worker in non-test code and without linking any generated BAML client or
// CFFI symbol.
//
// Production rootruntime selection is unchanged (default-deny). The real-population
// integration test constructs this runtime with the production spine executor.
type NativeRuntime struct {
	methods      map[string]bamlutils.StreamingMethod
	parseMethods map[string]bamlutils.ParseMethod
}

// NewNativeRuntime builds the runtime from an injected neutral executor (the
// production spine executor in the integration test; the runtime never makes a BAML
// request or parse call).
func NewNativeRuntime(exec bamlutils.NativeSpineUnaryExecutor) *NativeRuntime {
	sm, pm := BuildMethod(exec)
	return &NativeRuntime{
		methods:      map[string]bamlutils.StreamingMethod{MethodName: sm},
		parseMethods: map[string]bamlutils.ParseMethod{MethodName: pm},
	}
}

// InitRuntime is the pure-Go validation/no-op init: it loads no shared library and
// reads no environment, and validates the registry is non-empty so a misbuilt
// runtime fails loudly at boot.
func (r *NativeRuntime) InitRuntime() {
	if len(r.methods) == 0 || len(r.parseMethods) == 0 {
		panic("nativespinejsonfixture: NativeRuntime has an empty method registry")
	}
}

// Method returns the StreamingMethod for name, preserving the (value, ok) shape the
// worker handler's "method %q not found" contract depends on.
func (r *NativeRuntime) Method(name string) (bamlutils.StreamingMethod, bool) {
	m, ok := r.methods[name]
	return m, ok
}

// ParseMethod returns the ParseMethod for name, preserving the (value, ok) shape the
// worker handler's "parse method %q not found" contract depends on.
func (r *NativeRuntime) ParseMethod(name string) (bamlutils.ParseMethod, bool) {
	m, ok := r.parseMethods[name]
	return m, ok
}

// MakeAdapter returns a pure-Go adapter carrying the request context. It links no
// CFFI — that is the whole point of the fixture.
func (r *NativeRuntime) MakeAdapter(ctx context.Context) bamlutils.Adapter {
	return newFixtureAdapter(ctx)
}
