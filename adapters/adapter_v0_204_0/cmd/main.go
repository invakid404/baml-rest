package main

import (
	"github.com/invakid404/baml-rest/adapters/common/codegen"
)

const selfPkg = "github.com/invakid404/baml-rest/adapters/adapter_v0_204_0"

func main() {
	// BAML v0.204.0 predates the WithClient CallOption (introduced in
	// v0.219.0). Emitting WithClient references here would produce
	// "undefined: baml_client.WithClient" at adapter compile time.
	// HasWrapMapValues=true: v0.204's older CFFI shape requires the
	// adapter package's WrapMapValues helper to wrap nested options
	// before forwarding into BAML's AddLlmClient.
	opts := codegen.Options{
		SelfPkg:            selfPkg,
		SupportsWithClient: false,
		HasWrapMapValues:   true,
		HasHTTPClient:      false,
	}
	codegen.GenerateWithOptions(opts)
	codegen.GenerateFrameworkAdapter(opts, "")
}
