package v0_204_0

import (
    "embed"
)

//go:embed cmd embed.go go.mod go.sum
var source embed.FS

var Sources = make(map[string]embed.FS)

func init() {
    Sources["."] = source
}
