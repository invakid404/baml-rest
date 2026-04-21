//go:build integration

package integration

import (
	"context"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/invakid404/baml-rest/integration/mockllm"
	"github.com/invakid404/baml-rest/integration/testutil"
)

// TestGracefulShutdownDrainsInFlightCall verifies that a SIGTERM delivered
// while a /call request is in-flight lets the handler run to completion and
// return 200 (the pool's graceful shutdown path drains before workers die).
//
// Regression: fasthttp's RequestCtx.Done() is tied to a server-wide s.done
// channel that fasthttp closes as the very first step of Server.Shutdown.
// When handlers derived the worker-call context from c.RequestCtx(), any
// in-flight call was canceled before the pool could drain, producing a 500.
//
// The test uses a dedicated test environment because it kills the server.
func TestGracefulShutdownDrainsInFlightCall(t *testing.T) {
	// Matrix across the BAML_REST_USE_BUILD_REQUEST toggle is handled by CI
	// via env vars; we inherit whatever the outer TestMain decided.
	setupCtx, setupCancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer setupCancel()

	adapterVersion, err := testutil.GetAdapterVersionForBAML(BAMLVersion)
	if err != nil {
		t.Fatalf("Failed to get adapter version: %v", err)
	}
	bamlSrcPath, err := findTestdataPath()
	if err != nil {
		t.Fatalf("Failed to find testdata: %v", err)
	}

	env, err := testutil.Setup(setupCtx, testutil.SetupOptions{
		BAMLSrcPath:     bamlSrcPath,
		BAMLVersion:     BAMLVersion,
		AdapterVersion:  adapterVersion,
		BAMLSource:      BAMLSourcePath,
		UseBuildRequest: UseBuildRequest,
	})
	if err != nil {
		t.Fatalf("Failed to setup dedicated shutdown env: %v", err)
	}
	// Container will be stopped by the test; Terminate on cleanup is still
	// needed to remove the container and network.
	defer func() {
		if err := env.Terminate(context.Background()); err != nil {
			t.Logf("shutdown env Terminate: %v", err)
		}
	}()

	mockClient := mockllm.NewClient(env.MockLLMURL)
	bamlClient := testutil.NewBAMLRestClient(env.BAMLRestURL)

	// Scenario with a long InitialDelayMs so a /call is reliably in-flight
	// when we send SIGTERM. The delay needs to dwarf any scheduling jitter
	// between "fire request" and "stop container".
	const inFlightDelay = 3 * time.Second
	scenario := &mockllm.Scenario{
		ID:             "shutdown-drain",
		Provider:       "openai",
		Content:        `{"name": "Shutdown", "age": 1, "tags": ["drain"]}`,
		ChunkSize:      0,
		InitialDelayMs: int(inFlightDelay / time.Millisecond),
	}
	regCtx, regCancel := context.WithTimeout(setupCtx, 10*time.Second)
	if err := mockClient.RegisterScenario(regCtx, scenario); err != nil {
		regCancel()
		t.Fatalf("RegisterScenario: %v", err)
	}
	regCancel()

	// Fire the long-running call on a background goroutine. We want it
	// in-flight when SIGTERM arrives, and want to capture its final result.
	type callResult struct {
		resp *testutil.CallResponse
		err  error
	}
	resultCh := make(chan callResult, 1)

	callCtx, callCancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer callCancel()

	var inFlight atomic.Bool
	go func() {
		inFlight.Store(true)
		resp, err := bamlClient.Call(callCtx, testutil.CallRequest{
			Method: "GetPerson",
			Input:  map[string]any{"description": "shutdown-drain"},
			Options: &testutil.BAMLOptions{
				ClientRegistry: testutil.CreateTestClient(env.MockLLMInternal, scenario.ID),
			},
		})
		inFlight.Store(false)
		resultCh <- callResult{resp: resp, err: err}
	}()

	// Give the request time to reach the worker and start waiting on the
	// mock LLM's InitialDelayMs. A short poll with the /_debug/in-flight
	// endpoint would be more precise but requires the debug build — a plain
	// sleep is sufficient for a regression test.
	time.Sleep(500 * time.Millisecond)
	if !inFlight.Load() {
		t.Fatalf("call goroutine should still be in-flight after 500ms")
	}

	// Send SIGTERM to PID 1 inside the container (Go binary). Timeout is
	// large so the graceful shutdown has room to drain even if the
	// in-flight scenario takes its full InitialDelayMs to return.
	stopTimeout := 2 * time.Minute
	stopStart := time.Now()
	if err := env.BAMLRest.Stop(setupCtx, &stopTimeout); err != nil {
		t.Fatalf("container Stop (SIGTERM): %v", err)
	}
	stopDuration := time.Since(stopStart)
	t.Logf("container stopped after %s (SIGTERM→exit)", stopDuration)

	// Collect the call result.
	var result callResult
	select {
	case result = <-resultCh:
	case <-time.After(30 * time.Second):
		t.Fatalf("call goroutine did not return within 30s of container stop")
	}

	if result.err != nil {
		t.Fatalf("in-flight call errored during graceful shutdown: %v", result.err)
	}
	if result.resp.StatusCode != 200 {
		t.Fatalf("in-flight call returned status %d during graceful shutdown (expected 200): %s",
			result.resp.StatusCode, result.resp.Error)
	}
	var person struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(result.resp.Body, &person); err != nil {
		t.Fatalf("failed to unmarshal in-flight call response: %v (body=%s)", err, result.resp.Body)
	}
	if person.Name == "" {
		t.Fatalf("response body missing name field: %s", result.resp.Body)
	}

	// The stop must have taken at least the scenario's InitialDelayMs —
	// i.e. shutdown waited for the in-flight call rather than bailing out
	// immediately. Allow a margin for scheduling.
	if stopDuration < inFlightDelay/2 {
		t.Fatalf("container stopped in %s, less than half the in-flight delay %s: "+
			"shutdown did not wait for the drain", stopDuration, inFlightDelay)
	}

	// Sanity-check container logs: the drain-order markers should be in
	// the expected sequence, and there should be no "worker call failed"
	// with "context canceled" in the shutdown window.
	logs := collectContainerLogs(t, env.BAMLRest)
	assertShutdownLogOrdering(t, logs)
}

func collectContainerLogs(t *testing.T, c interface {
	Logs(context.Context) (io.ReadCloser, error)
}) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rc, err := c.Logs(ctx)
	if err != nil {
		t.Logf("collectContainerLogs: %v", err)
		return ""
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Logf("collectContainerLogs read: %v", err)
		return ""
	}
	return string(data)
}

func assertShutdownLogOrdering(t *testing.T, logs string) {
	t.Helper()

	// Must have received the shutdown signal and completed the pool drain.
	signalIdx := strings.Index(logs, "Received signal, initiating graceful shutdown")
	httpStoppedIdx := strings.Index(logs, "HTTP servers stopped, shutting down worker pool")
	drainCompleteIdx := strings.Index(logs, "All in-flight requests completed")

	if signalIdx < 0 {
		t.Errorf("shutdown logs missing 'Received signal' marker\nlogs:\n%s", logs)
		return
	}
	if httpStoppedIdx < 0 {
		t.Errorf("shutdown logs missing 'HTTP servers stopped' marker\nlogs:\n%s", logs)
		return
	}
	if drainCompleteIdx < 0 {
		t.Errorf("shutdown logs missing 'All in-flight requests completed' marker\nlogs:\n%s", logs)
		return
	}
	if !(signalIdx < httpStoppedIdx && httpStoppedIdx < drainCompleteIdx) {
		t.Errorf("shutdown log ordering wrong: signal=%d httpStopped=%d drainComplete=%d",
			signalIdx, httpStoppedIdx, drainCompleteIdx)
	}

	// The regression we're guarding against: the in-flight handler
	// surfacing context.Canceled from its worker call. A plain substring
	// match is good enough — the handler's error log uses these exact
	// tokens (see cmd/serve/main.go).
	shutdownWindow := logs[signalIdx:]
	if strings.Contains(shutdownWindow, "worker call failed") &&
		strings.Contains(shutdownWindow, "context canceled") {
		t.Errorf("in-flight worker call was canceled during shutdown (regression):\n%s",
			shutdownWindow)
	}
}
