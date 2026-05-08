package main

import (
	"context"
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
// and details=nil to omit those fields.
func writeFiberJSONErrorWithCode(c fiber.Ctx, message string, code apierror.Code, details json.RawMessage, statusCode int) error {
	return c.Status(statusCode).JSON(apierror.Response{
		Error:     message,
		Code:      code,
		Details:   details,
		RequestID: fiberrequestid.FromContext(c),
	})
}

// writeFiberWorkerError classifies and writes a worker-originated error.
//
// The actual error message is forwarded to the client verbatim — this is a
// developer-facing API where opaque "failed to process request" responses
// strand both human users and LLM-agent consumers. The classification logic:
//
//   - context.Canceled / DeadlineExceeded → 408 request_canceled
//   - retryable infrastructure failure that exhausted pool retries
//     (worker crash, gRPC Unavailable, transport reset) → 500 worker_unavailable
//   - everything else (BAML validation, LLM provider error, parse failure
//     bubbled from the worker) → 500 worker_error
//
// Stacktraces from worker panics, when present, are forwarded as
// details.stacktrace so an LLM agent receiving the error as feedback has
// the full picture. The full error is also logged via httplogger.SetError.
func writeFiberWorkerError(c fiber.Ctx, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return writeFiberJSONErrorWithCode(c, "request canceled", apierror.CodeRequestCanceled, nil, fiber.StatusRequestTimeout)
	}
	httplogger.SetError(c.Context(), err)
	code, details := classifyWorkerError(err)
	return writeFiberJSONErrorWithCode(c, err.Error(), code, details, fiber.StatusInternalServerError)
}

// writeFiberParseWorkerError is the /parse-endpoint variant of
// writeFiberWorkerError: a non-retryable failure here is a parse error
// (BAML couldn't validate raw LLM output against the method schema)
// rather than an LLM call failure.
func writeFiberParseWorkerError(c fiber.Ctx, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return writeFiberJSONErrorWithCode(c, "request canceled", apierror.CodeRequestCanceled, nil, fiber.StatusRequestTimeout)
	}
	httplogger.SetError(c.Context(), err)
	code, details := classifyWorkerError(err)
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

// classifyWorkerError inspects err and returns the apierror.Code that
// best describes the failure plus any structured details that should
// reach the client envelope. Worker-supplied codes (carried by
// *workerplugin.ErrorWithStack) take precedence; otherwise the
// classification falls back to pool.IsRetryableWorkerError to distinguish
// transient infrastructure failure from a real BAML/LLM error.
func classifyWorkerError(err error) (apierror.Code, json.RawMessage) {
	var details json.RawMessage

	// Worker-supplied details (BAML diagnostic JSON, parse failure
	// fields) — forward verbatim if they form a valid JSON value.
	var stackErr *workerplugin.ErrorWithStack
	if errors.As(err, &stackErr) {
		if d := stackErr.GetDetails(); len(d) > 0 && json.Valid(d) {
			details = json.RawMessage(d)
		}
	}

	// Worker-supplied code wins over heuristic classification — when
	// the worker has typed knowledge of the failure (e.g. "this is a
	// parse_error against the BAML schema"), we trust it.
	if stackErr != nil && stackErr.GetCode() != "" {
		return apierror.Code(stackErr.GetCode()), details
	}

	if pool.IsRetryableWorkerError(err) {
		return apierror.CodeWorkerUnavailable, details
	}
	return apierror.CodeWorkerError, details
}

// netHTTPHeaderApiError is the chi/net-http analogue of
// writeFiberJSONErrorWithCode. Lives in this file rather than
// unary_handlers.go so the chi handlers and the Fiber handlers share
// one definition of the error envelope and the worker-error classifier.
func writeChiJSONErrorWithCode(w http.ResponseWriter, r *http.Request, message string, code apierror.Code, details json.RawMessage, statusCode int) {
	apierror.WriteJSONWithCode(w, message, code, details, statusCode, unaryRequestID(r.Context()))
}

func writeChiWorkerErrorClassified(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		writeChiJSONErrorWithCode(w, r, "request canceled", apierror.CodeRequestCanceled, nil, http.StatusRequestTimeout)
		return
	}
	httplogger.SetError(r.Context(), err)
	code, details := classifyWorkerError(err)
	writeChiJSONErrorWithCode(w, r, err.Error(), code, details, http.StatusInternalServerError)
}

func writeChiParseWorkerError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		writeChiJSONErrorWithCode(w, r, "request canceled", apierror.CodeRequestCanceled, nil, http.StatusRequestTimeout)
		return
	}
	httplogger.SetError(r.Context(), err)
	code, details := classifyWorkerError(err)
	if code == apierror.CodeWorkerError {
		code = apierror.CodeParseError
	}
	writeChiJSONErrorWithCode(w, r, err.Error(), code, details, http.StatusInternalServerError)
}

func writeChiInternalError(w http.ResponseWriter, r *http.Request, err error) {
	httplogger.SetError(r.Context(), err)
	writeChiJSONErrorWithCode(w, r, err.Error(), apierror.CodeInternalError, nil, http.StatusInternalServerError)
}
