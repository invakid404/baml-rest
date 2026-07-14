package bamlutils

import (
	"embed"
)

//go:embed adapters.go awsstream/awsstream.go bamlparser/ast.go bamlparser/attributes.go bamlparser/bamlparser.go bamlparser/typeast.go bamlparser/typeparse.go bamlparser/value.go buildrequest/bedrock_stream_extract.go buildrequest/call_orchestrator.go buildrequest/call_support_debug.go buildrequest/call_support_parse.go buildrequest/call_support_stub.go buildrequest/client_resolution.go buildrequest/errors.go buildrequest/fallback_resolve.go buildrequest/legacy_child_registry.go buildrequest/legacy_log.go buildrequest/metadata.go buildrequest/native_callback.go buildrequest/orchestrator.go buildrequest/raw_error.go buildrequest/response_extract.go buildrequest/response_extract_borrowed.go buildrequest/retry_policy.go buildrequest/roundrobin/advancer.go buildrequest/roundrobin/coordinator.go buildrequest/roundrobin/resolver.go buildrequest/strategy_testhelper.go clientdefaults/clientdefaults.go clientdefaults/handlers.go dynamic.go dynamic_absent.go dynamic_order.go dynamic_render_order.go embed.go go.mod go.sum interfaces.go legacy_outcome.go llmhttp/awssigv4.go llmhttp/awsstream_exec.go llmhttp/borrow_guard.go llmhttp/borrow_guard_debug.go llmhttp/cache.go llmhttp/errors.go llmhttp/exact.go llmhttp/fast_backend.go llmhttp/idle_timeout.go llmhttp/llmhttp.go media.go native_shadow.go orderedmap.go pool.go promptdescriptor retry/retry.go schemadescriptor sse/extract.go sseclient/sseclient.go strategyparse/parser.go urlrewrite/urlrewrite.go versions.go
var source embed.FS

var Sources = make(map[string]embed.FS)

func init() {
	Sources["."] = source
}
