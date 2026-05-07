package main

import (
	"github.com/invakid404/baml-rest/adapters/common/codegen"
)

const selfPkg = "github.com/invakid404/baml-rest/adapters/adapter_v0_219_0"

func main() {
	// BAML v0.219.0 introduced the WithClient CallOption, so the
	// legacy dispatcher can emit per-attempt client overrides.
	// HasHTTPClient=true: v0.219 exposes the BuildRequest path's
	// per-request HTTP client override via SetHTTPClient.
	opts := codegen.Options{
		SelfPkg:            selfPkg,
		SupportsWithClient: true,
		HasWrapMapValues:   false,
		HasHTTPClient:      true,
	}
	codegen.GenerateWithOptions(opts)
	codegen.GenerateFrameworkAdapter(opts, "")
}
