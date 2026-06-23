// Package llmhttp provides a thin HTTP client wrapper for executing LLM
// requests (both streaming and non-streaming). It converts a generic request
// specification (URL, method, headers, body) into an HTTP request, sends it,
// and returns either a stream of SSE events or the complete response body.
//
// This package does NOT depend on the BAML SDK. The conversion from
// baml.HTTPRequest to the types in this package is done in the generated
// adapter code.
package llmhttp

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/valyala/fasthttp"

	"github.com/invakid404/baml-rest/bamlutils/sseclient"
	"github.com/invakid404/baml-rest/bamlutils/urlrewrite"
)

// Request describes an HTTP request to send to an LLM provider.
// This is a plain struct — the adapter codegen converts baml.HTTPRequest
// into this type.
type Request struct {
	URL     string
	Method  string
	Headers map[string]string
	Body    string // Raw request body (JSON string)

	// AWSAuth, when non-nil, drives the SigV4 signing hook after URL
	// rewrite and before backend dispatch. nil leaves headers untouched
	// — every non-bedrock provider hits that path. Used by both the
	// call path (Execute) and the streaming path (ExecuteStream /
	// ExecuteAWSStream).
	AWSAuth *AWSAuthConfig
}

// StreamResponse represents an active streaming HTTP connection to an LLM
// provider. The caller must call Close() when done reading events.
type StreamResponse struct {
	// StatusCode is the HTTP response status code.
	StatusCode int

	// Headers are the HTTP response headers.
	Headers http.Header

	// Events is the channel of SSE events parsed from the response body.
	// It is closed when the stream ends (EOF or error).
	Events <-chan sseclient.Event

	// Errc receives exactly one value (the terminal error) when the event
	// stream ends. nil means clean EOF.
	Errc <-chan error

	// body holds the response body for cleanup.
	body io.ReadCloser
}

// Close releases the HTTP connection and interrupts the SSE parser goroutine.
// Callers should drain the Events channel before calling Close to avoid losing
// buffered events. It is safe to call multiple times.
func (s *StreamResponse) Close() {
	if s == nil || s.body == nil {
		return
	}
	s.body.Close()
	s.body = nil
}

// Response represents a completed non-streaming HTTP response from an LLM
// provider. Unlike StreamResponse, the full body has already been read.
//
// # Lifetime / borrow contract
//
// A Response can hold a body that BORROWS pooled transport storage (the
// fasthttp response buffer, or a pooled drain buffer). Such a Response is
// produced only by Client.ExecuteBorrowed; the body accessors (BodyString /
// BodyBytes) then return data that is valid ONLY until Release/Close is
// called, after which the storage returns to its pool and may be overwritten.
// The consumer MUST call Release (typically `defer resp.Release()`) and MUST
// NOT retain any returned string/[]byte past that point. Anything that must
// outlive the response — raw/reasoning text crossing a retry boundary, an
// error diagnostic — must be copied before Release.
//
// Client.Execute (the default) never returns a borrow: its body is an owned
// copy (net/http already owns its io.ReadAll buffer; the fasthttp lanes are
// materialized into an owned copy before return), so the accessors stay valid
// for the life of the value and Release is a harmless no-op. External callers
// that do not want to manage a borrow should use Execute.
type Response struct {
	// StatusCode is the HTTP response status code.
	StatusCode int

	// Headers are the HTTP response headers.
	Headers http.Header

	// body is the complete response body bytes. On owned responses (Execute,
	// or the net/http lane) it is valid for the life of the Response; on a
	// borrowed response (ExecuteBorrowed fasthttp lanes) it aliases pooled
	// storage and is valid only until Release/Close. Stored privately so
	// Release can invalidate it — a stale exported field would be a
	// use-after-release footgun.
	body []byte

	// exposeBytes reports whether BodyBytes returns body or nil. It is true
	// only on the net/http unary lane, where io.ReadAll already owns a fresh
	// []byte; the fasthttp lanes leave it false so BodyBytes stays nil and the
	// orchestrator keeps routing them through the string extractor (the
	// per-backend extractor-routing invariant, see body_bytes_test.go). This
	// is independent of borrow vs owned.
	exposeBytes bool

	// fastResp / drainBuf hold the pooled transport storage backing body on a
	// borrowed response; Release returns it to its pool. At most one is
	// non-nil. Both nil means the body is owned (the net/http lane, or a copy
	// produced by Execute) and Release is a no-op. These are concrete fields
	// rather than a release closure so ExecuteBorrowed allocates nothing per
	// request beyond the Response itself — the point of the borrow is to drop
	// the full-body copy, not trade it for a closure.
	fastResp *fasthttp.Response // buffered fast lane: fasthttp.ReleaseResponse
	drainBuf *bytes.Buffer      // streamed fast lane: returnFastBodyBuf

	// released guards Release/Close against double-free and makes the
	// accessors fail closed (empty) after release.
	released bool
}

// borrowed reports whether r holds pooled storage that Release must return.
func (r *Response) borrowed() bool {
	return r != nil && (r.fastResp != nil || r.drainBuf != nil)
}

// BodyString returns the complete response body as a string. It is a
// zero-copy view over the body bytes (see bytesToBodyString), so callers MUST
// NOT mutate the body while the string is in use. On a borrowed response
// (ExecuteBorrowed) the returned string is valid only until Release/Close;
// on an owned response (Execute) it is valid for the life of the Response.
func (r *Response) BodyString() string {
	if r == nil {
		return ""
	}
	return bytesToBodyString(r.body)
}

// BodyBytes returns the complete response body as bytes on the net/http unary
// lane (where io.ReadAll already owns a fresh []byte), or nil on the fasthttp
// lanes and on a released response. The orchestrator branches on a non-nil
// BodyBytes to select the byte extractor, so this per-backend contract must
// hold. Treat as read-only: BodyString aliases the same backing array. On a
// borrowed response the slice is valid only until Release/Close.
func (r *Response) BodyBytes() []byte {
	if r == nil || !r.exposeBytes {
		return nil
	}
	return r.body
}

// Release returns any pooled storage backing the response body to its pool and
// invalidates BodyString/BodyBytes (they return empty/nil afterwards). It is
// idempotent and always safe to call, including on owned responses (where it
// is a no-op) and on nil. Callers of ExecuteBorrowed MUST call it; the
// canonical pattern is `defer resp.Release()` immediately after the error
// check.
func (r *Response) Release() {
	if r == nil || r.released {
		return
	}
	r.released = true
	// Owned responses (net/http, or a copy produced by Execute) hold no pooled
	// storage, so Release is a true no-op for them: leave body / exposeBytes
	// intact so a `defer resp.Release()` does not invalidate an owned body. Only
	// a genuinely borrowed response returns its pooled storage and clears the
	// now-dangling body view.
	if !r.borrowed() {
		return
	}
	r.body = nil
	if r.fastResp != nil {
		fasthttp.ReleaseResponse(r.fastResp)
		r.fastResp = nil
	}
	if r.drainBuf != nil {
		returnFastBodyBuf(r.drainBuf)
		r.drainBuf = nil
	}
}

// Close is an io.Closer-style alias for Release; it always returns nil. It
// lets a Response satisfy `func() error` close-on-defer call sites.
func (r *Response) Close() error {
	r.Release()
	return nil
}

// own returns an owned copy of r with any borrowed pooled storage released, so
// the result is safe to hold indefinitely. For an already-owned response (the
// net/http lane, or a body Execute already copied) it returns r unchanged. For
// a borrowed response it copies the body into a fresh caller-owned []byte,
// releases the borrow, and returns a new owned Response preserving
// StatusCode/Headers and the exposeBytes routing flag. This backs the safe
// Client.Execute path; ExecuteBorrowed skips it.
func (r *Response) own() *Response {
	if !r.borrowed() {
		return r
	}
	owned := make([]byte, len(r.body))
	copy(owned, r.body)
	out := &Response{
		StatusCode:  r.StatusCode,
		Headers:     r.Headers,
		body:        owned,
		exposeBytes: r.exposeBytes,
	}
	r.Release()
	return out
}

// bytesToBodyString returns a string that shares body's backing array
// without copying. body must be caller-owned and never mutated for the
// lifetime of the returned string — both conditions hold for the
// io.ReadAll buffer on the net/http unary success path. Returning ""
// for an empty body keeps the nil-pointer case safe.
func bytesToBodyString(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	return unsafe.String(unsafe.SliceData(body), len(body))
}

// MaxResponseBodyBytes is the maximum response body size that Execute() will
// read. This prevents unbounded memory consumption from misconfigured
// endpoints. 16 MiB is generous for any LLM completion.
const MaxResponseBodyBytes = 16 << 20 // 16 MiB

// MaxErrorBodyBytes is the maximum response body read for non-2xx error
// responses. Error bodies are included in HTTPError for diagnostics but
// do not need the full MaxResponseBodyBytes allowance.
const MaxErrorBodyBytes = 4096

// Client dispatches LLM requests between a net/http backend (for HTTP/2
// upstreams) and a fasthttp backend (for HTTP/1.1 upstreams). Per-origin
// routing is selected by an ALPN probe on first request and cached for the
// process lifetime. Use DefaultClient for a pre-configured instance or
// NewClient / NewClientWithOptions for custom settings.
//
// Per-Client URL rewrite rules apply to every outbound request via
// resolveRequestURL — supplied at construction so two Clients in the
// same process can route to different upstream endpoints without
// going through urlrewrite.GlobalRules.
//
// useGlobalRewriteRules distinguishes the legacy NewClient(...) seam
// (which falls back to urlrewrite.GlobalRules when rewriteRules is
// nil, preserving env-driven behaviour for non-migrated callers) from
// the explicit NewClientWithOptions seam (where nil rules mean "no
// rewrites" — programmatic callers opt into env/builtin by passing
// urlrewrite.LoadDefaultRules()). Without this split, an options
// client constructed with no rules would silently inherit process-
// global state at request time.
type Client struct {
	httpClient            *http.Client
	cache                 *protocolCache
	rewriteRules          []urlrewrite.Rule
	useGlobalRewriteRules bool

	// streamIdleTimeout is the inter-token idle read timeout (in
	// nanoseconds) applied to streaming responses via idleTimeoutReader.
	// 0 means infinite (no idle bound). Stored atomically so the debug
	// endpoint can adjust it at runtime; in production it is set once at
	// construction and never mutated.
	streamIdleTimeout atomic.Int64
}

// SetStreamIdleTimeout updates the inter-token idle read timeout applied to
// streaming responses. A non-positive duration disables the bound (infinite),
// matching BAML's idle_timeout_ms=0 semantics. Safe for concurrent use.
func (c *Client) SetStreamIdleTimeout(d time.Duration) {
	if c == nil {
		return
	}
	if d < 0 {
		d = 0
	}
	c.streamIdleTimeout.Store(int64(d))
}

// GetStreamIdleTimeout returns the current inter-token idle read timeout
// (0 means infinite).
func (c *Client) GetStreamIdleTimeout() time.Duration {
	if c == nil {
		return 0
	}
	return time.Duration(c.streamIdleTimeout.Load())
}

// FastHTTPClientOptions tunes the fasthttp backend. Each field affects
// only origins that route through fasthttp — either because Mode is
// ClientModeFastHTTP or because Auto mode resolved an origin to
// fasthttp via the ALPN probe.
//
// The net/http backend is tuned via ClientOptions.NetHTTPClient: the
// caller supplies a configured *http.Client whose Transport.* fields
// (MaxConnsPerHost, MaxIdleConnsPerHost, etc.) apply on that path.
type FastHTTPClientOptions struct {
	// MaxConns is the per-origin active connection cap for fasthttp.
	// Values <= 0 use llmhttp's effectively-unbounded default
	// (math.MaxInt32). fasthttp's own zero value falls back to
	// DefaultMaxConnsPerHost = 512, which is too low for LLM traffic
	// that funnels many concurrent requests to a few hosts.
	MaxConns int

	// MaxConnWaitTimeout is copied to fasthttp.HostClient. Zero keeps
	// fasthttp's default no-wait behaviour: when MaxConns is saturated,
	// Do returns ErrNoFreeConns immediately rather than blocking.
	MaxConnWaitTimeout time.Duration

	// MaxIdleConnDuration is copied to fasthttp.HostClient. Zero keeps
	// fasthttp's default (10 seconds). Higher values reduce TLS
	// handshakes for sustained LLM traffic.
	MaxIdleConnDuration time.Duration

	// TLSConfig is used by the fasthttp backend (per-origin HostClient)
	// and the auto-mode ALPN probe. When nil, the default
	// {MinVersion: TLS12} applies. The net/http backend gets its TLS
	// config from NetHTTPClient.Transport.TLSClientConfig.
	TLSConfig *tls.Config
}

// ClientOptions configures NewClientWithOptions / NewDefaultClientWithOptions.
// All fields are optional; the constructors apply sensible defaults
// matching the legacy env-driven NewClient(nil) shape when the
// corresponding field is zero.
type ClientOptions struct {
	// NetHTTPClient is the underlying *http.Client used by the net/http
	// backend. When nil, the constructor's default is used:
	// http.DefaultClient for NewClientWithOptions; the tuned
	// defaultLLMTransport-backed client for NewDefaultClientWithOptions.
	//
	// This field tunes only the net/http backend. The fasthttp backend
	// is tuned via FastHTTP below; under Mode == ClientModeAuto each
	// backend uses its own settings on the origins it serves.
	NetHTTPClient *http.Client
	// Mode forces a specific backend selection (Auto / FastHTTP /
	// NetHTTP). Defaults to ClientModeAuto when zero.
	Mode ClientMode
	// RewriteRules apply to every outbound request URL via
	// resolveRequestURL. The Client takes a defensive copy at
	// construction time, so mutating the supplied slice afterwards
	// has no effect on dispatched requests.
	//
	// Per-constructor semantics for the nil case:
	//   - NewClientWithOptions / NewDefaultClientWithOptions with
	//     RewriteRules == nil → no rewrites applied to this Client
	//     at request time. Explicit construction implies explicit
	//     rules; the Client does not silently inherit
	//     urlrewrite.GlobalRules.
	//   - The legacy NewClient(httpClient) path still falls back to
	//     urlrewrite.GlobalRules when no rules are installed, so
	//     existing env-driven call sites keep their behaviour
	//     unchanged.
	//
	// To opt into the env-driven defaults under the options
	// constructors, pass urlrewrite.LoadDefaultRules().
	RewriteRules []urlrewrite.Rule
	// FastHTTP tunes the fasthttp backend. Applies when Mode is
	// ClientModeFastHTTP or when Auto mode resolves an origin to
	// fasthttp. Ignored entirely when Mode is ClientModeNetHTTP.
	FastHTTP FastHTTPClientOptions

	// StreamIdleTimeout sets the inter-token idle read timeout applied to
	// streaming responses (see idleTimeoutReader). Unlike RewriteRules,
	// a nil value is NOT "disabled": it falls back to
	// DefaultStreamIdleTimeout so the production stream-hang is bounded
	// out of the box even for callers that don't configure it. A non-nil
	// value is used verbatim, including a pointer to 0 which means
	// infinite (no bound), matching BAML's idle_timeout_ms=0 semantics.
	//
	// cmd/serve and cmd/worker pass StreamIdleTimeoutFromEnv() here so the
	// BAML_REST_STREAM_IDLE_TIMEOUT env var is honoured; the explicit
	// constructors themselves do not read env (consistent with the rest of
	// ClientOptions).
	StreamIdleTimeout *time.Duration
}

// NewClient creates a new LLM HTTP client with the given http.Client.
// If httpClient is nil, http.DefaultClient is used.
//
// The client mode (auto / fasthttp / nethttp) is read from the
// BAML_REST_HTTP_CLIENT environment variable at construction time.
// URL rewrites resolve through urlrewrite.GlobalRules at request
// time — compatibility shape for callers that haven't migrated to
// NewClientWithOptions.
//
// # Injected http.Client scope
//
// When a request dispatches to the fasthttp backend (either via the
// auto-mode ALPN probe or the explicit "fasthttp" override), the
// supplied http.Client is used ONLY to derive:
//   - TLS configuration (Transport.TLSClientConfig), mirrored into the
//     probe dialer and the per-origin HostClient so private CAs and
//     custom verification work on the fasthttp path;
//   - Proxy resolution (Transport.Proxy), used to pin proxy-destined
//     origins back to net/http since fasthttp has no
//     ProxyFromEnvironment equivalent.
//
// Other fields of http.Client and http.Transport are intentionally NOT
// honoured on the fasthttp path — Timeout, CheckRedirect, Jar, custom
// RoundTripper wrappers, and Transport-level settings like
// MaxIdleConns, MaxConnsPerHost, or ResponseHeaderTimeout all bypass
// the fasthttp backend. This is by design: fasthttp is the alternative
// transport, not a layer underneath net/http. Callers who need a
// specific http.Client invariant to apply to every request should
// either wrap llmhttp.Client at a higher layer, or force the net/http
// path via BAML_REST_HTTP_CLIENT=nethttp.
func NewClient(httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	// Legacy seam: mirror the injected http.Client's TLS config into the
	// fasthttp/probe path so private CAs and custom verification keep
	// working under BAML_REST_HTTP_CLIENT=fasthttp / Auto.
	fast := FastHTTPClientOptions{
		TLSConfig: tlsConfigFromHTTPClient(httpClient),
	}
	c := &Client{
		httpClient:            httpClient,
		cache:                 newProtocolCache(loadClientMode(), fast, proxyFuncFromHTTPClient(httpClient)),
		useGlobalRewriteRules: true,
	}
	// Legacy/env seam: mirror the env-driven Mode resolution above by
	// reading BAML_REST_STREAM_IDLE_TIMEOUT (default 5m, "0" = infinite),
	// so DefaultClient is protected against silent provider stalls out of
	// the box.
	c.SetStreamIdleTimeout(StreamIdleTimeoutFromEnv())
	return c
}

// NewClientWithOptions constructs a Client from an explicit
// ClientOptions value. Unlike NewClient, the backend mode and URL
// rewrite rules are passed in rather than read from process env, so
// two Clients in the same process can route requests differently.
//
// RewriteRules is defensively cloned so a library caller mutating
// the supplied slice afterwards cannot race with concurrent
// Execute / ExecuteStream / ExecuteAWSStream calls reading
// c.rewriteRules at request time.
//
// NetHTTPClient nil falls back to http.DefaultClient (matching NewClient).
// For the tuned LLM transport, use NewDefaultClientWithOptions.
func NewClientWithOptions(opts ClientOptions) *Client {
	httpClient := opts.NetHTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	c := &Client{
		httpClient:   httpClient,
		cache:        newProtocolCache(opts.Mode, opts.FastHTTP, proxyFuncFromHTTPClient(httpClient)),
		rewriteRules: slices.Clone(opts.RewriteRules),
	}
	// A nil StreamIdleTimeout falls back to the protective default rather
	// than disabling the bound (see ClientOptions.StreamIdleTimeout). A
	// non-nil value is used verbatim, including a pointer to 0 = infinite.
	idle := DefaultStreamIdleTimeout
	if opts.StreamIdleTimeout != nil {
		idle = *opts.StreamIdleTimeout
	}
	c.SetStreamIdleTimeout(idle)
	return c
}

// NewDefaultClientWithOptions builds a Client whose underlying
// *http.Client uses defaultLLMTransport — the tuned LLM-traffic
// transport that backs the package-level DefaultClient. opts.NetHTTPClient
// is overridden.
//
// Use this when constructing a per-process LLM client at startup so
// the same connection-pool sizing the legacy DefaultClient relied on
// applies to the new explicit client.
func NewDefaultClientWithOptions(opts ClientOptions) *Client {
	httpClient := &http.Client{Transport: defaultLLMTransport}
	opts.NetHTTPClient = httpClient
	return NewClientWithOptions(opts)
}

// tlsConfigFromHTTPClient returns the TLSClientConfig of the supplied
// http.Client's *http.Transport when available. The returned config is
// NOT cloned — protocolCache.Clone()s it per probe, and the fasthttp
// HostClient reads its fields via tls.Config's own internal locking.
// Returns nil when the Transport is not an *http.Transport or has no TLS
// config; newProtocolCache treats nil as the default {MinVersion: TLS12}.
func tlsConfigFromHTTPClient(c *http.Client) *tls.Config {
	if c == nil || c.Transport == nil {
		return nil
	}
	t, ok := c.Transport.(*http.Transport)
	if !ok {
		return nil
	}
	return t.TLSClientConfig
}

// proxyFuncFromHTTPClient returns the Proxy resolver configured on the
// supplied http.Client's *http.Transport, falling back to
// http.ProxyFromEnvironment. Exposed so the protocol cache can pin
// proxy-targeted origins to net/http consistently with whatever the
// net/http backend itself does.
func proxyFuncFromHTTPClient(c *http.Client) func(*http.Request) (*url.URL, error) {
	if c != nil && c.Transport != nil {
		if t, ok := c.Transport.(*http.Transport); ok && t.Proxy != nil {
			return t.Proxy
		}
	}
	return http.ProxyFromEnvironment
}

// defaultLLMTransport is an HTTP transport tuned for LLM provider traffic.
//
// The BuildRequest path moves outbound HTTP calls from the BAML Rust runtime
// into Go. Go's http.DefaultTransport has MaxIdleConnsPerHost=2, which is
// far too low for a worker process that funnels many concurrent requests to
// the same small set of provider hosts (api.openai.com, api.anthropic.com,
// etc.). With the default, almost every request under concurrent load creates
// a new TCP+TLS connection (~100-300ms handshake overhead per request).
//
// The caps are deliberately uncapped: MaxConnsPerHost=0 (no active limit),
// MaxIdleConns=0 (no global idle limit), MaxIdleConnsPerHost=math.MaxInt32
// (effectively no per-host idle limit). Worker concurrency is bounded
// upstream by the pool size, so the transport itself does not need to
// throttle further; an artificial cap would just queue requests behind
// new TCP+TLS handshakes for no benefit.
var defaultLLMTransport = &http.Transport{
	Proxy: http.ProxyFromEnvironment,
	DialContext: (&net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}).DialContext,
	ForceAttemptHTTP2: true,
	TLSClientConfig: &tls.Config{
		MinVersion: tls.VersionTLS12,
	},
	TLSHandshakeTimeout:   10 * time.Second,
	MaxIdleConns:          0,
	MaxIdleConnsPerHost:   math.MaxInt32,
	MaxConnsPerHost:       0,
	IdleConnTimeout:       90 * time.Second,
	ExpectContinueTimeout: 1 * time.Second,
}

// DefaultClient is a Client with a transport tuned for LLM provider traffic.
// It is safe for concurrent use. See defaultLLMTransport for details on
// why the defaults differ from http.DefaultTransport.
var DefaultClient = NewClient(&http.Client{Transport: defaultLLMTransport})

// ExecuteStream sends the given request and returns a StreamResponse with
// SSE events parsed from the response body.
//
// The request is expected to be for a streaming LLM endpoint (the body
// should contain "stream": true or equivalent). The response is expected
// to be an SSE stream (Content-Type: text/event-stream).
//
// On success, the caller must call StreamResponse.Close() when done.
// On error (non-2xx status, connection failure), an error is returned and
// no cleanup is needed.
func (c *Client) ExecuteStream(ctx context.Context, req *Request) (*StreamResponse, error) {
	body, status, headers, err := c.openStream(ctx, req)
	if err != nil {
		return nil, err
	}

	// Start SSE parsing on the response body. The terminal value sseclient
	// emits on errc is run through classifyStreamErrc so a mid-stream typed
	// transport drop (ECONNRESET / EPIPE / ECONNREFUSED / net.ErrClosed) or
	// an idle timeout (ErrIdleTimeout) carries ErrTransportFlake out of the
	// streaming path — matching the non-streaming body-read site at Execute
	// below.
	events, errc := sseclient.Stream(ctx, body)

	return &StreamResponse{
		StatusCode: status,
		Headers:    headers,
		Events:     events,
		Errc:       classifyStreamErrc(errc),
		body:       body,
	}, nil
}

// openStream performs the shared streaming connect used by both the
// channel-based ExecuteStream and the synchronous ExecuteStreamCallback: URL
// rewrite, SigV4 signing, net/http request build + Do, status-code check,
// Content-Type validation, and the inter-token idle-read-timeout body wrap.
// It returns the wrapped body (caller owns Close), status code, and response
// headers. On error nothing needs cleanup.
func (c *Client) openStream(ctx context.Context, req *Request) (io.ReadCloser, int, http.Header, error) {
	if c == nil || c.httpClient == nil {
		return nil, 0, nil, fmt.Errorf("llmhttp: nil client")
	}
	if req == nil {
		return nil, 0, nil, fmt.Errorf("llmhttp: nil request")
	}

	rewritten := c.resolveRequestURL(req)

	// SigV4 signing runs after URL rewrite (so the signature matches
	// the host the request actually goes out with) and before either
	// backend dispatch (so net/http and fasthttp see the same signed
	// headers).
	if err := signRequest(ctx, req, rewritten); err != nil {
		return nil, 0, nil, err
	}

	// Streaming always routes through net/http, even when BAML_REST_HTTP_CLIENT
	// resolves the origin to fasthttp for unary Execute. fasthttp response
	// bodies/headers are pooled and released, which makes mid-stream lifetime
	// and chunked-truncation detection fragile; net/http's resp.Body is
	// caller-owned until Close and its stdlib chunked reader surfaces
	// io.ErrUnexpectedEOF natively. The protocol cache and ClientModeFastHTTP
	// therefore affect only unary Execute below — not SSE streaming. See
	// Stage 1 of the streaming memory effort (#475 follow-up).

	httpReq, err := buildHTTPRequest(ctx, req, rewritten)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("llmhttp: failed to build request: %w", err)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		if te := classifyTransportErr(err, "llmhttp: request failed", true); te != nil {
			return nil, 0, nil, te
		}
		return nil, 0, nil, fmt.Errorf("llmhttp: request failed: %w", err)
	}

	// Check for non-success status codes
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, MaxErrorBodyBytes))
		resp.Body.Close()
		return nil, 0, nil, &HTTPError{
			StatusCode: resp.StatusCode,
			Body:       string(body),
		}
	}

	// Validate Content-Type for SSE — reject anything that isn't explicitly
	// text/event-stream. Missing Content-Type is also rejected to fail closed
	// against proxy error pages or misconfigured servers. NDJSON is not
	// accepted because the SSE parser requires data: framing.
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	if !strings.Contains(ct, "text/event-stream") {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, MaxErrorBodyBytes))
		resp.Body.Close()
		if ct == "" {
			return nil, 0, nil, fmt.Errorf("llmhttp: missing Content-Type header (expected text/event-stream): %s", string(body))
		}
		return nil, 0, nil, fmt.Errorf("llmhttp: unexpected Content-Type %q (expected text/event-stream): %s", ct, string(body))
	}

	// Wrap the body with an inter-token idle read timeout before handing it
	// to the SSE parser. On a mid-stream provider stall the watchdog closes
	// the body and surfaces ErrIdleTimeout (a retryable transport flake),
	// so the read is bounded without coupling to any fixed total timeout.
	// A non-positive timeout returns resp.Body unwrapped (infinite). The
	// nil interrupt defaults to resp.Body.Close, which net/http supports
	// concurrently with a parked Read.
	body := newIdleTimeoutReader(ctx, resp.Body, nil, c.GetStreamIdleTimeout())

	return body, resp.StatusCode, resp.Header, nil
}

// StreamCallback represents an active streaming HTTP connection consumed via a
// synchronous per-event callback rather than the StreamResponse channel. It is
// used by the BuildRequest stream orchestrator to parse each SSE event's data
// bytes in place (gjson.GetBytes via sse.ExtractDeltaPartsFromBytes) WITHOUT
// materializing a durable Event.Data string per frame — the saving that the
// buffered channel path cannot make because there the parser runs ahead of the
// consumer.
//
// The channel-based ExecuteStream / StreamResponse path is unchanged and
// remains for all other consumers. The caller must call Close() when done.
type StreamCallback struct {
	// StatusCode is the HTTP response status code.
	StatusCode int

	// Headers are the HTTP response headers.
	Headers http.Header

	// body holds the (idle-timeout-wrapped) response body.
	body io.ReadCloser
}

// ExecuteStreamCallback connects exactly like ExecuteStream (same URL rewrite,
// signing, net/http transport, status/Content-Type checks, and idle-timeout
// body wrap) but returns a StreamCallback for synchronous, per-event byte
// consumption instead of a channel. The caller drives the stream with
// ForEach and must Close() when done.
func (c *Client) ExecuteStreamCallback(ctx context.Context, req *Request) (*StreamCallback, error) {
	body, status, headers, err := c.openStream(ctx, req)
	if err != nil {
		return nil, err
	}
	return &StreamCallback{
		StatusCode: status,
		Headers:    headers,
		body:       body,
	}, nil
}

// ForEach parses the SSE stream synchronously, invoking fn for each event in
// stream order on the calling goroutine. fn receives byte views valid only for
// the duration of the call (see sseclient.ScanEvents / EventBytes); it must
// fully consume them before returning. fn may return sseclient.ErrStopScan to
// halt parsing cleanly (ForEach returns nil).
//
// The terminal stream error is returned (nil on clean EOF), classified the
// same way StreamResponse.Errc is: a mid-stream typed transport drop or idle
// timeout carries the ErrTransportFlake umbrella, while io.ErrUnexpectedEOF
// (chunked truncation) falls through to the generic wrap so its
// content-integrity meaning is preserved.
func (s *StreamCallback) ForEach(ctx context.Context, fn sseclient.EventFunc) error {
	return classifyStreamErr(sseclient.ScanEvents(ctx, s.body, fn))
}

// Close releases the HTTP connection and interrupts a parked read. It is safe
// to call multiple times.
func (s *StreamCallback) Close() {
	if s == nil || s.body == nil {
		return
	}
	s.body.Close()
	s.body = nil
}

// DefaultCallTimeout is the maximum time Execute() will wait for a
// non-streaming LLM response when the caller's context has no deadline.
// This prevents a stalled provider from pinning a worker indefinitely
// after the early heartbeat has satisfied the pool's hung detector.
//
// The value is deliberately generous: most non-streaming completions finish
// in under 60 s, but complex prompts with large output can take longer.
// Callers that need a tighter or looser bound should set their own deadline
// on the context passed to Execute().
const DefaultCallTimeout = 5 * time.Minute

// Execute sends the given request and returns the complete response.
//
// Unlike ExecuteStream, this reads the entire response body (up to
// MaxResponseBodyBytes) and returns it as a string. The request is expected
// to be for a non-streaming LLM endpoint (the body should contain
// "stream": false or equivalent). No Content-Type validation is performed
// since non-streaming responses are typically application/json.
//
// If the provided context has no deadline, Execute wraps it with
// DefaultCallTimeout so that a stalled upstream cannot block forever.
// Callers that supply their own deadline are not affected.
//
// The optional onSuccess callback, if non-nil, is invoked after the HTTP
// response returns a 2xx status but before the response body is read.
// This allows the caller to emit a liveness signal (e.g. heartbeat) once
// a real provider response has been confirmed, without waiting for the
// full body to be buffered. This is important because body reads can be
// slow for large responses and the caller may have external timeouts
// (e.g. pool hung detection) that need a signal before the body is fully
// available.
//
// On error (non-2xx status, connection failure, timeout), an error is returned.
//
// The returned Response is OWNED: its body is safe to hold indefinitely and
// Release is a no-op (see Response's lifetime contract). Callers on the hot
// path that can release promptly should prefer ExecuteBorrowed to avoid the
// extra owned copy on the fasthttp lanes.
func (c *Client) Execute(ctx context.Context, req *Request, onSuccess func()) (*Response, error) {
	resp, err := c.executeBorrow(ctx, req, onSuccess)
	if err != nil {
		return nil, err
	}
	// Materialize an owned copy so external/non-borrow callers never have to
	// manage a pooled-buffer lifetime. own() is free on the net/http lane.
	return resp.own(), nil
}

// ExecuteBorrowed behaves exactly like Execute but returns a BORROWED Response
// on the fasthttp lanes: the body accessors alias pooled transport storage and
// are valid only until Release/Close. This removes the self-inflicted
// full-body copy the fasthttp buffered lane otherwise pays (wire→fasthttp
// buffer, then a second copy into an owned string). The consumer MUST call
// Release when done and MUST copy anything that has to outlive the response
// before releasing. See Response's lifetime contract.
//
// The net/http lane and error responses are unaffected — their bodies are
// already owned (Release is a no-op there), so the borrow is opt-in only for
// the lane where it actually saves a copy.
func (c *Client) ExecuteBorrowed(ctx context.Context, req *Request, onSuccess func()) (*Response, error) {
	return c.executeBorrow(ctx, req, onSuccess)
}

// executeBorrow is the shared unary dispatch core. It returns the body in its
// natural per-lane form, which may borrow pooled storage (fasthttp); Execute
// wraps it with own() for the safe path, ExecuteBorrowed returns it directly.
func (c *Client) executeBorrow(ctx context.Context, req *Request, onSuccess func()) (*Response, error) {
	if c == nil || c.httpClient == nil {
		return nil, fmt.Errorf("llmhttp: nil client")
	}
	if req == nil {
		return nil, fmt.Errorf("llmhttp: nil request")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	// Capture whether the ORIGINAL caller context can be cancelled before we
	// inject DefaultCallTimeout below. A context with no Done channel
	// (context.Background / context.TODO) can only ever be bounded by the
	// deadline we are about to add, so the fasthttp backend can call
	// DoDeadline synchronously and read a fully buffered body — no per-request
	// goroutine, no body intermediate. A context that was already cancellable
	// (server request contexts, retry/orchestration contexts) keeps the
	// goroutine race so a mid-flight cancel still returns the caller promptly.
	// This must be read before WithTimeout, which always adds a Done channel.
	cancellable := ctx.Done() != nil

	// Enforce a deadline on the outbound call if the caller didn't set one.
	// The HTTP client is shared with ExecuteStream (which must not have a
	// fixed timeout), so the timeout is applied per-request via context
	// rather than on the http.Client itself. The fasthttp backend reads the
	// same ctx.Deadline() to drive DoDeadline, so this injection covers both
	// dispatch paths.
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, DefaultCallTimeout)
		defer cancel()
	}

	fastMode := fastUnaryDeadlineOnly
	if cancellable {
		fastMode = fastUnaryCancellable
	}

	rewritten := c.resolveRequestURL(req)

	// SigV4 signing runs after URL rewrite (so the signature matches
	// the host the request actually goes out with) and before either
	// backend dispatch (so net/http and fasthttp see the same signed
	// headers).
	if err := signRequest(ctx, req, rewritten); err != nil {
		return nil, err
	}

	if c.cache != nil {
		if origin, err := parseOrigin(rewritten); err == nil {
			if entry := c.cache.resolve(ctx, origin); entry != nil && entry.decision == decisionFast && entry.host != nil {
				return c.executeFast(ctx, req, rewritten, entry, onSuccess, fastMode)
			}
		}
	}

	httpReq, err := buildHTTPRequest(ctx, req, rewritten)
	if err != nil {
		return nil, fmt.Errorf("llmhttp: failed to build request: %w", err)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		if te := classifyTransportErr(err, "llmhttp: request failed", true); te != nil {
			return nil, te
		}
		return nil, fmt.Errorf("llmhttp: request failed: %w", err)
	}
	defer resp.Body.Close()

	// Check HTTP status before reading the full body. For error responses,
	// read only a small diagnostic body (4KB, same as ExecuteStream) to
	// avoid tying up the worker on a large or slow error body. The full
	// MaxResponseBodyBytes limit is reserved for successful responses only.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, MaxErrorBodyBytes))
		return nil, &HTTPError{
			StatusCode: resp.StatusCode,
			Body:       string(errBody),
		}
	}

	// Signal that a real 2xx response has been received from the provider.
	// Fired before body read so the caller can emit liveness signals
	// (e.g. heartbeat) without waiting for the full body to buffer.
	if onSuccess != nil {
		onSuccess()
	}

	// Read successful response body with size limit to prevent unbounded
	// memory usage. Read MaxResponseBodyBytes+1 to detect truncation: if
	// we get exactly that many bytes, the response exceeded the limit.
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBodyBytes+1))
	if err != nil {
		if te := classifyTransportErr(err, "llmhttp: failed to read response body", false); te != nil {
			return nil, te
		}
		return nil, fmt.Errorf("llmhttp: failed to read response body: %w", err)
	}

	if int64(len(body)) > MaxResponseBodyBytes {
		return nil, fmt.Errorf("llmhttp: response body exceeds maximum size (%d bytes)", MaxResponseBodyBytes)
	}

	// The io.ReadAll buffer is already a fresh, caller-owned []byte, so this
	// lane is "borrowed" only nominally: release is nil (nothing to free) and
	// BodyBytes is exposed so downstream extraction reads it directly via
	// gjson.GetBytes (see buildrequest), with BodyString a zero-copy view over
	// it. own() therefore returns it unchanged for the safe Execute path.
	return &Response{
		StatusCode:  resp.StatusCode,
		Headers:     resp.Header,
		body:        body,
		exposeBytes: true,
	}, nil
}

// HTTPError represents a non-2xx HTTP response from the LLM provider.
type HTTPError struct {
	StatusCode int
	Body       string
}

func (e *HTTPError) Error() string {
	if e.Body != "" {
		return fmt.Sprintf("llmhttp: HTTP %d: %s", e.StatusCode, e.Body)
	}
	return fmt.Sprintf("llmhttp: HTTP %d", e.StatusCode)
}

// resolveRequestURL applies the per-Client URL rewrite rules to the
// request URL. Exposed as a method so the dispatcher can rewrite
// exactly once and feed the result to both the cache key resolver and
// whichever backend handles the request.
//
// The urlrewrite.GlobalRules fallback fires ONLY for Clients
// constructed via the legacy NewClient seam (useGlobalRewriteRules =
// true). Clients built through NewClientWithOptions /
// NewDefaultClientWithOptions never consult package globals — nil
// rules on those paths mean "no rewrites", as documented on
// ClientOptions.RewriteRules.
func (c *Client) resolveRequestURL(req *Request) string {
	if req == nil {
		return ""
	}
	rules := c.rewriteRules
	if rules == nil && c.useGlobalRewriteRules {
		rules = urlrewrite.GlobalRules()
	}
	if len(rules) > 0 {
		return urlrewrite.ApplyToURL(req.URL, rules)
	}
	return req.URL
}

// buildHTTPRequest converts a Request into a standard *http.Request using a
// pre-rewritten URL. The dispatcher applies the rewrite before consulting
// the protocol cache, so backends receive the final URL unchanged.
func buildHTTPRequest(ctx context.Context, req *Request, rewrittenURL string) (*http.Request, error) {
	if req == nil {
		return nil, fmt.Errorf("llmhttp: nil request")
	}

	var body io.Reader
	if req.Body != "" {
		body = strings.NewReader(req.Body)
	}

	httpReq, err := http.NewRequestWithContext(ctx, req.Method, rewrittenURL, body)
	if err != nil {
		return nil, err
	}

	for k, v := range req.Headers {
		// net/http ignores Request.Header["Host"] on the wire — it sends
		// Request.Host instead, which defaults to the URL host. Route a
		// caller-supplied Host header there so both backends (net/http
		// and fasthttp) agree on the effective virtual host.
		if strings.EqualFold(k, "Host") {
			httpReq.Host = v
			continue
		}
		httpReq.Header.Set(k, v)
	}

	return httpReq, nil
}

// SensitiveHeaderKeys contains header name substrings that indicate sensitive
// values which should be redacted in logs and error messages. Matches are
// case-insensitive.
var SensitiveHeaderKeys = []string{
	"authorization",
	"cookie",
	"key",
	"secret",
	"token",
	"credential",
	"session",
	"auth",
}

// RedactHeaders returns a copy of headers with sensitive values replaced by
// "REDACTED". Use this when logging or including headers in error messages.
func RedactHeaders(headers map[string]string) map[string]string {
	redacted := make(map[string]string, len(headers))
	for k, v := range headers {
		lower := strings.ToLower(k)
		isSensitive := false
		for _, sensitive := range SensitiveHeaderKeys {
			if strings.Contains(lower, sensitive) {
				isSensitive = true
				break
			}
		}
		if isSensitive {
			redacted[k] = "REDACTED"
		} else {
			redacted[k] = v
		}
	}
	return redacted
}
