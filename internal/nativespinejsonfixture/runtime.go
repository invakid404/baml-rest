package nativespinejsonfixture

import (
	"context"
	"reflect"

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
	if executorIsNil(exec) {
		// A nil executor would defer the failure to the emitted Impl goroutine (Call/Parse
		// on a nil receiver panics and kills the process); reject it here, at construction,
		// so a misbuilt runtime fails loudly and locally. This catches BOTH an untyped nil
		// interface AND a TYPED-NIL (e.g. a (*spine.UnaryExecutor)(nil) boxed in the
		// interface — non-nil interface, nil underlying pointer), which a plain `exec == nil`
		// misses (CodeRabbit #3 + follow-on).
		panic("nativespinejsonfixture: NewNativeRuntime requires a non-nil executor")
	}
	sm, pm := BuildMethod(exec)
	return &NativeRuntime{
		methods:      map[string]bamlutils.StreamingMethod{MethodName: sm},
		parseMethods: map[string]bamlutils.ParseMethod{MethodName: pm},
	}
}

// executorIsNil reports whether exec is an untyped nil interface OR a typed-nil of a
// nilable dynamic kind (pointer/func/map/chan/slice/interface) — either of which would
// panic on the first method dispatch. It is the guard NewNativeRuntime uses so a
// (*UnaryExecutor)(nil) is rejected at construction, not at dispatch.
func executorIsNil(exec bamlutils.NativeSpineUnaryExecutor) bool {
	if exec == nil {
		return true
	}
	v := reflect.ValueOf(exec)
	switch v.Kind() {
	case reflect.Ptr, reflect.Func, reflect.Map, reflect.Chan, reflect.Slice, reflect.Interface, reflect.UnsafePointer:
		return v.IsNil()
	default:
		return false
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
