//go:build nanollm_integration

package spine_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/internal/nativespinejsonfixture"
	"github.com/invakid404/baml-rest/workerplugin"
)

// drain collects every StreamResult until the channel closes, bounded by a deadline so a
// producer that never closes the channel fails HERE with a clear message instead of
// hanging until the package test timeout (CodeRabbit #8).
func drain(t *testing.T, ch <-chan *workerplugin.StreamResult) []*workerplugin.StreamResult {
	t.Helper()
	var out []*workerplugin.StreamResult
	deadline := time.After(10 * time.Second)
	for {
		select {
		case r, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, r)
		case <-deadline:
			t.Fatalf("stream channel did not close within 10s after %d result(s) — the producer never closed it", len(out))
			return out
		}
	}
}

// TestRealPopulation_CallAndParse drives the FULL real-population path end to end
// through worker.Handler with a real loopback OpenAI provider (NOT a fake executor):
//
//	provider response -> nanollm translate/extract -> native exact-JSON final parse ->
//	canonical JSON -> emitted DecodeStaticAliasFinal[OutputJson] -> generated final
//	carrier -> worker.Handler call envelope
//
// and the socket-free /parse route, asserting the concrete emitted union carrier and
// its worker JSON envelope, with no generated BAML parser anywhere on the path.
func TestRealPopulation_CallAndParse(t *testing.T) {
	// The model returns a JSON object; the five-arm alias parses it into the map arm
	// (variant4), whose value is the int arm (variant0).
	const modelText = `{"k":1}`

	lb := newLoopback(t, func(w http.ResponseWriter, r *http.Request) {
		okChatCompletion(w, modelText)
	})

	exec := newJSONStreamExec(t, lb.baseURL(), nil)
	h := newHandler(t, exec)
	ctx := context.Background()

	// --- Call route (one real provider request) --------------------------------
	ch, err := h.CallStream(ctx, jsonAliasMethod, callInput("weather"), bamlutils.StreamModeCall)
	if err != nil {
		t.Fatalf("CallStream: %v", err)
	}
	results := drain(t, ch)
	if len(results) != 1 {
		t.Fatalf("want 1 stream result, got %d", len(results))
	}
	if results[0].Error != nil {
		t.Fatalf("unexpected error frame: %v", results[0].Error)
	}
	assertJSONEnvelopeCarrier(t, results[0].Data, modelText)

	if got := lb.count(); got != 1 {
		t.Fatalf("provider request count = %d, want exactly 1 (one send, no retry/fallback)", got)
	}
	if snap := exec.Metrics().Snapshot(); snap.Claims != 1 || snap.Sockets != 1 || snap.Successes != 1 || snap.Failures != 0 || snap.Declines != 0 {
		t.Fatalf("metrics = %+v, want exactly one claim/socket/success", snap)
	}

	// --- Parse route (zero sockets) --------------------------------------------
	hitsBefore := lb.count()
	pres, err := h.Parse(ctx, jsonAliasMethod, parseInput(modelText))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	assertJSONEnvelopeCarrier(t, pres.Data, modelText)
	if lb.count() != hitsBefore {
		t.Fatalf("parse route opened a socket: hits went %d -> %d", hitsBefore, lb.count())
	}
}

// assertJSONEnvelopeCarrier proves the worker envelope bytes decode to the concrete
// emitted OutputJson carrier (the map arm holding an int) and re-marshal to the
// canonical model text — a concrete-carrier assertion a fake executor cannot satisfy.
func assertJSONEnvelopeCarrier(t *testing.T, data []byte, wantCanonical string) {
	t.Helper()
	// The envelope re-marshals canonically to the model text.
	if string(data) != wantCanonical {
		t.Fatalf("worker envelope = %s, want canonical %s", data, wantCanonical)
	}
	// It decodes into the concrete emitted union carrier: the map arm (variant4).
	var carrier nativespinejsonfixture.OutputJson
	if err := json.Unmarshal(data, &carrier); err != nil {
		t.Fatalf("envelope does not decode into OutputJson carrier: %v (%s)", err, data)
	}
	if !carrier.IsVariant4() {
		t.Fatalf("carrier is not the map arm (variant4): %s", data)
	}
	m := carrier.AsVariant4()
	if m == nil {
		t.Fatalf("map arm accessor returned nil")
	}
	inner, ok := (*m)["k"]
	if !ok || !inner.IsVariant0() {
		t.Fatalf("map value is not the int arm (variant0): %s", data)
	}
	if got := inner.AsVariant0(); got == nil || *got != 1 {
		t.Fatalf("int arm value = %v, want 1", got)
	}
}

// TestRealPopulation_StreamThroughWorkerHandler drives the FULL native-only stream path
// through worker.Handler — the same dispatch the booted native-only worker uses:
//
//	loopback SSE -> exact one-shot stream client -> shared cadence -> native
//	ParseStaticStreamPartial -> emitted DecodeStaticAliasStream[OutputJsonStream] ->
//	generated streamResult -> worker stream bridge -> workerplugin frames
//
// and asserts the whole public frame transcript (kind, order, bytes, raw) against the
// SHARED expectation fixture, with exactly one provider request. No generated BAML
// parser is anywhere on this path.
func TestRealPopulation_StreamThroughWorkerHandler(t *testing.T) {
	tr := loadTranscript(t)
	lb := newLoopback(t, func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, transcriptBody(tr), 0)
	})
	h := newHandler(t, newJSONStreamExec(t, lb.baseURL(), nil))

	for _, tc := range []struct {
		name     string
		mode     bamlutils.StreamMode
		needsRaw bool
	}{
		{"stream", bamlutils.StreamModeStream, false},
		{"stream_with_raw", bamlutils.StreamModeStreamWithRaw, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := lb.count()
			ch, err := h.CallStream(context.Background(), jsonAliasMethod, callInput("weather"), tc.mode)
			if err != nil {
				t.Fatalf("CallStream: %v", err)
			}
			frames := drain(t, ch)
			if len(frames) == 0 {
				t.Fatal("no frames")
			}
			if got := lb.count() - before; got != 1 {
				t.Fatalf("provider request count = %d, want exactly 1", got)
			}

			// The LAST frame is the single final; no error frame anywhere.
			last := frames[len(frames)-1]
			if last.Kind != workerplugin.StreamResultKindFinal {
				t.Fatalf("last frame kind = %v, want final", last.Kind)
			}
			if string(last.Data) != tr.Final {
				t.Fatalf("final envelope = %s, want %s", last.Data, tr.Final)
			}
			var structured []string
			var rawOnly int
			for i, f := range frames[:len(frames)-1] {
				if f.Error != nil {
					t.Fatalf("frame %d is an error frame: %v", i, f.Error)
				}
				if f.Kind != workerplugin.StreamResultKindStream {
					t.Fatalf("frame %d kind = %v, want stream", i, f.Kind)
				}
				if string(f.Data) == "null" {
					rawOnly++
					continue
				}
				structured = append(structured, string(f.Data))
			}
			var want []string
			for _, d := range tr.Deltas {
				if d.Emit {
					want = append(want, d.Partial)
				}
			}
			if len(structured) != len(want) {
				t.Fatalf("got %d structured partial frame(s), want %d", len(structured), len(want))
			}
			for i := range want {
				if structured[i] != want[i] {
					t.Fatalf("structured partial frame %d = %s, want %s", i, structured[i], want[i])
				}
			}
			if tc.needsRaw {
				if rawOnly == 0 {
					t.Fatal("/stream-with-raw produced no raw-only frame")
				}
				if last.Raw != tr.Accumulated {
					t.Fatalf("final frame raw = %q, want %q", last.Raw, tr.Accumulated)
				}
			} else {
				if rawOnly != 0 {
					t.Fatalf("/stream produced %d raw-only frame(s), want 0", rawOnly)
				}
				if last.Raw != "" {
					t.Fatalf("final frame raw = %q on /stream, want empty", last.Raw)
				}
			}
		})
	}
}

// TestRealPopulation_StreamParseIsSocketFree proves the worker's parse-STREAM route
// (`/parse` with stream=true) dispatches the NATIVE ParseMethod.StreamImpl and returns
// the POINTER carrier with zero provider sockets.
func TestRealPopulation_StreamParseIsSocketFree(t *testing.T) {
	lb := newLoopback(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("the stream-parse route contacted the provider")
	})
	h := newHandler(t, newJSONStreamExec(t, lb.baseURL(), nil))

	res, err := h.Parse(context.Background(), jsonAliasMethod, streamParseInput(`{"k":1`))
	if err != nil {
		t.Fatalf("Parse(stream=true): %v", err)
	}
	// The partial of an unterminated object is the object built so far.
	if string(res.Data) != `{}` && string(res.Data) != `{"k":1}` {
		t.Fatalf("stream-parse envelope = %s, want the native partial for the prefix", res.Data)
	}
	if lb.count() != 0 {
		t.Fatalf("stream parse opened %d socket(s), want 0", lb.count())
	}
}

// streamParseInput is the JSON envelope for a stream `/parse` of raw model text.
func streamParseInput(raw string) []byte {
	b, _ := json.Marshal(map[string]any{"raw": raw, "stream": true})
	return b
}
