// Command gen-staticserve-fixture emits the checked-in generated static-serve
// FIXTURE adapter (de-BAML Slice 8C) from the ctx-first staticserve_fixture
// baml_client + introspected. It is the static twin of dynclient/cmd/genadapter:
// it drives adapters/common/codegen over a compilable static project so the
// generated static /call serve seam can be exercised end-to-end.
//
// The fixture is a copy of internal/nativeprompt/testdata/static_oracle with the
// ctx-first client hacks applied (so Request.<Method>/Parse.<Method> are ctx-first,
// matching the generator's emission), used ONLY by the gated de-BAML static serve
// e2e/cutover tests. Run from the repo root:
//
//	cd internal/nativebody/nanollmprepare && \
//	  GOWORK=off CGO_ENABLED=1 go run ./cmd/gen-staticserve-fixture -root ../../..
package main

import (
	"flag"
	"os"
	"path/filepath"

	"github.com/invakid404/baml-rest/adapters/common/codegen"

	introspected "github.com/invakid404/baml-rest/internal/nativeprompt/testdata/staticserve_fixture/introspected"
)

const (
	selfPkg            = "github.com/invakid404/baml-rest/internal/nativeprompt/testdata/staticserve_fixture/generated"
	generatedClientPkg = "github.com/invakid404/baml-rest/internal/nativeprompt/testdata/staticserve_fixture/baml_client"
	introspectedPkg    = "github.com/invakid404/baml-rest/internal/nativeprompt/testdata/staticserve_fixture/introspected"
	bamlutilsPkg       = "github.com/invakid404/baml-rest/bamlutils"
	bamlPkg            = "github.com/boundaryml/baml/engine/language_client_go/pkg"

	adapterOutputRel          = "internal/nativeprompt/testdata/staticserve_fixture/generated/adapter.go"
	frameworkAdapterOutputRel = "internal/nativeprompt/testdata/staticserve_fixture/generated/adapter/adapter.go"
)

func main() {
	root := flag.String("root", ".", "repo root the output paths are resolved against")
	flag.Parse()

	adapterOut := filepath.Join(*root, adapterOutputRel)
	frameworkOut := filepath.Join(*root, frameworkAdapterOutputRel)
	if err := os.MkdirAll(filepath.Dir(frameworkOut), 0o755); err != nil {
		panic(err)
	}

	opts := codegen.Options{
		SelfPkg:            selfPkg,
		SupportsWithClient: true,
		HasWrapMapValues:   false,
		HasHTTPClient:      true,
		// De-BAML Slice 8C: emit the generated STATIC serve seam (installNativeStaticCall
		// + per-method DecodeNativeStaticFinal) for every static method.
		DeBAMLStaticServe: true,
		Packages: codegen.PackageConfig{
			OutputPkg:          selfPkg,
			OutputPkgName:      "generated",
			OutputPath:         adapterOut,
			GeneratedClientPkg: generatedClientPkg,
			IntrospectedPkg:    introspectedPkg,
			InterfacesPkg:      bamlutilsPkg,
			SSEPkg:             bamlutilsPkg + "/sse",
			BuildRequestPkg:    bamlutilsPkg + "/buildrequest",
			LLMHTTPPkg:         bamlutilsPkg + "/llmhttp",
			RetryPkg:           bamlutilsPkg + "/retry",
			BamlPkg:            bamlPkg,
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
	codegen.GenerateFrameworkAdapter(opts, frameworkOut)
}
