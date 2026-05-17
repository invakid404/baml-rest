// Command regenerate-dynclient orchestrates the full dynclient
// regeneration pipeline: patched BAML fork, BAML Go client
// generation off cmd/build/dynamic.baml, generated-client hacks,
// import rewrite, introspection, and the genadapter run that emits
// the framework adapter.
//
// Designed to be re-run any time the pinned BAML version in
// integration/baml_versions.json `.latest` advances. The orchestrator
// reads `.latest` at startup and uses it as the default BAML version;
// --baml-version overrides for one-off bumps.
//
// The runtime.go wrapper under dynclient/internal/generated is
// hand-written and not touched by this tool.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/cmd/hacks/hacks"
)

const (
	patchedBAMLModulePath = "github.com/invakid404/baml-rest/dynclient/internal/baml-patched"
	dynamicBAMLSource     = "cmd/build/dynamic.baml"
	bamlVersionsManifest  = "integration/baml_versions.json"
	defaultOutputRoot     = "dynclient/internal/generated"
	defaultPatchedBAMLOut = "dynclient/internal/baml-patched"
	npxBAMLPackage        = "@boundaryml/baml"
	genadapterPackage     = "./dynclient/cmd/genadapter"
	bamlCLIGenTimeout     = 10 * time.Minute
)

type config struct {
	BAMLVersion    string
	BAMLCLI        string
	OutputRoot     string
	PatchedBAMLOut string
	KeepTemp       bool
}

func main() {
	cfg := config{
		OutputRoot:     defaultOutputRoot,
		PatchedBAMLOut: defaultPatchedBAMLOut,
	}
	flag.StringVar(&cfg.BAMLVersion, "baml-version", "", "BAML version to pin (defaults to integration/baml_versions.json `.latest`)")
	flag.StringVar(&cfg.BAMLCLI, "baml-cli", os.Getenv("BAML_CLI_PATH"), "Path to baml-cli (defaults to $BAML_CLI_PATH; npx fallback when empty)")
	flag.StringVar(&cfg.OutputRoot, "output-root", cfg.OutputRoot, "Output root for the generated dynclient tree")
	flag.StringVar(&cfg.PatchedBAMLOut, "patched-baml-out", cfg.PatchedBAMLOut, "Output directory for the patched BAML fork")
	flag.BoolVar(&cfg.KeepTemp, "keep-temp", false, "Preserve the temp BAML project under the working directory for debugging")
	flag.Parse()

	if err := run(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "regenerate-dynclient: %v\n", err)
		os.Exit(1)
	}
}

func run(cfg config) error {
	repoRoot, err := findRepoRoot()
	if err != nil {
		return err
	}
	if err := os.Chdir(repoRoot); err != nil {
		return fmt.Errorf("chdir to repo root %s: %w", repoRoot, err)
	}

	version, err := resolveBAMLVersion(cfg.BAMLVersion)
	if err != nil {
		return err
	}
	fmt.Printf("==> Regenerating dynclient for BAML %s\n", version)

	bamlSrcDir, err := resolveBAMLModuleSourceDir(version)
	if err != nil {
		return err
	}
	fmt.Printf("==> Patched BAML source: %s\n", bamlSrcDir)

	fmt.Printf("==> Generating patched BAML module under %s\n", cfg.PatchedBAMLOut)
	if err := hacks.GeneratePatchedBAMLModule(bamlSrcDir, cfg.PatchedBAMLOut, patchedBAMLModulePath, version); err != nil {
		return fmt.Errorf("generating patched BAML module: %w", err)
	}

	tempProject, err := materializeBAMLProject(version)
	if err != nil {
		return err
	}
	if !cfg.KeepTemp {
		defer func() {
			_ = os.RemoveAll(tempProject)
		}()
	} else {
		fmt.Printf("==> Preserving temp BAML project at %s (--keep-temp)\n", tempProject)
	}

	fmt.Printf("==> Running BAML Go client generation in %s\n", tempProject)
	if err := runBAMLGenerate(cfg.BAMLCLI, version, tempProject); err != nil {
		return err
	}

	generatedRoot := filepath.Join(tempProject, "generated")
	srcClient := filepath.Join(generatedRoot, "baml_client")
	dstClient := filepath.Join(cfg.OutputRoot, "baml_client")
	fmt.Printf("==> Copying generated baml_client into %s\n", dstClient)
	if err := os.RemoveAll(dstClient); err != nil {
		return fmt.Errorf("removing stale baml_client: %w", err)
	}
	if err := copyTree(srcClient, dstClient); err != nil {
		return fmt.Errorf("copying baml_client: %w", err)
	}

	fmt.Println("==> Applying generated-client hacks (lazy_runtime, context_fix)")
	if err := hacks.ApplyAll(version, dstClient); err != nil {
		return fmt.Errorf("generated-client hacks: %w", err)
	}

	fmt.Printf("==> Rewriting BAML imports to %s\n", patchedBAMLModulePath)
	if err := hacks.RewriteGeneratedClientBAMLImports(dstClient, patchedBAMLModulePath); err != nil {
		return fmt.Errorf("rewriting baml_client imports: %w", err)
	}

	// BAML's generator emits the full Go-client preamble (encoding/json,
	// fmt, the BAML pkg, cffi) in every file, even when the dynamic-only
	// schema has no enums/type-aliases that reference them. Run goimports
	// on the rewritten client so the dead imports go away before
	// genadapter compiles against the package.
	fmt.Printf("==> Running goimports on %s\n", dstClient)
	if err := runGoimports(dstClient); err != nil {
		return err
	}

	dstBAMLSrc := filepath.Join(cfg.OutputRoot, "baml_src", "dynamic.baml")
	fmt.Printf("==> Copying dynamic.baml into %s\n", dstBAMLSrc)
	if err := os.MkdirAll(filepath.Dir(dstBAMLSrc), 0o755); err != nil {
		return fmt.Errorf("creating baml_src dir: %w", err)
	}
	if err := copyFile(dynamicBAMLSource, dstBAMLSrc); err != nil {
		return fmt.Errorf("copying dynamic.baml: %w", err)
	}

	introspectedDir := filepath.Join(cfg.OutputRoot, "introspected")
	fmt.Printf("==> Running parameterized introspection into %s\n", introspectedDir)
	if err := os.RemoveAll(introspectedDir); err != nil {
		return fmt.Errorf("removing stale introspected dir: %w", err)
	}
	if err := os.MkdirAll(introspectedDir, 0o755); err != nil {
		return fmt.Errorf("creating introspected dir: %w", err)
	}
	if err := runIntrospect(cfg.OutputRoot, dstClient); err != nil {
		return err
	}

	fmt.Println("==> Running dynclient/cmd/genadapter")
	if err := runGenadapter(); err != nil {
		return err
	}

	fmt.Printf("==> Running gofmt -w on %s\n", cfg.OutputRoot)
	if err := runGofmt(cfg.OutputRoot); err != nil {
		return err
	}

	fmt.Println("==> Dynclient regeneration complete")
	return nil
}

// findRepoRoot walks upward from the executable's runtime caller path
// to locate the repo root by go.mod. The orchestrator is run via
// `go run ./cmd/regenerate-dynclient`, so the working directory and
// the executable path both land somewhere under the repo tree.
func findRepoRoot() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("getting working directory: %w", err)
	}
	dir := cwd
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			data, readErr := os.ReadFile(filepath.Join(dir, "go.mod"))
			if readErr == nil && strings.Contains(string(data), "module github.com/invakid404/baml-rest\n") {
				return dir, nil
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", fmt.Errorf("cannot find baml-rest go.mod from %s", cwd)
}

// resolveBAMLVersion returns the override when supplied, otherwise
// reads integration/baml_versions.json `.latest` so the dynclient
// regen always matches the CI matrix's latest tested version.
func resolveBAMLVersion(override string) (string, error) {
	if override != "" {
		return bamlutils.NormalizeVersion(override), nil
	}
	data, err := os.ReadFile(bamlVersionsManifest)
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", bamlVersionsManifest, err)
	}
	var manifest struct {
		Latest string `json:"latest"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return "", fmt.Errorf("parsing %s: %w", bamlVersionsManifest, err)
	}
	if manifest.Latest == "" {
		return "", fmt.Errorf("%s has no `.latest` field", bamlVersionsManifest)
	}
	return bamlutils.NormalizeVersion(manifest.Latest), nil
}

// resolveBAMLModuleSourceDir asks the Go toolchain for the on-disk
// path of `github.com/boundaryml/baml@<version>`. The orchestrator
// requires the module to already be in the GOMODCACHE; downstream
// builds bring it in via the parent go.mod, so this is true in
// practice for any version the matrix has built against.
func resolveBAMLModuleSourceDir(version string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "mod", "download", "-x", "-json", "github.com/boundaryml/baml@"+version)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("downloading github.com/boundaryml/baml@%s: %w", version, err)
	}
	var info struct {
		Dir string `json:"Dir"`
	}
	if err := json.Unmarshal(out, &info); err != nil {
		return "", fmt.Errorf("parsing go mod download output: %w", err)
	}
	if info.Dir == "" {
		return "", fmt.Errorf("go mod download returned empty Dir for github.com/boundaryml/baml@%s", version)
	}
	return info.Dir, nil
}

// materializeBAMLProject writes a temp BAML project that contains
// dynamic.baml plus a generator block whose `client_package_name`
// resolves to the parent module's dynclient subtree. baml-cli /
// npx is then invoked in this directory.
func materializeBAMLProject(version string) (string, error) {
	dir, err := os.MkdirTemp("", "baml-rest-dynclient-")
	if err != nil {
		return "", fmt.Errorf("creating temp BAML project dir: %w", err)
	}
	bamlSrc := filepath.Join(dir, "baml_src")
	if err := os.MkdirAll(bamlSrc, 0o755); err != nil {
		return "", err
	}
	if err := copyFile(dynamicBAMLSource, filepath.Join(bamlSrc, "dynamic.baml")); err != nil {
		return "", err
	}
	semver := strings.TrimPrefix(version, "v")
	generatorBody := fmt.Sprintf(`generator baml_rest_dynclient_target {
  output_type "go"
  output_dir "../generated"
  version "%s"
  client_package_name "github.com/invakid404/baml-rest/dynclient/internal/generated"
}
`, semver)
	if err := os.WriteFile(filepath.Join(bamlSrc, "generators.baml"), []byte(generatorBody), 0o644); err != nil {
		return "", fmt.Errorf("writing generators.baml: %w", err)
	}
	return dir, nil
}

// runBAMLGenerate invokes baml-cli generate (or the npx fallback)
// inside the temp project. The CLI emits Go code into
// <tempdir>/generated per the generator block written by
// materializeBAMLProject.
func runBAMLGenerate(bamlCLI, version, projectDir string) error {
	ctx, cancel := context.WithTimeout(context.Background(), bamlCLIGenTimeout)
	defer cancel()
	var cmd *exec.Cmd
	if bamlCLI != "" {
		cmd = exec.CommandContext(ctx, bamlCLI, "generate", "--no-version-check")
	} else {
		npxPath, err := exec.LookPath("npx")
		if err != nil {
			return fmt.Errorf("no --baml-cli provided and npx not on PATH: %w", err)
		}
		semver := strings.TrimPrefix(version, "v")
		cmd = exec.CommandContext(ctx, npxPath, npxBAMLPackage+"@"+semver, "generate")
	}
	cmd.Dir = projectDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("baml generate timed out after %s", bamlCLIGenTimeout)
		}
		return fmt.Errorf("baml generate failed: %w", err)
	}
	return nil
}

// runIntrospect drives cmd/introspect via `go run` so the
// parameterized flags PR 2a landed populate the dynclient introspected
// package against the freshly generated client.
func runIntrospect(outputRoot, clientDir string) error {
	args := []string{
		"run",
		"./cmd/introspect",
		"--input-dir", clientDir,
		"--baml-src-dir", filepath.Join(outputRoot, "baml_src"),
		"--output-dir", filepath.Join(outputRoot, "introspected"),
		"--module-path", "github.com/invakid404/baml-rest/dynclient/internal/generated",
		"--interfaces-pkg", "github.com/invakid404/baml-rest/bamlutils",
		"--baml-module-path", patchedBAMLModulePath,
	}
	cmd := exec.Command("go", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("cmd/introspect: %w", err)
	}
	return nil
}

// runGenadapter invokes dynclient/cmd/genadapter via `go run` so it
// compiles against the introspected package that runIntrospect just
// wrote. A static import of the introspected package from this
// orchestrator would fix the package at build time, defeating the
// regen.
func runGenadapter() error {
	cmd := exec.Command("go", "run", genadapterPackage)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("dynclient/cmd/genadapter: %w", err)
	}
	return nil
}

// runGofmt formats the generated tree in place. The codegen emitters
// already produce gofmt-clean output, but a final pass keeps the
// committed artifacts stable across goimports/jen version bumps.
func runGofmt(dir string) error {
	cmd := exec.Command("gofmt", "-w", dir)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// runGoimports prunes unused imports the BAML generator leaves behind
// on dynamic-only clients. The orchestrator finds goimports on PATH or
// under $GOPATH/bin; the parent build pipeline (cmd/build/build.sh)
// already requires it for the same reason, so the dependency is
// reused.
func runGoimports(dir string) error {
	bin, err := exec.LookPath("goimports")
	if err != nil {
		if gopath := os.Getenv("GOPATH"); gopath != "" {
			candidate := filepath.Join(gopath, "bin", "goimports")
			if _, statErr := os.Stat(candidate); statErr == nil {
				bin = candidate
			}
		}
	}
	if bin == "" {
		return fmt.Errorf("goimports not found on PATH or $GOPATH/bin; install with `go install golang.org/x/tools/cmd/goimports@latest`")
	}
	cmd := exec.Command(bin, "-w", dir)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// copyTree mirrors src into dst, preserving file modes for regular
// files. Used for the BAML-generated baml_client tree where any
// special-mode files (executables, symlinks) would be a generator bug
// worth surfacing later.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(src, path)
		if relErr != nil {
			return relErr
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			return infoErr
		}
		return copyFileMode(path, target, info.Mode())
	})
}

func copyFile(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	return copyFileMode(src, dst, info.Mode())
}

func copyFileMode(src, dst string, mode fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode.Perm())
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}
