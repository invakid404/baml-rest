//go:build integration && nanollm_integration

package dynamic

// De-BAML #624 END-TO-END dynclient native-transport PARITY proof, driven through
// the PUBLIC go-get-able constructor github.com/invakid404/baml-rest/nativeserve.New
// (NOT the internal canary package directly). It is the in-process-library twin of
// native_serve_integration_test.go: same REAL generated dynamic call seam (dynclient
// + patched BAML + the nanollm-backed serve core), same S6 loopback harness, but the
// serve func comes from nativeserve.New — exactly what an external dynclient consumer
// would call and pass to dynclient.WithNativeServeComparator. It proves the public
// packaging delivers transport parity with the subprocess serve worker:
//
//   - FLAG ON, admitted /call: nativeserve.New's serve func runs once, CLAIMS exactly
//     ONE native provider RoundTrip (server sees exactly one request), returns the
//     native final THROUGH dynclient with planned_engine=native and a native winner,
//     and BAML NEVER sends. Exactly one native socket, flag="on".
//   - FLAG OFF: the seam gate keeps the callback nil, so the public serve func is
//     NEVER invoked — ZERO native FFI/socket/plan-build — and BAML serves the request
//     (one BAML provider request, baseline result, no engine metadata).
//   - PROVIDER NON-2XX: on an upstream 429 native claims one socket, fails with the
//     SAME provider_error disposition envelope as the serve worker, and there is NO
//     hidden same-child BAML resend (server sees exactly one request).
//
// Gated `integration && nanollm_integration` (BAML CFFI + nanollm); unreachable from
// production routing. Reuses the Slice-6 harness helpers (newLiveCaptureServer,
// dynFixtureByName, loopbackOracleHTTPClient, liveOracleRegistry, openAISuccess,
// toDynMessages, lastOutcomeEngine, …) defined alongside the sibling serve proof.

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/dynclient"
	"github.com/invakid404/baml-rest/internal/debaml"
	"github.com/invakid404/baml-rest/internal/nativeprompt"
	"github.com/invakid404/baml-rest/nativeserve"
)

// publicServeSpy wraps the serve func obtained from the PUBLIC nativeserve.New
// constructor, counting how many times the generated seam invokes it and exposing
// the registry New registered its bounded de-BAML collectors on (so the test can
// read native_sockets / attempts). It is the public-constructor twin of serveSpy.
type publicServeSpy struct {
	fn    bamlutils.NativeServeFunc
	calls atomic.Int64
	reg   *prometheus.Registry
}

func (s *publicServeSpy) Serve(ctx context.Context, req bamlutils.NativeServeRequest) bamlutils.NativeServeResult {
	s.calls.Add(1)
	return s.fn(ctx, req)
}

// newPublicServeSpy builds the serve func via nativeserve.New — the SAME call a
// dynclient consumer makes — registering its collectors on a fresh private registry.
func newPublicServeSpy(t *testing.T) *publicServeSpy {
	t.Helper()
	reg := prometheus.NewRegistry()
	fn, err := nativeserve.New(reg)
	if err != nil {
		t.Fatalf("nativeserve.New: %v", err)
	}
	if fn == nil {
		t.Fatal("nativeserve.New returned a nil serve func")
	}
	return &publicServeSpy{fn: fn, reg: reg}
}

func (s *publicServeSpy) sumCounter(t *testing.T, name, label, value string) float64 {
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

// runPublicServeDynamicCall drives one dynclient.DynamicCall wired with the public
// nativeserve.New serve func over the loopback capture server. It mirrors the
// sibling runServeDynamicCall exactly, differing only in the serve-func source.
func runPublicServeDynamicCall(t *testing.T, spy *publicServeSpy, deBAMLEnabled bool, status int, body []byte) (*liveCaptureServer, *dynclient.CallResult, error) {
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

// runPublicServeDynamicCallWith is runPublicServeDynamicCall with a CUSTOM message
// list (and the schema of the single_user_message fixture), so a test can exercise
// a specific message shape — e.g. multipart text parts — through the same public
// serve func + loopback harness.
func runPublicServeDynamicCallWith(t *testing.T, spy *publicServeSpy, deBAMLEnabled bool, msgs []nativeprompt.Message, status int, body []byte) (*liveCaptureServer, *dynclient.CallResult, error) {
	t.Helper()
	fx := dynFixtureByName(t, "single_user_message") // reuse its bounded schema
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
		Messages:            toDynMessages(msgs),
		ClientRegistry:      liveOracleRegistry(server.base()),
		OutputSchema:        fx.schema,
		PreserveSchemaOrder: bptr(true),
	})
	return server, res, callErr
}

// TestDynclientNativeServe_PublicConstructor_EmptyPlusNonemptyPartsServesNative is
// the P1 regression for #625: a multipart message whose parts are an EMPTY text
// part plus a NON-EMPTY one — [{type:"text",text:""},{type:"text",text:"hello"}] —
// must STILL be served NATIVELY, exactly as the original Slice-6 worker did. The
// shared prompt template drops the empty segment (`{% if p.text %}`) and native
// lowering does the same, so the message renders to a non-empty text-only shape
// that the native body support gate admits and S4 matches. Admission must NOT
// decline the empty part per-part (that over-broad guard would wrongly fall back to
// BAML and break the byte-identical worker). Proves: exactly ONE native provider
// request, native final THROUGH dynclient, planned_engine=native, BAML never sends.
func TestDynclientNativeServe_PublicConstructor_EmptyPlusNonemptyPartsServesNative(t *testing.T) {
	spy := newPublicServeSpy(t)
	msgs := []nativeprompt.Message{
		{Role: "user", Parts: []nativeprompt.ContentPart{{Text: sp("")}, {Text: sp("hello")}}},
	}
	server, res, err := runPublicServeDynamicCallWith(t, spy, true, msgs, http.StatusOK, openAISuccess(`{"answer":"ok"}`))
	if err != nil {
		t.Fatalf("DynamicCall must SERVE an empty+nonempty multipart message natively, got error: %v", err)
	}

	if got := server.count(); got != 1 {
		t.Fatalf("provider saw %d requests, want exactly 1 (native serves; BAML never sends)", got)
	}
	if got := spy.calls.Load(); got != 1 {
		t.Fatalf("public serve func invoked %d times, want exactly 1", got)
	}
	if got := spy.sumCounter(t, "baml_rest_debaml_native_sockets_total", "flag", "on"); got != 1 {
		t.Errorf("native_sockets{flag=on} = %v, want 1 (the empty part is dropped; the nonempty text is served natively)", got)
	}
	if !liveJSONSemEqual(t, res.Data, []byte(`{"answer":"ok"}`)) {
		t.Errorf("served structured output = %s, want {\"answer\":\"ok\"}", liveBodyDigest(res.Data))
	}
	winner, planned := lastOutcomeEngine(res)
	if planned != "native" {
		t.Errorf("planned_engine = %q, want native (empty-plus-nonempty parts must serve natively, not fall back)", planned)
	}
	if winner != bamlutils.NativeServeEngineNative && winner != bamlutils.NativeServeEngineBAMLParse {
		t.Errorf("winner_engine = %q, want native or native_baml_parse", winner)
	}
}

// TestDynclientNativeServe_PublicConstructor_WhollyEmptyRenderedDeclinesToBAML
// proves the intended fail-closed behavior is UNCHANGED: a message whose only part
// is an empty text part — [{type:"text",text:""}] — renders WHOLLY empty, so it is
// declined to BAML by the native body support gate (nativebody.SupportsOpenAIChat),
// exactly as on master S6 — NO native socket, NO native engine metadata. The
// decline happens downstream of admission (which admits the shape), not via an
// over-broad per-part admission guard.
func TestDynclientNativeServe_PublicConstructor_WhollyEmptyRenderedDeclinesToBAML(t *testing.T) {
	spy := newPublicServeSpy(t)
	msgs := []nativeprompt.Message{
		{Role: "user", Parts: []nativeprompt.ContentPart{{Text: sp("")}}},
	}
	server, res, callErr := runPublicServeDynamicCallWith(t, spy, true, msgs, http.StatusOK, openAISuccess(`{"answer":"ok"}`))

	// The request completes successfully — served by BAML, not native.
	if callErr != nil {
		t.Fatalf("DynamicCall on a wholly-empty-rendered message must succeed via BAML fallback, got: %v", callErr)
	}
	// Exactly one loopback provider request, and it was BAML's: the native path
	// declines PRE-SOCKET (the empty part is dropped at render so nothing renders to
	// send; the native body support gate declines with no socket claimed), so BAML
	// builds + sends the single provider request instead.
	if got := server.count(); got != 1 {
		t.Fatalf("provider saw %d requests, want exactly 1 (BAML serves; native opened no socket)", got)
	}
	if got := spy.sumCounter(t, "baml_rest_debaml_native_sockets_total", "flag", "on"); got != 0 {
		t.Errorf("native_sockets{flag=on} = %v, want 0 (a wholly-empty-rendered message must not open a native socket)", got)
	}
	// The public native callback WAS evaluated for this shape (flag on) — this is the
	// path that declined it — unlike the flag-off control, where it is never invoked.
	if got := spy.calls.Load(); got != 1 {
		t.Fatalf("public serve func invoked %d times, want exactly 1 (flag on: the callback ran and declined)", got)
	}
	// The final the caller receives is BAML's, equal to the provider answer, through
	// dynclient's ordinary CallResult envelope.
	if !liveJSONSemEqual(t, res.Data, []byte(`{"answer":"ok"}`)) {
		t.Errorf("served structured output = %s, want {\"answer\":\"ok\"}", liveBodyDigest(res.Data))
	}
	if winner, _ := lastOutcomeEngine(res); winner == bamlutils.NativeServeEngineNative {
		t.Errorf("winner_engine = %q, a wholly-empty message must never be SERVED by the native engine", winner)
	}
}

// TestDynclientNativeServe_PublicConstructor_FlagOnServesNative proves the flag-on
// public path: nativeserve.New's serve func runs once, CLAIMS exactly one native
// provider request, BAML never sends, and the native final is returned THROUGH
// dynclient with native engine metadata and exactly one counted native socket.
func TestDynclientNativeServe_PublicConstructor_FlagOnServesNative(t *testing.T) {
	spy := newPublicServeSpy(t)
	server, res, err := runPublicServeDynamicCall(t, spy, true, http.StatusOK, openAISuccess(`{"answer":"ok"}`))
	if err != nil {
		t.Fatalf("DynamicCall failed on an admitted fixture via nativeserve.New: %v", err)
	}

	// Exactly one provider request — NATIVE's. BAML never sent (native won).
	if got := server.count(); got != 1 {
		t.Fatalf("provider saw %d requests, want exactly 1 (native serves; BAML never sends)", got)
	}
	if got := spy.calls.Load(); got != 1 {
		t.Fatalf("public serve func invoked %d times, want exactly 1", got)
	}
	// Exactly one native socket, flag="on"; the invariant flag="off" series is flat.
	if got := spy.sumCounter(t, "baml_rest_debaml_native_sockets_total", "flag", "on"); got != 1 {
		t.Errorf("native_sockets{flag=on} = %v, want 1", got)
	}
	if got := spy.sumCounter(t, "baml_rest_debaml_native_sockets_total", "flag", "off"); got != 0 {
		t.Errorf("native_sockets{flag=off} = %v, want 0 (unreachable invariant)", got)
	}
	// The served structured output is native's, equal to the provider answer, and it
	// reached the caller THROUGH dynclient's ordinary CallResult envelope.
	if !liveJSONSemEqual(t, res.Data, []byte(`{"answer":"ok"}`)) {
		t.Errorf("served structured output = %s, want {\"answer\":\"ok\"}", liveBodyDigest(res.Data))
	}
	// Routing metadata: planned_engine=native, winner_engine is a native token — the
	// SAME envelope the serve worker produces.
	winner, planned := lastOutcomeEngine(res)
	if planned != "native" {
		t.Errorf("planned_engine = %q, want native", planned)
	}
	if winner != bamlutils.NativeServeEngineNative && winner != bamlutils.NativeServeEngineBAMLParse {
		t.Errorf("winner_engine = %q, want native or native_baml_parse", winner)
	}
}

// TestDynclientNativeServe_PublicConstructor_FlagOffServesBAML proves the flag-off
// control for the public path: zero native FFI/callback/socket, exactly one BAML
// provider request, the same served result as baseline, and no engine metadata.
func TestDynclientNativeServe_PublicConstructor_FlagOffServesBAML(t *testing.T) {
	spy := newPublicServeSpy(t)
	server, res, err := runPublicServeDynamicCall(t, spy, false, http.StatusOK, openAISuccess(`{"answer":"ok"}`))
	if err != nil {
		t.Fatalf("DynamicCall failed with flag off: %v", err)
	}

	if got := server.count(); got != 1 {
		t.Fatalf("provider saw %d requests, want exactly 1 (BAML serves)", got)
	}
	if got := spy.calls.Load(); got != 0 {
		t.Fatalf("public serve func invoked %d times with flag off, want 0 (hard-off)", got)
	}
	if got := spy.sumCounter(t, "baml_rest_debaml_native_sockets_total", "flag", "on"); got != 0 {
		t.Errorf("native_sockets{flag=on} = %v with flag off, want 0 (zero native FFI/socket/plan-build)", got)
	}
	if !liveJSONSemEqual(t, res.Data, []byte(`{"answer":"ok"}`)) {
		t.Errorf("served structured output = %s, want {\"answer\":\"ok\"}", liveBodyDigest(res.Data))
	}
	winner, planned := lastOutcomeEngine(res)
	if winner != "" || planned != "" {
		t.Errorf("flag-off metadata must omit engine tokens, got winner=%q planned=%q", winner, planned)
	}
}

// TestDynclientNativeServe_PublicConstructor_ProviderNon2xxEnvelope proves the
// provider non-2xx golden through the public path: on an upstream 429 native claims
// exactly one socket, fails with the SAME provider_error disposition envelope as the
// serve worker, and BAML is NOT re-sent for the same child (one request total).
func TestDynclientNativeServe_PublicConstructor_ProviderNon2xxEnvelope(t *testing.T) {
	spy := newPublicServeSpy(t)
	server, _, err := runPublicServeDynamicCall(t, spy, true, http.StatusTooManyRequests, []byte(`{"error":{"message":"rate limited"}}`))
	if err == nil {
		t.Fatalf("expected an error on a provider 429, got nil")
	}
	// The provider non-2xx maps to the SAME *llmhttp.HTTPError envelope the serve
	// worker produces (real upstream status), and it survives the dynclient outer
	// policy unwrapped — errors.As holds with the 429 status preserved.
	var httpErr *llmhttp.HTTPError
	if !errors.As(err, &httpErr) {
		t.Fatalf("err = %v, want an *llmhttp.HTTPError surfaced through dynclient", err)
	}
	if httpErr.StatusCode != http.StatusTooManyRequests {
		t.Errorf("HTTPError status = %d, want 429 (real upstream status)", httpErr.StatusCode)
	}
	if got := server.count(); got != 1 {
		t.Fatalf("provider saw %d requests, want exactly 1 (native only; NO BAML resend after claim)", got)
	}
	if got := spy.calls.Load(); got != 1 {
		t.Fatalf("public serve func invoked %d times, want exactly 1", got)
	}
	if got := spy.sumCounter(t, "baml_rest_debaml_native_sockets_total", "flag", "on"); got != 1 {
		t.Errorf("native_sockets{flag=on} = %v, want 1 (socket claimed even on non-2xx)", got)
	}
	if got := spy.sumCounter(t, "baml_rest_debaml_attempts_total", "outcome", "provider_error"); got != 1 {
		t.Errorf("provider_error attempts = %v, want 1 (same disposition envelope as the serve worker)", got)
	}
}
