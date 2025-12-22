package main

import (
	"github.com/invakid404/baml-rest/adapters/common/codegen"
)

const selfPkg = "github.com/invakid404/baml-rest/adapters/adapter_v0_215_0"

func main() {
	codegen.Generate(selfPkg)
}
