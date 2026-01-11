package pool

import (
	"embed"
)

//go:embed embed.go go.mod go.sum hclogzerolog.go pool.go
var source embed.FS

var Sources = make(map[string]embed.FS)

func init() {
	Sources["."] = source
}
