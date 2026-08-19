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
//     FIRST and hands workerboot ONLY a static build-capability advertisement, so
//     NEITHER nanollm.Version() (NewCapability) NOR nanollm.New() (ProbeRuntime)
//     runs — ZERO nanollm FFI at boot even though the archive is linked. The
//     worker still boots and serves (handshake), and it declines everything to
//     BAML exactly like the BAML-only worker.
//   - FLAG ON (BAML_REST_USE_DEBAML=1): the worker wires the native capability +
//     probe + shadow comparator, so the diagnostic reports the nanollm engine and
//     rollout_mode=shadow, with native serving still off (the comparator never
//     RoundTrips in this slice).
//
// WHAT COUNTS AS FFI EVIDENCE, AND WHAT DOES NOT. This file used to read the
// flag-off proof off the message form ("no native capability (BAML-only worker)")
// and off the ABSENCE of `"native_engine":"nanollm"`. Both stopped being true
// facts about FFI once the de-BAML serving-cutover S2 slice made this binary an
// attested NATIVE-CAPABLE artifact: the build stamps it `native_capable`, and the
// flag-off branch must therefore advertise that same static build fact, or
// workerboot derives `baml_only`, contradicts the stamp and refuses to serve —
// which would turn the kill switch itself into an outage.
//
// So the engine NAME is not FFI evidence: it is a compile-time constant
// (nativeworker.EngineName) that the flag-off branch reports without touching the
// engine. The fields that ARE evidence, and that this file now asserts, are:
//
//	native_runtime_initialized  false  — ProbeRuntime (nanollm.New) never ran
//	native_engine_version       ""     — resolving a version is nanollm.Version(),
//	                                     i.e. the capability FFI; an empty version
//	                                     alongside a present engine name is exactly
//	                                     "linked but never called"
//	rollout_mode                off    — no comparator was installed
//	native_serving              off
//
// plus the positive half of the S2 contract: artifact_profile stays
// native_capable and native_build_capable stays true, because the artifact IS
// native-capable — it is merely doing nothing native.
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

// bootShadowWorker boots the shadow worker with the supplied extra environment,
// reusing the suite-shared build, and returns the go-plugin handshake line (empty
// if the worker exited before handshaking) and the captured stderr.
func bootShadowWorker(t *testing.T, extraEnv ...string) (handshake, stderrOut string) {
	t.Helper()

	bin := buildSharedShadowWorker(t)

	runCtx, cancelRun := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancelRun()
	cmd := exec.CommandContext(runCtx, bin)
	// Inherit the ambient env EXCEPT BAML_REST_CLIENT_DEFAULTS: workerboot.Run
	// parses that var BEFORE the go-plugin handshake and EXITS on a malformed
	// value, so a malformed ambient value would spuriously fail this boot-smoke
	// test before it reaches the handshake. Every other inherited var is preserved.
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
// probe, no runtime init), still boots and serves, and still attests the
// native-capable artifact identity the build stamped into it.
//
// See the file header for why the engine NAME is not FFI evidence and
// native_engine_version / native_runtime_initialized are.
func TestShadowWorkerBoot_FlagOffZeroFFI(t *testing.T) {
	if testing.Short() {
		t.Skip("boot smoke builds + execs a worker binary; skipped in -short")
	}

	// BAML_REST_USE_DEBAML is DEFAULT-ON, so the OFF case must set an explicit
	// falsy value (the exact env the operator would use to revert to 100% BAML).
	handshake, errLog := bootShadowWorker(t, bamlutils.EnvUseDeBAML+"=0")

	// Booting AT ALL is half the contract. A flag-off native-capable artifact that
	// exits before the handshake is not a kill switch, it is an outage — the exact
	// failure this profile shipped with until the S2 repair.
	if handshake == "" {
		t.Fatalf("flag-off shadow worker exited before emitting a go-plugin handshake; stderr:\n%s", errLog)
	}

	// THE FFI PROOF. ProbeRuntime (nanollm.New) never ran, and no capability was
	// resolved — resolving one calls nanollm.Version(), which would have filled in
	// a non-empty engine version. An engine NAME with an EMPTY version is precisely
	// "the archive is linked and was never called".
	for _, want := range []string{
		`"native_runtime_initialized":false`,
		`"native_engine_version":""`,
		`"rollout_mode":"off"`,
		`"native_serving":"off"`,
	} {
		if !strings.Contains(errLog, want) {
			t.Fatalf("flag-off shadow worker did not report %s — the zero-FFI kill-switch contract is not met; stderr:\n%s", want, errLog)
		}
	}
	if strings.Contains(errLog, `"rollout_mode":"shadow"`) {
		t.Fatalf("flag-off shadow worker installed the shadow comparator; expected rollout_mode off; stderr:\n%s", errLog)
	}
	if strings.Contains(errLog, `"native_runtime_initialized":true`) {
		t.Fatalf("flag-off shadow worker initialized the native runtime — nanollm FFI ran at boot (kill switch violated); stderr:\n%s", errLog)
	}

	// THE ARTIFACT-IDENTITY HALF (de-BAML serving cutover S2). The build stamps
	// this binary native_capable; the flag-off branch must advertise the same
	// static build fact, or workerboot derives baml_only, the attestation rejects
	// the stamp, and the process exits without serving any BAML.
	for _, want := range []string{
		`"native_build_capable":true`,
		`"artifact_profile":"native_capable"`,
	} {
		if !strings.Contains(errLog, want) {
			t.Fatalf("flag-off shadow worker did not report %s — a native-capable artifact must keep attesting its own profile with the flag off; stderr:\n%s", want, errLog)
		}
	}
	if strings.Contains(errLog, `"artifact_profile":"baml_only"`) {
		t.Fatalf("flag-off shadow worker derived the baml_only profile; its native_capable build stamp would be contradicted and it would refuse to serve; stderr:\n%s", errLog)
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
	// The flag-ON FFI evidence, and the discriminator against the flag-off line:
	// a NON-EMPTY engine version can only come from nanollm.Version(), so this is
	// what shows the capability was really resolved rather than advertised.
	if strings.Contains(errLog, `"native_engine_version":""`) {
		t.Fatalf("flag-on shadow worker reported an EMPTY native engine version; the capability was advertised, not resolved, so nanollm.Version() never ran; stderr:\n%s", errLog)
	}
	if !strings.Contains(errLog, `"native_runtime_initialized":true`) {
		t.Fatalf("flag-on shadow worker did not initialize the native runtime; stderr:\n%s", errLog)
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
