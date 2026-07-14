//go:build nanollm_integration

package main

// BAML+nanollm worker startup smoke test (de-BAML cutover Slice 2).
//
// It proves the PACKAGING claim end to end: the isolated worker built from this
// out-of-go.work module (GOWORK=off + CGO, nanollm linked) boots, initializes
// BOTH native runtimes at startup, reports itself native-capable, and serves the
// go-plugin handler — with NO request routing. It intentionally does not send a
// request: routing stays hard-off in this slice.
//
// Mechanism (no gRPC client needed): build the worker, exec it with the
// go-plugin magic cookie, and assert two facts:
//
//   - stdout carries the go-plugin handshake line (`<core>|<app>|<net>|<addr>|
//     grpc|`) — emitted only AFTER InitRuntime (BAML), ProbeRuntime (nanollm),
//     worker.New, and goplugin.Serve all succeed, i.e. the handler is serving;
//   - stderr carries the native-capability startup diagnostic naming engine
//     "nanollm" with routing off — the build capability, hard-off.
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

func TestBAMLNanollmWorkerBootSmoke(t *testing.T) {
	if testing.Short() {
		t.Skip("boot smoke builds + execs a worker binary; skipped in -short")
	}

	bin := filepath.Join(t.TempDir(), "worker-native")

	// Build the BAML+nanollm worker exactly as build.sh's NATIVE_WORKER variant
	// does: from this module with GOWORK=off + CGO and the subprocess tag. The
	// nanollm archive links via the nativeworker import; no nanollm_integration
	// tag is needed because the bridge is untagged production code now.
	buildCtx, cancelBuild := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancelBuild()
	build := exec.CommandContext(buildCtx, "go", "build", "-tags=subprocess", "-o", bin, ".")
	build.Env = append(os.Environ(), "GOWORK=off", "CGO_ENABLED=1")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building BAML+nanollm worker failed: %v\n%s", err, out)
	}

	runCtx, cancelRun := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancelRun()
	cmd := exec.CommandContext(runCtx, bin)
	// go-plugin refuses to serve unless the magic cookie is present; supply it
	// (and nothing else — the worker resolves defaults with no env), so the
	// handshake proves an unconfigured worker still boots.
	cmd.Env = append(os.Environ(),
		workerplugin.Handshake.MagicCookieKey+"="+workerplugin.Handshake.MagicCookieValue,
	)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("starting worker: %v", err)
	}
	// Read stdout until the go-plugin handshake line appears or the pipe closes.
	// scanDone closes when the scanner goroutine has finished reading the pipe,
	// so join() can safely reap the command without racing an in-flight read.
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

	// join reaps the process AND joins os/exec's stderr-copy goroutine so the
	// captured stderr is safe to read afterward: kill the (long-serving) worker,
	// wait for the stdout scanner to finish reading, then cmd.Wait(). Using
	// cmd.Wait() (not Process.Wait()) is what joins the stderr copier. Idempotent
	// so the deferred safety-net call and the explicit pre-read calls are safe.
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
		// Handshake proves the handler is serving. Now assert the startup
		// diagnostic reported the native (nanollm) build capability, hard-off.
		errLog := stderr.String()
		if !strings.Contains(errLog, "native send capability linked") ||
			!strings.Contains(errLog, `"native_engine":"nanollm"`) {
			t.Fatalf("worker booted but did not report the nanollm native capability; stderr:\n%s", errLog)
		}
		if !strings.Contains(errLog, `"native_routing":"off"`) {
			t.Fatalf("native routing must be reported off (hard-off in this slice); stderr:\n%s", errLog)
		}
	case <-runCtx.Done():
		join()
		t.Fatalf("timed out waiting for worker handshake; stderr:\n%s", stderr.String())
	}
}
