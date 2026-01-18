package workerplugin

import (
	"embed"
)

//go:embed embed.go go.mod go.sum grpc.go plugin.go proto
var source embed.FS

var Sources = make(map[string]embed.FS)

func init() {
	Sources["."] = source
}
