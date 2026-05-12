package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils/buildrequest"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/internal/apierror"
)

// TestClassifyBAMLError pins the worker-side error → code mapping for
// the typed BuildRequest surfaces this PR wires through. Untyped /
// legacy errors must fall through to ("", nil) so the host's residual
// classifyWorkerError keeps owning them; PR 3 of #245 adds conservative
// string-prefix matching at the same boundary.
func TestClassifyBAMLError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		err         error
		wantCode    string
		wantDetails string // empty == nil
	}{
		{
			name:        "nil",
			err:         nil,
			wantCode:    "",
			wantDetails: "",
		},
		{
			name:        "ErrOutputParse direct",
			err:         &buildrequest.OutputParseError{Err: errors.New("Parsing error: bad")},
			wantCode:    string(apierror.CodeParseError),
			wantDetails: "",
		},
		{
			name:        "ErrOutputParse wrapped",
			err:         fmt.Errorf("worker: %w", &buildrequest.OutputParseError{Err: errors.New("Parsing error: bad")}),
			wantCode:    string(apierror.CodeParseError),
			wantDetails: "",
		},
		{
			name:        "HTTPError 429",
			err:         &llmhttp.HTTPError{StatusCode: 429, Body: "rate limit"},
			wantCode:    string(apierror.CodeProviderError),
			wantDetails: `{"status_code":429}`,
		},
		{
			name:        "HTTPError 503 wrapped",
			err:         fmt.Errorf("buildrequest: %w", &llmhttp.HTTPError{StatusCode: 503, Body: "upstream down"}),
			wantCode:    string(apierror.CodeProviderError),
			wantDetails: `{"status_code":503}`,
		},
		{
			name:        "transport flake wrapped",
			err:         fmt.Errorf("buildrequest: stream error: %w", llmhttp.ErrTransportFlake),
			wantCode:    string(apierror.CodeProviderError),
			wantDetails: "",
		},
		{
			name:        "untyped error",
			err:         errors.New("some other bug"),
			wantCode:    "",
			wantDetails: "",
		},
		{
			name:        "untyped LLM-shaped string is not classified",
			err:         errors.New(`LLM client "X" failed with status code: 500`),
			wantCode:    "",
			wantDetails: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotCode, gotDetails := classifyBAMLError(tc.err)
			if gotCode != tc.wantCode {
				t.Errorf("code: got %q, want %q", gotCode, tc.wantCode)
			}
			gotDetailsStr := string(gotDetails)
			if gotDetailsStr != tc.wantDetails {
				t.Errorf("details: got %q, want %q", gotDetailsStr, tc.wantDetails)
			}
		})
	}
}

// TestClassifyBAMLErrorHTTPErrorPrefersInnermost verifies that when a
// transport flake wraps an HTTPError-shaped chain (or vice versa), the
// HTTPError branch matches via errors.As. The match order matters: a
// future caller that wraps a HTTPError inside a TransportError would
// otherwise lose the status_code detail.
func TestClassifyBAMLErrorHTTPErrorPrefersInnermost(t *testing.T) {
	t.Parallel()

	// Double-wrap: HTTPError → fmt.Errorf → fmt.Errorf. errors.As must
	// still find the *HTTPError and emit status_code details.
	err := fmt.Errorf("outer: %w", fmt.Errorf("inner: %w", &llmhttp.HTTPError{StatusCode: 418}))

	code, details := classifyBAMLError(err)
	if code != string(apierror.CodeProviderError) {
		t.Fatalf("code: got %q, want provider_error", code)
	}
	if !strings.Contains(string(details), `"status_code":418`) {
		t.Fatalf("details missing status_code 418: %s", string(details))
	}
}
