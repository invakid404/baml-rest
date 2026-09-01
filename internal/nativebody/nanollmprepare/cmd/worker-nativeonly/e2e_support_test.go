//go:build nanollm_integration

package main

// Shared harness for the ExecBridge-U1b native-only worker booted-command e2e.
//
// TestMain generates the deployment-specific native registry from the exact U1
// fixture (through the PRODUCTION `cmd/introspect --native-spine-descriptors` +
// `cmd/gen-native-spine-worker` commands) and builds the REAL ./cmd/worker-nativeonly
// with the same GOWORK=off, CGO_ENABLED=1, subprocess + generated-runtime tags as
// build.sh. Every test boots that binary through the real HashiCorp go-plugin
// handshake (workerplugin.Handshake) and drives it over RPC — NOT in-process.
//
// The loopback OpenAI provider's base_url is baked into the descriptor BEFORE
// generation (its port is dynamic, so it is started first), and its per-request
// behaviour is swappable so one shared provider serves the success path, the
// decline table (zero hits), and the post-claim fault (one hit).
//
// Gated by nanollm_integration so a default (no-tag) build never needs nanollm or
// a C toolchain.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
	goplugin "github.com/hashicorp/go-plugin"

	"github.com/invakid404/baml-rest/workerplugin"
)

const nativeOnlyMethod = "StaticRecursiveAliasJSON"

// generated tags mirror build.sh's native-only worker build exactly.
const nativeOnlyBuildTags = "subprocess,debamlnativeonlygenerated"

var (
	// nativeOnlyBin is the built native-only worker binary (set by TestMain).
	nativeOnlyBin string
	// providerURL is the loopback provider base_url baked into the descriptor.
	providerURL string
	// providerHits counts every request the loopback provider receives.
	providerHits atomic.Int64
	// providerHandler is the swappable per-test provider behaviour.
	providerHandler atomic.Pointer[func(w http.ResponseWriter, r *http.Request)]
)

// setProviderHandler installs the per-test provider behaviour and resets the hit
// counter. Returns a function reading the current hit count.
func setProviderHandler(h func(w http.ResponseWriter, r *http.Request)) func() int64 {
	providerHits.Store(0)
	providerHandler.Store(&h)
	return providerHits.Load
}

func okChatCompletion(w http.ResponseWriter, content string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"choices": []any{
			map[string]any{"message": map[string]any{"role": "assistant", "content": content}},
		},
	})
}

func TestMain(m *testing.M) {
	// One loopback provider for the whole suite; its handler is swapped per test.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providerHits.Add(1)
		if hp := providerHandler.Load(); hp != nil {
			(*hp)(w, r)
			return
		}
		// Default: a well-formed success, so a stray request never hangs.
		okChatCompletion(w, `{"k":1}`)
	}))
	providerURL = srv.URL + "/v1"

	code, err := func() (int, error) {
		cleanup, err := generateAndBuild()
		if cleanup != nil {
			defer cleanup()
		}
		if err != nil {
			return 1, err
		}
		return m.Run(), nil
	}()
	srv.Close()
	if err != nil {
		fmt.Fprintf(os.Stderr, "native-only e2e setup failed: %v\n", err)
		os.Exit(1)
	}
	os.Exit(code)
}

// repoPaths resolves the repo root and the nanollmprepare module root from the
// test file location.
func repoPaths() (repoRoot, moduleRoot string) {
	_, file, _, _ := runtime.Caller(0)
	// .../internal/nativebody/nanollmprepare/cmd/worker-nativeonly/e2e_support_test.go
	moduleRoot = filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	repoRoot = filepath.Clean(filepath.Join(moduleRoot, "..", "..", ".."))
	return repoRoot, moduleRoot
}

// generateAndBuild runs the production introspect + generator against the U1
// fixture (base_url = the loopback), builds the native-only worker, and returns a
// cleanup that removes the generated registry (keeping the committed stub) and the
// binary.
func generateAndBuild() (func(), error) {
	repoRoot, moduleRoot := repoPaths()
	tmp, err := os.MkdirTemp("", "u1b-e2e-*")
	if err != nil {
		return nil, err
	}
	cleanupTmp := func() { _ = os.RemoveAll(tmp) }

	// Write the U1 fixture .baml corpus with the loopback base_url baked in.
	bamlSrc := filepath.Join(tmp, "baml_src")
	if err := os.MkdirAll(bamlSrc, 0o755); err != nil {
		cleanupTmp()
		return nil, err
	}
	clients := fmt.Sprintf(`client<llm> JSONOracle {
  provider openai
  options {
    model "gpt-4o-mini"
    api_key "sk-execbridge-u1-not-a-real-secret"
    base_url %q
  }
}
`, providerURL)
	files := map[string]string{
		"clients.baml":   clients,
		"types.baml":     "type JSON = int | string | bool | JSON[] | map<string, JSON>\n",
		"functions.baml": "function StaticRecursiveAliasJSON(topic: string) -> JSON {\n  client JSONOracle\n  prompt #\"Return a JSON document describing {{ topic }}.\"#\n}\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(bamlSrc, name), []byte(body), 0o644); err != nil {
			cleanupTmp()
			return nil, err
		}
	}

	// Production descriptor emission.
	descPath := filepath.Join(tmp, "descriptors.json")
	if out, err := runGo(repoRoot, nil, "run", "./cmd/introspect",
		"--native-spine-descriptors", descPath, "--baml-src-dir", bamlSrc); err != nil {
		cleanupTmp()
		return nil, fmt.Errorf("introspect: %w\n%s", err, out)
	}

	// Production registry generation into the real nativegenerated tree.
	nativeGenDir := filepath.Join(moduleRoot, "nativegenerated")
	if out, err := runGo(repoRoot, nil, "run", "./cmd/gen-native-spine-worker",
		"--descriptors", descPath, "--out-dir", nativeGenDir); err != nil {
		cleanupTmp()
		return nil, fmt.Errorf("gen-native-spine-worker: %w\n%s", err, out)
	}
	cleanupGen := func() { removeGeneratedRegistry(nativeGenDir) }

	// Build the real command with the same GOWORK=off + CGO + tags as build.sh.
	// Use -mod=readonly (not build.sh's -mod=mod) because this builds IN PLACE in the
	// real module tree, and -mod=mod would rewrite the committed go.mod; build.sh
	// builds in a throwaway extracted tree where mutation is harmless.
	bin := filepath.Join(tmp, "worker-nativeonly")
	env := append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=readonly", "CGO_ENABLED=1")
	if out, err := runGo(moduleRoot, env, "build", "-tags="+nativeOnlyBuildTags, "-o", bin, "./cmd/worker-nativeonly/"); err != nil {
		cleanupGen()
		cleanupTmp()
		return nil, fmt.Errorf("build worker-nativeonly: %w\n%s", err, out)
	}
	nativeOnlyBin = bin

	return func() {
		cleanupGen()
		cleanupTmp()
	}, nil
}

// removeGeneratedRegistry deletes the generated aggregate, embedded descriptor,
// and every generated subpackage directory, leaving the committed stub in place.
func removeGeneratedRegistry(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			_ = os.RemoveAll(filepath.Join(dir, name))
			continue
		}
		if name == "generated.go" || name == "project.json" {
			_ = os.Remove(filepath.Join(dir, name))
		}
	}
}

func runGo(dir string, env []string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = dir
	if env != nil {
		cmd.Env = env
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// bootedWorker is a live native-only worker subprocess dispensed over go-plugin.
type bootedWorker struct {
	worker workerplugin.Worker
	kill   func()
}

// bootWorker launches the built native-only binary through the real go-plugin
// handshake — the same handshake config, protocol, and dial options the pool uses
// — and dispenses the worker. It is a REAL booted command, not an in-process
// handler.
func bootWorker(t *testing.T) *bootedWorker {
	t.Helper()
	if nativeOnlyBin == "" {
		t.Fatal("native-only binary was not built (TestMain setup failed)")
	}
	cmd := exec.Command(nativeOnlyBin)
	cmd.Env = append(os.Environ(),
		workerplugin.Handshake.MagicCookieKey+"="+workerplugin.Handshake.MagicCookieValue,
	)
	client := goplugin.NewClient(&goplugin.ClientConfig{
		HandshakeConfig:  workerplugin.Handshake,
		Plugins:          map[string]goplugin.Plugin{"worker": &workerplugin.WorkerPlugin{}},
		Cmd:              cmd,
		AllowedProtocols: []goplugin.Protocol{goplugin.ProtocolGRPC},
		GRPCDialOptions:  workerplugin.GRPCDialOptions(),
		Logger:           hclog.NewNullLogger(),
		StartTimeout:     60 * time.Second,
	})
	var once sync.Once
	kill := func() { once.Do(client.Kill) }
	t.Cleanup(kill)

	rpc, err := client.Client()
	if err != nil {
		t.Fatalf("go-plugin handshake: %v", err)
	}
	raw, err := rpc.Dispense("worker")
	if err != nil {
		t.Fatalf("dispense worker: %v", err)
	}
	w, ok := raw.(workerplugin.Worker)
	if !ok {
		t.Fatalf("dispensed %T, not a workerplugin.Worker", raw)
	}
	return &bootedWorker{worker: w, kill: kill}
}

// drainFinal collects stream results until the channel closes (bounded).
func drainFinal(t *testing.T, ch <-chan *workerplugin.StreamResult) []*workerplugin.StreamResult {
	t.Helper()
	var out []*workerplugin.StreamResult
	deadline := time.After(30 * time.Second)
	for {
		select {
		case r, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, r)
		case <-deadline:
			t.Fatalf("stream channel did not close within 30s after %d result(s)", len(out))
			return out
		}
	}
}

func callInput(topic string) []byte { return []byte(fmt.Sprintf(`{"topic":%q}`, topic)) }
func parseInput(raw string) []byte  { b, _ := json.Marshal(map[string]string{"raw": raw}); return b }
