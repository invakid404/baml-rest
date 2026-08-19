//go:build artifactrehearsal || nativeartifactproof

package workerboot

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
	goplugin "github.com/hashicorp/go-plugin"
	dto "github.com/prometheus/client_model/go"
	"google.golang.org/protobuf/proto"

	"github.com/invakid404/baml-rest/workerplugin"
)

// Shared harness for the de-BAML serving-cutover S2 artifact proofs: it BUILDS
// nothing and asserts nothing on its own, it just boots a worker binary the way
// the pool does and reads its startup log and metrics back.
//
// Compiled under BOTH proof tags because the two lanes prove different halves of
// the same contract against the same mechanism:
//
//   - `artifactrehearsal` (ordinary CI): the BAML-only ROLLBACK artifact, built
//     from this pure-Go module;
//   - `nativeartifactproof` (the gated nanollm lane): the STANDARD native-capable
//     artifacts, supplied prebuilt because they need CGO and a linked nanollm
//     archive.

// syncBuffer is a concurrency-safe sink for the worker subprocess's stderr;
// go-plugin writes to it from its own goroutine while the test reads.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// bootedWorker is a live worker subprocess plus its captured startup log.
type bootedWorker struct {
	worker workerplugin.Worker
	stderr *syncBuffer
}

// bootWorker launches bin through the real go-plugin handshake — the same
// handshake config, protocol and dial options pool uses — and dispenses the
// worker. It returns an error rather than failing the test so the mislabel
// mutation can assert that booting is REFUSED.
func bootWorker(t *testing.T, bin string, env map[string]string) (*bootedWorker, error) {
	t.Helper()

	cmd := exec.Command(bin)
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}

	stderr := &syncBuffer{}
	client := goplugin.NewClient(&goplugin.ClientConfig{
		HandshakeConfig:  workerplugin.Handshake,
		Plugins:          map[string]goplugin.Plugin{"worker": &workerplugin.WorkerPlugin{}},
		Cmd:              cmd,
		AllowedProtocols: []goplugin.Protocol{goplugin.ProtocolGRPC},
		GRPCDialOptions:  workerplugin.GRPCDialOptions(),
		Logger:           hclog.NewNullLogger(),
		Stderr:           stderr,
		SyncStderr:       stderr,
		StartTimeout:     60 * time.Second,
	})
	t.Cleanup(client.Kill)

	rpc, err := client.Client()
	if err != nil {
		return &bootedWorker{stderr: stderr}, fmt.Errorf("go-plugin handshake: %w", err)
	}
	raw, err := rpc.Dispense("worker")
	if err != nil {
		return &bootedWorker{stderr: stderr}, fmt.Errorf("dispense worker: %w", err)
	}
	w, ok := raw.(workerplugin.Worker)
	if !ok {
		return &bootedWorker{stderr: stderr}, fmt.Errorf("dispensed %T, not a workerplugin.Worker", raw)
	}
	return &bootedWorker{worker: w, stderr: stderr}, nil
}

// startupSignal returns the de-BAML startup diagnostic line the booted worker
// emitted, decoded from its hclog JSON stderr.
func startupSignal(t *testing.T, b *bootedWorker) map[string]any {
	t.Helper()
	// go-plugin drains stderr on its own goroutine; the startup line is written
	// before the handshake completes, but the drain is asynchronous, so give it a
	// bounded moment to land rather than racing it.
	deadline := time.Now().Add(10 * time.Second)
	for {
		for _, line := range strings.Split(b.stderr.String(), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || !strings.HasPrefix(line, "{") {
				continue
			}
			var m map[string]any
			if err := json.Unmarshal([]byte(line), &m); err != nil {
				continue
			}
			msg, _ := m["@message"].(string)
			if !strings.HasPrefix(msg, "de-BAML worker startup:") {
				continue
			}
			// The IDENTITY record and the expectation ALERT share this message
			// prefix, and the alert is emitted right after. Selecting on the prefix
			// alone could therefore return the alert — which carries the profile and
			// the ID but none of the flag/lane fields — and every assertion about
			// rollout_mode or native_runtime_initialized would fail confusingly, or
			// worse, pass by absence. Require fields only the identity record has.
			if _, ok := m["native_build_capable"]; !ok {
				continue
			}
			if _, ok := m["rollout_mode"]; !ok {
				continue
			}
			return m
		}
		if time.Now().After(deadline) {
			t.Fatalf("no de-BAML startup line in worker stderr:\n%s", b.stderr.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// gatheredMetric returns the single sample of the named metric family from the
// worker's metrics RPC — the same payload the host merges into /metrics.
func gatheredMetric(t *testing.T, b *bootedWorker, name string) *dto.Metric {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	payload, err := b.worker.GetMetrics(ctx)
	if err != nil {
		t.Fatalf("GetMetrics: %v", err)
	}
	var seen []string
	for _, raw := range payload {
		var mf dto.MetricFamily
		if err := proto.Unmarshal(raw, &mf); err != nil {
			t.Fatalf("unmarshal metric family: %v", err)
		}
		seen = append(seen, mf.GetName())
		if mf.GetName() != name {
			continue
		}
		if len(mf.Metric) != 1 {
			t.Fatalf("metric %q has %d samples, want exactly 1 per process", name, len(mf.Metric))
		}
		return mf.Metric[0]
	}
	t.Fatalf("metric %q not exposed by the booted worker; it exposed %v", name, seen)
	return nil
}

// labelValue reads one label off a gathered sample.
func labelValue(m *dto.Metric, name string) string {
	for _, lp := range m.Label {
		if lp.GetName() == name {
			return lp.GetValue()
		}
	}
	return ""
}

// requireStderrContains waits (bounded) for want to appear in the worker's
// captured stderr.
//
// go-plugin drains the subprocess's stderr on its OWN goroutine, so a line the
// worker has already written may not have reached the buffer when the test looks.
// startupSignal polls for exactly that reason; the failed-handshake assertions
// used to read the buffer once and could lose a race with the drain. This is the
// same bounded wait, factored out — it changes nothing about the worker, only
// about when the test gives up looking.
func requireStderrContains(t *testing.T, b *bootedWorker, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if strings.Contains(b.stderr.String(), want) {
			return
		}
		if time.Now().After(deadline) {
			t.Errorf("worker stderr never contained %q:\n%s", want, b.stderr.String())
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// stderrSettled returns the worker's captured stderr after giving go-plugin's
// drain goroutine a bounded chance to finish.
//
// This is the DIAGNOSTIC counterpart to requireStderrContains. An assertion knows
// which substring it is waiting for and can poll for it; a failure message does
// not — it just wants "everything the worker managed to say". Reading the buffer
// the instant a boot fails races the same drain, and the result is the worst
// possible report: a test that failed because the worker refused to start, whose
// message shows an empty or truncated reason.
//
// So this waits until the buffer STOPS GROWING (two consecutive quiet samples)
// rather than for any particular content, with a short cap — it only ever runs on
// a path that is already failing, so it must not add meaningful runtime.
func stderrSettled(b *bootedWorker) string {
	deadline := time.Now().Add(3 * time.Second)
	prev := b.stderr.String()
	quiet := 0
	for time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		cur := b.stderr.String()
		if cur == prev {
			quiet++
			if quiet >= 2 {
				return cur
			}
			continue
		}
		quiet = 0
		prev = cur
	}
	return b.stderr.String()
}

// deBAMLMetricNames returns every de-BAML metric family the booted worker
// exposes.
func deBAMLMetricNames(t *testing.T, b *bootedWorker) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	payload, err := b.worker.GetMetrics(ctx)
	if err != nil {
		t.Fatalf("GetMetrics: %v", err)
	}
	var names []string
	for _, raw := range payload {
		var mf dto.MetricFamily
		if err := proto.Unmarshal(raw, &mf); err != nil {
			t.Fatalf("unmarshal metric family: %v", err)
		}
		if strings.HasPrefix(mf.GetName(), "baml_rest_debaml_") {
			names = append(names, mf.GetName())
		}
	}
	return names
}
