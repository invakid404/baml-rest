package llmhttp

// Exact-attempt transport primitive.
//
// This file adds a neutral, provider-agnostic carrier (ExactAttemptRequest /
// ExactAttemptResponse) and a net/http-only single-RoundTrip executor
// (ExactExecutor) BESIDE the legacy Request/Client above. It exists to send an
// already-verified plan's exact bytes — ordered header fields (repeated names
// allowed) and a raw []byte body — without the lossy map[string]string /
// string carrier the legacy path uses.
//
// Deliberate non-goals, so the "exact" claim stays honest:
//   - NO redirect following, NO cookie jar, NO URL rewrite, NO request
//     signing, NO fast/fasthttp backend, and NO hidden same-request retry.
//     A single RoundTrip on a RoundTripper is a single physical request by
//     construction — redirects and cookies are http.Client features, not
//     RoundTripper ones.
//   - The URL, header fields, and body are never mutated. Any URL/base
//     override or signing must happen in the planner BEFORE the plan reaches
//     this executor.
//
// This code lives in a production package but is UNREACHABLE from production
// routing: nothing in the generated adapter, handlers, or codegen calls it.
// It is the Phase 6 Slice 6a primitive; the nanollm PreparedRequest adapter
// and the response/translation pipeline are later slices and live outside the
// root module.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// HeaderField is a single ordered request header pair. Unlike the legacy
// Request.Headers map[string]string, a []HeaderField retains engine order and
// repeated names/values — the two properties the exact lane must not lose.
type HeaderField struct {
	Name  string
	Value string
}

// ExactAttemptRequest is the neutral, provider-agnostic carrier for one exact
// HTTP attempt. It knows nothing about model aliases, providers, or nanollm.
//
// Headers preserve pair order and repeated names; Body is raw bytes with no
// string conversion. BodyPresent distinguishes "no body" (nil/false) from a
// present zero-length body (true) so the executor never collapses the two
// through a `len(Body) == 0` test — the mistake the legacy `Body != ""` carrier
// makes.
type ExactAttemptRequest struct {
	Method      string
	URL         string
	Headers     []HeaderField // engine order; repeated names allowed
	Body        []byte        // raw bytes, no string conversion
	BodyPresent bool          // distinguish nil/no-body vs present zero-length
}

// ExactAttemptResponse retains the status, headers, and raw body of a
// completed attempt for EVERY HTTP status — 2xx and non-2xx alike are returned
// as data. Unlike the legacy Execute path, a non-2xx does not become an
// *HTTPError here; the caller (a later slice) decides how to interpret it.
type ExactAttemptResponse struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
}

// MaxExactErrorBodyBytes caps the body read for a non-2xx exact response at
// 64 KiB. This is deliberately lane-specific and distinct from the legacy
// MaxErrorBodyBytes (4 KiB diagnostic cap): it matches nanollm v0.3.2's
// executor input to its uniform error envelope, so the response side of a
// later slice sees the same bounded provider body nanollm's own Do would.
// Successful bodies reuse the existing MaxResponseBodyBytes (16 MiB) cap.
const MaxExactErrorBodyBytes = 64 << 10 // 64 KiB

// ErrExactDeclined is the umbrella sentinel for an attempt the exact lane
// refuses to send verbatim. Test/caller code gates on it via
// errors.Is(err, ErrExactDeclined); errors.As(err, &de) recovers the field and
// reason. A decline is NOT a transport failure — no socket is opened.
var ErrExactDeclined = errors.New("llmhttp: exact attempt declined")

// ExactDeclineError explains why an ExactAttemptRequest was declined before
// any RoundTrip. Field names a header (never a value — names are always safe to
// surface) and Reason is a short, secret-free explanation. It unwraps to
// ErrExactDeclined.
type ExactDeclineError struct {
	Field  string
	Reason string
}

func (e *ExactDeclineError) Error() string {
	return fmt.Sprintf("llmhttp: exact attempt declined (%s): %s", e.Field, e.Reason)
}

func (e *ExactDeclineError) Unwrap() error { return ErrExactDeclined }

// exactControlledHeaders are request headers the transport itself frames, or
// that carry ambiguous connection/framing semantics the exact lane refuses to
// forward verbatim. Presence of any of these (case-insensitive) makes the
// executor decline rather than silently let net/http override the value or
// mis-frame the request. Host is handled separately (a single Host is routed
// to http.Request.Host; a duplicate Host is ambiguous and declines).
var exactControlledHeaders = map[string]struct{}{
	"content-length":      {},
	"transfer-encoding":   {},
	"connection":          {},
	"keep-alive":          {},
	"proxy-connection":    {},
	"trailer":             {},
	"te":                  {},
	"upgrade":             {},
	"proxy-authenticate":  {},
	"proxy-authorization": {},
}

// defaultExactTransport backs NewExactExecutor(nil). It is a DEDICATED
// transport for the exact lane — deliberately NOT the shared defaultLLMTransport
// — because three otherwise-silent net/http.Transport behaviours would each
// break the exact-attempt contract:
//
//   - DisableCompression: true. A stock Transport auto-injects an
//     "Accept-Encoding: gzip" REQUEST header the plan never supplied, then
//     transparently gunzips the response and strips its Content-Encoding. That
//     would put an unplanned header on the wire, hand back a DECODED body
//     instead of the provider's raw bytes (with Headers no longer describing
//     it), and apply the 16 MiB / 64 KiB caps to decoded rather than raw bytes.
//     Disabling it means no Accept-Encoding is added and the raw body +
//     Content-Encoding are returned verbatim; a plan that itself sets
//     Accept-Encoding is still forwarded (net/http only auto-decodes a gzip it
//     implicitly requested).
//   - Proxy: nil. ProxyFromEnvironment would let a proxy URL carrying userinfo
//     make net/http inject a Proxy-Authorization header — an auth injection the
//     exact lane must never perform. The exact lane does not proxy.
//   - DisableKeepAlives: true. net/http can internally REPLAY a replayable
//     request (idempotent methods, or any request whose Body has GetBody — which
//     http.NewRequestWithContext sets for this byte-reader carrier) onto a fresh
//     connection when a pooled connection turns out to be stale
//     (shouldRetryRequest fires only on a REUSED connection). Never reusing a
//     connection removes that path entirely, so one RoundTrip is one physical
//     request for EVERY method, not just non-idempotent ones. This also keeps
//     the lane HTTP/1.1-only (ForceAttemptHTTP2 unset with an explicit
//     TLSClientConfig + DialContext), avoiding HTTP/2's separate stream-retry
//     semantics. The cost is no connection pooling — acceptable for a gated,
//     unreachable-from-routing exactness primitive.
//
// A caller MAY pass their own RoundTripper to NewExactExecutor (e.g. a
// loopback-guarded test transport); they then own these guarantees for that
// transport.
var defaultExactTransport = &http.Transport{
	Proxy: nil,
	DialContext: (&net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}).DialContext,
	TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
	TLSHandshakeTimeout:   10 * time.Second,
	DisableCompression:    true,
	DisableKeepAlives:     true,
	ExpectContinueTimeout: 1 * time.Second,
}

// ExactExecutor performs exactly one net/http RoundTrip for an
// ExactAttemptRequest.
//
// It dispatches over an http.RoundTripper — the dedicated defaultExactTransport
// by default (see NewExactExecutor), or a caller-supplied one. Using a
// RoundTripper (not an http.Client) already removes redirect following, the
// cookie jar, and URL rewrite, none of which a RoundTripper has. The default
// transport additionally disables keep-alives, compression, and proxying so
// that one RoundTrip is one unmodified physical request: no stale-connection
// replay, no injected Accept-Encoding / transparent decode, and no injected
// Proxy-Authorization. TLS/cancellation/the default unary timeout are preserved
// because none of them mutates the request.
type ExactExecutor struct {
	transport http.RoundTripper
}

// NewExactExecutor returns an ExactExecutor dispatching over rt. A nil rt uses
// defaultExactTransport — the hardened, transparent, single-attempt transport
// documented on that var (no keep-alive replay, no compression injection/decode,
// no proxy-auth injection). Pass your own RoundTripper only if it upholds the
// same transparency and single-physical-attempt guarantees; a stock pooled
// *http.Transport does NOT (it can inject Accept-Encoding and replay replayable
// requests onto a stale pooled connection).
func NewExactExecutor(rt http.RoundTripper) *ExactExecutor {
	return &ExactExecutor{transport: rt}
}

// Execute sends req with exactly one RoundTrip and returns the status, headers,
// and raw body for ANY status as data. Go error is reserved for the four
// non-response failure modes only: a decline (ExactDeclineError, before any
// socket), request construction, cancellation/timeout and transport failure,
// and response-body read failure. The response body is closed on every path.
//
// Body caps: a 2xx body over MaxResponseBodyBytes (16 MiB) fails the attempt
// (matching the legacy success cap); a non-2xx body is pinned to
// MaxExactErrorBodyBytes (64 KiB) and returned truncated as data.
//
// The URL, headers, and body are sent verbatim — no rewrite, no signing, no
// auth injection. Header pair order and repeated values are preserved up to
// http.Request via Header.Add; global HTTP/1.1 header-line order and casing are
// explicitly NOT claimed (net/http does not expose them, and HTTP/2 has no
// equivalent), so this executor makes no promise about them.
func (e *ExactExecutor) Execute(ctx context.Context, req *ExactAttemptRequest) (*ExactAttemptResponse, error) {
	return e.ExecuteWithHeartbeat(ctx, req, nil)
}

// ExecuteWithHeartbeat is Execute with an optional first-2xx liveness callback.
// onSuccess, when non-nil, is invoked EXACTLY ONCE the instant the provider
// returns a 2xx status line — AFTER RoundTrip observes the response headers but
// BEFORE the (possibly large/slow) body is buffered — so a pool hung-detector
// sees liveness while the body still streams and never judges the worker hung on
// a slow body. It is the exact-lane twin of the onSuccess callback the legacy
// Client.ExecuteBorrowed hands the BAML send path; it is a pre-canary
// compatibility requirement so unary hung-detection is unchanged when the native
// lane later transports (de-BAML cutover Slice 6).
//
// The callback observes ONLY liveness: it is called with no arguments and MUST
// NOT be able to mutate the request, the response, or the plan. It never fires on
// a non-2xx status (which is returned as ordinary data) nor on any pre-body error
// (decline, construction, transport). A nil onSuccess makes this identical to
// Execute.
func (e *ExactExecutor) ExecuteWithHeartbeat(ctx context.Context, req *ExactAttemptRequest, onSuccess func()) (*ExactAttemptResponse, error) {
	if e == nil {
		return nil, fmt.Errorf("llmhttp: nil ExactExecutor")
	}
	if req == nil {
		return nil, fmt.Errorf("llmhttp: nil ExactAttemptRequest")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	rt := e.transport
	if rt == nil {
		rt = defaultExactTransport
	}

	// Bound the attempt with the default unary timeout when the caller set no
	// deadline, mirroring Client.Execute. Applied via context, never on the
	// transport, so we neither mutate the request nor the shared RoundTripper.
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, DefaultCallTimeout)
		defer cancel()
	}

	httpReq, err := buildExactRequest(ctx, req)
	if err != nil {
		return nil, err
	}

	// Exactly one physical attempt. RoundTrip follows no redirects and consults
	// no cookie jar (both are http.Client concerns). With the default transport
	// (keep-alives disabled) net/http also cannot replay a replayable request
	// onto a stale pooled connection — shouldRetryRequest fires only on a REUSED
	// connection — so this single call is a single server-visible request. The
	// acceptance tests pin the invariant with a server-side request counter,
	// including a stale-connection/replay regression.
	resp, err := rt.RoundTrip(httpReq)
	if err != nil {
		// Initial-attempt wrap site: no body has been read yet, so a torn-down
		// reusable connection (bare EOF / GOAWAY) is an unambiguous transport
		// flake — pass staleConnTeardownAcceptable=true, as Client.Execute does
		// at its Do site. Cancellation/deadline stay reachable via errors.Is on
		// the wrapped chain regardless.
		if te := classifyTransportErr(err, "llmhttp: exact roundtrip failed", true); te != nil {
			return nil, te
		}
		return nil, fmt.Errorf("llmhttp: exact roundtrip failed: %w", err)
	}
	defer resp.Body.Close()

	success := resp.StatusCode >= 200 && resp.StatusCode < 300

	// First-2xx liveness: fire the heartbeat the instant the 2xx status line is
	// observed, BEFORE the body is read, so a slow/large body cannot stall
	// hung-detection. Never fires on a non-2xx (returned as data) or any pre-body
	// failure above. The request is never mutated — onSuccess takes no arguments.
	if success && onSuccess != nil {
		onSuccess()
	}

	limit := int64(MaxResponseBodyBytes)
	if !success {
		limit = int64(MaxExactErrorBodyBytes)
	}

	// Read limit+1 so an over-cap success is detectable; a non-2xx over-cap is
	// pinned to the cap and returned truncated.
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		// Body-read wrap site: bytes may already have been delivered, so the
		// gated stale-conn-teardown family must NOT be treated as an acceptable
		// flake here (staleConnTeardownAcceptable=false) — same contract as
		// Client.Execute's body read.
		if te := classifyTransportErr(err, "llmhttp: exact response read failed", false); te != nil {
			return nil, te
		}
		return nil, fmt.Errorf("llmhttp: exact response read failed: %w", err)
	}
	if int64(len(body)) > limit {
		if success {
			return nil, fmt.Errorf("llmhttp: exact response body exceeds maximum size (%d bytes)", MaxResponseBodyBytes)
		}
		body = body[:limit]
	}

	return &ExactAttemptResponse{
		StatusCode: resp.StatusCode,
		Headers:    resp.Header,
		Body:       body,
	}, nil
}

// tokenChar is the RFC 7230 header-field-name token character set. net/http's
// Transport rejects a name outside it at RoundTrip; the exact lane declines it
// pre-socket instead.
var tokenChar = func() [256]bool {
	var t [256]bool
	const specials = "!#$%&'*+-.^_`|~"
	for c := 'a'; c <= 'z'; c++ {
		t[c] = true
	}
	for c := 'A'; c <= 'Z'; c++ {
		t[c] = true
	}
	for c := '0'; c <= '9'; c++ {
		t[c] = true
	}
	for i := 0; i < len(specials); i++ {
		t[specials[i]] = true
	}
	return t
}()

// ValidHeaderName reports whether name is a valid HTTP/1 header field name — a
// non-empty RFC 7230 token. It mirrors the check net/http's Transport applies at
// RoundTrip, so the exact lane can decline a malformed name before any socket.
func ValidHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		if !tokenChar[name[i]] {
			return false
		}
	}
	return true
}

// ValidHeaderValue reports whether v is a valid HTTP/1 header field value: it
// carries no control byte other than horizontal tab (DEL and any byte < 0x20
// except HTAB are rejected). This mirrors what net/http enforces at RoundTrip,
// so an Authorization or any header value bearing an embedded NUL/CR/LF/control
// byte is declined pre-socket instead of being rejected mid-attempt by the
// transport. Bytes ≥ 0x80 are permitted, matching net/http.
func ValidHeaderValue(v string) bool {
	for i := 0; i < len(v); i++ {
		b := v[i]
		if (b < 0x20 && b != '\t') || b == 0x7f {
			return false
		}
	}
	return true
}

// PreflightHeaders runs the exact lane's full header-admissibility scan WITHOUT
// constructing an *http.Request or opening a socket — the validation-only half
// of buildExactRequest. It returns an *ExactDeclineError (wrapping
// ErrExactDeclined) when a header field name is not a valid token, a header
// field value carries a control byte net/http would reject at RoundTrip, a
// transport-controlled header (Content-Length, Transfer-Encoding, Connection,
// proxy/framing fields, …) is present, or a duplicate Host is ambiguous; and nil
// when every header is forwardable verbatim. Only header NAMES appear in a
// decline; values are never echoed, so a decline diagnostic can never leak a
// secret.
//
// It exists so a planner can prove a prepared plan is exact-transport-admissible
// as a pre-send admission step (the "exact-transport header validation succeeds
// before RoundTrip" gate) while still opening zero sockets. Execute's own
// buildExactRequest calls it first, so the executor and a preflighting planner
// share one source of truth for what the exact lane forwards.
func (r *ExactAttemptRequest) PreflightHeaders() error {
	hostCount := 0
	for _, h := range r.Headers {
		if !ValidHeaderName(h.Name) {
			return &ExactDeclineError{
				Field:  h.Name,
				Reason: "header field name is not a valid HTTP token",
			}
		}
		if !ValidHeaderValue(h.Value) {
			// The value is never echoed — only the name — so a control-bearing
			// secret can never leak through this diagnostic.
			return &ExactDeclineError{
				Field:  h.Name,
				Reason: "header field value carries a control byte net/http rejects at RoundTrip",
			}
		}
		lower := strings.ToLower(h.Name)
		if lower == "host" {
			hostCount++
			continue
		}
		if _, controlled := exactControlledHeaders[lower]; controlled {
			return &ExactDeclineError{
				Field:  h.Name,
				Reason: "transport-controlled header is not forwarded verbatim by the exact lane",
			}
		}
	}
	if hostCount > 1 {
		return &ExactDeclineError{
			Field:  "Host",
			Reason: "ambiguous duplicate Host header",
		}
	}
	return nil
}

// Preflight proves req is exact-transport-admissible WITHOUT opening a socket or
// performing a RoundTrip: it runs the same construction + header scan
// buildExactRequest performs (the checks net/http would otherwise enforce at
// RoundTrip) and discards the built *http.Request without ever dialing it. It
// returns the same decline Execute would refuse to send on, and nil when the
// plan is admissible. A planner uses it to prove admissibility at the
// exact-transport boundary with a hard guarantee of zero sockets — the executor
// it is called on is the very one a later send would use, so a stray RoundTrip
// would be observable on that executor's transport.
func (e *ExactExecutor) Preflight(req *ExactAttemptRequest) error {
	if e == nil {
		return fmt.Errorf("llmhttp: nil ExactExecutor")
	}
	if req == nil {
		return fmt.Errorf("llmhttp: nil ExactAttemptRequest")
	}
	_, err := buildExactRequest(context.Background(), req)
	return err
}

// buildExactRequest converts an ExactAttemptRequest into an *http.Request,
// declining ambiguous or transport-controlled header shapes before any socket
// is opened. The body is defensively copied at this ownership boundary so
// neither the executor nor the transport aliases the caller's slice — the
// carrier's "these are the exact bytes; do not mutate" contract.
func buildExactRequest(ctx context.Context, req *ExactAttemptRequest) (*http.Request, error) {
	// Scan headers first so a decline never opens a socket — the shared
	// PreflightHeaders gate a preflighting planner also uses. A single Host is
	// routed to http.Request.Host below; a duplicate Host is ambiguous.
	if err := req.PreflightHeaders(); err != nil {
		return nil, err
	}
	var host string
	hostCount := 0
	for _, h := range req.Headers {
		if strings.EqualFold(h.Name, "host") {
			hostCount++
			host = h.Value
		}
	}

	// Present body (including a present zero-length body) gets a byte reader so
	// BodyPresent survives; an absent body stays nil. The copy makes the reader
	// own its bytes independently of the caller.
	var body io.Reader
	if req.BodyPresent {
		buf := make([]byte, len(req.Body))
		copy(buf, req.Body)
		body = bytes.NewReader(buf)
	}

	httpReq, err := http.NewRequestWithContext(ctx, req.Method, req.URL, body)
	if err != nil {
		return nil, fmt.Errorf("llmhttp: exact request build failed: %w", err)
	}

	// net/http sends Request.Host (not a "Host" entry in Request.Header) on the
	// wire, so a caller-supplied Host must be routed here to take effect.
	if hostCount == 1 {
		httpReq.Host = host
	}

	// Add every non-Host field in pair order, retaining repeated names and
	// per-name value order. Header.Add (not Set) is what preserves repeats.
	for _, h := range req.Headers {
		if strings.EqualFold(h.Name, "Host") {
			continue
		}
		httpReq.Header.Add(h.Name, h.Value)
	}

	return httpReq, nil
}

// exactSafeHeaderValues is the allowlist of request-header names whose values
// are safe to render verbatim in diagnostics. Every other value — Authorization,
// api keys, cookies, signing material — is redacted to its name only. Matched
// case-insensitively.
var exactSafeHeaderValues = map[string]struct{}{
	"content-type":      {},
	"accept":            {},
	"anthropic-version": {},
}

// exactAttemptSeq backs NextExactAttemptID with a process-monotonic counter.
var exactAttemptSeq atomic.Uint64

// NextExactAttemptID returns a short, process-unique attempt ID for correlating
// diagnostics without embedding any request content.
func NextExactAttemptID() string {
	return "exact-" + strconv.FormatUint(exactAttemptSeq.Add(1), 10)
}

// RedactedURL returns rawURL with userinfo and the query string removed, so a
// diagnostic line can show scheme+host+path without leaking credentials that
// providers sometimes carry in user:password@host or in query parameters. A
// present query is replaced with a "?<redacted>" marker rather than dropped
// silently; an unparseable URL yields a fixed placeholder rather than echoing
// the raw (possibly secret-bearing) input.
func RedactedURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "<unparseable-url>"
	}
	u.User = nil
	hadQuery := u.RawQuery != "" || u.ForceQuery
	u.RawQuery = ""
	u.ForceQuery = false
	u.Fragment = ""
	out := u.String()
	if hadQuery {
		out += "?<redacted>"
	}
	return out
}

// RedactedSummary returns a single-line, secret-safe description of the attempt
// for logs or error diagnostics: attempt ID, method, redacted URL, ordered
// header NAMES (allowlisted values shown, everything else name-only), and the
// body length plus a short content hash. It never renders a non-allowlisted
// header value or any body bytes, matching nanollm's fail-closed redaction
// posture. This is the ONLY sanctioned way to format an ExactAttemptRequest for
// output.
func (r *ExactAttemptRequest) RedactedSummary(attemptID string) string {
	if r == nil {
		return "exact-attempt <nil>"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "exact-attempt id=%s method=%s url=%s headers=[", attemptID, r.Method, RedactedURL(r.URL))
	for i, h := range r.Headers {
		if i > 0 {
			b.WriteString(", ")
		}
		if _, safe := exactSafeHeaderValues[strings.ToLower(h.Name)]; safe {
			fmt.Fprintf(&b, "%s=%s", h.Name, h.Value)
		} else {
			b.WriteString(h.Name)
		}
	}
	b.WriteString("]")
	if r.BodyPresent {
		sum := sha256.Sum256(r.Body)
		fmt.Fprintf(&b, " body=%dB sha256:%s", len(r.Body), hex.EncodeToString(sum[:])[:12])
	} else {
		b.WriteString(" body=absent")
	}
	return b.String()
}
