package llmhttp

// Re-measurement harness for the #475 wrapper fix. Measures BOTH unary lanes so
// we don't flatter ourselves:
//
//   - wf-buffered: wrapped-fasthttp on the buffered fast lane (context.Background
//     + onSuccess==nil) — the easy lane; should approach raw-fasthttp.
//   - wf-streamed: wrapped-fasthttp on the production-shaped streamed lane
//     (cancellable ctx + onSuccess!=nil) — keeps the DoDeadline goroutine race,
//     but the body intermediate + io.ReadAll goroutine are gone, so it should
//     still show a big alloc/throughput/tail win vs before.
//   - raw-fasthttp: the synchronous-library floor (reference).
//
// Reuses helpers from llmhttp_bench_test.go / llmhttp_control_bench_test.go
// (mockServer, controlDoFunc, warmupDriver, runControlLoad, percentile, us).
//
// Run:
//   GOMAXPROCS=8 go test ./bamlutils/llmhttp -run '^$' -bench '^BenchmarkExecuteAfter$' -benchmem -benchtime=5s -count=6
//   GOMAXPROCS=8 BENCH_AFTER_OUT=/tmp/shared/475-after-load-table.md go test ./bamlutils/llmhttp -run '^TestExecuteAfterLoadDist$' -v

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// afterBackend factories take a testing.TB so the cancellable lane can register
// its context cancel via Cleanup (vet-clean, no leaked cancel).
type afterBackend struct {
	name string
	make func(tb testing.TB, m *mockServer) controlDoFunc
}

// wrappedFastBufferedDriver: background ctx + nil onSuccess → buffered fast lane.
func wrappedFastBufferedDriver(_ testing.TB, m *mockServer) controlDoFunc {
	client := newBenchClient(ClientModeFastHTTP)
	req := m.request()
	ctx := context.Background()
	return func() error {
		resp, err := client.ExecuteBorrowed(ctx, req, nil)
		if err != nil {
			return err
		}
		ok := resp.BodyString() == m.body
		got := len(resp.BodyString())
		resp.Release()
		if !ok {
			return fmt.Errorf("wf-buffered: body mismatch (%d vs %d bytes)", got, len(m.body))
		}
		return nil
	}
}

// wrappedFastStreamedDriver: cancellable ctx (never cancelled) + non-nil
// onSuccess → streamed-header lane with the DoDeadline goroutine race. This is
// the production-shaped lane (server request context + pool heartbeat).
func wrappedFastStreamedDriver(tb testing.TB, m *mockServer) controlDoFunc {
	client := newBenchClient(ClientModeFastHTTP)
	req := m.request()
	ctx, cancel := context.WithCancel(context.Background())
	tb.Cleanup(cancel)
	// Empty non-nil callback: selects the streamed-header lane without adding
	// per-request atomic overhead to the measured lane.
	onSuccess := func() {}
	return func() error {
		resp, err := client.ExecuteBorrowed(ctx, req, onSuccess)
		if err != nil {
			return err
		}
		ok := resp.BodyString() == m.body
		got := len(resp.BodyString())
		resp.Release()
		if !ok {
			return fmt.Errorf("wf-streamed: body mismatch (%d vs %d bytes)", got, len(m.body))
		}
		return nil
	}
}

func rawFastAfterDriver(_ testing.TB, m *mockServer) controlDoFunc {
	return rawFastHTTPDriver(m)
}

var afterBackends = []afterBackend{
	{"raw-fasthttp", rawFastAfterDriver},
	{"wf-buffered", wrappedFastBufferedDriver},
	{"wf-streamed", wrappedFastStreamedDriver},
}

// BenchmarkExecuteAfter: ns/op + B/op + allocs/op for both lanes on the key
// cases where the regression lived.
func BenchmarkExecuteAfter(b *testing.B) {
	servers := map[string]*mockServer{
		respSmall:  newMockServer(respSmall),
		respMedium: newMockServer(respMedium),
	}
	for _, m := range servers {
		b.Cleanup(m.close)
	}

	for _, bk := range afterBackends {
		for _, cs := range controlCases {
			m := servers[cs.size]
			name := fmt.Sprintf("%s/%s/c%d", bk.name, cs.size, cs.conc)
			b.Run(name, func(b *testing.B) {
				do := bk.make(b, m)
				warmupDriver(b, do, cs.conc)

				var errCount atomic.Int64
				remaining := int64(b.N)

				b.ReportAllocs()
				b.ResetTimer()

				var wg sync.WaitGroup
				for w := 0; w < cs.conc; w++ {
					wg.Add(1)
					go func() {
						defer wg.Done()
						for atomic.AddInt64(&remaining, -1) >= 0 {
							if err := do(); err != nil {
								errCount.Add(1)
							}
						}
					}()
				}
				wg.Wait()

				b.StopTimer()
				if n := errCount.Load(); n > 0 {
					b.Fatalf("%d errors/body-mismatches — fast-but-broken path must not score as a win", n)
				}
			})
		}
	}
}

// TestExecuteAfterLoadDist: latency distribution for both lanes on small/c256
// and medium/c256 (where the tail blew up before the fix).
func TestExecuteAfterLoadDist(t *testing.T) {
	// Heavy: fires 30k/8k-request load loops and would blow CI's unit-test
	// timeout. Gated to explicit measurement runs — skipped unless the output
	// env is present, or in -short mode.
	if testing.Short() || os.Getenv("BENCH_AFTER_OUT") == "" {
		t.Skip("load measurement; set BENCH_AFTER_OUT to run")
	}

	gomaxprocs := runtime.GOMAXPROCS(0)
	t.Logf("environment: GOMAXPROCS=%d NumCPU=%d go=%s os=%s/%s",
		gomaxprocs, runtime.NumCPU(), runtime.Version(), runtime.GOOS, runtime.GOARCH)

	loadCases := []controlCase{
		{respSmall, 256},
		{respMedium, 256},
	}

	var results []loadResult
	for _, cs := range loadCases {
		m := newMockServer(cs.size)
		for _, bk := range afterBackends {
			do := bk.make(t, m)
			res := runControlLoad(t, do, m, bk.name, cs.conc, loadCount(cs.size))
			res.respSize = cs.size
			if res.errors > 0 {
				t.Errorf("%s/%s/c%d: %d errors", bk.name, cs.size, cs.conc, res.errors)
			}
			if res.serverReq != int64(res.requests) {
				t.Errorf("%s/%s/c%d: server saw %d, expected %d", bk.name, cs.size, cs.conc, res.serverReq, res.requests)
			}
			results = append(results, res)
		}
		m.close()
	}

	var b strings.Builder
	fmt.Fprintf(&b, "\nafter-fix latency distribution — GOMAXPROCS=%d NumCPU=%d go=%s %s/%s\n\n",
		gomaxprocs, runtime.NumCPU(), runtime.Version(), runtime.GOOS, runtime.GOARCH)
	fmt.Fprintf(&b, "| backend | resp | conc | req/s | p50(us) | p95(us) | p99(us) | max(us) | errors | srv-reqs |\n")
	fmt.Fprintf(&b, "|---------|------|-----:|------:|--------:|--------:|--------:|--------:|-------:|---------:|\n")
	for _, r := range results {
		fmt.Fprintf(&b, "| %s | %s | %d | %.0f | %.1f | %.1f | %.1f | %.1f | %d | %d |\n",
			r.backend, r.respSize, r.conc, r.reqPerSec,
			us(r.p50), us(r.p95), us(r.p99), us(r.max), r.errors, r.serverReq)
	}
	t.Log(b.String())

	if out := os.Getenv("BENCH_AFTER_OUT"); out != "" {
		if err := os.WriteFile(out, []byte(b.String()), 0o644); err != nil {
			t.Logf("failed to write BENCH_AFTER_OUT=%s: %v", out, err)
		} else {
			t.Logf("wrote after load table to %s", out)
		}
	}
}
