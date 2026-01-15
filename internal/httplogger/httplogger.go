package httplogger

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/gregwebs/go-recovery"
	"github.com/rs/zerolog"
)

// Options configures the HTTP request logger middleware
type Options struct {
	// RecoverPanics enables panic recovery in the middleware.
	// Panics are logged as errors with stack traces and return HTTP 500.
	RecoverPanics bool

	// Skip is an optional predicate to skip logging for certain requests.
	// If nil, all requests are logged.
	Skip func(r *http.Request) bool

	// LogRequestHeaders specifies which headers to include in logs.
	// If nil, no headers are logged.
	LogRequestHeaders []string
}

// responseWriter wraps http.ResponseWriter to capture the status code
type responseWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func wrapResponseWriter(w http.ResponseWriter) *responseWriter {
	return &responseWriter{ResponseWriter: w, status: http.StatusOK}
}

func (rw *responseWriter) WriteHeader(code int) {
	if !rw.wroteHeader {
		rw.status = code
		rw.wroteHeader = true
		rw.ResponseWriter.WriteHeader(code)
	}
}

func (rw *responseWriter) Write(b []byte) (int, error) {
	if !rw.wroteHeader {
		rw.wroteHeader = true
	}
	return rw.ResponseWriter.Write(b)
}

func (rw *responseWriter) Unwrap() http.ResponseWriter {
	return rw.ResponseWriter
}

// contextKey is used to store the request logger in context
type contextKey struct{}

// RequestLogger creates HTTP request logging middleware using zerolog
func RequestLogger(logger zerolog.Logger, opts *Options) func(http.Handler) http.Handler {
	if opts == nil {
		opts = &Options{}
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Check if we should skip logging
			if opts.Skip != nil && opts.Skip(r) {
				next.ServeHTTP(w, r)
				return
			}

			start := time.Now()
			wrapped := wrapResponseWriter(w)

			// Create request-scoped logger with request ID
			reqLogger := logger.With().Logger()
			if reqID := middleware.GetReqID(r.Context()); reqID != "" {
				reqLogger = reqLogger.With().Str("request_id", reqID).Logger()
			}

			// Store logger in context for handlers to use
			ctx := context.WithValue(r.Context(), contextKey{}, &reqLogger)
			r = r.WithContext(ctx)

			var panicErr error
			if opts.RecoverPanics {
				panicErr = recovery.Call(func() error {
					next.ServeHTTP(wrapped, r)
					return nil
				})

				if panicErr != nil {
					// ErrAbortHandler is a sentinel panic used by http.TimeoutHandler
					// and similar middleware - it should propagate, not be recovered
					if errors.Is(panicErr, http.ErrAbortHandler) {
						panic(http.ErrAbortHandler)
					}

					// Log the panic with stack trace using %+v format
					reqLogger.Error().
						Str("method", r.Method).
						Str("path", r.URL.Path).
						Str("stack", fmt.Sprintf("%+v", panicErr)).
						Msg("Panic recovered")

					// Return 500 if no status was written
					if !wrapped.wroteHeader {
						http.Error(wrapped, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
					}
				}
			} else {
				next.ServeHTTP(wrapped, r)
			}

			duration := time.Since(start)

			// Determine log level based on status code
			var event *zerolog.Event
			switch {
			case wrapped.status >= 500:
				event = reqLogger.Error()
			case wrapped.status >= 400:
				event = reqLogger.Warn()
			default:
				event = reqLogger.Info()
			}

			event.
				Str("method", r.Method).
				Str("path", r.URL.Path).
				Int("status", wrapped.status).
				Dur("duration", duration)

			// Add request headers if configured
			for _, header := range opts.LogRequestHeaders {
				if val := r.Header.Get(header); val != "" {
					event.Str("header_"+header, val)
				}
			}

			event.Msg("Request completed")
		})
	}
}

// LogEntry retrieves the request-scoped logger from context
func LogEntry(ctx context.Context) *zerolog.Logger {
	if logger, ok := ctx.Value(contextKey{}).(*zerolog.Logger); ok {
		return logger
	}
	return nil
}

// stacktraceGetter is implemented by errors that carry a stacktrace
type stacktraceGetter interface {
	GetStacktrace() string
}

// SetError logs an error on the request-scoped logger.
// If the error implements stacktraceGetter, the stacktrace is also logged.
func SetError(ctx context.Context, err error) {
	if logger := LogEntry(ctx); logger != nil {
		logger.UpdateContext(func(c zerolog.Context) zerolog.Context {
			c = c.Err(err)
			// Check if error carries a stacktrace (e.g., from worker panics)
			if stErr, ok := err.(stacktraceGetter); ok {
				if st := stErr.GetStacktrace(); st != "" {
					c = c.Str("stack", st)
				}
			}
			return c
		})
	}
}
