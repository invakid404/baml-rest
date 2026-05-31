package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/invakid404/baml-rest/cmd/hacks/hacks"
)

const (
	// defaultPatchedBAMLModulePath is the module path the dynclient-only
	// patched fork declares in its go.mod. cmd/hacks treats it as the
	// rewrite target for `--rewrite-baml-imports-to` when the operator
	// supplies `--patched-baml-out` without a custom path.
	defaultPatchedBAMLModulePath = "github.com/invakid404/baml-rest/dynclient/baml-patched"
	// resolveModuleSrcTimeout caps `go list -m -f {{.Dir}} ...` when
	// resolving an upstream BAML source directory from `--baml-version`.
	resolveModuleSrcTimeout = 30 * time.Second
)

func main() {
	bamlClientDir := flag.String("baml-client-dir", "", "Path to the generated baml_client directory (required unless --skip-client-hacks is set)")
	bamlVersion := flag.String("baml-version", "", "Current BAML version (required)")
	patchedBAMLOut := flag.String("patched-baml-out", "", "When set, regenerate a checked-in patched BAML module at this path instead of patching the resolved module in place")
	patchedBAMLModulePath := flag.String("patched-baml-module-path", defaultPatchedBAMLModulePath, "Module path declared in the patched BAML go.mod when --patched-baml-out is set")
	bamlModuleSrc := flag.String("baml-module-src", "", "Override the BAML source directory used to seed the patched module; defaults to `go list -m -f {{.Dir}} github.com/boundaryml/baml`")
	rewriteBAMLImportsTo := flag.String("rewrite-baml-imports-to", "", "When set, rewrite generated-client imports of github.com/boundaryml/baml/... to the supplied module path after the client-side hacks run")
	skipClientHacks := flag.Bool("skip-client-hacks", false, "Skip the generated-client hacks (lazy_runtime, context_fix) and run only the patched-module / runtime-deadlock-fix steps")
	skipBAMLModulePatch := flag.Bool("skip-baml-module-patch", false, "Skip the BAML runtime-deadlock-fix / patched-module generation step")

	flag.Parse()

	if *bamlVersion == "" {
		fmt.Fprintln(os.Stderr, "Error: --baml-version is required")
		flag.Usage()
		os.Exit(1)
	}

	if !*skipClientHacks {
		if *bamlClientDir == "" {
			fmt.Fprintln(os.Stderr, "Error: --baml-client-dir is required unless --skip-client-hacks is set")
			flag.Usage()
			os.Exit(1)
		}
		if _, err := os.Stat(*bamlClientDir); os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "Error: baml_client directory does not exist: %s\n", *bamlClientDir)
			os.Exit(1)
		}

		fmt.Printf("Applying hacks for BAML version %s...\n", *bamlVersion)

		if err := hacks.ApplyAll(*bamlVersion, *bamlClientDir); err != nil {
			fmt.Fprintf(os.Stderr, "Error applying hacks: %v\n", err)
			os.Exit(1)
		}

		if *rewriteBAMLImportsTo != "" {
			if err := hacks.RewriteGeneratedClientBAMLImports(*bamlClientDir, *rewriteBAMLImportsTo); err != nil {
				fmt.Fprintf(os.Stderr, "Error rewriting generated-client BAML imports: %v\n", err)
				os.Exit(1)
			}
		}
	} else {
		fmt.Println("Skipping generated-client hacks per --skip-client-hacks")
	}

	if *skipBAMLModulePatch {
		fmt.Println("Skipping BAML module patching per --skip-baml-module-patch")
		fmt.Println("All applicable hacks applied successfully")
		return
	}

	if *patchedBAMLOut != "" {
		srcDir := *bamlModuleSrc
		if srcDir == "" {
			resolved, err := resolveBAMLModuleSourceDir()
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error resolving BAML module source: %v\n", err)
				os.Exit(1)
			}
			srcDir = resolved
		}
		if err := hacks.GeneratePatchedBAMLModule(srcDir, *patchedBAMLOut, *patchedBAMLModulePath, *bamlVersion); err != nil {
			fmt.Fprintf(os.Stderr, "Error generating patched BAML module: %v\n", err)
			os.Exit(1)
		}
	} else {
		if err := hacks.ApplyRuntimeDeadlockFix(*bamlVersion); err != nil {
			fmt.Fprintf(os.Stderr, "Error applying runtime deadlock fix: %v\n", err)
			os.Exit(1)
		}
		if err := hacks.ApplyDynamicOrderFix(*bamlVersion); err != nil {
			fmt.Fprintf(os.Stderr, "Error applying dynamic order fix: %v\n", err)
			os.Exit(1)
		}
		if err := hacks.ApplyBamlSerdeNilFix(*bamlVersion); err != nil {
			fmt.Fprintf(os.Stderr, "Error applying baml serde nil-value fix: %v\n", err)
			os.Exit(1)
		}
	}

	fmt.Println("All applicable hacks applied successfully")
}

// resolveBAMLModuleSourceDir invokes `go list -m -f {{.Dir}}
// github.com/boundaryml/baml` to locate the upstream module's source
// tree. Used as the default seed for GeneratePatchedBAMLModule when no
// explicit `--baml-module-src` is supplied.
func resolveBAMLModuleSourceDir() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), resolveModuleSrcTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "list", "-m", "-f", "{{.Dir}}", "github.com/boundaryml/baml")
	out, err := cmd.Output()
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", fmt.Errorf("timed out resolving baml module dir after %s", resolveModuleSrcTimeout)
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return "", fmt.Errorf("failed to resolve baml module dir: %s", strings.TrimSpace(string(exitErr.Stderr)))
		}
		return "", fmt.Errorf("failed to resolve baml module dir: %w", err)
	}
	dir := strings.TrimSpace(string(out))
	if dir == "" {
		return "", fmt.Errorf("`go list` returned empty BAML module dir")
	}
	return dir, nil
}
