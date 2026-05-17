package hacks

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// RewriteGeneratedClientBAMLImports rewrites every
// `github.com/boundaryml/baml/...` import inside the generated
// baml_client tree rooted at clientDir so it points at the supplied
// patched-fork module path instead. The walker reuses the same Go AST
// rewrite the patched-module generator runs against its own copy of
// the BAML sources, so the two trees stay import-aligned.
//
// targetModulePath must be the patched fork's declared `module` line
// (typically the same value passed to GeneratePatchedBAMLModule's
// modulePath). When it equals the upstream module path no work is
// performed.
func RewriteGeneratedClientBAMLImports(clientDir, targetModulePath string) error {
	clientDir = strings.TrimSpace(clientDir)
	targetModulePath = strings.TrimSpace(targetModulePath)
	if clientDir == "" {
		return fmt.Errorf("generated client directory is required to rewrite BAML imports")
	}
	if targetModulePath == "" {
		return fmt.Errorf("target module path is required to rewrite BAML imports")
	}
	if targetModulePath == upstreamBAMLModulePath {
		fmt.Printf("  Skipping BAML import rewrite in %s (target equals upstream)\n", clientDir)
		return nil
	}
	if _, err := os.Stat(clientDir); err != nil {
		return fmt.Errorf("inspecting generated-client dir %s: %w", clientDir, err)
	}
	count := 0
	err := filepath.WalkDir(clientDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		count++
		return rewriteBAMLImportsInFile(path, upstreamBAMLModulePath, targetModulePath)
	})
	if err != nil {
		return err
	}
	fmt.Printf("  Rewrote BAML imports in %d generated-client files under %s -> %s\n", count, clientDir, targetModulePath)
	return nil
}
