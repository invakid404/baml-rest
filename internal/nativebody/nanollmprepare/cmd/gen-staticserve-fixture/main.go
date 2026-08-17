// Command gen-staticserve-fixture emits the checked-in generated static-serve
// FIXTURE adapters (de-BAML Slice 8C) from their ctx-first baml_client +
// introspected packages. It is the static twin of dynclient/cmd/genadapter: it
// drives adapters/common/codegen over a compilable static project so the generated
// static /call serve seam can be exercised end-to-end.
//
// It emits TWO kinds of fixture, and the second is why this command has a table
// instead of a pair of constants:
//
//   - the MAIN fixture (internal/nativeprompt/testdata/staticserve_fixture) — a copy
//     of internal/nativeprompt/testdata/static_oracle with the ctx-first client hacks
//     applied, carrying the whole static corpus including the `this > 0` constraint
//     family and its declined siblings;
//   - the de-BAML Slice 7.2c-3 ISOLATED OPERATOR fixtures
//     (internal/nativeprompt/testdata/staticserve_op_fixtures/<op>) — one project per
//     newly admitted direct comparison, each declaring the two PRODUCTION-PINNED class
//     names `StaticCheckedAnswer` / `StaticAssertAnswer` exactly ONCE with its own
//     predicate.
//
// The isolated projects exist because one BAML project cannot declare a class twice
// and the 7.2c scope forbids renaming the classes to make six predicate variants
// coexist. Each also gets its own loopback PORT, so its generated client is
// addressable independently of the others.
//
// Every fixture is used ONLY by the gated de-BAML static serve e2e/cutover tests and
// never enters the production build. Run from the repo root:
//
//	cd internal/nativebody/nanollmprepare && \
//	  GOWORK=off CGO_ENABLED=1 go run ./cmd/gen-staticserve-fixture -root ../../..
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/invakid404/baml-rest/adapters/common/codegen"

	introspected "github.com/invakid404/baml-rest/internal/nativeprompt/testdata/staticserve_fixture/introspected"
	opEq "github.com/invakid404/baml-rest/internal/nativeprompt/testdata/staticserve_op_fixtures/eq/introspected"
	opGe "github.com/invakid404/baml-rest/internal/nativeprompt/testdata/staticserve_op_fixtures/ge/introspected"
	opLe "github.com/invakid404/baml-rest/internal/nativeprompt/testdata/staticserve_op_fixtures/le/introspected"
	opLt "github.com/invakid404/baml-rest/internal/nativeprompt/testdata/staticserve_op_fixtures/lt/introspected"
	opNe "github.com/invakid404/baml-rest/internal/nativeprompt/testdata/staticserve_op_fixtures/ne/introspected"
)

const (
	bamlutilsPkg = "github.com/invakid404/baml-rest/bamlutils"
	bamlPkg      = "github.com/boundaryml/baml/engine/language_client_go/pkg"

	fixtureRoot   = "internal/nativeprompt/testdata/staticserve_fixture"
	opFixtureRoot = "internal/nativeprompt/testdata/staticserve_op_fixtures"
)

// fixtureTarget is one generated static-serve fixture: where its packages live and
// which introspected artifact drives the emission.
//
// The Introspection is a VALUE rather than a package path because codegen reads Go
// data structures, not source. That is why adding a fixture means adding an import
// and a row here rather than passing a flag: a fixture the command cannot import is
// a fixture that does not compile, which is exactly when it must fail.
type fixtureTarget struct {
	// name is the identifier used in progress output and in the -only filter.
	name string
	// dir is the fixture's directory, relative to the repo root.
	dir string
	// introspection is the fixture's own introspected artifact.
	introspection codegen.Introspection
}

// fixtureTargets is every fixture this command emits, main first.
func fixtureTargets() []fixtureTarget {
	// Each row spells its Introspection out. A helper taking the ten fields would read
	// shorter but would let a field be passed in the wrong position silently — and a
	// misplaced ParseMethods/ParseStreamMethods pair produces an adapter that compiles.
	return []fixtureTarget{
		{name: "main", dir: fixtureRoot, introspection: codegen.Introspection{
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
		}},
		{name: "ge", dir: opFixtureRoot + "/ge", introspection: codegen.Introspection{
			SupportsWithClient: opGe.SupportsWithClient,
			Request:            opGe.Request,
			StreamRequest:      opGe.StreamRequest,
			StreamMethods:      opGe.StreamMethods,
			SyncMethods:        opGe.SyncMethods,
			SyncFuncs:          opGe.SyncFuncs,
			ParseMethods:       opGe.ParseMethods,
			ParseStreamMethods: opGe.ParseStreamMethods,
			ParseStreamFuncs:   opGe.ParseStreamFuncs,
			MediaParams:        opGe.MediaParams,
		}},
		{name: "lt", dir: opFixtureRoot + "/lt", introspection: codegen.Introspection{
			SupportsWithClient: opLt.SupportsWithClient,
			Request:            opLt.Request,
			StreamRequest:      opLt.StreamRequest,
			StreamMethods:      opLt.StreamMethods,
			SyncMethods:        opLt.SyncMethods,
			SyncFuncs:          opLt.SyncFuncs,
			ParseMethods:       opLt.ParseMethods,
			ParseStreamMethods: opLt.ParseStreamMethods,
			ParseStreamFuncs:   opLt.ParseStreamFuncs,
			MediaParams:        opLt.MediaParams,
		}},
		{name: "le", dir: opFixtureRoot + "/le", introspection: codegen.Introspection{
			SupportsWithClient: opLe.SupportsWithClient,
			Request:            opLe.Request,
			StreamRequest:      opLe.StreamRequest,
			StreamMethods:      opLe.StreamMethods,
			SyncMethods:        opLe.SyncMethods,
			SyncFuncs:          opLe.SyncFuncs,
			ParseMethods:       opLe.ParseMethods,
			ParseStreamMethods: opLe.ParseStreamMethods,
			ParseStreamFuncs:   opLe.ParseStreamFuncs,
			MediaParams:        opLe.MediaParams,
		}},
		{name: "eq", dir: opFixtureRoot + "/eq", introspection: codegen.Introspection{
			SupportsWithClient: opEq.SupportsWithClient,
			Request:            opEq.Request,
			StreamRequest:      opEq.StreamRequest,
			StreamMethods:      opEq.StreamMethods,
			SyncMethods:        opEq.SyncMethods,
			SyncFuncs:          opEq.SyncFuncs,
			ParseMethods:       opEq.ParseMethods,
			ParseStreamMethods: opEq.ParseStreamMethods,
			ParseStreamFuncs:   opEq.ParseStreamFuncs,
			MediaParams:        opEq.MediaParams,
		}},
		{name: "ne", dir: opFixtureRoot + "/ne", introspection: codegen.Introspection{
			SupportsWithClient: opNe.SupportsWithClient,
			Request:            opNe.Request,
			StreamRequest:      opNe.StreamRequest,
			StreamMethods:      opNe.StreamMethods,
			SyncMethods:        opNe.SyncMethods,
			SyncFuncs:          opNe.SyncFuncs,
			ParseMethods:       opNe.ParseMethods,
			ParseStreamMethods: opNe.ParseStreamMethods,
			ParseStreamFuncs:   opNe.ParseStreamFuncs,
			MediaParams:        opNe.MediaParams,
		}},
	}
}

func main() {
	root := flag.String("root", ".", "repo root the output paths are resolved against")
	only := flag.String("only", "", "emit just this fixture (main, ge, lt, le, eq, ne); empty means all")
	flag.Parse()

	emitted := 0
	for _, target := range fixtureTargets() {
		if *only != "" && *only != target.name {
			continue
		}
		selfPkg := "github.com/invakid404/baml-rest/" + target.dir + "/generated"
		adapterOut := filepath.Join(*root, target.dir, "generated", "adapter.go")
		frameworkOut := filepath.Join(*root, target.dir, "generated", "adapter", "adapter.go")
		if err := os.MkdirAll(filepath.Dir(frameworkOut), 0o755); err != nil {
			panic(err)
		}
		opts := codegen.Options{
			SelfPkg:            selfPkg,
			SupportsWithClient: true,
			HasWrapMapValues:   false,
			HasHTTPClient:      true,
			// De-BAML Slice 8C: emit the generated STATIC serve seam
			// (installNativeStaticCall + per-method DecodeNativeStaticFinal) for every
			// static method. It is emitted UNCONDITIONALLY and for every fixture —
			// codegen is schema-blind and makes no return-shape claim, which Slice
			// 7.2c-3 leaves exactly as it found it. Admission decides, in
			// nativeserve/admission.
			DeBAMLStaticServe: true,
			Packages: codegen.PackageConfig{
				OutputPkg:          selfPkg,
				OutputPkgName:      "generated",
				OutputPath:         adapterOut,
				GeneratedClientPkg: "github.com/invakid404/baml-rest/" + target.dir + "/baml_client",
				IntrospectedPkg:    "github.com/invakid404/baml-rest/" + target.dir + "/introspected",
				InterfacesPkg:      bamlutilsPkg,
				SSEPkg:             bamlutilsPkg + "/sse",
				BuildRequestPkg:    bamlutilsPkg + "/buildrequest",
				LLMHTTPPkg:         bamlutilsPkg + "/llmhttp",
				RetryPkg:           bamlutilsPkg + "/retry",
				BamlPkg:            bamlPkg,
				QueuePkg:           "github.com/enriquebris/goconcurrentqueue",
			},
			Introspection: target.introspection,
		}
		codegen.GenerateWithOptions(opts)
		codegen.GenerateFrameworkAdapter(opts, frameworkOut)
		fmt.Fprintf(os.Stderr, "gen-staticserve-fixture: emitted %s (%s)\n", target.name, target.dir)
		emitted++
	}
	if emitted == 0 {
		fmt.Fprintf(os.Stderr, "gen-staticserve-fixture: -only=%q matched no fixture\n", *only)
		os.Exit(1)
	}
}
