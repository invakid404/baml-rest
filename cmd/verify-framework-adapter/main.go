// verify-framework-adapter is the CI-side gate for #199 item 1 / PR
// 4a's verifier-only phase. It runs three independent checks against
// the codegen-emitted framework adapter.go for every per-adapter
// module:
//
//  1. Structural equivalence: emit the framework adapter.go, gofmt
//     it, parse with go/parser, and AST-diff against the source-
//     tree's hand-written file (also gofmted). Tolerates comment-
//     text differences; fails on any other AST-level divergence.
//
//  2. Deterministic emission: run the emitter twice and byte-diff the
//     outputs. Fails if any map iteration in the new emit code or in
//     a downstream jen helper is unsorted.
//
//  3. Behavioural test parity: copy the per-adapter module to a
//     tempdir, replace adapter/adapter.go with the codegen-emitted
//     candidate, rewrite the go.mod's relative `replace` directives
//     to absolute paths so the tempdir copy compiles, and run
//     `GOWORK=off go test ./adapter/`. Fails on any test failure.
//
// 4a leaves the hand-written adapter.go files in place. 4b removes
// them once 4a's verifier has been green for N consecutive main-
// branch runs covering at least one PR touching adapters/ or
// adapters/common/codegen/.
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
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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

	totalFailures := 0
	for _, fa := range adapterversions.FrameworkAdapters {
		fmt.Printf("=== %s ===\n", fa.DirName)
		failures := runChecks(repoRoot, fa)
		if failures == 0 {
			fmt.Printf("    PASS: all 3 checks green\n")
		} else {
			fmt.Printf("    FAIL: %d check(s) failed\n", failures)
		}
		totalFailures += failures
	}

	if totalFailures > 0 {
		fmt.Fprintf(os.Stderr, "\nFAIL: %d check failure(s) across all adapters\n", totalFailures)
		os.Exit(1)
	}
	fmt.Println("\nAll framework-adapter codegen-emit checks passed.")
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

	// Check 1: structural equivalence (AST diff after gofmt, ignoring
	// comments).
	handPath := filepath.Join(repoRoot, "adapters", fa.DirName, "adapter", "adapter.go")
	if err := checkStructuralEquivalence(candidatePath, handPath); err != nil {
		fmt.Printf("    Check 1 (structural equivalence): FAIL: %v\n", err)
		failures++
	} else {
		fmt.Printf("    Check 1 (structural equivalence): PASS\n")
	}

	// Check 2: deterministic emission.
	candidatePath2 := filepath.Join(tmp, "adapter_emitted_2.go")
	codegen.GenerateFrameworkAdapter(fa.Options, candidatePath2)
	if err := checkDeterministicEmission(candidatePath, candidatePath2); err != nil {
		fmt.Printf("    Check 2 (deterministic emission): FAIL: %v\n", err)
		failures++
	} else {
		fmt.Printf("    Check 2 (deterministic emission): PASS\n")
	}

	// Check 3: behavioural test parity.
	if err := checkBehaviouralTestParity(repoRoot, fa, candidatePath); err != nil {
		fmt.Printf("    Check 3 (behavioural test parity): FAIL: %v\n", err)
		failures++
	} else {
		fmt.Printf("    Check 3 (behavioural test parity): PASS\n")
	}

	return failures
}

// checkStructuralEquivalence verifies the codegen-emitted file and
// the hand-written file are AST-equivalent after gofmt, ignoring
// comments and node positions. Returns nil on equivalence; non-nil
// error otherwise with a short description of the first divergence.
func checkStructuralEquivalence(candidatePath, handPath string) error {
	candData, err := gofmtFile(candidatePath)
	if err != nil {
		return fmt.Errorf("gofmt candidate: %w", err)
	}
	handData, err := gofmtFile(handPath)
	if err != nil {
		return fmt.Errorf("gofmt hand-written: %w", err)
	}

	fset := token.NewFileSet()
	candFile, err := parser.ParseFile(fset, candidatePath, candData, parser.SkipObjectResolution)
	if err != nil {
		return fmt.Errorf("parse candidate: %w", err)
	}
	handFile, err := parser.ParseFile(fset, handPath, handData, parser.SkipObjectResolution)
	if err != nil {
		return fmt.Errorf("parse hand-written: %w", err)
	}

	candTokens := canonicalTokens(candFile)
	handTokens := canonicalTokens(handFile)

	if len(candTokens) != len(handTokens) {
		// Surface the first divergence location for easier debugging.
		return fmt.Errorf("AST shape differs: candidate has %d top-level decls, hand-written has %d", len(candTokens), len(handTokens))
	}

	for i := range candTokens {
		if candTokens[i] != handTokens[i] {
			return fmt.Errorf("decl #%d AST diverges:\n  candidate:    %s\n  hand-written: %s",
				i, truncate(candTokens[i]), truncate(handTokens[i]))
		}
	}
	return nil
}

// canonicalTokens walks the file's top-level declarations and reduces
// each to a comment-stripped, normalised string suitable for
// equivalence comparison. The transform: strip all comments, strip
// line/column positions, normalise import-spec aliasing, then format
// each decl back to source text.
func canonicalTokens(f *ast.File) []string {
	out := make([]string, 0, len(f.Decls))
	stripCommentsFile(f)
	for _, decl := range f.Decls {
		out = append(out, normaliseDecl(decl))
	}
	return out
}

// stripCommentsFile clears comment groups attached to the file and
// every node reachable from its declarations. We can't simply nil
// f.Comments because the *ast.GenDecl/FuncDecl carry their own Doc /
// inline-Comment fields that printer reattaches. Walk the tree and
// nil them all.
func stripCommentsFile(f *ast.File) {
	f.Comments = nil
	ast.Inspect(f, func(n ast.Node) bool {
		switch d := n.(type) {
		case *ast.GenDecl:
			d.Doc = nil
		case *ast.FuncDecl:
			d.Doc = nil
		case *ast.Field:
			d.Doc = nil
			d.Comment = nil
		case *ast.ImportSpec:
			d.Doc = nil
			d.Comment = nil
		case *ast.ValueSpec:
			d.Doc = nil
			d.Comment = nil
		case *ast.TypeSpec:
			d.Doc = nil
			d.Comment = nil
		}
		return true
	})
}

func normaliseDecl(decl ast.Decl) string {
	var buf bytes.Buffer
	fset := token.NewFileSet()
	if err := format.Node(&buf, fset, decl); err != nil {
		return fmt.Sprintf("<format error: %v>", err)
	}
	// Collapse all whitespace runs to a single space so cosmetic line
	// breaks in struct field initialisers / function bodies don't
	// register as differences.
	return collapseWhitespace(buf.String())
}

var wsRE = regexp.MustCompile(`\s+`)

func collapseWhitespace(s string) string {
	return strings.TrimSpace(wsRE.ReplaceAllString(s, " "))
}

func truncate(s string) string {
	const max = 200
	if len(s) <= max {
		return s
	}
	return s[:max] + "...[+" + fmt.Sprint(len(s)-max) + " bytes]"
}

// gofmtTimeout caps the gofmt subprocess at 30s. A single gofmted
// adapter.go is sub-second on healthy CI; 30s is the "hung process"
// circuit-breaker so the verifier fails fast with a specific timeout
// error rather than burning the workflow's broad timeout budget.
const gofmtTimeout = 30 * time.Second

func gofmtFile(path string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gofmtTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gofmt", path)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return nil, fmt.Errorf("gofmt timed out after %s on %s: %s", gofmtTimeout, path, errb.String())
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %s", err, errb.String())
	}
	return out.Bytes(), nil
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
// tempdir, replaces adapter/adapter.go with the codegen-emitted
// candidate, rewrites go.mod's relative `replace` directives to
// absolute paths (because the tempdir copy is one level removed from
// the source-tree's siblings), and runs `GOWORK=off go test
// ./adapter/`. The hand-written source-tree files are NEVER touched.
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

	// Swap the hand-written adapter.go for the codegen-emitted
	// candidate.
	candData, err := os.ReadFile(candidatePath)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "adapter", "adapter.go"), candData, 0644); err != nil {
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
// at 5 minutes. The post-PR-3 shared driver runs sub-second per
// adapter today; 5 minutes is the circuit-breaker for a hung test or
// an unresponsive `go` toolchain.
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
