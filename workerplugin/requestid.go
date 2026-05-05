package workerplugin

import "context"

// requestIDKey is the context key for the stable per-request identifier
// the pool threads into every CallStream dispatch. Unexported to force
// go through WithRequestID / RequestIDFromContext.
type requestIDKey struct{}

// WithRequestID attaches a request id to ctx. The worker plugin's gRPC
// client reads the value from the context passed to CallStream and
// forwards it as CallRequest.request_id; the worker-side server stashes
// the incoming request_id back onto the handler's context via the same
// key, so downstream BAML dispatch can pull it out through
// RequestIDFromContext.
//
// Separated from the gRPC types so callers outside workerplugin (pool,
// cmd/worker) can reference the key without importing proto symbols.
func WithRequestID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	// context.WithValue(nil, ...) panics; tests and standalone harnesses
	// sometimes pass a bare nil ctx. Background is the neutral parent —
	// no deadline, no values, no cancellation — which is exactly what a
	// nil-ctx caller was signalling.
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, requestIDKey{}, id)
}

// RequestIDFromContext returns the request id carried on ctx, or the
// empty string if none was set. Empty is a legal value — it signals to
// the host's SharedState store that idempotency caching should be
// skipped for this call, which is the correct fallback for test harnesses
// that don't assign request ids.
func RequestIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v, _ := ctx.Value(requestIDKey{}).(string)
	return v
}
