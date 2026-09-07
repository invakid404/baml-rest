//go:build integration && nanollm_integration

package listserve

// Scope §3.A proof 4 for the required scalar-LIST input widening: the streaming
// differential, on BOTH public stream routes, over the UNCHANGED exact-JSON
// raw-prefix/SSE corpus with new list-input witnesses.
//
// The per-prefix and final authority is REAL stock BAML: BAMLStreamParse is
// `CallFunctionParse` with `"stream": true` (what `ParseStream.<Method>` calls) and
// BAMLFinalParse is the same entry point with `"stream": false` (what
// `Parse.<Method>` calls), both over the SAME accumulated text the claimed native
// stream produced. The per-prefix drift/over-emit/BAML-unavailable controls are the
// production ones in nativeserve/streamoracle; nothing here weakens them.

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/internal/nativespine"
	"github.com/invakid404/baml-rest/internal/nativespinelistfixture"
)

// eventCollector records the COMPLETE ORDERED public event trace the claimed stream
// delivered, as the marshalled bytes a client would receive.
type eventCollector struct {
	mu     sync.Mutex
	events []collectedEvent
}

type collectedEvent struct {
	hasPartial bool
	partial    string
	raw        string
	reasoning  string
}

func (c *eventCollector) emit(ev bamlutils.NativeSpineStreamEvent) error {
	e := collectedEvent{hasPartial: ev.HasPartial, raw: ev.Raw, reasoning: ev.Reasoning}
	if ev.HasPartial {
		b, err := json.Marshal(ev.Partial)
		if err != nil {
			return err
		}
		e.partial = string(b)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, e)
	return nil
}

func (c *eventCollector) snapshot() []collectedEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]collectedEvent(nil), c.events...)
}

func (c *eventCollector) structured() []collectedEvent {
	out := []collectedEvent{}
	for _, e := range c.snapshot() {
		if e.hasPartial {
			out = append(out, e)
		}
	}
	return out
}

// prefixRecorder wraps the stock BAML per-prefix oracle and records every prefix it
// was handed, so the test can assert the oracle really ran per structured tick and
// re-derive BAML's own answer for the same prefixes independently.
type prefixRecorder struct {
	mu       sync.Mutex
	prefixes []string
	finals   []string
}

func (p *prefixRecorder) addPrefix(s string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.prefixes = append(p.prefixes, s)
}

func (p *prefixRecorder) addFinal(s string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.finals = append(p.finals, s)
}

func (p *prefixRecorder) seen() ([]string, []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.prefixes...), append([]string(nil), p.finals...)
}

// streamInv assembles the exact-cohort stream invocation the generated
// /stream{,-with-raw} seam builds, with the per-prefix and final oracles backed by
// the stock BAML runtime.
func streamInv(t *testing.T, leg *bamlLeg, r argRow, mode bamlutils.NativeStreamMode) bamlutils.NativeStaticStreamOracleInvocation {
	t.Helper()
	return streamInvRecording(t, leg, r, mode, &prefixRecorder{})
}

func streamInvRecording(t *testing.T, leg *bamlLeg, r argRow, mode bamlutils.NativeStreamMode, rec *prefixRecorder) bamlutils.NativeStaticStreamOracleInvocation {
	t.Helper()
	return bamlutils.NativeStaticStreamOracleInvocation{
		Method:                 nativespine.ListArgsFixtureMethod,
		Args:                   r.args(),
		ArgOrder:               argOrder(),
		Values:                 r.values(t),
		Mode:                   mode,
		Provider:               "openai",
		SingleLeaf:             true,
		NeedsRaw:               mode == bamlutils.NativeStreamModeStreamWithRaw,
		BuildBAMLStreamRequest: leg.buildRequestFn(t, r, true),
		BAMLStreamParse: func(ctx context.Context, prefix string) (bamlutils.BAMLStreamPrefixResult, error) {
			rec.addPrefix(prefix)
			v, err := leg.parseStream(ctx, t, r, prefix)
			if err != nil {
				// EVERY error is TERMINAL, exactly as the generated closure documents:
				// ParseStream signals "no partial for this prefix yet" by RETURNING a
				// value, never by erroring.
				return bamlutils.BAMLStreamPrefixResult{}, err
			}
			return bamlutils.BAMLStreamPrefixValue(v), nil
		},
		BAMLFinalParse: func(ctx context.Context, full string) (any, error) {
			rec.addFinal(full)
			return leg.parse(ctx, t, r, full)
		},
		DecodeNativeStreamPartial: nativespinelistfixture.StreamBinding().DecodePartial,
		DecodeNativeStreamFinal:   nativespinelistfixture.StreamBinding().Unary.DecodeFinal,
	}
}

// TestStreamComposite_NativeWinsOnBothRoutes is the streaming headline: on /stream
// and on /stream-with-raw, every point of the typed cross-product claims, serves ONE
// provider stream natively, and agrees with the stock BAML authority on every
// accumulated prefix and on the final.
//
// The non-vacuity guards are what make it evidence rather than decoration: the
// corpus must produce SEVERAL structured partials, the BAML per-prefix oracle must
// actually have been consulted per tick, and /stream-with-raw must carry raw text.
func TestStreamComposite_NativeWinsOnBothRoutes(t *testing.T) {
	modes := []struct {
		name string
		mode bamlutils.NativeStreamMode
	}{
		{"stream", bamlutils.NativeStreamModeStream},
		{"stream_with_raw", bamlutils.NativeStreamModeStreamWithRaw},
	}
	for _, m := range modes {
		for _, r := range argRows() {
			t.Run(m.name+"/"+r.name, func(t *testing.T) {
				server := newSSEServer(t, listStreamCorpus())
				leg := newBAMLLeg(t, server.baseURL())
				serve, exec := streamComposite(t, server.baseURL())

				rec := &prefixRecorder{}
				collector := &eventCollector{}
				res := serve(context.Background(), streamInvRecording(t, leg, r, m.mode, rec), collector.emit)

				if res.Disposition != bamlutils.NativeStaticStreamOracleSucceeded {
					t.Fatalf("disposition = %v (stage=%q reason=%q err=%v); the scalar-list cohort must be SERVED on this route",
						res.Disposition, res.Stage, res.Reason, res.Err)
				}
				if res.WinnerEngine != bamlutils.NativeStaticServeEngineNative {
					t.Fatalf("winner = %q, want %q — native drifted from the stock BAML authority on some prefix or on the final",
						res.WinnerEngine, bamlutils.NativeStaticServeEngineNative)
				}
				if snap := exec.Metrics().Snapshot(); snap.Claims != 1 || snap.Sockets != 1 || snap.Successes != 1 {
					t.Fatalf("executor counters = %+v, want claims=1 sockets=1 successes=1", snap)
				}
				if got := server.hits.Load(); got != 1 {
					t.Fatalf("the provider saw %d request(s), want exactly 1 (one DoStream, no resend)", got)
				}

				// NON-VACUITY: several real partials, and the BAML oracle consulted for
				// each of them. Two nearly-empty traces would otherwise agree trivially.
				structured := collector.structured()
				if len(structured) < 2 {
					t.Fatalf("the claimed stream published %d structured partial(s); the per-prefix comparison needs several", len(structured))
				}
				prefixes, finals := rec.seen()
				if len(prefixes) < len(structured) {
					t.Fatalf("the stock BAML per-prefix oracle saw %d prefix(es) for %d structured partial(s); it was not consulted per tick",
						len(prefixes), len(structured))
				}
				if len(finals) != 1 {
					t.Fatalf("the stock BAML FINAL oracle ran %d time(s), want exactly 1", len(finals))
				}
				if finals[0] != listStreamFinal {
					t.Errorf("the final oracle was handed %q, want the complete accumulated text %q", finals[0], listStreamFinal)
				}

				// The ordered public partials must be exactly what stock BAML parses for
				// the SAME accumulated prefixes — re-derived here rather than trusted
				// from the resolver's own verdict.
				assertPartialsMatchStockBAML(t, leg, r, prefixes, structured)

				if got := jsonOf(t, res.Final); got != listStreamFinal {
					t.Errorf("public final = %s, want %s", got, listStreamFinal)
				}

				if m.mode == bamlutils.NativeStreamModeStreamWithRaw {
					if res.Raw == "" {
						t.Error("/stream-with-raw produced no accumulated raw text")
					}
					sawRaw := false
					for _, e := range collector.snapshot() {
						if e.raw != "" {
							sawRaw = true
							break
						}
					}
					if !sawRaw {
						t.Error("/stream-with-raw emitted no raw-bearing event; the raw half of this route is unproven")
					}
				} else {
					for i, e := range collector.snapshot() {
						if e.raw != "" || e.reasoning != "" {
							t.Errorf("plain /stream event[%d] carried raw/reasoning text; that channel belongs to /stream-with-raw", i)
						}
					}
				}
			})
		}
	}
}

// TestStreamComposite_ReasoningRoute drives a corpus that carries reasoning deltas
// on /stream-with-raw, so the reasoning channel is exercised for a list-input
// witness rather than assumed to follow from the raw one.
func TestStreamComposite_ReasoningRoute(t *testing.T) {
	events := contentSSE([]string{"[1,", `"x",`, "true]"}, []string{"thinking ", "harder"})
	r := argRows()[0]

	server := newSSEServer(t, events)
	leg := newBAMLLeg(t, server.baseURL())
	serve, exec := streamComposite(t, server.baseURL())

	inv := streamInv(t, leg, r, bamlutils.NativeStreamModeStreamWithRaw)
	inv.IncludeReasoning = true
	collector := &eventCollector{}
	res := serve(context.Background(), inv, collector.emit)

	if res.Disposition != bamlutils.NativeStaticStreamOracleSucceeded {
		t.Fatalf("disposition = %v (stage=%q reason=%q err=%v)", res.Disposition, res.Stage, res.Reason, res.Err)
	}
	if res.WinnerEngine != bamlutils.NativeStaticServeEngineNative {
		t.Fatalf("winner = %q, want native", res.WinnerEngine)
	}
	if snap := exec.Metrics().Snapshot(); snap.Sockets != 1 {
		t.Fatalf("executor counters = %+v, want exactly one socket", snap)
	}
	if res.Reasoning == "" {
		t.Fatal("the reasoning channel is empty; the reasoning deltas in the corpus were dropped")
	}
	if got := jsonOf(t, res.Final); got != listStreamFinal {
		t.Errorf("public final = %s, want %s", got, listStreamFinal)
	}
}

// assertPartialsMatchStockBAML re-derives stock BAML's partial for each accumulated
// prefix the oracle was handed and requires the ordered public partials to be
// exactly those, in order.
//
// It re-parses rather than trusting the resolver's verdict on purpose: the resolver
// is the code under proof, so a test that only read its winner token would be
// asserting the implementation against itself.
func assertPartialsMatchStockBAML(t *testing.T, leg *bamlLeg, r argRow, prefixes []string, got []collectedEvent) {
	t.Helper()
	want := []string{}
	for _, p := range prefixes {
		v, err := leg.parseStream(context.Background(), t, r, p)
		if err != nil {
			t.Fatalf("re-deriving stock BAML's partial for a prefix failed: %v", err)
		}
		if bamlutils.IsBAMLStreamNoValue(v) {
			continue // BAML established no partial for this prefix; the tick is suppressed.
		}
		want = append(want, jsonOf(t, v))
	}
	if len(want) != len(got) {
		t.Fatalf("structured partial COUNT: native=%d stock-BAML=%d", len(got), len(want))
	}
	for i := range want {
		if got[i].partial != want[i] {
			t.Errorf("partial[%d]: native=%s stock-BAML=%s", i, got[i].partial, want[i])
		}
	}
}
