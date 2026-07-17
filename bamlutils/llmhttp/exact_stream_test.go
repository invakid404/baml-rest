package llmhttp

// Transport + lifecycle tests for the exact native STREAM lane (Phase 7A).
//
// One-request / one-socket assertions count REAL accepted TCP connections (via
// a counting net.Listener) and provider requests (via a server-side counter) —
// not merely calls to a high-level mock — matching the merge gate:
//
//   - at most one accepted provider request in every CLAIMED test;
//   - exactly one in each server-reached success/error test;
//   - zero accepted provider requests for mismatch / second-call / pre-claim
//     decline tests.

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// countingListener counts every accepted TCP connection so a test can prove the
// exact-lane one-socket invariant against real accepts (a mismatch/second-call
// must show ZERO, a claimed success/error exactly ONE).
type countingListener struct {
	net.Listener
	accepted atomic.Int64
}

func (l *countingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil {
		l.accepted.Add(1)
	}
	return c, err
}

// newCountingServer starts an httptest server whose raw TCP accepts are counted.
func newCountingServer(t *testing.T, h http.HandlerFunc) (*httptest.Server, *countingListener) {
	t.Helper()
	srv := httptest.NewUnstartedServer(h)
	cl := &countingListener{Listener: srv.Listener}
	srv.Listener = cl
	srv.Start()
	t.Cleanup(srv.Close)
	return srv, cl
}

// newCountingTLSServer starts an HTTP/2-capable TLS httptest server whose raw
// TCP accepts are counted (the counting listener sits below the TLS listener).
func newCountingTLSServer(t *testing.T, h http.HandlerFunc) (*httptest.Server, *countingListener) {
	t.Helper()
	srv := httptest.NewUnstartedServer(h)
	srv.EnableHTTP2 = true
	cl := &countingListener{Listener: srv.Listener}
	srv.Listener = cl
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv, cl
}

// testPlan is the admitted exact stream plan every test compares against.
func testPlan(serverURL string) *ExactAttemptRequest {
	return &ExactAttemptRequest{
		Method: "POST",
		URL:    serverURL + "/v1/chat/completions",
		Headers: []HeaderField{
			{"Content-Type", "application/json"},
			{"Authorization", "Bearer sk-test"},
		},
		Body:        []byte(`{"model":"gpt-4o","stream":true}`),
		BodyPresent: true,
	}
}

// mustBuildReq builds the *http.Request the exact plan describes (the request a
// matching DoStream re-prepare would produce), carrying ctx.
func mustBuildReq(t *testing.T, ctx context.Context, plan *ExactAttemptRequest) *http.Request {
	t.Helper()
	req, err := buildExactRequest(ctx, plan)
	if err != nil {
		t.Fatalf("buildExactRequest: %v", err)
	}
	return req
}

// drainRequestBody reads and discards the request body so a handler that then
// HOLDS the connection open does not leave the request body unread — an unread
// body keeps the server-side connection busy and makes httptest.Server.Close
// (run in t.Cleanup) block waiting for the handler, which would mask a
// correctly-terminating client stream as a test hang. It is a pure test-harness
// concern; the transport under test is unaffected.
func drainRequestBody(r *http.Request) {
	if r.Body != nil {
		_, _ = io.Copy(io.Discard, r.Body)
	}
}

// sseHandler streams the given chunks as an SSE response, flushing after each so
// the bytes are fragmented on the wire, and counts the request.
func sseHandler(reqCount *atomic.Int64, chunks ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if reqCount != nil {
			reqCount.Add(1)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		if fl != nil {
			fl.Flush()
		}
		for _, c := range chunks {
			_, _ = io.WriteString(w, c)
			if fl != nil {
				fl.Flush()
			}
		}
	}
}

// streamCallbacks records the ordered lifecycle callbacks a test installs.
type streamCallbacks struct {
	mu    sync.Mutex
	order []string
	claim atomic.Int64
	hdr   atomic.Int64
	first atomic.Int64
}

func (c *streamCallbacks) record(s string) {
	c.mu.Lock()
	c.order = append(c.order, s)
	c.mu.Unlock()
}

func (c *streamCallbacks) config(plan *ExactAttemptRequest, firstBody, idle time.Duration) ExactStreamClientConfig {
	return ExactStreamClientConfig{
		Expected:          plan,
		FirstBodyTimeout:  firstBody,
		IdleTimeout:       idle,
		OnClaim:           func() { c.claim.Add(1); c.record("claim") },
		OnResponseHeaders: func() { c.hdr.Add(1); c.record("headers") },
		OnFirstBody:       func() { c.first.Add(1); c.record("firstbody") },
	}
}

// TestExactStreamHappyPathOneSocket pins the happy path: exactly one accepted
// connection and one provider request, the full fragmented body reassembled
// byte-for-byte, and each lifecycle callback fired exactly once.
func TestExactStreamHappyPathOneSocket(t *testing.T) {
	var reqCount atomic.Int64
	chunks := []string{"data: {\"a\":1}\n\n", "data: {\"b\":2}\n\n", "data: [DONE]\n\n"}
	srv, cl := newCountingServer(t, sseHandler(&reqCount, chunks...))
	plan := testPlan(srv.URL)

	var cb streamCallbacks
	client, err := NewExactStreamClient(cb.config(plan, 2*time.Second, 2*time.Second))
	if err != nil {
		t.Fatalf("NewExactStreamClient: %v", err)
	}

	resp, err := client.Do(mustBuildReq(t, context.Background(), plan))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	if want := strings.Join(chunks, ""); string(body) != want {
		t.Errorf("body = %q, want %q", body, want)
	}
	if got := cl.accepted.Load(); got != 1 {
		t.Errorf("accepted connections = %d, want exactly 1", got)
	}
	if got := reqCount.Load(); got != 1 {
		t.Errorf("provider requests = %d, want exactly 1", got)
	}
	if cb.claim.Load() != 1 || cb.hdr.Load() != 1 || cb.first.Load() != 1 {
		t.Errorf("callbacks claim=%d headers=%d firstBody=%d, want 1/1/1", cb.claim.Load(), cb.hdr.Load(), cb.first.Load())
	}
}

// TestExactStreamFragmentedBytes proves the transport streams arbitrarily
// fragmented bytes — one byte at a time, including a split multi-byte UTF-8
// rune — without corruption.
func TestExactStreamFragmentedBytes(t *testing.T) {
	payload := "data: café ☕\n\n" // é and ☕ are multi-byte
	var reqCount atomic.Int64
	srv, cl := newCountingServer(t, func(w http.ResponseWriter, r *http.Request) {
		reqCount.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		fl.Flush()
		for i := 0; i < len(payload); i++ { // one raw byte per flush
			_, _ = w.Write([]byte{payload[i]})
			fl.Flush()
		}
	})
	plan := testPlan(srv.URL)

	var cb streamCallbacks
	client, err := NewExactStreamClient(cb.config(plan, 2*time.Second, 2*time.Second))
	if err != nil {
		t.Fatalf("NewExactStreamClient: %v", err)
	}
	resp, err := client.Do(mustBuildReq(t, context.Background(), plan))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(body) != payload {
		t.Errorf("reassembled body = %q, want %q", body, payload)
	}
	if cl.accepted.Load() != 1 || reqCount.Load() != 1 {
		t.Errorf("accepted=%d requests=%d, want 1/1", cl.accepted.Load(), reqCount.Load())
	}
}

// TestExactStreamMismatchZeroSocket proves any drift from the admitted plan —
// method, URL, host, header, or body — is a ZERO-SOCKET, non-claiming failure.
func TestExactStreamMismatchZeroSocket(t *testing.T) {
	cases := []struct {
		name      string
		wantField string
		mutate    func(t *testing.T, ctx context.Context, plan *ExactAttemptRequest, serverURL string) *http.Request
	}{
		{"method", "method", func(t *testing.T, ctx context.Context, plan *ExactAttemptRequest, _ string) *http.Request {
			req := mustBuildReq(t, ctx, plan)
			req.Method = "PUT"
			return req
		}},
		{"url", "url", func(t *testing.T, ctx context.Context, plan *ExactAttemptRequest, serverURL string) *http.Request {
			alt := *plan
			alt.URL = serverURL + "/v2/other/path"
			return mustBuildReq(t, ctx, &alt)
		}},
		{"url-bare-query", "url", func(t *testing.T, ctx context.Context, plan *ExactAttemptRequest, serverURL string) *http.Request {
			// A trailing "?" (ForceQuery) with an empty RawQuery must still be a
			// mismatch against the plan's bare path — the request target differs.
			alt := *plan
			alt.URL = serverURL + "/v1/chat/completions?"
			return mustBuildReq(t, ctx, &alt)
		}},
		{"host", "host", func(t *testing.T, ctx context.Context, plan *ExactAttemptRequest, _ string) *http.Request {
			req := mustBuildReq(t, ctx, plan)
			req.Host = "other.invalid"
			return req
		}},
		{"header-value", "header:Authorization", func(t *testing.T, ctx context.Context, plan *ExactAttemptRequest, _ string) *http.Request {
			req := mustBuildReq(t, ctx, plan)
			req.Header.Set("Authorization", "Bearer WRONG")
			return req
		}},
		{"header-extra", "header:X-Injected", func(t *testing.T, ctx context.Context, plan *ExactAttemptRequest, _ string) *http.Request {
			req := mustBuildReq(t, ctx, plan)
			req.Header.Add("X-Injected", "1")
			return req
		}},
		{"body", "body", func(t *testing.T, ctx context.Context, plan *ExactAttemptRequest, _ string) *http.Request {
			alt := *plan
			alt.Body = []byte(`{"model":"gpt-4o","stream":false}`)
			return mustBuildReq(t, ctx, &alt)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var reqCount atomic.Int64
			srv, cl := newCountingServer(t, sseHandler(&reqCount, "data: nope\n\n"))
			plan := testPlan(srv.URL)

			var cb streamCallbacks
			client, err := NewExactStreamClient(cb.config(plan, time.Second, time.Second))
			if err != nil {
				t.Fatalf("NewExactStreamClient: %v", err)
			}
			resp, err := client.Do(tc.mutate(t, context.Background(), plan, srv.URL))
			if resp != nil {
				resp.Body.Close()
			}
			if err == nil {
				t.Fatal("expected a plan-mismatch error, got nil")
			}
			var mm *ExactStreamPlanMismatchError
			if !errors.As(err, &mm) {
				t.Fatalf("error = %v, want *ExactStreamPlanMismatchError", err)
			}
			if mm.Field != tc.wantField {
				t.Errorf("mismatch field = %q, want %q", mm.Field, tc.wantField)
			}
			if !errors.Is(err, ErrExactStream) {
				t.Errorf("error does not unwrap to ErrExactStream: %v", err)
			}
			if got := cl.accepted.Load(); got != 0 {
				t.Errorf("accepted connections = %d, want 0 (zero socket on mismatch)", got)
			}
			if got := reqCount.Load(); got != 0 {
				t.Errorf("provider requests = %d, want 0", got)
			}
			if got := cb.claim.Load(); got != 0 {
				t.Errorf("claim fired %d times on a mismatch, want 0 (claim is post-comparison)", got)
			}
		})
	}
}

// TestExactStreamSecondRoundTripZeroSocket proves the one-shot gate: a second
// RoundTrip on the same client is refused with zero additional accepted
// connections.
func TestExactStreamSecondRoundTripZeroSocket(t *testing.T) {
	var reqCount atomic.Int64
	srv, cl := newCountingServer(t, sseHandler(&reqCount, "data: {\"x\":1}\n\n"))
	plan := testPlan(srv.URL)

	var cb streamCallbacks
	client, err := NewExactStreamClient(cb.config(plan, time.Second, time.Second))
	if err != nil {
		t.Fatalf("NewExactStreamClient: %v", err)
	}

	// First call: a real, successful, single-socket stream.
	resp, err := client.Do(mustBuildReq(t, context.Background(), plan))
	if err != nil {
		t.Fatalf("first Do: %v", err)
	}
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()

	// Second call on the same client: refused, zero additional socket.
	resp2, err2 := client.Do(mustBuildReq(t, context.Background(), plan))
	if resp2 != nil {
		resp2.Body.Close()
	}
	if !errors.Is(err2, ErrExactStreamSecondRoundTrip) {
		t.Fatalf("second Do error = %v, want ErrExactStreamSecondRoundTrip", err2)
	}
	if !errors.Is(err2, ErrExactStream) {
		t.Errorf("second-call error does not unwrap to ErrExactStream")
	}
	if got := cl.accepted.Load(); got != 1 {
		t.Errorf("accepted connections = %d, want exactly 1 (second call opened none)", got)
	}
	if got := reqCount.Load(); got != 1 {
		t.Errorf("provider requests = %d, want exactly 1", got)
	}
	if got := cb.claim.Load(); got != 1 {
		t.Errorf("claim fired %d times, want exactly 1 (second call never claims)", got)
	}
}

// TestExactStreamRedirectNotFollowed proves a redirect response is returned
// as-is and never followed (one request, one socket).
func TestExactStreamRedirectNotFollowed(t *testing.T) {
	var reqCount atomic.Int64
	srv, cl := newCountingServer(t, func(w http.ResponseWriter, r *http.Request) {
		reqCount.Add(1)
		w.Header().Set("Location", "https://elsewhere.invalid/next")
		w.WriteHeader(http.StatusFound) // 302
	})
	plan := testPlan(srv.URL)

	var cb streamCallbacks
	client, err := NewExactStreamClient(cb.config(plan, time.Second, time.Second))
	if err != nil {
		t.Fatalf("NewExactStreamClient: %v", err)
	}
	resp, err := client.Do(mustBuildReq(t, context.Background(), plan))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Errorf("status = %d, want 302 (returned, not followed)", resp.StatusCode)
	}
	if resp.Header.Get("Location") != "https://elsewhere.invalid/next" {
		t.Errorf("Location header lost; redirect appears followed")
	}
	if cl.accepted.Load() != 1 || reqCount.Load() != 1 {
		t.Errorf("accepted=%d requests=%d, want 1/1 (redirect not followed)", cl.accepted.Load(), reqCount.Load())
	}
	if cb.hdr.Load() != 0 {
		t.Errorf("response-header callback fired on a non-2xx redirect, want 0")
	}
}

// TestExactStreamSetCookieNoJar proves the client keeps no cookies: Jar is nil
// so a Set-Cookie induces no cookie state or follow-up.
func TestExactStreamSetCookieNoJar(t *testing.T) {
	var reqCount atomic.Int64
	srv, cl := newCountingServer(t, func(w http.ResponseWriter, r *http.Request) {
		reqCount.Add(1)
		if c, err := r.Cookie("sid"); err == nil {
			t.Errorf("request carried an unexpected cookie: %v", c)
		}
		w.Header().Set("Set-Cookie", "sid=abc; Path=/")
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: ok\n\n")
	})
	plan := testPlan(srv.URL)

	var cb streamCallbacks
	client, err := NewExactStreamClient(cb.config(plan, time.Second, time.Second))
	if err != nil {
		t.Fatalf("NewExactStreamClient: %v", err)
	}
	if client.Jar != nil {
		t.Fatal("client.Jar is non-nil; the exact stream lane must keep no cookies")
	}
	resp, err := client.Do(mustBuildReq(t, context.Background(), plan))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if cl.accepted.Load() != 1 || reqCount.Load() != 1 {
		t.Errorf("accepted=%d requests=%d, want 1/1", cl.accepted.Load(), reqCount.Load())
	}
}

// TestExactStreamProxyEnvIgnored proves proxy environment variables have no
// effect: the hardened transport has Proxy=nil, so the request goes direct even
// with a dead proxy configured.
func TestExactStreamProxyEnvIgnored(t *testing.T) {
	// A proxy that, if honored, would break the request (nothing listening).
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	t.Setenv("http_proxy", "http://127.0.0.1:1")

	// Direct structural guard: a loopback httptest target is bypassed by Go's
	// env-proxy logic regardless of the transport, so the behavioural check
	// below cannot catch a regression to ProxyFromEnvironment. Assert the
	// hardened transport pins Proxy=nil so such a regression fails here.
	if tr := newHardenedExactStreamTransport(); tr.Proxy != nil {
		t.Fatal("hardened stream transport has a non-nil Proxy; env proxies must never be honored")
	}

	var reqCount atomic.Int64
	srv, cl := newCountingServer(t, sseHandler(&reqCount, "data: direct\n\n"))
	plan := testPlan(srv.URL)

	var cb streamCallbacks
	client, err := NewExactStreamClient(cb.config(plan, time.Second, time.Second))
	if err != nil {
		t.Fatalf("NewExactStreamClient: %v", err)
	}
	resp, err := client.Do(mustBuildReq(t, context.Background(), plan))
	if err != nil {
		t.Fatalf("Do (proxy env should be ignored): %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "data: direct\n\n" {
		t.Errorf("body = %q, want direct response", body)
	}
	if cl.accepted.Load() != 1 || reqCount.Load() != 1 {
		t.Errorf("accepted=%d requests=%d, want 1/1", cl.accepted.Load(), reqCount.Load())
	}
}

// TestExactStreamStaleConnNoReplay proves a torn-down connection is not replayed
// onto a fresh socket: keep-alives are disabled, so a single Do yields exactly
// one accepted connection even when the server hangs up without responding.
func TestExactStreamStaleConnNoReplay(t *testing.T) {
	var reqCount atomic.Int64
	srv, cl := newCountingServer(t, func(w http.ResponseWriter, r *http.Request) {
		reqCount.Add(1)
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("server does not support hijacking")
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Fatalf("hijack: %v", err)
		}
		_ = conn.Close() // tear down without any response
	})
	plan := testPlan(srv.URL)

	var cb streamCallbacks
	client, err := NewExactStreamClient(cb.config(plan, time.Second, time.Second))
	if err != nil {
		t.Fatalf("NewExactStreamClient: %v", err)
	}
	resp, err := client.Do(mustBuildReq(t, context.Background(), plan))
	if resp != nil {
		resp.Body.Close()
	}
	if err == nil {
		t.Fatal("expected a transport error from the torn-down connection")
	}
	if got := cl.accepted.Load(); got != 1 {
		t.Errorf("accepted connections = %d, want exactly 1 (no stale-conn replay)", got)
	}
	if got := reqCount.Load(); got != 1 {
		t.Errorf("provider requests = %d, want exactly 1", got)
	}
}

// TestExactStreamGzipNotRequestedNorDecoded proves the transport neither
// advertises gzip nor transparently decodes a gzip response: no Accept-Encoding
// is sent, and a Content-Encoding: gzip body is returned raw.
func TestExactStreamGzipNotRequestedNorDecoded(t *testing.T) {
	var gzBody bytes.Buffer
	gzw := gzip.NewWriter(&gzBody)
	_, _ = gzw.Write([]byte("data: hello\n\n"))
	_ = gzw.Close()
	raw := gzBody.Bytes()

	// A buffered channel carries the handler-observed Accept-Encoding to the test
	// goroutine with a proper happens-before edge (no unsynchronized shared var).
	acceptEncodingCh := make(chan string, 1)
	var reqCount atomic.Int64
	srv, cl := newCountingServer(t, func(w http.ResponseWriter, r *http.Request) {
		reqCount.Add(1)
		select {
		case acceptEncodingCh <- r.Header.Get("Accept-Encoding"):
		default:
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(raw)
	})
	plan := testPlan(srv.URL)

	var cb streamCallbacks
	client, err := NewExactStreamClient(cb.config(plan, time.Second, time.Second))
	if err != nil {
		t.Fatalf("NewExactStreamClient: %v", err)
	}
	resp, err := client.Do(mustBuildReq(t, context.Background(), plan))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	var gotAcceptEncoding string
	select {
	case gotAcceptEncoding = <-acceptEncodingCh:
	case <-time.After(time.Second):
		t.Fatal("handler did not report the request Accept-Encoding")
	}
	if gotAcceptEncoding != "" {
		t.Errorf("Accept-Encoding sent = %q, want empty (gzip not requested)", gotAcceptEncoding)
	}
	if resp.Header.Get("Content-Encoding") != "gzip" {
		t.Errorf("Content-Encoding = %q, want gzip preserved (not transparently decoded)", resp.Header.Get("Content-Encoding"))
	}
	if !bytes.Equal(body, raw) {
		t.Errorf("body was transparently decoded; got %d bytes, want the %d raw gzip bytes", len(body), len(raw))
	}
	if cl.accepted.Load() != 1 || reqCount.Load() != 1 {
		t.Errorf("accepted=%d requests=%d, want 1/1", cl.accepted.Load(), reqCount.Load())
	}
}

// TestExactStreamHTTP2NotNegotiated proves the hardened stream transport stays
// HTTP/1.1 even against an HTTP/2-capable TLS server (the explicit empty
// TLSNextProto forces H1).
func TestExactStreamHTTP2NotNegotiated(t *testing.T) {
	var reqCount atomic.Int64
	srv, cl := newCountingTLSServer(t, sseHandler(&reqCount, "data: h1\n\n"))
	plan := testPlan(srv.URL)

	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	tr := newHardenedExactStreamTransport()
	// Trust the test cert only; keep the hardened TLSNextProto (H1-forcing).
	tr.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool}

	var cb streamCallbacks
	cfg := cb.config(plan, time.Second, time.Second)
	cfg.transport = tr
	client, err := NewExactStreamClient(cfg)
	if err != nil {
		t.Fatalf("NewExactStreamClient: %v", err)
	}
	resp, err := client.Do(mustBuildReq(t, context.Background(), plan))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.ProtoMajor != 1 {
		t.Errorf("negotiated %s, want HTTP/1.1 (H2 must not be negotiated)", resp.Proto)
	}
	_, _ = io.ReadAll(resp.Body)
	if cl.accepted.Load() != 1 || reqCount.Load() != 1 {
		t.Errorf("accepted=%d requests=%d, want 1/1", cl.accepted.Load(), reqCount.Load())
	}
}

// TestExactStreamHeadersBeforeFirstBody pins the callback ordering: claim (before
// the RoundTrip) precedes the response-header signal, which precedes the
// first-body signal.
func TestExactStreamHeadersBeforeFirstBody(t *testing.T) {
	srv, _ := newCountingServer(t, sseHandler(nil, "data: first\n\n", "data: second\n\n"))
	plan := testPlan(srv.URL)

	var cb streamCallbacks
	client, err := NewExactStreamClient(cb.config(plan, 2*time.Second, 2*time.Second))
	if err != nil {
		t.Fatalf("NewExactStreamClient: %v", err)
	}
	resp, err := client.Do(mustBuildReq(t, context.Background(), plan))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()

	cb.mu.Lock()
	got := strings.Join(cb.order, ",")
	cb.mu.Unlock()
	if got != "claim,headers,firstbody" {
		t.Errorf("callback order = %q, want claim,headers,firstbody", got)
	}
}

// TestExactStreamFirstBodyTimeout proves the first-body deadline fires when
// headers arrive but no body byte follows — and that it is distinct from idle.
func TestExactStreamFirstBodyTimeout(t *testing.T) {
	var reqCount atomic.Int64
	srv, cl := newCountingServer(t, func(w http.ResponseWriter, r *http.Request) {
		reqCount.Add(1)
		drainRequestBody(r)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		// Send headers, then no body until the client gives up (or a hard cap).
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
		}
	})
	plan := testPlan(srv.URL)

	var cb streamCallbacks
	// Short first-body bound; generous idle bound so the two are unambiguous.
	client, err := NewExactStreamClient(cb.config(plan, 150*time.Millisecond, 10*time.Second))
	if err != nil {
		t.Fatalf("NewExactStreamClient: %v", err)
	}
	resp, err := client.Do(mustBuildReq(t, context.Background(), plan))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	_, err = io.ReadAll(resp.Body)
	if !errors.Is(err, ErrExactStreamFirstBodyTimeout) {
		t.Fatalf("read error = %v, want ErrExactStreamFirstBodyTimeout", err)
	}
	if cb.hdr.Load() != 1 {
		t.Errorf("response-header callback fired %d times, want 1", cb.hdr.Load())
	}
	if cb.first.Load() != 0 {
		t.Errorf("first-body callback fired %d times, want 0 (no body arrived)", cb.first.Load())
	}
	if cl.accepted.Load() != 1 || reqCount.Load() != 1 {
		t.Errorf("accepted=%d requests=%d, want 1/1", cl.accepted.Load(), reqCount.Load())
	}
}

// TestExactStreamIdleTimeoutResetsOnEveryByte proves the inter-byte idle
// deadline resets on every raw byte — including SSE comment lines and partial
// lines — and fires only after a real silence following the first body byte.
func TestExactStreamIdleTimeoutResetsOnEveryByte(t *testing.T) {
	// Gaps well under the idle window keep the stream alive; the trailing silence
	// exceeds it. Comments (":...") and a split "da"+"ta:" line prove byte-level
	// (not event-level) resets.
	chunks := []string{"data: a\n\n", ": keepalive\n", "da", "ta: b\n\n", ": ping\n"}
	const idle = 500 * time.Millisecond
	const gap = 25 * time.Millisecond

	var reqCount atomic.Int64
	srv, cl := newCountingServer(t, func(w http.ResponseWriter, r *http.Request) {
		reqCount.Add(1)
		drainRequestBody(r)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		fl.Flush()
		for _, c := range chunks {
			_, _ = io.WriteString(w, c)
			fl.Flush()
			time.Sleep(gap)
		}
		// Then go silent past the idle window so the watchdog fires.
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
		}
	})
	plan := testPlan(srv.URL)

	var cb streamCallbacks
	client, err := NewExactStreamClient(cb.config(plan, 10*time.Second, idle))
	if err != nil {
		t.Fatalf("NewExactStreamClient: %v", err)
	}
	resp, err := client.Do(mustBuildReq(t, context.Background(), plan))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if !errors.Is(err, ErrExactStreamIdleTimeout) {
		t.Fatalf("read error = %v, want ErrExactStreamIdleTimeout", err)
	}
	// Every trickled byte was delivered (proving the resets worked) before the
	// idle sentinel surfaced.
	if want := strings.Join(chunks, ""); string(body) != want {
		t.Errorf("delivered body = %q, want %q (idle must reset on every byte)", body, want)
	}
	if cb.first.Load() != 1 {
		t.Errorf("first-body callback fired %d times, want 1", cb.first.Load())
	}
	if cl.accepted.Load() != 1 || reqCount.Load() != 1 {
		t.Errorf("accepted=%d requests=%d, want 1/1", cl.accepted.Load(), reqCount.Load())
	}
}

// TestExactStreamNon2xxPreserved proves a non-2xx response is returned with its
// status and body verbatim, without retry and without firing the 2xx callbacks.
func TestExactStreamNon2xxPreserved(t *testing.T) {
	const errBody = `{"error":{"message":"boom","type":"server_error"}}`
	var reqCount atomic.Int64
	srv, cl := newCountingServer(t, func(w http.ResponseWriter, r *http.Request) {
		reqCount.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, errBody)
	})
	plan := testPlan(srv.URL)

	var cb streamCallbacks
	client, err := NewExactStreamClient(cb.config(plan, time.Second, time.Second))
	if err != nil {
		t.Fatalf("NewExactStreamClient: %v", err)
	}
	resp, err := client.Do(mustBuildReq(t, context.Background(), plan))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 (non-2xx preserved)", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != errBody {
		t.Errorf("body = %q, want the provider error body verbatim", body)
	}
	if cb.hdr.Load() != 0 || cb.first.Load() != 0 {
		t.Errorf("2xx callbacks fired on a non-2xx: headers=%d firstBody=%d, want 0/0", cb.hdr.Load(), cb.first.Load())
	}
	if cb.claim.Load() != 1 {
		t.Errorf("claim fired %d times, want 1 (the request was sent)", cb.claim.Load())
	}
	if cl.accepted.Load() != 1 || reqCount.Load() != 1 {
		t.Errorf("accepted=%d requests=%d, want 1/1 (no retry)", cl.accepted.Load(), reqCount.Load())
	}
}

// TestExactStreamInvalidContentType proves a 2xx whose Content-Type is not
// text/event-stream is a bounded, typed error — and that an oversized,
// attacker-controlled Content-Type is truncated to the diagnostic bound rather
// than surfaced whole.
func TestExactStreamInvalidContentType(t *testing.T) {
	// Oversized non-SSE Content-Type: a real media type followed by a long
	// attacker-controlled parameter, well past the 128-byte diagnostic cap.
	oversizedCT := "application/json; junk=" + strings.Repeat("A", 4096)

	var reqCount atomic.Int64
	srv, cl := newCountingServer(t, func(w http.ResponseWriter, r *http.Request) {
		reqCount.Add(1)
		w.Header().Set("Content-Type", oversizedCT)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"not":"sse"}`)
	})
	plan := testPlan(srv.URL)

	var cb streamCallbacks
	client, err := NewExactStreamClient(cb.config(plan, time.Second, time.Second))
	if err != nil {
		t.Fatalf("NewExactStreamClient: %v", err)
	}
	resp, err := client.Do(mustBuildReq(t, context.Background(), plan))
	if resp != nil {
		resp.Body.Close()
	}
	var cte *ExactStreamContentTypeError
	if !errors.As(err, &cte) {
		t.Fatalf("error = %v, want *ExactStreamContentTypeError", err)
	}
	if !strings.Contains(cte.MediaType, "application/json") {
		t.Errorf("MediaType = %q, want it to mention application/json", cte.MediaType)
	}
	// The bounded-diagnostic guarantee: the surfaced media type never grows with
	// the attacker-controlled header (128-byte cap), so a truncation regression
	// fails here.
	if len(cte.MediaType) > 128 {
		t.Errorf("MediaType length = %d, want <= 128 (bounded diagnostic)", len(cte.MediaType))
	}
	if strings.Contains(cte.MediaType, strings.Repeat("A", 200)) {
		t.Error("MediaType surfaced the oversized attacker-controlled header unbounded")
	}
	if cb.hdr.Load() != 0 {
		t.Errorf("response-header callback fired on an invalid content type, want 0")
	}
	if cl.accepted.Load() != 1 || reqCount.Load() != 1 {
		t.Errorf("accepted=%d requests=%d, want 1/1", cl.accepted.Load(), reqCount.Load())
	}
}

// TestExactStreamCancelPreClaimZeroSocket proves an already-cancelled context
// declines before the claim with zero socket and no claim callback.
func TestExactStreamCancelPreClaimZeroSocket(t *testing.T) {
	var reqCount atomic.Int64
	srv, cl := newCountingServer(t, sseHandler(&reqCount, "data: unreached\n\n"))
	plan := testPlan(srv.URL)

	var cb streamCallbacks
	client, err := NewExactStreamClient(cb.config(plan, time.Second, time.Second))
	if err != nil {
		t.Fatalf("NewExactStreamClient: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled before the request is issued
	resp, err := client.Do(mustBuildReq(t, ctx, plan))
	if resp != nil {
		resp.Body.Close()
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if got := cl.accepted.Load(); got != 0 {
		t.Errorf("accepted connections = %d, want 0 (pre-claim cancel opens no socket)", got)
	}
	if got := cb.claim.Load(); got != 0 {
		t.Errorf("claim fired %d times on a pre-cancelled request, want 0", got)
	}
	if got := reqCount.Load(); got != 0 {
		t.Errorf("provider requests = %d, want 0", got)
	}
}

// TestExactStreamCancelAfterHeaders proves cancelling after headers (before the
// first body byte) promptly ends the stream with the caller's cancellation, not
// a first-body timeout.
func TestExactStreamCancelAfterHeaders(t *testing.T) {
	srv, cl := newCountingServer(t, func(w http.ResponseWriter, r *http.Request) {
		drainRequestBody(r)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		// Hold headers, send no body until the client cancels (or a hard cap).
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
		}
	})
	plan := testPlan(srv.URL)

	var cb streamCallbacks
	client, err := NewExactStreamClient(cb.config(plan, 10*time.Second, 10*time.Second))
	if err != nil {
		t.Fatalf("NewExactStreamClient: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	resp, err := client.Do(mustBuildReq(t, ctx, plan))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	_, err = io.ReadAll(resp.Body)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("read error = %v, want context.Canceled", err)
	}
	if got := cl.accepted.Load(); got != 1 {
		t.Errorf("accepted connections = %d, want exactly 1", got)
	}
}

// TestExactStreamCancelBetweenDeltas proves cancelling mid-stream (after a
// delivered delta) ends promptly with the caller's cancellation.
func TestExactStreamCancelBetweenDeltas(t *testing.T) {
	srv, cl := newCountingServer(t, func(w http.ResponseWriter, r *http.Request) {
		drainRequestBody(r)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		fl.Flush()
		_, _ = io.WriteString(w, "data: first\n\n")
		fl.Flush()
		// Then hold silent until the client cancels (or a hard cap).
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
		}
	})
	plan := testPlan(srv.URL)

	var cb streamCallbacks
	client, err := NewExactStreamClient(cb.config(plan, 10*time.Second, 10*time.Second))
	if err != nil {
		t.Fatalf("NewExactStreamClient: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	resp, err := client.Do(mustBuildReq(t, ctx, plan))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	buf := make([]byte, len("data: first\n\n"))
	if _, err := io.ReadFull(resp.Body, buf); err != nil {
		t.Fatalf("read first delta: %v", err)
	}
	if string(buf) != "data: first\n\n" {
		t.Fatalf("first delta = %q", buf)
	}
	if cb.first.Load() != 1 {
		t.Errorf("first-body callback fired %d times, want 1", cb.first.Load())
	}
	cancel()
	if _, err := io.ReadAll(resp.Body); !errors.Is(err, context.Canceled) {
		t.Fatalf("post-cancel read error = %v, want context.Canceled", err)
	}
	if cl.accepted.Load() != 1 {
		t.Errorf("accepted connections = %d, want exactly 1", cl.accepted.Load())
	}
}

// TestExactStreamNoGoroutineLeak runs a mix of terminal outcomes (success,
// first-body timeout, cancellation) and asserts the lane leaves no residual
// goroutines — timers stopped, bodies closed, watchers unwound.
func TestExactStreamNoGoroutineLeak(t *testing.T) {
	successSrv, _ := newCountingServer(t, sseHandler(nil, "data: a\n\n", "data: b\n\n"))
	hangSrv, _ := newCountingServer(t, func(w http.ResponseWriter, r *http.Request) {
		drainRequestBody(r)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	})

	runOnce := func() {
		// success
		{
			plan := testPlan(successSrv.URL)
			c, _ := NewExactStreamClient(ExactStreamClientConfig{Expected: plan, FirstBodyTimeout: time.Second, IdleTimeout: time.Second})
			resp, err := c.Do(mustBuildReq(t, context.Background(), plan))
			if err == nil {
				_, _ = io.ReadAll(resp.Body)
				resp.Body.Close()
			}
		}
		// first-body timeout
		{
			plan := testPlan(hangSrv.URL)
			c, _ := NewExactStreamClient(ExactStreamClientConfig{Expected: plan, FirstBodyTimeout: 30 * time.Millisecond, IdleTimeout: time.Second})
			resp, err := c.Do(mustBuildReq(t, context.Background(), plan))
			if err == nil {
				_, _ = io.ReadAll(resp.Body)
				resp.Body.Close()
			}
		}
		// cancellation after headers
		{
			plan := testPlan(hangSrv.URL)
			c, _ := NewExactStreamClient(ExactStreamClientConfig{Expected: plan, FirstBodyTimeout: 5 * time.Second, IdleTimeout: 5 * time.Second})
			ctx, cancel := context.WithCancel(context.Background())
			resp, err := c.Do(mustBuildReq(t, ctx, plan))
			if err == nil {
				go func() { time.Sleep(20 * time.Millisecond); cancel() }()
				_, _ = io.ReadAll(resp.Body)
				resp.Body.Close()
			}
			cancel()
		}
	}

	// Warm up, then measure across a batch.
	for i := 0; i < 5; i++ {
		runOnce()
	}
	runtime.GC()
	time.Sleep(100 * time.Millisecond)
	before := runtime.NumGoroutine()
	for i := 0; i < 30; i++ {
		runOnce()
	}
	runtime.GC()
	time.Sleep(200 * time.Millisecond)
	after := runtime.NumGoroutine()

	if after > before+4 {
		t.Errorf("goroutine leak suspected: before=%d after=%d", before, after)
	}
}

// TestExactStreamConcurrentCloseCancelSecondCall exercises concurrent
// Close / cancel / second-Do against a live stream. It is a race-detector
// target (`go test -race`): the one-shot gate, watchdog, and phase/first-byte
// state must be free of data races, and no path may panic.
func TestExactStreamConcurrentCloseCancelSecondCall(t *testing.T) {
	srv, cl := newCountingServer(t, func(w http.ResponseWriter, r *http.Request) {
		drainRequestBody(r)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		fl.Flush()
		_, _ = io.WriteString(w, "data: x\n\n")
		fl.Flush()
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	})
	plan := testPlan(srv.URL)

	for iter := 0; iter < 20; iter++ {
		beforeAccepted := cl.accepted.Load()
		client, err := NewExactStreamClient(ExactStreamClientConfig{Expected: plan, FirstBodyTimeout: 200 * time.Millisecond, IdleTimeout: 200 * time.Millisecond})
		if err != nil {
			t.Fatalf("NewExactStreamClient: %v", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		// The first Do wins the one-shot gate; a setup failure is fatal, not
		// skipped (a skip would let the whole test pass vacuously).
		resp, err := client.Do(mustBuildReq(t, ctx, plan))
		if err != nil {
			cancel()
			t.Fatalf("iter %d: initial Do: %v", iter, err)
		}

		var wg sync.WaitGroup
		wg.Add(4)
		go func() { defer wg.Done(); _, _ = io.ReadAll(resp.Body) }()
		go func() { defer wg.Done(); time.Sleep(time.Millisecond); _ = resp.Body.Close() }()
		go func() { defer wg.Done(); time.Sleep(time.Millisecond); cancel() }()
		go func() {
			defer wg.Done()
			// The concurrent second call must ALWAYS be refused without a socket
			// (the first Do already claimed the one-shot gate) and must never
			// panic regardless of interleaving with Close/cancel.
			r2, e2 := client.Do(mustBuildReq(t, context.Background(), plan))
			if r2 != nil {
				r2.Body.Close()
			}
			if !errors.Is(e2, ErrExactStreamSecondRoundTrip) {
				t.Errorf("iter %d: concurrent second Do error = %v, want ErrExactStreamSecondRoundTrip", iter, e2)
			}
		}()
		wg.Wait()
		cancel()

		// Exactly one physical connection per iteration: the first Do dialed once;
		// the refused second call opened none, and nothing was replayed.
		if got := cl.accepted.Load() - beforeAccepted; got != 1 {
			t.Errorf("iter %d: accepted connections this iteration = %d, want exactly 1", iter, got)
		}
	}
}

// TestExactStreamNilExpectedRejected proves the constructor refuses a nil plan.
func TestExactStreamNilExpectedRejected(t *testing.T) {
	if _, err := NewExactStreamClient(ExactStreamClientConfig{}); err == nil {
		t.Fatal("expected an error for a nil Expected plan")
	}
}

// TestExactStreamForceQueryDriftBothDirectionsZeroSocket proves a bare trailing
// "?" (ForceQuery, empty RawQuery) drift is a zero-socket, zero-claim mismatch
// in BOTH directions against a real listener — admitted-bare vs actual-"?" AND
// admitted-"?" vs actual-bare — the exact pair a RawQuery-only comparison would
// wrongly admit.
func TestExactStreamForceQueryDriftBothDirectionsZeroSocket(t *testing.T) {
	cases := []struct {
		name        string
		expectedURL func(base string) string
		actualURL   func(base string) string
	}{
		{"admitted-bare_actual-query",
			func(b string) string { return b + "/v1/chat/completions" },
			func(b string) string { return b + "/v1/chat/completions?" }},
		{"admitted-query_actual-bare",
			func(b string) string { return b + "/v1/chat/completions?" },
			func(b string) string { return b + "/v1/chat/completions" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var reqCount atomic.Int64
			srv, cl := newCountingServer(t, sseHandler(&reqCount, "data: nope\n\n"))

			expected := testPlan(srv.URL)
			expected.URL = tc.expectedURL(srv.URL)
			actual := testPlan(srv.URL)
			actual.URL = tc.actualURL(srv.URL)

			var cb streamCallbacks
			client, err := NewExactStreamClient(cb.config(expected, time.Second, time.Second))
			if err != nil {
				t.Fatalf("NewExactStreamClient: %v", err)
			}
			resp, err := client.Do(mustBuildReq(t, context.Background(), actual))
			if resp != nil {
				resp.Body.Close()
			}
			var mm *ExactStreamPlanMismatchError
			if !errors.As(err, &mm) || mm.Field != "url" {
				t.Fatalf("error = %v, want a url plan mismatch", err)
			}
			if cl.accepted.Load() != 0 || reqCount.Load() != 0 || cb.claim.Load() != 0 {
				t.Errorf("accepted=%d requests=%d claim=%d, want 0/0/0 (zero socket + zero claim)",
					cl.accepted.Load(), reqCount.Load(), cb.claim.Load())
			}
		})
	}
}

// TestExactStreamFirstBodyDeadlineArmedBeforeHeaderCallback proves the first-body
// deadline is armed BEFORE OnResponseHeaders: a header callback blocked well
// past the first-body window cannot extend it. The server observes its request
// context cancel (its connection severed) — driven by the first-body timer —
// WHILE the header callback is still blocked, which is only possible if the
// deadline was armed before (and independently of) the callback.
func TestExactStreamFirstBodyDeadlineArmedBeforeHeaderCallback(t *testing.T) {
	connSevered := make(chan struct{})
	releaseCb := make(chan struct{})
	srv, cl := newCountingServer(t, func(w http.ResponseWriter, r *http.Request) {
		drainRequestBody(r)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done() // fires when the first-body timer severs the conn
		close(connSevered)
	})
	plan := testPlan(srv.URL)

	const firstBody = 100 * time.Millisecond
	cfg := ExactStreamClientConfig{
		Expected:         plan,
		FirstBodyTimeout: firstBody,
		IdleTimeout:      10 * time.Second,
		OnResponseHeaders: func() {
			// Block far past the first-body window. If the deadline is armed
			// before this callback (correct), the first-body timer still fires and
			// severs the conn while we are blocked here.
			<-releaseCb
		},
	}
	client, err := NewExactStreamClient(cfg)
	if err != nil {
		t.Fatalf("NewExactStreamClient: %v", err)
	}

	// Do blocks until OnResponseHeaders returns; run it off the test goroutine.
	done := make(chan struct{})
	go func() {
		defer close(done)
		resp, derr := client.Do(mustBuildReq(t, context.Background(), plan))
		if derr == nil {
			_, _ = io.ReadAll(resp.Body)
			resp.Body.Close()
		}
	}()

	select {
	case <-connSevered:
		// Good: the first-body deadline fired independently of the still-blocked
		// header callback.
	case <-time.After(3 * time.Second):
		close(releaseCb)
		<-done
		t.Fatal("first-body deadline did not fire while the header callback was blocked; it appears armed only AFTER the callback")
	}
	close(releaseCb)
	<-done

	if got := cl.accepted.Load(); got != 1 {
		t.Errorf("accepted connections = %d, want exactly 1", got)
	}
}

// TestExactStreamIdleArmedBeforeFirstBodyCallback proves the idle deadline is
// switched on BEFORE OnFirstBody: a first-body callback blocked past the idle
// window cannot delay idle arming. The server observes its connection severed —
// driven by the idle timer — WHILE OnFirstBody is still blocked, which is only
// possible if idle was armed before (and independently of) the callback.
func TestExactStreamIdleArmedBeforeFirstBodyCallback(t *testing.T) {
	connSevered := make(chan struct{})
	releaseCb := make(chan struct{})
	srv, cl := newCountingServer(t, func(w http.ResponseWriter, r *http.Request) {
		drainRequestBody(r)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		fl.Flush()
		_, _ = io.WriteString(w, "data: first\n\n") // one body byte, then silence
		fl.Flush()
		<-r.Context().Done() // fires when the idle timer severs the conn
		close(connSevered)
	})
	plan := testPlan(srv.URL)

	const idle = 100 * time.Millisecond
	cfg := ExactStreamClientConfig{
		Expected:         plan,
		FirstBodyTimeout: 10 * time.Second, // large: the first byte arrives quickly
		IdleTimeout:      idle,
		OnFirstBody: func() {
			// Block far past the idle window. If idle is armed before this callback
			// (correct), the idle timer still fires and severs the conn while we
			// are blocked here.
			<-releaseCb
		},
	}
	client, err := NewExactStreamClient(cfg)
	if err != nil {
		t.Fatalf("NewExactStreamClient: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		resp, derr := client.Do(mustBuildReq(t, context.Background(), plan))
		if derr == nil {
			_, _ = io.ReadAll(resp.Body)
			resp.Body.Close()
		}
	}()

	select {
	case <-connSevered:
		// Good: the idle deadline fired independently of the still-blocked
		// first-body callback.
	case <-time.After(3 * time.Second):
		close(releaseCb)
		<-done
		t.Fatal("idle deadline did not fire while OnFirstBody was blocked; idle arming appears delayed until AFTER the callback")
	}
	close(releaseCb)
	<-done

	if got := cl.accepted.Load(); got != 1 {
		t.Errorf("accepted connections = %d, want exactly 1", got)
	}
}

// TestExactStreamBodyPresenceMismatchZeroSocket proves body PRESENCE is part of
// the admitted-plan match: a present zero-length body never matches an absent
// one (and vice versa), so bytes.Equal collapsing nil and empty cannot admit a
// presence drift. Each is a zero-socket, zero-claim mismatch.
func TestExactStreamBodyPresenceMismatchZeroSocket(t *testing.T) {
	cases := []struct {
		name            string
		expectedBody    []byte
		expectedPresent bool
		actualBody      []byte
		actualPresent   bool
	}{
		{"expected-present_actual-absent", []byte(`{"x":1}`), true, nil, false},
		{"expected-emptybody_actual-absent", []byte{}, true, nil, false},
		{"expected-absent_actual-emptybody", nil, false, []byte{}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var reqCount atomic.Int64
			srv, cl := newCountingServer(t, sseHandler(&reqCount, "data: nope\n\n"))

			expected := testPlan(srv.URL)
			expected.Body = tc.expectedBody
			expected.BodyPresent = tc.expectedPresent
			actual := testPlan(srv.URL)
			actual.Body = tc.actualBody
			actual.BodyPresent = tc.actualPresent

			var cb streamCallbacks
			client, err := NewExactStreamClient(cb.config(expected, time.Second, time.Second))
			if err != nil {
				t.Fatalf("NewExactStreamClient: %v", err)
			}
			resp, err := client.Do(mustBuildReq(t, context.Background(), actual))
			if resp != nil {
				resp.Body.Close()
			}
			var mm *ExactStreamPlanMismatchError
			if !errors.As(err, &mm) || mm.Field != "body" {
				t.Fatalf("error = %v, want a body plan mismatch", err)
			}
			if cl.accepted.Load() != 0 || reqCount.Load() != 0 || cb.claim.Load() != 0 {
				t.Errorf("accepted=%d requests=%d claim=%d, want 0/0/0 (zero socket + zero claim)",
					cl.accepted.Load(), reqCount.Load(), cb.claim.Load())
			}
		})
	}
}

// TestSameRequestURLExactness pins the exact-lane URL equality: equal under
// host-casing, but a drift in scheme, opaque, host, path, query, bare-query
// (ForceQuery), or userinfo is unequal.
func TestSameRequestURLExactness(t *testing.T) {
	mustParse := func(s string) *url.URL {
		u, err := url.Parse(s)
		if err != nil {
			t.Fatalf("parse %q: %v", s, err)
		}
		return u
	}
	equal := []struct{ name, a, b string }{
		{"identical", "http://h/p?q=1", "http://h/p?q=1"},
		{"host-case-insensitive", "http://Host/p", "http://host/p"},
	}
	for _, tc := range equal {
		if !sameRequestURL(mustParse(tc.a), mustParse(tc.b)) {
			t.Errorf("%s: sameRequestURL(%q,%q) = false, want true", tc.name, tc.a, tc.b)
		}
	}
	differ := []struct{ name, a, b string }{
		{"scheme", "http://h/p", "https://h/p"},
		{"opaque", "http:opaque", "http:other"},
		{"host", "http://h1/p", "http://h2/p"},
		{"path", "http://h/p1", "http://h/p2"},
		{"query", "http://h/p?a=1", "http://h/p?a=2"},
		{"force-query", "http://h/p", "http://h/p?"},
		{"userinfo", "http://u@h/p", "http://v@h/p"},
	}
	for _, tc := range differ {
		if sameRequestURL(mustParse(tc.a), mustParse(tc.b)) {
			t.Errorf("%s: sameRequestURL(%q,%q) = true, want false", tc.name, tc.a, tc.b)
		}
	}
}

// TestByteProgressWatchdogRejectsStaleCallback deterministically pins the
// generation guard: a timer callback from a superseded (re-armed or stopped)
// window never interrupts, so a stale, already-expired callback cannot surface a
// false idle timeout on a healthy stream. fire() is driven directly with
// specific generations (the real timers use an hour so they never fire here).
func TestByteProgressWatchdogRejectsStaleCallback(t *testing.T) {
	var interrupts atomic.Int64
	wd := &byteProgressWatchdog{interrupt: func() { interrupts.Add(1) }}

	wd.arm(time.Hour)
	gen1 := wd.gen

	// A callback from before this window must not interrupt.
	wd.fire(gen1 - 1)
	if interrupts.Load() != 0 {
		t.Fatalf("pre-window callback interrupted; interrupts=%d", interrupts.Load())
	}

	// Re-arm: the previous window's callback (gen1) is now superseded.
	wd.arm(time.Hour)
	gen2 := wd.gen
	wd.fire(gen1)
	if interrupts.Load() != 0 {
		t.Fatalf("superseded-window callback interrupted; interrupts=%d", interrupts.Load())
	}

	// The current window's callback fires exactly once (idempotent).
	wd.fire(gen2)
	wd.fire(gen2)
	if got := interrupts.Load(); got != 1 {
		t.Fatalf("current-window interrupts=%d, want exactly 1", got)
	}
	wd.markClosed() // stop the lingering hour timer

	// A stopped window's callback is also inert (stop advances the generation).
	var interrupts2 atomic.Int64
	wd2 := &byteProgressWatchdog{interrupt: func() { interrupts2.Add(1) }}
	wd2.arm(time.Hour)
	stale := wd2.gen
	wd2.stop()
	wd2.fire(stale)
	if interrupts2.Load() != 0 {
		t.Fatalf("stopped-window callback interrupted; interrupts=%d", interrupts2.Load())
	}

	// And a closed watchdog never interrupts, even at the current generation.
	wd3 := &byteProgressWatchdog{interrupt: func() { interrupts2.Add(1) }}
	wd3.arm(time.Hour)
	g := wd3.gen
	wd3.markClosed()
	wd3.fire(g)
	if interrupts2.Load() != 0 {
		t.Fatalf("closed watchdog interrupted; interrupts=%d", interrupts2.Load())
	}
}
