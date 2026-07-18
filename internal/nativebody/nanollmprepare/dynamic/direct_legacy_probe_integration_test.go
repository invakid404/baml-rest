//go:build integration && nanollm_integration

package dynamic

// De-BAML unary multi-provider S1 — DIRECT-LEGACY native-first probe END-TO-END
// proof, driven through the REAL generated dynclient seam (dynclient + patched
// BAML + the nanollm-backed serve core). It proves the routing seam the cold
// re-review demanded: a direct single unary-call leaf whose existing BAML route
// is LEGACY (a call-unsupported provider such as cerebras) runs the native serve
// probe FIRST and, on a native DECLINE, runs the EXACT ordinary legacy serving
// lifecycle — same result/error behaviour as the plain legacy path, with only
// planned_engine=native added — rather than collapsing the legacy stream into a
// terminal native attempt error.
//
// Gated `integration && nanollm_integration`. Reuses the Slice-6 harness helpers.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/dynclient"
	"github.com/invakid404/baml-rest/internal/debaml"
	"github.com/invakid404/baml-rest/workerplugin"
)

// cerebrasRegistry is a direct single-leaf registry for a CALL-UNSUPPORTED
// provider (cerebras). BAML v0.223 does not recognise the cerebras provider, so
// its legacy stream errors — but the routing (native probe → decline → ordinary
// legacy) is what this test proves, and the error is BAML's, identical on the
// flag-on probe and the flag-off ordinary legacy path.
func cerebrasRegistry(base string) *dynclient.ClientRegistry {
	return &dynclient.ClientRegistry{
		Primary: sp("TestClient"),
		Clients: []*dynclient.ClientProperty{{
			Name:     "TestClient",
			Provider: "cerebras",
			Options: map[string]any{
				"model":    fenceModel,
				"base_url": base + "/v1",
				"api_key":  fenceAPIKey,
			},
		}},
	}
}

// runDirectLegacyProbeCall drives a plain unary /call against a cerebras (legacy-
// route) registry with de-BAML on/off and a native serve callback installed.
func runDirectLegacyProbeCall(t *testing.T, spy *serveSpy, deBAMLEnabled bool) (*dynclient.CallResult, error) {
	t.Helper()
	fx := dynFixtureByName(t, "single_user_message")
	server := newLiveCaptureServer(t)
	server.setResponse(200, openAISuccess(`{"answer":"ok"}`))

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
	// De-BAML mprov S2 activates cerebras: a COMPLETE cerebras config now maps and
	// serves natively. This probe test proves the native-DECLINE routing (probe →
	// decline → ordinary legacy), so it forces a deterministic pre-socket decline
	// with an empty api_key — a StageCredentialSource/api_key_absent decline that
	// never consults ambient env (unlike an absent option). cerebras remains
	// BAML-legacy (v0.223 does not recognise it), so the ordinary legacy leg still
	// errors identically on the flag-on and flag-off paths.
	reg := cerebrasRegistry(server.base())
	reg.Clients[0].Options["api_key"] = ""
	res, callErr := client.DynamicCall(ctx, dynclient.Request{
		Messages:            toDynMessages(fx.messages),
		ClientRegistry:      reg,
		OutputSchema:        fx.schema,
		PreserveSchemaOrder: bptr(true),
	})
	return res, callErr
}

// TestDirectLegacyProbe_CerebrasDeclineRunsOrdinaryLegacy proves the live decline
// routing: with the flag ON and a serve callback installed, a direct cerebras
// (legacy-route) call whose native config is incomplete invokes the native serve
// probe EXACTLY ONCE, which DECLINES pre-socket (api_key_absent — the empty api_key
// forced by the helper), and the request then runs the ORDINARY legacy BAML stream.
// There is NO native socket, and the terminal outcome is BAML's — byte-identical to
// the flag-OFF ordinary legacy path (which never invokes the probe). This is the
// error-stream parity the re-review required: the decline runs the ordinary
// lifecycle, it does not collapse into a native terminal attempt error. (Under
// de-BAML mprov S2 a COMPLETE cerebras config admits and serves natively; the
// incomplete-config decline keeps this routing invariant under test.)
func TestDirectLegacyProbe_CerebrasDeclineRunsOrdinaryLegacy(t *testing.T) {
	// Flag ON: the probe fires, native declines api_key_absent, ordinary legacy runs.
	onSpy := newServeSpy(t)
	_, onErr := runDirectLegacyProbeCall(t, onSpy, true)

	if got := onSpy.calls.Load(); got != 1 {
		t.Fatalf("serve probe invoked %d times, want exactly 1 (direct-legacy probe fired)", got)
	}
	if got := onSpy.sumCounter(t, "baml_rest_debaml_declines_total", "reason", "api_key_absent"); got != 1 {
		t.Fatalf("declines{api_key_absent} = %v, want 1 (native declined pre-socket)", got)
	}
	// Pre-socket decline: no native provider socket was ever claimed.
	if got := onSpy.sumCounter(t, "baml_rest_debaml_native_sockets_total", "flag", "on"); got != 0 {
		t.Fatalf("native_sockets{on} = %v, want 0 (decline opens no socket)", got)
	}
	// The request itself runs the ordinary legacy BAML stream — BAML v0.223 does
	// not recognise cerebras, so it errors. The probe must NOT mask that with a
	// native success (the decline runs the ORDINARY legacy path).
	if onErr == nil {
		t.Fatalf("flag-on cerebras call unexpectedly succeeded; want BAML's legacy error via the ordinary lifecycle")
	}

	// Flag OFF: the probe is NEVER invoked; the plain legacy path runs and errors
	// the same way. This is the parity baseline.
	offSpy := newServeSpy(t)
	_, offErr := runDirectLegacyProbeCall(t, offSpy, false)
	if got := offSpy.calls.Load(); got != 0 {
		t.Fatalf("serve probe invoked %d times with flag off, want 0 (hard-off)", got)
	}
	if offErr == nil {
		t.Fatalf("flag-off cerebras call unexpectedly succeeded; want BAML's legacy error")
	}

	// Error PARITY: the flag-on probe-decline leg and the flag-off ordinary legacy
	// leg both surface BAML's legacy error — the probe decline did not fork the
	// error behaviour (the whole point of running the ordinary lifecycle).
	if onErr.Error() != offErr.Error() {
		t.Errorf("direct-legacy decline error diverged from the ordinary legacy path:\n on = %v\noff = %v", onErr, offErr)
	}
}

// runDirectLegacyProbeCallWithServe drives a cerebras direct-legacy /call with an
// ARBITRARY serve callback (used to inject non-decline dispositions), flag on.
func runDirectLegacyProbeCallWithServe(t *testing.T, serve bamlutils.NativeServeFunc) (*dynclient.CallResult, error) {
	t.Helper()
	fx := dynFixtureByName(t, "single_user_message")
	server := newLiveCaptureServer(t)
	server.setResponse(200, openAISuccess(`{"answer":"ok"}`))

	client, err := dynclient.New(
		dynclient.WithClientMode(llmhttp.ClientModeNetHTTP),
		dynclient.WithNetHTTPClient(loopbackOracleHTTPClient()),
		dynclient.WithDeBAML(true),
		dynclient.WithDeBAMLRenderer(debaml.Render),
		dynclient.WithNativeServeComparator(serve),
	)
	if err != nil {
		t.Fatalf("dynclient.New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), liveCallTimeout)
	defer cancel()
	res, callErr := client.DynamicCall(ctx, dynclient.Request{
		Messages:            toDynMessages(fx.messages),
		ClientRegistry:      cerebrasRegistry(server.base()),
		OutputSchema:        fx.schema,
		PreserveSchemaOrder: bptr(true),
	})
	return res, callErr
}

// TestDirectLegacyProbe_UnknownDispositionFailsClosed proves the CRITICAL
// fail-closed contract (CodeRabbit): a serve callback that returns an OUT-OF-ENUM
// disposition (NativeServeResult crosses a public, integer-backed boundary) must
// FAIL CLOSED — the generated probe emits a terminal error and does NOT run the
// ordinary legacy stream (which would be a hidden SECOND same-child BAML request
// after the native callback may have opened a socket). A GENUINE decline, by
// contrast, still runs the ordinary legacy stream.
func TestDirectLegacyProbe_UnknownDispositionFailsClosed(t *testing.T) {
	// Out-of-enum disposition -> terminal "unknown disposition" error; legacy NOT run.
	unknownServe := func(_ context.Context, _ bamlutils.NativeServeRequest) bamlutils.NativeServeResult {
		return bamlutils.NativeServeResult{Disposition: bamlutils.NativeServeDisposition(99)}
	}
	_, err := runDirectLegacyProbeCallWithServe(t, unknownServe)
	if err == nil {
		t.Fatalf("unknown disposition must fail closed with a terminal error")
	}
	if !strings.Contains(err.Error(), "unknown disposition") {
		t.Fatalf("unknown disposition error = %v, want the terminal unknown-disposition error (NOT BAML's legacy cerebras error — the legacy runner must NOT have run)", err)
	}

	// Genuine decline -> the ordinary legacy stream runs (BAML's cerebras error),
	// NOT the fail-closed terminal error.
	declineServe := func(_ context.Context, _ bamlutils.NativeServeRequest) bamlutils.NativeServeResult {
		return bamlutils.NativeServeResult{
			Disposition: bamlutils.NativeServeDeclined,
			Stage:       "mapping",
			Reason:      "mapping_unavailable",
		}
	}
	_, declErr := runDirectLegacyProbeCallWithServe(t, declineServe)
	if declErr == nil {
		t.Fatalf("a genuine decline should still run the ordinary legacy stream (BAML's cerebras error)")
	}
	if strings.Contains(declErr.Error(), "unknown disposition") {
		t.Fatalf("a genuine decline must run the legacy stream, not fail closed; got %v", declErr)
	}
}

// runDirectLegacyProbeCallWithRetry drives a cerebras direct-legacy /call with a
// per-request retry override installed (WithRequestRetryOverride — the same seam a
// REST caller uses). The override makes the generated router resolve
// __legacyRetryPolicy != nil, so bamlRest…DirectLegacyCall forwards
// hasRequestRetryOverride=true into the native serve request.
func runDirectLegacyProbeCallWithRetry(t *testing.T, spy *serveSpy, deBAMLEnabled bool) (*dynclient.CallResult, error) {
	t.Helper()
	fx := dynFixtureByName(t, "single_user_message")
	server := newLiveCaptureServer(t)
	server.setResponse(200, openAISuccess(`{"answer":"ok"}`))

	client, err := dynclient.New(
		dynclient.WithClientMode(llmhttp.ClientModeNetHTTP),
		dynclient.WithNetHTTPClient(loopbackOracleHTTPClient()),
		dynclient.WithDeBAML(deBAMLEnabled),
		dynclient.WithDeBAMLRenderer(debaml.Render),
		dynclient.WithNativeServeComparator(spy.Serve),
		// A resolved request retry policy => adapter.RetryConfig() != nil =>
		// __legacyRetryPolicy != nil => hasRequestRetryOverride=true. The 1ms/2-retry
		// policy never fires on the loopback success, so it does not perturb the send.
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
	res, callErr := client.DynamicCall(ctx, dynclient.Request{
		Messages:            toDynMessages(fx.messages),
		ClientRegistry:      cerebrasRegistry(server.base()),
		OutputSchema:        fx.schema,
		PreserveSchemaOrder: bptr(true),
	})
	return res, callErr
}

// TestDirectLegacyProbe_RetryOverrideDeclinesPreSocket proves the CodeRabbit
// retry-override fix (MAJOR functional correctness): a direct cerebras (legacy-
// route) leaf carrying a RESOLVED retry policy now forwards the TRUTHFUL
// HasRequestRetryOverride into the native serve request, so native admission
// declines PRE-SOCKET at the STRATEGY stage (request_retry_override) instead of
// silently dropping the fact. The decline REASON is the observable proof the fact
// propagated: before the fix emitDirectLegacyCall hardcoded
// HasRequestRetryOverride=false, so the SAME leaf declined mapping_unavailable (a
// LATER gate) and a resolved BAML retry policy could be bypassed by a native
// attempt. The request still runs the ordinary legacy stream (0 native sockets),
// byte-identical to the flag-off path — the retry policy stays with BAML.
//
// The complementary "no retry override -> native-first probe still fires" case is
// TestDirectLegacyProbe_CerebrasDeclineRunsOrdinaryLegacy (probe fires, declines
// mapping_unavailable).
func TestDirectLegacyProbe_RetryOverrideDeclinesPreSocket(t *testing.T) {
	onSpy := newServeSpy(t)
	_, onErr := runDirectLegacyProbeCallWithRetry(t, onSpy, true)

	// The probe fires exactly once (the router dispatches to the direct-legacy
	// helper); native admission then declines it at the strategy stage.
	if got := onSpy.calls.Load(); got != 1 {
		t.Fatalf("serve probe invoked %d times, want exactly 1 (direct-legacy probe fired)", got)
	}
	// Declined for the retry override — the truthful fact the fix forwards. The
	// STRATEGY gate runs BEFORE the cerebras mapping gate, so a propagated override
	// wins the reason.
	if got := onSpy.sumCounter(t, "baml_rest_debaml_declines_total", "reason", "request_retry_override"); got != 1 {
		t.Fatalf("declines{request_retry_override} = %v, want 1 (resolved retry policy forwarded + declined pre-socket)", got)
	}
	// It must NOT decline mapping_unavailable — that would mean the retry fact was
	// dropped (the pre-fix bug) and the earlier strategy gate never saw it.
	if got := onSpy.sumCounter(t, "baml_rest_debaml_declines_total", "reason", "mapping_unavailable"); got != 0 {
		t.Fatalf("declines{mapping_unavailable} = %v, want 0 (retry override must decline at the earlier strategy stage, proving the fact propagated)", got)
	}
	// Pre-socket: no native provider socket was claimed.
	if got := onSpy.sumCounter(t, "baml_rest_debaml_native_sockets_total", "flag", "on"); got != 0 {
		t.Fatalf("native_sockets{on} = %v, want 0 (retry-override decline opens no socket)", got)
	}
	// The request runs the ordinary legacy BAML stream (BAML v0.223 does not know
	// cerebras, so it errors) — the probe decline did not mask that.
	if onErr == nil {
		t.Fatalf("flag-on cerebras+retry call unexpectedly succeeded; want BAML's legacy error via the ordinary lifecycle")
	}

	// Flag OFF with the SAME retry override: the probe never fires, the plain legacy
	// path runs and errors the same way — the parity baseline.
	offSpy := newServeSpy(t)
	_, offErr := runDirectLegacyProbeCallWithRetry(t, offSpy, false)
	if got := offSpy.calls.Load(); got != 0 {
		t.Fatalf("serve probe invoked %d times with flag off, want 0 (hard-off)", got)
	}
	if offErr == nil {
		t.Fatalf("flag-off cerebras+retry call unexpectedly succeeded; want BAML's legacy error")
	}

	// Error PARITY: the flag-on retry-override-decline leg and the flag-off ordinary
	// legacy leg surface the SAME BAML legacy error — the retry-override decline runs
	// the ordinary lifecycle and does not fork the error behaviour.
	if onErr.Error() != offErr.Error() {
		t.Errorf("direct-legacy retry-override decline error diverged from the ordinary legacy path:\n on = %v\noff = %v", onErr, offErr)
	}
}

// errDetailRaw extracts details.raw from a DynamicCall error. The worker stream
// path merges a StreamResult's Raw() into ErrorDetails (worker.mergeRawDetail), and
// dynclient surfaces it as a *workerplugin.ErrorWithStack whose GetDetails() is the
// {"raw":...} JSON object — the same details.raw a REST caller observes.
func errDetailRaw(t *testing.T, err error) string {
	t.Helper()
	var ews *workerplugin.ErrorWithStack
	if !errors.As(err, &ews) {
		t.Fatalf("error %T is not a *workerplugin.ErrorWithStack; cannot read details.raw: %v", err, err)
	}
	details := ews.GetDetails()
	if len(details) == 0 {
		return ""
	}
	var m map[string]any
	if uerr := json.Unmarshal(details, &m); uerr != nil {
		t.Fatalf("error details is not a JSON object: %q (%v)", details, uerr)
	}
	raw, _ := m["raw"].(string)
	return raw
}

// TestDirectLegacyProbe_NativeWrapFailureRetainsRaw proves the CodeRabbit
// raw-diagnostic fix on the S2-live NATIVE-SUCCESS-but-WRAP-FAILED terminal branch:
// when a native serve SUCCEEDS but wrapDeBAMLDynamicOutput cannot decode FinalJSON,
// the generated direct-legacy helper now carries the native-owned raw channel
// (__res.Raw) as details.raw on the terminal OutputParseError — parity with the
// ordinary native-call route's FailNativeCallWithRaw(&OutputParseError, res.Raw).
// The terminal error is the NATIVE wrap error; there is NO ordinary legacy resend
// (which would instead surface BAML's cerebras error and clobber the native raw).
//
// (Unreachable in S1 production — every non-openai leaf mapping-declines pre-socket
// — so it is driven through the arbitrary-serve seam; the fix is required for S2
// parity.)
func TestDirectLegacyProbe_NativeWrapFailureRetainsRaw(t *testing.T) {
	const nativeRaw = "native-success-raw-channel-payload-δ"
	serve := func(_ context.Context, _ bamlutils.NativeServeRequest) bamlutils.NativeServeResult {
		return bamlutils.NativeServeResult{
			Disposition:  bamlutils.NativeServeSucceeded,
			FinalJSON:    []byte("<<not decodable dynamic-output json>>"),
			Raw:          nativeRaw,
			WinnerEngine: bamlutils.NativeServeEngineNative,
		}
	}
	_, err := runDirectLegacyProbeCallWithServe(t, serve)
	if err == nil {
		t.Fatal("native success with an undecodable FinalJSON must surface the terminal wrap error")
	}
	// details.raw must carry the native-owned Raw channel — the dropped value the
	// fix restores.
	if got := errDetailRaw(t, err); got != nativeRaw {
		t.Fatalf("details.raw = %q, want the native-owned raw %q retained on the wrap-failure terminal error", got, nativeRaw)
	}
	// The error is the NATIVE wrap error, not a legacy resend (which would surface
	// BAML's cerebras error). details.raw==nativeRaw already proves no resend
	// clobbered the frame; assert the message too for clarity.
	if strings.Contains(err.Error(), "cerebras") {
		t.Fatalf("native-terminal wrap failure must NOT fall through to the legacy cerebras stream; got %v", err)
	}
}

// TestDirectLegacyProbe_NativeFailureRetainsRawDiagnostic proves the CodeRabbit
// raw-diagnostic fix on the S2-live NATIVE-FAILURE terminal branch: a native serve
// FAILURE now carries the native-owned RawDiagnostic as details.raw on the terminal
// typed error — parity with the ordinary native-call route's
// FailNativeCallWithRaw(res.Err, res.RawDiagnostic). The failure is terminal with NO
// ordinary legacy resend (a resend would surface BAML's cerebras error instead).
//
// (Unreachable in S1 production; driven through the arbitrary-serve seam.)
func TestDirectLegacyProbe_NativeFailureRetainsRawDiagnostic(t *testing.T) {
	const diagRaw = "native-failure-raw-diagnostic-Ω"
	serve := func(_ context.Context, _ bamlutils.NativeServeRequest) bamlutils.NativeServeResult {
		return bamlutils.NativeServeResult{
			Disposition:   bamlutils.NativeServeFailed,
			Err:           errors.New("native provider transport error"),
			RawDiagnostic: diagRaw,
		}
	}
	_, err := runDirectLegacyProbeCallWithServe(t, serve)
	if err == nil {
		t.Fatal("native failure must surface the typed terminal error")
	}
	if got := errDetailRaw(t, err); got != diagRaw {
		t.Fatalf("details.raw = %q, want the native RawDiagnostic %q retained on the terminal native-failure error", got, diagRaw)
	}
	if strings.Contains(err.Error(), "cerebras") {
		t.Fatalf("native-terminal failure must NOT fall through to the legacy cerebras stream; got %v", err)
	}
}
