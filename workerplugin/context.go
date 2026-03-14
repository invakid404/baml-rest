package workerplugin

import "context"

type callerContextKey struct{}

// WithCallerContext annotates an attempt-scoped context with the original
// caller context so downstream code can distinguish true caller cancellation
// from internal per-attempt cancellation.
func WithCallerContext(ctx context.Context, caller context.Context) context.Context {
	if caller == nil {
		return ctx
	}
	return context.WithValue(ctx, callerContextKey{}, caller)
}

func callerContext(ctx context.Context) context.Context {
	if caller, ok := ctx.Value(callerContextKey{}).(context.Context); ok && caller != nil {
		return caller
	}
	return ctx
}
