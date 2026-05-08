package common

import (
	"embed"
)

//go:embed adapterversions codegen/codegen.go codegen/codegen_adapter.go codegen/codegen_buildrequest.go codegen/codegen_dynamic_types.go codegen/codegen_legacy_stream.go codegen/codegen_methods.go codegen/codegen_options.go codegen/codegen_reflect.go codegen/codegen_router.go codegen/codegen_stream_helpers.go embed.go go.mod go.sum helpers.go testdriver testhelpers/snapshot.go utils/dynamic.go
var source embed.FS

var Sources = make(map[string]embed.FS)

func init() {
	Sources["."] = source
}
