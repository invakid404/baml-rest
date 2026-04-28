//go:build unaryserver

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
// return values without spinning up a real worker pool.
type fakeUnaryCaller struct {
	result *workerplugin.CallResult
	err    error
}

func (f *fakeUnaryCaller) Call(_ context.Context, _ string, _ []byte, _ bamlutils.StreamMode) (*workerplugin.CallResult, error) {
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
	handler := makeChiDynamicCallHandlerWithEmitter(caller, bamlutils.StreamModeCall, emitter.emit)
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
	handler := makeChiDynamicCallHandlerWithEmitter(caller, bamlutils.StreamModeCall, emitter.emit)
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
	handler := makeChiDynamicCallHandlerWithEmitter(caller, bamlutils.StreamModeCall, emitter.emit)
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
