package admission

import (
	"errors"
	"fmt"
	"testing"

	nanollm "github.com/viktordanov/nanollm-ffi/go"
)

// TestClassifyEngineError proves the typed New/Prepare classifier (§5.1) keys on
// the nanollm *Error.Code — NEVER a string match: only unsupported_request and
// invalid_provider are ordinary pre-socket unsupported declines; every other code
// (including v0.4.3's generic "config" for an unknown prefix), a non-nanollm
// error, and a nil-classified-as-planner surface as a planner error.
func TestClassifyEngineError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want engineDisposition
	}{
		{"unsupported_request", &nanollm.Error{Code: codeUnsupportedRequest}, engineUnsupported},
		{"invalid_provider", &nanollm.Error{Code: codeInvalidProvider}, engineUnsupported},
		{"wrapped_unsupported", fmt.Errorf("build: %w", &nanollm.Error{Code: codeUnsupportedRequest}), engineUnsupported},
		{"config_is_planner", &nanollm.Error{Code: "config"}, enginePlannerError},
		{"other_code_is_planner", &nanollm.Error{Code: "abi"}, enginePlannerError},
		{"non_nanollm_is_planner", errors.New("plain error"), enginePlannerError},
		// A message that merely CONTAINS the code text must not be classified as
		// unsupported: the classifier never string-matches.
		{"string_lookalike_is_planner", errors.New("unsupported_request happened"), enginePlannerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyEngineError(tc.err); got != tc.want {
				t.Fatalf("classifyEngineError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestPrepareUnsupportedReason proves the engineUnsupported reason split: an
// invalid_provider code maps to the invalid_provider decline reason, and every
// other unsupported code (unsupported_request) maps to unsupported_request.
func TestPrepareUnsupportedReason(t *testing.T) {
	if got := prepareUnsupportedReason(&nanollm.Error{Code: codeInvalidProvider}); got != ReasonPrepareInvalidProvider {
		t.Errorf("invalid_provider -> %q, want %q", got, ReasonPrepareInvalidProvider)
	}
	if got := prepareUnsupportedReason(&nanollm.Error{Code: codeUnsupportedRequest}); got != ReasonPrepareUnsupportedRequest {
		t.Errorf("unsupported_request -> %q, want %q", got, ReasonPrepareUnsupportedRequest)
	}
}
