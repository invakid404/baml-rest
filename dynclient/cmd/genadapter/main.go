// Command genadapter emits the dynclient generated adapter.go and
// framework adapter/adapter.go from the freshly regenerated
// dynclient/internal/generated/introspected package. Lives under the
// dynclient subtree because Go's `internal/` visibility rule prevents
// a root-level cmd/... package from importing
// `dynclient/internal/generated/introspected`.
//
// Driven by cmd/regenerate-dynclient after the BAML codegen pipeline
// has produced baml_client, run the generated-client hacks, rewritten
// BAML imports, and emitted the introspected package. Output paths
// are repo-root-relative because the caller chdirs to the repo root
// before invocation.
package main

import (
	"github.com/invakid404/baml-rest/adapters/common/codegen"
	"github.com/invakid404/baml-rest/dynclient/internal/generated/introspected"
)

const (
	selfPkg            = "github.com/invakid404/baml-rest/dynclient/internal/generated"
	generatedClientPkg = selfPkg + "/baml_client"
	introspectedPkg    = selfPkg + "/introspected"
	bamlutilsPkg       = "github.com/invakid404/baml-rest/bamlutils"
	bamlPatchedPkg     = "github.com/invakid404/baml-rest/dynclient/internal/baml-patched/engine/language_client_go/pkg"

	adapterOutputPath          = "dynclient/internal/generated/adapter.go"
	frameworkAdapterOutputPath = "dynclient/internal/generated/adapter/adapter.go"
)

func main() {
	opts := codegen.Options{
		SelfPkg:            selfPkg,
		SupportsWithClient: true,
		HasWrapMapValues:   false,
		HasHTTPClient:      true,
		Packages: codegen.PackageConfig{
			OutputPkg:          selfPkg,
			OutputPkgName:      "generated",
			OutputPath:         adapterOutputPath,
			GeneratedClientPkg: generatedClientPkg,
			IntrospectedPkg:    introspectedPkg,
			InterfacesPkg:      bamlutilsPkg,
			SSEPkg:             bamlutilsPkg + "/sse",
			BuildRequestPkg:    bamlutilsPkg + "/buildrequest",
			LLMHTTPPkg:         bamlutilsPkg + "/llmhttp",
			RetryPkg:           bamlutilsPkg + "/retry",
			BamlPkg:            bamlPatchedPkg,
			QueuePkg:           "github.com/enriquebris/goconcurrentqueue",
		},
		Introspection: codegen.Introspection{
			SupportsWithClient: introspected.SupportsWithClient,
			Request:            introspected.Request,
			StreamRequest:      introspected.StreamRequest,
			StreamMethods:      introspected.StreamMethods,
			SyncMethods:        introspected.SyncMethods,
			SyncFuncs:          introspected.SyncFuncs,
			ParseMethods:       introspected.ParseMethods,
			ParseStreamMethods: introspected.ParseStreamMethods,
			ParseStreamFuncs:   introspected.ParseStreamFuncs,
			MediaParams:        introspected.MediaParams,
		},
	}
	codegen.GenerateWithOptions(opts)
	codegen.GenerateFrameworkAdapter(opts, frameworkAdapterOutputPath)
}
