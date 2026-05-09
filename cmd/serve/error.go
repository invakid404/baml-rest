package main

import (
	"bytes"
	"errors"
	"net/http"

	"github.com/goccy/go-json"
	"github.com/gofiber/fiber/v3"
	fiberrequestid "github.com/gofiber/fiber/v3/middleware/requestid"
	"github.com/invakid404/baml-rest/internal/apierror"
	"github.com/invakid404/baml-rest/internal/httplogger"
	"github.com/invakid404/baml-rest/pool"
	"github.com/invakid404/baml-rest/workerplugin"
)

// writeFiberJSONError writes a JSON-formatted error response for native Fiber handlers.
func writeFiberJSONError(c fiber.Ctx, message string, statusCode int) error {
	return writeFiberJSONErrorWithCode(c, message, "", nil, statusCode)
}

// writeFiberJSONErrorWithCode writes a JSON error response carrying a
// machine-readable code and optional structured details. Pass code=""
// and details=nil to omit those fields. details is validated through
// apierror.ValidDetails so an invalid json.RawMessage cannot corrupt
// the encoded body — same defense-in-depth as apierror.WriteJSONWithCode
// applies on the chi side.
func writeFiberJSONErrorWithCode(c fiber.Ctx, message string, code apierror.Code, details json.RawMessage, statusCode int) error {
	return c.Status(statusCode).JSON(apierror.Response{
		Error:     message,
		Code:      code,
		Details:   apierror.ValidDetails(details),
		RequestID: fiberrequestid.FromContext(c),
	})
}

// writeFiberWorkerError classifies and writes a worker-originated error.
//
// The actual error message is forwarded to the client verbatim — this is a
// developer-facing API where opaque "failed to process request" responses
// strand both human users and LLM-agent consumers. classifyWorkerError owns
// the code/status mapping; this helper routes a request_canceled outcome to
// 408 (so client-driven aborts don't inflate 5xx metrics) and everything
// else to 500 with the classified code/details.
//
// Even on the 408 cancellation tail the original err.Error() is forwarded
// (rather than a fixed "request canceled" string), so the response
// distinguishes context.Canceled vs context.DeadlineExceeded vs deeper
// pool diagnostic wraps like "could not acquire worker: context deadline
// exceeded (pool error: ...)". The 408 status + request_canceled code
// convey the semantic class; the body keeps the discriminating message
// so LLM-agent consumers and human debuggers can act on it.
//
// Stacktraces from worker panics, when present, are forwarded as
// details.stacktrace so an LLM agent receiving the error as feedback has
// the full picture. The full error is also logged via httplogger.SetError
// for non-cancellation paths.
func writeFiberWorkerError(c fiber.Ctx, err error) error {
	code, details := classifyWorkerError(err)
	if code == apierror.CodeRequestCanceled {
		return writeFiberJSONErrorWithCode(c, err.Error(), code, details, fiber.StatusRequestTimeout)
	}
	httplogger.SetError(c.Context(), err)
	return writeFiberJSONErrorWithCode(c, err.Error(), code, details, fiber.StatusInternalServerError)
}

// writeFiberParseWorkerError is the /parse-endpoint variant of
// writeFiberWorkerError: a non-retryable failure here is a parse error
// (BAML couldn't validate raw LLM output against the method schema)
// rather than an LLM call failure.
func writeFiberParseWorkerError(c fiber.Ctx, err error) error {
	code, details := classifyWorkerError(err)
	if code == apierror.CodeRequestCanceled {
		return writeFiberJSONErrorWithCode(c, err.Error(), code, details, fiber.StatusRequestTimeout)
	}
	httplogger.SetError(c.Context(), err)
	if code == apierror.CodeWorkerError {
		code = apierror.CodeParseError
	}
	return writeFiberJSONErrorWithCode(c, err.Error(), code, details, fiber.StatusInternalServerError)
}

// writeFiberInternalError surfaces a Go-side internal processing error
// (input conversion, output flattening) as 500 internal_error. The
// original message is forwarded — these errors reflect bugs in this
// server, not in BAML or the LLM, so opaqueness helps no one.
func writeFiberInternalError(c fiber.Ctx, err error) error {
	httplogger.SetError(c.Context(), err)
	return writeFiberJSONErrorWithCode(c, err.Error(), apierror.CodeInternalError, nil, fiber.StatusInternalServerError)
}

// fiberErrorHandler is the app-wide ErrorHandler. It catches
// framework-emitted errors (BodyLimit/413, route 404/405, malformed
// transport-level requests) plus any error a handler returned
// without writing a response itself, and converts them into the
// apierror JSON envelope so the wire shape is identical to what the
// chi path emits. Without this, Fiber's default ErrorHandler returns
// text/plain and strips the apierror.Code field — clients (and LLM
// agents) consuming errors then can't branch on the code at all for
// framework-level failures.
//
// Mapping:
//   - *fiber.Error 413 → CodeRequestTooLarge (parity with chi's
//     writeChiBodyReadError handling of MaxBytesReader exhaustion).
//   - Other *fiber.Error → JSON envelope with the framework-supplied
//     status and message; no apierror.Code is set because the host
//     has nothing meaningful to add (the fiber.Error message already
//     names the class — "Method Not Allowed", "Not Found", etc.).
//   - Anything else (a handler returning a non-fiber.Error without
//     writing a response): 500 internal_error.
func fiberErrorHandler(c fiber.Ctx, err error) error {
	var fiberErr *fiber.Error
	if errors.As(err, &fiberErr) {
		var code apierror.Code
		if fiberErr.Code == fiber.StatusRequestEntityTooLarge {
			code = apierror.CodeRequestTooLarge
		}
		return writeFiberJSONErrorWithCode(c, fiberErr.Message, code, nil, fiberErr.Code)
	}
	return writeFiberInternalError(c, err)
}

// classifyWorkerError inspects err and returns the apierror.Code that
// best describes the failure plus any structured details that should
// reach the client envelope. Precedence:
//  1. Worker-supplied code (carried by *workerplugin.ErrorWithStack)
//     wins — when the worker classified the error, we trust it.
//  2. pool.ErrPoolRetriesExhausted → worker_unavailable. Checked BEFORE
//     caller-cancellation because the pool's retry loop wraps the last
//     retryable err under the sentinel, and that last err is often a
//     hung-detection-driven gRPC Canceled / DeadlineExceeded — which
//     IsCallerCancellationError would otherwise misread as a client-
//     driven abort. Pool retry exhaustion is by definition infra, not
//     a client cancellation.
//  3. Caller cancellation (context.Canceled / DeadlineExceeded or
//     gRPC Canceled / DeadlineExceeded) maps to request_canceled. Uses
//     pool.IsTypedCancellationError (NOT IsCallerCancellationError) so
//     a plain worker error string like "openai: provider timeout:
//     context deadline exceeded" does NOT trigger this branch — only
//     typed signals (errors.Is, gRPC status, serialized "rpc error:
//     code = Canceled" prefix) qualify. Checked BEFORE
//     pool.IsRetryableWorkerError because the latter treats those gRPC
//     codes as retryable infrastructure too — without this short-circuit,
//     a client-driven abort surfaces as worker_unavailable on streaming
//     and unary paths alike.
//  4. pool.IsRetryableWorkerError → worker_unavailable (transient infra
//     failure, retries exhausted by the pool).
//  5. Default → worker_error (BAML / LLM application error).
//
// Details precedence: a worker-supplied JSON Details payload wins; if
// none is present, a worker-supplied stacktrace is wrapped as
// {"stacktrace": "..."} so an LLM agent receiving the error as feedback
// (or a human debugging) sees the panic trace alongside the message.
// Stacktraces are also independently logged via httplogger.SetError —
// the response copy is the explicit, agent-facing channel.
func classifyWorkerError(err error) (apierror.Code, json.RawMessage) {
	var workerCode apierror.Code
	var details json.RawMessage

	var stackErr *workerplugin.ErrorWithStack
	if errors.As(err, &stackErr) {
		workerCode, details = normalizeWorkerMetadata(stackErr.GetCode(), stackErr.GetDetails())
		if details == nil {
			if st := stackErr.GetStacktrace(); st != "" {
				details = stacktraceDetailsJSON(st)
			}
		}
	}

	if workerCode != "" {
		return workerCode, details
	}

	if errors.Is(err, pool.ErrPoolRetriesExhausted) {
		return apierror.CodeWorkerUnavailable, details
	}

	if pool.IsTypedCancellationError(err) {
		return apierror.CodeRequestCanceled, details
	}

	if pool.IsRetryableWorkerError(err) {
		return apierror.CodeWorkerUnavailable, details
	}
	return apierror.CodeWorkerError, details
}

// normalizeWorkerMetadata accepts the raw worker-supplied code +
// details bytes and returns the contract-compliant pair to put on the
// response envelope. The host treats both fields as untrusted because
// they're authored worker-side and there's no shared schema enforcing
// them, so off-contract values are dropped rather than forwarded:
//
//   - code is preserved only if it's worker-facing
//     (apierror.Code.IsWorkerFacing): worker_error, parse_error, or
//     internal_error. Request-layer codes (invalid_json,
//     request_canceled, etc.) and pool-admission codes
//     (worker_unavailable) are owned by the host — honoring a worker
//     that claimed e.g. request_canceled would let worker-side text
//     force the host's 408 classifier branch and dictate HTTP status
//     semantics. Non-worker-facing codes fall through to host-side
//     classification, which is the authoritative source for those
//     classes.
//   - details is preserved only if it parses as a JSON OBJECT; scalars,
//     arrays, and null are dropped. The OpenAPI schema declares
//     details as an object (additionalProperties), so anything else
//     would lie about the contract and confuse generated clients.
func normalizeWorkerMetadata(rawCode string, rawDetails []byte) (apierror.Code, json.RawMessage) {
	var code apierror.Code
	if c := apierror.Code(rawCode); c.IsWorkerFacing() {
		code = c
	}
	var details json.RawMessage
	if isJSONObject(rawDetails) {
		details = json.RawMessage(rawDetails)
	}
	return code, details
}

// isJSONObject reports whether b is a syntactically valid JSON
// document AND its top-level value is an object (`{...}`). Used to
// gate worker-supplied details against the OpenAPI contract.
func isJSONObject(b []byte) bool {
	trimmed := bytes.TrimLeft(b, " \t\n\r")
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return false
	}
	return json.Valid(trimmed)
}

// stacktraceDetailsJSON marshals st into the canonical
// {"stacktrace": "..."} envelope used by the response Details field.
// Falls back to nil on the (effectively impossible) marshal failure so
// callers don't ship a half-formed payload.
func stacktraceDetailsJSON(st string) json.RawMessage {
	payload, err := json.Marshal(struct {
		Stacktrace string `json:"stacktrace"`
	}{Stacktrace: st})
	if err != nil {
		return nil
	}
	return payload
}

// netHTTPHeaderApiError is the chi/net-http analogue of
// writeFiberJSONErrorWithCode. Lives in this file rather than
// unary_handlers.go so the chi handlers and the Fiber handlers share
// one definition of the error envelope and the worker-error classifier.
func writeChiJSONErrorWithCode(w http.ResponseWriter, r *http.Request, message string, code apierror.Code, details json.RawMessage, statusCode int) {
	apierror.WriteJSONWithCode(w, message, code, details, statusCode, unaryRequestID(r.Context()))
}

func writeChiWorkerErrorClassified(w http.ResponseWriter, r *http.Request, err error) {
	code, details := classifyWorkerError(err)
	if code == apierror.CodeRequestCanceled {
		writeChiJSONErrorWithCode(w, r, err.Error(), code, details, http.StatusRequestTimeout)
		return
	}
	httplogger.SetError(r.Context(), err)
	writeChiJSONErrorWithCode(w, r, err.Error(), code, details, http.StatusInternalServerError)
}

func writeChiParseWorkerError(w http.ResponseWriter, r *http.Request, err error) {
	code, details := classifyWorkerError(err)
	if code == apierror.CodeRequestCanceled {
		writeChiJSONErrorWithCode(w, r, err.Error(), code, details, http.StatusRequestTimeout)
		return
	}
	httplogger.SetError(r.Context(), err)
	if code == apierror.CodeWorkerError {
		code = apierror.CodeParseError
	}
	writeChiJSONErrorWithCode(w, r, err.Error(), code, details, http.StatusInternalServerError)
}

func writeChiInternalError(w http.ResponseWriter, r *http.Request, err error) {
	httplogger.SetError(r.Context(), err)
	writeChiJSONErrorWithCode(w, r, err.Error(), apierror.CodeInternalError, nil, http.StatusInternalServerError)
}
