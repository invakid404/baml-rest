package worker

import (
	baml_rest "github.com/invakid404/baml-rest"
)

// InitRuntime initializes the BAML runtime by loading the shared library.
// Thin wrapper over the generated baml_rest.InitBamlRuntime so callers in
// cmd/worker (subprocess binary) and the inprocess server wiring in
// cmd/serve hit the same entry point. Callers decide when to invoke it —
// New does not call it implicitly so startup ordering stays explicit.
func InitRuntime() {
	baml_rest.InitBamlRuntime()
}
