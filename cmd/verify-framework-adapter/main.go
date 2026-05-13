// verify-framework-adapter is the CI-side gate for the codegen-emit
// framework adapter.go produced by adapters/common/codegen. It runs
// two independent checks per adapter:
//
//  1. Deterministic emission: run the emitter twice and byte-diff the
//     outputs. Fails if any map iteration in the new emit code or in
//     a downstream jen helper is unsorted.
//
//  2. Behavioural test parity: copy the per-adapter module to a
//     tempdir, write the codegen-emitted adapter/adapter.go into it,
//     rewrite the go.mod's relative `replace` directives to absolute
//     paths so the tempdir copy compiles, and run
//     `GOWORK=off go test ./adapter/`. Fails on any test failure.
//
// The emitter (adapters/common/codegen) is the sole source of truth
// for adapter/adapter.go; per-adapter adapter/ subtrees in the source
// tree contain only the test harness (and, for v0.204,
// dynamic_value.go).
//
// Exit codes: 0 = all checks pass for all adapters; 1 = any failure.
//
// Invocation:
//
//	cd <repo-root>
//	go run ./cmd/verify-framework-adapter
//
// CI invokes the same command after a checkout-clean job. Output is
// human-readable; CI greps for "FAIL" on non-zero exit.
//
// Local-dev note: running `go test ./adapter` inside a per-adapter
// module from a fresh checkout requires emitting adapter.go first
// via `go run ./cmd/main.go` in that module. The production build
// flow (cmd/build/build.sh) runs the generator transparently.
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/invakid404/baml-rest/adapters/common/adapterversions"
	"github.com/invakid404/baml-rest/adapters/common/codegen"
)

func main() {
	repoRoot, err := os.Getwd()
	if err != nil {
		fail("getcwd: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repoRoot, "go.work")); err != nil {
		fail("must be invoked from the repo root (no go.work in %s)", repoRoot)
	}

	const checksPerAdapter = 2
	totalChecks := checksPerAdapter * len(adapterversions.FrameworkAdapters)
	totalFailures := 0
	for _, fa := range adapterversions.FrameworkAdapters {
		fmt.Printf("=== %s ===\n", fa.DirName)
		failures := runChecks(repoRoot, fa)
		if failures == 0 {
			fmt.Printf("    PASS: all %d checks green\n", checksPerAdapter)
		} else {
			fmt.Printf("    FAIL: %d check(s) failed\n", failures)
		}
		totalFailures += failures
	}

	if totalFailures > 0 {
		fmt.Fprintf(os.Stderr, "\nFAIL: %d/%d framework-adapter codegen-emit checks failed\n", totalFailures, totalChecks)
		os.Exit(1)
	}
	fmt.Printf("\nAll framework-adapter codegen-emit checks passed (%d/%d green).\n", totalChecks, totalChecks)
}

func runChecks(repoRoot string, fa adapterversions.FrameworkAdapter) int {
	tmp, err := os.MkdirTemp("", "verify-framework-"+fa.DirName+"-*")
	if err != nil {
		fail("mktemp: %v", err)
	}
	defer os.RemoveAll(tmp)

	// Emit candidate.
	candidatePath := filepath.Join(tmp, "adapter_emitted_1.go")
	codegen.GenerateFrameworkAdapter(fa.Options, candidatePath)

	failures := 0

	// Check 1: deterministic emission.
	candidatePath2 := filepath.Join(tmp, "adapter_emitted_2.go")
	codegen.GenerateFrameworkAdapter(fa.Options, candidatePath2)
	if err := checkDeterministicEmission(candidatePath, candidatePath2); err != nil {
		fmt.Printf("    Check 1 (deterministic emission): FAIL: %v\n", err)
		failures++
	} else {
		fmt.Printf("    Check 1 (deterministic emission): PASS\n")
	}

	// Check 2: behavioural test parity.
	if err := checkBehaviouralTestParity(repoRoot, fa, candidatePath); err != nil {
		fmt.Printf("    Check 2 (behavioural test parity): FAIL: %v\n", err)
		failures++
	} else {
		fmt.Printf("    Check 2 (behavioural test parity): PASS\n")
	}

	return failures
}

// checkDeterministicEmission byte-compares two consecutive emit
// outputs. They must be identical; any drift means the emitter has
// non-deterministic behaviour (typically an unsorted map iteration).
func checkDeterministicEmission(path1, path2 string) error {
	a, err := os.ReadFile(path1)
	if err != nil {
		return err
	}
	b, err := os.ReadFile(path2)
	if err != nil {
		return err
	}
	if !bytes.Equal(a, b) {
		return fmt.Errorf("emit run 1 (%d bytes) != emit run 2 (%d bytes)", len(a), len(b))
	}
	return nil
}

// checkBehaviouralTestParity copies the per-adapter module to a
// tempdir, writes the codegen-emitted candidate as adapter/adapter.go,
// rewrites go.mod's relative `replace` directives to absolute paths
// (because the tempdir copy is one level removed from the source-
// tree's siblings), and runs `GOWORK=off go test ./adapter/`. The
// source-tree files are NEVER touched.
func checkBehaviouralTestParity(repoRoot string, fa adapterversions.FrameworkAdapter, candidatePath string) error {
	srcDir := filepath.Join(repoRoot, "adapters", fa.DirName)
	tmpDir, err := os.MkdirTemp("", "verify-fwadapter-test-"+fa.DirName+"-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	if err := copyDir(srcDir, tmpDir); err != nil {
		return fmt.Errorf("copy adapter dir: %w", err)
	}

	// Write the codegen-emitted candidate as adapter/adapter.go. The
	// adapter/ directory may not exist in the copied tree if the source
	// adapter/ subtree has no non-test files at all (v0.215, v0.219);
	// create it explicitly so the test stays robust.
	candData, err := os.ReadFile(candidatePath)
	if err != nil {
		return err
	}
	adapterDir := filepath.Join(tmpDir, "adapter")
	if err := os.MkdirAll(adapterDir, 0o755); err != nil {
		return fmt.Errorf("mkdir adapter: %w", err)
	}
	if err := os.WriteFile(filepath.Join(adapterDir, "adapter.go"), candData, 0644); err != nil {
		return err
	}

	// Rewrite go.mod replaces from "../common" / "../../bamlutils" /
	// etc. to absolute paths so the tempdir's `go test` resolves them
	// against the repo source tree.
	if err := absoluteReplacesInGoMod(filepath.Join(tmpDir, "go.mod"), srcDir); err != nil {
		return fmt.Errorf("rewrite go.mod replaces: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), goTestTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "test", "./adapter/")
	cmd.Dir = tmpDir
	cmd.Env = append(os.Environ(), "GOWORK=off")
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err = cmd.Run()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("go test ./adapter/ timed out after %s\nstdout:\n%s\nstderr:\n%s",
			goTestTimeout, out.String(), errb.String())
	}
	if err != nil {
		return fmt.Errorf("%w\nstdout:\n%s\nstderr:\n%s", err, out.String(), errb.String())
	}
	return nil
}

// goTestTimeout caps the per-adapter `go test ./adapter/` subprocess
// at 5 minutes. The shared driver runs sub-second per adapter today;
// 5 minutes is the circuit-breaker for a hung test or an unresponsive
// `go` toolchain.
const goTestTimeout = 5 * time.Minute

func copyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode())
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, info.Mode())
	})
}

// absoluteReplacesInGoMod rewrites every relative `=> <relpath>`
// directive in goModPath so that <relpath> resolves to the same
// absolute location it would resolve to from origDir. This is the
// minimal patch that makes a tempdir-copied per-adapter module's
// go.mod work without depending on go.work.
func absoluteReplacesInGoMod(goModPath, origDir string) error {
	data, err := os.ReadFile(goModPath)
	if err != nil {
		return err
	}
	lines := strings.Split(string(data), "\n")
	for i, line := range lines {
		lines[i] = rewriteRelativeReplaceLine(line, origDir)
	}
	return os.WriteFile(goModPath, []byte(strings.Join(lines, "\n")), 0644)
}

// rewriteRelativeReplaceLine inspects one go.mod line for a `=>
// <relpath>` replace directive and rewrites <relpath> to its absolute
// equivalent (resolved against origDir) when <relpath> is genuinely
// relative — exactly ".", a "./" prefix, or a "../" prefix. Module-
// path replaces (`=> github.com/foo/bar v1.2.3`), absolute-path
// replaces, and lines with no `=>` are returned verbatim. Whitespace
// before/after the path, version suffixes, and trailing comments are
// preserved.
func rewriteRelativeReplaceLine(line, origDir string) string {
	idx := strings.Index(line, "=>")
	if idx < 0 {
		return line
	}
	prefix := line[:idx+len("=>")]
	rest := line[idx+len("=>"):]
	trimmed := strings.TrimLeft(rest, " \t")
	if trimmed == "" {
		return line
	}
	leadingWS := rest[:len(rest)-len(trimmed)]
	// Path token ends at the first whitespace boundary or EOL.
	end := len(trimmed)
	for j, r := range trimmed {
		if r == ' ' || r == '\t' {
			end = j
			break
		}
	}
	relPath := trimmed[:end]
	if !isRelativeReplacePath(relPath) {
		return line
	}
	tail := trimmed[end:] // everything after the path token, verbatim
	absPath := filepath.Clean(filepath.Join(origDir, relPath))
	return prefix + leadingWS + absPath + tail
}

// isRelativeReplacePath reports whether p is a relative filesystem
// path of the shape go.mod's `replace ... => <path>` directives use:
// either exactly "." or "..", or prefixed with "./" or "../". Module
// paths (e.g. "github.com/foo/bar") and absolute paths (e.g.
// "/usr/local") return false; the rewrite must leave them alone.
//
// Exact ".." is a separate arm because HasPrefix("..", "../") is
// false (no trailing slash). The previous substring matcher
// (`"=> .."`) caught both `=> ..` and `=> ../foo` by accident; the
// predicate has to spell that out.
func isRelativeReplacePath(p string) bool {
	return p == "." || p == ".." || strings.HasPrefix(p, "./") || strings.HasPrefix(p, "../")
}

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "verify-framework-adapter: "+format+"\n", a...)
	os.Exit(2)
}
