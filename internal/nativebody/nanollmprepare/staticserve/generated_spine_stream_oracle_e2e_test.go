//go:build integration && nanollm_integration

package staticserve

// ExecBridge-U1s / M3e-B — the IN-PROCESS cross-boundary STREAM proof, and the local twin of
// the booted S3b standard-artifact differential. It crosses ALL the real boundaries rather
// than stopping at a fake function:
//
//  1. a REAL standard generated static method installs the U1s oracle seam
//     (standardspineoracle.NewStaticStreamServeFromExecutor over a real
//     spine.StreamExecutor — the SAME adapter production wires);
//  2. the composite drives spine.StreamWithOracle, which runs the LIVE BAML StreamRequest
//     plan compare before its claim, opens ONE provider stream, and on every structured
//     cadence tick and on the final compares the native parse against BAML's parse of the
//     SAME accumulated prefix;
//  3. the generated method's OWN closures are what BAML's side of that comparison runs:
//     ParseStream.<Method> per prefix and Parse.<Method> for the final;
//  4. the COMPLETE ORDERED public-event trace is compared, event for event, against the
//     flag-off stock-BAML leg over the SAME SSE corpus.
//
// The spine is built from the FIXTURE's OWN baml_src, so its baked native plan byte-matches
// the fixture's live BAML plan and the attempt CLAIMS — which is what makes every assertion
// below about the served path rather than about a decline.

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/internal/nativebody/nanollmprepare/standardspineoracle"
	"github.com/invakid404/baml-rest/internal/nativespine"
	"github.com/invakid404/baml-rest/internal/nativespinejsonfixture"
	"github.com/invakid404/baml-rest/nativeserve/spine"

	fixture "github.com/invakid404/baml-rest/internal/nativeprompt/testdata/staticserve_fixture/generated"
	fwadapter "github.com/invakid404/baml-rest/internal/nativeprompt/testdata/staticserve_fixture/generated/adapter"
)

// fixtureBamlSrcStreamOracle builds the U1s stream composite over an executor whose baked
// native plan for StaticRecursiveAliasJSON BYTE-MATCHES the fixture's live BAML StreamRequest
// plan — by building the spine from the FIXTURE's OWN baml_src, exactly as
// scripts/build-s3b-static-fixture-artifact.sh does for the booted proof. It returns the
// counting serve func and the concrete executor for its bounded socket counter.
func fixtureBamlSrcStreamOracle(t *testing.T) (bamlutils.NativeStaticStreamOracleServeFunc, *spine.StreamExecutor, *int) {
	t.Helper()
	dir := filepath.Join("..", "..", "..", "nativeprompt", "testdata", "staticserve_fixture", "baml_src")
	sources := map[string]string{}
	for _, name := range []string{"clients.baml", "types.baml", "functions.baml"} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read fixture baml_src %s: %v", name, err)
		}
		sources[name] = string(b)
	}
	proj, err := nativespine.BuildFromSource(sources)
	if err != nil {
		t.Fatalf("BuildFromSource(fixture baml_src): %v", err)
	}
	exec, err := spine.NewPopulationStreamExecutor(proj, []spine.StreamRegistration{
		{Binding: nativespinejsonfixture.StreamBinding(), BuildMethod: nativespinejsonfixture.BuildMethod},
	}, nil)
	if err != nil {
		t.Fatalf("NewPopulationStreamExecutor(fixture baml_src): %v", err)
	}
	inner, err := standardspineoracle.NewStaticStreamServeFromExecutor(prometheus.NewRegistry(), exec)
	if err != nil {
		t.Fatalf("NewStaticStreamServeFromExecutor: %v", err)
	}
	calls := 0
	fn := func(ctx context.Context, inv bamlutils.NativeStaticStreamOracleInvocation, emit bamlutils.NativeSpineStreamEmit) bamlutils.NativeStaticStreamOracleServeResult {
		calls++
		return inner(ctx, inv, emit)
	}
	return fn, exec, &calls
}

// buildFixtureStreamOracleAdapter wires the fixture adapter with the U1s ORACLE comparator
// and NOTHING else native, so the generated seam's oracle-first resolution is what installs
// the lane. The legacy stream comparator is deliberately left nil: if the generated seam
// resolved it first, the oracle would never run and every assertion below would be about
// the wrong lane.
func buildFixtureStreamOracleAdapter(t *testing.T, serveFn bamlutils.NativeStaticStreamOracleServeFunc, flagOn bool, mode bamlutils.StreamMode) bamlutils.Adapter {
	t.Helper()
	fixtureInitRuntime()
	a := fixture.MakeAdapter(context.Background())
	ba, ok := a.(*fwadapter.BamlAdapter)
	if !ok {
		t.Fatalf("MakeAdapter returned %T, want *adapter.BamlAdapter", a)
	}
	ba.SetStreamMode(mode)
	ba.SetDeBAMLConfig(bamlutils.DeBAMLConfig{Enabled: flagOn})
	ba.SetNativeStaticStreamOracleServeComparator(serveFn)
	ba.SetHTTPClient(llmhttp.NewClient(&http.Client{Transport: &http.Transport{Proxy: nil}}))
	return a
}

// oracleReplayTrace drives ONE leg of the differential over the given SSE corpus.
func oracleReplayTrace(t *testing.T, events []string, flagOn bool, mode bamlutils.StreamMode) (streamTrace, int64, *spine.StreamExecutor, int) {
	t.Helper()
	serveFn, exec, calls := fixtureBamlSrcStreamOracle(t)
	server := newFixtureStreamServer(t, events)
	a := buildFixtureStreamOracleAdapter(t, serveFn, flagOn, mode)
	ch, err := fixture.StaticRecursiveAliasJSON(a, &fixture.StaticRecursiveAliasJsonInput{Topic: "arbitrary json"})
	tr := drainStreamTrace(t, ch, err)
	hits := server.count.Load()
	server.close()
	return tr, hits, exec, *calls
}

// oracleStreamCorpus is a fragmented SSE corpus: the answer arrives across several content
// deltas, so the differential spans several structured ticks — including prefixes that are
// not yet parseable — rather than one all-at-once frame. A single-frame corpus cannot
// observe per-tick behaviour at all, which is the whole risk of this slice.
func oracleStreamCorpus() []string {
	return contentSSE([]string{"[1,", `"x",`, "true]"}, nil)
}

// TestStreamOracleComposite_EventExactVsStockBAML is the headline in-process proof: with the
// flag on, the U1s lane serves the exact cohort and the COMPLETE ORDERED public-event trace
// is identical to the stock-BAML leg over the same SSE.
func TestStreamOracleComposite_EventExactVsStockBAML(t *testing.T) {
	native, nativeHits, exec, nativeCalls := oracleReplayTrace(t, oracleStreamCorpus(), true, bamlutils.StreamModeStream)
	baml, bamlHits, _, bamlCalls := oracleReplayTrace(t, oracleStreamCorpus(), false, bamlutils.StreamModeStream)

	if native.drainer != nil {
		t.Fatalf("the U1s stream failed: %v", native.drainer)
	}
	if nativeCalls != 1 {
		t.Fatalf("the oracle composite was invoked %d time(s) with the flag on, want exactly 1", nativeCalls)
	}
	if bamlCalls != 0 {
		t.Errorf("the oracle composite was invoked %d time(s) with the flag OFF; the seam must be hard-off", bamlCalls)
	}
	// NON-VACUITY: the corpus really produced several structured frames, or an event-exact
	// comparison of two nearly-empty traces would prove nothing.
	structured := 0
	for _, e := range baml.events {
		if e.kind == bamlutils.StreamResultKindStream {
			structured++
		}
	}
	if structured < 2 {
		t.Fatalf("the stock leg published %d structured frame(s); the per-tick comparison needs several", structured)
	}

	assertTraceEqual(t, "U1s /stream vs stock BAML", native, baml)

	if native.planned != "native" || native.winner != bamlutils.NativeStaticServeEngineNative {
		t.Errorf("planned=%q winner=%q, want the native winner — the plan matched and every prefix and the final agreed",
			native.planned, native.winner)
	}
	if nativeHits != 1 {
		t.Errorf("the provider saw %d request(s) on the native leg, want EXACTLY 1 (one DoStream, no resend)", nativeHits)
	}
	if bamlHits != 1 {
		t.Errorf("the provider saw %d request(s) on the stock leg, want exactly 1", bamlHits)
	}
	if snap := exec.Metrics().Snapshot(); snap.Sockets != 1 || snap.Claims != 1 || snap.Successes != 1 {
		t.Errorf("executor counters = %+v, want exactly one claim, one socket and one success", snap)
	}
}

// TestStreamOracleComposite_EventExactOnStreamWithRaw is the same proof on the second public
// streaming surface. /stream-with-raw carries raw through a DIFFERENT cadence branch — raw
// flows on ticks that release no structured partial — so a lane that got plain /stream right
// can still get this one wrong.
func TestStreamOracleComposite_EventExactOnStreamWithRaw(t *testing.T) {
	native, nativeHits, _, nativeCalls := oracleReplayTrace(t, oracleStreamCorpus(), true, bamlutils.StreamModeStreamWithRaw)
	baml, _, _, _ := oracleReplayTrace(t, oracleStreamCorpus(), false, bamlutils.StreamModeStreamWithRaw)

	if native.drainer != nil {
		t.Fatalf("the U1s /stream-with-raw stream failed: %v", native.drainer)
	}
	if nativeCalls != 1 {
		t.Fatalf("the oracle composite was invoked %d time(s), want exactly 1", nativeCalls)
	}
	// NON-VACUITY on the channel this surface exists for.
	sawRaw := false
	for _, e := range baml.events {
		if e.raw != "" {
			sawRaw = true
			break
		}
	}
	if !sawRaw {
		t.Fatal("the stock /stream-with-raw leg carried no raw text; the raw half of the comparison would be vacuous")
	}
	assertTraceEqual(t, "U1s /stream-with-raw vs stock BAML", native, baml)
	if native.winner != bamlutils.NativeStaticServeEngineNative {
		t.Errorf("winner = %q, want native", native.winner)
	}
	if nativeHits != 1 {
		t.Errorf("the provider saw %d request(s) on the native leg, want exactly 1", nativeHits)
	}
}

// TestStreamOracleComposite_PlanMismatchDeclinesToBAML is the cross-boundary DECLINE proof:
// a spine whose baked plan does NOT match the fixture's live BAML StreamRequest plan (built
// from the jsonalias corpus, a different client/model/prompt) declines PRE-SOCKET, the
// adapter maps it to a decline, and the generated seam runs BAML for the same request — one
// provider send, zero native sockets, and the public answer unchanged.
func TestStreamOracleComposite_PlanMismatchDeclinesToBAML(t *testing.T) {
	proj, err := nativespine.BuildFromSource(nativespine.JSONAliasFixtureSources)
	if err != nil {
		t.Fatalf("BuildFromSource(jsonalias): %v", err)
	}
	exec, err := spine.NewPopulationStreamExecutor(proj, []spine.StreamRegistration{
		{Binding: nativespinejsonfixture.StreamBinding(), BuildMethod: nativespinejsonfixture.BuildMethod},
	}, nil)
	if err != nil {
		t.Fatalf("NewPopulationStreamExecutor(jsonalias): %v", err)
	}
	inner, err := standardspineoracle.NewStaticStreamServeFromExecutor(prometheus.NewRegistry(), exec)
	if err != nil {
		t.Fatalf("NewStaticStreamServeFromExecutor: %v", err)
	}
	calls := 0
	serveFn := func(ctx context.Context, inv bamlutils.NativeStaticStreamOracleInvocation, emit bamlutils.NativeSpineStreamEmit) bamlutils.NativeStaticStreamOracleServeResult {
		calls++
		return inner(ctx, inv, emit)
	}

	server := newFixtureStreamServer(t, oracleStreamCorpus())
	a := buildFixtureStreamOracleAdapter(t, serveFn, true, bamlutils.StreamModeStream)
	ch, cerr := fixture.StaticRecursiveAliasJSON(a, &fixture.StaticRecursiveAliasJsonInput{Topic: "arbitrary json"})
	tr := drainStreamTrace(t, ch, cerr)
	hits := server.count.Load()
	server.close()

	if tr.drainer != nil {
		t.Fatalf("a declined U1s stream must be served by BAML, not fail: %v", tr.drainer)
	}
	if calls != 1 {
		t.Fatalf("the oracle composite was invoked %d time(s), want exactly 1 before the decline", calls)
	}
	if tr.winner == bamlutils.NativeStaticServeEngineNative {
		t.Errorf("winner = %q on a plan mismatch; BAML must serve a declined stream", tr.winner)
	}
	if hits != 1 {
		t.Errorf("the provider saw %d request(s), want exactly 1 — a pre-socket decline must add nothing to the wire", hits)
	}
	if snap := exec.Metrics().Snapshot(); snap.Sockets != 0 || snap.Claims != 0 {
		t.Errorf("a pre-socket decline claimed/opened a socket: %+v", snap)
	}
	// The public answer is still the streamed one.
	var final string
	for _, e := range tr.events {
		if e.kind == bamlutils.StreamResultKindFinal {
			final = e.final
		}
	}
	if final != `[1,"x",true]` {
		t.Errorf("the BAML-served final = %q, want %q", final, `[1,"x",true]`)
	}
}
