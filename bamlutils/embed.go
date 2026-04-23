package bamlutils

import (
	"embed"
)

//go:embed adapters.go buildrequest/call_orchestrator.go buildrequest/call_support_debug.go buildrequest/call_support_stub.go buildrequest/legacy_log.go buildrequest/orchestrator.go buildrequest/response_extract.go buildrequest/strategy_testhelper.go clientdefaults/clientdefaults.go clientdefaults/handlers.go dynamic.go embed.go go.mod go.sum interfaces.go legacy_outcome.go llmhttp/cache.go llmhttp/fast_backend.go llmhttp/llmhttp.go media.go pool.go retry/retry.go sse/extract.go sseclient/sseclient.go urlrewrite/urlrewrite.go versions.go
var source embed.FS

var Sources = make(map[string]embed.FS)

func init() {
	Sources["."] = source
}
