package baml_rest

import (
	"embed"
	"fmt"
	"path/filepath"

	"github.com/invakid404/baml-rest/adapters/adapter_v0_204_0"
	"github.com/invakid404/baml-rest/adapters/adapter_v0_215_0"
	"github.com/invakid404/baml-rest/adapters/adapter_v0_219_0"
	"github.com/invakid404/baml-rest/adapters/common"
	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/introspected"
	"github.com/invakid404/baml-rest/pool"
	"github.com/invakid404/baml-rest/worker"
	"github.com/invakid404/baml-rest/workerplugin"
)

//go:embed LICENSE NOTICE RELEASE.md adapter.go adapters cmd/build cmd/embed cmd/hacks/hacks/astutil.go cmd/hacks/hacks/context_fix.go cmd/hacks/hacks/dynamic_order_client.go cmd/hacks/hacks/dynamic_order_fix.go cmd/hacks/hacks/generated_client_rewrite.go cmd/hacks/hacks/hack.go cmd/hacks/hacks/lazy_runtime.go cmd/hacks/hacks/patched_module.go cmd/hacks/hacks/patches cmd/hacks/hacks/runtime_deadlock_fix.go cmd/hacks/hacks/serde_nil_fix.go cmd/hacks/hacks/unused_decode_var_fix.go cmd/hacks/main.go cmd/introspect/main.go cmd/regenerate-dynclient cmd/schema/main.go cmd/serve/config.go cmd/serve/debug.go cmd/serve/debug_stub.go cmd/serve/error.go cmd/serve/headers.go cmd/serve/main.go cmd/serve/openapi.json cmd/serve/streamwriter.go cmd/serve/unary.go cmd/serve/unary_handlers.go cmd/serve/unary_stub.go cmd/serve/worker cmd/serve/worker_mode.go cmd/serve/worker_mode_inprocess.go cmd/serve/worker_mode_subprocess.go cmd/verify-adapter-pins cmd/verify-framework-adapter/main.go cmd/worker embed.go go.mod go.sum go.work go.work.sum internal/apierror/error.go internal/debaml/coerce.go internal/debaml/extract.go internal/debaml/fix.go internal/debaml/parse.go internal/debaml/render.go internal/debaml/value.go internal/httplogger internal/memlimit/memlimit.go internal/nativeprompt/doc.go internal/nativeprompt/env.go internal/nativeprompt/input.go internal/nativeprompt/lower.go internal/nativeprompt/render.go internal/nativeprompt/rendered.go internal/nativeprompt/static_render.go internal/nativeprompt/static_support.go internal/nativeprompt/support.go internal/nativeprompt/template.go internal/nativeschema/build.go internal/nativeschema/prompt.go internal/nativeschema/recursion.go internal/rootruntime internal/schema/build.go internal/schema/index.go internal/schema/order.go internal/schema/outputformat/options.go internal/schema/outputformat/render.go internal/schema/outputformat/union.go internal/schema/schema.go internal/schema/static_descriptor.go internal/schema/testdata internal/schema/validate.go internal/unsafeutil renovate.json scripts
var source embed.FS

var Sources = make(map[string]embed.FS)

func init() {
	Sources["."] = source
	for key, value := range adapter_v0_204_0.Sources {
		path := filepath.Clean(fmt.Sprintf("./%s/%s", "adapters/adapter_v0_204_0", key))
		Sources[path] = value
	}
	for key, value := range adapter_v0_215_0.Sources {
		path := filepath.Clean(fmt.Sprintf("./%s/%s", "adapters/adapter_v0_215_0", key))
		Sources[path] = value
	}
	for key, value := range adapter_v0_219_0.Sources {
		path := filepath.Clean(fmt.Sprintf("./%s/%s", "adapters/adapter_v0_219_0", key))
		Sources[path] = value
	}
	for key, value := range common.Sources {
		path := filepath.Clean(fmt.Sprintf("./%s/%s", "adapters/common", key))
		Sources[path] = value
	}
	for key, value := range bamlutils.Sources {
		path := filepath.Clean(fmt.Sprintf("./%s/%s", "bamlutils", key))
		Sources[path] = value
	}
	for key, value := range introspected.Sources {
		path := filepath.Clean(fmt.Sprintf("./%s/%s", "introspected", key))
		Sources[path] = value
	}
	for key, value := range pool.Sources {
		path := filepath.Clean(fmt.Sprintf("./%s/%s", "pool", key))
		Sources[path] = value
	}
	for key, value := range worker.Sources {
		path := filepath.Clean(fmt.Sprintf("./%s/%s", "worker", key))
		Sources[path] = value
	}
	for key, value := range workerplugin.Sources {
		path := filepath.Clean(fmt.Sprintf("./%s/%s", "workerplugin", key))
		Sources[path] = value
	}
}
