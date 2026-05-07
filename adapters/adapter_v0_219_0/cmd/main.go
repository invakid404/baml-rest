package main

import (
	"github.com/invakid404/baml-rest/adapters/common/adapterversions"
	"github.com/invakid404/baml-rest/adapters/common/codegen"
)

const selfPkg = "github.com/invakid404/baml-rest/adapters/adapter_v0_219_0"

func main() {
	// BAML v0.219.0 introduced the WithClient CallOption and the
	// BuildRequest path's per-request HTTP client override. The
	// shared adapterversions inventory pairs SelfPkg with the right
	// flags; main.go just looks itself up.
	opts := adapterversions.MustOptionsForSelfPkg(selfPkg)
	codegen.GenerateWithOptions(opts)
	codegen.GenerateFrameworkAdapter(opts, "")
}
