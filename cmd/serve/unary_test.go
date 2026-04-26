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

// TestMakeChiDynamicCallHandler_EmitsHeadersOnError covers PR #192
// verdict-18: makeChiDynamicCallHandler previously discarded
// accumulated planned/outcome metadata when Pool.Call returned an
// error, leaving X-BAML-Path / X-BAML-Path-Reason absent on 500
// responses for runtime-options diagnoses. The handler now sets
// headers from result.Planned/Outcome before checking err.
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

	handler := makeChiDynamicCallHandler(caller, bamlutils.StreamModeCall)
	req := httptest.NewRequest(http.MethodPost, "/call/dynamic", bytes.NewReader(minimalDynamicInputJSON(t)))
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code < 400 {
		t.Errorf("status code: got %d, want >= 400 (handler must surface the worker error)", rec.Code)
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

	handler := makeChiDynamicCallHandler(caller, bamlutils.StreamModeCall)
	req := httptest.NewRequest(http.MethodPost, "/call/dynamic", bytes.NewReader(minimalDynamicInputJSON(t)))
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status code: got %d, want 200", rec.Code)
	}
	if got := rec.Header().Get(HeaderBAMLPath); got != "buildrequest" {
		t.Errorf("X-BAML-Path: got %q, want buildrequest", got)
	}
	// Header is set exactly once even though the success path used to
	// call setBAMLHeaders below the FlattenDynamicOutput call. The
	// verdict-18 patch moved the call upfront and removed the trailing
	// duplicate; pin that here so the duplicate doesn't sneak back via
	// http.Header.Set's silent overwrite (which would mask a regression).
	if vals := rec.Header().Values(HeaderBAMLPath); len(vals) != 1 {
		t.Errorf("X-BAML-Path emitted %d times, want exactly 1: %v", len(vals), vals)
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

	handler := makeChiDynamicCallHandler(caller, bamlutils.StreamModeCall)
	req := httptest.NewRequest(http.MethodPost, "/call/dynamic", bytes.NewReader(minimalDynamicInputJSON(t)))
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code < 400 {
		t.Errorf("status code: got %d, want >= 400", rec.Code)
	}
	if got := rec.Header().Get(HeaderBAMLPath); got != "" {
		t.Errorf("X-BAML-Path: got %q, want empty when result is nil", got)
	}
	if got := rec.Header().Get(HeaderBAMLPathReason); got != "" {
		t.Errorf("X-BAML-Path-Reason: got %q, want empty when result is nil", got)
	}
}
