package llmhttp

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/valyala/fasthttp"
)

// The ordinary-envelope differential for ExactTransportFailure.Ordinary.
//
// Every scenario is a raw loopback server that fails a request in one specific way.
// Each is driven twice per ordinary backend — once through the ORDINARY Client the
// worker builds (NewDefaultClientWithOptions with that backend forced, called the way
// the generated orchestrator calls it: a cancellable context and a non-nil first-2xx
// callback), and once through the production exact executor with the failure mapped
// by Ordinary — and the two errors must be byte-identical AND answer every typed
// question (TransportError category, ErrTransportFlake, *url.Error, *HTTPError
// status/body, the underlying cause, context sentinels) the same way. The mapping is
// therefore checked against the real ordinary transport, not against a restatement
// of its strings.

type failureScenario struct {
	name string
	// serve handles ONE accepted connection after the request was read.
	serve func(c net.Conn, w *bufio.Writer)
	// timeout bounds the call (0 = the default unary timeout).
	timeout time.Duration
}

func failureScenarios() []failureScenario {
	headers := func(w *bufio.Writer, status, extra string) {
		fmt.Fprintf(w, "HTTP/1.1 %s\r\nContent-Type: application/json\r\n%s\r\n", status, extra)
	}
	return []failureScenario{
		{name: "2xx body truncated (small)", serve: func(_ net.Conn, w *bufio.Writer) {
			headers(w, "200 OK", "Content-Length: 100\r\n")
			w.WriteString(`{"choices":`)
		}},
		{name: "2xx body truncated (large)", serve: func(_ net.Conn, w *bufio.Writer) {
			headers(w, "200 OK", "Content-Length: 2000000\r\n")
			w.WriteString(strings.Repeat("a", 300000))
		}},
		{name: "non-2xx body truncated (small)", serve: func(_ net.Conn, w *bufio.Writer) {
			headers(w, "429 Too Many Requests", "Content-Length: 100\r\n")
			w.WriteString(`{"error":`)
		}},
		{name: "non-2xx body truncated past the diagnostic cap", serve: func(_ net.Conn, w *bufio.Writer) {
			headers(w, "503 Service Unavailable", "Content-Length: 200000\r\n")
			w.WriteString(strings.Repeat("e", 10000))
		}},
		{name: "2xx body over the size cap", serve: func(_ net.Conn, w *bufio.Writer) {
			headers(w, "200 OK", fmt.Sprintf("Content-Length: %d\r\n", MaxResponseBodyBytes+1))
			w.WriteString(strings.Repeat(" ", MaxResponseBodyBytes+1))
		}},
		{name: "connection closed before the first response byte", serve: func(net.Conn, *bufio.Writer) {}},
		{name: "deadline before headers", timeout: 300 * time.Millisecond, serve: func(net.Conn, *bufio.Writer) {
			time.Sleep(time.Second)
		}},
		{name: "deadline during the body", timeout: 300 * time.Millisecond, serve: func(_ net.Conn, w *bufio.Writer) {
			headers(w, "200 OK", "Content-Length: 100\r\n")
			w.WriteString(`{"choices":`)
			w.Flush()
			time.Sleep(time.Second)
		}},
	}
}

// netOnlyScenarios are byte-identical on the net/http backend only: the exact lane IS a
// net/http RoundTripper, but fasthttp's own wording (or framing tolerance) for them is
// not reproducible from a net/http failure — see the residual guard below.
func netOnlyScenarios() []failureScenario {
	return []failureScenario{
		{name: "malformed status line", serve: func(_ net.Conn, w *bufio.Writer) {
			w.WriteString("NOT HTTP AT ALL\r\n\r\n")
		}},
		{name: "2xx chunked body unterminated", serve: func(_ net.Conn, w *bufio.Writer) {
			fmt.Fprintf(w, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nTransfer-Encoding: chunked\r\n\r\na\r\n0123456789\r\n")
		}},
	}
}

// failureServer serves sc for every connection and returns the request URL.
func failureServer(t *testing.T, sc failureScenario) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				br := bufio.NewReader(c)
				req, err := http.ReadRequest(br)
				if err != nil {
					return
				}
				_, _ = io.Copy(io.Discard, req.Body)
				w := bufio.NewWriter(c)
				sc.serve(c, w)
				_ = w.Flush()
			}(c)
		}
	}()
	return "http://" + ln.Addr().String() + "/v1/chat/completions"
}

func ordinaryCall(t *testing.T, backend OrdinaryBackend, rawURL string, timeout time.Duration) error {
	t.Helper()
	mode := ClientModeNetHTTP
	if backend == OrdinaryBackendFastHTTP {
		mode = ClientModeFastHTTP
	}
	ctx, cancel := scenarioContext(timeout)
	defer cancel()
	c := NewDefaultClientWithOptions(ClientOptions{Mode: mode})
	resp, err := c.ExecuteBorrowed(ctx, &Request{
		Method:  http.MethodPost,
		URL:     rawURL,
		Headers: map[string]string{"Content-Type": "application/json"},
		Body:    `{"model":"m"}`,
	}, func() {})
	if resp != nil {
		resp.Release()
	}
	if err == nil {
		t.Fatalf("the ordinary %v backend SUCCEEDED; the scenario does not fail the request", backend)
	}
	return err
}

func exactFailure(t *testing.T, rawURL string, timeout time.Duration) *ExactTransportFailure {
	t.Helper()
	ctx, cancel := scenarioContext(timeout)
	defer cancel()
	_, err := NewExactExecutor(nil).ExecuteWithHeartbeat(ctx, &ExactAttemptRequest{
		Method:      http.MethodPost,
		URL:         rawURL,
		Headers:     []HeaderField{{Name: "Content-Type", Value: "application/json"}},
		Body:        []byte(`{"model":"m"}`),
		BodyPresent: true,
	}, func() {})
	var f *ExactTransportFailure
	if !errors.As(err, &f) {
		t.Fatalf("exact err = %v (%T), want an *ExactTransportFailure", err, err)
	}
	return f
}

func scenarioContext(timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout > 0 {
		return context.WithTimeout(context.Background(), timeout)
	}
	return context.WithCancel(context.Background())
}

// errFacts is every typed question a caller (the worker classifier, the orchestrator's
// retry policy, a test) asks of a transport error.
type errFacts struct {
	transport       bool
	category        TransportFlakeCategory
	flake           bool
	urlErr          bool
	httpErr         bool
	status          int
	body            string
	unexpectedEOF   bool
	eof             bool
	connClosed      bool
	canceled        bool
	deadline        bool
	topLevelMessage string
}

func factsOf(err error) errFacts {
	f := errFacts{topLevelMessage: err.Error()}
	var te *TransportError
	if errors.As(err, &te) {
		f.transport, f.category = true, te.Category
	}
	var ue *url.Error
	f.urlErr = errors.As(err, &ue)
	var he *HTTPError
	if errors.As(err, &he) {
		f.httpErr, f.status, f.body = true, he.StatusCode, he.Body
	}
	f.flake = errors.Is(err, ErrTransportFlake)
	f.unexpectedEOF = errors.Is(err, io.ErrUnexpectedEOF)
	f.eof = errors.Is(err, io.EOF)
	f.connClosed = errors.Is(err, fasthttp.ErrConnectionClosed)
	f.canceled = errors.Is(err, context.Canceled)
	f.deadline = errors.Is(err, context.DeadlineExceeded)
	return f
}

// TestExactTransportFailureOrdinaryMatchesTheOrdinaryClient is the differential.
func TestExactTransportFailureOrdinaryMatchesTheOrdinaryClient(t *testing.T) {
	run := func(t *testing.T, sc failureScenario, backend OrdinaryBackend) {
		rawURL := failureServer(t, sc)
		want := ordinaryCall(t, backend, rawURL, sc.timeout)
		f := exactFailure(t, rawURL, sc.timeout)
		got := f.Ordinary(backend, http.MethodPost, rawURL)
		if got.Error() != want.Error() {
			t.Errorf("message:\n  ordinary: %q\n  mapped:   %q", want.Error(), got.Error())
		}
		if gf, wf := factsOf(got), factsOf(want); gf != wf {
			t.Errorf("typed facts:\n  ordinary: %+v\n  mapped:   %+v", wf, gf)
		}
		// The exact lane's own error is untouched by the mapping.
		if strings.Contains(got.Error(), "exact") || !strings.Contains(f.Error(), "exact") {
			t.Errorf("exact lane error %q / mapped %q: the mapping must change only the surfaced envelope", f.Error(), got.Error())
		}
	}
	for _, sc := range failureScenarios() {
		for _, backend := range []OrdinaryBackend{OrdinaryBackendFastHTTP, OrdinaryBackendNetHTTP} {
			sc, backend := sc, backend
			t.Run(fmt.Sprintf("%s/%s", sc.name, backendName(backend)), func(t *testing.T) { run(t, sc, backend) })
		}
	}
	for _, sc := range netOnlyScenarios() {
		sc := sc
		t.Run(sc.name+"/nethttp", func(t *testing.T) { run(t, sc, OrdinaryBackendNetHTTP) })
	}
	t.Run("connection refused/nethttp", func(t *testing.T) {
		rawURL := refusedURL(t)
		want := ordinaryCall(t, OrdinaryBackendNetHTTP, rawURL, 0)
		got := exactFailure(t, rawURL, 0).Ordinary(OrdinaryBackendNetHTTP, http.MethodPost, rawURL)
		if got.Error() != want.Error() || factsOf(got) != factsOf(want) {
			t.Errorf("ordinary %+v\nmapped   %+v", factsOf(want), factsOf(got))
		}
	})
}

// TestExactTransportFailureOrdinaryFastHTTPResidual is the REOPENING GUARD for what the
// mapping cannot reproduce on the fasthttp backend. It pins today's divergence: if a
// change makes any of these agree (or diverge differently), this test fails and the
// residual must be re-assessed rather than silently carried.
func TestExactTransportFailureOrdinaryFastHTTPResidual(t *testing.T) {
	t.Run("chunked body unterminated: fasthttp returns a success", func(t *testing.T) {
		sc := netOnlyScenarios()[1]
		rawURL := failureServer(t, sc)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		c := NewDefaultClientWithOptions(ClientOptions{Mode: ClientModeFastHTTP})
		resp, err := c.ExecuteBorrowed(ctx, &Request{Method: http.MethodPost, URL: rawURL, Body: "{}"}, func() {})
		if err != nil {
			t.Fatalf("fasthttp now rejects an unterminated chunked body (%v): the residual changed — re-assess it", err)
		}
		resp.Release()
		if f := exactFailure(t, rawURL, 0); f.Stage != ExactFailureBodyRead {
			t.Fatalf("exact stage = %v, want a body-read failure", f.Stage)
		}
	})
	t.Run("fasthttp wording around a dial / malformed-header failure", func(t *testing.T) {
		for _, rawURL := range []string{refusedURL(t), failureServer(t, netOnlyScenarios()[0])} {
			want := ordinaryCall(t, OrdinaryBackendFastHTTP, rawURL, 0)
			got := exactFailure(t, rawURL, 0).Ordinary(OrdinaryBackendFastHTTP, http.MethodPost, rawURL)
			if got.Error() == want.Error() {
				t.Fatalf("mapped and ordinary agree (%q): the residual closed — update the Ordinary doc and move the case to the differential", got.Error())
			}
			if !strings.HasPrefix(got.Error(), "llmhttp: request failed: ") || !strings.HasPrefix(want.Error(), "llmhttp: request failed: ") {
				t.Errorf("prefix parity lost:\n  ordinary: %q\n  mapped:   %q", want.Error(), got.Error())
			}
		}
	})
}

// TestExactTransportFailureResetKeepsCategoryParity covers a connection reset mid-body,
// whose message embeds the connection's ephemeral local port and therefore differs
// between ANY two connections. Parity is the prefix, the category, and the flake
// classification on both backends, with the address normalized away.
func TestExactTransportFailureResetKeepsCategoryParity(t *testing.T) {
	sc := failureScenario{name: "reset mid-body", serve: func(c net.Conn, w *bufio.Writer) {
		fmt.Fprintf(w, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 100000\r\n\r\n0123456789")
		_ = w.Flush()
		if tc, ok := c.(*net.TCPConn); ok {
			_ = tc.SetLinger(0)
		}
	}}
	addr := regexp.MustCompile(`tcp4? [0-9.:]+->[0-9.:]+`)
	for _, backend := range []OrdinaryBackend{OrdinaryBackendFastHTTP, OrdinaryBackendNetHTTP} {
		rawURL := failureServer(t, sc)
		want := ordinaryCall(t, backend, rawURL, 0)
		got := exactFailure(t, rawURL, 0).Ordinary(backend, http.MethodPost, rawURL)
		wn, gn := addr.ReplaceAllString(want.Error(), "tcp ADDR"), addr.ReplaceAllString(got.Error(), "tcp ADDR")
		if wn != gn {
			t.Errorf("%s: normalized message:\n  ordinary: %q\n  mapped:   %q", backendName(backend), wn, gn)
		}
		wf, gf := factsOf(want), factsOf(got)
		wf.topLevelMessage, gf.topLevelMessage = "", ""
		if wf != gf {
			t.Errorf("%s: typed facts:\n  ordinary: %+v\n  mapped:   %+v", backendName(backend), wf, gf)
		}
	}
}

// TestOrdinaryUnaryBackend pins the no-I/O backend decision against the ordinary
// Client's own dispatch for the modes and schemes it covers.
func TestOrdinaryUnaryBackend(t *testing.T) {
	cases := []struct {
		mode ClientMode
		url  string
		want OrdinaryBackend
	}{
		{ClientModeAuto, "http://127.0.0.1:9/v1", OrdinaryBackendFastHTTP},
		{ClientModeAuto, "HTTP://example.invalid/v1", OrdinaryBackendFastHTTP},
		{ClientModeAuto, "https://api.example.invalid/v1", OrdinaryBackendNetHTTP},
		{ClientModeFastHTTP, "https://api.example.invalid/v1", OrdinaryBackendFastHTTP},
		{ClientModeNetHTTP, "http://127.0.0.1:9/v1", OrdinaryBackendNetHTTP},
	}
	for _, tc := range cases {
		if got := OrdinaryUnaryBackend(tc.mode, tc.url); got != tc.want {
			t.Errorf("OrdinaryUnaryBackend(%v, %q) = %v, want %v", tc.mode, tc.url, got, tc.want)
		}
	}
	// The plain-http Auto decision is the protocol cache's own, with no probe.
	cache := newProtocolCache(modeAuto, FastHTTPClientOptions{}, nil)
	origin, err := parseOrigin("http://127.0.0.1:9/v1")
	if err != nil {
		t.Fatalf("parseOrigin: %v", err)
	}
	if d := cache.probe(origin); d != decisionFast {
		t.Fatalf("protocol cache decision for plain http = %v, want fast — OrdinaryUnaryBackend no longer mirrors it", d)
	}
}

func refusedURL(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return "http://" + addr + "/v1/chat/completions"
}

func backendName(b OrdinaryBackend) string {
	if b == OrdinaryBackendFastHTTP {
		return "fasthttp"
	}
	return "nethttp"
}
