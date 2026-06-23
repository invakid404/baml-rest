package llmhttp

// Control benchmark for issue #475 follow-up: separate "fasthttp the library"
// from "our llmhttp fasthttp wrapper".
//
// The original bench (llmhttp_bench_test.go) showed wrapped-fasthttp ns/op
// WORSE than wrapped-nethttp at c64+ and a medium/c256 tail blow-up, despite
// fasthttp's lower allocs. Prime suspect: OUR wrapper — executeFast spawns one
// goroutine per request for ctx-cancellable DoDeadline, and converts fasthttp
// headers->http.Header and body->string. fasthttp itself is synchronous (no
// per-request goroutine) and []byte-based.
//
// This file adds RAW variants that hit the SAME mock server with NO llmhttp
// wrapper:
//   - raw-nethttp:  http.Client.Do(POST) + io.ReadAll + Close, tuned transport.
//   - raw-fasthttp: AcquireRequest/Response + HostClient.Do (SYNCHRONOUS, no
//                   per-request goroutine, no ctx wrapping) + Body() + Release.
//                   No http.Header conversion, no []byte->string conversion.
//
// Run:
//   GOMAXPROCS=8 go test ./bamlutils/llmhttp -run '^$' -bench '^BenchmarkExecuteControl$' -benchmem -benchtime=5s -count=6
//   GOMAXPROCS=8 BENCH_CONTROL_OUT=/tmp/shared/475-control-load-table.md go test ./bamlutils/llmhttp -run '^TestExecuteControlLoadDist$' -v
//
// Benchmark-only; changes no production code. Reuses helpers from
// llmhttp_bench_test.go (newMockServer, benchRequestBody, percentile, us, ...).

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/valyala/fasthttp"
)

const benchAuthHeader = "Bearer sk-bench-0000000000000000000000000000"

// controlDoFunc performs exactly one request and returns a non-nil error on
// transport failure, non-2xx status, or a response body that doesn't match the
// expected fixed body. Verifying the body keeps a fast-but-broken path from
// scoring as a win.
type controlDoFunc func() error

// newRawNetHTTPTransport mirrors the tuning of defaultLLMTransport (keep-alive,
// effectively unbounded idle conns per host) on a fresh instance so the raw
// net/http control isn't sharing a pool with the wrapped client under test.
func newRawNetHTTPTransport() *http.Transport {
	return &http.Transport{
		// Explicit no-proxy (not ProxyFromEnvironment) so the raw control dials
		// the local server directly regardless of ambient proxy env — matching
		// the no-proxy bench clients without mutating process-global env.
		Proxy: noProxy,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:   true,
		MaxIdleConns:        0,
		MaxIdleConnsPerHost: math.MaxInt32,
		MaxConnsPerHost:     0,
		IdleConnTimeout:     90 * time.Second,
	}
}

// rawNetHTTPDriver: plain http.Client.Do + io.ReadAll, no llmhttp wrapper.
func rawNetHTTPDriver(m *mockServer) controlDoFunc {
	client := &http.Client{Transport: newRawNetHTTPTransport()}
	want := []byte(m.body)
	ctx := context.Background()
	return func() error {
		req, err := http.NewRequestWithContext(ctx, "POST", m.url, strings.NewReader(benchRequestBody))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", benchAuthHeader)
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("raw-nethttp: status %d", resp.StatusCode)
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return err
		}
		if !bytes.Equal(body, want) {
			return fmt.Errorf("raw-nethttp: body mismatch (%d vs %d bytes)", len(body), len(want))
		}
		return nil
	}
}

// rawFastHTTPDriver: synchronous fasthttp HostClient.Do, []byte body, no
// http.Header conversion, no per-request goroutine, no ctx wrapping. This is
// the raw-library baseline the wrapper's executeFast is compared against.
//
// MaxConns is set to the same effectively-unbounded value the wrapper's
// protocol cache uses (math.MaxInt32) so neither side trips ErrNoFreeConns.
// StreamResponseBody is left off (default) so Body() returns the fully
// buffered body — the standard synchronous fasthttp usage.
func rawFastHTTPDriver(m *mockServer) controlDoFunc {
	origin, err := parseOrigin(m.url)
	if err != nil {
		panic(fmt.Sprintf("rawFastHTTPDriver: bad mock url %q: %v", m.url, err))
	}
	hc := &fasthttp.HostClient{
		Name:                          "bench-raw-fasthttp",
		Addr:                          origin.addr(),
		IsTLS:                         origin.scheme == "https",
		DisableHeaderNamesNormalizing: true,
		MaxConns:                      math.MaxInt32,
	}
	want := []byte(m.body)
	return func() error {
		fReq := fasthttp.AcquireRequest()
		fResp := fasthttp.AcquireResponse()
		defer fasthttp.ReleaseRequest(fReq)
		defer fasthttp.ReleaseResponse(fResp)

		fReq.Header.DisableNormalizing()
		fReq.SetRequestURI(m.url)
		fReq.Header.SetMethod("POST")
		fReq.Header.Set("Content-Type", "application/json")
		fReq.Header.Set("Authorization", benchAuthHeader)
		fReq.SetBodyString(benchRequestBody)

		if err := hc.Do(fReq, fResp); err != nil {
			return err
		}
		if fResp.StatusCode() != http.StatusOK {
			return fmt.Errorf("raw-fasthttp: status %d", fResp.StatusCode())
		}
		if !bytes.Equal(fResp.Body(), want) {
			return fmt.Errorf("raw-fasthttp: body mismatch (%d vs %d bytes)", len(fResp.Body()), len(want))
		}
		return nil
	}
}

// wrappedDriver drives the real (*llmhttp.Client).Execute path for the given
// mode. The llmhttp.Request is shared read-only across worker goroutines
// (Execute does not mutate it for a nil-AWSAuth request).
func wrappedDriver(mode ClientMode, m *mockServer) controlDoFunc {
	client := newBenchClient(mode)
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
			return fmt.Errorf("wrapped: body mismatch (%d vs %d bytes)", got, len(m.body))
		}
		return nil
	}
}

type controlBackend struct {
	name string
	make func(*mockServer) controlDoFunc
}

var controlBackends = []controlBackend{
	{"raw-nethttp", rawNetHTTPDriver},
	{"raw-fasthttp", rawFastHTTPDriver},
	{"wrapped-nethttp", func(m *mockServer) controlDoFunc { return wrappedDriver(ClientModeNetHTTP, m) }},
	{"wrapped-fasthttp", func(m *mockServer) controlDoFunc { return wrappedDriver(ClientModeFastHTTP, m) }},
}

type controlCase struct {
	size string
	conc int
}

// The key cases where surprises appeared in the original grid.
var controlCases = []controlCase{
	{respSmall, 1},
	{respSmall, 256},
	{respMedium, 256},
}

// warmupDriver fires `conc` concurrent requests for a few rounds so connection
// pools are populated before measuring, and fails the test on any error or
// body mismatch.
func warmupDriver(tb testing.TB, do controlDoFunc, conc int) {
	tb.Helper()
	for r := 0; r < 3; r++ {
		var wg sync.WaitGroup
		var failed atomic.Bool
		for i := 0; i < conc; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := do(); err != nil {
					tb.Errorf("warmup request failed: %v", err)
					failed.Store(true)
				}
			}()
		}
		wg.Wait()
		if failed.Load() {
			tb.FailNow()
		}
	}
}

// BenchmarkExecuteControl measures raw-vs-wrapped for both backends on the key
// cases (small/c1, small/c256, medium/c256). ns/op + B/op + allocs/op via
// -benchmem. Same fixed-worker scheme as BenchmarkExecute.
func BenchmarkExecuteControl(b *testing.B) {
	servers := map[string]*mockServer{
		respSmall:  newMockServer(respSmall),
		respMedium: newMockServer(respMedium),
	}
	for _, m := range servers {
		b.Cleanup(m.close)
	}

	for _, bk := range controlBackends {
		for _, cs := range controlCases {
			m := servers[cs.size]
			name := fmt.Sprintf("%s/%s/c%d", bk.name, cs.size, cs.conc)
			b.Run(name, func(b *testing.B) {
				do := bk.make(m)
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

// runControlLoad fires `requests` requests at fixed `conc`, recording
// per-request latency for percentile reporting. Verifies zero errors and the
// server-observed request count.
func runControlLoad(tb testing.TB, do controlDoFunc, m *mockServer, name string, conc, requests int) loadResult {
	tb.Helper()
	warmupDriver(tb, do, conc)
	m.reqCount.Store(0)

	latencies := make([]time.Duration, requests)
	var errCount atomic.Int64
	var idx int64 = -1

	start := time.Now()
	var wg sync.WaitGroup
	for w := 0; w < conc; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := atomic.AddInt64(&idx, 1)
				if i >= int64(requests) {
					return
				}
				t0 := time.Now()
				err := do()
				latencies[i] = time.Since(t0)
				if err != nil {
					errCount.Add(1)
				}
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)

	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	return loadResult{
		backend:   name,
		respSize:  respMedium,
		conc:      conc,
		requests:  requests,
		errors:    errCount.Load(),
		serverReq: m.reqCount.Load(),
		resolved:  "n/a",
		elapsed:   elapsed,
		reqPerSec: float64(requests) / elapsed.Seconds(),
		p50:       percentile(latencies, 0.50),
		p95:       percentile(latencies, 0.95),
		p99:       percentile(latencies, 0.99),
		max:       latencies[len(latencies)-1],
	}
}

// TestExecuteControlLoadDist runs the latency-distribution loop on medium/c256
// for all four backends — the case where wrapped-fasthttp's tail blew up. This
// isolates whether the tail blow-up is fasthttp-inherent (raw-fasthttp tail
// also bad) or wrapper-induced (raw-fasthttp tail fine, wrapped-fasthttp bad).
func TestExecuteControlLoadDist(t *testing.T) {
	// Heavy: fires 30k/8k-request load loops and would blow CI's unit-test
	// timeout. Gated to explicit measurement runs — skipped unless the output
	// env is present, or in -short mode.
	if testing.Short() || os.Getenv("BENCH_CONTROL_OUT") == "" {
		t.Skip("load measurement; set BENCH_CONTROL_OUT to run")
	}

	gomaxprocs := runtime.GOMAXPROCS(0)
	t.Logf("environment: GOMAXPROCS=%d NumCPU=%d go=%s os=%s/%s",
		gomaxprocs, runtime.NumCPU(), runtime.Version(), runtime.GOOS, runtime.GOARCH)

	const conc = 256
	requests := loadCount(respMedium) // 8000

	m := newMockServer(respMedium)
	defer m.close()

	var results []loadResult
	for _, bk := range controlBackends {
		do := bk.make(m)
		res := runControlLoad(t, do, m, bk.name, conc, requests)
		if res.errors > 0 {
			t.Errorf("%s: %d errors — broken path must not be reported as fast", bk.name, res.errors)
		}
		if res.serverReq != int64(res.requests) {
			t.Errorf("%s: server saw %d requests, expected %d", bk.name, res.serverReq, res.requests)
		}
		results = append(results, res)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "\nmedium/c256 latency distribution — GOMAXPROCS=%d NumCPU=%d go=%s %s/%s\n\n",
		gomaxprocs, runtime.NumCPU(), runtime.Version(), runtime.GOOS, runtime.GOARCH)
	fmt.Fprintf(&b, "| backend | conc | req/s | p50(us) | p95(us) | p99(us) | max(us) | errors | srv-reqs |\n")
	fmt.Fprintf(&b, "|---------|-----:|------:|--------:|--------:|--------:|--------:|-------:|---------:|\n")
	for _, r := range results {
		fmt.Fprintf(&b, "| %s | %d | %.0f | %.1f | %.1f | %.1f | %.1f | %d | %d |\n",
			r.backend, r.conc, r.reqPerSec,
			us(r.p50), us(r.p95), us(r.p99), us(r.max), r.errors, r.serverReq)
	}
	t.Log(b.String())

	if out := os.Getenv("BENCH_CONTROL_OUT"); out != "" {
		if err := os.WriteFile(out, []byte(b.String()), 0o644); err != nil {
			t.Logf("failed to write BENCH_CONTROL_OUT=%s: %v", out, err)
		} else {
			t.Logf("wrote control load table to %s", out)
		}
	}
}
