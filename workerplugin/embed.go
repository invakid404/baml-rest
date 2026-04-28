package workerplugin

import (
	"embed"
)

//go:embed embed.go go.mod go.sum grpc.go multiconn.go plugin.go proto remote_advancer.go requestid.go sharedstate.go
var source embed.FS

var Sources = make(map[string]embed.FS)

func init() {
	Sources["."] = source
}
