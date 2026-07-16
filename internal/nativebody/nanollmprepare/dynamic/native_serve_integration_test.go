//go:build integration && nanollm_integration

package dynamic

// De-BAML cutover Slice 6 END-TO-END native SERVE proof, through the REAL
// generated dynamic call seam (dynclient + patched BAML + the nanollm-backed
// canary serve implementation). It is the serving twin of the Slice-4
// shadow_serve_integration_test proof and reuses that harness's fixtures +
// loopback capture server. It proves the serving-path behaviour the unit suites
// cannot:
//
//   - FLAG ON, admitted /call: the generated seam installs the Slice-1 native
//     child-attempt callback as the SERVE implementation, which runs admission +
//     the S4 plan-compare precondition and, on a match, CLAIMS exactly ONE native
//     provider RoundTrip (server sees exactly one request, produced by native),
//     translates/extracts/parses it, runs the S5 same-response BAML-parse safety
//     compare, and returns the native final through tryOneChild's ordinary
//     envelope. BAML NEVER sends. winner_engine is native / native_baml_parse and
//     planned_engine is native. Exactly one native socket is counted.
//   - FLAG OFF: the seam gate (DeBAMLConfig().Enabled) keeps the callback nil, so
//     the serve implementation is NEVER invoked — ZERO native FFI/callback/socket
//     — and BAML serves the request with exactly one BAML provider request, the
//     same result as baseline, and no planned/winner engine metadata.
//   - PROVIDER NON-2XX golden: on an upstream 429 native claims one socket, fails,
//     and there is NO hidden same-child BAML resend (server sees exactly one
//     request); DynamicCall surfaces an error.
//
// Gated `integration && nanollm_integration` (BAML CFFI + nanollm); nothing here
// is reachable from production routing.

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/dynclient"
	"github.com/invakid404/baml-rest/internal/debaml"
	"github.com/invakid404/baml-rest/internal/nativebody/nanollmprepare/admission"
	"github.com/invakid404/baml-rest/internal/nativebody/nanollmprepare/canary"
)

// panicRoundTripper panics inside RoundTrip, to drive a post-claim panic in the
// exact executor and prove the socket metric is still recorded exactly once (via
// the once-only defer) and BAML is never resent.
type panicRoundTripper struct{}

func (panicRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	panic("de-BAML serve test: injected executor panic")
}

// serveSpy wraps the real nanollm-backed serve implementation, counting how many
// times the generated seam invokes it and exposing the private registry so the
// test can read plan_compare / response_compare / native_sockets. Its exact
// executor is the hardened default one, which actually dials the loopback capture
// server (a real httptest server) — so a served attempt opens ONE real socket.
type serveSpy struct {
	fn    bamlutils.NativeServeFunc
	calls atomic.Int64
	reg   *prometheus.Registry
}

func (s *serveSpy) Serve(ctx context.Context, req bamlutils.NativeServeRequest) bamlutils.NativeServeResult {
	s.calls.Add(1)
	return s.fn(ctx, req)
}

func newServeSpy(t *testing.T) *serveSpy {
	t.Helper()
	reg := prometheus.NewRegistry()
	m, err := admission.NewMetrics(reg)
	if err != nil {
		t.Fatalf("admission.NewMetrics: %v", err)
	}
	srv := canary.NewServer(m, llmhttp.NewExactExecutor(nil))
	return &serveSpy{fn: srv.Serve, reg: reg}
}

func (s *serveSpy) sumCounter(t *testing.T, name, label, value string) float64 {
	t.Helper()
	fams, err := s.reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var sum float64
	for _, mf := range fams {
		if mf.GetName() != name {
			continue
		}
		for _, mm := range mf.GetMetric() {
			for _, lp := range mm.GetLabel() {
				if lp.GetName() == label && lp.GetValue() == value {
					sum += mm.GetCounter().GetValue()
				}
			}
		}
	}
	return sum
}

func runServeDynamicCall(t *testing.T, spy *serveSpy, deBAMLEnabled bool, status int, body []byte) (*liveCaptureServer, *dynclient.CallResult, error) {
	t.Helper()
	fx := dynFixtureByName(t, "single_user_message")
	server := newLiveCaptureServer(t)
	server.setResponse(status, body)

	client, err := dynclient.New(
		dynclient.WithClientMode(llmhttp.ClientModeNetHTTP),
		dynclient.WithNetHTTPClient(loopbackOracleHTTPClient()),
		dynclient.WithDeBAML(deBAMLEnabled),
		dynclient.WithDeBAMLRenderer(debaml.Render),
		dynclient.WithNativeServeComparator(spy.Serve),
	)
	if err != nil {
		t.Fatalf("dynclient.New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), liveCallTimeout)
	defer cancel()
	res, callErr := client.DynamicCall(ctx, dynclient.Request{
		Messages:            toDynMessages(fx.messages),
		ClientRegistry:      liveOracleRegistry(server.base()),
		OutputSchema:        fx.schema,
		PreserveSchemaOrder: bptr(true),
	})
	return server, res, callErr
}

func lastOutcomeEngine(res *dynclient.CallResult) (winner, planned string) {
	for i := range res.Metadata {
		md := res.Metadata[i]
		if md.Phase == bamlutils.MetadataPhaseOutcome {
			winner, planned = md.WinnerEngine, md.PlannedEngine
		}
	}
	return winner, planned
}

// TestNativeServe_FlagOnServesNative proves the flag-on serve path: the serve
// implementation runs once, CLAIMS exactly one native provider request, BAML
// never sends, and the native final is served through the ordinary envelope with
// winner/planned engine metadata and exactly one counted native socket.
func TestNativeServe_FlagOnServesNative(t *testing.T) {
	spy := newServeSpy(t)
	server, res, err := runServeDynamicCall(t, spy, true, http.StatusOK, openAISuccess(`{"answer":"ok"}`))
	if err != nil {
		t.Fatalf("DynamicCall failed on an admitted fixture: %v", err)
	}

	// Exactly one provider request — NATIVE's. BAML never sent (native won).
	if got := server.count(); got != 1 {
		t.Fatalf("provider saw %d requests, want exactly 1 (native serves; BAML never sends)", got)
	}
	if got := spy.calls.Load(); got != 1 {
		t.Fatalf("serve implementation invoked %d times, want exactly 1", got)
	}
	// Exactly one native socket, flag="on"; the invariant flag="off" series is flat.
	if got := spy.sumCounter(t, "baml_rest_debaml_native_sockets_total", "flag", "on"); got != 1 {
		t.Errorf("native_sockets{flag=on} = %v, want 1", got)
	}
	if got := spy.sumCounter(t, "baml_rest_debaml_native_sockets_total", "flag", "off"); got != 0 {
		t.Errorf("native_sockets{flag=off} = %v, want 0 (unreachable invariant)", got)
	}
	// The served structured output is native's, equal to the provider answer.
	if !liveJSONSemEqual(t, res.Data, []byte(`{"answer":"ok"}`)) {
		t.Errorf("served structured output = %s, want {\"answer\":\"ok\"}", liveBodyDigest(res.Data))
	}
	// Both safety comparisons actually RAN and matched (not merely "no mismatch"):
	// the S4 plan compare recorded a match on all five fields (method/target/host/
	// headers/body) for the single admitted request, and the S5 same-response
	// compare recorded at least one matched facet. Zero mismatches (zero-tolerance).
	if got := spy.sumCounter(t, "baml_rest_debaml_plan_compare_total", "result", "mismatch"); got != 0 {
		t.Errorf("plan_compare mismatches = %v, want 0", got)
	}
	if got := spy.sumCounter(t, "baml_rest_debaml_plan_compare_total", "result", "match"); got != 5 {
		t.Errorf("plan_compare matches = %v, want 5 (method/target/host/headers/body, one request)", got)
	}
	if got := spy.sumCounter(t, "baml_rest_debaml_response_compare_total", "result", "mismatch"); got != 0 {
		t.Errorf("response_compare mismatches = %v, want 0", got)
	}
	if got := spy.sumCounter(t, "baml_rest_debaml_response_compare_total", "result", "match"); got < 1 {
		t.Errorf("response_compare matches = %v, want >= 1 (the S5 safety compare ran)", got)
	}
	// Routing metadata: planned_engine=native, winner_engine is a native token.
	winner, planned := lastOutcomeEngine(res)
	if planned != "native" {
		t.Errorf("planned_engine = %q, want native", planned)
	}
	if winner != bamlutils.NativeServeEngineNative && winner != bamlutils.NativeServeEngineBAMLParse {
		t.Errorf("winner_engine = %q, want native or native_baml_parse", winner)
	}
}

// TestNativeServe_FlagOffServesBAML proves the flag-off control: zero native
// FFI/callback/socket, exactly one BAML provider request, and the same served
// result as baseline with no planned/winner engine metadata.
func TestNativeServe_FlagOffServesBAML(t *testing.T) {
	spy := newServeSpy(t)
	server, res, err := runServeDynamicCall(t, spy, false, http.StatusOK, openAISuccess(`{"answer":"ok"}`))
	if err != nil {
		t.Fatalf("DynamicCall failed with flag off: %v", err)
	}

	if got := server.count(); got != 1 {
		t.Fatalf("provider saw %d requests, want exactly 1 (BAML serves)", got)
	}
	if got := spy.calls.Load(); got != 0 {
		t.Fatalf("serve implementation invoked %d times with flag off, want 0 (hard-off)", got)
	}
	if got := spy.sumCounter(t, "baml_rest_debaml_native_sockets_total", "flag", "on"); got != 0 {
		t.Errorf("native_sockets{flag=on} = %v with flag off, want 0", got)
	}
	if !liveJSONSemEqual(t, res.Data, []byte(`{"answer":"ok"}`)) {
		t.Errorf("served structured output = %s, want {\"answer\":\"ok\"}", liveBodyDigest(res.Data))
	}
	winner, planned := lastOutcomeEngine(res)
	if winner != "" || planned != "" {
		t.Errorf("flag-off metadata must omit engine tokens, got winner=%q planned=%q", winner, planned)
	}
}

// TestNativeServe_ProviderNon2xxNoResend proves the provider non-2xx golden and
// the no-hidden-resend invariant: on an upstream 429 native claims exactly one
// socket, fails, and BAML is NOT re-sent for the same child (server sees exactly
// one request). DynamicCall surfaces an error.
func TestNativeServe_ProviderNon2xxNoResend(t *testing.T) {
	spy := newServeSpy(t)
	server, _, err := runServeDynamicCall(t, spy, true, http.StatusTooManyRequests, []byte(`{"error":{"message":"rate limited"}}`))
	if err == nil {
		t.Fatalf("expected an error on a provider 429, got nil")
	}
	if got := server.count(); got != 1 {
		t.Fatalf("provider saw %d requests, want exactly 1 (native only; NO BAML resend after claim)", got)
	}
	if got := spy.calls.Load(); got != 1 {
		t.Fatalf("serve implementation invoked %d times, want exactly 1", got)
	}
	if got := spy.sumCounter(t, "baml_rest_debaml_native_sockets_total", "flag", "on"); got != 1 {
		t.Errorf("native_sockets{flag=on} = %v, want 1 (socket claimed even on non-2xx)", got)
	}
	if got := spy.sumCounter(t, "baml_rest_debaml_attempts_total", "outcome", "provider_error"); got != 1 {
		t.Errorf("provider_error attempts = %v, want 1", got)
	}
}

// TestNativeServe_ExecutorPanicCountsSocketNoResend proves the once-only socket
// accounting: a panic in the exact executor (post-claim) is caught by the serve
// callback's guard and turned into a terminal FAILURE, yet native_sockets_total
// still records the claimed attempt EXACTLY ONCE (conservative transport-error via
// the post-claim defer) and BAML is NOT resent for the same child.
func TestNativeServe_ExecutorPanicCountsSocketNoResend(t *testing.T) {
	reg := prometheus.NewRegistry()
	m, err := admission.NewMetrics(reg)
	if err != nil {
		t.Fatalf("admission.NewMetrics: %v", err)
	}
	// Serve over an executor whose transport panics inside RoundTrip.
	srv := canary.NewServer(m, llmhttp.NewExactExecutor(panicRoundTripper{}))
	spy := &serveSpy{fn: srv.Serve, reg: reg}

	fx := dynFixtureByName(t, "single_user_message")
	server := newLiveCaptureServer(t)
	server.setResponse(http.StatusOK, openAISuccess(`{"answer":"ok"}`))

	client, err := dynclient.New(
		dynclient.WithClientMode(llmhttp.ClientModeNetHTTP),
		dynclient.WithNetHTTPClient(loopbackOracleHTTPClient()),
		dynclient.WithDeBAML(true),
		dynclient.WithDeBAMLRenderer(debaml.Render),
		dynclient.WithNativeServeComparator(spy.Serve),
	)
	if err != nil {
		t.Fatalf("dynclient.New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), liveCallTimeout)
	defer cancel()
	if _, callErr := client.DynamicCall(ctx, dynclient.Request{
		Messages:            toDynMessages(fx.messages),
		ClientRegistry:      liveOracleRegistry(server.base()),
		OutputSchema:        fx.schema,
		PreserveSchemaOrder: bptr(true),
	}); callErr == nil {
		t.Fatalf("expected an error from the panicking executor, got nil")
	}

	// The panic never reached a real send, and the failed native attempt is NOT
	// re-sent through BAML for the same child.
	if got := server.count(); got != 0 {
		t.Fatalf("provider saw %d requests, want 0 (panic before send; NO BAML resend)", got)
	}
	// Exactly one native socket counted despite the panic bypassing the normal
	// definitive-outcome record (the once-only defer covers it).
	if got := spy.sumCounter(t, "baml_rest_debaml_native_sockets_total", "flag", "on"); got != 1 {
		t.Errorf("native_sockets{flag=on} = %v, want exactly 1 (once-only defer counts a panicking claim)", got)
	}
	if got := spy.sumCounter(t, "baml_rest_debaml_native_sockets_total", "flag", "off"); got != 0 {
		t.Errorf("native_sockets{flag=off} = %v, want 0 (unreachable invariant)", got)
	}
	// A pre-parse (executor/transport) panic is recorded as internal_error, NOT
	// misclassified as parse_error.
	if got := spy.sumCounter(t, "baml_rest_debaml_attempts_total", "outcome", "internal_error"); got != 1 {
		t.Errorf("internal_error attempts = %v, want 1 (bounded panic outcome)", got)
	}
	if got := spy.sumCounter(t, "baml_rest_debaml_attempts_total", "outcome", "parse_error"); got != 0 {
		t.Errorf("parse_error attempts = %v, want 0 (an executor panic is not a parse error)", got)
	}
}

// TestNativeServe_PreAdmissionCancellation proves the pre-admission cancellation
// gate end to end: a request cancelled before the call fails, opens ZERO native
// sockets, and does not double-send (native + BAML). The dynclient envelope
// surfaces the orchestrator's generic drain error for a pre-cancelled call (its
// trySend drops the error frame while ctx is cancelled — pre-existing,
// BAML-neutral behaviour); the TYPED context-cancel disposition at the layer this
// PR owns is proven by the canary unit tests (TestServe_EntryCancellationDeclines
// and TestMapAttempt_ContextCancelledIsTransport).
func TestNativeServe_PreAdmissionCancellation(t *testing.T) {
	spy := newServeSpy(t)
	fx := dynFixtureByName(t, "single_user_message")
	server := newLiveCaptureServer(t)
	server.setResponse(http.StatusOK, openAISuccess(`{"answer":"ok"}`))

	client, err := dynclient.New(
		dynclient.WithClientMode(llmhttp.ClientModeNetHTTP),
		dynclient.WithNetHTTPClient(loopbackOracleHTTPClient()),
		dynclient.WithDeBAML(true),
		dynclient.WithDeBAMLRenderer(debaml.Render),
		dynclient.WithNativeServeComparator(spy.Serve),
	)
	if err != nil {
		t.Fatalf("dynclient.New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel BEFORE the call

	_, callErr := client.DynamicCall(ctx, dynclient.Request{
		Messages:            toDynMessages(fx.messages),
		ClientRegistry:      liveOracleRegistry(server.base()),
		OutputSchema:        fx.schema,
		PreserveSchemaOrder: bptr(true),
	})
	if callErr == nil {
		t.Fatalf("expected an error on a cancelled request, got nil")
	}
	// Zero native sockets: the request never claimed a native attempt (it declined
	// pre-socket, or the orchestrator bailed on the cancelled ctx before the
	// callback) — no native FFI, no socket.
	if got := spy.sumCounter(t, "baml_rest_debaml_native_sockets_total", "flag", "on"); got != 0 {
		t.Errorf("native_sockets{flag=on} = %v, want 0 on a cancelled request", got)
	}
	// No native+BAML double-send: at most one provider request total.
	if got := server.count(); got > 1 {
		t.Fatalf("provider saw %d requests, want <= 1 (no double-send on cancel)", got)
	}
}
