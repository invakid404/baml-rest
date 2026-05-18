package worker

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils/awsstream"
	"github.com/invakid404/baml-rest/bamlutils/buildrequest"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
)

// TestClassifyBAMLError pins the worker-side error → code mapping for
// both typed BuildRequest surfaces and the legacy CallStream+OnTick
// FFI strings. Unrecognized errors must fall through to ("", nil) so
// the host's residual classifyWorkerError keeps owning them with the
// worker_error default.
//
// provider_error details forward what baml-rest has from the failed
// upstream: the HTTP status when known, the raw body, and the BAML
// client name on legacy envelopes. Empty fields are omitted so the
// envelope stays terse — assertions below use full-string equality
// against the marshaled JSON to pin field ordering and omission.
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
			wantCode:    workerCodeParseError,
			wantDetails: "",
		},
		{
			name:        "ErrOutputParse wrapped",
			err:         fmt.Errorf("worker: %w", &buildrequest.OutputParseError{Err: errors.New("Parsing error: bad")}),
			wantCode:    workerCodeParseError,
			wantDetails: "",
		},
		{
			name:        "HTTPError 429 forwards body",
			err:         &llmhttp.HTTPError{StatusCode: 429, Body: "rate limit"},
			wantCode:    workerCodeProviderError,
			wantDetails: `{"status_code":429,"body":"rate limit"}`,
		},
		{
			name:        "HTTPError 503 wrapped forwards body",
			err:         fmt.Errorf("buildrequest: %w", &llmhttp.HTTPError{StatusCode: 503, Body: "upstream down"}),
			wantCode:    workerCodeProviderError,
			wantDetails: `{"status_code":503,"body":"upstream down"}`,
		},
		{
			// Pins the omitempty contract for the BuildRequest arm:
			// callers with no body get just status_code.
			name:        "HTTPError with empty body omits body field",
			err:         &llmhttp.HTTPError{StatusCode: 418},
			wantCode:    workerCodeProviderError,
			wantDetails: `{"status_code":418}`,
		},
		{
			name:        "transport flake wrapped",
			err:         fmt.Errorf("buildrequest: stream error: %w", llmhttp.ErrTransportFlake),
			wantCode:    workerCodeProviderError,
			wantDetails: "",
		},
		{
			// AWS event-stream transport errors carry :error-code /
			// :error-message headers; the arm forwards both into
			// provider_error details so the AWS-side reason is
			// observable upstream.
			name:        "awsstream TransportError direct",
			err:         &awsstream.TransportError{Code: "InternalServerError", Message: "service unavailable"},
			wantCode:    workerCodeProviderError,
			wantDetails: `{"error_code":"InternalServerError","error_message":"service unavailable"}`,
		},
		{
			// Wrappers (buildrequest: %w / orchestrator wrap) must not
			// hide the typed transport error — errors.As walks the
			// chain.
			name:        "awsstream TransportError wrapped",
			err:         fmt.Errorf("buildrequest: stream error: %w", &awsstream.TransportError{Code: "ThrottlingException", Message: "slow down"}),
			wantCode:    workerCodeProviderError,
			wantDetails: `{"error_code":"ThrottlingException","error_message":"slow down"}`,
		},
		{
			// A TransportError with only the code header populated
			// keeps the message field omitted from JSON via
			// omitempty.
			name:        "awsstream TransportError code only",
			err:         &awsstream.TransportError{Code: "InternalServerError"},
			wantCode:    workerCodeProviderError,
			wantDetails: `{"error_code":"InternalServerError"}`,
		},
		{
			// Bedrock modeled exceptions (in-band
			// :message-type=exception frames) classify as
			// provider_error with exception_type + exception_message
			// — distinct fields from error_code/error_message so
			// consumers can tell a torn transport apart from a
			// modeled exception even though both share the same
			// taxonomy code.
			name: "BedrockStreamException direct",
			err: &buildrequest.BedrockStreamException{
				ExceptionType: "ModelStreamErrorException",
				Payload:       []byte(`{"message":"the model refused"}`),
			},
			wantCode:    workerCodeProviderError,
			wantDetails: `{"exception_type":"ModelStreamErrorException","exception_message":"the model refused"}`,
		},
		{
			// Wrapper chains (orchestrator's "buildrequest: delta
			// extraction failed: %w") must not hide the typed
			// exception — errors.As walks the chain.
			name: "BedrockStreamException wrapped",
			err: fmt.Errorf("buildrequest: delta extraction failed: %w",
				&buildrequest.BedrockStreamException{
					ExceptionType: "ThrottlingException",
					Payload:       []byte(`{"message":"slow down"}`),
				}),
			wantCode:    workerCodeProviderError,
			wantDetails: `{"exception_type":"ThrottlingException","exception_message":"slow down"}`,
		},
		{
			// Payload without a recognised `message` field still
			// classifies as provider_error but drops the
			// exception_message detail via omitempty. The exception
			// type alone is enough operator context to triage.
			name: "BedrockStreamException no payload message",
			err: &buildrequest.BedrockStreamException{
				ExceptionType: "ValidationException",
				Payload:       []byte(`{"other":"shape"}`),
			},
			wantCode:    workerCodeProviderError,
			wantDetails: `{"exception_type":"ValidationException"}`,
		},
		{
			name:        "legacy Parsing error prefix",
			err:         errors.New("Parsing error: Failed to parse LLM response: missing field"),
			wantCode:    workerCodeParseError,
			wantDetails: "",
		},
		{
			// BAML v0.219 ClientHttpError envelope (engine/baml-runtime/
			// internal/llm_client/mod.rs ErrorCode::Display renders the
			// named enum followed by parenthesized digits, with the
			// provider body appended after "\nMessage:"). The full
			// body and client name now ride along with status_code.
			name:        "legacy v0.219 ServerError envelope forwards body and client_name",
			err:         errors.New("LLM client \"GPT4o\" failed with status code: ServerError (500)\nMessage: Internal Server Error"),
			wantCode:    workerCodeProviderError,
			wantDetails: `{"status_code":500,"body":"Message: Internal Server Error","client_name":"GPT4o"}`,
		},
		{
			name:        "legacy v0.219 RateLimited envelope forwards body and client_name",
			err:         errors.New("LLM client \"Claude\" failed with status code: RateLimited (429)\nMessage: Too Many Requests"),
			wantCode:    workerCodeProviderError,
			wantDetails: `{"status_code":429,"body":"Message: Too Many Requests","client_name":"Claude"}`,
		},
		{
			name:        "legacy v0.219 ServiceUnavailable envelope forwards body and client_name",
			err:         errors.New("LLM client \"GPT4o\" failed with status code: ServiceUnavailable (503)\nMessage: upstream down"),
			wantCode:    workerCodeProviderError,
			wantDetails: `{"status_code":503,"body":"Message: upstream down","client_name":"GPT4o"}`,
		},
		{
			name:        "legacy v0.219 InvalidAuthentication envelope forwards body and client_name",
			err:         errors.New("LLM client \"GPT4o\" failed with status code: InvalidAuthentication (401)\nMessage: bad api key"),
			wantCode:    workerCodeProviderError,
			wantDetails: `{"status_code":401,"body":"Message: bad api key","client_name":"GPT4o"}`,
		},
		{
			name:        "legacy v0.219 Unspecified error code envelope forwards body and client_name",
			err:         errors.New("LLM client \"GPT4o\" failed with status code: Unspecified error code: 418\nMessage: I'm a teapot"),
			wantCode:    workerCodeProviderError,
			wantDetails: `{"status_code":418,"body":"Message: I'm a teapot","client_name":"GPT4o"}`,
		},
		{
			name:        "legacy v0.219 BadResponse envelope forwards body and client_name",
			err:         errors.New("LLM client \"GPT4o\" failed with status code: BadResponse 599\nMessage: garbled response"),
			wantCode:    workerCodeProviderError,
			wantDetails: `{"status_code":599,"body":"Message: garbled response","client_name":"GPT4o"}`,
		},
		{
			// Bare-leading-digits form is kept for compatibility with
			// earlier BAML versions (and any future revert). Hand-built
			// or wrapped messages that match this shape continue to parse.
			// No "\nMessage:" suffix here → body is omitted; client_name
			// still rides along.
			name:        "legacy bare-digits compatibility form omits empty body",
			err:         errors.New(`LLM client "Old" failed with status code: 503 (upstream body: <html>)`),
			wantCode:    workerCodeProviderError,
			wantDetails: `{"status_code":503,"client_name":"Old"}`,
		},
		{
			// Status segment unparseable, single-line envelope → body
			// stays omitted, status_code dropped, client_name preserved.
			// Failed-closed: still provider_error, never falls through
			// to worker_error.
			name:        "legacy LLM client failed with unparseable status segment keeps client_name",
			err:         errors.New(`LLM client "X" failed with status code: nope`),
			wantCode:    workerCodeProviderError,
			wantDetails: `{"client_name":"X"}`,
		},
		{
			// Digits inside the trailing Message body must NOT leak into
			// the status_code detail when the enum segment itself has no
			// recognized digits. The body and client_name still ride
			// along as provider context.
			name:        "legacy unparseable enum segment forwards body without status_code",
			err:         errors.New("LLM client \"X\" failed with status code: SomeNewEnum\nMessage: error 12345 from upstream"),
			wantCode:    workerCodeProviderError,
			wantDetails: `{"body":"Message: error 12345 from upstream","client_name":"X"}`,
		},
		{
			// Pins the defensive contract: a future/unrecognized BAML
			// enum display that happens to use the same `Name (NNN)`
			// shape must NOT match the parenthesized branch and leak
			// the wrong status code. Go regexp distributes `^` only to
			// the first alternative; wrapping the alternation in a
			// non-capturing group plus an explicit enum allowlist is
			// what enforces this — without it, this case would parse
			// 599 and mislabel an unknown variant.
			name:        "legacy unrecognized enum name with parens does not leak status_code",
			err:         errors.New("LLM client \"X\" failed with status code: SomeNewEnum (599)\nMessage: who knows"),
			wantCode:    workerCodeProviderError,
			wantDetails: `{"body":"Message: who knows","client_name":"X"}`,
		},
		{
			// Pins the first-line-only marker contract: an LLM-client-
			// prefixed envelope whose first line lacks the recognized
			// status marker must NOT be classified just because the
			// later message body happens to contain the marker phrase
			// verbatim. Without first-line scoping, the body's
			// "failed with status code: ServerError (500)" would
			// satisfy the Index check and leak provider_error.
			name:        "legacy unknown envelope with status marker in body falls through",
			err:         errors.New("LLM client \"X\" generic-unknown-prefix\nMessage: failed with status code: ServerError (500)\nDetails: ..."),
			wantCode:    "",
			wantDetails: "",
		},
		{
			// Same contract for the timeout marker — body text can't
			// promote an unknown first-line envelope to provider_error.
			name:        "legacy unknown envelope with timeout marker in body falls through",
			err:         errors.New("LLM client \"X\" generic-unknown-prefix\nMessage: timed out: 30s elapsed before first byte"),
			wantCode:    "",
			wantDetails: "",
		},
		{
			// Pins the post-quote-anchor contract: the status marker
			// must begin immediately at the closing client-name quote,
			// not anywhere within firstLine. Here the first `"` ends a
			// weird client name ("weird-name") followed by extraneous
			// text, and a LATER `"` happens to be followed by the
			// marker. A whole-line Index search would classify this as
			// provider_error with status_code=500; the anchored
			// HasPrefix must reject it.
			name:        "legacy status marker after extraneous post-quote text falls through",
			err:         errors.New("LLM client \"weird-name\" extra stuff\" failed with status code: ServerError (500)\nMessage: not the real envelope"),
			wantCode:    "",
			wantDetails: "",
		},
		{
			// Same contract for the timeout marker — extraneous text
			// after the first closing quote must prevent classification
			// even when a later embedded `"` is followed by the timeout
			// marker substring.
			name:        "legacy timeout marker after extraneous post-quote text falls through",
			err:         errors.New("LLM client \"weird-name\" extra stuff\" timed out: 30s deadline"),
			wantCode:    "",
			wantDetails: "",
		},
		{
			// Empty client name (a malformed BAML envelope) — even
			// though the suffix immediately after the empty-name quote
			// matches the status marker, the helper bails on q==0 so
			// the classifier falls closed. Without this, baml-rest
			// would emit provider_error with no client_name and a
			// status pulled from an envelope shape that BAML doesn't
			// actually produce.
			name:        "legacy empty client name with status marker falls through",
			err:         errors.New("LLM client \"\" failed with status code: ServerError (500)\nMessage: ..."),
			wantCode:    "",
			wantDetails: "",
		},
		{
			name:        "legacy empty client name with timeout marker falls through",
			err:         errors.New("LLM client \"\" timed out: 30s"),
			wantCode:    "",
			wantDetails: "",
		},
		{
			// Timeout envelope is single-line per BAML's Display impl
			// (errors.rs:114), so body is empty; client_name still
			// surfaces. No status_code on timeout (no HTTP response).
			name:        "legacy LLM client timed out forwards client_name",
			err:         errors.New(`LLM client "GPT4o" timed out: deadline exceeded after 30s`),
			wantCode:    workerCodeProviderError,
			wantDetails: `{"client_name":"GPT4o"}`,
		},
		{
			// Multi-line future-shape timeout envelope: body forwarded
			// alongside client_name even though BAML doesn't currently
			// emit one. Mirrors the status-arm body extraction so the
			// two arms stay symmetric.
			name:        "legacy timeout with trailing body forwards both",
			err:         errors.New("LLM client \"GPT4o\" timed out: deadline\nDetails: 30s deadline"),
			wantCode:    workerCodeProviderError,
			wantDetails: `{"body":"Details: 30s deadline","client_name":"GPT4o"}`,
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
			// BAML v0.214's types/response.rs:159 emits this generic
			// anyhow!() string for the LLMResponse::Success-with-no-
			// parsed-result case. The underlying parse-failure details
			// are already discarded by the time the FFI sees this, so
			// there's no `Parsing error: ` substring to anchor on.
			// Classifying this as parse_error would conflate baml-rest/
			// BAML internal fallbacks with prompt-output parse failures
			// (the #245 taxonomy goal explicitly separates the two), so
			// it falls through to worker_error.
			name:        "BAML v0.214 generic no-result fallback is not classified",
			err:         errors.New("This should never happen - Please report this error to our team with BAML_LOG=info enabled so we can improve this error message"),
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
	if code != workerCodeProviderError {
		t.Fatalf("code: got %q, want provider_error", code)
	}
	if !strings.Contains(string(details), `"status_code":418`) {
		t.Fatalf("details missing status_code 418: %s", string(details))
	}
}

// TestMergeRawDetail pins the helper's four cases: empty-raw no-op,
// nil-details fresh object, object-details merge, and invalid-JSON
// defensive fallback. The helper is the worker-side gate that
// determines whether legacy CallStream+OnTick error envelopes carry
// the accumulated raw text on the wire (per #256), so each branch is
// load-bearing.
func TestMergeRawDetail(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		details []byte
		raw     string
		want    string
	}{
		{
			name:    "empty raw and nil details is no-op",
			details: nil,
			raw:     "",
			want:    "",
		},
		{
			name:    "empty raw with object details is no-op",
			details: []byte(`{"status_code":500}`),
			raw:     "",
			want:    `{"status_code":500}`,
		},
		{
			name:    "nil details produces fresh raw object",
			details: nil,
			raw:     "unparseable prose",
			want:    `{"raw":"unparseable prose"}`,
		},
		{
			name:    "empty-bytes details produces fresh raw object",
			details: []byte{},
			raw:     "x",
			want:    `{"raw":"x"}`,
		},
		{
			name:    "object details get raw merged in",
			details: []byte(`{"status_code":500,"body":"oops"}`),
			raw:     "tail",
			want:    `{"body":"oops","raw":"tail","status_code":500}`,
		},
		{
			name:    "invalid JSON details fall back to fresh raw object",
			details: []byte(`not json`),
			raw:     "salvaged",
			want:    `{"raw":"salvaged"}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := mergeRawDetail(tc.details, tc.raw)
			gotStr := string(got)
			// Object cases require key-set comparison rather than byte
			// equality because Go's map iteration is unordered; the
			// helper marshals from map[string]any so encoding/json's
			// alphabetical key order applies but pre-existing details
			// in the test case may not already be sorted.
			if tc.raw != "" && len(tc.details) > 0 && isJSONObject(tc.details) {
				wantSet := jsonObjectKeyset(t, []byte(tc.want))
				gotSet := jsonObjectKeyset(t, got)
				if !equalKeyset(wantSet, gotSet) {
					t.Errorf("merged details mismatch:\n  got  %s\n  want %s", gotStr, tc.want)
				}
				return
			}
			if gotStr != tc.want {
				t.Errorf("got %q, want %q", gotStr, tc.want)
			}
		})
	}
}

// isJSONObject reports whether b begins (after any leading whitespace)
// with '{'. Used by TestMergeRawDetail to pick between byte-equality
// and key-set comparison for object-shaped inputs.
func isJSONObject(b []byte) bool {
	for _, c := range b {
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			continue
		}
		return c == '{'
	}
	return false
}

func jsonObjectKeyset(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal %s: %v", string(b), err)
	}
	return m
}

func equalKeyset(a, b map[string]any) bool {
	if len(a) != len(b) {
		return false
	}
	for k, va := range a {
		vb, ok := b[k]
		if !ok {
			return false
		}
		if fmt.Sprintf("%v", va) != fmt.Sprintf("%v", vb) {
			return false
		}
	}
	return true
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
	if code != workerCodeProviderError {
		t.Fatalf("code: got %q, want provider_error", code)
	}
	if got := string(details); !strings.Contains(got, `"status_code":404`) {
		t.Fatalf("typed status_code 404 must win over legacy-parsed 599; got %s", got)
	}
}
