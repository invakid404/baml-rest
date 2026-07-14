//go:build integration && nanollm_integration

package dynamic

// De-BAML cutover Slice 4 END-TO-END one-send shadow proof, through the REAL
// generated dynamic call seam (dynclient + patched BAML + the nanollm-backed
// shadow comparator). It EXTENDS this package's no-send prepared-request oracle
// and live-differential harness (see prepared_request_integration_test.go /
// live_differential_integration_test.go, whose fixtures + loopback capture server
// it reuses) to prove the serving-path behaviour the unit suites cannot:
//
//   - FLAG ON: for an admitted `_dynamic` /call the generated seam installs the
//     Slice-1 native child-attempt callback, which runs the shadow comparator —
//     it builds the native plan, compares it against BAML's built plan WITHOUT a
//     socket, records plan_compare, and DECLINES. BAML then serves the request
//     with EXACTLY ONE provider request (the comparator opens zero sockets), and
//     the user-facing structured output is BAML's. Every compared field matches.
//   - FLAG OFF: the seam gate (DeBAMLConfig().Enabled) keeps the callback nil, so
//     the comparator is NEVER invoked — ZERO native FFI, ZERO native sockets, ZERO
//     plan build — and BAML serves normally.
//
// Native NEVER RoundTrips on either path (asserted via the comparator's own exact
// executor counting transport AND the single loopback provider request).
//
// Gated `integration && nanollm_integration` (BAML CFFI + nanollm); nothing here
// is reachable from production routing.

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/dynclient"
	"github.com/invakid404/baml-rest/internal/debaml"
	"github.com/invakid404/baml-rest/internal/nativebody/nanollmprepare/admission"
	"github.com/invakid404/baml-rest/internal/nativebody/nanollmprepare/shadow"
)

// shadowExecCounter is an http.RoundTripper that COUNTS RoundTrips without
// dialing. The shadow comparator's exact executor is built over it; the whole
// comparison must leave it at zero (the no-send predicate opens no socket).
type shadowExecCounter struct{ n atomic.Int64 }

func (c *shadowExecCounter) RoundTrip(req *http.Request) (*http.Response, error) {
	c.n.Add(1)
	if req.Body != nil {
		_, _ = io.Copy(io.Discard, req.Body)
		_ = req.Body.Close()
	}
	return &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{}`)),
		Request:    req,
	}, nil
}

// shadowSpy wraps the real nanollm-backed shadow comparator, counting how many
// times the generated seam invokes it and exposing the private registry so the
// test can read plan_compare.
type shadowSpy struct {
	fn    bamlutils.NativeShadowFunc
	calls atomic.Int64
	reg   *prometheus.Registry
	exec  *shadowExecCounter
}

func (s *shadowSpy) Compare(ctx context.Context, req bamlutils.NativeShadowRequest) bamlutils.NativeShadowResult {
	s.calls.Add(1)
	return s.fn(ctx, req)
}

func newShadowSpy(t *testing.T) *shadowSpy {
	t.Helper()
	reg := prometheus.NewRegistry()
	m, err := admission.NewMetrics(reg)
	if err != nil {
		t.Fatalf("admission.NewMetrics: %v", err)
	}
	exec := &shadowExecCounter{}
	c := shadow.NewComparator(m, llmhttp.NewExactExecutor(exec))
	return &shadowSpy{fn: c.Compare, reg: reg, exec: exec}
}

// sumPlanCompare sums the plan_compare counter across all fields for one result
// label ("match" / "mismatch").
func (s *shadowSpy) sumPlanCompare(t *testing.T, result string) float64 {
	t.Helper()
	fams, err := s.reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var sum float64
	for _, mf := range fams {
		if mf.GetName() != "baml_rest_debaml_plan_compare_total" {
			continue
		}
		for _, mm := range mf.GetMetric() {
			for _, lp := range mm.GetLabel() {
				if lp.GetName() == "result" && lp.GetValue() == result {
					sum += mm.GetCounter().GetValue()
				}
			}
		}
	}
	return sum
}

// sumDeclines sums the S3 admission declines counter for one (stage, reason) enum
// pair, so a control can assert the comparator declined for the SPECIFIC parity
// reason (retry override / rewrite-proxy) rather than merely "declined somehow".
func (s *shadowSpy) sumDeclines(t *testing.T, stage, reason string) float64 {
	t.Helper()
	fams, err := s.reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var sum float64
	for _, mf := range fams {
		if mf.GetName() != "baml_rest_debaml_declines_total" {
			continue
		}
		for _, mm := range mf.GetMetric() {
			var gotStage, gotReason string
			for _, lp := range mm.GetLabel() {
				switch lp.GetName() {
				case "stage":
					gotStage = lp.GetValue()
				case "reason":
					gotReason = lp.GetValue()
				}
			}
			if gotStage == stage && gotReason == reason {
				sum += mm.GetCounter().GetValue()
			}
		}
	}
	return sum
}

// proxyRecorder is a caller-supplied http.Transport.Proxy resolver that records
// the host of every request it is consulted for and never actually proxies. The
// de-BAML shadow-fact evaluation must never consult it for a synthesized/foreign
// host (the removed hard-coded api.openai.com probe), and must not consult it AT
// ALL before the flag/comparator gates — so on the flag-off path the only hosts it
// ever sees are the genuine loopback send target.
type proxyRecorder struct {
	mu    sync.Mutex
	hosts []string
}

func (p *proxyRecorder) resolve(req *http.Request) (*url.URL, error) {
	p.mu.Lock()
	p.hosts = append(p.hosts, req.URL.Hostname())
	p.mu.Unlock()
	return nil, nil
}

// foreignHostCalls returns the non-loopback hosts the resolver was consulted for.
// A genuine BAML send only ever targets the loopback capture server, so any
// non-loopback host is a shadow-attributable proxy evaluation that must not exist.
func (p *proxyRecorder) foreignHostCalls() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for _, h := range p.hosts {
		if !isLoopbackLiveHost(h) {
			out = append(out, h)
		}
	}
	return out
}

func runShadowDynamicCall(t *testing.T, spy *shadowSpy, deBAMLEnabled bool) (*liveCaptureServer, *dynclient.CallResult) {
	t.Helper()
	fx := dynFixtureByName(t, "single_user_message")
	server := newLiveCaptureServer(t)
	server.setResponse(http.StatusOK, openAISuccess(`{"answer":"ok"}`))

	client, err := dynclient.New(
		dynclient.WithClientMode(llmhttp.ClientModeNetHTTP),
		dynclient.WithNetHTTPClient(loopbackOracleHTTPClient()),
		dynclient.WithDeBAML(deBAMLEnabled),
		dynclient.WithDeBAMLRenderer(debaml.Render),
		dynclient.WithNativeShadowComparator(spy.Compare),
	)
	if err != nil {
		t.Fatalf("dynclient.New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), liveCallTimeout)
	defer cancel()
	res, err := client.DynamicCall(ctx, dynclient.Request{
		Messages:            toDynMessages(fx.messages),
		ClientRegistry:      liveOracleRegistry(server.base()),
		OutputSchema:        fx.schema,
		PreserveSchemaOrder: bptr(true),
	})
	if err != nil {
		t.Fatalf("DynamicCall failed on an admitted fixture: %v", err)
	}
	return server, res
}

// TestShadowServe_FlagOnComparesAndServesBAML proves the flag-on shadow path:
// the comparator runs once, opens zero sockets, records an all-match
// plan_compare, and BAML serves the request with exactly one provider request.
func TestShadowServe_FlagOnComparesAndServesBAML(t *testing.T) {
	spy := newShadowSpy(t)
	server, res := runShadowDynamicCall(t, spy, true)

	// Exactly one provider request — BAML's send. Native never sent.
	if got := server.count(); got != 1 {
		t.Fatalf("provider saw %d requests, want exactly 1 (BAML serves; native never sends)", got)
	}
	// The comparator ran once (one admitted child) and opened no socket.
	if got := spy.calls.Load(); got != 1 {
		t.Fatalf("shadow comparator invoked %d times, want exactly 1", got)
	}
	if got := spy.exec.n.Load(); got != 0 {
		t.Fatalf("shadow comparator opened %d socket(s); the no-send comparator must open zero", got)
	}
	// Every compared field matched (native plan == BAML plan through the real
	// seam) and nothing mismatched.
	if got := spy.sumPlanCompare(t, "mismatch"); got != 0 {
		t.Errorf("plan_compare mismatches = %v, want 0 (zero-tolerance)", got)
	}
	if got := spy.sumPlanCompare(t, "match"); got != 5 {
		t.Errorf("plan_compare matches = %v, want 5 (method/target/host/headers/body)", got)
	}
	// The user-facing structured output is BAML's, byte-identical to the served
	// answer.
	if !liveJSONSemEqual(t, res.Data, []byte(`{"answer":"ok"}`)) {
		t.Errorf("served structured output = %s, want {\"answer\":\"ok\"}", liveBodyDigest(res.Data))
	}
}

// TestShadowServe_URLRewriteDeclinesBeforeCompare proves the truthful
// rewrite/proxy parity-decline through the REAL generated seam: when the
// effective send client carries a URL rewrite rule, the installed shadow callback
// runs but the comparator declines at the strategy gate BEFORE building either
// plan — so NO plan_compare series is recorded and BAML's plan builder is never
// invoked — while BAML still serves the request exactly once. The rule is
// deliberately non-matching so BAML's actual send is unaffected; its mere presence
// is what WouldRewriteOrProxy reports (rewrite detection is conservative — any
// configured rule declines, since the exact lane performs no rewrite).
func TestShadowServe_URLRewriteDeclinesBeforeCompare(t *testing.T) {
	spy := newShadowSpy(t)
	fx := dynFixtureByName(t, "single_user_message")
	server := newLiveCaptureServer(t)
	server.setResponse(http.StatusOK, openAISuccess(`{"answer":"ok"}`))

	client, err := dynclient.New(
		dynclient.WithClientMode(llmhttp.ClientModeNetHTTP),
		dynclient.WithNetHTTPClient(loopbackOracleHTTPClient()),
		dynclient.WithDeBAML(true),
		dynclient.WithDeBAMLRenderer(debaml.Render),
		dynclient.WithNativeShadowComparator(spy.Compare),
		// A present-but-non-matching rewrite rule: WouldRewriteOrProxy reports
		// true (rules are installed), yet it never fires on the loopback URL, so
		// BAML's actual send stays on the capture server.
		dynclient.WithBaseURLRewrites([]dynclient.BaseURLRewriteRule{
			{From: "https://never-matches.invalid", To: "https://also-never.invalid"},
		}),
	)
	if err != nil {
		t.Fatalf("dynclient.New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), liveCallTimeout)
	defer cancel()
	res, err := client.DynamicCall(ctx, dynclient.Request{
		Messages:            toDynMessages(fx.messages),
		ClientRegistry:      liveOracleRegistry(server.base()),
		OutputSchema:        fx.schema,
		PreserveSchemaOrder: bptr(true),
	})
	if err != nil {
		t.Fatalf("DynamicCall failed on an admitted fixture: %v", err)
	}

	// BAML served exactly once; the comparator ran once but declined at the
	// strategy gate before building any plan.
	if got := server.count(); got != 1 {
		t.Fatalf("provider saw %d requests, want exactly 1 (BAML serves)", got)
	}
	if got := spy.calls.Load(); got != 1 {
		t.Fatalf("shadow comparator invoked %d times, want exactly 1", got)
	}
	if got := spy.exec.n.Load(); got != 0 {
		t.Fatalf("shadow comparator opened %d socket(s); the no-send comparator must open zero", got)
	}
	// The rewrite/proxy parity-decline fires BEFORE any plan build: zero series.
	if got := spy.sumPlanCompare(t, "match") + spy.sumPlanCompare(t, "mismatch"); got != 0 {
		t.Errorf("rewrite decline recorded %v plan_compare series, want 0 (no native/BAML plan build)", got)
	}
	if got := spy.sumDeclines(t, string(admission.StageStrategy), string(admission.ReasonURLRewriteOrProxy)); got != 1 {
		t.Errorf("strategy rewrite/proxy declines = %v, want 1 (rewrite fact propagated)", got)
	}
	if !liveJSONSemEqual(t, res.Data, []byte(`{"answer":"ok"}`)) {
		t.Errorf("served structured output = %s, want {\"answer\":\"ok\"}", liveBodyDigest(res.Data))
	}
}

// TestShadowServe_FlagOffZeroNative proves the kill switch: with the umbrella
// flag off the generated seam installs no native callback, so the comparator is
// never invoked (zero native FFI / socket / plan build) and BAML serves normally.
func TestShadowServe_FlagOffZeroNative(t *testing.T) {
	spy := newShadowSpy(t)
	server, res := runShadowDynamicCall(t, spy, false)

	if got := server.count(); got != 1 {
		t.Fatalf("provider saw %d requests, want exactly 1 (BAML serves)", got)
	}
	// Zero native everything: the comparator is never invoked, so no admission /
	// nanollm FFI, no plan build, and no exact-executor socket occurs.
	if got := spy.calls.Load(); got != 0 {
		t.Fatalf("flag-off must NOT invoke the shadow comparator, got %d calls (zero-FFI/plan-build violated)", got)
	}
	if got := spy.exec.n.Load(); got != 0 {
		t.Fatalf("flag-off opened %d native socket(s), want 0", got)
	}
	if got := spy.sumPlanCompare(t, "match") + spy.sumPlanCompare(t, "mismatch"); got != 0 {
		t.Errorf("flag-off recorded %v plan_compare series, want 0 (zero plan build)", got)
	}
	if !liveJSONSemEqual(t, res.Data, []byte(`{"answer":"ok"}`)) {
		t.Errorf("served structured output = %s, want {\"answer\":\"ok\"}", liveBodyDigest(res.Data))
	}
}

// TestShadowServe_RetryOverrideDeclinesBeforeCompare proves the truthful
// request-retry-override parity-decline through the REAL generated seam, the retry
// twin of TestShadowServe_URLRewriteDeclinesBeforeCompare. The request carries a
// resolved retry policy (delivered through __baml_options__.retry via
// WithRequestRetryOverride — the same seam a REST caller uses), so the generated
// dynamic call resolves cfg.RetryPolicy != nil and the installed shadow callback
// reports HasRequestRetryOverride to the comparator. A request retry override is a
// strategy the single-attempt exact lane would bypass, so the comparator declines
// at the S3 strategy gate BEFORE building either plan: BuildBAMLRequest is never
// invoked (proved by zero plan_compare series — neither a compare result nor a meta
// build-failure) and the decline is recorded under the retry-override stage/reason.
// BAML still serves exactly once; the constant 1ms/2-retry policy never fires on
// the loopback success, so BAML's actual send is a single request.
func TestShadowServe_RetryOverrideDeclinesBeforeCompare(t *testing.T) {
	spy := newShadowSpy(t)
	fx := dynFixtureByName(t, "single_user_message")
	server := newLiveCaptureServer(t)
	server.setResponse(http.StatusOK, openAISuccess(`{"answer":"ok"}`))

	client, err := dynclient.New(
		dynclient.WithClientMode(llmhttp.ClientModeNetHTTP),
		dynclient.WithNetHTTPClient(loopbackOracleHTTPClient()),
		dynclient.WithDeBAML(true),
		dynclient.WithDeBAMLRenderer(debaml.Render),
		dynclient.WithNativeShadowComparator(spy.Compare),
		// A resolved request retry policy: the strategy-aware resolver returns it as
		// cfg.RetryPolicy != nil, which the generated seam reports as
		// HasRequestRetryOverride. It never fires on the loopback success, so BAML's
		// actual send stays a single request (mirroring the rewrite control's
		// present-but-non-matching rule).
		dynclient.WithRequestRetryOverride(&dynclient.RetryConfig{
			MaxRetries: 2,
			Strategy:   "constant_delay",
			DelayMs:    1,
		}),
	)
	if err != nil {
		t.Fatalf("dynclient.New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), liveCallTimeout)
	defer cancel()
	res, err := client.DynamicCall(ctx, dynclient.Request{
		Messages:            toDynMessages(fx.messages),
		ClientRegistry:      liveOracleRegistry(server.base()),
		OutputSchema:        fx.schema,
		PreserveSchemaOrder: bptr(true),
	})
	if err != nil {
		t.Fatalf("DynamicCall failed on an admitted fixture: %v", err)
	}

	// BAML served exactly once; the comparator ran once but declined before build.
	if got := server.count(); got != 1 {
		t.Fatalf("provider saw %d requests, want exactly 1 (BAML serves)", got)
	}
	if got := spy.calls.Load(); got != 1 {
		t.Fatalf("shadow comparator invoked %d times, want exactly 1", got)
	}
	if got := spy.exec.n.Load(); got != 0 {
		t.Fatalf("shadow comparator opened %d socket(s); the no-send comparator must open zero", got)
	}
	// The retry-override parity-decline fires BEFORE any plan build: BuildBAMLRequest
	// is never invoked, so zero plan_compare series (neither compare nor meta).
	if got := spy.sumPlanCompare(t, "match") + spy.sumPlanCompare(t, "mismatch"); got != 0 {
		t.Errorf("retry-override decline recorded %v plan_compare series, want 0 (no native/BAML plan build)", got)
	}
	// And it declined for the RIGHT reason: the S3 strategy retry-override gate.
	if got := spy.sumDeclines(t, string(admission.StageStrategy), string(admission.ReasonRequestRetryOverride)); got != 1 {
		t.Errorf("strategy retry-override declines = %v, want 1 (cfg.RetryPolicy != nil propagated)", got)
	}
	if !liveJSONSemEqual(t, res.Data, []byte(`{"answer":"ok"}`)) {
		t.Errorf("served structured output = %s, want {\"answer\":\"ok\"}", liveBodyDigest(res.Data))
	}
}

// TestShadowServe_CustomProxyResolverFailsClosedWithoutConsulting proves the
// URL-only-preflight restriction through the REAL generated seam: when the effective
// send client carries a CALLER-SUPPLIED Proxy resolver (not the tuned default
// transport's URL-only http.ProxyFromEnvironment), the classification cannot prove
// its send-time decision from a bare URL preflight — the resolver could inspect
// headers/context/other request fields — so it FAILS CLOSED and declines at the
// strategy gate, and the resolver is NEVER consulted (no arbitrary callback runs
// against a synthesized request). BAML still serves the request exactly once; the
// loopback guard keeps the actual send on the capture server.
func TestShadowServe_CustomProxyResolverFailsClosedWithoutConsulting(t *testing.T) {
	spy := newShadowSpy(t)
	rec := &proxyRecorder{}
	fx := dynFixtureByName(t, "single_user_message")
	server := newLiveCaptureServer(t)
	server.setResponse(http.StatusOK, openAISuccess(`{"answer":"ok"}`))

	// A caller-supplied resolver that would NOT proxy (returns nil), recording every
	// host it is asked about. It must never be consulted by the parity classification.
	proxyClient := &http.Client{Transport: &http.Transport{
		Proxy:       rec.resolve,
		DialContext: loopbackDial,
	}}

	client, err := dynclient.New(
		dynclient.WithClientMode(llmhttp.ClientModeNetHTTP),
		dynclient.WithNetHTTPClient(proxyClient),
		dynclient.WithDeBAML(true),
		dynclient.WithDeBAMLRenderer(debaml.Render),
		dynclient.WithNativeShadowComparator(spy.Compare),
	)
	if err != nil {
		t.Fatalf("dynclient.New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), liveCallTimeout)
	defer cancel()
	res, err := client.DynamicCall(ctx, dynclient.Request{
		Messages:            toDynMessages(fx.messages),
		ClientRegistry:      liveOracleRegistry(server.base()),
		OutputSchema:        fx.schema,
		PreserveSchemaOrder: bptr(true),
	})
	if err != nil {
		t.Fatalf("DynamicCall failed on an admitted fixture: %v", err)
	}

	if got := server.count(); got != 1 {
		t.Fatalf("provider saw %d requests, want exactly 1 (BAML serves)", got)
	}
	if got := spy.calls.Load(); got != 1 {
		t.Fatalf("shadow comparator invoked %d times, want exactly 1", got)
	}
	if got := spy.exec.n.Load(); got != 0 {
		t.Fatalf("shadow comparator opened %d socket(s); the no-send comparator must open zero", got)
	}
	// A caller-supplied resolver fails closed BEFORE any plan is built — no
	// plan_compare series, and it declines for the rewrite/proxy reason.
	if got := spy.sumPlanCompare(t, "match") + spy.sumPlanCompare(t, "mismatch"); got != 0 {
		t.Errorf("custom-resolver decline recorded %v plan_compare series, want 0 (no native/BAML plan build)", got)
	}
	if got := spy.sumDeclines(t, string(admission.StageStrategy), string(admission.ReasonURLRewriteOrProxy)); got != 1 {
		t.Errorf("strategy rewrite/proxy declines = %v, want 1 (custom resolver fails closed)", got)
	}
	// The parity classification never probes a FOREIGN host: only BAML's genuine
	// loopback send may touch the resolver (the URL-only preflight fails closed
	// without invoking it — proven precisely in the llmhttp unit suite). This guards
	// against re-introducing a fixed-origin (api.openai.com) probe.
	if hosts := rec.foreignHostCalls(); len(hosts) != 0 {
		t.Errorf("classification probed foreign host(s) %v, want none", hosts)
	}
	if !liveJSONSemEqual(t, res.Data, []byte(`{"answer":"ok"}`)) {
		t.Errorf("served structured output = %s, want {\"answer\":\"ok\"}", liveBodyDigest(res.Data))
	}
}

// TestShadowServe_DefaultClientNoProxyReachesCompare is the production-fidelity
// regression guard for the "flag-on shadow worker is inert" P1. It does NOT inject
// a custom net/http client, so dynclient builds the EXACT client the real shadow
// worker uses — llmhttp.NewDefaultClientWithOptions over the tuned
// defaultLLMTransport, whose Proxy is http.ProxyFromEnvironment. The effective
// target is the loopback capture server, which Go's env resolver ALWAYS bypasses
// (loopback is never proxied, regardless of proxy env), so WouldRewriteOrProxy
// evaluates that exact target through the transport's own resolver and reports
// proxy-free — the shadow callback REACHES the native-vs-BAML plan comparison
// instead of declining. Before the URL-aware fix, ANY non-nil Transport.Proxy
// (including the default ProxyFromEnvironment) recorded ZERO plan_compare series
// and a url_rewrite_or_proxy decline on every request — the whole shadow profile
// was functionally inert; the earlier positive control masked it by injecting a
// Proxy:nil loopback client the production constructor never uses.
func TestShadowServe_DefaultClientNoProxyReachesCompare(t *testing.T) {
	// Pin a proxy-free environment for good measure; the loopback target is bypassed
	// regardless, so the reach-comparison outcome does not depend on the ambient env.
	for _, k := range []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy"} {
		t.Setenv(k, "")
	}

	spy := newShadowSpy(t)
	fx := dynFixtureByName(t, "single_user_message")
	server := newLiveCaptureServer(t)
	server.setResponse(http.StatusOK, openAISuccess(`{"answer":"ok"}`))

	client, err := dynclient.New(
		dynclient.WithClientMode(llmhttp.ClientModeNetHTTP),
		// NO WithNetHTTPClient: dynclient builds the PRODUCTION client via
		// llmhttp.NewDefaultClientWithOptions (defaultLLMTransport, Proxy:
		// http.ProxyFromEnvironment) — exactly what internal/workerboot does.
		dynclient.WithDeBAML(true),
		dynclient.WithDeBAMLRenderer(debaml.Render),
		dynclient.WithNativeShadowComparator(spy.Compare),
	)
	if err != nil {
		t.Fatalf("dynclient.New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), liveCallTimeout)
	defer cancel()
	res, err := client.DynamicCall(ctx, dynclient.Request{
		Messages:            toDynMessages(fx.messages),
		ClientRegistry:      liveOracleRegistry(server.base()),
		OutputSchema:        fx.schema,
		PreserveSchemaOrder: bptr(true),
	})
	if err != nil {
		t.Fatalf("DynamicCall failed on an admitted fixture: %v", err)
	}

	// BAML served exactly once; the comparator ran once and opened zero sockets.
	if got := server.count(); got != 1 {
		t.Fatalf("provider saw %d requests, want exactly 1 (BAML serves; native never sends)", got)
	}
	if got := spy.calls.Load(); got != 1 {
		t.Fatalf("shadow comparator invoked %d times, want exactly 1", got)
	}
	if got := spy.exec.n.Load(); got != 0 {
		t.Fatalf("shadow comparator opened %d socket(s); the no-send comparator must open zero", got)
	}
	// THE REGRESSION GUARD: the default-client shadow worker REACHES comparison —
	// it did NOT decline at the rewrite/proxy strategy gate, and it recorded a full
	// all-match plan_compare (method/target/host/headers/body).
	if got := spy.sumDeclines(t, string(admission.StageStrategy), string(admission.ReasonURLRewriteOrProxy)); got != 0 {
		t.Fatalf("default no-proxy client declined %v times at the rewrite/proxy gate, want 0 (shadow must not be inert)", got)
	}
	if got := spy.sumPlanCompare(t, "mismatch"); got != 0 {
		t.Errorf("plan_compare mismatches = %v, want 0 (zero-tolerance)", got)
	}
	if got := spy.sumPlanCompare(t, "match"); got != 5 {
		t.Fatalf("plan_compare matches = %v, want 5 (method/target/host/headers/body) — comparison must actually run", got)
	}
	if !liveJSONSemEqual(t, res.Data, []byte(`{"answer":"ok"}`)) {
		t.Errorf("served structured output = %s, want {\"answer\":\"ok\"}", liveBodyDigest(res.Data))
	}
}

// TestShadowServe_FlagOffNeverConsultsProxyForForeignHost proves the hard-off
// guarantee: on the flag-off / default BAML-only path the caller-supplied
// Transport.Proxy resolver is consulted ZERO times for any synthesized host. The
// rewrite/proxy predicate is a method value forwarded to the comparator, and the
// comparator (hence the resolver) runs ONLY when the callback is installed (flag on
// AND a comparator present); admission evaluates it against the GENUINE effective
// target, never a fixed/foreign probe. So on flag-off the resolver is consulted
// only for the genuine loopback send target (BAML's own send), never a foreign host.
func TestShadowServe_FlagOffNeverConsultsProxyForForeignHost(t *testing.T) {
	spy := newShadowSpy(t)
	rec := &proxyRecorder{}
	fx := dynFixtureByName(t, "single_user_message")
	server := newLiveCaptureServer(t)
	server.setResponse(http.StatusOK, openAISuccess(`{"answer":"ok"}`))

	recClient := &http.Client{Transport: &http.Transport{
		Proxy:       rec.resolve,
		DialContext: loopbackDial,
	}}

	client, err := dynclient.New(
		dynclient.WithClientMode(llmhttp.ClientModeNetHTTP),
		dynclient.WithNetHTTPClient(recClient),
		dynclient.WithDeBAML(false),
		dynclient.WithDeBAMLRenderer(debaml.Render),
		dynclient.WithNativeShadowComparator(spy.Compare),
	)
	if err != nil {
		t.Fatalf("dynclient.New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), liveCallTimeout)
	defer cancel()
	res, err := client.DynamicCall(ctx, dynclient.Request{
		Messages:            toDynMessages(fx.messages),
		ClientRegistry:      liveOracleRegistry(server.base()),
		OutputSchema:        fx.schema,
		PreserveSchemaOrder: bptr(true),
	})
	if err != nil {
		t.Fatalf("DynamicCall failed on an admitted fixture: %v", err)
	}

	if got := server.count(); got != 1 {
		t.Fatalf("provider saw %d requests, want exactly 1 (BAML serves)", got)
	}
	// Flag-off installs no callback: the comparator is never invoked and no plan
	// build / socket occurs.
	if got := spy.calls.Load(); got != 0 {
		t.Fatalf("flag-off must NOT invoke the shadow comparator, got %d calls", got)
	}
	if got := spy.sumPlanCompare(t, "match") + spy.sumPlanCompare(t, "mismatch"); got != 0 {
		t.Errorf("flag-off recorded %v plan_compare series, want 0", got)
	}
	// The core P2 assertion: the caller's proxy resolver was never consulted for a
	// synthesized/foreign host — the shadow-fact evaluation added zero proxy calls.
	if foreign := rec.foreignHostCalls(); len(foreign) != 0 {
		t.Errorf("flag-off consulted Transport.Proxy for foreign host(s) %v, want none (no eager probe)", foreign)
	}
	if !liveJSONSemEqual(t, res.Data, []byte(`{"answer":"ok"}`)) {
		t.Errorf("served structured output = %s, want {\"answer\":\"ok\"}", liveBodyDigest(res.Data))
	}
}
