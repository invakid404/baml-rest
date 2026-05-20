package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/goccy/go-json"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/workerplugin"
)

// fakeUnaryCaller satisfies unaryCaller and lets tests stub Pool.Call's
// return values without spinning up a real worker pool. Also records
// the most recent (methodName, inputJSON, streamMode) tuple so tests
// can assert what the handler routed to the worker layer — in
// particular, whether the worker-bound payload preserves order.
type fakeUnaryCaller struct {
	result *workerplugin.CallResult
	err    error

	gotMethod     string
	gotInputJSON  []byte
	gotStreamMode bamlutils.StreamMode
}

func (f *fakeUnaryCaller) Call(_ context.Context, methodName string, inputJSON []byte, streamMode bamlutils.StreamMode) (*workerplugin.CallResult, error) {
	f.gotMethod = methodName
	// Copy the slice — the caller may reuse the buffer after returning.
	f.gotInputJSON = append([]byte(nil), inputJSON...)
	f.gotStreamMode = streamMode
	return f.result, f.err
}

// fakeUnaryParser satisfies unaryParser for parse-handler tests.
// Mirrors fakeUnaryCaller's record-then-replay shape.
type fakeUnaryParser struct {
	result *workerplugin.ParseResult
	err    error

	gotMethod    string
	gotInputJSON []byte
}

func (f *fakeUnaryParser) Parse(_ context.Context, methodName string, inputJSON []byte) (*workerplugin.ParseResult, error) {
	f.gotMethod = methodName
	f.gotInputJSON = append([]byte(nil), inputJSON...)
	return f.result, f.err
}

// countingHeadersEmitter wraps the production emitter and records
// how many times it was called. The wrapped emitter still runs (so
// real headers land on the ResponseWriter for the value-equality
// assertions) — only the count is the test-only surface.
type countingHeadersEmitter struct {
	calls    int
	delegate chiHeadersEmitter
}

func (c *countingHeadersEmitter) emit(w http.ResponseWriter, planned, outcome []byte) {
	c.calls++
	c.delegate(w, planned, outcome)
}

// minimalDynamicInputJSON returns a JSON-encoded DynamicInput that
// passes Validate() — required so the chi dynamic handler reaches the
// underlying p.Call and exercises the result/err handling we care
// about.
func minimalDynamicInputJSON(t *testing.T) []byte {
	t.Helper()
	primary := "TestClient"
	textContent := "hello"
	input := bamlutils.DynamicInput{
		Messages: []bamlutils.DynamicMessage{
			{Role: "user", TextContent: &textContent},
		},
		ClientRegistry: &bamlutils.ClientRegistry{
			Primary: &primary,
			Clients: []*bamlutils.ClientProperty{
				{Name: "TestClient", Provider: "openai", Options: map[string]any{
					"model":    "gpt",
					"base_url": "http://x",
					"api_key":  "k",
				}},
			},
		},
		OutputSchema: &bamlutils.DynamicOutputSchema{
			Properties: map[string]*bamlutils.DynamicProperty{
				"answer": {Type: "string"},
			},
		},
	}
	body, err := json.Marshal(input)
	if err != nil {
		t.Fatalf("marshal DynamicInput: %v", err)
	}
	return body
}

// TestMakeChiDynamicCallHandler_EmitsHeadersOnError pins that
// makeChiDynamicCallHandler does NOT discard accumulated
// planned/outcome metadata when Pool.Call returns an error; X-BAML-
// Path / X-BAML-Path-Reason must be present on 500 responses so
// runtime-options diagnoses are visible. The handler sets headers
// from result.Planned/Outcome before checking err.
func TestMakeChiDynamicCallHandler_EmitsHeadersOnError(t *testing.T) {
	planned, err := json.Marshal(&bamlutils.Metadata{
		Phase:      bamlutils.MetadataPhasePlanned,
		Path:       "legacy",
		PathReason: "invalid-round-robin-start-override",
		Client:     "TestClient",
	})
	if err != nil {
		t.Fatalf("marshal planned: %v", err)
	}
	outcome, err := json.Marshal(&bamlutils.Metadata{
		Phase:          bamlutils.MetadataPhaseOutcome,
		WinnerProvider: "openai",
	})
	if err != nil {
		t.Fatalf("marshal outcome: %v", err)
	}

	caller := &fakeUnaryCaller{
		result: &workerplugin.CallResult{
			Planned: planned,
			Outcome: outcome,
		},
		err: errors.New("simulated worker failure"),
	}

	emitter := &countingHeadersEmitter{delegate: defaultChiHeadersEmitter}
	handler := makeChiDynamicCallHandlerWithEmitter(caller, bamlutils.StreamModeCall, false, emitter.emit)
	req := httptest.NewRequest(http.MethodPost, "/call/dynamic", bytes.NewReader(minimalDynamicInputJSON(t)))
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code < 400 {
		t.Errorf("status code: got %d, want >= 400 (handler must surface the worker error)", rec.Code)
	}
	if emitter.calls != 1 {
		t.Errorf("headers emitter calls: got %d, want 1 (must fire exactly once even on error tail)", emitter.calls)
	}
	if got := rec.Header().Get(HeaderBAMLPath); got != "legacy" {
		t.Errorf("X-BAML-Path: got %q, want legacy", got)
	}
	if got := rec.Header().Get(HeaderBAMLPathReason); got != "invalid-round-robin-start-override" {
		t.Errorf("X-BAML-Path-Reason: got %q, want invalid-round-robin-start-override", got)
	}
	if got := rec.Header().Get(HeaderBAMLClient); got != "TestClient" {
		t.Errorf("X-BAML-Client: got %q, want TestClient", got)
	}
	if got := rec.Header().Get(HeaderBAMLWinnerProvider); got != "openai" {
		t.Errorf("X-BAML-Winner-Provider: got %q, want openai (outcome must also propagate on error tail)", got)
	}
}

// TestMakeChiDynamicCallHandler_EmitsHeadersOnSuccess is the inverse
// guard. The success path must still set BAML headers exactly once
// (Pool.Call returns a CallResult with Data + metadata; handler runs
// FlattenDynamicOutput and writes 200). A future refactor that
// removed the result-nil guard could otherwise double-set or drop.
func TestMakeChiDynamicCallHandler_EmitsHeadersOnSuccess(t *testing.T) {
	planned, err := json.Marshal(&bamlutils.Metadata{
		Phase:  bamlutils.MetadataPhasePlanned,
		Path:   "buildrequest",
		Client: "TestClient",
	})
	if err != nil {
		t.Fatalf("marshal planned: %v", err)
	}

	caller := &fakeUnaryCaller{
		result: &workerplugin.CallResult{
			Data:    []byte(`{"answer":"hi"}`),
			Planned: planned,
		},
	}

	emitter := &countingHeadersEmitter{delegate: defaultChiHeadersEmitter}
	handler := makeChiDynamicCallHandlerWithEmitter(caller, bamlutils.StreamModeCall, false, emitter.emit)
	req := httptest.NewRequest(http.MethodPost, "/call/dynamic", bytes.NewReader(minimalDynamicInputJSON(t)))
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status code: got %d, want 200", rec.Code)
	}
	// Headers must fire exactly once on success. The previous test
	// used Header().Values() to count, but http.Header.Set silently
	// overwrites — so two Set calls with the same value produced the
	// same Values() output as one, making the assertion vacuous.
	// Counting via the injected emitter is the actual exactly-once
	// guard.
	if emitter.calls != 1 {
		t.Errorf("headers emitter calls: got %d, want 1", emitter.calls)
	}
	if got := rec.Header().Get(HeaderBAMLPath); got != "buildrequest" {
		t.Errorf("X-BAML-Path: got %q, want buildrequest", got)
	}
}

// TestMakeChiDynamicCallHandler_NoMetadataNoHeaders pins that when
// Pool.Call returns nil result + err (e.g., setup-time failure that
// never accumulated any metadata), the handler does not attempt to
// dereference result and emits no BAML headers.
func TestMakeChiDynamicCallHandler_NoMetadataNoHeaders(t *testing.T) {
	caller := &fakeUnaryCaller{
		result: nil,
		err:    errors.New("setup failure"),
	}

	emitter := &countingHeadersEmitter{delegate: defaultChiHeadersEmitter}
	handler := makeChiDynamicCallHandlerWithEmitter(caller, bamlutils.StreamModeCall, false, emitter.emit)
	req := httptest.NewRequest(http.MethodPost, "/call/dynamic", bytes.NewReader(minimalDynamicInputJSON(t)))
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code < 400 {
		t.Errorf("status code: got %d, want >= 400", rec.Code)
	}
	if emitter.calls != 0 {
		t.Errorf("headers emitter calls: got %d, want 0 (must not fire when result is nil)", emitter.calls)
	}
	if got := rec.Header().Get(HeaderBAMLPath); got != "" {
		t.Errorf("X-BAML-Path: got %q, want empty when result is nil", got)
	}
	if got := rec.Header().Get(HeaderBAMLPathReason); got != "" {
		t.Errorf("X-BAML-Path-Reason: got %q, want empty when result is nil", got)
	}
}

// orderedDynamicInputJSON returns a /call/_dynamic body with at least
// two top-level properties so preserve-order behavior is observable in
// the worker payload (single-property schemas never need an order
// slice). Includes the requested preserve_schema_order raw fragment
// inline so callers can probe the omitted/null/true/false matrix.
func orderedDynamicInputJSON(preserveField string) []byte {
	body := `{
      ` + preserveField + `
      "messages": [{"role": "user", "content": "go"}],
      "client_registry": {"primary": "X", "clients": [{"name": "X", "provider": "anthropic", "options": {"model": "m", "api_key": "k"}}]},
      "output_schema": {
        "properties": {
          "zulu": {"type": "string"},
          "alpha": {"type": "string"}
        }
      }
    }`
	return []byte(body)
}

// workerPayloadPreserveOrder decodes the worker-bound JSON and reports
// whether __baml_options__.type_builder.dynamic_types.preserve_order
// is the boolean true. Missing keys or non-true values all read as
// false — matches the worker's view of "no preserve-order opt-in".
func workerPayloadPreserveOrder(t *testing.T, data []byte) bool {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal worker payload: %v\n%s", err, data)
	}
	opts, _ := decoded["__baml_options__"].(map[string]any)
	if opts == nil {
		return false
	}
	tb, _ := opts["type_builder"].(map[string]any)
	if tb == nil {
		return false
	}
	dt, _ := tb["dynamic_types"].(map[string]any)
	if dt == nil {
		return false
	}
	got, _ := dt["preserve_order"].(bool)
	return got
}

// TestMakeChiDynamicCallHandler_PreserveSchemaOrderDefault walks the
// preserve_schema_order truth table at the chi handler boundary: server default × per-
// request field => worker preserve_order. The handler must defer to
// the explicit per-request value when present (true or false) and only
// fall back to the server default when the field is absent / null.
func TestMakeChiDynamicCallHandler_PreserveSchemaOrderDefault(t *testing.T) {
	cases := []struct {
		name           string
		serverDefault  bool
		preserveField  string // exact JSON fragment for preserve_schema_order
		wantPreserve   bool
	}{
		{
			name:          "default off, field omitted -> off",
			serverDefault: false,
			preserveField: ``,
			wantPreserve:  false,
		},
		{
			name:          "default off, field true -> on",
			serverDefault: false,
			preserveField: `"preserve_schema_order": true,`,
			wantPreserve:  true,
		},
		{
			name:          "default on, field omitted -> on",
			serverDefault: true,
			preserveField: ``,
			wantPreserve:  true,
		},
		{
			name:          "default on, field false -> off",
			serverDefault: true,
			preserveField: `"preserve_schema_order": false,`,
			wantPreserve:  false,
		},
		{
			name:          "default on, field null -> on (null is inherit)",
			serverDefault: true,
			preserveField: `"preserve_schema_order": null,`,
			wantPreserve:  true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			caller := &fakeUnaryCaller{
				result: &workerplugin.CallResult{Data: []byte(`{"zulu":"x","alpha":"y"}`)},
			}
			emitter := &countingHeadersEmitter{delegate: defaultChiHeadersEmitter}
			handler := makeChiDynamicCallHandlerWithEmitter(caller, bamlutils.StreamModeCall, tc.serverDefault, emitter.emit)
			req := httptest.NewRequest(http.MethodPost, "/call/_dynamic", bytes.NewReader(orderedDynamicInputJSON(tc.preserveField)))
			rec := httptest.NewRecorder()
			handler(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status: got %d body=%s", rec.Code, rec.Body.String())
			}
			if got := workerPayloadPreserveOrder(t, caller.gotInputJSON); got != tc.wantPreserve {
				t.Errorf("preserve_order: got %v want %v\nworker payload:\n%s", got, tc.wantPreserve, caller.gotInputJSON)
			}
		})
	}
}

// TestMakeChiDynamicParseHandler_PreserveSchemaOrderDefault mirrors
// the call-side truth table on the parse handler. The fakeUnaryParser
// records the routed worker input so the test can assert preserve_order
// the same way as the call-side check.
func TestMakeChiDynamicParseHandler_PreserveSchemaOrderDefault(t *testing.T) {
	cases := []struct {
		name          string
		serverDefault bool
		preserveField string
		wantPreserve  bool
	}{
		{
			name:          "default off, field omitted -> off",
			serverDefault: false,
			preserveField: ``,
			wantPreserve:  false,
		},
		{
			name:          "default off, field true -> on",
			serverDefault: false,
			preserveField: `"preserve_schema_order": true,`,
			wantPreserve:  true,
		},
		{
			name:          "default on, field omitted -> on",
			serverDefault: true,
			preserveField: ``,
			wantPreserve:  true,
		},
		{
			name:          "default on, field false -> off",
			serverDefault: true,
			preserveField: `"preserve_schema_order": false,`,
			wantPreserve:  false,
		},
		{
			name:          "default on, field null -> on (null is inherit)",
			serverDefault: true,
			preserveField: `"preserve_schema_order": null,`,
			wantPreserve:  true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parser := &fakeUnaryParser{result: &workerplugin.ParseResult{Data: []byte(`{"zulu":"x","alpha":"y"}`)}}
			body := []byte(`{
              ` + tc.preserveField + `
              "raw": "{\"zulu\":\"x\",\"alpha\":\"y\"}",
              "output_schema": {"properties": {"zulu": {"type": "string"}, "alpha": {"type": "string"}}}
            }`)
			handler := makeChiDynamicParseHandler(parser, tc.serverDefault)
			req := httptest.NewRequest(http.MethodPost, "/parse/_dynamic", bytes.NewReader(body))
			rec := httptest.NewRecorder()
			handler(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status: got %d body=%s", rec.Code, rec.Body.String())
			}
			if got := workerPayloadPreserveOrder(t, parser.gotInputJSON); got != tc.wantPreserve {
				t.Errorf("preserve_order: got %v want %v\nworker payload:\n%s", got, tc.wantPreserve, parser.gotInputJSON)
			}
		})
	}
}

// preserveSchemaOrderTruthTable is the shared 5-cell matrix used by
// every Fiber/chi handler test that exercises the server-default x
// per-request-field interaction. Defined once so the chi call/parse
// and Fiber call/parse/stream coverage stays in lockstep — a new row
// added here is picked up by all consumers.
var preserveSchemaOrderTruthTable = []struct {
	name          string
	serverDefault bool
	preserveField string
	wantPreserve  bool
}{
	{
		name:          "default off, field omitted -> off",
		serverDefault: false,
		preserveField: ``,
		wantPreserve:  false,
	},
	{
		name:          "default off, field true -> on",
		serverDefault: false,
		preserveField: `"preserve_schema_order": true,`,
		wantPreserve:  true,
	},
	{
		name:          "default on, field omitted -> on",
		serverDefault: true,
		preserveField: ``,
		wantPreserve:  true,
	},
	{
		name:          "default on, field false -> off",
		serverDefault: true,
		preserveField: `"preserve_schema_order": false,`,
		wantPreserve:  false,
	},
	{
		name:          "default on, field null -> on (null is inherit)",
		serverDefault: true,
		preserveField: `"preserve_schema_order": null,`,
		wantPreserve:  true,
	},
}

// orderedDynamicParseInputJSON mirrors orderedDynamicInputJSON for the
// /parse/_dynamic body shape. At least two top-level properties so
// preserve-order behavior is observable in the worker payload.
func orderedDynamicParseInputJSON(preserveField string) []byte {
	body := `{
      ` + preserveField + `
      "raw": "{\"zulu\":\"x\",\"alpha\":\"y\"}",
      "output_schema": {
        "properties": {
          "zulu": {"type": "string"},
          "alpha": {"type": "string"}
        }
      }
    }`
	return []byte(body)
}

// TestParseDynamicCallBody_PreserveSchemaOrderDefault covers the
// Fiber-side decode/default/validate/ToWorkerInput helper used by both
// /call/_dynamic and /stream/_dynamic. The Fiber call and stream
// handlers do not own a fakeUnaryCaller-style seam, so the helper is
// the smallest unit that captures the wire-visible behavior end-to-end
// without spinning up a Fiber app and a worker pool.
func TestParseDynamicCallBody_PreserveSchemaOrderDefault(t *testing.T) {
	for _, tc := range preserveSchemaOrderTruthTable {
		t.Run(tc.name, func(t *testing.T) {
			workerInput, statusCode, code, err := parseDynamicCallBody(orderedDynamicInputJSON(tc.preserveField), tc.serverDefault)
			if err != nil {
				t.Fatalf("parseDynamicCallBody: status=%d code=%q err=%v", statusCode, code, err)
			}
			if got := workerPayloadPreserveOrder(t, workerInput); got != tc.wantPreserve {
				t.Errorf("preserve_order: got %v want %v\nworker payload:\n%s", got, tc.wantPreserve, workerInput)
			}
		})
	}
}

// TestParseDynamicParseBody_PreserveSchemaOrderDefault covers the
// Fiber-side decode/default/validate/ToWorkerInput helper used by
// /parse/_dynamic. Mirrors TestParseDynamicCallBody but routes through
// the DynamicParseInput shape (raw + output_schema, no messages or
// client_registry).
func TestParseDynamicParseBody_PreserveSchemaOrderDefault(t *testing.T) {
	for _, tc := range preserveSchemaOrderTruthTable {
		t.Run(tc.name, func(t *testing.T) {
			workerInput, statusCode, code, err := parseDynamicParseBody(orderedDynamicParseInputJSON(tc.preserveField), tc.serverDefault)
			if err != nil {
				t.Fatalf("parseDynamicParseBody: status=%d code=%q err=%v", statusCode, code, err)
			}
			if got := workerPayloadPreserveOrder(t, workerInput); got != tc.wantPreserve {
				t.Errorf("preserve_order: got %v want %v\nworker payload:\n%s", got, tc.wantPreserve, workerInput)
			}
		})
	}
}
