//go:build nanollm_integration

package main

// BAML+nanollm SHADOW worker startup smoke test (de-BAML cutover Slice 4).
//
// It proves the flag-gated boot contract of the shadow deploy profile — the one
// property the direct-dynclient flag-off test (dynamic/shadow_serve_integration_test.go)
// structurally CANNOT catch, because it never boots this binary and so never
// exercises the pre-flag native capability/probe wiring:
//
//   - FLAG OFF (BAML_REST_USE_DEBAML=0): the worker resolves the umbrella flag
//     FIRST and hands workerboot a zero Options, so NEITHER nanollm.Version()
//     (NewCapability) NOR nanollm.New() (ProbeRuntime) runs — ZERO nanollm FFI at
//     boot even though the archive is linked. Observable proof: the startup
//     diagnostic reports the BAML-only branch ("no native capability") and NEVER
//     the native-capability line, which is emitted only after NewCapability's FFI
//     call succeeds. The worker still boots and serves (handshake), identical to
//     the BAML-only worker.
//   - FLAG ON (BAML_REST_USE_DEBAML=1): the worker wires the native capability +
//     probe + shadow comparator, so the diagnostic reports the nanollm engine and
//     rollout_mode=shadow, with native serving still off (the comparator never
//     RoundTrips in this slice).
//
// Mechanism (no gRPC client needed): build the shadow worker exactly as build.sh's
// SHADOW_WORKER variant does (this module, GOWORK=off + CGO, subprocess tag), exec
// it with the go-plugin magic cookie plus the flag env under test, and read the
// handshake line + startup diagnostic off stdout/stderr.
//
// Gated by nanollm_integration so the default (no-tag) build never needs nanollm
// or a C toolchain.

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/workerplugin"
)

// sharedShadowWorker* cache the shadow-worker build across the whole boot-smoke
// suite. Both boot tests differ ONLY in the runtime env they exec the binary
// with — not in the binary itself — so a per-test rebuild would pay the full
// GOWORK=off + CGO shadow-worker build twice for byte-identical output. The build
// is done lazily under sync.Once on the FIRST non-short test (so `-short`, which
// skips both tests before they reach the build, still never compiles); TestMain
// removes the cached binary's temp dir after the suite finishes.
var (
	sharedShadowWorkerOnce sync.Once
	sharedShadowWorkerBin  string
	sharedShadowWorkerErr  error
	sharedShadowWorkerDir  string
)

// buildSharedShadowWorker builds the shadow worker exactly as build.sh's
// SHADOW_WORKER variant does (this module, GOWORK=off + CGO, subprocess tag)
// EXACTLY ONCE and returns the cached binary path. A build failure is reported on
// the calling test with the full build log.
func buildSharedShadowWorker(t *testing.T) string {
	t.Helper()
	sharedShadowWorkerOnce.Do(func() {
		dir, err := os.MkdirTemp("", "worker-shadow-boot")
		if err != nil {
			sharedShadowWorkerErr = fmt.Errorf("creating shadow-worker build dir: %w", err)
			return
		}
		sharedShadowWorkerDir = dir
		bin := filepath.Join(dir, "worker-shadow")

		buildCtx, cancelBuild := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancelBuild()
		build := exec.CommandContext(buildCtx, "go", "build", "-tags=subprocess", "-o", bin, ".")
		build.Env = append(os.Environ(), "GOWORK=off", "CGO_ENABLED=1")
		if out, err := build.CombinedOutput(); err != nil {
			sharedShadowWorkerErr = fmt.Errorf("building BAML+nanollm shadow worker failed: %w\n%s", err, out)
			return
		}
		sharedShadowWorkerBin = bin
	})
	if sharedShadowWorkerErr != nil {
		t.Fatalf("%v", sharedShadowWorkerErr)
	}
	return sharedShadowWorkerBin
}

// TestMain removes the shared shadow-worker build dir after the suite runs. The
// binary is built lazily (sync.Once) during the tests, so in `-short` — where
// both boot tests skip before building — there is nothing to remove.
func TestMain(m *testing.M) {
	code := m.Run()
	if sharedShadowWorkerDir != "" {
		_ = os.RemoveAll(sharedShadowWorkerDir)
	}
	os.Exit(code)
}

// bootShadowWorker boots the shadow worker with the supplied extra environment,
// reusing the suite-shared build, and returns the go-plugin handshake line (empty
// if the worker exited before handshaking) and the captured stderr.
func bootShadowWorker(t *testing.T, extraEnv ...string) (handshake, stderrOut string) {
	t.Helper()

	bin := buildSharedShadowWorker(t)

	runCtx, cancelRun := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancelRun()
	cmd := exec.CommandContext(runCtx, bin)
	cmd.Env = append(os.Environ(),
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
		t.Fatalf("starting shadow worker: %v", err)
	}

	handshakeCh := make(chan string, 1)
	scanDone := make(chan struct{})
	go func() {
		defer close(scanDone)
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.Contains(line, "|grpc|") {
				handshakeCh <- line
				return
			}
		}
		close(handshakeCh) // EOF without a handshake (early exit/crash)
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
			return "", stderr.String()
		}
		return line, stderr.String()
	case <-runCtx.Done():
		join()
		return "", stderr.String()
	}
}

// TestShadowWorkerBoot_FlagOffZeroFFI proves the kill switch at the BOOT layer:
// a flag-off shadow worker executes ZERO nanollm FFI at startup (no capability
// probe, no runtime init) and boots identically to the BAML-only worker.
func TestShadowWorkerBoot_FlagOffZeroFFI(t *testing.T) {
	if testing.Short() {
		t.Skip("boot smoke builds + execs a worker binary; skipped in -short")
	}

	// BAML_REST_USE_DEBAML is DEFAULT-ON, so the OFF case must set an explicit
	// falsy value (the exact env the operator would use to revert to 100% BAML).
	handshake, errLog := bootShadowWorker(t, bamlutils.EnvUseDeBAML+"=0")

	if handshake == "" {
		t.Fatalf("flag-off shadow worker exited before emitting a go-plugin handshake; stderr:\n%s", errLog)
	}
	// The FFI proof: with the flag off the worker takes the BAML-only branch, so
	// NewCapability (nanollm.Version) was never called and the native-capability
	// line is absent. If any nanollm FFI had run at boot, this diagnostic would
	// carry the nanollm engine instead.
	if !strings.Contains(errLog, "no native capability (BAML-only worker)") {
		t.Fatalf("flag-off shadow worker did not report the BAML-only (zero-native) startup; stderr:\n%s", errLog)
	}
	if strings.Contains(errLog, `"native_engine":"nanollm"`) {
		t.Fatalf("flag-off shadow worker reported native capability — nanollm FFI ran at boot (kill switch violated); stderr:\n%s", errLog)
	}
	if !strings.Contains(errLog, `"rollout_mode":"off"`) {
		t.Fatalf("flag-off shadow worker did not report rollout_mode=off; stderr:\n%s", errLog)
	}
	if strings.Contains(errLog, `"rollout_mode":"shadow"`) {
		t.Fatalf("flag-off shadow worker installed the shadow comparator; expected rollout_mode off; stderr:\n%s", errLog)
	}
}

// TestShadowWorkerBoot_FlagOnWiresNativeAndShadow proves the flag-on shadow build
// wires the native capability + probe + comparator: the diagnostic reports the
// nanollm engine and rollout_mode=shadow, with native serving still off (the
// comparator never RoundTrips in this slice).
func TestShadowWorkerBoot_FlagOnWiresNativeAndShadow(t *testing.T) {
	if testing.Short() {
		t.Skip("boot smoke builds + execs a worker binary; skipped in -short")
	}

	handshake, errLog := bootShadowWorker(t, bamlutils.EnvUseDeBAML+"=1")

	if handshake == "" {
		t.Fatalf("flag-on shadow worker exited before emitting a go-plugin handshake; stderr:\n%s", errLog)
	}
	if !strings.Contains(errLog, `"native_engine":"nanollm"`) {
		t.Fatalf("flag-on shadow worker did not report the nanollm native capability; stderr:\n%s", errLog)
	}
	if !strings.Contains(errLog, `"rollout_mode":"shadow"`) {
		t.Fatalf("flag-on shadow worker did not report rollout_mode=shadow; stderr:\n%s", errLog)
	}
	// Native serving stays off in this slice: the comparator declines after a
	// no-socket plan comparison, so nothing is ever served natively.
	if !strings.Contains(errLog, `"native_serving":"off"`) {
		t.Fatalf("flag-on shadow worker must report native serving off; stderr:\n%s", errLog)
	}
}
