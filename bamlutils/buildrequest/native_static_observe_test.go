package buildrequest

import (
	"bytes"
	"context"
	"errors"
	"go/parser"
	"go/token"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/promptdescriptor"
)

// sentinel is SENSITIVE-looking material — mimicking a NativeStaticInvocation's
// descriptor prompt bytes, inline credential, bearer Authorization, and request body —
// that a leaky panic handler would emit. NOTHING carrying it may reach the logs.
const sentinel = "SENTINEL-prompt<<write about {{topic}}>>-api_key:sk-LEAK123-Authorization:Bearer sk-LEAK123-body:{\"model\":\"x\"}"

// TestObserveStaticFireAndForget_PanicIsContainedAndRedacted proves the de-BAML Slice
// 8B fire-and-forget observe dispatch (a) recovers an observer panic without crashing
// or propagating, and (b) NEVER writes the recovered value to the logs — only the
// fixed, payload-free breadcrumb — and never routes it to go-recovery's mutable
// process-global handler (this path does not use go-recovery at all).
func TestObserveStaticFireAndForget_PanicIsContainedAndRedacted(t *testing.T) {
	// Capture the stdlib log output for the duration.
	var logBuf bytes.Buffer
	prevOut := log.Writer()
	log.SetOutput(&logBuf)
	defer log.SetOutput(prevOut)

	// Observe the fixed breadcrumb: wrap the hook to count invocations while still
	// exercising the real (payload-free) default so the log-capture assertion is real.
	realHook := staticObserverPanicHook
	var hookCalls int
	staticObserverPanicHook = func() {
		hookCalls++
		realHook()
	}
	defer func() { staticObserverPanicHook = realHook }()

	// An observer that panics with the SENSITIVE sentinel as its payload.
	panicObserve := func(context.Context, bamlutils.NativeStaticInvocation) bamlutils.NativeStaticResult {
		panic(errors.New(sentinel))
	}
	// A sensitive invocation (descriptor prompt carries the sentinel too, so a stray
	// %+v of the invocation would also surface it).
	inv := bamlutils.NativeStaticInvocation{
		Method:     "M",
		Descriptor: promptdescriptor.Function{Method: "M", Prompt: sentinel},
	}

	// Drive the SYNCHRONOUS core (deterministic) — it must recover the panic and
	// return normally (no crash, no re-panic).
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("panic escaped observeStaticRecovered: %v", r)
			}
		}()
		observeStaticRecovered(panicObserve, inv)
	}()

	if hookCalls != 1 {
		t.Errorf("payload-free breadcrumb fired %d times, want exactly 1", hookCalls)
	}
	if got := logBuf.String(); strings.Contains(got, sentinel) {
		t.Errorf("SECURITY: sentinel payload leaked to logs")
	}
	if got := logBuf.String(); strings.Contains(got, "sk-LEAK123") {
		t.Errorf("SECURITY: credential fragment leaked to logs")
	}
	// The breadcrumb itself must be present and payload-free.
	if got := logBuf.String(); !strings.Contains(got, "static observer panic suppressed") {
		t.Errorf("expected the fixed payload-free breadcrumb, got %q", got)
	}
}

// TestObserveStaticFireAndForget_DoesNotBlockCaller proves the dispatch is genuinely
// fire-and-forget: the request goroutine returns immediately even if the observer
// blocks indefinitely.
func TestObserveStaticFireAndForget_DoesNotBlockCaller(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	blockObserve := func(context.Context, bamlutils.NativeStaticInvocation) bamlutils.NativeStaticResult {
		<-release
		return bamlutils.NativeStaticResult{Disposition: bamlutils.NativeStaticDeclined}
	}

	returned := make(chan struct{})
	go func() {
		ObserveStaticFireAndForget(blockObserve, bamlutils.NativeStaticInvocation{Method: "M"})
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(3 * time.Second):
		t.Fatal("ObserveStaticFireAndForget blocked the caller (not fire-and-forget)")
	}
}

// TestObserveStaticFireAndForget_PublicAsyncPanicContained exercises the PUBLIC
// entry point with a sentinel-PANICKING observer and proves the fire-and-forget
// contract end to end: the public call returns PROMPTLY (before the ACTUAL goroutine
// finishes — the observer is held blocked so the breadcrumb cannot have fired yet),
// the goroutine then recovers the sentinel panic (no crash/propagation), and NEITHER
// the sentinel prompt NOR any credential/body fragment reaches the captured logs.
func TestObserveStaticFireAndForget_PublicAsyncPanicContained(t *testing.T) {
	var logBuf bytes.Buffer
	prevOut := log.Writer()
	log.SetOutput(&logBuf)
	defer log.SetOutput(prevOut)

	realHook := staticObserverPanicHook
	recovered := make(chan struct{})
	staticObserverPanicHook = func() {
		realHook() // payload-free breadcrumb (log capture asserts it carries no payload)
		close(recovered)
	}
	defer func() { staticObserverPanicHook = realHook }()

	started := make(chan struct{})
	release := make(chan struct{})
	panicObserve := func(context.Context, bamlutils.NativeStaticInvocation) bamlutils.NativeStaticResult {
		close(started)
		<-release // hold the goroutine so the public call must return before recovery
		panic(errors.New(sentinel))
	}
	inv := bamlutils.NativeStaticInvocation{
		Method:     "M",
		Descriptor: promptdescriptor.Function{Method: "M", Prompt: sentinel},
	}

	// The PUBLIC call must return while the goroutine is still blocked in the observer.
	ObserveStaticFireAndForget(panicObserve, inv)

	<-started // the actual goroutine is running the observer
	select {
	case <-recovered:
		t.Fatal("public call did not return before the goroutine finished — not fire-and-forget")
	default: // good: returned promptly, goroutine still blocked (breadcrumb not fired yet)
	}

	close(release) // let the observer panic; the goroutine must recover it
	select {
	case <-recovered:
	case <-time.After(3 * time.Second):
		t.Fatal("goroutine did not recover the sentinel panic")
	}

	if got := logBuf.String(); strings.Contains(got, sentinel) || strings.Contains(got, "sk-LEAK123") {
		t.Errorf("SECURITY: sensitive panic payload leaked to logs via the public async path")
	}
	if got := logBuf.String(); !strings.Contains(got, "static observer panic suppressed") {
		t.Errorf("expected the fixed payload-free breadcrumb, got %q", got)
	}
}

// TestObserveStaticFireAndForget_NilObserve is a no-op (no goroutine, no panic).
func TestObserveStaticFireAndForget_NilObserve(t *testing.T) {
	ObserveStaticFireAndForget(nil, bamlutils.NativeStaticInvocation{Method: "M"})
}

// TestObserveStaticFireAndForget_BoundedConcurrencyDropsUnderSaturation proves the
// dispatch bounds concurrency: once staticObserveMaxInFlight observations are in
// flight (held blocked), a further call is DROPPED (no goroutine spawned, observer
// never invoked) rather than growing goroutines unboundedly. The dropped observation
// is best-effort and never affects serving.
func TestObserveStaticFireAndForget_BoundedConcurrencyDropsUnderSaturation(t *testing.T) {
	// Isolate with a fresh semaphore; in-flight goroutines from other tests release
	// into their own captured channel, so restoring here is race-free.
	prev := staticObserveSem
	staticObserveSem = make(chan struct{}, staticObserveMaxInFlight)
	defer func() { staticObserveSem = prev }()

	release := make(chan struct{})
	defer close(release)
	running := make(chan struct{}, staticObserveMaxInFlight)
	blockObserve := func(context.Context, bamlutils.NativeStaticInvocation) bamlutils.NativeStaticResult {
		running <- struct{}{}
		<-release
		return bamlutils.NativeStaticResult{Disposition: bamlutils.NativeStaticDeclined}
	}

	// Fill the semaphore to capacity (tokens are acquired synchronously in the call).
	for i := 0; i < staticObserveMaxInFlight; i++ {
		ObserveStaticFireAndForget(blockObserve, bamlutils.NativeStaticInvocation{Method: "M"})
	}
	// Wait until all capacity slots are actually running their (blocked) observers.
	for i := 0; i < staticObserveMaxInFlight; i++ {
		select {
		case <-running:
		case <-time.After(3 * time.Second):
			t.Fatalf("only %d/%d observations started", i, staticObserveMaxInFlight)
		}
	}

	// The next call must be DROPPED: its observer must never run.
	invoked := make(chan struct{}, 1)
	dropObserve := func(context.Context, bamlutils.NativeStaticInvocation) bamlutils.NativeStaticResult {
		invoked <- struct{}{}
		return bamlutils.NativeStaticResult{Disposition: bamlutils.NativeStaticDeclined}
	}
	ObserveStaticFireAndForget(dropObserve, bamlutils.NativeStaticInvocation{Method: "M"})
	select {
	case <-invoked:
		t.Fatal("observation was NOT dropped under saturation (extra goroutine spawned)")
	case <-time.After(250 * time.Millisecond):
		// good: at capacity → dropped → observer never ran.
	}
}

// TestProductionFileUsesNoRecoveryPackage is the global-handler canary as a
// SOURCE/AST assertion over the PRODUCTION native_static_observe.go: it must import
// NO go-recovery (or any "recovery") package and must not name a recovery package at
// all, so panic containment can only be the LOCAL deferred recover — no
// process-global recovery route can be (re)introduced without failing this test. The
// builtin recover() is fine; a `recovery.` package reference is not.
func TestProductionFileUsesNoRecoveryPackage(t *testing.T) {
	src, err := os.ReadFile("native_static_observe.go")
	if err != nil {
		t.Fatalf("read production file: %v", err)
	}
	f, err := parser.ParseFile(token.NewFileSet(), "native_static_observe.go", src, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse production file: %v", err)
	}
	for _, imp := range f.Imports {
		path := strings.Trim(imp.Path.Value, `"`)
		if strings.Contains(path, "recovery") {
			t.Errorf("production native_static_observe.go must import NO recovery package, found %q", path)
		}
	}
	// Belt-and-suspenders on the raw source: no go-recovery module reference, and no
	// `recovery.` package-qualified call anywhere (the builtin recover() is allowed).
	for _, forbidden := range []string{"gregwebs/go-recovery", "recovery."} {
		if bytes.Contains(src, []byte(forbidden)) {
			t.Errorf("production native_static_observe.go must not reference %q (global-handler route)", forbidden)
		}
	}
}
