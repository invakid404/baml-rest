package staticservefixture

// PROCESS-DETERMINISM guard for cmd/introspect emission.
//
// The sibling drift guard (fixture_drift_test.go) regenerates the staticserve
// introspection ONCE and byte-compares it against the committed file. That
// catches a stale fixture, but it cannot catch a generator whose output depends
// on Go's randomized MAP ITERATION order: a single run has no reference to
// disagree with, and repeated runs INSIDE one process do not reseed the hash.
//
// This is not hypothetical. jennifer renders a map literal's `Values(...)` in
// the order it is given (unlike a struct-literal `Dict`, which it sorts), so an
// emitter that ranges a Go map directly produces the same bytes in a different
// order on every process. `generateMediaParams` did exactly that, and it was
// INVISIBLE while the fixture had at most one media function — the emitted map
// had one entry, so every order was the same order. Adding a second media
// function turned it into an intermittent fixture-drift failure that looked like
// an environment flake.
//
// So this test runs the generator in MANY SEPARATE PROCESSES and requires every
// output to be byte-identical to the committed file. It is deliberately hosted
// against the staticserve fixture because that is the DISCRIMINATING project:
// it declares two media functions, so its emitted MediaParams map has two keys
// and a per-process ordering bug has a ~50% chance of showing up in any single
// run. With codegenRuns runs, a reordering emitter escaping this test is
// vanishingly unlikely.
//
// It is a fence for the whole CLASS, not just for MediaParams: any future
// map-keyed emission that forgets to sort its keys fails here.

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"testing"
)

// codegenRuns is the number of independent generator PROCESSES. Each has its own
// randomized map-iteration seed, so an unsorted two-key emission has roughly a
// coin-flip chance of differing per run; 16 runs puts a false pass at ~2^-15.
const codegenRuns = 16

// TestStaticserveFixtureCodegenIsProcessDeterministic requires cmd/introspect to
// emit byte-identical output for the staticserve fixture across independent
// processes, and for that output to equal the committed artifact.
func TestStaticserveFixtureCodegenIsProcessDeterministic(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping repeated codegen (invokes the Go toolchain) in -short mode")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skipf("go toolchain not available: %v", err)
	}

	repoRoot := repoRootFromCaller(t)
	committedPath := filepath.Join(repoRoot, "internal", "nativeprompt", "testdata",
		"staticserve_fixture", "introspected", "introspected.go")
	committed, err := os.ReadFile(committedPath)
	if err != nil {
		t.Fatalf("read committed fixture: %v", err)
	}

	// NON-VACUITY: this guard is only discriminating while the emitted MediaParams
	// map really does carry MORE THAN ONE key. With a single entry there is only
	// one possible order, unsorted == sorted, and the 16 runs below would agree no
	// matter what the emitter does — the test would keep passing while silently
	// fencing nothing.
	//
	// The question is therefore about the emitted MAP, and it is asked of the
	// emitted map: parse the generated file and count the DISTINCT top-level keys
	// of the MediaParams literal. A substring scan cannot answer it — the whole
	// file mentions `"StaticMedia…"` dozens of times (SyncMethods, ParseMethods,
	// the descriptor factories, …), so a project reduced to ONE direct-media
	// function would still satisfy any count-based threshold while collapsing the
	// map to a single entry.
	mediaKeys := mediaParamsKeys(t, committedPath)
	if len(mediaKeys) < 2 {
		t.Fatalf("the committed fixture's MediaParams map has %d key(s) %v; this guard needs a "+
			"multi-entry media map to be discriminating (see the file doc). Restore the media "+
			"witnesses in staticserve_fixture/baml_src, or move this guard to a fixture that has them.",
			len(mediaKeys), mediaKeys)
	}

	first := ""
	for i := 0; i < codegenRuns; i++ {
		out := runIntrospectOnce(t, repoRoot)
		if !bytes.Equal(out, committed) {
			t.Fatalf("run %d/%d emitted output differing from the committed fixture at offset %d — "+
				"the generator is not process-deterministic (an emitted map's keys are most likely unsorted) "+
				"or the fixture is stale; regenerate with scripts/regen-staticserve-fixture.sh",
				i+1, codegenRuns, firstDiffOffset(out, committed))
		}
		if first == "" {
			first = string(out)
		} else if string(out) != first {
			// Unreachable while the committed comparison above holds; kept so the
			// run-to-run property is stated independently of the committed bytes.
			t.Fatalf("run %d/%d differs from run 1: cmd/introspect is not process-deterministic", i+1, codegenRuns)
		}
	}
}

// runIntrospectOnce runs the generator in a FRESH process (a new map seed) into
// a fresh temp dir and returns the emitted bytes. The flags mirror the
// staticserve leg of scripts/regen-staticserve-fixture.sh exactly.
func runIntrospectOnce(t *testing.T, repoRoot string) []byte {
	t.Helper()
	outDir := t.TempDir()
	cmd := exec.Command("go", "run", "./cmd/introspect",
		"--input-dir", "internal/nativeprompt/testdata/staticserve_fixture/baml_client",
		"--baml-src-dir", "internal/nativeprompt/testdata/staticserve_fixture/baml_src",
		"--output-dir", outDir,
		"--module-path", "github.com/invakid404/baml-rest/internal/nativeprompt/testdata/staticserve_fixture",
		"--interfaces-pkg", "github.com/invakid404/baml-rest/bamlutils",
		"--baml-module-path", "github.com/boundaryml/baml",
	)
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("regenerating fixture failed: %v\n%s", err, out)
	}
	data, err := os.ReadFile(filepath.Join(outDir, "introspected.go"))
	if err != nil {
		t.Fatalf("read regenerated fixture: %v", err)
	}
	return data
}

// mediaParamsKeys parses the generated introspection file and returns the
// DISTINCT top-level keys of its `var MediaParams = map[string]...{...}`
// literal, sorted for a stable failure message.
//
// It reads the GENERATED ARTIFACT rather than importing the introspected
// package, because that package imports the generated baml_client and therefore
// the BAML CFFI — importing it would force this guard out of the default
// CGO-free lane it shares with the drift guard, for a fact that is plainly
// visible in the emitted source.
//
// An absent or non-literal MediaParams is a hard failure, not a zero: the guard
// must not quietly downgrade to "no keys found, nothing to check".
func mediaParamsKeys(t *testing.T, path string) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse generated fixture %s: %v", path, err)
	}

	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.VAR {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range vs.Names {
				if name.Name != "MediaParams" || i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.CompositeLit)
				if !ok {
					t.Fatalf("MediaParams is not a composite literal: %T", vs.Values[i])
				}
				seen := map[string]bool{}
				for _, elt := range lit.Elts {
					kv, ok := elt.(*ast.KeyValueExpr)
					if !ok {
						t.Fatalf("MediaParams element is not a key/value: %T", elt)
					}
					key, ok := kv.Key.(*ast.BasicLit)
					if !ok || key.Kind != token.STRING {
						t.Fatalf("MediaParams key is not a string literal: %T", kv.Key)
					}
					unquoted, uerr := strconv.Unquote(key.Value)
					if uerr != nil {
						t.Fatalf("unquote MediaParams key %q: %v", key.Value, uerr)
					}
					seen[unquoted] = true
				}
				out := make([]string, 0, len(seen))
				for k := range seen {
					out = append(out, k)
				}
				sort.Strings(out)
				return out
			}
		}
	}
	t.Fatalf("no `MediaParams` var found in %s; the emitter must still declare it for this guard to mean anything", path)
	return nil
}
