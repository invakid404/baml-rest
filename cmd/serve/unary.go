//go:build unaryserver

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/goccy/go-json"
	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/internal/apierror"
	"github.com/invakid404/baml-rest/internal/httplogger"
	"github.com/invakid404/baml-rest/pool"
	"github.com/rs/zerolog"
)

const defaultUnaryPort = 8081

func init() {
	serveCmd.Flags().IntVar(&unaryPort, "unary-port", defaultUnaryPort, "Port for the unary server (0 = disabled). Serves /call/*, /call-with-raw/*, /parse/* on a net/http server with reliable client-disconnect cancellation")
}

// newUnaryServer creates the chi-based unary HTTP server if unaryPort > 0.
// Returns nil when the unary server is disabled.
func newUnaryServer(logger zerolog.Logger, workerPool *pool.Pool, methodNames []string, hasDynamicMethod bool) *http.Server {
	if unaryPort <= 0 {
		return nil
	}
	return &http.Server{
		Addr:              fmt.Sprintf(":%d", unaryPort),
		Handler:           newUnaryRouter(logger, workerPool, methodNames, hasDynamicMethod),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      5 * time.Minute,
		IdleTimeout:       120 * time.Second,
	}
}

// newUnaryRouter builds a chi router that registers only unary endpoints
// (/call/*, /call-with-raw/*, /parse/*) against the shared worker pool.
// These handlers use r.Context() for cancellation, giving true client-disconnect
// detection on a real net/http server.
func newUnaryRouter(
	logger zerolog.Logger,
	workerPool *pool.Pool,
	methodNames []string,
	hasDynamicMethod bool,
) http.Handler {
	r := chi.NewRouter()

	// Middleware stack — mirrors the Fiber stack where it matters.
	r.Use(chiMetricsMiddleware)
	r.Use(middleware.RequestID)
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if reqID := middleware.GetReqID(req.Context()); reqID != "" {
				w.Header().Set("X-Request-Id", reqID)
			}
			next.ServeHTTP(w, req)
		})
	})
	r.Use(httplogger.RequestLogger(logger, &httplogger.Options{
		RecoverPanics:    true,
		RequestIDFromCtx: middleware.GetReqID,
	}))

	// Body size limiter.
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			req.Body = http.MaxBytesReader(w, req.Body, maxRequestBodyBytes)
			next.ServeHTTP(w, req)
		})
	})

	// Register static (schema-defined) endpoints.
	for _, name := range methodNames {
		if name == bamlutils.DynamicEndpointName {
			continue
		}

		r.Post(fmt.Sprintf("/call/%s", name), makeChiCallHandler(workerPool, name, bamlutils.StreamModeCall))
		r.Post(fmt.Sprintf("/call-with-raw/%s", name), makeChiCallHandler(workerPool, name, bamlutils.StreamModeCallWithRaw))
		r.Post(fmt.Sprintf("/parse/%s", name), makeChiParseHandler(workerPool, name))
	}

	// Register dynamic endpoints.
	if hasDynamicMethod {
		r.Post(fmt.Sprintf("/call/%s", bamlutils.DynamicEndpointName), makeChiDynamicCallHandler(workerPool, bamlutils.StreamModeCall))
		r.Post(fmt.Sprintf("/call-with-raw/%s", bamlutils.DynamicEndpointName), makeChiDynamicCallHandler(workerPool, bamlutils.StreamModeCallWithRaw))
		r.Post(fmt.Sprintf("/parse/%s", bamlutils.DynamicEndpointName), makeChiDynamicParseHandler(workerPool))
	}

	return r
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
			writeChiJSONError(w, r, "failed to read request body", statusCode)
			return
		}

		result, err := p.Call(ctx, methodName, body, streamMode)
		if err != nil {
			writeChiWorkerError(w, r, err, "failed to process request")
			return
		}

		w.Header().Set("Content-Type", "application/json")
		if streamMode.NeedsRaw() {
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(CallWithRawResponse{
				Data: result.Data,
				Raw:  result.Raw,
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
			writeChiJSONError(w, r, "failed to read request body", statusCode)
			return
		}

		result, err := p.Parse(ctx, methodName, body)
		if err != nil {
			writeChiWorkerError(w, r, err, "failed to parse response")
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(result.Data)
	}
}

func makeChiDynamicCallHandler(p *pool.Pool, streamMode bamlutils.StreamMode) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()

		body, statusCode, err := readUnaryBody(r)
		if err != nil {
			writeChiJSONError(w, r, "failed to read request body", statusCode)
			return
		}

		var input bamlutils.DynamicInput
		if err := json.Unmarshal(body, &input); err != nil {
			writeChiJSONError(w, r, "invalid JSON payload", http.StatusBadRequest)
			return
		}
		if err := input.Validate(); err != nil {
			writeChiJSONError(w, r, err.Error(), http.StatusBadRequest)
			return
		}

		workerInput, err := input.ToWorkerInput()
		if err != nil {
			httplogger.SetError(r.Context(), err)
			writeChiJSONError(w, r, "failed to process dynamic input", http.StatusInternalServerError)
			return
		}

		result, err := p.Call(ctx, bamlutils.DynamicMethodName, workerInput, streamMode)
		if err != nil {
			writeChiWorkerError(w, r, err, "failed to process request")
			return
		}

		flattenedData, err := bamlutils.FlattenDynamicOutput(result.Data)
		if err != nil {
			httplogger.SetError(r.Context(), err)
			writeChiJSONError(w, r, "failed to process response", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		if streamMode.NeedsRaw() {
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(CallWithRawResponse{
				Data: flattenedData,
				Raw:  result.Raw,
			})
			return
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(flattenedData)
	}
}

func makeChiDynamicParseHandler(p *pool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()

		body, statusCode, err := readUnaryBody(r)
		if err != nil {
			writeChiJSONError(w, r, "failed to read request body", statusCode)
			return
		}

		var input bamlutils.DynamicParseInput
		if err := json.Unmarshal(body, &input); err != nil {
			writeChiJSONError(w, r, "invalid JSON payload", http.StatusBadRequest)
			return
		}
		if err := input.Validate(); err != nil {
			writeChiJSONError(w, r, err.Error(), http.StatusBadRequest)
			return
		}

		workerInput, err := input.ToWorkerInput()
		if err != nil {
			httplogger.SetError(r.Context(), err)
			writeChiJSONError(w, r, "failed to process dynamic input", http.StatusInternalServerError)
			return
		}

		result, err := p.Parse(ctx, bamlutils.DynamicMethodName, workerInput)
		if err != nil {
			writeChiWorkerError(w, r, err, "failed to parse response")
			return
		}

		flattenedData, err := bamlutils.FlattenDynamicOutput(result.Data)
		if err != nil {
			httplogger.SetError(r.Context(), err)
			writeChiJSONError(w, r, "failed to process response", http.StatusInternalServerError)
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

// writeChiJSONError writes a JSON error response using the standard apierror envelope.
func writeChiJSONError(w http.ResponseWriter, r *http.Request, message string, statusCode int) {
	apierror.WriteJSON(w, message, statusCode, middleware.GetReqID(r.Context()))
}

// writeChiWorkerError classifies worker errors: context cancellation is reported
// as 408 (not 500) so it doesn't inflate error metrics.
func writeChiWorkerError(w http.ResponseWriter, r *http.Request, err error, fallbackMessage string) {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		writeChiJSONError(w, r, "request canceled", http.StatusRequestTimeout)
		return
	}
	httplogger.SetError(r.Context(), err)
	writeChiJSONError(w, r, fallbackMessage, http.StatusInternalServerError)
}

// chiMetricsMiddleware records the same HTTP metrics as the Fiber middleware
// (http_requests_total, http_request_duration_seconds, http_requests_inflight)
// using the shared prometheus collectors.
func chiMetricsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpRequestsInflight.Inc()
		defer httpRequestsInflight.Dec()
		start := time.Now()

		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)

		rctx := chi.RouteContext(r.Context())
		path := rctx.RoutePattern()
		if path == "" {
			path = "_unmatched"
		}

		status := ww.Status()
		if status == 0 {
			status = http.StatusOK // net/http implicit default
		}
		code := strconv.Itoa(status)
		httpRequestsTotal.WithLabelValues(r.Method, path, code).Inc()
		httpRequestDuration.WithLabelValues(r.Method, path, code).Observe(time.Since(start).Seconds())
	})
}
