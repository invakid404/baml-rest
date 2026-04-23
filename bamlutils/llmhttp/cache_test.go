package llmhttp

import (
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
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
		{"http://HOST/", "http://HOST:80", false}, // Hostname() preserves case; scheme normalised
		{"HTTPS://host/", "https://host:443", false},
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
	cache := newProtocolCache(modeAuto, nil)
	origin, err := parseOrigin("http://example.invalid/")
	if err != nil {
		t.Fatal(err)
	}
	entry := cache.resolve(origin)
	if entry.decision != decisionFast {
		t.Errorf("http:// expected decisionFast, got %d", entry.decision)
	}
	if entry.host == nil {
		t.Error("decisionFast should carry a fasthttp.HostClient")
	}
}

func TestProtocolCacheMode_OverrideFast(t *testing.T) {
	cache := newProtocolCache(modeFast, nil)
	origin, err := parseOrigin("https://api.openai.com/")
	if err != nil {
		t.Fatal(err)
	}
	entry := cache.resolve(origin)
	if entry.decision != decisionFast {
		t.Errorf("modeFast should pin decisionFast, got %d", entry.decision)
	}
	if entry.host == nil {
		t.Error("modeFast entry should carry a fasthttp.HostClient")
	}
}

func TestProtocolCacheMode_OverrideNet(t *testing.T) {
	cache := newProtocolCache(modeNet, nil)
	origin, err := parseOrigin("https://api.openai.com/")
	if err != nil {
		t.Fatal(err)
	}
	entry := cache.resolve(origin)
	if entry.decision != decisionNet {
		t.Errorf("modeNet should pin decisionNet, got %d", entry.decision)
	}
	if entry.host != nil {
		t.Error("modeNet entry must not allocate a HostClient")
	}
}

func TestProtocolCache_ProxyPinsNet(t *testing.T) {
	cache := newProtocolCache(modeAuto, nil)
	cache.proxyFunc = func(req *http.Request) (*url.URL, error) {
		return &url.URL{Scheme: "http", Host: "proxy.example:3128"}, nil
	}
	origin, err := parseOrigin("https://api.openai.com/")
	if err != nil {
		t.Fatal(err)
	}
	entry := cache.resolve(origin)
	if entry.decision != decisionNet {
		t.Errorf("proxy-configured origin expected decisionNet, got %d", entry.decision)
	}
}

func TestProtocolCache_CacheHitIsLockFree(t *testing.T) {
	// Regression check: resolve() on an already-installed origin must not
	// go through singleflight or any mutex — hit the atomic map and return.
	// We approximate this by racing many resolvers and ensuring no data
	// races trip the race detector. The `go test -race` pass catches it.
	cache := newProtocolCache(modeFast, nil)
	origin, err := parseOrigin("http://host/")
	if err != nil {
		t.Fatal(err)
	}
	// Warm once.
	_ = cache.resolve(origin)

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 1000; j++ {
				_ = cache.resolve(origin)
			}
		}()
	}
	wg.Wait()
}

func TestProtocolCache_SingleflightDedupe(t *testing.T) {
	// Two concurrent first-hits to the same origin must share one probe.
	// We count probe invocations by stubbing the probe branch via a
	// cache with a counted proxyFunc + modeAuto on https — proxyFunc is
	// consulted inside probe(), so counting its calls counts probes.
	cache := newProtocolCache(modeAuto, nil)
	var probes atomic.Int32
	cache.proxyFunc = func(req *http.Request) (*url.URL, error) {
		probes.Add(1)
		return &url.URL{Scheme: "http", Host: "proxy:3128"}, nil
	}
	origin, err := parseOrigin("https://host/")
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = cache.resolve(origin)
		}()
	}
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
	cache := newProtocolCache(modeAuto, nil)
	origin, _ := parseOrigin("https://x/")

	fast := cache.buildEntry(origin, decisionFast)
	if fast.host == nil {
		t.Error("decisionFast entry must carry a HostClient")
	}
	if !fast.host.IsTLS {
		t.Error("https origin should produce IsTLS=true HostClient")
	}
	if !strings.Contains(fast.host.Addr, ":443") {
		t.Errorf("https default port 443 expected in Addr, got %q", fast.host.Addr)
	}

	net := cache.buildEntry(origin, decisionNet)
	if net.host != nil {
		t.Error("decisionNet entry must not allocate a HostClient")
	}
}
