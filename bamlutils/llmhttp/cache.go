package llmhttp

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/valyala/fasthttp"
	"golang.org/x/sync/singleflight"
)

// clientMode selects how requests are dispatched between the net/http and
// fasthttp backends. Read once from BAML_REST_HTTP_CLIENT on Client
// construction.
type clientMode int

const (
	// modeAuto: per-origin ALPN probe on first request, then cache the
	// decision for the process lifetime.
	modeAuto clientMode = iota
	// modeFast: every request goes through fasthttp. The probe is skipped.
	// Callers who know their upstreams speak HTTP/1.1 avoid the cold-start
	// probe latency entirely.
	modeFast
	// modeNet: every request goes through net/http. Reproduces the behaviour
	// that shipped before the fasthttp backend existed; intended as an
	// emergency rollback switch.
	modeNet
)

// EnvVarClientMode is the environment variable that selects the client mode.
// Values: "auto" (default), "fasthttp", "nethttp". Unrecognised values fall
// back to "auto" so a typo never silently disables a backend.
const EnvVarClientMode = "BAML_REST_HTTP_CLIENT"

func loadClientMode() clientMode {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(EnvVarClientMode))) {
	case "fasthttp":
		return modeFast
	case "nethttp":
		return modeNet
	default:
		return modeAuto
	}
}

// decision is the cached routing outcome for a single origin. "unknown" is
// never stored — a cache entry exists only once a decision has been made.
type decision int

const (
	decisionNet decision = iota + 1
	decisionFast
)

// hostEntry is the cache payload for a single origin. host is populated only
// when decision == decisionFast; entries created for decisionNet never
// allocate a fasthttp.HostClient. Entries are treated as immutable after
// install so readers never take a lock.
type hostEntry struct {
	decision decision
	host     *fasthttp.HostClient
}

// protocolCache stores the routing decision (net/http vs fasthttp) per
// origin. Reads are lock-free via an atomic.Pointer to a copy-on-write map;
// writes take a short mutex around the atomic swap. A singleflight.Group
// coalesces concurrent first-requests to the same origin onto a single probe.
type protocolCache struct {
	mode        clientMode
	probeTLS    *tls.Config
	probeDialer *net.Dialer

	// proxyFunc, when non-nil, is consulted for https origins to pin
	// proxy-traffic to net/http (fasthttp has no ProxyFromEnvironment
	// equivalent). Default is http.ProxyFromEnvironment; tests can inject.
	proxyFunc func(*http.Request) (*url.URL, error)

	// probeTimeout bounds the ALPN probe handshake. Short by design — probe
	// failures fall back to net/http, and a hanging probe would stall every
	// first-hit caller via the singleflight barrier.
	probeTimeout time.Duration

	// fastClientTemplate seeds per-origin fasthttp.HostClient instances. The
	// relevant addr/TLS fields are overridden per origin at construction.
	fastClientTemplate *fasthttp.HostClient

	entries atomic.Pointer[map[string]*hostEntry]
	installMu sync.Mutex
	sf        singleflight.Group
}

// newProtocolCache constructs a cache configured for the given mode. tlsConf
// is mirrored into both the probe dialer and every per-origin HostClient so
// that private CAs / custom verification apply consistently across
// dispatched backends and the probe itself. A nil tlsConf is treated as the
// default {MinVersion: TLS12}. proxyFunc determines when origins are pinned
// to net/http for proxy traversal; a nil value disables proxy pinning
// (tests use this to force the ALPN probe path).
func newProtocolCache(mode clientMode, tlsConf *tls.Config, proxyFunc func(*http.Request) (*url.URL, error)) *protocolCache {
	if tlsConf == nil {
		tlsConf = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	c := &protocolCache{
		mode:         mode,
		probeTLS:     tlsConf,
		probeDialer:  &net.Dialer{Timeout: 2 * time.Second, KeepAlive: 30 * time.Second},
		proxyFunc:    proxyFunc,
		probeTimeout: 2 * time.Second,
		fastClientTemplate: &fasthttp.HostClient{
			Name:                          "baml-rest-llmhttp",
			DisableHeaderNamesNormalizing: true,
			StreamResponseBody:            true,
			MaxIdleConnDuration:           90 * time.Second,
			MaxConns:                      256,
			TLSConfig:                     tlsConf,
		},
	}
	empty := map[string]*hostEntry{}
	c.entries.Store(&empty)
	return c
}

// originURL holds the parsed form of a cache key. Carrying the parsed URL
// through the dispatcher avoids reparsing in the probe and backend code
// paths, which both need scheme/host/port.
type originURL struct {
	key      string // scheme://host:port — the cache key
	scheme   string
	hostname string
	port     string
}

// addr returns the "host:port" form that net.Dial and fasthttp.HostClient
// want, using net.JoinHostPort so IPv6 literals like "::1" come back as
// "[::1]:8080" rather than the ambiguous "::1:8080".
func (o originURL) addr() string { return net.JoinHostPort(o.hostname, o.port) }

// parseOrigin normalises a request URL into a cache key. The port is filled
// in from the scheme default when absent so that requests to e.g.
// "https://api.openai.com" and "https://api.openai.com:443" share a cache
// entry — otherwise the cache would fragment and probe the same origin
// twice. The key uses net.JoinHostPort for IPv6 bracket correctness.
func parseOrigin(rawURL string) (originURL, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return originURL{}, err
	}
	if u.Scheme == "" || u.Host == "" {
		return originURL{}, fmt.Errorf("llmhttp: url missing scheme or host: %q", rawURL)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return originURL{}, fmt.Errorf("llmhttp: unsupported scheme %q", u.Scheme)
	}
	host := u.Hostname()
	port := u.Port()
	if port == "" {
		if scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	return originURL{
		key:      scheme + "://" + net.JoinHostPort(host, port),
		scheme:   scheme,
		hostname: host,
		port:     port,
	}, nil
}

// resolve returns the routing decision for the given origin. On the hot
// path (entry already cached) it performs a single atomic load. On cold
// origin, a singleflight-gated probe populates the cache before returning.
//
// A cancelled ctx unblocks the caller early: the singleflight probe keeps
// running in the background so other waiters (and any subsequent request
// to the same origin) get a populated entry, but this caller returns nil
// so the dispatcher can fall back to net/http and let the request's own
// cancellation propagate. Returning nil here is the well-defined "we
// couldn't classify this origin in time" signal — the dispatcher treats
// it the same as a cache miss that hasn't resolved yet.
func (c *protocolCache) resolve(ctx context.Context, origin originURL) *hostEntry {
	if entry := c.lookup(origin.key); entry != nil {
		return entry
	}

	ch := c.sf.DoChan(origin.key, func() (any, error) {
		// Re-check after joining the flight: another caller may have
		// installed while we were queued behind the mutex in sf.Do.
		if entry := c.lookup(origin.key); entry != nil {
			return entry, nil
		}
		d := c.probe(origin)
		entry := c.buildEntry(origin, d)
		c.install(origin.key, entry)
		return entry, nil
	})
	select {
	case r := <-ch:
		return r.Val.(*hostEntry)
	case <-ctx.Done():
		return nil
	}
}

func (c *protocolCache) lookup(key string) *hostEntry {
	m := c.entries.Load()
	if m == nil {
		return nil
	}
	return (*m)[key]
}

// install publishes a new entry via copy-on-write. The mutex guards the
// "load → copy → swap" sequence so concurrent installs don't lose entries.
// Readers never observe the mutex.
func (c *protocolCache) install(key string, entry *hostEntry) {
	c.installMu.Lock()
	defer c.installMu.Unlock()
	old := c.entries.Load()
	next := make(map[string]*hostEntry, len(*old)+1)
	for k, v := range *old {
		next[k] = v
	}
	next[key] = entry
	c.entries.Store(&next)
}

// probe returns the routing decision for origin. Mode overrides short-
// circuit everything else; a configured proxy forces net/http for both
// http and https origins (fasthttp has no ProxyFromEnvironment); plain
// http:// without a proxy routes to fasthttp; otherwise an ALPN handshake
// decides. Probe failures default to net/http — safe because net/http
// handles both h1 and h2, so the request still succeeds; we just lose the
// fasthttp speedup until the process restarts.
//
// The TLS handshake runs with its own short timeout (probeTimeout). It
// is intentionally not ctx-aware: resolve() runs probe under singleflight
// and lets a cancelled ctx return early while the probe keeps running in
// the background to serve other waiters — tying the handshake timeout to
// the first caller's ctx would abort shared work on that caller's whim.
func (c *protocolCache) probe(origin originURL) decision {
	switch c.mode {
	case modeFast:
		return decisionFast
	case modeNet:
		return decisionNet
	}

	// Proxy check runs BEFORE the plain-http short-circuit: HTTP proxies
	// are commonly configured for http:// traffic too (outbound via a
	// corporate HTTP proxy, for example), and fasthttp would bypass the
	// proxy entirely by dialing the origin directly. Pinning to net/http
	// lets http.ProxyFromEnvironment (or the injected transport Proxy
	// func) route the request correctly.
	if c.proxyFunc != nil {
		req := &http.Request{
			URL:    &url.URL{Scheme: origin.scheme, Host: origin.addr()},
			Method: "GET",
			Host:   origin.addr(),
		}
		if p, err := c.proxyFunc(req); err == nil && p != nil {
			return decisionNet
		}
	}

	if origin.scheme == "http" {
		return decisionFast
	}

	cfg := c.probeTLS.Clone()
	cfg.ServerName = origin.hostname
	cfg.NextProtos = []string{"h2", "http/1.1"}

	probeCtx, cancel := context.WithTimeout(context.Background(), c.probeTimeout)
	defer cancel()
	dialer := &tls.Dialer{
		NetDialer: &net.Dialer{
			Timeout:   c.probeTimeout,
			KeepAlive: c.probeDialer.KeepAlive,
		},
		Config: cfg,
	}
	rawConn, err := dialer.DialContext(probeCtx, "tcp", origin.addr())
	if err != nil {
		return decisionNet
	}
	defer rawConn.Close()
	tlsConn, ok := rawConn.(*tls.Conn)
	if !ok {
		return decisionNet
	}
	if tlsConn.ConnectionState().NegotiatedProtocol == "h2" {
		return decisionNet
	}
	return decisionFast
}

// buildEntry constructs the immutable hostEntry that gets installed into
// the cache. The fasthttp.HostClient is allocated only when the decision is
// decisionFast — an origin pinned to net/http never pays the HostClient
// memory cost.
func (c *protocolCache) buildEntry(origin originURL, d decision) *hostEntry {
	if d != decisionFast {
		return &hostEntry{decision: d}
	}
	tmpl := c.fastClientTemplate
	hc := &fasthttp.HostClient{
		Name:                          tmpl.Name,
		Addr:                          origin.addr(),
		IsTLS:                         origin.scheme == "https",
		DisableHeaderNamesNormalizing: tmpl.DisableHeaderNamesNormalizing,
		StreamResponseBody:            tmpl.StreamResponseBody,
		MaxIdleConnDuration:           tmpl.MaxIdleConnDuration,
		MaxConns:                      tmpl.MaxConns,
		TLSConfig:                     tmpl.TLSConfig,
	}
	return &hostEntry{decision: d, host: hc}
}
