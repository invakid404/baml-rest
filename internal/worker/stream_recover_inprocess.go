//go:build inprocess

package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"runtime/debug"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/workerplugin"
)

// streamPanicErrorCode mirrors pool.panicErrorCode for inprocess
// builds. Duplicated rather than imported so this package does not
// gain a dependency on pool just for one string constant.
const streamPanicErrorCode = "internal_error"

// recoverBridgePanic recovers a panic in bridgeStreamResults' producer
// goroutine and surfaces it as one terminal stream error frame on out
// (unless the caller has already gone away). The companion
// !inprocess variant is empty: subprocess builds let bridge-goroutine
// panics terminate the worker process so the host pool restarts it.
//
// This uses a deferred recover() rather than wrapping the goroutine
// body in recovery.Call because the recover must run inside the
// existing goroutine; restructuring bridgeStreamResults to fit
// recovery.Call's func-value shape would complicate its defer order
// (the close(out) defer must remain last to fire first per LIFO).
func recoverBridgePanic(ctx context.Context, out chan<- *workerplugin.StreamResult, logger bamlutils.Logger) {
	r := recover()
	if r == nil {
		return
	}

	var panicErr error
	switch v := r.(type) {
	case error:
		panicErr = v
	default:
		panicErr = fmt.Errorf("%v", v)
	}

	stack := debug.Stack()
	sum := sha256.Sum256(stack)
	hash := hex.EncodeToString(sum[:6])
	if logger != nil {
		logger.Error("in-process bridge goroutine panic recovered",
			"method", "bridgeStreamResults",
			"panic_value", panicErr.Error(),
			"stack_hash", hash,
			"stack", string(stack),
		)
	}

	// If the caller has cancelled, the consumer side has already
	// stopped reading; sending here would block until the channel
	// closes and the defer chain unwinds. Drop the frame instead and
	// let the existing close(out) defer release the channel.
	select {
	case <-ctx.Done():
		return
	default:
	}

	frame := workerplugin.GetStreamResult()
	frame.Kind = workerplugin.StreamResultKindError
	frame.Error = fmt.Errorf("in-process bridge goroutine panic: %w", panicErr)
	frame.Stacktrace = string(stack)
	frame.ErrorCode = streamPanicErrorCode

	select {
	case out <- frame:
	case <-ctx.Done():
		workerplugin.ReleaseStreamResult(frame)
	}
}
