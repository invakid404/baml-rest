package buildrequest

import (
	"context"
	"log"

	"github.com/invakid404/baml-rest/bamlutils"
)

// De-BAML Slice 8B — panic-contained, PAYLOAD-REDACTED fire-and-forget dispatch for
// the generated static observe seam.
//
// The generated static /call + /parse hooks build a neutral bamlutils.NativeStaticInvocation
// and hand it here to run the discarded, observe-only observation off the request
// goroutine. This lives in buildrequest (active orchestration code, already the home
// of the generated adapter's request machinery) rather than the PASSIVE
// bamlutils/native_static_serve.go boundary.
//
// SECURITY: a panic escaping the observer can carry SENSITIVE data — a
// NativeStaticInvocation's descriptor holds prompt bytes and inline client
// credentials, and its BuildBAMLRequest plan closure yields the bearer Authorization
// header and request body. We therefore DELIBERATELY DO NOT use go-recovery's
// process-global, mutable ErrorHandler (whose default is log.Printf("%+v", err) and
// whose PanicError formatter emits the raw panic payload), which would write that
// data to process logs. Instead we recover locally in the goroutine and DROP the
// recovered value entirely, emitting only a fixed, payload-free breadcrumb — never the
// value, never a formatted error, never a re-panic.

// staticObserverPanicHook emits the fixed, PAYLOAD-FREE breadcrumb for a recovered
// static-observer panic. It is a package var so tests can observe that it fired
// WITHOUT the recovered value ever being formatted or logged. It MUST never receive,
// format, or log the recovered value.
var staticObserverPanicHook = func() {
	log.Print("buildrequest: debaml static observer panic suppressed (observe-only, no serving impact)")
}

// staticObserveMaxInFlight bounds the number of concurrent static observations. Each
// observation runs the full pre-socket pipeline (render + canonical body + nanollm
// New/Prepare FFI) on its own goroutine, so an unbounded spawn would let a burst of
// eligible static calls (flag on) create many orphaned FFI-doing goroutines. The bound
// caps that; when it is reached the observation is DROPPED (see ObserveStaticFireAndForget).
const staticObserveMaxInFlight = 16

// staticObserveSem is the bounded-concurrency gate for static observations. A token is
// acquired SYNCHRONOUSLY by the caller before the goroutine spawns and released when the
// observation returns.
var staticObserveSem = make(chan struct{}, staticObserveMaxInFlight)

// ObserveStaticFireAndForget runs observe(background, inv) FIRE-AND-FORGET with LOCAL
// panic containment and BOUNDED concurrency. It returns immediately (the observation
// runs on a new goroutine), so an observer panic or its full pre-socket work can never
// block or crash the request goroutine. It is observe-only: the result is discarded and
// the caller proceeds with BAML exactly as today (the injected observer always declines
// and opens no socket). It uses NO process-global handler and never logs the panic
// payload.
//
// Concurrency is capped at staticObserveMaxInFlight; when at capacity the observation is
// DROPPED rather than spawning an unbounded goroutine — it is best-effort and never
// affects serving, so a dropped measurement is a bounded observability gap, not a
// correctness issue. context.Background() is used deliberately: the observation is
// decoupled from the request lifecycle (it must be able to build the no-send plan even
// if the request is cancelled), and the bound keeps orphaned FFI work small and
// short-lived (Prepare opens no socket).
func ObserveStaticFireAndForget(observe bamlutils.NativeStaticObserveFunc, inv bamlutils.NativeStaticInvocation) {
	if observe == nil {
		return
	}
	// Capture the semaphore locally so an in-flight goroutine always releases into the
	// SAME channel it acquired from (keeps tests that swap the package var for isolation
	// race-free, and is harmless in production where the var never changes).
	sem := staticObserveSem
	select {
	case sem <- struct{}{}:
		go func() {
			defer func() { <-sem }()
			observeStaticRecovered(observe, inv)
		}()
	default:
		// At capacity — DROP this best-effort, observe-only observation. BAML still
		// serves; only the measurement is skipped.
	}
}

// observeStaticRecovered runs the observation synchronously with a LOCAL deferred
// recover that DROPS the recovered value (never formatting/logging/re-panicking it).
// It is separated from the goroutine spawn so tests can drive it deterministically.
func observeStaticRecovered(observe bamlutils.NativeStaticObserveFunc, inv bamlutils.NativeStaticInvocation) {
	defer func() {
		if r := recover(); r != nil {
			// DROP r — it may carry descriptor prompt/credentials or Authorization/body.
			// Emit only the fixed, payload-free breadcrumb; do NOT touch r further.
			staticObserverPanicHook()
		}
	}()
	_ = observe(context.Background(), inv)
}
