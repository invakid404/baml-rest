package nativespine_test

import (
	"flag"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/invakid404/baml-rest/adapters/common/codegen"
	"github.com/invakid404/baml-rest/bamlutils/projectdescriptor"
	"github.com/invakid404/baml-rest/internal/nativespine"
)

// updateGoldens regenerates the committed generated fixture file from the
// emitter. Mirrors the repo's "-update-*-goldens" convention. Run:
//
//	go test ./internal/nativespine/ -run TestNativeSpineCodegenGolden -update-native-spine-goldens -count=1
var updateGoldens = flag.Bool("update-native-spine-goldens", false,
	"regenerate internal/nativespinefixture/generated_greeting.go from the emitter")

const fixturePackageName = "nativespinefixture"

// generatedFixturePath returns the absolute path of the committed generated
// fixture file, computed from this test file's location.
func generatedFixturePath(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// <root>/internal/nativespine/codegen_golden_test.go -> <root>/internal/nativespinefixture/generated_greeting.go
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "nativespinefixture", "generated_greeting.go"))
}

// admittedMethod builds the descriptor from the shared fixture and returns the
// single admitted ClassStaticUnary method.
func admittedMethod(t *testing.T) projectdescriptor.Method {
	t.Helper()
	p, err := nativespine.BuildFromSource(nativespine.M1FixtureSources)
	if err != nil {
		t.Fatalf("BuildFromSource: %v", err)
	}
	var admitted []projectdescriptor.Method
	for _, m := range p.Methods {
		if m.Class == projectdescriptor.ClassStaticUnary {
			admitted = append(admitted, m)
		}
	}
	if len(admitted) != 1 {
		t.Fatalf("want exactly 1 admitted static-unary method, got %d", len(admitted))
	}
	return admitted[0]
}

// TestNativeSpineCodegenGolden proves the emitter is deterministic and that the
// committed generated fixture file is exactly what the emitter produces from the
// neutral descriptor. With -update it regenerates the committed file.
func TestNativeSpineCodegenGolden(t *testing.T) {
	m := admittedMethod(t)

	src, err := codegen.EmitNativeStaticUnary(m, codegen.NativeSpineOptions{PackageName: fixturePackageName})
	if err != nil {
		t.Fatalf("EmitNativeStaticUnary: %v", err)
	}

	// Determinism: a second emission from the same descriptor is byte-identical.
	src2, err := codegen.EmitNativeStaticUnary(m, codegen.NativeSpineOptions{PackageName: fixturePackageName})
	if err != nil {
		t.Fatalf("EmitNativeStaticUnary (2nd): %v", err)
	}
	if string(src) != string(src2) {
		t.Fatal("emitter is not deterministic: two emissions differ")
	}

	path := generatedFixturePath(t)
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
		t.Fatalf("committed %s is stale — re-run with -update-native-spine-goldens.\n"+
			"The generated file must equal EmitNativeStaticUnary(descriptor).", path)
	}
}
