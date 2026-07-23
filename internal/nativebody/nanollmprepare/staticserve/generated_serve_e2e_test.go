//go:build integration && nanollm_integration

package staticserve

// De-BAML Slice 8C generated-route END-TO-END serving proof (review P1.2). It drives
// the REAL generated static-serve fixture adapter (a compilable ctx-first generated
// adapter over staticserve_fixture, DeBAMLStaticServe on) through the PUBLIC serve
// constructor nativeserve.NewStaticServe, over a loopback capture server bound to the
// fixture's baked base_url — proving the GENERATED static /call seam actually invokes
// NativeStaticServeFunc and honours the one-send + tri-state ownership contract:
//
//   - FLAG ON, admitted /call: the generated /call installs the serve attempt, the
//     serve func runs once, CLAIMS exactly ONE native provider RoundTrip (server sees
//     one request), decodes the native final into the concrete StaticAnswer THROUGH
//     the generated method with winner=native, and BAML NEVER sends. One native socket.
//   - FLAG OFF: the seam gate keeps the callback nil, so the serve func is NEVER
//     invoked — ZERO native FFI/socket — and BAML serves (one BAML provider request).
//   - FLAG ON, call-with-raw: a PRE-CLAIM decline — the serve func runs once and
//     declines (Raw unproven), ZERO native sockets, and BAML serves the one request.
//   - FLAG ON, provider non-2xx: native claims one socket, fails, and there is NO
//     hidden BAML resend (server sees exactly one request).
//
// The fixture's StaticOracleClient base_url is repointed from the .invalid fence to
// the fixed loopback below so both native (descriptor ClientConfig) and BAML (baml
// source map) send to the SAME controllable server and the S4 plan compare matches.

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/nativeserve"

	fixturebaml "github.com/invakid404/baml-rest/internal/nativeprompt/testdata/staticserve_fixture/baml_client"
	fixture "github.com/invakid404/baml-rest/internal/nativeprompt/testdata/staticserve_fixture/generated"
	fwadapter "github.com/invakid404/baml-rest/internal/nativeprompt/testdata/staticserve_fixture/generated/adapter"
)

// fixtureLoopbackAddr is the FIXED loopback the fixture's StaticOracleClient base_url
// was repointed to (baml_source_map.go + introspected.go). A fixed port is required
// because the base_url is a baked literal; the test skips if it cannot bind.
const fixtureLoopbackAddr = "127.0.0.1:17654"

// staticServeSpy wraps the serve func from the PUBLIC nativeserve.NewStaticServe,
// counting generated-seam invocations and exposing the metrics registry so the test
// can read native_sockets{flag=on} to distinguish a native send from a BAML send.
type staticServeSpy struct {
	fn         bamlutils.NativeStaticServeFunc
	calls      atomic.Int64
	reg        *prometheus.Registry
	lastStage  atomic.Value // string
	lastReason atomic.Value // string
	lastDisp   atomic.Int64
}

func (s *staticServeSpy) Serve(ctx context.Context, inv bamlutils.NativeStaticInvocation) bamlutils.NativeStaticServeResult {
	s.calls.Add(1)
	res := s.fn(ctx, inv)
	s.lastStage.Store(res.Stage)
	s.lastReason.Store(res.Reason)
	s.lastDisp.Store(int64(res.Disposition))
	return res
}

func (s *staticServeSpy) lastDecline() (int64, string, string) {
	st, _ := s.lastStage.Load().(string)
	rs, _ := s.lastReason.Load().(string)
	return s.lastDisp.Load(), st, rs
}

func newStaticServeSpy(t *testing.T) *staticServeSpy {
	t.Helper()
	reg := prometheus.NewRegistry()
	fn, err := nativeserve.NewStaticServe(reg)
	if err != nil {
		t.Fatalf("nativeserve.NewStaticServe: %v", err)
	}
	if fn == nil {
		t.Fatal("nativeserve.NewStaticServe returned a nil serve func")
	}
	return &staticServeSpy{fn: fn, reg: reg}
}

func (s *staticServeSpy) nativeSockets(t *testing.T, flag string) float64 {
	t.Helper()
	return s.nativeSocketsFor(flag)
}

// nativeSocketsRaw returns native_sockets{flag=on} without a *testing.T (for the
// cutover-manifest classifier); -1 on a gather error.
func (s *staticServeSpy) nativeSocketsRaw() float64 {
	return s.nativeSocketsFor("on")
}

func (s *staticServeSpy) nativeSocketsFor(flag string) float64 {
	fams, err := s.reg.Gather()
	if err != nil {
		return -1
	}
	var sum float64
	for _, mf := range fams {
		if mf.GetName() != "baml_rest_debaml_native_sockets_total" {
			continue
		}
		for _, mm := range mf.GetMetric() {
			for _, lp := range mm.GetLabel() {
				if lp.GetName() == "flag" && lp.GetValue() == flag {
					sum += mm.GetCounter().GetValue()
				}
			}
		}
	}
	return sum
}

// fixtureServer is the loopback capture server bound to the fixture's baked base_url.
// The caller MUST close() it before binding the same fixed port again (the cutover
// manifest binds one per row), so it does NOT auto-register a t.Cleanup.
type fixtureServer struct {
	srv    *httptest.Server
	count  atomic.Int64
	closed atomic.Bool
}

func newFixtureServer(t *testing.T, status int, body []byte) *fixtureServer {
	t.Helper()
	// Bind the fixed base_url port. Bind failure is FATAL (never a silent Skip): the
	// generated-route serving/shadow proofs must NOT green-by-skip on a busy runner.
	// A brief retry absorbs a lingering TIME_WAIT from the prior row (each row closes
	// its server before the next), so only a genuine external collision fails here.
	ln := listenFixturePort(t)
	fs := &fixtureServer{}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fs.count.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	_ = srv.Listener.Close()
	srv.Listener = ln
	srv.Start()
	fs.srv = srv
	return fs
}

// close shuts the server down and frees the fixed port. Idempotent.
func (fs *fixtureServer) close() {
	if fs != nil && fs.srv != nil && fs.closed.CompareAndSwap(false, true) {
		fs.srv.Close()
	}
}

// listenFixturePort binds the fixed fixture loopback port, retrying briefly to
// absorb a lingering TIME_WAIT from the just-closed prior row. A persistent bind
// failure is FATAL — the pivotal generated-route proofs never skip.
func listenFixturePort(t *testing.T) net.Listener {
	t.Helper()
	var lastErr error
	for attempt := 0; attempt < 20; attempt++ {
		ln, err := net.Listen("tcp", fixtureLoopbackAddr)
		if err == nil {
			return ln
		}
		lastErr = err
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("cannot bind fixed fixture loopback %s after retries: %v (a genuine external collision, not a skip)", fixtureLoopbackAddr, lastErr)
	return nil
}

// openAIBareString returns an OpenAI-shaped 2xx whose assistant content is a BARE
// (non-JSON) string — the natural shape of a top-level `string` return, which native
// static SAP declines (no cleanly-claimable JSON candidate), so BAML parse-only wins.
func openAIBareString(s string) []byte {
	env, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": s}}},
	})
	return env
}

// openAIStaticAnswer returns an OpenAI-shaped 2xx whose assistant content is the
// flattened StaticAnswer JSON the fixture's StaticOutputFormat return decodes.
func openAIStaticAnswer(answer string, confidence int) []byte {
	inner, _ := json.Marshal(map[string]any{"answer": answer, "confidence": confidence})
	env, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": string(inner)}}},
	})
	return env
}

// buildFixtureAdapter constructs + configures the generated fixture framework adapter:
// the flag, the installed serve callback, a loopback-allowing HTTP client (for BAML's
// decline-path send), and the requested stream mode.
func buildFixtureAdapter(t *testing.T, spy *staticServeSpy, flagOn bool, mode bamlutils.StreamMode) bamlutils.Adapter {
	t.Helper()
	fixtureInitRuntime()
	a := fixture.MakeAdapter(context.Background())
	ba, ok := a.(*fwadapter.BamlAdapter)
	if !ok {
		t.Fatalf("MakeAdapter returned %T, want *adapter.BamlAdapter", a)
	}
	ba.SetStreamMode(mode)
	ba.SetDeBAMLConfig(bamlutils.DeBAMLConfig{Enabled: flagOn})
	ba.SetNativeStaticServeComparator(spy.Serve)
	// No runtime client registry: the request uses the descriptor's BAKED default
	// client (StaticOracleClient, loopback base_url), so it is a single default-client
	// request with NO client override (att.ClientOverride == ""), which the narrow
	// static surface admits. A runtime registry — even one identical to the default —
	// is surfaced as an unproven override and declines.
	// A loopback-allowing HTTP client so BAML's decline-path send reaches the fixture
	// loopback (native uses its own exact executor over a loopback-guarded transport).
	ba.SetHTTPClient(llmhttp.NewClient(&http.Client{Transport: &http.Transport{Proxy: nil}}))
	return a
}

// driveStaticOutputFormat invokes the generated StaticOutputFormat /call and drains
// the stream, returning the final result, the outcome winner/planned engine tokens,
// and any error.
func driveStaticOutputFormat(t *testing.T, a bamlutils.Adapter, topic string) (final any, winner, planned string, drainErr error) {
	t.Helper()
	ch, err := fixture.StaticOutputFormat(a, &fixture.StaticOutputFormatInput{Topic: topic})
	if err != nil {
		return nil, "", "", err
	}
	for r := range ch {
		switch r.Kind() {
		case bamlutils.StreamResultKindFinal:
			final = r.Final()
		case bamlutils.StreamResultKindError:
			drainErr = r.Error()
		case bamlutils.StreamResultKindMetadata:
			if md := r.Metadata(); md != nil && md.Phase == bamlutils.MetadataPhaseOutcome {
				winner = md.WinnerEngine
				planned = md.PlannedEngine
			}
		}
		r.Release()
	}
	return final, winner, planned, drainErr
}

// TestGeneratedStaticServe_FlagOnServesNative proves the generated /call seam invokes
// the serve func, CLAIMS exactly one native provider RoundTrip, decodes the concrete
// StaticAnswer, and BAML never sends.
func TestGeneratedStaticServe_FlagOnServesNative(t *testing.T) {
	spy := newStaticServeSpy(t)
	server := newFixtureServer(t, http.StatusOK, openAIStaticAnswer("sunny", 9))
	defer server.close()
	a := buildFixtureAdapter(t, spy, true, bamlutils.StreamModeCall)

	final, winner, planned, err := driveStaticOutputFormat(t, a, "weather")
	if err != nil {
		t.Fatalf("generated StaticOutputFormat /call errored: %v", err)
	}
	if got := spy.calls.Load(); got != 1 {
		t.Fatalf("static serve func invoked %d times, want exactly 1 (the generated /call installs + drives it)", got)
	}
	disp, stage, reason := spy.lastDecline()
	t.Logf("serve result: disposition=%d stage=%q reason=%q", disp, stage, reason)
	if got := server.count.Load(); got != 1 {
		t.Fatalf("provider saw %d requests, want exactly 1 (native serves; BAML never sends)", got)
	}
	if got := spy.nativeSockets(t, "on"); got != 1 {
		t.Errorf("native_sockets{flag=on} = %v, want 1 (native CLAIMED one socket); serve declined at stage=%q reason=%q", got, stage, reason)
	}
	if planned != "native" {
		t.Errorf("planned_engine = %q, want native", planned)
	}
	// Deterministic: the provider returns a WELL-FORMED StaticAnswer, so native wins
	// STRUCTURALLY (native_baml_parse would mean native fell back to BAML's parse — a
	// contradiction of "serves native" for this well-formed class response).
	if winner != bamlutils.NativeStaticServeEngineNative {
		t.Errorf("winner_engine = %q, want %q (structured native match on a well-formed StaticAnswer)", winner, bamlutils.NativeStaticServeEngineNative)
	}
	// The final is the method's concrete return type decoded via DecodeNativeStaticFinal.
	if final == nil {
		t.Fatal("final result is nil; want the decoded StaticAnswer")
	}
	if got := jsonOf(t, final); got != `{"answer":"sunny","confidence":9}` {
		t.Errorf("decoded final = %s, want the native StaticAnswer{sunny,9}", got)
	}
}

// TestGeneratedStaticServe_FlagOffServesBAML proves the flag-off control: the serve
// func is NEVER invoked (hard-off), zero native sockets, and BAML serves the one
// provider request through the generated route.
func TestGeneratedStaticServe_FlagOffServesBAML(t *testing.T) {
	spy := newStaticServeSpy(t)
	server := newFixtureServer(t, http.StatusOK, openAIStaticAnswer("sunny", 9))
	defer server.close()
	a := buildFixtureAdapter(t, spy, false, bamlutils.StreamModeCall)

	_, _, planned, err := driveStaticOutputFormat(t, a, "weather")
	if err != nil {
		t.Fatalf("flag-off generated StaticOutputFormat /call errored: %v", err)
	}
	if got := spy.calls.Load(); got != 0 {
		t.Fatalf("static serve func invoked %d times with flag off, want 0 (hard-off)", got)
	}
	if got := spy.nativeSockets(t, "on"); got != 0 {
		t.Errorf("native_sockets{flag=on} = %v with flag off, want 0", got)
	}
	if got := server.count.Load(); got != 1 {
		t.Fatalf("provider saw %d requests, want exactly 1 (BAML serves)", got)
	}
	if planned != "" {
		t.Errorf("flag-off planned_engine = %q, want empty", planned)
	}
}

// TestGeneratedStaticServe_CallWithRawDeclinesToBAML proves a PRE-CLAIM decline: with
// the flag on but call-with-raw, the generated /call installs + invokes the serve func,
// which declines (Raw unproven) with ZERO native sockets, and BAML serves the one
// request.
func TestGeneratedStaticServe_CallWithRawDeclinesToBAML(t *testing.T) {
	spy := newStaticServeSpy(t)
	server := newFixtureServer(t, http.StatusOK, openAIStaticAnswer("sunny", 9))
	defer server.close()
	a := buildFixtureAdapter(t, spy, true, bamlutils.StreamModeCallWithRaw)

	_, _, _, err := driveStaticOutputFormat(t, a, "weather")
	if err != nil {
		t.Fatalf("call-with-raw generated StaticOutputFormat errored: %v", err)
	}
	if got := spy.calls.Load(); got != 1 {
		t.Fatalf("static serve func invoked %d times, want exactly 1 (flag on: the callback ran and declined)", got)
	}
	if got := spy.nativeSockets(t, "on"); got != 0 {
		t.Errorf("native_sockets{flag=on} = %v, want 0 (call-with-raw declines PRE-SOCKET)", got)
	}
	if got := server.count.Load(); got != 1 {
		t.Fatalf("provider saw %d requests, want exactly 1 (BAML serves the declined call)", got)
	}
}

// TestGeneratedStaticServe_ProviderNon2xxNoResend proves the no-post-claim-resend
// contract through the generated route: on an upstream 429 native claims one socket,
// fails, and BAML is NOT re-sent (server sees exactly one request).
func TestGeneratedStaticServe_ProviderNon2xxNoResend(t *testing.T) {
	spy := newStaticServeSpy(t)
	server := newFixtureServer(t, http.StatusTooManyRequests, []byte(`{"error":{"message":"rate limited"}}`))
	defer server.close()
	a := buildFixtureAdapter(t, spy, true, bamlutils.StreamModeCall)

	_, _, _, err := driveStaticOutputFormat(t, a, "weather")
	if err == nil {
		t.Fatal("expected an error on a provider 429 through the generated route, got nil")
	}
	if got := spy.calls.Load(); got != 1 {
		t.Fatalf("static serve func invoked %d times, want exactly 1", got)
	}
	if got := spy.nativeSockets(t, "on"); got != 1 {
		t.Errorf("native_sockets{flag=on} = %v, want 1 (socket claimed even on non-2xx)", got)
	}
	if got := server.count.Load(); got != 1 {
		t.Fatalf("provider saw %d requests, want exactly 1 (native only; NO BAML resend after claim)", got)
	}
}

// fixtureInitRuntime triggers the fixture's Once-guarded BAML runtime init (the
// lazy_runtime hack requires it; a stock eager-init client would not). Idempotent.
func fixtureInitRuntime() {
	fixturebaml.InitRuntime()
}

func jsonOf(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal final: %v", err)
	}
	return string(b)
}
