//go:build unaryserver

package main

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/invakid404/baml-rest/bamlutils"
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
			reqID := middleware.GetReqID(req.Context())
			if reqID != "" {
				w.Header().Set("X-Request-Id", reqID)
			}
			// Bridge chi's middleware request ID into the untagged
			// handler-side context key so writeChiJSONError (which
			// lives in unary_handlers.go and must compile without a
			// chi dep) can populate the JSON error envelope's
			// request_id field.
			ctx := context.WithValue(req.Context(), unaryRequestIDKey{}, reqID)
			next.ServeHTTP(w, req.WithContext(ctx))
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
