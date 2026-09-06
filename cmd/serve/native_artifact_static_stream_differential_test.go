//go:build subprocess && nativeartifactproof

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/trustedclients"
	"github.com/invakid404/baml-rest/internal/artifactprofile"
	"github.com/invakid404/baml-rest/pool"
	"github.com/invakid404/baml-rest/workerplugin"
)

// ExecBridge-U1s / M3e-B — the BOOTED STANDARD-ARTIFACT STREAM DIFFERENTIAL.
//
// This is the acceptance proof for the default-serve flip. It boots the SHIPPED
// serve-profile worker (the S3b static fixture artifact: same entrypoint, same tag set,
// same -ldflags attestation stamp, one extra tag selecting a real static BAML project's
// method table) and drives the PUBLIC `/stream` and `/stream-with-raw` surfaces for the
// exact-U1s method against ONE deterministic loopback SSE corpus, twice:
//
//	flag OFF -> stock BAML v0.223 end to end
//	flag ON  -> the U1s lane: one native socket, a live StreamRequest plan match, the
//	            per-prefix + final BAML oracle, winner=native
//
// and requires the COMPLETE ORDERED PUBLIC TRANSCRIPT to be identical: the same structured
// frames in the same order with byte-identical Data, the same raw and reasoning channels,
// the same final, and one upstream request in each leg.
//
// # Why the transcript, and not just the final
//
// The whole risk of this slice is per-TICK: a native lane that produces the right final
// while emitting a different number of partials, or the same partials in a different shape,
// has changed what every streaming client sees. Comparing only the final would pass for
// exactly that regression. The corpus is deliberately fragmented into several content
// deltas so the comparison spans several structured ticks rather than one.
//
// It reuses the artifact the S3a/S3b step already builds and one fixed loopback provider,
// so it adds no CGO artifact build to the lane.

// staticStreamFragments splits the fixture method's JSON answer across several SSE content
// deltas, so the differential observes a sequence of structured ticks — including prefixes
// that are not yet parseable — rather than a single all-at-once frame.
var staticStreamFragments = []string{
	`{"answer"`,
	`:"ok"`,
	`,"detail":`,
	`{"n":1}}`,
}

// staticStreamChunks renders those fragments as the OpenAI chunk payloads the shared
// loopback provider writes, terminated by a stop chunk and [DONE].
func staticStreamChunks() []string {
	out := make([]string, 0, len(staticStreamFragments)+2)
	for _, f := range staticStreamFragments {
		b, _ := json.Marshal(map[string]any{
			"id": "static-stream-proof", "object": "chat.completion.chunk",
			"choices": []any{map[string]any{
				"index": 0, "delta": map[string]any{"content": f}, "finish_reason": nil,
			}},
		})
		out = append(out, string(b))
	}
	out = append(out,
		`{"id":"static-stream-proof","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`[DONE]`)
	return out
}

// staticStreamFrame is ONE public frame the worker published, reduced to the bytes and
// channels a client would actually receive. EVERY kind is recorded — including heartbeats
// and metadata — because the acceptance criterion is the COMPLETE ORDERED transcript, and a
// recorder that quietly drops a kind cannot observe a lane that stopped emitting it. (The
// first version of this file dropped heartbeat and metadata frames, which is precisely why a
// missing 2xx-liveness signal did not bite here.)
type staticStreamFrame struct {
	kind      workerplugin.StreamResultKind
	data      string
	raw       string
	reasoning string
}

func kindName(k workerplugin.StreamResultKind) string {
	switch k {
	case workerplugin.StreamResultKindStream:
		return "stream"
	case workerplugin.StreamResultKindFinal:
		return "final"
	case workerplugin.StreamResultKindError:
		return "error"
	case workerplugin.StreamResultKindHeartbeat:
		return "heartbeat"
	case workerplugin.StreamResultKindMetadata:
		return "metadata"
	default:
		return fmt.Sprintf("kind(%d)", k)
	}
}

func (f staticStreamFrame) String() string {
	return fmt.Sprintf("{kind:%s data:%s raw:%q reasoning:%q}", kindName(f.kind), f.data, f.raw, f.reasoning)
}

// staticStreamResult is one leg of the differential.
type staticStreamResult struct {
	frames []staticStreamFrame
	// heartbeats counts the 2xx-liveness frames, and metadata holds each metadata frame's
	// raw payload in order. Both are recorded separately from frames so an arm can assert
	// on them directly as well as through the ordered comparison.
	heartbeats       int
	metadata         []string
	final            string
	providerRequests int64
	metrics          routeProofResult
	artifactProfile  string
	artifactID       string
}

// structuredFrames returns only the partial frames, in order.
func (r staticStreamResult) structuredFrames() []staticStreamFrame {
	out := []staticStreamFrame{}
	for _, f := range r.frames {
		if f.kind == workerplugin.StreamResultKindStream {
			out = append(out, f)
		}
	}
	return out
}

// runStaticStreamProof boots the STATIC-capable artifact and drives ONE public streaming
// request through the real pool at the worker boundary the `/stream` handler uses.
func runStaticStreamProof(t *testing.T, provider *routeProofProvider, mode bamlutils.StreamMode, flagOn bool) staticStreamResult {
	t.Helper()
	bin := staticFixtureBinary(t)
	before := provider.calls.Load()

	t.Setenv("BAML_REST_USE_DEBAML", fmt.Sprintf("%t", flagOn))
	t.Setenv(trustedclients.EnvVar, staticFixtureDeclaration(feV1RouteFingerprint))

	workerPool, err := pool.New(&pool.Config{
		WorkerPath:         bin,
		PoolSize:           1,
		LogOutput:          io.Discard,
		WorkerStartTimeout: 120 * time.Second,
		// The shipped host arms this for a native-stream-capable artifact with the flag
		// on. Arming it in BOTH legs keeps the pool's behaviour a constant of the
		// differential rather than a second variable moving with the flag.
		DisableStreamInfrastructureRetries: true,
	})
	if err != nil {
		t.Fatalf("pool.New over the static-capable artifact: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = workerPool.Shutdown(ctx)
	})

	// The PUBLIC static request body, byte for byte, without a client_registry: the exact
	// cohort serves only the descriptor's default client, and a request registry override
	// declines pre-socket — so this is the shape a default-client deployment request has.
	input := []byte(staticFixtureBody(false))

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	results, err := workerPool.CallStream(ctx, staticFixtureJSONMethod, input, mode)
	if err != nil {
		t.Fatalf("CallStream on the public %v surface: %v", mode, err)
	}

	out := staticStreamResult{}
	for res := range results {
		if res == nil {
			continue
		}
		switch res.Kind {
		case workerplugin.StreamResultKindError:
			t.Fatalf("the %v stream returned an error frame: %v", mode, res.Error)
		case workerplugin.StreamResultKindStream, workerplugin.StreamResultKindFinal:
			out.frames = append(out.frames, staticStreamFrame{
				kind: res.Kind, data: string(res.Data), raw: res.Raw, reasoning: res.Reasoning,
			})
			if res.Kind == workerplugin.StreamResultKindFinal {
				out.final = string(res.Data)
			}
		case workerplugin.StreamResultKindHeartbeat:
			// The 2xx-liveness signal. Its PRESENCE and POSITION are part of the public
			// transcript — the pool's hung detector is what consumes it — so it is recorded
			// with no payload rather than dropped.
			out.frames = append(out.frames, staticStreamFrame{kind: res.Kind})
			out.heartbeats++
		case workerplugin.StreamResultKindMetadata:
			// Metadata ORDER is compared; its payload is not, because winner_engine
			// legitimately differs between the two legs (that difference is the whole
			// point of the flag). The routing facts that must NOT differ are asserted
			// explicitly by the caller.
			out.frames = append(out.frames, staticStreamFrame{kind: res.Kind})
			out.metadata = append(out.metadata, string(res.Data))
		default:
			// An unhandled kind is a transcript this recorder cannot compare. Failing is
			// the only honest response: silently dropping it is how a lane stops emitting
			// something and no test notices.
			t.Fatalf("the %v stream published an unhandled frame kind %s; the complete-transcript comparison cannot silently drop it", mode, kindName(res.Kind))
		}
	}
	out.providerRequests = provider.calls.Load() - before
	out.metrics = newRouteProofResult()
	readArtifactDeBAMLMetrics(t, workerPool, &out.metrics)
	out.artifactProfile = out.metrics.artifactProfile
	out.artifactID = out.metrics.artifactID
	return out
}

// assertStaticStreamTranscriptsMatch compares the COMPLETE ordered public transcript of the
// two legs frame by frame, including kind, exact Data bytes, raw and reasoning.
func assertStaticStreamTranscriptsMatch(t *testing.T, label string, stock, native staticStreamResult) {
	t.Helper()
	if len(stock.frames) != len(native.frames) {
		t.Errorf("%s: the two legs published a different NUMBER of public frames: stock=%d native=%d\n  stock:  %v\n  native: %v",
			label, len(stock.frames), len(native.frames), stock.frames, native.frames)
		return
	}
	for i := range stock.frames {
		if stock.frames[i] != native.frames[i] {
			t.Errorf("%s: public frame %d differs:\n  stock:  %s\n  native: %s", label, i, stock.frames[i], native.frames[i])
		}
	}
	// The two channels the ordered comparison alone would not make legible on failure.
	if stock.heartbeats != native.heartbeats {
		t.Errorf("%s: 2xx-liveness heartbeats: stock=%d native=%d — the native lane REPLACES the stock stream path, so it owes the same liveness the pool's hung detector watches",
			label, stock.heartbeats, native.heartbeats)
	}
	if len(stock.metadata) != len(native.metadata) {
		t.Errorf("%s: metadata frames: stock=%d native=%d", label, len(stock.metadata), len(native.metadata))
	}
}

// TestBootedArtifactDefaultServesTheExactJSONStaticStream is the headline acceptance proof:
// the standard worker DEFAULT-SERVES `/stream` for the exact-U1s cohort, and what the client
// receives is byte-identical to stock BAML.
func TestBootedArtifactDefaultServesTheExactJSONStaticStream(t *testing.T) {
	wantID := strings.TrimSpace(os.Getenv(staticFixtureWorkerArtifactIDEnv))
	if wantID == "" {
		t.Fatalf("%s is not set: this lane must BOOT a STATIC-CAPABLE artifact and stream a real request through it; a missing artifact is a lane misconfiguration, not a reason to report success", staticFixtureWorkerArtifactIDEnv)
	}

	provider := newRouteProofProviderAt(t, staticFixtureLoopbackAddr)
	provider.streamChunks = staticStreamChunks()

	stock := runStaticStreamProof(t, provider, bamlutils.StreamModeStream, false)
	native := runStaticStreamProof(t, provider, bamlutils.StreamModeStream, true)

	// The binary under proof is the SHIPPED artifact, checked before anything is claimed
	// on its behalf.
	if native.artifactProfile != string(artifactprofile.ProfileNativeCapable) {
		t.Fatalf("the booted static fixture publishes profile=%q, want %q", native.artifactProfile, artifactprofile.ProfileNativeCapable)
	}
	if native.artifactID != wantID {
		t.Fatalf("the booted static fixture publishes artifact_id=%q, want the shipped serve-profile artifact's %q", native.artifactID, wantID)
	}

	// The route really streamed on the STOCK leg: several structured frames, a final, and
	// exactly one BAML send. Without this the comparison could pass on two empty legs.
	if got := len(stock.structuredFrames()); got < 2 {
		t.Fatalf("the stock leg published %d structured frame(s); the fragmented corpus must produce several, or the per-tick comparison proves nothing", got)
	}
	if strings.TrimSpace(stock.final) == "" {
		t.Fatalf("the stock leg produced no final; the arm cannot compare anything")
	}
	if stock.providerRequests != 1 {
		t.Fatalf("the stock leg put %d request(s) on the wire, want exactly 1", stock.providerRequests)
	}
	// NON-VACUITY on the liveness channel: the stock path emits a 2xx heartbeat, so a
	// comparison that found none on either leg would prove nothing about the native lane
	// having stopped emitting it.
	if stock.heartbeats == 0 {
		t.Fatalf("the stock leg published no 2xx-liveness heartbeat; the liveness half of the transcript comparison would be vacuous")
	}

	// THE DIFFERENTIAL: the complete ordered public transcript is identical.
	assertStaticStreamTranscriptsMatch(t, "/stream", stock, native)

	// ONE native socket, one upstream request, and no second connection.
	if native.providerRequests != 1 {
		t.Errorf("the native leg put %d request(s) on the wire, want exactly 1 (native owns the one send)", native.providerRequests)
	}
	if native.metrics.nativeSockets != 1 {
		t.Errorf("native sockets = %v, want exactly 1", native.metrics.nativeSockets)
	}
	// The live StreamRequest plan compare ran and MATCHED (which is why it claimed), the
	// per-prefix/final oracle phase was recorded, and native won.
	if native.metrics.planCompareMatch != 1 {
		t.Errorf("plan_compare match = %v, want exactly 1 (one U1s request, one live BAML stream-plan match)", native.metrics.planCompareMatch)
	}
	if got := native.metrics.phaseBySurface["static_stream/same_response_oracle"]; got != 1 {
		t.Errorf("admission_phase{surface=static_stream,phase=same_response_oracle} = %v, want 1 (the per-prefix + final oracle ran)", got)
	}
	if got := native.metrics.winnerBySurface["static_stream/native"]; got != 1 {
		t.Errorf("winner{surface=static_stream,winner=native} = %v, want 1 — the exact cohort must be default-selected natively and agree with BAML on every tick", got)
	}
	// NO ENROLLMENT: the winner is attributed to the structural `none` cohort.
	if got := native.metrics.winnerBySurfaceCohort["static_stream/none/native"]; got != 1 {
		t.Errorf("winner{surface=static_stream,cohort=none,winner=native} = %v, want 1 (structural, enrollment-free)", got)
	}
	if got := native.metrics.winnerBySurfaceCohort["static_stream/"+feV1RouteCohort+"/native"]; got != 0 {
		t.Errorf("the U1s native serve was attributed to the fe-v1 enrollment (winner{fe_v1,native}=%v); the spine stream lane is enrollment-exempt", got)
	}
	// The STOCK leg is the control that makes the readings above causal: with the kill
	// switch on, nothing native may run at all.
	if got := stock.metrics.winnerBySurface["static_stream/native"]; got != 0 {
		t.Errorf("the flag-OFF leg recorded %v native stream winner(s); with the kill switch on nothing native may run", got)
	}
	if stock.metrics.nativeSockets != 0 {
		t.Errorf("the flag-OFF leg opened %v native socket(s), want 0", stock.metrics.nativeSockets)
	}
}

// TestBootedArtifactDefaultServesTheExactJSONStaticStreamWithRaw is the same proof on the
// second public streaming surface. It is a separate arm because /stream-with-raw carries the
// raw and reasoning channels through a DIFFERENT cadence branch — raw flows on ticks that
// release no structured partial — so a lane that got the plain-/stream cadence right can
// still get this one wrong.
func TestBootedArtifactDefaultServesTheExactJSONStaticStreamWithRaw(t *testing.T) {
	if strings.TrimSpace(os.Getenv(staticFixtureWorkerArtifactIDEnv)) == "" {
		t.Fatalf("%s is not set: this lane must BOOT a STATIC-CAPABLE artifact", staticFixtureWorkerArtifactIDEnv)
	}
	provider := newRouteProofProviderAt(t, staticFixtureLoopbackAddr)
	provider.streamChunks = staticStreamChunks()

	stock := runStaticStreamProof(t, provider, bamlutils.StreamModeStreamWithRaw, false)
	native := runStaticStreamProof(t, provider, bamlutils.StreamModeStreamWithRaw, true)

	if got := len(stock.structuredFrames()); got < 2 {
		t.Fatalf("the stock leg published %d structured frame(s); the arm needs several to compare per-tick behaviour", got)
	}
	// The raw channel really carried content on the stock leg, or the raw half of the
	// comparison would be vacuous.
	sawRaw := false
	for _, f := range stock.frames {
		if f.raw != "" {
			sawRaw = true
			break
		}
	}
	if !sawRaw {
		t.Fatalf("the stock /stream-with-raw leg carried no raw text; the raw comparison would be vacuous")
	}

	assertStaticStreamTranscriptsMatch(t, "/stream-with-raw", stock, native)

	if native.providerRequests != 1 || stock.providerRequests != 1 {
		t.Errorf("upstream requests: stock=%d native=%d, want exactly 1 each", stock.providerRequests, native.providerRequests)
	}
	if native.metrics.nativeSockets != 1 {
		t.Errorf("native sockets = %v, want exactly 1", native.metrics.nativeSockets)
	}
	if got := native.metrics.winnerBySurface["static_stream/native"]; got != 1 {
		t.Errorf("winner{surface=static_stream,winner=native} = %v, want 1", got)
	}
}

// TestBootedArtifactWithTheFlagOffStreamsWithNoNativeWork is the kill-switch arm: with
// BAML_REST_USE_DEBAML=false the booted standard artifact must expose NO de-BAML collector
// beyond the two unconditional artifact-identity gauges — no registry, no composite, no
// oracle, no socket — while still streaming the route.
//
// It is the complement of the non-vacuity check in the arms above: those prove the native
// lane IS installed with the flag on, this proves it is entirely absent with it off, so
// "zero native" can be read as a kill switch rather than as a decline.
func TestBootedArtifactWithTheFlagOffStreamsWithNoNativeWork(t *testing.T) {
	if strings.TrimSpace(os.Getenv(staticFixtureWorkerArtifactIDEnv)) == "" {
		t.Fatalf("%s is not set: this lane must BOOT a STATIC-CAPABLE artifact", staticFixtureWorkerArtifactIDEnv)
	}
	provider := newRouteProofProviderAt(t, staticFixtureLoopbackAddr)
	provider.streamChunks = staticStreamChunks()

	stock := runStaticStreamProof(t, provider, bamlutils.StreamModeStream, false)
	if strings.TrimSpace(stock.final) == "" {
		t.Fatal("the flag-off leg produced no final; it must still stream the route through BAML")
	}
	assertZeroNativeOnTheArtifact(t, "static stream (flag off)", stock.metrics)
	for _, name := range stock.metrics.deBAMLFamilies {
		if name != artifactprofile.ArtifactInfoMetric && name != artifactprofile.ExpectationMetric {
			t.Errorf("the flag-off artifact exposes de-BAML collector %q; the umbrella flag must construct no registry, composite, oracle or socket at all", name)
		}
	}
}
