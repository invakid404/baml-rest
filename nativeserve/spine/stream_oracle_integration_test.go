//go:build nanollm_integration

package spine

// ExecBridge-U1s / M3e-B StreamWithOracle LIVE-oracle matrix. It drives the REAL
// AdmitStaticSpineStreamOracleClaim over a deterministic loopback SSE provider:
//
//	live StreamRequest plan match -> claim -> ONE provider stream
//	  -> on EVERY structured cadence tick: native(prefix) AND BAML(prefix) over the SAME
//	     string, compared on PUBLIC marshaled bytes, decided BEFORE the event is public
//	  -> native(full) AND BAML(full) compared before the final is returned
//	  -> exactly one terminal outcome, and never a second provider request.
//
// The BAML leg is a stand-in whose per-prefix behaviour the test scripts, which is what
// makes the drift matrix drivable at all: whether the stand-in agrees with real BAML is the
// booted S3b differential's job, not this file's. What this file proves is the WIRING —
// that both legs run, on the identical prefix, before the emit, and that each matrix row
// produces the public behaviour the design requires.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/internal/debaml"
	"github.com/invakid404/baml-rest/internal/nativespinejsonfixture"
	"github.com/invakid404/baml-rest/internal/schema"
	"github.com/invakid404/baml-rest/nativeserve/admission"
)

// --- the deterministic provider ----------------------------------------------------

// streamOracleContent is the model text the provider streams. Split across several deltas
// it produces a mix of not-yet-parseable and parseable prefixes, which is exactly the
// alternation the per-prefix oracle has to survive.
const streamOracleContent = `{"weather":"sunny","temp":21}`

// streamOracleFragments splits the content so several ticks are structurally incomplete.
var streamOracleFragments = []string{`{"weather"`, `:"sunny"`, `,"temp"`, `:21}`}

func sseContentChunk(text string) string {
	b, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{"delta": map[string]any{"content": text}}},
	})
	return "data: " + string(b) + "\n\n"
}

// streamOracleBody renders the fragments interleaved with the noise frames a real provider
// sends — a role-only opener, empty content deltas, a finish-only frame and a usage frame.
// None of those may reach the oracle: a tick with no parseable delta produces no structured
// parse, so it must produce no BAML call either.
func streamOracleBody() string {
	var b strings.Builder
	b.WriteString(`data: {"choices":[{"delta":{"role":"assistant"}}]}` + "\n\n")
	for _, f := range streamOracleFragments {
		b.WriteString(sseContentChunk(f))
		b.WriteString(`data: {"choices":[{"delta":{"content":""}}]}` + "\n\n")
	}
	b.WriteString(`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n\n")
	b.WriteString(`data: {"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":2}}` + "\n\n")
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

// streamOracleProvider stands up a loopback SSE provider and counts requests.
type streamOracleProvider struct {
	srv  *httptest.Server
	hits atomic.Int64
}

func newStreamOracleProvider(t *testing.T, handler http.HandlerFunc) *streamOracleProvider {
	t.Helper()
	p := &streamOracleProvider{}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.hits.Add(1)
		handler(w, r)
	}))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *streamOracleProvider) baseURL() string { return p.srv.URL + "/v1" }

// okSSE writes the deterministic transcript as an event stream.
func okSSE(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	for _, line := range strings.SplitAfter(streamOracleBody(), "\n\n") {
		if line == "" {
			continue
		}
		_, _ = w.Write([]byte(line))
		if flusher != nil {
			flusher.Flush()
		}
	}
}

// --- the live oracle harness --------------------------------------------------------

// streamLiveOracle builds a real population stream executor at baseURL (its
// admitStreamClaimOracle is the production AdmitStaticSpineStreamOracleClaim) plus a
// BuildBAMLStreamRequest that byte-matches the executor's OWN native prepared stream plan.
// The plan is captured through the FROZEN admission entry (which runs no compare and opens
// no socket) and handed back as BAML's no-send plan, so the LIVE compare inside
// StreamWithOracle matches and the attempt claims.
func streamLiveOracle(t *testing.T, baseURL string) (*StreamExecutor, func(context.Context) (*llmhttp.Request, error), *schema.Bundle) {
	t.Helper()
	e, err := NewPopulationStreamExecutor(jsonAliasProjectAt(t, baseURL), []StreamRegistration{{
		Binding:     nativespinejsonfixture.StreamBinding(),
		BuildMethod: nativespinejsonfixture.BuildMethod,
	}}, nil)
	if err != nil {
		t.Fatalf("NewPopulationStreamExecutor: %v", err)
	}
	rm := e.registry[nativespinejsonfixture.MethodName]
	if rm == nil {
		t.Fatal("the jsonalias method is not registered")
	}
	in := e.oracleStaticStreamInput(rm, baseStreamInv(t))
	frozen, err := admission.AdmitStaticSpineStreamClaim(context.Background(), in)
	if err != nil {
		t.Fatalf("capture the native stream plan via the frozen claim: %v", err)
	}
	prep := frozen.Prepared
	hdr := map[string]string{}
	for _, p := range prep.Headers {
		hdr[p[0]] = p[1]
	}
	bamlReq := &llmhttp.Request{Method: prep.Method, URL: prep.URL, Headers: hdr, Body: string(prep.Body)}
	bundle := frozen.Bundle
	frozen.Close()
	return e, func(context.Context) (*llmhttp.Request, error) { return bamlReq, nil }, bundle
}

// baseStreamInv is the exact-cohort invocation shape, without the oracle closures (the
// plan-capture path needs only the descriptor/route facts).
func baseStreamInv(t *testing.T) bamlutils.NativeStaticStreamOracleInvocation {
	t.Helper()
	values, err := nativespinejsonfixture.StreamBinding().Unary.ProjectInput(
		&nativespinejsonfixture.StaticRecursiveAliasJsonInput{Topic: "weather"})
	if err != nil {
		t.Fatalf("ProjectInput: %v", err)
	}
	return bamlutils.NativeStaticStreamOracleInvocation{
		Method:     nativespinejsonfixture.MethodName,
		Values:     values,
		Mode:       bamlutils.NativeStreamModeStream,
		Provider:   "openai",
		SingleLeaf: true,
	}
}

// bamlLeg records every prefix its per-prefix closure receives and lets a test script the
// answer per structured tick.
type bamlLeg struct {
	mu       sync.Mutex
	prefixes []string
	finals   []string
	// script, when non-nil, decides the answer for the 1-based structured tick index.
	script func(n int, prefix string) (bamlutils.BAMLStreamPrefixResult, error)
	// finalFn, when non-nil, decides the final answer.
	finalFn func(full string) (any, error)
	// agree runs the SAME native parse+decode, so its public bytes match native's exactly.
	bundle *schema.Bundle
}

func (b *bamlLeg) agreeing(_ context.Context, prefix string) (bamlutils.BAMLStreamPrefixResult, error) {
	parsed, err := debaml.ParseStaticStreamPartial(context.Background(), b.bundle, prefix)
	if err != nil {
		return bamlutils.BAMLStreamPrefixResult{}, nil
	}
	v, derr := nativespinejsonfixture.StreamBinding().DecodePartial(parsed.JSON)
	if derr != nil {
		return bamlutils.BAMLStreamPrefixResult{}, nil
	}
	return bamlutils.BAMLStreamPrefixValue(v), nil
}

func (b *bamlLeg) parse(ctx context.Context, prefix string) (bamlutils.BAMLStreamPrefixResult, error) {
	b.mu.Lock()
	b.prefixes = append(b.prefixes, prefix)
	n := len(b.prefixes)
	b.mu.Unlock()
	if b.script != nil {
		return b.script(n, prefix)
	}
	return b.agreeing(ctx, prefix)
}

func (b *bamlLeg) final(_ context.Context, full string) (any, error) {
	b.mu.Lock()
	b.finals = append(b.finals, full)
	b.mu.Unlock()
	if b.finalFn != nil {
		return b.finalFn(full)
	}
	parsed, err := debaml.ParseStaticStreamFinal(context.Background(), b.bundle, full)
	if err != nil {
		return nil, err
	}
	return nativespinejsonfixture.StreamBinding().Unary.DecodeFinal(parsed.JSON)
}

func (b *bamlLeg) seenPrefixes() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.prefixes...)
}

// oracleEvent is one public event the executor delivered, kept as marshaled bytes so the
// comparison is on what a client would receive.
type oracleEvent struct {
	hasPartial bool
	partial    string
	raw        string
}

// oracleCollector records events and can copy the exact prefix the NATIVE leg saw.
type oracleCollector struct {
	mu     sync.Mutex
	events []oracleEvent
}

func (c *oracleCollector) emit(ev bamlutils.NativeSpineStreamEvent) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := oracleEvent{hasPartial: ev.HasPartial, raw: ev.Raw}
	if ev.HasPartial {
		b, err := json.Marshal(ev.Partial)
		if err != nil {
			return fmt.Errorf("marshal partial: %w", err)
		}
		e.partial = string(b)
	}
	c.events = append(c.events, e)
	return nil
}

func (c *oracleCollector) snapshot() []oracleEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]oracleEvent(nil), c.events...)
}

func (c *oracleCollector) structured() []oracleEvent {
	out := []oracleEvent{}
	for _, e := range c.snapshot() {
		if e.hasPartial {
			out = append(out, e)
		}
	}
	return out
}

// liveStreamInv assembles the full invocation: the matching plan builder, the scripted BAML
// legs, and the standard-lane decoders (the same proven decode cores a generated standard
// method supplies).
func liveStreamInv(t *testing.T, buildBAML func(context.Context) (*llmhttp.Request, error), leg *bamlLeg) bamlutils.NativeStaticStreamOracleInvocation {
	t.Helper()
	inv := baseStreamInv(t)
	inv.BuildBAMLStreamRequest = buildBAML
	inv.BAMLStreamParse = leg.parse
	inv.BAMLFinalParse = leg.final
	inv.DecodeNativeStreamPartial = nativespinejsonfixture.StreamBinding().DecodePartial
	inv.DecodeNativeStreamFinal = nativespinejsonfixture.StreamBinding().Unary.DecodeFinal
	return inv
}

// --- the matrix ---------------------------------------------------------------------

// TestStreamWithOracle_LiveOracle_MatchServesNativeOnTheSamePrefixes is the positive and
// the SAME-PREFIX proof: an agreeing BAML leg means every tick matches, native's decoded
// carrier is what reaches the client, the winner stays native, and the prefixes the BAML
// leg was handed are EXACTLY the accumulated parseable prefixes — one per structured tick,
// none for the role/empty/finish/usage frames.
func TestStreamWithOracle_LiveOracle_MatchServesNativeOnTheSamePrefixes(t *testing.T) {
	p := newStreamOracleProvider(t, func(w http.ResponseWriter, _ *http.Request) { okSSE(w) })
	e, buildBAML, bundle := streamLiveOracle(t, p.baseURL())
	leg := &bamlLeg{bundle: bundle}
	c := &oracleCollector{}

	res := e.StreamWithOracle(context.Background(), liveStreamInv(t, buildBAML, leg), c.emit)
	if res.Disposition != bamlutils.NativeSpineStreamSucceeded {
		t.Fatalf("disposition = %v (stage %q reason %q err %v), want succeeded", res.Disposition, res.Stage, res.Reason, res.Err)
	}
	if res.WinnerEngine != bamlutils.NativeStaticServeEngineNative {
		t.Errorf("winner = %q, want native — every prefix and the final matched", res.WinnerEngine)
	}
	if got := p.hits.Load(); got != 1 {
		t.Errorf("the provider saw %d request(s), want exactly 1", got)
	}
	// The BAML leg was handed the accumulated parseable prefix for EVERY structured tick,
	// and the accumulation is monotone and ends at the complete content.
	seen := leg.seenPrefixes()
	if len(seen) != len(streamOracleFragments) {
		t.Fatalf("BAML per-prefix calls = %d, want one per content delta (%d) — never for a role/empty/finish/usage frame",
			len(seen), len(streamOracleFragments))
	}
	want := ""
	for i, f := range streamOracleFragments {
		want += f
		if seen[i] != want {
			t.Errorf("BAML prefix %d = %q, want the accumulated parseable text %q", i, seen[i], want)
		}
	}
	if seen[len(seen)-1] != streamOracleContent {
		t.Errorf("the last BAML prefix = %q, want the full content %q", seen[len(seen)-1], streamOracleContent)
	}
	// Exactly one final comparison, over the same complete text.
	if len(leg.finals) != 1 || leg.finals[0] != streamOracleContent {
		t.Errorf("BAML final calls = %v, want exactly one over %q", leg.finals, streamOracleContent)
	}
	obs := res.Observations
	if obs.PrefixComparisons != len(streamOracleFragments) || obs.PrefixMatch+obs.PrefixNativeNoValue != obs.PrefixComparisons {
		t.Errorf("observations = %+v, want %d comparisons partitioned into match/native_no_value", obs, len(streamOracleFragments))
	}
	if !obs.PlanCompareRan || !obs.PlanMatched || !obs.SocketOpened || !obs.SocketResponded || !obs.FinalOracleRan {
		t.Errorf("observations = %+v, want plan compare + socket + final oracle evidence", obs)
	}
	if obs.Substituted || obs.Suppressed || obs.OracleTerminal {
		t.Errorf("observations = %+v, want no substitution/suppression on an all-match stream", obs)
	}
	if obs.FinalCompare != bamlutils.NativeStreamCompareMatch {
		t.Errorf("final compare = %q, want match", obs.FinalCompare)
	}
}

// bamlMarker is the distinguishable value the drift arms make BAML return, so an emitted
// partial can be attributed to an engine by its public bytes alone.
var bamlMarker = map[string]any{"served_by": "baml"}

const bamlMarkerBytes = `{"served_by":"baml"}`

// TestStreamWithOracle_LiveOracle_EarlyDriftServesBAMLValue: the FIRST parseable tick
// drifts, so BAML's same-prefix value is what becomes public — with no reset, no resend and
// no second provider request — and the attribution latches.
func TestStreamWithOracle_LiveOracle_EarlyDriftServesBAMLValue(t *testing.T) {
	p := newStreamOracleProvider(t, func(w http.ResponseWriter, _ *http.Request) { okSSE(w) })
	e, buildBAML, bundle := streamLiveOracle(t, p.baseURL())
	leg := &bamlLeg{bundle: bundle}
	first := true
	leg.script = func(_ int, prefix string) (bamlutils.BAMLStreamPrefixResult, error) {
		// Drift on the first tick where BOTH engines have a value; before that both
		// legitimately have none.
		agreed, _ := leg.agreeing(context.Background(), prefix)
		if agreed.HasValue && first {
			first = false
			return bamlutils.BAMLStreamPrefixResult{Value: bamlMarker, HasValue: true}, nil
		}
		return agreed, nil
	}
	c := &oracleCollector{}
	res := e.StreamWithOracle(context.Background(), liveStreamInv(t, buildBAML, leg), c.emit)
	if res.Disposition != bamlutils.NativeSpineStreamSucceeded {
		t.Fatalf("disposition = %v (err %v), want succeeded — drift is resolved from the response in hand, never by a second request", res.Disposition, res.Err)
	}
	if res.WinnerEngine != bamlutils.NativeStaticServeEngineBAMLParse {
		t.Errorf("winner = %q, want baml_parse_same_response", res.WinnerEngine)
	}
	if got := p.hits.Load(); got != 1 {
		t.Errorf("the provider saw %d request(s), want exactly 1 — a substituted prefix must not resend", got)
	}
	structured := c.structured()
	if len(structured) == 0 {
		t.Fatal("no structured partial reached the client")
	}
	if structured[0].partial != bamlMarkerBytes {
		t.Errorf("the first structured partial = %s, want BAML's same-prefix value %s", structured[0].partial, bamlMarkerBytes)
	}
	if res.Observations.PrefixBytesMismatch != 1 || !res.Observations.Substituted {
		t.Errorf("observations = %+v, want exactly one bytes_mismatch with substitution recorded", res.Observations)
	}
}

// TestStreamWithOracle_LiveOracle_LateDriftAfterNativeFrames: drift on the LAST parseable
// tick, after native frames already went out. The earlier frames stay native, the drifting
// one is BAML's, and nothing is re-sent or reset.
func TestStreamWithOracle_LiveOracle_LateDriftAfterNativeFrames(t *testing.T) {
	p := newStreamOracleProvider(t, func(w http.ResponseWriter, _ *http.Request) { okSSE(w) })
	e, buildBAML, bundle := streamLiveOracle(t, p.baseURL())
	leg := &bamlLeg{bundle: bundle}
	last := len(streamOracleFragments)
	leg.script = func(n int, prefix string) (bamlutils.BAMLStreamPrefixResult, error) {
		if n == last {
			return bamlutils.BAMLStreamPrefixResult{Value: bamlMarker, HasValue: true}, nil
		}
		return leg.agreeing(context.Background(), prefix)
	}
	c := &oracleCollector{}
	res := e.StreamWithOracle(context.Background(), liveStreamInv(t, buildBAML, leg), c.emit)
	if res.Disposition != bamlutils.NativeSpineStreamSucceeded {
		t.Fatalf("disposition = %v (err %v), want succeeded", res.Disposition, res.Err)
	}
	structured := c.structured()
	if len(structured) < 2 {
		t.Fatalf("structured partials = %d, want at least two so an EARLY native frame precedes the late drift", len(structured))
	}
	if structured[0].partial == bamlMarkerBytes {
		t.Error("the first structured partial came from BAML; only the LAST tick was scripted to drift")
	}
	if got := structured[len(structured)-1].partial; got != bamlMarkerBytes {
		t.Errorf("the last structured partial = %s, want BAML's same-prefix value", got)
	}
	if res.WinnerEngine != bamlutils.NativeStaticServeEngineBAMLParse {
		t.Errorf("winner = %q, want baml_parse_same_response after a late drift", res.WinnerEngine)
	}
	if got := p.hits.Load(); got != 1 {
		t.Errorf("the provider saw %d request(s), want exactly 1", got)
	}
}

// TestStreamWithOracle_LiveOracle_StickyLatchSurvivesALaterMatch: after ANY drift the
// attribution never returns to native, even when every later prefix and the final match.
func TestStreamWithOracle_LiveOracle_StickyLatchSurvivesALaterMatch(t *testing.T) {
	p := newStreamOracleProvider(t, func(w http.ResponseWriter, _ *http.Request) { okSSE(w) })
	e, buildBAML, bundle := streamLiveOracle(t, p.baseURL())
	leg := &bamlLeg{bundle: bundle}
	leg.script = func(n int, prefix string) (bamlutils.BAMLStreamPrefixResult, error) {
		if n == 1 {
			// Drift on the FIRST tick only; every later prefix and the final agree.
			return bamlutils.BAMLStreamPrefixResult{Value: bamlMarker, HasValue: true}, nil
		}
		return leg.agreeing(context.Background(), prefix)
	}
	c := &oracleCollector{}
	res := e.StreamWithOracle(context.Background(), liveStreamInv(t, buildBAML, leg), c.emit)
	if res.Disposition != bamlutils.NativeSpineStreamSucceeded {
		t.Fatalf("disposition = %v (err %v), want succeeded", res.Disposition, res.Err)
	}
	if res.WinnerEngine != bamlutils.NativeStaticServeEngineBAMLParse {
		t.Fatalf("winner = %q, want baml_parse_same_response — the latch must NOT clear on a later match", res.WinnerEngine)
	}
	if res.Observations.FinalCompare != bamlutils.NativeStreamCompareMatch {
		t.Errorf("final compare = %q, want match (the final itself agreed; only the attribution is sticky)", res.Observations.FinalCompare)
	}
	if !res.Observations.Substituted || res.Observations.PrefixMatch == 0 {
		t.Errorf("observations = %+v, want ONE substituted tick followed by matching ones — the latch must survive them", res.Observations)
	}
}

// TestStreamWithOracle_LiveOracle_BAMLNoValueSuppressesTheNativePartial: BAML is the
// authority after the claim, so a prefix it declines releases NOTHING even though native
// produced a value.
func TestStreamWithOracle_LiveOracle_BAMLNoValueSuppressesTheNativePartial(t *testing.T) {
	p := newStreamOracleProvider(t, func(w http.ResponseWriter, _ *http.Request) { okSSE(w) })
	e, buildBAML, bundle := streamLiveOracle(t, p.baseURL())
	leg := &bamlLeg{bundle: bundle}
	// BAML never establishes a partial; every tick is either both-no-value or a suppression.
	leg.script = func(int, string) (bamlutils.BAMLStreamPrefixResult, error) {
		return bamlutils.BAMLStreamPrefixResult{}, nil
	}
	c := &oracleCollector{}
	res := e.StreamWithOracle(context.Background(), liveStreamInv(t, buildBAML, leg), c.emit)
	if res.Disposition != bamlutils.NativeSpineStreamSucceeded {
		t.Fatalf("disposition = %v (err %v), want succeeded — suppression is not a failure", res.Disposition, res.Err)
	}
	if got := c.structured(); len(got) != 0 {
		t.Errorf("%d structured partial(s) reached the client while BAML established none; every one must be SUPPRESSED", len(got))
	}
	if !res.Observations.Suppressed || res.Observations.PrefixBAMLNoValue == 0 {
		t.Errorf("observations = %+v, want suppression recorded", res.Observations)
	}
	if res.WinnerEngine != bamlutils.NativeStaticServeEngineBAMLParse {
		t.Errorf("winner = %q, want baml_parse_same_response — a suppressed stream was not served purely natively", res.WinnerEngine)
	}
}

// TestStreamWithOracle_LiveOracle_FinalDriftServesBAMLFinal: the FINAL disagrees, so BAML's
// same-response final is returned — from the response already in hand.
func TestStreamWithOracle_LiveOracle_FinalDriftServesBAMLFinal(t *testing.T) {
	p := newStreamOracleProvider(t, func(w http.ResponseWriter, _ *http.Request) { okSSE(w) })
	e, buildBAML, bundle := streamLiveOracle(t, p.baseURL())
	leg := &bamlLeg{bundle: bundle}
	leg.finalFn = func(string) (any, error) { return bamlMarker, nil }
	c := &oracleCollector{}
	res := e.StreamWithOracle(context.Background(), liveStreamInv(t, buildBAML, leg), c.emit)
	if res.Disposition != bamlutils.NativeSpineStreamSucceeded {
		t.Fatalf("disposition = %v (err %v), want succeeded", res.Disposition, res.Err)
	}
	got, err := json.Marshal(res.Final)
	if err != nil {
		t.Fatalf("marshal final: %v", err)
	}
	if string(got) != bamlMarkerBytes {
		t.Errorf("final = %s, want BAML's same-response final %s", got, bamlMarkerBytes)
	}
	if res.WinnerEngine != bamlutils.NativeStaticServeEngineBAMLParse || res.Observations.FinalCompare != bamlutils.NativeStreamCompareBytesMismatch {
		t.Errorf("winner=%q finalCompare=%q, want a BAML-parse win on a byte-mismatched final", res.WinnerEngine, res.Observations.FinalCompare)
	}
	if got := p.hits.Load(); got != 1 {
		t.Errorf("the provider saw %d request(s), want exactly 1", got)
	}
}

// TestStreamWithOracle_LiveOracle_BAMLPrefixErrorIsTerminal: an oracle that cannot be
// ESTABLISHED for a prefix stops the claimed stream. The STRICT cadence is what carries it
// out; the legacy swallow-everything policy would have continued streaming unverified
// partials, which is the exact failure this lane exists to prevent.
func TestStreamWithOracle_LiveOracle_BAMLPrefixErrorIsTerminal(t *testing.T) {
	p := newStreamOracleProvider(t, func(w http.ResponseWriter, _ *http.Request) { okSSE(w) })
	e, buildBAML, bundle := streamLiveOracle(t, p.baseURL())
	leg := &bamlLeg{bundle: bundle}
	boom := errors.New("oracle unavailable")
	leg.script = func(int, string) (bamlutils.BAMLStreamPrefixResult, error) {
		return bamlutils.BAMLStreamPrefixResult{}, boom
	}
	c := &oracleCollector{}
	res := e.StreamWithOracle(context.Background(), liveStreamInv(t, buildBAML, leg), c.emit)
	if res.Disposition != bamlutils.NativeSpineStreamFailedAfterClaim {
		t.Fatalf("disposition = %v, want failed_after_claim — a claimed stream that lost its oracle must terminate", res.Disposition)
	}
	if res.Err == nil {
		t.Fatal("the terminal carries no error")
	}
	if got := p.hits.Load(); got != 1 {
		t.Errorf("the provider saw %d request(s), want exactly 1 — a post-claim terminal never resends", got)
	}
	if !res.Observations.OracleTerminal || !res.Observations.SocketOpened {
		t.Errorf("observations = %+v, want the oracle terminal + socket evidence carried out", res.Observations)
	}
	if len(c.structured()) != 0 {
		t.Errorf("%d structured partial(s) were released before the oracle failed; the comparison precedes the emit", len(c.structured()))
	}
}

// TestStreamWithOracle_LiveOracle_BAMLPrefixPanicIsBoundedAndTerminal: a panicking oracle is
// never converted into a no-value. It unwinds into the claimed guard, terminates the stream,
// and the recovered payload does not escape.
func TestStreamWithOracle_LiveOracle_BAMLPrefixPanicIsBoundedAndTerminal(t *testing.T) {
	p := newStreamOracleProvider(t, func(w http.ResponseWriter, _ *http.Request) { okSSE(w) })
	e, buildBAML, bundle := streamLiveOracle(t, p.baseURL())
	leg := &bamlLeg{bundle: bundle}
	leg.script = func(int, string) (bamlutils.BAMLStreamPrefixResult, error) {
		panic("oracle panic: " + secretPayload)
	}
	c := &oracleCollector{}
	res := e.StreamWithOracle(context.Background(), liveStreamInv(t, buildBAML, leg), c.emit)
	if res.Disposition != bamlutils.NativeSpineStreamFailedAfterClaim {
		t.Fatalf("disposition = %v, want failed_after_claim for a post-claim oracle panic", res.Disposition)
	}
	if res.Err == nil || strings.Contains(res.Err.Error(), secretPayload) {
		t.Errorf("the terminal error leaks the recovered panic payload: %v", res.Err)
	}
	if res.Reason != "panic" {
		t.Errorf("reason = %q, want the bounded panic token", res.Reason)
	}
	if got := p.hits.Load(); got != 1 {
		t.Errorf("the provider saw %d request(s), want exactly 1", got)
	}
	// The evidence accumulated before the panic still comes out.
	if !res.Observations.SocketOpened || !res.Observations.PlanMatched {
		t.Errorf("observations = %+v, want the pre-panic plan/socket evidence retained", res.Observations)
	}
}

// TestStreamWithOracle_LiveOracle_BAMLFinalFailureIsTerminalEvenWhenNativeSucceeded: the
// final has no valid no-value state, and BAML failing there is terminal regardless of
// native, because the request has lost its safety oracle exactly where it matters most.
func TestStreamWithOracle_LiveOracle_BAMLFinalFailureIsTerminalEvenWhenNativeSucceeded(t *testing.T) {
	for name, fn := range map[string]func(string) (any, error){
		"final error":    func(string) (any, error) { return nil, errors.New("Parse.Method failed") },
		"final no value": func(string) (any, error) { return nil, nil },
	} {
		t.Run(name, func(t *testing.T) {
			p := newStreamOracleProvider(t, func(w http.ResponseWriter, _ *http.Request) { okSSE(w) })
			e, buildBAML, bundle := streamLiveOracle(t, p.baseURL())
			leg := &bamlLeg{bundle: bundle, finalFn: fn}
			c := &oracleCollector{}
			res := e.StreamWithOracle(context.Background(), liveStreamInv(t, buildBAML, leg), c.emit)
			if res.Disposition != bamlutils.NativeSpineStreamFailedAfterClaim {
				t.Fatalf("disposition = %v, want failed_after_claim", res.Disposition)
			}
			if !res.Observations.OracleTerminal {
				t.Errorf("observations = %+v, want the oracle terminal recorded", res.Observations)
			}
			if got := p.hits.Load(); got != 1 {
				t.Errorf("the provider saw %d request(s), want exactly 1", got)
			}
		})
	}
}

// TestStreamWithOracle_LiveOracle_PlanMismatchDeclinesZeroSocket: the live StreamRequest
// plan compare is the PRE-CLAIM rail. A plan that does not byte-match declines with zero
// provider sockets and zero public events, so BAML serves the request.
func TestStreamWithOracle_LiveOracle_PlanMismatchDeclinesZeroSocket(t *testing.T) {
	p := newStreamOracleProvider(t, func(w http.ResponseWriter, _ *http.Request) { okSSE(w) })
	e, buildBAML, bundle := streamLiveOracle(t, p.baseURL())
	mismatched := func(ctx context.Context) (*llmhttp.Request, error) {
		req, err := buildBAML(ctx)
		if err != nil {
			return nil, err
		}
		drifted := *req
		drifted.Body = req.Body + " "
		return &drifted, nil
	}
	leg := &bamlLeg{bundle: bundle}
	c := &oracleCollector{}
	res := e.StreamWithOracle(context.Background(), liveStreamInv(t, mismatched, leg), c.emit)
	if res.Disposition != bamlutils.NativeSpineStreamDeclinedPreSocket {
		t.Fatalf("disposition = %v, want declined_pre_socket on a plan mismatch", res.Disposition)
	}
	if got := p.hits.Load(); got != 0 {
		t.Errorf("a plan-mismatch decline opened %d provider request(s); it must certify zero", got)
	}
	if len(c.snapshot()) != 0 {
		t.Errorf("a plan-mismatch decline delivered %d public event(s); it must certify zero", len(c.snapshot()))
	}
	if !res.Observations.PlanCompareRan || res.Observations.PlanMatched {
		t.Errorf("observations = %+v, want plan_compare ran and did NOT match", res.Observations)
	}
}

// TestStreamWithOracle_LiveOracle_ProviderFaultIsTerminal: a post-claim provider fault is
// terminal with one request and no BAML transport.
func TestStreamWithOracle_LiveOracle_ProviderFaultIsTerminal(t *testing.T) {
	p := newStreamOracleProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"slow down"}`))
	})
	e, buildBAML, bundle := streamLiveOracle(t, p.baseURL())
	leg := &bamlLeg{bundle: bundle}
	c := &oracleCollector{}
	res := e.StreamWithOracle(context.Background(), liveStreamInv(t, buildBAML, leg), c.emit)
	if res.Disposition != bamlutils.NativeSpineStreamFailedAfterClaim {
		t.Fatalf("disposition = %v, want failed_after_claim on a provider fault", res.Disposition)
	}
	var httpErr *llmhttp.HTTPError
	if !errors.As(res.Err, &httpErr) || httpErr.StatusCode != http.StatusTooManyRequests {
		t.Errorf("err = %v (%T), want the preserved provider status", res.Err, res.Err)
	}
	if got := p.hits.Load(); got != 1 {
		t.Errorf("the provider saw %d request(s), want exactly 1 — no retry, no resend", got)
	}
	if len(leg.seenPrefixes()) != 0 {
		t.Errorf("the BAML oracle ran %d time(s) on a stream that never produced a delta", len(leg.seenPrefixes()))
	}
}
