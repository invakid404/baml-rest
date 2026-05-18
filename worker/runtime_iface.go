package worker

import (
	"context"

	"github.com/invakid404/baml-rest/bamlutils"
)

// Runtime is the small surface of the generated baml_rest package the
// handler depends on for dispatch. Decoupling these calls from a direct
// root-package import lets cmd/worker and the in-process cmd/serve build
// supply the default rootruntime wrapper while leaving room for
// alternative providers to be plugged in by other entry points.
//
// Lookup methods preserve the `(value, ok)` shape of the underlying
// map indexes so the handler's existing "method %q not found" /
// "parse method %q not found" error contracts survive verbatim.
//
// InitRuntime stays on the interface even though BAML's runtime is
// process-global; keeping it here means every runtime touch point in
// the handler stack flows through one abstraction. Callers invoke it
// explicitly at process startup — worker.New does NOT call it
// implicitly so startup ordering remains observable in the binaries.
type Runtime interface {
	InitRuntime()
	Method(name string) (bamlutils.StreamingMethod, bool)
	ParseMethod(name string) (bamlutils.ParseMethod, bool)
	MakeAdapter(ctx context.Context) bamlutils.Adapter
}
