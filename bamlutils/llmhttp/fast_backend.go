package llmhttp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/valyala/fasthttp"
)

// fastUnaryMode selects how executeFast drives a unary request, derived in
// Client.Execute from whether the ORIGINAL caller context could be cancelled
// before DefaultCallTimeout was injected.
type fastUnaryMode int

const (
	// fastUnaryDeadlineOnly: the caller context had no Done channel
	// (context.Background / context.TODO), so the only cancellation source is
	// the deadline Execute injects. DoDeadline can run synchronously — no
	// per-request goroutine — and, when no onSuccess callback is needed, the
	// buffered HostClient reads the whole body before returning so the wrapper
	// just copies fResp.Body() once.
	fastUnaryDeadlineOnly fastUnaryMode = iota
	// fastUnaryCancellable: the caller context was already cancellable
	// (server request contexts, retry/orchestration contexts). DoDeadline runs
	// in a goroutine so a mid-flight cancel returns the caller promptly while
	// the orphaned Do winds down to its deadline and releases the pooled
	// objects (fasthttp exposes no way to abort an in-flight Do).
	fastUnaryCancellable
)

// executeFast runs a non-streaming request through fasthttp. The caller has
// already applied URL rewrite and resolved the per-origin host entry via the
// protocol cache.
//
// Two body-handling lanes:
//   - Buffered fast lane (deadline-only ctx AND no onSuccess): uses the
//     entry's buffered unaryHost (StreamResponseBody=false), so DoDeadline
//     reads the full body into fasthttp's pooled buffer and the wrapper does a
//     single string copy — no intermediate buffer, no body-read goroutine.
//   - Streamed-header lane (onSuccess != nil, or externally cancellable): uses
//     the streaming host so DoDeadline returns after 2xx headers and onSuccess
//     can fire before the body is read. The body is then drained synchronously
//     (no io.ReadAll goroutine) into a pooled buffer with a MaxResponseBodyBytes+1
//     limit.
func (c *Client) executeFast(ctx context.Context, req *Request, rewrittenURL string, entry *hostEntry, onSuccess func(), mode fastUnaryMode) (*Response, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	// Buffered fast lane: nothing to cancel beyond the deadline and no
	// header-time callback to honour, so we can let fasthttp buffer the whole
	// body and read it in one copy.
	if mode == fastUnaryDeadlineOnly && onSuccess == nil && entry.unaryHost != nil {
		return c.executeFastBuffered(ctx, req, rewrittenURL, entry.unaryHost)
	}
	return c.executeFastStreamed(ctx, req, rewrittenURL, entry.host, onSuccess, mode == fastUnaryCancellable)
}

// fastDeadline returns the deadline to pass to DoDeadline: the caller's if set,
// otherwise DefaultCallTimeout from now (Execute always injects one, so the
// fallback only guards direct executeFast callers).
func fastDeadline(ctx context.Context) time.Time {
	if d, ok := ctx.Deadline(); ok {
		return d
	}
	return time.Now().Add(DefaultCallTimeout)
}

// executeFastBuffered runs the buffered fast lane: synchronous DoDeadline
// against the buffered unaryHost, then a single copy of fResp.Body(). The
// host's MaxResponseBodySize enforces the body cap, so an oversized 2xx body
// surfaces as fasthttp.ErrBodyTooLarge, mapped to the same size-limit error
// the net/http and streamed paths return.
func (c *Client) executeFastBuffered(ctx context.Context, req *Request, rewrittenURL string, hc *fasthttp.HostClient) (*Response, error) {
	fReq := fasthttp.AcquireRequest()
	fResp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(fReq)
	defer fasthttp.ReleaseResponse(fResp)
	buildFastRequest(fReq, req, rewrittenURL)

	if err := hc.DoDeadline(fReq, fResp, fastDeadline(ctx)); err != nil {
		if errors.Is(err, fasthttp.ErrBodyTooLarge) {
			// Body exceeded the OOM backstop (maxBufferedResponseBackstop),
			// well above MaxResponseBodyBytes. We can't read status, so report
			// the size error; this only triggers for pathological bodies.
			return nil, fmt.Errorf("llmhttp: response body exceeds maximum size (%d bytes)", MaxResponseBodyBytes)
		}
		if ctxErr := awaitCtxIfPastDeadline(ctx); ctxErr != nil {
			return nil, ctxErr
		}
		if te := classifyTransportErr(err, "llmhttp: request failed", true); te != nil {
			return nil, te
		}
		return nil, fmt.Errorf("llmhttp: request failed: %w", err)
	}

	status := fResp.StatusCode()

	// Non-2xx: surface *HTTPError with the body capped to MaxErrorBodyBytes,
	// matching the net/http and streamed error-body policy. The buffered host's
	// MaxResponseBodySize is only an OOM backstop, so a non-2xx body larger
	// than MaxResponseBodyBytes still reaches here and is capped (preserving
	// the error contract — see B2 / maxBufferedResponseBackstop).
	if status < 200 || status >= 300 {
		body := fResp.Body()
		if len(body) > MaxErrorBodyBytes {
			body = body[:MaxErrorBodyBytes]
		}
		return nil, &HTTPError{StatusCode: status, Body: string(body)}
	}

	// Success: enforce the real MaxResponseBodyBytes limit in code (strictly
	// greater is rejected, exact-at-limit succeeds), matching the net/http and
	// streamed paths. The backstop on the host is higher, so a body between the
	// limit and the backstop is read here and rejected rather than silently
	// truncated.
	body := fResp.Body()
	if len(body) > MaxResponseBodyBytes {
		return nil, fmt.Errorf("llmhttp: response body exceeds maximum size (%d bytes)", MaxResponseBodyBytes)
	}

	// onSuccess is nil in this lane by construction (executeFast gates on it).
	return &Response{
		StatusCode: status,
		Headers:    fastHeadersToHTTP(&fResp.Header),
		Body:       string(body),
	}, nil
}

// executeFastStreamed runs the streamed-header lane against the streaming host
// (StreamResponseBody=true). DoDeadline returns after the 2xx headers so
// onSuccess can fire before the body is read; the body is then drained
// synchronously by drainFastBodyLimited. When cancellable, DoDeadline runs in a
// goroutine so a mid-flight ctx cancel returns the caller promptly.
func (c *Client) executeFastStreamed(ctx context.Context, req *Request, rewrittenURL string, hc *fasthttp.HostClient, onSuccess func(), cancellable bool) (*Response, error) {
	deadline := fastDeadline(ctx)

	fReq := fasthttp.AcquireRequest()
	fResp := fasthttp.AcquireResponse()
	buildFastRequest(fReq, req, rewrittenURL)

	var doErr error
	if cancellable {
		// Run DoDeadline in a goroutine so ctx cancellation returns promptly
		// even though DoDeadline is blocking and not ctx-aware. If ctx fires
		// first, the goroutine keeps running until the deadline and then
		// releases the pooled request/response — there is no mechanism to abort
		// Do mid-flight short of closing the underlying connection (HostClient
		// does not expose one for the shared pool).
		doneCh := make(chan error, 1)
		go func() {
			doneCh <- hc.DoDeadline(fReq, fResp, deadline)
		}()
		select {
		case <-ctx.Done():
			// Hand off cleanup so the pool isn't starved by the orphaned Do.
			// When the orphan Do eventually returns having received headers,
			// fResp holds an unread body stream; discard the connection rather
			// than letting ReleaseResponse pool a conn with unread bytes still
			// on the wire (B1). discard is a no-op when Do failed (no stream).
			go func() {
				<-doneCh
				closeFastConnDiscard(fResp)
				fasthttp.ReleaseRequest(fReq)
				fasthttp.ReleaseResponse(fResp)
			}()
			return nil, ctx.Err()
		case doErr = <-doneCh:
		}
	} else {
		// Deadline-only ctx with an onSuccess callback: nothing to cancel
		// beyond the deadline, so DoDeadline can run synchronously with no
		// per-request goroutine. The deadline still bounds a stalled upstream.
		doErr = hc.DoDeadline(fReq, fResp, deadline)
	}

	// Must not release until the body read completes — with
	// StreamResponseBody=true, fResp owns the body stream until it is fully
	// drained or force-closed. Releasing early would return the response (and
	// its live body stream reference) to the pool while the reader is still
	// using it. These defers run AFTER the body path below.
	defer fasthttp.ReleaseRequest(fReq)
	defer fasthttp.ReleaseResponse(fResp)

	if doErr != nil {
		// Normalise fasthttp's deadline-exceeded error to ctx.Err() when the
		// ctx timer is about to fire, so the caller's ctx-gated cleanup sees a
		// consistent state.
		if ctxErr := awaitCtxIfPastDeadline(ctx); ctxErr != nil {
			return nil, ctxErr
		}
		if te := classifyTransportErr(doErr, "llmhttp: request failed", true); te != nil {
			return nil, te
		}
		return nil, fmt.Errorf("llmhttp: request failed: %w", doErr)
	}

	status := fResp.StatusCode()

	// For non-2xx responses, read a bounded diagnostic body and surface it as
	// *HTTPError. Matches the net/http path's error-body policy exactly.
	//
	// Accepted trade-off: this diagnostic read is synchronous and NOT ctx-aware,
	// so a mid-flight ctx cancel during it can block until the request deadline
	// (DoDeadline / DefaultCallTimeout) on a rare stalled/trickled error body.
	// This is deliberate — restoring ctx-awareness here would require a
	// per-request body-read goroutine + force-close, reintroducing exactly the
	// race / double-close hazards this PR removed (fasthttp exposes no per-read
	// deadline to do it cleanly). The block is bounded by the already-set
	// request deadline.
	if status < 200 || status >= 300 {
		body := readFastErrorBodyCapped(fResp, MaxErrorBodyBytes)
		return nil, &HTTPError{StatusCode: status, Body: string(body)}
	}

	if onSuccess != nil {
		onSuccess()
	}

	body, err := drainFastBodyLimited(ctx, fResp, MaxResponseBodyBytes)
	if err != nil {
		return nil, err
	}

	return &Response{
		StatusCode: status,
		Headers:    fastHeadersToHTTP(&fResp.Header),
		Body:       body,
	}, nil
}

// buildFastRequest populates fReq from the llmhttp.Request. Caller supplies
// the rewritten URL so the dispatcher can apply URL rewrite exactly once
// (the net/http path has the same factoring).
func buildFastRequest(fReq *fasthttp.Request, req *Request, rewrittenURL string) {
	// HostClient.DisableHeaderNamesNormalizing only affects response
	// parsing inside fasthttp; request headers are normalised to
	// canonical case unless we explicitly disable it on the request
	// header struct. Do that before Set() so the caller's exact casing
	// reaches the wire — some upstreams key routing / auth off
	// case-sensitive header names.
	fReq.Header.DisableNormalizing()

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

// readFastErrorBodyCapped reads up to limit bytes of a non-2xx diagnostic body
// synchronously (bounded by the conn read deadline DoDeadline installed). It
// reads limit+1 bytes to detect truncation: if the error body exceeded the cap,
// unread bytes remain on the wire, so the connection MUST be discarded rather
// than pooled (B1) — otherwise the next request would read leftover garbage. A
// fully-read (<= limit) error body leaves the conn clean and poolable.
//
// Synchronous and NOT ctx-aware by design: a mid-flight ctx cancel can block
// here until the request deadline on a rare stalled error body (see the call
// site in executeFastStreamed for the full rationale — adding ctx-awareness
// would reintroduce the per-request goroutine + force-close race removed here).
//
// Used by the unary streamed-header lane (executeFastStreamed).
func readFastErrorBodyCapped(resp *fasthttp.Response, limit int) []byte {
	stream := resp.BodyStream()
	if stream == nil {
		body := resp.Body()
		if len(body) > limit {
			return body[:limit]
		}
		return body
	}

	raw, err := io.ReadAll(io.LimitReader(stream, int64(limit)+1))
	if err != nil || len(raw) > limit {
		// We stopped before EOF — either the body exceeded the diagnostic cap
		// (len > limit) or the read errored/timed out/reset mid-body. Either
		// way unread bytes may remain on the wire, so the conn is dirty and
		// MUST be discarded, not pooled (B1).
		if len(raw) > limit {
			raw = raw[:limit]
		}
		closeFastConnDiscard(resp)
		return raw
	}
	// Clean EOF read within the cap: the conn can return to the pool.
	_ = resp.CloseBodyStream()
	return raw
}

// fastBodyBufPool reuses scratch buffers for draining streamed unary response
// bodies, so the per-request growing intermediate that io.ReadAll allocated is
// replaced by a pooled buffer. Buffers larger than maxPooledFastBody are not
// returned to the pool so an occasional oversized body cannot pin large memory.
var fastBodyBufPool = sync.Pool{New: func() any { return new(bytes.Buffer) }}

// maxPooledFastBody caps the capacity of a buffer kept in fastBodyBufPool.
// 1 MiB comfortably holds typical LLM completions while keeping the pool's
// retained footprint bounded.
const maxPooledFastBody = 1 << 20

// drainFastBodyLimited reads a streamed unary response body up to maxBytes,
// synchronously (no per-request goroutine). It drains BodyStream into a pooled
// scratch buffer with a maxBytes+1 limit so a body exceeding the cap is
// reported — matching Execute's contract that oversized responses are an error,
// not a silent truncation — and matching the net/http path's +1/strictly-greater
// check exactly.
//
// Unlike the previous goroutine-based reader, a mid-body ctx cancel is not
// force-closed here: the read is bounded by the conn deadline DoDeadline
// installed (the same deadline Execute injected), consistent with fasthttp
// having no real mid-flight abort. awaitCtxIfPastDeadline still normalises a
// deadline-race error to ctx.Err() so downstream cancellation suppression stays
// consistent.
func drainFastBodyLimited(ctx context.Context, resp *fasthttp.Response, maxBytes int) (string, error) {
	stream := resp.BodyStream()
	if stream == nil {
		// Body already buffered (defensive — the streaming host should always
		// yield a stream). One copy, with the same strictly-greater cap check.
		body := resp.Body()
		if len(body) > maxBytes {
			return "", fmt.Errorf("llmhttp: response body exceeds maximum size (%d bytes)", maxBytes)
		}
		return string(body), nil
	}

	buf := fastBodyBufPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer func() {
		if buf.Cap() <= maxPooledFastBody {
			fastBodyBufPool.Put(buf)
		}
	}()

	// LimitReader caps at maxBytes+1. If the body is <= maxBytes the underlying
	// stream reaches EOF and the connection is clean → poolable. If we stop at
	// maxBytes+1 (oversize) or hit a read error, unread body bytes remain on the
	// wire, so the connection MUST be discarded — otherwise the next request on
	// that pooled conn would read leftover garbage (B1).
	_, err := buf.ReadFrom(io.LimitReader(stream, int64(maxBytes)+1))
	if err != nil {
		closeFastConnDiscard(resp)
		// fasthttp's per-conn SetDeadline (installed by DoDeadline) can fire a
		// tick ahead of the Go ctx timer. If we're past the deadline, wait
		// briefly for the ctx timer to land and surface ctx.Err() so the
		// orchestrator's trySend cancellation suppression stays consistent.
		if ctxErr := awaitCtxIfPastDeadline(ctx); ctxErr != nil {
			return "", ctxErr
		}
		if te := classifyTransportErr(err, "llmhttp: failed to read response body", false); te != nil {
			return "", te
		}
		return "", fmt.Errorf("llmhttp: failed to read response body: %w", err)
	}
	if buf.Len() > maxBytes {
		// Oversize: we stopped before EOF, so the conn is dirty — discard it.
		closeFastConnDiscard(resp)
		return "", fmt.Errorf("llmhttp: response body exceeds maximum size (%d bytes)", maxBytes)
	}
	// Full body read to EOF: the connection is clean and can return to the pool.
	_ = resp.CloseBodyStream()
	return buf.String(), nil
}

// closeFastConnDiscard closes a streamed fasthttp response body in a way that
// DISCARDS the underlying pooled connection rather than returning it for reuse.
//
// fasthttp's streamed-body close closure (client.go doNonNilReqResp) pools the
// connection unless Connection: close is set on the response or the close
// carries an error. On any path that stops before the body EOF — oversized
// success, a read error/deadline, the capped non-2xx diagnostic, the ctx-cancel
// orphan — unread bytes remain on the wire, so reusing the conn would corrupt
// the next request. Setting Connection: close before CloseBodyStream forces
// that closure down the CloseConn path. CloseBodyStream (not a raw
// CloseWithError) is used so resp.bodyStream is nil'd and a later
// ReleaseResponse → Reset does not double-run the close closure.
func closeFastConnDiscard(resp *fasthttp.Response) {
	resp.SetConnectionClose()
	_ = resp.CloseBodyStream()
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
