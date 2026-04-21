//go:build integration

package integration

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
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
	// needed to remove the container and network. Bound teardown so a
	// hung cleanup doesn't stall the whole integration leg.
	defer func() {
		termCtx, termCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer termCancel()
		if err := env.Terminate(termCtx); err != nil {
			t.Logf("shutdown env Terminate: %v", err)
		}
	}()

	mockClient := mockllm.NewClient(env.MockLLMURL)

	// Build the set of clients we'll exercise concurrently. Fiber is
	// always tested; chi is added only when the matrix enabled it, so
	// CI legs match the semantics of forEachUnaryClient elsewhere.
	fiberClient := testutil.NewBAMLRestClient(env.BAMLRestURL)
	clients := []namedClient{
		{Name: "fiber", Client: fiberClient},
	}
	if unaryEnabled {
		clients = append(clients, namedClient{
			Name:   "chi",
			Client: testutil.NewBAMLRestClient(env.BAMLRestUnaryURL),
		})
	}

	// Scenario with a long InitialDelayMs so the /call is reliably
	// in-flight when we send SIGTERM. The delay needs to dwarf any
	// scheduling jitter between "observed in-flight" and "stop container".
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
	var wg sync.WaitGroup
	for _, nc := range clients {
		wg.Add(1)
		go func(nc namedClient) {
			defer wg.Done()
			resp, err := nc.Client.Call(callCtx, testutil.CallRequest{
				Method:  "GetPerson",
				Input:   map[string]any{"description": "shutdown-drain-" + nc.Name},
				Options: opts,
			})
			results <- callResult{backend: nc.Name, resp: resp, err: err}
		}(nc)
	}

	// Gate on a server-observed signal: the pool's in-flight counter
	// ticks up only once a request has been dispatched to a worker, so
	// this proves both backends' handlers actually reached the worker
	// layer (i.e. have live work to drain). Without this, SIGTERM could
	// race the transport and fire before the regression path is even
	// exercised.
	waitCtx, waitCancel := context.WithTimeout(callCtx, 20*time.Second)
	if err := waitForPoolInFlight(waitCtx, fiberClient, len(clients)); err != nil {
		waitCancel()
		t.Fatalf("pool never saw %d in-flight requests: %v", len(clients), err)
	}
	waitCancel()

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

// waitForPoolInFlight polls /_debug/in-flight until the pool's in-flight
// counter across all workers reaches min. Available because the test
// container is built with debug endpoints (see testutil.Setup).
func waitForPoolInFlight(ctx context.Context, c *testutil.BAMLRestClient, min int) error {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		status, err := c.GetInFlightStatus(ctx)
		if err == nil {
			total := 0
			for _, w := range status.Workers {
				total += w.InFlight
			}
			if total >= min {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("deadline waiting for >=%d in-flight: %w", min, ctx.Err())
		case <-ticker.C:
		}
	}
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

	// The regression we're guarding against: a single zerolog entry that
	// is a "worker call failed" error whose error field is "context
	// canceled". Match per-line so unrelated post-signal entries can't
	// conspire to false-positive. (The chi handler classifies
	// cancellation as 408 and doesn't emit this log; the per-backend
	// status-code check above catches chi regressions directly.)
	shutdownWindow := logs[signalIdx:]
	for _, line := range strings.Split(shutdownWindow, "\n") {
		if strings.Contains(line, "worker call failed") &&
			strings.Contains(line, "context canceled") {
			t.Errorf("in-flight worker call was canceled during shutdown (regression): %s", line)
			return
		}
	}
}
