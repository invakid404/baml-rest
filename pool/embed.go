package pool

import (
	"embed"
)

//go:embed config_inprocess.go config_subprocess.go embed.go go.mod go.sum hclogzerolog.go native_stacks_inprocess.go native_stacks_subprocess.go pool.go worker_start_common.go worker_start_inprocess.go worker_start_subprocess.go
var source embed.FS

var Sources = make(map[string]embed.FS)

func init() {
	Sources["."] = source
}
