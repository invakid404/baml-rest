package codegen

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMaybeWriteDeBAMLHelper_WritesWhenEmitted pins that the helper is
// written next to adapter.go (package + import paths parameterized) when a
// de-BAML call was emitted.
func TestMaybeWriteDeBAMLHelper_WritesWhenEmitted(t *testing.T) {
	dir := t.TempDir()
	g := &generator{
		pkgs: PackageConfig{
			OutputPath:         filepath.Join(dir, "adapter.go"),
			OutputPkgName:      "genx",
			InterfacesPkg:      "github.com/invakid404/baml-rest/bamlutils",
			GeneratedClientPkg: "example.com/x/baml_client",
		},
		emittedDeBAMLCall: true,
	}
	g.maybeWriteDeBAMLHelper()

	b, err := os.ReadFile(filepath.Join(dir, "debaml.go"))
	if err != nil {
		t.Fatalf("debaml.go not written: %v", err)
	}
	got := string(b)
	for _, want := range []string{
		"package genx",
		`bamlutils "github.com/invakid404/baml-rest/bamlutils"`,
		`types "example.com/x/baml_client/types"`,
		"func maybeApplyDeBAMLOutputFormat(",
		"func rewriteOutputFormat(",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("emitted debaml.go missing %q\n%s", want, got)
		}
	}
}

// TestMaybeWriteDeBAMLHelper_RemovesStaleWhenNotEmitted pins the
// idempotency fix: a generation that emits no de-BAML call removes any
// debaml.go a prior generation left in the same output directory, so the
// package can't carry an orphaned helper.
func TestMaybeWriteDeBAMLHelper_RemovesStaleWhenNotEmitted(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, "debaml.go")
	if err := os.WriteFile(stale, []byte("package genx\n"), 0o644); err != nil {
		t.Fatalf("seed stale helper: %v", err)
	}
	g := &generator{
		pkgs:              PackageConfig{OutputPath: filepath.Join(dir, "adapter.go")},
		emittedDeBAMLCall: false,
	}
	g.maybeWriteDeBAMLHelper()

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale debaml.go not removed (stat err = %v)", err)
	}
}

// TestMaybeWriteDeBAMLHelper_NoopWhenNoneAndAbsent pins that the
// not-emitted path is a clean no-op when there is no helper to remove (the
// common case for every non-de-BAML generator).
func TestMaybeWriteDeBAMLHelper_NoopWhenNoneAndAbsent(t *testing.T) {
	dir := t.TempDir()
	g := &generator{
		pkgs:              PackageConfig{OutputPath: filepath.Join(dir, "adapter.go")},
		emittedDeBAMLCall: false,
	}
	g.maybeWriteDeBAMLHelper() // must not panic
	if _, err := os.Stat(filepath.Join(dir, "debaml.go")); !os.IsNotExist(err) {
		t.Errorf("debaml.go unexpectedly present: %v", err)
	}
}
