package bamlutils

import (
	"embed"
)

//go:embed adapters.go buildrequest/call_orchestrator.go buildrequest/orchestrator.go buildrequest/response_extract.go dynamic.go embed.go go.mod go.sum interfaces.go llmhttp/llmhttp.go media.go pool.go retry/retry.go sse/extract.go sseclient/sseclient.go urlrewrite/urlrewrite.go versions.go
var source embed.FS

var Sources = make(map[string]embed.FS)

func init() {
	Sources["."] = source
}
