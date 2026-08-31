//go:build nanollm_integration

package spine_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/internal/nativespinejsonfixture"
	"github.com/invakid404/baml-rest/workerplugin"
)

func drain(ch <-chan *workerplugin.StreamResult) []*workerplugin.StreamResult {
	var out []*workerplugin.StreamResult
	for r := range ch {
		out = append(out, r)
	}
	return out
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

	exec := newJSONExec(t, lb.baseURL(), nil)
	h := newHandler(t, exec)
	ctx := context.Background()

	// --- Call route (one real provider request) --------------------------------
	ch, err := h.CallStream(ctx, jsonAliasMethod, callInput("weather"), bamlutils.StreamModeCall)
	if err != nil {
		t.Fatalf("CallStream: %v", err)
	}
	results := drain(ch)
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
