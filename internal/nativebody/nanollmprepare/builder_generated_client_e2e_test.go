//go:build nanollm_integration

package nanollmprepare_test

// Generated-client packaging e2e for the de-BAML cutover Slice 2 native worker
// (second review fix). Unlike builder_packaging_e2e_test.go (which builds the
// checked-in STUB tree, where root's InitBamlRuntime imports nothing), this test
// reproduces the real builder's generated-client dependency closure and the
// actual go.mod-overlay preparation step:
//
//  1. Assemble a build context (like the embed bundle: no isolated module) and
//     restore the isolated module from the committed opaque tar.
//  2. Stand in for `npx baml generate` + `go mod init ./baml_client` by creating
//     a real nested baml_client module, and for the adapter generator by
//     rewriting root's adapter.go so InitBamlRuntime imports and calls
//     baml_client.InitRuntime — the exact import edge that makes the worker (via
//     root) transitively need the generated client.
//  3. Prove the gap the review flagged: WITHOUT the overlay, the GOWORK=off
//     worker build cannot resolve the nested baml_client and fails.
//  4. Run the ACTUAL overlay step build.sh runs (go run ./cmd/build/
//     nativeworker-overlay), then build the BAML+nanollm worker — it now
//     resolves the local generated client — embed it, build the CGO-free host,
//     and rerun the host-zero-nanollm checks.
//
// Gated by nanollm_integration (links nanollm); skipped under -short (copies the
// repo and runs several Go builds).

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNativeWorkerGeneratedClientE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("copies the repo and runs multiple Go builds; skipped in -short")
	}

	repoRoot, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	tarPath := filepath.Join(repoRoot, "cmd", "build", "nativeworker_module.tar")
	if _, err := os.Stat(tarPath); err != nil {
		t.Fatalf("committed opaque asset missing: %v", err)
	}

	ctx := t.TempDir()
	// Canonicalize: on macOS t.TempDir() is under /var (a symlink to
	// /private/var). `go work use` writes canonicalized paths, so a workspace
	// rooted at the /var alias mis-resolves module membership. EvalSymlinks keeps
	// GOWORK, cmd.Dir, and the go tool's canonicalization consistent.
	if real, err := filepath.EvalSymlinks(ctx); err == nil {
		ctx = real
	}
	const modRel = "internal/nativebody/nanollmprepare"

	// (1) Module-less context (mirrors the embed bundle), then tar-restore.
	skip := map[string]bool{".jj": true, ".git": true, "node_modules": true, "target": true}
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
		if d.IsDir() && skip[d.Name()] {
			return filepath.SkipDir
		}
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
			return nil
		}
		return copyFile(path, dst)
	})
	if err != nil {
		t.Fatalf("assembling context: %v", err)
	}
	if out, err := runIn(ctx, nil, "tar", "-xf", tarPath); err != nil {
		t.Fatalf("tar extract: %v\n%s", err, out)
	}

	// (2) Stand-in generated client + the adapter generator's import edge.
	bamlClientDir := filepath.Join(ctx, "baml_client")
	if err := os.MkdirAll(bamlClientDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeString(t, filepath.Join(bamlClientDir, "go.mod"),
		"module github.com/invakid404/baml-rest/baml_client\n\ngo 1.26.5\n")
	// A unique symbol lets us confirm the LOCAL generated client is what linked.
	writeString(t, filepath.Join(bamlClientDir, "client.go"),
		"package baml_client\n\n// GeneratedClientMarker proves the local generated client linked.\nconst GeneratedClientMarker = \"nativeworker-e2e-generated-client\"\n\nfunc InitRuntime() {}\n")
	// Rewrite root adapter.go (the generator overwrites it in a real build) so
	// InitBamlRuntime imports + calls the generated client.
	writeString(t, filepath.Join(ctx, "adapter.go"), `package baml_rest

import (
	"context"

	_ "github.com/enriquebris/goconcurrentqueue"

	baml_client "github.com/invakid404/baml-rest/baml_client"
	"github.com/invakid404/baml-rest/bamlutils"
)

var Methods = map[string]bamlutils.StreamingMethod{}

var ParseMethods = map[string]bamlutils.ParseMethod{}

func MakeAdapter(context.Context) bamlutils.Adapter { return (bamlutils.Adapter)(nil) }

func InitBamlRuntime() { baml_client.InitRuntime(); _ = baml_client.GeneratedClientMarker }
`)

	modDir := filepath.Join(ctx, modRel)
	workerOut := filepath.Join(ctx, "cmd", "serve", "worker")
	buildEnv := envSet(os.Environ(), "GOWORK=off", "CGO_ENABLED=1")

	// (3) Negative control: without the overlay the nested baml_client is
	// unresolvable under GOWORK=off.
	out, err := runIn(modDir, buildEnv, "go", "build", "-tags=subprocess", "-o", workerOut, "./cmd/worker")
	if err == nil {
		t.Fatal("expected the pre-overlay worker build to FAIL (baml_client unresolved)")
	}
	if !strings.Contains(out, "baml_client") {
		t.Fatalf("pre-overlay build failed for an unexpected reason (want baml_client):\n%s", out)
	}

	// (4) Run the ACTUAL overlay step, then build — it must now resolve the
	// local generated client.
	if out, err := runIn(ctx, nil, "go", "run", "./cmd/build/nativeworker-overlay",
		"--module-dir", modRel,
		"--baml-client", "../../../baml_client",
		"--baml-version", "v0.223.0",
	); err != nil {
		t.Fatalf("overlay step failed: %v\n%s", err, out)
	}
	modEnv := envSet(os.Environ(), "GOWORK=off", "CGO_ENABLED=1", "GOFLAGS=-mod=mod")
	if out, err := runIn(modDir, modEnv, "go", "build", "-tags=subprocess", "-o", workerOut, "./cmd/worker"); err != nil {
		t.Fatalf("post-overlay worker build failed: %v\n%s", err, out)
	}
	if out, err := runIn(ctx, nil, "go", "tool", "nm", workerOut); err != nil {
		t.Fatalf("nm worker: %v\n%s", err, out)
	} else if !strings.Contains(strings.ToLower(out), "nanollm") {
		t.Fatal("native worker did not link nanollm")
	}

	// (4 cont.) The HOST build is the normal root/workspace build and also
	// imports baml_client (via the same generated adapter.go). The real builder
	// exposes the generated client to it with `go work use ./baml_client`
	// (build.sh) — GOWORK=off is only for the isolated worker. Mirror that here,
	// then build the CGO-free host embedding the worker and assert zero nanollm.
	// The host build is a workspace build, so point GOWORK at the context's own
	// go.work (the test process itself runs under GOWORK=off, which these steps
	// would otherwise inherit).
	wsEnv := envSet(os.Environ(), "GOWORK="+filepath.Join(ctx, "go.work"))
	if out, err := runIn(ctx, wsEnv, "go", "work", "use", "./baml_client"); err != nil {
		t.Fatalf("go work use ./baml_client: %v\n%s", err, out)
	}
	hostBin := filepath.Join(ctx, "host-subprocess")
	hostEnv := envSet(wsEnv, "CGO_ENABLED=0")
	if out, err := runIn(ctx, hostEnv, "go", "build", "-tags=subprocess", "-o", hostBin, "./cmd/serve"); err != nil {
		t.Fatalf("building CGO-free host: %v\n%s", err, out)
	}
	if out, err := runIn(ctx, hostEnv, "go", "list", "-deps", "-tags", "subprocess", "./cmd/serve"); err != nil {
		t.Fatalf("go list -deps host: %v\n%s", err, out)
	} else if strings.Contains(out, "github.com/viktordanov/nanollm") {
		t.Fatal("host import graph references nanollm")
	}
	if out, err := runIn(ctx, nil, "go", "tool", "nm", hostBin); err != nil {
		t.Fatalf("nm host: %v\n%s", err, out)
	} else if strings.Contains(strings.ToLower(out), "nanollm") {
		t.Fatal("host symbol table carries nanollm symbols")
	}
}

func writeString(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// envSet returns a copy of base with each override applied by KEY: any existing
// "KEY=..." entry is removed before the override is appended, so the resulting
// environment has exactly one entry per overridden key (the test process may
// already carry e.g. GOWORK=off, which a plain append would not shadow reliably).
func envSet(base []string, overrides ...string) []string {
	keys := make(map[string]bool, len(overrides))
	for _, o := range overrides {
		if i := strings.IndexByte(o, '='); i >= 0 {
			keys[o[:i]] = true
		}
	}
	out := make([]string, 0, len(base)+len(overrides))
	for _, e := range base {
		i := strings.IndexByte(e, '=')
		if i >= 0 && keys[e[:i]] {
			continue
		}
		out = append(out, e)
	}
	return append(out, overrides...)
}
