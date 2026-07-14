package worker

import (
	"embed"
)

//go:embed admin.go defaults.go embed.go errors.go go.mod go.sum handler.go metrics.go native_capability.go options.go parse.go runtime_iface.go sharedstate.go stream.go stream_recover_inprocess.go stream_recover_subprocess.go
var source embed.FS

var Sources = make(map[string]embed.FS)

func init() {
	Sources["."] = source
}
