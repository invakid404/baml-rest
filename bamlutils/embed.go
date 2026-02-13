package bamlutils

import (
	"embed"
)

//go:embed adapters.go dynamic.go embed.go go.mod go.sum interfaces.go interfaces_test.go media.go pool.go sse versions.go
var source embed.FS

var Sources = make(map[string]embed.FS)

func init() {
	Sources["."] = source
}
