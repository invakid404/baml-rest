package bamlutils

import (
	"embed"
)

//go:embed adapters.go buildrequest/call_orchestrator.go buildrequest/call_support_debug.go buildrequest/call_support_parse.go buildrequest/call_support_stub.go buildrequest/client_resolution.go buildrequest/fallback_resolve.go buildrequest/legacy_child_registry.go buildrequest/legacy_log.go buildrequest/metadata.go buildrequest/orchestrator.go buildrequest/response_extract.go buildrequest/retry_policy.go buildrequest/roundrobin/advancer.go buildrequest/roundrobin/coordinator.go buildrequest/roundrobin/resolver.go buildrequest/strategy_testhelper.go clientdefaults/clientdefaults.go clientdefaults/handlers.go dynamic.go embed.go go.mod go.sum interfaces.go legacy_outcome.go llmhttp/cache.go llmhttp/errors.go llmhttp/fast_backend.go llmhttp/llmhttp.go media.go pool.go retry/retry.go sse/extract.go sseclient/sseclient.go strategyparse/parser.go urlrewrite/urlrewrite.go versions.go
var source embed.FS

var Sources = make(map[string]embed.FS)

func init() {
	Sources["."] = source
}
