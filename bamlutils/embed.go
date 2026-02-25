package bamlutils

import (
	"embed"
)

//go:embed adapters.go dynamic.go embed.go go.mod go.sum interfaces.go media.go pool.go sse/extract.go versions.go
var source embed.FS

var Sources = make(map[string]embed.FS)

func init() {
	Sources["."] = source
}
