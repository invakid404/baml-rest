//go:build integration

package integration

import (
	"context"
	"io"
	"strings"
	"sync"
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
// When Fiber handlers derived the worker-call context from c.RequestCtx(),
// any in-flight call was canceled before the pool could drain, producing a
// 500. The chi/net-http unary server uses r.Context() (connection-scoped,
// not server-shutdown-scoped), so it was already correct — but we still
// cover it here so a future regression on either stack is caught.
//
// The test uses a dedicated test environment because it kills the server.
// It mirrors the outer matrix's unary-server axis: when the shared
// UnaryClient is configured (UNARY_SERVER=true leg), we enable chi in the
// dedicated container too and assert the drain property on both stacks.
func TestGracefulShutdownDrainsInFlightCall(t *testing.T) {
	setupCtx, setupCancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer setupCancel()

	// Match the outer matrix leg's unary-server setting. UnaryClient is
	// non-nil iff TestMain enabled the chi server for this run.
	unaryEnabled := UnaryClient != nil

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
		UnaryServer:     unaryEnabled,
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

	// Build the set of clients we'll exercise concurrently. Fiber is
	// always tested; chi is added only when the matrix enabled it, so
	// CI legs match the semantics of forEachUnaryClient elsewhere.
	clients := []namedClient{
		{Name: "fiber", Client: testutil.NewBAMLRestClient(env.BAMLRestURL)},
	}
	if unaryEnabled {
		clients = append(clients, namedClient{
			Name:   "chi",
			Client: testutil.NewBAMLRestClient(env.BAMLRestUnaryURL),
		})
	}

	// Scenario with a long InitialDelayMs so the /call is reliably
	// in-flight when we send SIGTERM. The delay needs to dwarf any
	// scheduling jitter between "fire request" and "stop container".
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

	// Fire one in-flight call per backend on background goroutines so a
	// single SIGTERM exercises the drain path on both stacks at once.
	type callResult struct {
		backend string
		resp    *testutil.CallResponse
		err     error
	}

	callCtx, callCancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer callCancel()

	opts := &testutil.BAMLOptions{
		ClientRegistry: testutil.CreateTestClient(env.MockLLMInternal, scenario.ID),
	}

	results := make(chan callResult, len(clients))
	var inFlight atomic.Int32
	var wg sync.WaitGroup
	for _, nc := range clients {
		wg.Add(1)
		inFlight.Add(1)
		go func(nc namedClient) {
			defer wg.Done()
			resp, err := nc.Client.Call(callCtx, testutil.CallRequest{
				Method:  "GetPerson",
				Input:   map[string]any{"description": "shutdown-drain-" + nc.Name},
				Options: opts,
			})
			inFlight.Add(-1)
			results <- callResult{backend: nc.Name, resp: resp, err: err}
		}(nc)
	}

	// Give the requests time to reach the worker and start waiting on the
	// mock LLM's InitialDelayMs. A short poll with /_debug/in-flight would
	// be more precise but requires the debug build — a plain sleep is
	// sufficient for a regression test.
	time.Sleep(500 * time.Millisecond)
	if got := inFlight.Load(); int(got) != len(clients) {
		t.Fatalf("expected %d in-flight calls after 500ms, got %d", len(clients), got)
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

	// Collect all results.
	waitDone := make(chan struct{})
	go func() { wg.Wait(); close(waitDone) }()
	select {
	case <-waitDone:
	case <-time.After(30 * time.Second):
		t.Fatalf("call goroutines did not return within 30s of container stop")
	}
	close(results)

	for r := range results {
		if r.err != nil {
			t.Errorf("[%s] in-flight call errored during graceful shutdown: %v", r.backend, r.err)
			continue
		}
		if r.resp.StatusCode != 200 {
			t.Errorf("[%s] in-flight call returned status %d during graceful shutdown (expected 200): %s",
				r.backend, r.resp.StatusCode, r.resp.Error)
			continue
		}
		var person struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(r.resp.Body, &person); err != nil {
			t.Errorf("[%s] failed to unmarshal response: %v (body=%s)", r.backend, err, r.resp.Body)
			continue
		}
		if person.Name == "" {
			t.Errorf("[%s] response body missing name field: %s", r.backend, r.resp.Body)
		}
	}

	// The stop must have taken at least half the in-flight delay — i.e.
	// shutdown waited for the drain rather than bailing immediately.
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
	// match is good enough — the Fiber handler's error log uses these
	// exact tokens (see cmd/serve/main.go). The chi handler classifies
	// cancellation as 408 without this log line, which is fine: the
	// per-result status-code check above catches chi regressions.
	shutdownWindow := logs[signalIdx:]
	if strings.Contains(shutdownWindow, "worker call failed") &&
		strings.Contains(shutdownWindow, "context canceled") {
		t.Errorf("in-flight worker call was canceled during shutdown (regression):\n%s",
			shutdownWindow)
	}
}
