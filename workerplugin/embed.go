package workerplugin

import (
	"embed"
)

//go:embed embed.go go.mod go.sum grpc.go multiconn.go plugin.go proto requestid.go sharedstate.go
var source embed.FS

var Sources = make(map[string]embed.FS)

func init() {
	Sources["."] = source
}
