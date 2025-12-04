package bamlutils

import (
	"embed"
)

//go:embed embed.go go.mod go.sum interfaces.go versions.go
var source embed.FS

var Sources = make(map[string]embed.FS)

func init() {
	Sources["."] = source
}
