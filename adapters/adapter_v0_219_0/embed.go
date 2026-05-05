package adapter_v0_219_0

import (
	"embed"
)

//go:embed adapter/adapter.go cmd embed.go go.mod go.sum utils/dynamic.go
var source embed.FS

var Sources = make(map[string]embed.FS)

func init() {
	Sources["."] = source
}
