package main

import (
	"github.com/invakid404/baml-rest/adapters/common/codegen"
)

const selfPkg = "github.com/invakid404/baml-rest/adapters/adapter_v0_204_0"

func main() {
	// BAML v0.204.0 predates the WithClient CallOption (introduced in
	// v0.219.0). Emitting WithClient references here would produce
	// "undefined: baml_client.WithClient" at adapter compile time.
	codegen.GenerateWithOptions(codegen.Options{
		SelfPkg:            selfPkg,
		SupportsWithClient: false,
	})
}
