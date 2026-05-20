package llmhttp

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseOrigin(t *testing.T) {
	cases := []struct {
		in      string
		wantKey string
		wantErr bool
	}{
		{"http://host/path", "http://host:80", false},
		{"https://host/path", "https://host:443", false},
		{"http://host:8080/", "http://host:8080", false},
		{"https://host:8443/", "https://host:8443", false},
		// Hostnames are case-insensitive per RFC 3986 — the cache key
		// must lower-case them so "HOST" and "host" share one entry.
		{"http://HOST/", "http://host:80", false},
		{"HTTPS://host/", "https://host:443", false},
		{"http://Mixed.Case.example/", "http://mixed.case.example:80", false},
		// IPv6 literals must be bracketed in the key so the port delimiter
		// is unambiguous — `::1:8080` could mean host ::1 port 8080 or
		// host ::1:8080 with no port.
		{"http://[::1]/", "http://[::1]:80", false},
		{"https://[::1]:8443/", "https://[::1]:8443", false},
		{"ftp://host/", "", true},
		{"no-scheme", "", true},
		{"://bad", "", true},
	}
	for _, c := range cases {
		got, err := parseOrigin(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseOrigin(%q) expected error, got %q", c.in, got.key)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseOrigin(%q) unexpected error: %v", c.in, err)
			continue
		}
		if got.key != c.wantKey {
			t.Errorf("parseOrigin(%q) key = %q, want %q", c.in, got.key, c.wantKey)
		}
	}
}

func TestProtocolCacheMode_HTTPAlwaysFast(t *testing.T) {
	// Plain http:// must never probe — the cache decides "fast" immediately.
	// The fact that the probe TLS dialer points at nothing is sufficient
	// proof that we didn't attempt one.
	cache := newProtocolCache(modeAuto, FastHTTPClientOptions{}, nil)
	origin, err := parseOrigin("http://example.invalid/")
	if err != nil {
		t.Fatal(err)
	}
	entry := cache.resolve(context.Background(), origin)
	if entry.decision != decisionFast {
		t.Errorf("http:// expected decisionFast, got %d", entry.decision)
	}
	if entry.host == nil {
		t.Error("decisionFast should carry a fasthttp.HostClient")
	}
}

func TestProtocolCacheMode_OverrideFast(t *testing.T) {
	cache := newProtocolCache(modeFast, FastHTTPClientOptions{}, nil)
	origin, err := parseOrigin("https://api.openai.com/")
	if err != nil {
		t.Fatal(err)
	}
	entry := cache.resolve(context.Background(), origin)
	if entry.decision != decisionFast {
		t.Errorf("modeFast should pin decisionFast, got %d", entry.decision)
	}
	if entry.host == nil {
		t.Error("modeFast entry should carry a fasthttp.HostClient")
	}
}

func TestProtocolCacheMode_OverrideNet(t *testing.T) {
	cache := newProtocolCache(modeNet, FastHTTPClientOptions{}, nil)
	origin, err := parseOrigin("https://api.openai.com/")
	if err != nil {
		t.Fatal(err)
	}
	entry := cache.resolve(context.Background(), origin)
	if entry.decision != decisionNet {
		t.Errorf("modeNet should pin decisionNet, got %d", entry.decision)
	}
	if entry.host != nil {
		t.Error("modeNet entry must not allocate a HostClient")
	}
}

func TestProtocolCache_ProxyPinsNet(t *testing.T) {
	proxy := func(req *http.Request) (*url.URL, error) {
		return &url.URL{Scheme: "http", Host: "proxy.example:3128"}, nil
	}
	cache := newProtocolCache(modeAuto, FastHTTPClientOptions{}, proxy)
	origin, err := parseOrigin("https://api.openai.com/")
	if err != nil {
		t.Fatal(err)
	}
	entry := cache.resolve(context.Background(), origin)
	if entry.decision != decisionNet {
		t.Errorf("proxy-configured origin expected decisionNet, got %d", entry.decision)
	}
}

// TestProtocolCache_ProxyPinsNetForHTTP regression guards the fix for
// Codex HIGH 3: plain http:// used to short-circuit to fasthttp before
// the proxy check, which would bypass a configured HTTP proxy entirely.
func TestProtocolCache_ProxyPinsNetForHTTP(t *testing.T) {
	proxy := func(req *http.Request) (*url.URL, error) {
		return &url.URL{Scheme: "http", Host: "proxy.example:3128"}, nil
	}
	cache := newProtocolCache(modeAuto, FastHTTPClientOptions{}, proxy)
	origin, err := parseOrigin("http://internal.service/")
	if err != nil {
		t.Fatal(err)
	}
	entry := cache.resolve(context.Background(), origin)
	if entry.decision != decisionNet {
		t.Errorf("http:// with proxy expected decisionNet (so ProxyFromEnvironment applies via net/http), got %d", entry.decision)
	}
}

func TestProtocolCache_CacheHitIsLockFree(t *testing.T) {
	// Regression check: resolve() on an already-installed origin must not
	// go through singleflight or any mutex — hit the atomic map and return.
	// We approximate this by racing many resolvers and ensuring no data
	// races trip the race detector. The `go test -race` pass catches it.
	cache := newProtocolCache(modeFast, FastHTTPClientOptions{}, nil)
	origin, err := parseOrigin("http://host/")
	if err != nil {
		t.Fatal(err)
	}
	// Warm once.
	_ = cache.resolve(context.Background(), origin)

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 1000; j++ {
				_ = cache.resolve(context.Background(), origin)
			}
		}()
	}
	wg.Wait()
}

func TestProtocolCache_SingleflightDedupe(t *testing.T) {
	// Two concurrent first-hits to the same origin must share one probe.
	// We count probe invocations by stubbing the probe branch via a
	// counted proxyFunc + modeAuto on https — proxyFunc is consulted
	// inside probe(), so counting its calls counts probes.
	var probes atomic.Int32
	proxy := func(req *http.Request) (*url.URL, error) {
		probes.Add(1)
		return &url.URL{Scheme: "http", Host: "proxy:3128"}, nil
	}
	cache := newProtocolCache(modeAuto, FastHTTPClientOptions{}, proxy)
	origin, err := parseOrigin("https://host/")
	if err != nil {
		t.Fatal(err)
	}

	// Start barrier: without it, goroutines are spawned sequentially and
	// the probe path is fast enough (atomic increment + map CoW install)
	// that goroutine 1 may finish and populate the cache before
	// goroutine 2 is even scheduled. That would make the test pass on
	// sequential execution, not on singleflight dedup. Blocking all
	// spawned goroutines on startCh and closing it once they're all
	// parked forces a genuine race into resolve().
	var wg sync.WaitGroup
	var ready sync.WaitGroup
	startCh := make(chan struct{})
	for i := 0; i < 16; i++ {
		wg.Add(1)
		ready.Add(1)
		go func() {
			defer wg.Done()
			ready.Done()
			<-startCh
			_ = cache.resolve(context.Background(), origin)
		}()
	}
	ready.Wait()
	close(startCh)
	wg.Wait()

	if n := probes.Load(); n != 1 {
		t.Errorf("expected exactly one probe, got %d", n)
	}
}

func TestLoadClientMode(t *testing.T) {
	cases := map[string]clientMode{
		"":         modeAuto,
		"auto":     modeAuto,
		"AUTO":     modeAuto,
		"fasthttp": modeFast,
		"FastHTTP": modeFast,
		"nethttp":  modeNet,
		"NetHTTP":  modeNet,
		"garbage":  modeAuto, // unknown values fall back, don't silently pick wrong backend
	}
	for v, want := range cases {
		t.Run(v, func(t *testing.T) {
			t.Setenv(EnvVarClientMode, v)
			if got := loadClientMode(); got != want {
				t.Errorf("loadClientMode(%q) = %d, want %d", v, got, want)
			}
		})
	}
}

// TestBuildEntry_FastCarriesHost documents the invariant that a fast
// decision always ships with a non-nil HostClient (dispatcher nil-checks
// it), while a net decision never allocates one.
func TestBuildEntry_FastCarriesHost(t *testing.T) {
	cache := newProtocolCache(modeAuto, FastHTTPClientOptions{}, nil)
	origin, err := parseOrigin("https://x/")
	if err != nil {
		t.Fatalf("parseOrigin: %v", err)
	}

	fast := cache.buildEntry(origin, decisionFast)
	if fast.host == nil {
		t.Error("decisionFast entry must carry a HostClient")
	}
	if !fast.host.IsTLS {
		t.Error("https origin should produce IsTLS=true HostClient")
	}
	if fast.host.Addr != "x:443" {
		t.Errorf("https default port 443 expected in Addr, got %q", fast.host.Addr)
	}

	net := cache.buildEntry(origin, decisionNet)
	if net.host != nil {
		t.Error("decisionNet entry must not allocate a HostClient")
	}
}

// TestProbe_PreservesCallerServerName guards against the Auto-mode probe
// clobbering a caller-supplied TLSConfig.ServerName with the URL
// hostname. A caller that overrides SNI via
// FastHTTPClientOptions.TLSConfig.ServerName (e.g. to talk to an LLM
// gateway whose cert advertises a different name than the dial target)
// must see that ServerName reach the wire during the probe — otherwise
// the probe handshake would fail and the origin would be silently pinned
// to net/http, defeating the configured fasthttp tuning.
//
// The test captures the ClientHello SNI server-side via
// GetConfigForClient. With the fix, the wire SNI equals the caller's
// override. Without the fix, the probe would overwrite ServerName with
// "127.0.0.1" (the URL hostname), which Go's TLS stack strips before
// emitting SNI — so the buggy path observes an empty SNI on the wire.
func TestProbe_PreservesCallerServerName(t *testing.T) {
	const callerSNI = "custom.sni.example"

	sniCh := make(chan string, 1)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	server.TLS = &tls.Config{
		GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			select {
			case sniCh <- hello.ServerName:
			default:
			}
			return nil, nil
		},
	}
	server.StartTLS()
	defer server.Close()

	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}

	// InsecureSkipVerify so the probe handshake completes despite the
	// caller's ServerName not matching the test certificate's SAN — we
	// care about what SNI hits the wire, not about cert verification.
	tlsCfg := &tls.Config{
		ServerName:         callerSNI,
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS12,
	}
	cache := newProtocolCache(modeAuto, FastHTTPClientOptions{TLSConfig: tlsCfg}, nil)

	origin, err := parseOrigin("https://" + u.Host + "/")
	if err != nil {
		t.Fatalf("parseOrigin: %v", err)
	}
	if got := cache.resolve(context.Background(), origin); got == nil {
		t.Fatal("resolve returned nil")
	}

	select {
	case sni := <-sniCh:
		if sni != callerSNI {
			t.Errorf("probe sent SNI %q on the wire, want caller-supplied %q (URL hostname was %q)", sni, callerSNI, origin.hostname)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("probe did not reach the TLS server within 5s")
	}

	// The caller's TLSConfig itself must not be mutated by the probe —
	// the clone is what gets the NextProtos / ServerName tweaks.
	if tlsCfg.ServerName != callerSNI {
		t.Errorf("caller's TLSConfig.ServerName mutated to %q; probe must clone before edit", tlsCfg.ServerName)
	}
}
