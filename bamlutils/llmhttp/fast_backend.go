package llmhttp

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/valyala/fasthttp"

	"github.com/invakid404/baml-rest/bamlutils/sseclient"
)

// executeFast runs a non-streaming request through fasthttp. The caller has
// already applied URL rewrite and resolved the per-origin HostClient via the
// protocol cache.
func (c *Client) executeFast(ctx context.Context, req *Request, rewrittenURL string, hc *fasthttp.HostClient, onSuccess func()) (*Response, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	deadline, hasDeadline := ctx.Deadline()
	if !hasDeadline {
		deadline = time.Now().Add(DefaultCallTimeout)
	}

	fReq := fasthttp.AcquireRequest()
	fResp := fasthttp.AcquireResponse()
	buildFastRequest(fReq, req, rewrittenURL)

	// Run Do in a goroutine so ctx cancellation returns promptly even though
	// fasthttp.HostClient.DoDeadline is blocking and not ctx-aware. If ctx
	// fires before Do returns, the goroutine keeps running until the
	// deadline and then releases the pooled request/response — there is no
	// mechanism to abort Do mid-flight short of closing the underlying
	// connection (HostClient does not expose one).
	doneCh := make(chan error, 1)
	go func() {
		doneCh <- hc.DoDeadline(fReq, fResp, deadline)
	}()

	select {
	case <-ctx.Done():
		// Hand off cleanup so the pool isn't starved by the orphaned Do.
		go func() {
			<-doneCh
			fasthttp.ReleaseRequest(fReq)
			fasthttp.ReleaseResponse(fResp)
		}()
		return nil, ctx.Err()
	case err := <-doneCh:
		// Must not release until body read completes — with
		// StreamResponseBody=true, fResp owns the body stream until it is
		// fully drained or force-closed. Releasing early would return the
		// response object (and its live body stream reference) to the pool
		// while the reader is still using it, corrupting the next borrower.
		// The deferred release below runs AFTER the body path runs.
		defer fasthttp.ReleaseRequest(fReq)
		defer fasthttp.ReleaseResponse(fResp)
		if err != nil {
			// See awaitCtxIfPastDeadline comment in readFastBodyLimitedCtx:
			// normalise fasthttp's deadline-exceeded error to ctx.Err()
			// when the ctx timer is about to fire, so the caller's
			// ctx-gated cleanup sees a consistent state.
			if ctxErr := awaitCtxIfPastDeadline(ctx); ctxErr != nil {
				return nil, ctxErr
			}
			return nil, fmt.Errorf("llmhttp: request failed: %w", err)
		}

		status := fResp.StatusCode()

		// For non-2xx responses, read a bounded diagnostic body (regardless
		// of StreamResponseBody mode) and surface it as *HTTPError. Matches
		// the net/http path's error-body policy exactly.
		if status < 200 || status >= 300 {
			body := readFastBodyCappedCtx(ctx, fResp, MaxErrorBodyBytes)
			return nil, &HTTPError{StatusCode: status, Body: string(body)}
		}

		if onSuccess != nil {
			onSuccess()
		}

		// StreamResponseBody is on at the client level; for non-streaming
		// responses we still need the full body. Read through BodyStream
		// with the same +1 / truncation check as the net/http path so a
		// provider sending > MaxResponseBodyBytes is reported, not silently
		// truncated. Reading is ctx-aware because Do returned after headers
		// (StreamResponseBody=true) — the caller's deadline therefore must
		// be enforced during the body read, not just the headers read.
		body, err := readFastBodyLimitedCtx(ctx, fResp, MaxResponseBodyBytes)
		if err != nil {
			return nil, err
		}

		return &Response{
			StatusCode: status,
			Headers:    fastHeadersToHTTP(&fResp.Header),
			Body:       string(body),
		}, nil
	}
}

// executeStreamFast runs a streaming request through fasthttp. The returned
// StreamResponse owns the fasthttp request/response until Close() is
// called; callers must always Close even after draining Events.
//
// A per-request fasthttp.HostClient is used instead of the pooled one the
// protocol cache owns. The rationale: ctx cancellation during an
// in-progress body Read has to close the underlying TCP conn to unblock
// the reader (fasthttp's own CloseWithError mutates requestStream state
// concurrently with an in-flight Read, which the race detector flags as a
// real pool-corruption hazard). A per-request HostClient lets us install a
// custom Dial that captures the conn we can close externally, while
// keeping other streams' pooled conns untouched. Streaming requests are
// long-lived and low-rate, so losing per-origin pooling here costs little.
func (c *Client) executeStreamFast(ctx context.Context, req *Request, rewrittenURL string, tmpl *fasthttp.HostClient) (*StreamResponse, error) {
	slot := &captureSlot{}
	hc := newStreamHostClient(ctx, tmpl, slot)

	fReq := fasthttp.AcquireRequest()
	fResp := fasthttp.AcquireResponse()
	buildFastRequest(fReq, req, rewrittenURL)

	// fasthttp's HostClient.Do enforces c.IsTLS == req.URI().isHTTPS(). Our
	// stream HostClient runs with IsTLS=false (TLS is handled inside Dial
	// so trackedConn observes plaintext wire bytes), so the request URI
	// must also declare http for the check to pass. The on-wire request
	// line does not carry the scheme; the only observable effect of this
	// rewrite is that fasthttp is willing to submit the request.
	if tmpl.IsTLS {
		fReq.URI().SetScheme("http")
	}

	// Pre-arm the ctx watcher BEFORE Do so ctx cancel during
	// connect/TLS/first-byte closes the captured conn as soon as it
	// arrives in the slot. Without this, a stalled upstream during the
	// header phase would ignore ctx cancel entirely — a regression vs
	// net/http which respects ctx across the whole round-trip.
	preDoCtx, cancelPreDo := context.WithCancel(ctx)
	defer cancelPreDo()
	go func() {
		<-preDoCtx.Done()
		if ctx.Err() != nil {
			slot.shutdown()
		}
	}()

	// For streaming, we do not set a Do-level deadline: SSE streams can be
	// long-lived and the caller's ctx is the only bound we want. Any client-
	// level ReadTimeout on the HostClient is left at 0 for the same reason.
	doneCh := make(chan error, 1)
	go func() {
		doneCh <- hc.Do(fReq, fResp)
	}()
	var doErr error
	select {
	case <-ctx.Done():
		slot.shutdown()
		<-doneCh
		fasthttp.ReleaseRequest(fReq)
		fasthttp.ReleaseResponse(fResp)
		return nil, ctx.Err()
	case doErr = <-doneCh:
	}
	// Pre-Do watcher has served its purpose — the post-Do reader wires its
	// own ctx watcher on the same slot.
	cancelPreDo()
	if doErr != nil {
		fasthttp.ReleaseRequest(fReq)
		fasthttp.ReleaseResponse(fResp)
		return nil, fmt.Errorf("llmhttp: request failed: %w", doErr)
	}

	// fasthttp defaults a missing response Content-Type to
	// "text/plain; charset=utf-8" when ContentType() is read. That synthetic
	// default would mask the "missing Content-Type" condition — a distinct
	// upstream misbehaviour worth surfacing — behind an "unexpected
	// Content-Type" error. Opt out so ContentType() reflects wire truth.
	// Must be set AFTER Do, because HostClient.Do internally calls
	// resp.Reset() which clears SetNoDefaultContentType back to false.
	fResp.Header.SetNoDefaultContentType(true)

	status := fResp.StatusCode()
	if status < 200 || status >= 300 {
		body := readFastBodyCappedSlot(ctx, fResp, slot, MaxErrorBodyBytes)
		releaseFastExchange(fReq, fResp)
		return nil, &HTTPError{StatusCode: status, Body: string(body)}
	}

	ct := strings.ToLower(string(fResp.Header.ContentType()))
	if !strings.Contains(ct, "text/event-stream") {
		body := readFastBodyCappedSlot(ctx, fResp, slot, MaxErrorBodyBytes)
		releaseFastExchange(fReq, fResp)
		if ct == "" {
			return nil, fmt.Errorf("llmhttp: missing Content-Type header (expected text/event-stream): %s", string(body))
		}
		return nil, fmt.Errorf("llmhttp: unexpected Content-Type %q (expected text/event-stream): %s", ct, string(body))
	}

	bodyStream := fResp.BodyStream()
	if bodyStream == nil {
		// Defensive: without StreamResponseBody on the HostClient the body
		// would be pre-buffered and BodyStream would be nil. That would be
		// a configuration bug in this package.
		releaseFastExchange(fReq, fResp)
		return nil, fmt.Errorf("llmhttp: fasthttp response has no body stream (is StreamResponseBody enabled?)")
	}

	// Snapshot response headers BEFORE handing the body stream to sseclient:
	// fasthttp's requestStream.Read invokes ReadTrailer which mutates the
	// response header, which would race with any downstream read of
	// resp.Header — including fastHeadersToHTTP itself if it ran after the
	// stream parser started.
	headers := fastHeadersToHTTP(&fResp.Header)

	rc := newFastStreamReader(ctx, fReq, fResp, bodyStream, slot)

	events, errc := sseclient.Stream(ctx, rc)

	return &StreamResponse{
		StatusCode: status,
		Headers:    headers,
		Events:     events,
		Errc:       errc,
		body:       rc,
	}, nil
}

// buildFastRequest populates fReq from the llmhttp.Request. Caller supplies
// the rewritten URL so the dispatcher can apply URL rewrite exactly once
// (the net/http path has the same factoring).
func buildFastRequest(fReq *fasthttp.Request, req *Request, rewrittenURL string) {
	fReq.SetRequestURI(rewrittenURL)
	if req.Method != "" {
		fReq.Header.SetMethod(req.Method)
	}
	// fasthttp infers Host from the URI; an explicit Host header in the
	// caller's Headers map wins, matching net/http's http.Request.Host
	// override semantics.
	for k, v := range req.Headers {
		if strings.EqualFold(k, "Host") {
			fReq.Header.SetHost(v)
			continue
		}
		fReq.Header.Set(k, v)
	}
	if req.Body != "" {
		fReq.SetBodyString(req.Body)
	}
}

// fastHeadersToHTTP converts a fasthttp.ResponseHeader into an http.Header so
// the public StreamResponse/Response types expose a stable shape regardless
// of which backend served the request.
func fastHeadersToHTTP(h *fasthttp.ResponseHeader) http.Header {
	out := make(http.Header)
	h.VisitAll(func(key, value []byte) {
		out.Add(string(key), string(value))
	})
	return out
}

// captureSlot owns the net.Conn dialed for a per-request HostClient so the
// dispatcher can close it directly on ctx cancel. Closing at the socket
// layer unblocks any blocked Read without racing against fasthttp's
// internal requestStream state — safe under -race unlike CloseWithError.
//
// The wrapped conn also tracks whether the last bytes observed on the
// wire form a valid HTTP/1.1 chunked terminator, which lets the stream
// reader distinguish a graceful end-of-response from a mid-stream
// disconnect — fasthttp's chunked parser reports both as io.EOF, so
// without this we would silently accept truncated SSE responses as
// successful completions.
//
// closer is updated as the conn stack is built: first the raw TCP conn
// (registered before TLS handshake, so ctx cancel during handshake
// closes the underlying socket promptly), then the tls.Conn after
// HandshakeContext succeeds. Either form, when closed, severs the
// socket and unblocks every layer above it.
type captureSlot struct {
	mu     sync.Mutex
	closer io.Closer
	wrap   *trackedConn
	dead   bool
}

func (s *captureSlot) setCloser(c io.Closer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dead {
		// Shutdown already fired; close whatever was just handed to us.
		_ = c.Close()
		return
	}
	s.closer = c
}

func (s *captureSlot) setWrap(w *trackedConn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dead {
		_ = w.Close()
		return
	}
	s.wrap = w
}

// shutdown closes the captured conn (if any) and marks the slot as dead so
// any subsequent Dial-stored conn is closed on arrival. Idempotent.
func (s *captureSlot) shutdown() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dead {
		return
	}
	s.dead = true
	if s.closer != nil {
		_ = s.closer.Close()
		s.closer = nil
	}
}

// sawCleanEnd reports whether the captured conn observed a proper HTTP/1.1
// chunked end-of-message marker before reaching EOF. Returns false when
// no conn was ever stored (early abort) so callers treat missing-conn as
// "can't assert cleanliness" rather than silently succeeding.
func (s *captureSlot) sawCleanEnd() bool {
	s.mu.Lock()
	w := s.wrap
	s.mu.Unlock()
	if w == nil {
		return false
	}
	return w.sawTerminator()
}

// streamDialTimeout bounds a single TCP+TLS setup for a streaming request
// whose ctx has no deadline of its own. High enough to tolerate a slow
// intercontinental TLS handshake; not so high that a dead route stalls
// the caller for minutes.
const streamDialTimeout = 30 * time.Second

// newStreamHostClient builds a fasthttp.HostClient configured like the
// cache's per-origin template but with a custom, ctx-aware Dial that
//
//   - respects ctx cancellation during DNS/TCP connect (via
//     net.Dialer.DialContext) and during TLS handshake (via
//     tls.Conn.HandshakeContext) — without this, a stalled setup would
//     ignore ctx cancel until fasthttp's own dial timeout fires;
//   - handles TLS itself when the template's IsTLS is true, and passes
//     the TLS-wrapped conn back as a non-TLS result (HostClient.IsTLS is
//     false on the returned client) so fasthttp doesn't double-wrap. We
//     do this so trackedConn can observe PLAINTEXT chunked body bytes
//     — fasthttp's built-in TLS path would leave our tracker looking
//     at ciphertext, blinding the terminator check.
//
// The slot is populated twice: first with the raw TCP conn (pre-
// handshake, so ctx cancel has something to close immediately) and then
// with the tls.Conn after the handshake completes. Either closure
// severs the socket.
func newStreamHostClient(ctx context.Context, tmpl *fasthttp.HostClient, slot *captureSlot) *fasthttp.HostClient {
	return &fasthttp.HostClient{
		Name:                          tmpl.Name,
		Addr:                          tmpl.Addr,
		IsTLS:                         false, // we do TLS ourselves — see doc comment
		DisableHeaderNamesNormalizing: tmpl.DisableHeaderNamesNormalizing,
		StreamResponseBody:            tmpl.StreamResponseBody,
		MaxIdleConnDuration:           tmpl.MaxIdleConnDuration,
		MaxConns:                      tmpl.MaxConns,
		Dial: func(addr string) (net.Conn, error) {
			dialer := &net.Dialer{
				Timeout:   streamDialTimeout,
				KeepAlive: 30 * time.Second,
			}
			rawConn, err := dialer.DialContext(ctx, "tcp", addr)
			if err != nil {
				return nil, err
			}
			// Register raw conn early so ctx cancel during handshake
			// closes the underlying socket even though HandshakeContext
			// wasn't installed yet.
			slot.setCloser(rawConn)

			var outer net.Conn = rawConn
			if tmpl.IsTLS {
				tlsCfg := tmpl.TLSConfig.Clone()
				if tlsCfg.ServerName == "" {
					host, _, splitErr := net.SplitHostPort(addr)
					if splitErr == nil {
						tlsCfg.ServerName = host
					}
				}
				tlsConn := tls.Client(rawConn, tlsCfg)
				if err := tlsConn.HandshakeContext(ctx); err != nil {
					_ = rawConn.Close()
					return nil, err
				}
				// Closing the tls.Conn closes the raw socket underneath;
				// updating the slot's closer is strictly for clarity —
				// the raw closer would also work.
				slot.setCloser(tlsConn)
				outer = tlsConn
			}

			w := &trackedConn{Conn: outer}
			slot.setWrap(w)
			return w, nil
		},
	}
}

// trackedConn wraps a net.Conn to observe the trailing bytes of the
// response stream. Specifically it tracks whether the chunked encoding
// terminator "0\r\n\r\n" was the final five bytes before EOF — the only
// sequence that denotes a graceful end-of-message for an HTTP/1.1
// chunked response. Reads that don't end with that sequence indicate a
// mid-stream disconnect; the stream reader surfaces io.ErrUnexpectedEOF
// instead of a clean EOF so the orchestrator can treat the response as
// incomplete rather than a successful (but truncated) completion.
//
// Limitation: chunk extensions (e.g. "0;ext=val\r\n\r\n") are NOT
// recognised. Real LLM providers don't emit them, so a false negative
// (reporting truncation when the response was actually clean) would
// require an explicit extension on the zero-size chunk — a scenario we
// haven't observed in practice. Under-reporting here is safer than
// over-reporting: a false "clean end" would mask real truncation.
type trackedConn struct {
	net.Conn
	mu     sync.Mutex
	tail   [5]byte
	filled int
	closed atomic.Bool
}

func (t *trackedConn) Read(p []byte) (int, error) {
	n, err := t.Conn.Read(p)
	if n > 0 {
		t.mu.Lock()
		t.recordTail(p[:n])
		t.mu.Unlock()
	}
	return n, err
}

// recordTail keeps the last 5 bytes of the stream. Caller holds t.mu.
func (t *trackedConn) recordTail(buf []byte) {
	if len(buf) >= len(t.tail) {
		copy(t.tail[:], buf[len(buf)-len(t.tail):])
		t.filled = len(t.tail)
		return
	}
	// buf is shorter than tail — shift left, append at end.
	shift := len(buf)
	copy(t.tail[:len(t.tail)-shift], t.tail[shift:])
	copy(t.tail[len(t.tail)-shift:], buf)
	if t.filled+shift > len(t.tail) {
		t.filled = len(t.tail)
	} else {
		t.filled += shift
	}
}

func (t *trackedConn) sawTerminator() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.filled < len(t.tail) {
		return false
	}
	return bytes.Equal(t.tail[:], []byte{'0', '\r', '\n', '\r', '\n'})
}

func (t *trackedConn) Close() error {
	t.closed.Store(true)
	return t.Conn.Close()
}

// readFastBodyCappedSlot reads up to limit bytes from the body, respecting
// ctx cancellation. On ctx fire it closes the captured conn via slot, which
// unblocks the blocked read without racing against fasthttp's internal
// requestStream state (see fastStreamReader doc comment).
func readFastBodyCappedSlot(ctx context.Context, resp *fasthttp.Response, slot *captureSlot, limit int) []byte {
	stream := resp.BodyStream()
	if stream == nil {
		body := resp.Body()
		if len(body) > limit {
			return body[:limit]
		}
		return body
	}

	type result struct {
		buf []byte
	}
	resCh := make(chan result, 1)
	go func() {
		buf, _ := io.ReadAll(io.LimitReader(stream, int64(limit)))
		resCh <- result{buf: buf}
	}()

	select {
	case <-ctx.Done():
		slot.shutdown()
		r := <-resCh
		return r.buf
	case r := <-resCh:
		_ = resp.CloseBodyStream()
		return r.buf
	}
}

// forceCloseWaitWindow bounds how long a ctx-cancelled Execute waits for
// the Read goroutine to exit naturally via the conn's SetReadDeadline
// (installed by DoDeadline) before falling back to fasthttp's
// CloseWithError. At 500 ms the vast majority of cancel-timeouts hit the
// natural path — ctx.Done and SetReadDeadline fire within a scheduling
// tick of each other — while an explicit cancel fired before any deadline
// remains responsive enough for interactive callers. Letting the Read
// exit naturally is strictly preferable because CloseWithError mutates
// fasthttp's requestStream state concurrently with an in-flight Read,
// which the race detector flags as a real pool-corruption hazard.
const forceCloseWaitWindow = 500 * time.Millisecond

// readFastBodyCappedCtx reads up to limit bytes from the response body,
// respecting ctx cancellation. On ctx fire it waits briefly for the Read
// goroutine to exit naturally via the conn's SetReadDeadline (installed by
// DoDeadline) before falling back to force-close — this avoids racing
// with the in-flight Read on fasthttp's requestStream internal state.
//
// Used by Execute. Streaming paths should use readFastBodyCappedSlot,
// which closes the captured conn directly and has no race window.
func readFastBodyCappedCtx(ctx context.Context, resp *fasthttp.Response, limit int) []byte {
	stream := resp.BodyStream()
	if stream == nil {
		body := resp.Body()
		if len(body) > limit {
			return body[:limit]
		}
		return body
	}

	resCh := make(chan []byte, 1)
	go func() {
		buf, _ := io.ReadAll(io.LimitReader(stream, int64(limit)))
		resCh <- buf
	}()

	select {
	case <-ctx.Done():
		select {
		case buf := <-resCh:
			return buf
		case <-time.After(forceCloseWaitWindow):
			forceCloseFastBodyStream(resp, ctx.Err())
			return <-resCh
		}
	case buf := <-resCh:
		_ = resp.CloseBodyStream()
		return buf
	}
}

// readFastBodyLimitedCtx reads the full response body up to maxBytes,
// respecting ctx cancellation. Returns an error when the body exceeds the
// cap — matching Execute's contract that oversized responses are reportable
// rather than silently truncated. On ctx fire the underlying connection is
// force-closed and ctx.Err() is returned, mirroring the net/http path where
// a cancelled body read surfaces the context error.
//
// Used exclusively by Execute: see readFastBodyCappedCtx's doc comment for
// why the CloseWithError race is benign on that code path.
func readFastBodyLimitedCtx(ctx context.Context, resp *fasthttp.Response, maxBytes int) ([]byte, error) {
	stream := resp.BodyStream()
	if stream == nil {
		// Non-streaming path (shouldn't happen with StreamResponseBody on,
		// but keep a correct fallback).
		body := resp.Body()
		if len(body) > maxBytes {
			return nil, fmt.Errorf("llmhttp: response body exceeds maximum size (%d bytes)", maxBytes)
		}
		return append([]byte(nil), body...), nil
	}

	type result struct {
		buf []byte
		err error
	}
	resCh := make(chan result, 1)
	go func() {
		buf, err := io.ReadAll(io.LimitReader(stream, int64(maxBytes)+1))
		resCh <- result{buf: buf, err: err}
	}()

	select {
	case <-ctx.Done():
		// Wait for the Read goroutine to exit naturally via the conn's
		// SetReadDeadline (see forceCloseWaitWindow doc) before falling
		// back to force-close. Read result is discarded — we're
		// returning ctx.Err() either way.
		select {
		case <-resCh:
		case <-time.After(forceCloseWaitWindow):
			forceCloseFastBodyStream(resp, ctx.Err())
			<-resCh
		}
		return nil, ctx.Err()
	case r := <-resCh:
		_ = resp.CloseBodyStream()
		if r.err != nil {
			// fasthttp's per-conn SetDeadline (installed by DoDeadline)
			// can fire a tick ahead of the Go ctx timer. Returning a
			// non-ctx error here would make the orchestrator's trySend
			// race with ctx.Done() — occasionally emitting an error
			// result even though the cancellation path should suppress
			// it. If we're past the deadline, wait briefly for the ctx
			// timer to land and surface ctx.Err() instead.
			if err := awaitCtxIfPastDeadline(ctx); err != nil {
				return nil, err
			}
			return nil, fmt.Errorf("llmhttp: failed to read response body: %w", r.err)
		}
		if int64(len(r.buf)) > int64(maxBytes) {
			return nil, fmt.Errorf("llmhttp: response body exceeds maximum size (%d bytes)", maxBytes)
		}
		return r.buf, nil
	}
}

// forceCloseFastBodyStream triggers a true connection close on a fasthttp
// response body stream via CloseWithError — which internally routes
// through HostClient.CloseConn to sever the TCP socket. This is what
// unblocks a reader parked in a blocking syscall Read.
//
// Safe to call only when the in-flight Read goroutine (if any) has
// already exited requestStream.Read. Execute satisfies that invariant
// because its conn carries a SetReadDeadline from DoDeadline; streaming
// callers do not, so they use captureSlot.shutdown instead.
func forceCloseFastBodyStream(resp *fasthttp.Response, reason error) {
	stream := resp.BodyStream()
	if stream == nil {
		return
	}
	if rcw, ok := stream.(fasthttp.ReadCloserWithError); ok {
		_ = rcw.CloseWithError(reason)
		return
	}
	if cl, ok := stream.(io.Closer); ok {
		_ = cl.Close()
	}
}

// awaitCtxIfPastDeadline returns ctx.Err() when ctx is already done or when
// its deadline has passed. In the second case it blocks briefly until the
// ctx timer fires so callers observe a consistent ctx state — required to
// avoid racy error-result emission on downstream trySend-style dispatchers
// that key suppression off ctx.Done().
func awaitCtxIfPastDeadline(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	deadline, ok := ctx.Deadline()
	if !ok || time.Now().Before(deadline) {
		return nil
	}
	<-ctx.Done()
	return ctx.Err()
}

// releaseFastExchange closes any open body stream (to return the conn to
// the pool) and releases the pooled request/response.
func releaseFastExchange(fReq *fasthttp.Request, fResp *fasthttp.Response) {
	_ = fResp.CloseBodyStream()
	fasthttp.ReleaseRequest(fReq)
	fasthttp.ReleaseResponse(fResp)
}

// fastStreamReader wraps the fasthttp BodyStream so it can be handed to
// sseclient.Stream as an io.ReadCloser. Close is idempotent.
//
// Cancellation strategy: on ctx cancel, the watcher closes the underlying
// net.Conn directly (captured via a custom Dial on the per-request
// HostClient). That is a purely OS-level close — pending Read returns with
// a socket error, sseclient exits, then the consumer's Close() path runs
// fasthttp's normal CloseBodyStream cleanup. Closing at the socket layer
// avoids racing with fasthttp's internal requestStream state mutations,
// which CloseWithError would otherwise trigger concurrently with an
// in-flight Read (a real pool-corruption hazard under -race).
type fastStreamReader struct {
	stream io.Reader
	fReq   *fasthttp.Request
	fResp  *fasthttp.Response
	slot   *captureSlot

	// chunked is true when Transfer-Encoding: chunked was used, i.e. the
	// response has no Content-Length and the "0\r\n\r\n" terminator is the
	// only way to distinguish a clean end from a dropped conn. For
	// Content-Length responses, fasthttp's own byte-count tracking makes
	// io.EOF a reliable clean-end signal, so we skip the tail check.
	chunked bool

	// closed signals the watcher goroutine to exit when Close() is invoked
	// via the consumer path. Distinct from the caller's ctx so a normal
	// Close doesn't trip the force-close branch.
	closed chan struct{}
	once   sync.Once
}

func newFastStreamReader(ctx context.Context, fReq *fasthttp.Request, fResp *fasthttp.Response, stream io.Reader, slot *captureSlot) *fastStreamReader {
	// fasthttp reports ContentLength() == -1 for Transfer-Encoding: chunked.
	// Anything else (positive length, or -2 identity) is a length-delimited
	// body whose EOF from fasthttp is authoritative.
	r := &fastStreamReader{
		stream:  stream,
		fReq:    fReq,
		fResp:   fResp,
		slot:    slot,
		chunked: fResp.Header.ContentLength() == -1,
		closed:  make(chan struct{}),
	}
	// Watcher: on ctx cancel, close the captured conn so the blocked
	// Read() in the SSE parser unblocks. Watcher exits cleanly when the
	// consumer closes normally.
	go func() {
		select {
		case <-ctx.Done():
			slot.shutdown()
		case <-r.closed:
		}
	}()
	return r
}

func (r *fastStreamReader) Read(p []byte) (int, error) {
	n, err := r.stream.Read(p)
	if err == io.EOF && r.chunked && !r.slot.sawCleanEnd() {
		// fasthttp's chunked reader reports io.EOF for both a graceful
		// end-of-message (0-size chunk followed by empty trailers) AND a
		// mid-stream connection drop between chunks (parseChunkSize
		// returning EOF with no hex digits seen). The captured conn's
		// tail tracker distinguishes them: a proper terminator ends with
		// "0\r\n\r\n". Without this translation, truncated SSE responses
		// would surface as clean EOF → sseclient exits without error →
		// orchestrator emits a Final result with partial content, which
		// silently corrupts the caller's view of the stream.
		//
		// Scoped to chunked responses: a Content-Length-delimited body
		// has its own byte-count bound enforced by fasthttp, so EOF
		// there is authoritative.
		return n, io.ErrUnexpectedEOF
	}
	return n, err
}

func (r *fastStreamReader) Close() error {
	r.once.Do(func() {
		close(r.closed)
		// Release fasthttp pool entries after sseclient has exited. The
		// watcher may have closed the socket; CloseBodyStream is still
		// correct here — it releases the bufio.Reader and marks the
		// response body stream nil. Safe to call after socket close; the
		// underlying conn is just already dead.
		_ = r.fResp.CloseBodyStream()
		fasthttp.ReleaseRequest(r.fReq)
		fasthttp.ReleaseResponse(r.fResp)
		// Close the captured conn even on a clean stream end. The
		// per-request HostClient is thrown away after this call, so any
		// conn CloseBodyStream returned to its pool would otherwise sit
		// idle holding an FD until MaxIdleConnDuration expires and the
		// HostClient is GC'd. slot.shutdown is idempotent with the
		// ctx-watcher path.
		r.slot.shutdown()
	})
	return nil
}
