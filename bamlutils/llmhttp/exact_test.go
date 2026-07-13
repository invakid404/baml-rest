package llmhttp

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// exactCapture records what a loopback test server observed for one or more
// requests. Guarded by mu because httptest serves each request on its own
// goroutine.
type exactCapture struct {
	mu      sync.Mutex
	count   int
	method  string
	target  string // r.RequestURI — path + raw query, exactly as sent
	host    string // effective virtual host (r.Host)
	body    []byte
	headers http.Header
	cookies []*http.Cookie
}

func (c *exactCapture) record(r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.count++
	c.method = r.Method
	c.target = r.RequestURI
	c.host = r.Host
	c.body = body
	c.headers = r.Header.Clone()
	c.cookies = r.Cookies()
}

func (c *exactCapture) requests() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.count
}

// exactCaptured is a race-safe snapshot of the fields exactCapture.record wrote
// under mu. Assertions run on the test goroutine while record runs on the
// httptest server goroutine, so reading the exactCapture fields directly would
// be a data race the -race lane flags. snapshot copies them under the lock,
// establishing a happens-before, and assertion sites read the returned value.
type exactCaptured struct {
	count   int
	method  string
	target  string
	host    string
	body    []byte
	headers http.Header
	cookies []*http.Cookie
}

func (c *exactCapture) snapshot() exactCaptured {
	c.mu.Lock()
	defer c.mu.Unlock()
	return exactCaptured{
		count:   c.count,
		method:  c.method,
		target:  c.target,
		host:    c.host,
		body:    c.body,
		headers: c.headers,
		cookies: c.cookies,
	}
}

// loopbackGuardedTransport is the general test client transport: it mirrors the
// exact lane's transparency + single-attempt contract (Proxy:nil,
// DisableCompression, DisableKeepAlives) and adds a dial guard rejecting any
// non-loopback destination, matching the Phase 6 credential/egress fence. Tests
// that specifically pin the DEFAULT transport (gzip transparency, no-injection,
// stale-conn replay) use NewExactExecutor(nil) instead, so they exercise
// defaultExactTransport rather than this mirror.
func loopbackGuardedTransport() *http.Transport {
	return &http.Transport{
		Proxy:              nil,
		DisableCompression: true,
		DisableKeepAlives:  true,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, _, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
				return nil, fmt.Errorf("exact test: refusing non-loopback dial to %s", addr)
			}
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
	}
}

func newExactExecutor() *ExactExecutor {
	return NewExactExecutor(loopbackGuardedTransport())
}

// TestExactExecuteCapturesWireDetails pins exact method, request target
// (path+query), effective host, and raw body BYTES over a real loopback socket,
// plus that a 2xx status and raw response body come back as data.
func TestExactExecuteCapturesWireDetails(t *testing.T) {
	cap := &exactCapture{}
	const respBody = `{"ok":true}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.record(r)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(201)
		io.WriteString(w, respBody)
	}))
	defer srv.Close()

	// Body deliberately carries bytes a text pipeline could mangle: a NUL, a
	// lone 0xFF (invalid UTF-8), and a trailing newline.
	rawBody := []byte("{\"x\":\"\x00\xff\"}\n")
	req := &ExactAttemptRequest{
		Method:      "POST",
		URL:         srv.URL + "/v1/chat/completions?model=gpt-4o&n=2",
		Headers:     []HeaderField{{"Content-Type", "application/json"}, {"Host", "api.example.test"}},
		Body:        rawBody,
		BodyPresent: true,
	}

	resp, err := newExactExecutor().Execute(context.Background(), req)
	if err != nil {
		t.Fatalf("Execute: unexpected error: %v", err)
	}
	if resp.StatusCode != 201 {
		t.Errorf("StatusCode = %d, want 201", resp.StatusCode)
	}
	if string(resp.Body) != respBody {
		t.Errorf("Body = %q, want %q", resp.Body, respBody)
	}

	got := cap.snapshot()
	if got.method != "POST" {
		t.Errorf("server method = %q, want POST", got.method)
	}
	if got.target != "/v1/chat/completions?model=gpt-4o&n=2" {
		t.Errorf("server target = %q, want /v1/chat/completions?model=gpt-4o&n=2", got.target)
	}
	if got.host != "api.example.test" {
		t.Errorf("effective host = %q, want api.example.test (Host header must route to Request.Host)", got.host)
	}
	if !bytes.Equal(got.body, rawBody) {
		t.Errorf("server body = %x, want %x (raw bytes must survive)", got.body, rawBody)
	}
}

// TestExactRepeatedHeaderValuesAndOrder proves repeated header values survive
// and per-name value order is retained. Global header-name order/casing is
// explicitly NOT claimed — this test asserts neither.
func TestExactRepeatedHeaderValuesAndOrder(t *testing.T) {
	cap := &exactCapture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.record(r)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	req := &ExactAttemptRequest{
		Method: "POST",
		URL:    srv.URL,
		Headers: []HeaderField{
			{"X-Multi", "first"},
			{"X-Other", "between"},
			{"X-Multi", "second"},
			{"X-Multi", "third"},
		},
		Body:        []byte("{}"),
		BodyPresent: true,
	}
	if _, err := newExactExecutor().Execute(context.Background(), req); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	headers := cap.snapshot().headers
	got := headers.Values("X-Multi")
	want := []string{"first", "second", "third"}
	if len(got) != len(want) {
		t.Fatalf("X-Multi values = %v, want %v (repeated values must survive)", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("X-Multi[%d] = %q, want %q (per-name value order must be retained)", i, got[i], want[i])
		}
	}
	if v := headers.Get("X-Other"); v != "between" {
		t.Errorf("X-Other = %q, want between", v)
	}
}

// TestExactDeclineAmbiguousHost declines a duplicate Host without opening a
// socket.
func TestExactDeclineAmbiguousHost(t *testing.T) {
	cap := &exactCapture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.record(r)
	}))
	defer srv.Close()

	req := &ExactAttemptRequest{
		Method:      "POST",
		URL:         srv.URL,
		Headers:     []HeaderField{{"Host", "a.test"}, {"Host", "b.test"}},
		Body:        []byte("{}"),
		BodyPresent: true,
	}
	_, err := newExactExecutor().Execute(context.Background(), req)
	if !errors.Is(err, ErrExactDeclined) {
		t.Fatalf("err = %v, want ErrExactDeclined", err)
	}
	var de *ExactDeclineError
	if !errors.As(err, &de) || de.Field != "Host" {
		t.Errorf("decline field = %+v, want Host", de)
	}
	if n := cap.requests(); n != 0 {
		t.Errorf("server saw %d requests, want 0 (decline must not open a socket)", n)
	}
}

// TestExactDeclineControlledHeaders declines transport-controlled/hop-by-hop
// header shapes without opening a socket.
func TestExactDeclineControlledHeaders(t *testing.T) {
	cases := []HeaderField{
		{"Content-Length", "5"},
		{"content-length", "5"}, // case-insensitive
		{"Transfer-Encoding", "chunked"},
		{"Connection", "keep-alive"},
		{"Keep-Alive", "timeout=5"},
		{"Proxy-Connection", "keep-alive"},
		{"Trailer", "X-Foo"},
		{"TE", "trailers"},
		{"Upgrade", "websocket"},
		{"Proxy-Authenticate", "Basic"},
		{"Proxy-Authorization", "Basic x"},
	}
	cap := &exactCapture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.record(r)
	}))
	defer srv.Close()

	for _, hf := range cases {
		t.Run(hf.Name, func(t *testing.T) {
			req := &ExactAttemptRequest{
				Method:      "POST",
				URL:         srv.URL,
				Headers:     []HeaderField{hf},
				Body:        []byte("{}"),
				BodyPresent: true,
			}
			_, err := newExactExecutor().Execute(context.Background(), req)
			if !errors.Is(err, ErrExactDeclined) {
				t.Fatalf("%s: err = %v, want ErrExactDeclined", hf.Name, err)
			}
		})
	}
	if n := cap.requests(); n != 0 {
		t.Errorf("server saw %d requests, want 0 (declines must not open a socket)", n)
	}
}

// TestExactSingleHostRouted confirms a single Host header is routed to the
// effective virtual host and is not additionally emitted as a Header entry.
func TestExactSingleHostRouted(t *testing.T) {
	cap := &exactCapture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.record(r)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	req := &ExactAttemptRequest{
		Method:      "POST",
		URL:         srv.URL,
		Headers:     []HeaderField{{"Host", "vhost.test"}},
		Body:        []byte("{}"),
		BodyPresent: true,
	}
	if _, err := newExactExecutor().Execute(context.Background(), req); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	got := cap.snapshot()
	if got.host != "vhost.test" {
		t.Errorf("effective host = %q, want vhost.test", got.host)
	}
	if v := got.headers.Get("Host"); v != "" {
		t.Errorf("Host present in Header map = %q, want empty (routed to Request.Host)", v)
	}
}

// TestExactSingleRoundTripNoRedirect proves a 3xx is returned as data and the
// redirect is NOT followed: exactly one request reaches the server.
func TestExactSingleRoundTripNoRedirect(t *testing.T) {
	cap := &exactCapture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.record(r)
		w.Header().Set("Location", "/redirected")
		w.WriteHeader(302)
		io.WriteString(w, "moved")
	}))
	defer srv.Close()

	req := &ExactAttemptRequest{Method: "POST", URL: srv.URL, Body: []byte("{}"), BodyPresent: true}
	resp, err := newExactExecutor().Execute(context.Background(), req)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if resp.StatusCode != 302 {
		t.Errorf("StatusCode = %d, want 302 (redirect must be returned as data)", resp.StatusCode)
	}
	if string(resp.Body) != "moved" {
		t.Errorf("Body = %q, want moved", resp.Body)
	}
	if n := cap.requests(); n != 1 {
		t.Errorf("server saw %d requests, want exactly 1 (no redirect follow)", n)
	}
}

// TestExactNoCookieJar proves no cookie jar: a Set-Cookie on the first response
// does not cause a Cookie header on a subsequent attempt.
func TestExactNoCookieJar(t *testing.T) {
	cap := &exactCapture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.record(r)
		http.SetCookie(w, &http.Cookie{Name: "sid", Value: "secret"})
		w.WriteHeader(200)
	}))
	defer srv.Close()

	exec := newExactExecutor()
	req := &ExactAttemptRequest{Method: "GET", URL: srv.URL}
	if _, err := exec.Execute(context.Background(), req); err != nil {
		t.Fatalf("first Execute: %v", err)
	}
	if _, err := exec.Execute(context.Background(), req); err != nil {
		t.Fatalf("second Execute: %v", err)
	}
	got := cap.snapshot()
	if len(got.cookies) != 0 {
		t.Errorf("second request carried cookies %v, want none (no jar)", got.cookies)
	}
	if v := got.headers.Get("Cookie"); v != "" {
		t.Errorf("second request Cookie header = %q, want empty", v)
	}
}

// TestExactNonSuccessReturnedAsData proves every non-2xx status is data, not a
// Go error, with the raw body preserved and no HTTPError conversion.
func TestExactNonSuccessReturnedAsData(t *testing.T) {
	for _, status := range []int{400, 401, 429, 500, 503} {
		t.Run(fmt.Sprintf("%d", status), func(t *testing.T) {
			body := fmt.Sprintf(`{"error":{"code":%d}}`, status)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				io.WriteString(w, body)
			}))
			defer srv.Close()

			req := &ExactAttemptRequest{Method: "POST", URL: srv.URL, Body: []byte("{}"), BodyPresent: true}
			resp, err := newExactExecutor().Execute(context.Background(), req)
			if err != nil {
				t.Fatalf("Execute: unexpected error for status %d: %v", status, err)
			}
			if resp.StatusCode != status {
				t.Errorf("StatusCode = %d, want %d", resp.StatusCode, status)
			}
			if string(resp.Body) != body {
				t.Errorf("Body = %q, want %q", resp.Body, body)
			}
		})
	}
}

// bodyOfSize writes n bytes of filler for the given status.
func bodyOfSize(status, n int) http.HandlerFunc {
	chunk := bytes.Repeat([]byte("a"), 32*1024)
	return func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		for n > 0 {
			k := len(chunk)
			if k > n {
				k = n
			}
			w.Write(chunk[:k])
			n -= k
		}
	}
}

// TestExactSuccessBodyCap pins the 16 MiB success cap: exactly-at-cap succeeds,
// one byte over fails the attempt.
func TestExactSuccessBodyCap(t *testing.T) {
	t.Run("at-cap", func(t *testing.T) {
		srv := httptest.NewServer(bodyOfSize(200, MaxResponseBodyBytes))
		defer srv.Close()
		resp, err := newExactExecutor().Execute(context.Background(), &ExactAttemptRequest{Method: "GET", URL: srv.URL})
		if err != nil {
			t.Fatalf("at-cap Execute: %v", err)
		}
		if len(resp.Body) != MaxResponseBodyBytes {
			t.Errorf("body len = %d, want %d", len(resp.Body), MaxResponseBodyBytes)
		}
	})
	t.Run("over-cap", func(t *testing.T) {
		srv := httptest.NewServer(bodyOfSize(200, MaxResponseBodyBytes+1))
		defer srv.Close()
		_, err := newExactExecutor().Execute(context.Background(), &ExactAttemptRequest{Method: "GET", URL: srv.URL})
		if err == nil || !strings.Contains(err.Error(), "exceeds maximum size") {
			t.Fatalf("over-cap err = %v, want 'exceeds maximum size'", err)
		}
	})
}

// TestExactErrorBodyCap pins the 64 KiB non-2xx cap: an over-cap error body is
// truncated to exactly 64 KiB and returned as data; an under-cap body is full.
func TestExactErrorBodyCap(t *testing.T) {
	t.Run("over-cap-truncates", func(t *testing.T) {
		srv := httptest.NewServer(bodyOfSize(500, MaxExactErrorBodyBytes*2))
		defer srv.Close()
		resp, err := newExactExecutor().Execute(context.Background(), &ExactAttemptRequest{Method: "GET", URL: srv.URL})
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if resp.StatusCode != 500 {
			t.Errorf("StatusCode = %d, want 500", resp.StatusCode)
		}
		if len(resp.Body) != MaxExactErrorBodyBytes {
			t.Errorf("body len = %d, want %d (64 KiB pinned)", len(resp.Body), MaxExactErrorBodyBytes)
		}
	})
	t.Run("under-cap-full", func(t *testing.T) {
		srv := httptest.NewServer(bodyOfSize(500, 1000))
		defer srv.Close()
		resp, err := newExactExecutor().Execute(context.Background(), &ExactAttemptRequest{Method: "GET", URL: srv.URL})
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if len(resp.Body) != 1000 {
			t.Errorf("body len = %d, want 1000", len(resp.Body))
		}
	})
}

// TestExactContextCanceledBeforeExecute returns an error and opens no socket
// when the context is already canceled.
func TestExactContextCanceledBeforeExecute(t *testing.T) {
	cap := &exactCapture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.record(r)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := newExactExecutor().Execute(ctx, &ExactAttemptRequest{Method: "GET", URL: srv.URL})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	// A context that is already done makes RoundTrip fail before it dials, so
	// no socket must reach the server. Sleep briefly to let any stray late
	// connection land before asserting zero.
	time.Sleep(50 * time.Millisecond)
	if n := cap.requests(); n != 0 {
		t.Errorf("server saw %d requests, want 0 (pre-canceled context must open no socket)", n)
	}
}

// TestExactTimeoutSingleAttempt proves a deadline exceeded against a slow
// server is a Go error and does NOT trigger a second attempt.
func TestExactTimeoutSingleAttempt(t *testing.T) {
	cap := &exactCapture{}
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.record(r)
		<-release // block until the test lets go
		w.WriteHeader(200)
	}))
	defer srv.Close()
	defer close(release)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := newExactExecutor().Execute(ctx, &ExactAttemptRequest{Method: "GET", URL: srv.URL})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	// Give any (erroneous) retry a chance to land, then assert exactly one hit.
	time.Sleep(50 * time.Millisecond)
	if n := cap.requests(); n != 1 {
		t.Errorf("server saw %d requests, want exactly 1 (no hidden retry)", n)
	}
}

// TestExactReadFailureSingleAttempt drives a truncated response over a real
// socket (Content-Length claims more than is sent, then the peer closes) and
// asserts a classified read error with no second attempt.
func TestExactReadFailureSingleAttempt(t *testing.T) {
	cap := &exactCapture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.record(r)
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Errorf("ResponseWriter is not a Hijacker")
			return
		}
		conn, buf, err := hj.Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		// Promise 1000 bytes, deliver 10, then drop the connection.
		buf.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 1000\r\n\r\n0123456789")
		buf.Flush()
		conn.Close()
	}))
	defer srv.Close()

	_, err := newExactExecutor().Execute(context.Background(), &ExactAttemptRequest{Method: "GET", URL: srv.URL})
	if err == nil {
		t.Fatal("Execute: want read error, got nil")
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("err = %v, want io.ErrUnexpectedEOF", err)
	}
	if n := cap.requests(); n != 1 {
		t.Errorf("server saw %d requests, want exactly 1 (read failure must not retry)", n)
	}
}

// TestExactNoInjectedAuthOrRewrite proves the executor never rewrites the URL
// or injects auth/signing: the server sees only headers we set (plus
// transport-generated framing), and no Authorization we did not supply.
func TestExactNoInjectedAuthOrRewrite(t *testing.T) {
	cap := &exactCapture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.record(r)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	target := "/v1/x?a=1&b=2"
	req := &ExactAttemptRequest{
		Method:      "POST",
		URL:         srv.URL + target,
		Headers:     []HeaderField{{"X-Custom", "v"}},
		Body:        []byte("{}"),
		BodyPresent: true,
	}
	if _, err := newExactExecutor().Execute(context.Background(), req); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	got := cap.snapshot()
	if got.target != target {
		t.Errorf("target = %q, want %q (no URL rewrite)", got.target, target)
	}
	if v := got.headers.Get("Authorization"); v != "" {
		t.Errorf("Authorization = %q, want empty (no signing/auth injection)", v)
	}
	if v := got.headers.Get("X-Custom"); v != "v" {
		t.Errorf("X-Custom = %q, want v", v)
	}
}

// blockingBodyRoundTripper closes ready once its request is fully constructed
// (so the body reader exists but has not been consumed), then blocks on proceed
// before reading the request body into gotBody. This lets a test mutate the
// caller's body slice in the window BETWEEN construction and body consumption —
// the only window where a non-copying executor would be observably wrong.
type blockingBodyRoundTripper struct {
	ready   chan struct{}
	proceed chan struct{}
	gotBody []byte
}

func (rt *blockingBodyRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	close(rt.ready) // request built; body not yet read
	<-rt.proceed    // wait until the test has mutated the caller's slice
	b, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	rt.gotBody = b
	return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(""))}, nil
}

// TestExactBodyOwnership proves the executor copies the caller's body at the
// ownership boundary, discriminatingly: the caller's slice is mutated in the
// window AFTER request construction but BEFORE the transport consumes the body,
// and the transport must still read the ORIGINAL bytes. A non-copying executor
// (bytes.NewReader over the caller's slice) would read the mutated bytes here
// and fail — unlike a post-Execute mutation, which every implementation
// survives.
func TestExactBodyOwnership(t *testing.T) {
	original := []byte("original-body-bytes")
	body := make([]byte, len(original))
	copy(body, original)

	rt := &blockingBodyRoundTripper{ready: make(chan struct{}), proceed: make(chan struct{})}
	exec := NewExactExecutor(rt)

	done := make(chan error, 1)
	go func() {
		_, err := exec.Execute(context.Background(),
			&ExactAttemptRequest{Method: "POST", URL: "http://x.test", Body: body, BodyPresent: true})
		done <- err
	}()

	<-rt.ready // request constructed; body reader exists but is unread
	for i := range body {
		body[i] = 'X' // mutate the caller slice in the pre-consumption window
	}
	close(rt.proceed) // let the transport read the (copied) body now

	if err := <-done; err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !bytes.Equal(rt.gotBody, original) {
		t.Errorf("transport read %q, want original %q (executor must copy the body at construction, before the transport reads it)", rt.gotBody, original)
	}
}

// closeTrackingBody wraps a body and records that Close was called.
type closeTrackingBody struct {
	io.Reader
	closed *bool
}

func (b *closeTrackingBody) Close() error { *b.closed = true; return nil }

// fakeRoundTripper returns a canned response and counts RoundTrip calls, so
// tests can assert body-close-on-every-path and single-attempt independent of a
// live socket.
type fakeRoundTripper struct {
	count int
	resp  *http.Response
	err   error
}

func (f *fakeRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	f.count++
	return f.resp, f.err
}

// errReader fails every Read with err — drives the response-read-failure path.
type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }

// sizedReader yields n bytes of filler then EOF without pre-allocating them —
// drives the over-cap success path.
type sizedReader struct{ remaining int }

func (r *sizedReader) Read(p []byte) (int, error) {
	if r.remaining <= 0 {
		return 0, io.EOF
	}
	n := len(p)
	if n > r.remaining {
		n = r.remaining
	}
	r.remaining -= n
	return n, nil
}

// TestExactClosesBodyOnEveryPath asserts the response body is closed AND exactly
// one RoundTrip is made on every response-body path: a normal 2xx read, a normal
// non-2xx read, a mid-read failure, and an over-cap 2xx that fails the attempt.
// The last two exercise the error returns from Execute's body-read section,
// where a missed defer would leak the connection.
func TestExactClosesBodyOnEveryPath(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    func(closed *bool) io.ReadCloser
		wantErr bool
	}{
		{"2xx-read", 200, func(c *bool) io.ReadCloser {
			return &closeTrackingBody{Reader: strings.NewReader("hi"), closed: c}
		}, false},
		{"non-2xx-read", 500, func(c *bool) io.ReadCloser {
			return &closeTrackingBody{Reader: strings.NewReader("nope"), closed: c}
		}, false},
		{"read-failure", 200, func(c *bool) io.ReadCloser {
			return &closeTrackingBody{Reader: errReader{err: io.ErrUnexpectedEOF}, closed: c}
		}, true},
		{"over-cap-success", 200, func(c *bool) io.ReadCloser {
			return &closeTrackingBody{Reader: &sizedReader{remaining: MaxResponseBodyBytes + 1}, closed: c}
		}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			closed := false
			rt := &fakeRoundTripper{resp: &http.Response{
				StatusCode: tc.status,
				Header:     http.Header{},
				Body:       tc.body(&closed),
			}}
			exec := NewExactExecutor(rt)
			_, err := exec.Execute(context.Background(), &ExactAttemptRequest{Method: "GET", URL: "http://x.test"})
			if tc.wantErr && err == nil {
				t.Error("Execute: want error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("Execute: unexpected error: %v", err)
			}
			if !closed {
				t.Error("response body was not closed")
			}
			if rt.count != 1 {
				t.Errorf("RoundTrip called %d times, want 1", rt.count)
			}
		})
	}
}

// TestExactBodyPresentDistinction proves the carrier distinguishes an absent
// body (nil http.Request body) from a present zero-length body (non-nil, len 0)
// — the collapse the legacy `Body != ""` carrier cannot avoid.
func TestExactBodyPresentDistinction(t *testing.T) {
	t.Run("absent", func(t *testing.T) {
		httpReq, err := buildExactRequest(context.Background(), &ExactAttemptRequest{Method: "GET", URL: "http://x.test"})
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		if httpReq.Body != nil {
			t.Errorf("absent body: httpReq.Body = %v, want nil", httpReq.Body)
		}
	})
	t.Run("present-empty", func(t *testing.T) {
		httpReq, err := buildExactRequest(context.Background(), &ExactAttemptRequest{Method: "POST", URL: "http://x.test", Body: []byte{}, BodyPresent: true})
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		if httpReq.Body == nil {
			t.Error("present zero-length body: httpReq.Body = nil, want non-nil")
		}
		if httpReq.ContentLength != 0 {
			t.Errorf("present zero-length body: ContentLength = %d, want 0", httpReq.ContentLength)
		}
	})
}

// TestExactRedaction proves diagnostics never leak secrets: RedactedURL drops
// userinfo and query, and RedactedSummary shows names/allowlisted values only.
func TestExactRedaction(t *testing.T) {
	t.Run("url", func(t *testing.T) {
		got := RedactedURL("https://user:pass@api.openai.com/v1/chat?api_key=sk-secret&x=1")
		if strings.Contains(got, "pass") || strings.Contains(got, "sk-secret") || strings.Contains(got, "api_key") {
			t.Errorf("RedactedURL leaked secret material: %q", got)
		}
		if !strings.Contains(got, "api.openai.com/v1/chat") {
			t.Errorf("RedactedURL = %q, want to retain scheme+host+path", got)
		}
		if !strings.HasSuffix(got, "?<redacted>") {
			t.Errorf("RedactedURL = %q, want a ?<redacted> query marker", got)
		}
	})
	t.Run("summary", func(t *testing.T) {
		req := &ExactAttemptRequest{
			Method: "POST",
			URL:    "https://api.openai.com/v1/chat?api_key=sk-secret",
			Headers: []HeaderField{
				{"Authorization", "Bearer sk-super-secret"},
				{"Content-Type", "application/json"},
				{"anthropic-version", "2023-06-01"},
				{"X-Api-Key", "raw-key-value"},
			},
			Body:        []byte(`{"model":"gpt-4o"}`),
			BodyPresent: true,
		}
		got := req.RedactedSummary("exact-42")
		for _, leak := range []string{"sk-super-secret", "sk-secret", "raw-key-value", `{"model"`} {
			if strings.Contains(got, leak) {
				t.Errorf("RedactedSummary leaked %q in %q", leak, got)
			}
		}
		// Header names and allowlisted values must be present.
		for _, want := range []string{"Authorization", "Content-Type=application/json", "anthropic-version=2023-06-01", "X-Api-Key", "method=POST", "id=exact-42"} {
			if !strings.Contains(got, want) {
				t.Errorf("RedactedSummary = %q, want it to contain %q", got, want)
			}
		}
		// Body length + hash prefix, never bytes.
		if !strings.Contains(got, "body=18B sha256:") {
			t.Errorf("RedactedSummary = %q, want body length + hash", got)
		}
	})
	t.Run("absent-body", func(t *testing.T) {
		got := (&ExactAttemptRequest{Method: "GET", URL: "http://x.test"}).RedactedSummary("exact-1")
		if !strings.Contains(got, "body=absent") {
			t.Errorf("RedactedSummary = %q, want body=absent", got)
		}
	})
}

// TestExactDeclineErrorFormatNoValueLeak makes sure a decline error never
// embeds a header value.
func TestExactDeclineErrorFormatNoValueLeak(t *testing.T) {
	req := &ExactAttemptRequest{
		Method:  "POST",
		URL:     "http://x.test",
		Headers: []HeaderField{{"Content-Length", "1234567-secretish"}},
	}
	_, err := NewExactExecutor(&fakeRoundTripper{}).Execute(context.Background(), req)
	if !errors.Is(err, ErrExactDeclined) {
		t.Fatalf("err = %v, want ErrExactDeclined", err)
	}
	if strings.Contains(err.Error(), "1234567-secretish") {
		t.Errorf("decline error leaked a header value: %v", err)
	}
}

// TestExactNilGuards covers the nil executor / nil request guards.
func TestExactNilGuards(t *testing.T) {
	var nilExec *ExactExecutor
	if _, err := nilExec.Execute(context.Background(), &ExactAttemptRequest{}); err == nil {
		t.Error("nil executor: want error")
	}
	if _, err := NewExactExecutor(&fakeRoundTripper{}).Execute(context.Background(), nil); err == nil {
		t.Error("nil request: want error")
	}
}

// TestExactDefaultTransportContract pins the transparency + single-attempt
// configuration of the default exact-lane transport directly: Proxy:nil (no
// env-proxy Proxy-Authorization injection), DisableCompression (no Accept-
// Encoding injection / transparent gunzip), DisableKeepAlives (no reused-
// connection replay). These are the config-level guarantees the behavioural
// tests below exercise over a socket.
func TestExactDefaultTransportContract(t *testing.T) {
	if defaultExactTransport.Proxy != nil {
		t.Error("defaultExactTransport.Proxy must be nil (no env-proxy auth injection)")
	}
	if !defaultExactTransport.DisableCompression {
		t.Error("defaultExactTransport.DisableCompression must be true (no Accept-Encoding injection / transparent decode)")
	}
	if !defaultExactTransport.DisableKeepAlives {
		t.Error("defaultExactTransport.DisableKeepAlives must be true (no stale-conn replay)")
	}
}

// gzipBytes returns s gzip-compressed.
func gzipBytes(t *testing.T, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(s)); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

// TestExactTransparentGzipRaw proves the DEFAULT exact lane (NewExactExecutor(nil)
// => defaultExactTransport) neither injects an Accept-Encoding request header nor
// transparently decodes a gzip response: on BOTH 2xx and non-2xx, the raw
// provider bytes are returned verbatim and Content-Encoding is preserved. This
// is the P1 fix regression — a stock pooled transport would inject gzip and hand
// back a decoded body with Content-Encoding stripped.
func TestExactTransparentGzipRaw(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
	}{{"2xx", 200}, {"non-2xx", 500}} {
		t.Run(tc.name, func(t *testing.T) {
			raw := gzipBytes(t, `{"content":"hello gzip world"}`)
			// Capture the request headers through the lock-held exactCapture, so
			// the handler-goroutine write is published to the test goroutine over
			// a real synchronization edge (record's Unlock -> snapshot's Lock).
			// Completing the HTTP exchange is NOT such an edge for a bare local.
			cap := &exactCapture{}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				cap.record(r)
				w.Header().Set("Content-Encoding", "gzip")
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				w.Write(raw)
			}))
			defer srv.Close()

			resp, err := NewExactExecutor(nil).Execute(context.Background(),
				&ExactAttemptRequest{Method: "POST", URL: srv.URL, Body: []byte("{}"), BodyPresent: true})
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if resp.StatusCode != tc.status {
				t.Errorf("StatusCode = %d, want %d", resp.StatusCode, tc.status)
			}
			reqHeaders := cap.snapshot().headers
			if _, seen := reqHeaders["Accept-Encoding"]; seen {
				t.Errorf("server saw Accept-Encoding=%q, want none (must NOT be injected)", reqHeaders.Get("Accept-Encoding"))
			}
			if ce := resp.Headers.Get("Content-Encoding"); ce != "gzip" {
				t.Errorf("Content-Encoding = %q, want gzip (must be preserved)", ce)
			}
			if !bytes.Equal(resp.Body, raw) {
				t.Errorf("Body len = %d, want raw gzip len %d (response was transparently decoded)", len(resp.Body), len(raw))
			}
		})
	}
}

// TestExactForwardsPlannedAcceptEncoding proves a caller-supplied Accept-Encoding
// is forwarded verbatim and the response is still returned raw (net/http only
// auto-decodes a gzip it implicitly requested).
func TestExactForwardsPlannedAcceptEncoding(t *testing.T) {
	raw := gzipBytes(t, `{"x":1}`)
	// Publish the handler-observed request header through the lock-held
	// exactCapture rather than a bare local, so the read is race-clean.
	cap := &exactCapture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.record(r)
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(200)
		w.Write(raw)
	}))
	defer srv.Close()

	resp, err := NewExactExecutor(nil).Execute(context.Background(), &ExactAttemptRequest{
		Method:  "GET",
		URL:     srv.URL,
		Headers: []HeaderField{{"Accept-Encoding", "gzip"}},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := cap.snapshot().headers.Get("Accept-Encoding"); got != "gzip" {
		t.Errorf("server Accept-Encoding = %q, want gzip (planned value must be forwarded)", got)
	}
	if !bytes.Equal(resp.Body, raw) {
		t.Errorf("Body was decoded; want raw gzip bytes")
	}
}

// TestExactSingleAttemptNoConnReuseReplay is the P1 single-physical-attempt
// regression. The default exact transport disables keep-alives, so net/http
// never reuses a pooled connection and therefore never replays a replayable
// (here, GET) request onto a stale one. Even when the server drops the second
// request's connection mid-exchange, the server sees EXACTLY one hit per Execute
// (2 total, never 3) and each request lands on its own connection.
//
// With a stock pooled transport the second Execute would reuse the pooled
// connection, hit transportReadFromServerError on the drop, and — GET being
// replayable — replay onto a fresh connection, producing a THIRD server hit.
func TestExactSingleAttemptNoConnReuseReplay(t *testing.T) {
	var mu sync.Mutex
	count := 0
	conns := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		count++
		conns[r.RemoteAddr]++
		n := count
		mu.Unlock()
		if n == 2 {
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Errorf("ResponseWriter is not a Hijacker")
				return
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			conn.Close() // drop without a valid response
			return
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	exec := NewExactExecutor(nil)
	req := &ExactAttemptRequest{Method: "GET", URL: srv.URL}
	if _, err := exec.Execute(context.Background(), req); err != nil {
		t.Fatalf("first Execute: %v", err)
	}
	if _, err := exec.Execute(context.Background(), req); err == nil {
		t.Fatal("second Execute: want error from dropped connection, got nil")
	}
	// Let any erroneous replay reach the server before asserting.
	time.Sleep(75 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if count != 2 {
		t.Errorf("server saw %d requests, want exactly 2 (no stale-conn replay)", count)
	}
	for addr, c := range conns {
		if c != 1 {
			t.Errorf("connection %s served %d requests, want 1 (keep-alives disabled, no reuse)", addr, c)
		}
	}
}
