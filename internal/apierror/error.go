// Package apierror provides utilities for returning consistent JSON error responses.
package apierror

import (
	stdjson "encoding/json"
	"net/http"

	"github.com/bytedance/sonic"
)

// Code is a stable, machine-readable identifier for an error class.
// Clients (including LLM agents consuming errors as tool feedback) can branch
// on these without parsing free-form messages.
type Code string

// allCodes is the canonical ordered list of public Code constants.
// Single source of truth for IsKnown() (membership check) and
// AllCodes() (enum exposed to the OpenAPI generator). Add new codes
// here so both consumers stay in sync.
var allCodes = []Code{
	CodeInvalidJSON,
	CodeInvalidRequest,
	CodeRequestTooLarge,
	CodeBodyReadError,
	CodeNotAcceptable,
	CodeRequestCanceled,
	CodeWorkerUnavailable,
	CodeWorkerError,
	CodeProviderError,
	CodeParseError,
	CodeInternalError,
}

// AllCodes returns the canonical list of public Code constants in
// declaration order. Returned slice is a copy so callers can't mutate
// the package-level source. Used by the OpenAPI schema generator to
// build the error-code enum without duplicating the list.
func AllCodes() []Code {
	out := make([]Code, len(allCodes))
	copy(out, allCodes)
	return out
}

// IsKnown reports whether c is one of the documented Code constants.
// The HTTP layer validates worker-supplied codes against this set
// before forwarding them so the public OpenAPI enum stays
// authoritative — a worker emitting an unknown code falls back to
// host-side classification rather than introducing an off-contract
// value into the response envelope.
func (c Code) IsKnown() bool {
	for _, k := range allCodes {
		if c == k {
			return true
		}
	}
	return false
}

// IsWorkerFacing reports whether c is a code a worker is allowed to
// self-classify as. Workers can only emit codes that describe what
// happened inside the worker process — request-layer codes
// (invalid_json, request_too_large, request_canceled, ...) and pool-
// admission codes (worker_unavailable) are owned by the host and
// must NOT be honored if a worker supplies them: doing so would let
// worker-side text dictate HTTP status semantics (e.g. claiming
// request_canceled to force a 408 from the host's classifier).
//
// The allow-list is intentionally narrow: the worker-classified
// outcomes the host trusts are exactly worker_error (residual
// BAML/worker failure), provider_error (upstream LLM provider
// returned a non-2xx HTTP status or transport failed), parse_error
// (BAML couldn't coerce raw text into the method schema), and
// internal_error (a Go-side processing bug inside the worker).
// Unknown or non-worker-facing codes fall back to host-side
// classification.
func (c Code) IsWorkerFacing() bool {
	switch c {
	case CodeWorkerError, CodeProviderError, CodeParseError, CodeInternalError:
		return true
	}
	return false
}

const (
	// CodeInvalidJSON: request body wasn't valid JSON.
	CodeInvalidJSON Code = "invalid_json"
	// CodeInvalidRequest: request was syntactically valid JSON but failed
	// schema/semantic validation (e.g. dynamic input Validate()).
	CodeInvalidRequest Code = "invalid_request"
	// CodeRequestTooLarge: request body exceeded the configured size limit.
	CodeRequestTooLarge Code = "request_too_large"
	// CodeBodyReadError: I/O error reading the request body.
	CodeBodyReadError Code = "body_read_error"
	// CodeNotAcceptable: client's Accept header doesn't intersect with
	// formats the endpoint can produce.
	CodeNotAcceptable Code = "not_acceptable"
	// CodeRequestCanceled: client disconnected or the request deadline
	// was exceeded before completion.
	CodeRequestCanceled Code = "request_canceled"
	// CodeWorkerUnavailable: the BAML worker pool exhausted retries on
	// retryable infrastructure failures (worker crash, gRPC Unavailable,
	// transport reset). Retrying the request later may succeed.
	CodeWorkerUnavailable Code = "worker_unavailable"
	// CodeWorkerError: residual BAML/worker failure that doesn't fit a
	// more specific code (parse, provider, internal, cancellation,
	// pool-unavailable, etc.). Should be rare once the per-class codes
	// are wired through both the BuildRequest and legacy paths; remains
	// the catch-all for unclassified worker-side failures.
	CodeWorkerError Code = "worker_error"
	// CodeProviderError: upstream LLM provider returned a non-2xx HTTP
	// status or the transport failed before BAML could parse a
	// response. Always carries details.status_code when the upstream
	// produced an HTTP response; the field is absent when the failure
	// was at the transport layer (DNS, TCP, TLS, mid-stream reset).
	CodeProviderError Code = "provider_error"
	// CodeParseError: BAML couldn't coerce or validate raw text into
	// the method's schema. Applies to both /parse (raw input from the
	// request body) and /call / /call-with-raw / /stream (raw LLM
	// output that didn't conform to the method's return type).
	CodeParseError Code = "parse_error"
	// CodeInternalError: an internal Go-side processing error (output
	// flattening, input conversion, panic). Indicates a bug in this
	// server, not in the BAML schema or LLM call.
	CodeInternalError Code = "internal_error"
)

// Response is the standard JSON error response format.
//
// Code is a stable identifier for the error class; clients should branch
// on Code rather than parsing Error. Details carries optional structured
// context (e.g. stacktrace from a worker panic) when available — the field
// is omitted entirely when there's nothing structured to report.
type Response struct {
	Error     string          `json:"error"`
	Code      Code            `json:"code,omitempty"`
	Details   stdjson.RawMessage `json:"details,omitempty"`
	RequestID string          `json:"request_id,omitempty"`
}

// WriteJSON writes a JSON-formatted error response with the given status code.
// The requestID parameter is optional and will be omitted from the response if empty.
func WriteJSON(w http.ResponseWriter, message string, statusCode int, requestID string) {
	WriteJSONWithCode(w, message, "", nil, statusCode, requestID)
}

// WriteJSONWithCode writes a JSON-formatted error response carrying a
// machine-readable code and optional structured details. Pass code=""
// and details=nil to omit those fields from the response.
//
// details is validated with json.Valid before being placed on the
// response: invalid bytes are dropped and Details is omitted, so the
// encoded body is always well-formed JSON. Upstream callers
// (cmd/serve/error.go normalizeWorkerMetadata) already gate to JSON
// objects, but this helper is exported and the validation here is
// defense-in-depth — without it, a future caller passing an invalid
// stdjson.RawMessage would emit malformed bytes verbatim because
// stdjson.RawMessage's MarshalJSON returns its input as-is.
func WriteJSONWithCode(w http.ResponseWriter, message string, code Code, details stdjson.RawMessage, statusCode int, requestID string) {
	resp := Response{
		Error:     message,
		Code:      code,
		Details:   ValidDetails(details),
		RequestID: requestID,
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)

	// Best effort - if encoding fails, we've already written the status code
	_ = sonic.ConfigDefault.NewEncoder(w).Encode(resp)
}

// ValidDetails returns details unchanged when the bytes parse as
// valid JSON, or nil otherwise. Callers placing a stdjson.RawMessage
// onto the Response envelope should run it through this helper so
// the encoded body is always well-formed — RawMessage is verbatim-
// marshalled, so an invalid payload would otherwise corrupt the
// response stream. nil and empty inputs are passed through (and
// omit from the envelope via the omitempty json tag).
func ValidDetails(details stdjson.RawMessage) stdjson.RawMessage {
	if len(details) == 0 {
		return details
	}
	if !sonic.Valid(details) {
		return nil
	}
	return details
}
