//go:build integration && nanollm_integration

package staticserve

// ExecBridge-U1c REQUIRED cross-boundary decline->BAML proof (design §11 "Required
// spine-specific fallback proof"). It crosses ALL THREE boundaries rather than stopping
// at a fake function:
//
//  1. a REAL standard generated static method installs the SPINE-backed
//     NativeStaticServeFunc (standardspineoracle.NewStaticServeFromExecutor over a real
//     spine.UnaryExecutor — the SAME adapter production wires);
//  2. the spine returns NativeSpineDeclinedPreSocket (a registry miss for a non-U1
//     method, and a live plan mismatch for the U1 method against a different baked plan);
//  3. the adapter maps that to NativeStaticServeDeclined;
//  4. buildrequest.CallConfig invokes the ORIGINAL BAML attempt;
//  5. capture shows ZERO native sockets (the spine executor's own counter) plus EXACTLY
//     ONE BAML provider request, and a successful public response.
//
// This is the proof of the outer-composite sentence at bamlutils/native_spine_unary.go.
// The native-only e2e proves the OPPOSITE artifact policy (decline is terminal — no
// fallback) and is not evidence for this standard composite.

import (
	"context"
	"encoding/json"
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

// spineServeFixture builds the SPINE-backed NativeStaticServeFunc the production standard
// composite installs, over a real spine executor admitting exactly the U1 method
// (StaticRecursiveAliasJSON, the JSONOracle-client jsonalias fixture — a DIFFERENT baked
// plan than the fixture's StaticOracleClient, so the live plan compare mismatches). It
// returns the counting serve func and the concrete executor for its bounded socket
// counter.
func spineServeFixture(t *testing.T) (bamlutils.NativeStaticServeFunc, *spine.UnaryExecutor, *int) {
	t.Helper()
	proj, err := nativespine.BuildFromSource(nativespine.JSONAliasFixtureSources)
	if err != nil {
		t.Fatalf("BuildFromSource(jsonalias): %v", err)
	}
	exec, err := spine.NewPopulationExecutor(proj, []spine.UnaryRegistration{
		{Binding: nativespinejsonfixture.Binding(), BuildMethod: nativespinejsonfixture.BuildMethod},
	}, nil)
	if err != nil {
		t.Fatalf("NewPopulationExecutor: %v", err)
	}
	inner, err := standardspineoracle.NewStaticServeFromExecutor(prometheus.NewRegistry(), exec)
	if err != nil {
		t.Fatalf("NewStaticServeFromExecutor: %v", err)
	}
	calls := 0
	fn := func(ctx context.Context, inv bamlutils.NativeStaticInvocation) bamlutils.NativeStaticServeResult {
		calls++
		return inner(ctx, inv)
	}
	return fn, exec, &calls
}

// fixtureBamlSrcSpineServe builds the spine-backed serve func over an executor whose baked
// native plan for StaticRecursiveAliasJSON BYTE-MATCHES the fixture's live BAML plan — by
// building the spine from the FIXTURE's OWN baml_src (the StaticOracleClient project at the
// :17654 loopback), NOT JSONAliasFixtureSources, which mismatches on client/model/prompt by
// design. This is the in-process analog of scripts/build-s3b-static-fixture-artifact.sh
// (introspect over the same baml_src), so native CAN win here and a single-fact near-miss
// flip through the real adapter seam is DISCRIMINATING (the pre-fix synthesizing code would
// claim on the match).
func fixtureBamlSrcSpineServe(t *testing.T) (bamlutils.NativeStaticServeFunc, *spine.UnaryExecutor, *int) {
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
	exec, err := spine.NewPopulationExecutor(proj, []spine.UnaryRegistration{
		{Binding: nativespinejsonfixture.Binding(), BuildMethod: nativespinejsonfixture.BuildMethod},
	}, nil)
	if err != nil {
		t.Fatalf("NewPopulationExecutor(fixture baml_src): %v", err)
	}
	inner, err := standardspineoracle.NewStaticServeFromExecutor(prometheus.NewRegistry(), exec)
	if err != nil {
		t.Fatalf("NewStaticServeFromExecutor: %v", err)
	}
	calls := 0
	fn := func(ctx context.Context, inv bamlutils.NativeStaticInvocation) bamlutils.NativeStaticServeResult {
		calls++
		return inner(ctx, inv)
	}
	return fn, exec, &calls
}

// driveStaticRecursiveAliasJSON drives the U1 method through the generated seam and drains
// its stream to the final value (or the first stream error).
func driveStaticRecursiveAliasJSON(t *testing.T, a bamlutils.Adapter, topic string) (any, error) {
	t.Helper()
	ch, err := fixture.StaticRecursiveAliasJSON(a, &fixture.StaticRecursiveAliasJsonInput{Topic: topic})
	if err != nil {
		return nil, err
	}
	var final any
	var drainErr error
	for r := range ch {
		switch r.Kind() {
		case bamlutils.StreamResultKindFinal:
			final = r.Final()
		case bamlutils.StreamResultKindError:
			drainErr = r.Error()
		}
		r.Release()
	}
	return final, drainErr
}

// TestSpineComposite_MatchingPlanNativeWins is the POSITIVE control for the near-miss below:
// with the fixture's own baml_src baked into the spine, the live BAML plan compare MATCHES,
// so the exact-U1 request CLAIMS and native serves the one socket (no BAML fallback). This is
// what makes the call-with-raw near-miss discriminating — proving native WOULD win absent the
// one flipped fact.
func TestSpineComposite_MatchingPlanNativeWins(t *testing.T) {
	serveFn, exec, calls := fixtureBamlSrcSpineServe(t)
	server := newFixtureServer(t, http.StatusOK, openAIJSONMap())
	defer server.close()
	a := buildFixtureAdapterWithServe(t, serveFn, true) // StreamModeCall: final, no raw

	final, drainErr := driveStaticRecursiveAliasJSON(t, a, "weather")
	if drainErr != nil {
		t.Fatalf("StaticRecursiveAliasJSON through the matching-plan spine composite errored: %v", drainErr)
	}
	if *calls != 1 {
		t.Fatalf("spine serve func invoked %d times, want exactly 1", *calls)
	}
	if snap := exec.Metrics().Snapshot(); snap.Claims != 1 || snap.Sockets != 1 || snap.Successes != 1 || snap.Declines != 0 {
		t.Errorf("spine metrics = %+v; want a native WIN (claims=1 sockets=1 successes=1 declines=0) — the fixture-baml_src plan must byte-match", snap)
	}
	if got := server.count.Load(); got != 1 {
		t.Fatalf("provider saw %d requests, want exactly 1 (native's single socket, no BAML resend)", got)
	}
	if final == nil {
		t.Fatal("final is nil; native should have served the decoded JSON")
	}
}

// TestSpineComposite_CallWithRawNearMissDeclinesToBAML drives a call-with-raw NEAR-MISS
// through the REAL generated seam (installNativeStaticCall reads adapter.StreamMode().
// NeedsRaw() -> inv.Raw, adapter-authoritative). Even though the baked plan MATCHES (native
// would otherwise win, per the positive control), the forwarded raw fact declines at the MODE
// gate PRE-SOCKET, and BAML serves the one request. This BITES the pre-fix synthesizing code,
// which dropped inv.Raw, reached the matching plan, CLAIMED, and opened a native socket.
func TestSpineComposite_CallWithRawNearMissDeclinesToBAML(t *testing.T) {
	serveFn, exec, calls := fixtureBamlSrcSpineServe(t)
	server := newFixtureServer(t, http.StatusOK, openAIJSONMap())
	defer server.close()
	a := buildFixtureAdapterWithServe(t, serveFn, true)
	// The ONLY change vs the native-win control: the adapter reports /call-with-raw.
	a.(*fwadapter.BamlAdapter).SetStreamMode(bamlutils.StreamModeCallWithRaw)

	_, drainErr := driveStaticRecursiveAliasJSON(t, a, "weather")
	if drainErr != nil {
		t.Fatalf("call-with-raw through the spine composite errored: %v", drainErr)
	}
	if *calls != 1 {
		t.Fatalf("spine serve func invoked %d times, want exactly 1", *calls)
	}
	if snap := exec.Metrics().Snapshot(); snap.Sockets != 0 || snap.Claims != 0 || snap.Declines != 1 {
		t.Errorf("spine metrics = %+v; want a pre-socket MODE-gate decline (sockets=0 claims=0 declines=1). A native socket here means the raw fact was NOT forwarded — the pre-fix defect.", snap)
	}
	if got := server.count.Load(); got != 1 {
		t.Fatalf("provider saw %d requests, want exactly 1 (raw near-miss declined pre-socket; BAML served)", got)
	}
}

// buildFixtureAdapterWithServe mirrors buildFixtureAdapter but installs an arbitrary
// serve func (here the spine-backed one) as the generated seam's static serve comparator.
func buildFixtureAdapterWithServe(t *testing.T, serveFn bamlutils.NativeStaticServeFunc, flagOn bool) bamlutils.Adapter {
	t.Helper()
	fixtureInitRuntime()
	a := fixture.MakeAdapter(context.Background())
	ba, ok := a.(*fwadapter.BamlAdapter)
	if !ok {
		t.Fatalf("MakeAdapter returned %T, want *adapter.BamlAdapter", a)
	}
	ba.SetStreamMode(bamlutils.StreamModeCall)
	ba.SetDeBAMLConfig(bamlutils.DeBAMLConfig{Enabled: flagOn})
	ba.SetNativeStaticServeComparator(serveFn)
	ba.SetHTTPClient(llmhttp.NewClient(&http.Client{Transport: &http.Transport{Proxy: nil}}))
	return a
}

// openAIJSONMap returns an OpenAI-shaped 2xx whose assistant content is a JSON object,
// which the fixture's `-> JSON` five-arm alias return parses.
func openAIJSONMap() []byte {
	inner, _ := json.Marshal(map[string]any{"weather": "sunny"})
	env, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": string(inner)}}},
	})
	return env
}

// TestSpineComposite_RegistryMissDeclinesToBAML drives a NON-U1 generated method
// (StaticOutputFormat, a StaticAnswer class return) through the spine-backed serve func.
// The spine registry admits only StaticRecursiveAliasJSON, so the method MISSES at the
// registry stage -> a pre-socket decline -> the adapter maps it to a BAML fallback, and
// BAML serves the one request. Zero native sockets.
func TestSpineComposite_RegistryMissDeclinesToBAML(t *testing.T) {
	serveFn, exec, calls := spineServeFixture(t)
	server := newFixtureServer(t, http.StatusOK, openAIStaticAnswer("sunny", 9))
	defer server.close()
	a := buildFixtureAdapterWithServe(t, serveFn, true)

	final, _, planned, err := driveStaticOutputFormat(t, a, "weather")
	if err != nil {
		t.Fatalf("StaticOutputFormat through the spine composite errored: %v", err)
	}
	if *calls != 1 {
		t.Fatalf("spine serve func invoked %d times, want exactly 1 (the generated /call installs + drives it)", *calls)
	}
	if snap := exec.Metrics().Snapshot(); snap.Sockets != 0 || snap.Claims != 0 || snap.Declines != 1 {
		t.Errorf("spine metrics = %+v; want a single pre-socket decline (sockets=0 claims=0 declines=1)", snap)
	}
	if got := server.count.Load(); got != 1 {
		t.Fatalf("provider saw %d requests, want exactly 1 (spine declined; BAML served)", got)
	}
	if planned != "native" {
		t.Errorf("planned_engine = %q, want native (a serve callback was installed)", planned)
	}
	if final == nil {
		t.Fatal("final is nil; BAML should have served the decoded StaticAnswer")
	}
	if got := jsonOf(t, final); got != `{"answer":"sunny","confidence":9}` {
		t.Errorf("BAML-served final = %s, want StaticAnswer{sunny,9}", got)
	}
}

// TestSpineComposite_PlanMismatchDeclinesToBAML drives the U1 method
// (StaticRecursiveAliasJSON) through the spine composite. The spine ADMITS the method but
// its baked native plan (the JSONOracle client) does not byte-match the fixture's live
// BAML plan (StaticOracleClient), so the live plan compare declines PRE-SOCKET and BAML
// serves the one request. Zero native sockets.
func TestSpineComposite_PlanMismatchDeclinesToBAML(t *testing.T) {
	serveFn, exec, calls := spineServeFixture(t)
	server := newFixtureServer(t, http.StatusOK, openAIJSONMap())
	defer server.close()
	a := buildFixtureAdapterWithServe(t, serveFn, true)

	ch, err := fixture.StaticRecursiveAliasJSON(a, &fixture.StaticRecursiveAliasJsonInput{Topic: "weather"})
	if err != nil {
		t.Fatalf("StaticRecursiveAliasJSON install errored: %v", err)
	}
	var final any
	var drainErr error
	for r := range ch {
		switch r.Kind() {
		case bamlutils.StreamResultKindFinal:
			final = r.Final()
		case bamlutils.StreamResultKindError:
			drainErr = r.Error()
		}
		r.Release()
	}
	if drainErr != nil {
		t.Fatalf("StaticRecursiveAliasJSON through the spine composite errored: %v", drainErr)
	}
	if *calls != 1 {
		t.Fatalf("spine serve func invoked %d times, want exactly 1", *calls)
	}
	if snap := exec.Metrics().Snapshot(); snap.Sockets != 0 || snap.Claims != 0 || snap.Declines != 1 {
		t.Errorf("spine metrics = %+v; want a single pre-socket plan-mismatch decline (sockets=0 claims=0 declines=1)", snap)
	}
	if got := server.count.Load(); got != 1 {
		t.Fatalf("provider saw %d requests, want exactly 1 (plan mismatch declined; BAML served)", got)
	}
	if final == nil {
		t.Fatal("final is nil; BAML should have served the decoded JSON")
	}
	// Assert the DECODED content, not just non-nil, so a wrong-but-non-nil BAML result cannot
	// pass: BAML must decode the exact five-arm JSON alias from openAIJSONMap's assistant body.
	if got := jsonOf(t, final); got != `{"weather":"sunny"}` {
		t.Errorf("BAML-served final = %s, want the decoded JSON alias {\"weather\":\"sunny\"}", got)
	}
}
