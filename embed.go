package baml_rest

import (
    "embed"
    "fmt"
    "path/filepath"
    "github.com/invakid404/baml-rest/bamlutils"
)

//go:embed adapter.go cmd embed.go go.mod go.sum go.work go.work.sum introspected.go
var source embed.FS

var Sources = make(map[string]embed.FS)

func init() {
    Sources["."] = source
    for key, value := range bamlutils.Sources {
        path := filepath.Clean(fmt.Sprintf("./%s/%s", "bamlutils", key))
        Sources[path] = value
    }
}
