package nativespine_test

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/invakid404/baml-rest/adapters/common/codegen"
	"github.com/invakid404/baml-rest/bamlutils/projectdescriptor"
	"github.com/invakid404/baml-rest/internal/nativespine"
)

// updateJSONAliasGolden regenerates the committed ExecBridge-U1 JSON-alias carrier
// fixture from the emitter. Run:
//
//	go test ./internal/nativespine/ -run TestJSONAliasCodegenGolden -update-native-spine-goldens -count=1
//
// (shares the -update-native-spine-goldens flag defined in codegen_golden_test.go).

const jsonAliasFixturePackageName = "nativespinejsonfixture"

func jsonAliasFixturePath(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "nativespinejsonfixture", "generated_json_alias.go"))
}

// admittedJSONAliasMethod builds the JSON-alias corpus and returns the single
// admitted StaticRecursiveAliasJSON method.
func admittedJSONAliasMethod(t *testing.T) projectdescriptor.Method {
	t.Helper()
	p, err := nativespine.BuildFromSource(nativespine.JSONAliasFixtureSources)
	if err != nil {
		t.Fatalf("BuildFromSource: %v", err)
	}
	for _, m := range p.Methods {
		if m.Name == "StaticRecursiveAliasJSON" {
			return m
		}
	}
	t.Fatalf("StaticRecursiveAliasJSON not admitted (methods=%d diagnostics=%d)", len(p.Methods), len(p.Diagnostics))
	return projectdescriptor.Method{}
}

// TestJSONAliasCodegenGolden proves the ExecBridge-U1 JSON-alias carrier fixture is
// exactly what the emitter produces from the neutral descriptor. With -update it
// regenerates the committed file.
func TestJSONAliasCodegenGolden(t *testing.T) {
	m := admittedJSONAliasMethod(t)

	src, err := codegen.EmitNativeStaticUnary(m, codegen.NativeSpineOptions{PackageName: jsonAliasFixturePackageName})
	if err != nil {
		t.Fatalf("EmitNativeStaticUnary: %v", err)
	}

	path := jsonAliasFixturePath(t)
	if *updateGoldens {
		if err := os.WriteFile(path, src, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("regenerated %s (%d bytes)", path, len(src))
		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read committed golden: %v", err)
	}
	if string(want) != string(src) {
		t.Fatalf("committed %s is stale — re-run with -update-native-spine-goldens.", path)
	}
}
