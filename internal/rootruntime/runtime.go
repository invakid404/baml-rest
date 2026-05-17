// Package rootruntime adapts the root baml_rest generated package to
// the small Runtime interface internal/worker depends on for dispatch.
//
// Lives outside internal/worker so the worker package itself never
// imports the root generated package: cmd/worker and the in-process
// cmd/serve build construct a Runtime{} here and pass it into
// worker.New, keeping the dependency direction one-way.
package rootruntime

import (
	"context"

	baml_rest "github.com/invakid404/baml-rest"
	"github.com/invakid404/baml-rest/bamlutils"
)

// Runtime is the zero-state shim around the root baml_rest package.
// All state lives in baml_rest's package globals; this type only
// forwards calls.
type Runtime struct{}

// InitRuntime loads the BAML shared library exactly once via the
// upstream sync.Once wired in cmd/hacks/hacks/lazy_runtime.go.
func (Runtime) InitRuntime() {
	baml_rest.InitBamlRuntime()
}

// Method looks up a streaming BAML method by name. The (value, ok)
// shape is preserved so the worker handler's existing
// `method %q not found` error contract is unchanged.
func (Runtime) Method(name string) (bamlutils.StreamingMethod, bool) {
	method, ok := baml_rest.Methods[name]
	return method, ok
}

// ParseMethod looks up a parse-only BAML method by name. Same
// (value, ok) contract as Method.
func (Runtime) ParseMethod(name string) (bamlutils.ParseMethod, bool) {
	method, ok := baml_rest.ParseMethods[name]
	return method, ok
}

// MakeAdapter delegates to the root package's MakeAdapter, which the
// generated framework adapter overrides at build time.
func (Runtime) MakeAdapter(ctx context.Context) bamlutils.Adapter {
	return baml_rest.MakeAdapter(ctx)
}
