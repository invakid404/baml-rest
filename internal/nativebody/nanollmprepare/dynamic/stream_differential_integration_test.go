//go:build integration && nanollm_integration

package dynamic

// De-BAML Phase 7C — the GATED two-leg BAML-vs-native STREAMING loopback
// differential (the core 7C green bar), reworked per the cold-review findings.
//
// It stands up a deterministic scripted OpenAI SSE server and runs, over the
// ADMITTED schema surface (SupportsNativeStream nil) and BOTH public streaming
// modes:
//
//  1. ORACLE leg — BAML-as-served: dynclient.DynamicStream / DynamicStreamRaw
//     WithDeBAML(false). BAML StreamRequest + BAML parse-stream/final through the
//     real BuildRequest orchestrator. This is the reference StreamResult cadence.
//
//  2. ORCHESTRATOR-PARITY leg — the SAME real route WithDeBAML(true) + the native
//     parser: proves the native parser reproduces the FULL StreamResult sequence
//     (including planned metadata, raw/reasoning accumulation and byte order) with
//     a native-parser FALLBACK COUNT OF ZERO — i.e. NO admitted prefix invoked BAML
//     for partial OR final parsing (I6).
//
//  3. NATIVE-TRANSPORT leg — the literal §6.2 candidate: admission.AdmitStreamClaim +
//     execute.RunStream (the 7B exact one-shot lane, ONE accepted TCP socket) driving
//     BOTH native-only closures — debaml.ParseNativeStreamPartial per frame AND
//     debaml.ParseNativeStreamFinal at the end — reproducing the orchestrator cadence.
//     Its partial+final DATA sequence must equal the oracle's byte-for-byte, with NO
//     BAML parser or BAML provider send in the leg at all.
//
// The comparator is STRICT: byte-exact StreamResult Data (flattened JSON, key order
// PRESERVED — no normalizing re-marshal), metadata event count+order, raw/reasoning
// accumulation, and reset. Exactly one provider request + one accepted TCP connection
// per leg.
//
// The admitted surface is the §11 matrix SupportsNativeStream enforces (proven
// divergence-free byte-by-byte vs live BAML in integration/bamlfuzz_parse_recovery
// and the acceptance probe): a >=2-field (or single non-string-absorbing) required-
// field class, no unions/optionals, no non-last unquoted-scalar field, no scalar
// map value, no single string-absorbing field — string / list / map<string,
// non-scalar> / nested-class / enum / last-scalar values. Streams are SPACED JSON
// (BAML's tokenization), the case a native lane serves.
//
// Egress fence: literal fence model/alias/api key, a 127.0.0.1 loopback base, nanollm
// UseProcessEnv:false / MaxRetries:0, both HTTP clients Proxy:nil + the shared
// loopbackDial guard. Gated integration && nanollm_integration; nothing here is
// reachable from production routing.

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/dynclient"
	"github.com/invakid404/baml-rest/internal/debaml"
	"github.com/invakid404/baml-rest/internal/nativebody"
	"github.com/invakid404/baml-rest/internal/nativeprompt"
	"github.com/invakid404/baml-rest/nativeserve/admission"
	"github.com/invakid404/baml-rest/nativeserve/execute"
)

const streamDiffTimeout = 60 * time.Second

// --- scripted OpenAI SSE server ---

type sseDelta struct {
	Content   string
	Reasoning string
}

// scriptedSSEServer serves a deterministic OpenAI chat.completions SSE stream and
// counts accepted TCP connections (ConnState StateNew) so the one-socket invariant
// is asserted against REAL sockets. fragment writes each event one byte at a time
// (arbitrary TCP fragmentation, §6.3) — the decoder must reassemble it identically.
// status!=200 / truncate exercise the fault matrix.
type scriptedSSEServer struct {
	t        *testing.T
	srv      *httptest.Server
	deltas   []sseDelta
	fragment bool
	status   int
	body     []byte // non-2xx body
	truncate bool   // close mid-stream without [DONE]

	conns atomic.Int64
	mu    sync.Mutex
	recs  []recordedLive
}

func newScriptedSSEServer(t *testing.T, deltas []sseDelta) *scriptedSSEServer {
	t.Helper()
	s := &scriptedSSEServer{t: t, deltas: deltas, status: http.StatusOK}
	s.srv = httptest.NewServer(http.HandlerFunc(s.handle))
	s.srv.Config.ConnState = func(_ net.Conn, st http.ConnState) {
		if st == http.StateNew {
			s.conns.Add(1)
		}
	}
	t.Cleanup(s.srv.Close)
	return s
}

func (s *scriptedSSEServer) base() string   { return s.srv.URL }
func (s *scriptedSSEServer) connCount() int { return int(s.conns.Load()) }

func (s *scriptedSSEServer) reqCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.recs)
}

func (s *scriptedSSEServer) rec(i int) recordedLive {
	s.t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if i >= len(s.recs) {
		s.t.Fatalf("scriptedSSEServer: no request at index %d (have %d)", i, len(s.recs))
	}
	return s.recs[i]
}

func (s *scriptedSSEServer) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	s.recs = append(s.recs, recordedLive{Method: r.Method, RequestURI: r.RequestURI, Host: r.Host, Header: r.Header.Clone(), Body: body})
	s.mu.Unlock()

	if s.status != http.StatusOK {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(s.status)
		_, _ = w.Write(s.body)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		s.t.Fatalf("scriptedSSEServer: ResponseWriter is not a Flusher")
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	write := func(payload string) {
		// NO timing barrier: delivery is DETERMINISTIC by construction. The orchestrator's
		// partial send targets the dynclient result channel, which is buffered at 100
		// (dynclient/internal/generated/adapter.go — Baml_Rest_Dynamic `make(chan …, 100)`),
		// and each fixture scripts far fewer than 100 partial-producing deltas. So the
		// nonblocking send's drop branch (orchestrator.go trySendPartialShared `default:`)
		// can never fire here regardless of scheduler timing — every partial is enqueued and
		// the blocking pull consumer (dynclient Stream.Next) drains them in order. (Replaces a
		// prior time.Sleep(2ms) that only made the drop unlikely, not impossible.)
		frame := "data: " + payload + "\n\n"
		if s.fragment {
			for i := 0; i < len(frame); i++ {
				_, _ = io.WriteString(w, frame[i:i+1])
				flusher.Flush()
			}
			return
		}
		_, _ = io.WriteString(w, frame)
		flusher.Flush()
	}
	for i, d := range s.deltas {
		delta := map[string]any{}
		if d.Content != "" {
			delta["content"] = d.Content
		}
		if d.Reasoning != "" {
			delta["reasoning_content"] = d.Reasoning
		}
		write(mustJSON(s.t, map[string]any{
			"id": "chatcmpl-test", "object": "chat.completion.chunk", "model": fenceModel,
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": nil}},
		}))
		if s.truncate && i == len(s.deltas)/2 {
			return // close mid-stream, no finish/usage/[DONE]
		}
	}
	write(mustJSON(s.t, map[string]any{"id": "chatcmpl-test", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}}))
	write(mustJSON(s.t, map[string]any{"id": "chatcmpl-test", "choices": []any{}, "usage": map[string]any{"prompt_tokens": 11, "completion_tokens": 7, "total_tokens": 18}}))
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal chunk: %v", err)
	}
	return string(b)
}

// --- fixtures (admitted surface, SPACED streams) ---

type streamDiffFixture struct {
	name      string
	schema    *bamlutils.DynamicOutputSchema
	messages  []nativeprompt.Message
	deltas    []sseDelta
	reasoning bool // exercises the include_reasoning channel (stream-with-raw only)
	fenced    bool // scripted inside a ```json fence (exercises fence-preamble + close)
}

func dstr() *bamlutils.DynamicProperty { return &bamlutils.DynamicProperty{Type: "string"} }
func dint() *bamlutils.DynamicProperty { return &bamlutils.DynamicProperty{Type: "int"} }
func dsch(kv ...bamlutils.OrderedEntry[*bamlutils.DynamicProperty]) *bamlutils.DynamicOutputSchema {
	return &bamlutils.DynamicOutputSchema{Properties: bamlutils.MustOrderedMap(kv...)}
}
func userMsg(text string) []nativeprompt.Message {
	return []nativeprompt.Message{{Role: "user", Content: sp(text)}}
}
func contentDeltas(pieces ...string) []sseDelta {
	out := make([]sseDelta, 0, len(pieces))
	for _, p := range pieces {
		out = append(out, sseDelta{Content: p})
	}
	return out
}

func streamDiffFixtures() []streamDiffFixture {
	person := dsch(bamlutils.OrderedKV("name", dstr()), bamlutils.OrderedKV("age", dint()))
	nameTags := dsch(bamlutils.OrderedKV("name", dstr()), bamlutils.OrderedKV("tags", &bamlutils.DynamicProperty{Type: "list", Items: &bamlutils.DynamicTypeSpec{Type: "string"}}))
	labeledNums := dsch(bamlutils.OrderedKV("label", dstr()), bamlutils.OrderedKV("nums", &bamlutils.DynamicProperty{Type: "list", Items: &bamlutils.DynamicTypeSpec{Type: "int"}}))
	// map<string,string> is DECLINED pre-transport (BAML overwrite-on-duplicate-key
	// native cannot reproduce), so this admitted-fixture list uses a list<string>+int
	// 3-field class instead (a class with the int LAST, which the matrix admits).
	strListInt := dsch(bamlutils.OrderedKV("name", dstr()), bamlutils.OrderedKV("tags", &bamlutils.DynamicProperty{Type: "list", Items: &bamlutils.DynamicTypeSpec{Type: "string"}}), bamlutils.OrderedKV("count", dint()))
	nested := &bamlutils.DynamicOutputSchema{
		Properties: bamlutils.MustOrderedMap(bamlutils.OrderedKV("label", dstr()), bamlutils.OrderedKV("inner", &bamlutils.DynamicProperty{Ref: "Inner"})),
		Classes:    bamlutils.MustOrderedMap(bamlutils.OrderedKV("Inner", &bamlutils.DynamicClass{Properties: bamlutils.MustOrderedMap(bamlutils.OrderedKV("x", dstr()), bamlutils.OrderedKV("y", dint()))})),
	}
	answerScore := dsch(bamlutils.OrderedKV("answer", dstr()), bamlutils.OrderedKV("score", dint()))

	return []streamDiffFixture{
		{name: "person", schema: person, messages: userMsg("Give a person."),
			deltas: contentDeltas(`{`, `"name": "`, `Ada`, `", "age": `, `36`, `}`)},
		{name: "name_tags", schema: nameTags, messages: userMsg("Give tags."),
			deltas: contentDeltas(`{`, `"name": "`, `Ada`, `", "tags": [`, `"x"`, `, "y"`, `]}`)},
		{name: "labeled_nums", schema: labeledNums, messages: userMsg("Give nums."),
			deltas: contentDeltas(`{`, `"label": "`, `L`, `", "nums": [`, `1`, `, 2`, `]}`)},
		{name: "str_list_int", schema: strListInt, messages: userMsg("Give tags and count."),
			deltas: contentDeltas(`{`, `"name": "`, `A`, `", "tags": [`, `"x"`, `, "y"`, `], "count": `, `2`, `}`)},
		{name: "nested", schema: nested, messages: userMsg("Give nested."),
			deltas: contentDeltas(`{`, `"label": "`, `L`, `", "inner": {`, `"x": "v"`, `, "y": 9`, `}}`)},
		{name: "person_fenced", schema: person, messages: userMsg("Fenced person."), fenced: true,
			deltas: contentDeltas("Here you go:\n", "```json\n", `{`, `"name": "`, `Ada`, `", "age": `, `36`, `}`, "\n`", "``", "`")},
		{name: "answer_reasoning", schema: answerScore, messages: userMsg("Reason then answer."), reasoning: true,
			deltas: []sseDelta{{Reasoning: "let me think"}, {Content: `{`}, {Content: `"answer": "`}, {Reasoning: " still"}, {Content: `hi`}, {Content: `", "score": `}, {Content: `5`}, {Content: `}`}}},
	}
}

// --- captured StreamResult event (byte-exact) ---

type wireEvent struct {
	kind      dynclient.EventKind
	data      string // raw flattened Data bytes (key order preserved)
	raw       string
	reasoning string
}

// drainDynclient drains a dynclient stream into the FULL wireEvent sequence,
// retaining metadata / reset / partial / final in order (P2-a: metadata count+order
// is compared, not dropped).
func drainDynclient(t *testing.T, s *dynclient.Stream) []wireEvent {
	t.Helper()
	var out []wireEvent
	for {
		ev, err := s.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return append(out, wireEvent{kind: "error", data: err.Error()})
		}
		out = append(out, wireEvent{kind: ev.Kind, data: string(ev.Data), raw: ev.Raw, reasoning: ev.Reasoning})
	}
	return out
}

func nonMeta(evs []wireEvent) []wireEvent {
	out := make([]wireEvent, 0, len(evs))
	for _, e := range evs {
		if e.kind == dynclient.EventMetadata {
			continue
		}
		out = append(out, e)
	}
	return out
}

// --- native-parser spy (orchestrator-parity leg) ---

type streamDiffSpy struct {
	streamClaims, streamFallbacks, streamErrors atomic.Int64
	finalClaims, finalFallbacks, finalErrors    atomic.Int64
}

func (s *streamDiffSpy) Parse(ctx context.Context, req bamlutils.DeBAMLParseRequest) (bamlutils.DeBAMLParseResult, error) {
	res, err := debaml.Parse(ctx, req)
	claim, fb, e := &s.finalClaims, &s.finalFallbacks, &s.finalErrors
	if req.Stream {
		claim, fb, e = &s.streamClaims, &s.streamFallbacks, &s.streamErrors
	}
	switch {
	case err == nil:
		claim.Add(1)
	case unwrapIs(err, bamlutils.ErrDeBAMLParseUnsupported):
		fb.Add(1)
	default:
		e.Add(1)
	}
	return res, err
}

func unwrapIs(err, target error) bool {
	for e := err; e != nil; {
		if e == target {
			return true
		}
		u, ok := e.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		e = u.Unwrap()
	}
	return false
}

// --- leg runners ---

func streamDiffRequest(base string, f streamDiffFixture, withRaw bool) dynclient.Request {
	preserve := true
	return dynclient.Request{
		Messages: toDynMessages(f.messages), ClientRegistry: liveOracleRegistry(base), OutputSchema: f.schema,
		PreserveSchemaOrder: &preserve, IncludeReasoning: withRaw && f.reasoning,
	}
}

func runDynclientLeg(t *testing.T, deBAML bool, spy *streamDiffSpy, base string, f streamDiffFixture, withRaw bool) []wireEvent {
	t.Helper()
	opts := []dynclient.Option{dynclient.WithClientMode(llmhttp.ClientModeNetHTTP), dynclient.WithNetHTTPClient(loopbackOracleHTTPClient()), dynclient.WithDeBAML(deBAML)}
	if deBAML {
		opts = append(opts, dynclient.WithDeBAMLRenderer(debaml.Render), dynclient.WithDeBAMLParser(spy.Parse))
	}
	c, err := dynclient.New(opts...)
	if err != nil {
		t.Fatalf("dynclient.New(deBAML=%v): %v", deBAML, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), streamDiffTimeout)
	defer cancel()
	var (
		stream *dynclient.Stream
		serr   error
	)
	if withRaw {
		stream, serr = c.DynamicStreamRaw(ctx, streamDiffRequest(base, f, withRaw))
	} else {
		stream, serr = c.DynamicStream(ctx, streamDiffRequest(base, f, withRaw))
	}
	if serr != nil {
		t.Fatalf("DynamicStream(deBAML=%v,withRaw=%v): %v", deBAML, withRaw, serr)
	}
	defer stream.Close()
	return drainDynclient(t, stream)
}

// candidateSpy is the native-transport candidate leg's own §6.2 zero-BAML evidence,
// recorded from REAL runtime events rather than structural constants (so a future
// regression that reintroduced a BAML path trips it):
//
//   - bamlParse increments IFF the native-only FINAL closure declines an admitted
//     final — the EXACT boundary at which a hybrid caller would RE-PARSE the final
//     through BAML (the only BAML-parse path the candidate could take). It is wired
//     to that branch in runNativeTransportLeg, which no longer fatals there but records
//     and returns so this counter is the single detector.
//   - bamlSend is set (by the caller, after the leg runs) from the scripted server's
//     ACTUAL request count minus the single native exact send: any provider request
//     BEYOND that one native send is an extra (e.g. BAML llmhttp) send.
//   - nativeFinalInvariant increments on the terminal ErrDeBAMLNativeStreamUnsupported;
//     nativePartialEmits / nativeFinalCalls pin the closures were actually exercised.
//
// All of bamlParse, bamlSend, nativeFinalInvariant MUST be 0 across the admitted
// corpus + fault set.
type candidateSpy struct {
	nativePartialCalls, nativePartialEmits atomic.Int64
	nativeFinalCalls, nativeFinalInvariant atomic.Int64
	bamlParse, bamlSend                    atomic.Int64
}

// runNativeTransportLeg drives the LITERAL §6.2 candidate: AdmitStreamClaim +
// execute.RunStream (one exact socket) + BOTH native-only closures reproducing the
// orchestrator cadence (zero throttle: parse at every content frame, matching the
// zero-default ParseThrottleInterval the oracle uses). It returns the partial+final
// wireEvent sequence and records the candidate's native/zero-BAML counters in spy.
func runNativeTransportLeg(t *testing.T, srv *scriptedSSEServer, f streamDiffFixture, withRaw bool, spy *candidateSpy) []wireEvent {
	t.Helper()
	client := newLiveNativeClient(t, srv.base())
	defer client.Close()
	adm := admission.NewAdmitter(nil, liveExactExecutor())
	claim, err := adm.AdmitStreamClaim(context.Background(), nativeAdmissionInput(srv.base(), f, withRaw))
	if err != nil {
		t.Fatalf("AdmitStreamClaim: %v", err)
	}
	defer claim.Close()

	var events []wireEvent
	var accParse, accRaw, accReason strings.Builder
	emit := func(data []byte) {
		ev := wireEvent{kind: dynclient.EventPartial}
		if data != nil {
			flat, ferr := bamlutils.FlattenDynamicOutput(data)
			if ferr != nil {
				flat = data
			}
			ev.data = string(flat)
		} else {
			// A reasoning/raw-only partial carries NO parsed value; the orchestrator
			// forwards a nil stream value, which dynclient serializes as JSON null.
			ev.data = "null"
		}
		if withRaw {
			ev.raw = accRaw.String()
			ev.reasoning = accReason.String()
		}
		events = append(events, ev)
	}
	_, rerr := execute.RunStream(context.Background(), execute.StreamConfig{
		Client:           claim.Client(),
		Request:          claim.Request(),
		Expected:         claim.ExactRequest,
		IncludeReasoning: withRaw && f.reasoning,
		FirstBodyTimeout: 15 * time.Second,
		IdleTimeout:      15 * time.Second,
		EmitDelta: func(d execute.StreamDelta) error {
			accParse.WriteString(d.ParseableDelta)
			accRaw.WriteString(d.RawDelta)
			accReason.WriteString(d.ReasoningDelta)
			if d.ParseableDelta == "" {
				// Reasoning/raw-only frame: the orchestrator emits a raw/reasoning
				// partial ONLY in with-raw mode; plain stream drops it.
				if withRaw && (d.RawDelta != "" || d.ReasoningDelta != "") {
					emit(nil)
				}
				return nil
			}
			// Content frame: run the native-only PARTIAL closure on accumulated text.
			spy.nativePartialCalls.Add(1)
			part, perr := debaml.ParseNativeStreamPartial(context.Background(), f.schema, accParse.String())
			if perr != nil {
				return perr
			}
			if part != nil {
				spy.nativePartialEmits.Add(1)
				emit(part)
			}
			return nil
		},
	})
	if rerr != nil {
		t.Fatalf("execute.RunStream: %v", rerr)
	}
	// Native-only FINAL closure + the dynclient final pipeline (flatten + inject
	// absent optionals + reorder-by-schema) so it matches the oracle final byte-exact.
	spy.nativeFinalCalls.Add(1)
	finalJSON, ferr := debaml.ParseNativeStreamFinal(context.Background(), f.schema, accParse.String())
	if ferr != nil {
		// The native-only FINAL closure declined an admitted final. A hybrid caller
		// would RE-PARSE it through BAML at exactly this point — the only BAML-parse
		// the candidate could take. Record it in the spy (bamlParse + the terminal
		// invariant) and RETURN WITHOUT a final event instead of fataling here, so the
		// caller's zero-BAML and wire-parity assertions are the single detector and the
		// bamlParse counter is a genuine runtime observation, not a structural zero.
		if unwrapIs(ferr, debaml.ErrDeBAMLNativeStreamUnsupported) {
			spy.nativeFinalInvariant.Add(1)
		}
		spy.bamlParse.Add(1)
		t.Logf("ParseNativeStreamFinal DECLINED an admitted final (would-be BAML re-parse): %v", ferr)
		return events
	}
	fin := finalPipeline(t, finalJSON, f.schema)
	fev := wireEvent{kind: dynclient.EventFinal, data: string(fin)}
	if withRaw {
		fev.raw = accRaw.String()
		fev.reasoning = accReason.String()
	}
	events = append(events, fev)
	return events
}

func finalPipeline(t *testing.T, flat []byte, schema *bamlutils.DynamicOutputSchema) []byte {
	t.Helper()
	out, err := bamlutils.FlattenDynamicOutput(flat)
	if err != nil {
		out = flat
	}
	if out, err = bamlutils.InjectAbsentOptionals(out, schema); err != nil {
		t.Fatalf("InjectAbsentOptionals: %v", err)
	}
	if out, err = bamlutils.ReorderDynamicOutputBySchema(out, schema); err != nil {
		t.Fatalf("ReorderDynamicOutputBySchema: %v", err)
	}
	return out
}

func nativeAdmissionInput(base string, f streamDiffFixture, withRaw bool) admission.Input {
	mode := admission.ModeStream
	if withRaw {
		mode = admission.ModeStreamWithRaw
	}
	msgs := make([]bamlutils.DynamicMessage, 0, len(f.messages))
	for i := range f.messages {
		m := &f.messages[i]
		dm := bamlutils.DynamicMessage{Role: m.Role}
		if m.Content != nil {
			dm.TextContent = m.Content
		}
		msgs = append(msgs, dm)
	}
	return admission.Input{
		WorkerCapable: true, RequestAPIPresent: true, OnBuildRequestRoute: true, FlagEnabled: true,
		Method: "Baml_Rest_Dynamic", Mode: mode, SingleLeaf: true, ResolvedProvider: nativebody.ProviderOpenAI,
		Registry: liveOracleRegistry(base), Alias: fenceAlias, Messages: msgs, OutputSchema: f.schema,
	}
}

// --- comparators (strict byte / metadata) ---

func assertWireParity(t *testing.T, label string, oracle, cand []wireEvent) {
	t.Helper()
	if len(oracle) != len(cand) {
		t.Fatalf("%s: cadence length mismatch oracle=%d cand=%d\n oracle=%+v\n cand  =%+v", label, len(oracle), len(cand), oracle, cand)
	}
	for i := range oracle {
		o, c := oracle[i], cand[i]
		if o.kind != c.kind {
			t.Errorf("%s: event[%d] kind: oracle=%s cand=%s", label, i, o.kind, c.kind)
			continue
		}
		// STRICT byte equality — key order preserved, no normalizing re-marshal.
		if o.data != c.data {
			t.Errorf("%s: event[%d] (%s) DATA mismatch:\n oracle=%s\n cand  =%s", label, i, o.kind, o.data, c.data)
		}
		if o.raw != c.raw {
			t.Errorf("%s: event[%d] (%s) raw mismatch: oracle=%q cand=%q", label, i, o.kind, o.raw, c.raw)
		}
		if o.reasoning != c.reasoning {
			t.Errorf("%s: event[%d] (%s) reasoning mismatch: oracle=%q cand=%q", label, i, o.kind, o.reasoning, c.reasoning)
		}
	}
}

func metaCount(evs []wireEvent) int {
	n := 0
	for _, e := range evs {
		if e.kind == dynclient.EventMetadata {
			n++
		}
	}
	return n
}

// --- the differential ---

// TestStreamDifferential_BAMLvsNative is the two-leg semantic differential over the
// admitted surface and both public modes.
func TestStreamDifferential_BAMLvsNative(t *testing.T) {
	for _, f := range streamDiffFixtures() {
		f := f
		if err := debaml.SupportsNativeStream(f.schema); err != nil {
			t.Fatalf("fixture %q schema is NOT admitted (SupportsNativeStream): %v", f.name, err)
		}
		t.Run(f.name, func(t *testing.T) {
			for _, withRaw := range []bool{false, true} {
				withRaw := withRaw
				if f.reasoning && !withRaw {
					// reasoning is a with-raw-only channel; still run plain mode below to
					// prove reasoning deltas are dropped (no partial) in plain stream.
				}
				mode := "stream"
				if withRaw {
					mode = "stream_with_raw"
				}
				t.Run(mode, func(t *testing.T) { runStreamDifferentialCase(t, f, withRaw, false) })
			}
		})
	}
}

// runStreamDifferentialCase runs all three legs for one fixture/mode. When fragment
// is true, EVERY leg's server delivers each SSE event one byte at a time (the
// IDENTICAL arbitrary-fragmentation schedule on both the BAML oracle and the native
// candidate, §6.2/§6.3), proving the decoders reassemble it identically.
func runStreamDifferentialCase(t *testing.T, f streamDiffFixture, withRaw, fragment bool) {
	// Oracle leg.
	oSrv := newScriptedSSEServer(t, f.deltas)
	oSrv.fragment = fragment
	oracle := runDynclientLeg(t, false, nil, oSrv.base(), f, withRaw)
	if oSrv.connCount() != 1 || oSrv.reqCount() != 1 {
		t.Errorf("oracle leg: conns=%d reqs=%d, want 1/1", oSrv.connCount(), oSrv.reqCount())
	}
	if metaCount(oracle) == 0 {
		t.Errorf("oracle leg: expected >=1 metadata event")
	}

	// Orchestrator-parity leg (real route, native parser, spy).
	hSrv := newScriptedSSEServer(t, f.deltas)
	hSrv.fragment = fragment
	spy := &streamDiffSpy{}
	hybrid := runDynclientLeg(t, true, spy, hSrv.base(), f, withRaw)
	if hSrv.connCount() != 1 || hSrv.reqCount() != 1 {
		t.Errorf("orchestrator-parity leg: conns=%d reqs=%d, want 1/1", hSrv.connCount(), hSrv.reqCount())
	}
	if fb := spy.streamFallbacks.Load(); fb != 0 {
		t.Errorf("native parser FELL BACK to BAML on %d stream prefix(es) — no admitted prefix may invoke BAML (I6)", fb)
	}
	if fb := spy.finalFallbacks.Load(); fb != 0 {
		t.Errorf("native parser FELL BACK to BAML on %d final(s) — no admitted final may invoke BAML (I6)", fb)
	}
	if e := spy.streamErrors.Load() + spy.finalErrors.Load(); e != 0 {
		t.Errorf("native parser returned %d CLAIMED (non-sentinel) parse error(s)", e)
	}
	if spy.streamClaims.Load() == 0 || spy.finalClaims.Load() == 0 {
		t.Errorf("native-first seam not exercised: streamClaims=%d finalClaims=%d", spy.streamClaims.Load(), spy.finalClaims.Load())
	}
	// Full byte-exact parity INCLUDING metadata count+order.
	assertWireParity(t, "orchestrator-parity", oracle, hybrid)

	// Native-transport leg (real one-shot socket + BOTH native-only closures).
	nSrv := newScriptedSSEServer(t, f.deltas)
	nSrv.fragment = fragment
	cspy := &candidateSpy{}
	native := runNativeTransportLeg(t, nSrv, f, withRaw, cspy)
	// bamlSend is a REAL observation: the candidate makes exactly ONE native exact
	// send, so any provider request the server saw BEYOND that one is an extra (BAML)
	// send. Derive it from the server's actual request count.
	cspy.bamlSend.Store(int64(nSrv.reqCount() - 1))
	if nSrv.connCount() != 1 || nSrv.reqCount() != 1 {
		t.Errorf("native-transport leg: conns=%d reqs=%d, want exactly 1/1", nSrv.connCount(), nSrv.reqCount())
	}
	// §6.2 candidate zero-BAML evidence on the candidate leg itself.
	if cspy.nativePartialEmits.Load() == 0 || cspy.nativeFinalCalls.Load() != 1 {
		t.Errorf("candidate native closures not exercised: partialEmits=%d finalCalls=%d", cspy.nativePartialEmits.Load(), cspy.nativeFinalCalls.Load())
	}
	if inv := cspy.nativeFinalInvariant.Load(); inv != 0 {
		t.Errorf("candidate native FINAL returned the terminal invariant %d time(s) — a would-be BAML fallback on an admitted final", inv)
	}
	if bp, bs := cspy.bamlParse.Load(), cspy.bamlSend.Load(); bp != 0 || bs != 0 {
		t.Errorf("candidate leg invoked BAML: parse=%d (native-final declines) send=%d (extra provider requests), want 0/0", bp, bs)
	}
	// Its partial+final DATA sequence equals the oracle's non-metadata events
	// byte-for-byte (RunStream does not emit orchestrator metadata; metadata parity
	// is proven by the orchestrator-parity leg above).
	assertWireParity(t, "native-transport", nonMeta(oracle), native)
	// Outbound request byte-parity vs BAML for the native exact plan (method, request
	// target, and body bytes; the per-server host:port and BAML's transport-only
	// baml-original-url are excluded — the exact native plan's host parity vs a real
	// server is proven in 7B).
	assertRequestBodyParity(t, oSrv.rec(0), nSrv.rec(0))
}

// assertRequestBodyParity compares the BAML oracle's outbound request against the
// native exact plan's: identical method, request target, and body bytes (the
// request-plan gate). Host is excluded (each leg has its own loopback server, so the
// port differs); native's body equals BAML's StreamRequest body per the 7B oracle.
func assertRequestBodyParity(t *testing.T, oracle, native recordedLive) {
	t.Helper()
	if oracle.Method != native.Method {
		t.Errorf("request method: oracle=%q native=%q", oracle.Method, native.Method)
	}
	if oracle.RequestURI != native.RequestURI {
		t.Errorf("request target: oracle=%q native=%q", oracle.RequestURI, native.RequestURI)
	}
	if string(oracle.Body) != string(native.Body) {
		t.Errorf("request body:\n oracle=%s\n native=%s", oracle.Body, native.Body)
	}
}

// TestStreamDifferential_Fragmentation proves arbitrary TCP fragmentation (each SSE
// event delivered one byte at a time) on the IDENTICAL schedule for BOTH legs does
// not change the outcome: every leg's decoder reassembles the bytes and the native
// candidate reproduces the BAML oracle's StreamResult sequence byte-for-byte, one
// socket per leg — across several admitted fixtures and both modes.
func TestStreamDifferential_Fragmentation(t *testing.T) {
	all := streamDiffFixtures()
	pick := map[string]bool{"person": true, "name_tags": true, "nested": true, "answer_reasoning": true}
	for _, f := range all {
		if !pick[f.name] {
			continue
		}
		f := f
		t.Run(f.name, func(t *testing.T) {
			for _, withRaw := range []bool{false, true} {
				withRaw := withRaw
				mode := "stream"
				if withRaw {
					mode = "stream_with_raw"
				}
				t.Run(mode, func(t *testing.T) { runStreamDifferentialCase(t, f, withRaw, true /*fragment*/) })
			}
		})
	}
}

// runOracleFaultLeg runs the BAML oracle (dynclient.DynamicStream) against a fault
// server and reports whether the public stream terminated WITHOUT a successful final.
// It establishes the shared contractual public outcome the native leg is compared to.
func runOracleFaultLeg(t *testing.T, srv *scriptedSSEServer, f streamDiffFixture) (terminated bool, sawFinal bool) {
	t.Helper()
	evs := runDynclientLeg(t, false, nil, srv.base(), f, false)
	for _, e := range evs {
		switch e.kind {
		case dynclient.EventFinal:
			sawFinal = true
		case "error":
			terminated = true
		}
	}
	// A stream that ended without a final event is a terminated/degraded outcome too.
	if !sawFinal {
		terminated = true
	}
	return terminated, sawFinal
}

// runNativeFaultLeg drives the COMPLETE native-candidate public path against a fault
// server — AdmitStreamClaim + execute.RunStream accumulating the parseable text, THEN
// the native-only FINAL closure on the accumulated (possibly truncated/unclosed) text
// — and reports the same public outcome shape as runOracleFaultLeg: whether the stream
// terminated WITHOUT a successful final, plus the accepted-socket count. This closes the
// gap where a truncated stream (RunStream treats io.EOF as completion) would otherwise
// look "successful" without the final closure being run.
func runNativeFaultLeg(t *testing.T, srv *scriptedSSEServer, f streamDiffFixture) (terminated bool, sawFinal bool, conns int) {
	t.Helper()
	client := newLiveNativeClient(t, srv.base())
	defer client.Close()
	adm := admission.NewAdmitter(nil, liveExactExecutor())
	claim, err := adm.AdmitStreamClaim(context.Background(), nativeAdmissionInput(srv.base(), f, false))
	if err != nil {
		t.Fatalf("AdmitStreamClaim: %v", err)
	}
	defer claim.Close()
	var accParse strings.Builder
	_, rerr := execute.RunStream(context.Background(), execute.StreamConfig{
		Client: claim.Client(), Request: claim.Request(), Expected: claim.ExactRequest,
		FirstBodyTimeout: 15 * time.Second, IdleTimeout: 15 * time.Second,
		EmitDelta: func(d execute.StreamDelta) error { accParse.WriteString(d.ParseableDelta); return nil },
	})
	conns = srv.connCount()
	if rerr != nil {
		// Transport terminated (e.g. non-2xx before any body): no successful final.
		return true, false, conns
	}
	// Transport returned (io.EOF counts as completion). The native FINAL closure on the
	// accumulated — truncated, so typically an UNCLOSED object — decides the terminal
	// outcome: a decline is a terminated/no-final public result, a success is a final.
	if _, ferr := debaml.ParseNativeStreamFinal(context.Background(), f.schema, accParse.String()); ferr != nil {
		return true, false, conns
	}
	return false, true, conns
}

// TestStreamDifferential_Faults exercises the §6.3 fault matrix with a TWO-LEG
// contractual-outcome comparison: the BAML oracle and the native candidate BOTH
// terminate without a successful final, each makes exactly one provider request, and
// the native leg never falls back / retries after claim.
func TestStreamDifferential_Faults(t *testing.T) {
	f := streamDiffFixtures()[0] // person
	t.Run("non_2xx", func(t *testing.T) {
		// Oracle leg: BAML against a 503 must terminate without a final.
		oSrv := newScriptedSSEServer(t, f.deltas)
		oSrv.status, oSrv.body = http.StatusServiceUnavailable, []byte(`{"error":"unavailable"}`)
		oterm, ofinal := runOracleFaultLeg(t, oSrv, f)
		if !oterm || ofinal {
			t.Errorf("oracle non-2xx: terminated=%v sawFinal=%v, want terminated with no final", oterm, ofinal)
		}
		if oSrv.connCount() != 1 {
			t.Errorf("oracle non-2xx: conns=%d, want exactly 1", oSrv.connCount())
		}
		// Native candidate: its COMPLETE public outcome must EQUAL the oracle's
		// (terminated, no successful final), one socket, no retry.
		nSrv := newScriptedSSEServer(t, f.deltas)
		nSrv.status, nSrv.body = http.StatusServiceUnavailable, []byte(`{"error":"unavailable"}`)
		nterm, nfinal, conns := runNativeFaultLeg(t, nSrv, f)
		if nterm != oterm || nfinal != ofinal {
			t.Errorf("non-2xx candidate outcome (terminated=%v sawFinal=%v) != oracle (terminated=%v sawFinal=%v)", nterm, nfinal, oterm, ofinal)
		}
		if conns != 1 {
			t.Errorf("native non-2xx: conns=%d, want exactly 1 (no retry/fallback)", conns)
		}
	})
	t.Run("truncation_eof", func(t *testing.T) {
		// Oracle leg: BAML against a truncated stream terminates without a clean final.
		oSrv := newScriptedSSEServer(t, f.deltas)
		oSrv.truncate = true
		oterm, ofinal := runOracleFaultLeg(t, oSrv, f)
		t.Logf("oracle truncation: terminated=%v sawFinal=%v", oterm, ofinal)
		if oSrv.connCount() != 1 {
			t.Errorf("oracle truncation: conns=%d, want exactly 1", oSrv.connCount())
		}
		// Native candidate: drive the COMPLETE public path (RunStream + native FINAL
		// closure on the truncated/unclosed accumulation) and assert its terminal/no-
		// final outcome EQUALS the oracle's — RunStream alone would look successful on
		// io.EOF, so the final closure is what establishes the shared contractual result.
		nSrv := newScriptedSSEServer(t, f.deltas)
		nSrv.truncate = true
		nterm, nfinal, conns := runNativeFaultLeg(t, nSrv, f)
		t.Logf("native truncation: terminated=%v sawFinal=%v", nterm, nfinal)
		if nterm != oterm || nfinal != ofinal {
			t.Errorf("truncation candidate outcome (terminated=%v sawFinal=%v) != oracle (terminated=%v sawFinal=%v)", nterm, nfinal, oterm, ofinal)
		}
		if conns != 1 {
			t.Errorf("truncation: conns=%d, want exactly 1 (no retry/fallback)", conns)
		}
	})
}

// TestStreamDifferential_UnclosedFinal is the round-4 P1 case: a NORMAL completed
// stream (finish_reason:stop + [DONE], no truncation) whose accumulated model text is
// a COMPLETE-BUT-UNCLOSED object — all field VALUES present, only the closing brace
// missing. BAML's stream final runs the non-stream Parse that closes it at EOF; the
// native-only FINAL closure (ParseNativeStreamFinal -> completeUnclosedFinal) must
// reproduce that byte-exact with NO BAML fallback. Two-leg (BAML oracle vs native
// candidate), both public modes. This exercises ONLY the FINAL: the PARTIAL cadence is
// unchanged (an incomplete trailing value still nulls/defers in the partials, exactly
// as for the closed fixtures).
func TestStreamDifferential_UnclosedFinal(t *testing.T) {
	person := dsch(bamlutils.OrderedKV("name", dstr()), bamlutils.OrderedKV("age", dint()))
	if err := debaml.SupportsNativeStream(person); err != nil {
		t.Fatalf("person schema not admitted: %v", err)
	}
	// deltas WITHOUT a closing '}' — the object is complete-but-unclosed at [DONE].
	f := streamDiffFixture{
		name: "person_unclosed", schema: person, messages: userMsg("Give a person."),
		deltas: contentDeltas(`{`, `"name": "`, `Ada`, `", "age": `, `36`),
	}
	for _, withRaw := range []bool{false, true} {
		withRaw := withRaw
		mode := "stream"
		if withRaw {
			mode = "stream_with_raw"
		}
		t.Run(mode, func(t *testing.T) {
			// Oracle: BAML DynamicStream. Its final is the non-stream Parse of the
			// unclosed accumulation.
			oSrv := newScriptedSSEServer(t, f.deltas)
			oracle := runDynclientLeg(t, false, nil, oSrv.base(), f, withRaw)
			if oSrv.connCount() != 1 || oSrv.reqCount() != 1 {
				t.Errorf("oracle leg: conns=%d reqs=%d, want 1/1", oSrv.connCount(), oSrv.reqCount())
			}
			var of *wireEvent
			for i := range oracle {
				if oracle[i].kind == dynclient.EventFinal {
					of = &oracle[i]
				}
			}
			if of == nil {
				t.Fatalf("oracle produced no final event for the unclosed [DONE] stream")
			}

			// Candidate: AdmitStreamClaim + RunStream + native closures; the FINAL closure
			// completes the unclosed object NATIVELY (no fallback).
			nSrv := newScriptedSSEServer(t, f.deltas)
			cspy := &candidateSpy{}
			native := runNativeTransportLeg(t, nSrv, f, withRaw, cspy)
			cspy.bamlSend.Store(int64(nSrv.reqCount() - 1))
			if nSrv.connCount() != 1 || nSrv.reqCount() != 1 {
				t.Errorf("candidate leg: conns=%d reqs=%d, want 1/1", nSrv.connCount(), nSrv.reqCount())
			}
			if cspy.nativeFinalCalls.Load() != 1 || cspy.nativeFinalInvariant.Load() != 0 {
				t.Errorf("candidate final: calls=%d invariant=%d, want 1/0 (native-owned EOF completion, no would-be fallback)", cspy.nativeFinalCalls.Load(), cspy.nativeFinalInvariant.Load())
			}
			if bp, bs := cspy.bamlParse.Load(), cspy.bamlSend.Load(); bp != 0 || bs != 0 {
				t.Errorf("candidate invoked BAML: parse=%d send=%d, want 0/0", bp, bs)
			}

			// The candidate's partial+final DATA sequence equals the oracle's non-metadata
			// events byte-for-byte — INCLUDING the EOF-completed final.
			assertWireParity(t, "unclosed-final", nonMeta(oracle), native)
			nf := native[len(native)-1]
			if nf.kind != dynclient.EventFinal {
				t.Fatalf("candidate: last event is not a final: %+v", nf)
			}
			if nf.data != of.data {
				t.Errorf("candidate final data = %s, want (oracle) %s", nf.data, of.data)
			}
			t.Logf("unclosed [DONE] final: oracle=%s candidate=%s", of.data, nf.data)
		})
	}
}

// TestStreamDifferential_TrailingCommaPartial is the round-6 P1 #3 case: an admitted
// Root{name:string, age:int} stream whose LAST content frame is a CLOSED object with a
// trailing comma before the brace ({"name":"Ada","age":36,}). That frame is parsed as
// a PARTIAL (before finish_reason/[DONE]); BAML emits {name,age:36} (greedy-read "36,"
// then int-trim), and the native-only PARTIAL closure must now emit the SAME
// (parseUnquotedScalarStream's '}' trailing-comma exception), not a silent no-emit.
// Two-leg (BAML oracle vs native candidate), both public modes; strict wire parity.
func TestStreamDifferential_TrailingCommaPartial(t *testing.T) {
	person := dsch(bamlutils.OrderedKV("name", dstr()), bamlutils.OrderedKV("age", dint()))
	if err := debaml.SupportsNativeStream(person); err != nil {
		t.Fatalf("person schema not admitted: %v", err)
	}
	// Last content frame accumulates to `{"name":"Ada","age":36,}` (closed, trailing
	// comma) BEFORE finish_reason:stop + [DONE].
	f := streamDiffFixture{
		name: "person_trailing_comma", schema: person, messages: userMsg("Give a person."),
		deltas: contentDeltas(`{`, `"name": "`, `Ada`, `", "age": `, `36`, `,}`),
	}
	for _, withRaw := range []bool{false, true} {
		withRaw := withRaw
		mode := "stream"
		if withRaw {
			mode = "stream_with_raw"
		}
		t.Run(mode, func(t *testing.T) {
			oSrv := newScriptedSSEServer(t, f.deltas)
			oracle := runDynclientLeg(t, false, nil, oSrv.base(), f, withRaw)
			if oSrv.connCount() != 1 || oSrv.reqCount() != 1 {
				t.Errorf("oracle leg: conns=%d reqs=%d, want 1/1", oSrv.connCount(), oSrv.reqCount())
			}
			nSrv := newScriptedSSEServer(t, f.deltas)
			cspy := &candidateSpy{}
			native := runNativeTransportLeg(t, nSrv, f, withRaw, cspy)
			cspy.bamlSend.Store(int64(nSrv.reqCount() - 1))
			if nSrv.connCount() != 1 || nSrv.reqCount() != 1 {
				t.Errorf("candidate leg: conns=%d reqs=%d, want 1/1", nSrv.connCount(), nSrv.reqCount())
			}
			if cspy.nativeFinalInvariant.Load() != 0 || cspy.bamlParse.Load() != 0 || cspy.bamlSend.Load() != 0 {
				t.Errorf("candidate invoked BAML: invariant=%d parse=%d send=%d, want 0/0/0", cspy.nativeFinalInvariant.Load(), cspy.bamlParse.Load(), cspy.bamlSend.Load())
			}
			// The candidate's partial+final DATA sequence equals the oracle's non-metadata
			// events byte-for-byte — INCLUDING the trailing-comma partial frame.
			assertWireParity(t, "trailing-comma-partial", nonMeta(oracle), native)
			// The trailing-comma boundary must have emitted a partial (not a no-emit):
			// at least one partial carries the completed {name,age:36} value.
			if cspy.nativePartialEmits.Load() == 0 {
				t.Errorf("candidate emitted no partials — the trailing-comma partial was lost")
			}
		})
	}
}
