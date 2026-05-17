package baml_patched

import (
	"embed"
)

//go:embed BAML_REST_SOURCE_VERSION LICENSE PATCHES.md embed.go engine/language_client_go/README_WINDOWS.md engine/language_client_go/baml_go/baml_cffi_wrapper.c engine/language_client_go/baml_go/baml_cffi_wrapper.h engine/language_client_go/baml_go/exports.go engine/language_client_go/baml_go/lib_common.go engine/language_client_go/baml_go/lib_unix.go engine/language_client_go/baml_go/lib_windows.go engine/language_client_go/baml_go/raw_objects engine/language_client_go/baml_go/serde engine/language_client_go/baml_go/shared engine/language_client_go/generate_checksums.sh engine/language_client_go/package.json engine/language_client_go/pkg/callbacks.go engine/language_client_go/pkg/cffi engine/language_client_go/pkg/lib.go engine/language_client_go/pkg/rawobjects_class_builder.go engine/language_client_go/pkg/rawobjects_class_property_builder.go engine/language_client_go/pkg/rawobjects_client_registry.go engine/language_client_go/pkg/rawobjects_collector.go engine/language_client_go/pkg/rawobjects_constructors.go engine/language_client_go/pkg/rawobjects_enum_builder.go engine/language_client_go/pkg/rawobjects_enum_value_builder.go engine/language_client_go/pkg/rawobjects_function_args.go engine/language_client_go/pkg/rawobjects_function_log.go engine/language_client_go/pkg/rawobjects_http_body.go engine/language_client_go/pkg/rawobjects_http_request.go engine/language_client_go/pkg/rawobjects_http_response.go engine/language_client_go/pkg/rawobjects_llm_call.go engine/language_client_go/pkg/rawobjects_llm_stream_call.go engine/language_client_go/pkg/rawobjects_llmrenderable.go engine/language_client_go/pkg/rawobjects_media.go engine/language_client_go/pkg/rawobjects_public.go engine/language_client_go/pkg/rawobjects_sse_response.go engine/language_client_go/pkg/rawobjects_stream_timing.go engine/language_client_go/pkg/rawobjects_tick_callback.go engine/language_client_go/pkg/rawobjects_timing.go engine/language_client_go/pkg/rawobjects_type_builder.go engine/language_client_go/pkg/rawobjects_type_def.go engine/language_client_go/pkg/rawobjects_usage.go engine/language_client_go/pkg/rawobjects_utils.go engine/language_client_go/pkg/runtime.go engine/language_client_go/pkg/stream_result.go go.mod go.sum
var source embed.FS

var Sources = make(map[string]embed.FS)

func init() {
	Sources["."] = source
}
