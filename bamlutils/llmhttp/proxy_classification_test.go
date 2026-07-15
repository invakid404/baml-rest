package llmhttp

// Unit coverage for the de-BAML one-send shadow parity-decline rewrite/proxy
// classification (WouldRewriteOrProxy / httpClientProxiesURL).
//
// The proxy decision is EXACT for exactly one resolver: this package's tuned
// defaultLLMTransport, whose Proxy is http.ProxyFromEnvironment — a resolver known
// to inspect nothing but the request URL. It is evaluated against the effective
// target via the transport's OWN cached resolver (no independent env parsing, so no
// selector/trim mismatch — including Unicode-whitespace values — and no divergence
// from the resolver's cached snapshot). Any OTHER non-nil Proxy resolver may inspect
// headers/context/other request fields a URL-only preflight cannot faithfully
// populate, so it FAILS CLOSED and is never even consulted; an explicit nil Proxy is
// the documented never-proxy shape. These tests pin every branch.

import (
	"net/http"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils/urlrewrite"
)

// clientWithTransport builds an options-constructed Client over an *http.Client
// with the given transport (no global rewrite rules, no rewrite config).
func clientWithTransport(t *testing.T, tr http.RoundTripper) *Client {
	t.Helper()
	return NewClientWithOptions(ClientOptions{NetHTTPClient: &http.Client{Transport: tr}})
}

// recordingResolver counts how many times it is consulted; it never proxies.
type recordingResolver struct{ n atomic.Int64 }

func (r *recordingResolver) resolve(*http.Request) (*url.URL, error) {
	r.n.Add(1)
	return nil, nil
}

const httpsTarget = "https://api.openai.com/v1/chat/completions"
const loopbackTarget = "http://127.0.0.1:8081/v1/chat/completions"

// TestWouldRewriteOrProxy_DefaultTransportReachesForDirectTarget pins the real
// production constructor (NewDefaultClientWithOptions → defaultLLMTransport, whose
// Proxy is http.ProxyFromEnvironment): its send to a loopback target is provably
// direct (Go's env resolver ALWAYS bypasses loopback, regardless of proxy env), so
// the transport IS evaluated (not blanket-declined) and the comparison reaches. This
// is deterministic — it depends on neither the ambient env nor the resolver cache.
func TestWouldRewriteOrProxy_DefaultTransportReachesForDirectTarget(t *testing.T) {
	c := NewDefaultClientWithOptions(ClientOptions{})
	if c.WouldRewriteOrProxy(loopbackTarget) {
		t.Fatal("the default transport must reach comparison for a provably-direct (loopback) target")
	}
}

func TestWouldRewriteOrProxy_NilProxyTransportIsProxyFree(t *testing.T) {
	// An explicit nil Proxy is the documented never-proxy shape — even on a custom
	// transport it is provably direct, so it reaches comparison.
	c := clientWithTransport(t, &http.Transport{Proxy: nil})
	if c.WouldRewriteOrProxy(httpsTarget) {
		t.Fatal("an explicit nil-Proxy transport is the documented never-proxy shape; must be proxy-free")
	}
}

// TestWouldRewriteOrProxy_CustomResolverFailsClosedWithoutConsulting is the core of
// the URL-only-preflight fix: a caller-supplied Proxy resolver — which may inspect
// headers, context, or other request fields a bare URL preflight cannot populate —
// FAILS CLOSED, and is NEVER consulted (so no arbitrary callback runs against a
// synthesized request, and no secret-bearing request is fed to it). This holds even
// though the resolver here would return "direct" — a custom resolver's URL-only
// verdict is simply not trusted.
func TestWouldRewriteOrProxy_CustomResolverFailsClosedWithoutConsulting(t *testing.T) {
	rec := &recordingResolver{}
	c := clientWithTransport(t, &http.Transport{Proxy: rec.resolve})
	if !c.WouldRewriteOrProxy(httpsTarget) {
		t.Fatal("a caller-supplied Proxy resolver must FAIL CLOSED (would-proxy)")
	}
	if got := rec.n.Load(); got != 0 {
		t.Fatalf("a custom resolver must never be consulted by the URL-only preflight, got %d call(s)", got)
	}
}

func TestWouldRewriteOrProxy_NonTransportRoundTripperFailsClosed(t *testing.T) {
	c := clientWithTransport(t, roundTripperFunc(func(*http.Request) (*http.Response, error) { return nil, nil }))
	if !c.WouldRewriteOrProxy(httpsTarget) {
		t.Fatal("a non-*http.Transport RoundTripper has unknowable proxy behaviour; must FAIL CLOSED")
	}
}

// TestWouldRewriteOrProxy_TypedNilTransportFailsClosed guards a typed-nil
// (*http.Transport)(nil): the interface is non-nil so the type assertion succeeds
// with a nil pointer — reading t.Proxy would panic. The classification must instead
// treat it as uninspectable and FAIL CLOSED (no panic).
func TestWouldRewriteOrProxy_TypedNilTransportFailsClosed(t *testing.T) {
	c := clientWithTransport(t, (*http.Transport)(nil))
	if !c.WouldRewriteOrProxy(httpsTarget) {
		t.Fatal("a typed-nil *http.Transport is uninspectable; must FAIL CLOSED (and never panic)")
	}
}

func TestWouldRewriteOrProxy_RewriteRuleAlwaysDeclines(t *testing.T) {
	// A present rewrite rule is a would-rewrite regardless of the proxy verdict.
	c := NewClientWithOptions(ClientOptions{
		NetHTTPClient: &http.Client{Transport: &http.Transport{Proxy: nil}},
		RewriteRules:  []urlrewrite.Rule{{From: "https://a.invalid", To: "https://b.invalid"}},
	})
	if !c.WouldRewriteOrProxy(httpsTarget) {
		t.Fatal("a client carrying a rewrite rule must report would-rewrite")
	}
}

func TestWouldRewriteOrProxy_NilClientFailsClosed(t *testing.T) {
	// A nil-bound public method value must FAIL CLOSED: an unknowable client's
	// rewrite config and transport cannot be proven proxy-free, so it must decline
	// rather than reach comparison (consistent with the fail-closed contract).
	var c *Client
	if !c.WouldRewriteOrProxy(httpsTarget) {
		t.Fatal("a nil Client must FAIL CLOSED (would-rewrite/proxy true), not be classified proxy-free")
	}
}
