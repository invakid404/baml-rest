package apierror

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestValidDetails pins the json.Valid contract: invalid bytes drop
// to nil so callers placing a json.RawMessage on the response
// envelope can't accidentally corrupt the encoded body.
func TestValidDetails(t *testing.T) {
	tests := []struct {
		name string
		in   json.RawMessage
		want bool // true if expect input passed through, false if dropped
	}{
		{"nil passes", nil, true},
		{"empty passes", json.RawMessage{}, true},
		{"valid object", json.RawMessage(`{"k":"v"}`), true},
		{"valid nested object", json.RawMessage(`{"a":{"b":[1,2]}}`), true},
		{"valid array", json.RawMessage(`[1,2,3]`), true},
		{"valid scalar string", json.RawMessage(`"x"`), true},
		{"valid scalar number", json.RawMessage(`42`), true},
		{"valid null", json.RawMessage(`null`), true},
		{"truncated object dropped", json.RawMessage(`{"k":`), false},
		{"garbage dropped", json.RawMessage(`}garbage{`), false},
		{"unterminated string dropped", json.RawMessage(`"abc`), false},
		{"trailing junk dropped", json.RawMessage(`{}xx`), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ValidDetails(tt.in)
			if tt.want {
				if string(got) != string(tt.in) {
					t.Errorf("ValidDetails(%q) = %q, want pass-through %q", tt.in, got, tt.in)
				}
			} else {
				if got != nil {
					t.Errorf("ValidDetails(%q) = %q, want nil", tt.in, got)
				}
			}
		})
	}
}

// TestWriteJSONWithCode_DropsInvalidDetails pins the defensive guard
// in WriteJSONWithCode: an invalid json.RawMessage must not corrupt
// the encoded body. Without ValidDetails the invalid bytes would be
// emitted verbatim because json.RawMessage's MarshalJSON returns its
// input as-is. Verified by re-parsing the response body — if the
// guard is removed, this test fails on json.Unmarshal.
func TestWriteJSONWithCode_DropsInvalidDetails(t *testing.T) {
	rr := httptest.NewRecorder()
	WriteJSONWithCode(rr, "boom", CodeWorkerError, json.RawMessage(`{"k":`), 500, "req-1")

	var parsed Response
	if err := json.Unmarshal(rr.Body.Bytes(), &parsed); err != nil {
		t.Fatalf("response body is not valid JSON: %v\nbody: %s", err, rr.Body.String())
	}
	if parsed.Error != "boom" {
		t.Errorf("Error = %q, want %q", parsed.Error, "boom")
	}
	if parsed.Code != CodeWorkerError {
		t.Errorf("Code = %q, want %q", parsed.Code, CodeWorkerError)
	}
	// Details must be omitted when invalid: the omitempty json tag
	// drops nil RawMessage from the wire.
	if len(parsed.Details) != 0 {
		t.Errorf("Details = %q, want empty (invalid input dropped)", parsed.Details)
	}
	if strings.Contains(rr.Body.String(), `"details"`) {
		t.Errorf("response body must not include details field when invalid; body: %s", rr.Body.String())
	}
}

// TestWriteJSONWithCode_PreservesValidDetails verifies the guard is
// pass-through for legitimate object payloads — the production case.
func TestWriteJSONWithCode_PreservesValidDetails(t *testing.T) {
	rr := httptest.NewRecorder()
	WriteJSONWithCode(rr, "boom", CodeWorkerError, json.RawMessage(`{"stacktrace":"goroutine 1 ..."}`), 500, "req-1")

	var parsed struct {
		Error   string `json:"error"`
		Code    Code   `json:"code"`
		Details struct {
			Stacktrace string `json:"stacktrace"`
		} `json:"details"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &parsed); err != nil {
		t.Fatalf("response body is not valid JSON: %v", err)
	}
	if parsed.Details.Stacktrace != "goroutine 1 ..." {
		t.Errorf("Details.Stacktrace = %q, want forwarded value", parsed.Details.Stacktrace)
	}
}

// TestIsWorkerFacing pins the worker-facing whitelist used to gate
// worker-supplied codes at the host. Request-layer and pool-admission
// codes must be rejected even though they're IsKnown(), so a worker
// can't claim e.g. request_canceled to force the host's 408 branch.
func TestIsWorkerFacing(t *testing.T) {
	tests := []struct {
		code Code
		want bool
	}{
		{CodeWorkerError, true},
		{CodeProviderError, true},
		{CodeParseError, true},
		{CodeInternalError, true},
		// Request-layer (host-owned) — not worker-facing.
		{CodeInvalidJSON, false},
		{CodeInvalidRequest, false},
		{CodeRequestTooLarge, false},
		{CodeBodyReadError, false},
		{CodeNotAcceptable, false},
		{CodeRequestCanceled, false},
		// Pool-admission (host-owned) — not worker-facing.
		{CodeWorkerUnavailable, false},
		// Unknown / empty.
		{Code(""), false},
		{Code("made_up"), false},
	}
	for _, tt := range tests {
		t.Run(string(tt.code), func(t *testing.T) {
			if got := tt.code.IsWorkerFacing(); got != tt.want {
				t.Errorf("Code(%q).IsWorkerFacing() = %v, want %v", tt.code, got, tt.want)
			}
		})
	}
}
