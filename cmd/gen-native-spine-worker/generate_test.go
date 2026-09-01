package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils/projectdescriptor"
	"github.com/invakid404/baml-rest/bamlutils/promptdescriptor"
	"github.com/invakid404/baml-rest/bamlutils/schemadescriptor"
	"github.com/invakid404/baml-rest/internal/nativespine"
)

// jsonAliasDescriptorJSON builds the exact five-arm JSON-alias project descriptor
// (the U1 fixture) and marshals it the way `cmd/introspect --native-spine-descriptors`
// would, so the generator test drives the production descriptor -> generator path.
func jsonAliasDescriptorJSON(t *testing.T) []byte {
	t.Helper()
	proj, err := nativespine.BuildFromSource(nativespine.JSONAliasFixtureSources)
	if err != nil {
		t.Fatalf("BuildFromSource: %v", err)
	}
	if err := proj.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	data, err := json.MarshalIndent(proj, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return data
}

// collectTree returns a relpath->bytes map of every file under root, for
// determinism comparison.
func collectTree(t *testing.T, root string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = b
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return out
}

// seedStub writes the committed fail-loud stub into dir, mirroring the real
// nativegenerated package (whose only checked-in file is generated_off.go). The
// generator's cleaner requires it to be present before it will delete siblings.
func seedStub(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, stubFileName), []byte("// committed stub\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestGenerateEmitsAdmittedRegistry(t *testing.T) {
	data := jsonAliasDescriptorJSON(t)
	out := t.TempDir()
	seedStub(t, out)
	if err := Generate(data, out, defaultRegistryPackagePath); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	// The embedded descriptor and the aggregate both exist.
	if _, err := os.Stat(filepath.Join(out, projectJSONFileName)); err != nil {
		t.Fatalf("project.json missing: %v", err)
	}
	agg, err := os.ReadFile(filepath.Join(out, aggregateFileName))
	if err != nil {
		t.Fatalf("generated.go missing: %v", err)
	}
	aggStr := string(agg)
	for _, want := range []string{
		"//go:build " + buildTag,
		"package " + registryPackageName,
		"//go:embed " + projectJSONFileName,
		"func candidates() []spine.UnaryRegistration",
		"func NewRuntime() (worker.Runtime, error)",
		"spine.NewWorkerRuntime(proj, candidates(), nil)",
	} {
		if !strings.Contains(aggStr, want) {
			t.Errorf("generated.go missing %q", want)
		}
	}

	// Exactly one subpackage (the one admitted method), and it carries the emitted
	// generic identifiers.
	dirs := subpackageDirs(t, out)
	if len(dirs) != 1 {
		t.Fatalf("want exactly 1 subpackage, got %d: %v", len(dirs), dirs)
	}
	sub := dirs[0]
	// The subpackage import must be referenced by the aggregate.
	if !strings.Contains(aggStr, defaultRegistryPackagePath+"/"+sub) {
		t.Errorf("aggregate does not import subpackage %q", sub)
	}
	subFiles, err := os.ReadDir(filepath.Join(out, sub))
	if err != nil {
		t.Fatalf("read subpackage: %v", err)
	}
	if len(subFiles) != 1 {
		t.Fatalf("subpackage has %d files, want 1", len(subFiles))
	}
	src, err := os.ReadFile(filepath.Join(out, sub, subFiles[0].Name()))
	if err != nil {
		t.Fatalf("read emitted file: %v", err)
	}
	srcStr := string(src)
	for _, want := range []string{
		"package " + sub,
		`const MethodName = "StaticRecursiveAliasJSON"`,
		"func BuildMethod(",
		"func Binding()",
	} {
		if !strings.Contains(srcStr, want) {
			t.Errorf("emitted subpackage source missing %q", want)
		}
	}
}

func TestGenerateIsDeterministic(t *testing.T) {
	data := jsonAliasDescriptorJSON(t)
	a := t.TempDir()
	b := t.TempDir()
	seedStub(t, a)
	seedStub(t, b)
	if err := Generate(data, a, defaultRegistryPackagePath); err != nil {
		t.Fatalf("Generate(a): %v", err)
	}
	if err := Generate(data, b, defaultRegistryPackagePath); err != nil {
		t.Fatalf("Generate(b): %v", err)
	}
	ta, tb := collectTree(t, a), collectTree(t, b)
	if len(ta) != len(tb) {
		t.Fatalf("file count differs: %d vs %d", len(ta), len(tb))
	}
	for rel, ba := range ta {
		bb, ok := tb[rel]
		if !ok {
			t.Errorf("file %q only in first run", rel)
			continue
		}
		if string(ba) != string(bb) {
			t.Errorf("file %q differs between runs (non-deterministic)", rel)
		}
	}
}

func TestGenerateCleansStaleOutput(t *testing.T) {
	data := jsonAliasDescriptorJSON(t)
	out := t.TempDir()

	// Pre-seed the output dir with a stale generated subpackage, a stale aggregate,
	// a stale ROOT-LEVEL generated .go from a previous layout (an allowlist-only
	// clean would leave it behind to compile into the package), and a committed stub
	// that MUST survive.
	staleDir := filepath.Join(out, "mremovedmethod_0011223344ff")
	if err := os.MkdirAll(staleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staleDir, "generated_removedmethod.go"), []byte("package mremovedmethod_0011223344ff\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, aggregateFileName), []byte("// stale\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	staleRootGo := filepath.Join(out, "stale_root_from_old_layout.go")
	if err := os.WriteFile(staleRootGo, []byte("//go:build debamlnativeonlygenerated\n\npackage nativegenerated\n\nfunc StaleLeftover() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	staleProjJSON := filepath.Join(out, projectJSONFileName)
	if err := os.WriteFile(staleProjJSON, []byte("{stale}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	seedStub(t, out)
	stub := filepath.Join(out, stubFileName)

	if err := Generate(data, out, defaultRegistryPackagePath); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	if _, err := os.Stat(staleDir); !os.IsNotExist(err) {
		t.Errorf("stale subpackage survived generation (err=%v) — a removed method must not linger", err)
	}
	if _, err := os.Stat(staleRootGo); !os.IsNotExist(err) {
		t.Errorf("stale ROOT-LEVEL generated .go survived generation (err=%v) — it would compile into nativegenerated", err)
	}
	if b, err := os.ReadFile(stub); err != nil || string(b) != "// committed stub\n" {
		t.Errorf("committed stub was disturbed (bytes=%q err=%v) — generation must leave generated_off.go untouched", b, err)
	}
	// The fresh aggregate replaced the stale one.
	agg, err := os.ReadFile(filepath.Join(out, aggregateFileName))
	if err != nil || strings.Contains(string(agg), "stale") {
		t.Errorf("aggregate was not regenerated (err=%v)", err)
	}
	// The stale embedded descriptor was replaced with the fresh one (project.json is a
	// stale-regeneration artifact too, not merely the aggregate).
	proj, err := os.ReadFile(filepath.Join(out, projectJSONFileName))
	if err != nil || strings.Contains(string(proj), "stale") {
		t.Errorf("project.json was not regenerated (err=%v)", err)
	}
	if !strings.Contains(string(proj), "StaticRecursiveAliasJSON") {
		t.Errorf("regenerated project.json does not carry the admitted method descriptor")
	}
}

// TestCleanOutputDirRefusesMissingDir proves a non-existent output directory is
// refused, not silently accepted: it carries no committed stub, so generating into it
// would create a stubless registry package (a mistyped --out-dir).
func TestCleanOutputDirRefusesMissingDir(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	if err := cleanOutputDir(missing); err == nil {
		t.Fatalf("cleanOutputDir accepted a non-existent dir; it must be refused (no committed stub)")
	}
	// And the full Generate path must not create the stubless directory.
	if err := Generate(jsonAliasDescriptorJSON(t), missing, defaultRegistryPackagePath); err == nil {
		t.Fatalf("Generate accepted a non-existent --out-dir; it must refuse rather than create a stubless registry")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("Generate created the refused directory %q (err=%v)", missing, err)
	}
}

// TestCleanOutputDirRefusesStublessDir proves an existing directory that lacks the
// committed stub is refused, and that the refusal deletes nothing (foreign files
// survive).
func TestCleanOutputDirRefusesStublessDir(t *testing.T) {
	dir := t.TempDir() // exists, no stub
	readme := filepath.Join(dir, "README.md")
	if err := os.WriteFile(readme, []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := cleanOutputDir(dir); err == nil {
		t.Fatalf("cleanOutputDir accepted a stubless dir; a dir without the committed stub is not the registry package and must be refused")
	}
	if _, err := os.Stat(readme); err != nil {
		t.Fatalf("cleanOutputDir deleted a foreign file in a refused dir: %v", err)
	}
}

// TestCleanOutputDirPreservesForeignFiles proves that inside a valid (stub-bearing)
// registry directory, files this generator never emits are preserved while stale
// generated output is removed.
func TestCleanOutputDirPreservesForeignFiles(t *testing.T) {
	dir := t.TempDir()
	seedStub(t, dir)
	foreign := map[string]string{"README.md": "readme\n", ".gitignore": "*.tmp\n", "go.mod": "module x\n"}
	for name, body := range foreign {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A stale generated artifact that MUST be removed.
	if err := os.WriteFile(filepath.Join(dir, aggregateFileName), []byte("// stale\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := cleanOutputDir(dir); err != nil {
		t.Fatalf("cleanOutputDir: %v", err)
	}
	for name, want := range foreign {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || string(b) != want {
			t.Errorf("foreign file %q was not preserved (bytes=%q err=%v)", name, b, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, aggregateFileName)); !os.IsNotExist(err) {
		t.Errorf("stale aggregate survived (err=%v)", err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, stubFileName)); err != nil || string(b) != "// committed stub\n" {
		t.Errorf("committed stub was disturbed (bytes=%q err=%v)", b, err)
	}
}

// TestSubpackageNameIsCollisionProof proves the subpackage name is a valid Go
// identifier that includes a hash suffix (not only a sanitized method name), so
// two methods that sanitize to the same string still land in distinct packages.
func TestSubpackageNameIsCollisionProof(t *testing.T) {
	ident := regexp.MustCompile(`^m[a-z0-9]*_[0-9a-f]{12}$`)
	// "Foo-Bar" and "FooBar" both sanitize to "foobar" but must not collide.
	a := subpackageDirName("Foo-Bar")
	b := subpackageDirName("FooBar")
	if !ident.MatchString(a) || !ident.MatchString(b) {
		t.Fatalf("names are not valid collision-proof identifiers: %q %q", a, b)
	}
	if a == b {
		t.Fatalf("distinct methods collided to the same subpackage %q", a)
	}
	// Deterministic.
	if subpackageDirName("Foo-Bar") != a {
		t.Fatalf("subpackageDirName is not deterministic")
	}
}

func TestGenerateRejectsEmptyCandidateProject(t *testing.T) {
	// A project with zero static-unary methods yields no candidate; the generator
	// must refuse rather than emit an empty registry.
	proj := projectdescriptor.Project{
		Version:                 projectdescriptor.Version,
		PromptDescriptorVersion: promptdescriptor.Version,
		SchemaVersion:           schemadescriptor.Version,
	}
	data, err := json.Marshal(proj)
	if err != nil {
		t.Fatal(err)
	}
	// Assert the specific candidate-free refusal, not merely any error: a method-less
	// project can also fail earlier in proj.Validate(), which would pass this test
	// without reaching the intended candidate-free branch.
	err = Generate(data, t.TempDir(), defaultRegistryPackagePath)
	if err == nil {
		t.Fatalf("Generate accepted a candidate-free project, want refusal")
	}
	if !strings.Contains(err.Error(), "no codegen-admitted static-unary method") {
		t.Fatalf("error = %v, want the candidate-free refusal", err)
	}
}

func subpackageDirs(t *testing.T, out string) []string {
	t.Helper()
	entries, err := os.ReadDir(out)
	if err != nil {
		t.Fatal(err)
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}
	return dirs
}
