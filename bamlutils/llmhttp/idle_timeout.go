package llmhttp

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// isClosedConnErr reports whether err is the "use of closed connection" error
// that a Read returns after our interrupt has closed the underlying conn.
// net.ErrClosed only arises when the LOCAL side closed the conn — the upstream
// cannot induce it — so combined with the fired flag it is strong provenance
// that an error is our own idle close rather than a genuine upstream failure.
func isClosedConnErr(err error) bool {
	return errors.Is(err, net.ErrClosed) || errors.Is(err, os.ErrClosed)
}

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

// EnvVarStreamFirstBodyTimeout selects the exact native stream lane's
// first-body read timeout: the bound from a successful response header line
// until the FIRST raw upstream body byte. It is DELIBERATELY distinct from
// EnvVarStreamIdleTimeout because the idle bound only arms after the first
// byte, so a provider that returns headers and then never sends a body would
// otherwise be bounded only by the caller context (scope §3.2 / §5.10). The
// value is a Go duration string; "0" means infinite (no first-body bound).
// Unset or unparseable falls back to DefaultStreamFirstBodyTimeout.
//
// This bound is consumed ONLY by the exact native stream transport
// (exact_stream.go), which is unrouted in Phase 7A — the legacy BAML streaming
// path is unaffected.
const EnvVarStreamFirstBodyTimeout = "BAML_REST_STREAM_FIRST_BODY_TIMEOUT"

// DefaultStreamFirstBodyTimeout bounds the response-header-to-first-body gap on
// the exact native stream lane when unconfigured. Per scope §11 the production
// value is an owner-chosen operational number; the §11 default is to reuse the
// idle timeout value while keeping this bound SEPARATELY named and observable,
// so it can be tuned independently of the inter-byte idle bound.
const DefaultStreamFirstBodyTimeout = DefaultStreamIdleTimeout

// ParseStreamFirstBodyTimeout interprets a raw env-style string into a
// first-body timeout duration, with the same fail-safe semantics as
// ParseStreamIdleTimeout: empty / unparseable / negative collapses to
// DefaultStreamFirstBodyTimeout so a typo never silently disables the bound; an
// explicit "0" is preserved as "infinite".
func ParseStreamFirstBodyTimeout(raw string) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return DefaultStreamFirstBodyTimeout
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 {
		return DefaultStreamFirstBodyTimeout
	}
	return d
}

// StreamFirstBodyTimeoutFromEnv resolves the first-body timeout from
// BAML_REST_STREAM_FIRST_BODY_TIMEOUT.
func StreamFirstBodyTimeoutFromEnv() time.Duration {
	return ParseStreamFirstBodyTimeout(os.Getenv(EnvVarStreamFirstBodyTimeout))
}

// byteProgressWatchdog is a race-safe, single-shot inter-byte deadline used by
// the exact native stream lane's first-body/idle reader (exact_stream.go). It
// factors out the timer/fired/closed discipline the legacy BAML
// idleTimeoutReader (below) carries inline: the two-phase stream reader reuses
// exactly this deadline logic rather than reimplementing it. The legacy reader
// is deliberately left with its own inline copy so its white-box suite — which
// pins the reader's internal fields — stays byte-identical; the two agree on
// semantics by construction (this type is a direct extraction of that code).
//
//   - arm(timeout) starts a fresh timer window on every call (to a possibly
//     different duration, which the two-phase stream reader relies on). A call
//     after markClosed is a no-op, so a Read racing Close can never re-arm a
//     stopped watchdog.
//   - fire() runs interrupt() EXACTLY ONCE (CAS-guarded), severing a parked
//     Read; concurrent fire/Close/cancel all collapse to a single interrupt.
//   - hasFired() lets the reader distinguish "our own deadline close" from a
//     genuine upstream error on the byte-delivering path.
//
// Stale-callback safety: time.Timer.Stop / Reset do NOT retract a callback
// whose timer has already expired but whose goroutine has not yet run, so a
// superseded window's callback could otherwise interrupt a LATER window and
// surface a false idle timeout on a healthy, progressing stream. Each window
// carries a monotonically-increasing generation; arm/stop/markClosed advance it
// and each timer callback captures its own generation, so fire is a no-op unless
// its window is still the current one. (The legacy idleTimeoutReader keeps its
// simpler inline Reset-based copy; only this reused watchdog is hardened.)
type byteProgressWatchdog struct {
	// interrupt is the race-safe action invoked exactly once on fire to unblock
	// a parked Read (typically the body's Close). Set before the timer is armed.
	interrupt func()

	fired atomic.Bool

	mu     sync.Mutex
	timer  *time.Timer
	gen    uint64 // generation of the currently-armed window
	closed bool
}

// arm starts a fresh timer window, invalidating any pending callback from a
// previous window via the generation. It is a no-op once markClosed has run, so
// a late Read cannot resurrect a closed watchdog's timer.
func (w *byteProgressWatchdog) arm(timeout time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return
	}
	w.gen++
	gen := w.gen
	if w.timer != nil {
		w.timer.Stop()
	}
	w.timer = time.AfterFunc(timeout, func() { w.fire(gen) })
}

// stop halts the timer and advances the generation (so a stale, already-expired
// callback cannot fire) without marking the watchdog closed, so it may be
// re-armed later (the "data wins, wait for the next gap" path).
func (w *byteProgressWatchdog) stop() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.gen++
	if w.timer != nil {
		w.timer.Stop()
	}
}

// fire is the timer callback for one window. It runs interrupt exactly once, and
// only while its window is still current (gen match) and the watchdog is open —
// the generation check drops a stale callback from a superseded window. The
// gen check and the fired CAS are performed together under the lock so a window
// switch racing this callback cannot let a superseded window still interrupt.
func (w *byteProgressWatchdog) fire(gen uint64) {
	w.mu.Lock()
	if w.closed || gen != w.gen || !w.fired.CompareAndSwap(false, true) {
		w.mu.Unlock()
		return
	}
	w.mu.Unlock()
	w.interrupt()
}

// hasFired reports whether the watchdog's interrupt has run.
func (w *byteProgressWatchdog) hasFired() bool { return w.fired.Load() }

// markClosed stops the timer, advances the generation (invalidating any pending
// callback), and blocks any future arm, so the watchdog is inert after the
// reader's Close.
func (w *byteProgressWatchdog) markClosed() {
	w.mu.Lock()
	w.closed = true
	w.gen++
	if w.timer != nil {
		w.timer.Stop()
	}
	w.mu.Unlock()
}

// idleTimeoutReader wraps a streaming response body with an inter-byte idle
// read timeout. It is created in the HTTP layer (ExecuteStream) before the
// body is handed to sseclient.Stream. Streaming runs exclusively over
// net/http (Stage 1 of the streaming memory effort, #475 follow-up), so the
// wrapped body is always a net/http resp.Body — an io.ReadCloser whose
// Close() severs the connection race-safely (it may be called concurrently
// with a parked Read).
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
// that severs the connection to unblock the parked Read. For the net/http
// streaming path that is resp.Body.Close(), which net/http supports
// concurrently with a parked Read.
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
// genuinely-idle (n==0) read is what surfaces the sentinel. Only our OWN
// idle-close error is suppressed on the n>0 path; a genuine upstream error
// (e.g. io.ErrUnexpectedEOF) is propagated alongside the bytes so a real
// truncation is never masked as a clean end.
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
// Read. A nil interrupt defaults to body.Close(), which is correct for the
// net/http streaming backend (resp.Body.Close may be called concurrently with
// a parked Read and is idempotent). ExecuteStream relies on this default.
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
		// DATA WINS: bytes that were genuinely read are always delivered,
		// never discarded. This is the load-bearing safety property — a Read
		// that produced bytes NEVER returns ErrIdleTimeout, so a stream that
		// keeps trickling any bytes within the idle window is never killed at
		// the boundary.
		if err == nil {
			// Clean data read — reset the idle timer for the next gap.
			r.armOrReset()
			return n, nil
		}
		// An error rode in with the bytes. Suppress it ONLY when it is
		// provably OUR idle close: the watchdog fired AND the error is a
		// closed-connection error, which only our interrupt (closing the
		// local conn) can produce — the upstream cannot make a local read
		// return net.ErrClosed. `fired` alone is insufficient: it proves the
		// callback ran, not that THIS error came from our close, so a genuine
		// io.ErrUnexpectedEOF (net/http's truncated-chunked signal)
		// that merely races the timer must keep its real identity. When it IS
		// our close, deliver the bytes now and let the subsequent n==0 read
		// surface the ErrIdleTimeout sentinel.
		if r.fired.Load() && isClosedConnErr(err) {
			return n, nil
		}
		// Genuine upstream error — deliver the bytes AND propagate the error
		// with its real identity so the scanner drains the bytes then reports
		// it. Returning (n, nil) here would end the stream with a nil Errc and
		// run parseFinal on a partial accumulator (silent truncation).
		r.stop()
		return n, err
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
// multiple times: the underlying closer (net/http resp.Body) is idempotent.
func (r *idleTimeoutReader) Close() error {
	r.mu.Lock()
	r.closed = true
	if r.timer != nil {
		r.timer.Stop()
	}
	r.mu.Unlock()
	return r.closer.Close()
}
