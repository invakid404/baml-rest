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
// both typed BuildRequest surfaces and the legacy CallStream+OnTick
// FFI strings. Unrecognized errors must fall through to ("", nil) so
// the host's residual classifyWorkerError keeps owning them with the
// worker_error default.
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
			name:        "legacy Parsing error prefix",
			err:         errors.New("Parsing error: Failed to parse LLM response: missing field"),
			wantCode:    string(apierror.CodeParseError),
			wantDetails: "",
		},
		{
			// BAML v0.219 ClientHttpError envelope (engine/baml-runtime/
			// internal/llm_client/mod.rs ErrorCode::Display renders the
			// named enum followed by parenthesized digits, with the
			// provider body appended after "\nMessage:").
			name:        "legacy v0.219 ServerError envelope",
			err:         errors.New("LLM client \"GPT4o\" failed with status code: ServerError (500)\nMessage: Internal Server Error"),
			wantCode:    string(apierror.CodeProviderError),
			wantDetails: `{"status_code":500}`,
		},
		{
			name:        "legacy v0.219 RateLimited envelope",
			err:         errors.New("LLM client \"Claude\" failed with status code: RateLimited (429)\nMessage: Too Many Requests"),
			wantCode:    string(apierror.CodeProviderError),
			wantDetails: `{"status_code":429}`,
		},
		{
			name:        "legacy v0.219 ServiceUnavailable envelope",
			err:         errors.New("LLM client \"GPT4o\" failed with status code: ServiceUnavailable (503)\nMessage: upstream down"),
			wantCode:    string(apierror.CodeProviderError),
			wantDetails: `{"status_code":503}`,
		},
		{
			name:        "legacy v0.219 InvalidAuthentication envelope",
			err:         errors.New("LLM client \"GPT4o\" failed with status code: InvalidAuthentication (401)\nMessage: bad api key"),
			wantCode:    string(apierror.CodeProviderError),
			wantDetails: `{"status_code":401}`,
		},
		{
			name:        "legacy v0.219 Unspecified error code envelope",
			err:         errors.New("LLM client \"GPT4o\" failed with status code: Unspecified error code: 418\nMessage: I'm a teapot"),
			wantCode:    string(apierror.CodeProviderError),
			wantDetails: `{"status_code":418}`,
		},
		{
			name:        "legacy v0.219 BadResponse envelope",
			err:         errors.New("LLM client \"GPT4o\" failed with status code: BadResponse 599\nMessage: garbled response"),
			wantCode:    string(apierror.CodeProviderError),
			wantDetails: `{"status_code":599}`,
		},
		{
			// Bare-leading-digits form is kept for compatibility with
			// earlier BAML versions (and any future revert). Hand-built
			// or wrapped messages that match this shape continue to parse.
			name:        "legacy bare-digits compatibility form",
			err:         errors.New(`LLM client "Old" failed with status code: 503 (upstream body: <html>)`),
			wantCode:    string(apierror.CodeProviderError),
			wantDetails: `{"status_code":503}`,
		},
		{
			name:        "legacy LLM client failed with unparseable status segment",
			err:         errors.New(`LLM client "X" failed with status code: nope`),
			wantCode:    string(apierror.CodeProviderError),
			wantDetails: "",
		},
		{
			// Digits inside the trailing Message body must NOT leak into
			// the status_code detail when the enum segment itself has no
			// recognized digits.
			name:        "legacy unparseable enum segment ignores Message digits",
			err:         errors.New("LLM client \"X\" failed with status code: SomeNewEnum\nMessage: error 12345 from upstream"),
			wantCode:    string(apierror.CodeProviderError),
			wantDetails: "",
		},
		{
			name:        "legacy LLM client timed out",
			err:         errors.New(`LLM client "GPT4o" timed out: deadline exceeded after 30s`),
			wantCode:    string(apierror.CodeProviderError),
			wantDetails: "",
		},
		{
			name:        "untyped unrelated error falls through",
			err:         errors.New("some other bug"),
			wantCode:    "",
			wantDetails: "",
		},
		{
			name:        "BAML Internal Failure is not classified",
			err:         errors.New("Internal Failure: BAML runtime panic"),
			wantCode:    "",
			wantDetails: "",
		},
		{
			name:        "broad Failed to parse outside Parsing error envelope falls through",
			err:         errors.New("Failed to parse LLM response: not a JSON envelope"),
			wantCode:    "",
			wantDetails: "",
		},
		{
			name:        "LLM client prefix without recognized marker falls through",
			err:         errors.New(`LLM client "X" exploded for unknown reasons`),
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

// TestClassifyBAMLErrorTypedBeforeLegacy pins the resolution order:
// typed surfaces win over the legacy string classifier. A wrapper
// chain whose top-level Error() matches the legacy prefix while
// carrying a typed *llmhttp.HTTPError inside must emit the typed
// StatusCode, not the prefix-parsed one — otherwise a future call site
// that surfaces both shapes simultaneously would lose typed status_code
// fidelity to a string scrape.
func TestClassifyBAMLErrorTypedBeforeLegacy(t *testing.T) {
	t.Parallel()

	// fmt.Errorf's %w prepends to the chain's Error() string, so msg
	// starts with the legacy LLM-client prefix and would parse a 599
	// from the parenthesized v0.219-shape enum segment, but the
	// wrapped *HTTPError carries the authoritative 404.
	err := fmt.Errorf("LLM client \"X\" failed with status code: ServerError (599): %w", &llmhttp.HTTPError{StatusCode: 404})
	code, details := classifyBAMLError(err)
	if code != string(apierror.CodeProviderError) {
		t.Fatalf("code: got %q, want provider_error", code)
	}
	if got := string(details); !strings.Contains(got, `"status_code":404`) {
		t.Fatalf("typed status_code 404 must win over legacy-parsed 599; got %s", got)
	}
}
