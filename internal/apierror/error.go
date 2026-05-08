// Package apierror provides utilities for returning consistent JSON error responses.
package apierror

import (
	"net/http"

	"github.com/goccy/go-json"
)

// Code is a stable, machine-readable identifier for an error class.
// Clients (including LLM agents consuming errors as tool feedback) can branch
// on these without parsing free-form messages.
type Code string

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
	// CodeWorkerError: the BAML worker returned an application-level
	// error — typically a BAML validation failure, a parse error against
	// the BAML schema, or an upstream LLM provider error. The accompanying
	// message comes verbatim from BAML / the LLM provider.
	CodeWorkerError Code = "worker_error"
	// CodeParseError: the /parse endpoint failed to parse raw LLM output
	// against the BAML method's schema. Distinguished from CodeWorkerError
	// because parse failures don't involve an LLM call.
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
	Details   json.RawMessage `json:"details,omitempty"`
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
func WriteJSONWithCode(w http.ResponseWriter, message string, code Code, details json.RawMessage, statusCode int, requestID string) {
	resp := Response{
		Error:     message,
		Code:      code,
		Details:   details,
		RequestID: requestID,
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)

	// Best effort - if encoding fails, we've already written the status code
	_ = json.NewEncoder(w).Encode(resp)
}
