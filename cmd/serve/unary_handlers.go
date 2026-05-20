package main

import (
	"context"
	"errors"
	"io"
	"net/http"

	"github.com/bytedance/sonic"
	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/internal/apierror"
	"github.com/invakid404/baml-rest/pool"
	"github.com/invakid404/baml-rest/workerplugin"
)

// unaryCaller is the subset of *pool.Pool the unary chi handlers depend
// on. Declared as an interface so tests can construct mocks without
// spinning up a real worker pool. *pool.Pool satisfies it via Pool.Call
// (signature matches by structural typing). Production routing in
// newUnaryRouter passes the pool directly.
type unaryCaller interface {
	Call(ctx context.Context, methodName string, inputJSON []byte, streamMode bamlutils.StreamMode) (*workerplugin.CallResult, error)
}

// unaryParser is the parse-side analog of unaryCaller for the dynamic
// /parse/_dynamic chi handler. Declared so tests can record the worker
// input the handler routed without depending on *pool.Pool; *pool.Pool
// satisfies the interface structurally via Pool.Parse.
type unaryParser interface {
	Parse(ctx context.Context, methodName string, inputJSON []byte) (*workerplugin.ParseResult, error)
}

// chiHeadersEmitter is the function-typed seam the chi dynamic call
// handler uses to publish BAML observability headers from a CallResult's
// planned/outcome JSON. Extracted so tests can substitute a counting
// wrapper and verify call frequency (e.g., exactly once on success,
// once on error tail, zero when no result was produced) — assertions
// the previous Header().Values()-based test could not actually make,
// because http.Header.Set silently overwrites on repeat calls.
type chiHeadersEmitter func(w http.ResponseWriter, planned, outcome []byte)

// defaultChiHeadersEmitter is the production header emitter: decodes
// the JSON-encoded planned/outcome payloads and forwards them to
// setBAMLHeaders via netHTTPHeaderSetter. Tests inject their own
// emitter into makeChiDynamicCallHandler instead of swapping this var
// (avoids the global-mutation race that makes parallel tests unsafe).
func defaultChiHeadersEmitter(w http.ResponseWriter, planned, outcome []byte) {
	setBAMLHeaders(netHTTPHeaderSetter(w.Header()), decodeMetadataJSON(planned), decodeMetadataJSON(outcome))
}

// unaryRequestIDKey carries the per-request ID populated by the chi
// router's middleware bridge (see newUnaryRouter). Decouples the
// untagged JSON-error helper below from chi/middleware.GetReqID so the
// handler factories compile in default builds without a chi dep.
type unaryRequestIDKey struct{}

func unaryRequestID(ctx context.Context) string {
	v, _ := ctx.Value(unaryRequestIDKey{}).(string)
	return v
}

// ---------------------------------------------------------------------------
// chi handler factories
// ---------------------------------------------------------------------------

func makeChiCallHandler(p *pool.Pool, methodName string, streamMode bamlutils.StreamMode) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()

		body, statusCode, err := readUnaryBody(r)
		if err != nil {
			writeChiBodyReadError(w, r, err, statusCode)
			return
		}

		result, err := p.Call(ctx, methodName, body, streamMode)
		// Surface routing observability headers even on the error path
		// (Pool.Call preserves accumulated planned/outcome metadata in
		// the result for error tails). See cmd/serve/main.go for the
		// full rationale.
		if result != nil {
			setBAMLHeaders(netHTTPHeaderSetter(w.Header()), decodeMetadataJSON(result.Planned), decodeMetadataJSON(result.Outcome))
		}
		if err != nil {
			writeChiWorkerErrorClassified(w, r, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if streamMode.NeedsRaw() {
			w.WriteHeader(http.StatusOK)
			_ = sonic.ConfigDefault.NewEncoder(w).Encode(CallWithRawResponse{
				Data:      result.Data,
				Raw:       result.Raw,
				Reasoning: result.Reasoning,
			})
			return
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(result.Data)
	}
}

func makeChiParseHandler(p *pool.Pool, methodName string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()

		body, statusCode, err := readUnaryBody(r)
		if err != nil {
			writeChiBodyReadError(w, r, err, statusCode)
			return
		}

		result, err := p.Parse(ctx, methodName, body)
		if err != nil {
			writeChiParseWorkerError(w, r, err)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(result.Data)
	}
}

func makeChiDynamicCallHandler(p unaryCaller, streamMode bamlutils.StreamMode, preserveSchemaOrderDefault bool) http.HandlerFunc {
	return makeChiDynamicCallHandlerWithEmitter(p, streamMode, preserveSchemaOrderDefault, defaultChiHeadersEmitter)
}

// makeChiDynamicCallHandlerWithEmitter is the test-injectable form of
// makeChiDynamicCallHandler. Production callers go through the
// default-emitter wrapper above; tests pass a counting wrapper so they
// can assert how many times the headers seam was invoked.
//
// preserveSchemaOrderDefault is the server-level fallback for the
// dynamic preserve_schema_order opt-in. It is applied via
// applyPreserveSchemaOrderDefault after Unmarshal and before Validate
// so per-request true/false always wins over the server default.
func makeChiDynamicCallHandlerWithEmitter(p unaryCaller, streamMode bamlutils.StreamMode, preserveSchemaOrderDefault bool, emit chiHeadersEmitter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()

		body, statusCode, err := readUnaryBody(r)
		if err != nil {
			writeChiBodyReadError(w, r, err, statusCode)
			return
		}

		var input bamlutils.DynamicInput
		if err := sonic.Unmarshal(body, &input); err != nil {
			writeChiJSONErrorWithCode(w, r, err.Error(), apierror.CodeInvalidJSON, nil, http.StatusBadRequest)
			return
		}
		applyPreserveSchemaOrderDefault(&input, preserveSchemaOrderDefault)
		if err := input.Validate(); err != nil {
			writeChiJSONErrorWithCode(w, r, err.Error(), apierror.CodeInvalidRequest, nil, http.StatusBadRequest)
			return
		}

		workerInput, err := input.ToWorkerInput()
		if err != nil {
			writeChiInternalError(w, r, err)
			return
		}

		result, err := p.Call(ctx, bamlutils.DynamicMethodName, workerInput, streamMode)
		// Surface routing observability headers even on the error path
		// — Pool.Call preserves accumulated planned/outcome metadata in
		// the result for error tails. Mirrors the static chi handler
		// above and the fiber dynamic handler in cmd/serve/main.go.
		if result != nil {
			emit(w, result.Planned, result.Outcome)
		}
		if err != nil {
			writeChiWorkerErrorClassified(w, r, err)
			return
		}

		flattenedData, err := bamlutils.FlattenDynamicOutput(result.Data)
		if err != nil {
			writeChiInternalError(w, r, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if streamMode.NeedsRaw() {
			w.WriteHeader(http.StatusOK)
			_ = sonic.ConfigDefault.NewEncoder(w).Encode(CallWithRawResponse{
				Data:      flattenedData,
				Raw:       result.Raw,
				Reasoning: result.Reasoning,
			})
			return
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(flattenedData)
	}
}

func makeChiDynamicParseHandler(p unaryParser, preserveSchemaOrderDefault bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()

		body, statusCode, err := readUnaryBody(r)
		if err != nil {
			writeChiBodyReadError(w, r, err, statusCode)
			return
		}

		var input bamlutils.DynamicParseInput
		if err := sonic.Unmarshal(body, &input); err != nil {
			writeChiJSONErrorWithCode(w, r, err.Error(), apierror.CodeInvalidJSON, nil, http.StatusBadRequest)
			return
		}
		applyParsePreserveSchemaOrderDefault(&input, preserveSchemaOrderDefault)
		if err := input.Validate(); err != nil {
			writeChiJSONErrorWithCode(w, r, err.Error(), apierror.CodeInvalidRequest, nil, http.StatusBadRequest)
			return
		}

		workerInput, err := input.ToWorkerInput()
		if err != nil {
			writeChiInternalError(w, r, err)
			return
		}

		result, err := p.Parse(ctx, bamlutils.DynamicMethodName, workerInput)
		if err != nil {
			writeChiParseWorkerError(w, r, err)
			return
		}

		flattenedData, err := bamlutils.FlattenDynamicOutput(result.Data)
		if err != nil {
			writeChiInternalError(w, r, err)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(flattenedData)
	}
}

// readUnaryBody reads the request body and returns the appropriate HTTP status
// code on failure. MaxBytesReader errors yield 413; other read errors yield 400.
func readUnaryBody(r *http.Request) ([]byte, int, error) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return nil, http.StatusRequestEntityTooLarge, err
		}
		return nil, http.StatusBadRequest, err
	}
	return body, 0, nil
}

// writeChiBodyReadError classifies a body-read failure: 413
// request_too_large for MaxBytesReader exhaustion (statusCode 413
// produced by readUnaryBody), other I/O failures as 400 body_read_error.
func writeChiBodyReadError(w http.ResponseWriter, r *http.Request, err error, statusCode int) {
	code := apierror.CodeBodyReadError
	if statusCode == http.StatusRequestEntityTooLarge {
		code = apierror.CodeRequestTooLarge
	}
	writeChiJSONErrorWithCode(w, r, err.Error(), code, nil, statusCode)
}
