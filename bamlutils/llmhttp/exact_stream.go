package llmhttp

// Exact-attempt STREAMING transport primitive (de-BAML Phase 7A).
//
// This file adds the neutral, one-shot streaming counterpart to exact.go's
// unary ExactExecutor: an *http.Client whose RoundTripper sends EXACTLY one
// admitted request and streams the response body back unbuffered, with a
// first-body deadline and an inter-byte idle deadline that are independent of
// the caller context.
//
// It exists so a later (out-of-root-module) slice can hand it to nanollm's
// DoStream: DoStream re-prepares the provider request and drives the SSE
// decoder over whatever http.Client it is given. Passing it THIS client means
// the request DoStream actually produces is compared, at RoundTrip, against the
// already-admitted plan before any socket opens — so a nanollm re-prepare drift
// or a retry/fallback regression becomes a ZERO-SOCKET mismatch or refused
// second call, never a duplicate physical request (scope §5.7, invariants
// I3/I4).
//
// Deliberate non-goals, so the "exact" claim stays honest:
//   - NO redirect following (CheckRedirect returns http.ErrUseLastResponse),
//     NO cookie jar (Jar is nil), NO URL rewrite, NO request signing, and NO
//     hidden same-request retry. The one-shot RoundTripper gate makes a second
//     RoundTrip — however net/http, a redirect, or a nanollm regression might
//     induce it — a zero-socket failure by construction.
//   - The transport disables proxy, compression, keep-alive replay, and HTTP/2
//     exactly as the unary exact lane does (shared newHardenedExactTransport),
//     plus an explicit empty TLSNextProto to pin HTTP/1.1.
//   - This code is UNREACHABLE from production routing in Phase 7A: nothing in
//     the generated adapter, handlers, orchestrator, or codegen constructs it,
//     and it imports NO nanollm.
//
// The transport claims (fires OnClaim) immediately before the single underlying
// RoundTrip and only after the plan comparison passes — that is the scope's I4
// point of no return. A mismatch, a refused second call, a pre-claim context
// cancellation, or a body-read failure all return before the claim with no
// socket opened.

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
)

// ExactStreamClientConfig configures a one-shot exact streaming *http.Client.
//
// Expected is the admitted plan the outgoing request is compared against.
// FirstBodyTimeout and IdleTimeout are the two independent byte-progress
// deadlines (scope §5.10). The three callbacks are liveness/lifecycle signals
// fired in a fixed order — OnClaim (immediately before the one RoundTrip) →
// OnResponseHeaders (on a valid 2xx SSE header line) → OnFirstBody (on the
// first raw upstream body byte) — and any of them may be nil.
type ExactStreamClientConfig struct {
	// Expected is the admitted exact request plan. The RoundTripper compares the
	// request it is actually asked to send against this plan (method, URL,
	// effective host, ordered header values, and raw body bytes); any drift is a
	// zero-socket ExactStreamPlanMismatchError. Required.
	Expected *ExactAttemptRequest

	// FirstBodyTimeout bounds the gap from a successful response header line to
	// the first raw upstream body byte. <= 0 means no first-body bound. It is
	// deliberately distinct from IdleTimeout (scope §3.2 / §5.10).
	FirstBodyTimeout time.Duration

	// IdleTimeout bounds the inter-byte gap AFTER the first body byte. It is
	// reset on every subsequent raw byte (including SSE comment/keepalive bytes
	// and partial lines). <= 0 means no idle bound.
	IdleTimeout time.Duration

	// OnClaim, if non-nil, fires exactly once immediately before the single
	// underlying RoundTrip — the transport-claim point of no return (I4). It does
	// not fire on any pre-claim failure (mismatch, refused second call, pre-claim
	// cancellation, body-read failure).
	OnClaim func()

	// OnResponseHeaders, if non-nil, fires exactly once when a valid 2xx SSE
	// response header line is observed, BEFORE any body byte. It is an idempotent
	// liveness signal only; it never fires on a non-2xx (returned as data) or on
	// an invalid content type. It always precedes OnFirstBody.
	OnResponseHeaders func()

	// OnFirstBody, if non-nil, fires exactly once on the first raw upstream body
	// byte (including an SSE comment byte). Liveness/metric signal only.
	OnFirstBody func()

	// transport is an optional injected underlying RoundTripper. nil selects a
	// fresh hardened HTTP/1.1 transport (newHardenedExactStreamTransport). It is
	// unexported so external callers always get the hardened default; the
	// in-package test suite injects a loopback/counting transport through it.
	transport http.RoundTripper
}

// NewExactStreamClient builds a one-shot exact streaming *http.Client from cfg.
// The returned client follows no redirects (CheckRedirect returns
// http.ErrUseLastResponse), keeps no cookies (nil Jar), and enforces no total
// Client.Timeout — the total bound is the caller's request context, while the
// first-body and inter-byte idle bounds are enforced on the streamed body. A
// caller uses it exactly once: the underlying RoundTripper refuses a second
// RoundTrip without touching the network.
func NewExactStreamClient(cfg ExactStreamClientConfig) (*http.Client, error) {
	if cfg.Expected == nil {
		return nil, fmt.Errorf("llmhttp: exact stream client requires a non-nil Expected plan")
	}
	if _, err := url.Parse(cfg.Expected.URL); err != nil {
		return nil, fmt.Errorf("llmhttp: exact stream client Expected.URL is not parseable: %w", err)
	}

	underlying := cfg.transport
	if underlying == nil {
		underlying = newHardenedExactStreamTransport()
	}

	// Snapshot the admitted plan (struct + both slices) so a later caller
	// mutation of Expected, its Headers, or its Body can neither change the
	// comparison target nor race with RoundTrip — the plan is admitted once and
	// frozen here.
	expected := *cfg.Expected
	expected.Headers = append([]HeaderField(nil), cfg.Expected.Headers...)
	expected.Body = append([]byte(nil), cfg.Expected.Body...)

	rt := &exactStreamRoundTripper{
		expected:          &expected,
		underlying:        underlying,
		firstBodyTimeout:  cfg.FirstBodyTimeout,
		idleTimeout:       cfg.IdleTimeout,
		onClaim:           cfg.OnClaim,
		onResponseHeaders: cfg.OnResponseHeaders,
		onFirstBody:       cfg.OnFirstBody,
	}
	return &http.Client{
		Transport: rt,
		// Never follow a redirect: return the 3xx response as-is. Combined with
		// the one-shot gate this makes a redirect a single-request, single-socket
		// event whose follow-up would be refused anyway.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Jar: nil,
		// Total-time bound is the caller context; the transport owns the
		// first-body/idle bounds. A Client.Timeout would wrap the body in its own
		// cancel and blur those two deadlines together.
		Timeout: 0,
	}, nil
}

// newHardenedExactStreamTransport returns the exact stream lane's underlying
// transport: the shared hardened unary transport (no proxy / no compression /
// no keep-alive replay) plus an explicit empty TLSNextProto so HTTP/2 is never
// negotiated. A stock net/http.Transport enables HTTP/2 automatically only when
// TLSClientConfig is nil or ForceAttemptHTTP2 is set — neither holds here — but
// pinning TLSNextProto makes the HTTP/1.1-only guarantee explicit and immune to
// a future default change, so the one-physical-request property never depends
// on HTTP/2's separate stream-retry semantics.
func newHardenedExactStreamTransport() *http.Transport {
	t := newHardenedExactTransport()
	t.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	return t
}

// exactStreamRoundTripper is the one-shot exact stream RoundTripper. Its RoundTrip
// is entered at most once (used gate); it compares the outgoing request against
// the admitted plan, claims immediately before the single underlying RoundTrip,
// returns non-2xx responses verbatim for the caller's bounded error translator,
// and wraps a valid 2xx SSE body with the first-body and idle deadlines.
type exactStreamRoundTripper struct {
	expected   *ExactAttemptRequest
	underlying http.RoundTripper

	firstBodyTimeout time.Duration
	idleTimeout      time.Duration

	onClaim           func()
	onResponseHeaders func()
	onFirstBody       func()

	// used is the atomic one-shot gate. It flips true on the FIRST entry
	// (including a mismatch or pre-claim decline), so any later entry is a
	// refused, zero-socket second call.
	used atomic.Bool
}

func (rt *exactStreamRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	// 1. One-shot gate: a second entry never touches the network. The
	//    RoundTripper contract still requires closing the outgoing body.
	if !rt.used.CompareAndSwap(false, true) {
		closeRequestBody(req)
		return nil, ErrExactStreamSecondRoundTrip
	}

	// 2. Read the outgoing body within the bounded admitted size and restore a
	//    fresh, self-owned copy so the single send re-emits the exact bytes.
	//    actualPresent distinguishes an absent body from a present zero-length one.
	actualBody, actualPresent, err := readAndRestoreExactBody(req, rt.expected)
	if err != nil {
		return nil, err
	}

	// 3. Compare the request DoStream (or a test) actually produced against the
	//    admitted plan. Any drift is a zero-socket mismatch.
	if mm := compareExactStreamPlan(req, actualBody, actualPresent, rt.expected); mm != nil {
		closeRequestBody(req)
		return nil, mm
	}

	// 4. Honour an already-cancelled/expired caller context BEFORE the claim, so a
	//    pre-cancelled request declines with zero socket and no claim callback.
	if ctxErr := req.Context().Err(); ctxErr != nil {
		closeRequestBody(req)
		return nil, ctxErr
	}

	// 5. Transport claim — the point of no return (I4) — immediately before the
	//    one underlying RoundTrip.
	if rt.onClaim != nil {
		rt.onClaim()
	}

	// 6. The single physical request.
	resp, err := rt.underlying.RoundTrip(req)
	if err != nil {
		return nil, err
	}

	// 7. Non-2xx: return status + body VERBATIM for the caller's bounded error
	//    translator (e.g. nanollm's ProviderStatusError path). Never consumed or
	//    retried here; the response-header/first-body callbacks do not fire.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp, nil
	}

	// 8. 2xx: validate text/event-stream WITHOUT buffering the body.
	if err := validateSSEContentType(resp); err != nil {
		_ = resp.Body.Close()
		return nil, err
	}

	// 9. Wrap the body FIRST — constructing the reader arms the first-body
	//    deadline, so the response-header-to-first-byte window starts the instant
	//    valid 2xx SSE headers are observed and cannot be extended by a slow or
	//    blocked OnResponseHeaders callback — then fire the response-header
	//    liveness signal (2xx only). Wrapping does not fire OnFirstBody (that
	//    fires on the first body Read), so the callback order stays
	//    claim -> headers -> first body.
	resp.Body = newExactStreamBody(req.Context(), resp.Body, rt.onFirstBody, rt.firstBodyTimeout, rt.idleTimeout)
	if rt.onResponseHeaders != nil {
		rt.onResponseHeaders()
	}
	return resp, nil
}

// closeRequestBody closes the outgoing request body if present, satisfying the
// RoundTripper contract on the early-return (pre-send) paths. NopCloser.Close is
// a no-op, so closing an already-restored body is harmless.
func closeRequestBody(req *http.Request) {
	if req != nil && req.Body != nil {
		_ = req.Body.Close()
	}
}

// readAndRestoreExactBody reads the outgoing request body (bounded at the
// admitted size + 1, so an over-size body is detectable as a mismatch rather
// than silently truncated-and-compared), closes the original, and restores a
// fresh self-owned body with a pinned ContentLength/GetBody so the single
// underlying RoundTrip re-sends a known-length body. It returns the bytes read
// for the plan comparison and whether a body was PRESENT (so a present
// zero-length body is distinguishable from an absent one). An absent body
// (req.Body == nil) returns (nil, false, nil); a present empty body — which
// http.NewRequest collapses to http.NoBody — returns (nil, true, nil).
func readAndRestoreExactBody(req *http.Request, expected *ExactAttemptRequest) ([]byte, bool, error) {
	if req.Body == nil {
		return nil, false, nil
	}
	if req.Body == http.NoBody {
		// Present but zero-length: nothing to read or restore; NoBody already
		// transmits as an empty body.
		return nil, true, nil
	}
	limit := int64(len(expected.Body)) + 1
	buf, err := io.ReadAll(io.LimitReader(req.Body, limit))
	_ = req.Body.Close()
	if err != nil {
		return nil, true, &ExactStreamBodyReadError{Cause: err}
	}
	req.Body = io.NopCloser(bytes.NewReader(buf))
	req.ContentLength = int64(len(buf))
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(buf)), nil
	}
	return buf, true, nil
}

// compareExactStreamPlan compares the actual outgoing request against the
// admitted plan across method, URL, effective host, headers, and body,
// returning a secret-free *ExactStreamPlanMismatchError on the first drift (or
// nil on a full match). actualPresent reports whether the actual request
// carried a body, so a present zero-length body never matches an absent one.
// Only field kinds and header NAMES ever appear in the diagnostic — never a URL
// query, header value, or body byte.
func compareExactStreamPlan(req *http.Request, actualBody []byte, actualPresent bool, expected *ExactAttemptRequest) *ExactStreamPlanMismatchError {
	if req.Method != expected.Method {
		return &ExactStreamPlanMismatchError{Field: "method"}
	}

	expURL, err := url.Parse(expected.URL)
	if err != nil || !sameRequestURL(req.URL, expURL) {
		return &ExactStreamPlanMismatchError{Field: "url"}
	}

	if !strings.EqualFold(requestEffectiveHost(req), planEffectiveHost(expected, expURL)) {
		return &ExactStreamPlanMismatchError{Field: "host"}
	}

	if field := compareExactStreamHeaders(req.Header, expected.Headers); field != "" {
		return &ExactStreamPlanMismatchError{Field: field}
	}

	// Body PRESENCE first: bytes.Equal collapses nil and empty, so a present
	// zero-length body would otherwise match an absent plan (and vice versa).
	if actualPresent != expected.BodyPresent {
		return &ExactStreamPlanMismatchError{Field: "body"}
	}
	if actualPresent && !bytes.Equal(actualBody, expected.Body) {
		return &ExactStreamPlanMismatchError{Field: "body"}
	}
	return nil
}

// sameRequestURL reports whether two request URLs are exact-lane equal: same
// scheme, opaque, host (case-insensitive), escaped path, raw query, bare-query
// marker, and userinfo.
func sameRequestURL(actual, expected *url.URL) bool {
	if actual == nil || expected == nil {
		return actual == expected
	}
	if !strings.EqualFold(actual.Scheme, expected.Scheme) {
		return false
	}
	// Opaque data (a non-hierarchical URL like "scheme:opaque?query") is the
	// transmitted request target when set, so it must match too.
	if actual.Opaque != expected.Opaque {
		return false
	}
	if !strings.EqualFold(actual.Host, expected.Host) {
		return false
	}
	if actual.EscapedPath() != expected.EscapedPath() {
		return false
	}
	if actual.RawQuery != expected.RawQuery {
		return false
	}
	// A bare trailing "?" (ForceQuery) changes the transmitted request target
	// even though RawQuery is empty for both "/p" and "/p?", so it must match too.
	if actual.ForceQuery != expected.ForceQuery {
		return false
	}
	// Userinfo (credentials carried in the URL) must match exactly. Userinfo.String
	// guards a nil receiver, so this is safe when either side has no userinfo.
	return actual.User.String() == expected.User.String()
}

// requestEffectiveHost is the host net/http will actually send: Request.Host
// when set, otherwise the URL host.
func requestEffectiveHost(req *http.Request) string {
	if req.Host != "" {
		return req.Host
	}
	return req.URL.Host
}

// planEffectiveHost is the host the admitted plan targets: its single Host
// header field when present (buildExactRequest routes that to Request.Host),
// otherwise the plan URL host.
func planEffectiveHost(expected *ExactAttemptRequest, expURL *url.URL) string {
	for _, h := range expected.Headers {
		if strings.EqualFold(h.Name, "host") {
			return h.Value
		}
	}
	return expURL.Host
}

// compareExactStreamHeaders compares the actual request headers against the
// admitted plan's ordered header fields, both canonicalized and with Host
// excluded (routed to Request.Host and compared as the effective host). Per-name
// value order is significant; cross-name order is not compared (net/http's
// http.Header cannot preserve it — that facet is an admission/parity concern).
// It returns "header:<CanonicalName>" on the first mismatch or "" on a match.
func compareExactStreamHeaders(actual http.Header, expected []HeaderField) string {
	exp := make(map[string][]string)
	for _, h := range expected {
		if strings.EqualFold(h.Name, "host") {
			continue
		}
		key := http.CanonicalHeaderKey(h.Name)
		exp[key] = append(exp[key], h.Value)
	}

	act := make(map[string][]string, len(actual))
	for k, vs := range actual {
		if strings.EqualFold(k, "host") {
			continue
		}
		key := http.CanonicalHeaderKey(k)
		act[key] = append(act[key], vs...)
	}

	for key, expVals := range exp {
		actVals, ok := act[key]
		if !ok || !equalStringSlices(expVals, actVals) {
			return "header:" + key
		}
	}
	for key := range act {
		if _, ok := exp[key]; !ok {
			return "header:" + key
		}
	}
	return ""
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// validateSSEContentType admits a 2xx response only when its Content-Type media
// type is text/event-stream, without buffering the body. Anything else is a
// bounded, typed ExactStreamContentTypeError.
func validateSSEContentType(resp *http.Response) error {
	ct := resp.Header.Get("Content-Type")
	mediaType, _, err := mime.ParseMediaType(ct)
	if err != nil || !strings.EqualFold(mediaType, "text/event-stream") {
		return &ExactStreamContentTypeError{MediaType: boundedMediaType(ct)}
	}
	return nil
}

// boundedMediaType caps the surfaced Content-Type so a pathological header can
// never produce an unbounded diagnostic. A Content-Type carries no secret, so
// the (bounded) value is safe to surface.
func boundedMediaType(ct string) string {
	const max = 128
	if ct == "" {
		return "<empty>"
	}
	if len(ct) > max {
		return ct[:max]
	}
	return ct
}

// streamDeadlinePhase distinguishes which of the two byte-progress deadlines a
// firstBodyIdleReader's watchdog fired in, so an n==0 read after a fire surfaces
// the phase-correct sentinel.
type streamDeadlinePhase int32

const (
	phaseFirstBody streamDeadlinePhase = iota
	phaseIdle
)

// firstBodyIdleReader wraps a streamed 2xx SSE body with two independent
// byte-progress deadlines: a first-body deadline (armed at construction, i.e.
// from successful response headers, until the first raw body byte) and, after
// the first byte, an inter-byte idle deadline reset on every subsequent raw
// byte. It reuses byteProgressWatchdog for the timer discipline and mirrors
// idleTimeoutReader's "data wins" rule: bytes genuinely read are always
// delivered, and a terminal sentinel is surfaced only on a subsequent n==0 read.
type firstBodyIdleReader struct {
	ctx    context.Context
	r      io.Reader
	closer io.Closer

	onFirstBody      func()
	firstBodyTimeout time.Duration
	idleTimeout      time.Duration

	wd byteProgressWatchdog

	// sawFirstByte guards the one-time transition out of the first-body phase.
	sawFirstByte atomic.Bool
	// timedOutPhase records which deadline fired. It is written once inside
	// onDeadline (CAS-guarded by the watchdog) and read only after hasFired() is
	// observed true, which — because onDeadline stores it before closing the conn
	// that unblocks the parked Read — is causally ordered after the store.
	timedOutPhase atomic.Int32
}

// newExactStreamBody wraps a streamed body with the first-body + idle deadlines
// and the first-body liveness signal. With no deadlines and no OnFirstBody it
// returns the body unwrapped, so an unbounded stream pays zero added cost
// (mirrors newIdleTimeoutReader). The first-body deadline is armed immediately:
// this reader is constructed right after successful response headers.
func newExactStreamBody(ctx context.Context, body io.ReadCloser, onFirstBody func(), firstBodyTimeout, idleTimeout time.Duration) io.ReadCloser {
	if body == nil {
		return body
	}
	if firstBodyTimeout <= 0 && idleTimeout <= 0 && onFirstBody == nil {
		return body
	}
	if ctx == nil {
		ctx = context.Background()
	}
	r := &firstBodyIdleReader{
		ctx:              ctx,
		r:                body,
		closer:           body,
		onFirstBody:      onFirstBody,
		firstBodyTimeout: firstBodyTimeout,
		idleTimeout:      idleTimeout,
	}
	r.wd.interrupt = r.onDeadline
	if firstBodyTimeout > 0 {
		r.wd.arm(firstBodyTimeout)
	}
	return r
}

func (r *firstBodyIdleReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)

	if n > 0 {
		firstByte := r.sawFirstByte.CompareAndSwap(false, true)

		if err == nil {
			// DATA WINS: bytes genuinely read are always delivered. Reset the idle
			// deadline for the next gap FIRST — on the first byte this is the
			// first-body -> idle switch, and doing it BEFORE OnFirstBody means a
			// slow/blocking callback can neither delay idle arming nor leave the
			// now-superseded first-body timer armed to fire (and mis-classify as
			// idle) during the callback: arm() advances the watchdog generation,
			// dropping the stale first-body timer.
			r.armIdle()
			if firstByte && r.onFirstBody != nil {
				r.onFirstBody()
			}
			return n, nil
		}

		// Bytes rode in with an error. Still fire the first-body liveness signal
		// (a body byte was observed) before classifying the error. Suppress the
		// error only when it is provably OUR own deadline close (watchdog fired
		// AND a closed-conn error, which only our interrupt can produce); deliver
		// the bytes and let the next n==0 read surface the phase-correct sentinel.
		if firstByte && r.onFirstBody != nil {
			r.onFirstBody()
		}
		if r.wd.hasFired() && isClosedConnErr(err) {
			return n, nil
		}
		r.wd.stop()
		return n, err
	}

	// n == 0: safe to surface a terminal condition. If the watchdog fired, our
	// deadline close stopped the stream — return the phase-correct sentinel,
	// preferring ctx.Err() so a caller cancel is not mislabelled a provider stall.
	if r.wd.hasFired() {
		if ctxErr := r.ctx.Err(); ctxErr != nil {
			return 0, ctxErr
		}
		if streamDeadlinePhase(r.timedOutPhase.Load()) == phaseFirstBody {
			return 0, ErrExactStreamFirstBodyTimeout
		}
		return 0, ErrExactStreamIdleTimeout
	}
	if err != nil {
		r.wd.stop()
	}
	return 0, err
}

// armIdle switches the watchdog to the inter-byte idle deadline after a body
// byte. A non-positive idle timeout means "no idle bound": stop the watchdog so
// the now-irrelevant first-body timer cannot fire, rather than re-arm it.
func (r *firstBodyIdleReader) armIdle() {
	if r.idleTimeout <= 0 {
		r.wd.stop()
		return
	}
	r.wd.arm(r.idleTimeout)
}

// onDeadline is the watchdog interrupt: it records which phase fired (from
// sawFirstByte) and severs the connection to unblock a parked Read. The watchdog
// CAS-guards it to run at most once.
func (r *firstBodyIdleReader) onDeadline() {
	if r.sawFirstByte.Load() {
		r.timedOutPhase.Store(int32(phaseIdle))
	} else {
		r.timedOutPhase.Store(int32(phaseFirstBody))
	}
	_ = r.closer.Close()
}

// Close stops the watchdog and closes the underlying body. Safe to call
// multiple times: the underlying net/http resp.Body Close is idempotent.
func (r *firstBodyIdleReader) Close() error {
	r.wd.markClosed()
	return r.closer.Close()
}
