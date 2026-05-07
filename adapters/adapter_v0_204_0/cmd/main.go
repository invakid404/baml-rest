package main

import (
	"github.com/invakid404/baml-rest/adapters/common/adapterversions"
	"github.com/invakid404/baml-rest/adapters/common/codegen"
)

const selfPkg = "github.com/invakid404/baml-rest/adapters/adapter_v0_204_0"

func main() {
	// BAML v0.204.0 predates the WithClient CallOption (introduced in
	// v0.219.0) and uses the older CFFI shape that requires the
	// adapter package's WrapMapValues helper. Both flags live in the
	// shared adapterversions inventory; main.go fetches its own
	// options via SelfPkg lookup so flag-value drift is impossible.
	opts := adapterversions.MustOptionsForSelfPkg(selfPkg)
	codegen.GenerateWithOptions(opts)
	codegen.GenerateFrameworkAdapter(opts, "")
}
