package llmhttp

// Focused benchmark for issue #475: does fasthttp actually beat net/http for
// our high-volume HTTP/1.1 plaintext BuildRequest path, or are we carrying its
// complexity for no payoff?
//
// This measures the REAL shared (*llmhttp.Client).Execute path — header
// conversion, the cancellation goroutine, body handling, response
// normalization — NOT raw fasthttp/net/http. Backends are selected by explicit
// ClientMode (no env), against a local in-process plaintext HTTP/1.1 server.
//
// Two measurement vehicles:
//   (1) BenchmarkExecute    — testing.B sub-benchmarks → ns/op, B/op, allocs/op
//   (2) TestExecuteLoadDist — fixed-worker load loop → req/s, p50/p95/p99/max,
//                             errors, server-observed request count
//
// Run (record GOMAXPROCS + machine):
//   go build ./bamlutils/llmhttp/
//   GOMAXPROCS=8 go test ./bamlutils/llmhttp -run '^$' -bench '^BenchmarkExecute$' -benchmem -benchtime=5s -count=6
//   GOMAXPROCS=8 go test ./bamlutils/llmhttp -run '^TestExecuteLoadDist$' -v
//
// This file is benchmark-only; it changes no production code.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// noProxy is an explicit "no proxy for any request" resolver. Bench clients
// install it on their transports so the auto-mode probe never pins our
// plaintext http:// origin to net/http via a proxy, and the net/http backend
// dials the local server directly — WITHOUT mutating process-global proxy env
// (which would leak into every other test in the package). A nil Transport.Proxy
// is NOT sufficient: proxyFuncFromHTTPClient falls back to
// http.ProxyFromEnvironment when Proxy is nil, so we must set an explicit func.
func noProxy(*http.Request) (*url.URL, error) { return nil, nil }

// --- payloads -------------------------------------------------------------

// benchRequestBody is a small OpenAI-style chat-completion request (~1.5 KB),
// resembling generated BuildRequest traffic. Request stays small in every
// case; only the response size varies as a sub-case.
var benchRequestBody = buildRequestBody()

func buildRequestBody() string {
	// pad the user message so the whole JSON lands around 1.5 KB.
	userContent := strings.Repeat(
		"Summarize the following passage in two sentences, preserving key facts. ", 18)
	return fmt.Sprintf(
		`{"model":"gpt-4o","messages":[`+
			`{"role":"system","content":"You are a helpful assistant that answers concisely."},`+
			`{"role":"user","content":%q}],`+
			`"temperature":0.7,"max_tokens":256,"top_p":1,"stream":false}`,
		userContent)
}

const (
	respSmall  = "small"  // ~256 B JSON completion
	respMedium = "medium" // ~64 KB JSON completion
)

// mockBody returns a fixed JSON response body of approximately the requested
// size. Small mimics a terse completion; medium (~64 KB) exposes
// allocation/body-handling differences between the backends.
func mockBody(size string) string {
	switch size {
	case respMedium:
		// ~64 KB of content inside an OpenAI-style envelope.
		content := strings.Repeat("The quick brown fox jumps over the lazy dog. ", 1456) // ~64 KB
		return fmt.Sprintf(
			`{"id":"chatcmpl-bench","object":"chat.completion","model":"gpt-4o",`+
				`"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":%q}}],`+
				`"usage":{"prompt_tokens":42,"completion_tokens":4096,"total_tokens":4138}}`,
			content)
	default:
		return `{"id":"chatcmpl-bench","object":"chat.completion","model":"gpt-4o",` +
			`"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"Hello! How can I help you today?"}}],` +
			`"usage":{"prompt_tokens":42,"completion_tokens":9,"total_tokens":51}}`
	}
}

// mockServer is a local in-process plaintext HTTP/1.1 server. It accepts POST,
// drains the request body (so keep-alive connections are reused), and returns a
// fixed JSON body with Content-Type: application/json. No logging, no sleeps.
type mockServer struct {
	srv      *httptest.Server
	url      string // server.URL + "/v1/chat/completions"
	body     string // exact expected response body
	reqCount atomic.Int64
}

func newMockServer(size string) *mockServer {
	m := &mockServer{body: mockBody(size)}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Drain + discard the request body so the connection is reusable.
		_, _ = io.Copy(io.Discard, r.Body)
		_ = r.Body.Close()
		m.reqCount.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, m.body)
	}))
	m.url = m.srv.URL + "/v1/chat/completions"
	return m
}

func (m *mockServer) close() { m.srv.Close() }

func (m *mockServer) request() *Request {
	return &Request{
		Method: "POST",
		URL:    m.url,
		Headers: map[string]string{
			"Content-Type":  "application/json",
			"Authorization": "Bearer sk-bench-0000000000000000000000000000",
		},
		Body: benchRequestBody,
	}
}

// --- client construction --------------------------------------------------

type backendCase struct {
	name string
	mode ClientMode
}

var backendCases = []backendCase{
	{"nethttp", ClientModeNetHTTP},
	{"fasthttp", ClientModeFastHTTP},
	{"auto", ClientModeAuto},
}

// newBenchClient builds a Client for the given mode via the explicit options
// constructor — NOT env. It clones the tuned defaultLLMTransport and installs an
// explicit no-proxy resolver so the auto-mode probe resolves our plaintext
// origin to fasthttp and the net/http backend dials directly, with no
// process-global proxy-env mutation. The fasthttp side uses the
// effectively-unbounded MaxConns default (math.MaxInt32) so high concurrency
// never trips ErrNoFreeConns.
func newBenchClient(mode ClientMode) *Client {
	tr := defaultLLMTransport.Clone()
	tr.Proxy = noProxy
	return NewClientWithOptions(ClientOptions{
		Mode:          mode,
		NetHTTPClient: &http.Client{Transport: tr},
	})
}

// warmup fires `conc` concurrent requests (a few rounds) before measuring so
// the connection pool is populated and, for auto mode, the per-origin protocol
// cache (ALPN probe) is resolved. Returns the resolved decision string for
// auto-mode reporting. It also fails the test if any warmup request errors or
// the body doesn't match — a broken-but-fast path must not look like a win.
func warmup(tb testing.TB, client *Client, m *mockServer, conc int) {
	tb.Helper()
	ctx := context.Background()
	rounds := 3
	for r := 0; r < rounds; r++ {
		var wg sync.WaitGroup
		var failed atomic.Bool
		for i := 0; i < conc; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				resp, err := client.Execute(ctx, m.request(), nil)
				if err != nil {
					tb.Errorf("warmup request failed: %v", err)
					failed.Store(true)
					return
				}
				if resp.BodyString() != m.body {
					tb.Errorf("warmup body mismatch: got %d bytes, want %d", len(resp.BodyString()), len(m.body))
					failed.Store(true)
				}
				resp.Release()
			}()
		}
		wg.Wait()
		if failed.Load() {
			tb.FailNow()
		}
	}
}

// resolvedDecision reports whether the client's protocol cache routed the mock
// origin to fasthttp or net/http (after warmup). Used to confirm what "auto"
// actually resolved to.
func resolvedDecision(client *Client, m *mockServer) string {
	if client.cache == nil {
		return "no-cache"
	}
	origin, err := parseOrigin(m.url)
	if err != nil {
		return "parse-error"
	}
	entry := client.cache.lookup(origin.key)
	if entry == nil {
		return "unresolved"
	}
	switch entry.decision {
	case decisionFast:
		return "fasthttp"
	case decisionNet:
		return "nethttp"
	default:
		return "unknown"
	}
}

// --- vehicle 1: testing.B sub-benchmarks ----------------------------------

var benchConcurrency = []int{1, 16, 64, 256}

// BenchmarkExecute measures (*Client).Execute across backend × response-size ×
// concurrency. Each leaf warms up the client, resets the timer, then runs b.N
// requests spread across a fixed number of worker goroutines. -benchmem yields
// B/op and allocs/op for the timed region.
func BenchmarkExecute(b *testing.B) {
	for _, size := range []string{respSmall, respMedium} {
		m := newMockServer(size)
		b.Cleanup(m.close)
		for _, bc := range backendCases {
			for _, conc := range benchConcurrency {
				name := fmt.Sprintf("%s/%s/c%d", bc.name, size, conc)
				b.Run(name, func(b *testing.B) {
					client := newBenchClient(bc.mode)
					warmup(b, client, m, conc)

					var errCount atomic.Int64
					remaining := int64(b.N)
					ctx := context.Background()

					b.ReportAllocs()
					b.ResetTimer()

					var wg sync.WaitGroup
					for w := 0; w < conc; w++ {
						wg.Add(1)
						go func() {
							defer wg.Done()
							req := m.request()
							for atomic.AddInt64(&remaining, -1) >= 0 {
								// ExecuteBorrowed exercises the consumer-managed
								// borrow lane this benchmark exists to measure: on
								// fasthttp the buffered/streamed body is borrowed
								// (no owned copy) and freed by Release. BodyString
								// is a zero-copy view valid until Release.
								resp, err := client.ExecuteBorrowed(ctx, req, nil)
								if err != nil {
									errCount.Add(1)
									continue
								}
								if resp.BodyString() != m.body {
									errCount.Add(1)
								}
								resp.Release()
							}
						}()
					}
					wg.Wait()

					b.StopTimer()
					if n := errCount.Load(); n > 0 {
						b.Fatalf("%d errors/body-mismatches during benchmark — fast-but-broken path must not score as a win", n)
					}
				})
			}
		}
	}
}

// --- vehicle 2: fixed-worker latency-distribution load test ---------------

type loadResult struct {
	backend   string
	respSize  string
	conc      int
	requests  int
	errors    int64
	serverReq int64
	resolved  string
	elapsed   time.Duration
	reqPerSec float64
	p50, p95  time.Duration
	p99, max  time.Duration
}

// loadCount returns how many requests to fire for the given response size.
// Medium fires fewer because each request moves ~64 KB through the body path.
func loadCount(size string) int {
	if size == respMedium {
		return 8000
	}
	return 30000
}

// runLoad fires exactly `requests` requests at fixed `conc` concurrency,
// recording per-request latency. It verifies zero errors and that every
// response body matched the expected fixed body.
func runLoad(tb testing.TB, client *Client, m *mockServer, bc backendCase, size string, conc, requests int) loadResult {
	tb.Helper()
	warmup(tb, client, m, conc)
	resolved := resolvedDecision(client, m)

	// Reset the server-observed counter so it reflects only the measured run.
	m.reqCount.Store(0)

	latencies := make([]time.Duration, requests)
	var errCount atomic.Int64
	var idx int64 = -1
	ctx := context.Background()

	start := time.Now()
	var wg sync.WaitGroup
	for w := 0; w < conc; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := m.request()
			for {
				i := atomic.AddInt64(&idx, 1)
				if i >= int64(requests) {
					return
				}
				t0 := time.Now()
				resp, err := client.ExecuteBorrowed(ctx, req, nil)
				lat := time.Since(t0)
				latencies[i] = lat
				if err != nil {
					errCount.Add(1)
					continue
				}
				if resp.BodyString() != m.body {
					errCount.Add(1)
				}
				resp.Release()
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)

	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	res := loadResult{
		backend:   bc.name,
		respSize:  size,
		conc:      conc,
		requests:  requests,
		errors:    errCount.Load(),
		serverReq: m.reqCount.Load(),
		resolved:  resolved,
		elapsed:   elapsed,
		reqPerSec: float64(requests) / elapsed.Seconds(),
		p50:       percentile(latencies, 0.50),
		p95:       percentile(latencies, 0.95),
		p99:       percentile(latencies, 0.99),
		max:       latencies[len(latencies)-1],
	}
	return res
}

func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	rank := int(p * float64(len(sorted)))
	if rank >= len(sorted) {
		rank = len(sorted) - 1
	}
	return sorted[rank]
}

// TestExecuteLoadDist is the latency-distribution vehicle. testing.B alone
// can't produce percentiles, so this fires a fixed number of requests at each
// concurrency level and reports req/s + p50/p95/p99/max + errors +
// server-observed request count. Run with -v to see the table:
//
//	GOMAXPROCS=8 go test ./bamlutils/llmhttp -run '^TestExecuteLoadDist$' -v
func TestExecuteLoadDist(t *testing.T) {
	// Heavy: fires 30k/8k-request load loops and would blow CI's unit-test
	// timeout. Gated to explicit measurement runs — skipped unless the output
	// env (which a measurement run sets anyway) is present, or in -short mode.
	if testing.Short() || os.Getenv("BENCH_LOAD_OUT") == "" {
		t.Skip("load measurement; set BENCH_LOAD_OUT to run")
	}

	gomaxprocs := runtime.GOMAXPROCS(0)
	t.Logf("environment: GOMAXPROCS=%d NumCPU=%d go=%s os=%s/%s",
		gomaxprocs, runtime.NumCPU(), runtime.Version(), runtime.GOOS, runtime.GOARCH)

	var results []loadResult
	for _, size := range []string{respSmall, respMedium} {
		m := newMockServer(size)
		for _, bc := range backendCases {
			for _, conc := range benchConcurrency {
				client := newBenchClient(bc.mode)
				res := runLoad(t, client, m, bc, size, conc, loadCount(size))
				if res.errors > 0 {
					t.Errorf("%s/%s/c%d: %d errors — broken path must not be reported as fast",
						bc.name, size, conc, res.errors)
				}
				if res.serverReq != int64(res.requests) {
					t.Errorf("%s/%s/c%d: server saw %d requests, expected %d",
						bc.name, size, conc, res.serverReq, res.requests)
				}
				results = append(results, res)
			}
		}
		m.close()
	}

	// Emit a markdown table to the test log (captured into the results file).
	var b strings.Builder
	fmt.Fprintf(&b, "\nGOMAXPROCS=%d NumCPU=%d go=%s %s/%s\n\n",
		gomaxprocs, runtime.NumCPU(), runtime.Version(), runtime.GOOS, runtime.GOARCH)
	fmt.Fprintf(&b, "| backend | resp | conc | resolved | req/s | p50(us) | p95(us) | p99(us) | max(us) | errors | srv-reqs |\n")
	fmt.Fprintf(&b, "|---------|------|-----:|----------|------:|--------:|--------:|--------:|--------:|-------:|---------:|\n")
	for _, r := range results {
		fmt.Fprintf(&b, "| %s | %s | %d | %s | %.0f | %.1f | %.1f | %.1f | %.1f | %d | %d |\n",
			r.backend, r.respSize, r.conc, r.resolved, r.reqPerSec,
			us(r.p50), us(r.p95), us(r.p99), us(r.max), r.errors, r.serverReq)
	}
	t.Log(b.String())

	// Also write the table to a file when BENCH_LOAD_OUT is set, so the load
	// table can be captured independently of -v log formatting.
	if out := os.Getenv("BENCH_LOAD_OUT"); out != "" {
		if err := os.WriteFile(out, []byte(b.String()), 0o644); err != nil {
			t.Logf("failed to write BENCH_LOAD_OUT=%s: %v", out, err)
		} else {
			t.Logf("wrote load table to %s", out)
		}
	}
}

func us(d time.Duration) float64 { return float64(d.Microseconds()) }
