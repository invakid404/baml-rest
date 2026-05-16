//go:build inprocess

package pool

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"runtime/debug"

	"github.com/gregwebs/go-recovery"
	"github.com/rs/zerolog"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/workerplugin"
)

// panicErrorCode is the worker-facing classification attached to errors
// synthesised from a recovered Go panic. Kept as a local constant rather
// than imported from internal/apierror so the pool module does not gain
// a dependency on the root module just for one string.
const panicErrorCode = "internal_error"

// recoveringWorker wraps a workerplugin.Worker and turns Go panics in
// the inner worker's methods into structured error returns. It exists
// only in inprocess builds; subprocess builds run the worker out of
// process where panics already terminate the child cleanly and the
// pool restart loop handles recovery.
//
// CallStream's recovery covers the synchronous call only — panics in
// the bridge goroutine that produces stream frames are caught by
// internal/worker.recoverBridgePanic inside that goroutine.
type recoveringWorker struct {
	inner  workerplugin.Worker
	logger zerolog.Logger
}

var _ workerplugin.Worker = (*recoveringWorker)(nil)

func newRecoveringWorker(inner workerplugin.Worker, logger zerolog.Logger) workerplugin.Worker {
	return &recoveringWorker{inner: inner, logger: logger}
}

// logPanic records a recovered panic at error level with the method
// name, the panic value, a short stack hash for grouping, and the full
// stack. The stack hash lets operators bucket repeated panics in log
// aggregators without storing or hashing the full trace at query time.
func (r *recoveringWorker) logPanic(method string, panicErr error, stack []byte) {
	sum := sha256.Sum256(stack)
	hash := hex.EncodeToString(sum[:6])
	r.logger.Error().
		Str("method", method).
		Str("panic_value", panicErr.Error()).
		Str("stack_hash", hash).
		Str("stack", string(stack)).
		Msg("in-process worker panic recovered")
}

// panicError builds the structured error returned to callers when a
// recovered panic interrupts a worker method. The error text embeds
// the method name so HTTP responses and logs identify the failure
// site without inspecting the stacktrace; the panic value follows so
// the original message stays visible.
func (r *recoveringWorker) panicError(method string, panicErr error, stack []byte) error {
	err := fmt.Errorf("in-process worker panic in %s: %w", method, panicErr)
	return workerplugin.NewErrorWithMetadata(err, string(stack), panicErrorCode, nil)
}

func (r *recoveringWorker) CallStream(ctx context.Context, methodName string, inputJSON []byte, streamMode bamlutils.StreamMode) (<-chan *workerplugin.StreamResult, error) {
	ch, err := recovery.Call1(func() (<-chan *workerplugin.StreamResult, error) {
		return r.inner.CallStream(ctx, methodName, inputJSON, streamMode)
	})
	if err == nil {
		return ch, nil
	}
	stack := debug.Stack()
	r.logPanic("CallStream", err, stack)
	// Surface the panic as a normal terminal error frame rather than a
	// method error so callers' streaming loops finish naturally and
	// retry policies that treat a method error as a transport fault
	// do not retry a deterministic in-process crash.
	out := make(chan *workerplugin.StreamResult, 1)
	frame := workerplugin.GetStreamResult()
	frame.Kind = workerplugin.StreamResultKindError
	frame.Error = fmt.Errorf("in-process worker panic in CallStream: %w", err)
	frame.Stacktrace = string(stack)
	frame.ErrorCode = panicErrorCode
	out <- frame
	close(out)
	return out, nil
}

func (r *recoveringWorker) Health(ctx context.Context) (bool, error) {
	ok, err := recovery.Call1(func() (bool, error) {
		return r.inner.Health(ctx)
	})
	if err == nil {
		return ok, nil
	}
	stack := debug.Stack()
	r.logPanic("Health", err, stack)
	return false, r.panicError("Health", err, stack)
}

func (r *recoveringWorker) GetMetrics(ctx context.Context) ([][]byte, error) {
	res, err := recovery.Call1(func() ([][]byte, error) {
		return r.inner.GetMetrics(ctx)
	})
	if err == nil {
		return res, nil
	}
	stack := debug.Stack()
	r.logPanic("GetMetrics", err, stack)
	return nil, r.panicError("GetMetrics", err, stack)
}

func (r *recoveringWorker) TriggerGC(ctx context.Context) (*workerplugin.GCResult, error) {
	res, err := recovery.Call1(func() (*workerplugin.GCResult, error) {
		return r.inner.TriggerGC(ctx)
	})
	if err == nil {
		return res, nil
	}
	stack := debug.Stack()
	r.logPanic("TriggerGC", err, stack)
	return nil, r.panicError("TriggerGC", err, stack)
}

func (r *recoveringWorker) Parse(ctx context.Context, methodName string, inputJSON []byte) (*workerplugin.ParseResult, error) {
	res, err := recovery.Call1(func() (*workerplugin.ParseResult, error) {
		return r.inner.Parse(ctx, methodName, inputJSON)
	})
	if err == nil {
		return res, nil
	}
	stack := debug.Stack()
	r.logPanic("Parse", err, stack)
	return nil, r.panicError("Parse", err, stack)
}

func (r *recoveringWorker) GetGoroutines(ctx context.Context, filter string) (*workerplugin.GoroutinesResult, error) {
	res, err := recovery.Call1(func() (*workerplugin.GoroutinesResult, error) {
		return r.inner.GetGoroutines(ctx, filter)
	})
	if err == nil {
		return res, nil
	}
	stack := debug.Stack()
	r.logPanic("GetGoroutines", err, stack)
	return nil, r.panicError("GetGoroutines", err, stack)
}
