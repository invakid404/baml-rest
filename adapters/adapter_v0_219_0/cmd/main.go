package main

import (
	"github.com/invakid404/baml-rest/adapters/common/codegen"
)

const selfPkg = "github.com/invakid404/baml-rest/adapters/adapter_v0_219_0"

func main() {
	// BAML v0.219.0 introduced the WithClient CallOption, so the
	// legacy dispatcher can emit per-attempt client overrides.
	codegen.GenerateWithOptions(codegen.Options{
		SelfPkg:            selfPkg,
		SupportsWithClient: true,
	})
}
