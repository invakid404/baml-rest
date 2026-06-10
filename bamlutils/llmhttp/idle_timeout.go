package llmhttp

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// EnvVarStreamIdleTimeout selects the server-level inter-token idle read
// timeout enforced on the build-request streaming path. The value is a Go
// duration string (e.g. "300s", "5m"). "0" means infinite (no idle bound),
// matching BAML's idle_timeout_ms=0 semantics. Unset or unparseable falls
// back to DefaultStreamIdleTimeout so the production hang is bounded out of
// the box.
const EnvVarStreamIdleTimeout = "BAML_REST_STREAM_IDLE_TIMEOUT"

// DefaultStreamIdleTimeout is the inter-token idle read timeout applied when
// the caller does not configure one. It is deliberately generous — large
// enough that no legitimately-progressing stream (including reasoning models
// with long thinking gaps, which still emit periodic bytes/keepalives) is
// ever killed, but small enough that a silently-stalled provider socket is
// torn down in minutes rather than pinning a worker forever. It mirrors
// BAML's request_timeout_ms default (5 minutes).
const DefaultStreamIdleTimeout = 5 * time.Minute

// ErrIdleTimeout is the sentinel returned by idleTimeoutReader when the
// underlying stream delivers no bytes within the configured idle window
// after the first byte. classifyTransportErr maps it to a retryable
// *TransportError (TransportFlakeIdleTimeout) so a stalled provider stream
// surfaces as a transport flake rather than a clean io.EOF — the latter
// would let the orchestrator treat a stalled stream as a successfully
// completed (but truncated) response. See classifyTransportErr.
var ErrIdleTimeout = errors.New("llmhttp: stream idle timeout")

// ParseStreamIdleTimeout interprets a raw env-style string into an idle
// timeout duration. Empty / unparseable / negative input collapses to
// DefaultStreamIdleTimeout so a typo never silently disables the bound; an
// explicit "0" is preserved as "infinite" (BAML idle_timeout_ms=0 parity).
func ParseStreamIdleTimeout(raw string) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return DefaultStreamIdleTimeout
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 {
		return DefaultStreamIdleTimeout
	}
	return d
}

// StreamIdleTimeoutFromEnv resolves the idle timeout from
// BAML_REST_STREAM_IDLE_TIMEOUT. Used by cmd/serve and cmd/worker at startup
// so the per-process llmhttp.Client matches the env-driven configuration.
func StreamIdleTimeoutFromEnv() time.Duration {
	return ParseStreamIdleTimeout(os.Getenv(EnvVarStreamIdleTimeout))
}

// idleTimeoutReader wraps a streaming response body with an inter-byte idle
// read timeout. It is created in the HTTP layer (ExecuteStream /
// executeStreamFast) before the body is handed to sseclient.Stream, so it is
// transport-agnostic: the net/http resp.Body and the fasthttp
// *fastStreamReader are both io.ReadCloser whose Close() severs the
// connection race-safely.
//
// Timer discipline (the load-bearing safety properties):
//
//   - The timer is (re)set on every Read that returns at least one byte —
//     including SSE ':'-comment keepalive bytes and partial lines, which the
//     sseclient swallows without emitting an Event. Resetting at the byte
//     level (rather than per SSE event) is what makes a slow-but-trickling
//     stream safe: a provider sending only comment keepalives during a long
//     gap keeps the timer alive.
//
//   - The timer is armed lazily on the first byte, so first-token latency (a
//     provider "thinking" before any output) is not conflated with an
//     inter-token stall. A stream that never delivers a first byte is bounded
//     elsewhere (the caller's context), not here.
//
// On fire, the watchdog sets fired and runs interrupt() — a race-safe action
// that severs the connection to unblock the parked Read WITHOUT releasing any
// pooled transport state (which the SSE scanner goroutine may still hold a
// reference to). For net/http that is resp.Body.Close(); for fasthttp it is
// the captured socket's shutdown (slot.shutdown), mirroring the existing
// ctx-cancel watcher — the full fastStreamReader.Close (which returns pooled
// request/response objects to fasthttp) is deferred to the consumer's Close()
// after the scanner has exited, exactly as on the ctx-cancel path.
//
// When the parked Read returns with NO usable data (n==0) because of the
// idle close, Read surfaces ErrIdleTimeout (or ctx.Err() when the context was
// cancelled, so a client cancel is not mislabelled a provider stall).
// Returning a non-EOF sentinel is mandatory: a clean io.EOF here would make
// the orchestrator run parseFinal on the truncated accumulator and report a
// silent, possibly-malformed success.
//
// Conversely, when a Read returns n>0 the bytes are delivered unconditionally
// — even if the watchdog fired in the race window between the underlying Read
// returning and our inspection. Data always wins: a Read that produced bytes
// never returns ErrIdleTimeout, so a stream that keeps trickling bytes within
// the idle window is never false-killed at the boundary. A subsequent
// genuinely-idle (n==0) read is what surfaces the sentinel.
type idleTimeoutReader struct {
	ctx       context.Context
	r         io.Reader
	closer    io.Closer
	interrupt func()
	timeout   time.Duration

	fired atomic.Bool

	mu     sync.Mutex
	timer  *time.Timer
	closed bool
}

// newIdleTimeoutReader wraps body with an idle read timeout. A non-positive
// timeout means "no idle bound" (BAML idle_timeout_ms=0 parity) — the body is
// returned unwrapped so there is zero added cost on that path.
//
// interrupt is the race-safe action invoked on idle fire to unblock a parked
// Read without releasing pooled transport state. A nil interrupt defaults to
// body.Close(), which is correct for the net/http backend (resp.Body.Close
// may be called concurrently with a parked Read and is idempotent). The
// fasthttp backend must pass slot.shutdown so the pooled request/response are
// released only later, by the consumer's Close(), once the scanner has exited.
func newIdleTimeoutReader(ctx context.Context, body io.ReadCloser, interrupt func(), timeout time.Duration) io.ReadCloser {
	if body == nil || timeout <= 0 {
		return body
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if interrupt == nil {
		interrupt = func() { _ = body.Close() }
	}
	return &idleTimeoutReader{
		ctx:       ctx,
		r:         body,
		closer:    body,
		interrupt: interrupt,
		timeout:   timeout,
	}
}

func (r *idleTimeoutReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)

	if n > 0 {
		// DATA WINS, unconditionally. Bytes that were genuinely read — even
		// if the idle watchdog fired and closed the conn in the race window
		// between this Read returning and our inspection — must be delivered,
		// never discarded. This is the load-bearing safety property: a Read
		// that produced bytes NEVER returns ErrIdleTimeout, so a stream that
		// keeps trickling any bytes within the idle window is never killed at
		// the boundary. Reset the idle timer to bound the NEXT inter-byte gap
		// and defer any accompanying error (e.g. a final io.EOF) to the
		// following Read, where it is handled with the data already drained.
		r.armOrReset()
		return n, nil
	}

	// n == 0: this Read produced no usable data, so it is safe to surface a
	// terminal condition. If the watchdog fired, the idle close is what
	// stopped the bytes — return the sentinel (preferring ctx.Err() so a
	// client cancel is not mislabelled a provider stall).
	if r.fired.Load() {
		if ctxErr := r.ctx.Err(); ctxErr != nil {
			return 0, ctxErr
		}
		return 0, ErrIdleTimeout
	}

	if err != nil {
		// Stream ending (clean EOF or a real read error) with no data — stop
		// the watchdog so it cannot fire spuriously after a clean end.
		r.stop()
	}
	return 0, err
}

// armOrReset starts the idle timer on the first byte and resets it on every
// subsequent byte. The timer runs while the next Read blocks; if no byte
// arrives within timeout, fire() severs the connection.
func (r *idleTimeoutReader) armOrReset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	if r.timer == nil {
		r.timer = time.AfterFunc(r.timeout, r.fire)
		return
	}
	r.timer.Reset(r.timeout)
}

func (r *idleTimeoutReader) stop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.timer != nil {
		r.timer.Stop()
	}
}

// fire is the watchdog callback. It is idempotent (CAS-guarded) and runs the
// race-safe interrupt to unblock a parked Read. The interrupt is safe to run
// concurrently with the in-flight Read and with the consumer's Close() / the
// existing ctx-cancel watcher on both backends; whichever severs the conn
// first wins, the rest are no-ops.
func (r *idleTimeoutReader) fire() {
	if r.fired.CompareAndSwap(false, true) {
		r.interrupt()
	}
}

// Close stops the watchdog and closes the underlying body. Safe to call
// multiple times: the underlying closers (net/http resp.Body, fastStreamReader)
// are idempotent.
func (r *idleTimeoutReader) Close() error {
	r.mu.Lock()
	r.closed = true
	if r.timer != nil {
		r.timer.Stop()
	}
	r.mu.Unlock()
	return r.closer.Close()
}
