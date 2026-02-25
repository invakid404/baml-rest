package common

import (
	"embed"
)

//go:embed codegen/codegen.go embed.go go.mod go.sum helpers.go
var source embed.FS

var Sources = make(map[string]embed.FS)

func init() {
	Sources["."] = source
}
