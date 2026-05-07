package main

import (
	"github.com/invakid404/baml-rest/adapters/common/codegen"
)

const selfPkg = "github.com/invakid404/baml-rest/adapters/adapter_v0_215_0"

func main() {
	// BAML v0.215.0 predates the WithClient CallOption (introduced in
	// v0.219.0). Emitting WithClient references here would produce
	// "undefined: baml_client.WithClient" at adapter compile time.
	// v0.215.0+ properly handles nested maps in CFFI, so
	// HasWrapMapValues stays false.
	opts := codegen.Options{
		SelfPkg:            selfPkg,
		SupportsWithClient: false,
		HasWrapMapValues:   false,
		HasHTTPClient:      false,
	}
	codegen.GenerateWithOptions(opts)
	codegen.GenerateFrameworkAdapter(opts, "")
}
