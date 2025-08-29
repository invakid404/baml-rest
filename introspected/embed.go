package introspected

import (
    "embed"
)

//go:embed embed.go go.mod introspected.go
var source embed.FS

var Sources = make(map[string]embed.FS)

func init() {
    Sources["."] = source
}
