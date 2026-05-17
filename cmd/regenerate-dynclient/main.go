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

	"golang.org/x/mod/modfile"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/cmd/hacks/hacks"
)

const (
	rootModulePath        = "github.com/invakid404/baml-rest"
	patchedBAMLModulePath = rootModulePath + "/dynclient/baml-patched"
	dynamicBAMLSource     = "cmd/build/dynamic.baml"
	bamlVersionsManifest  = "integration/baml_versions.json"
	// outputRoot pins the on-disk layout the genadapter command and
	// the generator block's client_package_name compile against. The
	// orchestrator does not expose this (or patchedBAMLOut) as a flag
	// because multiple call sites bake the paths into source —
	// materializeBAMLProject's BAML generator block, runIntrospect's
	// --module-path, the rewrite target threaded into
	// hacks.RewriteGeneratedClientBAMLImports, and
	// dynclient/cmd/genadapter's package-level constants. A
	// non-default value would silently produce a tree the genadapter
	// run cannot import.
	outputRoot          = "dynclient/internal/generated"
	patchedBAMLOut      = "dynclient/baml-patched"
	generatedModulePath = rootModulePath + "/" + outputRoot
	npxBAMLPackage      = "@boundaryml/baml"
	genadapterPackage   = "./dynclient/cmd/genadapter"
	bamlCLIGenTimeout   = 10 * time.Minute
	provenanceFileName  = "BAML_REST_SOURCE_VERSION"
	generatorIdentity   = "cmd/regenerate-dynclient"
)

type config struct {
	BAMLVersion string
	BAMLCLI     string
	KeepTemp    bool
}

func main() {
	var cfg config
	flag.StringVar(&cfg.BAMLVersion, "baml-version", "", "BAML version to pin (defaults to integration/baml_versions.json `.latest`)")
	flag.StringVar(&cfg.BAMLCLI, "baml-cli", os.Getenv("BAML_CLI_PATH"), "Path to baml-cli (defaults to $BAML_CLI_PATH; npx fallback when empty)")
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

	bamlSrcDir, bamlSum, err := resolveBAMLModule(version)
	if err != nil {
		return err
	}
	fmt.Printf("==> Patched BAML source: %s\n", bamlSrcDir)

	fmt.Printf("==> Generating patched BAML module under %s\n", patchedBAMLOut)
	if err := hacks.GeneratePatchedBAMLModule(bamlSrcDir, patchedBAMLOut, patchedBAMLModulePath, version); err != nil {
		return fmt.Errorf("generating patched BAML module: %w", err)
	}
	// cmd/hacks writes BAML_REST_SOURCE_VERSION with the patch shape
	// it controls (path + sha256). The upstream module sum and the
	// generator-identity stamp live in the orchestrator — append them
	// so future readers can verify the fork against `go mod download`
	// without rerunning the regen.
	if err := appendProvenance(patchedBAMLOut, bamlSum); err != nil {
		return fmt.Errorf("recording provenance: %w", err)
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
	dstClient := filepath.Join(outputRoot, "baml_client")
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

	dstBAMLSrc := filepath.Join(outputRoot, "baml_src", "dynamic.baml")
	fmt.Printf("==> Copying dynamic.baml into %s\n", dstBAMLSrc)
	if err := os.MkdirAll(filepath.Dir(dstBAMLSrc), 0o755); err != nil {
		return fmt.Errorf("creating baml_src dir: %w", err)
	}
	if err := copyFile(dynamicBAMLSource, dstBAMLSrc); err != nil {
		return fmt.Errorf("copying dynamic.baml: %w", err)
	}

	introspectedDir := filepath.Join(outputRoot, "introspected")
	fmt.Printf("==> Running parameterized introspection into %s\n", introspectedDir)
	if err := os.RemoveAll(introspectedDir); err != nil {
		return fmt.Errorf("removing stale introspected dir: %w", err)
	}
	if err := os.MkdirAll(introspectedDir, 0o755); err != nil {
		return fmt.Errorf("creating introspected dir: %w", err)
	}
	if err := runIntrospect(outputRoot, dstClient); err != nil {
		return err
	}

	fmt.Println("==> Running dynclient/cmd/genadapter")
	if err := runGenadapter(); err != nil {
		return err
	}

	fmt.Printf("==> Running gofmt -w on %s\n", outputRoot)
	if err := runGofmt(outputRoot); err != nil {
		return err
	}

	// GeneratePatchedBAMLModule wipes the patched-baml output dir each
	// run, which removes the embed.go cmd/embed had written there.
	// Re-run cmd/embed so the parent's source-FS view of the patched
	// fork stays in sync — `go build ./...` would otherwise fail to
	// resolve `import "<patched>"` from the parent embed.go.
	fmt.Println("==> Running cmd/embed")
	if err := runEmbed(); err != nil {
		return err
	}

	fmt.Println("==> Dynclient regeneration complete")
	return nil
}

// findRepoRoot walks upward from the working directory looking for a
// go.mod whose `module` directive matches rootModulePath. Unreadable
// or unparseable go.mod files are treated as "not the root" and the
// walk continues — substring matching against the raw bytes would
// miss CRLF-encoded go.mod files and false-positive on substrings
// inside replace directives.
func findRepoRoot() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("getting working directory: %w", err)
	}
	dir := cwd
	for {
		goModPath := filepath.Join(dir, "go.mod")
		data, readErr := os.ReadFile(goModPath)
		if readErr == nil {
			parsed, parseErr := modfile.Parse(goModPath, data, nil)
			if parseErr == nil && parsed.Module != nil && parsed.Module.Mod.Path == rootModulePath {
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

// resolveBAMLModule asks the Go toolchain for the on-disk path AND
// the canonical h1 sum of `github.com/boundaryml/baml@<version>`. The
// orchestrator requires the module to already be in the GOMODCACHE;
// downstream builds bring it in via the parent go.mod, so this is
// true in practice for any version the matrix has built against. The
// sum is recorded in BAML_REST_SOURCE_VERSION so readers can verify
// the fork's source against `go mod download` without rerunning the
// regen.
func resolveBAMLModule(version string) (dir, sum string, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "mod", "download", "-x", "-json", "github.com/boundaryml/baml@"+version)
	cmd.Stderr = os.Stderr
	out, runErr := cmd.Output()
	if runErr != nil {
		return "", "", fmt.Errorf("downloading github.com/boundaryml/baml@%s: %w", version, runErr)
	}
	var info struct {
		Dir string `json:"Dir"`
		Sum string `json:"Sum"`
	}
	if jsonErr := json.Unmarshal(out, &info); jsonErr != nil {
		return "", "", fmt.Errorf("parsing go mod download output: %w", jsonErr)
	}
	if info.Dir == "" {
		return "", "", fmt.Errorf("go mod download returned empty Dir for github.com/boundaryml/baml@%s", version)
	}
	return info.Dir, info.Sum, nil
}

// appendProvenance augments the BAML_REST_SOURCE_VERSION file
// cmd/hacks/hacks.GeneratePatchedBAMLModule writes with the upstream
// module sum (so future readers can cross-check against `go mod
// download`) and the generator identity (so it is obvious which tool
// rebuilds the fork). Re-runs replace the file rather than appending
// duplicate lines.
func appendProvenance(patchedBAMLOut, upstreamSum string) error {
	versionPath := filepath.Join(patchedBAMLOut, provenanceFileName)
	existing, err := os.ReadFile(versionPath)
	if err != nil {
		return fmt.Errorf("reading %s: %w", versionPath, err)
	}
	var rebuilt strings.Builder
	dropPrefixes := []string{"upstream_sum=", "generated_by="}
	for _, line := range strings.Split(string(existing), "\n") {
		drop := false
		for _, prefix := range dropPrefixes {
			if strings.HasPrefix(line, prefix) {
				drop = true
				break
			}
		}
		if drop {
			continue
		}
		rebuilt.WriteString(line)
		rebuilt.WriteString("\n")
	}
	body := strings.TrimRight(rebuilt.String(), "\n")
	if body != "" {
		body += "\n"
	}
	if upstreamSum != "" {
		body += fmt.Sprintf("upstream_sum=%s\n", upstreamSum)
	}
	body += fmt.Sprintf("generated_by=%s\n", generatorIdentity)
	return os.WriteFile(versionPath, []byte(body), 0o644)
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
  client_package_name "%s"
}
`, semver, generatedModulePath)
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
		"--module-path", generatedModulePath,
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

// runEmbed re-runs cmd/embed at the repo root so the parent embed.go
// (and the embed.go inside the freshly regenerated patched-baml
// module) reflect the regenerated tree. The patched-module generator
// wipes its output dir each run; cmd/embed restores the per-module
// embed.go that the parent's Sources merge depends on.
func runEmbed() error {
	cmd := exec.Command("go", "run", "./cmd/embed")
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
