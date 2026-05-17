// Package generated is the dynamic-only BAML adapter. Most of this
// package's source files are emitted by cmd/regenerate-dynclient — the
// BAML client under baml_client/, the introspected/ reflection bundle,
// the framework adapter under adapter/, and the top-level adapter.go.
//
// This file is hand-written. It exposes Runtime, the zero-state
// implementation of internal/worker.Runtime that bridges between the
// worker dispatch loop and the generated dispatcher above. The dynclient
// regen pipeline must not overwrite it.
package generated

import (
	"context"

	"github.com/invakid404/baml-rest/bamlutils"
)

// Runtime is the worker-facing entry point. State lives in the
// generated package globals (Methods, ParseMethods, MakeAdapter,
// InitBamlRuntime); Runtime just forwards calls.
type Runtime struct{}

// InitRuntime loads the BAML shared library exactly once via the
// upstream sync.Once wired in by cmd/hacks/hacks/lazy_runtime.go.
func (Runtime) InitRuntime() {
	InitBamlRuntime()
}

// Method looks up a streaming BAML method by name. Preserves the
// (value, ok) shape the worker handler relies on for the
// "method %q not found" error path.
func (Runtime) Method(name string) (bamlutils.StreamingMethod, bool) {
	method, ok := Methods[name]
	return method, ok
}

// ParseMethod looks up a parse-only BAML method by name. Same
// (value, ok) contract as Method.
func (Runtime) ParseMethod(name string) (bamlutils.ParseMethod, bool) {
	method, ok := ParseMethods[name]
	return method, ok
}

// MakeAdapter delegates to the generated MakeAdapter — the framework
// adapter overrides this at codegen time, so the indirection lets
// callers swap in alternate runtimes without recompiling the worker.
func (Runtime) MakeAdapter(ctx context.Context) bamlutils.Adapter {
	return MakeAdapter(ctx)
}
