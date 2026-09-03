//go:build nanollm_integration

package main

// BAML+nanollm SERVE-capable worker startup smoke test (de-BAML cutover Slice 6).
//
// It proves the PACKAGING + flag-gating claim end to end: the isolated worker
// built from this out-of-go.work module (GOWORK=off + CGO, nanollm linked) boots,
// serves the go-plugin handler, and reports the correct startup diagnostic for
// BOTH umbrella-flag states:
//
//   - FLAG ON (default / unset): both native runtimes initialize at startup, the
//     serve factory is installed, and the diagnostic reports
//     native_build_capable=true, native_runtime_initialized=true,
//     rollout_mode=serve, native_serving=eligible, engine "nanollm".
//   - FLAG OFF (BAML_REST_USE_DEBAML=0): ZERO native FFI at boot (no capability
//     Version probe, no runtime init, no serve factory) — yet the binary still
//     advertises a STATIC build capability, so the diagnostic reports
//     native_build_capable=true, native_runtime_initialized=false,
//     rollout_mode=off, native_serving=off. This is the flag-off kill switch:
//     the serve-capable binary behaves exactly like the BAML-only worker.
//
// Mechanism (no gRPC client needed): build the worker once, exec it with the
// go-plugin magic cookie under each flag state, and assert the handshake line
// (`<core>|<app>|<net>|<addr>|grpc|`, emitted only after startup succeeds) plus
// the startup diagnostic fields.
//
// Gated by nanollm_integration so the default (no-tag) build never needs nanollm
// or a C toolchain; it runs in the nanollm-prepare / nanollm-send lanes.

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/invakid404/baml-rest/workerplugin"
)

// buildNativeWorker builds the serve-capable BAML+nanollm worker exactly as
// build.sh's NATIVE_WORKER variant does (GOWORK=off + CGO + the subprocess +
// profile-neutral registry tags) and returns its path. Since ExecBridge-U1c the standard
// serve worker's serveProfileOptions installs standardspineoracle.NewStaticServe, which
// drives NewExecutor over the generated spine registry, so the smoke build must GENERATE
// a registry (an empty, all-decline one is enough to boot) and carry the generic tag —
// the production builder guarantees the same, and a missing registry must fail loud, not
// silently degrade to all-BAML.
func buildNativeWorker(t *testing.T) string {
	t.Helper()
	generateEmptyNativeSpineRegistry(t)
	bin := filepath.Join(t.TempDir(), "worker-native")
	buildCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	build := exec.CommandContext(buildCtx, "go", "build", "-tags=subprocess,debamlnativespinegenerated", "-o", bin, ".")
	build.Env = append(os.Environ(), "GOWORK=off", "CGO_ENABLED=1")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building serve-capable BAML+nanollm worker failed: %v\n%s", err, out)
	}
	return bin
}

// generateEmptyNativeSpineRegistry generates an all-decline (empty-population) native
// spine registry into internal/nativebody/nanollmprepare/nativegenerated so the standard
// worker built with the generic tag links a real (all-decline) aggregate rather than the
// fail-loud stub — enough for the boot smoke, where every request would fall back to
// BAML. It restores the committed stub-only state on cleanup.
func generateEmptyNativeSpineRegistry(t *testing.T) {
	t.Helper()
	root := repoRootDir(t)
	genDir := filepath.Join(root, "internal", "nativebody", "nanollmprepare", "nativegenerated")

	// gen-native-spine-worker is a ROOT-module command; run it from the repo root under
	// the workspace (it needs no CGO). --empty writes an all-decline generated.go +
	// project.json into the committed registry package (whose only checked-in file is the
	// stub) from a minimal valid project.
	gen := exec.Command("go", "run", "./cmd/gen-native-spine-worker",
		"--empty",
		"--out-dir", "internal/nativebody/nanollmprepare/nativegenerated")
	gen.Dir = root
	if out, err := gen.CombinedOutput(); err != nil {
		t.Fatalf("generating empty native spine registry: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		// Restore the committed stub-only state (an empty population emits no subpackage).
		_ = os.Remove(filepath.Join(genDir, "generated.go"))
		_ = os.Remove(filepath.Join(genDir, "project.json"))
	})
}

// repoRootDir walks up from the test's working directory to the repo root (the directory
// carrying go.work).
func repoRootDir(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.work")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("repo root (go.work) not found above the test working directory")
		}
		dir = parent
	}
}

// envWithoutClientDefaults returns os.Environ() with any BAML_REST_CLIENT_DEFAULTS
// entry removed, so an inherited (developer/CI) value can't reach the worker under
// test. workerboot.Run parses that var before the go-plugin handshake and exits on
// a malformed value; filtering it keeps the boot-smoke test's outcome independent
// of the ambient environment. Every other inherited variable is preserved.
func envWithoutClientDefaults() []string {
	src := os.Environ()
	out := make([]string, 0, len(src))
	for _, kv := range src {
		if strings.HasPrefix(kv, "BAML_REST_CLIENT_DEFAULTS=") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// bootWorkerStderr execs bin with the go-plugin magic cookie plus extraEnv, waits
// for the handshake line to prove the handler is serving, and returns the startup
// diagnostic stderr.
func bootWorkerStderr(t *testing.T, bin string, extraEnv ...string) string {
	t.Helper()
	runCtx, cancelRun := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancelRun()
	cmd := exec.CommandContext(runCtx, bin)
	// Inherit the ambient env EXCEPT BAML_REST_CLIENT_DEFAULTS: workerboot.Run
	// parses that var BEFORE the go-plugin handshake and EXITS on a malformed
	// value, so a malformed developer/CI ambient value would spuriously fail this
	// packaging/flag boot-smoke test before it reaches the handshake. Every other
	// inherited variable is preserved.
	cmd.Env = append(envWithoutClientDefaults(),
		workerplugin.Handshake.MagicCookieKey+"="+workerplugin.Handshake.MagicCookieValue,
	)
	cmd.Env = append(cmd.Env, extraEnv...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting worker: %v", err)
	}

	handshakeCh := make(chan string, 1)
	scanDone := make(chan struct{})
	go func() {
		defer close(scanDone)
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if line := scanner.Text(); strings.Contains(line, "|grpc|") {
				handshakeCh <- line
				return
			}
		}
		close(handshakeCh)
	}()

	var joinOnce sync.Once
	join := func() {
		joinOnce.Do(func() {
			_ = cmd.Process.Kill()
			<-scanDone
			_ = cmd.Wait()
		})
	}
	defer join()

	select {
	case line, ok := <-handshakeCh:
		join()
		if !ok || line == "" {
			t.Fatalf("worker exited before emitting a go-plugin handshake; stderr:\n%s", stderr.String())
		}
		return stderr.String()
	case <-runCtx.Done():
		join()
		t.Fatalf("timed out waiting for worker handshake; stderr:\n%s", stderr.String())
		return ""
	}
}

func TestServeCapableWorkerBootSmoke(t *testing.T) {
	if testing.Short() {
		t.Skip("boot smoke builds + execs a worker binary; skipped in -short")
	}
	bin := buildNativeWorker(t)

	t.Run("flag on serves", func(t *testing.T) {
		// Explicit BAML_REST_USE_DEBAML=1 (not merely relying on the default-on
		// resolution) so an inherited BAML_REST_USE_DEBAML=0 in CI/dev can't select
		// the flag-off branch and fail this case. The serve factory is installed and
		// both runtimes initialize.
		errLog := bootWorkerStderr(t, bin, "BAML_REST_USE_DEBAML=1")
		for _, want := range []string{
			`"debaml_flag_enabled":true`,
			`"native_engine":"nanollm"`,
			`"native_build_capable":true`,
			`"native_runtime_initialized":true`,
			`"rollout_mode":"serve"`,
			`"native_serving":"eligible"`,
		} {
			if !strings.Contains(errLog, want) {
				t.Fatalf("flag-on serve diagnostic missing %s; stderr:\n%s", want, errLog)
			}
		}
	})

	t.Run("flag off is zero-native kill switch", func(t *testing.T) {
		// BAML_REST_USE_DEBAML=0 => flag off: no serve factory, no runtime init, no
		// FFI — but the static build capability is still advertised.
		errLog := bootWorkerStderr(t, bin, "BAML_REST_USE_DEBAML=0")
		for _, want := range []string{
			`"native_engine":"nanollm"`,
			`"native_build_capable":true`,
			`"native_runtime_initialized":false`,
			`"rollout_mode":"off"`,
			`"native_serving":"off"`,
			`"debaml_flag_enabled":false`,
		} {
			if !strings.Contains(errLog, want) {
				t.Fatalf("flag-off diagnostic missing %s; stderr:\n%s", want, errLog)
			}
		}
	})
}
