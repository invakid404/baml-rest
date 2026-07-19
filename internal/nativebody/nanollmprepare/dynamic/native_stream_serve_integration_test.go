//go:build integration && nanollm_integration

package dynamic

// De-BAML Phase 7D END-TO-END native STREAM serve proof, through the REAL generated
// dynamic StreamRequest seam (dynclient + patched BAML + the nanollm-backed canary
// StreamServer). It is the streaming twin of native_serve_integration_test.go and
// proves the four 7D merge-gate states the unit suites cannot:
//
//   - FLAG ON, admitted stream: the generated seam installs StreamConfig.NativeAttempt
//     as the native STREAM serve implementation, which admits + plan-compares and, on
//     a match, CLAIMS exactly ONE native provider request (the scripted SSE server
//     sees exactly one accepted TCP connection), streams every partial through the
//     orchestrator's EmitDelta, and returns winner_engine=native. BAML NEVER sends.
//   - EXACTLY ONE physical native request: the server's ConnState(StateNew) counter
//     is 1 — a real socket count, not a mock call count.
//   - FLAG OFF: the seam gate (DeBAMLConfig().Enabled) keeps the callback hard-off, so
//     the stream serve implementation is NEVER invoked (ZERO native FFI/socket) and
//     BAML streams the request with no winner/planned engine metadata (kill switch).
//   - DECLINED SCHEMA: a stream-unsupported schema declines PRE-TRANSPORT (no native
//     socket) and BAML streams it — winner_engine stays empty.
//
// Gated `integration && nanollm_integration` (BAML CFFI + nanollm).

import (
	"context"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/dynclient"
	"github.com/invakid404/baml-rest/internal/debaml"
	"github.com/invakid404/baml-rest/nativeserve/canary"
)

// streamServeSpy wraps the real nanollm-backed StreamServer.Serve, counting how many
// times the generated stream seam invokes it and recording the last disposition so a
// test can assert Completed vs Declined without inspecting the wire.
type streamServeSpy struct {
	fn              bamlutils.NativeStreamServeFunc
	calls           atomic.Int64
	lastDisposition atomic.Int32
}

func (s *streamServeSpy) Serve(ctx context.Context, req bamlutils.NativeStreamServeRequest) bamlutils.NativeStreamServeResult {
	s.calls.Add(1)
	res := s.fn(ctx, req)
	s.lastDisposition.Store(int32(res.Disposition))
	return res
}

func newStreamServeSpy() *streamServeSpy {
	srv := canary.NewStreamServer(nil, liveExactExecutor(), 30*time.Second, 30*time.Second)
	return &streamServeSpy{fn: srv.Serve}
}

// personFixture returns the admitted {name:string, age:int} stream fixture.
func personFixture(t *testing.T) streamDiffFixture {
	t.Helper()
	for _, f := range streamDiffFixtures() {
		if f.name == "person" {
			return f
		}
	}
	t.Fatalf("person fixture not found")
	return streamDiffFixture{}
}

// runStreamServeLeg drives a dynclient DynamicStream through the generated stream
// seam with the native stream serve comparator installed (when deBAML), against the
// scripted SSE server, and returns the partial/final counts plus the outcome-phase
// winner/planned engine tokens.
func runStreamServeLeg(t *testing.T, deBAML bool, spy *streamServeSpy, srv *scriptedSSEServer, f streamDiffFixture) (partials, finals int, winner, planned string) {
	t.Helper()
	// Install the native stream comparator + renderer + parser ALWAYS, toggling ONLY
	// the umbrella flag (WithDeBAML). This is what makes the flag-off leg a GENUINE
	// end-to-end kill-switch proof: the native seam IS installed, and the flag-off
	// leg proves the generated seam's DeBAMLConfig().Enabled gate keeps it unreached
	// (spy.calls==0) — not a vacuous assertion on an un-installed spy. Mirrors the
	// unary native_serve_integration_test harness.
	opts := []dynclient.Option{
		dynclient.WithClientMode(llmhttp.ClientModeNetHTTP),
		dynclient.WithNetHTTPClient(loopbackOracleHTTPClient()),
		dynclient.WithDeBAML(deBAML),
		dynclient.WithDeBAMLRenderer(debaml.Render),
		dynclient.WithDeBAMLParser(debaml.Parse),
		dynclient.WithNativeStreamServeComparator(spy.Serve),
	}
	c, err := dynclient.New(opts...)
	if err != nil {
		t.Fatalf("dynclient.New(deBAML=%v): %v", deBAML, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), streamDiffTimeout)
	defer cancel()
	preserve := true
	stream, serr := c.DynamicStream(ctx, dynclient.Request{
		Messages:            toDynMessages(f.messages),
		ClientRegistry:      liveOracleRegistry(srv.base()),
		OutputSchema:        f.schema,
		PreserveSchemaOrder: &preserve,
	})
	if serr != nil {
		t.Fatalf("DynamicStream(deBAML=%v): %v", deBAML, serr)
	}
	defer stream.Close()
	for {
		ev, err := stream.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("stream.Next: %v", err)
		}
		switch ev.Kind {
		case dynclient.EventPartial:
			partials++
		case dynclient.EventFinal:
			finals++
		case dynclient.EventMetadata:
			if ev.Metadata != nil && ev.Metadata.Phase == bamlutils.MetadataPhaseOutcome {
				winner, planned = ev.Metadata.WinnerEngine, ev.Metadata.PlannedEngine
			}
		}
	}
	return partials, finals, winner, planned
}

// TestNativeStreamServe_FlagOnRoutesNative_OneSocket proves flag-on native routing:
// the stream serve implementation runs once, CLAIMS exactly ONE native provider
// request (one accepted TCP connection), streams partials, returns a final, marks
// winner_engine=native, and BAML never sends.
func TestNativeStreamServe_FlagOnRoutesNative_OneSocket(t *testing.T) {
	f := personFixture(t)
	srv := newScriptedSSEServer(t, f.deltas)
	spy := newStreamServeSpy()

	partials, finals, winner, planned := runStreamServeLeg(t, true, spy, srv, f)

	if got := srv.connCount(); got != 1 {
		t.Fatalf("ONE-PHYSICAL-REQUEST VIOLATED: server accepted %d TCP connections, want exactly 1 (native serves; BAML never sends)", got)
	}
	if got := srv.reqCount(); got != 1 {
		t.Fatalf("server saw %d provider requests, want exactly 1", got)
	}
	if got := spy.calls.Load(); got != 1 {
		t.Fatalf("stream serve implementation invoked %d times, want exactly 1", got)
	}
	if got := bamlutils.NativeStreamDisposition(spy.lastDisposition.Load()); got != bamlutils.NativeStreamCompleted {
		t.Fatalf("stream serve disposition = %d, want Completed", got)
	}
	if finals != 1 {
		t.Errorf("want exactly 1 final, got %d", finals)
	}
	if partials == 0 {
		t.Errorf("native stream must emit partials, got 0")
	}
	if winner != bamlutils.NativeServeEngineNative {
		t.Errorf("winner_engine = %q, want native", winner)
	}
	if planned != "native" {
		t.Errorf("planned_engine = %q, want native", planned)
	}
}

// TestNativeStreamServe_FlagOffZeroNative is the kill-switch proof end-to-end: with
// the flag off the stream serve implementation is NEVER invoked (zero native
// FFI/socket), BAML streams the request, and no engine metadata is emitted.
func TestNativeStreamServe_FlagOffZeroNative(t *testing.T) {
	f := personFixture(t)
	srv := newScriptedSSEServer(t, f.deltas)
	spy := newStreamServeSpy()

	partials, finals, winner, planned := runStreamServeLeg(t, false, spy, srv, f)

	if got := spy.calls.Load(); got != 0 {
		t.Fatalf("KILL SWITCH VIOLATED: stream serve implementation invoked %d times with the flag off, want 0", got)
	}
	if got := srv.connCount(); got != 1 {
		t.Fatalf("flag-off BAML stream should open exactly 1 connection, got %d", got)
	}
	if finals != 1 {
		t.Errorf("flag-off BAML stream: want 1 final, got %d", finals)
	}
	if partials == 0 {
		t.Errorf("flag-off BAML stream should emit partials, got 0")
	}
	if winner != "" || planned != "" {
		t.Errorf("flag-off metadata must omit engine tokens, got winner=%q planned=%q", winner, planned)
	}
}

// TestNativeStreamServe_DeclinedSchemaFallsToBAML proves the pre-transport decline:
// a stream-unsupported schema (a bool field is outside the native stream SAP bounds)
// declines BEFORE the transport is claimed (no native socket) and BAML streams it —
// winner_engine stays empty and exactly one provider request is seen.
func TestNativeStreamServe_DeclinedSchemaFallsToBAML(t *testing.T) {
	// {name:string, flag:bool} — bool declines at SupportsNativeStream (pre-transport).
	schema := dsch(
		bamlutils.OrderedKV("name", dstr()),
		bamlutils.OrderedKV("flag", &bamlutils.DynamicProperty{Type: "bool"}),
	)
	f := streamDiffFixture{
		name:     "declined_bool",
		schema:   schema,
		messages: userMsg("Give a name and a flag."),
		deltas:   contentDeltas(`{`, `"name": "`, `Ada`, `", "flag": `, `true`, `}`),
	}
	srv := newScriptedSSEServer(t, f.deltas)
	spy := newStreamServeSpy()

	partials, finals, winner, _ := runStreamServeLeg(t, true, spy, srv, f)

	if got := spy.calls.Load(); got != 1 {
		t.Fatalf("stream serve implementation invoked %d times, want exactly 1 (admits then declines)", got)
	}
	if got := bamlutils.NativeStreamDisposition(spy.lastDisposition.Load()); got != bamlutils.NativeStreamDeclined {
		t.Fatalf("stream serve disposition = %d, want Declined (pre-transport)", got)
	}
	if got := srv.reqCount(); got != 1 {
		t.Fatalf("declined→BAML must see exactly 1 provider request (BAML's), got %d", got)
	}
	if finals != 1 {
		t.Errorf("declined→BAML: want 1 final, got %d", finals)
	}
	if partials == 0 {
		t.Errorf("declined→BAML should emit partials, got 0")
	}
	if winner != "" {
		t.Errorf("declined→BAML winner_engine must be empty, got %q", winner)
	}
}
