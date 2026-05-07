package main

import (
	"github.com/invakid404/baml-rest/adapters/common/adapterversions"
	"github.com/invakid404/baml-rest/adapters/common/codegen"
)

const selfPkg = "github.com/invakid404/baml-rest/adapters/adapter_v0_215_0"

func main() {
	// BAML v0.215.0 predates the WithClient CallOption (introduced in
	// v0.219.0) but already handles nested maps in CFFI, so neither
	// SupportsWithClient nor HasWrapMapValues fires. The shared
	// adapterversions inventory is the source of truth for this
	// matrix.
	opts := adapterversions.MustOptionsForSelfPkg(selfPkg)
	codegen.GenerateWithOptions(opts)
	codegen.GenerateFrameworkAdapter(opts, "")
}
