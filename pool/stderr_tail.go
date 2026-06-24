package pool

import (
	"bytes"
	"fmt"
	"strings"
	"sync"
)

const (
	// stderrTailMaxLines bounds the worker stderr ring buffer to the
	// most recent N lines. A panic header plus its goroutine stack and
	// the surrounding worker restart/error context fit comfortably;
	// older lines are dropped.
	stderrTailMaxLines = 100

	// stderrTailMaxBytes caps total retained bytes regardless of line
	// count, so a worker emitting pathologically long lines (or no
	// newlines at all) can never grow the buffer without bound.
	stderrTailMaxBytes = 64 * 1024
)

// stderrRingBuffer is a thread-safe, bounded ring buffer that retains
// the most recent stderr output of a subprocess worker. It implements
// io.Writer so it can be attached to go-plugin's ClientConfig.Stderr
// and SyncStderr drains. Only the last stderrTailMaxLines lines (and at
// most stderrTailMaxBytes bytes) are kept; older output is discarded as
// new lines arrive, so the buffer never grows without bound.
//
// It is consulted only on terminal worker-infrastructure failures (see
// withWorkerStderr), so the steady-state cost is a bounded set of line
// strings per live worker and nothing on the happy path.
type stderrRingBuffer struct {
	mu       sync.Mutex
	lines    []string // most-recent-last, bounded by maxLines/maxBytes
	curBytes int      // running sum of len(lines)
	partial  []byte   // bytes after the last newline, not yet a full line
	maxLines int
	maxBytes int
}

func newStderrRingBuffer() *stderrRingBuffer {
	return &stderrRingBuffer{
		maxLines: stderrTailMaxLines,
		maxBytes: stderrTailMaxBytes,
	}
}

// Write splits p into newline-delimited lines and retains the most
// recent ones. Partial trailing data (no newline yet) is buffered until
// the rest of the line arrives. It never returns an error and always
// reports the full length consumed, so go-plugin's stderr-drain
// goroutine is never stalled by the sink.
func (r *stderrRingBuffer) Write(p []byte) (int, error) {
	n := len(p)

	r.mu.Lock()
	defer r.mu.Unlock()

	rest := p
	for {
		i := bytes.IndexByte(rest, '\n')
		if i < 0 {
			r.partial = append(r.partial, rest...)
			// Bound the partial accumulator too: a producer that never
			// emits a newline must not grow it without limit. Keep the
			// most recent maxBytes (where the crash text most likely is).
			if len(r.partial) > r.maxBytes {
				r.partial = append(r.partial[:0], r.partial[len(r.partial)-r.maxBytes:]...)
			}
			break
		}
		// string(...) copies, so reusing partial's backing array next
		// iteration is safe.
		line := string(append(r.partial, rest[:i]...))
		r.partial = r.partial[:0]
		r.appendLine(line)
		rest = rest[i+1:]
	}
	return n, nil
}

// appendLine adds one completed line and evicts the oldest lines until
// both the line-count and byte-count bounds hold. At least one line is
// always retained even if it alone exceeds maxBytes, so the freshest
// output is never dropped entirely.
func (r *stderrRingBuffer) appendLine(line string) {
	r.lines = append(r.lines, line)
	r.curBytes += len(line)
	for len(r.lines) > r.maxLines || (r.curBytes > r.maxBytes && len(r.lines) > 1) {
		r.curBytes -= len(r.lines[0])
		r.lines = r.lines[1:]
	}
}

// snapshot returns the retained stderr tail as a single newline-joined
// string and the number of lines it contains. A non-empty partial
// trailing line (output not yet newline-terminated, e.g. a worker that
// died mid-line) is included as a final line so a panic header that
// never got its trailing newline still surfaces.
func (r *stderrRingBuffer) snapshot() (string, int) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if len(r.lines) == 0 && len(r.partial) == 0 {
		return "", 0
	}

	out := make([]string, 0, len(r.lines)+1)
	out = append(out, r.lines...)
	if len(r.partial) > 0 {
		out = append(out, string(r.partial))
	}
	return strings.Join(out, "\n"), len(out)
}

// withWorkerStderr appends the failed worker's captured stderr tail to a
// terminal pool error so the host sees the underlying panic/stack right
// next to the bare transport symptom (Unavailable / "error reading from
// server: EOF") instead of having to dig through worker logs (issue
// #450). It is a no-op when:
//   - err is nil,
//   - h is nil (no failed handle was recorded for this attempt), or
//   - the handle has no captured stderr — in-process builds never
//     populate one, and a subprocess worker that died before emitting
//     anything has an empty buffer.
//
// The original error is wrapped with %w, so errors.Is(err,
// ErrPoolRetriesExhausted) and any embedded gRPC status remain intact.
// Call this ONLY on terminal retry-exhaustion / no-worker-available
// paths — never on the happy path or per-attempt retries — to keep the
// tail out of normal-operation logs.
func withWorkerStderr(err error, h *workerHandle) error {
	if err == nil || h == nil || h.stderrTail == nil {
		return err
	}
	tail, n := h.stderrTail.snapshot()
	if tail == "" {
		return err
	}
	return fmt.Errorf("%w\n--- worker %d stderr (last %d lines) ---\n%s", err, h.id, n, tail)
}
