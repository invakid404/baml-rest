package staticoracle

// Regeneration drift guard for the checked-in de-BAML Phase 8A metadata fixture
// (internal/nativeprompt/testdata/static_oracle/introspected/introspected.go).
//
// The gated differentials consume the EMITTED fixture as the candidate leg, so
// the fixture MUST stay byte-identical to what `go run ./cmd/introspect` produces
// for this stock-BAML static project. This guard re-runs the exact generation
// into a temp dir and byte-compares, failing with the regen command if drift is
// detected. It is CGO-free (cmd/introspect parses the baml_client as text; it
// never links the BAML CFFI) and carries no build tag, so it runs in the default
// `go test` suite as part of the codegen-idempotence proof.
//
// On mismatch it reports only the byte length, the first differing offset, and
// the regen command — never the differing bytes — so a diagnostic cannot leak the
// fixture's raw prompt/client-literal content.

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestFixtureRegenerationNoDrift(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping fixture regeneration (invokes the Go toolchain) in -short mode")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skipf("go toolchain not available: %v", err)
	}

	repoRoot := staticOracleRepoRoot(t)
	committed := filepath.Join(repoRoot, "internal", "nativeprompt", "testdata", "static_oracle", "introspected", "introspected.go")
	want, err := os.ReadFile(committed)
	if err != nil {
		t.Fatalf("read committed fixture: %v", err)
	}

	outDir := t.TempDir()
	// Same flags cmd/regenerate-dynclient's runIntrospect uses, retargeted at the
	// stock-oracle project. module-path roots the fixture's baml_client import at
	// its on-disk location so the generated file compiles under CGO in the oracle
	// suites; interfaces-pkg roots promptdescriptor/bamlparser/schemadescriptor.
	args := []string{
		"run", "./cmd/introspect",
		"--input-dir", "internal/nativeprompt/testdata/static_oracle/baml_client",
		"--baml-src-dir", "internal/nativeprompt/testdata/static_oracle/baml_src",
		"--output-dir", outDir,
		"--module-path", "github.com/invakid404/baml-rest/internal/nativeprompt/testdata/static_oracle",
		"--interfaces-pkg", "github.com/invakid404/baml-rest/bamlutils",
		"--baml-module-path", "github.com/boundaryml/baml",
	}
	cmd := exec.Command("go", args...)
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("regenerating fixture failed: %v\n%s", err, out)
	}

	got, err := os.ReadFile(filepath.Join(outDir, "introspected.go"))
	if err != nil {
		t.Fatalf("read regenerated fixture: %v", err)
	}

	if !bytes.Equal(got, want) {
		off := firstDiffOffset(got, want)
		t.Fatalf("checked-in static_oracle introspected fixture is STALE (committed %d bytes, regenerated %d bytes, first diff at offset %d).\n"+
			"Regenerate with:\n"+
			"  go run ./cmd/introspect \\\n"+
			"    --input-dir internal/nativeprompt/testdata/static_oracle/baml_client \\\n"+
			"    --baml-src-dir internal/nativeprompt/testdata/static_oracle/baml_src \\\n"+
			"    --output-dir internal/nativeprompt/testdata/static_oracle/introspected \\\n"+
			"    --module-path github.com/invakid404/baml-rest/internal/nativeprompt/testdata/static_oracle \\\n"+
			"    --interfaces-pkg github.com/invakid404/baml-rest/bamlutils \\\n"+
			"    --baml-module-path github.com/boundaryml/baml",
			len(want), len(got), off)
	}
}

func firstDiffOffset(a, b []byte) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}

func staticOracleRepoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// file = <repo>/internal/nativeprompt/staticoracle/fixture_drift_test.go
	return filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(file))))
}
