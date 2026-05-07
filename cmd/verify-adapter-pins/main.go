// verify-adapter-pins asserts that every module listed in
// adapterversions.PinnedModules still resolves
// github.com/boundaryml/baml to its canonical pin. It is the
// post-tidy gate scripts/sync.sh runs to catch the workspace-MVS lift
// regression that motivated this command (see go.work for the full
// rationale): if `go work sync` ever pulls common's BAML pin forward,
// the per-adapter `replace ../common` directives propagate the lift
// into every adapter on the next `go mod tidy`, silently breaking
// per-adapter version isolation.
//
// For each PinnedModule p, the verifier runs
//
//	GOWORK=off go list -m github.com/boundaryml/baml
//
// inside <repoRoot>/<p.DirName> and compares the second whitespace-
// separated field to p.BAMLVersion. GOWORK=off is mandatory: with a
// workspace active, `go list -m` returns the workspace-resolved
// version, not the per-module pin we want to assert.
//
// Exit codes: 0 = every pin matches; 1 = one or more mismatches; 2 =
// invocation error (wrong cwd, subprocess failure, etc.).
//
// Invocation:
//
//	cd <repo-root>
//	go run ./cmd/verify-adapter-pins
//
// scripts/sync.sh invokes this as the final step of a normal sync,
// and `scripts/sync.sh --check-pins-only` runs only this verifier.
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
)

// goListTimeout caps each `go list -m` subprocess. The command is
// metadata-only and finishes well under a second on a warm cache; 30s
// is the same circuit-breaker pattern cmd/verify-framework-adapter
// uses for gofmt.
const goListTimeout = 30 * time.Second

const bamlModulePath = "github.com/boundaryml/baml"

func main() {
	repoRoot, err := os.Getwd()
	if err != nil {
		fail("getcwd: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repoRoot, "go.work")); err != nil {
		fail("must be invoked from the repo root (no go.work in %s)", repoRoot)
	}

	type drift struct {
		dirName string
		got     string
		want    string
	}
	var drifts []drift

	for _, p := range adapterversions.PinnedModules {
		got, err := resolveBAMLVersion(repoRoot, p.DirName)
		if err != nil {
			fail("resolve baml version for %s: %v", p.DirName, err)
		}
		if got != p.BAMLVersion {
			drifts = append(drifts, drift{dirName: p.DirName, got: got, want: p.BAMLVersion})
			fmt.Printf("  %s: got %s, want %s  FAIL\n", p.DirName, got, p.BAMLVersion)
			continue
		}
		fmt.Printf("  %s: %s  ok\n", p.DirName, got)
	}

	if len(drifts) > 0 {
		fmt.Fprintf(os.Stderr, "\nadapter pin drift:\n")
		for _, d := range drifts {
			fmt.Fprintf(os.Stderr, "  %s: got %s, want %s\n", d.dirName, d.got, d.want)
		}
		os.Exit(1)
	}
	fmt.Printf("\nAll %d pinned BAML version(s) match.\n", len(adapterversions.PinnedModules))
}

// resolveBAMLVersion runs `GOWORK=off go list -m
// github.com/boundaryml/baml` in <repoRoot>/<dirName> and returns the
// resolved version string (e.g. "v0.204.0"). GOWORK=off is required
// so the per-module go.mod is the source of truth — under a workspace
// the command would return the workspace-MVS-selected version
// instead, which is exactly the value the verifier exists to detect.
func resolveBAMLVersion(repoRoot, dirName string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), goListTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "list", "-m", bamlModulePath)
	cmd.Dir = filepath.Join(repoRoot, dirName)
	cmd.Env = append(os.Environ(), "GOWORK=off")
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "", fmt.Errorf("go list -m timed out after %s in %s: %s", goListTimeout, dirName, errb.String())
	}
	if err != nil {
		return "", fmt.Errorf("%w: %s", err, errb.String())
	}
	// `go list -m <path>` prints "<path> <version>" on a single line.
	// Anything else is a toolchain shape change worth surfacing
	// loudly rather than silently parsing the wrong field.
	fields := strings.Fields(strings.TrimSpace(out.String()))
	if len(fields) < 2 {
		return "", fmt.Errorf("unexpected `go list -m` output for %s: %q", dirName, out.String())
	}
	return fields[1], nil
}

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "verify-adapter-pins: "+format+"\n", a...)
	os.Exit(2)
}
