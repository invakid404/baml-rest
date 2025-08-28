package baml_rest

import "embed"

//go:embed baml cmd embed.go go.mod go.sum introspected.go
var Source embed.FS