//go:build nanollm_integration

package nanollmprepare_test

// End-to-end builder packaging test for the de-BAML cutover Slice 2 native
// worker variant. It reproduces the production path the real builder
// (cmd/build + build.sh with NATIVE_WORKER=true) takes:
//
//  1. Assemble a build context that EXCLUDES the isolated nanollmprepare module
//     (exactly how cmd/build lays down bamlrest.Sources — the module is
//     .embedignore'd) by copying the repo minus that directory.
//  2. Restore the module from the committed opaque asset
//     (cmd/build/nativeworker_module.tar) with `tar -xf`, as build.sh does.
//  3. Build the BAML+nanollm worker FROM the restored module with GOWORK=off +
//     CGO, and drop it at the host embed location (cmd/serve/worker).
//  4. Build the subprocess host with CGO_ENABLED=0 (embedding that worker) and
//     rerun the host-zero-nanollm checks: the host import graph and symbol table
//     must contain no nanollm even though it embeds a nanollm-linked worker.
//
// This proves the central Slice-2 deliverable is reachable through the packaging
// path (opaque asset -> extract -> build -> embed), not just a direct
// full-checkout `go build`. Gated by nanollm_integration (it links nanollm) and
// skipped under -short (it copies the repo and runs two Go builds).

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestNativeWorkerBuilderPackagingE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("copies the repo and runs two Go builds; skipped in -short")
	}

	// CWD is this package dir (the module root); the repo root is three levels
	// up (internal/nativebody/nanollmprepare -> repo root).
	repoRoot, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	tarPath := filepath.Join(repoRoot, "cmd", "build", "nativeworker_module.tar")
	if _, err := os.Stat(tarPath); err != nil {
		t.Fatalf("committed opaque asset missing: %v", err)
	}

	ctx := t.TempDir()

	// (1) Build context WITHOUT the isolated module (mirrors the embed bundle,
	// which excludes it), skipping VCS/build junk for speed.
	const modRel = "internal/nativebody/nanollmprepare"
	skipDirName := map[string]bool{".jj": true, ".git": true, "node_modules": true, "target": true}
	err = filepath.WalkDir(repoRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(repoRoot, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if d.IsDir() && skipDirName[d.Name()] {
			return filepath.SkipDir
		}
		// Exclude the isolated module entirely — it must arrive via the tar.
		if rel == modRel || strings.HasPrefix(rel, modRel+string(filepath.Separator)) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		dst := filepath.Join(ctx, rel)
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		if !d.Type().IsRegular() {
			return nil // skip symlinks/irregular entries
		}
		return copyFile(path, dst)
	})
	if err != nil {
		t.Fatalf("assembling module-less build context: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ctx, modRel)); err == nil {
		t.Fatalf("precondition failed: isolated module present in context before tar restore")
	}

	// (2) Restore the module from the opaque asset, exactly as build.sh does.
	if out, err := runIn(ctx, nil, "tar", "-xf", tarPath); err != nil {
		t.Fatalf("tar extract failed: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(ctx, modRel, "go.mod")); err != nil {
		t.Fatalf("module not restored from tar: %v", err)
	}

	// (3) Build the BAML+nanollm worker from the restored module and drop it at
	// the host embed location.
	workerOut := filepath.Join(ctx, "cmd", "serve", "worker")
	buildEnv := append(os.Environ(), "GOWORK=off", "CGO_ENABLED=1")
	if out, err := runIn(filepath.Join(ctx, modRel), buildEnv,
		"go", "build", "-tags=subprocess", "-o", workerOut, "./cmd/worker"); err != nil {
		t.Fatalf("building native worker from restored module: %v\n%s", err, out)
	}
	// Sanity: the worker itself IS nanollm-linked (this is the native variant).
	if out, err := runIn(ctx, nil, "go", "tool", "nm", workerOut); err != nil {
		t.Fatalf("nm worker: %v\n%s", err, out)
	} else if !strings.Contains(strings.ToLower(out), "nanollm") {
		t.Fatal("expected the native worker to carry nanollm symbols; it did not")
	}

	// (4) Build the CGO-free subprocess host embedding that worker, and assert
	// the host stays zero-nanollm (link graph + symbol table).
	hostBin := filepath.Join(ctx, "host-subprocess")
	hostEnv := append(os.Environ(), "CGO_ENABLED=0")
	if out, err := runIn(ctx, hostEnv,
		"go", "build", "-tags=subprocess", "-o", hostBin, "./cmd/serve"); err != nil {
		t.Fatalf("building CGO-free subprocess host: %v\n%s", err, out)
	}
	if out, err := runIn(ctx, hostEnv, "go", "list", "-deps", "-tags", "subprocess", "./cmd/serve"); err != nil {
		t.Fatalf("go list -deps host: %v\n%s", err, out)
	} else if strings.Contains(out, "github.com/viktordanov/nanollm") {
		t.Fatal("host import graph transitively references nanollm")
	}
	if out, err := runIn(ctx, nil, "go", "tool", "nm", hostBin); err != nil {
		t.Fatalf("nm host: %v\n%s", err, out)
	} else if strings.Contains(strings.ToLower(out), "nanollm") {
		t.Fatal("host binary symbol table carries nanollm symbols (embedded worker payload must stay opaque)")
	}
}

// runIn runs a command in dir with optional extra env and returns combined
// output. A nil env inherits the process environment.
func runIn(dir string, env []string, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if env != nil {
		cmd.Env = env
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, info.Mode().Perm())
}
