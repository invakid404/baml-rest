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
// build.sh's NATIVE_WORKER variant does (GOWORK=off + CGO + subprocess tag) and
// returns its path.
func buildNativeWorker(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "worker-native")
	buildCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	build := exec.CommandContext(buildCtx, "go", "build", "-tags=subprocess", "-o", bin, ".")
	build.Env = append(os.Environ(), "GOWORK=off", "CGO_ENABLED=1")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building serve-capable BAML+nanollm worker failed: %v\n%s", err, out)
	}
	return bin
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
