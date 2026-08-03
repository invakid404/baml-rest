//go:build integration && nanollm_integration

package staticserve

// De-BAML Phase 3b generated-route STREAM serving + EVENT-EXACT SSE-replay proof. It drives
// the REAL generated static-serve fixture adapter's STREAM method (StaticRecursiveAliasJSON)
// through nativeserve.NewStaticStream over a loopback SSE server bound to the fixture's baked
// base_url, and compares the COMPLETE ORDERED PUBLIC-EVENT TRACE (event kind, partial+final
// bytes, raw + reasoning text, reset placement, metadata/terminal ordering) of the FLAG-ON
// (native) run against the FLAG-OFF (BAML) run over the SAME SSE — for both /stream and
// /stream-with-raw(+reasoning). It also proves: role-only/finish-only/usage-only suppression
// on every channel; SSE chunk boundaries varied INDEPENDENTLY of content incl a real
// mid-multibyte-UTF-8 raw-byte split (no panic); one native socket + zero BAML resend; a
// fault AFTER a first emitted body byte is TERMINAL; and the decline manifest (exactly
// StaticRecursiveAliasJSON served, every other stream method BAML-served with 0 native
// sockets + 1 BAML request).

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/internal/debaml"
	"github.com/invakid404/baml-rest/nativeserve"

	fixture "github.com/invakid404/baml-rest/internal/nativeprompt/testdata/staticserve_fixture/generated"
	fwadapter "github.com/invakid404/baml-rest/internal/nativeprompt/testdata/staticserve_fixture/generated/adapter"
)

// ---- native stream serve spy -------------------------------------------------------------

type streamServeSpy struct {
	fn       bamlutils.NativeStaticStreamServeFunc
	calls    atomic.Int64
	lastDisp atomic.Int64 // bamlutils.NativeStreamDisposition of the last invocation
	// A DECLINED result carries bounded stage/reason tokens (see
	// bamlutils.NativeStreamServeResult); canary forwards the real StaticDecline's
	// pair. Retained so a one-callback decline row can assert WHERE native stepped
	// aside, not merely THAT it did.
	lastStage  atomic.Value // string
	lastReason atomic.Value // string
}

func (s *streamServeSpy) Serve(ctx context.Context, inv bamlutils.NativeStaticStreamInvocation) bamlutils.NativeStreamServeResult {
	s.calls.Add(1)
	res := s.fn(ctx, inv)
	s.lastDisp.Store(int64(res.Disposition))
	s.lastStage.Store(res.Stage)
	s.lastReason.Store(res.Reason)
	return res
}

// lastDecline mirrors staticServeSpy.lastDecline for the stream lane: the last
// invocation's disposition plus its declined-only stage/reason tokens.
func (s *streamServeSpy) lastDecline() (int64, string, string) {
	st, _ := s.lastStage.Load().(string)
	rs, _ := s.lastReason.Load().(string)
	return s.lastDisp.Load(), st, rs
}

// nativeSocketsZero reports whether the native stream lane opened ZERO provider sockets for
// the last request: either the seam was never installed (callback not invoked) OR the
// callback ran and returned NativeStreamDeclined — a PRE-TRANSPORT decline, whose contract is
// no socket / no EmitDelta. (A claim opens a socket and reports Completed/FailedAfterClaim.)
func (s *streamServeSpy) nativeSocketsZero() bool {
	return s.calls.Load() == 0 || s.lastDisp.Load() == int64(bamlutils.NativeStreamDeclined)
}

func newStreamServeSpy(t *testing.T) *streamServeSpy {
	t.Helper()
	fn, err := nativeserve.NewStaticStream(prometheus.NewRegistry())
	if err != nil {
		t.Fatalf("nativeserve.NewStaticStream: %v", err)
	}
	if fn == nil {
		t.Fatal("nativeserve.NewStaticStream returned a nil serve func")
	}
	return &streamServeSpy{fn: fn}
}

// ---- loopback SSE servers ----------------------------------------------------------------

// newFixtureStreamServer returns each SSE data event in order (text/event-stream) as one
// `data: …\n\n` frame, counting accepted requests. Terminate `events` with "[DONE]".
func newFixtureStreamServer(t *testing.T, events []string) *fixtureServer {
	t.Helper()
	return newRawSSEServer(t, func(w http.ResponseWriter, fl http.Flusher) {
		for _, e := range events {
			fmt.Fprintf(w, "data: %s\n\n", e)
			if fl != nil {
				fl.Flush()
			}
		}
	})
}

// newRawSSEServer binds the fixed loopback and runs write over the raw response writer +
// flusher (for byte-level control of the SSE stream). It counts accepted requests.
func newRawSSEServer(t *testing.T, write func(w http.ResponseWriter, fl http.Flusher)) *fixtureServer {
	t.Helper()
	ln := listenFixturePort(t)
	fs := &fixtureServer{}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fs.count.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		write(w, fl)
	}))
	_ = srv.Listener.Close()
	srv.Listener = ln
	srv.Start()
	fs.srv = srv
	// Backstop the FIXED base_url port: callers still close() explicitly between the
	// sequential flag-on/flag-off drives that rebind the same port, but a t.Fatalf before
	// that explicit close would otherwise leak the listener and break later tests reusing
	// the loopback. fs.close is idempotent (CompareAndSwap), so this cleanup is a safe no-op
	// once the explicit close has run. (An ephemeral :0 port is NOT usable here — the fixture
	// adapter's base_url is a baked literal pinned to fixtureLoopbackAddr.)
	t.Cleanup(fs.close)
	return fs
}

// ---- adapter wiring ----------------------------------------------------------------------

func buildFixtureStreamAdapter(t *testing.T, spy *streamServeSpy, flagOn bool, mode bamlutils.StreamMode, includeReasoning bool) bamlutils.Adapter {
	t.Helper()
	fixtureInitRuntime()
	a := fixture.MakeAdapter(context.Background())
	ba, ok := a.(*fwadapter.BamlAdapter)
	if !ok {
		t.Fatalf("MakeAdapter returned %T, want *adapter.BamlAdapter", a)
	}
	ba.SetStreamMode(mode)
	ba.SetIncludeReasoning(includeReasoning)
	ba.SetDeBAMLConfig(bamlutils.DeBAMLConfig{Enabled: flagOn})
	ba.SetDeBAMLParser(debaml.Parse)
	ba.SetNativeStaticStreamServeComparator(spy.Serve)
	ba.SetHTTPClient(llmhttp.NewClient(&http.Client{Transport: &http.Transport{Proxy: nil}}))
	return a
}

// ---- ordered public-event trace ----------------------------------------------------------

// capturedEvent is one public stream event, reduced to the byte-comparable facets. The
// metadata engine token (native vs baml) is DELIBERATELY excluded — it is the one facet that
// legitimately differs between the two legs — but the event kind, metadata PHASE, and its
// ORDERING relative to partials/final are compared.
type capturedEvent struct {
	kind      bamlutils.StreamResultKind
	metaPhase bamlutils.MetadataPhase
	partial   string
	final     string
	raw       string
	reasoning string
	reset     bool
}

type streamTrace struct {
	events  []capturedEvent
	winner  string
	planned string
	drainer error
}

// drainStreamTrace drains ch, capturing the complete ordered event trace. Heartbeat events
// are liveness (fired on 2xx headers, timing-dependent) and are omitted from the ordered
// trace; every other event kind is captured in order.
func drainStreamTrace(t *testing.T, ch <-chan bamlutils.StreamResult, err error) streamTrace {
	t.Helper()
	var tr streamTrace
	if err != nil {
		tr.drainer = err
		return tr
	}
	for r := range ch {
		ev := capturedEvent{kind: r.Kind(), raw: r.Raw(), reasoning: r.Reasoning(), reset: r.Reset()}
		switch r.Kind() {
		case bamlutils.StreamResultKindStream:
			ev.partial = jsonOf(t, r.Stream())
		case bamlutils.StreamResultKindFinal:
			ev.final = jsonOf(t, r.Final())
		case bamlutils.StreamResultKindError:
			tr.drainer = r.Error()
		case bamlutils.StreamResultKindMetadata:
			if md := r.Metadata(); md != nil {
				ev.metaPhase = md.Phase
				if md.Phase == bamlutils.MetadataPhaseOutcome {
					tr.winner = md.WinnerEngine
					tr.planned = md.PlannedEngine
				}
			}
		case bamlutils.StreamResultKindHeartbeat:
			r.Release()
			continue // liveness-only; omit from the ordered trace
		}
		tr.events = append(tr.events, ev)
		r.Release()
	}
	return tr
}

// assertTraceEqual asserts the two ordered event traces are byte-identical (the engine token
// excluded). It is the STRICT event-exact comparison the SSE-replay differential requires.
func assertTraceEqual(t *testing.T, label string, native, baml streamTrace) {
	t.Helper()
	if native.drainer != nil || baml.drainer != nil {
		t.Fatalf("%s: drain error native=%v baml=%v", label, native.drainer, baml.drainer)
	}
	if len(native.events) != len(baml.events) {
		t.Fatalf("%s: event COUNT native=%d baml=%d\n native=%+v\n baml=%+v",
			label, len(native.events), len(baml.events), native.events, baml.events)
	}
	for i := range native.events {
		if native.events[i] != baml.events[i] {
			t.Errorf("%s: event[%d] native=%+v baml=%+v", label, i, native.events[i], baml.events[i])
		}
	}
}

// ---- OpenAI SSE builders -----------------------------------------------------------------

func openAIChunk(delta, finish string) string {
	fin := "null"
	if finish != "" {
		fin = `"` + finish + `"`
	}
	return fmt.Sprintf(`{"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":%s,"finish_reason":%s}]}`, delta, fin)
}

func openAIUsageChunk() string {
	return `{"id":"c","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":4,"completion_tokens":6,"total_tokens":10}}`
}

// splitContent splits a content string into chunks at the given byte sizes (the remainder is
// one final chunk), so SSE boundaries are varied INDEPENDENTLY of JSON token structure.
func splitContent(content string, sizes ...int) []string {
	var chunks []string
	i := 0
	for _, sz := range sizes {
		if i >= len(content) {
			break
		}
		end := i + sz
		if end > len(content) {
			end = len(content)
		}
		chunks = append(chunks, content[i:end])
		i = end
	}
	if i < len(content) {
		chunks = append(chunks, content[i:])
	}
	return chunks
}

// contentSSE builds an SSE that streams content in the given (arbitrary-boundary) chunks,
// wrapped with a role-only opener, a usage-only frame, and a finish-only closer (the three
// chunk kinds that must be SUPPRESSED — no partial), then [DONE]. If reasoning is non-empty
// it is streamed (as reasoning_content deltas) BEFORE the content.
func contentSSE(contentChunks, reasoningChunks []string) []string {
	events := []string{openAIChunk(`{"role":"assistant"}`, "")}
	for _, rc := range reasoningChunks {
		events = append(events, openAIChunk(fmt.Sprintf(`{"reasoning_content":%q}`, rc), ""))
	}
	for _, cc := range contentChunks {
		events = append(events, openAIChunk(fmt.Sprintf(`{"content":%q}`, cc), ""))
	}
	events = append(events, openAIUsageChunk(), openAIChunk(`{}`, "stop"), "[DONE]")
	return events
}

// ---- SSE-replay differential (STRICT, event-exact) ---------------------------------------

func TestAliasStreamSSEReplay_Stream(t *testing.T) {
	// A representative alias value, chunked at arbitrary (non-token) byte boundaries.
	content := `[1,"x",true,{"z":9,"a":8},[2,3]]`
	events := contentSSE(splitContent(content, 3, 5, 2, 7, 4, 6), nil)

	native := replayTrace(t, events, true, bamlutils.StreamModeStream, false)
	baml := replayTrace(t, events, false, bamlutils.StreamModeStream, false)
	if native.winner != bamlutils.NativeServeEngineNative {
		t.Fatalf("native leg did not serve natively (winner=%q)", native.winner)
	}
	assertTraceEqual(t, "stream", native, baml)
	t.Logf("SSE-replay /stream: %d events byte-identical native==BAML", len(native.events))
}

func TestAliasStreamSSEReplay_StreamWithRawReasoning(t *testing.T) {
	content := `{"a":[1,2,3],"b":"héllo","c":true}`
	events := contentSSE(splitContent(content, 4, 3, 9, 5), []string{"thin", "king…"})

	native := replayTrace(t, events, true, bamlutils.StreamModeStreamWithRaw, true)
	baml := replayTrace(t, events, false, bamlutils.StreamModeStreamWithRaw, true)
	if native.winner != bamlutils.NativeServeEngineNative {
		t.Fatalf("native leg did not serve natively (winner=%q)", native.winner)
	}
	assertTraceEqual(t, "stream-with-raw+reasoning", native, baml)
	// The raw + reasoning channels must be populated on this variant.
	var sawRaw, sawReasoning bool
	for _, e := range native.events {
		sawRaw = sawRaw || e.raw != ""
		sawReasoning = sawReasoning || e.reasoning != ""
	}
	if !sawRaw {
		t.Error("stream-with-raw: no raw text surfaced on any native event")
	}
	if !sawReasoning {
		t.Error("stream-with-raw: no reasoning text surfaced on any native event")
	}
	t.Logf("SSE-replay /stream-with-raw+reasoning: %d events byte-identical native==BAML (raw+reasoning present)", len(native.events))
}

// replayTrace drives one leg (native flag-on, or BAML flag-off) over the SSE events and
// returns the ordered event trace.
func replayTrace(t *testing.T, events []string, flagOn bool, mode bamlutils.StreamMode, includeReasoning bool) streamTrace {
	t.Helper()
	spy := newStreamServeSpy(t)
	server := newFixtureStreamServer(t, events)
	a := buildFixtureStreamAdapter(t, spy, flagOn, mode, includeReasoning)
	ch, err := fixture.StaticRecursiveAliasJSON(a, &fixture.StaticRecursiveAliasJsonInput{Topic: "arbitrary json"})
	tr := drainStreamTrace(t, ch, err)
	server.close()
	if flagOn && spy.calls.Load() != 1 {
		t.Fatalf("flag-on: native serve invoked %d times, want 1", spy.calls.Load())
	}
	if !flagOn && spy.calls.Load() != 0 {
		t.Fatalf("flag-off: native serve invoked %d times, want 0", spy.calls.Load())
	}
	return tr
}

// ---- mid-multibyte-UTF-8 raw-byte split (no-panic) ---------------------------------------

// TestAliasStreamSSEReplay_MidUTF8Split flushes the SSE response bytes with a split INSIDE a
// multibyte UTF-8 sequence (the ☕ in a content delta), proving the transport/SSE decoder
// reassembles the raw byte stream and the native parser never panics + still matches BAML.
func TestAliasStreamSSEReplay_MidUTF8Split(t *testing.T) {
	// One content frame carrying `["☕","x"]`; ☕ = 0xE2 0x98 0x95.
	frame := []byte(`data: ` + openAIChunk(`{"content":"[\"☕\",\"x\"]"}`, "") + "\n\n")
	// Find the ☕ bytes and split the raw stream in the MIDDLE of them.
	cup := []byte{0xE2, 0x98, 0x95}
	idx := bytes.Index(frame, cup)
	if idx < 0 {
		t.Fatal("test setup: ☕ bytes not found in the frame")
	}
	closer := []byte("data: " + openAIChunk(`{}`, "stop") + "\n\ndata: [DONE]\n\n")

	drive := func(flagOn bool) streamTrace {
		spy := newStreamServeSpy(t)
		server := newRawSSEServer(t, func(w http.ResponseWriter, fl http.Flusher) {
			// role opener, then the content frame SPLIT mid-☕, then the closer.
			fmt.Fprintf(w, "data: %s\n\n", openAIChunk(`{"role":"assistant"}`, ""))
			if fl != nil {
				fl.Flush()
			}
			_, _ = w.Write(frame[:idx+1]) // ends mid-☕ (after 0xE2)
			if fl != nil {
				fl.Flush()
			}
			_, _ = w.Write(frame[idx+1:]) // the rest of ☕ + frame tail
			if fl != nil {
				fl.Flush()
			}
			_, _ = w.Write(closer)
			if fl != nil {
				fl.Flush()
			}
		})
		a := buildFixtureStreamAdapter(t, spy, flagOn, bamlutils.StreamModeStream, false)
		ch, err := fixture.StaticRecursiveAliasJSON(a, &fixture.StaticRecursiveAliasJsonInput{Topic: "arbitrary json"})
		tr := drainStreamTrace(t, ch, err)
		server.close()
		return tr
	}

	native := drive(true) // must not panic
	baml := drive(false)
	if native.winner != bamlutils.NativeServeEngineNative {
		t.Fatalf("mid-UTF-8: native leg did not serve natively (winner=%q)", native.winner)
	}
	assertTraceEqual(t, "mid-utf8-split", native, baml)
	// The reassembled multibyte content must still yield a genuine FINAL: search every event
	// (the final need not be last) for a StreamResultKindFinal carrying a non-empty final
	// value, proving the mid-☕ split did not truncate the coerced result to empty.
	foundFinal := false
	for _, e := range native.events {
		if e.kind == bamlutils.StreamResultKindFinal && e.final != "" {
			foundFinal = true
			break
		}
	}
	if !foundFinal {
		t.Fatalf("mid-utf8-split: no StreamResultKindFinal event with a non-empty final value; events=%+v", native.events)
	}
	t.Logf("SSE-replay mid-UTF-8 raw split: no panic, %d events byte-identical native==BAML", len(native.events))
}

// ---- serving ownership: flag-on native, flag-off BAML, post-first-byte fault -------------

func TestGeneratedStaticStream_FlagOnServesNative(t *testing.T) {
	events := contentSSE([]string{"[1,", `"x",`, "true]"}, nil)
	spy := newStreamServeSpy(t)
	server := newFixtureStreamServer(t, events)
	a := buildFixtureStreamAdapter(t, spy, true, bamlutils.StreamModeStream, false)
	ch, err := fixture.StaticRecursiveAliasJSON(a, &fixture.StaticRecursiveAliasJsonInput{Topic: "arbitrary json"})
	tr := drainStreamTrace(t, ch, err)
	server.close()

	if tr.drainer != nil {
		t.Fatalf("native stream serve error: %v", tr.drainer)
	}
	if tr.planned != "native" || tr.winner != bamlutils.NativeServeEngineNative {
		t.Errorf("planned=%q winner=%q, want native (stream plan-compare match)", tr.planned, tr.winner)
	}
	if got := server.count.Load(); got != 1 {
		t.Errorf("provider saw %d requests, want EXACTLY 1 (one DoStream, no resend)", got)
	}
	if spy.calls.Load() != 1 {
		t.Errorf("native stream serve invoked %d times, want 1", spy.calls.Load())
	}
	var final string
	for _, e := range tr.events {
		if e.kind == bamlutils.StreamResultKindFinal {
			final = e.final
		}
	}
	if final != `[1,"x",true]` {
		t.Errorf("native final = %q, want %q", final, `[1,"x",true]`)
	}
}

func TestGeneratedStaticStream_FlagOff(t *testing.T) {
	events := contentSSE([]string{"[1,", `"x",`, "true]"}, nil)
	spy := newStreamServeSpy(t)
	server := newFixtureStreamServer(t, events)
	a := buildFixtureStreamAdapter(t, spy, false, bamlutils.StreamModeStream, false)
	ch, err := fixture.StaticRecursiveAliasJSON(a, &fixture.StaticRecursiveAliasJsonInput{Topic: "arbitrary json"})
	tr := drainStreamTrace(t, ch, err)
	server.close()

	if tr.drainer != nil {
		t.Fatalf("flag-off (BAML) stream error: %v", tr.drainer)
	}
	if spy.calls.Load() != 0 {
		t.Errorf("flag-off: native stream serve invoked %d times, want 0 (seam hard-off)", spy.calls.Load())
	}
	if tr.winner == bamlutils.NativeServeEngineNative {
		t.Errorf("flag-off winner=%q, must NOT be native", tr.winner)
	}
}

// TestGeneratedStaticStream_PostFirstBodyFaultTerminal injects a fault AFTER a first emitted
// body byte (a valid role+content frame is flushed, THEN the connection is aborted mid-stream
// with no [DONE]) and asserts the native stream FAILS terminally with EXACTLY one provider
// socket and NO BAML resend/retry/fallback/pool-replay.
func TestGeneratedStaticStream_PostFirstBodyFaultTerminal(t *testing.T) {
	spy := newStreamServeSpy(t)
	server := newRawSSEServer(t, func(w http.ResponseWriter, fl http.Flusher) {
		// First BODY bytes: a role opener + a content delta (a first partial emits).
		fmt.Fprintf(w, "data: %s\n\n", openAIChunk(`{"role":"assistant"}`, ""))
		fmt.Fprintf(w, "data: %s\n\n", openAIChunk(`{"content":"[1,2"}`, ""))
		if fl != nil {
			fl.Flush()
		}
		// Abort the response AFTER the first body byte — no [DONE], no clean close.
		panic(http.ErrAbortHandler)
	})
	a := buildFixtureStreamAdapter(t, spy, true, bamlutils.StreamModeStream, false)
	ch, err := fixture.StaticRecursiveAliasJSON(a, &fixture.StaticRecursiveAliasJsonInput{Topic: "arbitrary json"})
	tr := drainStreamTrace(t, ch, err)
	server.close()

	if tr.drainer == nil {
		t.Fatal("post-first-byte abort must surface a TERMINAL stream error, got nil")
	}
	// The premise is a POST-first-BYTE fault: a partial MUST have emitted before the abort.
	// Without this, a pre-emission fault would still surface a terminal error and pass above,
	// silently degrading the no-fallback-after-first-byte proof into a pre-emission case.
	sawPartial := false
	for _, e := range tr.events {
		if e.kind == bamlutils.StreamResultKindStream {
			sawPartial = true
			break
		}
	}
	if !sawPartial {
		t.Error("no partial emitted before the abort — the fault was NOT post-first-body (the terminal-error assertion alone would not prove post-claim terminality)")
	}
	if got := server.count.Load(); got != 1 {
		t.Errorf("provider saw %d requests, want EXACTLY 1 (no BAML resend/retry/replay after the native claim)", got)
	}
	if tr.winner == bamlutils.NativeServeEngineNative {
		t.Errorf("a failed native stream must not report winner=native (got %q)", tr.winner)
	}
}

// ---- decline manifest: EVERY non-JSON static stream method is BAML-served ----------------

// TestStaticStreamManifest is the SIBLING stream partition manifest (frozen 8C/Phase-2/
// Phase-3a-final/dynamic manifests untouched): EXACTLY StaticRecursiveAliasJSON is served in
// stream natively; EVERY other static-serve stream method declines pre-transport → 0 native
// sockets, exactly 1 BAML request, no native winner.
func TestStaticStreamManifest(t *testing.T) {
	events := contentSSE([]string{`"served"`}, nil)

	// Served row: the exact JSON alias.
	{
		spy := newStreamServeSpy(t)
		server := newFixtureStreamServer(t, events)
		a := buildFixtureStreamAdapter(t, spy, true, bamlutils.StreamModeStream, false)
		ch, err := fixture.StaticRecursiveAliasJSON(a, &fixture.StaticRecursiveAliasJsonInput{Topic: "j"})
		tr := drainStreamTrace(t, ch, err)
		server.close()
		if tr.winner != bamlutils.NativeServeEngineNative {
			t.Errorf("StaticRecursiveAliasJSON stream winner=%q, want native (the ONLY stream-served method)", tr.winner)
		}
	}

	// Declined rows: EVERY other fixture static-serve stream method → BAML-served with ZERO
	// native sockets + exactly ONE provider request + no native winner.
	for _, row := range declinedStreamRows() {
		t.Run(row.name, func(t *testing.T) {
			spy := newStreamServeSpy(t)
			// A plain-string SSE: BAML always ISSUES its one request regardless of whether
			// its own parse of the content matches the method's schema — the manifest proves
			// the NATIVE decline (0 sockets) + one BAML request, not BAML's parse outcome.
			server := newFixtureStreamServer(t, contentSSE([]string{`"served"`}, nil))
			a := buildFixtureStreamAdapter(t, spy, true, bamlutils.StreamModeStream, false)
			ch, err := row.drive(a)
			tr := drainStreamTrace(t, ch, err)
			bamlRequests := server.count.Load()
			zeroSockets := spy.nativeSocketsZero()
			server.close()

			// Distinguish "the callback ran and DECLINED" from "no callback was ever
			// installed". nativeSocketsZero() accepts BOTH (calls == 0 || declined), so
			// on its own it would stay green if codegen dropped this method's descriptor
			// or projector: the route would fall back to the ordinary BAML path — a safe
			// over-decline, but it would no longer prove the NATIVE pre-transport refusal
			// this manifest exists for.
			calls := spy.calls.Load()
			if row.callbackExpected {
				if calls != 1 {
					t.Fatalf("%s stream: serve func invoked %d times, want 1 — the method has an emitted descriptor + projector, so the seam MUST install the callback (a codegen loss would read as 0)", row.name, calls)
				}
				disp, stage, reason := spy.lastDecline()
				if disp != int64(bamlutils.NativeStreamDeclined) {
					t.Errorf("%s stream: disposition=%d, want NativeStreamDeclined (stage=%q reason=%q)", row.name, disp, stage, reason)
				}
				// The STAGE is uniform and pinned: both gates these rows reach are
				// StagePrompt — the stream return-shape gate
				// (admission/static_stream.go, reasonReturnShapeUnproven) and the
				// native-final bundle check (admission/static.go,
				// reasonReturnBundleFinalUnsupported). Pinning it is what proves a
				// PROMPT-stage refusal; a gate moved to another stage would otherwise
				// keep this matrix green with a merely non-empty token.
				if stage != "prompt" {
					t.Errorf("%s stream: declined at stage=%q, want %q (reason=%q)", row.name, stage, "prompt", reason)
				}
				// The REASON is deliberately NOT pinned per row: these rows decline for
				// genuinely different causes (return_shape_decoder_unproven vs
				// return_bundle_native_final_unsupported), so a fixed value would be the
				// wrong assertion. But a decline that names nothing is not diagnosable.
				if reason == "" {
					t.Errorf("%s stream: declined with an empty reason (stage=%q), want a bounded reason token", row.name, stage)
				}
			} else if calls != 0 {
				t.Errorf("%s stream: serve func invoked %d times, want 0 (no descriptor/projector is emitted, so no callback should be installed)", row.name, calls)
			}
			if tr.winner == bamlutils.NativeServeEngineNative {
				t.Errorf("%s stream winner=%q, must NOT be native (declines pre-transport)", row.name, tr.winner)
			}
			if !zeroSockets {
				t.Errorf("%s stream: native lane opened a socket (last disposition=%d), want 0 (declined pre-transport)", row.name, spy.lastDisp.Load())
			}
			if bamlRequests != 1 {
				t.Errorf("%s stream: provider saw %d requests, want exactly 1 (BAML serves the single request)", row.name, bamlRequests)
			}
		})
	}
}

// declinedStreamRow is one non-JSON static-serve stream method that must decline natively.
type declinedStreamRow struct {
	// callbackExpected records whether codegen emitted BOTH a descriptor and an
	// argument projector for this method — i.e. whether the generated stream
	// adapter installs the native callback at all.
	//
	//   true  -> the callback MUST run and return NativeStreamDeclined.
	//   false -> the seam never installs one, so it must NOT run (calls == 0).
	//
	// This is pinned BY HAND and deliberately NOT derived from
	// introspected.StaticPromptDescriptors/ArgumentProjectors: deriving it would
	// let a codegen loss silently flip a row to "no callback expected" and keep
	// this matrix green, which is exactly the regression the flag exists to catch.
	callbackExpected bool

	name  string
	drive func(a bamlutils.Adapter) (<-chan bamlutils.StreamResult, error)
}

// callbackRow builds a declined row for a method that HAS an emitted descriptor and
// argument projector, so the generated seam installs the native callback and it must
// RUN and decline. Every row below is one: the fixture's only descriptor-less methods
// are the three media routes in introspected.StaticPromptDeclines, and those are not
// stream-driven here (generated_media_unreachable_e2e_test.go owns them).
func callbackRow(name string, drive func(a bamlutils.Adapter) (<-chan bamlutils.StreamResult, error)) declinedStreamRow {
	return declinedStreamRow{callbackExpected: true, name: name, drive: drive}
}

// declinedStreamRows enumerates EVERY non-JSON fixture static-serve stream method (the
// FINAL-served-but-STREAM-DECLINED JsonValue alias and its arm-reordered witness, both
// recursive-class SCCs + their annotated/loop variants, the flat/scalar classes, and the
// multi-arg/role methods). None is the exact five-arm JSON alias, so all decline
// pre-transport.
//
// De-BAML Phase 3c KEEPS StaticRecursiveAliasJsonValue here even though it is served on
// the FINAL lane. The static-stream gate admits by DESCRIPTOR SHAPE pre-socket, and a
// claimed native stream has no route back to BAML (a partial-parser error becomes no
// event; a final-parser error is TERMINAL), so a family whose parse can decline on a
// VALUE — the shared #583 jsonish-recovery residual plus this family's negative-zero
// decline — must not claim a stream socket. The unary lane has no such hazard (BAML
// parse-only repairs the same response), which is why the FINAL manifest serves it. See
// internal/debaml/static_stream_serve.go.
func declinedStreamRows() []declinedStreamRow {
	return []declinedStreamRow{
		callbackRow("StaticRecursiveAliasJsonValue", func(a bamlutils.Adapter) (<-chan bamlutils.StreamResult, error) {
			return fixture.StaticRecursiveAliasJsonValue(a, &fixture.StaticRecursiveAliasJsonValueInput{Topic: "wide"})
		}),
		callbackRow("StaticRecursiveAliasJsonValueReordered", func(a bamlutils.Adapter) (<-chan bamlutils.StreamResult, error) {
			return fixture.StaticRecursiveAliasJsonValueReordered(a, &fixture.StaticRecursiveAliasJsonValueReorderedInput{Topic: "reordered"})
		}),
		callbackRow("StaticCompletion", func(a bamlutils.Adapter) (<-chan bamlutils.StreamResult, error) {
			return fixture.StaticCompletion(a, &fixture.StaticCompletionInput{Topic: "t"})
		}),
		callbackRow("StaticCompletionOutputFormat", func(a bamlutils.Adapter) (<-chan bamlutils.StreamResult, error) {
			return fixture.StaticCompletionOutputFormat(a, &fixture.StaticCompletionOutputFormatInput{Topic: "t"})
		}),
		callbackRow("StaticOutputFormat", func(a bamlutils.Adapter) (<-chan bamlutils.StreamResult, error) {
			return fixture.StaticOutputFormat(a, &fixture.StaticOutputFormatInput{Topic: "t"})
		}),
		callbackRow("StaticPrimitiveArgs", func(a bamlutils.Adapter) (<-chan bamlutils.StreamResult, error) {
			return fixture.StaticPrimitiveArgs(a, &fixture.StaticPrimitiveArgsInput{Text: "t", Count: 1, Ratio: 1, Flag: true})
		}),
		callbackRow("StaticRecursiveA", func(a bamlutils.Adapter) (<-chan bamlutils.StreamResult, error) {
			return fixture.StaticRecursiveA(a, &fixture.StaticRecursiveAInput{Topic: "t"})
		}),
		callbackRow("StaticRecursiveB", func(a bamlutils.Adapter) (<-chan bamlutils.StreamResult, error) {
			return fixture.StaticRecursiveB(a, &fixture.StaticRecursiveBInput{Topic: "t"})
		}),
		callbackRow("StaticRecursiveLoop", func(a bamlutils.Adapter) (<-chan bamlutils.StreamResult, error) {
			return fixture.StaticRecursiveLoop(a, &fixture.StaticRecursiveLoopInput{Topic: "t"})
		}),
		callbackRow("StaticRecursiveNode", func(a bamlutils.Adapter) (<-chan bamlutils.StreamResult, error) {
			return fixture.StaticRecursiveNode(a, &fixture.StaticRecursiveNodeInput{Topic: "t"})
		}),
		callbackRow("StaticRecursiveNodeAnn", func(a bamlutils.Adapter) (<-chan bamlutils.StreamResult, error) {
			return fixture.StaticRecursiveNodeAnn(a, &fixture.StaticRecursiveNodeAnnInput{Topic: "t"})
		}),
		callbackRow("StaticRoleChat", func(a bamlutils.Adapter) (<-chan bamlutils.StreamResult, error) {
			return fixture.StaticRoleChat(a, &fixture.StaticRoleChatInput{Topic: "t", Count: 1})
		}),
	}
}
